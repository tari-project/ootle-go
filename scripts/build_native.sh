#!/usr/bin/env bash
#
# build_native.sh — build + vendor the native lib for the HOST platform.
#
# Builds the monorepo's `ootle_sdk_ffi_c` crate and vendors:
#   - the shared C header  -> internal/cffi/lib/ootle_sdk.h
#   - the static library   -> internal/cffi/lib/<goos>_<goarch>/libootle_sdk_ffi_c.a
# then strips it and records provenance. Other platforms are produced by the CI matrix
# (see docs/native-lib.md). The libs are committed; consumers need only `go get`.
#
# Configuration (env):
#   OOTLE_MONOREPO   path to the tari-ootle monorepo (default: ../tari-ootle)
#   OOTLE_PROFILE    cargo build profile: "release" (default) or "debug"
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

OOTLE_MONOREPO="${OOTLE_MONOREPO:-${REPO_DIR}/../tari-ootle}"
OOTLE_PROFILE="${OOTLE_PROFILE:-release}"

if [[ ! -d "${OOTLE_MONOREPO}" ]]; then
  echo "error: OOTLE_MONOREPO does not exist: ${OOTLE_MONOREPO}" >&2
  echo "       set OOTLE_MONOREPO to the path of the tari-ootle monorepo." >&2
  exit 1
fi
OOTLE_MONOREPO="$(cd "${OOTLE_MONOREPO}" && pwd)"

CARGO_FLAGS=()
TARGET_SUBDIR="debug"
if [[ "${OOTLE_PROFILE}" == "release" ]]; then
  CARGO_FLAGS+=("--release")
  TARGET_SUBDIR="release"
elif [[ "${OOTLE_PROFILE}" != "debug" ]]; then
  echo "error: OOTLE_PROFILE must be 'release' or 'debug' (got '${OOTLE_PROFILE}')" >&2
  exit 1
fi

PLAT="$(go env GOOS)_$(go env GOARCH)"
VENDOR_DIR="${REPO_DIR}/internal/cffi/lib"
PLAT_DIR="${VENDOR_DIR}/${PLAT}"
HEADER_SRC="${OOTLE_MONOREPO}/crates/ootle_sdk_ffi_c/include/ootle_sdk.h"
LIB_SRC="${OOTLE_MONOREPO}/target/${TARGET_SUBDIR}/libootle_sdk_ffi_c.a"
LIB_DEST="${PLAT_DIR}/libootle_sdk_ffi_c.a"

echo "==> Building ootle_sdk_ffi_c (${OOTLE_PROFILE}) for ${PLAT} in ${OOTLE_MONOREPO}"
( cd "${OOTLE_MONOREPO}" && cargo build -p ootle_sdk_ffi_c "${CARGO_FLAGS[@]}" )

[[ -f "${HEADER_SRC}" ]] || { echo "error: header not found: ${HEADER_SRC}" >&2; exit 1; }
[[ -f "${LIB_SRC}" ]]    || { echo "error: static lib not found: ${LIB_SRC}" >&2; exit 1; }

mkdir -p "${PLAT_DIR}"
echo "==> Vendoring header    -> ${VENDOR_DIR}/ootle_sdk.h"
cp "${HEADER_SRC}" "${VENDOR_DIR}/ootle_sdk.h"
echo "==> Vendoring staticlib -> ${LIB_DEST}"
cp "${LIB_SRC}" "${LIB_DEST}"

# Strip debug symbols in place, keeping the global symbols cgo needs to link.
case "$(uname -s)" in
  Darwin)               STRIP_CMD="strip -S"; strip -S "${LIB_DEST}" ;;
  MINGW*|MSYS*|CYGWIN*) STRIP_CMD="(skipped on Windows)" ;;
  *)                    STRIP_CMD="strip --strip-debug"; strip --strip-debug "${LIB_DEST}" ;;
esac
echo "==> Stripped (${STRIP_CMD})"

# Linker's real native-lib needs — recorded as a hint for the cgo LDFLAGS in cffi.go.
# Non-fatal: the vendored artifact is the one from `cargo build` above.
NATIVE_LIBS="$(
  cd "${OOTLE_MONOREPO}" &&
  cargo rustc -p ootle_sdk_ffi_c "${CARGO_FLAGS[@]}" -- --print native-static-libs 2>&1 |
  sed -n 's/.*native-static-libs: //p'
)" || true

COMMIT="$(git -C "${OOTLE_MONOREPO}" rev-parse HEAD)"
REMOTE="$(git -C "${OOTLE_MONOREPO}" remote get-url origin 2>/dev/null || echo unknown)"
CRATE_VERSION="$(
  cd "${OOTLE_MONOREPO}" &&
  cargo metadata --no-deps --format-version 1 |
  jq -r '.packages[] | select(.name=="ootle_sdk_ffi_c") | .version'
)"
ABI="$(sed -nE 's/.*ExpectedABIVersion[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "${REPO_DIR}/internal/cffi/cffi.go")"
[[ -n "${ABI}" ]] || { echo "error: could not extract ExpectedABIVersion from cffi.go" >&2; exit 1; }
SIZE="$(wc -c < "${LIB_DEST}" | tr -d ' ')"
if command -v sha256sum >/dev/null 2>&1; then
  SHA="$(sha256sum "${LIB_DEST}" | awk '{print $1}')"
else
  SHA="$(shasum -a 256 "${LIB_DEST}" | awk '{print $1}')"
fi
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

cat > "${PLAT_DIR}/provenance.json" <<EOF
{
  "platform": "${PLAT}",
  "monorepo_remote": "${REMOTE}",
  "monorepo_commit": "${COMMIT}",
  "crate_version": "${CRATE_VERSION}",
  "abi": "${ABI}",
  "profile": "${OOTLE_PROFILE}",
  "strip": "${STRIP_CMD}",
  "native_static_libs": "${NATIVE_LIBS}",
  "size_bytes": ${SIZE},
  "sha256": "${SHA}",
  "built_at": "${BUILT_AT}"
}
EOF
echo "==> Wrote ${PLAT_DIR}/provenance.json"

"${SCRIPT_DIR}/gen_provenance.sh"

echo "==> Done. Vendored ${PLAT} from ${OOTLE_MONOREPO}@${COMMIT:0:12} (profile=${OOTLE_PROFILE})."
