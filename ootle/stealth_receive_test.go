package ootle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// scanFixture mirrors a committed stealth_scan/* vector (compare:"bytes" — scanning is RNG-free
// and byte-stable). The input carries the network, the scan keys (view_secret + optional
// account_secret + skip_memo), and the InboundStealthOutput to scan. The expected block is either
// the full DecryptedOutput (is_mine) or a {"$none":true} marker (not mine).
type scanFixture struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Input     struct {
		ScanInput struct {
			Network       Network              `json:"network"`
			ViewSecret    string               `json:"view_secret"`
			AccountSecret *string              `json:"account_secret"`
			SkipMemo      bool                 `json:"skip_memo"`
			Output        InboundStealthOutput `json:"output"`
		} `json:"stealth_scan_input"`
	} `json:"input"`
	Expected struct {
		Decrypted json.RawMessage `json:"decrypted"`
	} `json:"expected"`
}

// scanKeys builds the StealthScanKeys from the fixture's scan input.
func (f scanFixture) scanKeys() StealthScanKeys {
	return StealthScanKeys{
		ViewSecret:    f.Input.ScanInput.ViewSecret,
		AccountSecret: f.Input.ScanInput.AccountSecret,
		SkipMemo:      f.Input.ScanInput.SkipMemo,
	}
}

// expectedDecrypted decodes the fixture's expected block. The bool reports whether a decrypted
// output is expected (true) or the not-mine marker ({"$none":true}) is present (false).
func (f scanFixture) expectedDecrypted(t *testing.T) (DecryptedOutput, bool) {
	t.Helper()
	var none struct {
		None bool `json:"$none"`
	}
	if err := json.Unmarshal(f.Expected.Decrypted, &none); err == nil && none.None {
		return DecryptedOutput{}, false
	}
	var out DecryptedOutput
	if err := json.Unmarshal(f.Expected.Decrypted, &out); err != nil {
		t.Fatalf("decode expected.decrypted: %v", err)
	}
	return out, true
}

func loadScanFixture(t *testing.T, name string) scanFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "stealth_scan", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scan fixture %s: %v", name, err)
	}
	var f scanFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode scan fixture %s: %v", name, err)
	}
	if f.Operation != "scan_stealth_output" {
		t.Fatalf("scan fixture %s has operation %q, want scan_stealth_output", name, f.Operation)
	}
	return f
}

// (a) Scan an output the core produced: a known-mine fixture decrypts to the expected value/mask.
func TestScanStealthOutput_Mine(t *testing.T) {
	f := loadScanFixture(t, "mine_basic.json")
	want, isMine := f.expectedDecrypted(t)
	if !isMine {
		t.Fatalf("fixture mine_basic expected a decrypted output, got the not-mine marker")
	}

	got, err := ScanStealthOutput(f.Input.ScanInput.Network, f.scanKeys(), f.Input.ScanInput.Output)
	if err != nil {
		t.Fatalf("ScanStealthOutput returned error: %v", err)
	}
	if got == nil {
		t.Fatal("ScanStealthOutput returned nil without an error")
	}
	if !got.IsMine {
		t.Fatalf("IsMine = false, want true")
	}
	if got.Value != want.Value {
		t.Errorf("Value = %d, want %d", got.Value, want.Value)
	}
	if got.Mask != want.Mask {
		t.Errorf("Mask = %q, want %q", got.Mask, want.Mask)
	}
	// mine_basic sets skip_memo=true, so the memo must be nil regardless of the fixture's
	// expected.memo (which is null here).
	if got.Memo != nil {
		t.Errorf("Memo = %+v, want nil (skip_memo set)", got.Memo)
	}
}

// (b) Output not mine: the not-mine fixture yields a non-nil DecryptedOutput with IsMine=false and
// zeroed value/mask — and NO error.
func TestScanStealthOutput_NotMine(t *testing.T) {
	f := loadScanFixture(t, "not_mine.json")
	if _, isMine := f.expectedDecrypted(t); isMine {
		t.Fatalf("fixture not_mine expected the not-mine marker, got a decrypted output")
	}

	got, err := ScanStealthOutput(f.Input.ScanInput.Network, f.scanKeys(), f.Input.ScanInput.Output)
	if err != nil {
		t.Fatalf("ScanStealthOutput returned error for a not-mine output: %v", err)
	}
	if got == nil {
		t.Fatal("ScanStealthOutput returned nil for a not-mine output; want non-nil with IsMine=false")
	}
	if got.IsMine {
		t.Fatalf("IsMine = true, want false")
	}
	if got.Value != 0 {
		t.Errorf("Value = %d, want 0 for a not-mine output", got.Value)
	}
	if got.Mask != "" {
		t.Errorf("Mask = %q, want \"\" for a not-mine output", got.Mask)
	}
	if got.Memo != nil {
		t.Errorf("Memo = %+v, want nil for a not-mine output", got.Memo)
	}
}

