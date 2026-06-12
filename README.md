# ootle-go

Idiomatic Go SDK over the **Tari Ootle core** (`ootle-sdk-core`), consumed through the
flat C ABI (`ootle_sdk_ffi_c`). All value-critical logic — transaction encoding,
input-resolution want derivation, deterministic sealing, result typing — lives in the
Rust core; this module is a thin host: cgo marshalling, an HTTP transport, a two-phase
driver loop, and ergonomic Go types.

> **Module path** `github.com/tari-project/ootle-go`. To rename, update `module` in
> `go.mod` and the matching import paths.

## Features

- `Client.SendPublicTransfer` / `SendPublicTransferDeterministic` — the full two-phase
  flow (build → resolve inputs → seal+encode → submit → wait → parse a typed
  `FinalizedResult`).
- `BuildAndEncodePublicTransfer` — one-call build + seed-reproducible seal +
  BOR-encode, reproducible byte-for-byte vs the Rust core.
- **Confidential (stealth) transfers:** `Client.SendStealthTransfer[Deterministic]` +
  the one-shot `BuildAndEncodeStealthTransfer` (send), and the stateless `ScanStealthOutput`
  (receive — decrypt an inbound UTXO to recover value/mask/memo). All stealth crypto — input
  decryption, seeded proofs, signing/sealing, ownership/tag checks — stays in the core.
- A thin indexer-REST `transport` (`FetchSubstates` / `Submit` / `GetResult`).
- **Event streaming (SSE):** `Client.WatchEvents` streams typed template events from the
  indexer (`GET /transactions/events/stream`), with automatic reconnect + `Last-Event-ID`
  resume. Pure host JSON — no FFI/ABI change.
- **Multi-round resolution across the C ABI:** `apply_fetched_substates`'s `NeedMore`
  exposes the *concrete* next-fetch ids (`fetch_ids`) — including the vault id the core
  discovers inside a fetched component — and the driver fetches exactly those, so a thin
  host converges in the realistic 1–2 rounds without deriving anything itself.
- 14 vendored golden vectors run across the C ABI — byte-for-byte where the bytes are
  reproducible, semantically where the stealth proofs/signatures are not — plus a drift
  check against the monorepo source of truth.

## Install

```sh
go get github.com/tari-project/ootle-go/ootle
```

That's it. Prebuilt, stripped native libs are committed per platform under
`internal/cffi/lib/`, so `go build` / `go test` work out of the box — **no Rust, no
monorepo, no extra steps.** Supported targets: `darwin/arm64`, `darwin/amd64`,
`linux/amd64`, `linux/arm64`, `windows/amd64`.

The lib is **statically linked**, so no runtime `LD_LIBRARY_PATH`/`DYLD_*` is required.
The cgo directives live in `internal/cffi/cffi.go`; `import "C"` and `unsafe` are confined
to that one file. On first use the wrapper asserts `ootle_abi_version()` equals the
expected ABI tag, so a stale lib fails loudly instead of mis-marshalling.

> **Maintainers:** the native libs are regenerated from the `tari-ootle` monorepo. See
> [`docs/native-lib.md`](docs/native-lib.md) for the one-command host rebuild
> (`make native`) and the all-platform CI flow.

## Quickstart

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/tari-project/ootle-go/ootle"
	"github.com/tari-project/ootle-go/transport"
)

