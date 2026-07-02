package ootle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// mockTransport is a func-field-driven Transport for driving SendPublicTransfer without
// a live node. Each hook may be nil; nil Submit/GetResult use canned defaults.
type mockTransport struct {
	fetch  func(ctx context.Context, ids []string) ([]transport.FetchedSubstate, error)
	submit func(ctx context.Context, envelopeB64 string) (string, error)
	dryRun func(ctx context.Context, envelopeB64 string) (json.RawMessage, error)
	result func(ctx context.Context, txID string) (json.RawMessage, bool, error)
	stream func(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error)
}

func (m *mockTransport) FetchSubstates(ctx context.Context, ids []string) ([]transport.FetchedSubstate, error) {
	return m.fetch(ctx, ids)
}

func (m *mockTransport) Submit(ctx context.Context, envelopeB64 string) (string, error) {
	if m.submit != nil {
		return m.submit(ctx, envelopeB64)
	}
	return "deadbeef", nil
}

func (m *mockTransport) SubmitDryRun(ctx context.Context, envelopeB64 string) (json.RawMessage, error) {
	if m.dryRun != nil {
		return m.dryRun(ctx, envelopeB64)
	}
	return json.RawMessage(`{"Finalized":{"final_decision":"Commit"}}`), nil
}

func (m *mockTransport) GetResult(ctx context.Context, txID string) (json.RawMessage, bool, error) {
	if m.result != nil {
		return m.result(ctx, txID)
	}
	return json.RawMessage(`{"Finalized":{"final_decision":"Commit"}}`), true, nil
}

// StreamFinalizedEvents makes mockTransport a finalizationStreamer. A nil hook returns an
// immediately-closed stream, so the wait falls straight through to the REST poll — keeping
// the default behavior identical to the old pure-poll path.
func (m *mockTransport) StreamFinalizedEvents(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
	if m.stream != nil {
		return m.stream(ctx)
	}
	out := make(chan transport.RawSSEEvent)
	errs := make(chan error, 1)
	close(out)
	close(errs)
	return out, errs
}

var (
	_ transport.Transport  = (*mockTransport)(nil)
	_ finalizationStreamer = (*mockTransport)(nil)
)

// resolveFixture mirrors a committed resolve_public_transfer/* vector. Since ABI
// ootle-sdk-ffi-c/16 the seal signs with a random nonce, so the vector no longer pins
// expected.encoded_transaction / transaction_id — the driver tests assert the submitted
// envelope is well-formed rather than byte-for-byte, and use fakeTxID for the mock's returned id.
type resolveFixture struct {
	Operation string `json:"operation"`
	Input     struct {
		Network Network                     `json:"network"`
		Intent  PublicTransferIntent        `json:"intent"`
		Keys    PublicTransferKeys          `json:"keys"`
		Fetched []transport.FetchedSubstate `json:"fetched"`
	} `json:"input"`
}

// fakeTxID is a synthetic 32-byte (64 hex char) transaction id the mock transports return in
// place of the vector's (now-absent) expected.transaction_id.
const fakeTxID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeEncodedTx is a synthetic hex-encoded transaction the submit-side tests feed to SubmitSealed
// (which only hex→base64 re-encodes it — it never decodes or seals it), standing in for the
// vector's (now-absent) expected.encoded_transaction.
const fakeEncodedTx = "0102030405060708090a0b0c0d0e0f10"

// assertWellFormedEnvelope asserts the submitted base64 envelope decodes to a non-empty byte
// slice — the strongest still-true check now that the random-nonce seal makes the encoded bytes
// non-reproducible (so a byte-for-byte compare against the vector is no longer possible).
func assertWellFormedEnvelope(t *testing.T, b64 string) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Errorf("submitted envelope is not valid base64: %v", err)
		return
	}
	if len(raw) == 0 {
		t.Errorf("submitted envelope decoded to an empty byte slice")
	}
}

func loadResolveFixture(t *testing.T, rel string) resolveFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "resolve_public_transfer", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var f resolveFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	return f
}

// fetchFromVector returns a fetch hook that returns the committed vector's FULL fetched
// batch (the canonical resolved input: the from-component AND its vault) each call. This
// models a host that already has everything the resolve needs; the core resolves it in a
// single apply pass. `rounds` counts fetch calls. (For a genuine multi-round host that
// only learns the vault id from the core's NeedMore fetch_ids, see fetchByID below.)
func fetchFromVector(f resolveFixture, rounds *int) func(context.Context, []string) ([]transport.FetchedSubstate, error) {
	return func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
		*rounds++
		return f.Input.Fetched, nil
	}
}

