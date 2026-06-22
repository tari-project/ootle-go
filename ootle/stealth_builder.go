package ootle

import "fmt"

// This file adds a fluent surface over the confidential (stealth) send path. It only
// composes the existing StealthOutputSpec / StealthTransferIntent shapes — all stealth crypto
// stays in the core. The one host-side check is an input-validation guard: Intent validates
// the revealed-balance bucket before the core call so a mismatch is an early typed error
// instead of a cryptic core failure. The guard rejects bad input; it never computes a balance.

// NewStealthOutput builds a confidential output to a recipient, defaulting PayTo to the
// privacy-preserving stealth one-time key. Chain the With* options for the rarer fields.
func NewStealthOutput(destAccountPK, destViewPK string, amount uint64, resource string) StealthOutputSpec {
	return StealthOutputSpec{
		DestinationAccountPublicKey: destAccountPK,
		DestinationViewPublicKey:    destViewPK,
		Amount:                      amount,
		ResourceAddress:             resource,
		PayTo:                       PayToStealthPublicKey,
	}
}

// WithRevealed sets the per-output revealed (plaintext) deposit, in µTari. The per-output
// revealed amounts must reconcile with the transfer's revealed-output total (the builder
// validates this in Intent).
func (o StealthOutputSpec) WithRevealed(amount uint64) StealthOutputSpec {
	o.RevealedAmount = amount
	return o
}

// WithMemo attaches an optional encrypted memo.
func (o StealthOutputSpec) WithMemo(m *StealthMemo) StealthOutputSpec {
	o.Memo = m
	return o
}

// WithResourceViewKey sets the resource view key (lowercase hex) that drives the
// viewable-balance proof.
func (o StealthOutputSpec) WithResourceViewKey(hex string) StealthOutputSpec {
	o.ResourceViewKey = &hex
	return o
}

// WithUtxoTag sets the optional 4-byte UTXO tag (lowercase hex, 8 chars).
func (o StealthOutputSpec) WithUtxoTag(hex string) StealthOutputSpec {
	o.UtxoTag = &hex
	return o
}

// WithMinimumValuePromise sets the minimum value promise (range-proof lower bound), in µTari.
func (o StealthOutputSpec) WithMinimumValuePromise(amount uint64) StealthOutputSpec {
	o.MinimumValuePromise = amount
	return o
}

// WithPayTo overrides the spend condition (e.g. PayToAccessRuleAllowAll). The escape hatch
// off the stealth-key default NewStealthOutput sets.
func (o StealthOutputSpec) WithPayTo(p StealthPayTo) StealthOutputSpec {
	o.PayTo = p
	return o
}

// StealthTransferBuilder assembles a StealthTransferIntent through a fluent chain instead of a
// bare struct literal. It tracks the revealed input/output running sums as outputs and
// revealed buckets are added, and leaves Inputs resolved through the explicit spend list (a
// confidential transfer names the UTXOs it spends; the driver still fetches their substates).
type StealthTransferBuilder struct {
	from               string
	resource           string
	fee                uint64
	inputs             []StealthTransferInput
	outputs            []StealthOutputSpec
	revealedInput      uint64
	revealedOutput     uint64
	payFeeFromRevealed bool
	minEpoch           *uint64
	maxEpoch           *uint64
	dryRun             bool
}

// NewStealthTransfer starts a confidential-transfer builder funded from fromAccount
// (component_<hex>) over the given resource (resource_<hex>).
func NewStealthTransfer(fromAccount, resource string) *StealthTransferBuilder {
	return &StealthTransferBuilder{from: fromAccount, resource: resource}
}

// Fee sets the fee to pay, in µTari.
func (b *StealthTransferBuilder) Fee(microTari uint64) *StealthTransferBuilder {
	b.fee = microTari
	return b
}

// SpendRevealedInput adds amount (µTari) to the revealed-input bucket — the plaintext value
// drawn from the from-account's revealed vault to fund the transfer.
func (b *StealthTransferBuilder) SpendRevealedInput(amount uint64) *StealthTransferBuilder {
	b.revealedInput += amount
	return b
}

// SpendUTXO adds a confidential input, pulling the commitment from the decoded UTXO. The
// caller supplies their own key context — owner account public key, spend secret, and the
// canonical substate id (utxo_<resource>_<commitment>) to fetch — since the host derives no
// ids.
func (b *StealthTransferBuilder) SpendUTXO(utxo InboundStealthOutput, ownerAccountPK, spendSecret, substateID string) *StealthTransferBuilder {
	b.inputs = append(b.inputs, StealthTransferInput{
		Commitment:            utxo.Commitment,
		OwnerAccountPublicKey: ownerAccountPK,
		SpendSecret:           spendSecret,
		UtxoSubstateID:        substateID,
	})
	return b
}

