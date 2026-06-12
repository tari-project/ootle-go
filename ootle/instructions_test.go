package ootle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// These tests prove the InstructionSpec/ArgValue mirror marshals to the core's tagged enum form, the
// host builders compose (never encode), and ootle_build_unsigned_instructions drives the apply/seal
// surface to the committed bytes.

// --- arg / instruction marshalling (the host mirrors the core's tagged enum; encodes nothing) --------

func TestArgValueMarshalsToTaggedEnum(t *testing.T) {
	cases := []struct {
		arg  ArgValue
		want string
	}{
		{ArgAmount(1000), `{"Amount":1000}`},
		{ArgAddress("resource_72"), `{"Address":"resource_72"}`},
		{ArgWorkspace("bucket"), `{"Workspace":"bucket"}`},
		{ArgString("withdraw"), `{"String":"withdraw"}`},
		{ArgBool(true), `{"Bool":true}`},
		{ArgU64(42), `{"U64":42}`},
		{ArgBytes([]byte{0xde, 0xad, 0xbe, 0xef}), `{"Bytes":"deadbeef"}`},
		{ArgI64(-5), `{"I64":-5}`},
		{ArgI64(42), `{"I64":42}`},
		{ArgNonFungibleID("u32_7"), `{"NonFungibleId":"u32_7"}`},
		{ArgNonFungibleID(NonFungibleU64(42)), `{"NonFungibleId":"u64_42"}`},
		{ArgAddress("vault_dd"), `{"Address":"vault_dd"}`}, // the widened kind still marshals identically
		{ArgList(ArgU64(1), ArgU64(2)), `{"List":[{"U64":1},{"U64":2}]}`},
		{ArgList(), `{"List":[]}`}, // empty, not null
		{ArgList(ArgList(ArgU64(1))), `{"List":[{"List":[{"U64":1}]}]}`},
		{ArgList(ArgNonFungibleID(NonFungibleU32(7))), `{"List":[{"NonFungibleId":"u32_7"}]}`},
		{ArgSome(ArgU64(7)), `{"Optional":{"U64":7}}`},
		{ArgNone(), `{"Optional":null}`},
		{ArgSome(ArgList(ArgAddress("component_71"))), `{"Optional":{"List":[{"Address":"component_71"}]}}`}, // nesting
	}
	for _, c := range cases {
		got, err := json.Marshal(c.arg)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.arg, err)
		}
		if string(got) != c.want {
			t.Errorf("ArgValue marshal: got %s, want %s", got, c.want)
		}
	}

	// An empty ArgValue (no constructor used) must error rather than silently emit a wrong shape.
	if _, err := json.Marshal(ArgValue{}); err == nil {
		t.Error("an unset ArgValue must fail to marshal")
	}
}

// TestNonFungibleIDHelpers proves each canonical-string builder produces the exact form the core's parser
// accepts.
func TestNonFungibleIDHelpers(t *testing.T) {
	if got := NonFungibleU32(7); got != "u32_7" {
		t.Errorf("NonFungibleU32(7): got %q, want %q", got, "u32_7")
	}
	if got := NonFungibleU64(42); got != "u64_42" {
		t.Errorf("NonFungibleU64(42): got %q, want %q", got, "u64_42")
	}
	if got := NonFungibleString("abc"); got != "str_abc" {
		t.Errorf("NonFungibleString(\"abc\"): got %q, want %q", got, "str_abc")
	}
	want := "uuid_0102" + strings.Repeat("00", 30)
	if got := NonFungibleUUID([32]byte{0x01, 0x02}); got != want {
		t.Errorf("NonFungibleUUID: got %q, want %q", got, want)
	}
}