func main() {
	// Connect builds the indexer transport + a network-bound client in one call.
	client := ootle.Connect(transport.DefaultBaseURL, // indexer REST, 127.0.0.1:18300
		ootle.WithNetwork(ootle.NetworkLocalNet),
		ootle.WithPollInterval(2*time.Second),
	)

	// An Account bundles the account + view keypairs and the derived address.
	sender, err := ootle.AccountFromSeed([32]byte{ /* your 32-byte seed */ })
	if err != nil {
		panic(err)
	}

	// The builder leaves Inputs empty ⇒ the resolved (two-phase) path: the driver derives
	// the want list, fetches the substates, applies them, then seals + submits + waits.
	intent := ootle.NewTransfer(sender.Address).
		ToPublicKey("<recipient-pubkey-hex>").
		Resource("resource_<hex>").
		Amount(ootle.Tari(1)). // µTari (1 TARI = 1,000,000 µTari)
		Fee(2_000).            // µTari
		Intent()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res, err := client.SendPublicTransfer(ctx, intent, sender.TransferKeys())
	if err != nil {
		panic(err)
	}
	if res.IsCommit() {
		fmt.Printf("committed %s, fees paid %d µTari\n",
			res.Submit.TransactionID, res.FeeReceipt.TotalFeesPaid)
		bal, _ := client.AccountBalance(ctx, sender.Address, "resource_<hex>")
		fmt.Printf("sender balance now %d µTari\n", bal)
	}
}
```

To estimate the fee first, build the same intent with `.DryRun()` (or call
`intent.AsDryRun()`) and read `res.EstimatedFeeOr(0)` — a dry-run executes fully but never
commits. For reproducible / golden-vector parity use `SendPublicTransferDeterministic` with a
`DeterministicTransferKeys` bundle carrying a pinned 32-byte seed; the sealed bytes/id are then
byte-for-byte reproducible. Amounts are **µTari** (`ootle.Tari(n)` converts whole TARI;
1 TARI = 1,000,000 µTari).

## Stealth (confidential) transfers

The confidential surface mirrors the public one. **Send** drives fetch → build → resolve →
seal → submit → wait → parse — the same multi-round NeedMore convergence loop the public
path uses; the host fetches each stealth-input UTXO the core asks for and the core resolves
+ decrypts them internally. **Receive** is a stateless, RNG-free decrypt — `ScanStealthOutput`
reveals whether an inbound UTXO is yours and, if so, its value, mask, and memo. No crypto runs
in Go.

```go
// Send: one confidential output funded by the sender's revealed bucket. The builder tracks
// the revealed input/output sums and validates them before the core call.
intent, err := ootle.NewStealthTransfer("component_<sender-hex>", "resource_<hex>").
	SpendRevealedInput(1_000). // µTari drawn from the revealed vault
	ToStealthOutput("<recipient-account-pubkey-hex>", "<recipient-view-pubkey-hex>", 1_000).
	Intent(2_000) // fee in µTari
if err != nil {
	panic(err)
}
res, err := client.SendStealthTransfer(ctx, intent,
	ootle.StealthProductionKeys{AccountSecret: "<account-secret-hex>"})

// Receive: decrypt an inbound UTXO you fetched from the indexer.
out, err := ootle.ScanStealthOutput(ootle.NetworkLocalNet,
	ootle.StealthScanKeys{ViewSecret: "<view-secret-hex>"}, inbound)
