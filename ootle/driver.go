package ootle

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// maxResolutionRounds caps the two-phase resolution loop. Realistic depth is 1–2
// round-trips (ledger VR4); 3 is the safety cap. An uncapped loop on a misbehaving
// indexer would hang — past the cap the driver returns ErrResolutionDidNotConverge.
const maxResolutionRounds = 3

// defaultPollInterval is how often SendPublicTransfer polls the indexer for the
// finalized result while waiting.
const defaultPollInterval = time.Second

// ErrResolutionDidNotConverge is returned when the two-phase input resolution loop does
// not reach the Resolved state within maxResolutionRounds. It is wrapped in an *Error
// with code "RESOLUTION" so callers can branch with errors.Is on this sentinel and also
// inspect the stable code via errors.As(*Error).
var ErrResolutionDidNotConverge = errors.New("ootle: input resolution did not converge within the round cap")

// Client is the headline Go SDK surface: it composes the cgo two-phase core calls with
// a Transport into a single SendPublicTransfer call. The Client owns only the driver
// loop, cgo marshalling, and ergonomics; every value-critical operation (encoding,
// want-derivation, sealing, result typing) is a core call. It is safe for concurrent
// use as long as the supplied Transport is.
type Client struct {
	transport transport.Transport
	// network is the L1 network this client targets. Every Send*/PrepareCosign method reads
	// it; it is set once via WithNetwork. A zero value (unset) makes those methods return a
	// VALIDATION error.
	network      Network
	pollInterval time.Duration
	// finalizationWait enables the SSE-preferred finalization wait (on by default). It only
	// takes effect when the transport implements finalizationStreamer.
	finalizationWait bool
	// finalizationTimeout bounds the SSE phase of the wait before falling back to polling.
	finalizationTimeout time.Duration
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithNetwork sets the L1 network this client targets. Every Send*/PrepareCosign method
// reads it, so it need not be repeated per call. Without it those methods return a
// VALIDATION error.
func WithNetwork(n Network) ClientOption {
	return func(c *Client) { c.network = n }
}

// WithPollInterval sets the indexer result-polling interval used while waiting for a
// finalized result (default 1s). Values <= 0 are ignored.
func WithPollInterval(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.pollInterval = d
		}
	}
}

// WithFinalizationTimeout tunes the safety-net timeout on the SSE finalization wait before
// it falls back to REST polling (default 30s). The surrounding context remains the hard
// deadline; this only bounds the SSE phase. Values <= 0 are ignored.
func WithFinalizationTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.finalizationTimeout = d
		}
	}
}

// WithoutFinalizationWait disables the SSE finalization wait; every real submission then
// uses the pure REST result-poll. The wait is on by default.
func WithoutFinalizationWait() ClientOption {
	return func(c *Client) { c.finalizationWait = false }
}

