// Command manual_co_signing demonstrates the two-party co-signing hand-off: party A builds
// and resolves a public transfer, ships the unsigned record to party B, B authorizes it
// (committing to A's seal public key without ever seeing A's secret), then A attaches both
// authorizations, seals, and submits the cosigned transaction via Client.SubmitSealed.
//
// On the cosign seal path the seal signer is a pure relay and carries no authority — every
// owner proof comes from an attached authorization. So A authorizes its own record too (it
// owns the spending account); otherwise the transfer rejects with Access Denied on pay_fee.
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
	"log"

	"github.com/tari-project/ootle-go/examples/internal/common"
	"github.com/tari-project/ootle-go/ootle"
)

// Run drives the co-sign hand-off end-to-end and asserts the cosigned transfer commits.
func Run(ctx context.Context) error {
	env := common.LoadEnv()
	if env.TariResource == "" {
		log.Print("set OOTLE_TARI_RESOURCE to run manual_co_signing (a transfer needs a resource)")
		return nil
	}

	a, client, err := common.NewFundedIdentity(ctx, env)
	if err != nil {
		return err
	}
	recip, err := ootle.NewAccount()
	if err != nil {
		return err
	}
	// The remote co-signer. B holds only its own secret; A never sees it.
	b, err := ootle.GenerateAccountKey()
	if err != nil {
		return err
	}

	intent := ootle.NewTransfer(a.Address).
		ToPublicKey(recip.Keys.AccountPublicKey).
		Resource(env.TariResource).
		Amount(ootle.Tari(1)).
		Fee(3000).
		Intent()

	// Party A: build + resolve, then ship the unsigned record (the wire boundary).
	sealer, err := client.PrepareCosign(ctx, intent)
	if err != nil {
		return err
	}
	defer sealer.Close() // no-op once sealed; releases the handle on error paths.

	record, err := sealer.UnsignedRecord()
	if err != nil {
		return err
	}

	// Party B authorizes A's record, committing to A's seal public key (A's account key).
	authB, err := ootle.AddSignature(env.Network, record, a.Keys.AccountPublicKey, b.AccountSecret)
	if err != nil {
		return err
	}
	// Party A authorizes too: the seal signer is unauthorized once any auth is attached, so
	// A's owner proof for its spending account must come from A's own authorization.
	authA, err := ootle.AddSignature(env.Network, record, a.Keys.AccountPublicKey, a.Keys.AccountSecret)
	if err != nil {
		return err
	}

	// Party A attaches both authorizations, seals, and submits.
	encoded, err := sealer.SealWithAuth(a.TransferKeys(), []ootle.Authorization{authA, authB})
	if err != nil {
		return err
	}
	res, err := client.SubmitSealed(ctx, encoded)
	if err != nil {
		return err
	}
	return common.MustCommit("co-signed transfer", res)
}

func main() { common.RunMain(Run) }