// (c) Core error surfaced: a malformed (wrong-width) view secret is rejected by the core with a
// typed *Error carrying a stable code (PARSE/KEY/VALIDATION per the boundary error model).
func TestScanStealthOutput_MalformedKey(t *testing.T) {
	f := loadScanFixture(t, "mine_basic.json")
	bad := f.scanKeys()
	bad.ViewSecret = "00" // valid hex but wrong width (1 byte, not 32) → core parse/key error

	got, err := ScanStealthOutput(f.Input.ScanInput.Network, bad, f.Input.ScanInput.Output)
	if err == nil {
		t.Fatalf("expected an error for a wrong-width view secret, got nil (out=%+v)", got)
	}
	if got != nil {
		t.Errorf("expected nil *DecryptedOutput alongside the error, got %+v", got)
	}
	oe, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *ootle.Error", err)
	}
	switch oe.Code {
	case "PARSE", "KEY", "VALIDATION":
		// expected structural codes
	default:
		t.Errorf("error code = %q, want one of PARSE/KEY/VALIDATION", oe.Code)
	}
}

// (d) cffi wrapper isolation: calling cffi.ScanStealthOutput directly with valid fixture inputs
// returns a non-empty dataJSON that parses to a DecryptedOutput, with no C panic.
func TestCffiScanStealthOutput_Direct(t *testing.T) {
	f := loadScanFixture(t, "mine_basic.json")
	netByte, ok := f.Input.ScanInput.Network.ByteValue()
	if !ok {
		t.Fatalf("unknown network %q", f.Input.ScanInput.Network)
	}
	scanKeysJSON, err := json.Marshal(f.scanKeys())
	if err != nil {
		t.Fatalf("marshal scan keys: %v", err)
	}
	outputJSON, err := json.Marshal(f.Input.ScanInput.Output)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}

	dataJSON, cerr := cffi.ScanStealthOutput(netByte, string(scanKeysJSON), string(outputJSON))
	if cerr != nil {
		t.Fatalf("cffi.ScanStealthOutput returned error: %v", cerr)
	}
	if dataJSON == "" {
		t.Fatal("cffi.ScanStealthOutput returned empty dataJSON")
	}
	var out DecryptedOutput
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		t.Fatalf("dataJSON does not parse to DecryptedOutput: %v (raw=%s)", err, dataJSON)
	}
	if !out.IsMine {
		t.Errorf("IsMine = false, want true for mine_basic")
	}
}

// ScanStealthOutputs (batch helper): a mix of mine + not-mine inputs returns only the mine ones.
//
// Note: the two committed fixtures share the same sender_public_nonce, so the not-mine fixture's
// ciphertext (crafted to be not-mine only vs its OWN c8.. view secret) still decrypts under the
// mine fixture's 78.. view secret. To get a genuinely not-mine input here we corrupt the mine
// output's encrypted_data (an AEAD tag mismatch ⇒ decrypt fails ⇒ IsMine=false), and pair it with
// the untouched mine output (IsMine=true), both scanned under the mine keys.
func TestScanStealthOutputs_FiltersMine(t *testing.T) {
	mine := loadScanFixture(t, "mine_basic.json")

	corrupt := mine.Input.ScanInput.Output
	// Flip the first byte of the ciphertext so the AEAD authentication fails ⇒ not mine.
	if len(corrupt.EncryptedData) < 2 {
		t.Fatal("mine fixture encrypted_data too short to corrupt")
	}
	flip := "ff"
	if corrupt.EncryptedData[:2] == "ff" {
		flip = "00"
	}
	corrupt.EncryptedData = flip + corrupt.EncryptedData[2:]

	results, err := ScanStealthOutputs(
		mine.Input.ScanInput.Network,
		mine.scanKeys(),
		[]InboundStealthOutput{mine.Input.ScanInput.Output, corrupt},
	)
	if err != nil {
		t.Fatalf("ScanStealthOutputs returned error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d mine outputs, want 1", len(results))
	}
	if !results[0].IsMine {
		t.Errorf("returned output IsMine = false, want true")
	}
}
