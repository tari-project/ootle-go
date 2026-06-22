package ootle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// This file adds the confidential (stealth) SEND path to the Go SDK. It mirrors the
// public-transfer surface in driver.go / ootle.go, threading the core's stealth C ABI
// (ootle_sdk.h, ABI ootle-sdk-ffi-c/4). All value-critical work — input decryption,
// want-derivation, vault/UTXO discovery, seeded proof generation, signing/sealing, and
// encoding — stays in the Rust core; this host only marshals JSON, fetches the substates the
// core asks for, and frees the opaque handle.
//
// # Stealth resolves exactly like the public two-phase flow
//
// Stealth inputs resolve through the SAME host-driven NeedMore { fetch_ids } convergence
// loop the public path uses: build (handle + want list) →
// fetch → apply → (NeedMore? fetch the returned fetch_ids and repeat) → resolved → seal →
// submit → wait → parse. The build does not take the input UTXOs up front; the core hands
// back the concrete UTXO ids to fetch in the first apply's fetch_ids (the host derives no
// substate ids). The shared helpers (collectFetchIDs, parseResolution, hexToBase64,
// waitResult, cffi.ParseFinalizedResult) and the FinalizedResult / EncodedPublicTransfer types
// are reused from the public path unchanged.

// StealthTransferInput is one stealth UTXO to spend. The commitment + owner account public
// key identify the on-chain UTXO substate; the spend secret (a view-only secret able to
// decrypt that owner's UTXOs) lets the core recover the spend mask. The spend secret is
// supplied here for ergonomics but crosses the C ABI as a SEPARATE positional array
// (spend_secrets_json), one per input — it is NOT part of the intent's input struct.
//
// UtxoSubstateID is the canonical substate id of the UTXO (utxo_<resource>_<commitment>) the
// driver fetches before building. The caller — who created or received the UTXO — knows it;
// supplying it keeps the host thin (no engine-id derivation here).
type StealthTransferInput struct {
	// Commitment is the 32-byte Pedersen commitment of the UTXO, lowercase hex.
	Commitment string
	// OwnerAccountPublicKey is the owner account public key whose view secret decrypts this
	// UTXO, lowercase hex (matches the WantItem::StealthUtxo owner_account_pk).
	OwnerAccountPublicKey string
	// SpendSecret is the view-only secret scalar that decrypts this input's spend mask,
	// lowercase hex. It crosses the boundary positionally in spend_secrets_json.
	SpendSecret string
	// UtxoSubstateID is the canonical UTXO substate id to fetch (utxo_<resource>_<commitment>).
	UtxoSubstateID string
}

// stealthInputSpec is the wire mirror of the core's StealthInputSpec (one element of
// intent.inputs). It carries only the commitment + owner account pk — the spend secret is
// sent separately. snake_case field names match types/stealth.rs::StealthInputSpec.
type stealthInputSpec struct {
	Commitment     string `json:"commitment"`
	OwnerAccountPK string `json:"owner_account_pk"`
}

// StealthOutputSpec is one confidential output. It mirrors the core's StealthOutputSpec
// serde shape exactly (types/stealth.rs): snake_case fields, hex byte strings, u64 amounts.
// Optional fields use *T with a plain json tag (NOT omitempty): the core has no
// #[serde(default)] on them, so the key must be present — nil marshals to null, which the
// core accepts.
type StealthOutputSpec struct {
	// DestinationAccountPublicKey is the recipient's account public key, lowercase hex.
	DestinationAccountPublicKey string `json:"destination_account_pk"`
	// DestinationViewPublicKey is the recipient's view public key, lowercase hex.
	DestinationViewPublicKey string `json:"destination_view_pk"`
	// Amount is the blinded (confidential) output value in µTari.
	Amount uint64 `json:"amount"`
	// RevealedAmount is the revealed (plaintext) value in µTari deposited into the recipient's
	// account, like a normal public transfer. The core has #[serde(default)] on this field, so
	// omitempty is safe: a zero value (no revealed deposit) is omitted and defaults to 0. The
	// per-output revealed amounts must sum to the intent's RevealedOutputAmount.
	RevealedAmount uint64 `json:"revealed_amount,omitempty"`
	// ResourceAddress is the resource of this output (resource_<hex>).
	ResourceAddress string `json:"resource_address"`
	// ResourceViewKey, when set (lowercase hex), drives the ElGamal viewable-balance proof.
	ResourceViewKey *string `json:"resource_view_key"`
	// Memo is an optional encrypted memo, one of the StealthMemo variants (Message/Bytes).
	Memo *StealthMemo `json:"memo"`
	// PayTo selects the spend-condition for the output ("StealthPublicKey" default, or
	// "AccessRuleAllowAll"). Defaults to StealthPublicKey when empty.
	PayTo StealthPayTo `json:"pay_to"`
	// UtxoTag is an optional 4-byte tag, lowercase hex (8 chars, little-endian u32).
	UtxoTag *string `json:"utxo_tag"`
	// MinimumValuePromise is the minimum value promise (range-proof lower bound).
	MinimumValuePromise uint64 `json:"minimum_value_promise"`
}

