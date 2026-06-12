// Package common is the shared glue for the SDK examples: env loading, the bounded-run
// entrypoint, a client/transport constructor, and the faucet/funded-identity setup the
// examples need before they can demonstrate the SDK. It holds no value-critical logic and
// no convenience the SDK itself provides — those are called directly from the example mains.
package common

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/tari-project/ootle-go/ootle"
	"github.com/tari-project/ootle-go/transport"
)

// DefaultFaucetFee is the fee (µTari) used for the faucet claim when none is given.
const DefaultFaucetFee uint64 = 2000

// runTimeout bounds every example run; the network operations dominate it.
const runTimeout = 3 * time.Minute

// RunMain is the standard entrypoint body for an example: it runs fn under a bounded
// context and exits non-zero (via log.Fatalf) on error. Every example's main is just
// `func main() { common.RunMain(Run) }`.
func RunMain(fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		log.Fatalf("%v", err)
	}
}

// Env is the resolved run configuration, read from OOTLE_* env vars.
type Env struct {
	IndexerURL   string        // OOTLE_INDEXER_URL
	Network      ootle.Network // OOTLE_NETWORK
	TariResource string        // OOTLE_TARI_RESOURCE ("" if unset — no safe default exists)
}

// LoadEnv resolves the run configuration from the environment, applying defaults.
func LoadEnv() Env {
	return Env{
		IndexerURL:   envOr("OOTLE_INDEXER_URL", transport.DefaultBaseURL),
		Network:      ootle.Network(envOr("OOTLE_NETWORK", string(ootle.NetworkLocalNet))),
		TariResource: os.Getenv("OOTLE_TARI_RESOURCE"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// NewClient builds an ootle.Client over the indexer at env.IndexerURL, bound to env.Network.
func NewClient(env Env) *ootle.Client {
	return ootle.Connect(env.IndexerURL, ootle.WithNetwork(env.Network))
}

// Transport builds a transport.Client over the indexer at env.IndexerURL, for direct substate reads.
func Transport(env Env) *transport.Client {
	return transport.NewClient(env.IndexerURL)
}

// Faucet claims from the network faucet into acct via the core faucet builder
// (Faucet().Take(pk) over SendInstructions), waits for finality, and asserts the commit.
// fee defaults to DefaultFaucetFee when 0.
func Faucet(ctx context.Context, c *ootle.Client, acct ootle.Account, fee uint64) (ootle.FinalizedResult, error) {
	if fee == 0 {
		fee = DefaultFaucetFee
	}
	intent := ootle.Faucet("").Take(acct.Keys.AccountPublicKey).Intent(fee)
	res, err := c.SendInstructions(ctx, intent, acct.TransferKeys())
	if err != nil {
		return ootle.FinalizedResult{}, fmt.Errorf("faucet claim: %w", err)
	}
	if err := MustCommit("faucet", res); err != nil {
		return res, err
	}
	return res, nil
}

// NewFundedIdentity mints a fresh account, builds a client, and funds it via the faucet.
// It returns the client too so the caller reuses it.
func NewFundedIdentity(ctx context.Context, env Env) (ootle.Account, *ootle.Client, error) {
	acct, err := ootle.NewAccount()
	if err != nil {
		return ootle.Account{}, nil, fmt.Errorf("new account: %w", err)
	}
	c := NewClient(env)
	if _, err := Faucet(ctx, c, acct, 0); err != nil {
		return ootle.Account{}, nil, err
	}
	return acct, c, nil
}

// MustCommit returns an error (with the reject reason) if res is not a full commit, and logs the
// committed tx id otherwise. A nil Outcome means the result is still pending.
func MustCommit(label string, res ootle.FinalizedResult) error {
	if res.Pending() {
		return fmt.Errorf("%s: still pending (no outcome)", label)
	}
	if !res.IsCommit() {
		return fmt.Errorf("%s: not committed: %+v", label, res.RejectReason())
	}
	log.Printf("%s: committed tx %s", label, res.Submit.TransactionID)
	return nil
}
