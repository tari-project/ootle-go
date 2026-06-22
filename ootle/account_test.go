package ootle

import (
	"errors"
	"strings"
	"testing"
)

// TestTariUnits checks the µTari conversion helpers.
func TestTariUnits(t *testing.T) {
	if MicroTari != 1 {
		t.Errorf("MicroTari = %d, want 1", MicroTari)
	}
	if Tari(0) != 0 {
		t.Errorf("Tari(0) = %d, want 0", Tari(0))
	}
	if Tari(2) != 2_000_000 {
		t.Errorf("Tari(2) = %d, want 2_000_000", Tari(2))
	}
}

// TestNewAccount asserts a fresh account carries well-formed keys, a canonical address, and that
// the keypair re-derives the same address.
func TestNewAccount(t *testing.T) {
	a, err := NewAccount()
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if !isHex32(a.Keys.AccountSecret) || !isHex32(a.Keys.AccountPublicKey) {
		t.Errorf("malformed account keypair: %+v", a.Keys)
	}
	if !isHex32(a.View.ViewSecret) || !isHex32(a.View.ViewPublicKey) {
		t.Errorf("malformed view keypair: %+v", a.View)
	}
	if !strings.HasPrefix(a.Address, "component_") {
		t.Errorf("address is not canonical component_<hex>: %s", a.Address)
	}
	derived, err := a.Keys.DeriveAddress()
	if err != nil {
		t.Fatalf("DeriveAddress: %v", err)
	}
	if derived != a.Address {
		t.Errorf("DeriveAddress() = %s, want %s", derived, a.Address)
	}
}

// TestAccountFromSeed asserts the seed path is deterministic and that the account and view keys
// (distinct KDF branches) differ.
func TestAccountFromSeed(t *testing.T) {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	a1, err := AccountFromSeed(seed)
	if err != nil {
		t.Fatalf("AccountFromSeed: %v", err)
	}
	a2, err := AccountFromSeed(seed)
	if err != nil {
		t.Fatalf("AccountFromSeed (2): %v", err)
	}
	if a1 != a2 {
		t.Errorf("AccountFromSeed must be deterministic: %+v != %+v", a1, a2)
	}
	if a1.Keys.AccountSecret == a1.View.ViewSecret {
		t.Errorf("account and view secrets for the same seed must differ (distinct branches)")
	}
	if !strings.HasPrefix(a1.Address, "component_") {
		t.Errorf("address is not canonical component_<hex>: %s", a1.Address)
	}
}

// TestAccountKeyBundles asserts the accessor bundles carry the right secrets.
func TestAccountKeyBundles(t *testing.T) {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	a, err := AccountFromSeed(seed)
	if err != nil {
		t.Fatalf("AccountFromSeed: %v", err)
	}

	if got := a.TransferKeys(); got.AccountSecret != a.Keys.AccountSecret {
		t.Errorf("TransferKeys.AccountSecret = %q, want %q", got.AccountSecret, a.Keys.AccountSecret)
	}
	if got := a.StealthKeys(); got.AccountSecret != a.Keys.AccountSecret {
		t.Errorf("StealthKeys.AccountSecret = %q, want %q", got.AccountSecret, a.Keys.AccountSecret)
	}

	scan := a.ScanKeys()
	if scan.ViewSecret != a.View.ViewSecret {
		t.Errorf("ScanKeys.ViewSecret = %q, want %q", scan.ViewSecret, a.View.ViewSecret)
	}
	if scan.AccountSecret == nil {
		t.Fatalf("ScanKeys.AccountSecret is nil, want non-nil for an Account")
	}
	if *scan.AccountSecret != a.Keys.AccountSecret {
		t.Errorf("ScanKeys.AccountSecret = %q, want %q", *scan.AccountSecret, a.Keys.AccountSecret)
	}
}

// TestDeriveAddress_BadHex asserts a malformed public key surfaces a typed *Error (KEY).
func TestDeriveAddress_BadHex(t *testing.T) {
	_, err := AccountKeyPair{AccountPublicKey: "zz"}.DeriveAddress()
	if err == nil {
		t.Fatal("DeriveAddress on bad hex must error")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("DeriveAddress error is not *Error: %T %v", err, err)
	}
	if e.Code != "KEY" {
		t.Errorf("DeriveAddress error code = %q, want KEY", e.Code)
	}
}
