package ootle

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// --- test fixtures ---------------------------------------------------------------------

// writeSSEFrame writes one SSE frame (event/id/data, then the blank dispatch line) and
// flushes it. The handler MUST flush before blocking so srv.Close() can tear down cleanly.
func writeSSEFrame(t *testing.T, w http.ResponseWriter, event, id, data string) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("ResponseWriter is not an http.Flusher")
	}
	var b strings.Builder
	if event != "" {
		fmt.Fprintf(&b, "event: %s\n", event)
	}
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatalf("write SSE frame: %v", err)
	}
	flusher.Flush()
}

// ootleClient builds an ootle.Client over a real *transport.Client pointed at url, so the
// eventStreamer type-assertion in WatchEvents succeeds.
func ootleClient(url string) *Client {
	return NewClient(transport.NewClient(url))
}

// recvEvent receives one Event or fails on timeout / unexpected error / close.
func recvEvent(t *testing.T, out <-chan Event, errs <-chan error) Event {
	t.Helper()
	select {
	case ev, ok := <-out:
		if !ok {
			t.Fatalf("out closed before an event arrived")
		}
		return ev
	case err := <-errs:
		t.Fatalf("unexpected error before event: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for an event")
	}
	return Event{}
}

const fullBody = `{"transaction_id":"abc123","event":{"substate_id":"component_deadbeef","template_address":"tmpl_001","payload":{"amount":"100","to":"acct_x"}}}`

// --- tests -----------------------------------------------------------------------------

// TestWatchEventsTypedMapping covers happy-path typed decode: topic from event:, id parsed
// to int64, body fields, and the flat map[string]string payload.
func TestWatchEventsTypedMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEFrame(t, w, "std.vault.withdraw", "4271", fullBody)
		<-r.Context().Done() // flushed already; safe to block
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := ootleClient(srv.URL).WatchEvents(ctx, EventFilter{})

	ev := recvEvent(t, out, errs)
	if ev.ID != 4271 {
		t.Errorf("ID = %d, want 4271", ev.ID)
	}
	if ev.Topic != "std.vault.withdraw" {
		t.Errorf("Topic = %q, want std.vault.withdraw", ev.Topic)
	}
	if ev.TransactionID != "abc123" {
		t.Errorf("TransactionID = %q, want abc123", ev.TransactionID)
	}
	if ev.SubstateID != "component_deadbeef" {
		t.Errorf("SubstateID = %q, want component_deadbeef", ev.SubstateID)
	}
	if ev.TemplateAddress != "tmpl_001" {
		t.Errorf("TemplateAddress = %q, want tmpl_001", ev.TemplateAddress)
	}
	want := map[string]string{"amount": "100", "to": "acct_x"}
	if len(ev.Payload) != len(want) {
		t.Fatalf("Payload = %v, want %v", ev.Payload, want)
	}
	for k, v := range want {
		if ev.Payload[k] != v {
			t.Errorf("Payload[%q] = %q, want %q", k, ev.Payload[k], v)
		}
	}
}

