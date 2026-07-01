package ootle

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/tari-project/ootle-go/internal/cffi"
	"github.com/tari-project/ootle-go/transport"
)

// Golden-vector runner.
//
// The fixtures under testdata/fixtures/ are a byte-identical, checked-in copy of the
// core's fixtures — the single source of truth. This file:
//
//   - vendoredFixturesDir / loadGoldenFixtures walk that tree and decode each fixture;
//   - TestGoldenVectors runs every vector through the C ABI and asserts byte-for-byte
//     hex parity (encode ops) or canonicalized-structure parity (the parse op);
//   - TestGoldenVectors_UnknownOperationFails makes an unknown operation a hard failure,
//     so a new core vector can't silently go unchecked;
//   - TestGoldenVectors_CoverageParity asserts every operation present in the vendored
//     tree has a runner arm;
//   - TestFixtureDrift asserts the vendored copy is byte-identical to the source.

// Operation ids, matching the core's fixtures.
const (
	opBuildAndEncodePublicTransfer   = "build_and_encode_public_transfer"
	opResolveAndEncodePublicTransfer = "resolve_and_encode_public_transfer"
	opParseFinalizedResult           = "parse_finalized_result"
	// Stealth (confidential-transfer) operations. The receive/scan arm (opScanStealthOutput) is
	// RNG-free and byte-stable. The two SEND arms compare semantically: their proofs + signatures are
	// not byte-stable, so the encoded bytes / id are not reproducible across runs.
	opBuildAndEncodeStealthTransfer = "build_and_encode_stealth_transfer"
	opBuildStealthOutputsStatement  = "build_stealth_outputs_statement"
	opScanStealthOutput             = "scan_stealth_output"
	// Stealth UTXO decode. RNG-free, byte-stable: a fetched UTXO substate (id + value) decodes into
	// the receive-shaped InboundStealthOutput.
	opDecodeStealthUTXO = "decode_stealth_utxo"
	// Deterministic keygen ops. RNG-free, byte-stable: the seed derives the keypair via the canonical
	// wallet KDF, reproducing the {secret, public_key} object exactly.
	opDeriveAccountKeyFromSeed = "derive_account_key_from_seed"
	opDeriveViewKeyFromSeed    = "derive_view_key_from_seed"
	// Account-address derivation. RNG-free, byte-stable: the public key hashes to a canonical
	// component_<hex> via the engine derivation.
	opDeriveAccountAddress = "derive_account_address"
	// Address parse/format codec. RNG-free, byte-stable: format_identity_address encodes an {network,
	// account_key, view_only_key, pay_ref?} record into an otl_… bech32m string; parse_address parses
	// a substate id OR an otl_… identity into the kind-tagged ParsedAddress.
	opFormatIdentityAddress = "format_identity_address"
	opParseAddress          = "parse_address"
	// Typed substate decode + account balances. RNG-free, byte-stable: a fetched substate decodes into
	// the kind-tagged DecodedSubstate; an account component + its fetched vaults sum into the
	// per-resource revealed balance. u64 balances stay native.
	opDecodeSubstate  = "decode_substate"
	opAccountBalances = "account_balances"
	// Generic builder. RNG-free deterministic seal, byte-stable: a GenericTransactionIntent lowers +
	// resolves + seals to the same encoded transaction, via ootle_build_unsigned_instructions driving
	// the apply/seal surface.
	opBuildAndEncodeInstructions = "build_and_encode_instructions"
	// Builtin faucet builder. RNG-free deterministic seal, byte-stable: a FaucetClaimIntent builds +
	// resolves + seals to the same encoded transaction, via ootle_build_faucet_claim driving the
	// apply/seal surface.
	opBuildAndEncodeFaucetClaim = "build_and_encode_faucet_claim"
	// Typed-arg DSL vectors that pin the literal CBOR bytes the core's encode_arg produces for one
	// ArgValue. There is no standalone arg-encode C ABI entry point (the host must never re-encode
	// args), so the bytes cross the ABI indirectly: they appear verbatim inside the
	// build_and_encode_instructions sealed vectors, which runEncodeInstructions byte-compares. The
	// encode_arg arm validates the vendored vector is well-formed.
	opEncodeArg = "encode_arg"
	// Co-signing: the authorize→attach→seal hand-off.
	//
	// cosign_seal_with_auth is the full authorize→attach→seal, semantic compare (the sealed tx carries
	// a Schnorr seal scalar that is byte-unstable). Go drives the whole flow over the C ABI —
	// build+resolve a handle, extract the unsigned record, authorize (production), seal-with-auth —
	// then hands the seal back to ootle_validate_stealth_transfer (the shared decode+verify-all-sigs
	// canonicalizer) and compares the deterministic decoded fields (signer pubkeys +
	// is_seal_signer_authorized survive).
	opCosignSealWithAuth = "cosign_seal_with_auth"
)

// goldenFixture is the generic, op-agnostic view of one committed vector. Op-specific
// input/expected fields are kept as json.RawMessage and decoded per-arm, so the loader
// stays a single code path over all operations.
type goldenFixture struct {
	Name      string `json:"name"`
	Operation string `json:"operation"`
	Input     struct {
		// Encode ops (build / resolve).
		Network *Network              `json:"network"`
		Intent  *PublicTransferIntent `json:"intent"`
		Keys    *PublicTransferKeys   `json:"keys"`
		// Resolve op only.
		Fetched []transport.FetchedSubstate `json:"fetched"`
		// Generic builder op only (build_and_encode_instructions) — the GenericTransactionIntent, handed
		// to the C ABI verbatim as a JSON string (the core lowers it; the host never re-encodes args).
		GenericIntent json.RawMessage `json:"generic_intent"`
		// Faucet-claim op only (build_and_encode_faucet_claim) — the FaucetClaimIntent, handed to the C
		// ABI verbatim.
		FaucetIntent json.RawMessage `json:"faucet_intent"`
		// Encode-arg op only — the single ArgValue whose CBOR bytes the core pins. Kept raw; there is no
		// host-side arg encoder.
		ArgValue json.RawMessage `json:"arg_value"`
		// Parse op only — committed verbatim, handed to the core as a JSON string.
		RawResult json.RawMessage `json:"raw_result"`
		// Scan op only — the receive-side scan input ({network, view_secret, account_secret?,
		// skip_memo?, output}); decoded per-arm into a scanGoldenInput.
		StealthScanInput json.RawMessage `json:"stealth_scan_input"`
		// Stealth send ops (build_and_encode_stealth_transfer / build_stealth_outputs_statement).
		// The network is shared (Network above) and the input UTXO substates reuse the same
		// "fetched" field as the resolve op (Fetched above); the rest of the stealth payload is
		// kept as raw JSON and handed to the C ABI verbatim, so the op-agnostic loader stays one
		// code path.
		StealthIntent json.RawMessage `json:"stealth_intent"`
		StealthKeys   json.RawMessage `json:"stealth_keys"`
		// Outputs-statement op only — the lowercase-hex 32-byte build seed that pins the statement's
		// per-output masks/nonces.
		StealthSeed  string   `json:"stealth_seed"`
		SpendSecrets []string `json:"spend_secrets"`
		// Keygen ops only — the lowercase-hex 32-byte seed.
		Seed string `json:"seed"`
		// Address-derive AND format-identity ops — the lowercase-hex 32-byte account public key.
		AccountPublicKey string `json:"account_public_key"`
		// Format-identity op only — the lowercase-hex 32-byte view-only public key + optional pay_ref.
		ViewOnlyKey string `json:"view_only_key"`
		PayRef      string `json:"pay_ref"`
		// Parse-address op only — the address string (component_/resource_<hex> or otl_…).
		Address string `json:"address"`
		// Decode-stealth-utxo op only — the UTXO substate id + the indexer's SubstateValue JSON.
		// SubstateValue is also reused by decode_substate (any substate) and account_balances (the
		// account component).
		SubstateID    string          `json:"substate_id"`
		SubstateValue json.RawMessage `json:"substate_value"`
		// Account-balances op only — the account's already-fetched vault substates.
		VaultSubstates []transport.FetchedSubstate `json:"vault_substates"`
		// Co-sign ops only — party A's seal public key (hex), party B's secret + build seed (hex).
		// The intent/fetched/keys above are reused to build A's resolved handle.
		CosignSealPK       string `json:"cosign_seal_pk"`
		CosignSignerSecret string `json:"cosign_signer_secret"`
		CosignSignerSeed   string `json:"cosign_signer_seed"`
	} `json:"input"`
	Expected struct {
		// Encode ops — lowercase hex, compared byte-for-byte as strings.
		EncodedTransaction string `json:"encoded_transaction"`
		TransactionID      string `json:"transaction_id"`
		// Parse op — canonical JSON of the FinalizedResult, compared structurally.
		Parsed json.RawMessage `json:"parsed"`
		// Scan op — the expected DecryptedOutput, or {"$none":true} for a not-mine output.
		Decrypted json.RawMessage `json:"decrypted"`
		// Stealth send semantic ops — the deterministic decoded fields the core locks (the
		// proofs/signature scalars are nulled). The Go host cannot decode BOR / verify the crypto, so
		// it cannot byte-compare these; it instead drives the same core op over the C ABI and asserts a
		// well-formed seal (see runStealthEncodeBuild). The fields are decoded so a malformed fixture
		// still fails the loader.
		SealedTransactionSemantic json.RawMessage `json:"sealed_transaction_semantic"`
		StealthOutputsStatement   json.RawMessage `json:"stealth_outputs_statement"`
		AggregatedOutputMask      string          `json:"aggregated_output_mask"`
		// Keygen ops only — the derived keypair object ({account_secret,account_public_key} or
		// {view_secret,view_public_key}), compared structurally (byte-stable hex fields inside).
		Keypair json.RawMessage `json:"keypair"`
		// Address-derive op only — the derived canonical component_<hex>, compared as a string.
		ComponentAddress string `json:"component_address"`
		// Format-identity op only — the encoded otl_… bech32m, compared as a string.
		Bech32m string `json:"bech32m"`
		// Parse-address op only — the kind-tagged ParsedAddress, compared structurally.
		ParsedAddress json.RawMessage `json:"parsed_address"`
		// Decode-stealth-utxo op only — the decoded InboundStealthOutput, compared structurally.
		InboundOutput json.RawMessage `json:"inbound_output"`
		// Decode-substate op only — the kind-tagged DecodedSubstate, compared structurally.
		DecodedSubstate json.RawMessage `json:"decoded_substate"`
		// Account-balances op only — the Vec<ResourceBalance>, compared structurally.
		AccountBalances json.RawMessage `json:"account_balances"`
		// Encode-arg op only — the core's pinned literal CBOR bytes (lowercase hex). Validated for
		// well-formedness; the ABI-crossing byte-compare lives in the generic-build vectors.
		EncodedArgBytes string `json:"encoded_arg_bytes"`
		// Cosign add-signature op only — the core's seed-derived {public_key, signature}. Cannot be
		// byte-reproduced over the random-only FFI; the Go arm checks functional correctness.
		CosignAuthorization json.RawMessage `json:"cosign_authorization"`
	} `json:"expected"`
	// Compare is the per-fixture comparison mode: "bytes" (the default, byte-for-byte) or
	// "semantic" (the stealth send ops — proofs/signatures are not byte-stable). Empty ⇒ "bytes".
	Compare string `json:"compare"`
}

