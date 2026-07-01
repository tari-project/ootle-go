package ootle

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tari-project/ootle-go/internal/cffi"
)

// Network is the L1 network keyword (e.g. "esmeralda"). The FFI boundary takes the
// discriminant byte, mapped here by ByteValue.
type Network string

const (
	NetworkMainNet   Network = "mainnet"
	NetworkStageNet  Network = "stagenet"
	NetworkNextNet   Network = "nextnet"
	NetworkLocalNet  Network = "localnet"
	NetworkIgor      Network = "igor"
	NetworkEsmeralda Network = "esmeralda"
)

// networkBytes maps each network keyword to its L1 discriminant byte. The FFI ops take
// this byte, not the keyword.
var networkBytes = map[Network]uint8{
	NetworkMainNet:   0x00,
	NetworkStageNet:  0x01,
	NetworkNextNet:   0x02,
	NetworkLocalNet:  0x10,
	NetworkIgor:      0x24,
	NetworkEsmeralda: 0x26,
}

// ByteValue returns the network's L1 discriminant byte and whether the keyword is known.
func (n Network) ByteValue() (uint8, bool) {
	b, ok := networkBytes[n]
	return b, ok
}

// Error is the typed boundary error: the stable core error code (e.g. "PARSE", "KEY",
// "VALIDATION", "RESOLUTION", "INTERNAL") plus the human-readable message. It implements
// the error interface.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// cause is an optional wrapped sentinel (e.g. ErrResolutionDidNotConverge) so callers
	// can use errors.Is on a known sentinel while still inspecting the stable Code via
	// errors.As. It is not serialized (it carries the same information as Code/Message).
	cause error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped sentinel (if any) for errors.Is.
func (e *Error) Unwrap() error { return e.cause }

// fromCffiError converts an internal cffi error into a public *Error, preserving the
// stable code. Non-typed errors (e.g. ABI mismatch) are wrapped under code "INTERNAL".
func fromCffiError(err error) error {
	if err == nil {
		return nil
	}
	var ce *cffi.Error
	if errors.As(err, &ce) {
		return &Error{Code: ce.Code, Message: ce.Message}
	}
	return &Error{Code: "INTERNAL", Message: err.Error()}
}

// InputRef is one explicit input: a canonical substate id plus an optional version.
// Mirrors the core's InputRef (a nil version is unversioned).
type InputRef struct {
	SubstateID string  `json:"substate_id"`
	Version    *uint32 `json:"version"`
}

// TransferRecipient is the tagged recipient of a public transfer: exactly one of a
// lowercase-hex account public key or a component address string. The fields are
// unexported so a recipient can only be built through PublicKeyRecipient /
// AccountRecipient — a both-set / neither-set value is unrepresentable. It marshals to
// the core's externally-tagged enum form:
//
//	{"PublicKey": "<hex>"}  or  {"Account": "component_<hex>"}
type TransferRecipient struct {
	publicKey *string
	account   *string
}

// PublicKeyRecipient builds a recipient from a lowercase-hex account public key.
func PublicKeyRecipient(hexPublicKey string) TransferRecipient {
	return TransferRecipient{publicKey: &hexPublicKey}
}

// AccountRecipient builds a recipient from an existing account component address.
func AccountRecipient(componentAddress string) TransferRecipient {
	return TransferRecipient{account: &componentAddress}
}

// MarshalJSON emits the externally-tagged enum the core expects.
func (r TransferRecipient) MarshalJSON() ([]byte, error) {
	switch {
	case r.publicKey != nil:
		return json.Marshal(map[string]string{"PublicKey": *r.publicKey})
	case r.account != nil:
		return json.Marshal(map[string]string{"Account": *r.account})
	default:
		return nil, errors.New("ootle: TransferRecipient has no variant set (use PublicKeyRecipient or AccountRecipient)")
	}
}

// UnmarshalJSON decodes the externally-tagged enum form.
func (r *TransferRecipient) UnmarshalJSON(data []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["PublicKey"]; ok {
		r.publicKey = &v
		return nil
	}
	if v, ok := raw["Account"]; ok {
		r.account = &v
		return nil
	}
	return errors.New("ootle: TransferRecipient JSON has neither PublicKey nor Account")
}

// PublicTransferIntent is the developer-facing description of a transfer. Amounts are
// in µTari (u64). Mirrors the core's PublicTransferIntent shape.
type PublicTransferIntent struct {
	FromAccount     string            `json:"from_account"`
	Recipient       TransferRecipient `json:"recipient"`
	ResourceAddress string            `json:"resource_address"`
	Amount          uint64            `json:"amount"`
	Fee             uint64            `json:"fee"`
	Inputs          []InputRef        `json:"inputs"`
	MinEpoch        *uint64           `json:"min_epoch"`
	MaxEpoch        *uint64           `json:"max_epoch"`
	DryRun          bool              `json:"dry_run"`
}

// AsDryRun returns a dry-run copy of this intent; the receiver is left unchanged. A
// dry-run submission estimates the fee without committing (see FinalizedResult.EstimatedFeeOr).
func (i PublicTransferIntent) AsDryRun() PublicTransferIntent {
	i.DryRun = true
	return i
}

