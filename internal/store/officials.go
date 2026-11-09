package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Roles. An admin can do everything, including managing officials; a
// registrar keeps the residents' register; a treasurer runs the dividend
// cycle and publishes the ledger. Every official can view every officials'
// page.
const (
	RoleAdmin     = "admin"
	RoleRegistrar = "registrar"
	RoleTreasurer = "treasurer"
)

// Roles lists the roles in display order.
var Roles = []string{RoleAdmin, RoleRegistrar, RoleTreasurer}

// Official is an official's account.
type Official struct {
	Username  string
	Name      string
	Role      string
	CreatedAt string
	Disabled  bool
}

// Can reports whether the official's role allows work done by role.
func (o *Official) Can(role string) bool {
	return !o.Disabled && (o.Role == RoleAdmin || o.Role == role)
}

// Errors returned for officials.
var (
	ErrBadCredentials  = errors.New("store: username or password is not correct")
	ErrInvalidOfficial = errors.New("store: invalid official")
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

// PasswordIterations is the PBKDF2-SHA256 work factor for new passwords.
// Tests lower it.
var PasswordIterations = 600_000

// MinPasswordLength is the shortest password accepted.
const MinPasswordLength = 12

// GeneratePassword returns a random password of 16 lowercase letters and
// digits, in four groups for reading aloud.
func GeneratePassword() (string, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16], nil
}

func hashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("%w: passwords must have at least %d characters", ErrInvalidOfficial, MinPasswordLength)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, PasswordIterations, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", PasswordIterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, password string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	enc := base64.RawStdEncoding
	salt, err2 := enc.DecodeString(parts[2])
	want, err3 := enc.DecodeString(parts[3])
	if err != nil || err2 != nil || err3 != nil || iter < 1 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// AddOfficial creates an account.
func (s *Store) AddOfficial(username, name, role, password string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: usernames are 2 to 32 lowercase letters, digits, dots, hyphens or underscores, starting with a letter", ErrInvalidOfficial)
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return fmt.Errorf("%w: a name is required", ErrInvalidOfficial)
	}
	if !validRole(role) {
		return fmt.Errorf("%w: role must be admin, registrar or treasurer", ErrInvalidOfficial)
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO officials (username, name, role, password_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		username, name, role, hash, now())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("%w: %s already has an account", ErrInvalidOfficial, username)
	}
	return err
}

func validRole(role string) bool {
	for _, r := range Roles {
		if r == role {
			return true
		}
	}
	return false
}

// SetPassword replaces an official's password and signs them out everywhere.
func (s *Store) SetPassword(username, password string) error {
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	return s.updateOfficial(username, `UPDATE officials SET password_hash = ? WHERE username = ?`, hash, username)
}

// SetDisabled disables or re-enables an account. Disabling signs the
// official out everywhere.
func (s *Store) SetDisabled(username string, disabled bool) error {
	return s.updateOfficial(username, `UPDATE officials SET disabled = ? WHERE username = ?`, disabled, username)
}

func (s *Store) updateOfficial(username, query string, args ...any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE username = ?`, username); err != nil {
		return err
	}
	return tx.Commit()
}

// Official returns an account.
func (s *Store) Official(username string) (*Official, error) {
	var o Official
	err := s.db.QueryRow(`SELECT username, name, role, created_at, disabled FROM officials WHERE username = ?`, username).
		Scan(&o.Username, &o.Name, &o.Role, &o.CreatedAt, &o.Disabled)
	if err != nil {
		return nil, notFound(err)
	}
	return &o, nil
}

// Officials lists every account, by username.
func (s *Store) Officials() ([]Official, error) {
	rows, err := s.db.Query(`SELECT username, name, role, created_at, disabled FROM officials ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Official
	for rows.Next() {
		var o Official
		if err := rows.Scan(&o.Username, &o.Name, &o.Role, &o.CreatedAt, &o.Disabled); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// CountAdmins returns the number of enabled admin accounts.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM officials WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// Authenticate checks a username and password.
func (s *Store) Authenticate(username, password string) (*Official, error) {
	var hash string
	var disabled bool
	err := s.db.QueryRow(`SELECT password_hash, disabled FROM officials WHERE username = ?`, username).Scan(&hash, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the same time as a real check, so timing does not reveal
		// which usernames exist.
		dummyOnce.Do(func() { dummyHash, _ = hashPassword("not-a-real-password") })
		checkPassword(dummyHash, password)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if !checkPassword(hash, password) || disabled {
		return nil, ErrBadCredentials
	}
	return s.Official(username)
}

var (
	dummyOnce sync.Once
	dummyHash string
)

func tokenHash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// NewSession signs an official in and returns the session token.
func (s *Store) NewSession(username string, length time.Duration) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	expires := time.Now().Add(length).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now()); err != nil {
		return "", err
	}
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, username, expires_at) VALUES (?, ?, ?)`, tokenHash(token), username, expires)
	return token, err
}

// Session returns the signed-in official for a session token.
func (s *Store) Session(token string) (*Official, error) {
	var username, expires string
	err := s.db.QueryRow(`SELECT username, expires_at FROM sessions WHERE token_hash = ?`, tokenHash(token)).Scan(&username, &expires)
	if err != nil {
		return nil, notFound(err)
	}
	if t, err := time.Parse(time.RFC3339, expires); err != nil || time.Now().After(t) {
		return nil, ErrNotFound
	}
	o, err := s.Official(username)
	if err != nil {
		return nil, err
	}
	if o.Disabled {
		return nil, ErrNotFound
	}
	return o, nil
}

// EndSession signs a session out.
func (s *Store) EndSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, tokenHash(token))
	return err
}

// Action is one entry in the officials' activity log.
type Action struct {
	At       string
	Username string
	Action   string
	Detail   string
}

// LogAction records that an official did something.
func (s *Store) LogAction(username, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO official_actions (at, username, action, detail) VALUES (?, ?, ?, ?)`, now(), username, action, detail)
	return err
}

// Actions returns the most recent actions, newest first.
func (s *Store) Actions(limit int) ([]Action, error) {
	rows, err := s.db.Query(`SELECT at, username, action, detail FROM official_actions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		if err := rows.Scan(&a.At, &a.Username, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Secret returns a random secret kept under name, creating it on first use.
func (s *Store) Secret(name string, size int) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, name).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	v = make([]byte, size)
	if _, err := rand.Read(v); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings (key, value) VALUES (?, ?)`, name, v); err != nil {
		return nil, err
	}
	return s.Secret(name, size)
}
