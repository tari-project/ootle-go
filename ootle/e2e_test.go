//go:build e2e

// Package ootle live end-to-end test.
//
// This file is compiled ONLY under the `e2e` build tag, so the default
// `go test ./...` never builds (and never runs) it — CI without a node stays
// green. To run it you must (a) opt in with the tag and (b) point it at a live
// indexer and supply the transfer parameters via env. Without those it skips
// cleanly even when the tag is set.
//
//	go test -tags e2e -run TestE2EPublicTransfer -v ./...
//
// # Standing up a local node
//
// The `tari_swarm_daemon` spins up a full localnet (Minotari base node + wallet,
// an Ootle validator node, an Ootle wallet, and an Indexer) for development:
//
//	cargo run --bin tari_swarm_daemon --release -- -c data/swarm/config.toml init
//	cargo run --bin tari_swarm_daemon --release -- -c data/swarm/config.toml start
//
// The admin UI at http://localhost:8080 links to each component's web UI and
// JSON-RPC endpoint, including the indexer. This SDK's transport targets the
// indexer REST API (not the wallet daemon's JSON-RPC, whose server-side
// detect_inputs would bypass the core's two-phase resolution). Read the
// indexer's REST base URL off the swarm UI and pass it as OOTLE_E2E_INDEXER_URL.
//
// # Env vars
//
//	OOTLE_E2E=1                  enable the test (any other value / unset ⇒ skip)
//	OOTLE_E2E_INDEXER_URL        indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_E2E_NETWORK            network keyword (default "localnet")
//	OOTLE_E2E_FROM_ACCOUNT       sender account component address (component_<hex>)
//	OOTLE_E2E_ACCOUNT_SECRET     sender account secret key (lowercase hex, no 0x)
//	OOTLE_E2E_RECIPIENT_PK       recipient account public key (lowercase hex)
//	OOTLE_E2E_RESOURCE           resource address (default the TARI/XTR2 resource)
//	OOTLE_E2E_AMOUNT             amount in µTari (default 1000)
//	OOTLE_E2E_FEE                fee in µTari (default 2000)
//
// # Multi-round resolution (live)
//
// `SendPublicTransfer` drives the two-phase resolution loop host-side: it fetches
// the substate ids the core hands back, applies them, and repeats on NeedMore. The
// public transfer's want set discovers a *vault* substate id only after the
// from-account component is fetched and parsed. The C ABI's
// `ootle_apply_fetched_substates` NeedMore response exposes that discovered vault id
// in its `fetch_ids` array, and the driver fetches exactly those next — so the loop
// converges in the realistic 1–2 rounds. (See the multi-round unit tests in
// driver_test.go for the offline proof.)
package ootle

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/tari-project/ootle-go/transport"
)

// readRevealedBalance reads an account's revealed balance for one resource live, driving the
// host-side fetch loop: fetch the account component, ask the core which vault ids to fetch
// (AccountBalanceWants), fetch those, then AccountBalances sums the revealed balance per resource in
// the core. The host only marshals + fetches — no value-critical logic in Go. Returns the revealed
// balance for `resource` (0 if the account holds no vault of that resource).
func readRevealedBalance(ctx context.Context, t *testing.T, tr transport.Transport, account, resource string) uint64 {
	t.Helper()

	// 1. Fetch the account component substate.
	accountFetched, err := tr.FetchSubstates(ctx, []string{account})
	if err != nil {
		t.Fatalf("e2e: fetch account %s: %v", account, err)
	}
	if len(accountFetched) != 1 {
		t.Fatalf("e2e: expected 1 account substate, got %d", len(accountFetched))
	}
	accountValue := accountFetched[0].SubstateValue

	// 2. Ask the core which vault ids to fetch (discovery stays in the core).
	wantIDs, err := AccountBalanceWants(accountValue)
	if err != nil {
		t.Fatalf("e2e: AccountBalanceWants: %v", err)
	}

	// 3. Fetch those vaults.
	var vaults []transport.FetchedSubstate
	if len(wantIDs) > 0 {
		vaults, err = tr.FetchSubstates(ctx, wantIDs)
		if err != nil {
			t.Fatalf("e2e: fetch vaults %v: %v", wantIDs, err)
		}
	}

	// 4. Sum the revealed balance per resource in the core.
	balances, err := AccountBalances(accountValue, vaults)
	if err != nil {
		t.Fatalf("e2e: AccountBalances: %v", err)
	}
	for _, b := range balances {
		if b.ResourceAddress == resource {
			return b.RevealedBalance
		}
	}
	return 0
}

