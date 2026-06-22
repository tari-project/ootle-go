// Typed, reconnecting event streaming over the indexer's
// GET /transactions/events/stream endpoint.
//
// Each raw SSE frame from transport.StreamTransactionEvents is decoded into a typed Event,
// and a reconnect-with-Last-Event-ID loop with backoff is layered on top. Decoding is plain
// JSON; the streamed Event's flat string→string payload is distinct from result.go's
// EventSummary/EventPayload (the finalized-result [key,value] tuple shape).

package ootle

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// reconnectBackoffMin / reconnectBackoffMax bound the exponential backoff between
// reconnect attempts in WatchEvents: start at 1s, double up to a 5s cap, reset to the
// minimum after a connection that delivered at least one frame.
const (
	reconnectBackoffMin = time.Second
	reconnectBackoffMax = 5 * time.Second
)

// EventFilter selects which transaction events WatchEvents streams. Empty string fields
// are unset (no filter on that dimension). It mirrors transport.TransactionEventQuery.
type EventFilter struct {
	// Topic filters by exact event topic (e.g. "std.vault.withdraw").
	Topic string
	// SubstateID filters by emitting substate id (e.g. component_<hex>).
	SubstateID string
	// TemplateAddress filters by emitting template address.
	TemplateAddress string
	// ResourceAddress filters by resource address.
	ResourceAddress string
	// AfterID seeds the resume cursor on the first connect (exclusive: events strictly after
	// this id). Usually nil — thereafter the last delivered event id is the cursor.
	AfterID *int64
}

// Event is one decoded transaction event delivered by WatchEvents. It is distinct from
// result.go's EventSummary (the finalized-result shape): the streamed payload is a flat
// string→string map, not a [key,value] tuple list.
type Event struct {
	// ID is the DB event id (the SSE id: field parsed to int64), the resume cursor's value.
	// It is 0 for the rare live frame that carries no id:.
	ID int64
	// Topic is the event topic (the SSE event: field); it is absent from the data: body.
	Topic string
	// TransactionID is the hex transaction id that emitted the event.
	TransactionID string
	// SubstateID is the emitting substate id, or "" when the engine event had none
	// (the wire substate_id was null).
	SubstateID string
	// TemplateAddress is the address of the template that emitted the event.
	TemplateAddress string
	// Payload is the event payload as a flat string→string map. It is nil when the wire
	// payload was absent or null.
	Payload map[string]string
}

// sseTxEvent mirrors the indexer's data: body for /transactions/events/stream. The topic
// rides the SSE event: field only and is intentionally absent here.
type sseTxEvent struct {
	TransactionID string `json:"transaction_id"`
	Event         struct {
		SubstateID      *string           `json:"substate_id"` // nullable
		TemplateAddress string            `json:"template_address"`
		Payload         map[string]string `json:"payload"`
	} `json:"event"`
}

// eventStreamer is the optional streaming capability of a transport. The concrete
// *transport.Client satisfies it; a plain 3-method transport.Transport mock does not.
// WatchEvents reaches the stream by type-asserting the Client's stored Transport to this
// interface rather than widening Transport (which would break every 3-method mock).
type eventStreamer interface {
	StreamTransactionEvents(ctx context.Context, q transport.TransactionEventQuery, lastEventID string) (<-chan transport.RawSSEEvent, <-chan error)
}

// filterToQuery copies an EventFilter into the transport's query shape (a trivial
// field-for-field copy, including the AfterID resume seed).
func filterToQuery(f EventFilter) transport.TransactionEventQuery {
	return transport.TransactionEventQuery{
		Topic:           f.Topic,
		SubstateID:      f.SubstateID,
		TemplateAddress: f.TemplateAddress,
		ResourceAddress: f.ResourceAddress,
		AfterID:         f.AfterID,
	}
}