// ToStealthOutput appends a pure confidential output (no revealed deposit) to a recipient. For
// a per-output revealed deposit use ToOutput(NewStealthOutput(...).WithRevealed(n)).
func (b *StealthTransferBuilder) ToStealthOutput(destAccountPK, destViewPK string, amount uint64) *StealthTransferBuilder {
	return b.ToOutput(NewStealthOutput(destAccountPK, destViewPK, amount, b.resource))
}

// ToOutput appends a fully-formed output spec (e.g. one carrying a memo or a per-output
// revealed deposit). When the spec carries a per-output RevealedAmount, declare the matching
// intent-level total with ToRevealedOutput; Intent validates the two reconcile.
func (b *StealthTransferBuilder) ToOutput(spec StealthOutputSpec) *StealthTransferBuilder {
	b.outputs = append(b.outputs, spec)
	return b
}

// ToRevealedOutput adds amount (µTari) to the revealed-output bucket (e.g. change back to
// self). It declares the intent-level revealed-output total; per-output WithRevealed deposits
// must reconcile with it (the builder validates this in Intent).
func (b *StealthTransferBuilder) ToRevealedOutput(amount uint64) *StealthTransferBuilder {
	b.revealedOutput += amount
	return b
}

// PayFeeFromRevealed forces the account key to seal so the fee is paid from the from-account's
// revealed vault. Required for a pure confidential-input spend (no revealed input), where the
// account would otherwise never sign and the engine would deny the fee.
func (b *StealthTransferBuilder) PayFeeFromRevealed() *StealthTransferBuilder {
	b.payFeeFromRevealed = true
	return b
}

// MinEpoch sets the earliest epoch this transfer is valid in.
func (b *StealthTransferBuilder) MinEpoch(epoch uint64) *StealthTransferBuilder {
	b.minEpoch = &epoch
	return b
}

// MaxEpoch sets the latest epoch this transfer is valid in.
func (b *StealthTransferBuilder) MaxEpoch(epoch uint64) *StealthTransferBuilder {
	b.maxEpoch = &epoch
	return b
}

// DryRun marks the transfer as a dry run (fee estimation without committing).
func (b *StealthTransferBuilder) DryRun() *StealthTransferBuilder {
	b.dryRun = true
	return b
}

// Intent validates the revealed-balance bucket and assembles the StealthTransferIntent.
//
// It enforces two input-validation guards before the core call (it computes no proof and no
// confidential balance — that stays in the core):
//   - the sum of every output's RevealedAmount must equal the declared revealed-output total
//     (ToRevealedOutput); a mismatch is a VALIDATION error naming both totals;
//   - a confidential-input spend with no revealed input must set PayFeeFromRevealed, or the
//     account never signs the fee — a missing flag is an actionable VALIDATION error.
func (b *StealthTransferBuilder) Intent(fee uint64) (StealthTransferIntent, error) {
	var perOutputRevealed uint64
	for _, o := range b.outputs {
		perOutputRevealed += o.RevealedAmount
	}
	if perOutputRevealed != b.revealedOutput {
		return StealthTransferIntent{}, &Error{
			Code: "VALIDATION",
			Message: fmt.Sprintf(
				"revealed-output mismatch: per-output revealed amounts sum to %d but the declared revealed-output total is %d",
				perOutputRevealed, b.revealedOutput,
			),
		}
	}
	if len(b.inputs) > 0 && b.revealedInput == 0 && !b.payFeeFromRevealed {
		return StealthTransferIntent{}, &Error{
			Code:    "VALIDATION",
			Message: "confidential-input spend with no revealed input must set PayFeeFromRevealed so the account key signs the fee",
		}
	}

	return StealthTransferIntent{
		FromAccount:          b.from,
		ResourceAddress:      b.resource,
		Fee:                  fee,
		Inputs:               b.inputs,
		Outputs:              b.outputs,
		RevealedInputAmount:  b.revealedInput,
		RevealedOutputAmount: b.revealedOutput,
		MinEpoch:             b.minEpoch,
		MaxEpoch:             b.maxEpoch,
		DryRun:               b.dryRun,
		PayFeeFromRevealed:   b.payFeeFromRevealed,
	}, nil
}
