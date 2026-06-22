// SSE-preferred finalization wait for real (non-dry-run) submissions.
//
// submitAndWait is the shared submit→wait→parse tail for every Send* driver. When the
// transport can stream the indexer's lifecycle events (GET /events), it gates the result
// read on the TransactionFinalized frame for the submitted tx id — a later, safer edge than
// /transactions/{id}/result turning non-pending. Any stream fault, close, or timeout
// silently falls back to the REST poll (waitResult), which is also bounded by ctx; polling
// is never raced against the stream.

package ootle

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// finalizedEventName is the SSE event: field of the indexer's TransactionFinalized
// lifecycle frame (clients/tari_indexer_client::event::IndexerEvent::as_event_name). All
// other frames on GET /events (e.g. "NewEpoch") are ignored by the wait.
const finalizedEventName = "TransactionFinalized"

// defaultFinalizationTimeout is the safety-net timeout on the SSE wait before falling back
// to REST polling, so a silently-missed broadcast frame can never hang the call. The
// surrounding ctx remains the hard deadline; this only bounds the SSE phase. Tunable via
// WithFinalizationTimeout.
const defaultFinalizationTimeout = 30 * time.Second

// Internal sentinels distinguishing the SSE-wait exit reasons. They never surface to the
// caller — each one merely routes the wait to the silent REST fallback (see submitAndWait).
var (
	errFinalizationStreamClosed = errors.New("ootle: finalization stream closed before the transaction finalized")
	errFinalizationTimeout      = errors.New("ootle: finalization SSE wait timed out")
)

// finalizationStreamer is the optional transport capability to stream the indexer's
// lifecycle events (GET /events). Mirroring eventStreamer (events.go), the wait
// type-asserts the stored Transport to it rather than widening Transport (which would break
// the mocks). The concrete *transport.Client satisfies it.
type finalizationStreamer interface {
	StreamFinalizedEvents(ctx context.Context) (<-chan transport.RawSSEEvent, <-chan error)
}

// finalizedEvent decodes only what the wait needs from a TransactionFinalized body: the tx
// id to match. The outcome is left to parseFinalized on the REST result; the frame just
// gates that read.
type finalizedEvent struct {
	TransactionID string `json:"transaction_id"`
}

// submitAndWait is the shared submit→wait→parse tail for every real (non-dry-run)
// submission. SubmitSealed (public/generic/cosign) and the stealth send both route through
// it so they share one wait policy. It takes the base64 transaction envelope, submits it,
// waits for finalization (SSE-preferred, REST-poll fallback), and types the raw result via
// the core parser.
func (c *Client) submitAndWait(ctx context.Context, envelopeB64 string) (FinalizedResult, error) {
	fs, ok := c.transport.(finalizationStreamer)
	if !ok || !c.finalizationWait {
		// No streaming capability, or the wait was disabled: pure REST poll.
		txID, err := c.transport.Submit(ctx, envelopeB64)
		if err != nil {
			return FinalizedResult{}, err
		}
		raw, err := c.waitResult(ctx, txID)
		if err != nil {
			return FinalizedResult{}, err
		}
		return c.parseFinalized(raw)
	}

	// Subscribe BEFORE submit. GET /events is a no-replay broadcast, so a frame emitted in
	// the gap between submit and subscribe would be missed. StreamFinalizedEvents opens the
	// HTTP request synchronously, so the subscription is already live once it returns.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, serrs := fs.StreamFinalizedEvents(streamCtx)

	txID, err := c.transport.Submit(ctx, envelopeB64)
	if err != nil {
		return FinalizedResult{}, err
	}

	// Wait for the gate; any fault/close/timeout is swallowed and falls through to the REST
	// poll below (which confirms the result and bounds the wait by ctx). Silent fallback.
	_ = c.awaitFinalized(ctx, txID, events, serrs)

	// Release the SSE connection before polling (a fallback poll may run a while).
	cancel()

	raw, err := c.waitResult(ctx, txID)
	if err != nil {
		return FinalizedResult{}, err
	}
	return c.parseFinalized(raw)
}

// awaitFinalized blocks until a TransactionFinalized frame for txID arrives (returns nil),
// or the stream errors/closes, the timeout fires, or ctx is done (each returns non-nil, so
// the caller falls back to polling). It never blocks forever.
//
// Non-matching frames — other event types, other tx ids, undecodable bodies — are skipped
// and reading continues. Any outcome opens the gate; commit-vs-reject typing is done later
// by parseFinalized on the REST result.
func (c *Client) awaitFinalized(
	ctx context.Context, txID string,
	events <-chan transport.RawSSEEvent, serrs <-chan error,
) error {
	timer := time.NewTimer(c.finalizationTimeout)
	defer timer.Stop()
	for {
		select {
		case ev, open := <-events:
			if !open {
				return errFinalizationStreamClosed
			}
			if ev.Event != finalizedEventName {
				continue
			}
			var fe finalizedEvent
			if json.Unmarshal(ev.Data, &fe) != nil {
				continue // undecodable frame — skip and keep reading
			}
			if fe.TransactionID == txID {
				return nil // gate opened
			}
		case err := <-serrs:
			if err != nil {
				return err
			}
			// serrs yielded nil: the buffered error channel was drained/closed without a
			// terminal error. Treat it as a stream end and fall back.
			return errFinalizationStreamClosed
		case <-timer.C:
			return errFinalizationTimeout
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