// StealthPayTo is the spend-condition selector for a stealth output. It marshals as the
// bare serde unit-variant string the core expects.
type StealthPayTo string

const (
	// PayToStealthPublicKey locks the output to the recipient's one-time stealth public key
	// (the default, privacy-preserving spend condition).
	PayToStealthPublicKey StealthPayTo = "StealthPublicKey"
	// PayToAccessRuleAllowAll makes the output spendable by anyone (no stealth lock).
	PayToAccessRuleAllowAll StealthPayTo = "AccessRuleAllowAll"
)

// StealthMemo is the optional output memo: exactly one of Message (a UTF-8 string) or Bytes
// (raw bytes). It marshals to the core's externally-tagged StealthMemo enum:
//
//	{"Message": "hello"}   or   {"Bytes": [1, 2, 3]}
type StealthMemo struct {
	Message *string
	Bytes   []byte
}

// MessageMemo builds a text memo.
func MessageMemo(s string) *StealthMemo { return &StealthMemo{Message: &s} }

// BytesMemo builds a raw-bytes memo.
func BytesMemo(b []byte) *StealthMemo { return &StealthMemo{Bytes: b} }

// MarshalJSON emits the externally-tagged StealthMemo enum. The Bytes variant serializes as
// a JSON array of byte numbers (the core's Vec<u8> default), matching types/stealth.rs.
func (m StealthMemo) MarshalJSON() ([]byte, error) {
	switch {
	case m.Message != nil && m.Bytes != nil:
		return nil, &Error{Code: "ENCODING", Message: "StealthMemo has both Message and Bytes set"}
	case m.Message != nil:
		return json.Marshal(map[string]string{"Message": *m.Message})
	case m.Bytes != nil:
		nums := make([]int, len(m.Bytes))
		for i, b := range m.Bytes {
			nums[i] = int(b)
		}
		return json.Marshal(map[string][]int{"Bytes": nums})
	default:
		return nil, &Error{Code: "ENCODING", Message: "StealthMemo has neither Message nor Bytes set"}
	}
}

// UnmarshalJSON decodes the externally-tagged StealthMemo enum form.
func (m *StealthMemo) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["Message"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return err
		}
		m.Message = &s
		return nil
	}
	if v, ok := raw["Bytes"]; ok {
		var b []byte
		if err := json.Unmarshal(v, &b); err != nil {
			return err
		}
		m.Bytes = b
		return nil
	}
	return &Error{Code: "ENCODING", Message: "StealthMemo JSON has neither Message nor Bytes"}
}

// StealthTransferIntent is the developer-facing description of a confidential transfer. It
// mirrors the core's StealthTransferIntent serde shape exactly (types/stealth.rs). Amounts
// are µTari (u64).
//
// Inputs are stealth UTXOs to spend (empty for a revealed-only transfer); the driver fetches
// each one's substate before building. RevealedInputAmount / RevealedOutputAmount carry the
// (non-confidential) revealed input/output bucket amounts.
type StealthTransferIntent struct {
	FromAccount          string                 `json:"from_account"`
	ResourceAddress      string                 `json:"resource_address"`
	Fee                  uint64                 `json:"fee"`
	Inputs               []StealthTransferInput `json:"-"`
	Outputs              []StealthOutputSpec    `json:"outputs"`
	RevealedInputAmount  uint64                 `json:"revealed_input_amount"`
	RevealedOutputAmount uint64                 `json:"revealed_output_amount"`
	MinEpoch             *uint64                `json:"min_epoch"`
	MaxEpoch             *uint64                `json:"max_epoch"`
	DryRun               bool                   `json:"dry_run"`
	// PayFeeFromRevealed pays the fee from the from-account's revealed (XTR) vault even when there is
	// no revealed input. The fee is always charged from FromAccount via pay_fee, which only the
	// account key can authorize; that key seals automatically when RevealedInputAmount > 0. A pure
	// confidential-input spend has no revealed input, so without this the account never signs and the
	// engine denies the fee — set this to force the account-key seal. The fee stays out of the balance
	// proof (the confidential inputs/outputs balance on their own).
	PayFeeFromRevealed bool `json:"pay_fee_from_revealed"`
}

