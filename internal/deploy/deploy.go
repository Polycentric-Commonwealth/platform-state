// Package deploy runs the State platform as a long-lived deployment: its
// configuration, signing key, description, attestations and database live
// in one directory, it finds other instances by discovery, and it keeps
// running when they are unreachable.
//
// The directory holds:
//
//	pcstate.json       configuration (see Config)
//	keys/<id>.key      the signing key's seed, unpadded base64url, mode 0600
//	description.json   the State's signed instance description
//	attestations.json  the parent's attestations of the State's keys
//	state.db           the database
package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/protocol"
	"github.com/polycentric-commonwealth/foundation/record"
	"github.com/polycentric-commonwealth/foundation/registry"

	"github.com/polycentric-commonwealth/platform-state/internal/state"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
	"github.com/polycentric-commonwealth/platform-state/internal/web"
)

// File names inside a deployment directory.
const (
	ConfigFile       = "pcstate.json"
	DescriptionFile  = "description.json"
	AttestationsFile = "attestations.json"
	DatabaseFile     = "state.db"
	KeysDir          = "keys"
)

// Config is a deployment's configuration, kept in pcstate.json.
type Config struct {
	// Instance is the State's instance identifier, a domain name.
	Instance string `json:"instance"`
	// Name is the State's human-readable name.
	Name string `json:"name"`
	// BaseURL is the public https URL of this platform; the protocol is
	// served under BaseURL + "pc/".
	BaseURL string `json:"base_url"`
	// Listen is the address to serve on, such as ":8443".
	Listen string `json:"listen"`
	// TLSCert and TLSKey, if set, make the platform serve HTTPS itself.
	// Otherwise it serves plain HTTP and expects a TLS proxy in front.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`

	// Parent attests the State's keys; Commonwealth settles its dividends.
	// They are often the same instance.
	Parent       string `json:"parent"`
	Commonwealth string `json:"commonwealth"`

	// Anchors are the pinned trust anchors, normally the Commonwealth.
	Anchors []Anchor `json:"anchors"`

	// Witnesses cosign the State's ledger checkpoints.
	Witnesses        []string `json:"witnesses"`
	WitnessThreshold int      `json:"witness_threshold"`

	// Peers maps instances to the origin they are reached at, when that is
	// not https://<instance>, for example inside a private network.
	Peers map[string]string `json:"peers,omitempty"`

	// CAFile, if set, names PEM certificates trusted for peers in addition
	// to the system's roots.
	CAFile string `json:"ca_file,omitempty"`

	// KeyID is the current signing key's identifier.
	KeyID string `json:"key_id"`
}

// Anchor is a pinned trust anchor.
type Anchor struct {
	Instance  string `json:"instance"`
	PublicKey string `json:"public_key"`
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	for name, id := range map[string]string{"instance": c.Instance, "parent": c.Parent, "commonwealth": c.Commonwealth} {
		if !record.ValidInstanceID(id) {
			return fmt.Errorf("deploy: %s must be an instance identifier (a lowercase domain name), got %q", name, id)
		}
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("deploy: name is required")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || !strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("deploy: base_url must be an https URL ending in /, got %q", c.BaseURL)
	}
	if c.Listen == "" {
		return errors.New("deploy: listen is required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("deploy: set both tls_cert and tls_key, or neither")
	}
	if len(c.Anchors) == 0 {
		return errors.New("deploy: at least one trust anchor is required")
	}
	for _, a := range c.Anchors {
		if _, err := registry.DecodePublicKey(a.PublicKey); err != nil || !record.ValidInstanceID(a.Instance) {
			return fmt.Errorf("deploy: anchor %q: an instance and an ed25519 public key are required", a.Instance)
		}
	}
	if len(c.Witnesses) == 0 || c.WitnessThreshold < 1 || c.WitnessThreshold > len(c.Witnesses) {
		return errors.New("deploy: need at least one witness and a threshold between 1 and the number of witnesses")
	}
	for id, origin := range c.Peers {
		if u, err := url.Parse(origin); err != nil || u.Scheme != "https" || u.Host == "" || !record.ValidInstanceID(id) {
			return fmt.Errorf("deploy: peer %s must map to an https origin, got %q", id, origin)
		}
	}
	if c.KeyID == "" {
		return errors.New("deploy: key_id is required")
	}
	return nil
}

// Init creates a deployment directory with a new signing key, a signed
// description and an empty database. The directory must not already hold a
// deployment. It returns the signed description, which the parent needs in
// order to attest the State's key.
func Init(dir string, cfg Config) (*record.Envelope, error) {
	if cfg.KeyID == "" {
		cfg.KeyID = "k1"
	}
	if len(cfg.Witnesses) == 0 {
		cfg.Witnesses = []string{cfg.Commonwealth}
	}
	if cfg.WitnessThreshold == 0 {
		cfg.WitnessThreshold = 1
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, ConfigFile)); err == nil {
		return nil, fmt.Errorf("deploy: %s already holds a deployment", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, KeysDir), 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, KeysDir, cfg.KeyID+".key")
	if err := writeNew(keyPath, []byte(base64.RawURLEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return nil, err
	}
	desc := &registry.Description{
		Instance: cfg.Instance, Layer: record.State, Name: cfg.Name, Parent: cfg.Parent,
		Endpoints: map[string]string{"base": cfg.BaseURL + "pc/"},
		Keys: []registry.Key{{ID: cfg.KeyID, PublicKey: registry.EncodePublicKey(pub),
			ValidFrom: time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Format(time.RFC3339)}},
	}
	env, err := registry.NewDescription(desc, cfg.KeyID, priv)
	if err != nil {
		return nil, err
	}
	data, err := env.Marshal()
	if err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, DescriptionFile), data, 0o644); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, AttestationsFile), []byte("[]\n"), 0o644); err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(dir, DatabaseFile))
	if err != nil {
		return nil, err
	}
	db.Close()
	// The database holds the residents' register: readable by the
	// platform's user only.
	if err := os.Chmod(filepath.Join(dir, DatabaseFile), 0o600); err != nil {
		return nil, err
	}
	if err := saveJSON(filepath.Join(dir, ConfigFile), cfg, true); err != nil {
		return nil, err
	}
	return env, nil
}

