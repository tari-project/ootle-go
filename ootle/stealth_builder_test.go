package ootle

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestNewStealthOutput_Defaults(t *testing.T) {
	o := NewStealthOutput("destacct", "destview", 1000, "resource_tari")

	if o.DestinationAccountPublicKey != "destacct" || o.DestinationViewPublicKey != "destview" {
		t.Fatalf("destination fields not set: %+v", o)
	}
	if o.Amount != 1000 {
		t.Fatalf("Amount = %d, want 1000", o.Amount)
	}
	if o.ResourceAddress != "resource_tari" {
		t.Fatalf("ResourceAddress = %q, want resource_tari", o.ResourceAddress)
	}
	if o.PayTo != PayToStealthPublicKey {
		t.Fatalf("PayTo = %q, want %q", o.PayTo, PayToStealthPublicKey)
	}
}

func TestStealthOutput_WithOptionsChain(t *testing.T) {
	memo := MessageMemo("hi")
	o := NewStealthOutput("a", "v", 5, "resource_tari").
		WithRevealed(2).
		WithMemo(memo).
		WithResourceViewKey("cafe").
		WithUtxoTag("deadbeef").
		WithMinimumValuePromise(3).
		WithPayTo(PayToAccessRuleAllowAll)

	if o.RevealedAmount != 2 {
		t.Fatalf("RevealedAmount = %d, want 2", o.RevealedAmount)
	}
	if o.Memo != memo {
		t.Fatalf("Memo not set")
	}
	if o.ResourceViewKey == nil || *o.ResourceViewKey != "cafe" {
		t.Fatalf("ResourceViewKey = %v, want cafe", o.ResourceViewKey)
	}
	if o.UtxoTag == nil || *o.UtxoTag != "deadbeef" {
		t.Fatalf("UtxoTag = %v, want deadbeef", o.UtxoTag)
	}
	if o.MinimumValuePromise != 3 {
		t.Fatalf("MinimumValuePromise = %d, want 3", o.MinimumValuePromise)
	}
	if o.PayTo != PayToAccessRuleAllowAll {
		t.Fatalf("PayTo = %q, want %q", o.PayTo, PayToAccessRuleAllowAll)
	}
}

func TestStealthTransferBuilder_RevealedSums(t *testing.T) {
	intent, err := NewStealthTransfer("component_from", "resource_tari").
		SpendRevealedInput(1000).
		SpendRevealedInput(500).
		ToStealthOutput("recipacct", "recipview", 800).
		ToOutput(NewStealthOutput("selfacct", "selfview", 0, "resource_tari").WithRevealed(700)).
		ToRevealedOutput(700).
		Intent(50)
	if err != nil {
		t.Fatalf("Intent: %v", err)
	}

	if intent.RevealedInputAmount != 1500 {
		t.Fatalf("RevealedInputAmount = %d, want 1500", intent.RevealedInputAmount)
	}
	if intent.RevealedOutputAmount != 700 {
		t.Fatalf("RevealedOutputAmount = %d, want 700", intent.RevealedOutputAmount)
	}
	if intent.Fee != 50 {
		t.Fatalf("Fee = %d, want 50", intent.Fee)
	}
	if len(intent.Outputs) != 2 {
		t.Fatalf("len(Outputs) = %d, want 2", len(intent.Outputs))
	}
	if len(intent.Inputs) != 0 {
		t.Fatalf("len(Inputs) = %d, want 0 (revealed-only)", len(intent.Inputs))
	}
}

