//   Copyright 2026 The Tari Project
//   SPDX-License-Identifier: BSD-3-Clause

package cffi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// esmeraldaByte is the L1 discriminant byte for the "esmeralda" network (mirrors the ootle package's
// Network.ByteValue; duplicated here so this internal-package test stays self-contained).
const esmeraldaByte uint8 = 0x26

// buildRevealedStealthHandle builds a fresh stealth handle from the committed revealed-only fixture
// (zero stealth inputs ⇒ no fetch loop needed, so the build alone yields a usable handle). The caller
// owns the returned handle.
func buildRevealedStealthHandle(t *testing.T) *StealthHandle {
	t.Helper()
	// internal/cffi/ -> ../../ootle/testdata/fixtures/stealth_transfer/ (the SDK package owns the fixtures)
	path := filepath.Join("..", "..", "ootle", "testdata", "fixtures", "stealth_transfer", "account_key_seal_with_revealed_input.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		Input struct {
			Intent json.RawMessage `json:"stealth_intent"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	h, _, err := BuildStealthUnsigned(esmeraldaByte, string(fx.Input.Intent))
	if err != nil {
		t.Fatalf("BuildStealthUnsigned: %v", err)
	}
	if h == nil {
		t.Fatal("BuildStealthUnsigned returned a nil handle")
	}
	return h
}

// TestStealthHandleToPublicConsumerIsDeterministicError proves the C-side kind-tag guard
// turns a misrouted opaque handle into a deterministic error rather
// than undefined behaviour: a stealth handle reinterpreted as a public handle and fed to the public
// ApplyFetchedSubstates consumer returns a clean error (kind mismatch → "INVALID"), does not crash,
// and is NOT consumed — so it is still freed correctly afterwards with the stealth free fn (a bad
// free / double free would crash the test).
func TestStealthHandleToPublicConsumerIsDeterministicError(t *testing.T) {
	sh := buildRevealedStealthHandle(t)
	// The guard must leave the handle intact on the rejected misroute; free it once, correctly.
	defer FreeStealthHandle(sh)

	err := RouteStealthHandleToPublicApply(sh, "[]")
	if err == nil {
		t.Fatal("routing a stealth handle to the public ApplyFetchedSubstates must error, not succeed (kind guard failed)")
	}
	cerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected a typed *Error, got %T: %v", err, err)
	}
	if cerr.Code != "INVALID" {
		t.Fatalf("expected a deterministic INVALID kind-mismatch error, got code %q (%s)", cerr.Code, cerr.Message)
	}
}
