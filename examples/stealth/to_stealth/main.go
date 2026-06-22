// Command to_stealth makes a mixed confidential transfer: one confidential stealth output to a
// fresh recipient plus revealed change back to the sender, funded by a revealed input bucket.
// The balance proof excludes the fee, so revealed_input == confidential + revealed change:
// 3 TARI in = 1 confidential + 2 revealed change; the fee is paid separately from the account's
// revealed vault. It asserts the commit, then fetches the recipient's UTXO and scans it with the
// recipient's view secret to verify the confidential value.
//
// The revealed change is carried both as the second output's per-output RevealedAmount and as the
// intent's top-level RevealedOutputAmount; the per-output revealed amounts must sum to the
// top-level RevealedOutputAmount.
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

// Run makes a mixed confidential + revealed-change transfer and scans the recipient's output.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run to_stealth (a transfer needs a resource)")
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

	confidential, change := ootle.Tari(1), ootle.Tari(2)
	// Revealed change back to self: a self output carrying only a revealed deposit (Amount 0).
	changeOutput := ootle.NewStealthOutput(me.Keys.AccountPublicKey, me.View.ViewPublicKey, 0, env.TariResource).
		WithRevealed(change)
	intent, err := ootle.NewStealthTransfer(me.Address, env.TariResource).
		SpendRevealedInput(confidential+change).
		ToStealthOutput(recip.Keys.AccountPublicKey, recip.View.ViewPublicKey, confidential).
		ToOutput(changeOutput).
		ToRevealedOutput(change).
		Intent(ootle.Tari(1))
	if err != nil {
		return fmt.Errorf("build stealth to-stealth: %w", err)
	}

	res, err := client.SendStealthTransfer(ctx, intent, me.StealthKeys())
	if err != nil {
		return fmt.Errorf("stealth to-stealth: %w", err)
	}
	if err := common.MustCommit("stealth to-stealth", res); err != nil {
		return err
	}

	// Both outputs create a utxo_ substate (the confidential output and the revealed change), so
	// scan every new utxo_ with the recipient's view secret and keep the one that decrypts.
	out, err := scanRecipientOutput(ctx, env, res.DiffSummary, recip)
	if err != nil {
		return err
	}
	if out.Value != confidential {
		return fmt.Errorf("recovered confidential value %d != sent %d", out.Value, confidential)
	}
	fmt.Printf("sent %d µTari confidentially to %s (revealed change %d µTari); recovered is_mine=%v value=%d\n",
		confidential, recip.Address, change, out.IsMine, out.Value)
	return nil
}

// scanRecipientOutput fetches every new utxo_ in the diff and scans it with the recipient's scan
// keys, returning the first that decrypts as theirs. Both outputs create a utxo_ (the confidential
// output and the revealed change), so it walks the new UTXOs until one decrypts.
func scanRecipientOutput(ctx context.Context, env common.Env, diff *ootle.DiffSummary, recip ootle.Account) (*ootle.DecryptedOutput, error) {
	tr := common.Transport(env)
	keys := recip.ScanKeys()
	var tried []string
	for {
		utxoID, ok := diff.NewUTXO(tried...)
		if !ok {
			return nil, fmt.Errorf("no new utxo_ in the diff decrypted with the recipient's view secret")
		}
		tried = append(tried, utxoID)
		sub, err := tr.FetchSubstate(ctx, utxoID)
		if err != nil {
			return nil, fmt.Errorf("fetch utxo %s: %w", utxoID, err)
		}
		if sub == nil {
			continue
		}
		out, err := ootle.ScanStealthSubstate(env.Network, keys, sub.SubstateID, sub.SubstateValue)
		if err != nil {
			return nil, fmt.Errorf("scan utxo %s: %w", utxoID, err)
		}
		if out.IsMine {
			return out, nil
		}
	}
}

func main() { common.RunMain(Run) }