// intentWire is the exact JSON shape the core deserializes: Inputs become the on-wire
// stealthInputSpec list (commitment + owner pk only — the spend secret is sent separately).
type intentWire struct {
	FromAccount          string              `json:"from_account"`
	ResourceAddress      string              `json:"resource_address"`
	Fee                  uint64              `json:"fee"`
	Inputs               []stealthInputSpec  `json:"inputs"`
	Outputs              []StealthOutputSpec `json:"outputs"`
	RevealedInputAmount  uint64              `json:"revealed_input_amount"`
	RevealedOutputAmount uint64              `json:"revealed_output_amount"`
	MinEpoch             *uint64             `json:"min_epoch"`
	MaxEpoch             *uint64             `json:"max_epoch"`
	DryRun               bool                `json:"dry_run"`
	PayFeeFromRevealed   bool                `json:"pay_fee_from_revealed"`
}

// MarshalJSON emits the core's StealthTransferIntent shape, splitting the Go-side Inputs
// into the on-wire input specs (the spend secrets are carried out-of-band; see
// spendSecrets).
func (i StealthTransferIntent) MarshalJSON() ([]byte, error) {
	w := intentWire{
		FromAccount:          i.FromAccount,
		ResourceAddress:      i.ResourceAddress,
		Fee:                  i.Fee,
		Inputs:               make([]stealthInputSpec, len(i.Inputs)),
		Outputs:              i.Outputs,
		RevealedInputAmount:  i.RevealedInputAmount,
		RevealedOutputAmount: i.RevealedOutputAmount,
		MinEpoch:             i.MinEpoch,
		MaxEpoch:             i.MaxEpoch,
		DryRun:               i.DryRun,
		PayFeeFromRevealed:   i.PayFeeFromRevealed,
	}
	if w.Outputs == nil {
		w.Outputs = []StealthOutputSpec{}
	}
	for n, in := range i.Inputs {
		w.Inputs[n] = stealthInputSpec{Commitment: in.Commitment, OwnerAccountPK: in.OwnerAccountPublicKey}
	}
	return json.Marshal(w)
}

// spendSecrets returns the per-input spend secrets, positional per intent.Inputs, ready to
// marshal into the C ABI's spend_secrets_json array. Empty for a revealed-only transfer.
func (i StealthTransferIntent) spendSecrets() []string {
	secrets := make([]string, len(i.Inputs))
	for n, in := range i.Inputs {
		secrets[n] = in.SpendSecret
	}
	return secrets
}

// utxoSubstateIDs returns the canonical UTXO substate ids the driver must fetch before the
// core build, one per intent.Inputs. The caller supplies them (no engine-id derivation here —
// the host stays thin). Every stealth input MUST carry a non-empty UtxoSubstateID: the core
// expects a fetched UTXO for each input, so a blank id is rejected as a VALIDATION error
// up front rather than surfacing as a cryptic core resolution failure later.
func (i StealthTransferIntent) utxoSubstateIDs() ([]string, error) {
	ids := make([]string, 0, len(i.Inputs))
	for n, in := range i.Inputs {
		if in.UtxoSubstateID == "" {
			return nil, &Error{
				Code:    "VALIDATION",
				Message: fmt.Sprintf("stealth input %d is missing UtxoSubstateID (the utxo_<resource>_<commitment> id to fetch)", n),
			}
		}
		ids = append(ids, in.UtxoSubstateID)
	}
	return ids, nil
}

