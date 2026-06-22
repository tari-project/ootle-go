package ootle

// TransferBuilder assembles a PublicTransferIntent through a fluent chain instead of a bare
// struct literal. It leaves Inputs empty so the two-phase resolver derives them — the
// builder never asks the caller to assemble explicit inputs.
type TransferBuilder struct {
	intent PublicTransferIntent
}

// NewTransfer starts a public-transfer builder funded from fromAccount (component_<hex>).
func NewTransfer(fromAccount string) *TransferBuilder {
	return &TransferBuilder{intent: PublicTransferIntent{FromAccount: fromAccount}}
}

// ToPublicKey sends to a lowercase-hex account public key (the account is created on receipt
// if it does not yet exist).
func (b *TransferBuilder) ToPublicKey(hexPublicKey string) *TransferBuilder {
	b.intent.Recipient = PublicKeyRecipient(hexPublicKey)
	return b
}

// ToAccount sends to an existing account component address (component_<hex>).
func (b *TransferBuilder) ToAccount(componentAddress string) *TransferBuilder {
	b.intent.Recipient = AccountRecipient(componentAddress)
	return b
}

// Resource sets the resource being transferred (resource_<hex>).
func (b *TransferBuilder) Resource(address string) *TransferBuilder {
	b.intent.ResourceAddress = address
	return b
}

// Amount sets the amount to transfer, in µTari.
func (b *TransferBuilder) Amount(microTari uint64) *TransferBuilder {
	b.intent.Amount = microTari
	return b
}

// Fee sets the fee to pay, in µTari.
func (b *TransferBuilder) Fee(microTari uint64) *TransferBuilder {
	b.intent.Fee = microTari
	return b
}

// MinEpoch sets the earliest epoch this transfer is valid in.
func (b *TransferBuilder) MinEpoch(epoch uint64) *TransferBuilder {
	b.intent.MinEpoch = &epoch
	return b
}

// MaxEpoch sets the latest epoch this transfer is valid in.
func (b *TransferBuilder) MaxEpoch(epoch uint64) *TransferBuilder {
	b.intent.MaxEpoch = &epoch
	return b
}

// DryRun marks the transfer as a dry run (fee estimation without committing).
func (b *TransferBuilder) DryRun() *TransferBuilder {
	b.intent.DryRun = true
	return b
}

// Intent returns the assembled PublicTransferIntent. The caller may still adjust it (e.g.
// .AsDryRun()) before sending.
func (b *TransferBuilder) Intent() PublicTransferIntent {
	return b.intent
}
