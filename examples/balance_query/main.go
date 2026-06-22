// Command balance_query mints a fresh identity, funds it from the faucet, then reads its
// account balances back from the indexer: the TARI revealed balance (when a resource is
// configured) plus every resource the account holds a vault for.
//
// It demonstrates the read path — FetchSubstates → AccountBalanceWants → FetchSubstates →
// AccountBalances — where the host only drives the fetch loop and the core sums the balances.
//
// Configuration is read from the environment:
//
//	OOTLE_INDEXER_URL      indexer REST base URL (default transport.DefaultBaseURL)
//	OOTLE_NETWORK          network keyword (default "localnet")
//	OOTLE_TARI_RESOURCE    TARI resource address; when set, the positive-balance assertion runs
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
)

// Run funds a fresh identity and prints its balances, asserting a positive TARI balance when
// OOTLE_TARI_RESOURCE is set.
func Run(ctx context.Context) error {
	env := common.LoadEnv()

	id, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}

	if env.TariResource != "" {
		bal, err := client.AccountBalance(ctx, id.Address, env.TariResource)
		if err != nil {
			return err
		}
		fmt.Printf("account %s balance = %d µTari\n", id.Address, bal)
		if bal == 0 {
			return fmt.Errorf("expected a positive balance after the faucet claim, got 0")
		}
	} else {
		log.Print("OOTLE_TARI_RESOURCE not set; skipping the balance assertion (the faucet commit already proved funding)")
	}

	// Print every resource balance the account holds, asking the core which vaults to fetch.
	balances, err := client.AccountBalances(ctx, id.Address)
	if err != nil {
		return err
	}
	for _, b := range balances {
		fmt.Printf("    %s = %d µTari\n", b.ResourceAddress, b.RevealedBalance)
	}
	return nil
}

func main() { common.RunMain(Run) }