// --- Deterministic / reproducible-build API ---------------------------------------------
//
// The type and functions below pin a single build seed so the SIGNATURES the core produces are
// reproducible (the embedded bulletproof + viewable-balance proofs are not byte-stable). The core
// expands the seed into every nonce/mask it needs. Production callers want StealthProductionKeys
// plus the Client.SendStealthTransfer driver; reach for these only when you need signature parity
// or an offline one-shot build+encode.

// StealthTransferKeys is the seed-reproducible signing bundle; production callers should use
// StealthProductionKeys. It mirrors the C ABI's keys_json ({account_secret, seed}, all lowercase
// hex). The seed pins every derived nonce so the signatures reproduce byte-for-byte.
type StealthTransferKeys struct {
	AccountSecret string `json:"account_secret"`
	Seed          string `json:"seed,omitempty"`
}

// StealthProductionKeys is the production key bundle. On the production path the seed is drawn
// freshly from OsRng inside the core, so only AccountSecret need be set; the bundle keeps one shape,
// so this is an alias of StealthTransferKeys rather than a duplicate struct (Seed uses omitempty so
// a production caller omits it). The encoded bytes/id are not reproducible on the production path.
type StealthProductionKeys = StealthTransferKeys

// EncodedStealthTransfer is an alias for EncodedPublicTransfer: the stealth send path emits
// the same {encoded_transaction, transaction_id} wire shape as the public path (confirmed in
// stealth_abi.rs). Use either type interchangeably.
type EncodedStealthTransfer = EncodedPublicTransfer

// stealthSealFunc is the seal+encode core call for the stealth flow (random-nonce or
// seed-reproducible). It CONSUMES the stealth handle. Unlike the public sealFunc it also takes the
// network byte (the stealth partial does not carry it). The seed (deterministic path) travels inside
// keysJSON. Both cffi stealth seal wrappers satisfy this shape.
type stealthSealFunc func(h *cffi.StealthHandle, networkByte uint8, keysJSON string) (string, error)

// SendStealthTransfer drives a confidential transfer end-to-end against the indexer, using
// the production (random-entropy) seal: fetch input UTXOs → build unsigned (stealth handle)
// → seal+encode → submit → wait → parse. This is the path production callers want; the
// encoded bytes are NOT reproducible (random entropy).
//
// The opaque stealth handle is freed on every path (success, error, panic). Context
// cancellation propagates to the transport.
func (c *Client) SendStealthTransfer(ctx context.Context, intent StealthTransferIntent, keys StealthProductionKeys) (FinalizedResult, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return c.sendStealthTransfer(ctx, c.network, intent, string(keysJSON), "", true, cffi.SealAndEncodeStealthTransfer)
}

// SendStealthTransferDeterministic is the seed-reproducible counterpart of SendStealthTransfer;
// production callers should use SendStealthTransfer.
// With fixed fetched substates and a pinned build seed the SIGNATURES the core produces are
// reproducible; the embedded proofs (bulletproof + viewable-balance) are not byte-stable
// (stealth send vectors compare semantically). The seed travels in keys.Seed. Everything else
// (fetch, submit, wait, parse, handle lifetime) is identical to SendStealthTransfer.
func (c *Client) SendStealthTransferDeterministic(ctx context.Context, intent StealthTransferIntent, keys StealthTransferKeys) (FinalizedResult, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return c.sendStealthTransfer(ctx, c.network, intent, string(keysJSON), keys.Seed, false, cffi.SealAndEncodeStealthTransferWithSeed)
}

