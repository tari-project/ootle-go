package ootle

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// Authorization is one co-signer's authorization over an unsigned transaction: the signer's public
// key and the Schnorr signature, both lowercase hex (no 0x). It carries no secret material. Party B
// produces it with AddSignature; party A attaches it via CosignSealer.SealWithAuth. Mirrors the core's
// Authorization shape.
type Authorization struct {
	PublicKey string `json:"public_key"`
	Signature string `json:"signature"`
}

// AddSignature is PARTY B's step in the co-sign hand-off: authorize party A's unsigned-transaction
// record, committing to A's seal public key (so the signature binds to the exact tx A will seal). The
// production (random-nonce) Schnorr signature stays in the core; the host only marshals JSON.
//
// unsignedRecordJSON is the UnsignedTransactionRecord JSON A shipped (from
// CosignSealer.UnsignedRecord). sealPublicKeyHex is A's seal public key (lowercase hex). signerSecretHex
// is B's secret key (lowercase hex) — it is transient and the host is responsible for protecting it.
//
// Bad signer-secret hex is a "KEY" error; bad seal-pk hex is "PARSE". network is validated but does not
// affect the signing message (the record carries the network).
func AddSignature(network Network, unsignedRecordJSON, sealPublicKeyHex, signerSecretHex string) (Authorization, error) {
	var out Authorization
	netByte, ok := network.ByteValue()
	if !ok {
		return out, &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	dataJSON, cerr := cffi.AddSignature(netByte, unsignedRecordJSON, sealPublicKeyHex, signerSecretHex)
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	var wrap struct {
		Authorization Authorization `json:"authorization"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &wrap); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal authorization: %v", err)}
	}
	return wrap.Authorization, nil
}

// CosignSealer is PARTY A's in-progress co-sign transaction: a fully-resolved partial held open between
// shipping the unsigned record (UnsignedRecord) and attaching the collected authorizations + sealing
// (SealWithAuth / SealWithAuthProduction). It owns the opaque core handle; the caller MUST call exactly
// one of SealWithAuth[Production] or Close to release it (the seal calls consume it).
//
// CosignSealer is NOT safe for concurrent use.
type CosignSealer struct {
	handle  *cffi.Handle
	network Network
	netByte uint8
}

// PrepareCosign is PARTY A's step 1: build + resolve a public transfer to a fully-resolved partial,
// ready to ship to co-signers and seal. It drives the same two-phase resolution loop as
// SendPublicTransfer (build → fetch → apply, repeated on NeedMore up to the round cap), then returns a
// CosignSealer holding the resolved handle. The caller must seal it (SealWithAuth[Production]) or Close
// it.
//
// The intent's Inputs must be empty (the resolved path), exactly as SendPublicTransfer requires.
func (c *Client) PrepareCosign(ctx context.Context, intent PublicTransferIntent) (*CosignSealer, error) {
	network := c.network
	netByte, nErr := resolveNetworkByte(network)
	if nErr != nil {
		return nil, nErr
	}
	if len(intent.Inputs) != 0 {
		return nil, &Error{
			Code:    "VALIDATION",
			Message: "PrepareCosign requires an empty intent.Inputs (the resolved path)",
		}
	}
	intentJSON, mErr := intent.marshalJSON()
	if mErr != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal intent: %v", mErr)}
	}

	handle, wantListJSON, cErr := cffi.BuildUnsigned(netByte, string(intentJSON))
	if cErr != nil {
		return nil, fromCffiError(cErr)
	}
	// Free the live handle on any error path below; on success the CosignSealer owns it.
	ok := false
	defer func() {
		if !ok {
			cffi.FreeHandle(handle)
		}
	}()

	if rErr := c.resolveInputs(ctx, &handle, wantListJSON); rErr != nil {
		return nil, rErr
	}

	ok = true // the sealer owns the handle now.
	return &CosignSealer{handle: handle, network: network, netByte: netByte}, nil
}

// UnsignedRecord returns the serializable unsigned-transaction record A ships to co-signers (party B's
// AddSignature input). It BORROWS the handle (does not consume it), so A can ship the record and still
// seal afterwards. An unresolved partial is a "RESOLUTION" error (PrepareCosign always returns a
// resolved one, so this is a safety guard).
func (s *CosignSealer) UnsignedRecord() (string, error) {
	if s == nil || s.handle == nil {
		return "", &Error{Code: "INTERNAL", Message: "UnsignedRecord called on a closed/consumed CosignSealer"}
	}
	recordJSON, cerr := cffi.UnsignedRecordForCosign(s.handle)
	if cerr != nil {
		return "", fromCffiError(cerr)
	}
	return recordJSON, nil
}

// SealWithAuth is PARTY A's final step and the supported offline/HSM co-sign seal: attach the collected
// authorizations and seal from a single build seed, producing the submit-ready encoded transaction
// byte-for-byte reproducibly. It CONSUMES the sealer (the handle is released); do not reuse it. An empty
// authorizations slice seals as a plain single-key transfer.
func (s *CosignSealer) SealWithAuth(keys DeterministicTransferKeys, authorizations []Authorization) (EncodedPublicTransfer, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return EncodedPublicTransfer{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return s.sealWithAuth(string(keysJSON), authorizations, cffi.SealAndEncodeWithAuthWithSeed)
}

// SealWithAuthProduction is the production (random-nonce) counterpart of SealWithAuth. keys is just the
// account secret. The bytes/id are not reproducible. CONSUMES the sealer.
func (s *CosignSealer) SealWithAuthProduction(keys PublicTransferKeys, authorizations []Authorization) (EncodedPublicTransfer, error) {
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return EncodedPublicTransfer{}, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal keys: %v", err)}
	}
	return s.sealWithAuth(string(keysJSON), authorizations, cffi.SealAndEncodeWithAuth)
}

// cosignSealFunc is the seal-with-auth core call (deterministic or production). It CONSUMES the handle.
type cosignSealFunc func(h *cffi.Handle, keysJSON, authorizationsJSON string) (string, error)

// sealWithAuth is the shared body for both key paths: marshal the authorizations, call the consuming
// seal, and decode the encoded transfer.
func (s *CosignSealer) sealWithAuth(keysJSON string, authorizations []Authorization, seal cosignSealFunc) (EncodedPublicTransfer, error) {
	var out EncodedPublicTransfer
	if s == nil || s.handle == nil {
		return out, &Error{Code: "INTERNAL", Message: "SealWithAuth called on a closed/consumed CosignSealer"}
	}
	if authorizations == nil {
		authorizations = []Authorization{}
	}
	authsJSON, err := json.Marshal(authorizations)
	if err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal authorizations: %v", err)}
	}
	h := s.handle
	s.handle = nil // the seal consumes it.
	encodedJSON, cerr := seal(h, keysJSON, string(authsJSON))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if uerr := json.Unmarshal([]byte(encodedJSON), &out); uerr != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal encoded transfer: %v", uerr)}
	}
	return out, nil
}

// Close releases the sealer's handle without sealing (for the abandon path). Null-safe and idempotent;
// a no-op once a Seal call has consumed the handle.
func (s *CosignSealer) Close() {
	if s == nil || s.handle == nil {
		return
	}
	cffi.FreeHandle(s.handle)
	s.handle = nil
}
