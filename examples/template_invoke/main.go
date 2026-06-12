// Command template_invoke calls into a stablecoin template. With OOTLE_STABLECOIN_TEMPLATE
// set it instantiates a fresh component (passing typed instantiate args and depositing the
// returned admin badge into the sender's account via the workspace), then reads the new
// component_<hex> from the diff. With OOTLE_STABLECOIN_COMPONENT set it attaches to that
// existing component instead. Either way it then calls total_supply() on the component.
//
// The instantiate args use the typed Arg* DSL: ArgAmount for the initial supply, ArgString
// for the symbol, ArgMetadata for the token metadata map, ArgBytes for the view key, and
// ArgBool for the wrapped-token flag. Template calls can also pass ArgI64, ArgNonFungibleID,
// ArgList, and ArgSome/ArgNone; see the arg_dsl example for those.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL          indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK              network keyword (default "localnet")
//	OOTLE_STABLECOIN_TEMPLATE  template_<hex> to instantiate (instantiate path)
//	OOTLE_STABLECOIN_COMPONENT component_<hex> to attach to (existing-component path)
//
// One of OOTLE_STABLECOIN_TEMPLATE or OOTLE_STABLECOIN_COMPONENT is REQUIRED; with neither
// set the example logs and exits cleanly.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run instantiates (or attaches to) a stablecoin component and calls total_supply().
func Run(ctx context.Context) error {
	tmpl := os.Getenv("OOTLE_STABLECOIN_TEMPLATE")
	comp := os.Getenv("OOTLE_STABLECOIN_COMPONENT")
	if tmpl == "" && comp == "" {
		log.Print("set OOTLE_STABLECOIN_TEMPLATE or OOTLE_STABLECOIN_COMPONENT to run template_invoke")
		return nil
	}

	env := common.LoadEnv()
	sender, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}

	component := comp
	if component == "" {
		// instantiate path: construct the component, deposit the returned admin badge into
		// the sender's account, then take the new component_ out of the diff.
		const badge = "admin_badge"
		// instantiate(initial_supply: Amount, token_symbol: String, token_metadata: Metadata,
		//             view_key: RistrettoPublicKeyBytes, enable_wrapped_token: bool)
		intent := ootle.NewTransaction().
			PayFeeFromAccount(sender.Address, 5000).
			CallFunction(tmpl, "instantiate",
				ootle.ArgAmount(1_000_000_000),
				ootle.ArgString("OGO"),
				ootle.ArgMetadata(map[string]string{"provider_name": "OotleGoExample"}),
				ootle.ArgBytes(make([]byte, 32)), // 32-byte zero view key
				ootle.ArgBool(false),
			).
			SaveOutput(badge).
			CallMethod(sender.Address, "deposit", ootle.ArgWorkspace(badge)).
			Intent()
		res, err := client.SendInstructions(ctx, intent, sender.TransferKeys())
		if err != nil {
			return fmt.Errorf("instantiate: %w", err)
		}
		if err := common.MustCommit("instantiate", res); err != nil {
			return err
		}
		var ok bool
		component, ok = res.DiffSummary.NewComponent(sender.Address)
		if !ok {
			return fmt.Errorf("no new component_ in instantiate diff")
		}
		fmt.Printf("instantiated component: %s\n", component)
	}

	supply := ootle.NewTransaction().
		PayFeeFromAccount(sender.Address, 3000).
		CallMethod(component, "total_supply").
		Intent()
	res, err := client.SendInstructions(ctx, supply, sender.TransferKeys())
	if err != nil {
		return fmt.Errorf("total_supply: %w", err)
	}
	if err := common.MustCommit("total_supply", res); err != nil {
		return err
	}
	fmt.Printf("called total_supply on %s\n", component)
	return nil
}

func main() { common.RunMain(Run) }
