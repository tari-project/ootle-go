// Package transport is the thin HTTP boundary to a Tari Ootle indexer's REST API.
//
// It does I/O and JSON marshalling and nothing else: it fetches the substates the
// core asked for, submits the already-sealed transaction envelope, and fetches the
// raw finalized result. It deliberately holds NO domain logic — no encoding, no
// want-derivation, no result-typing. Those all live in the Rust core (reached via
// the parent ootle package + internal/cffi). This package never imports internal/cffi
// and never decodes engine types such as SubstateValue or ExecuteResult.
//
// # Indexer, not wallet daemon (VR8)
//
// The transport targets the indexer REST API (default 127.0.0.1:18300):
//
//	POST /transactions             — submit the base64 transaction envelope
//	POST /transactions/dry-run     — evaluate a dry-run envelope, result returned inline
//	GET  /transactions/{id}/result — fetch the raw IndexerTransactionFinalizedResult
//	POST /substates/fetch          — batch-fetch substates (chunked at 20)
//	GET  /substates/{id}           — single substate fetch (404 ⇒ not found)
//	GET  /events                   — lifecycle SSE broadcast (TransactionFinalized); the
//	                                 finalization wait subscribes here (see sse.go). It is a
//	                                 *Client streaming method, not part of the Transport interface.
//
// It deliberately does NOT use the wallet daemon's JSON-RPC transactions.submit:
// that path runs server-side detect_inputs, which would resolve inputs on the server
// and bypass the core's two-phase input resolution. The indexer path needs no JWT
// handshake; auth is an optional, pluggable seam (see Authorizer) reserved for a
// future wallet-daemon backend.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the indexer's default REST API address
// (applications/tari_indexer/src/config.rs: api_listen_address = 127.0.0.1:18300).
const DefaultBaseURL = "http://127.0.0.1:18300"

// fetchChunk is the maximum number of substate ids per POST /substates/fetch call.
// The indexer accepts up to 50, but the Python transport and the Rust resolver both
// chunk at 20; match them to stay within the tested limits.
const fetchChunk = 20

// DefaultTimeout is the per-request timeout applied when the caller does not supply
// an *http.Client of their own.
const DefaultTimeout = 30 * time.Second

// FetchedSubstate is the boundary record the core's apply_fetched_substates consumes.
// It mirrors crates/ootle_sdk_core/src/inputs.rs::FetchedSubstate exactly:
//
//	{ "substate_id": <id>, "version": <u32>, "substate_value": <SubstateValue JSON> }
//
// SubstateValue is carried verbatim as json.RawMessage — the transport never decodes
// it; the core parses it into the internal SubstateValue (a malformed value ⇒ the
// core's PARSE error). The indexer returns each substate as { "version", "substate" };
// FetchSubstates does the trivial field rename (substate → substate_value, map key →
// substate_id) and nothing more.
type FetchedSubstate struct {
	SubstateID    string          `json:"substate_id"`
	Version       uint32          `json:"version"`
	SubstateValue json.RawMessage `json:"substate_value"`
}

// HTTPError is the typed transport error for a non-2xx (and non-allowed-404) HTTP
// response, or a request/transport failure. It is deliberately distinct from the
// core's ootle.Error so callers can tell an I/O failure from a core-logic failure.
type HTTPError struct {
	// Method and URL identify the failed request.
	Method string
	URL    string
	// Status is the HTTP status code, or 0 for a transport-level failure (the request
	// never produced a response — e.g. connection refused, timeout, context cancelled).
	Status int
	// Body is the response body (truncated by the server's limits), empty for a
	// transport-level failure.
	Body string
	// Err is the underlying transport error, if any (nil for an HTTP-status error).
	Err error
}

func (e *HTTPError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("indexer request failed: %s %s: %v", e.Method, e.URL, e.Err)
	}
	if e.Body == "" {
		return fmt.Sprintf("indexer returned HTTP %d for %s %s", e.Status, e.Method, e.URL)
	}
	return fmt.Sprintf("indexer returned HTTP %d for %s %s: %s", e.Status, e.Method, e.URL, e.Body)
}

// Unwrap exposes the underlying transport error for errors.Is/As.
func (e *HTTPError) Unwrap() error { return e.Err }

// Authorizer is the pluggable auth seam. The indexer REST path needs none, so the
// default is a no-op. A future wallet-daemon backend can implement the
// auth.method/auth.request + Bearer-JWT handshake here without reshaping the transport.
type Authorizer interface {
	// Authorize mutates the outgoing request to add credentials (e.g. an Authorization
	// header). It is called on every request just before it is sent.
	Authorize(req *http.Request) error
}

// noopAuthorizer is the default Authorizer: it adds nothing. Reflects VR8 — the indexer
// REST path is intentionally unauthenticated; the daemon JSON-RPC path (which would
// bypass core resolution) is not used.
type noopAuthorizer struct{}

func (noopAuthorizer) Authorize(*http.Request) error { return nil }

