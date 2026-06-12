#!/usr/bin/env bash
#
# sync_fixtures.sh — re-vendor the golden-vector fixtures from the monorepo.
#
# The committed fixtures in the monorepo's `crates/ootle_sdk_core/fixtures/` are the
# single source of truth (generated from the Rust core: lowercase hex, no `0x`). This
# script copies them byte-for-byte into this repo's `ootle/testdata/fixtures/`, preserving the
# `<group>/<name>.json` layout. The drift test (TestFixtureDrift) fails if the vendored
# copy ever diverges from that source.
#
# Re-run this whenever the core regenerates a fixture or adds a new vector, then commit
# the updated `ootle/testdata/fixtures/`.
#
# Configuration (env):
#   OOTLE_MONOREPO   path to the tari-ootle monorepo (default: ../tari-ootle)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

OOTLE_MONOREPO="${OOTLE_MONOREPO:-${REPO_DIR}/../tari-ootle}"

if [[ ! -d "${OOTLE_MONOREPO}" ]]; then
  echo "error: OOTLE_MONOREPO does not exist: ${OOTLE_MONOREPO}" >&2
  echo "       set OOTLE_MONOREPO to the path of the tari-ootle monorepo." >&2
  exit 1
fi
OOTLE_MONOREPO="$(cd "${OOTLE_MONOREPO}" && pwd)"

SRC_DIR="${OOTLE_MONOREPO}/crates/ootle_sdk_core/fixtures"
DST_DIR="${REPO_DIR}/ootle/testdata/fixtures"

if [[ ! -d "${SRC_DIR}" ]]; then
  echo "error: fixtures source not found: ${SRC_DIR}" >&2
  exit 1
fi

echo "==> Syncing fixtures from ${SRC_DIR}"
# Wipe the vendored tree first so a fixture deleted in the monorepo is also removed here
# (keeps the vendored copy an exact mirror — the drift test enforces it).
rm -rf "${DST_DIR}"
mkdir -p "${DST_DIR}"

# Copy every group's *.json (skip the source's README.md — only fixtures are vendored).
count=0
while IFS= read -r -d '' f; do
  rel="${f#"${SRC_DIR}"/}"
  mkdir -p "${DST_DIR}/$(dirname "${rel}")"
  cp "${f}" "${DST_DIR}/${rel}"
  count=$((count + 1))
done < <(find "${SRC_DIR}" -type f -name '*.json' -print0)

echo "==> Vendored ${count} fixture(s) -> ${DST_DIR}"
echo "==> Done. Run 'go test ./...' to verify drift + vector parity."
