package ootle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// fixture mirrors the committed golden-vector JSON shape (the fields this step asserts). Since ABI
// ootle-sdk-ffi-c/16 the seal signs with a random nonce, so the vector no longer pins
// expected.encoded_transaction / transaction_id — the smoke test asserts a well-formed envelope.
type fixture struct {
	Operation string `json:"operation"`
	Input     struct {
		Network Network              `json:"network"`
		Intent  PublicTransferIntent `json:"intent"`
		Keys    PublicTransferKeys   `json:"keys"`
	} `json:"input"`
}

func loadFixture(t *testing.T, rel string) fixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", rel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	return f
}

// oneShotBuildEncode drives the offline one-shot build+seal+encode over the C ABI (the non-seed
// production ootle_build_and_encode_public_transfer). It mirrors what the SDK's public entry points do
// — validate the network client-side, then hand the intent/keys to the core — so the smoke and
// error-contract tests below exercise the real cgo boundary. Errors carry the stable *cffi.Error code.
func oneShotBuildEncode(network Network, intent PublicTransferIntent, keys PublicTransferKeys) (EncodedPublicTransfer, error) {
	var out EncodedPublicTransfer
	netByte, ok := network.ByteValue()
	if !ok {
		return out, &cffi.Error{Code: "VALIDATION", Message: "unknown network " + string(network)}
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return out, &cffi.Error{Code: "ENCODING", Message: err.Error()}
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return out, &cffi.Error{Code: "ENCODING", Message: err.Error()}
	}
	dataJSON, cerr := cffi.BuildAndEncodePublicTransfer(netByte, string(intentJSON), string(keysJSON))
	if cerr != nil {
		return out, cerr
	}
	if uerr := json.Unmarshal([]byte(dataJSON), &out); uerr != nil {
		return out, &cffi.Error{Code: "ENCODING", Message: uerr.Error()}
	}
	return out, nil
}

// TestBuildAndEncodePublicTransfer_SmokeVector proves the C boundary is real: a Go call drives the
// core's one-shot build+seal+encode through cgo to a well-formed encoded transaction. The random-nonce
// seal (ABI ootle-sdk-ffi-c/16) is not byte-reproducible, so the arm asserts well-formedness rather
// than the exact committed bytes.
func TestBuildAndEncodePublicTransfer_SmokeVector(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	if f.Operation != "build_and_encode_public_transfer" {
		t.Fatalf("unexpected fixture operation %q", f.Operation)
	}

	got, err := oneShotBuildEncode(f.Input.Network, f.Input.Intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("oneShotBuildEncode: %v", err)
	}
	assertWellFormedEnc(t, got)
}

// TestBuildAndEncodePublicTransfer_MalformedIntent proves a bad input surfaces as a typed error
// carrying the stable core code, not a panic.
func TestBuildAndEncodePublicTransfer_MalformedIntent(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	intent := f.Input.Intent
	intent.FromAccount = "not-a-valid-substate-id" // forces a core PARSE error.

	_, err := oneShotBuildEncode(f.Input.Network, intent, f.Input.Keys)
	if err == nil {
		t.Fatal("expected an error for a malformed intent, got nil")
	}
	var ce *cffi.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *cffi.Error, got %T: %v", err, err)
	}
	if ce.Code == "" {
		t.Fatalf("expected a non-empty stable error code, got empty (message: %q)", ce.Message)
	}
	// The core maps an unparseable substate id / address to a parse/validation class.
	switch ce.Code {
	case "PARSE", "VALIDATION", "INVALID", "ENCODING":
		// acceptable stable codes for a malformed intent
	default:
		t.Errorf("unexpected error code %q (message: %q)", ce.Code, ce.Message)
	}
}

// TestUnknownNetwork proves an unknown network keyword is rejected client-side with a typed
// VALIDATION error and never reaches the FFI.
func TestUnknownNetwork(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	_, err := oneShotBuildEncode("not-a-network", f.Input.Intent, f.Input.Keys)
	var ce *cffi.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected *cffi.Error, got %T: %v", err, err)
	}
	if ce.Code != "VALIDATION" {
		t.Errorf("expected VALIDATION for unknown network, got %q", ce.Code)
	}
}

// TestABIVersion asserts the vendored lib reports the frozen ABI tag the wrapper expects.
func TestABIVersion(t *testing.T) {
	if got := ABIVersion(); got != cffi.ExpectedABIVersion {
		t.Errorf("ABI version mismatch: lib reports %q, wrapper expects %q", got, cffi.ExpectedABIVersion)
	}
}
