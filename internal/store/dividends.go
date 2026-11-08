package store

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/polycentric-commonwealth/foundation/amount"
	"github.com/polycentric-commonwealth/foundation/dividend"
	"github.com/polycentric-commonwealth/foundation/record"
)

// ErrState reports an action that does not fit a settlement's status.
var ErrState = errors.New("store: not allowed in the current state")

// Snapshot returns the residents to include in an eligibility count, and
// how many of them are adults on the given day, read together so the count
// and the list agree.
func (s *Store) Snapshot(on time.Time) (ids []string, adults int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	cutoff := on.AddDate(-18, 0, 0).Format("2006-01-02")
	rows, err := tx.Query(`SELECT id, birth_date <= ? FROM residents WHERE status = 'active' ORDER BY id`, cutoff)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var adult bool
		if err := rows.Scan(&id, &adult); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
		if adult {
			adults++
		}
	}
	return ids, adults, rows.Err()
}

// SaveCount stores a sent eligibility-count record and the residents it
// counted.
func (s *Store) SaveCount(env *record.Envelope, residentIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "sent", env.To, false); err != nil {
		return err
	}
	for _, id := range residentIDs {
		if _, err := tx.Exec(`INSERT INTO eligibility_snapshots (count_record, resident_id) VALUES (?, ?)`, env.ID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasCountForPeriod reports whether an eligibility count was already sent
// for the period.
func (s *Store) HasCountForPeriod(period string) (bool, error) {
	counts, err := s.Records(dividend.EligibilityCountType, 1000)
	if err != nil {
		return false, err
	}
	for _, r := range counts {
		rejected := len(r.Delivery) >= 8 && r.Delivery[:8] == "rejected"
		if c, err := dividend.DecodeCount(r.Envelope); err == nil && c.Period == period && r.Direction == "sent" && !rejected {
			return true, nil
		}
	}
	return false, nil
}

// Settlement is a dividend settlement received from the Commonwealth.
type Settlement struct {
	RecordID     string
	IssuedAt     string
	Issuer       string
	Period       string
	Basis        string
	Unit         string
	PerPerson    string
	Recipients   int64
	Total        string
	Status       string // received or paid
	Confirmation string // id of the confirmation record, once sent
}

// SaveSettlement stores a received settlement record and its details.
func (s *Store) SaveSettlement(env *record.Envelope, d *dividend.Settlement) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "received", env.Issuer, false); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO settlements (record_id, period, basis, unit, per_person, recipients, total) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		env.ID, d.Period, d.Basis, d.Unit, d.PerPerson, d.Recipients, d.Total); err != nil {
		return err
	}
	return tx.Commit()
}

const settlementColumns = `s.record_id, r.issued_at, r.peer, s.period, s.basis, s.unit, s.per_person, s.recipients, s.total, s.status, COALESCE(s.confirmation, '')`

func scanSettlement(scan func(...any) error) (*Settlement, error) {
	var st Settlement
	err := scan(&st.RecordID, &st.IssuedAt, &st.Issuer, &st.Period, &st.Basis, &st.Unit, &st.PerPerson, &st.Recipients, &st.Total, &st.Status, &st.Confirmation)
	return &st, err
}

// Settlements lists received settlements, newest first.
func (s *Store) Settlements() ([]Settlement, error) {
	rows, err := s.db.Query(`SELECT ` + settlementColumns + ` FROM settlements s JOIN records r ON r.id = s.record_id ORDER BY r.issued_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Settlement
	for rows.Next() {
		st, err := scanSettlement(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, rows.Err()
}

// SettlementFor returns a settlement by record id.
func (s *Store) SettlementFor(id string) (*Settlement, error) {
	st, err := scanSettlement(s.db.QueryRow(`SELECT `+settlementColumns+` FROM settlements s JOIN records r ON r.id = s.record_id WHERE s.record_id = ?`, id).Scan)
	if err != nil {
		return nil, notFound(err)
	}
	return st, nil
}

// SettlementForBasis returns the settlement based on an eligibility count.
func (s *Store) SettlementForBasis(countID string) (*Settlement, error) {
	st, err := scanSettlement(s.db.QueryRow(`SELECT `+settlementColumns+` FROM settlements s JOIN records r ON r.id = s.record_id WHERE s.basis = ?`, countID).Scan)
	if err != nil {
		return nil, notFound(err)
	}
	return st, nil
}

// Pay credits the per-person amount to every resident counted in the
// settlement's eligibility count, in one transaction, and marks the
// settlement paid. It returns the number paid and the total.
func (s *Store) Pay(settlementID string) (int64, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()

	var basis, perPerson, unit, status string
	var recipients int64
	err = tx.QueryRow(`SELECT basis, per_person, unit, status, recipients FROM settlements WHERE record_id = ?`, settlementID).
		Scan(&basis, &perPerson, &unit, &status, &recipients)
	if err != nil {
		return 0, "", notFound(err)
	}
	if status != "received" {
		return 0, "", fmt.Errorf("%w: settlement is already %s", ErrState, status)
	}
	res, err := tx.Exec(`INSERT INTO payments (settlement, resident_id, amount, unit, paid_at)
		SELECT ?, resident_id, ?, ?, ? FROM eligibility_snapshots WHERE count_record = ?`,
		settlementID, perPerson, unit, now(), basis)
	if err != nil {
		return 0, "", err
	}
	paid, _ := res.RowsAffected()
	if paid != recipients {
		return 0, "", fmt.Errorf("%w: %d residents were counted but the settlement is for %d", ErrState, paid, recipients)
	}
	total, err := amount.MulInt(perPerson, paid)
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(`UPDATE settlements SET status = 'paid' WHERE record_id = ?`, settlementID); err != nil {
		return 0, "", err
	}
	return paid, total, tx.Commit()
}

// SaveConfirmation stores a sent confirmation and links it to its
// settlement.
func (s *Store) SaveConfirmation(settlementID string, env *record.Envelope) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertRecord(tx, env, "sent", env.To, false); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE settlements SET confirmation = ? WHERE record_id = ?`, env.ID, settlementID); err != nil {
		return err
	}
	return tx.Commit()
}

// Payment is one dividend payment to a resident.
type Payment struct {
	Settlement string
	Period     string
	Amount     string
	Unit       string
	PaidAt     string
}

// ResidentPayments lists a resident's dividend payments, newest first, and
// their total.
func (s *Store) ResidentPayments(residentID string) ([]Payment, string, error) {
	rows, err := s.db.Query(`SELECT p.settlement, s.period, p.amount, p.unit, p.paid_at FROM payments p
		JOIN settlements s ON s.record_id = p.settlement WHERE p.resident_id = ? ORDER BY s.period DESC`, residentID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Payment
	sum := new(big.Rat)
	for rows.Next() {
		var p Payment
		if err := rows.Scan(&p.Settlement, &p.Period, &p.Amount, &p.Unit, &p.PaidAt); err != nil {
			return nil, "", err
		}
		v, err := amount.Parse(p.Amount)
		if err != nil {
			return nil, "", err
		}
		sum.Add(sum, v)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	total, err := amount.Format(sum)
	return out, total, err
}
