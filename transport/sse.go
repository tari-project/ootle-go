// I/O-only SSE streaming for the indexer's two Server-Sent-Events endpoints:
// GET /transactions/events/stream (template events, with an id: resume cursor) and
// GET /events (the lifecycle broadcast: NewEpoch, TransactionFinalized — no replay/resume).
// Both open the stream, parse the raw SSE framing, and push each frame onto a channel as a
// RawSSEEvent without decoding the data: body. Typed decoding and the reconnect loop live
// one layer up, in the ootle package.
//
// Channel discipline: errs is buffered (cap 1) and carries at most one terminal error;
// both channels are closed exactly once when the stream ends, errors, or ctx is cancelled.
// StreamTransactionEvents and StreamFinalizedEvents are methods on the concrete *Client
// only — they are intentionally not part of the Transport interface.

package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// RawSSEEvent is one dispatched SSE frame, handed up with its data: body left as raw
// JSON. The transport never decodes Data — the caller (ootle.WatchEvents) unmarshals it.
type RawSSEEvent struct {
	// Event is the SSE "event:" field. For /transactions/events/stream this is the
	// event topic (e.g. "std.vault.withdraw").
	Event string
	// ID is the SSE "id:" field — the DB event id (an i64) as a string, "" if absent.
	// It is the resume cursor passed back as the Last-Event-ID header on reconnect.
	ID string
	// Data is the SSE "data:" field — the raw JSON body, NOT decoded by the transport.
	Data json.RawMessage
}

// TransactionEventQuery holds the optional filter query parameters for
// /transactions/events/stream. Empty string fields and a nil AfterID are omitted.
type TransactionEventQuery struct {
	// Topic filters by event topic (exact match); query param ?topic=.
	Topic string
	// SubstateID filters by emitting substate id (e.g. component_<hex>); ?substate_id=.
	SubstateID string
	// TemplateAddress filters by template address; ?template_address=.
	TemplateAddress string
	// ResourceAddress filters by resource address; ?resource_address=.
	ResourceAddress string
	// AfterID resumes from this event id (exclusive); ?after_id=. Usually nil — prefer
	// the Last-Event-ID resume cursor (the lastEventID argument).
	AfterID *int64
}

// streamHTTPClient returns a dedicated *http.Client for the long-lived stream with no
// timeout. The default Client.httpClient carries a 30s DefaultTimeout that would kill a
// stream; ctx is the only deadline here. A shallow copy preserves any custom Transport
// installed via WithHTTPClient without mutating the polling client.
func (c *Client) streamHTTPClient() *http.Client {
	if c.httpClient == nil {
		return &http.Client{} // Timeout 0 ⇒ no timeout; ctx is the only deadline.
	}
	sc := *c.httpClient
	sc.Timeout = 0
	return &sc
}

// StreamTransactionEvents opens GET /transactions/events/stream, parses the raw SSE
// framing, and delivers each frame on the returned out channel as a RawSSEEvent. The
// data: body is handed up as raw JSON — it is NOT decoded here.
//
// q supplies optional filter query params (empty ones omitted). lastEventID, when
// non-empty, is sent as the Last-Event-ID header so the server replays missed events;
// this method sets it once and never reconnects (the reconnect loop is the caller's).
//
// On a request-build, auth, transport, or non-2xx-open failure the returned errs channel
// carries a single *HTTPError and both channels are closed; the read loop never starts.
// On success a reader goroutine runs until: the server closes the stream (io.EOF ⇒ a
// single terminal error on errs), any other read error with ctx still live (that error on
// errs), or ctx is cancelled (clean end, no error pushed). errs is buffered (cap 1); both
// channels are always closed exactly once.
func (c *Client) StreamTransactionEvents(
	ctx context.Context, q TransactionEventQuery, lastEventID string,
) (<-chan RawSSEEvent, <-chan error) {
	full := c.baseURL + "/transactions/events/stream"
	if qs := q.values().Encode(); qs != "" {
		full += "?" + qs
	}
	return c.openStream(ctx, full, lastEventID)
}

// StreamFinalizedEvents opens GET /events — the indexer's top-level lifecycle broadcast
// (NewEpoch, TransactionFinalized) — delivering each frame as a RawSSEEvent. No query
// params, no Last-Event-ID resume.
//
// The stream is a live broadcast with NO replay: a frame emitted before you subscribe is
// lost. Callers MUST open it before submitting the transaction they want to observe. The
// HTTP request runs synchronously here, so the subscription is already live once this
// returns — a submit afterwards cannot race ahead of it.
//
// Failure and channel-close discipline match StreamTransactionEvents.
func (c *Client) StreamFinalizedEvents(ctx context.Context) (<-chan RawSSEEvent, <-chan error) {
	return c.openStream(ctx, c.baseURL+"/events", "")
}

