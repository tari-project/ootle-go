package ootle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// stealthFixture mirrors a committed stealth_transfer/* vector (semantic compare for send: the
// expected output is a decoded sealed_transaction_semantic, not byte-stable hex). The driver
// tests only exercise the boundary + pipeline; the full semantic-parity runner lives in
// golden_vectors_test.go.
type stealthFixture struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Compare   string `json:"compare"`
	Input     struct {
		Network Network                     `json:"network"`
		Fetched []transport.FetchedSubstate `json:"fetched"`
		Intent  stealthIntentFixture        `json:"stealth_intent"`
		Keys    StealthTransferKeys         `json:"stealth_keys"`
		// SpendSecrets is the positional per-input spend-secret array (separate from the
		// intent, per the C ABI). Empty for a revealed-only transfer.
		SpendSecrets []string `json:"spend_secrets"`
	} `json:"input"`
	Expected struct {
		// SealedTransactionSemantic is the decoded, semantically-compared expected tx.
		SealedTransactionSemantic json.RawMessage `json:"sealed_transaction_semantic"`
	} `json:"expected"`
}

// stealthIntentFixture mirrors the fixture's stealth_intent shape (inputs carry only
// commitment + owner_account_pk, exactly the on-wire StealthInputSpec). The loader zips it
// with SpendSecrets + derives UTXO ids to build the Go-facing StealthTransferIntent.
type stealthIntentFixture struct {
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
}

// toIntent reconstructs the Go-facing StealthTransferIntent from the fixture, zipping the
// on-wire inputs with the positional spend secrets and the fetched substate ids (the fetched
// list is positional with the inputs in these vectors).
func (f stealthFixture) toIntent(t *testing.T) StealthTransferIntent {
	t.Helper()
	inputs := make([]StealthTransferInput, len(f.Input.Intent.Inputs))
	for i, in := range f.Input.Intent.Inputs {
		secret := ""
		if i < len(f.Input.SpendSecrets) {
			secret = f.Input.SpendSecrets[i]
		}
		utxoID := ""
		if i < len(f.Input.Fetched) {
			utxoID = f.Input.Fetched[i].SubstateID
		}
		inputs[i] = StealthTransferInput{
			Commitment:            in.Commitment,
			OwnerAccountPublicKey: in.OwnerAccountPK,
			SpendSecret:           secret,
			UtxoSubstateID:        utxoID,
		}
	}
	return StealthTransferIntent{
		FromAccount:          f.Input.Intent.FromAccount,
		ResourceAddress:      f.Input.Intent.ResourceAddress,
		Fee:                  f.Input.Intent.Fee,
		Inputs:               inputs,
		Outputs:              f.Input.Intent.Outputs,
		RevealedInputAmount:  f.Input.Intent.RevealedInputAmount,
		RevealedOutputAmount: f.Input.Intent.RevealedOutputAmount,
		MinEpoch:             f.Input.Intent.MinEpoch,
		MaxEpoch:             f.Input.Intent.MaxEpoch,
		DryRun:               f.Input.Intent.DryRun,
	}
}

func loadStealthFixture(t *testing.T, rel string) stealthFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "stealth_transfer", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read stealth fixture %s: %v", path, err)
	}
	var f stealthFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal stealth fixture %s: %v", path, err)
	}
	if f.Operation != "build_and_encode_stealth_transfer" {
		t.Fatalf("unexpected stealth fixture operation %q", f.Operation)
	}
	return f
}

// seedStealthAccount adds the from-account component + its vault to a fixture-seeded fetch index,
// as the indexer hands them back. The live stealth driver resolves them — the fee (and any
// revealed-input withdraw) reference the account — so a mock transport must serve them or
// resolution fails with "component/vault not found". All stealth_transfer fixtures share the same
// from_account (component_aaaa…) and resource (resource_0101…); the component references vault_cccc…
// which holds that resource.
func seedStealthAccount(index map[string]transport.FetchedSubstate) {
	const componentID = "component_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const vaultID = "vault_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	index[componentID] = transport.FetchedSubstate{
		SubstateID:    componentID,
		SubstateValue: json.RawMessage(`{"Component":{"body":{"state":[{"@cbor":"tag","tag":132,"value":{"@cbor":"bytes","hex":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}]},"header":{"access_rules":{"default":"DenyAll","method_access":{}},"entity_id":"00","owner_rule":"None","template_address":"0000000000000000000000000000000000000000000000000000000000000000"}}}`),
	}
	index[vaultID] = transport.FetchedSubstate{
		SubstateID:    vaultID,
		SubstateValue: json.RawMessage(`{"Vault":{"freeze_flags":0,"resource_container":{"Fungible":{"address":"resource_0101010101010101010101010101010101010101010101010101010101010101","amount":"1000000","locked_amount":"0"}}}}`),
	}
}