// sendStealthTransfer is the shared stealth driver for both key paths. It marshals the
// intent + spend secrets, seeds the stealth partial (want list, no inputs fetched), drives the
// host-driven NeedMore fetch loop (the same convergence the public path uses) until resolved, then
// seals + encodes, submits, waits and parses.
//
// production selects the build entry point (production draws a fresh OS-RNG seed and ignores
// seedHex; the seeded path pins every nonce from seedHex). seal is the handle-consuming
// seal+encode call.
//
// Handle lifetime: BuildStealthUnsigned* returns a stealth handle the driver owns.
// ApplyFetchedSubstatesStealth and seal CONSUME the handle (the cffi wrappers nil the *StealthHandle
// even on error, returning the next one). The deferred guard frees whatever handle the driver
// currently owns; after a consuming call the driver re-points `handle` (or nils it on seal), so the
// guard never double-frees and always frees the live handle on every early return (fetch error,
// resolution failure, transport error) and panic.
func (c *Client) sendStealthTransfer(ctx context.Context, network Network, intent StealthTransferIntent, keysJSON, seedHex string, production bool, seal stealthSealFunc) (result FinalizedResult, err error) {
	netByte, nErr := resolveNetworkByte(network)
	if nErr != nil {
		return FinalizedResult{}, nErr
	}

	intentJSON, mErr := json.Marshal(intent)
	if mErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal intent: %v", mErr)}
	}
	secretsJSON, sErr := json.Marshal(intent.spendSecrets())
	if sErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal spend secrets: %v", sErr)}
	}
	secrets := string(secretsJSON)

	// Validate the per-input UTXO ids up front so a missing id is a clean VALIDATION error rather
	// than a cryptic resolution failure deeper in the loop (the caller supplies them; the host
	// stays thin — no engine id derivation here).
	if _, idErr := intent.utxoSubstateIDs(); idErr != nil {
		return FinalizedResult{}, idErr
	}

	// --- Phase 1: seed the stealth resolver (handle + want list, no inputs fetched) ----------
	var handle *cffi.StealthHandle
	var wantListJSON string
	var bErr error
	if production {
		handle, wantListJSON, bErr = cffi.BuildStealthUnsigned(netByte, string(intentJSON))
	} else {
		handle, wantListJSON, bErr = cffi.BuildStealthUnsignedWithSeed(netByte, string(intentJSON), seedHex)
	}
	if bErr != nil {
		return FinalizedResult{}, fromCffiError(bErr)
	}
	// Free the live handle on every exit path (incl. panic). After a consuming call the driver
	// re-points `handle`, so this frees the live one, never a consumed one.
	defer func() { cffi.FreeStealthHandle(handle) }()

	// --- Phase 2: bounded host-driven resolution loop ---------------------------------------
	// Round 0's fetch set comes from the build want list's seeds (the stealth UTXO ids). Every
	// subsequent round fetches the concrete ids the core hands back in NeedMore (`fetch_ids`).
	// Mirrors sendPublicTransfer exactly.
	ids, idErr := collectFetchIDs(wantListJSON)
	if idErr != nil {
		return FinalizedResult{}, idErr
	}
	for round := 0; ; round++ {
		var fetched []transport.FetchedSubstate
		if len(ids) > 0 {
			fetched, err = c.transport.FetchSubstates(ctx, ids)
			if err != nil {
				return FinalizedResult{}, err
			}
		}
		// The core expects a JSON array; a nil slice marshals to `null`, which it rejects.
		if fetched == nil {
			fetched = []transport.FetchedSubstate{}
		}
		fetchedJSON, jErr := json.Marshal(fetched)
		if jErr != nil {
			return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal fetched substates: %v", jErr)}
		}

		// ApplyFetchedSubstatesStealth consumes `handle` (even on error) and returns the next
		// handle to thread forward. Re-point `handle` immediately so the deferred guard frees the
		// right one.
		next, resolutionJSON, aErr := cffi.ApplyFetchedSubstatesStealth(handle, netByte, string(fetchedJSON), secrets)
		handle = next // nil on error; the new handle on success
		if aErr != nil {
			return FinalizedResult{}, fromCffiError(aErr)
		}

		res, rErr := parseResolution(resolutionJSON)
		if rErr != nil {
			return FinalizedResult{}, rErr
		}
		if res.resolved {
			break
		}
		// NeedMore: prefer the concrete fetch_ids the core discovered; fall back to deriving from
		// the want list. Loop again, up to the cap.
		ids = res.fetchIDs
		if len(ids) == 0 {
			ids, idErr = collectFetchIDs(res.wantListJSON)
			if idErr != nil {
				return FinalizedResult{}, idErr
			}
		}
		if round+1 >= maxResolutionRounds {
			return FinalizedResult{}, &Error{
				Code:    "RESOLUTION",
				Message: fmt.Sprintf("%v (capped at %d rounds)", ErrResolutionDidNotConverge, maxResolutionRounds),
				cause:   ErrResolutionDidNotConverge,
			}
		}
	}

	// --- Phase 3: seal + encode (consumes the handle) ---------------------------------------
	encodedJSON, seErr := seal(handle, netByte, keysJSON)
	handle = nil // consumed; the guard must not free it
	if seErr != nil {
		return FinalizedResult{}, fromCffiError(seErr)
	}
	var encoded EncodedStealthTransfer
	if uErr := json.Unmarshal([]byte(encodedJSON), &encoded); uErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal encoded stealth transfer: %v", uErr)}
	}

	// --- Submit + wait for finalization + parse ---------------------------------------------
	// Route through the shared SSE-preferred / REST-fallback tail (see finalization.go) so
	// stealth gets the same finalization wait as the public/generic/cosign paths.
	envelopeB64, eErr := hexToBase64(encoded.EncodedTransaction)
	if eErr != nil {
		return FinalizedResult{}, eErr
	}
	return c.submitAndWait(ctx, envelopeB64)
}