// Transport is the contract the two-phase driver depends on. *Client
// satisfies it; tests substitute an httptest-backed *Client or a hand-rolled mock.
type Transport interface {
	// FetchSubstates fetches the given substate ids (chunked at 20) and returns them
	// in the core's FetchedSubstate shape, ready to hand to apply_fetched_substates.
	// Ids the indexer does not return are simply absent from the slice (not an error).
	FetchSubstates(ctx context.Context, ids []string) ([]FetchedSubstate, error)
	// Submit posts the base64 transaction envelope and returns the transaction id.
	Submit(ctx context.Context, envelopeB64 string) (txID string, err error)
	// SubmitDryRun posts the base64 envelope to /transactions/dry-run, which evaluates it
	// without committing and returns the raw IndexerTransactionFinalizedResult synchronously
	// (no polling). The envelope's dry_run flag must be set.
	SubmitDryRun(ctx context.Context, envelopeB64 string) (raw json.RawMessage, err error)
	// GetResult fetches the raw IndexerTransactionFinalizedResult for txID. raw is the
	// indexer's `result` value passed through verbatim (what the core's
	// parse_finalized_result consumes); finalized is true unless the result is the
	// "Pending" variant. No domain decoding happens here.
	GetResult(ctx context.Context, txID string) (raw json.RawMessage, finalized bool, err error)
}

// compile-time assertion that *Client implements Transport.
var _ Transport = (*Client)(nil)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient supplies a custom *http.Client (e.g. with a tuned transport or
// timeout). When set, the Client does not impose its own timeout.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.httpClient = c }
}

// WithAuthorizer installs an Authorizer (default is a no-op).
func WithAuthorizer(a Authorizer) Option {
	return func(cl *Client) {
		if a != nil {
			cl.auth = a
		}
	}
}

// Client is the concrete net/http transport against one indexer base URL.
type Client struct {
	baseURL    string
	httpClient *http.Client
	auth       Authorizer
}

// NewClient builds a Client for the indexer at baseURL (use DefaultBaseURL for the
// local default). The base URL's trailing slash is trimmed. With no WithHTTPClient
// option, a default *http.Client with DefaultTimeout is used.
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    noopAuthorizer{},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return c
}

// BaseURL returns the configured indexer base URL (trailing slash trimmed).
func (c *Client) BaseURL() string { return c.baseURL }

// ---- request shapes (match the indexer's tari_indexer_client::types) ----

// submitRequest is the body of POST /transactions (SubmitTransactionRequest).
type submitRequest struct {
	Transaction string `json:"transaction"`
}

// submitResponse is the body of SubmitTransactionResponse.
type submitResponse struct {
	TransactionID string `json:"transaction_id"`
}

// fetchRequest is the body of POST /substates/fetch (GetSubstatesRequest).
type fetchRequest struct {
	Requests   []string `json:"requests"`
	CachedOnly bool     `json:"cached_only"`
}

// indexerSubstate is one entry of GetSubstatesResponse.substates (and the body of
// GET /substates/{id}): { "version", "substate" }. `substate` is the SubstateValue JSON,
// carried verbatim.
type indexerSubstate struct {
	Version  uint32          `json:"version"`
	Substate json.RawMessage `json:"substate"`
}

// fetchResponse is the body of GetSubstatesResponse.
type fetchResponse struct {
	Substates map[string]indexerSubstate `json:"substates"`
}

// resultResponse is the body of GET /transactions/{id}/result
// (GetTransactionResultResponse). `result` is the raw IndexerTransactionFinalizedResult.
type resultResponse struct {
	Result json.RawMessage `json:"result"`
}

// ---- the calls the two-phase flow needs ----

// FetchSubstates implements Transport.FetchSubstates. It chunks ids at fetchChunk,
// POSTs each chunk to /substates/fetch, and merges the responses into a single slice
// of FetchedSubstate. The order follows the input ids (then chunk order); substates the
// indexer omits are simply absent (no error). The reshape from the indexer's
// { version, substate } map to FetchedSubstate is trivial field renaming only.
func (c *Client) FetchSubstates(ctx context.Context, ids []string) ([]FetchedSubstate, error) {
	out := make([]FetchedSubstate, 0, len(ids))
	for start := 0; start < len(ids); start += fetchChunk {
		end := start + fetchChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		body, err := json.Marshal(fetchRequest{Requests: chunk, CachedOnly: false})
		if err != nil {
			return nil, fmt.Errorf("transport: marshal fetch request: %w", err)
		}
		raw, err := c.do(ctx, http.MethodPost, "/substates/fetch", body, false)
		if err != nil {
			return nil, err
		}
		var resp fetchResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("transport: decode /substates/fetch response: %w", err)
		}
		// Preserve request order within the chunk so the result is deterministic
		// (map iteration is not). Substates absent from the response are skipped.
		for _, id := range chunk {
			sub, ok := resp.Substates[id]
			if !ok {
				continue
			}
			out = append(out, FetchedSubstate{
				SubstateID:    id,
				Version:       sub.Version,
				SubstateValue: sub.Substate,
			})
		}
	}
	return out, nil
}

