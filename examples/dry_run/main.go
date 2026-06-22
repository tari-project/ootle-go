// Command dry_run demonstrates fee estimation without committing: a valid dry-run that the
// engine would accept (prints its estimated fee) and an over-spend dry-run the engine would
// reject (prints the reject reason). It never submits a real transfer.
//
// A dry-run executes fully — it carries the same fee receipt, events, and diff an accepted
// transfer would — but is never committed on-chain. Its distinguishing marker is a non-nil
// EstimatedFee (the minimum max_fee for a real send); a committed transfer leaves that nil.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    TARI resource address — REQUIRED; unset ⇒ logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run performs a valid and an over-spend dry-run, printing the estimated fee and reject reason.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run dry_run (a transfer needs a resource)")
		return nil
	}

	sender, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}
	recip, err := ootle.NewAccount()
	if err != nil {
		return err
	}

	transfer := ootle.NewTransfer(sender.Address).
		ToPublicKey(recip.Keys.AccountPublicKey).
		Resource(env.TariResource).
		Fee(2000).
		DryRun()

	r1, err := client.SendPublicTransfer(ctx, transfer.Amount(ootle.Tari(1)).Intent(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("valid dry-run: %w", err)
	}
	if r1.Pending() {
		return fmt.Errorf("valid dry-run is still pending (no outcome)")
	}
	fmt.Printf("valid dry-run: commit=%v estimated_fee=%d µTari\n", r1.IsCommit(), r1.EstimatedFeeOr(0))
	if !r1.IsCommit() {
		return fmt.Errorf("valid dry-run was not accepted: %+v", r1.RejectReason())
	}

	// An amount well beyond the funded balance: the engine rejects the dry-run.
	r2, err := client.SendPublicTransfer(ctx, transfer.Amount(ootle.Tari(1_000_000)).Intent(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("over-spend dry-run: %w", err)
	}
	if r2.Pending() {
		return fmt.Errorf("over-spend dry-run is still pending (no outcome)")
	}
	fmt.Printf("over-spend dry-run: commit=%v reason=%+v\n", r2.IsCommit(), r2.RejectReason())

	// Both runs were dry-runs, never committed: each is marked by a non-nil EstimatedFee.
	if r1.EstimatedFee == nil || r2.EstimatedFee == nil {
		return fmt.Errorf("a dry-run did not surface an estimated fee (the dry-run marker)")
	}
	return nil
}

func main() { common.RunMain(Run) }