// BuildAndEncodeStealthTransfer is the one-shot seed-reproducible stealth send:
// it builds (resolves inputs + assembles) and seals/encodes in a single core call, with no
// transport involved — a reproducible offline one-shot for callers that drive their own
// submission. Production callers driving a transfer end-to-end want Client.SendStealthTransfer.
// The caller supplies the already-fetched input UTXO substates directly
// (positional resolution still happens in the core) and the build seed in keys.Seed. The output
// is reproducible for the signatures the core produces; the embedded proofs are not byte-stable.
// Errors carry the stable core code via *Error.
func BuildAndEncodeStealthTransfer(network Network, intent StealthTransferIntent, fetched []transport.FetchedSubstate, keys StealthTransferKeys) (EncodedStealthTransfer, error) {
	var out EncodedStealthTransfer

	netByte, ok := network.ByteValue()
	if !ok {
		return out, &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	// This is the seed-reproducible one-shot: a build seed is mandatory. Reject an empty seed up front
	// with a clear error rather than surfacing an opaque core parse failure. Drive the transfer
	// end-to-end with Client.SendStealthTransfer for the random path.
	if keys.Seed == "" {
		return out, &Error{Code: "VALIDATION", Message: "BuildAndEncodeStealthTransfer requires keys.Seed (the 32-byte build seed); use Client.SendStealthTransfer for the random path"}
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal intent: %v", err)}
	}
	if fetched == nil {
		fetched = []transport.FetchedSubstate{}
	}
	fetchedJSON, err := json.Marshal(fetched)
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal fetched substates: %v", err)}
	}
	secretsJSON, err := json.Marshal(intent.spendSecrets())
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal spend secrets: %v", err)}
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}

	dataJSON, cerr := cffi.BuildAndEncodeStealthTransferWithSeed(netByte, string(intentJSON), string(fetchedJSON), string(secretsJSON), string(keysJSON))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal result: %v", err)}
	}
	return out, nil
}

// --- stealth receive (scan) -------------------------------------------------------------------
//
// The receive path is the easy, RNG-free half of stealth: a caller holds an inbound UTXO (fetched
// from the indexer by any means) plus their view keys, and one stateless core call reveals whether
// the output is theirs and, if so, its value, mask, and memo. There is no transport, no driver
// loop, and no opaque handle — ScanStealthOutput is a pure free function, NOT a method on Client (a
// stateless operation does not belong on a transport-holding client). Scanning is byte-stable.

// StealthScanKeys is the receive-side key bundle. It mirrors the C ABI's scan_keys_json shape
// exactly ({view_secret, account_secret?, skip_memo?}); all secrets are lowercase hex.
//
// ViewSecret (required) is the view secret scalar paired with the sender's public nonce to
// re-derive the AEAD key and decrypt the output. AccountSecret (optional) enables the ownership
// checks — when supplied the scanner verifies the output's spend-condition + UTXO tag are addressed
// to that account; when nil those checks are skipped (a successful decrypt alone is treated as
// is_mine). SkipMemo, when true, skips memo decoding (the value/mask are still recovered).
//
// AccountSecret uses omitempty so a nil omits the key entirely, which the core's #[serde(default)]
// Option accepts (the common view-only-scan case). SkipMemo likewise uses omitempty so the common
// (false) case omits the key, also satisfying the core's #[serde(default)].
type StealthScanKeys struct {
	// ViewSecret is the view secret scalar (32 bytes, lowercase hex). Required.
	ViewSecret string `json:"view_secret"`
	// AccountSecret is the optional account secret (32 bytes, lowercase hex) enabling ownership
	// (spend-condition + tag) verification. Nil/absent ⇒ those checks are skipped.
	AccountSecret *string `json:"account_secret,omitempty"`
	// SkipMemo, when true, skips memo decryption/decoding. Defaults to false.
	SkipMemo bool `json:"skip_memo,omitempty"`
}

