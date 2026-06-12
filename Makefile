# ootle-go — Makefile
#
# Prebuilt native libs are committed per platform under internal/cffi/lib/, so plain
# `go build` / `go test` work out of the box — `make native` is a MAINTAINER step to
# regenerate the host lib from the monorepo. See docs/native-lib.md.
#
# Config (env): OOTLE_MONOREPO (default ../tari-ootle), OOTLE_PROFILE (release|debug).

.PHONY: build native native-all sync-fixtures test vet fmt fmt-check check clean example

# Maintainer: rebuild + vendor the native lib for the HOST platform from the monorepo.
native:
	./scripts/build_native.sh

# Maintainer: rebuild ALL platforms via the GitHub Actions matrix (native runners).
native-all:
	@echo "Trigger the all-platform build against a monorepo ref (tag/SHA):"
	@echo "  gh workflow run native-libs.yml -f monorepo_ref=<ref>"
	@command -v gh >/dev/null 2>&1 && { \
	  read -p "monorepo_ref (blank to skip): " ref; \
	  [ -n "$$ref" ] && gh workflow run native-libs.yml -f monorepo_ref="$$ref" || true; \
	} || echo "(install the GitHub CLI 'gh' to trigger it from here)"

# Re-vendor the golden-vector fixtures from the monorepo (single source of truth).
# The drift test (TestFixtureDrift) fails if the vendored copy diverges from the source.
sync-fixtures:
	./scripts/sync_fixtures.sh

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needs to run on:"; echo "$$out"; exit 1; fi

# Full local gate: build, vet, test, fmt-check (against the committed lib).
check: build vet test fmt-check

clean:
	go clean ./...

# Run one example against a live indexer: make example NAME=balance_query
example:
	go run ./examples/$(NAME)
