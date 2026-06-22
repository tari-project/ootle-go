package ootle

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
)

// This file is the generic transaction intent + a typed-arg DSL that mirror the core's
// GenericTransactionIntent / InstructionSpec / ArgValue, plus ergonomic builders (Faucet,
// PublishTemplate, CallFunction, CallMethod) that only compose InstructionSpec values.
//
// No instruction/arg encoding lives here. The builders assemble an InstructionSpec list; the
// value-critical lowering + CBOR/arg encoding stays in the core, reached over the single
// ootle_build_unsigned_instructions entry point. A new builtin template means a new Go builder, never
// a new FFI fn.

// ArgValue is one typed argument, mirroring the core's ArgValue DSL. Exactly one field is set; the core
// encodes it onto the engine's InstructionArg. It marshals to the core's externally-tagged enum form,
// e.g. {"Amount": 1000} or {"Workspace": "bucket"}.
//
// Use the Arg* constructors rather than setting fields directly.
type ArgValue struct {
	amount        *uint64
	address       *string
	workspace     *string
	str           *string
	boolean       *bool
	u64           *uint64
	bytes         []byte // non-nil (incl. empty) ⇒ a Bytes arg
	bytesSet      bool
	metadata      map[string]string // non-nil ⇒ a Metadata arg
	i64           *int64
	nonFungibleID *string
	list          []ArgValue // a List arg; see listSet for the empty case
	listSet       bool
	optional      *ArgValue // an Optional payload; nil with optionalSet ⇒ None
	optionalSet   bool
}

// ArgAmount is a µTari token amount (encoded by the core as the engine Amount).
func ArgAmount(microTari uint64) ArgValue { return ArgValue{amount: &microTari} }

// ArgAddress is a canonical substate address string — any of the engine's address kinds: component_,
// resource_, vault_, nft_, template_, txreceipt_, vnfp_, utxo_, or tombstone_. The core parses it and
// encodes the inner typed address; a malformed address surfaces as a "PARSE" error at build time.
func ArgAddress(address string) ArgValue { return ArgValue{address: &address} }

// ArgWorkspace references a value previously put on the workspace by its label (see
// PutLastInstructionOutputOnWorkspace). The numeric workspace id is resolved by the core at the call
// point — never host-side.
func ArgWorkspace(label string) ArgValue { return ArgValue{workspace: &label} }

// ArgString is a UTF-8 string literal.
func ArgString(s string) ArgValue { return ArgValue{str: &s} }

// ArgBool is a boolean literal.
func ArgBool(b bool) ArgValue { return ArgValue{boolean: &b} }

// ArgU64 is a u64 literal (stays native u64 end-to-end; never a float).
func ArgU64(n uint64) ArgValue { return ArgValue{u64: &n} }

// ArgI64 is a signed integer literal. Covers i8..i64 by the core's minimal encoding; negatives encode as
// a CBOR negative integer.
func ArgI64(n int64) ArgValue { return ArgValue{i64: &n} }

// ArgBytes is a raw bytes literal (marshalled to the core as lowercase hex).
func ArgBytes(b []byte) ArgValue {
	if b == nil {
		b = []byte{}
	}
	return ArgValue{bytes: b, bytesSet: true}
}

// ArgMetadata is a string→string metadata map, encoded by the core as the engine Metadata
// (a CBOR-tagged map). Templates taking a Metadata parameter (resource builders,
// stable_coin::instantiate) expect this. A nil map encodes as an empty Metadata.
func ArgMetadata(m map[string]string) ArgValue {
	if m == nil {
		m = map[string]string{}
	}
	return ArgValue{metadata: m}
}

// ArgNonFungibleID is a non-fungible id value in canonical string form: "uuid_<64-hex>", "str_<text>",
// "u32_<n>", or "u64_<n>". This is the id alone — an NFT address (resource + id) is a normal
// ArgAddress("nft_…"). The core validates the string; a malformed id surfaces as "PARSE" at build time.
// Use the NonFungible* helpers to build the string from a native value.
func ArgNonFungibleID(canonical string) ArgValue { return ArgValue{nonFungibleID: &canonical} }

