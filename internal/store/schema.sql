-- Civil registry. Personal data: never sent to other instances.
CREATE TABLE IF NOT EXISTS residents (
    id            TEXT PRIMARY KEY,           -- resident number, e.g. R-000123
    name          TEXT NOT NULL,
    birth_date    TEXT NOT NULL,              -- YYYY-MM-DD
    registered_at TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive'))
);

-- Every signed record the State sent or received, as JSON.
CREATE TABLE IF NOT EXISTS records (
    id         TEXT PRIMARY KEY,
    direction  TEXT NOT NULL CHECK (direction IN ('sent', 'received')),
    type       TEXT NOT NULL,
    peer       TEXT NOT NULL,
    issued_at  TEXT NOT NULL,
    body       BLOB NOT NULL,
    stored_at  TEXT NOT NULL,
    delivery   TEXT NOT NULL DEFAULT ''     -- for sent records: pending, accepted, duplicate or rejected
);
CREATE INDEX IF NOT EXISTS records_type ON records (type, issued_at);

-- Which residents an eligibility count included, so the payment run pays
-- exactly the people counted, and the committed recipient list behind the
-- count's recipients_root, so the State can answer an audit. Salts are
-- secret until an auditor samples their entry.
CREATE TABLE IF NOT EXISTS eligibility_snapshots (
    count_record TEXT NOT NULL REFERENCES records (id),
    resident_id  TEXT NOT NULL REFERENCES residents (id),
    position     INTEGER NOT NULL,
    salt         BLOB NOT NULL,
    commitment   BLOB NOT NULL,
    PRIMARY KEY (count_record, resident_id),
    UNIQUE (count_record, position)
);

-- Eligibility audits of this State's counts.
CREATE TABLE IF NOT EXISTS audits (
    request   TEXT PRIMARY KEY REFERENCES records (id),
    count     TEXT NOT NULL REFERENCES records (id),
    response  TEXT REFERENCES records (id),
    finding   TEXT REFERENCES records (id)
);

CREATE TABLE IF NOT EXISTS settlements (
    record_id     TEXT PRIMARY KEY REFERENCES records (id),
    period        TEXT NOT NULL,
    basis         TEXT NOT NULL REFERENCES records (id),
    unit          TEXT NOT NULL,
    per_person    TEXT NOT NULL,
    recipients    INTEGER NOT NULL,
    total         TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'received' CHECK (status IN ('received', 'paid')),
    confirmation  TEXT REFERENCES records (id)
);

-- Individual dividend payments. Personal data.
CREATE TABLE IF NOT EXISTS payments (
    settlement  TEXT NOT NULL REFERENCES settlements (record_id),
    resident_id TEXT NOT NULL REFERENCES residents (id),
    amount      TEXT NOT NULL,
    unit        TEXT NOT NULL,
    paid_at     TEXT NOT NULL,
    PRIMARY KEY (settlement, resident_id)
);

-- The State's public ledger.
CREATE TABLE IF NOT EXISTS ledger_entries (
    idx   INTEGER PRIMARY KEY,
    entry BLOB NOT NULL,
    leaf  BLOB NOT NULL
);

-- The ledger's signed checkpoints and the witnesses' cosignatures, so
-- evidence survives a restart.
CREATE TABLE IF NOT EXISTS ledger_checkpoints (
    seq  INTEGER PRIMARY KEY,
    id   TEXT NOT NULL UNIQUE,
    body BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS ledger_cosignatures (
    checkpoint TEXT NOT NULL REFERENCES ledger_checkpoints (id),
    witness    TEXT NOT NULL,
    body       BLOB NOT NULL,
    PRIMARY KEY (checkpoint, witness)
);

-- What this State, acting as a witness, last cosigned for each log.
CREATE TABLE IF NOT EXISTS witnessed_logs (
    origin TEXT PRIMARY KEY,
    size   INTEGER NOT NULL,
    root   BLOB NOT NULL
);

-- Officials' accounts. Every official has their own account and one role.
CREATE TABLE IF NOT EXISTS officials (
    username      TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'registrar', 'treasurer')),
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    disabled      INTEGER NOT NULL DEFAULT 0
);

-- Signed-in sessions. Only a hash of each session token is stored.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash BLOB PRIMARY KEY,
    username   TEXT NOT NULL REFERENCES officials (username),
    expires_at TEXT NOT NULL
);

-- Which official did what, for the State's own accountability. Internal:
-- official records sent to other instances are signed by the instance.
CREATE TABLE IF NOT EXISTS official_actions (
    id       INTEGER PRIMARY KEY,
    at       TEXT NOT NULL,
    username TEXT NOT NULL,
    action   TEXT NOT NULL,
    detail   TEXT NOT NULL DEFAULT ''
);

-- Small values the platform keeps between restarts, such as the secret for
-- form tokens.
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
