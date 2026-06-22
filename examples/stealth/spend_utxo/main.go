// Command spend_utxo exercises the full confidential lifecycle: deposit a stealth UTXO to self,
// discover and decrypt it, then spend it as a confidential input to a fresh recipient. The deposit
// half is hard-asserted (commit + scan recovers the value); the spend half reports its commit
// outcome.
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

// Run deposits a stealth UTXO to self, decrypts it, then spends it as a confidential input.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run spend_utxo (a confidential spend needs a resource)")
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

	// --- Deposit a confidential UTXO to self ------------------------------------------------
	depositAmount := ootle.Tari(2)
	deposit, err := ootle.NewStealthTransfer(me.Address, env.TariResource).
		SpendRevealedInput(depositAmount).
		ToStealthOutput(me.Keys.AccountPublicKey, me.View.ViewPublicKey, depositAmount).
		Intent(3000)
	if err != nil {
		return fmt.Errorf("build stealth deposit: %w", err)
	}
	depRes, err := client.SendStealthTransfer(ctx, deposit, me.StealthKeys())
	if err != nil {
		return fmt.Errorf("stealth deposit: %w", err)
	}
	if err := common.MustCommit("stealth deposit", depRes); err != nil {
		return err
	}

	utxoID, ok := depRes.DiffSummary.NewUTXO()
	if !ok {
		return fmt.Errorf("no utxo_ substate in the deposit diff")
	}

	tr := common.Transport(env)
	sub, err := tr.FetchSubstate(ctx, utxoID)
	if err != nil {
		return fmt.Errorf("fetch utxo %s: %w", utxoID, err)
	}
	if sub == nil {
		return fmt.Errorf("utxo %s not found on the indexer", utxoID)
	}
	// Decode the UTXO once, then scan it for ownership/value. The decoded output already
	// carries the on-chain commitment, so the spend below pulls it from there.
	utxo, err := ootle.DecodeStealthUTXO(sub.SubstateID, sub.SubstateValue)
	if err != nil {
		return fmt.Errorf("decode utxo %s: %w", utxoID, err)
	}
	out, err := ootle.ScanStealthOutput(env.Network, me.ScanKeys(), utxo)
	if err != nil {
		return fmt.Errorf("scan utxo %s: %w", utxoID, err)
	}
	if !out.IsMine || out.Value != depositAmount {
		return fmt.Errorf("deposit scan failed: is_mine=%v value=%d (want %d)", out.IsMine, out.Value, depositAmount)
	}
	fmt.Printf("deposited and recovered %d µTari (utxo %s)\n", out.Value, utxoID)

	// --- Spend the UTXO as a confidential input ---------------------------------------------
	// SpendUTXO pulls the commitment from the decoded output (no id string-slicing). The
	// confidential input funds the confidential output (even balance, no revealed bucket);
	// PayFeeFromRevealed forces the account key to seal so the fee is paid from the account's
	// revealed vault — without it a pure confidential spend can't authorize the fee.
	spend, err := ootle.NewStealthTransfer(me.Address, env.TariResource).
		SpendUTXO(utxo, me.Keys.AccountPublicKey, me.View.ViewSecret, utxoID).
		ToStealthOutput(recip.Keys.AccountPublicKey, recip.View.ViewPublicKey, depositAmount).
		PayFeeFromRevealed().
		Intent(ootle.Tari(1))
	if err != nil {
		return fmt.Errorf("build stealth spend: %w", err)
	}
	spendRes, err := client.SendStealthTransfer(ctx, spend, me.StealthKeys())
	if err != nil {
		return fmt.Errorf("stealth spend: %w", err)
	}

	// Surface the spend outcome rather than hard-asserting, so a reject still prints its reason.
	switch {
	case spendRes.Pending():
		fmt.Printf("spend submitted, still pending (tx %s)\n", spendRes.Submit.TransactionID)
	case spendRes.IsCommit():
		fmt.Printf("spend committed tx %s\n", spendRes.Submit.TransactionID)
	default:
		fmt.Printf("spend not committed (tx %s): %+v\n", spendRes.Submit.TransactionID, spendRes.RejectReason())
	}
	return nil
}

func main() { common.RunMain(Run) }