// marshalJSON serializes the intent for the C ABI. A nil Inputs is normalized to an empty
// slice: the core's inputs field is a non-defaulted Vec, so `null` is rejected.
func (i PublicTransferIntent) marshalJSON() ([]byte, error) {
	if i.Inputs == nil {
		i.Inputs = []InputRef{}
	}
	return json.Marshal(i)
}

// PublicTransferKeys is the production key bundle: only the account secret (lowercase
// hex). The seal uses a fresh random nonce, so the encoded bytes are not reproducible.
type PublicTransferKeys struct {
	AccountSecret string `json:"account_secret"`
}

// EncodedPublicTransfer is the headline output: the submit-ready BOR-encoded transaction
// bytes plus the transaction id, both lowercase hex.
type EncodedPublicTransfer struct {
	EncodedTransaction string `json:"encoded_transaction"`
	TransactionID      string `json:"transaction_id"`
}

// ABIVersion returns the ABI tag reported by the vendored native lib.
func ABIVersion() string {
	return cffi.ABIVersion()
}

// --- identity keygen (group A) ----------------------------------------------------------

// AccountKeyPair is a minted account identity: the owner secret + its public key, both lowercase
// hex (no 0x). The secret signs/authorizes/seals public transfers; the public key derives the
// account address. The secret is plaintext — the host is responsible for protecting it.
type AccountKeyPair struct {
	AccountSecret    string `json:"account_secret"`
	AccountPublicKey string `json:"account_public_key"`
}

// ViewKeyPair is a minted view (stealth-receive) identity: the view secret + its public key, both
// lowercase hex. The view secret scans inbound stealth UTXOs; the public key is shared so senders
// can address stealth outputs to this identity.
type ViewKeyPair struct {
	ViewSecret    string `json:"view_secret"`
	ViewPublicKey string `json:"view_public_key"`
}

// DeriveAddress returns the canonical account component address ("component_<hex>") for this
// keypair's public key. It owns the hex-decode → 32-byte → DeriveAccountAddress dance; a
// malformed/short public key surfaces a "KEY" *Error rather than a wrong address.
func (k AccountKeyPair) DeriveAddress() (string, error) {
	pkBytes, err := hex.DecodeString(k.AccountPublicKey)
	if err != nil || len(pkBytes) != 32 {
		return "", &Error{Code: "KEY", Message: fmt.Sprintf("bad account public key hex %q", k.AccountPublicKey)}
	}
	var pk [32]byte
	copy(pk[:], pkBytes)
	return DeriveAccountAddress(pk)
}

// TransferKeys returns the production key bundle for a public transfer (the account secret only;
// the seal draws a fresh random nonce).
func (k AccountKeyPair) TransferKeys() PublicTransferKeys {
	return PublicTransferKeys{AccountSecret: k.AccountSecret}
}

// GenerateAccountKey mints a fresh account keypair from OsRng (production). The result is
// non-reproducible by design — call it once per new identity. Errors carry the stable core code.
func GenerateAccountKey() (AccountKeyPair, error) {
	var out AccountKeyPair
	dataJSON, cerr := cffi.GenerateAccountKey()
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal account keypair: %v", err)}
	}
	return out, nil
}

// GenerateViewKey mints a fresh view keypair from OsRng (production). Non-reproducible by design.
func GenerateViewKey() (ViewKeyPair, error) {
	var out ViewKeyPair
	dataJSON, cerr := cffi.GenerateViewKey()
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal view keypair: %v", err)}
	}
	return out, nil
}

// DeriveAccountKeyFromSeed deterministically derives an account keypair from a 32-byte seed (no
// RNG; the canonical wallet KDF). The same seed reproduces the same keypair byte-for-byte. The
// seed is passed as lowercase hex; bad/odd/uppercase/wrong-length hex yields a "PARSE" error.
func DeriveAccountKeyFromSeed(seed [32]byte) (AccountKeyPair, error) {
	var out AccountKeyPair
	dataJSON, cerr := cffi.DeriveAccountKeyFromSeed(hex.EncodeToString(seed[:]))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal account keypair: %v", err)}
	}
	return out, nil
}

// DeriveViewKeyFromSeed deterministically derives a view keypair from a 32-byte seed (no RNG).
// The same seed reproduces the same keypair. Account and view keys derived from the SAME seed
// differ (distinct KDF branch labels), so one seed yields a complete identity.
func DeriveViewKeyFromSeed(seed [32]byte) (ViewKeyPair, error) {
	var out ViewKeyPair
	dataJSON, cerr := cffi.DeriveViewKeyFromSeed(hex.EncodeToString(seed[:]))
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal view keypair: %v", err)}
	}
	return out, nil
}

// --- account-address derivation (group A) -----------------------------------------------

// accountAddressResult is the wire shape of ootle_derive_account_address's data JSON.
type accountAddressResult struct {
	ComponentAddress string `json:"component_address"`
}

