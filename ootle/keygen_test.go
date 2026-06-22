package ootle

import (
	"encoding/hex"
	"testing"
)

// isHex32 reports whether s is a 64-char lowercase-hex string (a 32-byte key).
func isHex32(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// TestGenerateAccountKey_ProducesFreshHexKeys asserts the production account keygen returns a
// well-formed (64-hex) secret + public key and that two calls differ (OsRng).
func TestGenerateAccountKey_ProducesFreshHexKeys(t *testing.T) {
	a, err := GenerateAccountKey()
	if err != nil {
		t.Fatalf("GenerateAccountKey: %v", err)
	}
	if !isHex32(a.AccountSecret) || !isHex32(a.AccountPublicKey) {
		t.Fatalf("malformed account keypair: %+v", a)
	}
	b, err := GenerateAccountKey()
	if err != nil {
		t.Fatalf("GenerateAccountKey (2): %v", err)
	}
	if a.AccountSecret == b.AccountSecret {
		t.Errorf("two OsRng account draws must differ, got identical secret")
	}
}

// TestGenerateViewKey_ProducesFreshHexKeys mirrors the account check for the view keypair.
func TestGenerateViewKey_ProducesFreshHexKeys(t *testing.T) {
	v, err := GenerateViewKey()
	if err != nil {
		t.Fatalf("GenerateViewKey: %v", err)
	}
	if !isHex32(v.ViewSecret) || !isHex32(v.ViewPublicKey) {
		t.Fatalf("malformed view keypair: %+v", v)
	}
}

// TestDeriveKeysFromSeed_AreReproducibleAndDomainSeparated asserts the seed path is reproducible and
// that account vs view keys derived from the SAME seed differ (distinct KDF branch labels).
func TestDeriveKeysFromSeed_AreReproducibleAndDomainSeparated(t *testing.T) {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	a1, err := DeriveAccountKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("DeriveAccountKeyFromSeed: %v", err)
	}
	a2, err := DeriveAccountKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("DeriveAccountKeyFromSeed (2): %v", err)
	}
	if a1 != a2 {
		t.Errorf("seed path must be reproducible: %+v != %+v", a1, a2)
	}
	if !isHex32(a1.AccountSecret) || !isHex32(a1.AccountPublicKey) {
		t.Fatalf("malformed derived account keypair: %+v", a1)
	}

	v1, err := DeriveViewKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("DeriveViewKeyFromSeed: %v", err)
	}
	if a1.AccountSecret == v1.ViewSecret {
		t.Errorf("account and view keys for the same seed must differ (distinct branch labels)")
	}
}

// TestDeriveAccountKeyFromSeed_MatchesVendoredVector cross-checks the typed wrapper against the
// committed keys/account_from_seed vector (an extra explicit guard alongside the golden runner).
func TestDeriveAccountKeyFromSeed_MatchesVendoredVector(t *testing.T) {
	const seedHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	const wantPublic = "f6f89e316e6ba5f05e5250ddd4a5d3ed39dcd038cf812cc6a154b6ec0951d25f"
	const wantSecret = "62b68e1ef95edb2461279871c378d95a1189fe824f2dbcbcdb53d659d77d7a06"

	var seed [32]byte
	for i := 0; i < 32; i++ {
		seed[i] = byte(i + 1)
	}
	got, err := DeriveAccountKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("DeriveAccountKeyFromSeed: %v", err)
	}
	if got.AccountSecret != wantSecret || got.AccountPublicKey != wantPublic {
		t.Errorf("derived account keypair mismatch:\n got:  %+v\n want secret=%s public=%s", got, wantSecret, wantPublic)
	}
}

// TestDeriveAccountAddress_MatchesVendoredVector derives the account address from the public key the
// keygen vector produces (the keygen → address-derive chain) and asserts it matches the committed
// address-derive vector (the lost-funds vector). The derivation stays in the core; Go only marshals.
func TestDeriveAccountAddress_MatchesVendoredVector(t *testing.T) {
	// The seed-derived account public key from keys/account_from_seed.json.
	const pubHex = "f6f89e316e6ba5f05e5250ddd4a5d3ed39dcd038cf812cc6a154b6ec0951d25f"
	const wantAddr = "component_26cf65a80010d961aa64950a5677fd9d3852adcf3618aa7fe171f6dda8b961ae"

	pkBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pkBytes) != 32 {
		t.Fatalf("bad pubHex (err=%v len=%d)", err, len(pkBytes))
	}
	var pk [32]byte
	copy(pk[:], pkBytes)

	got, err := DeriveAccountAddress(pk)
	if err != nil {
		t.Fatalf("DeriveAccountAddress: %v", err)
	}
	if got != wantAddr {
		t.Errorf("derived address mismatch:\n got:  %s\n want: %s", got, wantAddr)
	}
}

// TestDeriveAccountAddress_IsDeterministicAndCanonical asserts the same public key derives the same
// canonical component_<hex> on every call, and distinct keys derive distinct addresses. (The Go
// wrapper always sends a well-formed 32-byte hex, so the PARSE path is exercised at the FFI level in
// the Rust c_abi tests, not here.)
func TestDeriveAccountAddress_IsDeterministicAndCanonical(t *testing.T) {
	var a, b [32]byte
	for i := range a {
		a[i] = 7
	}
	b[0] = 9

	addrA1, err := DeriveAccountAddress(a)
	if err != nil {
		t.Fatalf("DeriveAccountAddress(a): %v", err)
	}
	addrA2, err := DeriveAccountAddress(a)
	if err != nil {
		t.Fatalf("DeriveAccountAddress(a) again: %v", err)
	}
	if addrA1 != addrA2 {
		t.Errorf("derivation is not deterministic: %s != %s", addrA1, addrA2)
	}
	if len(addrA1) < len("component_") || addrA1[:len("component_")] != "component_" {
		t.Errorf("address is not canonical component_<hex>: %s", addrA1)
	}
	addrB, err := DeriveAccountAddress(b)
	if err != nil {
		t.Fatalf("DeriveAccountAddress(b): %v", err)
	}
	if addrA1 == addrB {
		t.Errorf("distinct keys derived identical addresses: %s", addrA1)
	}
}