// NonFungibleU32, NonFungibleU64, NonFungibleString, and NonFungibleUUID build the canonical id string
// passed to ArgNonFungibleID. NonFungibleUUID takes the engine's 32-byte id (rendered as 64 hex chars),
// not a 16-byte RFC-4122 UUID.
func NonFungibleU32(n uint32) string    { return "u32_" + strconv.FormatUint(uint64(n), 10) }
func NonFungibleU64(n uint64) string    { return "u64_" + strconv.FormatUint(n, 10) }
func NonFungibleString(s string) string { return "str_" + s }
func NonFungibleUUID(b [32]byte) string { return "uuid_" + hex.EncodeToString(b[:]) }

// ArgList is a list argument, encoded by the core as an array of the lowered elements. Elements may be
// any ArgValue, including nested ArgList or ArgSome. ArgList() is a valid empty list (never null). A
// nested ArgWorkspace is rejected by the core with "VALIDATION" (workspace ids resolve only at the top
// level).
func ArgList(items ...ArgValue) ArgValue {
	if items == nil {
		items = []ArgValue{}
	}
	return ArgValue{list: items, listSet: true}
}

// ArgSome is a present optional value. Pair with ArgNone for the absent case.
func ArgSome(inner ArgValue) ArgValue { return ArgValue{optional: &inner, optionalSet: true} }

// ArgNone is an absent optional value (encoded as null).
func ArgNone() ArgValue { return ArgValue{optionalSet: true} }

// MarshalJSON emits the core's externally-tagged ArgValue enum. Bytes marshal to lowercase hex (the core
// rejects uppercase), matching the byte newtypes' discipline.
func (a ArgValue) MarshalJSON() ([]byte, error) {
	switch {
	case a.amount != nil:
		return json.Marshal(map[string]uint64{"Amount": *a.amount})
	case a.address != nil:
		return json.Marshal(map[string]string{"Address": *a.address})
	case a.workspace != nil:
		return json.Marshal(map[string]string{"Workspace": *a.workspace})
	case a.str != nil:
		return json.Marshal(map[string]string{"String": *a.str})
	case a.boolean != nil:
		return json.Marshal(map[string]bool{"Bool": *a.boolean})
	case a.u64 != nil:
		return json.Marshal(map[string]uint64{"U64": *a.u64})
	case a.i64 != nil:
		return json.Marshal(map[string]int64{"I64": *a.i64})
	case a.nonFungibleID != nil:
		return json.Marshal(map[string]string{"NonFungibleId": *a.nonFungibleID})
	case a.listSet:
		return json.Marshal(map[string][]ArgValue{"List": nonNilArgs(a.list)})
	case a.optionalSet:
		// A nil payload is None ⇒ {"Optional":null}; a non-nil payload re-enters MarshalJSON.
		return json.Marshal(map[string]*ArgValue{"Optional": a.optional})
	case a.bytesSet:
		return json.Marshal(map[string]string{"Bytes": hex.EncodeToString(a.bytes)})
	case a.metadata != nil:
		return json.Marshal(map[string]map[string]string{"Metadata": a.metadata})
	default:
		return nil, errors.New("ootle: ArgValue has no variant set (use an Arg* constructor)")
	}
}

// ComponentRef is a reference to the component a CallMethod targets, mirroring the core's externally
// tagged ComponentRef: either an on-ledger address ({"Address": "component_<hex>"}) or a label bound to a
// component on the workspace ({"Workspace": "<label>"}). Use ComponentAtAddress / ComponentOnWorkspace.
type ComponentRef struct {
	address   *string
	workspace *string
}

// ComponentAtAddress references an on-ledger component by address (component_<hex>). An address-targeted
// CallMethod derives an all_component_vaults want for that component.
func ComponentAtAddress(address string) ComponentRef { return ComponentRef{address: &address} }

// ComponentOnWorkspace references a component produced earlier in the same phase and bound to the
// workspace under label (see PutLastInstructionOutputOnWorkspace). A workspace-targeted CallMethod
// derives no want (nothing on-ledger to fetch). The label must be bound in the same phase.
func ComponentOnWorkspace(label string) ComponentRef { return ComponentRef{workspace: &label} }