// e2eEnv reads a required env var, skipping the test (not failing) when unset so
// a partially-configured environment never turns CI red.
func e2eEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("e2e: %s not set; skipping live transfer", key)
	}
	return v
}

// e2eEnvDefault reads an optional env var with a fallback.
func e2eEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestE2EPublicTransfer(t *testing.T) {
	if os.Getenv("OOTLE_E2E") != "1" {
		t.Skip("e2e: set OOTLE_E2E=1 (and a live indexer + params) to run the live transfer")
	}

	baseURL := e2eEnvDefault("OOTLE_E2E_INDEXER_URL", transport.DefaultBaseURL)
	network := Network(e2eEnvDefault("OOTLE_E2E_NETWORK", string(NetworkLocalNet)))
	fromAccount := e2eEnv(t, "OOTLE_E2E_FROM_ACCOUNT")
	accountSecret := e2eEnv(t, "OOTLE_E2E_ACCOUNT_SECRET")
	recipientPK := e2eEnv(t, "OOTLE_E2E_RECIPIENT_PK")
	resource := e2eEnv(t, "OOTLE_E2E_RESOURCE")

	amount, err := strconv.ParseUint(e2eEnvDefault("OOTLE_E2E_AMOUNT", "1000"), 10, 64)
	if err != nil {
		t.Fatalf("e2e: bad OOTLE_E2E_AMOUNT: %v", err)
	}
	fee, err := strconv.ParseUint(e2eEnvDefault("OOTLE_E2E_FEE", "2000"), 10, 64)
	if err != nil {
		t.Fatalf("e2e: bad OOTLE_E2E_FEE: %v", err)
	}

	client := NewClient(
		transport.NewClient(baseURL),
		WithNetwork(network),
		WithPollInterval(2*time.Second),
	)

	// Empty Inputs ⇒ the resolved (two-phase) path, exactly what we want to
	// exercise live. The driver derives the want list, fetches, applies, repeats.
	intent := PublicTransferIntent{
		FromAccount:     fromAccount,
		Recipient:       PublicKeyRecipient(recipientPK),
		ResourceAddress: resource,
		Amount:          amount,
		Fee:             fee,
		Inputs:          nil,
	}

	// A generous deadline: build + resolve (1–2 rounds) + submit + wait for
	// finalization on a localnet can take tens of seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Read the sender's revealed balance for the resource before the transfer, via the core's
	// AccountBalanceWants → fetch → AccountBalances path (the same read a host uses to verify a
	// transfer landed). The sum is value-critical and stays in the core.
	tr := transport.NewClient(baseURL)
	balanceBefore := readRevealedBalance(ctx, t, tr, fromAccount, resource)
	t.Logf("e2e: sender revealed balance before = %d µTari", balanceBefore)

	result, err := client.SendPublicTransfer(ctx, intent, PublicTransferKeys{
		AccountSecret: accountSecret,
	})
	if err != nil {
		// A RESOLUTION error here means the two-phase loop could not converge against
		// this live indexer (e.g. the from-account has no vault for the resource).
		// Surface it explicitly so a CI run records why, rather than a bare error.
		var oe *Error
		if errors.As(err, &oe) && oe.Code == "RESOLUTION" {
			t.Fatalf("e2e: resolution did not converge (%v).\n"+
				"Check the from-account holds a vault for the chosen resource on this network.", err)
		}
		t.Fatalf("e2e: SendPublicTransfer failed: %v", err)
	}

	if result.Submit.Outcome == nil {
		t.Fatalf("e2e: result has no outcome (still pending?): %+v", result)
	}
	if !result.Submit.Outcome.IsCommit() {
		t.Fatalf("e2e: expected Commit, got reject: %+v", result.Submit.Outcome.RejectReason())
	}
	if result.FeeReceipt == nil {
		t.Fatalf("e2e: committed result has no fee receipt: %+v", result)
	}
	t.Logf("e2e: committed tx %s, fees paid %d µTari, %d events",
		result.Submit.TransactionID, result.FeeReceipt.TotalFeesPaid, len(result.Events))

	// Transfer-delta assert: read the sender's revealed balance after the transfer and confirm it
	// dropped by exactly amount + the fees actually paid (the fee receipt is authoritative). The whole
	// balance computation runs in the core; the host only reads the two snapshots and subtracts to
	// form the expected delta — it never sums vault values itself.
	feesPaid := result.FeeReceipt.TotalFeesPaid
	balanceAfter := readRevealedBalance(ctx, t, tr, fromAccount, resource)
	t.Logf("e2e: sender revealed balance after = %d µTari", balanceAfter)

	wantSpent := amount + feesPaid
	if balanceBefore < balanceAfter {
		t.Fatalf("e2e: sender balance grew after a transfer (before=%d after=%d)", balanceBefore, balanceAfter)
	}
	gotSpent := balanceBefore - balanceAfter
	if gotSpent != wantSpent {
		t.Fatalf("e2e: revealed-balance delta mismatch: sender dropped by %d, want amount+fees=%d (amount=%d fees=%d)",
			gotSpent, wantSpent, amount, feesPaid)
	}
}

