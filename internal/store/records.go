package store

import (
	"database/sql"
	"encoding/json"

	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/record"
)

// StoredRecord is a signed record the State sent or received.
type StoredRecord struct {
	Envelope  *record.Envelope
	Direction string
	Peer      string
	Delivery  string // for sent records: pending, accepted, duplicate or rejected: reason
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertRecord(x execer, env *record.Envelope, direction, peer string, ignoreDuplicate bool) error {
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	verb := "INSERT"
	if ignoreDuplicate {
		verb = "INSERT OR IGNORE"
	}
	delivery := ""
	if direction == "sent" {
		delivery = "pending"
	}
	_, err = x.Exec(verb+` INTO records (id, direction, type, peer, issued_at, body, stored_at, delivery) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		env.ID, direction, env.Type, peer, env.IssuedAt, body, now(), delivery)
	return err
}

// SetDelivery records the outcome of delivering a sent record.
func (s *Store) SetDelivery(id, status string) error {
	_, err := s.db.Exec(`UPDATE records SET delivery = ? WHERE id = ? AND direction = 'sent'`, status, id)
	return err
}

// PendingDeliveries lists sent records that have no receipt yet.
func (s *Store) PendingDeliveries() ([]StoredRecord, error) {
	return s.query(`SELECT body, direction, peer, delivery FROM records WHERE direction = 'sent' AND delivery = 'pending' ORDER BY issued_at`)
}

// SaveSent stores a record the State sent.
func (s *Store) SaveSent(env *record.Envelope) error {
	return insertRecord(s.db, env, "sent", env.To, false)
}

// Record returns a stored record by id.
func (s *Store) Record(id string) (*StoredRecord, error) {
	var body []byte
	var r StoredRecord
	err := s.db.QueryRow(`SELECT body, direction, peer, delivery FROM records WHERE id = ?`, id).Scan(&body, &r.Direction, &r.Peer, &r.Delivery)
	if err != nil {
		return nil, notFound(err)
	}
	if r.Envelope, err = record.Parse(body); err != nil {
		return nil, err
	}
	return &r, nil
}

// Records lists stored records of a type, newest first.
func (s *Store) Records(typ string, limit int) ([]StoredRecord, error) {
	return s.query(`SELECT body, direction, peer, delivery FROM records WHERE type = ? ORDER BY issued_at DESC, id DESC LIMIT ?`, typ, limit)
}

func (s *Store) query(q string, args ...any) ([]StoredRecord, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredRecord
	for rows.Next() {
		var body []byte
		var r StoredRecord
		if err := rows.Scan(&body, &r.Direction, &r.Peer, &r.Delivery); err != nil {
			return nil, err
		}
		if r.Envelope, err = record.Parse(body); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Inbox adapts the store to protocol.Store, so received records are kept
// and duplicates detected across restarts.
type Inbox struct{ S *Store }

// Seen reports whether a record with this id was received before.
func (i Inbox) Seen(id string) (bool, error) {
	var n int
	err := i.S.db.QueryRow(`SELECT COUNT(*) FROM records WHERE id = ? AND direction = 'received'`, id).Scan(&n)
	return n > 0, err
}

// Accept stores a received record. A handler may already have stored it in
// the same transaction as its own data, so duplicates are ignored here.
func (i Inbox) Accept(env *record.Envelope) error {
	return insertRecord(i.S.db, env, "received", env.Issuer, true)
}

// Ledger adapts the store to ledger.Storage.
type Ledger struct{ S *Store }

// Append stores an entry at the next index.
func (l Ledger) Append(entry []byte, leaf ledger.Hash) (uint64, error) {
	tx, err := l.S.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n uint64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM ledger_entries`).Scan(&n); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO ledger_entries (idx, entry, leaf) VALUES (?, ?, ?)`, n, entry, leaf[:]); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Size returns the number of entries.
func (l Ledger) Size() (uint64, error) {
	var n uint64
	err := l.S.db.QueryRow(`SELECT COUNT(*) FROM ledger_entries`).Scan(&n)
	return n, err
}

// Entry returns the entry at index.
func (l Ledger) Entry(i uint64) ([]byte, error) {
	var entry []byte
	err := l.S.db.QueryRow(`SELECT entry FROM ledger_entries WHERE idx = ?`, i).Scan(&entry)
	return entry, notFound(err)
}

// LeafHashes returns the leaf hashes of the first n entries.
func (l Ledger) LeafHashes(n uint64) ([]ledger.Hash, error) {
	rows, err := l.S.db.Query(`SELECT leaf FROM ledger_entries WHERE idx < ? ORDER BY idx`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ledger.Hash, 0, n)
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var h ledger.Hash
		copy(h[:], b)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if uint64(len(out)) != n {
		return nil, ErrNotFound
	}
	return out, nil
}
