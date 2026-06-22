package ootle

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// fixture mirrors the committed golden-vector JSON shape (the fields this step asserts).
type fixture struct {
	Operation string `json:"operation"`
	Input     struct {
		Network Network                   `json:"network"`
		Intent  PublicTransferIntent      `json:"intent"`
		Keys    DeterministicTransferKeys `json:"keys"`
	} `json:"input"`
	Expected struct {
		EncodedTransaction string `json:"encoded_transaction"`
		TransactionID      string `json:"transaction_id"`
	} `json:"expected"`
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

// TestBuildAndEncodePublicTransfer_SmokeVector proves the C boundary is real: a Go call
// reproduces the committed core vector's encoded_transaction + transaction_id
// byte-for-byte (hex string compare) through cgo.
func TestBuildAndEncodePublicTransfer_SmokeVector(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	if f.Operation != "build_and_encode_public_transfer" {
		t.Fatalf("unexpected fixture operation %q", f.Operation)
	}

	got, err := BuildAndEncodePublicTransfer(f.Input.Network, f.Input.Intent, f.Input.Keys)
	if err != nil {
		t.Fatalf("BuildAndEncodePublicTransfer: %v", err)
	}

	if got.EncodedTransaction != f.Expected.EncodedTransaction {
		t.Errorf("encoded_transaction mismatch:\n got:  %s\n want: %s", got.EncodedTransaction, f.Expected.EncodedTransaction)
	}
	if got.TransactionID != f.Expected.TransactionID {
		t.Errorf("transaction_id mismatch:\n got:  %s\n want: %s", got.TransactionID, f.Expected.TransactionID)
	}
}

// TestBuildAndEncodePublicTransfer_MalformedIntent proves a bad input surfaces as a
// typed Go error carrying the stable core code, not a panic.
func TestBuildAndEncodePublicTransfer_MalformedIntent(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	intent := f.Input.Intent
	intent.FromAccount = "not-a-valid-substate-id" // forces a core PARSE error.

	_, err := BuildAndEncodePublicTransfer(f.Input.Network, intent, f.Input.Keys)
	if err == nil {
		t.Fatal("expected an error for a malformed intent, got nil")
	}
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code == "" {
		t.Fatalf("expected a non-empty stable error code, got empty (message: %q)", oe.Message)
	}
	// The core maps an unparseable substate id / address to a parse/validation class.
	switch oe.Code {
	case "PARSE", "VALIDATION", "INVALID", "ENCODING":
		// acceptable stable codes for a malformed intent
	default:
		t.Errorf("unexpected error code %q (message: %q)", oe.Code, oe.Message)
	}
}

// TestUnknownNetwork proves an unknown network keyword is rejected client-side with a
// typed error and never reaches the FFI.
func TestUnknownNetwork(t *testing.T) {
	f := loadFixture(t, "public_transfer/single_key_basic.json")
	_, err := BuildAndEncodePublicTransfer("not-a-network", f.Input.Intent, f.Input.Keys)
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *ootle.Error, got %T: %v", err, err)
	}
	if oe.Code != "VALIDATION" {
		t.Errorf("expected VALIDATION for unknown network, got %q", oe.Code)
	}
}

// TestABIVersion asserts the vendored lib reports the frozen ABI tag the wrapper expects.
func TestABIVersion(t *testing.T) {
	if got := ABIVersion(); got != cffi.ExpectedABIVersion {
		t.Errorf("ABI version mismatch: lib reports %q, wrapper expects %q", got, cffi.ExpectedABIVersion)
	}
}
