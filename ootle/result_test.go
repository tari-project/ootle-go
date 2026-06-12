package ootle

import "testing"

func TestPublicTransferIntent_AsDryRun(t *testing.T) {
	base := PublicTransferIntent{FromAccount: "acct", Amount: 100}
	dry := base.AsDryRun()
	if !dry.DryRun {
		t.Fatalf("AsDryRun did not set DryRun")
	}
	if base.DryRun {
		t.Fatalf("AsDryRun mutated the receiver")
	}
	if dry.FromAccount != base.FromAccount || dry.Amount != base.Amount {
		t.Fatalf("AsDryRun did not preserve other fields: %+v", dry)
	}
}

func TestGenericTransactionIntent_AsDryRun(t *testing.T) {
	base := GenericTransactionIntent{Fee: 500}
	dry := base.AsDryRun()
	if !dry.DryRun {
		t.Fatalf("AsDryRun did not set DryRun")
	}
	if base.DryRun {
		t.Fatalf("AsDryRun mutated the receiver")
	}
	if dry.Fee != base.Fee {
		t.Fatalf("AsDryRun did not preserve other fields: %+v", dry)
	}
}

func TestGenericTransactionIntent_AsDryRun_FaucetClaimIndependent(t *testing.T) {
	base := Faucet("").Take("deadbeef").Intent(500)
	if base.faucetClaim == nil {
		t.Fatalf("faucet intent has nil faucetClaim")
	}
	dry := base.AsDryRun()
	if dry.faucetClaim == base.faucetClaim {
		t.Fatalf("AsDryRun did not deep-copy faucetClaim")
	}
	if base.faucetClaim.DryRun {
		t.Fatalf("AsDryRun mutated the receiver's faucetClaim")
	}
}

func TestFinalizedResult_OutcomeAccessors(t *testing.T) {
	reason := &RejectReason{Code: "EXECUTION_FAILURE", Message: "boom"}

	pending := FinalizedResult{}
	if !pending.Pending() {
		t.Fatalf("pending: Pending()=false")
	}
	if pending.IsCommit() {
		t.Fatalf("pending: IsCommit()=true")
	}
	if pending.RejectReason() != nil {
		t.Fatalf("pending: RejectReason()=%+v", pending.RejectReason())
	}

	commit := FinalizedResult{Submit: SubmitResult{Outcome: &TransactionOutcome{Commit: true}}}
	if commit.Pending() {
		t.Fatalf("commit: Pending()=true")
	}
	if !commit.IsCommit() {
		t.Fatalf("commit: IsCommit()=false")
	}
	if commit.RejectReason() != nil {
		t.Fatalf("commit: RejectReason()=%+v", commit.RejectReason())
	}

	reject := FinalizedResult{Submit: SubmitResult{Outcome: &TransactionOutcome{Reject: reason}}}
	if reject.IsCommit() {
		t.Fatalf("reject: IsCommit()=true")
	}
	if reject.RejectReason() != reason {
		t.Fatalf("reject: RejectReason()=%+v want %+v", reject.RejectReason(), reason)
	}

	feeOnly := FinalizedResult{Submit: SubmitResult{Outcome: &TransactionOutcome{OnlyFeeCommit: reason}}}
	if feeOnly.IsCommit() {
		t.Fatalf("onlyFeeCommit: IsCommit()=true")
	}
	if feeOnly.RejectReason() != reason {
		t.Fatalf("onlyFeeCommit: RejectReason()=%+v want %+v", feeOnly.RejectReason(), reason)
	}
}

func TestFinalizedResult_EstimatedFeeOr(t *testing.T) {
	fee := uint64(42)
	withFee := FinalizedResult{EstimatedFee: &fee}
	if got := withFee.EstimatedFeeOr(7); got != 42 {
		t.Fatalf("EstimatedFeeOr with estimate = %d, want 42", got)
	}
	var noFee FinalizedResult
	if got := noFee.EstimatedFeeOr(7); got != 7 {
		t.Fatalf("EstimatedFeeOr without estimate = %d, want 7", got)
	}
}

func TestDiffSummary_FirstUp(t *testing.T) {
	diff := &DiffSummary{Up: []UpSubstate{
		{SubstateID: "component_aaa"},
		{SubstateID: "component_bbb"},
		{SubstateID: "template_ccc"},
		{SubstateID: "utxo_ddd"},
	}}

	if id, ok := diff.NewComponent(); !ok || id != "component_aaa" {
		t.Fatalf("NewComponent = %q,%v want component_aaa,true", id, ok)
	}
	if id, ok := diff.NewComponent("component_aaa"); !ok || id != "component_bbb" {
		t.Fatalf("NewComponent excluding aaa = %q,%v want component_bbb,true", id, ok)
	}
	if id, ok := diff.NewTemplate(); !ok || id != "template_ccc" {
		t.Fatalf("NewTemplate = %q,%v want template_ccc,true", id, ok)
	}
	if id, ok := diff.NewUTXO(); !ok || id != "utxo_ddd" {
		t.Fatalf("NewUTXO = %q,%v want utxo_ddd,true", id, ok)
	}
	if id, ok := diff.FirstUp(PrefixResource); ok || id != "" {
		t.Fatalf("FirstUp(resource) = %q,%v want \"\",false", id, ok)
	}

	var nilDiff *DiffSummary
	if id, ok := nilDiff.FirstUp(PrefixComponent); ok || id != "" {
		t.Fatalf("nil FirstUp = %q,%v want \"\",false", id, ok)
	}
}
