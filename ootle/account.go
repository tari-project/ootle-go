package ootle

// Account is a complete account identity: an account keypair, its view keypair, and the
// derived account component address. It bundles everything the Send* and scan paths need so
// callers never hand-roll the hex-decode/derive dance or thread two keypairs by hand.
type Account struct {
	Keys    AccountKeyPair
	View    ViewKeyPair
	Address string
}

// NewAccount mints a fresh production identity: a random account keypair, a random view keypair,
// and the account component address derived from the account public key.
func NewAccount() (Account, error) {
	keys, err := GenerateAccountKey()
	if err != nil {
		return Account{}, err
	}
	view, err := GenerateViewKey()
	if err != nil {
		return Account{}, err
	}
	addr, err := keys.DeriveAddress()
	if err != nil {
		return Account{}, err
	}
	return Account{Keys: keys, View: view, Address: addr}, nil
}

// AccountFromSeed deterministically derives a complete identity from a single 32-byte seed. The
// account and view keypairs use distinct KDF branches, so one seed yields both; the same seed
// reproduces the same Account.
func AccountFromSeed(seed [32]byte) (Account, error) {
	keys, err := DeriveAccountKeyFromSeed(seed)
	if err != nil {
		return Account{}, err
	}
	view, err := DeriveViewKeyFromSeed(seed)
	if err != nil {
		return Account{}, err
	}
	addr, err := keys.DeriveAddress()
	if err != nil {
		return Account{}, err
	}
	return Account{Keys: keys, View: view, Address: addr}, nil
}

// TransferKeys returns the production key bundle for a public transfer.
func (a Account) TransferKeys() PublicTransferKeys {
	return a.Keys.TransferKeys()
}

// StealthKeys returns the production key bundle for a stealth send: the account secret only (the
// production path draws a fresh random build seed internally).
func (a Account) StealthKeys() StealthProductionKeys {
	return StealthProductionKeys{AccountSecret: a.Keys.AccountSecret}
}

// ScanKeys returns the scan bundle for receiving stealth outputs addressed to this account. It
// sets AccountSecret as well as ViewSecret, enabling the scanner's ownership checks (spend
// condition + UTXO tag). A view-only caller who wants a decrypt-only scan can build
// StealthScanKeys{ViewSecret: ...} directly.
func (a Account) ScanKeys() StealthScanKeys {
	secret := a.Keys.AccountSecret
	return StealthScanKeys{ViewSecret: a.View.ViewSecret, AccountSecret: &secret}
}
