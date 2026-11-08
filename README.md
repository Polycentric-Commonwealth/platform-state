# State platform

The reference platform for a **State** in the [Polycentric Commonwealth](https://github.com/Polycentric-Commonwealth/foundation): the layer that keeps the residents' register, pays every resident the Commonwealth dividend, and publishes every official act in a public ledger.

It is a starting point that each State may configure, fork or replace entirely. Anything that speaks the protocols in the [foundation](https://github.com/Polycentric-Commonwealth/foundation/tree/main/specs) repository can take its place.

## Try it

With [Go](https://go.dev/dl/) 1.24 or later installed:

```sh
go run ./cmd/pcstate dev
```

This starts the State together with a small local Commonwealth on your machine, registers 60 fictional residents, and prints the web address and an officials' password. Then:

1. Open the web interface and sign in as an official.
2. Report an eligibility count for a period. The local Commonwealth settles it within a couple of seconds.
3. Pay the settlement. Every counted resident is credited, and a signed confirmation goes back to the Commonwealth.
4. Look at the public **Dividends** page: a few seconds after the count, the Commonwealth's Audit Office checks a sample of the people counted and the result appears in the Audit column.
5. Look at the **Public ledger**, and try **My dividend** with a resident's number and date of birth from the register.

To see a State caught inflating its count, invent some residents:

```sh
go run ./cmd/pcstate dev -residents 570 -ghosts 30
```

The audit finds roughly 25 of 500 sampled people missing and charges the State about three times what it gained.

Everything is created fresh on each run (keys, database and certificates) and removed when you stop the server. Options: `-addr`, `-password`, `-residents`, `-per-person`, `-name` and `-ghosts`.

## What it does

| Area | Who uses it | What it does |
| --- | --- | --- |
| Overview, Dividends | Everyone | The State's key facts and every dividend period: count, settlement, payment and audit result |
| Public ledger | Everyone | Every record the State has issued, each checked against its signature and the witnesses' cosignatures, with a colour fingerprint for comparing copies |
| My dividend | Residents | A resident's own payments |
| Dividend cycle | Officials | Report eligibility counts, pay settlements, resend undelivered records |
| Residents | Officials | Register residents and mark them active or inactive |
| Ledger | Officials | Publish a checkpoint and collect witness cosignatures |
| `/pc/inbox`, `/pc/ledger/…`, `/.well-known/pc-instance` | Other instances | The [integration protocol](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/integration-protocol.md) and [public ledger](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/public-ledger.md) endpoints |

## How it is built

- **Go**, with the [foundation](https://github.com/Polycentric-Commonwealth/foundation) library for signed records, the instance registry, the protocol, dividends and the ledger.
- **SQLite** through `modernc.org/sqlite`, a pure-Go driver, so the platform still builds to a single file with no C toolchain or database server. It is the only dependency outside Go and the foundation library.
- **Server-rendered HTML** with one small stylesheet and no JavaScript. Pages work on low-end phones, and a strict Content Security Policy blocks injected scripts.

| Package | Role |
| --- | --- |
| [`internal/store`](internal/store/) | The database: residents, sent and received records, eligibility snapshots, settlements, payments and ledger entries |
| [`internal/state`](internal/state/) | The State's core: reporting counts, checking settlements, paying, logging and witnessing |
| [`internal/web`](internal/web/) | Public pages, the resident lookup and the officials' workspace |
| [`internal/devnet`](internal/devnet/) | The local Commonwealth (with a stand-in Audit Office and simulated field checks) and sample residents used by `pcstate dev` and the tests |
| [`cmd/pcstate`](cmd/pcstate/) | The command |

### Where personal data lives

Residents, the list of who each count included, and individual payments stay in this State's database. Only totals leave it: eligibility counts and payment confirmations carry numbers, never names. The ledger holds only records the State sends, so it contains no personal data either.

The one exception is an [eligibility audit](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/eligibility-audit.md). When the Audit Office samples a count, the State reveals the name and date of birth of the sampled residents only (at most 600), so auditors can find them. That answer is sent to the Audit Office alone and is never logged.

### How a dividend period works

1. An official reports a period. The State snapshots its active residents, commits to that exact list with a salted Merkle root, sends the counts and the root to the Commonwealth, and logs the signed count in its ledger.
2. The Commonwealth replies with a signed settlement. The State accepts it only if it matches the count it sent, and only once.
3. An official pays it. In one database transaction, every resident in the snapshot is credited. A resident who leaves after the count is still paid for that period.
4. The State sends and logs a signed confirmation of the totals paid.
5. The Audit Office samples the committed list. The State answers automatically, opening only the sampled entries, and receives a signed finding: passed, or a repayment of three times the estimated overpayment.

Each logged record triggers a new checkpoint, which the witnesses cosign. If a record cannot be delivered, it stays stored and logged, and officials can send it again.

## Not yet built

This version runs only as a local development network. Before a real State could use it, it needs:

- **Production mode:** persistent keys, configuration, joining a real registry, and fetching other instances' descriptions.
- **Real identity:** residents currently look up payments with their resident number and date of birth, which is a development placeholder. Officials share one password; real deployments need individual accounts and roles.
- **Real payments:** dividends are credited to a record in the database, not sent through a bank or payment system.
- **Real audits:** the dev network's Audit Office "verifies" people by looking them up in the State's own database. A real one sends auditors into the field, and repayments are not yet deducted from anything.
- **Persistent checkpoints:** the ledger's entries are stored in SQLite, but its checkpoints and cosignatures are held in memory by the foundation library and are lost on restart.
- **The other State modules** in the [platforms plan](https://github.com/Polycentric-Commonwealth/foundation/blob/main/docs/design/platforms-plan.md#6-state-platform): legislature, law registry, elections, taxes and rents, courts, migration and more.

## Licence

Code is licensed under the [Apache License 2.0](LICENSE). See the foundation repository for the project's licensing decisions.
