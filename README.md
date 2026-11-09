# State platform

The reference platform for a **State** in the [Polycentric Commonwealth](https://github.com/Polycentric-Commonwealth/foundation): the layer that keeps the residents' register, pays every resident the Commonwealth dividend, and publishes every official act in a public ledger.

It is a starting point that each State may configure, fork or replace entirely. Anything that speaks the protocols in the [foundation](https://github.com/Polycentric-Commonwealth/foundation/tree/main/specs) repository can take its place.

## Try it

With [Go](https://go.dev/dl/) 1.24 or later installed:

```sh
go run ./cmd/pcstate dev
```

This starts the State together with a small local Commonwealth on your machine, registers 60 fictional residents, and prints the web address and the password of an `admin` official. Then:

1. Open the web interface and sign in as `admin`.
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

## Deploy it

A deployment keeps its key, configuration, attestations and database in one directory, discovers other instances from their published descriptions, and keeps serving while they are unreachable.

```sh
go build ./cmd/pcstate

# 1. Create the deployment. The anchor is the Commonwealth's pinned public key.
./pcstate init -dir /srv/pcstate -instance state.example -name "Example State" \
  -base-url https://state.example/ -listen :8443 -tls-cert cert.pem -tls-key key.pem \
  -parent commonwealth.example -anchor commonwealth.example=ed25519:KEY

# 2. Send /srv/pcstate/description.json to the parent (a Region or the Commonwealth).
#    It checks the key with you out of band and returns an attestation.
./pcstate attestation add -dir /srv/pcstate attestation.json

# 3. Run it.
./pcstate serve -dir /srv/pcstate
```

`init` prints the password of the first admin official, once. Without `-tls-cert`, the platform serves plain HTTP and expects a TLS proxy in front; `-base-url` must be the public https address either way. Other flags: `-witness` (repeatable) and `-witness-threshold` choose who cosigns the ledger, `-peer ID=https://host:port` reaches an instance at an address other than its domain name, and `-ca-file` trusts extra certificates.

### Officials

Every official has their own account with one role:

| Role | Can |
| --- | --- |
| `registrar` | Keep the residents' register |
| `treasurer` | Report counts, pay settlements, resend records and publish the ledger |
| `admin` | Everything, and manage accounts |

Every official can view every officials' page. Admins add, reset and disable accounts on the **Officials** page, or from the command line with `pcstate official add|reset|disable|list -dir DIR`. Passwords are generated, shown once and stored as PBKDF2-SHA256 hashes. Each action is recorded with the official who took it; records sent to other instances are still signed by the State.

### Trying a deployment

The [Commonwealth platform](https://github.com/Polycentric-Commonwealth/platform-commonwealth) can be deployed next to it: its officials attest the State, settle periods and run audits. For a quicker test, `pcstate devcommonwealth` runs a stand-in Commonwealth on its own: it attests States, settles every count, audits it (passing every valid opening, since it cannot see a State's register) and witnesses ledgers.

```sh
./pcstate devcommonwealth run -dir cw -base-url https://127.0.0.1:9443/ -listen 127.0.0.1:9443 \
  -tls-cert cert.pem -tls-key key.pem -ca-file cert.pem \
  -peer state.example=https://127.0.0.1:8443     # prints the -anchor for pcstate init
./pcstate devcommonwealth attest -dir cw /srv/pcstate/description.json > attestation.json
```

The test `internal/deploy` runs the whole arrangement: a stand-in Commonwealth and two deployed States over HTTPS, one witnessing the other's ledger, through a dividend cycle, an audit and a restart.

## What it does

| Area | Who uses it | What it does |
| --- | --- | --- |
| Overview, Dividends | Everyone | The State's key facts and every dividend period: count, settlement, payment and audit result |
| Public ledger | Everyone | Every record the State has issued, each checked against its signature and the witnesses' cosignatures, with a colour fingerprint for comparing copies |
| My dividend | Residents | A resident's own payments |
| Dividend cycle | Officials | Report eligibility counts, pay settlements, resend undelivered records |
| Residents | Officials | Register residents and mark them active or inactive |
| Ledger | Officials | Publish a checkpoint and collect witness cosignatures |
| Officials | Officials | Accounts, roles and the activity log |
| `/pc/inbox`, `/pc/ledger/…`, `/.well-known/pc-instance`, `/.well-known/pc-attestations` | Other instances | The [integration protocol](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/integration-protocol.md) and [public ledger](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/public-ledger.md) endpoints |

## How it is built

- **Go**, with the [foundation](https://github.com/Polycentric-Commonwealth/foundation) library for signed records, the instance registry, the protocol, dividends and the ledger.
- **SQLite** through `modernc.org/sqlite`, a pure-Go driver, so the platform still builds to a single file with no C toolchain or database server. It is the only dependency outside Go and the foundation library.
- **Server-rendered HTML** with one small stylesheet and no JavaScript. Pages work on low-end phones, and a strict Content Security Policy blocks injected scripts.

| Package | Role |
| --- | --- |
| [`internal/store`](internal/store/) | The database: residents, records, eligibility snapshots, settlements, payments, the ledger with its checkpoints and cosignatures, officials and sessions |
| [`internal/state`](internal/state/) | The State's core: reporting counts, checking settlements, paying, logging and witnessing |
| [`internal/web`](internal/web/) | Public pages, the resident lookup and the officials' workspace |
| [`internal/deploy`](internal/deploy/) | Deployments: configuration, keys, attestations, discovery and serving |
| [`internal/devnet`](internal/devnet/) | The stand-in Commonwealth (with an Audit Office and simulated field checks), sample residents, and the development network used by `pcstate dev`, `pcstate devcommonwealth` and the tests |
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

Deployments work, but before a real State could rely on this, it needs:

- **Resident sign-in:** residents look up payments with their resident number and date of birth, a placeholder. The plan is passkeys enrolled with a one-time code ([federated identity](https://github.com/Polycentric-Commonwealth/foundation/blob/main/specs/federated-identity.md#signing-in)). Officials have individual accounts but no second factor yet.
- **Uniqueness and credentials:** registering residents' uniqueness tokens and issuing them credentials, using the foundation's `uniqueness` and `credential` packages.
- **Real payments:** dividends are credited to a record in the database, not sent through a bank or payment system.
- **Real audits:** the stand-in Audit Office passes every valid opening, or in `pcstate dev` looks people up in the State's own database. A real one sends auditors into the field, and repayments are not yet deducted from anything.
- **Key rotation and backups:** commands to rotate the signing key and to back up and restore a deployment.
- **The other State modules** in the [platforms plan](https://github.com/Polycentric-Commonwealth/foundation/blob/main/docs/design/platforms-plan.md#6-state-platform): legislature, law registry, elections, taxes and rents, courts, migration and more.

## Licence

Code is licensed under the [Apache License 2.0](LICENSE). See the foundation repository for the project's licensing decisions.
