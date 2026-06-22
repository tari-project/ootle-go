//go:build e2e

// Package ootle live confidential (stealth) round-trip end-to-end test.
//
// Compiled ONLY under the `e2e` build tag (same convention as e2e_test.go), so the
// default `go test ./...` never builds or runs it — CI without a node stays green. To
// run it you must (a) opt in with the tag, (b) set OOTLE_E2E=1, and (c) point it at a
// live indexer + supply the sender/recipient stealth parameters via env. Without those
// it SKIPS cleanly (never fails) even when the tag is set.
//
//	go test -tags e2e -run TestStealthRoundTrip -v ./...
//
// # What it exercises
//
// A full send→receive round-trip across the C ABI against a live network:
//
//  1. SendStealthTransfer (production path — random entropy, a realistic submission)
//     drives the confidential transfer end-to-end: fetch input UTXOs → build → seal →
//     submit → wait. It asserts the FinalizedResult is a Commit.
//  2. The new stealth output UTXO substate is fetched from the indexer
//     (transport.FetchSubstate). Its on-chain substate id is supplied via
//     OOTLE_E2E_STEALTH_OUTPUT_UTXO — the host stays thin (no engine-side id derivation
//     here; the sender knows the commitment of the output it just created).
//  3. ScanStealthOutput decrypts that UTXO with the recipient's view keys and asserts
//     IsMine == true and Value == the sent amount — the receive half of the round-trip.
//
// # Standing up a local node (don't invent one — use the monorepo's swarm)
//
// As e2e_test.go: from a `tari-ootle` checkout, `tari_swarm_daemon` spins up a full
// localnet (base node + wallet, an Ootle validator, an Ootle wallet, and an Indexer):
//
//	cargo run --bin tari_swarm_daemon --release -- -c data/swarm/config.toml init
//	cargo run --bin tari_swarm_daemon --release -- -c data/swarm/config.toml start
//
// Read the indexer's REST base URL off the swarm admin UI (http://localhost:8080) and
// pass it as OOTLE_E2E_INDEXER_URL. The SDK transport targets the indexer REST API (VR8).
//
// # Env vars
//
//	OOTLE_E2E=1                        enable the test (any other value / unset ⇒ skip)
//	OOTLE_E2E_INDEXER_URL              indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_E2E_NETWORK                  network keyword (default "localnet")
//	OOTLE_E2E_STEALTH_FROM_ACCOUNT     sender account component address (component_<hex>)
//	OOTLE_E2E_STEALTH_ACCOUNT_SECRET   sender account secret key (lowercase hex, no 0x)
//	OOTLE_E2E_STEALTH_RESOURCE         stealth resource address (resource_<hex>)
//	OOTLE_E2E_STEALTH_DEST_ACCOUNT_PK  recipient account public key (lowercase hex)
//	OOTLE_E2E_STEALTH_DEST_VIEW_PK     recipient view public key (lowercase hex)
//	OOTLE_E2E_STEALTH_VIEW_SECRET      recipient view secret (lowercase hex) — for the scan
//	OOTLE_E2E_STEALTH_OUTPUT_UTXO      the created output's UTXO substate id (utxo_<res>_<commitment>)
//	OOTLE_E2E_STEALTH_AMOUNT           amount in µTari (default 1000)
//	OOTLE_E2E_STEALTH_FEE              fee in µTari (default 2000)
//
// The SCAN half decodes the fetched UTXO substate into the inbound output via the core's
// ScanStealthSubstate (decode → scan in one C call) — NO env-supplied crypto fields. The transport
// carries the UTXO substate value verbatim (json.RawMessage); the core derives the commitment +
// resource off the substate id and the crypto fields off the value body, then scans. The host does
// no crypto/CBOR.
package ootle

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

