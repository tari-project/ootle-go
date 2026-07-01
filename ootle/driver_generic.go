package ootle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// This file is the GROUP B driver: the two-phase loop over the GENERIC builder entry point. It reuses
// the EXISTING apply/seal/free C surface unchanged — the only difference from sendPublicTransfer is the
// phase-1 call (BuildUnsignedInstructions instead of BuildUnsigned). No value-critical logic lives here.

// SendInstructions drives a generic instruction transaction end-to-end against the indexer using the
// production (random-nonce) seal: build unsigned (generic) → resolve inputs (bounded two-phase loop) →
// seal+encode → submit → wait → parse. This is the path production callers want; the encoded bytes are
// NOT reproducible (random seal nonce).
//
// The intent's Inputs must be empty: this is the resolved path. A non-empty Inputs makes the core
// short-circuit to the explicit path and skip resolution; the driver rejects it with a VALIDATION error.
func (c *Client) SendInstructions(ctx context.Context, intent GenericTransactionIntent, keys PublicTransferKeys) (FinalizedResult, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return c.sendInstructions(ctx, c.network, intent, string(keysJSON), cffi.SealAndEncode)
}

// sendInstructions is the shared generic two-phase driver. It is the generic twin of sendPublicTransfer:
// identical resolution loop + handle lifetime, only phase 1 differs (the generic build entry point). The
// returned handle is a HandleKind::Public handle, so ApplyFetchedSubstates / seal consume it unchanged.
func (c *Client) sendInstructions(ctx context.Context, network Network, intent GenericTransactionIntent, keysJSON string, seal sealFunc) (result FinalizedResult, err error) {
	netByte, nErr := resolveNetworkByte(network)
	if nErr != nil {
		return FinalizedResult{}, nErr
	}
	// Empty explicit inputs ⇒ the resolved path (mirrors sendPublicTransfer): a non-empty set would make
	// the core short-circuit the explicit path and never run the wants loop.
	if len(intent.Inputs) != 0 {
		return FinalizedResult{}, &Error{
			Code:    "VALIDATION",
			Message: "SendInstructions requires an empty intent.Inputs (the resolved path); supply explicit inputs only via the explicit-path API",
		}
	}

	// A faucet claim (Faucet().Take()) is a distinct core entry point; everything after the build —
	// the resolution loop, seal, submit — is identical, so only the phase-1 call differs.
	var handle *cffi.Handle
	var wantListJSON string
	var cErr error
	if intent.faucetClaim != nil {
		claimJSON, mErr := json.Marshal(intent.faucetClaim)
		if mErr != nil {
			return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal faucet claim intent: %v", mErr)}
		}
		handle, wantListJSON, cErr = cffi.BuildFaucetClaim(netByte, string(claimJSON))
	} else {
		intentJSON, mErr := intent.marshalIntent()
		if mErr != nil {
			return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal generic intent: %v", mErr)}
		}
		handle, wantListJSON, cErr = cffi.BuildUnsignedInstructions(netByte, string(intentJSON))
	}
	if cErr != nil {
		return FinalizedResult{}, fromCffiError(cErr)
	}
	// Free the live handle on every exit path (incl. panic). After a consuming call the driver
	// re-points `handle`, so this frees the live one, never a consumed one.
	defer func() { cffi.FreeHandle(handle) }()

	// --- Phase 2: bounded resolution loop (shared with the public path) -----------------------------
	if rErr := c.resolveInputs(ctx, &handle, wantListJSON); rErr != nil {
		return FinalizedResult{}, rErr
	}

	// --- Seal + encode (consumes the handle) ------------------------------------------------------
	encodedJSON, sErr := seal(handle, keysJSON)
	handle = nil // consumed; the guard must not free it
	if sErr != nil {
		return FinalizedResult{}, fromCffiError(sErr)
	}
	var encoded EncodedPublicTransfer
	if uErr := json.Unmarshal([]byte(encodedJSON), &encoded); uErr != nil {
		return FinalizedResult{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal encoded transfer: %v", uErr)}
	}

	// A dry run evaluates inline via a dedicated endpoint; the normal path submits and polls.
	if intent.DryRun {
		return c.submitDryRun(ctx, encoded)
	}
	return c.SubmitSealed(ctx, encoded)
}