// FetchSubstate fetches a single substate via GET /substates/{id}. A 404 ⇒ (nil, nil)
// (definitively not found, not an error), matching the Python transport. The returned
// FetchedSubstate carries the SubstateValue verbatim for the core to parse.
func (c *Client) FetchSubstate(ctx context.Context, id string) (*FetchedSubstate, error) {
	path := "/substates/" + url.PathEscape(id)
	raw, err := c.do(ctx, http.MethodGet, path, nil, true)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil // 404 → not found
	}
	var sub indexerSubstate
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, fmt.Errorf("transport: decode /substates/%s response: %w", id, err)
	}
	return &FetchedSubstate{
		SubstateID:    id,
		Version:       sub.Version,
		SubstateValue: sub.Substate,
	}, nil
}

// Submit implements Transport.Submit: POST /transactions with the base64 envelope,
// returning the transaction id. This is the only write call; it is never retried.
func (c *Client) Submit(ctx context.Context, envelopeB64 string) (string, error) {
	body, err := json.Marshal(submitRequest{Transaction: envelopeB64})
	if err != nil {
		return "", fmt.Errorf("transport: marshal submit request: %w", err)
	}
	raw, err := c.do(ctx, http.MethodPost, "/transactions", body, false)
	if err != nil {
		return "", err
	}
	var resp submitResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("transport: decode /transactions response: %w", err)
	}
	return resp.TransactionID, nil
}

// SubmitDryRun implements Transport.SubmitDryRun: POST /transactions/dry-run with the
// base64 envelope, returning the response body verbatim. The body is the
// SubmitTransactionDryRunResponse the core's parse_finalized_result decodes (it routes on
// the top-level `result` key). Never retried.
func (c *Client) SubmitDryRun(ctx context.Context, envelopeB64 string) (json.RawMessage, error) {
	body, err := json.Marshal(submitRequest{Transaction: envelopeB64})
	if err != nil {
		return nil, fmt.Errorf("transport: marshal dry-run request: %w", err)
	}
	raw, err := c.do(ctx, http.MethodPost, "/transactions/dry-run", body, false)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// GetResult implements Transport.GetResult: GET /transactions/{id}/result. It unwraps
// the response envelope's `result` field and returns it verbatim — this is exactly the
// raw IndexerTransactionFinalizedResult the core's parse_finalized_result decodes. The
// finalized flag is derived ONLY from the Pending-vs-not distinction (the "Pending"
// JSON string); the body is never decoded here.
func (c *Client) GetResult(ctx context.Context, txID string) (json.RawMessage, bool, error) {
	path := "/transactions/" + url.PathEscape(txID) + "/result"
	raw, err := c.do(ctx, http.MethodGet, path, nil, false)
	if err != nil {
		return nil, false, err
	}
	var resp resultResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, fmt.Errorf("transport: decode /transactions/%s/result response: %w", txID, err)
	}
	finalized := !isPending(resp.Result)
	return resp.Result, finalized, nil
}

// WaitResult polls GetResult until the result is finalized or ctx is done. It is
// deliberately dumb: it does no domain decoding and treats any non-Pending result as
// finalized (the core types it). The poll interval defaults to 1s when interval <= 0.
func (c *Client) WaitResult(ctx context.Context, txID string, interval time.Duration) (json.RawMessage, error) {
	if interval <= 0 {
		interval = time.Second
	}
	for {
		raw, finalized, err := c.GetResult(ctx, txID)
		if err != nil {
			return nil, err
		}
		if finalized {
			return raw, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// isPending reports whether the raw IndexerTransactionFinalizedResult is the "Pending"
// unit variant (serde serialises a unit enum variant as the bare JSON string "Pending").
// Any other shape (an object, or the "Rejected" variant) is considered finalized — the
// core's parser types it.
func isPending(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return string(trimmed) == `"Pending"`
}

// do builds and sends one request, applies the Authorizer, and returns the raw response
// body. When allow404 is true, a 404 yields (nil, nil). Any other non-2xx, or a
// transport failure, yields an *HTTPError. The body is JSON when non-nil; Content-Type
// is set accordingly.
func (c *Client) do(ctx context.Context, method, path string, body []byte, allow404 bool) ([]byte, error) {
	full := c.baseURL + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return nil, &HTTPError{Method: method, URL: full, Err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if err := c.auth.Authorize(req); err != nil {
		return nil, &HTTPError{Method: method, URL: full, Err: err}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &HTTPError{Method: method, URL: full, Err: err}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if allow404 && resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	// A read failure must be surfaced before status branching: otherwise a partial
	// body would be handed to the JSON decoder (2xx) or buried in an HTTPError (non-2xx).
	if readErr != nil {
		return nil, &HTTPError{Method: method, URL: full, Status: resp.StatusCode, Err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{
			Method: method,
			URL:    full,
			Status: resp.StatusCode,
			Body:   string(respBody),
		}
	}
	return respBody, nil
}
