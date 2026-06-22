package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// decodeBody reads and JSON-decodes a request body into v, failing the test on error.
func decodeBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
}

func TestFetchSubstatesRoundTrip(t *testing.T) {
	var gotReq fetchRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/substates/fetch" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		decodeBody(t, r, &gotReq)
		// Echo a substate back for each requested id.
		subs := map[string]indexerSubstate{}
		for i, id := range gotReq.Requests {
			subs[id] = indexerSubstate{Version: uint32(i), Substate: json.RawMessage(`{"Vault":{"id":"` + id + `"}}`)}
		}
		writeJSON(t, w, fetchResponse{Substates: subs})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ids := []string{"component_aaa", "vault_bbb", "resource_ccc"}
	got, err := c.FetchSubstates(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchSubstates: %v", err)
	}
	if gotReq.CachedOnly {
		t.Errorf("cached_only = true, want false")
	}
	if len(got) != len(ids) {
		t.Fatalf("got %d substates, want %d", len(got), len(ids))
	}
	// Order must follow the request order.
	for i, id := range ids {
		if got[i].SubstateID != id {
			t.Errorf("substate[%d].SubstateID = %q, want %q", i, got[i].SubstateID, id)
		}
		// substate_value carried verbatim.
		if !strings.Contains(string(got[i].SubstateValue), id) {
			t.Errorf("substate[%d].SubstateValue = %q, want it to carry %q verbatim", i, got[i].SubstateValue, id)
		}
	}
}

func TestFetchSubstatesChunksAt20(t *testing.T) {
	var mu sync.Mutex
	var chunks [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fetchRequest
		decodeBody(t, r, &req)
		mu.Lock()
		chunks = append(chunks, req.Requests)
		mu.Unlock()
		subs := map[string]indexerSubstate{}
		for _, id := range req.Requests {
			subs[id] = indexerSubstate{Version: 1, Substate: json.RawMessage(`{}`)}
		}
		writeJSON(t, w, fetchResponse{Substates: subs})
	}))
	defer srv.Close()

	// 45 ids → 3 chunks of 20, 20, 5.
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = fmt.Sprintf("component_%02d", i)
	}
	c := NewClient(srv.URL)
	got, err := c.FetchSubstates(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchSubstates: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("merged %d substates, want %d", len(got), len(ids))
	}
	// Order must be preserved across chunk boundaries (map iteration is not ordered).
	for i, id := range ids {
		if got[i].SubstateID != id {
			t.Errorf("got[%d].SubstateID = %q, want %q (order mismatch across chunks)", i, got[i].SubstateID, id)
		}
	}
	if len(chunks) != 3 {
		t.Fatalf("made %d requests, want 3", len(chunks))
	}
	wantSizes := []int{20, 20, 5}
	for i, size := range wantSizes {
		if len(chunks[i]) != size {
			t.Errorf("chunk[%d] size = %d, want %d", i, len(chunks[i]), size)
		}
	}
}

func TestFetchSubstatesOmitsMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req fetchRequest
		decodeBody(t, r, &req)
		// Return only the first id; the rest are "missing".
		subs := map[string]indexerSubstate{}
		if len(req.Requests) > 0 {
			subs[req.Requests[0]] = indexerSubstate{Version: 0, Substate: json.RawMessage(`{}`)}
		}
		writeJSON(t, w, fetchResponse{Substates: subs})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.FetchSubstates(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("FetchSubstates: %v", err)
	}
	if len(got) != 1 || got[0].SubstateID != "a" {
		t.Fatalf("got %+v, want only substate 'a'", got)
	}
}

func TestFetchSubstateSingle404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/substates/") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.FetchSubstate(context.Background(), "component_missing")
	if err != nil {
		t.Fatalf("FetchSubstate on 404 returned error: %v", err)
	}
	if got != nil {
		t.Fatalf("FetchSubstate on 404 = %+v, want nil", got)
	}
}

func TestFetchSubstateSingleOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/substates/component_xyz" {
			t.Errorf("path = %q, want /substates/component_xyz", r.URL.Path)
		}
		writeJSON(t, w, indexerSubstate{Version: 7, Substate: json.RawMessage(`{"Component":{}}`)})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.FetchSubstate(context.Background(), "component_xyz")
	if err != nil {
		t.Fatalf("FetchSubstate: %v", err)
	}
	if got == nil || got.SubstateID != "component_xyz" || got.Version != 7 {
		t.Fatalf("got %+v, want id=component_xyz version=7", got)
	}
	if string(got.SubstateValue) != `{"Component":{}}` {
		t.Errorf("SubstateValue = %q, want it carried verbatim", got.SubstateValue)
	}
}

func TestSubmit(t *testing.T) {
	const envelope = "AAEC-base64-envelope"
	var gotReq submitRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/transactions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		decodeBody(t, r, &gotReq)
		writeJSON(t, w, submitResponse{TransactionID: "deadbeef"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	id, err := c.Submit(context.Background(), envelope)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if gotReq.Transaction != envelope {
		t.Errorf("posted transaction = %q, want %q", gotReq.Transaction, envelope)
	}
	if id != "deadbeef" {
		t.Errorf("txID = %q, want deadbeef", id)
	}
}

func TestGetResultPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/transactions/abc/result" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"result":"Pending"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	raw, finalized, err := c.GetResult(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if finalized {
		t.Errorf("finalized = true, want false for Pending")
	}
	if string(raw) != `"Pending"` {
		t.Errorf("raw = %q, want \"Pending\" verbatim", raw)
	}
}

func TestGetResultFinalizedVerbatim(t *testing.T) {
	const finalized = `{"Finalized":{"final_decision":"Commit","execution_result":{"finalize":{"result":{"Accept":{}}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":`+finalized+`}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	raw, isFinal, err := c.GetResult(context.Background(), "abc")
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if !isFinal {
		t.Errorf("finalized = false, want true")
	}
	// Returned byte-for-byte verbatim for the core's parse_finalized_result: the
	// transport must carry the `result` field through untouched (json.RawMessage).
	if string(raw) != finalized {
		t.Errorf("raw = %s, want exactly %s (must be verbatim)", raw, finalized)
	}
}

func TestWaitResultPollsUntilFinalized(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			_, _ = io.WriteString(w, `{"result":"Pending"}`)
			return
		}
		_, _ = io.WriteString(w, `{"result":{"Finalized":{"final_decision":"Commit"}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	raw, err := c.WaitResult(context.Background(), "abc", time.Millisecond)
	if err != nil {
		t.Fatalf("WaitResult: %v", err)
	}
	if !strings.Contains(string(raw), "Finalized") {
		t.Errorf("raw = %q, want a Finalized result", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls < 3 {
		t.Errorf("polled %d times, want at least 3", calls)
	}
}

func TestWaitResultRespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"result":"Pending"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.WaitResult(ctx, "abc", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitResult err = %v, want context.DeadlineExceeded", err)
	}
}

func TestHTTPErrorOnServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Submit(context.Background(), "env")
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v (%T), want *HTTPError", err, err)
	}
	if he.Status != http.StatusInternalServerError {
		t.Errorf("HTTPError.Status = %d, want 500", he.Status)
	}
	if !strings.Contains(he.Body, "boom") {
		t.Errorf("HTTPError.Body = %q, want it to contain the server body", he.Body)
	}
}

func TestAuthorizerInvoked(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(t, w, submitResponse{TransactionID: "x"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAuthorizer(authorizerFunc(func(req *http.Request) error {
		req.Header.Set("Authorization", "Bearer test-token")
		return nil
	})))
	if _, err := c.Submit(context.Background(), "env"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
	}
}

func TestBaseURLTrailingSlashTrimmed(t *testing.T) {
	c := NewClient("http://host:18300/")
	if c.BaseURL() != "http://host:18300" {
		t.Errorf("BaseURL() = %q, want trailing slash trimmed", c.BaseURL())
	}
}

// authorizerFunc adapts a func to the Authorizer interface for tests.
type authorizerFunc func(*http.Request) error

func (f authorizerFunc) Authorize(req *http.Request) error { return f(req) }

// writeJSON encodes v as the response body, failing the test on error.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
