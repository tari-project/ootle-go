package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// sseFrame writes one SSE frame (event/id/data, then the blank dispatch line) and flushes
// it. data may contain "\n" to emit a multi-line data: body.
func sseFrame(t *testing.T, w http.ResponseWriter, event, id, data string) {
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
	b.WriteString("\n") // blank line dispatches the frame
	if _, err := w.Write([]byte(b.String())); err != nil {
		t.Fatalf("write SSE frame: %v", err)
	}
	flusher.Flush()
}

// sseRaw writes a literal string to the stream and flushes (for comment/keep-alive lines).
func sseRaw(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("ResponseWriter is not an http.Flusher")
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("write SSE raw: %v", err)
	}
	flusher.Flush()
}

// recvEvent receives one RawSSEEvent (or fails on timeout / unexpected error/close).
func recvEvent(t *testing.T, out <-chan RawSSEEvent, errs <-chan error) RawSSEEvent {
	t.Helper()
	select {
	case ev, ok := <-out:
		if !ok {
			t.Fatalf("out closed before an event arrived")
		}
		return ev
	case err := <-errs:
		t.Fatalf("unexpected error before event: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for an event")
	}
	return RawSSEEvent{}
}

func TestStreamTransactionEventsNormalFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFrame(t, w, "std.vault.withdraw", "4271", `{"transaction_id":"aa"}`)
		sseFrame(t, w, "std.vault.deposit", "4272", `{"transaction_id":"bb"}`)
		// Keep the connection open until the test is done reading.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")

	ev1 := recvEvent(t, out, errs)
	if ev1.Event != "std.vault.withdraw" || ev1.ID != "4271" || string(ev1.Data) != `{"transaction_id":"aa"}` {
		t.Errorf("frame 1 = %+v", ev1)
	}
	ev2 := recvEvent(t, out, errs)
	if ev2.Event != "std.vault.deposit" || ev2.ID != "4272" || string(ev2.Data) != `{"transaction_id":"bb"}` {
		t.Errorf("frame 2 = %+v", ev2)
	}
}

func TestStreamTransactionEventsKeepAliveIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseRaw(t, w, ": keep-alive\n\n")
		sseFrame(t, w, "topic", "1", `{"a":1}`)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")

	ev := recvEvent(t, out, errs)
	if ev.Event != "topic" || ev.ID != "1" || string(ev.Data) != `{"a":1}` {
		t.Errorf("event = %+v, keep-alive should have been ignored", ev)
	}
}

func TestStreamTransactionEventsMultiLineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A JSON value split across two data: lines; joined with "\n" it is still valid.
		sseFrame(t, w, "topic", "9", "{\n\"k\":\"v\"}")
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")

	ev := recvEvent(t, out, errs)
	if string(ev.Data) != "{\n\"k\":\"v\"}" {
		t.Errorf("Data = %q, want multi-line joined with \\n", ev.Data)
	}
}

