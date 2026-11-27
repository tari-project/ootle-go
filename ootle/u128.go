package ootle

import (
	"math/big"
	"math/bits"
)

// U128 is an unsigned 128-bit amount in µTari, the core's boundary shape for a value wider than a
// u64: {"hi": <u64>, "lo": <u64>}, reassembled as hi<<64 | lo. Revealed amounts cross the C ABI this
// way (stealth revealed inputs/outputs and revealed vault/account balances), since they span the
// engine's full Amount width. The zero value is zero.
type U128 struct {
	Hi uint64 `json:"hi"`
	Lo uint64 `json:"lo"`
}

// U128FromUint64 widens a u64 µTari amount.
func U128FromUint64(v uint64) U128 { return U128{Lo: v} }

// U128FromBig converts a non-negative big.Int that fits in 128 bits; ok is false otherwise.
func U128FromBig(v *big.Int) (U128, bool) {
	if v == nil || v.Sign() < 0 || v.BitLen() > 128 {
		return U128{}, false
	}
	lo := new(big.Int).And(v, new(big.Int).SetUint64(^uint64(0))).Uint64()
	hi := new(big.Int).Rsh(v, 64).Uint64()
	return U128{Hi: hi, Lo: lo}, true
}

// IsZero reports whether the amount is zero.
func (u U128) IsZero() bool { return u.Hi == 0 && u.Lo == 0 }

// Uint64 narrows the amount to a u64; ok is false when it does not fit.
func (u U128) Uint64() (v uint64, ok bool) { return u.Lo, u.Hi == 0 }

// Big returns the amount as a big.Int.
func (u U128) Big() *big.Int {
	v := new(big.Int).SetUint64(u.Hi)
	v.Lsh(v, 64)
	return v.Or(v, new(big.Int).SetUint64(u.Lo))
}

// String formats the amount in decimal.
func (u U128) String() string { return u.Big().String() }

// add returns u+v and whether it overflowed 128 bits.
func (u U128) add(v U128) (U128, bool) {
	lo, carry := bits.Add64(u.Lo, v.Lo, 0)
	hi, overflow := bits.Add64(u.Hi, v.Hi, carry)
	return U128{Hi: hi, Lo: lo}, overflow != 0
}
