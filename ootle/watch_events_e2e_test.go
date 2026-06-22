//go:build e2e

// Package ootle live end-to-end smoke for WatchEvents (SSE event streaming).
//
// This file is compiled ONLY under the `e2e` build tag, so the default
// `go test ./...` never builds (and never runs) it — CI without a node stays
// green. To run it you must (a) opt in with the tag and (b) point it at a live
// indexer via env. Without those it skips cleanly even when the tag is set.
//
//	go test -tags e2e -run TestE2EWatchEvents -v ./ootle/...
//
// # What it proves
//
// It drives a self-contained event-emitting transaction (a faucet claim from a
// freshly minted identity, exactly like TestE2ESelfFundingViaFaucet) and asserts
// that at least one matching transaction event arrives over Client.WatchEvents
// within a deadline. The watch is opened BEFORE the claim is submitted so the
// live broadcast can never be missed in the gap between submit and connect.
//
// # Standing up a local node
//
// See the header of e2e_test.go (the `tari_swarm_daemon` localnet) for bring-up;
// read the indexer's REST base URL off the swarm UI and pass it as
// OOTLE_E2E_INDEXER_URL.
//
// # Env vars
//
//	OOTLE_E2E=1                  enable the test (any other value / unset ⇒ skip)
//	OOTLE_E2E_FAUCET=1           confirm the network faucet is available
//	OOTLE_E2E_INDEXER_URL        indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_E2E_NETWORK            network keyword (default "localnet")
//	OOTLE_E2E_FEE                fee in µTari (default 2000)
package ootle

import (
	"context"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// TestE2EWatchEvents is the live SSE smoke. It mints a fresh identity, opens a WatchEvents
// stream, then claims from the network faucet (an event-emitting transaction) and asserts a
// matching event arrives on the stream within a deadline.
//
// A faucet claim is the same self-contained, no-pre-funded-account path
// TestE2ESelfFundingViaFaucet uses, and it commits real engine events under a known
// transaction id. The test watches unfiltered and matches Event.TransactionID against the
// committed tx — the strongest deterministic key, since every emitted event carries it.
func TestE2EWatchEvents(t *testing.T) {
	if os.Getenv("OOTLE_E2E") != "1" {
		t.Skip("e2e: set OOTLE_E2E=1 (and a live indexer) to run the live WatchEvents smoke")
	}
	// Require an explicit opt-in so this never silently runs against a network without the faucet.
	if os.Getenv("OOTLE_E2E_FAUCET") != "1" {
		t.Skip("e2e: set OOTLE_E2E_FAUCET=1 to run the live WatchEvents faucet smoke")
	}

	baseURL := e2eEnvDefault("OOTLE_E2E_INDEXER_URL", transport.DefaultBaseURL)
	network := Network(e2eEnvDefault("OOTLE_E2E_NETWORK", string(NetworkLocalNet)))
	fee, err := strconv.ParseUint(e2eEnvDefault("OOTLE_E2E_FEE", "2000"), 10, 64)
	if err != nil {
		t.Fatalf("e2e: bad OOTLE_E2E_FEE: %v", err)
	}

	// 1. Mint a fresh identity and derive its account address — all in the core.
	kp, err := GenerateAccountKey()
	if err != nil {
		t.Fatalf("e2e: GenerateAccountKey: %v", err)
	}
	var pk [32]byte
	pkBytes, err := hex.DecodeString(kp.AccountPublicKey)
	if err != nil || len(pkBytes) != 32 {
		t.Fatalf("e2e: bad account public key hex %q: %v", kp.AccountPublicKey, err)
	}
	copy(pk[:], pkBytes)
	account, err := DeriveAccountAddress(pk)
	if err != nil {
		t.Fatalf("e2e: DeriveAccountAddress: %v", err)
	}
	t.Logf("e2e: fresh account %s", account)

	client := NewClient(transport.NewClient(baseURL), WithNetwork(network), WithPollInterval(2*time.Second))

	// 2. Open the watch BEFORE submitting so the live broadcast cannot be missed in the gap
	//    between submit and connect. Cancel on success/return so the reconnect goroutine in
	//    WatchEvents exits cleanly. Crucially the watch context must OUTLIVE the (up to
	//    3-minute) submit below — its countdown starts here, before SendInstructions runs — so
	//    it is sized to cover the full submit budget PLUS the post-submit event-wait window
	//    (the inner `deadline` timer is the real bound on the assert loop).
	watchCtx, cancelWatch := context.WithTimeout(context.Background(), 3*time.Minute+90*time.Second)
	defer cancelWatch()
	events, errs := client.WatchEvents(watchCtx, EventFilter{})

	// 3. Self-funding claim from the network faucet via the host Faucet() helper. The claim commits
	//    real engine events under a known transaction id.
	submitCtx, cancelSubmit := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancelSubmit()
	intent := Faucet("").Take(kp.AccountPublicKey).Intent(fee)
	result, err := client.SendInstructions(submitCtx, intent, PublicTransferKeys{AccountSecret: kp.AccountSecret})
	if err != nil {
		t.Fatalf("e2e: faucet SendInstructions failed: %v", err)
	}
	if result.Submit.Outcome == nil || !result.Submit.Outcome.IsCommit() {
		t.Fatalf("e2e: faucet claim did not commit: %+v", result)
	}
	txID := result.Submit.TransactionID
	t.Logf("e2e: faucet-claim tx %s committed with %d events", txID, len(result.Events))
	if len(result.Events) == 0 {
		t.Fatalf("e2e: committed faucet claim emitted no events; nothing for WatchEvents to deliver")
	}

	// 4. Assert a matching event arrives on the stream. Match by transaction id — the strongest
	//    deterministic key (every emitted event carries it). Tolerate a repeated event id across
	//    a reconnect (at-least-once delivery); we assert ≥1 match, not an exact count.
	deadline := time.After(60 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("e2e: event stream closed before a matching event arrived")
			}
			if ev.TransactionID == txID {
				t.Logf("e2e: matched event id=%d topic=%q substate=%q tx=%s",
					ev.ID, ev.Topic, ev.SubstateID, ev.TransactionID)
				return // success; defer cancelWatch() tears the stream down
			}
			t.Logf("e2e: ignoring unrelated event id=%d tx=%s", ev.ID, ev.TransactionID)
		case err, ok := <-errs:
			if !ok {
				errs = nil // error channel closed; keep waiting on events/deadline
				continue
			}
			// Reconnect warnings are non-fatal; the stream keeps trying. Surface a terminal
			// error (e.g. a transport that cannot stream) loudly.
			t.Logf("e2e: watch error (non-fatal warning): %v", err)
		case <-deadline:
			t.Fatalf("e2e: no event for tx %s within deadline", txID)
		case <-watchCtx.Done():
			t.Fatalf("e2e: watch context done before a matching event arrived: %v", watchCtx.Err())
		}
	}
}