func TestStealthRoundTrip(t *testing.T) {
	if os.Getenv("OOTLE_E2E") != "1" {
		t.Skip("e2e: set OOTLE_E2E=1 (and a live indexer + stealth params) to run the live round-trip")
	}

	baseURL := e2eEnvDefault("OOTLE_E2E_INDEXER_URL", transport.DefaultBaseURL)
	network := Network(e2eEnvDefault("OOTLE_E2E_NETWORK", string(NetworkLocalNet)))

	fromAccount := e2eEnv(t, "OOTLE_E2E_STEALTH_FROM_ACCOUNT")
	accountSecret := e2eEnv(t, "OOTLE_E2E_STEALTH_ACCOUNT_SECRET")
	resource := e2eEnv(t, "OOTLE_E2E_STEALTH_RESOURCE")
	destAccountPK := e2eEnv(t, "OOTLE_E2E_STEALTH_DEST_ACCOUNT_PK")
	destViewPK := e2eEnv(t, "OOTLE_E2E_STEALTH_DEST_VIEW_PK")
	viewSecret := e2eEnv(t, "OOTLE_E2E_STEALTH_VIEW_SECRET")
	outputUTXO := e2eEnv(t, "OOTLE_E2E_STEALTH_OUTPUT_UTXO")

	amount, err := strconv.ParseUint(e2eEnvDefault("OOTLE_E2E_STEALTH_AMOUNT", "1000"), 10, 64)
	if err != nil {
		t.Fatalf("e2e: bad OOTLE_E2E_STEALTH_AMOUNT: %v", err)
	}
	fee, err := strconv.ParseUint(e2eEnvDefault("OOTLE_E2E_STEALTH_FEE", "2000"), 10, 64)
	if err != nil {
		t.Fatalf("e2e: bad OOTLE_E2E_STEALTH_FEE: %v", err)
	}

	client := NewClient(
		transport.NewClient(baseURL),
		WithNetwork(network),
		WithPollInterval(2*time.Second),
	)

	// A revealed-input-funded confidential transfer: one stealth output to the recipient,
	// funded by the sender's revealed bucket (no stealth inputs to spend ⇒ no fetch round).
	// This is the simplest live send that still exercises seal + submit + a real output UTXO.
	intent := StealthTransferIntent{
		FromAccount:     fromAccount,
		ResourceAddress: resource,
		Fee:             fee,
		Outputs: []StealthOutputSpec{
			{
				DestinationAccountPublicKey: destAccountPK,
				DestinationViewPublicKey:    destViewPK,
				Amount:                      amount,
				ResourceAddress:             resource,
				PayTo:                       PayToStealthPublicKey,
			},
		},
		RevealedInputAmount: amount,
	}

	// Generous deadline: build + submit + finalization on a localnet can take tens of seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// --- Send half --------------------------------------------------------------------------
	result, err := client.SendStealthTransfer(ctx, intent, StealthProductionKeys{
		AccountSecret: accountSecret,
	})
	if err != nil {
		t.Fatalf("e2e: SendStealthTransfer failed: %v", err)
	}
	if result.Submit.Outcome == nil {
		t.Fatalf("e2e: result has no outcome (still pending?): %+v", result)
	}
	if !result.Submit.Outcome.IsCommit() {
		t.Fatalf("e2e: expected Commit, got reject: %+v", result.Submit.Outcome.RejectReason())
	}
	t.Logf("e2e: stealth transfer committed tx %s", result.Submit.TransactionID)

	// --- Receive half -----------------------------------------------------------------------
	// Fetch the created output UTXO substate from the indexer and scan it with the recipient's
	// view keys via ScanStealthSubstate (decode → scan in one C call). The transport carries the
	// SubstateValue verbatim; the core derives the commitment + resource off the substate id and
	// the crypto fields off the value body, then scans. NO env-supplied crypto fields — the host
	// does no crypto/CBOR.
	fetched, err := client.transport.FetchSubstates(ctx, []string{outputUTXO})
	if err != nil {
		t.Fatalf("e2e: FetchSubstates(%s): %v", outputUTXO, err)
	}
	if len(fetched) == 0 {
		t.Fatalf("e2e: output UTXO %s not found on the indexer (404)", outputUTXO)
	}
	sub := fetched[0]

	scanKeys := StealthScanKeys{ViewSecret: viewSecret}
	out, err := ScanStealthSubstate(network, scanKeys, sub.SubstateID, sub.SubstateValue)
	if err != nil {
		t.Fatalf("e2e: ScanStealthSubstate(%s): %v", sub.SubstateID, err)
	}
	if !out.IsMine {
		t.Fatalf("e2e: scanned output is not mine (expected the recipient's view secret to decrypt it)")
	}
	if out.Value != amount {
		t.Fatalf("e2e: scanned value %d != sent amount %d", out.Value, amount)
	}
	t.Logf("e2e: stealth round-trip OK — recovered value %d µTari, mask %s", out.Value, out.Mask)
}