// (a) ABI version guard — fails immediately if a stale lib is linked or ExpectedABIVersion does
// not match the version exposing the current entry-point set (the seed-based, random-default ABI).
func TestStealthABIVersionIsCurrent(t *testing.T) {
	const want = "ootle-sdk-ffi-c/16"
	if got := cffi.ABIVersion(); got != want {
		t.Fatalf("ABI version = %q, want %s (rebuild via make native)", got, want)
	}
	// Sanity: the public-package accessor agrees.
	if ABIVersion() != want {
		t.Fatalf("ootle.ABIVersion() = %q, want %s", ABIVersion(), want)
	}
}

// (b) cffi wrapper smoke — drive the one-shot seed-reproducible stealth build+encode with a
// seed-pinned fixture and assert it round-trips the boundary (no error, non-empty bytes).
// Full semantic parity lives in golden_vectors_test.go; this just proves the C boundary
// marshals correctly.
func TestBuildAndEncodeStealthTransfer_Smoke(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	encoded, err := BuildAndEncodeStealthTransfer(f.Input.Network, intent, f.Input.Fetched, f.Input.Keys)
	if err != nil {
		t.Fatalf("BuildAndEncodeStealthTransfer: %v", err)
	}
	if encoded.EncodedTransaction == "" {
		t.Error("encoded_transaction is empty")
	}
	if encoded.TransactionID == "" {
		t.Error("transaction_id is empty")
	}
}

// (b3) ValidateStealthTransfer over the C ABI: a freshly sealed transfer canonicalizes (decode +
// verify-all-sigs succeed, the byte-unstable null set is zeroed, signer public keys survive). A
// flipped trailing byte (the seal signature scalar) is REJECTED — a hard error, not a falsy ok —
// mirroring the Rust tamper test. Decode + Schnorr verify run in the core; Go only marshals.
func TestValidateStealthTransfer_GoodAndTampered(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	encoded, err := BuildAndEncodeStealthTransfer(f.Input.Network, intent, f.Input.Fetched, f.Input.Keys)
	if err != nil {
		t.Fatalf("BuildAndEncodeStealthTransfer: %v", err)
	}
	netByte, ok := f.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("unknown network %q", f.Input.Network)
	}

	// Good seal → canonical JSON, the null set zeroed, no error.
	canonical, verr := cffi.ValidateStealthTransfer(netByte, encoded.EncodedTransaction)
	if verr != nil {
		t.Fatalf("ValidateStealthTransfer(good seal): %v", verr)
	}
	var canon map[string]any
	if uerr := json.Unmarshal([]byte(canonical), &canon); uerr != nil {
		t.Fatalf("canonical JSON does not unmarshal: %v", uerr)
	}

	// Tampered seal → rejected with a hard error (VALIDATION or ENCODING), never a falsy success.
	hexRunes := []rune(encoded.EncodedTransaction)
	last := len(hexRunes) - 1
	if hexRunes[last] == '0' {
		hexRunes[last] = 'f'
	} else {
		hexRunes[last] = '0'
	}
	if _, terr := cffi.ValidateStealthTransfer(netByte, string(hexRunes)); terr == nil {
		t.Fatal("a tampered seal must be rejected, but ValidateStealthTransfer returned no error")
	} else if cerr, isErr := terr.(*cffi.Error); isErr {
		if cerr.Code != "VALIDATION" && cerr.Code != "ENCODING" {
			t.Errorf("tampered seal: error code = %q, want VALIDATION or ENCODING", cerr.Code)
		}
	}
}