// TestWatchEventsSubstateIDNull covers substate_id: null mapping to "".
func TestWatchEventsSubstateIDNull(t *testing.T) {
	const body = `{"transaction_id":"tx1","event":{"substate_id":null,"template_address":"tmpl_2","payload":{"k":"v"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEFrame(t, w, "topic.x", "7", body)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := ootleClient(srv.URL).WatchEvents(ctx, EventFilter{})

	ev := recvEvent(t, out, errs)
	if ev.SubstateID != "" {
		t.Errorf("SubstateID = %q, want empty string for null", ev.SubstateID)
	}
	if ev.Payload["k"] != "v" {
		t.Errorf("Payload[k] = %q, want v", ev.Payload["k"])
	}
}

// TestWatchEventsReconnectReplaysFromLastEventID asserts that after the first connection
// delivers a frame (id 10) and closes, the client reconnects and the SECOND request carries
// Last-Event-ID: 10.
func TestWatchEventsReconnectReplaysFromLastEventID(t *testing.T) {
	var (
		mu        sync.Mutex
		connCount int
		secondLEI string
	)
	secondConnected := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connCount++
		n := connCount
		mu.Unlock()

		if n == 1 {
			// First connection: deliver one frame then return (closes the stream → EOF).
			writeSSEFrame(t, w, "topic.a", "10", fullBody)
			return
		}
		// Second connection: capture the resume header, then signal and block.
		mu.Lock()
		secondLEI = r.Header.Get("Last-Event-ID")
		mu.Unlock()
		// Flush headers before blocking so srv.Close() isn't deadlocked.
		if f, ok := w.(http.Flusher); ok {
			w.WriteHeader(http.StatusOK)
			f.Flush()
		}
		close(secondConnected)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Shrink the reconnect wait so the test is fast (deterministic, not racy).
	out, errs := ootleClient(srv.URL).WatchEvents(ctx, EventFilter{})

	// First event arrives.
	ev := recvEvent(t, out, errs)
	if ev.ID != 10 {
		t.Fatalf("first event ID = %d, want 10", ev.ID)
	}

	// Mid-stream EOF surfaces as a warning on errs (non-fatal), and a reconnect follows.
	select {
	case err := <-errs:
		if err == nil {
			t.Fatalf("expected a non-nil warning on reconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for the reconnect warning")
	}

	select {
	case <-secondConnected:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the second connection")
	}

	mu.Lock()
	got := secondLEI
	mu.Unlock()
	if got != "10" {
		t.Errorf("second request Last-Event-ID = %q, want 10", got)
	}
}

// TestWatchEventsBadFrameSkipped asserts a non-JSON data: frame between two good frames is
// skipped and the stream is NOT killed.
func TestWatchEventsBadFrameSkipped(t *testing.T) {
	good1 := `{"transaction_id":"t1","event":{"substate_id":"s1","template_address":"a1","payload":{"n":"1"}}}`
	good2 := `{"transaction_id":"t2","event":{"substate_id":"s2","template_address":"a2","payload":{"n":"2"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEFrame(t, w, "topic", "1", good1)
		writeSSEFrame(t, w, "topic", "2", "this-is-not-json")
		writeSSEFrame(t, w, "topic", "3", good2)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := ootleClient(srv.URL).WatchEvents(ctx, EventFilter{})

	ev1 := recvEvent(t, out, errs)
	if ev1.TransactionID != "t1" {
		t.Fatalf("first event tx = %q, want t1", ev1.TransactionID)
	}
	ev2 := recvEvent(t, out, errs)
	if ev2.TransactionID != "t2" {
		t.Fatalf("second event tx = %q, want t2 (bad frame should be skipped)", ev2.TransactionID)
	}
	if ev2.ID != 3 {
		t.Errorf("second good event ID = %d, want 3", ev2.ID)
	}
}

// mock3MethodTransport implements ONLY the base Transport methods and not eventStreamer.
type mock3MethodTransport struct{}

func (mock3MethodTransport) FetchSubstates(ctx context.Context, ids []string) ([]transport.FetchedSubstate, error) {
	return nil, nil
}
func (mock3MethodTransport) Submit(ctx context.Context, envelopeB64 string) (string, error) {
	return "", nil
}
func (mock3MethodTransport) SubmitDryRun(ctx context.Context, envelopeB64 string) (json.RawMessage, error) {
	return nil, nil
}
func (mock3MethodTransport) GetResult(ctx context.Context, txID string) (json.RawMessage, bool, error) {
	return nil, false, nil
}

// TestWatchEventsUnsupportedTransport asserts a 3-method transport yields one terminal
// *Error (code INVALID) on errs, with both channels closed and no events.
func TestWatchEventsUnsupportedTransport(t *testing.T) {
	c := NewClient(mock3MethodTransport{})
	out, errs := c.WatchEvents(context.Background(), EventFilter{})

	select {
	case err, ok := <-errs:
		if !ok {
			t.Fatalf("errs closed without a terminal error")
		}
		var oe *Error
		if !asOotleError(err, &oe) {
			t.Fatalf("error = %v (%T), want *ootle.Error", err, err)
		}
		if oe.Code != "INVALID" {
			t.Errorf("Code = %q, want INVALID", oe.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for the terminal error")
	}

	// errs must be closed after the single error.
	if _, ok := <-errs; ok {
		t.Errorf("errs delivered a second value, want closed")
	}
	// out must be closed with no events.
	if ev, ok := <-out; ok {
		t.Errorf("out delivered an event %v, want closed", ev)
	}
}

// TestWatchEventsCtxCancelCleanClose asserts ctx cancellation closes both channels with no
// error value emitted.
func TestWatchEventsCtxCancelCleanClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush headers then block until the client cancels — no frames.
		if f, ok := w.(http.Flusher); ok {
			w.WriteHeader(http.StatusOK)
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	out, errs := ootleClient(srv.URL).WatchEvents(ctx, EventFilter{})

	// Give the stream a moment to connect, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// Both channels must close; no error value is emitted.
	timeout := time.After(5 * time.Second)
	for out != nil || errs != nil {
		select {
		case ev, ok := <-out:
			if !ok {
				out = nil
				continue
			}
			t.Errorf("unexpected event after cancel: %v", ev)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			t.Errorf("unexpected error after cancel: %v", err)
		case <-timeout:
			t.Fatalf("timed out waiting for clean close after cancel")
		}
	}
}
