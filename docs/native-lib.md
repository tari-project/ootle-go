# Regenerating the native lib (`libootle_sdk_ffi_c.a`)

> **Maintainer doc.** Consumers of `ootle-go` never need any of this — `go get` + `go build`
> carries prebuilt libs.

`ootle-go` statically links a native lib built from the
[`tari-ootle`](https://github.com/tari-project/tari-ootle) monorepo's `ootle_sdk_ffi_c`
crate. We commit one stripped `.a` per platform plus the shared header:

```
internal/cffi/lib/
  ootle_sdk.h                       # shared ABI header (committed)
  PROVENANCE.md                     # generated summary of every vendored lib
  darwin_arm64/libootle_sdk_ffi_c.a # committed, ~32 MB
  darwin_arm64/provenance.json      # per-platform source of truth
  darwin_amd64/...
  linux_amd64/...
  linux_arm64/...
  windows_amd64/...
```

Libs are built **release + stripped** (debug is ~6× larger). Each platform's
`provenance.json` records exactly which monorepo commit it came from; `PROVENANCE.md` is a
generated rollup.

---

## TL;DR — "the monorepo changed, what do I do?"

```sh
# 1. Rebuild the HOST lib from your monorepo checkout (default ../tari-ootle):
OOTLE_MONOREPO=/path/to/tari-ootle make native

# 2. If the ABI changed, bump ExpectedABIVersion in internal/cffi/cffi.go (see below).

# 3. Verify locally:
make check
go test ./ootle/ -run TestGoldenVectors -v

# 4. Rebuild the OTHER platforms via CI (you can only natively build your host):
make native-all            # triggers the GitHub Actions matrix

# 5. Merge the CI PR (all platforms, one monorepo ref) + commit any ABI bump.
```

`make native` only rebuilds the lib for **your** OS/arch. Everything else comes from CI.

---

## Build profile & stripping

`scripts/build_native.sh` builds `release` by default and strips debug symbols in place,
keeping the global symbols cgo needs to link:

- macOS / BSD: `strip -S`
- Linux / Windows (GNU binutils): `strip --strip-debug`

Do **not** use `--strip-all` / `--strip-unneeded` on the archive — it can remove symbols the
linker still needs. Override the profile with `OOTLE_PROFILE=debug` only for local debugging.

---

## Link flags

The per-platform cgo `LDFLAGS` live in `internal/cffi/cffi.go`. The exact native libs a
platform needs are recorded in each `provenance.json` (`native_static_libs`, captured via
`cargo rustc -- --print native-static-libs`). If a platform fails to link in CI with
undefined symbols, add the missing `-l…` to that platform's `#cgo` line.

---

## ABI bumps (do not skip)

The header `ootle_sdk.h` and `ExpectedABIVersion` in `internal/cffi/cffi.go` (currently
`ootle-sdk-ffi-c/12`) are a frozen contract. If the monorepo changed the C ABI:

1. `make native` re-vendors the header automatically, so it can never drift from its lib.
2. The new lib reports a new `ootle_abi_version()`. The wrapper asserts it matches
   `ExpectedABIVersion` on first use and **fails loudly** otherwise — never a silent
   mis-marshal.
3. Bump `ExpectedABIVersion` to the new tag and reconcile any Go-side struct/JSON changes.

If regenerated tests suddenly fail with an ABI mismatch, this is the cause.

---

## All platforms (CI)

You can only natively build your host locally. The full set is produced by the
`native-libs.yml` GitHub Actions matrix on native runners:

| Target | Runner |
|---|---|
| darwin/arm64 | macos-14 |
| darwin/amd64 | macos-13 |
| linux/amd64 | ubuntu-latest |
| linux/arm64 | ubuntu-24.04-arm |
| windows/amd64 | windows-latest (GNU toolchain) |

Trigger it against a specific monorepo ref; each job builds → strips → verifies
(`go build` + golden vectors) → uploads its lib, and an aggregation job commits all
platforms in one PR and regenerates `PROVENANCE.md`. A private monorepo needs a
`MONOREPO_TOKEN` repo secret with read access.

`make native-all` is the entry point.

---

## Checklist before merging regenerated libs

- [ ] `make check` passes (build + vet + test + gofmt).
- [ ] `TestGoldenVectors`, `TestFixtureDrift`, `TestGoldenVectors_CoverageParity` pass.
- [ ] If the ABI changed: `ExpectedABIVersion` bumped, header re-vendored, Go types reconciled.
- [ ] `PROVENANCE.md` + every `provenance.json` updated.
- [ ] All platforms were built from the **same** monorepo ref.