func TestInstructionSpecMarshalsToTaggedEnum(t *testing.T) {
	cases := []struct {
		instr InstructionSpec
		want  string
	}{
		{
			CallFunction("aa", "take_free_coins"),
			`{"CallFunction":{"template_address":"aa","function":"take_free_coins","args":[]}}`,
		},
		{
			CallMethod("component_71", "withdraw", ArgAddress("resource_72"), ArgAmount(1000000)),
			`{"CallMethod":{"call":{"Address":"component_71"},"method":"withdraw","args":[{"Address":"resource_72"},{"Amount":1000000}]}}`,
		},
		{
			CallMethodOnWorkspace("acct", "take", ArgWorkspace("acct")),
			`{"CallMethod":{"call":{"Workspace":"acct"},"method":"take","args":[{"Workspace":"acct"}]}}`,
		},
		{
			CreateAccount("fe"),
			`{"CreateAccount":{"owner_public_key":"fe","owner_rule":null,"bucket_workspace_id":null}}`,
		},
		{
			CreateAccount("fe", WithOwnerRule(OwnerByPublicKey("ab")), WithBucket("b")),
			`{"CreateAccount":{"owner_public_key":"fe","owner_rule":{"ByPublicKey":"ab"},"bucket_workspace_id":"b"}}`,
		},
		{
			PublishTemplate(0),
			`{"PublishTemplate":{"blob_index":0,"metadata_hash":null}}`,
		},
		{
			PublishTemplate(0, WithMetadataHash([]byte{0xde, 0xad})),
			`{"PublishTemplate":{"blob_index":0,"metadata_hash":"dead"}}`,
		},
		{
			PutLastInstructionOutputOnWorkspace("bucket"),
			`{"PutLastInstructionOutputOnWorkspace":{"key":"bucket"}}`,
		},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.instr)
		if err != nil {
			t.Fatalf("marshal instruction: %v", err)
		}
		if string(got) != c.want {
			t.Errorf("InstructionSpec marshal: got %s, want %s", got, c.want)
		}
	}
}

func TestFeeSourceMarshalsToTaggedEnum(t *testing.T) {
	cases := []struct {
		src  FeeSource
		want string
	}{
		{FeeFromAccount("component_71"), `{"FromAccount":"component_71"}`},
		{FeeFromWorkspaceComponent("acct"), `{"FromWorkspaceComponent":{"label":"acct"}}`},
		{FeeFromBucket("b"), `{"FromBucket":{"label":"b"}}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.src)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.src, err)
		}
		if string(got) != c.want {
			t.Errorf("FeeSource marshal: got %s, want %s", got, c.want)
		}
	}
	if _, err := json.Marshal(FeeSource{}); err == nil {
		t.Error("an unset FeeSource must fail to marshal")
	}
}

func TestOwnerRuleSpecMarshalsToTaggedEnum(t *testing.T) {
	cases := []struct {
		rule OwnerRuleSpec
		want string
	}{
		{OwnerOwnedBySigner(), `"OwnedBySigner"`},
		{OwnerNone(), `"None"`},
		{OwnerByPublicKey("ab"), `{"ByPublicKey":"ab"}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.rule)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.rule, err)
		}
		if string(got) != c.want {
			t.Errorf("OwnerRuleSpec marshal: got %s, want %s", got, c.want)
		}
	}
	if _, err := json.Marshal(OwnerRuleSpec{}); err == nil {
		t.Error("an unset OwnerRuleSpec must fail to marshal")
	}
}

// --- host builders compose the right InstructionSpec sequence ----------------------------------------

// TestFaucetTakeRoutesToCoreFaucetClaim asserts Faucet().Take().Intent() carries a FaucetClaimIntent
// (the core faucet builder path) with the recipient + fee pinned and no host-composed instructions or
// explicit inputs. The faucetComponent passed to Faucet() is ignored on this path.
func TestFaucetTakeRoutesToCoreFaucetClaim(t *testing.T) {
	intent := Faucet(XtrFaucetComponentAddress).Take("fea0").Intent(2000)
	if len(intent.Instructions) != 0 || len(intent.FeeInstructions) != 0 {
		t.Errorf("Take() must compose no instructions, got %d fee / %d main", len(intent.FeeInstructions), len(intent.Instructions))
	}
	if len(intent.Inputs) != 0 {
		t.Errorf("Take().Intent() must leave Inputs empty (resolved path), got %d", len(intent.Inputs))
	}
	if intent.faucetClaim == nil {
		t.Fatal("Take().Intent() must carry a faucet claim")
	}
	if intent.faucetClaim.RecipientPublicKey != "fea0" || intent.faucetClaim.Fee != 2000 {
		t.Errorf("faucet claim: got recipient=%q fee=%d, want fea0/2000", intent.faucetClaim.RecipientPublicKey, intent.faucetClaim.Fee)
	}
}