// (b2) cffi wrapper smoke — revealed-only transfer (no stealth inputs ⇒ empty fetched /
// spend_secrets). Proves the empty-input path marshals and round-trips.
func TestBuildAndEncodeStealthTransfer_RevealedOnly(t *testing.T) {
	f := loadStealthFixture(t, "account_key_seal_with_revealed_input.json")
	intent := f.toIntent(t)
	if len(intent.Inputs) != 0 {
		t.Fatalf("expected a revealed-only fixture with no stealth inputs, got %d", len(intent.Inputs))
	}

	encoded, err := BuildAndEncodeStealthTransfer(f.Input.Network, intent, nil, f.Input.Keys)
	if err != nil {
		t.Fatalf("BuildAndEncodeStealthTransfer (revealed only): %v", err)
	}
	if encoded.EncodedTransaction == "" || encoded.TransactionID == "" {
		t.Error("revealed-only transfer produced empty encoded bytes / id")
	}
}

// (c) Mock-transport integration — drive SendStealthTransfer end-to-end against a
// mock transport seeded from a stealth-input vector. Asserts: no error; the driver fetched the
// input UTXO substate id; it submitted a non-empty base64 envelope; and the parsed
// FinalizedResult is a Commit.
func TestSendStealthTransfer_MockTransport(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	// Index the fixture's fetched substates by id so the mock serves them by request.
	index := make(map[string]transport.FetchedSubstate, len(f.Input.Fetched))
	for _, s := range f.Input.Fetched {
		index[s.SubstateID] = s
	}
	seedStealthAccount(index)
	wantUtxoID := f.Input.Fetched[0].SubstateID

	// A real finalized-result + its expected parse, from a committed parse vector (the driver
	// routes the raw result through the core's parser).
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	var fetchedIDs []string
	var gotEnvelope string
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			fetchedIDs = append(fetchedIDs, ids...)
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if s, ok := index[id]; ok {
					out = append(out, s)
				}
			}
			return out, nil
		},
		submit: func(_ context.Context, envelopeB64 string) (string, error) {
			gotEnvelope = envelopeB64
			return "stealthtxid", nil
		},
		result: func(_ context.Context, _ string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	result, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("SendStealthTransfer: %v", err)
	}

	// The driver fetched exactly the input UTXO substate id (derived from the intent input,
	// supplied by the caller — no engine derivation in the host).
	foundUtxo := false
	for _, id := range fetchedIDs {
		if id == wantUtxoID {
			foundUtxo = true
		}
	}
	if !foundUtxo {
		t.Errorf("driver did not fetch the input UTXO id %q; fetched=%v", wantUtxoID, fetchedIDs)
	}
	// The submitted envelope is a non-empty, valid base64 string (the core produced the bytes;
	// stealth send compares semantically, not byte-for-byte).
	if gotEnvelope == "" {
		t.Fatal("driver submitted an empty envelope")
	}
	if _, derr := base64.StdEncoding.DecodeString(gotEnvelope); derr != nil {
		t.Errorf("submitted envelope is not valid base64: %v", derr)
	}
	if !result.Submit.Outcome.IsCommit() {
		t.Errorf("result outcome IsCommit() = false, want true")
	}
	// The driver routed the raw result through the core's parser and typed it identically to
	// the committed parse vector's expected.parsed.
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed FinalizedResult mismatch from committed parse vector")
	}
}

// TestSendStealthTransfer_UsesSSEGate proves the stealth send routes through the
// shared SSE-preferred wait: it opens the finalization stream and gates on the matching frame
// rather than going straight to the REST poll.
func TestSendStealthTransfer_UsesSSEGate(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	index := make(map[string]transport.FetchedSubstate, len(f.Input.Fetched))
	for _, s := range f.Input.Fetched {
		index[s.SubstateID] = s
	}
	seedStealthAccount(index)
	rawResult, _ := loadParseRaw(t, "accept.json")

	const txID = "stealthtxid"
	streamOpened := false
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if s, ok := index[id]; ok {
					out = append(out, s)
				}
			}
			return out, nil
		},
		submit: func(context.Context, string) (string, error) { return txID, nil },
		stream: func(context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			streamOpened = true
			out := make(chan transport.RawSSEEvent, 1)
			errs := make(chan error, 1)
			out <- finalizedFrame(txID)
			return out, errs
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	if _, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys); err != nil {
		t.Fatalf("SendStealthTransfer: %v", err)
	}
	if !streamOpened {
		t.Error("stealth send did not open the finalization stream — it bypassed the shared SSE wait")
	}
}