// NewClient builds a Client over the given Transport (e.g. transport.NewClient(baseURL)
// for a live indexer, or a mock for tests).
func NewClient(t transport.Transport, opts ...ClientOption) *Client {
	c := &Client{
		transport:           t,
		pollInterval:        defaultPollInterval,
		finalizationWait:    true,
		finalizationTimeout: defaultFinalizationTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect builds a Client over a fresh indexer transport at baseURL. It is shorthand for
// NewClient(transport.NewClient(baseURL), opts...); pair it with WithNetwork to set the
// target network.
func Connect(baseURL string, opts ...ClientOption) *Client {
	return NewClient(transport.NewClient(baseURL), opts...)
}

// resolveNetworkByte maps a client's configured network to its byte value, distinguishing
// an unset network (no WithNetwork) from an unknown one for a clear VALIDATION error.
func resolveNetworkByte(network Network) (uint8, error) {
	if network == "" {
		return 0, &Error{Code: "VALIDATION", Message: "no network configured on the client (use WithNetwork)"}
	}
	netByte, ok := network.ByteValue()
	if !ok {
		return 0, &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	return netByte, nil
}

// SendPublicTransfer drives a public transfer end-to-end against the indexer, using the
// production (random-nonce) seal: build unsigned → resolve inputs (bounded two-phase
// loop) → seal+encode → submit → wait → parse. This is the path production callers want;
// the encoded bytes are NOT reproducible (random seal nonce).
//
// The intent's Inputs must be empty: SendPublicTransfer is the resolved path. A non-empty
// Inputs would make the core short-circuit to the explicit path and skip resolution; the
// driver rejects it with a VALIDATION error rather than silently change behaviour.
//
// The opaque core handle is freed on every path (success, error, panic). Context
// cancellation propagates to the transport.
func (c *Client) SendPublicTransfer(ctx context.Context, intent PublicTransferIntent, keys PublicTransferKeys) (FinalizedResult, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return c.sendPublicTransfer(ctx, c.network, intent, string(keysJSON), cffi.SealAndEncode)
}

// --- Deterministic / reproducible-build API ---------------------------------------------
//
// The methods below pin a single build seed so the encoded bytes are reproducible byte-for-byte.
// Production callers want SendPublicTransfer and PublicTransferKeys; reach for these only
// when you need byte parity for an identical build.

// SendPublicTransferDeterministic is the seed-reproducible counterpart of SendPublicTransfer;
// production callers should use SendPublicTransfer. With fixed fetched substates and a pinned
// seed the sealed bytes/id are reproducible byte-for-byte. Everything else (the resolution
// loop, submit, wait, parse, handle lifetime) is identical.
func (c *Client) SendPublicTransferDeterministic(ctx context.Context, intent PublicTransferIntent, keys DeterministicTransferKeys) (FinalizedResult, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return c.sendPublicTransfer(ctx, c.network, intent, string(keysJSON), cffi.SealAndEncodeWithSeed)
}

// sealFunc is the seal+encode core call (random-nonce or seed-reproducible). It CONSUMES the
// handle. Both cffi.SealAndEncode and cffi.SealAndEncodeWithSeed satisfy it.
type sealFunc func(h *cffi.Handle, keysJSON string) (string, error)

// sendPublicTransfer is the shared two-phase driver for both key paths. seal is the
// handle-consuming seal+encode core call.
//
// Handle lifetime: BuildUnsigned returns a handle the driver owns. ApplyFetchedSubstates
// and seal CONSUME the handle (the cffi wrapper nils the *Handle it was given even on
// error). The deferred guard frees whatever handle the driver currently owns; once a
// consuming call takes ownership, the driver re-points `handle` at the returned handle
// (or nil), so the guard never double-frees a consumed handle and always frees the live
// one on every early return (resolution failure, transport error, parse error) and panic.
func (c *Client) sendPublicTransfer(ctx context.Context, network Network, intent PublicTransferIntent, keysJSON string, seal sealFunc) (result FinalizedResult, err error) {
	netByte, nErr := resolveNetworkByte(network)
	if nErr != nil {
		return FinalizedResult{}, nErr
	}
	// Empty explicit inputs ⇒ the resolved path. A non-empty set would make the core
	// short-circuit the explicit path and never run the wants loop (gotcha in the step
	// doc); reject it loudly instead of silently changing semantics.
	if len(intent.Inputs) != 0 {
		return FinalizedResult{}, &Error{
			Code:    "VALIDATION",
			Message: "SendPublicTransfer requires an empty intent.Inputs (the resolved path); supply explicit inputs only via the explicit-path API",
		}
	}

	intentJSON, mErr := intent.marshalJSON()
	if mErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal intent: %v", mErr)}
	}

	handle, wantListJSON, cErr := cffi.BuildUnsigned(netByte, string(intentJSON))
	if cErr != nil {
		return FinalizedResult{}, fromCffiError(cErr)
	}
	// Free the live handle on every exit path (incl. panic). After a consuming call the
	// driver re-points `handle`, so this frees the live one, never a consumed one.
	defer func() { cffi.FreeHandle(handle) }()

	// --- Phase 2: bounded resolution loop ----------------------------------------------
	if rErr := c.resolveInputs(ctx, &handle, wantListJSON); rErr != nil {
		return FinalizedResult{}, rErr
	}

	// --- Seal + encode (consumes the handle) -------------------------------------------
	encodedJSON, sErr := seal(handle, keysJSON)
	handle = nil // consumed; the guard must not free it
	if sErr != nil {
		return FinalizedResult{}, fromCffiError(sErr)
	}
	var encoded EncodedPublicTransfer
	if uErr := json.Unmarshal([]byte(encodedJSON), &encoded); uErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal encoded transfer: %v", uErr)}
	}

	// A dry run evaluates without committing via a dedicated endpoint that returns the
	// result inline; the normal path submits and polls.
	if intent.DryRun {
		return c.submitDryRun(ctx, encoded)
	}
	return c.SubmitSealed(ctx, encoded)
}

// submitDryRun submits an already-sealed dry-run envelope to /transactions/dry-run and
// types the inline result. No polling — the indexer evaluates it synchronously.
func (c *Client) submitDryRun(ctx context.Context, encoded EncodedPublicTransfer) (FinalizedResult, error) {
	envelopeB64, err := hexToBase64(encoded.EncodedTransaction)
	if err != nil {
		return FinalizedResult{}, err
	}
	raw, err := c.transport.SubmitDryRun(ctx, envelopeB64)
	if err != nil {
		return FinalizedResult{}, err
	}
	return c.parseFinalized(raw)
}

// SubmitSealed submits an already-sealed, BOR-encoded transaction (e.g. from
// CosignSealer.SealWithAuth) and waits for the finalized, typed result. It is the
// submit→wait→parse tail of the Send* drivers, exposed for callers that sealed
// out-of-band — co-signing, offline signing, or an HSM hand-off. The wait is
// SSE-preferred with a REST-poll fallback (see submitAndWait / finalization.go).
func (c *Client) SubmitSealed(ctx context.Context, encoded EncodedPublicTransfer) (FinalizedResult, error) {
	envelopeB64, err := hexToBase64(encoded.EncodedTransaction)
	if err != nil {
		return FinalizedResult{}, err
	}
	return c.submitAndWait(ctx, envelopeB64)
}

// parseFinalized types a raw IndexerTransactionFinalizedResult via the core's parser.
func (c *Client) parseFinalized(raw json.RawMessage) (FinalizedResult, error) {
	parsedJSON, err := cffi.ParseFinalizedResult(string(raw))
	if err != nil {
		return FinalizedResult{}, fromCffiError(err)
	}
	var result FinalizedResult
	if err := json.Unmarshal([]byte(parsedJSON), &result); err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal finalized result: %v", err)}
	}
	return result, nil
}

