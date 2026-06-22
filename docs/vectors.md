# Golden vectors & comparison modes

> Anti-drift is the whole point of the golden vectors: the Go SDK must reproduce the
> **same** vectors as the Rust core across the C ABI. But not every operation produces
> byte-stable output — so each fixture carries an explicit comparison mode. This document
> records what each mode asserts and which fixture group uses which.

## The single source of truth

`ootle/testdata/fixtures/` is a **byte-identical, checked-in copy** of the monorepo's
`crates/ootle_sdk_core/fixtures/`. The fixtures are generated *from the core*
(`OOTLE_REGEN_FIXTURES=1`) and re-vendored with `make sync-fixtures`. `TestFixtureDrift`
fails hard if the copy ever diverges from the source. The Go runner
(`golden_vectors_test.go`) decodes each fixture op-agnostically and dispatches on its
`operation`, honoring the per-fixture `"compare"` field.

## The two comparison modes

Each fixture declares `"compare": "bytes" | "semantic"` (absent ⇒ `"bytes"`, matching the
Rust harness's `#[serde(default = "default_compare")]`).

### `"compare": "bytes"` — byte-for-byte reproducible

The encoded output (or the decrypted scan result) is fully deterministic given the pinned
inputs, so the Go SDK must reproduce it **exactly**. The runner asserts the produced
lowercase-hex `encoded_transaction` + `transaction_id` equal the committed values
character-for-character (encode ops), or that the produced `DecryptedOutput` / not-mine
sentinel equals the committed `expected.decrypted` (scan op). A byte compare here is the
strongest possible anti-drift proof — it would catch any CBOR/encoding drift across the
boundary.

### `"compare": "semantic"` — proofs/signatures are not byte-stable

The confidential **send** path embeds an aggregated bulletproof and (for view-key outputs) an
ElGamal viewable-balance proof. Even with a pinned seed, the prover's internal `SysRng`
blinds the final scalars, so those proof bytes are **not** reproducible — and because the
seal/authorization signatures sign a digest that commits to the proofs, and the
`transaction_id` hashes the sealed bytes, the *entire* sealed transaction's bytes/id are
byte-unstable. A byte compare is therefore impossible.

**How the Rust core runner compares semantically** (the source of truth,
`crates/ootle_sdk_core/tests/golden_vectors.rs`): it re-builds + re-seals the transfer, then
(1) **decodes** the sealed BOR bytes back into a `Transaction` and **verifies every
signature** on it, and (2) compares the **decoded** transaction structurally against
`expected.sealed_transaction_semantic` with the byte-unstable fields nulled out
(`agg_range_proof`, `balance_proof`, and every Schnorr `signature` scalar — the signer
*public keys* survive, so the key-selection contract is still locked). The outputs-statement
vectors are compared the same way (validate cryptographically, then compare the statement with
`agg_range_proof` nulled + the deterministic `aggregated_output_mask`).

**How the Go runner compares semantically** (the **same** accept/reject as the Rust core
runner): the Go host (1) drives `ootle_build_and_encode_stealth_transfer_with_seed` over the
fixture input (intent + keys-with-seed + fetched UTXOs + spend secrets) to produce the seal,
then (2) hands that seal **back** across the C ABI to `ootle_validate_stealth_transfer`, which
BOR-decodes it, **verifies every signature**, and returns the decoded transaction as canonical
JSON with the byte-unstable set (`agg_range_proof`, `balance_proof`, every Schnorr `signature`
scalar) nulled — the signer *public keys* survive. The Go arm then (3) compares that canonical
JSON structurally against `expected.sealed_transaction_semantic`. This is the **full**
deterministic-field lock: a marshalling drift, a malformed intent, a bad seed, an
input-mask decryption failure, a balance-proof failure, **a bad signature, or a
deterministic-field mismatch** all fail the Go arm — exactly as they fail the Rust runner.

The value-critical crypto — BOR decode, Schnorr verification, and the shared null set (the
core's `decode_and_canonicalize_sealed_transfer`) — lives entirely in the core C fn. The
thin Go host re-ports none of it; it only marshals JSON and compares the canonical structure.

## Which fixture group uses which mode

| Group | Operation | Mode | What the Go runner asserts |
|---|---|---|---|
| `public_transfer/` (3) | `build_and_encode_public_transfer` | `bytes` | Byte-for-byte `encoded_transaction` + `transaction_id`. |
| `resolve_public_transfer/` (2) | `resolve_and_encode_public_transfer` | `bytes` | Byte-for-byte, driving build → apply → seal over the cgo seams. |
| `parse_finalized_result/` (3) | `parse_finalized_result` | `bytes`¹ | Canonicalized-structure equality of the typed `FinalizedResult`. |
| `stealth_scan/` (2) | `scan_stealth_output` | `bytes` | Exact `DecryptedOutput` (or not-mine sentinel) — decryption is RNG-free. |
| `stealth_transfer/` (2) | `build_and_encode_stealth_transfer` | `semantic` | Seal over the C ABI, then `ootle_validate_stealth_transfer` (decode + verify-all-sigs + nulled canonical JSON) compared field-for-field against `sealed_transaction_semantic` — the same full compare as the Rust runner (see above). |
| `stealth_outputs_statement/` (2) | `build_stealth_outputs_statement` | `semantic` | Drives `ootle_build_stealth_outputs_statement_with_seed` over the C ABI with the fixture's `intent` + `stealth_seed`, then compares the returned `aggregated_output_mask` byte-for-byte and the `outputs_statement` (with `agg_range_proof` nulled) field-for-field against the vendored fixture — a real cross-boundary re-execution, not just a structural sanity check. |

¹ The parse vectors omit `"compare"`, so they take the `bytes` default — but parse compares a
*canonicalized structure*, not raw bytes (there is no CBOR byte stream; serde key order is not
significant). This is the same exception the core runner makes.

## Why there is no ephemeral-seal golden vector (conclusive)

The stealth signer supports three seal cases: account-key seal, stealth `c+k` seal, and an
**ephemeral-key seal** (used for privacy when a transfer has nothing to spend). The ephemeral case
fires only when `can_sign_with_ephemeral_key()` is true — i.e. `must_sign_with_account_key == false`
(no revealed input) **and** there are no stealth inputs (`seal_signer.is_none() &&
other_signers.is_empty()`). That shape has `inputs_statement.revealed_amount == 0` **and**
`inputs_statement.inputs.is_empty()`, which the engine's `validate_transfer` pre-flight
**categorically rejects** (`crates/engine_types/src/stealth/transfer.rs:157-161`, *"No inputs or
revealed inputs provided"*). It is a cryptographic invariant: with zero public excess the balance
signature `r + e·0 = r` would leak the signing nonce.

Consequently **no balanced transaction can both trigger the ephemeral case and pass assembly**, so a
full-pipeline (build → seal → encode) ephemeral golden vector **cannot exist** — there is no
`stealth_transfer/` ephemeral fixture and there never will be. The ephemeral seal is exercised by
the core's `sign_seal.rs` unit tests **by necessity** (they hand-build the ephemeral sig-reqs against
a real, balanced unsigned tx; see the `ephemeral_partial` doc). This is a complete, intended outcome,
not a TODO. In production (reference `WalletStealthAuthorizer`) the ephemeral key is generated fresh
(`EphemeralKeySigner::random()`) for a transfer that needs a seal key but has no inputs/required
signers — and such a transfer must still be funded another way to balance, moving it back into the
account-key / `c+k` cases the send vectors already cover.

## Re-running

```sh
go test -run TestGoldenVectors ./...                                 # all vectors, per-fixture mode
OOTLE_MONOREPO=../../tari-ootle go test -run TestFixtureDrift ./...  # vendored == source (path is relative to ./ootle)
make sync-fixtures                                                   # re-vendor from the core
```