// decode maps one raw SSE frame to a typed Event. The topic comes from raw.Event, the id
// from raw.ID (parsed to int64; "" → 0, a live frame may lack an id: only in edge cases),
// and the rest from the JSON data: body. A nil/null wire payload leaves Event.Payload nil.
func decode(raw transport.RawSSEEvent) (Event, error) {
	var body sseTxEvent
	if err := json.Unmarshal(raw.Data, &body); err != nil {
		return Event{}, err
	}

	var id int64
	if raw.ID != "" {
		parsed, err := strconv.ParseInt(raw.ID, 10, 64)
		if err != nil {
			return Event{}, err
		}
		id = parsed
	}

	substateID := ""
	if body.Event.SubstateID != nil {
		substateID = *body.Event.SubstateID
	}

	return Event{
		ID:              id,
		Topic:           raw.Event,
		TransactionID:   body.TransactionID,
		SubstateID:      substateID,
		TemplateAddress: body.Event.TemplateAddress,
		Payload:         body.Event.Payload,
	}, nil
}

// WatchEvents streams transaction events matching filter as typed Events. It returns
// immediately; the work runs in a goroutine and both channels are closed exactly once when
// the watch ends. If the transport does not support streaming, one terminal *Error (code
// "INVALID") is delivered on errs and both channels close.
//
// Across mid-stream failures it reconnects automatically, resuming from the last delivered
// event id so the server replays anything missed (filter.AfterID seeds the cursor only on
// the first connect). Reconnect errors are surfaced on errs as non-fatal warnings; reading
// only the events channel never deadlocks the loop. Reconnect backoff grows from 1s to a 5s
// cap. Delivery is at-least-once: a repeated event id may appear across a reconnect boundary,
// and callers must tolerate it.
//
// A single undecodable frame is skipped and the stream continues; errs carries only
// connection-level problems. Cancelling ctx ends the watch cleanly with no error emitted.
func (c *Client) WatchEvents(ctx context.Context, filter EventFilter) (<-chan Event, <-chan error) {
	s, ok := c.transport.(eventStreamer)
	if !ok {
		out := make(chan Event)
		errs := make(chan error, 1)
		errs <- &Error{Code: "INVALID", Message: "transport does not support event streaming"}
		close(out)
		close(errs)
		return out, errs
	}

	out := make(chan Event)
	errs := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errs)

		// lastID is the raw id: string of the last emitted frame; it round-trips verbatim as
		// the Last-Event-ID header (parse to int64 only for the typed Event.ID). Seed it from
		// the filter's AfterID so the first connect resumes from there.
		lastID := ""
		if filter.AfterID != nil {
			lastID = strconv.FormatInt(*filter.AfterID, 10)
		}
		backoff := reconnectBackoffMin

		for ctx.Err() == nil {
			q := filterToQuery(filter)
			// Send ?after_id= only on the first connect (empty lastID). Once lastID is set,
			// the Last-Event-ID header is the single cursor — sending both is redundant.
			if lastID != "" {
				q.AfterID = nil
			}

			raw, serrs := s.StreamTransactionEvents(ctx, q, lastID)

			delivered := false
			for rawEv := range raw {
				ev, err := decode(rawEv)
				if err != nil {
					// Bad frame: skip and keep streaming (see doc comment).
					continue
				}
				// Advance the cursor from the raw id: string so the header round-trips verbatim.
				lastID = rawEv.ID
				delivered = true
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}

			// The transport closed out; read its single terminal error (if any).
			err := <-serrs

			if ctx.Err() != nil {
				// Clean cancel: no error emitted.
				return
			}

			// Mid-stream failure with ctx still live: surface as a non-fatal warning
			// (non-blocking so a slow consumer can't deadlock the reconnect), then back off.
			if err != nil {
				select {
				case errs <- err:
				default:
				}
			}

			// Reset backoff after a productive connection; otherwise grow it (capped).
			if delivered {
				backoff = reconnectBackoffMin
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			if !delivered {
				if backoff *= 2; backoff > reconnectBackoffMax {
					backoff = reconnectBackoffMax
				}
			}
		}
	}()

	return out, errs
}
