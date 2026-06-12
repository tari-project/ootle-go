//go:build e2e

// Examples live end-to-end smoke test.
//
// This file is compiled ONLY under the `e2e` build tag, so the default
// `go test ./...` never builds (and never runs) it — CI without a node stays
// green. To run it you must (a) opt in with the tag and (b) point it at a live
// indexer via env. Without OOTLE_E2E=1 it skips cleanly even when the tag is set.
//
//	go test -tags e2e -run TestExamplesSmoke -v ./examples/...
//
// It drives the self-funding, URL-only examples in their own subtests, each with
// a 3-minute deadline. The examples are `package main` programs, which Go does
// not let a test import for their exported Run, so each subtest runs the example
// via `go run ./examples/<name>` and asserts a zero exit. Every example mints a
// fresh throwaway identity and self-funds from the faucet, so the subtests are
// independent and need no shared state or cleanup. The examples that assert a
// resource balance skip that check cleanly when OOTLE_TARI_RESOURCE is unset, so
// the smoke stays green on a minimally-configured swarm and exercises the full
// assert path when it is set.
//
// The stealth and artifact-gated examples are intentionally excluded: they need
// a stealth-capable resource or external artifacts and are environment-sensitive.
//
// # Standing up a local node
//
// The monorepo's `tari_swarm_daemon` spins up a full localnet (Minotari base
// node + wallet, an Ootle validator node, an Ootle wallet, and an Indexer). Read
// the indexer's REST base URL off the swarm admin UI and pass it as
// OOTLE_INDEXER_URL. The examples target the indexer REST API, not the wallet
// daemon's JSON-RPC.
//
// # Env vars (the examples' own OOTLE_* config; no test-only vars)
//
//	OOTLE_E2E=1            enable the smoke (any other value / unset ⇒ skip)
//	OOTLE_INDEXER_URL      indexer REST base URL (default 127.0.0.1:18300)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    resource address; when set, balance-delta asserts run
package examples_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestExamplesSmoke(t *testing.T) {
	if os.Getenv("OOTLE_E2E") != "1" {
		t.Skip("e2e: set OOTLE_E2E=1 (and a live indexer) to run the examples smoke")
	}

	// The self-funding, URL-only examples: each needs at most OOTLE_TARI_RESOURCE
	// and commits a fresh identity's transaction on its own.
	examples := []string{
		"balance_query",
		"fungible_transfer",
		"dry_run",
		"workspace_chain",
		"manual_co_signing",
	}

	for _, name := range examples {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, "go", "run", "./"+name)
			cmd.Env = os.Environ()          // forward the OOTLE_* config to the example
			cmd.WaitDelay = 5 * time.Second // let a timed-out run be reaped, not left running
			out, err := cmd.CombinedOutput()
			t.Logf("e2e: %s output:\n%s", name, out)
			if err != nil {
				t.Fatalf("e2e: %s failed: %v", name, err)
			}
		})
	}
}
