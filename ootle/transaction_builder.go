package ootle

// TransactionBuilder assembles a GenericTransactionIntent through a fluent chain instead of a
// bare struct literal. It chains the existing instruction / arg / fee-source constructors and
// leaves Inputs empty so the two-phase resolver derives them — the builder never exposes an
// Inputs setter (a non-empty Inputs selects the explicit-only path and defeats resolution).
// ExtraInput is the sanctioned escape hatch for inputs the instruction list cannot reveal.
//
// The builtin faucet path is not part of this builder; use FaucetBuilder for that.
type TransactionBuilder struct {
	intent GenericTransactionIntent
}

// NewTransaction starts a generic-transaction builder. Set a fee source with one of the
// PayFeeFrom* methods before sending (the core requires it).
func NewTransaction() *TransactionBuilder {
	return &TransactionBuilder{}
}

// PayFeeFromAccount pays the fee from an existing on-ledger account (component_<hex>),
// setting both the fee amount (µTari) and the fee source together.
func (b *TransactionBuilder) PayFeeFromAccount(component string, fee uint64) *TransactionBuilder {
	b.intent.Fee = fee
	b.intent.FeePayment = FeeFromAccount(component)
	return b
}

// PayFeeFromWorkspaceComponent pays the fee from a component bound to the workspace under
// label during the fee phase (the self-funding pattern), setting fee + source together.
func (b *TransactionBuilder) PayFeeFromWorkspaceComponent(label string, fee uint64) *TransactionBuilder {
	b.intent.Fee = fee
	b.intent.FeePayment = FeeFromWorkspaceComponent(label)
	return b
}

// PayFeeFromBucket pays the fee from a workspace bucket bound under label during the fee
// phase, setting fee + source together.
func (b *TransactionBuilder) PayFeeFromBucket(label string, fee uint64) *TransactionBuilder {
	b.intent.Fee = fee
	b.intent.FeePayment = FeeFromBucket(label)
	return b
}

// FeeInstruction appends one or more instructions to the fee-phase list (run before the fee
// is paid; used to create + fund a self-funding payer).
func (b *TransactionBuilder) FeeInstruction(spec ...InstructionSpec) *TransactionBuilder {
	b.intent.FeeInstructions = append(b.intent.FeeInstructions, spec...)
	return b
}

// CallMethod appends a CallMethod against an on-ledger component address.
func (b *TransactionBuilder) CallMethod(component, method string, args ...ArgValue) *TransactionBuilder {
	return b.Add(CallMethod(component, method, args...))
}

// CallMethodOnWorkspace appends a CallMethod against a component bound to the workspace under
// label in the same phase.
func (b *TransactionBuilder) CallMethodOnWorkspace(label, method string, args ...ArgValue) *TransactionBuilder {
	return b.Add(CallMethodOnWorkspace(label, method, args...))
}

// CallFunction appends a CallFunction (template_address::function(args)).
func (b *TransactionBuilder) CallFunction(template, fn string, args ...ArgValue) *TransactionBuilder {
	return b.Add(CallFunction(template, fn, args...))
}

// CreateAccount appends a create-or-fetch of an account component for ownerPublicKeyHex
// (lowercase hex). Use WithOwnerRule / WithBucket for the optional surface.
func (b *TransactionBuilder) CreateAccount(ownerPublicKeyHex string, opts ...CreateAccountOption) *TransactionBuilder {
	return b.Add(CreateAccount(ownerPublicKeyHex, opts...))
}

// PublishTemplate appends a PublishTemplate referencing the blob at blobIndex in the intent's
// Blobs list. Use WithMetadataHash for the optional metadata hash.
func (b *TransactionBuilder) PublishTemplate(blobIndex uint32, opts ...PublishTemplateOption) *TransactionBuilder {
	return b.Add(PublishTemplate(blobIndex, opts...))
}

// SaveOutput appends a PutLastInstructionOutputOnWorkspace storing the previous
// instruction's output under key (later referenced by ArgWorkspace(key)).
func (b *TransactionBuilder) SaveOutput(key string) *TransactionBuilder {
	return b.Add(PutLastInstructionOutputOnWorkspace(key))
}

// Add appends one or more raw instruction specs to the main-phase list.
func (b *TransactionBuilder) Add(spec ...InstructionSpec) *TransactionBuilder {
	b.intent.Instructions = append(b.intent.Instructions, spec...)
	return b
}

// ExtraInput appends caller-pinned inputs always merged with the resolved set (deduped by
// id). Use for inputs a template references internally that the instruction list cannot
// reveal. This is not the explicit-only Inputs path.
func (b *TransactionBuilder) ExtraInput(ref ...InputRef) *TransactionBuilder {
	b.intent.ExtraInputs = append(b.intent.ExtraInputs, ref...)
	return b
}

// Blob appends one or more transaction blobs (e.g. WASM binaries), referenced by index from a
// PublishTemplate instruction.
func (b *TransactionBuilder) Blob(blob ...BlobSpec) *TransactionBuilder {
	b.intent.Blobs = append(b.intent.Blobs, blob...)
	return b
}

// MinEpoch sets the earliest epoch this transaction is valid in.
func (b *TransactionBuilder) MinEpoch(epoch uint64) *TransactionBuilder {
	b.intent.MinEpoch = &epoch
	return b
}

// MaxEpoch sets the latest epoch this transaction is valid in.
func (b *TransactionBuilder) MaxEpoch(epoch uint64) *TransactionBuilder {
	b.intent.MaxEpoch = &epoch
	return b
}

// DryRun marks the transaction as a dry run (fee estimation without committing).
func (b *TransactionBuilder) DryRun() *TransactionBuilder {
	b.intent.DryRun = true
	return b
}

// Intent returns the assembled GenericTransactionIntent. The caller may still adjust it (e.g.
// .AsDryRun()) before sending.
func (b *TransactionBuilder) Intent() GenericTransactionIntent {
	return b.intent
}