// (c2) Multi-round stealth resolution — drives the host-driven NeedMore { fetch_ids } loop over a
// stealth input. The build no longer takes the UTXO up front; the core hands back the concrete UTXO
// id in the first apply's fetch_ids, which the driver fetches in a later round. Asserts: the input
// UTXO is fetched (in a round AFTER the seed round) and the transfer completes as a Commit. This is
// the Go arm of the C-ABI multi-round convergence test.
func TestSendStealthTransfer_MultiRoundFetchLoop(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)
	if len(intent.Inputs) == 0 {
		t.Fatal("expected a fixture with at least one stealth input")
	}

	index := make(map[string]transport.FetchedSubstate, len(f.Input.Fetched))
	for _, s := range f.Input.Fetched {
		index[s.SubstateID] = s
	}
	seedStealthAccount(index)
	wantUtxoID := f.Input.Fetched[0].SubstateID

	rawResult, _ := loadParseRaw(t, "accept.json")

	// Record each fetch round's requested ids so we can assert the UTXO is fetched off fetch_ids
	// (the core hands it back), NOT supplied up front.
	var rounds [][]string
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			rounds = append(rounds, append([]string(nil), ids...))
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if s, ok := index[id]; ok {
					out = append(out, s)
				}
			}
			return out, nil
		},
		submit: func(_ context.Context, _ string) (string, error) { return "stealthtxid", nil },
		result: func(_ context.Context, _ string) (json.RawMessage, bool, error) { return rawResult, true, nil },
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	result, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("SendStealthTransfer: %v", err)
	}

	// The stealth UTXO want carries NO seed id in the want list (its id is the derived
	// utxo_<resource>_<commitment> address the core would have to compute), so collectFetchIDs yields
	// nothing for the seed round and the driver fetches the UTXO only once the core hands its id back
	// in the first apply's NeedMore.fetch_ids. Assert: every fetch round that ran requested exactly
	// the ids the core asked for (never an id the host derived itself), and the UTXO was among them.
	if got, _ := collectFetchIDs(`{"want_list":[{"kind":"stealth_utxo","commitment":"x","owner_account_pk":"y","resource_address":"r","required":true}]}`); len(got) != 0 {
		t.Fatalf("collectFetchIDs must derive NO id from a stealth_utxo want (the core hands it back via fetch_ids), got %v", got)
	}
	// The seed round (round 0) fetches the from-account component (the account wants' seed id), NOT
	// the UTXO: the stealth UTXO want carries no seed id, so the core hands its id back via
	// NeedMore.fetch_ids in a later round. Assert the UTXO is absent from the seed round and fetched
	// in a subsequent round — proving it came off fetch_ids, not a host-derived seed (a thin-host
	// violation).
	inRound := func(round int, want string) bool {
		if round >= len(rounds) {
			return false
		}
		for _, id := range rounds[round] {
			if id == want {
				return true
			}
		}
		return false
	}
	if inRound(0, wantUtxoID) {
		t.Errorf("the UTXO id %q must not appear in the seed round; rounds=%v", wantUtxoID, rounds)
	}
	fetchedUtxoLater := false
	for r := 1; r < len(rounds); r++ {
		if inRound(r, wantUtxoID) {
			fetchedUtxoLater = true
		}
	}
	if !fetchedUtxoLater {
		t.Fatalf("the input UTXO %q was never fetched via NeedMore fetch_ids; rounds=%v", wantUtxoID, rounds)
	}
	if !result.Submit.Outcome.IsCommit() {
		t.Errorf("result outcome IsCommit() = false, want true")
	}
}