// openStream builds and opens an SSE GET request at full (which already includes any query
// string), sets the SSE Accept header and an optional Last-Event-ID, authorizes it, and on
// a 2xx starts the readStream reader goroutine. The Do() is synchronous, so by the time the
// channels are returned the server has accepted the subscription — callers relying on
// subscribe-before-submit can depend on that ordering.
//
// On a request-build, auth, transport, or non-2xx-open failure the returned errs channel
// carries a single *HTTPError and both channels are closed; the read loop never starts.
// errs is buffered (cap 1); both channels are always closed exactly once.
func (c *Client) openStream(ctx context.Context, full, lastEventID string) (<-chan RawSSEEvent, <-chan error) {
	const method = http.MethodGet

	// fail returns a pair of channels carrying a single terminal *HTTPError on errs,
	// both already closed, with no reader goroutine started.
	fail := func(err *HTTPError) (<-chan RawSSEEvent, <-chan error) {
		out := make(chan RawSSEEvent)
		errs := make(chan error, 1)
		errs <- err
		close(out)
		close(errs)
		return out, errs
	}

	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return fail(&HTTPError{Method: method, URL: full, Err: err})
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if err := c.auth.Authorize(req); err != nil {
		return fail(&HTTPError{Method: method, URL: full, Err: err})
	}

	resp, err := c.streamHTTPClient().Do(req)
	if err != nil {
		return fail(&HTTPError{Method: method, URL: full, Err: err})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return fail(&HTTPError{Method: method, URL: full, Status: resp.StatusCode, Body: string(body)})
	}

	out := make(chan RawSSEEvent)
	errs := make(chan error, 1)
	go readStream(ctx, resp, out, errs)
	return out, errs
}

// values builds the query parameters, skipping empty strings and a nil AfterID.
func (q TransactionEventQuery) values() url.Values {
	v := url.Values{}
	if q.Topic != "" {
		v.Set("topic", q.Topic)
	}
	if q.SubstateID != "" {
		v.Set("substate_id", q.SubstateID)
	}
	if q.TemplateAddress != "" {
		v.Set("template_address", q.TemplateAddress)
	}
	if q.ResourceAddress != "" {
		v.Set("resource_address", q.ResourceAddress)
	}
	if q.AfterID != nil {
		v.Set("after_id", strconv.FormatInt(*q.AfterID, 10))
	}
	return v
}

// readStream is the reader goroutine: it parses the SSE framing off resp.Body and emits
// one RawSSEEvent per dispatched frame on out. It owns closing resp.Body, out, and errs.
func readStream(ctx context.Context, resp *http.Response, out chan<- RawSSEEvent, errs chan<- error) {
	defer resp.Body.Close()
	defer close(out)
	defer close(errs)

	r := bufio.NewReader(resp.Body)
	var (
		event   string
		id      string
		data    strings.Builder
		hasData bool
	)

	// dispatch sends the accumulated frame on out and resets the per-frame accumulators.
	// It reports whether ctx was cancelled (caller must stop). A frame with no data: line
	// (e.g. only keep-alive comments) is dropped, never emitted as an empty RawSSEEvent.
	dispatch := func() (cancelled bool) {
		if hasData {
			ev := RawSSEEvent{Event: event, ID: id, Data: json.RawMessage(data.String())}
			select {
			case out <- ev:
			case <-ctx.Done():
				return true
			}
		}
		event, id, hasData = "", "", false
		data.Reset()
		return false
	}

	for {
		line, err := r.ReadString('\n')
		// Process the line content first: a final line without a trailing newline still
		// arrives alongside io.EOF and must be parsed before we act on the error.
		if line != "" {
			trimmed := strings.TrimRight(line, "\n")
			trimmed = strings.TrimRight(trimmed, "\r")
			switch {
			case trimmed == "":
				if dispatch() {
					return
				}
			case strings.HasPrefix(trimmed, ":"):
				// Keep-alive comment line — ignore.
			default:
				field, value := splitField(trimmed)
				switch field {
				case "event":
					event = value
				case "id":
					id = value
				case "data":
					if hasData {
						data.WriteByte('\n')
					}
					data.WriteString(value)
					hasData = true
				}
				// Unknown fields are ignored.
			}
		}

		if err != nil {
			if ctx.Err() != nil {
				// ctx cancellation surfaces here as a body read error; treat as a clean
				// end and do not push a terminal error.
				return
			}
			// Flush any pending frame the server left undispatched before reporting EOF.
			if errors.Is(err, io.EOF) {
				if dispatch() {
					return
				}
			}
			errs <- &HTTPError{Method: http.MethodGet, URL: resp.Request.URL.String(), Err: err}
			return
		}
	}
}

// splitField splits an SSE line into its field name and value on the first ':',
// stripping exactly one leading space from the value (per the SSE spec — never
// TrimSpace, which would corrupt JSON and the multi-line "\n" joins). A line with no
// ':' is treated as a field with an empty value.
func splitField(line string) (field, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, ""
	}
	field = line[:i]
	value = line[i+1:]
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	return field, value
}