// InboundStealthOutput is one inbound stealth UTXO to scan — the on-the-wire shape of a created
// stealth output, reduced to the fields the receiver needs to decrypt + claim it. It mirrors the
// core's InboundStealthOutput serde shape exactly (types/stealth.rs): snake_case fields, lowercase
// hex byte strings. The caller fetched this from the indexer and passes it through; the host does
// no crypto on it.
//
// Optional fields (SpendPublicKey, UtxoTag) use *string with plain json tags (no omitempty): the
// core's Option fields have no #[serde(default)], so the keys must be present — nil marshals to
// null, which the core accepts.
type InboundStealthOutput struct {
	// Commitment is the on-chain Pedersen commitment (32 bytes, lowercase hex).
	Commitment string `json:"commitment"`
	// EncryptedData is the AEAD-encrypted output payload (variable length, lowercase hex).
	EncryptedData string `json:"encrypted_data"`
	// SenderPublicNonce is the sender's ephemeral public nonce R (32 bytes, lowercase hex); the
	// receiver pairs it with their view secret to re-derive the AEAD key.
	SenderPublicNonce string `json:"sender_public_nonce"`
	// PayTo is the spend condition controlling one-time key derivation (StealthPublicKey or
	// AccessRuleAllowAll). Reuses the send-side StealthPayTo selector.
	PayTo StealthPayTo `json:"pay_to"`
	// SpendPublicKey is the on-chain one-time spend public key for a StealthPublicKey output
	// (lowercase hex). Nil for AccessRuleAllowAll. Verified against the account when AccountSecret
	// is supplied.
	SpendPublicKey *string `json:"spend_public_key"`
	// UtxoTag is the optional 4-byte UTXO scanning tag (lowercase hex, 8 chars). Verified against
	// the receiver-derived tag when AccountSecret is supplied.
	UtxoTag *string `json:"utxo_tag"`
	// ResourceAddress is the resource being transferred (resource_<hex>), needed to re-derive the
	// scanning tag.
	ResourceAddress string `json:"resource_address"`
}

// DecryptedOutput is the receive-side result of scanning an inbound stealth UTXO. It mirrors the
// core's DecryptedOutput serde shape (types/stealth.rs) plus the not-mine sentinel the C ABI emits.
//
// IsMine is the only field that is always meaningful: true means the output belongs to the scanning
// keys and Value/Mask (and Memo, unless SkipMemo) are populated; false means the output is not
// theirs and Value is 0, Mask is "", Memo is nil. The core never returns an error to signal
// not-mine — a failed decrypt is mapped to IsMine=false. Memo reuses the externally-tagged
// StealthMemo (the same type the send path uses); it is nil when absent or when SkipMemo was set.
type DecryptedOutput struct {
	// IsMine reports whether the output is addressed to the scanning keys. See the type doc.
	IsMine bool `json:"is_mine"`
	// Value is the recovered plaintext value in µTari (0 when not mine). No omitempty: the core
	// always serializes this field and a mine output's value may legitimately be 0, so the key
	// must survive a re-marshal.
	Value uint64 `json:"value"`
	// Mask is the recovered Pedersen commitment mask (secret scalar, lowercase hex; "" when not
	// mine). No omitempty: the core always serializes it (see Value).
	Mask string `json:"mask"`
	// Memo is the decoded memo, if present and SkipMemo was false; nil otherwise. Keeps omitempty:
	// the core's Option<StealthMemo> omits None.
	Memo *StealthMemo `json:"memo,omitempty"`
}