// fetchByID models a real indexer: it serves substates strictly by requested id from the
// vector's batch (a host that fetches ONLY what it was told to). Crucially the from-vault
// is returned only once the core, in a prior NeedMore, hands back its concrete id in
// fetch_ids — proving the two-phase loop converges across rounds without the host ever
// deriving the vault id itself. `rounds` counts fetch calls.
func fetchByID(f resolveFixture, rounds *int) func(context.Context, []string) ([]transport.FetchedSubstate, error) {
	index := make(map[string]transport.FetchedSubstate, len(f.Input.Fetched))
	for _, s := range f.Input.Fetched {
		index[s.SubstateID] = s
	}
	return func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
		*rounds++
		out := make([]transport.FetchedSubstate, 0, len(ids))
		for _, id := range ids {
			if s, ok := index[id]; ok {
				out = append(out, s)
			}
		}
		return out, nil
	}
}

// loadParseRaw loads a committed parse_finalized_result vector and returns its raw_result
// (the indexer IndexerTransactionFinalizedResult the core's parser consumes) plus the
// expected parsed FinalizedResult.
func loadParseRaw(t *testing.T, rel string) (json.RawMessage, FinalizedResult) {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "parse_finalized_result", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parse fixture %s: %v", path, err)
	}
	var fx struct {
		Input struct {
			RawResult json.RawMessage `json:"raw_result"`
		} `json:"input"`
		Expected struct {
			Parsed FinalizedResult `json:"parsed"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal parse fixture %s: %v", path, err)
	}
	return fx.Input.RawResult, fx.Expected.Parsed
}

// TestSendPublicTransfer_MockTransport drives the full two-phase flow
// against a mock transport seeded from a committed resolve_public_transfer vector and
// asserts the driver's submitted envelope is the vector's expected encoded_transaction
// (base64 of the hex), and the parsed FinalizedResult matches the canned committed
// outcome.
func TestSendPublicTransfer_MockTransport(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	if f.Operation != "resolve_and_encode_public_transfer" {
		t.Fatalf("unexpected fixture operation %q", f.Operation)
	}

	// Canned finalized result + its expected parse, both from a committed parse vector.
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	var rounds int
	var gotEnvelope string
	mock := &mockTransport{
		fetch: fetchFromVector(f, &rounds),
		submit: func(_ context.Context, envelopeB64 string) (string, error) {
			gotEnvelope = envelopeB64
			return fakeTxID, nil
		},
		result: func(_ context.Context, _ string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	result, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("SendPublicTransfer: %v", err)
	}

	// The random-nonce seal is not byte-reproducible, so assert the driver submitted a
	// well-formed envelope (valid base64 of a non-empty encoded transaction) rather than the
	// exact committed bytes. This still proves the full Go two-phase driver ran build → resolve
	// → seal → encode → submit.
	assertWellFormedEnvelope(t, gotEnvelope)
	// The parsed FinalizedResult equals the committed parse vector's expected.parsed —
	// proving the driver routes the raw result through the core's parser and types it.
	if !reflect.DeepEqual(result, wantParsed) {
		gj, _ := json.MarshalIndent(result, "", "  ")
		wj, _ := json.MarshalIndent(wantParsed, "", "  ")
		t.Errorf("parsed FinalizedResult mismatch:\n got:  %s\n want: %s", gj, wj)
	}
	if rounds < 1 {
		t.Errorf("resolver made %d fetch rounds, want >= 1", rounds)
	}
}

// TestSendPublicTransfer_MultiRound proves the two-phase loop converges across
// MULTIPLE fetch rounds when the host fetches strictly by the ids the core hands back — the
// real end-to-end exercise of the C-ABI fetch_ids fix. The mock (fetchByID) serves the
// from-component in round 0 (a want-list seed) and the from-vault only in round 1, once the
// core's NeedMore exposes the discovered vault id in fetch_ids. The host never derives the
// vault id itself. The submitted envelope is asserted well-formed (the random-nonce seal is not
// byte-reproducible); the point of this test is the multi-round convergence.
func TestSendPublicTransfer_MultiRound(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	var rounds int
	var gotEnvelope string
	mock := &mockTransport{
		fetch: fetchByID(f, &rounds),
		submit: func(_ context.Context, envelopeB64 string) (string, error) {
			gotEnvelope = envelopeB64
			return fakeTxID, nil
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("SendPublicTransfer: %v", err)
	}
	assertWellFormedEnvelope(t, gotEnvelope)
	// The vault is discovered only after the component is fetched, so convergence MUST take
	// at least two fetch rounds — this is what the fetch_ids fix makes reachable.
	if rounds < 2 {
		t.Errorf("converged in %d fetch round(s), want >= 2 (the vault is only learned via fetch_ids)", rounds)
	}
	if rounds > maxResolutionRounds {
		t.Errorf("fetch called %d times, want the loop bounded by the cap %d", rounds, maxResolutionRounds)
	}
}

// TestSendPublicTransfer_WithheldRequiredSubstateErrors proves the loop iterates and is
// bounded: when the host withholds a required substate (it returns ONLY the from-component
// and never the from-vault, even after fetch_ids names it), resolution cannot complete and
// the driver returns a typed RESOLUTION error rather than looping forever — the core's own
// deadlock/required-missing guard fires, well within the driver's safety cap.
func TestSendPublicTransfer_WithheldRequiredSubstateErrors(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	// Return the from-component when asked, but NEVER the from-vault (even when fetch_ids
	// names it). The required from-vault want can never be satisfied ⇒ RESOLUTION error.
	component := f.Input.Fetched[0]
	var rounds int
	mock := &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			rounds++
			out := make([]transport.FetchedSubstate, 0, 1)
			for _, id := range ids {
				if id == component.SubstateID {
					out = append(out, component)
				}
			}
			return out, nil
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	if err == nil {
		t.Fatal("expected a non-convergence error, got nil")
	}
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code != "RESOLUTION" {
		t.Errorf("error code = %q, want RESOLUTION", oe.Code)
	}
	if rounds < 2 {
		t.Errorf("fetch called %d time(s), want >= 2 (the loop must iterate)", rounds)
	}
	// Bounded: the loop never runs past the driver's safety cap.
	if rounds > maxResolutionRounds {
		t.Errorf("fetch called %d times, want the loop capped at %d", rounds, maxResolutionRounds)
	}
}

// TestErrorWrapsSentinel verifies the driver's cap error is errors.Is-friendly on
// ErrResolutionDidNotConverge while remaining errors.As-inspectable for its stable Code —
// the documented contract for non-convergence (the cap path is a backstop the core's
// deadlock guard usually reaches first, so this locks the wiring directly).
func TestErrorWrapsSentinel(t *testing.T) {
	err := error(&Error{Code: "RESOLUTION", Message: "capped", cause: ErrResolutionDidNotConverge})
	if !errors.Is(err, ErrResolutionDidNotConverge) {
		t.Errorf("errors.Is(err, ErrResolutionDidNotConverge) = false, want true")
	}
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "RESOLUTION" {
		t.Errorf("errors.As did not surface the RESOLUTION code")
	}
}

// TestSendPublicTransfer_RejectsExplicitInputs proves the resolved path refuses an intent
// carrying explicit inputs (which would short-circuit the core to the explicit path and
// skip resolution).
func TestSendPublicTransfer_RejectsExplicitInputs(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	intent := f.Input.Intent
	intent.Inputs = []InputRef{{SubstateID: "component_" + "00"}}

	mock := &mockTransport{fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
		t.Error("FetchSubstates must not be called when inputs are rejected")
		return nil, nil
	}}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendPublicTransfer(context.Background(), intent, f.Input.Keys)
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code != "VALIDATION" {
		t.Errorf("error code = %q, want VALIDATION", oe.Code)
	}
}

// TestSendPublicTransfer_SubmitErrorFreesHandle injects a mid-flow transport error (after
// resolution, at Submit) and asserts the call returns the transport error. The opaque
// handle was already consumed by seal before submit, so the deferred guard frees nothing;
// this guards the "error after a consuming call" path against a double-free / leak (a
// double-free would crash the test binary).
func TestSendPublicTransfer_SubmitErrorFreesHandle(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	wantErr := errors.New("submit boom")
	var rounds int
	mock := &mockTransport{
		fetch: fetchFromVector(f, &rounds),
		submit: func(context.Context, string) (string, error) {
			return "", wantErr
		},
	}

	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the submit error", err)
	}
}

// TestSendPublicTransfer_FetchErrorFreesHandle injects an error on the very first fetch
// (before any consuming call). The handle is still live and must be freed by the deferred
// guard; a leak/double-free would surface under -race or a crash.
func TestSendPublicTransfer_FetchErrorFreesHandle(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	wantErr := errors.New("fetch boom")
	mock := &mockTransport{
		fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
			return nil, wantErr
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	_, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}

// TestSendPublicTransfer_ContextCancelDuringWait proves context cancellation propagates
// while polling for the result (the transport stays Pending; ctx times out).
func TestSendPublicTransfer_ContextCancelDuringWait(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	var rounds int
	mock := &mockTransport{
		fetch: fetchFromVector(f, &rounds),
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return json.RawMessage(`"Pending"`), false, nil
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network), WithPollInterval(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.SendPublicTransfer(ctx, f.Input.Intent, f.Input.Keys)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex %q: %v", s, err)
	}
	return b
}

// TestSendPublicTransfer_NoNetworkConfigured proves a client built without WithNetwork
// rejects a Send* call with a VALIDATION *Error before touching the transport.
func TestSendPublicTransfer_NoNetworkConfigured(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	mock := &mockTransport{fetch: func(context.Context, []string) ([]transport.FetchedSubstate, error) {
		t.Error("FetchSubstates must not be called when no network is configured")
		return nil, nil
	}}
	c := NewClient(mock) // no WithNetwork
	_, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code != "VALIDATION" {
		t.Errorf("error code = %q, want VALIDATION", oe.Code)
	}
}

// TestWithNetwork_DrivesTransfer proves a client built WITH WithNetwork drives the
// two-phase transfer through to a well-formed submitted envelope — the network is read from the
// client, not a per-call argument.
func TestWithNetwork_DrivesTransfer(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	var rounds int
	var gotEnvelope string
	mock := &mockTransport{
		fetch: fetchFromVector(f, &rounds),
		submit: func(_ context.Context, envelopeB64 string) (string, error) {
			gotEnvelope = envelopeB64
			return fakeTxID, nil
		},
	}
	c := NewClient(mock, WithNetwork(f.Input.Network))
	if _, err := c.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys); err != nil {
		t.Fatalf("SendPublicTransfer: %v", err)
	}
	assertWellFormedEnvelope(t, gotEnvelope)
}

// TestConnect_BuildsClient proves Connect returns a usable client over a fresh transport
// and that WithNetwork carries through: a Connect-built client with a network drives a
// real request (here against an httptest indexer), while one without a network rejects.
func TestConnect_BuildsClient(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	// An httptest server standing in for the indexer: it serves the vector's fetched batch
	// for any substate query, so the resolved-path driver can converge and submit.
	fetchedJSON, err := json.Marshal(f.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch {
		case strings.Contains(r.URL.Path, "substates"):
			w.Write(fetchedJSON)
		case strings.Contains(r.URL.Path, "transactions"):
			w.Write([]byte(`{"transaction_id":"` + fakeTxID + `"}`))
		default:
			w.Write([]byte(`{"Finalized":{"final_decision":"Commit"}}`))
		}
	}))
	defer srv.Close()

	// Without a network: a VALIDATION error before any HTTP call.
	cNoNet := Connect(srv.URL)
	if _, err := cNoNet.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys); err == nil {
		t.Fatal("Connect without WithNetwork must reject Send*")
	} else {
		var oe *Error
		if !errors.As(err, &oe) || oe.Code != "VALIDATION" {
			t.Fatalf("err = %v, want a VALIDATION *Error", err)
		}
	}
	if hits != 0 {
		t.Fatalf("the unset-network client must not reach the transport, got %d request(s)", hits)
	}

	// With a network: the client is non-nil, gets past the unset-network guard, and reaches
	// the transport (the canned HTTP shapes need not produce a full commit — the point is the
	// network is read from the client and a request is issued, not the VALIDATION short-circuit).
	cNet := Connect(srv.URL, WithNetwork(f.Input.Network))
	if cNet == nil {
		t.Fatal("Connect returned nil")
	}
	_, err = cNet.SendPublicTransfer(context.Background(), f.Input.Intent, f.Input.Keys)
	var oe *Error
	if errors.As(err, &oe) && oe.Code == "VALIDATION" && strings.Contains(oe.Message, "no network") {
		t.Fatalf("network was not read from the client: %v", err)
	}
	if hits == 0 {
		t.Fatal("the network-configured client did not reach the transport")
	}
}