// DeriveAccountAddress derives the canonical account component address ("component_<hex>") from an
// account public key. It calls the same engine derivation the transfer builder uses to place a
// recipient account — the host never re-implements the hash (a wrong derivation would send funds to
// an address nobody controls). Network-independent (no network parameter).
//
// pubKey is the 32-byte account public key. The crypto stays entirely in the core; Go only marshals
// the hex in and the address string out.
func DeriveAccountAddress(pubKey [32]byte) (string, error) {
	dataJSON, cerr := cffi.DeriveAccountAddress(hex.EncodeToString(pubKey[:]))
	if cerr != nil {
		return "", fromCffiError(cerr)
	}
	var out accountAddressResult
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return "", &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal account address: %v", err)}
	}
	return out.ComponentAddress, nil
}

// --- address parse/format codec (group A) -----------------------------------------------

// AddressKind is the discriminant of a parsed address: an engine substate id ("component" /
// "resource") or an otl_… identity ("identity").
type AddressKind string

const (
	// AddressKindComponent is an engine component_<hex> substate address.
	AddressKindComponent AddressKind = "component"
	// AddressKindResource is an engine resource_<hex> substate address.
	AddressKindResource AddressKind = "resource"
	// AddressKindIdentity is an otl_… bech32m identity / payment address.
	AddressKindIdentity AddressKind = "identity"
)

// ParsedAddress is the kind-tagged result of ParseAddress, mirroring the core's ParsedAddress enum.
// For the substate kinds (component/resource) only Kind + Canonical are set; for the identity kind
// the decoded fields (Network, AccountKey, ViewOnlyKey, optional PayRef) and the canonical Bech32m
// string are set. The two keys are never swapped (the core reads them by name).
type ParsedAddress struct {
	Kind AddressKind `json:"kind"`
	// Canonical is the canonical "<prefix>_<hex>" string (component/resource kinds only).
	Canonical string `json:"canonical,omitempty"`
	// Network is the network the HRP encodes (identity kind only).
	Network Network `json:"network,omitempty"`
	// AccountKey is the account (spend) public key, lowercase hex (identity kind only).
	AccountKey string `json:"account_key,omitempty"`
	// ViewOnlyKey is the view-only public key, lowercase hex (identity kind only).
	ViewOnlyKey string `json:"view_only_key,omitempty"`
	// PayRef is the optional payment reference, lowercase hex (identity kind only; nil if absent).
	PayRef *string `json:"pay_ref,omitempty"`
	// Bech32m is the canonical otl_… string (identity kind only).
	Bech32m string `json:"bech32m,omitempty"`
}

// ParseAddress parses an address string into a kind-tagged ParsedAddress. It handles BOTH a
// component_/resource_<hex> engine substate id AND an otl_… bech32m identity / payment address —
// dispatching on prefix inside the core. The value-critical bech32m codec stays in the core; Go only
// marshals the string in and the tagged record out. An unknown
// prefix, a malformed substate id, or a bad bech32m string (checksum / HRP / length) is a "PARSE"
// error.
func ParseAddress(address string) (ParsedAddress, error) {
	var out ParsedAddress
	dataJSON, cerr := cffi.ParseAddress(address)
	if cerr != nil {
		return out, fromCffiError(cerr)
	}
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return out, &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal parsed address: %v", err)}
	}
	return out, nil
}

// identityAddressResult is the wire shape of ootle_format_identity_address's data JSON.
type identityAddressResult struct {
	Bech32m string `json:"bech32m"`
}

// FormatIdentityAddress formats an otl_… identity / payment address (bech32m) from its parts. The
// network selects the network-qualified HRP (otl_ MainNet, otl_loc_ LocalNet, otl_esm_ Esmeralda,
// …); accountKey and viewKey are the 32-byte public keys; payRef is an OPTIONAL payment reference
// (pass nil for none; over 64 bytes is a "PARSE" error). The bech32m encoding stays entirely in the
// core (the host re-implements nothing) — Go only marshals the bytes in and the string out.
//
// The two keys are passed labelled (account vs view), and the core constructs the address by field
// name, so they can never be silently swapped.
func FormatIdentityAddress(network Network, accountKey, viewKey [32]byte, payRef []byte) (string, error) {
	netByte, ok := network.ByteValue()
	if !ok {
		return "", &Error{Code: "VALIDATION", Message: fmt.Sprintf("unknown network %q", network)}
	}
	payRefHex := ""
	if payRef != nil {
		payRefHex = hex.EncodeToString(payRef)
	}
	dataJSON, cerr := cffi.FormatIdentityAddress(netByte, hex.EncodeToString(accountKey[:]), hex.EncodeToString(viewKey[:]), payRefHex)
	if cerr != nil {
		return "", fromCffiError(cerr)
	}
	var out identityAddressResult
	if err := json.Unmarshal([]byte(dataJSON), &out); err != nil {
		return "", &Error{Code: "ENCODING", Message: fmt.Sprintf("unmarshal identity address: %v", err)}
	}
	return out.Bech32m, nil
}
