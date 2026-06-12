//   Copyright 2026 The Tari Project
//   SPDX-License-Identifier: BSD-3-Clause

package cffi

// #include "ootle_sdk.h"
import "C"

import "unsafe"

// RouteStealthHandleToPublicApply deliberately MISROUTES a stealth handle: it reinterprets the
// handle's pointer as the public opaque type and feeds it to the public-path
// ootle_apply_fetched_substates consumer — exactly the cross-type confusion the Go StealthHandle /
// Handle separation normally prevents at compile time, and that the C-side kind tag catches
// at runtime.
//
// It exists ONLY to let an in-module test prove the guard turns this misuse into a deterministic
// error rather than undefined behaviour. cgo cannot live in a _test.go file, so the unsafe cross-cast
// is confined to this regular source file. The handle is NOT consumed on the (expected) error path —
// the C guard rejects it before taking ownership — so `h` remains owned by the caller and must still
// be freed with FreeStealthHandle. Never call this outside a test.
func RouteStealthHandleToPublicApply(h *StealthHandle, fetchedJSON string) error {
	if err := ensureABI(); err != nil {
		return err
	}
	if h == nil || h.ptr == nil {
		return &Error{Code: "INVALID", Message: "nil stealth handle"}
	}
	cFetched := C.CString(fetchedJSON)
	defer C.free(unsafe.Pointer(cFetched))

	// The host mistake: reinterpret the stealth handle pointer as the public handle type.
	misrouted := (*C.OotlePartialTransaction)(unsafe.Pointer(h.ptr))
	env := consume(C.ootle_apply_fetched_substates(misrouted, cFetched))
	if !env.ok {
		return env.asError()
	}
	// The public consumer accepted (and consumed) the stealth handle — the guard FAILED. Returning nil
	// signals that to the test (which asserts a non-nil error). Nothing else to free: a consumed handle
	// is taken by value on the C side.
	return nil
}