func TestStreamTransactionEventsHeaderAndQuery(t *testing.T) {
	t.Run("with last-event-id and filters", func(t *testing.T) {
		var gotPath, gotHeader string
		var gotQuery url.Values
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotHeader = r.Header.Get("Last-Event-ID")
			gotQuery = r.URL.Query()
			w.Header().Set("Content-Type", "text/event-stream")
			// Commit the response so the client's Do() returns; then hold the stream
			// open until the test releases it (or the client disconnects).
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-done:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(done)

		c := NewClient(srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		after := int64(7)
		q := TransactionEventQuery{Topic: "std.vault.withdraw", AfterID: &after}
		out, _ := c.StreamTransactionEvents(ctx, q, "4271")
		// Give the request time to reach the handler, then cancel.
		time.Sleep(100 * time.Millisecond)
		cancel()
		<-out // drain until close

		if gotPath != "/transactions/events/stream" {
			t.Errorf("path = %q", gotPath)
		}
		if gotHeader != "4271" {
			t.Errorf("Last-Event-ID = %q, want 4271", gotHeader)
		}
		if got := gotQuery.Get("topic"); got != "std.vault.withdraw" {
			t.Errorf("topic = %q", got)
		}
		if got := gotQuery.Get("after_id"); got != "7" {
			t.Errorf("after_id = %q, want 7", got)
		}
		if _, present := gotQuery["substate_id"]; present {
			t.Errorf("empty substate_id should be omitted, got %v", gotQuery["substate_id"])
		}
	})

	t.Run("without last-event-id", func(t *testing.T) {
		hadHeader := true
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, hadHeader = r.Header["Last-Event-Id"]
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-done:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(done)

		c := NewClient(srv.URL)
		ctx, cancel := context.WithCancel(context.Background())
		out, _ := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")
		time.Sleep(100 * time.Millisecond)
		cancel()
		<-out

		if hadHeader {
			t.Errorf("Last-Event-ID header should be absent when lastEventID is empty")
		}
	})
}

func TestStreamTransactionEventsCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFrame(t, w, "topic", "1", `{"a":1}`)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	out, errs := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")

	// Consume the first frame, then cancel mid-stream.
	_ = recvEvent(t, out, errs)
	cancel()

	// out must close and errs must NOT carry a ctx error.
	select {
	case _, ok := <-out:
		if ok {
			// drain any in-flight frame, then expect close
			select {
			case _, ok := <-out:
				if ok {
					t.Errorf("out should close after ctx cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("out did not close after ctx cancel")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("out did not close after ctx cancel")
	}

	select {
	case err, ok := <-errs:
		if ok && err != nil {
			t.Errorf("errs should not carry a ctx error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("errs did not close after ctx cancel")
	}
}

func TestStreamTransactionEventsMidStreamEOF(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseFrame(t, w, "topic", "1", `{"a":1}`)
		// Handler returns ⇒ body closes ⇒ client sees EOF.
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := c.StreamTransactionEvents(ctx, TransactionEventQuery{}, "")

	ev := recvEvent(t, out, errs)
	if ev.ID != "1" {
		t.Errorf("ID = %q, want 1", ev.ID)
	}

	// A single terminal error on errs, then both channels close.
	select {
	case err := <-errs:
		if err == nil {
			t.Fatalf("expected a terminal EOF error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no terminal error on mid-stream EOF")
	}
	// out must be closed.
	select {
	case _, ok := <-out:
		if ok {
			t.Errorf("out should be closed after EOF")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("out did not close after EOF")
	}
}

func TestStreamTransactionEventsNon2xxOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	out, errs := c.StreamTransactionEvents(context.Background(), TransactionEventQuery{}, "")

	select {
	case err := <-errs:
		var he *HTTPError
		if !errors.As(err, &he) {
			t.Fatalf("error = %v, want *HTTPError", err)
		}
		if he.Status != http.StatusInternalServerError {
			t.Errorf("Status = %d, want 500", he.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no error on non-2xx open")
	}
	// out must be closed with no frames.
	select {
	case ev, ok := <-out:
		if ok {
			t.Errorf("got frame %+v, want closed out", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("out did not close on non-2xx open")
	}
}

// TestStreamFinalizedEvents checks GET /events: the top-level path is hit (no query, no
// Last-Event-ID), keep-alive comments are ignored, and a TransactionFinalized frame decodes
// with its event: line and JSON body intact.
func TestStreamFinalizedEvents(t *testing.T) {
	var gotPath string
	hadHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, hadHeader = r.Header["Last-Event-Id"]
		w.Header().Set("Content-Type", "text/event-stream")
		sseRaw(t, w, ": keep-alive\n\n")
		sseFrame(t, w, "NewEpoch", "", `{"epoch":7}`)
		sseFrame(t, w, "TransactionFinalized", "", `{"transaction_id":"aa","outcome":"Commit"}`)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, errs := c.StreamFinalizedEvents(ctx)

	ev1 := recvEvent(t, out, errs)
	if ev1.Event != "NewEpoch" || string(ev1.Data) != `{"epoch":7}` {
		t.Errorf("frame 1 = %+v", ev1)
	}
	ev2 := recvEvent(t, out, errs)
	if ev2.Event != "TransactionFinalized" || string(ev2.Data) != `{"transaction_id":"aa","outcome":"Commit"}` {
		t.Errorf("frame 2 = %+v", ev2)
	}

	if gotPath != "/events" {
		t.Errorf("path = %q, want /events", gotPath)
	}
	if hadHeader {
		t.Errorf("Last-Event-ID header should be absent for /events")
	}
}

// Guard the parser helper directly to keep the framing rules pinned.
func TestSplitField(t *testing.T) {
	cases := []struct{ in, field, value string }{
		{"data: {\"a\":1}", "data", `{"a":1}`},
		{"data:no-space", "data", "no-space"},
		{"data:  two-spaces", "data", " two-spaces"}, // only one leading space stripped
		{"event: topic", "event", "topic"},
		{"id:4271", "id", "4271"},
		{"bare", "bare", ""},
	}
	for _, tc := range cases {
		f, v := splitField(tc.in)
		if f != tc.field || v != tc.value {
			t.Errorf("splitField(%q) = (%q, %q), want (%q, %q)", tc.in, f, v, tc.field, tc.value)
		}
	}
}