// (d) Revealed-only send (no stealth inputs) still resolves the from-account: it withdraws the
// revealed input from the account and pays the fee from it, so the driver fetches the account
// component + its vault (never a stealth UTXO), declares them as inputs, and completes the pipeline.
func TestSendStealthTransfer_RevealedOnlyFetchesAccount(t *testing.T) {
	f := loadStealthFixture(t, "account_key_seal_with_revealed_input.json")
	intent := f.toIntent(t)
	if len(intent.Inputs) != 0 {
		t.Fatalf("expected a revealed-only fixture with no stealth inputs, got %d", len(intent.Inputs))
	}

	index := make(map[string]transport.FetchedSubstate)
	seedStealthAccount(index)
	rawResult, _ := loadParseRaw(t, "accept.json")
	var fetchedIDs []string
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			fetchedIDs = append(fetchedIDs, ids...)
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if s, ok := index[id]; ok {
					out = append(out, s)
				}
			}
			return out, nil
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	result, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("SendStealthTransfer (revealed only): %v", err)
	}
	// The from-account component is fetched (resolution declares it + its vault as inputs).
	foundComponent := false
	for _, id := range fetchedIDs {
		if id == intent.FromAccount {
			foundComponent = true
		}
	}
	if !foundComponent {
		t.Errorf("driver did not fetch the from-account %q; fetched=%v", intent.FromAccount, fetchedIDs)
	}
	if !result.Submit.Outcome.IsCommit() {
		t.Error("revealed-only transfer outcome IsCommit() = false, want true")
	}
}

// (e) Handle freed on the fetch-error path — inject a transport error on the first fetch
// (before the handle is even built; the handle is built AFTER the fetch in the stealth flow).
// The build never runs, so there is no handle to leak; this guards that a fetch error returns
// cleanly. A separate submit-error case (below) covers the post-build path.
func TestSendStealthTransfer_FetchErrorReturns(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	wantErr := errors.New("fetch boom")
	mock := &mockTransport{
		fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
			return nil, wantErr
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}

// (e2) Handle freed on the submit-error path — the handle is built and consumed by seal before
// submit, so the deferred guard frees nothing; a double-free here would crash the test binary.
// This guards the "error after a consuming call" path against a double-free / use-after-free.
func TestSendStealthTransfer_SubmitErrorFreesHandle(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)

	index := make(map[string]transport.FetchedSubstate, len(f.Input.Fetched))
	for _, s := range f.Input.Fetched {
		index[s.SubstateID] = s
	}
	seedStealthAccount(index)
	wantErr := errors.New("submit boom")
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if s, ok := index[id]; ok {
					out = append(out, s)
				}
			}
			return out, nil
		},
		submit: func(context.Context, string) (string, error) {
			return "", wantErr
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the submit error", err)
	}
}

// (f) Unknown-network rejection — a bad network is a VALIDATION error before any C call.
func TestSendStealthTransfer_UnknownNetwork(t *testing.T) {
	f := loadStealthFixture(t, "account_key_seal_with_revealed_input.json")
	intent := f.toIntent(t)
	mock := &mockTransport{fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
		t.Error("must not fetch on an unknown network")
		return nil, nil
	}}
	c := NewClient(mock, WithNetwork(Network("nope")))
	_, err := c.SendStealthTransfer(context.Background(), intent, StealthProductionKeys{AccountSecret: "00"})
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}
}

// (g) A stealth input missing its UtxoSubstateID is a VALIDATION error before any fetch or C
// call — the core expects a fetched UTXO per input, so the host rejects the gap up front.
func TestSendStealthTransfer_MissingUtxoIDRejected(t *testing.T) {
	f := loadStealthFixture(t, "stealth_seal_with_input.json")
	intent := f.toIntent(t)
	intent.Inputs[0].UtxoSubstateID = "" // drop the id

	mock := &mockTransport{fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
		t.Error("must not fetch when an input id is missing")
		return nil, nil
	}}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendStealthTransfer(context.Background(), intent, f.Input.Keys)
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "VALIDATION" {
		t.Fatalf("err = %v, want a VALIDATION *Error", err)
	}
}

