// Command counter_deploy deploys a Counter component from a published template, calls
// increase() on it, then decodes the component's substate. This runs as TWO transactions:
// the first calls the constructor and creates the component; the second calls increase() on
// the resolved component_<hex> address. A workspace value cannot be a method receiver, so the
// newly created component must be read out of the first transaction's diff before it can be
// the receiver of the second.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL       indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK           network keyword (default "localnet")
//	OOTLE_COUNTER_TEMPLATE  template_<hex> of a published Counter — REQUIRED (e.g. from
//	                        publish_template); unset ⇒ logs and exits cleanly
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run deploys a Counter, increments it, then decodes the resulting component.
func Run(ctx context.Context) error {
	tmpl := os.Getenv("OOTLE_COUNTER_TEMPLATE")
	if tmpl == "" {
		log.Print("set OOTLE_COUNTER_TEMPLATE to run counter_deploy")
		return nil
	}

	env := common.LoadEnv()
	sender, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}

	// tx1: construct the component.
	deploy := ootle.NewTransaction().
		PayFeeFromAccount(sender.Address, 3000).
		CallFunction(tmpl, "new").
		Intent()
	r1, err := client.SendInstructions(ctx, deploy, sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if err := common.MustCommit("deploy", r1); err != nil {
		return err
	}
	counter, ok := r1.DiffSummary.NewComponent(sender.Address)
	if !ok {
		return fmt.Errorf("no new component_ in deploy diff")
	}
	fmt.Printf("deployed counter: %s\n", counter)

	// tx2: increment on the resolved address (a workspace ref can't be a receiver).
	inc := ootle.NewTransaction().
		PayFeeFromAccount(sender.Address, 3000).
		CallMethod(counter, "increase").
		Intent()
	r2, err := client.SendInstructions(ctx, inc, sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("increase: %w", err)
	}
	if err := common.MustCommit("increase", r2); err != nil {
		return err
	}

	sub, err := common.Transport(env).FetchSubstate(ctx, counter)
	if err != nil {
		return fmt.Errorf("fetch counter %s: %w", counter, err)
	}
	if sub == nil {
		return fmt.Errorf("counter %s not found", counter)
	}
	dec, err := ootle.DecodeSubstate(sub.SubstateValue)
	if err != nil {
		return fmt.Errorf("decode counter: %w", err)
	}
	fmt.Printf("counter component kind=%s\n", dec.Kind)
	return nil
}

func main() { common.RunMain(Run) }
