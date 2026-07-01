package ootle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// cosignFixture mirrors a committed cosign/* vector: a resolved public transfer (network/intent/keys
// /fetched) plus the co-sign material (A's seal pk, B's secret).
type cosignFixture struct {
	Operation string `json:"operation"`
	Input     struct {
		Network            Network                     `json:"network"`
		Intent             PublicTransferIntent        `json:"intent"`
		Keys               PublicTransferKeys          `json:"keys"`
		Fetched            []transport.FetchedSubstate `json:"fetched"`
		CosignSealPK       string                      `json:"cosign_seal_pk"`
		CosignSignerSecret string                      `json:"cosign_signer_secret"`
	} `json:"input"`
}

func loadCosignFixture(t *testing.T, rel string) cosignFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "cosign", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cosign fixture %s: %v", path, err)
	}
	var f cosignFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal cosign fixture %s: %v", path, err)
	}
	return f
}

// TestManualCoSigning is the manual co-signing example: the authorize → attach → seal hand-off across
// two in-process "parties", driven entirely offline (no node, no submission). It exercises the full
// cross-ABI cosign logic:
//
//	Party A: PrepareCosign (build + resolve a public transfer to a handle) → UnsignedRecord (the
//	         serializable record to ship to B).
//	Party B: AddSignature over A's record, committing to A's seal public key (B never sees A's secret,
//	         A never sees B's).
//	Party A: SealWithAuth (attach B's authorization + seal) → a submit-ready, validating transaction.
//
// All value-critical work (the signing message digest, the Schnorr signature, the
// is_seal_signer_authorized rule, BOR encode + signature verification) stays in the core; this host
// only marshals the record/authorization JSON between the two parties.
//
// The transfer is resolved against a mock transport seeded from the committed cosign vector's fetched
// batch — so the example needs no live indexer. (Real submission would hand the sealed bytes to a
// node; that step is intentionally omitted here to keep the cosign logic exercised offline.)
func TestManualCoSigning(t *testing.T) {
	f := loadCosignFixture(t, "seal_with_auth.json")
	if f.Operation != "cosign_seal_with_auth" {
		t.Fatalf("unexpected operation %q", f.Operation)
	}
	if f.Input.CosignSealPK == "" || f.Input.CosignSignerSecret == "" {
		t.Fatalf("cosign fixture missing cosign_seal_pk / cosign_signer_secret")
	}

	// A mock transport that serves the vector's full fetched batch on every fetch — the partial
	// resolves in a single apply pass, offline.
	mock := &mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return f.Input.Fetched, nil
		},
	}
	client := NewClient(mock, WithNetwork(f.Input.Network))

	// --- Party A, step 1: build + resolve, then derive the record to ship to B. -------------------
	sealer, err := client.PrepareCosign(context.Background(), f.Input.Intent)
	if err != nil {
		t.Fatalf("PrepareCosign (party A): %v", err)
	}
	// Always release the handle if we bail before sealing.
	defer sealer.Close()

	record, err := sealer.UnsignedRecord()
	if err != nil {
		t.Fatalf("UnsignedRecord (party A): %v", err)
	}
	if record == "" {
		t.Fatalf("UnsignedRecord returned an empty record")
	}

	// --- Party B: authorize A's record, committing to A's seal public key. ------------------------
	// B receives only `record` + A's PUBLIC seal key; B signs with its OWN secret.
	auth, err := AddSignature(f.Input.Network, record, f.Input.CosignSealPK, f.Input.CosignSignerSecret)
	if err != nil {
		t.Fatalf("AddSignature (party B): %v", err)
	}
	if auth.PublicKey == "" || auth.Signature == "" {
		t.Fatalf("AddSignature returned an empty authorization: %+v", auth)
	}

	// --- Party A, step 2: attach B's authorization and seal. --------------------------------------
	encoded, err := sealer.SealWithAuth(f.Input.Keys, []Authorization{auth})
	if err != nil {
		t.Fatalf("SealWithAuth (party A): %v", err)
	}
	if encoded.EncodedTransaction == "" || encoded.TransactionID == "" {
		t.Fatalf("SealWithAuth produced an empty transaction: %+v", encoded)
	}

	// --- Verify the cosigned seal validates (decode + verify EVERY signature, in the core). -------
	// A bad/foreign authorization or a wrong is_seal_signer_authorized flag would fail here. We reuse
	// the shared sealed-transfer canonicalizer (ootle_validate_stealth_transfer) — it BOR-decodes and
	// verifies all signatures, returning an error if any fails.
	netByte, _ := f.Input.Network.ByteValue()
	if _, verr := cffi.ValidateStealthTransfer(netByte, encoded.EncodedTransaction); verr != nil {
		t.Fatalf("the cosigned sealed transaction failed validation: %v", verr)
	}
	t.Logf("manual_co_signing: cosigned tx %s sealed + validated offline (authorizer pk %s)", encoded.TransactionID, auth.PublicKey)
}

// TestManualCoSigning_TamperedAuthorizationRejected proves the seal-side verification rejects a
// tampered authorization: flip a byte of B's signature and the cosigned seal no longer validates. This
// keeps the example honest — the authorization is load-bearing, not decorative.
func TestManualCoSigning_TamperedAuthorizationRejected(t *testing.T) {
	f := loadCosignFixture(t, "seal_with_auth.json")
	mock := &mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return f.Input.Fetched, nil
		},
	}
	client := NewClient(mock, WithNetwork(f.Input.Network))

	sealer, err := client.PrepareCosign(context.Background(), f.Input.Intent)
	if err != nil {
		t.Fatalf("PrepareCosign: %v", err)
	}
	defer sealer.Close()

	record, err := sealer.UnsignedRecord()
	if err != nil {
		t.Fatalf("UnsignedRecord: %v", err)
	}
	auth, err := AddSignature(f.Input.Network, record, f.Input.CosignSealPK, f.Input.CosignSignerSecret)
	if err != nil {
		t.Fatalf("AddSignature: %v", err)
	}

	// Tamper: flip the last hex nibble of the signature scalar (still 64 bytes, but won't verify).
	sig := []byte(auth.Signature)
	last := len(sig) - 1
	if sig[last] == '0' {
		sig[last] = '1'
	} else {
		sig[last] = '0'
	}
	auth.Signature = string(sig)

	encoded, sealErr := sealer.SealWithAuth(f.Input.Keys, []Authorization{auth})
	if sealErr != nil {
		// Acceptable outcome: a non-canonical signature is rejected at attach time ("KEY").
		var e *Error
		if !asOotleError(sealErr, &e) || (e.Code != "KEY" && e.Code != "VALIDATION") {
			t.Fatalf("tampered authorization: unexpected seal error %v", sealErr)
		}
		return
	}
	// Otherwise it sealed but must FAIL verification.
	netByte, _ := f.Input.Network.ByteValue()
	if _, verr := cffi.ValidateStealthTransfer(netByte, encoded.EncodedTransaction); verr == nil {
		t.Fatal("a tampered authorization produced a validating seal — verification is broken")
	}
}

// asOotleError is a tiny errors.As helper kept local to the example so it reads top-to-bottom.
func asOotleError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