// TestE2ESelfFundingViaFaucet has a fresh identity self-fund via the host Faucet() helper over the
// core faucet builder, with no pre-funded account supplied. It mints a keypair → derives its account
// address → claims from the network faucet → reads the funded revealed balance back. Everything
// value-critical (keygen, address derivation, the faucet claim build, seal, balance sum) is a core
// call; the host only drives.
//
// The core builds the complete self-funding claim (the network faucet deposits into the freshly created
// account), so Faucet("").Take(pk) is the complete claim. The faucet's addresses live in the core, not
// the host.
func TestE2ESelfFundingViaFaucet(t *testing.T) {
	if os.Getenv("OOTLE_E2E") != "1" {
		t.Skip("e2e: set OOTLE_E2E=1 (and a live indexer) to run the self-funding faucet claim")
	}
	// Require an explicit opt-in so this never silently runs against a network without the faucet.
	if os.Getenv("OOTLE_E2E_FAUCET") != "1" {
		t.Skip("e2e: set OOTLE_E2E_FAUCET=1 to run the live faucet self-funding")
	}

	baseURL := e2eEnvDefault("OOTLE_E2E_INDEXER_URL", transport.DefaultBaseURL)
	network := Network(e2eEnvDefault("OOTLE_E2E_NETWORK", string(NetworkLocalNet)))
	resource := e2eEnvDefault("OOTLE_E2E_RESOURCE", "")
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 2. Self-funding claim from the network faucet via the host Faucet() helper: the core funds the
	//    freshly created account and pays the fee from it.
	intent := Faucet("").Take(kp.AccountPublicKey).Intent(fee)
	result, err := client.SendInstructions(ctx, intent, PublicTransferKeys{AccountSecret: kp.AccountSecret})
	if err != nil {
		t.Fatalf("e2e: faucet SendInstructions failed: %v", err)
	}
	if result.Submit.Outcome == nil || !result.Submit.Outcome.IsCommit() {
		t.Fatalf("e2e: faucet claim did not commit: %+v", result)
	}
	t.Logf("e2e: faucet-claim tx %s committed", result.Submit.TransactionID)

	// 3. Verify the funding landed: the account's revealed TARI balance is now positive (the faucet
	//    deposits 1,000 TARI). The balance sum stays in the core.
	if resource == "" {
		t.Log("e2e: OOTLE_E2E_RESOURCE not set; skipping the post-claim balance assertion (the commit above already proves the claim)")
		return
	}
	tr := transport.NewClient(baseURL)
	balance := readRevealedBalance(ctx, t, tr, account, resource)
	t.Logf("e2e: account revealed balance after faucet = %d µTari", balance)
	if balance == 0 {
		t.Fatalf("e2e: expected a positive revealed balance after the faucet claim, got 0")
	}
}