func TestStealthTransferBuilder_RevealedMismatchError(t *testing.T) {
	_, err := NewStealthTransfer("component_from", "resource_tari").
		ToOutput(NewStealthOutput("a", "v", 0, "resource_tari").WithRevealed(300)).
		ToRevealedOutput(500). // declares 500 but per-output sums to 300
		Intent(10)
	if err == nil {
		t.Fatal("expected a VALIDATION error on revealed-output mismatch")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "VALIDATION" {
		t.Fatalf("error = %v, want *Error{Code:VALIDATION}", err)
	}
}

func TestStealthTransferBuilder_PayFeeFromRevealedGuard(t *testing.T) {
	utxo := InboundStealthOutput{Commitment: "abc123", ResourceAddress: "resource_tari"}

	// Confidential input, no revealed input, flag unset ⇒ VALIDATION error.
	_, err := NewStealthTransfer("component_from", "resource_tari").
		SpendUTXO(utxo, "owneracct", "spendsecret", "utxo_resource_tari_abc123").
		ToStealthOutput("recipacct", "recipview", 1000).
		Intent(5)
	if err == nil {
		t.Fatal("expected a VALIDATION error when PayFeeFromRevealed is unset on a confidential spend")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != "VALIDATION" {
		t.Fatalf("error = %v, want *Error{Code:VALIDATION}", err)
	}

	// Same chain with the flag set ⇒ ok.
	intent, err := NewStealthTransfer("component_from", "resource_tari").
		SpendUTXO(utxo, "owneracct", "spendsecret", "utxo_resource_tari_abc123").
		ToStealthOutput("recipacct", "recipview", 1000).
		PayFeeFromRevealed().
		Intent(5)
	if err != nil {
		t.Fatalf("Intent with PayFeeFromRevealed: %v", err)
	}
	if !intent.PayFeeFromRevealed {
		t.Fatal("PayFeeFromRevealed flag not set on the intent")
	}
}

func TestStealthTransferBuilder_SpendUTXOStructured(t *testing.T) {
	utxo := InboundStealthOutput{
		Commitment:      "feedface",
		ResourceAddress: "resource_tari",
	}
	intent, err := NewStealthTransfer("component_from", "resource_tari").
		SpendUTXO(utxo, "owneracct", "spendsecret", "utxo_resource_tari_feedface").
		ToStealthOutput("recipacct", "recipview", 2000).
		PayFeeFromRevealed().
		Intent(5)
	if err != nil {
		t.Fatalf("Intent: %v", err)
	}

	if len(intent.Inputs) != 1 {
		t.Fatalf("len(Inputs) = %d, want 1", len(intent.Inputs))
	}
	in := intent.Inputs[0]
	if in.Commitment != "feedface" {
		t.Fatalf("commitment = %q, want feedface (pulled from the decoded UTXO)", in.Commitment)
	}
	if in.OwnerAccountPublicKey != "owneracct" || in.SpendSecret != "spendsecret" {
		t.Fatalf("input key context not set: %+v", in)
	}
	if in.UtxoSubstateID != "utxo_resource_tari_feedface" {
		t.Fatalf("UtxoSubstateID = %q", in.UtxoSubstateID)
	}

	// The on-wire intent carries commitment + owner pk (spend secret stays out-of-band).
	wire, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	var got struct {
		Inputs []struct {
			Commitment     string `json:"commitment"`
			OwnerAccountPK string `json:"owner_account_pk"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	if len(got.Inputs) != 1 {
		t.Fatalf("wire inputs = %d, want 1", len(got.Inputs))
	}
	if got.Inputs[0].Commitment != "feedface" || got.Inputs[0].OwnerAccountPK != "owneracct" {
		t.Fatalf("wire input = %+v", got.Inputs[0])
	}
	if bytes.Contains(wire, []byte("spendsecret")) {
		t.Fatalf("spend secret value leaked into the intent JSON: %s", wire)
	}
}

func TestStealthTransferBuilder_DryRunAndEpochs(t *testing.T) {
	intent, err := NewStealthTransfer("component_from", "resource_tari").
		ToStealthOutput("recipacct", "recipview", 1000).
		SpendRevealedInput(1000).
		MinEpoch(3).
		MaxEpoch(9).
		DryRun().
		Intent(5)
	if err != nil {
		t.Fatalf("Intent: %v", err)
	}
	if !intent.DryRun {
		t.Fatal("DryRun() did not set the flag")
	}
	if intent.MinEpoch == nil || *intent.MinEpoch != 3 {
		t.Fatalf("MinEpoch = %v, want 3", intent.MinEpoch)
	}
	if intent.MaxEpoch == nil || *intent.MaxEpoch != 9 {
		t.Fatalf("MaxEpoch = %v, want 9", intent.MaxEpoch)
	}
}
