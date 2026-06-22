package ootle

// Well-known resource addresses, mirroring the core's
// tari_template_lib_types::constants. These are network-independent fixed
// addresses, safe to use directly instead of hardcoding the hex.
const (
	// TariResource is the native TARI/XTR resource used to pay fees. It is a
	// fungible resource with 6 decimals (1 TARI = 1_000_000 µTari).
	TariResource = "resource_0101010101010101010101010101010101010101010101010101010101010101"

	// PublicIdentityResource is the resource for public identity-based
	// non-fungible tokens (virtual ownership by public key).
	PublicIdentityResource = "resource_0100000000000000000000000000000000000000000000000000000000000000"
)

// MicroTari is the base money unit: 1 µTari. Amounts are µTari throughout the SDK.
const MicroTari uint64 = 1

// Tari converts a whole-TARI amount to µTari (Tari(2) == 2_000_000), so call sites
// read in whole TARI instead of counting zeros.
func Tari(whole uint64) uint64 { return whole * 1_000_000 }