// LoadConfig reads a deployment's configuration.
func LoadConfig(dir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("deploy: %w (run pcstate init first)", err)
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("deploy: %s: %w", ConfigFile, err)
	}
	return &cfg, cfg.Validate()
}

// OpenStore opens a deployment's database, for command-line tools.
func OpenStore(dir string) (*store.Store, error) {
	if _, err := LoadConfig(dir); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, DatabaseFile))
}

// AddAttestation checks an attestation from the parent and adds it to the
// deployment, to be published from the next start. The parent's signature
// is verified when the platform connects to the parent.
func AddAttestation(dir string, data []byte) (*registry.Attestation, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	env, err := record.Parse(data)
	if err != nil {
		return nil, err
	}
	a, err := registry.ParseAttestation(env)
	if err != nil {
		return nil, err
	}
	if env.Issuer != cfg.Parent || a.Subject != cfg.Instance || a.SubjectLayer != record.State {
		return nil, fmt.Errorf("deploy: attestation must be issued by %s for %s as a State; it is from %s for %s as %s",
			cfg.Parent, cfg.Instance, env.Issuer, a.Subject, a.SubjectLayer)
	}
	desc, err := readRecord(filepath.Join(dir, DescriptionFile))
	if err != nil {
		return nil, err
	}
	d, err := registry.ParseDescription(desc)
	if err != nil {
		return nil, err
	}
	mine, _ := d.Key(cfg.KeyID)
	found := false
	for _, k := range a.Keys {
		if k.ID == mine.ID && k.PublicKey == mine.PublicKey {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("deploy: attestation does not list the current key %s with its public key", cfg.KeyID)
	}
	list, err := readAttestations(dir)
	if err != nil {
		return nil, err
	}
	for _, old := range list {
		if old.ID == env.ID {
			return a, nil
		}
	}
	list = append(list, env)
	return a, saveJSON(filepath.Join(dir, AttestationsFile), list, false)
}

// Instance is an opened deployment.
type Instance struct {
	Config  *Config
	Dir     string
	State   *state.State
	Store   *store.Store
	Fetcher *registry.Fetcher
	HTTP    *http.Client

	// Logf reports background problems; it defaults to log.Printf.
	Logf func(format string, args ...any)

	self         *record.Envelope
	attestations []*record.Envelope
	mu           sync.Mutex
	connected    bool
}

// Open loads a deployment. It does not contact other instances; Connect
// and Run do.
func Open(dir string) (*Instance, error) {
	cfg, err := LoadConfig(dir)
	if err != nil {
		return nil, err
	}
	seed, err := os.ReadFile(filepath.Join(dir, KeysDir, cfg.KeyID+".key"))
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, fmt.Errorf("deploy: key file for %s is not valid", cfg.KeyID)
	}
	if info, err := os.Stat(filepath.Join(dir, KeysDir, cfg.KeyID+".key")); err == nil && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("deploy: key file for %s must not be readable by others (chmod 600)", cfg.KeyID)
	}
	desc, err := readRecord(filepath.Join(dir, DescriptionFile))
	if err != nil {
		return nil, err
	}
	attestations, err := readAttestations(dir)
	if err != nil {
		return nil, err
	}
	client, err := httpClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	var anchors []registry.Anchor
	for _, a := range cfg.Anchors {
		anchors = append(anchors, registry.Anchor{Instance: a.Instance, PublicKey: a.PublicKey})
	}
	reg, err := registry.New(anchors...)
	if err != nil {
		return nil, err
	}
	db, err := store.Open(filepath.Join(dir, DatabaseFile))
	if err != nil {
		return nil, err
	}
	in := &Instance{Config: cfg, Dir: dir, Store: db, HTTP: client, Logf: log.Printf, self: desc, attestations: attestations}
	in.Fetcher = &registry.Fetcher{Registry: reg, HTTP: client, Origin: in.origin}
	id := protocol.Identity{Instance: cfg.Instance, Layer: record.State, KeyID: cfg.KeyID, Key: ed25519.NewKeyFromSeed(raw)}
	in.State, err = state.New(state.Config{
		Identity: id, Description: desc, Registry: reg, Store: db,
		Commonwealth: cfg.Commonwealth, HTTP: client,
		Witnesses:    ledger.WitnessPolicy{Witnesses: cfg.Witnesses, Threshold: cfg.WitnessThreshold},
		Attestations: attestations,
		Discover:     in.Fetcher.Discover,
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return in, nil
}

func (in *Instance) origin(instance string) string {
	if o, ok := in.Config.Peers[instance]; ok {
		return o
	}
	return "https://" + instance
}

// Connect discovers the Commonwealth, the parent and the witnesses, then
// adds the State's own attestations and description to its registry. It is
// safe to call again to refresh.
func (in *Instance) Connect(ctx context.Context) error {
	var errs []error
	peers := append([]string{in.Config.Commonwealth, in.Config.Parent}, in.Config.Witnesses...)
	seen := map[string]bool{in.Config.Instance: true}
	for _, p := range peers {
		if seen[p] {
			continue
		}
		seen[p] = true
		if err := in.Fetcher.Discover(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	reg := in.State.Registry
	for _, a := range in.attestations {
		if err := reg.AddAttestation(a); err != nil {
			errs = append(errs, fmt.Errorf("own attestation %s: %w", a.ID, err))
		}
	}
	if err := reg.AddDescription(in.self); err != nil && !errors.Is(err, registry.ErrStale) {
		errs = append(errs, fmt.Errorf("own description: %w", err))
	}
	if err := reg.Verify(in.self); err != nil {
		errs = append(errs, fmt.Errorf("the State's own key is not yet trusted (add the parent's attestation with pcstate attestation add): %w", err))
	}
	in.mu.Lock()
	in.connected = len(errs) == 0
	in.mu.Unlock()
	return errors.Join(errs...)
}

// Connected reports whether the last Connect succeeded.
func (in *Instance) Connected() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.connected
}

// Run connects, retrying every 30 seconds until it succeeds, and then
// refreshes every 6 hours, until ctx is done. The platform serves
// throughout: being disconnected only delays delivery to other instances.
func (in *Instance) Run(ctx context.Context) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := in.Connect(cctx)
		cancel()
		if err != nil {
			in.Logf("pcstate: not fully connected yet, retrying in 30s: %v", err)
			wait = 30 * time.Second
			continue
		}
		wait = 6 * time.Hour
	}
}

