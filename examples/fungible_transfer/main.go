// Command fungible_transfer funds a fresh sender from the faucet, estimates the fee with a
// dry-run, then makes a real single-recipient TARI transfer to a fresh (unfunded) recipient
// whose account the engine creates implicitly. It verifies the commit and, when a resource is
// configured, the balance deltas on both sides.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    TARI resource address — REQUIRED (a transfer names a resource);
//	                       unset ⇒ the example logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run runs a dry-run fee estimate followed by a real transfer and asserts the outcome.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run fungible_transfer (a transfer needs a resource)")
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

	amount, fee := ootle.Tari(2), uint64(3000)
	transfer := ootle.NewTransfer(sender.Address).
		ToPublicKey(recip.Keys.AccountPublicKey).
		Resource(env.TariResource).
		Amount(amount).
		Fee(fee)

	// Dry-run first: estimate the fee without committing anything. Use the intent's
	// copy-on-write AsDryRun() so the shared builder is left unchanged and the real send
	// below still commits (TransferBuilder.DryRun() would mutate the builder in place).
	dr, err := client.SendPublicTransfer(ctx, transfer.Intent().AsDryRun(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}
	if !dr.IsCommit() {
		return fmt.Errorf("dry-run did not commit: %+v", dr.RejectReason())
	}
	fmt.Printf("estimated fee = %d µTari\n", dr.EstimatedFeeOr(0))

	senderBefore, err := client.AccountBalance(ctx, sender.Address, env.TariResource)
	if err != nil {
		return err
	}

	res, err := client.SendPublicTransfer(ctx, transfer.Intent(), sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("transfer: %w", err)
	}
	if err := common.MustCommit("transfer", res); err != nil {
		return err
	}
	if res.FeeReceipt == nil {
		return fmt.Errorf("committed transfer has no fee receipt")
	}

	// Balance deltas: the sender drops by amount + the fees actually paid; the recipient holds
	// exactly amount. The sums run in the core; the host only subtracts the two snapshots.
	feesPaid := res.FeeReceipt.TotalFeesPaid
	senderAfter, err := client.AccountBalance(ctx, sender.Address, env.TariResource)
	if err != nil {
		return err
	}
	if senderBefore < senderAfter {
		return fmt.Errorf("sender balance grew after a transfer (before=%d after=%d)", senderBefore, senderAfter)
	}
	if got, want := senderBefore-senderAfter, amount+feesPaid; got != want {
		return fmt.Errorf("sender delta mismatch: dropped by %d, want amount+fees=%d (amount=%d fees=%d)", got, want, amount, feesPaid)
	}

	recipBal, err := client.AccountBalance(ctx, recip.Address, env.TariResource)
	if err != nil {
		return err
	}
	if recipBal != amount {
		return fmt.Errorf("recipient balance mismatch: got %d, want %d", recipBal, amount)
	}
	fmt.Printf("transferred %d µTari to %s (fees paid %d µTari)\n", amount, recip.Address, feesPaid)
	return nil
}

func main() { common.RunMain(Run) }