// ScanStealthOutput decrypts an inbound stealth UTXO with the caller's scan keys. It is a pure,
// stateless call — no transport, no driver loop, no opaque handle.
//
// On success it returns a NON-NIL *DecryptedOutput: IsMine=true means the output belongs to these
// keys and Value/Mask/Memo are populated; IsMine=false means the output is not theirs (Value=0,
// Mask=""). Callers MUST distinguish ownership via IsMine, NOT by checking for nil — a nil return
// only ever accompanies a non-nil error. An error is returned only on a structural failure (bad
// network, malformed keys/output, or an internal core fault), carrying the stable core code.
func ScanStealthOutput(network Network, scanKeys StealthScanKeys, output InboundStealthOutput) (*DecryptedOutput, error) {
	netByte, ok := network.ByteValue()
	if !ok {
		return nil, &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	scanKeysJSON, err := json.Marshal(scanKeys)
	if err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal scan keys: %v", err)}
	}
	outputJSON, err := json.Marshal(output)
	if err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal output: %v", err)}
	}

	dataJSON, cerr := cffi.ScanStealthOutput(netByte, string(scanKeysJSON), string(outputJSON))
	if cerr != nil {
		return nil, fromCffiError(cerr)
	}

	var out DecryptedOutput
	if uerr := json.Unmarshal([]byte(dataJSON), &out); uerr != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal decrypted output: %v", uerr)}
	}
	return &out, nil
}

// ScanStealthOutputs scans a slice of inbound UTXOs and returns only those that are mine (a thin
// loop over ScanStealthOutput). It short-circuits on the first structural error. Outputs that are
// not mine are silently dropped — only IsMine=true results are returned.
func ScanStealthOutputs(network Network, scanKeys StealthScanKeys, outputs []InboundStealthOutput) ([]*DecryptedOutput, error) {
	var mine []*DecryptedOutput
	for _, out := range outputs {
		d, err := ScanStealthOutput(network, scanKeys, out)
		if err != nil {
			return nil, err
		}
		if d.IsMine {
			mine = append(mine, d)
		}
	}
	return mine, nil
}

// DecodeStealthUTXO decodes a fetched UTXO substate (the shape the indexer returns) into the
// receive-shaped InboundStealthOutput the scanner consumes. It is a pure, stateless call — the
// single shared UTXO decode lives in the Rust core (the spend path reuses the same extraction).
//
// substateID is the UTXO's canonical address string (utxo_<resource>_<commitment>) — it carries the
// on-chain commitment + resource address the value body omits. substateValue is the SubstateValue
// JSON the indexer returned (transport.FetchedSubstate.SubstateValue), passed through verbatim; the
// host does NO crypto/CBOR on it. An error carries the stable core code (PARSE for a malformed id /
// value, INVALID for a non-UTXO / frozen / burnt substate, KEY for a malformed nonce).
func DecodeStealthUTXO(substateID string, substateValue json.RawMessage) (InboundStealthOutput, error) {
	var out InboundStealthOutput
	idJSON, err := json.Marshal(substateID)
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal substate id: %v", err)}
	}

	dataJSON, cerr := cffi.DecodeStealthUTXO(string(idJSON), string(substateValue))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if uerr := json.Unmarshal([]byte(dataJSON), &out); uerr != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal inbound output: %v", uerr)}
	}
	return out, nil
}

// ScanStealthSubstate fuses DecodeStealthUTXO and ScanStealthOutput: it decodes a fetched UTXO
// substate and scans it with the caller's scan keys in one core call (one shared decode + one
// RNG-free scan). It is a pure, stateless call.
//
// substateID / substateValue are as in DecodeStealthUTXO. On success it returns a NON-NIL
// *DecryptedOutput: IsMine=true means the output belongs to these keys (Value/Mask/Memo populated);
// IsMine=false means it is not theirs (Value=0, Mask=""). Callers MUST distinguish ownership via
// IsMine, NOT by checking for nil — a nil return only ever accompanies a non-nil error. An error is
// returned only on a structural failure (bad network, malformed keys / id / value), carrying the
// stable core code.
func ScanStealthSubstate(network Network, scanKeys StealthScanKeys, substateID string, substateValue json.RawMessage) (*DecryptedOutput, error) {
	netByte, ok := network.ByteValue()
	if !ok {
		return nil, &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	scanKeysJSON, err := json.Marshal(scanKeys)
	if err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal scan keys: %v", err)}
	}
	idJSON, err := json.Marshal(substateID)
	if err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal substate id: %v", err)}
	}

	dataJSON, cerr := cffi.ScanStealthSubstate(netByte, string(scanKeysJSON), string(idJSON), string(substateValue))
	if cerr != nil {
		return nil, fromCffiError(cerr)
	}
	var out DecryptedOutput
	if uerr := json.Unmarshal([]byte(dataJSON), &out); uerr != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal decrypted output: %v", uerr)}
	}
	return &out, nil
}
