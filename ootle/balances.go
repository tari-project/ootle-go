package ootle

import (
	"context"

	"github.com/tari-project/ootle-go/transport"
)

// AccountBalances reads an account's revealed balance per resource. It drives the host-side
// fetch loop end-to-end: fetch the account component, ask the core which vault ids back its
// balances (AccountBalanceWants), fetch exactly those, then let the core sum each resource's
// revealed amount across its vaults (AccountBalances). All value-critical arithmetic stays in
// the core; the method only sequences the fetches.
//
// The returned RevealedBalance values are the revealed/unlocked amounts only — never a
// confidential total, which requires separate UTXO scanning. Context cancellation flows
// through the underlying fetches. An absent account substate is a RESOLUTION *Error.
func (c *Client) AccountBalances(ctx context.Context, account string) ([]ResourceBalance, error) {
	accountFetched, err := c.transport.FetchSubstates(ctx, []string{account})
	if err != nil {
		return nil, err
	}
	if len(accountFetched) != 1 {
		return nil, &Error{Code: "RESOLUTION", Message: "expected 1 account substate for " + account}
	}
	accountValue := accountFetched[0].SubstateValue

	wantIDs, err := AccountBalanceWants(accountValue)
	if err != nil {
		return nil, err
	}

	var vaults []transport.FetchedSubstate
	if len(wantIDs) > 0 {
		vaults, err = c.transport.FetchSubstates(ctx, wantIDs)
		if err != nil {
			return nil, err
		}
	}

	return AccountBalances(accountValue, vaults)
}

// AccountBalance reads a single resource's revealed balance for an account. It returns 0 when
// the account holds no vault of that resource (not an error). Any fetch/decode failure from
// the underlying AccountBalances read propagates unchanged. The value is the revealed/unlocked
// amount only, never a confidential total.
func (c *Client) AccountBalance(ctx context.Context, account, resource string) (uint64, error) {
	balances, err := c.AccountBalances(ctx, account)
	if err != nil {
		return 0, err
	}
	for _, b := range balances {
		if b.ResourceAddress == resource {
			return b.RevealedBalance, nil
		}
	}
	return 0, nil
}
