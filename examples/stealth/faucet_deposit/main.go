// Command faucet_deposit funds a fresh identity from the faucet (revealed TARI), then makes a
// confidential deposit to itself: a single stealth output funded by the revealed input bucket.
// It fetches the created UTXO back from the indexer and decrypts it with the view secret,
// asserting the output is ours and recovers the deposited value.
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

// Run deposits a stealth UTXO to self, then scans it back to verify ownership and value.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run faucet_deposit (a stealth deposit needs a resource)")
		return nil
	}

	me, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}

	amount := ootle.Tari(2)
	intent, err := ootle.NewStealthTransfer(me.Address, env.TariResource).
		SpendRevealedInput(amount).
		ToStealthOutput(me.Keys.AccountPublicKey, me.View.ViewPublicKey, amount).
		Intent(3000)
	if err != nil {
		return fmt.Errorf("build stealth deposit: %w", err)
	}

	res, err := client.SendStealthTransfer(ctx, intent, me.StealthKeys())
	if err != nil {
		return fmt.Errorf("stealth deposit: %w", err)
	}
	if err := common.MustCommit("stealth deposit", res); err != nil {
		return err
	}

	utxoID, ok := res.DiffSummary.NewUTXO()
	if !ok {
		return fmt.Errorf("no utxo_ substate in the commit diff")
	}

	// Fetch the created UTXO and scan it with our view secret. The core derives the commitment
	// and resource off the substate id and the crypto fields off the value body — the host does
	// no crypto.
	tr := common.Transport(env)
	sub, err := tr.FetchSubstate(ctx, utxoID)
	if err != nil {
		return fmt.Errorf("fetch utxo %s: %w", utxoID, err)
	}
	if sub == nil {
		return fmt.Errorf("utxo %s not found on the indexer", utxoID)
	}

	out, err := ootle.ScanStealthSubstate(env.Network, me.ScanKeys(), sub.SubstateID, sub.SubstateValue)
	if err != nil {
		return fmt.Errorf("scan utxo %s: %w", utxoID, err)
	}
	if !out.IsMine {
		return fmt.Errorf("scanned deposit is not mine (view secret did not decrypt it)")
	}
	if out.Value != amount {
		return fmt.Errorf("recovered value %d != deposited %d", out.Value, amount)
	}
	fmt.Printf("deposited %d µTari confidentially; recovered is_mine=%v value=%d\n", amount, out.IsMine, out.Value)
	return nil
}

func main() { common.RunMain(Run) }