// Marshalling unit tests for the boundary records — assert the Go structs serialize to the
// exact serde shapes the core deserializes (snake_case, externally-tagged enums, null for
// unset Options, positional split of inputs vs spend secrets).
func TestStealthIntentMarshalShape(t *testing.T) {
	view := "77" + repeat62("0")
	intent := StealthTransferIntent{
		FromAccount:     "component_aa",
		ResourceAddress: "resource_bb",
		Fee:             2000,
		Inputs: []StealthTransferInput{{
			Commitment:            "33" + repeat62("0"),
			OwnerAccountPublicKey: "44" + repeat62("0"),
			SpendSecret:           "a2" + repeat62("0"),
			UtxoSubstateID:        "utxo_resource_bb_33",
		}},
		Outputs: []StealthOutputSpec{{
			DestinationAccountPublicKey: "55" + repeat62("0"),
			DestinationViewPublicKey:    "66" + repeat62("0"),
			Amount:                      1000000,
			ResourceAddress:             "resource_bb",
			ResourceViewKey:             &view,
			Memo:                        MessageMemo("hello"),
			PayTo:                       PayToAccessRuleAllowAll,
			MinimumValuePromise:         5,
		}},
		RevealedInputAmount: 7,
		PayFeeFromRevealed:  true,
	}

	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("re-unmarshal intent: %v", err)
	}

	// pay_fee_from_revealed crosses as a top-level bool (the core defaults it, but the host always emits it).
	if string(m["pay_fee_from_revealed"]) != "true" {
		t.Errorf("pay_fee_from_revealed = %s, want true", m["pay_fee_from_revealed"])
	}

	// The intent's inputs carry ONLY commitment + owner_account_pk (the spend secret is split
	// out into the separate spend_secrets array — it must not leak into the input object).
	var inputs []map[string]json.RawMessage
	if err := json.Unmarshal(m["inputs"], &inputs); err != nil {
		t.Fatalf("decode inputs: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("inputs len = %d, want 1", len(inputs))
	}
	if _, ok := inputs[0]["spend_secret"]; ok {
		t.Error("intent input must not carry spend_secret (it crosses separately)")
	}
	if _, ok := inputs[0]["commitment"]; !ok {
		t.Error("intent input missing commitment")
	}
	if _, ok := inputs[0]["owner_account_pk"]; !ok {
		t.Error("intent input missing owner_account_pk")
	}

	// spendSecrets() returns the positional secret list.
	secrets := intent.spendSecrets()
	if len(secrets) != 1 || secrets[0] != "a2"+repeat62("0") {
		t.Errorf("spendSecrets() = %v, want the single positional secret", secrets)
	}
	// utxoSubstateIDs() returns the caller-supplied id (no derivation).
	ids, idErr := intent.utxoSubstateIDs()
	if idErr != nil {
		t.Fatalf("utxoSubstateIDs(): %v", idErr)
	}
	if len(ids) != 1 || ids[0] != "utxo_resource_bb_33" {
		t.Errorf("utxoSubstateIDs() = %v, want the single UTXO id", ids)
	}

	// The output's optional fields serialize as keys (null when unset, not omitted) and the
	// pay_to / memo enums use the core's external-tag shapes.
	var outputs []map[string]json.RawMessage
	if err := json.Unmarshal(m["outputs"], &outputs); err != nil {
		t.Fatalf("decode outputs: %v", err)
	}
	out := outputs[0]
	for _, key := range []string{"resource_view_key", "memo", "utxo_tag", "pay_to", "minimum_value_promise"} {
		if _, ok := out[key]; !ok {
			t.Errorf("output missing required key %q", key)
		}
	}
	if string(out["pay_to"]) != `"AccessRuleAllowAll"` {
		t.Errorf("pay_to = %s, want \"AccessRuleAllowAll\"", out["pay_to"])
	}
	if string(out["utxo_tag"]) != "null" {
		t.Errorf("utxo_tag = %s, want null", out["utxo_tag"])
	}
	if string(out["memo"]) != `{"Message":"hello"}` {
		t.Errorf("memo = %s, want {\"Message\":\"hello\"}", out["memo"])
	}
}

func TestStealthMemoBytesMarshal(t *testing.T) {
	raw, err := json.Marshal(BytesMemo([]byte{1, 2, 3}))
	if err != nil {
		t.Fatalf("marshal bytes memo: %v", err)
	}
	if string(raw) != `{"Bytes":[1,2,3]}` {
		t.Errorf("bytes memo = %s, want {\"Bytes\":[1,2,3]}", raw)
	}
	// Round-trip back.
	var m StealthMemo
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal bytes memo: %v", err)
	}
	if len(m.Bytes) != 3 || m.Bytes[0] != 1 {
		t.Errorf("round-trip bytes memo = %v", m.Bytes)
	}
}

// repeat62 returns n copies of s, used to pad short hex test values to 64 chars where the
// shape (not the value) is under test.
func repeat62(s string) string {
	out := ""
	for i := 0; i < 62; i++ {
		out += s
	}
	return out
}