// MarshalJSON emits the core's externally-tagged ComponentRef enum.
func (r ComponentRef) MarshalJSON() ([]byte, error) {
	switch {
	case r.address != nil:
		return json.Marshal(map[string]string{"Address": *r.address})
	case r.workspace != nil:
		return json.Marshal(map[string]string{"Workspace": *r.workspace})
	default:
		return nil, errors.New("ootle: ComponentRef has no variant set (use ComponentAtAddress or ComponentOnWorkspace)")
	}
}

// OwnerRuleSpec is the optional owner rule for a created account, mirroring the core's reduced, closed
// OwnerRuleSpec set: "OwnedBySigner" (the engine default), "None", or {"ByPublicKey": "<hex>"}. The zero
// value is unset (marshals to an error); use the Owner* constructors.
type OwnerRuleSpec struct {
	kind        ownerRuleKind
	byPublicKey *string
}

type ownerRuleKind uint8

const (
	ownerRuleUnset ownerRuleKind = iota
	ownerRuleOwnedBySigner
	ownerRuleNone
	ownerRuleByPublicKey
)

// OwnerOwnedBySigner is the engine-default owner rule (owned by the transaction signer).
func OwnerOwnedBySigner() OwnerRuleSpec { return OwnerRuleSpec{kind: ownerRuleOwnedBySigner} }

// OwnerNone is the no-owner rule.
func OwnerNone() OwnerRuleSpec { return OwnerRuleSpec{kind: ownerRuleNone} }

// OwnerByPublicKey owns the account by a specific public key (lowercase hex).
func OwnerByPublicKey(publicKeyHex string) OwnerRuleSpec {
	return OwnerRuleSpec{kind: ownerRuleByPublicKey, byPublicKey: &publicKeyHex}
}

// MarshalJSON emits the core's OwnerRuleSpec enum: the unit variants as bare strings, ByPublicKey as a
// tagged object.
func (o OwnerRuleSpec) MarshalJSON() ([]byte, error) {
	switch o.kind {
	case ownerRuleOwnedBySigner:
		return json.Marshal("OwnedBySigner")
	case ownerRuleNone:
		return json.Marshal("None")
	case ownerRuleByPublicKey:
		return json.Marshal(map[string]string{"ByPublicKey": *o.byPublicKey})
	default:
		return nil, errors.New("ootle: OwnerRuleSpec has no variant set (use an Owner* constructor)")
	}
}

// InstructionSpec is one boundary instruction, mirroring the core's 5-variant InstructionSpec. Exactly
// one field is set; it marshals to the core's externally-tagged enum form. Use the instruction
// constructors (CallFunction, CallMethod, CreateAccount, PublishTemplate,
// PutLastInstructionOutputOnWorkspace) rather than setting fields directly.
type InstructionSpec struct {
	callFunction    *callFunction
	callMethod      *callMethod
	createAccount   *createAccount
	publishTemplate *publishTemplate
	putWorkspace    *putWorkspace
}

type callFunction struct {
	TemplateAddress string     `json:"template_address"`
	Function        string     `json:"function"`
	Args            []ArgValue `json:"args"`
}

type callMethod struct {
	Call   ComponentRef `json:"call"`
	Method string       `json:"method"`
	Args   []ArgValue   `json:"args"`
}

type createAccount struct {
	OwnerPublicKey    string         `json:"owner_public_key"`
	OwnerRule         *OwnerRuleSpec `json:"owner_rule"`          // nil ⇒ null (engine default)
	BucketWorkspaceID *string        `json:"bucket_workspace_id"` // nil ⇒ null (no bucket)
}

type publishTemplate struct {
	BlobIndex    uint32  `json:"blob_index"`
	MetadataHash *string `json:"metadata_hash"` // nil ⇒ null; otherwise lowercase hex
}

type putWorkspace struct {
	Key string `json:"key"`
}

// CallFunction builds a CallFunction instruction (template_address::function(args)).
func CallFunction(templateAddress, function string, args ...ArgValue) InstructionSpec {
	return InstructionSpec{callFunction: &callFunction{TemplateAddress: templateAddress, Function: function, Args: nonNilArgs(args)}}
}

