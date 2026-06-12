// Command publish_template publishes a WASM template to the network. It reads the WASM
// blob, dry-runs the publish to surface the fee, publishes it for real, finds the new
// template_<hex> in the diff, and prints an OOTLE_COUNTER_TEMPLATE= line that the
// counter_deploy example consumes.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TEMPLATE_WASM    path to a compiled template .wasm — REQUIRED; unset ⇒ logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run publishes the WASM template named by OOTLE_TEMPLATE_WASM.
func Run(ctx context.Context) error {
	path := os.Getenv("OOTLE_TEMPLATE_WASM")
	if path == "" {
		log.Print("set OOTLE_TEMPLATE_WASM to run publish_template")
		return nil
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read wasm %s: %w", path, err)
	}

	env := common.LoadEnv()
	sender, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}

	// Publishing a WASM blob on-chain is far heavier than an ordinary transfer; start from a
	// generous ceiling and let the dry-run below tighten it to the real cost.
	const ceilingFee = 1_000_000
	tx := ootle.NewTransaction().
		PayFeeFromAccount(sender.Address, ceilingFee).
		Blob(ootle.NewBlob(wasm)).
		PublishTemplate(0)

	fee := uint64(ceilingFee)
	if dr, err := client.SendInstructions(ctx, tx.Intent().AsDryRun(), sender.TransferKeys()); err != nil {
		return fmt.Errorf("dry-run publish: %w", err)
	} else if !dr.IsCommit() {
		return fmt.Errorf("dry-run publish was not accepted: %+v", dr.RejectReason())
	} else if est := dr.EstimatedFeeOr(0); est != 0 {
		fmt.Printf("estimated publish fee = %d µTari\n", est)
		fee = est
	}

	intent := tx.PayFeeFromAccount(sender.Address, fee).Intent()
	res, err := client.SendInstructions(ctx, intent, sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	if err := common.MustCommit("publish", res); err != nil {
		return err
	}

	tmpl, ok := res.DiffSummary.NewTemplate()
	if !ok {
		return fmt.Errorf("no template_ in diff")
	}
	fmt.Printf("OOTLE_COUNTER_TEMPLATE=%s\n", tmpl)
	return nil
}

func main() { common.RunMain(Run) }