// compareMode returns the fixture's comparison mode, defaulting to "bytes".
func (fx goldenFixture) compareMode() string {
	if fx.Compare == "" {
		return "bytes"
	}
	return fx.Compare
}

// vendoredFixturesDir is the checked-in copy of the core fixtures.
func vendoredFixturesDir() string {
	return filepath.Join("testdata", "fixtures")
}

// loadGoldenFixtures walks the vendored fixtures tree, decodes every *.json into a
// goldenFixture, and returns them sorted by relative path for stable ordering. It fails
// the test on any unreadable / unparseable file.
func loadGoldenFixtures(t *testing.T) []struct {
	rel string
	fx  goldenFixture
} {
	t.Helper()
	root := vendoredFixturesDir()
	var out []struct {
		rel string
		fx  goldenFixture
	}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var fx goldenFixture
		if uerr := json.Unmarshal(raw, &fx); uerr != nil {
			t.Fatalf("parse fixture %s: %v", path, uerr)
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, struct {
			rel string
			fx  goldenFixture
		}{rel: rel, fx: fx})
		return nil
	})
	if err != nil {
		t.Fatalf("walk fixtures: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("no fixtures found under %s", root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// TestGoldenVectors runs every committed vector through the C ABI and asserts parity:
// byte-for-byte hex (encode ops) or canonicalized structure (parse op). The Go SDK
// reproduces the same golden vectors as the core, across the C boundary.
func TestGoldenVectors(t *testing.T) {
	for _, entry := range loadGoldenFixtures(t) {
		entry := entry
		t.Run(entry.rel, func(t *testing.T) {
			runGoldenVector(t, entry.fx)
		})
	}
}

// runGoldenVector dispatches one fixture on its operation. An unknown operation fails
// the suite, so a new core vector cannot silently go unverified.
func runGoldenVector(t *testing.T, fx goldenFixture) {
	t.Helper()
	switch fx.Operation {
	case opBuildAndEncodePublicTransfer:
		runEncodeBuild(t, fx)
	case opResolveAndEncodePublicTransfer:
		runEncodeResolve(t, fx)
	case opParseFinalizedResult:
		runParse(t, fx)
	case opScanStealthOutput:
		// RNG-free, byte-stable: scan the fixture's inbound output with its scan keys and assert the
		// decrypted value/mask/memo (or the not-mine signal) matches expected.
		runScan(t, fx)
	case opDecodeStealthUTXO:
		// RNG-free, byte-stable: decode the fixture's fetched UTXO substate (id + value) over the C ABI
		// and assert the InboundStealthOutput matches expected structurally.
		runDecodeStealthUTXO(t, fx)
	case opBuildAndEncodeStealthTransfer:
		// Full stealth send, semantic compare: the embedded bulletproof + balance-proof signature are
		// byte-unstable, and the seal/auth signatures sign a digest over them, so the sealed bytes/id
		// are not reproducible. The runner drives the same core op over the C ABI and asserts a
		// well-formed seal (see runStealthEncodeBuild).
		runStealthEncodeBuild(t, fx)
	case opBuildStealthOutputsStatement:
		// The intermediate outputs-statement build, semantic compare. The Go host re-executes it over
		// the C ABI and compares the deterministic fields (see runStealthOutputsStatement).
		runStealthOutputsStatement(t, fx)
	case opDeriveAccountKeyFromSeed, opDeriveViewKeyFromSeed:
		// Deterministic keygen, byte-stable: derive the keypair from the fixture's seed over the C ABI
		// and assert the {secret, public_key} object matches expected.keypair exactly.
		runDeriveKeyFromSeed(t, fx)
	case opDeriveAccountAddress:
		// Account-address derivation, byte-stable: derive the address from the fixture's public key
		// over the C ABI and assert the canonical component_<hex> string matches expected.
		runDeriveAccountAddress(t, fx)
	case opFormatIdentityAddress:
		// Identity-address formatting, byte-stable: encode the fixture's {network, keys, pay_ref?} over
		// the C ABI and assert the otl_… bech32m string matches expected.
		runFormatIdentityAddress(t, fx)
	case opParseAddress:
		// Address parsing, byte-stable: parse the fixture's address over the C ABI and assert the
		// kind-tagged ParsedAddress matches expected structurally.
		runParseAddress(t, fx)
	case opDecodeSubstate:
		// Typed substate decode, byte-stable: decode the fixture's substate over the C ABI and assert
		// the kind-tagged DecodedSubstate matches expected structurally (u64 balances stay native).
		runDecodeSubstate(t, fx)
	case opAccountBalances:
		// Account balances, byte-stable: sum the account's revealed balances over the C ABI and assert
		// the per-resource balances match expected (u64 sums stay native).
		runAccountBalances(t, fx)
	case opBuildAndEncodeInstructions:
		// Generic builder, byte-stable: lower + resolve + seal the fixture's GenericTransactionIntent
		// over the C ABI entry point (driving apply/seal) and assert the encoded transaction + id hex
		// match the vector byte-for-byte.
		runEncodeInstructions(t, fx)
	case opBuildAndEncodeFaucetClaim:
		// Builtin faucet builder, byte-stable: build + resolve + seal the fixture's FaucetClaimIntent
		// over ootle_build_faucet_claim (driving apply/seal) and assert the encoded transaction + id hex
		// match the vector byte-for-byte.
		runEncodeFaucetClaim(t, fx)
	case opEncodeArg:
		// There is no standalone arg-encode C ABI fn (the host must not re-encode args), so validate
		// the vendored vector is well-formed. The bytes cross the ABI inside the generic-build vectors
		// above (byte-compared there).
		runEncodeArg(t, fx)
	case opCosignSealWithAuth:
		// Co-sign full seal, semantic compare: build+resolve → authorize → seal-with-auth → validate,
		// comparing the deterministic decoded fields (see runCosignSealWithAuth).
		runCosignSealWithAuth(t, fx)
	default:
		t.Fatalf("fixture %q: unknown operation %q (no Go runner arm — add one or the vector goes unchecked)", fx.Name, fx.Operation)
	}
}

// runDeriveKeyFromSeed exercises the deterministic keygen path through the typed wrapper and asserts
// the derived keypair matches the vector's expected.keypair structurally (the lowercase-hex fields
// inside are the byte-exact assertion; the seed path is RNG-free, so this is reproducible).
func runDeriveKeyFromSeed(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: keygen op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if fx.Input.Seed == "" {
		t.Fatalf("fixture %q: keygen op requires input.seed", fx.Name)
	}
	if len(fx.Expected.Keypair) == 0 {
		t.Fatalf("fixture %q: keygen op requires expected.keypair", fx.Name)
	}

	seed, err := hex.DecodeString(fx.Input.Seed)
	if err != nil || len(seed) != 32 {
		t.Fatalf("fixture %q: input.seed must be 32-byte lowercase hex (err=%v len=%d)", fx.Name, err, len(seed))
	}
	var seed32 [32]byte
	copy(seed32[:], seed)

	var got json.RawMessage
	switch fx.Operation {
	case opDeriveAccountKeyFromSeed:
		kp, derr := DeriveAccountKeyFromSeed(seed32)
		if derr != nil {
			t.Fatalf("fixture %q: DeriveAccountKeyFromSeed: %v", fx.Name, derr)
		}
		got, _ = json.Marshal(kp)
	case opDeriveViewKeyFromSeed:
		kp, derr := DeriveViewKeyFromSeed(seed32)
		if derr != nil {
			t.Fatalf("fixture %q: DeriveViewKeyFromSeed: %v", fx.Name, derr)
		}
		got, _ = json.Marshal(kp)
	default:
		t.Fatalf("fixture %q: runDeriveKeyFromSeed called for non-keygen op %q", fx.Name, fx.Operation)
	}

	gotV := canonicalizeJSON(t, got)
	wantV := canonicalizeJSON(t, fx.Expected.Keypair)
	if !reflect.DeepEqual(gotV, wantV) {
		gj, _ := json.MarshalIndent(gotV, "", "  ")
		wj, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("fixture %q: keypair mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// runDeriveAccountAddress exercises the account-address derivation through the typed wrapper and
// asserts the derived canonical component_<hex> matches the vector's expected.component_address
// byte-for-byte (string compare; the derivation is an RNG-free domain-separated hash). The crypto
// stays in the core — Go only marshals the public key in and compares the address string out.
func runDeriveAccountAddress(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: address-derive op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if fx.Input.AccountPublicKey == "" {
		t.Fatalf("fixture %q: address-derive op requires input.account_public_key", fx.Name)
	}
	if fx.Expected.ComponentAddress == "" {
		t.Fatalf("fixture %q: address-derive op requires expected.component_address", fx.Name)
	}

	pk, err := hex.DecodeString(fx.Input.AccountPublicKey)
	if err != nil || len(pk) != 32 {
		t.Fatalf("fixture %q: input.account_public_key must be 32-byte lowercase hex (err=%v len=%d)", fx.Name, err, len(pk))
	}
	var pk32 [32]byte
	copy(pk32[:], pk)

	got, derr := DeriveAccountAddress(pk32)
	if derr != nil {
		t.Fatalf("fixture %q: DeriveAccountAddress: %v", fx.Name, derr)
	}
	if got != fx.Expected.ComponentAddress {
		t.Errorf("fixture %q: component_address mismatch:\n got:  %s\n want: %s", fx.Name, got, fx.Expected.ComponentAddress)
	}
}

// runFormatIdentityAddress exercises identity-address formatting through the typed wrapper and asserts
// the encoded otl_… bech32m matches the vector byte-for-byte (string compare; bech32m is RNG-free).
// The codec stays in the core — Go only marshals the keys in and compares the string out.
func runFormatIdentityAddress(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: format-identity op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: format-identity op requires input.network", fx.Name)
	}
	if fx.Input.AccountPublicKey == "" || fx.Input.ViewOnlyKey == "" {
		t.Fatalf("fixture %q: format-identity op requires input.account_public_key + input.view_only_key", fx.Name)
	}
	if fx.Expected.Bech32m == "" {
		t.Fatalf("fixture %q: format-identity op requires expected.bech32m", fx.Name)
	}

	account, err := hex.DecodeString(fx.Input.AccountPublicKey)
	if err != nil || len(account) != 32 {
		t.Fatalf("fixture %q: input.account_public_key must be 32-byte lowercase hex (err=%v len=%d)", fx.Name, err, len(account))
	}
	view, err := hex.DecodeString(fx.Input.ViewOnlyKey)
	if err != nil || len(view) != 32 {
		t.Fatalf("fixture %q: input.view_only_key must be 32-byte lowercase hex (err=%v len=%d)", fx.Name, err, len(view))
	}
	var account32, view32 [32]byte
	copy(account32[:], account)
	copy(view32[:], view)

	var payRef []byte
	if fx.Input.PayRef != "" {
		payRef, err = hex.DecodeString(fx.Input.PayRef)
		if err != nil {
			t.Fatalf("fixture %q: input.pay_ref must be lowercase hex: %v", fx.Name, err)
		}
	}

	got, derr := FormatIdentityAddress(*fx.Input.Network, account32, view32, payRef)
	if derr != nil {
		t.Fatalf("fixture %q: FormatIdentityAddress: %v", fx.Name, derr)
	}
	if got != fx.Expected.Bech32m {
		t.Errorf("fixture %q: bech32m mismatch:\n got:  %s\n want: %s", fx.Name, got, fx.Expected.Bech32m)
	}
}

// runParseAddress exercises address parsing through the typed wrapper and asserts the kind-tagged
// ParsedAddress matches the vector's expected.parsed_address structurally (parsing is RNG-free, so
// the record is byte-stable; compared as canonicalized JSON for key-order insensitivity).
func runParseAddress(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: parse-address op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if fx.Input.Address == "" {
		t.Fatalf("fixture %q: parse-address op requires input.address", fx.Name)
	}
	if len(fx.Expected.ParsedAddress) == 0 {
		t.Fatalf("fixture %q: parse-address op requires expected.parsed_address", fx.Name)
	}

	parsed, derr := ParseAddress(fx.Input.Address)
	if derr != nil {
		t.Fatalf("fixture %q: ParseAddress: %v", fx.Name, derr)
	}
	got, _ := json.Marshal(parsed)

	gotV := canonicalizeJSON(t, got)
	wantV := canonicalizeJSON(t, fx.Expected.ParsedAddress)
	if !reflect.DeepEqual(gotV, wantV) {
		gj, _ := json.MarshalIndent(gotV, "", "  ")
		wj, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("fixture %q: parsed_address mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// runDecodeStealthUTXO exercises the stealth UTXO decode through the typed wrapper and asserts the
// decoded InboundStealthOutput matches the vector's expected.inbound_output structurally (the
// decode is RNG-free, so this is byte-stable; compared as canonicalized JSON for key-order
// insensitivity). Also fuses decode → scan via ScanStealthSubstate and asserts the value the
// mine_basic scan vector commits (decode → scan composes across the C ABI), proving the receive
// path runs with NO env-supplied crypto fields.
func runDecodeStealthUTXO(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: decode op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if fx.Input.SubstateID == "" {
		t.Fatalf("fixture %q: decode op requires input.substate_id", fx.Name)
	}
	if len(fx.Input.SubstateValue) == 0 {
		t.Fatalf("fixture %q: decode op requires input.substate_value", fx.Name)
	}
	if len(fx.Expected.InboundOutput) == 0 {
		t.Fatalf("fixture %q: decode op requires expected.inbound_output", fx.Name)
	}

	inbound, derr := DecodeStealthUTXO(fx.Input.SubstateID, fx.Input.SubstateValue)
	if derr != nil {
		t.Fatalf("fixture %q: DecodeStealthUTXO: %v", fx.Name, derr)
	}
	got, _ := json.Marshal(inbound)

	gotV := canonicalizeJSON(t, got)
	wantV := canonicalizeJSON(t, fx.Expected.InboundOutput)
	if !reflect.DeepEqual(gotV, wantV) {
		gj, _ := json.MarshalIndent(gotV, "", "  ")
		wj, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("fixture %q: inbound_output mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// runDecodeSubstate exercises the typed substate decode through the typed wrapper and asserts the
// kind-tagged DecodedSubstate matches the vector's expected.decoded_substate structurally (the decode
// is RNG-free, so this is byte-stable; u64 balances stay native).
func runDecodeSubstate(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: decode_substate op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Input.SubstateValue) == 0 {
		t.Fatalf("fixture %q: decode_substate op requires input.substate_value", fx.Name)
	}
	if len(fx.Expected.DecodedSubstate) == 0 {
		t.Fatalf("fixture %q: decode_substate op requires expected.decoded_substate", fx.Name)
	}

	decoded, derr := DecodeSubstate(fx.Input.SubstateValue)
	if derr != nil {
		t.Fatalf("fixture %q: DecodeSubstate: %v", fx.Name, derr)
	}
	got, _ := json.Marshal(decoded)

	gotV := canonicalizeJSON(t, got)
	wantV := canonicalizeJSON(t, fx.Expected.DecodedSubstate)
	if !reflect.DeepEqual(gotV, wantV) {
		gj, _ := json.MarshalIndent(gotV, "", "  ")
		wj, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("fixture %q: decoded_substate mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// runAccountBalances exercises the per-resource revealed balance sum through the typed wrapper and
// asserts the balances match the vector's expected.account_balances structurally (the sum is
// RNG-free; the u64 revealed balances stay native).
func runAccountBalances(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: account_balances op must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Input.SubstateValue) == 0 {
		t.Fatalf("fixture %q: account_balances op requires input.substate_value (the account component)", fx.Name)
	}
	if len(fx.Expected.AccountBalances) == 0 {
		t.Fatalf("fixture %q: account_balances op requires expected.account_balances", fx.Name)
	}

	balances, berr := AccountBalances(fx.Input.SubstateValue, fx.Input.VaultSubstates)
	if berr != nil {
		t.Fatalf("fixture %q: AccountBalances: %v", fx.Name, berr)
	}
	got, _ := json.Marshal(balances)

	gotV := canonicalizeJSON(t, got)
	wantV := canonicalizeJSON(t, fx.Expected.AccountBalances)
	if !reflect.DeepEqual(gotV, wantV) {
		gj, _ := json.MarshalIndent(gotV, "", "  ")
		wj, _ := json.MarshalIndent(wantV, "", "  ")
		t.Errorf("fixture %q: account_balances mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// runEncodeBuild exercises the one-call build+seal+encode path over the C ABI. The seal signs with a
// fresh random nonce (ABI ootle-sdk-ffi-c/16), so the encoded bytes/id are NOT reproducible and there
// is no public-transaction decode/verify FFI to canonicalize them. So — mirroring the monorepo's own
// FFI test — the arm asserts the encoded envelope is well-formed (see assertWellFormedEncoded). The
// deterministic sealed_transaction_semantic block is re-verified only by the Rust core tests.
func runEncodeBuild(t *testing.T, fx goldenFixture) {
	t.Helper()
	net, intent, keys := requireEncodeInput(t, fx)
	netByte, ok := net.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, net)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	dataJSON, cerr := cffi.BuildAndEncodePublicTransfer(netByte, string(intentJSON), string(keysJSON))
	if cerr != nil {
		t.Fatalf("BuildAndEncodePublicTransfer over C ABI: %v", cerr)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(dataJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	assertSemanticSealed(t, fx, netByte, encoded.EncodedTransaction)
}

// runEncodeResolve drives the two-phase resolved path directly through the cgo seams:
// BuildUnsigned → ApplyFetchedSubstates(the vector's full fetched batch) → SealAndEncode. The
// committed vector carries the complete fetched batch (component + vault together), so a single apply
// pass resolves. The random-nonce seal is not byte-reproducible, so the arm asserts a well-formed
// encoded envelope (see runEncodeBuild). (The Client driver loop is exercised separately in
// driver_test.go.)
func runEncodeResolve(t *testing.T, fx goldenFixture) {
	t.Helper()
	net, intent, keys := requireEncodeInput(t, fx)
	if fx.Input.Fetched == nil {
		t.Fatalf("fixture %q: resolve_and_encode requires input.fetched", fx.Name)
	}

	netByte, ok := net.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, net)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}

	handle, _, err := cffi.BuildUnsigned(netByte, string(intentJSON))
	if err != nil {
		t.Fatalf("BuildUnsigned: %v", err)
	}
	// Free whatever handle is live on any early exit. A closure (not `defer
	// FreeHandle(handle)`) is required: a direct deferred call evaluates its argument at
	// the defer statement, capturing the original handle, and would miss the reassigned
	// `next` (leaking it on an early `t.Fatalf`). The closure reads `handle` at run time,
	// so it always frees the currently-owned handle (nil'd once a consuming call takes it).
	defer func() { cffi.FreeHandle(handle) }()

	next, resolutionJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	handle = next // ApplyFetchedSubstates consumed the old handle.
	if err != nil {
		t.Fatalf("ApplyFetchedSubstates: %v", err)
	}
	// The vector's full batch must resolve in this single pass.
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resolutionJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		t.Fatalf("fixture %q: single-batch resolve did not converge (status=%q); the vector's fetched batch should fully resolve", fx.Name, res.Status)
	}

	encodedJSON, err := cffi.SealAndEncode(handle, string(keysJSON))
	handle = nil // consumed by SealAndEncode.
	if err != nil {
		t.Fatalf("SealAndEncode: %v", err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	assertSemanticSealed(t, fx, netByte, encoded.EncodedTransaction)
}

// runEncodeInstructions drives the generic builder's two-phase path directly through the cgo seams:
// BuildUnsignedInstructions(the fixture's generic_intent) → ApplyFetchedSubstates(the vector's fetched
// batch) → SealAndEncode. The generic-build vectors carry explicit inputs (so an empty fetched batch
// resolves in one pass). The random-nonce seal is not byte-reproducible, so the arm asserts a
// well-formed encoded envelope (see runEncodeBuild). The generic entry point reuses the same apply/seal
// surface as the public path.
func runEncodeInstructions(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: build_and_encode_instructions requires input.network", fx.Name)
	}
	if len(fx.Input.GenericIntent) == 0 {
		t.Fatalf("fixture %q: build_and_encode_instructions requires input.generic_intent", fx.Name)
	}
	if fx.Input.Keys == nil {
		t.Fatalf("fixture %q: build_and_encode_instructions requires input.keys", fx.Name)
	}
	if fx.Input.Fetched == nil {
		t.Fatalf("fixture %q: build_and_encode_instructions requires input.fetched", fx.Name)
	}

	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, *fx.Input.Network)
	}
	keysJSON, err := json.Marshal(fx.Input.Keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}

	// The intent is handed to the C ABI verbatim (the core lowers + encodes; the host never re-encodes).
	handle, _, err := cffi.BuildUnsignedInstructions(netByte, string(fx.Input.GenericIntent))
	if err != nil {
		t.Fatalf("BuildUnsignedInstructions: %v", err)
	}
	// Free whatever handle is live on any early exit (closure, not a direct deferred FreeHandle, so the
	// reassigned `next` is freed — see runEncodeResolve).
	defer func() { cffi.FreeHandle(handle) }()

	next, resolutionJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	handle = next // consumed the old handle.
	if err != nil {
		t.Fatalf("ApplyFetchedSubstates: %v", err)
	}
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resolutionJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		t.Fatalf("fixture %q: single-batch resolve did not converge (status=%q)", fx.Name, res.Status)
	}

	encodedJSON, err := cffi.SealAndEncode(handle, string(keysJSON))
	handle = nil // consumed by SealAndEncode.
	if err != nil {
		t.Fatalf("SealAndEncode: %v", err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	assertSemanticSealed(t, fx, netByte, encoded.EncodedTransaction)
}

// runEncodeFaucetClaim drives the builtin faucet builder's two-phase path through the cgo seams:
// BuildFaucetClaim(the fixture's faucet_intent) → ApplyFetchedSubstates(the vector's fetched batch) →
// SealAndEncode. The vector carries the faucet's fetched component + vault, so it resolves in one pass.
// The random-nonce seal is not byte-reproducible, so the arm asserts a well-formed encoded envelope
// (see runEncodeBuild). The faucet entry point reuses the same apply/seal surface as the generic and
// public paths.
func runEncodeFaucetClaim(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: build_and_encode_faucet_claim requires input.network", fx.Name)
	}
	if len(fx.Input.FaucetIntent) == 0 {
		t.Fatalf("fixture %q: build_and_encode_faucet_claim requires input.faucet_intent", fx.Name)
	}
	if fx.Input.Keys == nil {
		t.Fatalf("fixture %q: build_and_encode_faucet_claim requires input.keys", fx.Name)
	}
	if fx.Input.Fetched == nil {
		t.Fatalf("fixture %q: build_and_encode_faucet_claim requires input.fetched", fx.Name)
	}

	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, *fx.Input.Network)
	}
	keysJSON, err := json.Marshal(fx.Input.Keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}

	// The intent is handed to the C ABI verbatim (the core builds + lowers; the host never re-encodes).
	handle, _, err := cffi.BuildFaucetClaim(netByte, string(fx.Input.FaucetIntent))
	if err != nil {
		t.Fatalf("BuildFaucetClaim: %v", err)
	}
	// Free whatever handle is live on any early exit (closure, not a direct deferred FreeHandle, so the
	// reassigned `next` is freed — see runEncodeResolve).
	defer func() { cffi.FreeHandle(handle) }()

	next, resolutionJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	handle = next // consumed the old handle.
	if err != nil {
		t.Fatalf("ApplyFetchedSubstates: %v", err)
	}
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resolutionJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		t.Fatalf("fixture %q: single-batch resolve did not converge (status=%q)", fx.Name, res.Status)
	}

	encodedJSON, err := cffi.SealAndEncode(handle, string(keysJSON))
	handle = nil // consumed by SealAndEncode.
	if err != nil {
		t.Fatalf("SealAndEncode: %v", err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("unmarshal encoded transfer: %v", uerr)
	}
	assertSemanticSealed(t, fx, netByte, encoded.EncodedTransaction)
}

// runEncodeArg validates a typed-arg DSL vector. There is no standalone arg-encode C ABI entry point —
// re-encoding an ArgValue host-side would duplicate the engine's CBOR encoder in Go. So this arm does
// not re-encode: it asserts the vendored vector is well-formed (a present ArgValue input + a
// lowercase-hex encoded_arg_bytes). The ABI-crossing byte-compare for these encodings is the
// build_and_encode_instructions vectors, whose sealed bytes embed exactly these arg encodings.
func runEncodeArg(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "bytes" {
		t.Fatalf("fixture %q: encode_arg must use the default \"bytes\" compare (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Input.ArgValue) == 0 {
		t.Fatalf("fixture %q: encode_arg requires input.arg_value", fx.Name)
	}
	if fx.Expected.EncodedArgBytes == "" || !isLowerHex(fx.Expected.EncodedArgBytes) || len(fx.Expected.EncodedArgBytes)%2 != 0 {
		t.Fatalf("fixture %q: expected.encoded_arg_bytes must be non-empty even-length lowercase hex, got %q", fx.Name, fx.Expected.EncodedArgBytes)
	}
}

// runParse exercises the result parser and asserts canonicalized structural equality
// (JSON key order can differ, so both sides are canonicalized before comparing — a byte
// compare would be brittle here).
func runParse(t *testing.T, fx goldenFixture) {
	t.Helper()
	if len(fx.Input.RawResult) == 0 {
		t.Fatalf("fixture %q: parse_finalized_result requires input.raw_result", fx.Name)
	}
	if len(fx.Expected.Parsed) == 0 {
		t.Fatalf("fixture %q: parse_finalized_result requires expected.parsed", fx.Name)
	}

	parsedJSON, err := cffi.ParseFinalizedResult(string(fx.Input.RawResult))
	if err != nil {
		t.Fatalf("ParseFinalizedResult: %v", err)
	}

	got := canonicalizeJSON(t, []byte(parsedJSON))
	want := canonicalizeJSON(t, fx.Expected.Parsed)
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		wj, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("fixture %q: parsed FinalizedResult mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// scanGoldenInput is the per-arm view of a scan_stealth_output fixture's stealth_scan_input
// block: the network + scan keys (view_secret + optional account_secret + skip_memo) + the
// inbound output to scan. Decoded only inside runScan so the generic loader stays op-agnostic.
type scanGoldenInput struct {
	Network       Network              `json:"network"`
	ViewSecret    string               `json:"view_secret"`
	AccountSecret *string              `json:"account_secret"`
	SkipMemo      bool                 `json:"skip_memo"`
	Output        InboundStealthOutput `json:"output"`
}

// runScan executes one scan_stealth_output vector through ScanStealthOutput (over the C ABI)
// and asserts byte-stable parity against expected.decrypted: either a full DecryptedOutput
// (is_mine) or the {"$none":true} not-mine marker. Scanning is RNG-free, so this is an exact
// compare.
func runScan(t *testing.T, fx goldenFixture) {
	t.Helper()
	if len(fx.Input.StealthScanInput) == 0 {
		t.Fatalf("fixture %q: scan_stealth_output requires input.stealth_scan_input", fx.Name)
	}
	if len(fx.Expected.Decrypted) == 0 {
		t.Fatalf("fixture %q: scan_stealth_output requires expected.decrypted", fx.Name)
	}

	var in scanGoldenInput
	if err := json.Unmarshal(fx.Input.StealthScanInput, &in); err != nil {
		t.Fatalf("fixture %q: decode stealth_scan_input: %v", fx.Name, err)
	}

	scanKeys := StealthScanKeys{
		ViewSecret:    in.ViewSecret,
		AccountSecret: in.AccountSecret,
		SkipMemo:      in.SkipMemo,
	}
	got, err := ScanStealthOutput(in.Network, scanKeys, in.Output)
	if err != nil {
		t.Fatalf("fixture %q: ScanStealthOutput: %v", fx.Name, err)
	}
	if got == nil {
		t.Fatalf("fixture %q: ScanStealthOutput returned nil without error", fx.Name)
	}

	// The not-mine marker {"$none":true} ⇒ expect IsMine=false.
	var none struct {
		None bool `json:"$none"`
	}
	if jerr := json.Unmarshal(fx.Expected.Decrypted, &none); jerr == nil && none.None {
		if got.IsMine {
			t.Errorf("fixture %q: expected not-mine, got IsMine=true (%+v)", fx.Name, got)
		}
		return
	}

	var want DecryptedOutput
	if jerr := json.Unmarshal(fx.Expected.Decrypted, &want); jerr != nil {
		t.Fatalf("fixture %q: decode expected.decrypted: %v", fx.Name, jerr)
	}
	if got.IsMine != want.IsMine || got.Value != want.Value || got.Mask != want.Mask {
		t.Errorf("fixture %q: scan mismatch:\n got:  %+v\n want: %+v", fx.Name, got, want)
	}
	if !reflect.DeepEqual(got.Memo, want.Memo) {
		t.Errorf("fixture %q: scan memo mismatch:\n got:  %+v\n want: %+v", fx.Name, got.Memo, want.Memo)
	}
}

// runStealthEncodeBuild runs the full stealth-send vector (build_and_encode_stealth_transfer)
// over the C ABI under the semantic compare.
//
// The Go arm performs the full deterministic-field compare: it (1) seals the transfer over the
// C ABI, then (2) hands the seal back to ootle_validate_stealth_transfer, which BOR-decodes it,
// verifies every signature, and returns the canonical JSON with the byte-unstable fields
// (agg_range_proof / balance_proof / signature scalars) nulled, and (3) compares that JSON
// to expected.sealed_transaction_semantic. The value-critical crypto — BOR decode + Schnorr
// verification + the shared null set — lives in the core; the Go host only marshals and compares
// JSON. A marshalling drift, a malformed intent, bad-length entropy, an input-decryption failure,
// a balance-proof failure, or a deterministic-field mismatch all fail here.
func runStealthEncodeBuild(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "semantic" {
		t.Fatalf("fixture %q: build_and_encode_stealth_transfer must declare \"compare\":\"semantic\" (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Expected.SealedTransactionSemantic) == 0 {
		t.Fatalf("fixture %q: semantic send vector requires expected.sealed_transaction_semantic", fx.Name)
	}
	net, intentJSON := requireStealthSendInput(t, fx)
	if len(fx.Input.StealthKeys) == 0 {
		t.Fatalf("fixture %q: build_and_encode_stealth_transfer requires input.stealth_keys", fx.Name)
	}

	netByte, ok := net.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, net)
	}

	fetched := fx.Input.Fetched
	if fetched == nil {
		fetched = []transport.FetchedSubstate{}
	}
	fetchedJSON, err := json.Marshal(fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}
	secrets := fx.Input.SpendSecrets
	if secrets == nil {
		secrets = []string{}
	}
	secretsJSON, err := json.Marshal(secrets)
	if err != nil {
		t.Fatalf("marshal spend secrets: %v", err)
	}

	// Drive the core entry point straight through the cgo seam. The build seed travels inside
	// stealth_keys (the seed-reproducible path).
	dataJSON, cerr := cffi.BuildAndEncodeStealthTransferWithSeed(
		netByte,
		string(intentJSON),
		string(fetchedJSON),
		string(secretsJSON),
		string(fx.Input.StealthKeys),
	)
	if cerr != nil {
		t.Fatalf("fixture %q: BuildAndEncodeStealthTransferWithSeed over C ABI failed: %v", fx.Name, cerr)
	}

	var encoded EncodedStealthTransfer
	if uerr := json.Unmarshal([]byte(dataJSON), &encoded); uerr != nil {
		t.Fatalf("fixture %q: unmarshal encoded stealth transfer: %v", fx.Name, uerr)
	}

	// Hand the seal just produced back across the C ABI to ootle_validate_stealth_transfer, which
	// BOR-decodes it, verifies every signature, and returns the canonical JSON with the byte-unstable
	// set nulled. Decode + Schnorr verify stay in the core; Go only compares the JSON.
	canonicalJSON, verr := cffi.ValidateStealthTransfer(netByte, encoded.EncodedTransaction)
	if verr != nil {
		t.Fatalf("fixture %q: ValidateStealthTransfer (decode+verify) over C ABI failed: %v", fx.Name, verr)
	}
	got := canonicalizeJSON(t, []byte(canonicalJSON))
	want := canonicalizeJSON(t, fx.Expected.SealedTransactionSemantic)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fixture %q: sealed_transaction (deterministic fields) mismatch:\n got:  %v\n want: %v", fx.Name, got, want)
	}
}

// runStealthOutputsStatement honors a build_stealth_outputs_statement vector under the
// semantic compare. The C ABI exposes a standalone outputs-statement entry point
// (ootle_build_stealth_outputs_statement_with_seed), so the Go host re-executes the op over the C ABI
// with the fixture's intent + build seed and compares the deterministic fields it gets back against
// the vendored fixture: the aggregated_output_mask byte-for-byte and the outputs_statement (with the
// byte-unstable agg_range_proof nulled) field-for-field. The statement build stays in the core.
func runStealthOutputsStatement(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "semantic" {
		t.Fatalf("fixture %q: build_stealth_outputs_statement must declare \"compare\":\"semantic\" (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Input.StealthIntent) == 0 || fx.Input.StealthSeed == "" {
		t.Fatalf("fixture %q: outputs-statement vector requires input.stealth_intent + input.stealth_seed", fx.Name)
	}
	if len(fx.Expected.StealthOutputsStatement) == 0 {
		t.Fatalf("fixture %q: outputs-statement vector requires expected.stealth_outputs_statement", fx.Name)
	}
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: outputs-statement vector requires input.network", fx.Name)
	}
	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, *fx.Input.Network)
	}

	// Drive the standalone core entry point straight through the cgo seam.
	dataJSON, cerr := cffi.BuildStealthOutputsStatementWithSeed(netByte, string(fx.Input.StealthIntent), fx.Input.StealthSeed)
	if cerr != nil {
		t.Fatalf("fixture %q: BuildStealthOutputsStatementWithSeed over C ABI failed: %v", fx.Name, cerr)
	}

	var got struct {
		OutputsStatement     json.RawMessage `json:"outputs_statement"`
		AggregatedOutputMask string          `json:"aggregated_output_mask"`
	}
	if uerr := json.Unmarshal([]byte(dataJSON), &got); uerr != nil {
		t.Fatalf("fixture %q: unmarshal outputs-statement result: %v", fx.Name, uerr)
	}

	// (1) The aggregated output mask is the deterministic 32-byte scalar — byte-compared even in
	// semantic mode; a non-hex / wrong-length mask is drift.
	if len(fx.Expected.AggregatedOutputMask) != 64 || !isLowerHex(fx.Expected.AggregatedOutputMask) {
		t.Fatalf("fixture %q: expected.aggregated_output_mask is not a 64-hex (32-byte) scalar: %q", fx.Name, fx.Expected.AggregatedOutputMask)
	}
	if got.AggregatedOutputMask != fx.Expected.AggregatedOutputMask {
		t.Errorf("fixture %q: aggregated_output_mask mismatch:\n got:  %s\n want: %s", fx.Name, got.AggregatedOutputMask, fx.Expected.AggregatedOutputMask)
	}

	// (2) The statement (agg_range_proof nulled by the C fn — matching the fixture) must equal the
	// recorded deterministic statement structurally.
	gotStmt := canonicalizeJSON(t, got.OutputsStatement)
	wantStmt := canonicalizeJSON(t, fx.Expected.StealthOutputsStatement)
	if !reflect.DeepEqual(gotStmt, wantStmt) {
		t.Errorf("fixture %q: outputs_statement (deterministic fields) mismatch:\n got:  %v\n want: %v", fx.Name, gotStmt, wantStmt)
	}
	// Defensive: the byte-unstable bulletproof is nulled on both sides (semantic), and outputs is non-empty.
	stmtMap, _ := gotStmt.(map[string]interface{})
	if stmtMap == nil || stmtMap["agg_range_proof"] != nil {
		t.Errorf("fixture %q: outputs_statement.agg_range_proof must be nulled (semantic)", fx.Name)
	}
	if outs, _ := stmtMap["outputs"].([]interface{}); len(outs) == 0 {
		t.Errorf("fixture %q: outputs_statement.outputs is empty", fx.Name)
	}
}

// requireStealthSendInput extracts the (network, intent JSON) a stealth send op needs, failing the
// test if any required field is absent. The build seed travels inside stealth_keys.
func requireStealthSendInput(t *testing.T, fx goldenFixture) (Network, json.RawMessage) {
	t.Helper()
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: stealth send op requires input.network", fx.Name)
	}
	if len(fx.Input.StealthIntent) == 0 {
		t.Fatalf("fixture %q: stealth send op requires input.stealth_intent", fx.Name)
	}
	return *fx.Input.Network, fx.Input.StealthIntent
}

// isLowerHex reports whether s is a non-empty, even-length, all-lowercase-hex string.
func isLowerHex(s string) bool {
	if s == "" || len(s)%2 != 0 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// requireEncodeInput extracts the (network, intent, keys) an encode op needs, failing
// the test if any is absent.
func requireEncodeInput(t *testing.T, fx goldenFixture) (Network, PublicTransferIntent, PublicTransferKeys) {
	t.Helper()
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: encode op requires input.network", fx.Name)
	}
	if fx.Input.Intent == nil {
		t.Fatalf("fixture %q: encode op requires input.intent", fx.Name)
	}
	if fx.Input.Keys == nil {
		t.Fatalf("fixture %q: encode op requires input.keys", fx.Name)
	}
	return *fx.Input.Network, *fx.Input.Intent, *fx.Input.Keys
}

// cosignResolveHandle builds + resolves party A's public handle from the fixture's
// network/intent/fetched. The caller owns the returned handle (consume via seal or free it).
func cosignResolveHandle(t *testing.T, fx goldenFixture) (*cffi.Handle, uint8) {
	t.Helper()
	if fx.Input.Network == nil {
		t.Fatalf("fixture %q: cosign op requires input.network", fx.Name)
	}
	if fx.Input.Intent == nil {
		t.Fatalf("fixture %q: cosign op requires input.intent", fx.Name)
	}
	if fx.Input.Fetched == nil {
		t.Fatalf("fixture %q: cosign op requires input.fetched", fx.Name)
	}
	netByte, ok := fx.Input.Network.ByteValue()
	if !ok {
		t.Fatalf("fixture %q: unknown network %q", fx.Name, *fx.Input.Network)
	}
	intentJSON, err := json.Marshal(fx.Input.Intent)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	fetchedJSON, err := json.Marshal(fx.Input.Fetched)
	if err != nil {
		t.Fatalf("marshal fetched: %v", err)
	}
	handle, _, err := cffi.BuildUnsigned(netByte, string(intentJSON))
	if err != nil {
		t.Fatalf("fixture %q: BuildUnsigned: %v", fx.Name, err)
	}
	next, resolutionJSON, err := cffi.ApplyFetchedSubstates(handle, string(fetchedJSON))
	if err != nil {
		t.Fatalf("fixture %q: ApplyFetchedSubstates: %v", fx.Name, err)
	}
	var res resolutionEnvelope
	if uerr := json.Unmarshal([]byte(resolutionJSON), &res); uerr != nil {
		t.Fatalf("unmarshal resolution: %v", uerr)
	}
	if res.Status != "resolved" {
		cffi.FreeHandle(next)
		t.Fatalf("fixture %q: cosign batch did not resolve in one pass (status=%q)", fx.Name, res.Status)
	}
	return next, netByte
}

// cosignAuthorize derives A's unsigned record from a resolved handle (BORROW — does not consume it),
// then has party B authorize it (production) over the C ABI. Returns B's authorization JSON object.
func cosignAuthorize(t *testing.T, fx goldenFixture, handle *cffi.Handle, netByte uint8) json.RawMessage {
	t.Helper()
	if fx.Input.CosignSealPK == "" || fx.Input.CosignSignerSecret == "" {
		t.Fatalf("fixture %q: cosign op requires input.cosign_seal_pk + input.cosign_signer_secret", fx.Name)
	}
	recordJSON, err := cffi.UnsignedRecordForCosign(handle)
	if err != nil {
		t.Fatalf("fixture %q: UnsignedRecordForCosign: %v", fx.Name, err)
	}
	authJSON, err := cffi.AddSignature(netByte, recordJSON, fx.Input.CosignSealPK, fx.Input.CosignSignerSecret)
	if err != nil {
		t.Fatalf("fixture %q: AddSignature: %v", fx.Name, err)
	}
	var wrap struct {
		Authorization json.RawMessage `json:"authorization"`
	}
	if uerr := json.Unmarshal([]byte(authJSON), &wrap); uerr != nil {
		t.Fatalf("fixture %q: unmarshal authorization: %v", fx.Name, uerr)
	}
	return wrap.Authorization
}

// runCosignSealWithAuth drives the full authorize→attach→seal flow over the C ABI (semantic compare):
// build+resolve A's handle → extract the unsigned record → B authorizes (production) → A seals with
// the authorization attached → hand the seal back to ootle_validate_stealth_transfer (the shared
// decode+verify-all-sigs canonicalizer) and compare the deterministic decoded fields against the
// vendored vector. The signer public keys + is_seal_signer_authorized survive the null set; the
// Schnorr scalars are nulled. Decode + Schnorr verify stay in the core.
func runCosignSealWithAuth(t *testing.T, fx goldenFixture) {
	t.Helper()
	if fx.compareMode() != "semantic" {
		t.Fatalf("fixture %q: cosign_seal_with_auth must declare \"compare\":\"semantic\" (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Expected.SealedTransactionSemantic) == 0 {
		t.Fatalf("fixture %q: cosign_seal_with_auth requires expected.sealed_transaction_semantic", fx.Name)
	}
	if fx.Input.Keys == nil {
		t.Fatalf("fixture %q: cosign_seal_with_auth requires input.keys", fx.Name)
	}

	handle, netByte := cosignResolveHandle(t, fx)
	defer func() { cffi.FreeHandle(handle) }()

	auth := cosignAuthorize(t, fx, handle, netByte)

	keysJSON, err := json.Marshal(fx.Input.Keys)
	if err != nil {
		t.Fatalf("marshal keys: %v", err)
	}
	authsJSON, err := json.Marshal([]json.RawMessage{auth})
	if err != nil {
		t.Fatalf("marshal authorizations: %v", err)
	}
	encodedJSON, err := cffi.SealAndEncodeWithAuth(handle, string(keysJSON), string(authsJSON))
	handle = nil // consumed.
	if err != nil {
		t.Fatalf("fixture %q: SealAndEncodeWithAuth: %v", fx.Name, err)
	}
	var encoded EncodedPublicTransfer
	if uerr := json.Unmarshal([]byte(encodedJSON), &encoded); uerr != nil {
		t.Fatalf("fixture %q: unmarshal encoded transfer: %v", fx.Name, uerr)
	}
	assertSemanticSealed(t, fx, netByte, encoded.EncodedTransaction)
}

// assertSemanticSealed is the ABI-16 semantic counterpart of the old byte-for-byte sealed-transaction
// compare. A random-nonce seal (ootle-sdk-ffi-c/16) is no longer byte-reproducible, so instead of
// comparing the encoded bytes the runner hands them back to the core over the C ABI, which BOR-decodes
// the sealed transaction, verifies EVERY signature, and returns canonical JSON with the byte-unstable
// Schnorr scalars nulled; the runner then byte-compares the deterministic fields (instructions, inputs,
// fee, signer public keys) against the fixture's expected.sealed_transaction_semantic. This preserves
// the cross-language ENCODING conformance guarantee while tolerating the random signature bytes, and
// mirrors the Rust golden-vector compare. The decode+verify is transaction-agnostic (it works for the
// public sealed ops as well as stealth — see decode_and_canonicalize_sealed_transfer in the core);
// ootle_validate_stealth_transfer is simply the entry point that exposes it.
func assertSemanticSealed(t *testing.T, fx goldenFixture, netByte uint8, encodedHex string) {
	t.Helper()
	if fx.compareMode() != "semantic" {
		t.Fatalf("fixture %q: sealed op must declare \"compare\":\"semantic\" (got %q)", fx.Name, fx.compareMode())
	}
	if len(fx.Expected.SealedTransactionSemantic) == 0 {
		t.Fatalf("fixture %q: semantic sealed op requires expected.sealed_transaction_semantic", fx.Name)
	}
	canonicalJSON, verr := cffi.ValidateStealthTransfer(netByte, encodedHex)
	if verr != nil {
		t.Fatalf("fixture %q: decode+verify (ootle_validate_stealth_transfer) over C ABI failed: %v", fx.Name, verr)
	}
	got := canonicalizeJSON(t, []byte(canonicalJSON))
	want := canonicalizeJSON(t, fx.Expected.SealedTransactionSemantic)
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.MarshalIndent(got, "", "  ")
		wj, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("fixture %q: sealed_transaction (deterministic fields) mismatch:\n got:  %s\n want: %s", fx.Name, gj, wj)
	}
}

// canonicalizeJSON unmarshals raw JSON to an interface{} (encoding/json sorts object
// keys when re-marshalling maps, and reflect.DeepEqual is order-insensitive over maps),
// giving an order-insensitive structural value for the parse-vector compare.
func canonicalizeJSON(t *testing.T, raw []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonicalize json: %v", err)
	}
	return v
}

// knownOperations is the set of operations runGoldenVector dispatches. The coverage-parity
// test asserts every operation present under testdata/fixtures/ is in this set, so an
// uncovered op can't slip through.
var knownOperations = map[string]bool{
	opBuildAndEncodePublicTransfer:   true,
	opResolveAndEncodePublicTransfer: true,
	opParseFinalizedResult:           true,
	opBuildAndEncodeStealthTransfer:  true,
	opBuildStealthOutputsStatement:   true,
	opScanStealthOutput:              true,
	opDecodeStealthUTXO:              true,
	opDeriveAccountKeyFromSeed:       true,
	opDeriveViewKeyFromSeed:          true,
	opDeriveAccountAddress:           true,
	opFormatIdentityAddress:          true,
	opParseAddress:                   true,
	opDecodeSubstate:                 true,
	opAccountBalances:                true,
	opBuildAndEncodeInstructions:     true,
	opBuildAndEncodeFaucetClaim:      true,
	opEncodeArg:                      true,
	opCosignSealWithAuth:             true,
}

// TestGoldenVectors_CoverageParity asserts every operation appearing in the vendored
// fixtures has a runner arm. A new core vector with an unhandled operation fails here
// (in addition to the per-vector default arm).
func TestGoldenVectors_CoverageParity(t *testing.T) {
	for _, entry := range loadGoldenFixtures(t) {
		if !knownOperations[entry.fx.Operation] {
			t.Errorf("fixture %s has operation %q with no Go runner arm", entry.rel, entry.fx.Operation)
		}
	}
}

// TestGoldenVectors_UnknownOperationFails proves an unknown operation is a hard failure
// (not a skip): the dispatcher's default arm calls t.Fatalf, so a new core vector cannot
// go unverified.
func TestGoldenVectors_UnknownOperationFails(t *testing.T) {
	ft := &fatalRecorder{}
	dispatchUnknown(ft, goldenFixture{Name: "synthetic/bogus", Operation: "no_such_operation"})
	if !ft.fatal {
		t.Fatal("an unknown operation must fail the suite, but the dispatcher did not call Fatal")
	}
}

// fatalRecorder is a tiny testing.TB stub that records whether Fatalf/Fatal was called,
// so TestGoldenVectors_UnknownOperationFails can assert the unknown-op arm fails without
// actually failing the test.
type fatalRecorder struct {
	testing.TB
	fatal bool
}

func (f *fatalRecorder) Helper()                           {}
func (f *fatalRecorder) Fatalf(format string, args ...any) { f.fatal = true }
func (f *fatalRecorder) Fatal(args ...any)                 { f.fatal = true }

// dispatchUnknown runs the same switch as runGoldenVector against a TB stub. Kept in
// lockstep with runGoldenVector's default arm (both must Fatal on an unknown op).
func dispatchUnknown(tb testing.TB, fx goldenFixture) {
	switch fx.Operation {
	case opBuildAndEncodePublicTransfer, opResolveAndEncodePublicTransfer, opParseFinalizedResult,
		opBuildAndEncodeStealthTransfer, opBuildStealthOutputsStatement, opScanStealthOutput,
		opDecodeStealthUTXO,
		opDeriveAccountKeyFromSeed, opDeriveViewKeyFromSeed, opDeriveAccountAddress,
		opFormatIdentityAddress, opParseAddress,
		opDecodeSubstate, opAccountBalances, opBuildAndEncodeInstructions, opBuildAndEncodeFaucetClaim,
		opEncodeArg, opCosignSealWithAuth:
		// covered elsewhere
	default:
		tb.Fatalf("fixture %q: unknown operation %q", fx.Name, fx.Operation)
	}
}

// TestFixtureDrift asserts the vendored fixtures are byte-identical to the source: if the
// source is regenerated and the vendored copy is not re-synced (make sync-fixtures), this
// fails.
//
// The source path is OOTLE_MONOREPO (default ../../tari-ootle, relative to this package
// dir). If it isn't present (e.g. a CI job without the sibling checkout), the check is
// skipped with a clear message — but when it is present, any divergence is a hard failure.
func TestFixtureDrift(t *testing.T) {
	monorepo := os.Getenv("OOTLE_MONOREPO")
	if monorepo == "" {
		// Relative to the test's CWD (this package dir, ./ootle), the sibling source
		// checkout is two levels up: ootle-go/ootle -> ootle-go -> <parent>/tari-ootle.
		monorepo = filepath.Join("..", "..", "tari-ootle")
	}
	srcDir := filepath.Join(monorepo, "crates", "ootle_sdk_core", "fixtures")
	if _, err := os.Stat(srcDir); err != nil {
		t.Skipf("source fixtures not found at %s (set OOTLE_MONOREPO to enable the drift check); SKIPPING — vendored copy not verified against source", srcDir)
	}

	vendored := vendoredFixturesDir()

	// Every source *.json must have a byte-identical vendored counterpart.
	srcFiles := jsonFilesRel(t, srcDir)
	vendFiles := jsonFilesRel(t, vendored)

	for rel := range srcFiles {
		if _, ok := vendFiles[rel]; !ok {
			t.Errorf("drift: source fixture %q is not vendored under %s — run `make sync-fixtures`", rel, vendored)
			continue
		}
		srcBytes, err := os.ReadFile(filepath.Join(srcDir, rel))
		if err != nil {
			t.Fatalf("read source %s: %v", rel, err)
		}
		vendBytes, err := os.ReadFile(filepath.Join(vendored, rel))
		if err != nil {
			t.Fatalf("read vendored %s: %v", rel, err)
		}
		if string(srcBytes) != string(vendBytes) {
			t.Errorf("drift: vendored fixture %q differs from the source (%d vs %d bytes) — run `make sync-fixtures`", rel, len(vendBytes), len(srcBytes))
		}
	}
	// No vendored fixture may exist without a source (a stale leftover).
	for rel := range vendFiles {
		if _, ok := srcFiles[rel]; !ok {
			t.Errorf("drift: vendored fixture %q has no source — run `make sync-fixtures`", rel)
		}
	}
}

// jsonFilesRel returns the set of *.json files under root, keyed by their root-relative
// path (slash-separated for stable cross-OS comparison).
func jsonFilesRel(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestDryRunSurfacesEstimatedFee parses the dry-run vector's raw_result through the C ABI
// and asserts the FinalizedResult surfaces EstimatedFee as a bare u64 (> 2^53, so a float
// coercion would have corrupted it). The dry-run field crosses the boundary intact — the
// Go host reads the same estimate the core computed.
func TestDryRunSurfacesEstimatedFee(t *testing.T) {
	path := filepath.Join("testdata", "fixtures", "parse_finalized_result", "dry_run.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dry-run fixture: %v", err)
	}
	var fx struct {
		Input struct {
			RawResult json.RawMessage `json:"raw_result"`
		} `json:"input"`
		Expected struct {
			Parsed struct {
				EstimatedFee *uint64 `json:"estimated_fee"`
			} `json:"parsed"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal dry-run fixture: %v", err)
	}
	if fx.Expected.Parsed.EstimatedFee == nil {
		t.Fatal("dry-run vector must pin an expected estimated_fee")
	}

	parsedJSON, err := cffi.ParseFinalizedResult(string(fx.Input.RawResult))
	if err != nil {
		t.Fatalf("ParseFinalizedResult: %v", err)
	}
	var got FinalizedResult
	if err := json.Unmarshal([]byte(parsedJSON), &got); err != nil {
		t.Fatalf("unmarshal FinalizedResult: %v", err)
	}
	if got.EstimatedFee == nil {
		t.Fatal("dry-run FinalizedResult must surface EstimatedFee")
	}
	if *got.EstimatedFee != *fx.Expected.Parsed.EstimatedFee {
		t.Fatalf("EstimatedFee mismatch: got %d want %d", *got.EstimatedFee, *fx.Expected.Parsed.EstimatedFee)
	}
	if *got.EstimatedFee <= (uint64(1) << 53) {
		t.Fatalf("dry-run vector should exercise a u64 above 2^53, got %d", *got.EstimatedFee)
	}
}