// CallMethod builds a CallMethod instruction against an on-ledger component address
// (component_address.method(args)). For a component produced on the workspace, use CallMethodOnWorkspace.
func CallMethod(componentAddress, method string, args ...ArgValue) InstructionSpec {
	return InstructionSpec{callMethod: &callMethod{Call: ComponentAtAddress(componentAddress), Method: method, Args: nonNilArgs(args)}}
}

// CallMethodOnWorkspace builds a CallMethod against a component bound to the workspace under label (see
// PutLastInstructionOutputOnWorkspace), in the same phase. It derives no want (nothing on-ledger to fetch).
func CallMethodOnWorkspace(label, method string, args ...ArgValue) InstructionSpec {
	return InstructionSpec{callMethod: &callMethod{Call: ComponentOnWorkspace(label), Method: method, Args: nonNilArgs(args)}}
}

// CreateAccountOption configures the optional fields of a CreateAccount instruction (owner rule, deposit
// bucket). Pass them to CreateAccount; omit for the engine defaults (owner_rule = null, no bucket).
type CreateAccountOption func(*createAccount)

// WithOwnerRule sets the created account's owner rule (default: null ⇒ engine default).
func WithOwnerRule(rule OwnerRuleSpec) CreateAccountOption {
	return func(c *createAccount) { c.OwnerRule = &rule }
}

// WithBucket deposits the workspace bucket bound under label into the account on creation. The label must
// be bound by a PutLastInstructionOutputOnWorkspace in the same phase.
func WithBucket(workspaceLabel string) CreateAccountOption {
	return func(c *createAccount) { c.BucketWorkspaceID = &workspaceLabel }
}

// CreateAccount builds an idempotent create-or-fetch of an account component for ownerPublicKey
// (lowercase-hex 32-byte account public key). With no options it lowers to the engine-default,
// no-bucket Instruction::CreateAccount. Use WithOwnerRule / WithBucket for the optional surface.
func CreateAccount(ownerPublicKeyHex string, opts ...CreateAccountOption) InstructionSpec {
	ca := &createAccount{OwnerPublicKey: ownerPublicKeyHex}
	for _, opt := range opts {
		opt(ca)
	}
	return InstructionSpec{createAccount: ca}
}

// PublishTemplateOption configures the optional fields of a PublishTemplate instruction.
type PublishTemplateOption func(*publishTemplate)

// WithMetadataHash sets the published template's metadata hash. It is marshalled to lowercase hex (the
// core rejects uppercase). Omit for null.
func WithMetadataHash(hash []byte) PublishTemplateOption {
	return func(p *publishTemplate) {
		h := hex.EncodeToString(hash)
		p.MetadataHash = &h
	}
}

// PublishTemplate builds a PublishTemplate instruction referencing the blob at blobIndex in the intent's
// Blobs list. Use WithMetadataHash for the optional metadata hash (default: null).
func PublishTemplate(blobIndex uint32, opts ...PublishTemplateOption) InstructionSpec {
	pt := &publishTemplate{BlobIndex: blobIndex}
	for _, opt := range opts {
		opt(pt)
	}
	return InstructionSpec{publishTemplate: pt}
}

// PutLastInstructionOutputOnWorkspace builds the instruction that stores the previous instruction's
// output on the workspace under key (later referenced by ArgWorkspace(key)).
func PutLastInstructionOutputOnWorkspace(key string) InstructionSpec {
	return InstructionSpec{putWorkspace: &putWorkspace{Key: key}}
}

// nonNilArgs ensures the args slice marshals to a JSON array ([]), never null.
func nonNilArgs(args []ArgValue) []ArgValue {
	if args == nil {
		return []ArgValue{}
	}
	return args
}

