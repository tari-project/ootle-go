// Package ootle is the idiomatic Go SDK over the Tari Ootle fat core. It mirrors the
// core's boundary records as Go structs (json-tagged), marshals them across the C ABI
// via internal/cffi, and returns typed results and errors. All value-critical logic
// (encoding, want-derivation, sealing, result typing) lives in the Rust core; this
// package is a thin host.
//
// # Layout
//
// The SDK lives in this package (./ootle); the import path is
// github.com/tari-project/ootle-go/ootle. Supporting code is split by responsibility:
//
//	internal/cffi   the ONLY import "C"/unsafe — the cgo wrapper over ootle_sdk.h
//	transport       the indexer-REST boundary (pluggable via the Transport interface)
//
// A typical caller connects to an indexer, builds an intent with a fluent builder, and drives
// the transfer. The builder leaves Inputs empty, so the driver takes the resolved (two-phase)
// path: derive the want list, fetch the substates, seal, submit, and wait.
//
//	client := ootle.Connect(transport.DefaultBaseURL, ootle.WithNetwork(ootle.NetworkLocalNet))
//
//	intent := ootle.NewTransfer(sender.Address).
//		ToPublicKey(recipientPublicKeyHex).
//		Resource(tariResource).
//		Amount(ootle.Tari(1)). // µTari (1 TARI = 1,000,000 µTari)
//		Fee(2000).
//		Intent()
//
//	res, err := client.SendPublicTransfer(ctx, intent, sender.TransferKeys())
//	if err == nil && res.IsCommit() {
//		bal, _ := client.AccountBalance(ctx, sender.Address, tariResource)
//		_ = bal
//	}
//
// An ootle.Account (ootle.NewAccount / ootle.AccountFromSeed) bundles the account keypair, the
// view keypair, and the derived address that the Send* and scan paths consume.
package ootle