// TestFaucetTakeFreeCoinsDepositComposesTestTemplateSequence asserts the test-faucet shape:
// take_free_coins → put-on-workspace → deposit(Workspace).
func TestFaucetTakeFreeCoinsDepositComposesTestTemplateSequence(t *testing.T) {
	faucet := "component_99"
	account := "component_71"
	got := Faucet(faucet).TakeFreeCoins("free").Deposit(account, "free").Instructions()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal faucet instructions: %v", err)
	}
	want := `[{"CallMethod":{"call":{"Address":"component_99"},"method":"take_free_coins","args":[]}},` +
		`{"PutLastInstructionOutputOnWorkspace":{"key":"free"}},` +
		`{"CallMethod":{"call":{"Address":"component_71"},"method":"deposit","args":[{"Workspace":"free"}]}}]`
	if string(gotJSON) != want {
		t.Errorf("Faucet().TakeFreeCoins().Deposit() sequence:\n got:  %s\n want: %s", gotJSON, want)
	}
}

// --- the generic entry point reuses the apply/seal surface (no new lifecycle) ------------------------

// genericFixture mirrors a committed generic_build/* vector (the generic_intent is held raw and handed
// to the C ABI verbatim — the host never re-encodes args).
type genericFixture struct {
	Operation string `json:"operation"`
	Input     struct {
		Network       Network                     `json:"network"`
		Keys          DeterministicTransferKeys   `json:"keys"`
		Fetched       []transport.FetchedSubstate `json:"fetched"`
		GenericIntent json.RawMessage             `json:"generic_intent"`
		FaucetIntent  json.RawMessage             `json:"faucet_intent"`
	} `json:"input"`
	Expected struct {
		EncodedTransaction string `json:"encoded_transaction"`
		TransactionID      string `json:"transaction_id"`
	} `json:"expected"`
}

func loadGenericFixture(t *testing.T, rel string) genericFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var f genericFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	return f
}

