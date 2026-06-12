package ootle

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// finalizedFrame builds a TransactionFinalized SSE frame for txID.
func finalizedFrame(txID string) transport.RawSSEEvent {
	return transport.RawSSEEvent{
		Event: finalizedEventName,
		Data:  json.RawMessage(`{"transaction_id":"` + txID + `","outcome":"Commit"}`),
	}
}

// silentStream returns an open stream that never delivers a frame and closes only when ctx
// is cancelled — used to exercise the timeout and ctx-cancel paths.
func silentStream(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
	out := make(chan transport.RawSSEEvent)
	errs := make(chan error, 1)
	go func() {
		<-ctx.Done()
		close(out)
		close(errs)
	}()
	return out, errs
}

// TestSubmitSealed_SSEHappyPath: a matching TransactionFinalized frame opens the gate and
// the typed result is then read and parsed.
func TestSubmitSealed_SSEHappyPath(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, wantParsed := loadParseRaw(t, "accept.json")
	txID := f.Expected.TransactionID

	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return txID, nil },
		stream: func(context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			out := make(chan transport.RawSSEEvent, 1)
			errs := make(chan error, 1)
			out <- finalizedFrame(txID)
			return out, errs
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock)
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed result mismatch:\n got:  %+v\n want: %+v", result, wantParsed)
	}
}

// TestSubmitSealed_SSEPreferredOverPoll: with no matching frame, the wait blocks until the
// safety-net timeout before polling — proving it does not race the poll ahead of the gate.
// (A result is available immediately; only the SSE wait delays the read.)
func TestSubmitSealed_SSEPreferredOverPoll(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, _ := loadParseRaw(t, "accept.json")

	const timeout = 100 * time.Millisecond
	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		stream: silentStream,
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithFinalizationTimeout(timeout))
	start := time.Now()
	if _, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction}); err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < timeout/2 {
		t.Errorf("returned in %v, want >= %v — the poll raced ahead of the SSE gate", elapsed, timeout/2)
	}
}

// TestSubmitSealed_IgnoresUnrelatedFrames: NewEpoch and a TransactionFinalized for a
// different tx id must not open the gate — the wait times out and falls back instead.
func TestSubmitSealed_IgnoresUnrelatedFrames(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, _ := loadParseRaw(t, "accept.json")

	const timeout = 100 * time.Millisecond
	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		stream: func(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			out := make(chan transport.RawSSEEvent, 2)
			errs := make(chan error, 1)
			out <- transport.RawSSEEvent{Event: "NewEpoch", Data: json.RawMessage(`{"epoch":7}`)}
			out <- finalizedFrame("00ff") // a different tx id
			go func() {
				<-ctx.Done()
				close(out)
				close(errs)
			}()
			return out, errs
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithFinalizationTimeout(timeout))
	start := time.Now()
	if _, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction}); err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if elapsed := time.Since(start); elapsed < timeout/2 {
		t.Errorf("returned in %v, want >= %v — an unrelated frame opened the gate", elapsed, timeout/2)
	}
}

// TestSubmitSealed_StreamErrorFallsBack: a terminal stream error before any frame falls
// back to the REST poll and still returns the result. No hang.
func TestSubmitSealed_StreamErrorFallsBack(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		stream: func(context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			out := make(chan transport.RawSSEEvent)
			errs := make(chan error, 1)
			errs <- errors.New("stream boom")
			close(out)
			close(errs)
			return out, errs
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock)
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed result mismatch after fallback")
	}
}

// TestSubmitSealed_StreamClosedFallsBack: the stream closing with no matching frame falls
// back to the REST poll.
func TestSubmitSealed_StreamClosedFallsBack(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	// A nil stream hook yields an immediately-closed stream (see mockTransport).
	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock)
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed result mismatch after fallback")
	}
}

// TestSubmitSealed_NoStreamerUsesPoll: a transport without the streaming capability uses
// the pure REST poll, unchanged.
func TestSubmitSealed_NoStreamerUsesPoll(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	tr := &pollOnlyTransport{raw: rawResult, txID: f.Expected.TransactionID}
	if _, ok := transport.Transport(tr).(finalizationStreamer); ok {
		t.Fatal("pollOnlyTransport must not implement finalizationStreamer")
	}

	c := NewClient(tr)
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed result mismatch on poll-only path")
	}
}

// TestSubmitSealed_DisabledSkipsStream: WithoutFinalizationWait must not open the stream at
// all and still return the polled result.
func TestSubmitSealed_DisabledSkipsStream(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")
	rawResult, wantParsed := loadParseRaw(t, "accept.json")

	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		stream: func(context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			t.Error("StreamFinalizedEvents must not be called when the wait is disabled")
			return silentStream(context.Background())
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return rawResult, true, nil
		},
	}

	c := NewClient(mock, WithoutFinalizationWait())
	result, err := c.SubmitSealed(context.Background(), EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if err != nil {
		t.Fatalf("SubmitSealed: %v", err)
	}
	if !reflect.DeepEqual(result, wantParsed) {
		t.Errorf("parsed result mismatch with wait disabled")
	}
}

// TestSubmitSealed_ContextCancelDuringSSE: cancelling ctx mid-wait returns the ctx error and
// tears down the stream (the child stream ctx is cancelled — no goroutine leak).
func TestSubmitSealed_ContextCancelDuringSSE(t *testing.T) {
	f := loadResolveFixture(t, "single_key_basic.json")

	var streamCtx context.Context
	mock := &mockTransport{
		submit: func(context.Context, string) (string, error) { return f.Expected.TransactionID, nil },
		stream: func(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error) {
			streamCtx = ctx
			return silentStream(ctx)
		},
		result: func(context.Context, string) (json.RawMessage, bool, error) {
			return json.RawMessage(`"Pending"`), false, nil
		},
	}

	c := NewClient(mock, WithFinalizationTimeout(5*time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := c.SubmitSealed(ctx, EncodedPublicTransfer{EncodedTransaction: f.Expected.EncodedTransaction})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if streamCtx == nil || streamCtx.Err() == nil {
		t.Error("stream context was not cancelled — possible goroutine leak")
	}
}

// pollOnlytransport is a Transport WITHOUT the StreamFinalizedEvents capability, used to
// prove the no-SSE path. GetResult always returns raw as finalized.
type pollOnlyTransport struct {
	raw  json.RawMessage
	txID string
}

func (p *pollOnlyTransport) FetchSubstates(context.Context, []string) ([]transport.FetchedSubstate, error) {
	return nil, nil
}
func (p *pollOnlyTransport) Submit(context.Context, string) (string, error) { return p.txID, nil }
func (p *pollOnlyTransport) SubmitDryRun(context.Context, string) (json.RawMessage, error) {
	return p.raw, nil
}
func (p *pollOnlyTransport) GetResult(context.Context, string) (json.RawMessage, bool, error) {
	return p.raw, true, nil
}

var _ transport.Transport = (*pollOnlyTransport)(nil)
