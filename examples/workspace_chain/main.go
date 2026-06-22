// Command workspace_chain pipes a bucket through the workspace in a single generic
// transaction: withdraw TARI from the sender, put the resulting bucket on the workspace,
// deposit it into a (pre-funded) recipient, and atomically create one more account. It
// dry-runs the chain first to surface the fee and any reject reason, verifies the commit,
// finds the newly created component in the diff, and — when a resource is configured —
// checks the recipient gained the transferred amount.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    TARI resource address — REQUIRED (the withdraw names a resource);
//	                       unset ⇒ logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run drives the withdraw → workspace → deposit chain plus an atomic account creation.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run workspace_chain (the withdraw needs a resource)")
		return nil
	}

	sender, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}
	// The recipient must already exist as an account to receive a deposit, so fund it too.
	recip, _, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}
	// Created atomically inside the chain — never funded.
	extra, err := ootle.NewAccount()
	if err != nil {
		return err
	}

	amount := ootle.Tari(2)
	recipBefore, err := client.AccountBalance(ctx, recip.Address, env.TariResource)
	if err != nil {
		return err
	}

	tx := ootle.NewTransaction().
		PayFeeFromAccount(sender.Address, 3000).
		CallMethod(sender.Address, "withdraw", ootle.ArgAddress(env.TariResource), ootle.ArgAmount(amount)).
		SaveOutput("bucket").
		CallMethod(recip.Address, "deposit", ootle.ArgWorkspace("bucket")).
		Add(ootle.CreateAccount(extra.Keys.AccountPublicKey))

	// Dry-run first to surface the estimated fee and any reject reason before paying for the real send.
	dr, err := client.SendInstructions(ctx, tx.Intent().AsDryRun(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("dry-run workspace chain: %w", err)
	}
	if dr.Pending() {
		return fmt.Errorf("dry-run workspace chain is still pending (no outcome)")
	}
	if !dr.IsCommit() {
		return fmt.Errorf("dry-run workspace chain was not accepted: %+v", dr.RejectReason())
	}
	fmt.Printf("dry-run ok, estimated fee = %d µTari\n", dr.EstimatedFeeOr(0))

	res, err := client.SendInstructions(ctx, tx.Intent(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("workspace chain: %w", err)
	}
	if err := common.MustCommit("workspace chain", res); err != nil {
		return err
	}

	newAcct, ok := res.DiffSummary.NewComponent(sender.Address, recip.Address)
	if !ok {
		return fmt.Errorf("expected a newly created component_ account in the diff")
	}
	fmt.Printf("created extra account: %s\n", newAcct)

	// Best-effort balance check: the recipient gained the transferred amount.
	recipAfter, err := client.AccountBalance(ctx, recip.Address, env.TariResource)
	if err != nil {
		return err
	}
	if recipAfter < recipBefore {
		return fmt.Errorf("recipient balance shrank after a deposit (before=%d after=%d)", recipBefore, recipAfter)
	}
	if got := recipAfter - recipBefore; got != amount {
		return fmt.Errorf("recipient delta mismatch: gained %d, want %d", got, amount)
	}
	fmt.Printf("deposited %d µTari into %s\n", amount, recip.Address)
	return nil
}

func main() { common.RunMain(Run) }
