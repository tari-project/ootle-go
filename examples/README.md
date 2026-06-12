# Examples

Runnable programs that demonstrate the `ootle-go` SDK. Each reads its configuration from
environment variables, so you can run any of them with a single `go run` (or
`make example NAME=<dir>`).

## Prerequisites

- **Native lib.** The examples import `ootle` → `internal/cffi`, so the vendored native
  static lib must be present to compile them. If `go run`/`go build` fails to link, run
  `make native` once (see the [root README](../README.md#installbuild)).
- **A live indexer.** Most examples fund and submit transactions against a running indexer.
  Two examples are read-only exceptions: `watch_events` only subscribes to the event stream,
  and `arg_dsl` runs fully offline (no indexer at all). A local swarm provides an indexer for
  the rest — see the root README for standing up `tari_swarm_daemon`.

Most examples self-fund: they mint a fresh throwaway identity and claim from the faucet, so
no pre-funded account is required.

## Environment variables

| Var | Used by | Default |
|-----|---------|---------|
| `OOTLE_INDEXER_URL` | all | `http://127.0.0.1:18300` |
| `OOTLE_NETWORK` | all | `localnet` |
| `OOTLE_TARI_RESOURCE` | balance / transfer / stealth | none (no safe default; balance-delta checks are skipped when unset) |
| `OOTLE_TEMPLATE_WASM` | `publish_template` | none (path to a compiled `.wasm`; skip if unset) |
| `OOTLE_COUNTER_TEMPLATE` | `counter_deploy` | none (`template_<hex>`; skip if unset) |
| `OOTLE_STABLECOIN_TEMPLATE` / `_COMPONENT` | `template_invoke` | none (skip if neither set) |

| Example | Purpose | Read-only? | Env vars |
|---|---|---|---|
| [`watch_events`](./watch_events) | Stream typed template events from the indexer over SSE (`Client.WatchEvents`) and print them as they arrive, with automatic reconnect/resume. | Yes | `OOTLE_INDEXER_URL` (default `http://127.0.0.1:18300`), `OOTLE_COMPONENT_ADDRESS` (optional `component_<hex>` filter), `OOTLE_EVENT_TOPIC` (optional exact-topic filter) |
| [`balance_query`](./balance_query) | Fund a fresh identity, then read its TARI revealed balance and every resource balance back from the indexer (`Client.AccountBalance` / `Client.AccountBalances`). | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (asserts a positive balance when set) |
| [`fungible_transfer`](./fungible_transfer) | Dry-run fee estimate, then a real single-recipient TARI transfer to a fresh recipient, asserting the commit and both balance deltas. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required**; skips cleanly when unset) |
| [`dry_run`](./dry_run) | Fee estimation only — a valid dry-run (prints `EstimatedFee`) and an over-spend dry-run (prints the reject reason); never submits a real transfer. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required**; skips cleanly when unset) |
| [`workspace_chain`](./workspace_chain) | Dry-run, then pipe a bucket through the workspace (withdraw → put-on-workspace → deposit) plus an atomic `CreateAccount`, all in one generic transaction; finds the new `component_` in the diff. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required**; skips cleanly when unset) |
| [`manual_co_signing`](./manual_co_signing) | Two-party co-sign hand-off: A builds + resolves + ships the unsigned record, both A and B authorize it (the seal signer carries no authority on the cosign path), A attaches both + seals + submits via `Client.SubmitSealed`. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required**; skips cleanly when unset) |
| [`stealth/faucet_deposit`](./stealth/faucet_deposit) | Confidential deposit to self funded by a revealed input, then fetch the created UTXO and decrypt it with the view secret (`SendStealthTransfer` → `ScanStealthSubstate`), asserting `IsMine` and the recovered value. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required** stealth-capable resource; skips cleanly when unset) |
| [`stealth/to_revealed`](./stealth/to_revealed) | Revealed input → revealed output through the stealth builder (degenerate proof, no confidential value); asserts the engine commits it. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required** stealth-capable resource; skips cleanly when unset) |
| [`stealth/to_stealth`](./stealth/to_stealth) | Mixed transfer: one confidential stealth output to a fresh recipient plus revealed change to self; asserts the commit and scans the recipient's UTXO to verify the confidential value. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required** stealth-capable resource; skips cleanly when unset) |
| [`stealth/spend_utxo`](./stealth/spend_utxo) | Full lifecycle: deposit a stealth UTXO to self, decrypt it, then spend it as a confidential input. Deposit + scan are hard-asserted; the spend reports its commit outcome. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TARI_RESOURCE` (**required** stealth-capable resource; skips cleanly when unset) |
| [`publish_template`](./publish_template) | Publish a WASM template: dry-run the publish (prints the fee), publish it, find the new `template_` in the diff, and print an `OOTLE_COUNTER_TEMPLATE=` line for the others. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_TEMPLATE_WASM` (**required** path to a `.wasm`; skips cleanly when unset) |
| [`counter_deploy`](./counter_deploy) | Deploy a Counter from a template, then `increase()` it — **two transactions**, since the newly created `component_` (read from tx1's diff) is the receiver of tx2; decodes the component afterwards. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_COUNTER_TEMPLATE` (**required** `template_<hex>`; skips cleanly when unset) |
| [`template_invoke`](./template_invoke) | Instantiate a stablecoin template with typed args (admin badge deposited via the workspace) or attach to an existing component, then call `total_supply()`. | No | `OOTLE_INDEXER_URL`, `OOTLE_NETWORK`, `OOTLE_STABLECOIN_TEMPLATE` / `OOTLE_STABLECOIN_COMPONENT` (**required** — one or the other; skips cleanly when neither set) |
| [`arg_dsl`](./arg_dsl) | Build a template-call instruction using the typed argument DSL (`ArgI64`, `ArgNonFungibleID` + the `NonFungible*` helpers, `ArgList`, `ArgSome`/`ArgNone`), marshal it, and print the wire JSON. | Yes | none (offline; needs no indexer) |

### `OOTLE_TARI_RESOURCE` caveat

There is no safe built-in default for the TARI/XTR resource address, so it has none. The
balance, transfer, and stealth examples still **commit** their transaction without it, but
**skip their balance-delta assertion** (with a log line) when it is unset. Set it to the
resource address on your network to exercise the full assert path.

## Running

```sh
go run ./examples/watch_events
```

`watch_events` is **read-only** — it only subscribes to the indexer's event stream and never
funds, signs, or submits anything, so it is safe to run against any indexer. Point it at a
running indexer, optionally narrow it with a component address or topic, then drive a
transaction from another terminal (e.g. a faucet claim or `counter.increase()`) and watch
matching event frames print. Press Ctrl-C to stop; the watch is cancelled cleanly.

The self-funding, URL-only examples need no extra config beyond the indexer URL:

```sh
go run ./examples/balance_query
go run ./examples/fungible_transfer
go run ./examples/dry_run
go run ./examples/workspace_chain
go run ./examples/manual_co_signing
```

### Bootstrap order (artifact-gated examples)

The template examples depend on artifacts produced by an earlier run. Run them in order:

1. **Publish a template.** Point `OOTLE_TEMPLATE_WASM` at a compiled `.wasm` and run
   `publish_template`; it prints an `OOTLE_COUNTER_TEMPLATE=template_<hex>` line for the
   published address.

   ```sh
   OOTLE_TEMPLATE_WASM=/path/to/counter.wasm go run ./examples/publish_template
   ```

2. **Deploy from that template.** Export the printed address and run `counter_deploy`:

   ```sh
   OOTLE_COUNTER_TEMPLATE=template_<hex> go run ./examples/counter_deploy
   ```

3. **Invoke a template.** `template_invoke` either instantiates from
   `OOTLE_STABLECOIN_TEMPLATE` or attaches to an existing `OOTLE_STABLECOIN_COMPONENT`.

Each artifact-gated example skips cleanly (logs and exits 0) when its required env var is
unset, so running them without the artifacts is harmless.

### Live smoke test

A build-tagged smoke test drives the URL-only examples above against a live indexer. It is
compiled only under the `e2e` tag and additionally skips unless `OOTLE_E2E=1`, so the
default `go test ./...` never builds or runs it:

```sh
OOTLE_E2E=1 OOTLE_INDEXER_URL=<url> OOTLE_TARI_RESOURCE=<res> \
  go test -tags e2e -run TestExamplesSmoke -v ./examples/...
```
