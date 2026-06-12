// Command to_revealed sends through the confidential (stealth) builder but with a fully revealed
// input and a fully revealed output — a degenerate proof with no confidential value. It funds a
// fresh sender from the faucet and pays a fresh recipient, asserting the engine accepts the
// transfer. With no confidential value the balance proof is exact, so the revealed input must
// equal the revealed output; the fee is paid separately from the account's revealed vault.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    stealth-capable resource address — REQUIRED; unset ⇒ logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run sends a revealed-input-to-revealed-output transfer via the stealth builder.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run to_revealed (a transfer needs a resource)")
		return nil
	}

	me, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}
	recip, err := ootle.NewAccount()
	if err != nil {
		return err
	}

	revealed := ootle.Tari(2)
	// No confidential value: the revealed input must match the revealed output exactly. The
	// recipient output carries no confidential value (Amount 0), only a revealed deposit.
	output := ootle.NewStealthOutput(recip.Keys.AccountPublicKey, recip.View.ViewPublicKey, 0, env.TariResource).
		WithRevealed(revealed)
	intent, err := ootle.NewStealthTransfer(me.Address, env.TariResource).
		SpendRevealedInput(revealed).
		ToOutput(output).
		ToRevealedOutput(revealed).
		Intent(3000)
	if err != nil {
		return fmt.Errorf("build stealth to-revealed: %w", err)
	}

	res, err := client.SendStealthTransfer(ctx, intent, me.StealthKeys())
	if err != nil {
		return fmt.Errorf("stealth to-revealed: %w", err)
	}
	if err := common.MustCommit("stealth to-revealed", res); err != nil {
		return err
	}
	fmt.Printf("revealed %d µTari to %s through the stealth builder\n", revealed, recip.Address)
	return nil
}

func main() { common.RunMain(Run) }
