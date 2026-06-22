// Package cffi is the sole holder of `import "C"` and `unsafe` in ootle-go. It wraps
// the flat C ABI exposed by the `ootle_sdk_ffi_c` crate (vendored header + static lib
// under ./lib) and presents a small, safe Go surface to the rest of the module. No other
// package may import "C".
//
// Memory / ownership discipline (mirrors ootle_sdk.h):
//   - Every C string argument is built with C.CString and freed with C.free (deferred).
//   - Every returned OotleResult is freed exactly once with ootle_result_free, which
//     frees its three strings but NEVER the handle.
//   - The opaque handle (OotlePartialTransaction*) has its own lifecycle: consuming ops
//     (apply/seal) take it by value, so it must never be freed afterwards; a handle that
//     is never consumed is freed with ootle_partial_transaction_free.
package cffi

/*
#cgo CFLAGS: -I${SRCDIR}/lib
#cgo darwin,arm64  LDFLAGS: ${SRCDIR}/lib/darwin_arm64/libootle_sdk_ffi_c.a -ldl -lm
#cgo darwin,amd64  LDFLAGS: ${SRCDIR}/lib/darwin_amd64/libootle_sdk_ffi_c.a -ldl -lm
#cgo linux,amd64   LDFLAGS: ${SRCDIR}/lib/linux_amd64/libootle_sdk_ffi_c.a -ldl -lm -lpthread
#cgo linux,arm64   LDFLAGS: ${SRCDIR}/lib/linux_arm64/libootle_sdk_ffi_c.a -ldl -lm -lpthread
#cgo windows,amd64 LDFLAGS: ${SRCDIR}/lib/windows_amd64/libootle_sdk_ffi_c.a -lws2_32 -luserenv -lbcrypt -lntdll -ladvapi32
#cgo darwin LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include "ootle_sdk.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// ExpectedABIVersion is the frozen ABI tag the vendored lib must report. A mismatch
// means the vendored lib drifted from this wrapper — fail loudly rather than
// mis-marshal. Keep in sync with `ootle_abi_version()` in ootle_sdk.h.
const ExpectedABIVersion = "ootle-sdk-ffi-c/15"

var (
	abiOnce sync.Once
	abiErr  error
)

// Error is the typed error carried across the boundary: the stable core error code
// (e.g. "PARSE", "KEY", "VALIDATION", "INTERNAL") plus the human-readable message.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Handle is an opaque, non-nil pointer to a core PartialTransaction. The rest of the
// module threads it across two-phase calls without inspecting it. Consuming ops set
// it to a freed state by taking ownership; see FreeHandle.
type Handle struct {
	ptr *C.OotlePartialTransaction
}

// checkABIVersion asserts the vendored lib's ABI tag matches ExpectedABIVersion. It is
// called once before any op via ensureABI. The returned pointer is static storage and
// must NOT be freed.
func checkABIVersion() error {
	got := C.GoString(C.ootle_abi_version())
	if got != ExpectedABIVersion {
		return fmt.Errorf("ootle ABI version mismatch: vendored lib reports %q, wrapper expects %q (rebuild via make build / scripts/build_native.sh)", got, ExpectedABIVersion)
	}
	return nil
}

func ensureABI() error {
	abiOnce.Do(func() { abiErr = checkABIVersion() })
	return abiErr
}

// envelope is the decoded, Go-owned view of an OotleResult. Strings are copied out of
// C memory (so the C strings can be freed immediately); handle ownership transfers to
// the caller.
type envelope struct {
	ok       bool
	code     string
	message  string
	dataJSON string
	handle   *C.OotlePartialTransaction
}

// consume reads an OotleResult into a Go-owned envelope and frees the result's three
// C strings exactly once. It does NOT free the handle (that ownership transfers out).
func consume(res C.OotleResult) envelope {
	// All three string fields are copied into Go-owned memory BEFORE ootle_result_free
	// runs (it frees those C strings). data_json is NULL on the error path and may be
	// NULL generally; cStr guards it. error_code/error_message are "" (never NULL) on
	// success and set on error, but cStr keeps them defensive too. handle ownership
	// transfers out — ootle_result_free never touches it.
	env := envelope{
		ok:       res.ok == 1,
		code:     cStr(res.error_code),
		message:  cStr(res.error_message),
		dataJSON: cStr(res.data_json),
		handle:   res.handle,
	}
	C.ootle_result_free(res)
	return env
}

// cStr copies a (possibly NULL) C string into a Go string; NULL maps to "".
func cStr(p *C.char) string {
	if p == nil {
		return ""
	}
	return C.GoString(p)
}

// asError turns a non-ok envelope into a typed *Error. Callers must check env.ok first.
func (e envelope) asError() error {
	return &Error{Code: e.code, Message: e.message}
}

// FreeHandle frees a never-consumed handle (null-safe). Call it on a handle that will
// NOT be passed to a consuming op (apply/seal). Never call it after a consuming op.
func FreeHandle(h *Handle) {
	if h == nil || h.ptr == nil {
		return
	}
	C.ootle_partial_transaction_free(h.ptr)
	h.ptr = nil
}

// BuildAndEncodePublicTransfer wraps ootle_build_and_encode_public_transfer (random-nonce
// default: a fresh OS-RNG seed). keysJSON is {account_secret}. The bytes/id are not
// reproducible. Returns the data JSON ({encoded_transaction, transaction_id}) on success.
func BuildAndEncodePublicTransfer(networkByte uint8, intentJSON, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	env := consume(C.ootle_build_and_encode_public_transfer(C.uint8_t(networkByte), cIntent, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// BuildAndEncodePublicTransferWithSeed wraps the seed-reproducible counterpart. keysJSON is
// {account_secret, seed [, seal_secret]}. The bytes/id are reproducible byte-for-byte from the
// seed + intent.
func BuildAndEncodePublicTransferWithSeed(networkByte uint8, intentJSON, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	env := consume(C.ootle_build_and_encode_public_transfer_with_seed(C.uint8_t(networkByte), cIntent, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// BuildUnsigned wraps ootle_build_unsigned (phase 1). On success it returns a fresh
// Handle (caller owns it — must consume it via Apply/Seal or free via FreeHandle) and
// the want-list data JSON.
func BuildUnsigned(networkByte uint8, intentJSON string) (*Handle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))

	env := consume(C.ootle_build_unsigned(C.uint8_t(networkByte), cIntent))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_build_unsigned returned ok without a handle"}
	}
	return &Handle{ptr: env.handle}, env.dataJSON, nil
}

// BuildUnsignedInstructions wraps ootle_build_unsigned_instructions (phase 1, the generic builder).
// It is the SINGLE generic-builder entry point: it lowers a GenericTransactionIntent (an explicit
// instruction list) and returns the SAME public Handle + want-list data JSON as BuildUnsigned. The
// returned handle is driven by the EXISTING ApplyFetchedSubstates + SealAndEncode[Production] — there
// are no generic apply/seal/free wrappers (one new entry, zero new lifecycle surface). Caller owns
// the handle (consume via Apply/Seal or free via FreeHandle).
func BuildUnsignedInstructions(networkByte uint8, intentJSON string) (*Handle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))

	env := consume(C.ootle_build_unsigned_instructions(C.uint8_t(networkByte), cIntent))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_build_unsigned_instructions returned ok without a handle"}
	}
	return &Handle{ptr: env.handle}, env.dataJSON, nil
}

// BuildFaucetClaim wraps ootle_build_faucet_claim (phase 1, the builtin faucet builder). intentJSON is
// a FaucetClaimIntent; the core emits the complete self-funding claim and owns the faucet's input set.
// It returns the SAME public Handle + want-list data JSON as BuildUnsignedInstructions and is driven by
// the EXISTING ApplyFetchedSubstates + SealAndEncode[Production] surface — no new lifecycle. Caller owns
// the handle (consume via Apply/Seal or free via FreeHandle).
func BuildFaucetClaim(networkByte uint8, intentJSON string) (*Handle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))

	env := consume(C.ootle_build_faucet_claim(C.uint8_t(networkByte), cIntent))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_build_faucet_claim returned ok without a handle"}
	}
	return &Handle{ptr: env.handle}, env.dataJSON, nil
}

// ApplyFetchedSubstates wraps ootle_apply_fetched_substates (phase 2). It CONSUMES the
// input handle (the C side takes it by value, even on error). On success it returns a
// new Handle to thread forward plus the resolution data JSON. The input Handle is
// invalidated regardless of outcome — callers must not reuse or free it.
func ApplyFetchedSubstates(h *Handle, fetchedJSON string) (*Handle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	if h == nil || h.ptr == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ApplyFetchedSubstates called with a nil/consumed handle"}
	}
	cFetched := C.CString(fetchedJSON)
	defer C.free(unsafe.Pointer(cFetched))

	ptr := h.ptr
	h.ptr = nil // the C call consumes it; invalidate our copy first to prevent double-free.
	env := consume(C.ootle_apply_fetched_substates(ptr, cFetched))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_apply_fetched_substates returned ok without a handle"}
	}
	return &Handle{ptr: env.handle}, env.dataJSON, nil
}

// SealAndEncode wraps ootle_seal_and_encode (random-nonce default). It CONSUMES the handle and
// returns the encoded data JSON. keysJSON is {account_secret}; the bytes/id are not reproducible.
// No handle is returned. The input Handle is invalidated regardless of outcome.
func SealAndEncode(h *Handle, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncode called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	ptr := h.ptr
	h.ptr = nil
	env := consume(C.ootle_seal_and_encode(ptr, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// SealAndEncodeWithSeed wraps the seed-reproducible counterpart. CONSUMES the handle. keysJSON is
// {account_secret, seed [, seal_secret]}; the bytes/id are reproducible byte-for-byte.
func SealAndEncodeWithSeed(h *Handle, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncodeWithSeed called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	ptr := h.ptr
	h.ptr = nil
	env := consume(C.ootle_seal_and_encode_with_seed(ptr, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// ParseFinalizedResult wraps ootle_parse_finalized_result. Returns the serialized
// FinalizedResult JSON on success.
func ParseFinalizedResult(rawJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cRaw := C.CString(rawJSON)
	defer C.free(unsafe.Pointer(cRaw))

	env := consume(C.ootle_parse_finalized_result(cRaw))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// ABIVersion returns the vendored lib's reported ABI tag (does not free — static storage).
func ABIVersion() string {
	return C.GoString(C.ootle_abi_version())
}

// --- identity keygen (group A) ----------------------------------------------------------
//
// Stateless, handle-free ops: mint a fresh keypair (OsRng, production) or derive one
// deterministically from a 32-byte seed (the canonical wallet KDF — no RNG). All four return
// the keypair as a JSON object on success ({account_secret, account_public_key} or
// {view_secret, view_public_key}, lowercase hex). The crypto stays in the core.

// GenerateAccountKey wraps ootle_generate_account_key. Returns the account keypair data JSON
// ({account_secret, account_public_key}, lowercase hex). Non-reproducible (OsRng).
func GenerateAccountKey() (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	env := consume(C.ootle_generate_account_key())
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// GenerateViewKey wraps ootle_generate_view_key. Returns the view keypair data JSON
// ({view_secret, view_public_key}, lowercase hex). Non-reproducible (OsRng).
func GenerateViewKey() (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	env := consume(C.ootle_generate_view_key())
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// DeriveAccountKeyFromSeed wraps ootle_derive_account_key_from_seed. seedHex is a lowercase-hex
// 32-byte seed; bad/odd/uppercase/wrong-length hex is a "PARSE" error. Returns the account keypair
// data JSON. Reproducible byte-for-byte for the same seed.
func DeriveAccountKeyFromSeed(seedHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cSeed := C.CString(seedHex)
	defer C.free(unsafe.Pointer(cSeed))

	env := consume(C.ootle_derive_account_key_from_seed(cSeed))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// DeriveViewKeyFromSeed wraps ootle_derive_view_key_from_seed. seedHex is a lowercase-hex 32-byte
// seed; bad hex is a "PARSE" error. Returns the view keypair data JSON. Reproducible.
func DeriveViewKeyFromSeed(seedHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cSeed := C.CString(seedHex)
	defer C.free(unsafe.Pointer(cSeed))

	env := consume(C.ootle_derive_view_key_from_seed(cSeed))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// DeriveAccountAddress wraps ootle_derive_account_address. accountPublicKeyHex is a lowercase-hex
// 32-byte account public key; bad/odd/uppercase/wrong-length hex is a "PARSE" error. Returns the
// data JSON ({component_address: "component_<hex>"}). The consensus-shaped derivation stays in the
// core — Go only marshals the hex in and the address string out.
func DeriveAccountAddress(accountPublicKeyHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cPK := C.CString(accountPublicKeyHex)
	defer C.free(unsafe.Pointer(cPK))

	env := consume(C.ootle_derive_account_address(cPK))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// --- address codec (group A) ------------------------------------------------------------
//
// Two stateless, handle-free ops over one address surface. ParseAddress handles BOTH a
// component_/resource_<hex> substate id AND an otl_… bech32m identity; FormatIdentityAddress
// emits the otl_… bech32m. The bech32m codec stays in the core — Go only marshals strings.

// ParseAddress wraps ootle_parse_address. addressStr is a component_/resource_<hex> substate id
// or an otl_… bech32m identity. Returns the kind-tagged ParsedAddress data JSON on success; an
// unknown prefix / malformed id / bad bech32m is a "PARSE" error.
func ParseAddress(addressStr string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cAddr := C.CString(addressStr)
	defer C.free(unsafe.Pointer(cAddr))

	env := consume(C.ootle_parse_address(cAddr))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// FormatIdentityAddress wraps ootle_format_identity_address. networkByte selects the
// network-qualified HRP; accountKeyHex / viewOnlyKeyHex are lowercase-hex 32-byte public keys;
// payRefHex is an OPTIONAL lowercase-hex payment reference ("" ⇒ no pay_ref, passed as a NULL
// pointer). Returns the {bech32m} data JSON on success. Bad key/pay_ref hex or an oversize pay_ref
// (> 64 bytes) is a "PARSE" error; an unknown network byte / null key is "INVALID".
func FormatIdentityAddress(networkByte uint8, accountKeyHex, viewOnlyKeyHex, payRefHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cAccount := C.CString(accountKeyHex)
	defer C.free(unsafe.Pointer(cAccount))
	cView := C.CString(viewOnlyKeyHex)
	defer C.free(unsafe.Pointer(cView))

	// An empty payRefHex maps to a NULL C pointer (the absent pay_ref), matching the C ABI's
	// nullable pay_ref_hex contract. A non-empty value is marshalled and freed normally.
	var cPayRef *C.char
	if payRefHex != "" {
		cPayRef = C.CString(payRefHex)
	}
	defer C.free(unsafe.Pointer(cPayRef))

	env := consume(C.ootle_format_identity_address(C.uint8_t(networkByte), cAccount, cView, cPayRef))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// --- stealth send -----------------------------------------------------------------------
//
// The stealth send flow threads a SEPARATE opaque handle type, OotleStealthPartialTransaction,
// distinct from the public-path OotlePartialTransaction. The shared OotleResult envelope types
// its `handle` field as *OotlePartialTransaction, so the stealth build fns return the stealth
// handle in that field BY CAST (see ootle_sdk.h on ootle_build_stealth_unsigned). The host must
// reinterpret it as *OotleStealthPartialTransaction and route it ONLY to the *_stealth* consumers
// / ootle_stealth_partial_transaction_free — NEVER to the public-path apply/seal/free (which would
// reinterpret the wrong type = UB). StealthHandle below enforces that routing at the Go type level.

// StealthHandle is an opaque, non-nil pointer to a core StealthPartialTransaction (a DIFFERENT type
// from Handle). It is returned by BuildStealthUnsigned and consumed by SealAndEncodeStealthTransfer*
// (or freed via FreeStealthHandle if abandoned). It must never be passed to the public-path ops.
type StealthHandle struct {
	ptr *C.OotleStealthPartialTransaction
}

// FreeStealthHandle frees a never-consumed stealth handle (null-safe). Call it on a handle that
// will NOT be passed to a consuming stealth seal op. Never call it after a consuming op, and never
// pass a public-path Handle to it.
func FreeStealthHandle(h *StealthHandle) {
	if h == nil || h.ptr == nil {
		return
	}
	C.ootle_stealth_partial_transaction_free(h.ptr)
	h.ptr = nil
}

// BuildAndEncodeStealthTransfer wraps ootle_build_and_encode_stealth_transfer — the one-shot
// random-nonce (fresh OS-RNG seed) stealth send. fetchedJSON is a JSON array of FetchedSubstate
// (every stealth-input UTXO up front), spendSecretsJSON a JSON array of hex scalars (positional
// per intent.inputs); keysJSON is {account_secret}. The bytes/id are not reproducible. Returns
// the EncodedPublicTransfer data JSON on success. Stateless — no handle.
func BuildAndEncodeStealthTransfer(networkByte uint8, intentJSON, fetchedJSON, spendSecretsJSON, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cFetched := C.CString(fetchedJSON)
	defer C.free(unsafe.Pointer(cFetched))
	cSecrets := C.CString(spendSecretsJSON)
	defer C.free(unsafe.Pointer(cSecrets))
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	env := consume(C.ootle_build_and_encode_stealth_transfer(C.uint8_t(networkByte), cIntent, cFetched, cSecrets, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// BuildAndEncodeStealthTransferWithSeed wraps the seed-reproducible counterpart. keysJSON is
// {account_secret, seed}; the single build seed pins every derived nonce. The signatures the core
// produces are reproducible; the embedded proofs are not byte-stable. Stateless — no handle.
func BuildAndEncodeStealthTransferWithSeed(networkByte uint8, intentJSON, fetchedJSON, spendSecretsJSON, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cFetched := C.CString(fetchedJSON)
	defer C.free(unsafe.Pointer(cFetched))
	cSecrets := C.CString(spendSecretsJSON)
	defer C.free(unsafe.Pointer(cSecrets))
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	env := consume(C.ootle_build_and_encode_stealth_transfer_with_seed(C.uint8_t(networkByte), cIntent, cFetched, cSecrets, cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// BuildStealthUnsigned wraps ootle_build_stealth_unsigned (phase 1, random-nonce default). On success
// it returns a fresh StealthHandle (caller owns it — must drive it via ApplyFetchedSubstatesStealth to
// resolved, then consume it via SealAndEncodeStealthTransfer*, or free via FreeStealthHandle) plus
// the want-list data JSON ({"want_list":[…]}). The build takes no fetched/spend_secrets up
// front: the host drives the NeedMore fetch loop (same as the public path).
//
// The handle comes back in OotleResult.handle typed as *OotlePartialTransaction; it is the stealth
// type by cast (see the header). We reinterpret it as *OotleStealthPartialTransaction via
// unsafe.Pointer and wrap it in StealthHandle so it can never reach the public-path ops.
func BuildStealthUnsigned(networkByte uint8, intentJSON string) (*StealthHandle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))

	env := consume(C.ootle_build_stealth_unsigned(C.uint8_t(networkByte), cIntent))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_build_stealth_unsigned returned ok without a handle"}
	}
	return &StealthHandle{ptr: (*C.OotleStealthPartialTransaction)(unsafe.Pointer(env.handle))}, env.dataJSON, nil
}

// BuildStealthUnsignedWithSeed wraps the seed-reproducible counterpart. seedHex is the lowercase-hex
// 32-byte build seed that pins every derived nonce; seal the resulting handle with the seeded seal.
// Returns the handle + want-list data JSON.
func BuildStealthUnsignedWithSeed(networkByte uint8, intentJSON, seedHex string) (*StealthHandle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cSeed := C.CString(seedHex)
	defer C.free(unsafe.Pointer(cSeed))

	env := consume(C.ootle_build_stealth_unsigned_with_seed(C.uint8_t(networkByte), cIntent, cSeed))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_build_stealth_unsigned_with_seed returned ok without a handle"}
	}
	return &StealthHandle{ptr: (*C.OotleStealthPartialTransaction)(unsafe.Pointer(env.handle))}, env.dataJSON, nil
}

// ApplyFetchedSubstatesStealth wraps ootle_apply_fetched_substates_stealth (phase 2). It CONSUMES the
// input stealth handle (the C side takes it by value, even on error) and returns a new StealthHandle
// to thread forward plus the resolution data JSON ({"status":"resolved"} or
// {"status":"need_more","fetch_ids":[…]}). networkByte must be the transfer's network (the handle
// does not carry it). fetchedJSON is a JSON array of FetchedSubstate; spendSecretsJSON a JSON array of
// hex scalars (positional per intent.inputs). The input StealthHandle is invalidated regardless of
// outcome — callers must not reuse or free it.
func ApplyFetchedSubstatesStealth(h *StealthHandle, networkByte uint8, fetchedJSON, spendSecretsJSON string) (*StealthHandle, string, error) {
	if err := ensureABI(); err != nil {
		return nil, "", err
	}
	if h == nil || h.ptr == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ApplyFetchedSubstatesStealth called with a nil/consumed handle"}
	}
	cFetched := C.CString(fetchedJSON)
	defer C.free(unsafe.Pointer(cFetched))
	cSecrets := C.CString(spendSecretsJSON)
	defer C.free(unsafe.Pointer(cSecrets))

	ptr := h.ptr
	h.ptr = nil // the C call consumes it; invalidate our copy first to prevent double-free.
	env := consume(C.ootle_apply_fetched_substates_stealth(ptr, C.uint8_t(networkByte), cFetched, cSecrets))
	if !env.ok {
		return nil, "", env.asError()
	}
	if env.handle == nil {
		return nil, "", &Error{Code: "INTERNAL", Message: "ootle_apply_fetched_substates_stealth returned ok without a handle"}
	}
	return &StealthHandle{ptr: (*C.OotleStealthPartialTransaction)(unsafe.Pointer(env.handle))}, env.dataJSON, nil
}

// SealAndEncodeStealthTransfer wraps ootle_seal_and_encode_stealth (random-nonce default; the seal
// nonces are drawn from a fresh OS-RNG seed). It CONSUMES the stealth handle (the C side takes it by
// value, even on error) and returns the EncodedPublicTransfer data JSON. networkByte must be the
// transfer's network (the partial does not carry it). keysJSON is {account_secret}. The input
// StealthHandle is invalidated regardless of outcome — callers must not reuse or free it.
func SealAndEncodeStealthTransfer(h *StealthHandle, networkByte uint8, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncodeStealthTransfer called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	ptr := h.ptr
	h.ptr = nil // the C call consumes it; invalidate our copy first to prevent double-free.
	env := consume(C.ootle_seal_and_encode_stealth(ptr, C.uint8_t(networkByte), cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// SealAndEncodeStealthTransferWithSeed wraps the seed-reproducible counterpart. keysJSON is
// {account_secret, seed}; the seed pins the seal nonces. CONSUMES the handle.
func SealAndEncodeStealthTransferWithSeed(h *StealthHandle, networkByte uint8, keysJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncodeStealthTransferWithSeed called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))

	ptr := h.ptr
	h.ptr = nil
	env := consume(C.ootle_seal_and_encode_stealth_with_seed(ptr, C.uint8_t(networkByte), cKeys))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// --- stealth receive --------------------------------------------------------------------------
//
// The stealth receive (scan) path is stateless and handle-free: a single C call decrypts one
// inbound UTXO with the caller's view keys. Unlike the send flow there is no opaque handle and no
// driver loop — this is structurally identical to BuildAndEncodePublicTransfer (marshal two C
// strings → call → consume → check env.ok).

// ScanStealthOutput wraps ootle_scan_stealth_output. scanKeysJSON is
// {view_secret, account_secret?, skip_memo?}; outputJSON is an InboundStealthOutput. On success it
// returns the data JSON, which is ALWAYS a JSON object — the full DecryptedOutput ({"is_mine":true,
// ...}) when addressed to the scanner, or {"is_mine":false} when not (a SUCCESS envelope, never an
// error, and never null). Stateless — no handle is involved, so nothing to free beyond the result.
func ScanStealthOutput(networkByte uint8, scanKeysJSON, outputJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cScanKeys := C.CString(scanKeysJSON)
	defer C.free(unsafe.Pointer(cScanKeys))
	cOutput := C.CString(outputJSON)
	defer C.free(unsafe.Pointer(cOutput))

	env := consume(C.ootle_scan_stealth_output(C.uint8_t(networkByte), cScanKeys, cOutput))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// DecodeStealthUTXO wraps ootle_decode_stealth_utxo — the single shared UTXO decode in the core.
// substateIDJSON is the UTXO's canonical address string (utxo_<resource>_<commitment>) as a JSON
// string; substateValueJSON is the SubstateValue JSON the indexer returned, verbatim. On success it
// returns the decoded InboundStealthOutput JSON. The commitment + resource address come off the id;
// the crypto fields come off the value body — all decode work stays in the core (no Go crypto/CBOR).
// Stateless — no handle is involved.
func DecodeStealthUTXO(substateIDJSON, substateValueJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cID := C.CString(substateIDJSON)
	defer C.free(unsafe.Pointer(cID))
	cValue := C.CString(substateValueJSON)
	defer C.free(unsafe.Pointer(cValue))

	env := consume(C.ootle_decode_stealth_utxo(cID, cValue))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// ScanStealthSubstate wraps ootle_scan_stealth_substate — the fused decode → scan in the core.
// scanKeysJSON is {view_secret, account_secret?, skip_memo?}; substateIDJSON is the UTXO's canonical
// address string as a JSON string; substateValueJSON is the SubstateValue JSON the indexer returned,
// verbatim. On success it returns the data JSON, which is ALWAYS a JSON object — the full
// DecryptedOutput ({"is_mine":true,...}) when addressed to the scanner, or {"is_mine":false} when not
// (a SUCCESS envelope, never an error, and never null). Stateless — no handle is involved.
func ScanStealthSubstate(networkByte uint8, scanKeysJSON, substateIDJSON, substateValueJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cScanKeys := C.CString(scanKeysJSON)
	defer C.free(unsafe.Pointer(cScanKeys))
	cID := C.CString(substateIDJSON)
	defer C.free(unsafe.Pointer(cID))
	cValue := C.CString(substateValueJSON)
	defer C.free(unsafe.Pointer(cValue))

	env := consume(C.ootle_scan_stealth_substate(C.uint8_t(networkByte), cScanKeys, cID, cValue))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// ValidateStealthTransfer wraps ootle_validate_stealth_transfer — the shared sealed-transfer
// canonicalizer in the core. sealedHex is the lowercase-hex
// encoded_transaction (the TransactionEnvelope wire form) produced by a stealth seal op. The core
// BOR-decodes it, VERIFIES every signature, and returns the decoded transaction as canonical JSON
// with the byte-unstable set ["agg_range_proof","balance_proof","signature"] nulled (signer public
// keys survive). Decode + Schnorr verification stay in the core.
//
// A bad signature is an ERROR (typed *Error with code "VALIDATION"), never a falsy success;
// malformed/odd hex is "PARSE"; undecodable bytes "ENCODING"; a null arg / unknown network
// "INVALID". Stateless — no handle is involved.
func ValidateStealthTransfer(networkByte uint8, sealedHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cSealed := C.CString(sealedHex)
	defer C.free(unsafe.Pointer(cSealed))

	env := consume(C.ootle_validate_stealth_transfer(C.uint8_t(networkByte), cSealed))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// BuildStealthOutputsStatementWithSeed wraps ootle_build_stealth_outputs_statement_with_seed — the
// standalone seed-reproducible outputs-statement builder, in the core. intentJSON is a
// StealthTransferIntent carrying the outputs; seedHex is the lowercase-hex 32-byte build seed that
// pins every output's mask/nonces. On success it returns the data JSON, a JSON object
// {"outputs_statement": {...}, "aggregated_output_mask": "<64-hex>"}. The statement's byte-unstable
// agg_range_proof is nulled (semantic); the aggregated_output_mask is byte-stable.
//
// Malformed intent JSON / bad seed hex is "PARSE"; an invalid intent is "VALIDATION"; a null arg /
// unknown network is "INVALID". Stateless — no handle is involved.
func BuildStealthOutputsStatementWithSeed(networkByte uint8, intentJSON, seedHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cIntent := C.CString(intentJSON)
	defer C.free(unsafe.Pointer(cIntent))
	cSeed := C.CString(seedHex)
	defer C.free(unsafe.Pointer(cSeed))

	env := consume(C.ootle_build_stealth_outputs_statement_with_seed(C.uint8_t(networkByte), cIntent, cSeed))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// DecodeSubstate wraps ootle_decode_substate — the typed substate decode in the core.
// substateValueJSON is the indexer's SubstateValue JSON, passed through verbatim. On success it
// returns the kind-tagged DecodedSubstate JSON ({"kind":...,"value":{...}}). u64 balances stay native
// (no float). All decode work stays in the core (no Go CBOR). Stateless — no handle is involved.
// A malformed/unknown substate yields "PARSE"; a non-u64 balance "VALIDATION"; a null arg "INVALID".
func DecodeSubstate(substateValueJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cValue := C.CString(substateValueJSON)
	defer C.free(unsafe.Pointer(cValue))

	env := consume(C.ootle_decode_substate(cValue))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// AccountBalances wraps ootle_account_balances — the per-resource REVEALED balance sum in the core.
// accountSubstateJSON is the account Component substate JSON; vaultSubstatesJSON is a JSON array of
// FetchedSubstate records ({substate_id, version, substate_value}) — the vaults the host already
// fetched. On success it returns {"balances":[{"resource_address":...,"revealed_balance":<u64>}]};
// revealed balances stay native u64. The sum stays in the core (no Go arithmetic on values).
//
// A referenced vault not supplied is a "RESOLUTION" error, NEVER a silent zero; a non-component
// account / non-vault entry is "VALIDATION"; malformed JSON "PARSE"; a null arg "INVALID". Stateless.
func AccountBalances(accountSubstateJSON, vaultSubstatesJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cAccount := C.CString(accountSubstateJSON)
	defer C.free(unsafe.Pointer(cAccount))
	cVaults := C.CString(vaultSubstatesJSON)
	defer C.free(unsafe.Pointer(cVaults))

	env := consume(C.ootle_account_balances(cAccount, cVaults))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// AccountBalanceWants wraps ootle_account_balance_wants — names the vault substate ids a host should
// fetch to satisfy AccountBalances for an account. accountSubstateJSON is the account Component
// substate JSON. On success it returns {"fetch_ids":["vault_<hex>",...]} — the same component-vault
// discovery AccountBalances does, surfaced as opaque ids (the fetch_ids pattern). The discovery stays
// in the core. A non-component account is "VALIDATION"; malformed JSON "PARSE"; a null arg "INVALID".
func AccountBalanceWants(accountSubstateJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cAccount := C.CString(accountSubstateJSON)
	defer C.free(unsafe.Pointer(cAccount))

	env := consume(C.ootle_account_balance_wants(cAccount))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// --- co-signing (group C) ---------------------------------------------------------------------
//
// The co-sign hand-off is authorize → attach → seal across parties. Party A resolves a public handle,
// extracts the serializable UnsignedTransactionRecord to ship (UnsignedRecordForCosign — a BORROW, the
// handle is NOT consumed), party B signs it committing to A's seal pubkey (AddSignature), and A seals
// the resolved handle with B's authorizations attached (SealAndEncodeWithAuth[Production], which
// CONSUME the handle). All value-critical work (the signing message digest, the Schnorr signature, the
// is_seal_signer_authorized rule, BOR encode) stays in the core; the host only marshals JSON.

// UnsignedRecordForCosign wraps ootle_unsigned_record_for_cosign. It BORROWS the public handle (does
// NOT consume it) and returns the UnsignedTransactionRecord JSON ({"unsigned":...}) party A ships to B.
// The handle is left intact — consume it later via SealAndEncodeWithAuth* or free it via FreeHandle.
// An unresolved partial is a "RESOLUTION" error.
func UnsignedRecordForCosign(h *Handle) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "UnsignedRecordForCosign called with a nil/consumed handle"}
	}
	// Borrowing call — do NOT nil h.ptr (the handle survives for a later consume/free).
	env := consume(C.ootle_unsigned_record_for_cosign(h.ptr))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// AddSignature wraps ootle_add_signature (party B, production random nonce). unsignedJSON is the
// UnsignedTransactionRecord A shipped; sealPublicKeyHex is A's seal public key (lowercase hex);
// signerSecretHex is B's secret (lowercase hex). On success it returns the data JSON
// {"authorization":{"public_key":"<hex>","signature":"<hex>"}}. Bad key hex is "KEY"; bad seal-pk hex
// is "PARSE". Stateless — no handle is involved. signerSecretHex crosses as transient hex.
func AddSignature(networkByte uint8, unsignedJSON, sealPublicKeyHex, signerSecretHex string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	cUnsigned := C.CString(unsignedJSON)
	defer C.free(unsafe.Pointer(cUnsigned))
	cSealPK := C.CString(sealPublicKeyHex)
	defer C.free(unsafe.Pointer(cSealPK))
	cSecret := C.CString(signerSecretHex)
	defer C.free(unsafe.Pointer(cSecret))

	env := consume(C.ootle_add_signature(C.uint8_t(networkByte), cUnsigned, cSealPK, cSecret))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// SealAndEncodeWithAuth wraps ootle_seal_and_encode_with_auth (party A, random-nonce default). It
// CONSUMES the handle and returns the EncodedPublicTransfer data JSON. keysJSON is {account_secret};
// authorizationsJSON is a JSON array [{"public_key":"<hex>","signature":"<hex>"},...] (empty ⇒
// behaves like the plain single-key seal). The bytes/id are not reproducible. The input Handle is
// invalidated regardless of outcome.
func SealAndEncodeWithAuth(h *Handle, keysJSON, authorizationsJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncodeWithAuth called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))
	cAuths := C.CString(authorizationsJSON)
	defer C.free(unsafe.Pointer(cAuths))

	ptr := h.ptr
	h.ptr = nil // the C call consumes it; invalidate our copy first to prevent double-free.
	env := consume(C.ootle_seal_and_encode_with_auth(ptr, cKeys, cAuths))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}

// SealAndEncodeWithAuthWithSeed wraps the seed-reproducible counterpart. keysJSON is
// {account_secret, seed [, seal_secret]}. CONSUMES the handle. The bytes/id are reproducible
// byte-for-byte.
func SealAndEncodeWithAuthWithSeed(h *Handle, keysJSON, authorizationsJSON string) (string, error) {
	if err := ensureABI(); err != nil {
		return "", err
	}
	if h == nil || h.ptr == nil {
		return "", &Error{Code: "INTERNAL", Message: "SealAndEncodeWithAuthWithSeed called with a nil/consumed handle"}
	}
	cKeys := C.CString(keysJSON)
	defer C.free(unsafe.Pointer(cKeys))
	cAuths := C.CString(authorizationsJSON)
	defer C.free(unsafe.Pointer(cAuths))

	ptr := h.ptr
	h.ptr = nil
	env := consume(C.ootle_seal_and_encode_with_auth_with_seed(ptr, cKeys, cAuths))
	if !env.ok {
		return "", env.asError()
	}
	return env.dataJSON, nil
}
