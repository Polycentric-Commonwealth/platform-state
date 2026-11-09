package store

import (
	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/record"
)

// SaveCheckpoint stores a signed checkpoint of the State's ledger. It
// implements ledger.CheckpointStorage.
func (l Ledger) SaveCheckpoint(env *record.Envelope) error {
	body, err := env.Marshal()
	if err != nil {
		return err
	}
	_, err = l.S.db.Exec(`INSERT INTO ledger_checkpoints (id, body) VALUES (?, ?)`, env.ID, body)
	return err
}

// SaveCosignature stores a witness's cosignature of a stored checkpoint.
func (l Ledger) SaveCosignature(checkpoint string, cos *record.Envelope) error {
	body, err := cos.Marshal()
	if err != nil {
		return err
	}
	_, err = l.S.db.Exec(`INSERT OR REPLACE INTO ledger_cosignatures (checkpoint, witness, body) VALUES (?, ?, ?)`, checkpoint, cos.Issuer, body)
	return err
}

// Checkpoints returns every stored checkpoint, oldest first, with its
// cosignatures.
func (l Ledger) Checkpoints() ([]ledger.StoredCheckpoint, error) {
	rows, err := l.S.db.Query(`SELECT c.id, c.body, s.body FROM ledger_checkpoints c
		LEFT JOIN ledger_cosignatures s ON s.checkpoint = c.id ORDER BY c.seq, s.witness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.StoredCheckpoint
	last := ""
	for rows.Next() {
		var id string
		var cp, cos []byte
		if err := rows.Scan(&id, &cp, &cos); err != nil {
			return nil, err
		}
		if id != last {
			env, err := record.Parse(cp)
			if err != nil {
				return nil, err
			}
			out = append(out, ledger.StoredCheckpoint{Checkpoint: env})
			last = id
		}
		if cos != nil {
			env, err := record.Parse(cos)
			if err != nil {
				return nil, err
			}
			out[len(out)-1].Cosignatures = append(out[len(out)-1].Cosignatures, env)
		}
	}
	return out, rows.Err()
}

// Witnessed is the State's memory as a witness of other instances' logs. It
// implements ledger.WitnessStore. Keeping it in the database matters: a
// witness that forgot what it cosigned could be shown a forked log.
type Witnessed struct{ S *Store }

// Get returns what the State last cosigned for a log.
func (w Witnessed) Get(origin string) (ledger.WitnessState, bool, error) {
	var size uint64
	var root []byte
	err := w.S.db.QueryRow(`SELECT size, root FROM witnessed_logs WHERE origin = ?`, origin).Scan(&size, &root)
	if err := notFound(err); err == ErrNotFound {
		return ledger.WitnessState{}, false, nil
	} else if err != nil {
		return ledger.WitnessState{}, false, err
	}
	st := ledger.WitnessState{Size: size}
	copy(st.Root[:], root)
	return st, true, nil
}

// Put records a new cosigned size and root for a log.
func (w Witnessed) Put(origin string, st ledger.WitnessState) error {
	_, err := w.S.db.Exec(`INSERT INTO witnessed_logs (origin, size, root) VALUES (?, ?, ?)
		ON CONFLICT (origin) DO UPDATE SET size = excluded.size, root = excluded.root`, origin, st.Size, st.Root[:])
	return err
}
