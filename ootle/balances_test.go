package ootle

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tari-project/ootle-go/transport"
)

// balanceFixture mirrors the account_balances golden vector: an account component substate
// plus the vault substates its balances are summed from.
type balanceFixture struct {
	Input struct {
		SubstateValue  json.RawMessage             `json:"substate_value"`
		VaultSubstates []transport.FetchedSubstate `json:"vault_substates"`
	} `json:"input"`
	Expected struct {
		AccountBalances []ResourceBalance `json:"account_balances"`
	} `json:"expected"`
}

func loadBalanceFixture(t *testing.T) balanceFixture {
	t.Helper()
	path := filepath.Join("testdata", "fixtures", "account_balances", "multi_vault_u64.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var f balanceFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", path, err)
	}
	return f
}

const testAccountAddress = "component_0000000000000000000000000000000000000000000000000000000000000000"

// balanceMock serves the fixture's account component for the account id and its vault
// substates by id, modelling an indexer that fetches strictly what it is told to.
func balanceMock(t *testing.T, f balanceFixture) *mockTransport {
	t.Helper()
	byID := map[string]transport.FetchedSubstate{
		testAccountAddress: {SubstateID: testAccountAddress, SubstateValue: f.Input.SubstateValue},
	}
	for _, v := range f.Input.VaultSubstates {
		byID[v.SubstateID] = v
	}
	return &mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			out := make([]transport.FetchedSubstate, 0, len(ids))
			for _, id := range ids {
				if sub, ok := byID[id]; ok {
					out = append(out, sub)
				}
			}
			return out, nil
		},
	}
}

func TestClientAccountBalances(t *testing.T) {
	f := loadBalanceFixture(t)
	c := NewClient(balanceMock(t, f))

	balances, err := c.AccountBalances(context.Background(), testAccountAddress)
	if err != nil {
		t.Fatalf("AccountBalances: %v", err)
	}
	if len(f.Expected.AccountBalances) == 0 {
		t.Fatal("fixture has no expected balances")
	}
	if len(balances) != len(f.Expected.AccountBalances) {
		t.Fatalf("got %d balances, want %d", len(balances), len(f.Expected.AccountBalances))
	}
	for i, want := range f.Expected.AccountBalances {
		if balances[i] != want {
			t.Fatalf("balance[%d] = %+v, want %+v", i, balances[i], want)
		}
	}

	want := f.Expected.AccountBalances[0]
	got, err := c.AccountBalance(context.Background(), testAccountAddress, want.ResourceAddress)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != want.RevealedBalance {
		t.Fatalf("AccountBalance = %d, want %d", got, want.RevealedBalance)
	}
}

func TestClientAccountBalanceMissingResource(t *testing.T) {
	f := loadBalanceFixture(t)
	c := NewClient(balanceMock(t, f))

	got, err := c.AccountBalance(context.Background(), testAccountAddress, "resource_deadbeef")
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if got != 0 {
		t.Fatalf("AccountBalance for absent resource = %d, want 0", got)
	}
}

func TestClientAccountBalancesAbsentVault(t *testing.T) {
	f := loadBalanceFixture(t)
	// Serve the account component but no vaults: the core must surface a referenced-but-
	// unsupplied vault as a RESOLUTION error, never a silent zero.
	c := NewClient(&mockTransport{
		fetch: func(_ context.Context, ids []string) ([]transport.FetchedSubstate, error) {
			if len(ids) == 1 && ids[0] == testAccountAddress {
				return []transport.FetchedSubstate{{SubstateID: testAccountAddress, SubstateValue: f.Input.SubstateValue}}, nil
			}
			// Account references vaults, but the fetch returns none of them.
			return []transport.FetchedSubstate{}, nil
		},
	})

	_, err := c.AccountBalances(context.Background(), testAccountAddress)
	var e *Error
	if !errors.As(err, &e) || e.Code != "RESOLUTION" {
		t.Fatalf("expected RESOLUTION *Error for absent vault, got %v", err)
	}
}

func TestClientAccountBalancesAbsentAccount(t *testing.T) {
	c := NewClient(&mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return nil, nil
		},
	})

	_, err := c.AccountBalances(context.Background(), testAccountAddress)
	var e *Error
	if !errors.As(err, &e) || e.Code != "RESOLUTION" {
		t.Fatalf("expected RESOLUTION *Error for absent account, got %v", err)
	}
}

func TestClientAccountBalancesTransportError(t *testing.T) {
	sentinel := errors.New("boom")
	c := NewClient(&mockTransport{
		fetch: func(_ context.Context, _ []string) ([]transport.FetchedSubstate, error) {
			return nil, sentinel
		},
	})

	_, err := c.AccountBalances(context.Background(), testAccountAddress)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected transport error to propagate, got %v", err)
	}
}