// waitResult polls the transport's GetResult until the result is finalized or ctx is
// done. It mirrors transport.Client.WaitResult but works against any Transport (so the
// mock-transport integration test drives the same path). It does no domain decoding —
// the core's parse_finalized_result types the raw bytes.
func (c *Client) waitResult(ctx context.Context, txID string) (json.RawMessage, error) {
	interval := c.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	for {
		raw, finalized, err := c.transport.GetResult(ctx, txID)
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

// resolveInputs drives the bounded two-phase resolution loop from a build's want-list to a
// fully-resolved handle. It is shared by every public-path driver (SendPublicTransfer,
// SendInstructions, PrepareCosign): they differ only in the phase-1 build call, never the loop.
//
// *handle is the live, caller-owned handle; resolveInputs re-points it through each
// ApplyFetchedSubstates consume (the cffi wrapper nils the handle it is given and hands back the
// next one), so the caller's deferred free guard always targets the live handle. On return nil,
// *handle is the resolved handle ready to seal; on error *handle is the live (or nil) handle the
// guard frees.
//
// Round 0's fetch set comes from the want-list seeds (component/substate ids). Every subsequent
// round fetches the *concrete* ids the core hands back in NeedMore (`fetch_ids`) — including the
// vault ids the resolver discovered inside a fetched component, which the seeds alone could never
// name. It converges in 1–2 rounds; past maxResolutionRounds it returns ErrResolutionDidNotConverge.
func (c *Client) resolveInputs(ctx context.Context, handle **cffi.Handle, wantListJSON string) error {
	ids, idErr := collectFetchIDs(wantListJSON)
	if idErr != nil {
		return idErr
	}
	for round := 0; ; round++ {
		fetched, fErr := c.transport.FetchSubstates(ctx, ids)
		if fErr != nil {
			return fErr
		}
		// The core expects a JSON array; a nil slice marshals to `null`, which it rejects.
		if fetched == nil {
			fetched = []transport.FetchedSubstate{}
		}
		fetchedJSON, jErr := json.Marshal(fetched)
		if jErr != nil {
			return &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal fetched substates: %v", jErr)}
		}

		// ApplyFetchedSubstates consumes *handle (even on error) and returns the next handle.
		// Re-point immediately so the caller's deferred guard frees the right one.
		next, resolutionJSON, aErr := cffi.ApplyFetchedSubstates(*handle, string(fetchedJSON))
		*handle = next // nil on error; the new handle on success
		if aErr != nil {
			return fromCffiError(aErr)
		}

		res, rErr := parseResolution(resolutionJSON)
		if rErr != nil {
			return rErr
		}
		if res.resolved {
			return nil
		}
		// NeedMore: prefer the concrete fetch_ids the core discovered; fall back to deriving from
		// the want list (older cores that don't emit fetch_ids). Loop again, up to the cap.
		ids = res.fetchIDs
		if len(ids) == 0 {
			ids, idErr = collectFetchIDs(res.wantListJSON)
			if idErr != nil {
				return idErr
			}
		}
		if round+1 >= maxResolutionRounds {
			return &Error{
				Code:    "RESOLUTION",
				Message: fmt.Sprintf("%v (capped at %d rounds)", ErrResolutionDidNotConverge, maxResolutionRounds),
				cause:   ErrResolutionDidNotConverge,
			}
		}
	}
}

// --- want-list → fetch ids (transport-level marshalling, NOT domain logic) -------------

// wantList is the BuildUnsigned / NeedMore envelope: {"want_list":[…WantItem…]}.
type wantListEnvelope struct {
	WantList []wantItem `json:"want_list"`
}

// wantItem mirrors the core's `kind`-tagged WantItem (crates/ootle_sdk_core/src/inputs.rs).
// The driver only reads the address strings to know what to fetch — it derives nothing.
// All three variants carry their seed id in one of these two fields; per the core's
// WantItem::seed_substate_ids, vault_for_resource / all_component_vaults seed on the
// component address and specific_substate seeds on the substate id.
type wantItem struct {
	Kind             string `json:"kind"`
	ComponentAddress string `json:"component_address,omitempty"`
	SubstateID       string `json:"substate_id,omitempty"`
}

// collectFetchIDs extracts the deduplicated substate ids to fetch from a want-list JSON
// envelope. This is pure marshalling — reading the address strings the core already put
// in the want items — and mirrors the core's WantList::seed_substate_ids ordering (want
// order, dedup, first-seen). It contains NO want-resolution logic (that lives in the core
// behind ApplyFetchedSubstates); if deriving the fetch set ever required real resolution,
// that would belong in the core, not here.
func collectFetchIDs(wantListJSON string) ([]string, error) {
	var env wantListEnvelope
	if err := json.Unmarshal([]byte(wantListJSON), &env); err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal want list: %v", err)}
	}
	out := make([]string, 0, len(env.WantList))
	seen := make(map[string]struct{}, len(env.WantList))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, w := range env.WantList {
		switch w.Kind {
		case "vault_for_resource", "all_component_vaults":
			add(w.ComponentAddress)
		case "specific_substate":
			add(w.SubstateID)
		case "stealth_utxo":
			// A stealth UTXO want's seed id is the derived utxo_<resource>_<commitment> address,
			// NOT a field in the want JSON. Deriving it host-side would be engine-shaped work the
			// thin host must not do — so we fetch nothing on the seed round and let the core hand
			// back the concrete id in the first apply's NeedMore.fetch_ids (which the driver then
			// fetches). Vault/UTXO discovery stays in the core.
		default:
			// Unknown variant: fall back to whichever address field is populated so a
			// future want kind still fetches something rather than silently fetching
			// nothing. (A genuinely new resolution shape would be a core follow-up.)
			add(w.ComponentAddress)
			add(w.SubstateID)
		}
	}
	return out, nil
}

// --- resolution status (marshalling of the core's {status,…} envelope) -----------------

// resolution is the decoded ApplyFetchedSubstates result: either resolved, or NeedMore
// carrying the concrete next-fetch ids (and, for fallback, the next want-list JSON).
type resolution struct {
	resolved bool
	// fetchIDs is the authoritative concrete next-fetch set the core discovered (incl. vault
	// ids read out of a fetched component). Empty only against an older core that predates
	// the fetch_ids field, in which case wantListJSON is used to derive the seeds.
	fetchIDs     []string
	wantListJSON string
}

// resolutionEnvelope mirrors the core's apply_fetched_substates data JSON:
// {"status":"resolved"} or {"status":"need_more","want_list":[…],"fetch_ids":[…]}.
type resolutionEnvelope struct {
	Status   string          `json:"status"`
	WantList json.RawMessage `json:"want_list"`
	FetchIDs []string        `json:"fetch_ids"`
}

// parseResolution decodes the core's resolution envelope. On NeedMore it captures the
// concrete fetch_ids and re-wraps the want list back into the {"want_list":…} envelope
// collectFetchIDs expects (the fallback path for a core without fetch_ids).
func parseResolution(resolutionJSON string) (resolution, error) {
	var env resolutionEnvelope
	if err := json.Unmarshal([]byte(resolutionJSON), &env); err != nil {
		return resolution{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal resolution: %v", err)}
	}
	switch env.Status {
	case "resolved":
		return resolution{resolved: true}, nil
	case "need_more":
		wl := env.WantList
		if len(wl) == 0 {
			wl = json.RawMessage("[]")
		}
		// Re-wrap the bare want-list array back into the {"want_list":…} envelope that
		// collectFetchIDs (and the BuildUnsigned output) use.
		body, err := json.Marshal(map[string]json.RawMessage{"want_list": wl})
		if err != nil {
			return resolution{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("rewrap want list: %v", err)}
		}
		return resolution{fetchIDs: env.FetchIDs, wantListJSON: string(body)}, nil
	default:
		return resolution{}, &Error{Code: "INTERNAL", Message: fmt.Sprintf("unknown resolution status %q", env.Status)}
	}
}

// hexToBase64 converts the core's lowercase-hex encoded transaction into the base64
// envelope the indexer's POST /transactions expects (trivial marshalling, allowed
// host-side; the bytes themselves are produced by the core).
func hexToBase64(hexStr string) (string, error) {
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		return "", &Error{Code: "ENCODING", Message: fmt.Sprintf("decode encoded transaction hex: %v", err)}
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