// Handler returns the platform's web interface and protocol endpoints.
func (in *Instance) Handler() (http.Handler, error) {
	return web.New(in.State, in.Store, true)
}

// Serve runs the platform until ctx is done.
func (in *Instance) Serve(ctx context.Context) error {
	h, err := in.Handler()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", in.Config.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: 2 * time.Minute}
	go in.Run(ctx)
	errc := make(chan error, 1)
	go func() {
		if in.Config.TLSCert != "" {
			errc <- srv.ServeTLS(ln, in.Config.TLSCert, in.Config.TLSKey)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// Close waits for background work and closes the database.
func (in *Instance) Close() error {
	in.State.Close()
	return in.Store.Close()
}

func httpClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("deploy: %s holds no PEM certificates", caFile)
		}
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	}}, nil
}

func readRecord(path string) (*record.Envelope, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return record.Parse(data)
}

func readAttestations(dir string) ([]*record.Envelope, error) {
	data, err := os.ReadFile(filepath.Join(dir, AttestationsFile))
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("deploy: %s: %w", AttestationsFile, err)
	}
	var out []*record.Envelope
	for _, r := range raw {
		env, err := record.Parse(r)
		if err != nil {
			return nil, fmt.Errorf("deploy: %s: %w", AttestationsFile, err)
		}
		out = append(out, env)
	}
	return out, nil
}

func saveJSON(path string, v any, fresh bool) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if fresh {
		return writeNew(path, data, 0o644)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