// TestBuildUnsignedInstructionsHandleDrivesExistingApplySeal proves the generic build's handle is
// interchangeable with the public path: BuildUnsignedInstructions → ApplyFetchedSubstates →
// SealAndEncode reproduces the committed vector byte-for-byte. There are no generic apply/seal wrappers
// — the generic entry point's handle is a public handle.
func TestBuildUnsignedInstructionsHandleDrivesExistingApplySeal(t *testing.T) {
	fx := loadGenericFixture(t, "generic_build/call_method_transfer.json")
	if fx.Operation != opBuildAndEncodeInstructions {
		t.Fatalf("unexpected fixture operation %q", fx.Operation)
	}
	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("unknown network %q", fx.Input.Network)
	}
	keysJSON, err := json.Marshal(fx.Input.Keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}

	handle, _, err := cffi.BuildUnsignedInstructions(netByte, string(fx.Input.GenericIntent))
	if err != nil {
		t.Fatalf("BuildUnsignedInstructions: %v", err)
	}
	defer func() { cffi.FreeHandle(handle) }()

	next, resJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	handle = next
	if err != nil {
		t.Fatalf("ApplyFetchedSubstates: %v", err)
	}
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		t.Fatalf("expected resolved, got %q", res.Status)
	}

	encodedJSON, err := cffi.SealAndEncodeWithSeed(handle, string(keysJSON))
	handle = nil
	if err != nil {
		t.Fatalf("SealAndEncode: %v", err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	if encoded.EncodedTransaction != fx.Expected.EncodedTransaction {
		t.Errorf("encoded_transaction mismatch:\n got:  %s\n want: %s", encoded.EncodedTransaction, fx.Expected.EncodedTransaction)
	}
	if encoded.TransactionID != fx.Expected.TransactionID {
		t.Errorf("transaction_id mismatch:\n got:  %s\n want: %s", encoded.TransactionID, fx.Expected.TransactionID)
	}
}

// TestFaucetTakeIntentReproducesGoldenVector proves the Go-builder-assembled faucet claim — not just the
// raw fixture JSON — is accepted by the core and reproduces the committed faucet_claim vector
// byte-for-byte. It assembles via Faucet().Take().Intent(), asserts the builder's FaucetClaimIntent
// matches the fixture's faucet_intent shape, then drives BuildFaucetClaim → ApplyFetchedSubstates →
// SealAndEncodeWithSeed.
func TestFaucetTakeIntentReproducesGoldenVector(t *testing.T) {
	fx := loadGenericFixture(t, "generic_build/faucet_claim.json")

	// Values mirror generic_build/faucet_claim.json.
	const ownerPK = "fea009ee8681783f3e9ed6152d3b7fc204a7ba78cde9808cdedae3dd221af013"
	intent := Faucet(XtrFaucetComponentAddress).Take(ownerPK).Intent(2000)
	if intent.faucetClaim == nil {
		t.Fatal("Take().Intent() must carry a faucet claim")
	}
	claimJSON, err := json.Marshal(intent.faucetClaim)
	if err != nil {
		t.Fatalf("marshal faucet claim: %v", err)
	}

	// The Go builder must emit the canonical faucet_intent shape.
	var gotShape, wantShape any
	if uerr := json.Unmarshal(claimJSON, &gotShape); uerr != nil {
		t.Fatalf("unmarshal built claim: %v", uerr)
	}
	if uerr := json.Unmarshal(fx.Input.FaucetIntent, &wantShape); uerr != nil {
		t.Fatalf("unmarshal fixture faucet_intent: %v", uerr)
	}
	if !reflect.DeepEqual(gotShape, wantShape) {
		t.Errorf("built claim shape diverges from fixture:\n got:  %s\n want: %s", claimJSON, fx.Input.FaucetIntent)
	}

	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("unknown network %q", fx.Input.Network)
	}
	keysJSON, err := json.Marshal(fx.Input.Keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}

	handle, _, err := cffi.BuildFaucetClaim(netByte, string(claimJSON))
	if err != nil {
		t.Fatalf("BuildFaucetClaim: %v", err)
	}
	defer func() { cffi.FreeHandle(handle) }()

	next, resJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	handle = next
	if err != nil {
		t.Fatalf("ApplyFetchedSubstates: %v", err)
	}
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		t.Fatalf("expected resolved, got %q", res.Status)
	}

	encodedJSON, err := cffi.SealAndEncodeWithSeed(handle, string(keysJSON))
	handle = nil
	if err != nil {
		t.Fatalf("SealAndEncode: %v", err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	if encoded.EncodedTransaction != fx.Expected.EncodedTransaction {
		t.Errorf("encoded_transaction mismatch:\n got:  %s\n want: %s", encoded.EncodedTransaction, fx.Expected.EncodedTransaction)
	}
	if encoded.TransactionID != fx.Expected.TransactionID {
		t.Errorf("transaction_id mismatch:\n got:  %s\n want: %s", encoded.TransactionID, fx.Expected.TransactionID)
	}
}

// TestBuildUnsignedInstructionsBadIntentIsParseError proves a malformed intent surfaces the stable
// "PARSE" code over the ABI, not a crash.
func TestBuildUnsignedInstructionsBadIntentIsParseError(t *testing.T) {
	_, _, err := cffi.BuildUnsignedInstructions(0x26 /* esmeralda */, "{ not valid json")
	if err == nil {
		t.Fatal("malformed intent must error")
	}
	var ce *cffi.Error
	if !errors.As(err, &ce) || ce.Code != "PARSE" {
		t.Fatalf("expected a PARSE cffi.Error, got %v", err)
	}
}

// TestSendInstructionsRejectsExplicitInputs proves the generic driver rejects a non-empty Inputs (which
// would short-circuit the core's explicit path) with a VALIDATION error — mirroring SendPublicTransfer.
func TestSendInstructionsRejectsExplicitInputs(t *testing.T) {
	c := NewClient(&mockTransport{}, WithNetwork(NetworkEsmeralda))
	version := uint32(0)
	intent := GenericTransactionIntent{
		Fee:          2000,
		FeePayment:   FeeFromAccount("component_71"),
		Instructions: []InstructionSpec{CallMethod("component_71", "noop")},
		Inputs:       []InputRef{{SubstateID: "component_71", Version: &version}},
	}
	_, err := c.SendInstructions(context.Background(), intent, PublicTransferKeys{AccountSecret: "65"})
	if err == nil {
		t.Fatal("a non-empty Inputs must be rejected")
	}
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "VALIDATION" {
		t.Fatalf("expected a VALIDATION *Error, got %v", err)
	}
}
