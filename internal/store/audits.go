package store

import (
	"github.com/polycentric-commonwealth/foundation/audit"
	"github.com/polycentric-commonwealth/foundation/record"
)

// Audit is an eligibility audit of one of this State's counts.
type Audit struct {
	Request  string
	Count    string
	Response *StoredRecord // nil until the State has answered
	Finding  *audit.Finding
}

// SaveAuditRequest stores a received audit request for a count.
func (s *Store) SaveAuditRequest(env *record.Envelope, countID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "received", env.Issuer, false); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audits (request, count) VALUES (?, ?)`, env.ID, countID); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveAuditResponse stores the State's answer to an audit request. It holds
// personal data for the sampled residents and is never logged.
func (s *Store) SaveAuditResponse(requestID string, env *record.Envelope) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "sent", env.To, false); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE audits SET response = ? WHERE request = ?`, env.ID, requestID); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveFinding stores a received audit finding.
func (s *Store) SaveFinding(env *record.Envelope, requestID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "received", env.Issuer, false); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE audits SET finding = ? WHERE request = ? AND finding IS NULL`, env.ID, requestID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrState
	}
	return tx.Commit()
}

// AuditByRequest returns an audit by its request id.
func (s *Store) AuditByRequest(requestID string) (*Audit, error) {
	return s.audit(`SELECT request, count, COALESCE(response, ''), COALESCE(finding, '') FROM audits WHERE request = ?`, requestID)
}

// AuditForCount returns the most recent audit of a count.
func (s *Store) AuditForCount(countID string) (*Audit, error) {
	return s.audit(`SELECT a.request, a.count, COALESCE(a.response, ''), COALESCE(a.finding, '') FROM audits a
		JOIN records r ON r.id = a.request WHERE a.count = ? ORDER BY r.issued_at DESC LIMIT 1`, countID)
}

func (s *Store) audit(q string, arg string) (*Audit, error) {
	var a Audit
	var response, finding string
	if err := s.db.QueryRow(q, arg).Scan(&a.Request, &a.Count, &response, &finding); err != nil {
		return nil, notFound(err)
	}
	if response != "" {
		r, err := s.Record(response)
		if err != nil {
			return nil, err
		}
		a.Response = r
	}
	if finding != "" {
		r, err := s.Record(finding)
		if err != nil {
			return nil, err
		}
		if a.Finding, err = audit.DecodeFinding(r.Envelope); err != nil {
			return nil, err
		}
	}
	return &a, nil
}