if out.IsMine {
	fmt.Printf("received %d µTari (mask %s)\n", out.Value, out.Mask)
}
```

For reproducible parity use `SendStealthTransferDeterministic` /
`BuildAndEncodeStealthTransfer` with a `StealthTransferKeys` bundle carrying a pinned 32-byte
seed. With a pinned seed the *signatures* are reproducible but the embedded bulletproof +
viewable-balance proof are **not** byte-stable — so the stealth send golden vectors compare
**semantically**, not byte-for-byte (see [`docs/vectors.md`](./docs/vectors.md)).

## Supported operations

| Go entry point | What it does |
|---|---|
| `Client.SendPublicTransfer` | Two-phase public transfer with a production (random-nonce) seal. |
| `Client.SendPublicTransferDeterministic` | Same flow with a pinned seed — reproducible bytes/id. |
| `BuildAndEncodePublicTransfer` | One-call build + seed-reproducible seal + BOR-encode (no networking). |
| `Client.SendStealthTransfer` | Confidential transfer, production (random-seed) seal: fetch → build → seal → submit → wait. |
| `Client.SendStealthTransferDeterministic` | Same confidential flow with a pinned seed (signatures reproducible; proofs semantic). |
| `BuildAndEncodeStealthTransfer` | One-call seed-reproducible confidential build + seal + BOR-encode (no networking). |
| `ScanStealthOutput` / `ScanStealthOutputs` | Stateless receive: decrypt an inbound UTXO → `DecryptedOutput` (`IsMine` / value / mask / memo). |
| `Client.SubmitSealed` | Submit an out-of-band sealed transaction (co-signing / offline) and wait for the typed result. |
| `Client.WatchEvents` | Stream typed template events (SSE) with reconnect/resume. |
| `transport.Client` | Indexer REST: `FetchSubstates` / `Submit` / `GetResult` / `WaitResult`. |
| `ABIVersion` | The native lib's ABI tag (sanity / drift check). |

## Examples

A runnable [`examples/`](./examples) suite demonstrates each entry point against a live
indexer: balance reads, fungible + confidential (stealth) transfers, dry-run fee estimation,
workspace chaining, manual co-signing, template publish/deploy/invoke, and SSE event
streaming. Most self-fund a fresh throwaway identity from the faucet, so they need only an
indexer URL. See [`examples/README.md`](./examples/README.md) for the full table, env vars,
and bootstrap order.

## Error codes

Failures surface as `*ootle.Error` with a **stable** `Code` (the core's
`OotleSdkError::code()`) plus a human-readable `Message`; `errors.As(err, &oe)` recovers
the typed error. The codes are part of the public contract — they are never renamed.

| `Error.Code` | Meaning |
|---|---|
| `ENCODING` | BOR/CBOR or byte-level encode/decode failed (incl. Go-side marshalling of the boundary JSON). |
| `KEY` | A key, signature, or nonce secret was malformed or underivable. |
| `PARSE` | A string/structured value failed to parse into an internal type (address, substate id, fetched substate value, …). |
| `VALIDATION` | A semantically-valid-looking value failed a domain rule (e.g. an out-of-range amount, an empty input set, an unknown network). |
| `INVALID` | A value is structurally invalid for the requested operation. |
| `RESOLUTION` | Input resolution did not converge (also `ootle.ErrResolutionDidNotConverge`, recoverable via `errors.Is`). |
| `STEALTH` | A cryptographic or protocol-level confidential-transfer failure (e.g. invalid/short stealth entropy, an input-mask decryption failure, a balance-proof or range-proof failure). |
| `INTERNAL` | A panic was caught at the C boundary (`catch_unwind`), or a non-typed host error. |

A finalized **reject** is not an `Error` — it is a populated `FinalizedResult` whose
`Submit.Outcome` is `Reject`/`OnlyFeeCommit` carrying a `RejectReason{Code, AbortCode,
Message}`. Top-level reject codes:

| `RejectReason.Code` | |
|---|---|
| `EXECUTION_FAILURE` · `SUBSTATE_NOT_FOUND` · `FAILED_TO_LOCK_INPUTS` · `FAILED_TO_LOCK_OUTPUTS` · `FOREIGN_PLEDGE_INPUT_CONFLICT` · `FOREIGN_SHARD_GROUP_DECIDED_TO_ABORT` · `INSUFFICIENT_FEES_PAID` · `FEE_PAYMENT_IN_MAIN_INTENT` · `ABORT` | sequenced-then-aborted / failed |
| `MEMPOOL_REJECTED` | rejected before sequencing (failed validation, never executed) |

When `Code == "ABORT"` (or a foreign-shard abort), `AbortCode` carries the canonical
abort sub-code — one of: `FOREIGN_PLEDGE_INPUT_CONFLICT`, `LOCK_INPUTS_FAILED`,
`LOCK_OUTPUTS_FAILED`, `LOCK_INPUTS_OUTPUTS_FAILED`, `EXECUTION_FAILURE`,
`ONE_OR_MORE_INPUTS_NOT_FOUND`, `INSUFFICIENT_FEES_PAID`, `FEE_PAYMENT_IN_MAIN_INTENT`,
`EPOCH_EXPIRED`. Branch on `AbortCode` instead of parsing `Message`.

## Golden vectors & drift

`testdata/fixtures/` holds a **checked-in copy** of the monorepo fixtures
(`crates/ootle_sdk_core/fixtures/`), the single source of truth (lowercase hex, no `0x`):
14 vectors — 3 `public_transfer`, 2 `resolve_public_transfer`, 3 `parse_finalized_result`,
2 `stealth_outputs_statement`, 2 `stealth_transfer` (send), 2 `stealth_scan` (receive).

- `TestGoldenVectors` runs them through the C ABI under each fixture's **per-fixture
  comparison mode** (`"compare": "bytes" | "semantic"`): byte-for-byte on the reproducible
  encode/scan vectors, canonicalized structure on parse vectors, and a **semantic** compare
  on the stealth send vectors (whose proofs/signatures are not byte-stable). See
  [`docs/vectors.md`](./docs/vectors.md) for what each mode asserts and which group uses which.
- `TestGoldenVectors_CoverageParity` asserts every operation in the tree has a runner arm,
  and `TestGoldenVectors_UnknownOperationFails` keeps the unknown-op guard honest.
- `TestFixtureDrift` asserts the vendored copy is byte-identical to the monorepo source.
- `make sync-fixtures` re-vendors from the monorepo (`OOTLE_MONOREPO`, default
  `../tari-ootle`).

## Live end-to-end test

Two live e2e tests exercise the public (`SendPublicTransfer`) and confidential
(`SendStealthTransfer` → `ScanStealthOutput` round-trip) flows against a real indexer. They
are compiled **only** under the `e2e` build tag and additionally **skip** unless
`OOTLE_E2E=1` and the params are supplied, so the default `go test ./...` (and CI without a
node) never runs them:

```sh
go test ./...                                    # e2e is NOT built — always green
go test -tags e2e -run TestE2EPublicTransfer -v  # public; skips without OOTLE_E2E=1 + params
go test -tags e2e -run TestStealthRoundTrip -v   # confidential send→receive round-trip
```

Stand up a local node with the monorepo's **`tari_swarm_daemon`** (a full localnet:
Minotari base node + wallet, an Ootle validator node, an Ootle wallet, and an Indexer) —
see the monorepo top-level README. Point the test at the indexer's REST URL
(`OOTLE_E2E_INDEXER_URL`); the env vars are documented in
[`e2e_test.go`](./ootle/e2e_test.go) (public) and [`stealth_e2e_test.go`](./ootle/stealth_e2e_test.go)
(confidential round-trip). The transport targets the **indexer**, not the wallet daemon's
JSON-RPC (whose server-side `detect_inputs` would bypass the core's two-phase resolution).

## Multi-round input resolution

The public transfer's want set discovers a *vault* substate id only after the from-account
**component** is fetched and parsed. The C ABI handles this without any host-side
want-derivation: `ootle_apply_fetched_substates`'s **NeedMore** response carries the
core's **concrete next-fetch ids**:

```json
{ "status": "need_more", "want_list": [ … ], "fetch_ids": [ "vault_…" ] }
```

`fetch_ids` is the authoritative set — it includes the vault id the core discovered inside
the fetched component, which the `want_list` seeds alone could never name. The Go driver
fetches exactly `fetch_ids` next (falling back to deriving from `want_list` only against an
older core that predates the field), so a thin host converges in the realistic 1–2 rounds.

This keeps the host thin: it never parses a component or derives a vault id — that stays in
the core behind `apply_fetched_substates`. Convergence is proven offline by the multi-round
tests in [`driver_test.go`](./ootle/driver_test.go) (which serve substates strictly by requested
id) and the C-ABI integration test, with no live node required.

## Layout

```
ootle/                       # public `ootle` package (import .../ootle-go/ootle)
  doc.go                     #   package overview + layout
  ootle.go                   #   idiomatic types + build/encode entry points
  driver.go                  #   Client + two-phase SendPublicTransfer driver loop
  stealth.go                 #   confidential send (SendStealthTransfer*) + receive (ScanStealthOutput)
  result.go                  #   typed FinalizedResult / RejectReason / fee / diff / event types
  e2e_test.go                #   live public e2e (build tag `e2e` + OOTLE_E2E gate; skips by default)
  stealth_e2e_test.go        #   live confidential send→receive round-trip (same `e2e` gate)
  testdata/fixtures/         #   vendored golden vectors (single source of truth: monorepo)
transport/                   # thin indexer-REST transport (no domain logic, no cgo)
internal/cffi/               # the ONLY `import "C"`/`unsafe`; cgo wrapper over ootle_sdk.h
  lib/ootle_sdk.h            #   vendored C header (committed)
  lib/libootle_sdk_ffi_c.a   #   vendored static lib (git-ignored; regenerated)
docs/vectors.md              # golden-vector comparison-mode (bytes vs semantic) reference
scripts/build_native.sh      # builds the monorepo FFI crate + vendors header & lib
scripts/sync_fixtures.sh     # re-vendors the golden vectors from the monorepo
Makefile                     # build / native / test / sync-fixtures / check
```

## License

BSD-3-Clause. See [LICENSE](./LICENSE).