// MarshalJSON emits the core's externally-tagged InstructionSpec enum. Args slices are normalised to
// `[]` (never `null`) as a second line of defence — the core's serde rejects a null `args` array, and
// the constructors already route through nonNilArgs, but a directly-built variant could carry nil.
func (i InstructionSpec) MarshalJSON() ([]byte, error) {
	switch {
	case i.callFunction != nil:
		i.callFunction.Args = nonNilArgs(i.callFunction.Args)
		return json.Marshal(map[string]*callFunction{"CallFunction": i.callFunction})
	case i.callMethod != nil:
		i.callMethod.Args = nonNilArgs(i.callMethod.Args)
		return json.Marshal(map[string]*callMethod{"CallMethod": i.callMethod})
	case i.createAccount != nil:
		return json.Marshal(map[string]*createAccount{"CreateAccount": i.createAccount})
	case i.publishTemplate != nil:
		return json.Marshal(map[string]*publishTemplate{"PublishTemplate": i.publishTemplate})
	case i.putWorkspace != nil:
		return json.Marshal(map[string]*putWorkspace{"PutLastInstructionOutputOnWorkspace": i.putWorkspace})
	default:
		return nil, errors.New("ootle: InstructionSpec has no variant set (use an instruction constructor)")
	}
}

// BlobSpec is a transaction blob (e.g. the WASM binary for PublishTemplate), referenced by index from a
// PublishTemplate instruction. The bytes marshal to lowercase hex.
type BlobSpec struct {
	bytes []byte
}

// NewBlob wraps raw blob bytes.
func NewBlob(b []byte) BlobSpec {
	if b == nil {
		b = []byte{}
	}
	return BlobSpec{bytes: b}
}

// MarshalJSON emits {"bytes":"<lowercase-hex>"} (the core rejects uppercase hex).
func (b BlobSpec) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"bytes": hex.EncodeToString(b.bytes)})
}

// FeeSource is how a transaction's fee is paid, mirroring the core's externally tagged FeeSource. Exactly
// one variant is set. A self-funding fee (a payer created in the same transaction) must create + fund
// that payer in FeeInstructions. Use the Fee* constructors.
type FeeSource struct {
	fromAccount            *string
	fromWorkspaceComponent *string
	fromBucket             *string
}

// FeeFromAccount pays the fee from an existing on-ledger account (component_<hex>). It derives a
// required TARI vault want for that account — the account must already exist on-ledger.
func FeeFromAccount(component string) FeeSource { return FeeSource{fromAccount: &component} }

// FeeFromWorkspaceComponent pays the fee from a component bound to the workspace under label during the
// fee phase (the self-funding pattern). It derives no fee vault want; label must be bound by a
// PutLastInstructionOutputOnWorkspace in FeeInstructions.
func FeeFromWorkspaceComponent(label string) FeeSource {
	return FeeSource{fromWorkspaceComponent: &label}
}

// FeeFromBucket pays the fee from a workspace bucket bound under label during the fee phase
// (PayFeeFromBucket; no refund). It derives no fee vault want; label must be bound in FeeInstructions.
func FeeFromBucket(label string) FeeSource { return FeeSource{fromBucket: &label} }

// MarshalJSON emits the core's externally-tagged FeeSource enum.
func (f FeeSource) MarshalJSON() ([]byte, error) {
	switch {
	case f.fromAccount != nil:
		return json.Marshal(map[string]string{"FromAccount": *f.fromAccount})
	case f.fromWorkspaceComponent != nil:
		return json.Marshal(map[string]map[string]string{"FromWorkspaceComponent": {"label": *f.fromWorkspaceComponent}})
	case f.fromBucket != nil:
		return json.Marshal(map[string]map[string]string{"FromBucket": {"label": *f.fromBucket}})
	default:
		return nil, errors.New("ootle: FeeSource has no variant set (use a Fee* constructor; set GenericTransactionIntent.FeePayment)")
	}
}

