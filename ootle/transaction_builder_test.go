package ootle

import "testing"

// sameTxJSON asserts two generic intents marshal to byte-identical core JSON.
func sameTxJSON(t *testing.T, got, want GenericTransactionIntent) {
	t.Helper()
	gj, err := got.marshalIntent()
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wj, err := want.marshalIntent()
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gj) != string(wj) {
		t.Fatalf("intent mismatch:\n got: %s\nwant: %s", gj, wj)
	}
}

// withdraw → workspace → deposit shape.
func TestTransactionBuilder_WorkspaceChain(t *testing.T) {
	got := NewTransaction().
		PayFeeFromAccount("component_payer", 100).
		CallMethod("component_payer", "withdraw", ArgAddress("resource_tari"), ArgAmount(500)).
		SaveOutput("bucket").
		CallMethodOnWorkspace("dest", "deposit", ArgWorkspace("bucket")).
		Intent()

	want := GenericTransactionIntent{
		Fee:        100,
		FeePayment: FeeFromAccount("component_payer"),
		Instructions: []InstructionSpec{
			CallMethod("component_payer", "withdraw", ArgAddress("resource_tari"), ArgAmount(500)),
			PutLastInstructionOutputOnWorkspace("bucket"),
			CallMethodOnWorkspace("dest", "deposit", ArgWorkspace("bucket")),
		},
	}
	sameTxJSON(t, got, want)

	if len(got.Inputs) != 0 {
		t.Fatalf("builder must leave Inputs empty, got %d", len(got.Inputs))
	}
}

func TestTransactionBuilder_PublishTemplateWithFeeInstruction(t *testing.T) {
	blob := NewBlob([]byte{0x01, 0x02})
	got := NewTransaction().
		PayFeeFromWorkspaceComponent("payer", 200).
		FeeInstruction(
			CallMethod("component_src", "withdraw", ArgAmount(200)),
			PutLastInstructionOutputOnWorkspace("payer"),
		).
		Blob(blob).
		PublishTemplate(0).
		MaxEpoch(42).
		DryRun().
		Intent()

	want := GenericTransactionIntent{
		Fee:        200,
		FeePayment: FeeFromWorkspaceComponent("payer"),
		FeeInstructions: []InstructionSpec{
			CallMethod("component_src", "withdraw", ArgAmount(200)),
			PutLastInstructionOutputOnWorkspace("payer"),
		},
		Instructions: []InstructionSpec{PublishTemplate(0)},
		Blobs:        []BlobSpec{blob},
	}
	want.MaxEpoch = 42
	want.DryRun = true

	sameTxJSON(t, got, want)
}

func TestTransactionBuilder_ExtraInputAndCallFunction(t *testing.T) {
	ref := InputRef{SubstateID: "component_dep"}
	got := NewTransaction().
		PayFeeFromBucket("fee_bucket", 10).
		CallFunction("template_x", "instantiate", ArgString("hi")).
		CreateAccount("aa11", WithOwnerRule(OwnerOwnedBySigner())).
		ExtraInput(ref).
		MinEpoch(5).
		Intent()

	want := GenericTransactionIntent{
		Fee:        10,
		FeePayment: FeeFromBucket("fee_bucket"),
		Instructions: []InstructionSpec{
			CallFunction("template_x", "instantiate", ArgString("hi")),
			CreateAccount("aa11", WithOwnerRule(OwnerOwnedBySigner())),
		},
		ExtraInputs: []InputRef{ref},
	}
	want.MinEpoch = ptrU64(5)

	sameTxJSON(t, got, want)

	if len(got.Inputs) != 0 {
		t.Fatalf("builder must leave Inputs empty, got %d", len(got.Inputs))
	}
}

func ptrU64(v uint64) *uint64 { return &v }

func TestFaucetBuilder_MinEpochAndDryRun(t *testing.T) {
	intent := Faucet(XtrFaucetComponentAddress).
		Take("fea0").
		MinEpoch(3).
		MaxEpoch(12).
		DryRun().
		Intent(1500)
	if intent.faucetClaim == nil {
		t.Fatal("expected faucetClaim to be set")
	}
	if intent.faucetClaim.MinEpoch == nil || *intent.faucetClaim.MinEpoch != 3 {
		t.Fatalf("MinEpoch = %v, want 3", intent.faucetClaim.MinEpoch)
	}
	if intent.faucetClaim.MaxEpoch != 12 {
		t.Fatalf("MaxEpoch = %d, want 12", intent.faucetClaim.MaxEpoch)
	}
	if !intent.faucetClaim.DryRun {
		t.Fatal("expected DryRun to be true")
	}
}

func TestBuilders_IntentClonesSlicesAndMinEpoch(t *testing.T) {
	txBuilder := NewTransaction().
		PayFeeFromAccount("component_payer", 100).
		CallMethod("component_payer", "noop").
		ExtraInput(InputRef{SubstateID: "component_dep"}).
		MinEpoch(7)
	first := txBuilder.Intent()

	// Mutating the returned intent or continuing to build must not mutate first.
	*first.MinEpoch = 99
	first.Instructions[0] = CallMethod("component_other", "mutated")
	txBuilder.CallMethod("component_payer", "second").MinEpoch(15)

	second := txBuilder.Intent()
	if *second.MinEpoch != 15 {
		t.Fatalf("second.MinEpoch = %d, want 15", *second.MinEpoch)
	}
	if len(first.Instructions) != 1 {
		t.Fatalf("first.Instructions len = %d, want 1", len(first.Instructions))
	}

	trBuilder := NewTransfer("component_from").ToAccount("component_to").MinEpoch(4)
	trFirst := trBuilder.Intent()
	*trFirst.MinEpoch = 88
	if *trBuilder.Intent().MinEpoch != 4 {
		t.Fatalf("TransferBuilder MinEpoch mutated through returned Intent")
	}
}
