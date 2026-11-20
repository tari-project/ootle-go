package ootle

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// cosignSignerPublicKey is the account public key of the cosign vector's cosign_signer_secret (party
// B), the key the vector's sealed transaction carries. The record names it as the sole authorizer; a
// wrong value fails loudly, since AddSignature refuses a signer outside the record's set.
const cosignSignerPublicKey = "fcee5a0d228484be47257110278cd3ab7146ab74e4c3aff7d4d14cd63753ed3c"

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
// All value-critical work (the signing message digest, the sr25519 signature, the
// main_signer rule, BOR encode + signature verification) stays in the core; this host
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

	record, err := sealer.UnsignedRecord([]string{cosignSignerPublicKey})
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
	// A bad/foreign authorization or a wrong main_signer would fail here. We reuse
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

	record, err := sealer.UnsignedRecord([]string{cosignSignerPublicKey})
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

// cosignMainSignerSetup resolves the cosign vector into a sealer with mainSigner set, and has B
// authorize the resulting record.
func cosignMainSignerSetup(t *testing.T, mainSigner func(bPublicKey string) string) (*CosignSealer, cosignFixture, Authorization) {
	t.Helper()
	f := loadCosignFixture(t, "seal_with_auth.json")
	mock := &mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return f.Input.Fetched, nil
		},
	}
	sealer, err := NewClient(mock, WithNetwork(f.Input.Network)).PrepareCosign(context.Background(), f.Input.Intent)
	if err != nil {
		t.Fatalf("PrepareCosign: %v", err)
	}
	t.Cleanup(sealer.Close)

	sealer.SetMainSigner(mainSigner(cosignSignerPublicKey))
	record, err := sealer.UnsignedRecord([]string{cosignSignerPublicKey})
	if err != nil {
		t.Fatalf("UnsignedRecord: %v", err)
	}
	auth, err := AddSignature(f.Input.Network, record, f.Input.CosignSealPK, f.Input.CosignSignerSecret)
	if err != nil {
		t.Fatalf("AddSignature: %v", err)
	}
	return sealer, f, auth
}

// TestManualCoSigning_MainSigner seals with B named as the main signer: the record and the seal
// both declare it, so B's authorization verifies.
func TestManualCoSigning_MainSigner(t *testing.T) {
	sealer, f, auth := cosignMainSignerSetup(t, func(b string) string { return b })

	encoded, err := sealer.SealWithAuth(f.Input.Keys, []Authorization{auth})
	if err != nil {
		t.Fatalf("SealWithAuth: %v", err)
	}
	netByte, _ := f.Input.Network.ByteValue()
	sealed, err := cffi.ValidateStealthTransfer(netByte, encoded.EncodedTransaction)
	if err != nil {
		t.Fatalf("the cosigned sealed transaction failed validation: %v", err)
	}
	if want := `"main_signer":{"Authorizer":"` + auth.PublicKey + `"}`; !strings.Contains(sealed, want) {
		t.Errorf("sealed transaction does not declare %s", want)
	}
}

// TestManualCoSigning_MainSignerMustAuthorize names A's seal key as the main signer while only B
// authorizes: the core refuses the seal with INVALID.
func TestManualCoSigning_MainSignerMustAuthorize(t *testing.T) {
	sealer, f, auth := cosignMainSignerSetup(t, func(string) string { return "" })
	sealer.SetMainSigner(f.Input.CosignSealPK)
	// The record B signed declared no main signer, but the core checks membership first.
	_, err := sealer.SealWithAuth(f.Input.Keys, []Authorization{auth})
	var e *Error
	if !asOotleError(err, &e) || e.Code != "INVALID" {
		t.Fatalf("SealWithAuth with a main signer outside the authorizations: err = %v, want INVALID", err)
	}
}

// cosignSealer resolves the cosign vector into a fresh sealer (no main signer).
func cosignSealer(t *testing.T) (*CosignSealer, cosignFixture) {
	t.Helper()
	f := loadCosignFixture(t, "seal_with_auth.json")
	mock := &mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return f.Input.Fetched, nil
		},
	}
	sealer, err := NewClient(mock, WithNetwork(f.Input.Network)).PrepareCosign(context.Background(), f.Input.Intent)
	if err != nil {
		t.Fatalf("PrepareCosign: %v", err)
	}
	t.Cleanup(sealer.Close)
	return sealer, f
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	var e *Error
	if !asOotleError(err, &e) || e.Code != code {
		t.Fatalf("%s: err = %v, want %s", what, err, code)
	}
}

// TestManualCoSigning_AuthorizerSet covers the authorizer set the record fixes before anyone signs.
func TestManualCoSigning_AuthorizerSet(t *testing.T) {
	t.Run("empty set is invalid", func(t *testing.T) {
		sealer, _ := cosignSealer(t)
		for _, set := range [][]string{nil, {}} {
			_, err := sealer.UnsignedRecord(set)
			wantCode(t, "UnsignedRecord(empty)", err, "INVALID")
		}
	})

	t.Run("main signer outside the set is invalid", func(t *testing.T) {
		sealer, f := cosignSealer(t)
		sealer.SetMainSigner(f.Input.CosignSealPK)
		_, err := sealer.UnsignedRecord([]string{cosignSignerPublicKey})
		wantCode(t, "UnsignedRecord with an outside main signer", err, "INVALID")
	})

	t.Run("signer outside the set is refused", func(t *testing.T) {
		sealer, f := cosignSealer(t)
		record, err := sealer.UnsignedRecord([]string{f.Input.CosignSealPK})
		if err != nil {
			t.Fatalf("UnsignedRecord: %v", err)
		}
		_, err = AddSignature(f.Input.Network, record, f.Input.CosignSealPK, f.Input.CosignSignerSecret)
		wantCode(t, "AddSignature by a non-authorizer", err, "INVALID")
	})

	t.Run("a missing co-signer does not verify", func(t *testing.T) {
		sealer, f := cosignSealer(t)
		other, err := GenerateAccountKey()
		if err != nil {
			t.Fatalf("GenerateAccountKey: %v", err)
		}
		record, err := sealer.UnsignedRecord([]string{cosignSignerPublicKey, other.AccountPublicKey})
		if err != nil {
			t.Fatalf("UnsignedRecord: %v", err)
		}
		auth, err := AddSignature(f.Input.Network, record, f.Input.CosignSealPK, f.Input.CosignSignerSecret)
		if err != nil {
			t.Fatalf("AddSignature: %v", err)
		}
		_, err = sealer.SealWithAuth(f.Input.Keys, []Authorization{auth})
		wantCode(t, "SealWithAuth missing a co-signer", err, "VALIDATION")
	})
}
