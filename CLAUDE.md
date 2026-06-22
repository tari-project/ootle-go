# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`ootle-go` is an idiomatic Go SDK over the **Tari Ootle core** (`ootle-sdk-core`),
consumed through a flat C ABI (`ootle_sdk_ffi_c`). The guiding principle is **thin host,
fat core**: all value-critical logic (transaction encoding, input-resolution want
derivation, deterministic sealing, result typing, all stealth crypto) lives in the Rust
core. This module only does cgo marshalling, HTTP transport, the two-phase driver loop,
and ergonomic Go types. When tempted to add domain logic here, the answer is almost
always to push it into the core instead.

## The native lib is committed — `go build` works out of the box

The Go build statically links a prebuilt, stripped `libootle_sdk_ffi_c.a` committed per
platform under `internal/cffi/lib/<goos>_<goarch>/`. **Consumers and contributors need no
Rust and no monorepo** — `go build ./...` / `go test ./...` / `go vet ./...` /
`gofmt -l .` work directly. To run one test:

```sh
go test ./ootle/ -run TestGoldenVectors -v
make check           # build + vet + test + gofmt-check  (the full local gate)
```

### Maintainer: regenerating the native lib
Only needed when the core/ABI changes. `make native` rebuilds the **host** lib from the
monorepo; CI (`make native-all`) rebuilds all platforms. Full procedure — including ABI
bumps — is in `docs/native-lib.md`.

```sh
OOTLE_MONOREPO=/path/to/tari-ootle make native   # host lib (release+strip), default ../tari-ootle
make sync-fixtures                                # re-vendor golden vectors from the monorepo
```

Each lib's origin (monorepo commit, crate version, ABI tag, sha256) is recorded in
`internal/cffi/lib/<plat>/provenance.json` and rolled up into `PROVENANCE.md`.

### ABI version pinning
`internal/cffi/cffi.go` holds `ExpectedABIVersion` (e.g. `"ootle-sdk-ffi-c/12"`). On
first use the wrapper asserts `ootle_abi_version()` matches and fails loudly otherwise.
**When the core ABI changes, bump this constant** and re-vendor — a stale lib mismatch is
a hard error, not a silent mis-marshal.

## Architecture

The boundary records of the Rust core are mirrored as json-tagged Go structs, marshalled
across the C ABI as JSON, and returned as typed results/errors. Three layers:

- **`internal/cffi/`** — the **only** place `import "C"` and `unsafe` are allowed. cgo
  wrapper over the vendored `lib/ootle_sdk.h`. Owns all C memory discipline (CString /
  free, `OotleResult` freeing, opaque `Handle` lifecycle — consuming ops like apply/seal
  take the handle by value so it must not be freed afterward). No other package may
  `import "C"`.
- **`transport/`** — thin indexer-REST boundary (`FetchSubstates` / `Submit` /
  `GetResult` / `WaitResult`), plus SSE streaming (`sse.go`). No domain logic, no cgo.
  Pluggable via the `Transport` interface. Targets the **indexer** REST API, *not* the
  wallet daemon JSON-RPC (whose server-side `detect_inputs` would bypass the core's
  two-phase resolution).
- **`ootle/`** — the public package (import path `github.com/tari-project/ootle-go/ootle`).
  `Client` over a transport; idiomatic types and entry points.

### Two-phase driver loop (`ootle/driver.go`)
The headline flow is `Client.SendPublicTransfer`: build → resolve inputs → seal+encode →
submit → wait → parse a typed `FinalizedResult`. Input resolution is **multi-round across
the C ABI**: `apply_fetched_substates` returns a `NeedMore` carrying concrete `fetch_ids`
(including a vault id the core discovers inside a fetched component). The driver fetches
exactly those ids — it never parses a component or derives a vault id itself. Converges in
1–2 rounds; proven offline by `driver_test.go` (no live node needed).

### Key source files in `ootle/`
- `driver.go` — `Client` + the two-phase public-transfer driver loop.
- `stealth.go` — confidential send (`SendStealthTransfer*`) + stateless receive
  (`ScanStealthOutput`). All stealth crypto stays in the core.
- `ootle.go` — idiomatic types + build/encode entry points (`BuildAndEncode*`).
- `result.go` — typed `FinalizedResult` / `RejectReason` / fee / diff / event types.
- `events.go` — `Client.WatchEvents` (typed template events over SSE, reconnect + resume).

### Deterministic vs production paths
Each transfer has a production variant (random: a fresh OS-RNG build seed) and a `*Deterministic`
variant (a pinned 32-byte build seed the core expands into every nonce/mask) used for
golden-vector parity. With a pinned seed: public sealed bytes/id are **byte-for-byte**
reproducible; stealth **signatures** are reproducible but the bulletproof + viewable-balance
proof are **not** byte-stable — so stealth send vectors compare **semantically**.

## Errors vs rejects (important distinction)

- An **`*ootle.Error`** carries a **stable** `Code` (`ENCODING`, `KEY`, `PARSE`,
  `VALIDATION`, `INVALID`, `RESOLUTION`, `STEALTH`, `INTERNAL`) — part of the public
  contract, never renamed. Recover via `errors.As`. `RESOLUTION` is also matchable as
  `ootle.ErrResolutionDidNotConverge` via `errors.Is`.
- A finalized **reject is not an error** — it's a populated `FinalizedResult` whose
  `Submit.Outcome` is `Reject`/`OnlyFeeCommit` with a `RejectReason{Code, AbortCode,
  Message}`. Branch on `AbortCode`, never parse `Message`.

## Golden vectors & drift

`ootle/testdata/fixtures/` is a **checked-in copy** of the monorepo fixtures
(`crates/ootle_sdk_core/fixtures/`, the single source of truth). Each fixture declares a
per-fixture `"compare": "bytes" | "semantic"` mode (see `docs/vectors.md`).
`TestGoldenVectors` runs them through the C ABI; `TestFixtureDrift` fails if the vendored
copy diverges from the monorepo; `TestGoldenVectors_CoverageParity` asserts every
operation has a runner arm. Re-vendor with `make sync-fixtures`.

## Live e2e tests

`e2e_test.go` / `stealth_e2e_test.go` / `watch_events_e2e_test.go` are gated behind the
`e2e` build tag **and** skip unless `OOTLE_E2E=1` plus params are set, so default
`go test ./...` never builds or runs them. Running them needs a local node
(monorepo's `tari_swarm_daemon`); env vars are documented at the top of each test file.

```sh
go test -tags e2e -run TestE2EPublicTransfer -v
go test -tags e2e -run TestStealthRoundTrip -v
```

## Conventions

- Keep `import "C"`/`unsafe` confined to `internal/cffi`.
- `gofmt -l .` must print nothing; `make check` is the gate to run before considering work done.
