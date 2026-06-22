package ootle

import (
	"encoding/json"
	"fmt"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// DecodedSubstate is the kind-tagged view of a fetched substate the core returns from DecodeSubstate.
// It mirrors the core's decoded-substate shape: an externally-tagged enum
// {"kind":"component|vault|resource|other","value":{...}}. Kind is always set; Value is the raw
// per-kind object the caller unmarshals into the matching typed view (VaultValue / ComponentValue /
// ResourceValue) once it has branched on Kind. The host does no CBOR/crypto — the core did the decode.
type DecodedSubstate struct {
	Kind  string          `json:"kind"`
	Value json.RawMessage `json:"value"`
}

// ComponentValue is the DecodedSubstate.Value for Kind=="component".
type ComponentValue struct {
	TemplateAddress string `json:"template_address"`
	EntityID        string `json:"entity_id"`
	// VaultIDs are the vault_<hex> ids embedded in the component state, in state order.
	VaultIDs []string `json:"vault_ids"`
}

// VaultValue is the DecodedSubstate.Value for Kind=="vault". RevealedBalance is the revealed/unlocked
// amount only, as a native uint64 — not a confidential total. The confidential side is surfaced
// explicitly: ConfidentialCommitmentCount + VaultKind ("fungible"|"confidential"); a confidential
// vault's spendable value requires scanning the relevant stealth UTXOs separately, never read this as
// a confidential balance.
type VaultValue struct {
	ResourceAddress             string `json:"resource_address"`
	RevealedBalance             uint64 `json:"revealed_balance"`
	ConfidentialCommitmentCount uint64 `json:"confidential_commitment_count"`
	VaultKind                   string `json:"kind"`
}

// ResourceValue is the DecodedSubstate.Value for Kind=="resource".
type ResourceValue struct {
	ResourceType string  `json:"resource_type"`
	TotalSupply  *uint64 `json:"total_supply,omitempty"`
}

// ResourceBalance is one resource's revealed balance for an account (an AccountBalances element). It
// mirrors the core's ResourceBalance shape. RevealedBalance is the revealed/unlocked sum across the
// account's vaults of this resource, as a native uint64 (no float) — not a confidential total.
type ResourceBalance struct {
	ResourceAddress string `json:"resource_address"`
	RevealedBalance uint64 `json:"revealed_balance"`
}

// accountBalancesEnvelope is the wire shape of ootle_account_balances's data JSON.
type accountBalancesEnvelope struct {
	Balances []ResourceBalance `json:"balances"`
}

// accountBalanceWantsEnvelope is the wire shape of ootle_account_balance_wants's data JSON.
type accountBalanceWantsEnvelope struct {
	FetchIDs []string `json:"fetch_ids"`
}

// DecodeSubstate decodes any fetched substate (the indexer's SubstateValue JSON) into the kind-tagged
// DecodedSubstate. It is a pure, stateless call — the decode lives in the core (no Go CBOR). A vault's
// RevealedBalance is the revealed amount only. An error carries the stable core code ("PARSE" for a
// malformed/unknown substate, "VALIDATION" for a non-u64 balance).
func DecodeSubstate(substateValue json.RawMessage) (DecodedSubstate, error) {
	var out DecodedSubstate
	dataJSON, cerr := cffi.DecodeSubstate(string(substateValue))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal decoded substate: %v", err)}
	}
	return out, nil
}

// AccountBalances computes the revealed balance per resource for an account, summed across its vaults
// in the core (no value-critical arithmetic in Go). account is the account Component substate JSON;
// vaults are the vault substates the host already fetched (the ids AccountBalanceWants named). The core
// rediscovers the account's vault ids from its state and matches them by id: a referenced vault not
// supplied is a "RESOLUTION" error, never a silent zero. Balances are native uint64 and are the
// revealed/unlocked amounts only — a confidential total needs separate UTXO scans.
func AccountBalances(account json.RawMessage, vaults []transport.FetchedSubstate) ([]ResourceBalance, error) {
	vaultsJSON, err := json.Marshal(vaults)
	if err != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("marshal vault substates: %v", err)}
	}
	dataJSON, cerr := cffi.AccountBalances(string(account), string(vaultsJSON))
	if cerr != nil {
		return nil, fromCffiError(cerr)
	}
	var env accountBalancesEnvelope
	if uerr := json.Unmarshal([]byte(dataJSON), &env); uerr != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal account balances: %v", uerr)}
	}
	return env.Balances, nil
}

// AccountBalanceWants names the vault substate ids a host should fetch to satisfy AccountBalances for
// an account. account is the account Component substate JSON; the returned ids are the same
// component-vault discovery AccountBalances does, surfaced as opaque ids. The discovery stays in the
// core. A non-component account is "VALIDATION"; a malformed substate "PARSE".
func AccountBalanceWants(account json.RawMessage) ([]string, error) {
	dataJSON, cerr := cffi.AccountBalanceWants(string(account))
	if cerr != nil {
		return nil, fromCffiError(cerr)
	}
	var env accountBalanceWantsEnvelope
	if uerr := json.Unmarshal([]byte(dataJSON), &env); uerr != nil {
		return nil, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal account balance wants: %v", uerr)}
	}
	return env.FetchIDs, nil
}