// GenericTransactionIntent is the developer-facing generic intent: a fee source + a fee-phase and a
// main-phase instruction list + typed args + blobs + inputs. It mirrors the core's
// GenericTransactionIntent shape. Amounts are µTari (u64). Build it directly, or with the host builders
// (Faucet, etc.) which compose the instruction lists and fee source.
type GenericTransactionIntent struct {
	// Fee is the fee to pay, in µTari.
	Fee uint64 `json:"fee"`
	// FeePayment is the fee source (required). Set it via a Fee* constructor.
	FeePayment FeeSource `json:"fee_payment"`
	// FeeInstructions is the fee-phase instruction list, run before the fee is paid. Use it to create +
	// fund a self-funding payer. Empty ([]) when not self-funding.
	FeeInstructions []InstructionSpec `json:"fee_instructions"`
	// Instructions is the ordered main-phase instruction list.
	Instructions []InstructionSpec `json:"instructions"`
	// Blobs are transaction blobs (e.g. WASM binaries), referenced by index from PublishTemplate.
	Blobs []BlobSpec `json:"blobs"`
	// Inputs is the explicit input set. Empty ⇒ want-list resolution (the two-phase path). A non-empty
	// Inputs short-circuits derivation entirely.
	Inputs []InputRef `json:"inputs"`
	// ExtraInputs are caller-pinned requirements always MERGED with the resolved set (deduped by id),
	// unlike Inputs which selects the explicit-only path. Use for inputs a template references
	// internally that the instruction list cannot reveal.
	ExtraInputs []InputRef `json:"extra_inputs"`
	// MinEpoch is the optional earliest epoch this transaction is valid in.
	MinEpoch *uint64 `json:"min_epoch"`
	// MaxEpoch is the optional latest epoch this transaction is valid in.
	MaxEpoch *uint64 `json:"max_epoch"`
	// DryRun marks this as a dry run.
	DryRun bool `json:"dry_run"`

	// faucetClaim, when set by Faucet().Take(), routes this intent through the core's builtin faucet
	// builder instead of the generic instruction builder. Unexported (never serialized); the generic
	// fields above are ignored on that path. See sendInstructions.
	faucetClaim *FaucetClaimIntent
}

// AsDryRun returns a dry-run copy of this generic intent; the receiver is left unchanged.
// It sets the generic intent's DryRun flag. The faucet-claim path carries its own flag via
// FaucetBuilder and is not toggled here, but the claim pointer is copied so the returned
// intent never aliases the receiver's.
func (g GenericTransactionIntent) AsDryRun() GenericTransactionIntent {
	g.DryRun = true
	if g.faucetClaim != nil {
		claim := *g.faucetClaim
		g.faucetClaim = &claim
	}
	return g
}

// FaucetClaimIntent is the intent for a self-funding network-faucet claim, mirroring the core's
// FaucetClaimIntent. The core builds the complete claim (create the recipient account, fund it from the
// faucet, pay the fee from it) and owns the faucet's full input set, so the host pins no faucet address.
type FaucetClaimIntent struct {
	// RecipientPublicKey is the claiming account's owner public key (lowercase-hex 32 bytes).
	RecipientPublicKey string `json:"recipient_public_key"`
	// Fee is the fee to pay, in µTari.
	Fee uint64 `json:"fee"`
	// MinEpoch is the optional earliest epoch this transaction is valid in.
	MinEpoch *uint64 `json:"min_epoch"`
	// MaxEpoch is the optional latest epoch this transaction is valid in.
	MaxEpoch *uint64 `json:"max_epoch"`
	// DryRun marks this as a dry run (e.g. for fee estimation).
	DryRun bool `json:"dry_run"`
}

// marshalIntent renders the intent to the core's JSON, normalising nil slices to [] (the core rejects
// `null` for fee_instructions/instructions/blobs/inputs). An unset FeePayment surfaces as a marshal
// error (FeeSource requires a variant).
func (g GenericTransactionIntent) marshalIntent() ([]byte, error) {
	out := g
	if out.FeeInstructions == nil {
		out.FeeInstructions = []InstructionSpec{}
	}
	if out.Instructions == nil {
		out.Instructions = []InstructionSpec{}
	}
	if out.Blobs == nil {
		out.Blobs = []BlobSpec{}
	}
	if out.Inputs == nil {
		out.Inputs = []InputRef{}
	}
	if out.ExtraInputs == nil {
		out.ExtraInputs = []InputRef{}
	}
	// out's json tags already match the core's wire shape, so marshal it directly. (out is a
	// value copy, so the nil-slice normalisation above does not mutate the caller's intent.)
	return json.Marshal(out)
}

