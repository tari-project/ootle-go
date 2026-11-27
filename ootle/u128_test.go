package ootle

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
)

func TestU128(t *testing.T) {
	v := U128{Hi: 1, Lo: 2}
	raw, err := json.Marshal(v)
	if err != nil || string(raw) != `{"hi":1,"lo":2}` {
		t.Fatalf("Marshal = %s, %v", raw, err)
	}
	var back U128
	if err := json.Unmarshal(raw, &back); err != nil || back != v {
		t.Fatalf("Unmarshal = %+v, %v", back, err)
	}
	if got, want := v.String(), "18446744073709551618"; got != want {
		t.Errorf("String = %s, want %s", got, want)
	}
	if got, ok := U128FromBig(v.Big()); !ok || got != v {
		t.Errorf("U128FromBig(Big) = %+v, %v", got, ok)
	}
	if _, ok := U128FromBig(new(big.Int).Lsh(big.NewInt(1), 128)); ok {
		t.Error("U128FromBig accepted 2^128")
	}
	if _, ok := U128FromBig(big.NewInt(-1)); ok {
		t.Error("U128FromBig accepted a negative value")
	}
	if n, ok := U128FromUint64(7).Uint64(); !ok || n != 7 {
		t.Errorf("Uint64 = %d, %v", n, ok)
	}
	if _, ok := v.Uint64(); ok {
		t.Error("Uint64 narrowed a value wider than a u64")
	}
}

// The builder sums revealed buckets at 128 bits: two max-u64 outputs reconcile with a matching
// total, which a u64 sum would have wrapped.
func TestStealthBuilder_RevealedSumIs128Bit(t *testing.T) {
	out := NewStealthOutput("aa", "bb", 1, TariResource).WithRevealed(math.MaxUint64)
	intent, err := NewStealthTransfer("component_x", TariResource).
		ToOutput(out).ToOutput(out).
		ToRevealedOutput(math.MaxUint64).ToRevealedOutput(math.MaxUint64).
		Intent(1)
	if err != nil {
		t.Fatalf("Intent: %v", err)
	}
	if want := (U128{Hi: 1, Lo: math.MaxUint64 - 1}); intent.RevealedOutputAmount != want {
		t.Fatalf("RevealedOutputAmount = %s, want %s", intent.RevealedOutputAmount, want)
	}
}
