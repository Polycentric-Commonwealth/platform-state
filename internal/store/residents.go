package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Resident is a person in the State's civil registry.
type Resident struct {
	ID           string
	Name         string
	BirthDate    string // YYYY-MM-DD
	RegisteredAt string
	Status       string
}

// ErrInvalidResident reports resident details that cannot be stored.
var ErrInvalidResident = errors.New("store: invalid resident")

// AddResident registers a new active resident and returns their resident
// number.
func (s *Store) AddResident(name, birthDate string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || utf8.RuneCountInString(name) > 200 {
		return "", fmt.Errorf("%w: name must be 1 to 200 characters", ErrInvalidResident)
	}
	b, err := time.Parse("2006-01-02", birthDate)
	if err != nil {
		return "", fmt.Errorf("%w: date of birth must be YYYY-MM-DD", ErrInvalidResident)
	}
	if b.After(time.Now()) || b.Year() < 1890 {
		return "", fmt.Errorf("%w: date of birth is out of range", ErrInvalidResident)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM residents`).Scan(&n); err != nil {
		return "", err
	}
	id := fmt.Sprintf("R-%06d", n+1)
	if _, err := tx.Exec(`INSERT INTO residents (id, name, birth_date, registered_at) VALUES (?, ?, ?, ?)`,
		id, name, birthDate, now()); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// SetResidentStatus marks a resident active or inactive (for example after
// moving away or dying). Residents are never deleted, so past records stay
// explainable.
func (s *Store) SetResidentStatus(id, status string) error {
	if status != "active" && status != "inactive" {
		return fmt.Errorf("%w: status must be active or inactive", ErrInvalidResident)
	}
	res, err := s.db.Exec(`UPDATE residents SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Resident returns one resident.
func (s *Store) Resident(id string) (*Resident, error) {
	var r Resident
	err := s.db.QueryRow(`SELECT id, name, birth_date, registered_at, status FROM residents WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.BirthDate, &r.RegisteredAt, &r.Status)
	if err != nil {
		return nil, notFound(err)
	}
	return &r, nil
}

// Residents lists residents whose name or number contains query (all if
// empty), newest first, up to limit.
func (s *Store) Residents(query string, limit int) ([]Resident, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	rows, err := s.db.Query(`SELECT id, name, birth_date, registered_at, status FROM residents
		WHERE name LIKE ? ESCAPE '\' OR id LIKE ? ESCAPE '\'
		ORDER BY id DESC LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resident
	for rows.Next() {
		var r Resident
		if err := rows.Scan(&r.ID, &r.Name, &r.BirthDate, &r.RegisteredAt, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResidentCounts returns the number of active residents, and how many of
// them are adults on the given day.
func (s *Store) ResidentCounts(on time.Time) (active, adults int64, err error) {
	cutoff := on.AddDate(-18, 0, 0).Format("2006-01-02")
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(birth_date <= ?), 0) FROM residents WHERE status = 'active'`, cutoff).
		Scan(&active, &adults)
	return active, adults, err
}

// ActiveResidentIDs returns the numbers of every active resident.
func (s *Store) ActiveResidentIDs() ([]string, error) {
	rows, err := s.db.Query(`SELECT id FROM residents WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