// --- Host-side ergonomic builders (not in the core) --------------------------------------------------
//
// These compose InstructionSpec values only. They name no encoding and touch no bytes — the core owns
// all of that behind ootle_build_unsigned_instructions.

// FaucetBuilder is the fluent constructor for a faucet claim.
//
//   - Take(account): a self-funding claim from the network faucet, built by the core. The faucetComponent
//     passed to Faucet() is ignored on this path — the core owns the faucet's addresses. Call Intent(fee)
//     for the assembled intent, then drive it with SendInstructions.
//   - TakeFreeCoins(...).Deposit(account): a test template's `take_free_coins() -> Bucket`, against the
//     faucetComponent passed to Faucet(). Assumes a pre-existing fee account — use Instructions() and set
//     FeePayment (e.g. FeeFromAccount) yourself.
type FaucetBuilder struct {
	faucetComponent string
	instructions    []InstructionSpec
	claim           *FaucetClaimIntent
}

// XtrFaucetComponentAddress is the canonical testnet TARI faucet component address. Pass it to Faucet()
// to claim from the deployed faucet.
const XtrFaucetComponentAddress = "component_0102030000000000000000000000000000000000000000000000000000000000"

// XtrFaucetAmount is the fixed amount the deployed faucet dispenses per claim: 1,000 TARI in µTari.
const XtrFaucetAmount uint64 = 1_000 * 1_000_000

// Faucet starts a faucet-claim builder. faucetComponent is used only by the TakeFreeCoins/Deposit
// (test-template) path; Take ignores it (the core owns the network faucet's addresses).
func Faucet(faucetComponent string) *FaucetBuilder {
	return &FaucetBuilder{faucetComponent: faucetComponent}
}

// Take marks a self-funding claim from the network faucet for ownerPublicKeyHex (the recipient's owner
// public key, lowercase hex). The core builds the complete claim and owns the faucet's input set. Call
// Intent(fee) for the assembled intent.
func (f *FaucetBuilder) Take(ownerPublicKeyHex string) *FaucetBuilder {
	f.claim = &FaucetClaimIntent{RecipientPublicKey: ownerPublicKeyHex}
	return f
}

// TakeFreeCoins claims from the test faucet template's `take_free_coins() -> Bucket` (no args) and puts
// the returned bucket on the workspace under bucketLabel, ready for Deposit. Use this only against a
// faucet whose deployed method is `take_free_coins`; the deployed testnet faucet uses Take(account)
// instead.
func (f *FaucetBuilder) TakeFreeCoins(bucketLabel string) *FaucetBuilder {
	f.instructions = append(f.instructions,
		CallMethod(f.faucetComponent, "take_free_coins"),
		PutLastInstructionOutputOnWorkspace(bucketLabel),
	)
	return f
}

// Deposit deposits a previously workspace-bound bucket into the account component. Pair it with
// TakeFreeCoins (the bucket label must match). Not needed after Take (the deployed faucet self-deposits).
func (f *FaucetBuilder) Deposit(accountComponent, bucketLabel string) *FaucetBuilder {
	f.instructions = append(f.instructions,
		CallMethod(accountComponent, "deposit", ArgWorkspace(bucketLabel)),
	)
	return f
}

// Instructions returns the composed main-phase instruction sequence (TakeFreeCoins/Deposit). For the
// self-funding Take path, use Intent.
func (f *FaucetBuilder) Instructions() []InstructionSpec {
	return f.instructions
}

// Intent assembles the intent for a Take claim (fee in µTari). It requires a preceding Take; the returned
// intent routes through the core faucet builder when sent via SendInstructions. For the
// TakeFreeCoins/Deposit path use Instructions instead.
func (f *FaucetBuilder) Intent(fee uint64) GenericTransactionIntent {
	if f.claim == nil {
		// No Take(): an intent with no fee source, which fails loudly on send rather than silently
		// building an empty transaction.
		return GenericTransactionIntent{Fee: fee}
	}
	claim := *f.claim
	claim.Fee = fee
	// The core reads the fee from the claim; the outer Fee is unused on this path.
	return GenericTransactionIntent{faucetClaim: &claim}
}
