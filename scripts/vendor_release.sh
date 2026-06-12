#!/usr/bin/env bash
#
# vendor_release.sh — vendor ONE platform's native lib from a tari-ootle release asset.
#
# Unlike build_native.sh (which compiles the crate from a monorepo checkout with Rust),
# this downloads the prebuilt zip that tari-ootle's `ffi_libs.yml` attaches to a release,
# verifies its checksum, and vendors:
#   - the shared C header  -> internal/cffi/lib/ootle_sdk.h
#   - the static library   -> internal/cffi/lib/<goos>_<goarch>/libootle_sdk_ffi_c.a
# then records provenance. The release libs are already release-built + stripped, so no
# Rust toolchain and no monorepo checkout are needed. See docs/native-lib.md.
#
# Requires: gh (authenticated), jq, unzip. Run from anywhere.
#
# Usage:
#   vendor_release.sh --tari-platform macos-arm64 --goos darwin --goarch arm64 \
#                     --tag v0.1.0 --repo tari-project/tari-ootle
#
set -euo pipefail

CRATE="ootle_sdk_ffi_c"
TARI_PLATFORM="" GOOS="" GOARCH="" TAG="" REPO="tari-project/tari-ootle"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tari-platform) TARI_PLATFORM="$2"; shift 2 ;;
    --goos)          GOOS="$2"; shift 2 ;;
    --goarch)        GOARCH="$2"; shift 2 ;;
    --tag)           TAG="$2"; shift 2 ;;
    --repo)          REPO="$2"; shift 2 ;;
    *) echo "error: unknown arg: $1" >&2; exit 1 ;;
  esac
done
for v in TARI_PLATFORM GOOS GOARCH TAG; do
  [[ -n "${!v}" ]] || { echo "error: --${v,,} is required" >&2; exit 1; }
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VENDOR_DIR="${REPO_DIR}/internal/cffi/lib"
PLAT="${GOOS}_${GOARCH}"
PLAT_DIR="${VENDOR_DIR}/${PLAT}"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}';
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
check_sha() {  # check_sha <sha256-file> (run in the dir holding both files)
  if command -v sha256sum >/dev/null 2>&1; then sha256sum --check "$1";
  else shasum -a 256 --check "$1"; fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "==> Downloading ${CRATE} ${TARI_PLATFORM} from ${REPO}@${TAG}"
# The non-musl linux-x86_64 pattern intentionally excludes the *-linux-x86_64-musl.zip asset
# (different suffix). Includes prereleases/drafts via --pattern on the resolved tag.
gh release download "${TAG}" --repo "${REPO}" --dir "${WORK}" \
  --pattern "${CRATE}-*-${TARI_PLATFORM}.zip" \
  --pattern "${CRATE}-*-${TARI_PLATFORM}.zip.sha256"

ZIP="$(ls "${WORK}"/${CRATE}-*-${TARI_PLATFORM}.zip)"
[[ -f "${ZIP}" ]] || { echo "error: asset not found for ${TARI_PLATFORM}" >&2; exit 1; }
BASE="$(basename "${ZIP}")"

echo "==> Verifying checksum"
( cd "${WORK}" && check_sha "${BASE}.sha256" )

echo "==> Extracting ${BASE}"
EXTRACT="${WORK}/extract"
mkdir -p "${EXTRACT}"
unzip -q "${ZIP}" -d "${EXTRACT}"

LIB_SRC="$(find "${EXTRACT}" -name 'libootle_sdk_ffi_c.a' -print -quit)"
HEADER_SRC="$(find "${EXTRACT}" -name 'ootle_sdk.h' -print -quit)"
[[ -f "${LIB_SRC}" ]]    || { echo "error: libootle_sdk_ffi_c.a not in ${BASE}" >&2; exit 1; }
[[ -f "${HEADER_SRC}" ]] || { echo "error: ootle_sdk.h not in ${BASE}" >&2; exit 1; }

mkdir -p "${PLAT_DIR}"
echo "==> Vendoring header    -> ${VENDOR_DIR}/ootle_sdk.h"
cp "${HEADER_SRC}" "${VENDOR_DIR}/ootle_sdk.h"
LIB_DEST="${PLAT_DIR}/libootle_sdk_ffi_c.a"
echo "==> Vendoring staticlib -> ${LIB_DEST}"
cp "${LIB_SRC}" "${LIB_DEST}"

# Asset name is ootle_sdk_ffi_c-<version>-<short-sha>-<platform>.zip. Recover version + the
# monorepo short commit from it (platform may contain hyphens, so peel from both ends).
MIDDLE="${BASE#${CRATE}-}"; MIDDLE="${MIDDLE%-${TARI_PLATFORM}.zip}"
CRATE_VERSION="${MIDDLE%-*}"
SHORT_SHA="${MIDDLE##*-}"

ABI="$(sed -nE 's/.*ExpectedABIVersion[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "${REPO_DIR}/internal/cffi/cffi.go")"
[[ -n "${ABI}" ]] || { echo "error: could not extract ExpectedABIVersion from cffi.go" >&2; exit 1; }
SIZE="$(wc -c < "${LIB_DEST}" | tr -d ' ')"
SHA="$(sha256_of "${LIB_DEST}")"

cat > "${PLAT_DIR}/provenance.json" <<EOF
{
  "platform": "${PLAT}",
  "source": "release-asset",
  "monorepo_remote": "https://github.com/${REPO}",
  "monorepo_commit": "${SHORT_SHA}",
  "release_tag": "${TAG}",
  "asset": "${BASE}",
  "crate_version": "${CRATE_VERSION}",
  "abi": "${ABI}",
  "profile": "release",
  "strip": "(stripped upstream by tari-ootle ffi_libs.yml)",
  "size_bytes": ${SIZE},
  "sha256": "${SHA}"
}
EOF
echo "==> Wrote ${PLAT_DIR}/provenance.json"
echo "==> Done. Vendored ${PLAT} from ${REPO}@${TAG} (${BASE})."
