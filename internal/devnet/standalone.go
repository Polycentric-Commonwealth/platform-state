package devnet

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/polycentric-commonwealth/foundation/protocol"
	"github.com/polycentric-commonwealth/foundation/record"
	"github.com/polycentric-commonwealth/foundation/registry"
)

// StandaloneOptions configures a stand-in Commonwealth running on its own,
// for testing deployed State platforms across processes or machines.
type StandaloneOptions struct {
	Dir       string            // keeps the Commonwealth's key between runs
	Instance  string            // its instance identifier
	BaseURL   string            // its public https URL, ending in /
	Listen    string            // address to serve on
	TLSCert   string            // certificate and key to serve HTTPS
	TLSKey    string            //
	CAFile    string            // extra PEM roots for reaching States
	Peers     map[string]string // instance to origin overrides
	PerPerson string            // dividend per person, in XCU
	Delay     time.Duration     // how long settling takes
	Logf      func(string, ...any)
}

const standaloneKey = "commonwealth.key"

// LoadOrCreateKey returns the stand-in Commonwealth's key in dir, creating
// it on first use.
func LoadOrCreateKey(dir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dir, standaloneKey)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		_, priv, err := ed25519.GenerateKey(crand.Reader)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(base64.RawURLEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
			return nil, err
		}
		return priv, nil
	} else if err != nil {
		return nil, err
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("devnet: %s is not a valid key file", path)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Anchor returns the trust anchor for the stand-in Commonwealth's key, in
// the form States pass to pcstate init: instance=ed25519:key.
func Anchor(instance string, priv ed25519.PrivateKey) string {
	return instance + "=" + registry.EncodePublicKey(priv.Public().(ed25519.PublicKey))
}

// Attest returns the stand-in Commonwealth's attestation of every current
// key in a State's signed description.
func Attest(instance string, priv ed25519.PrivateKey, description []byte) (*record.Envelope, error) {
	env, err := record.Parse(description)
	if err != nil {
		return nil, err
	}
	d, err := registry.ParseDescription(env)
	if err != nil {
		return nil, err
	}
	if d.Parent != instance {
		return nil, fmt.Errorf("devnet: %s names %s as its parent, not %s", d.Instance, d.Parent, instance)
	}
	k, ok := d.Key(env.Key)
	if !ok {
		return nil, fmt.Errorf("devnet: description is signed with a key it does not list")
	}
	pub, err := registry.DecodePublicKey(k.PublicKey)
	if err != nil {
		return nil, err
	}
	if err := env.Verify(pub); err != nil {
		return nil, err
	}
	a := &registry.Attestation{Subject: d.Instance, SubjectLayer: d.Layer}
	for _, k := range d.Keys {
		if k.RevokedAt == "" {
			a.Keys = append(a.Keys, registry.AttestedKey{ID: k.ID, PublicKey: k.PublicKey})
		}
	}
	return registry.NewAttestation(instance, record.Commonwealth, "k1", priv, a)
}

// RunCommonwealth serves a stand-in Commonwealth until ctx is done. It
// discovers States when their first record arrives, settles their counts,
// audits them (passing every valid opening, since it cannot see their
// registers) and witnesses their ledgers.
func RunCommonwealth(ctx context.Context, o StandaloneOptions) error {
	priv, err := LoadOrCreateKey(o.Dir)
	if err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	desc := &registry.Description{
		Instance: o.Instance, Layer: record.Commonwealth, Name: "Commonwealth (stand-in for testing)",
		Endpoints: map[string]string{"base": o.BaseURL + "pc/"},
		Keys:      []registry.Key{{ID: "k1", PublicKey: registry.EncodePublicKey(pub), ValidFrom: "2026-01-01T00:00:00Z"}},
	}
	signed, err := registry.NewDescription(desc, "k1", priv)
	if err != nil {
		return err
	}
	reg, err := registry.New(registry.Anchor{Instance: o.Instance, PublicKey: registry.EncodePublicKey(pub)})
	if err != nil {
		return err
	}
	if err := reg.AddDescription(signed); err != nil {
		return err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return err
		}
		pool.AppendCertsFromPEM(pem)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	fetcher := &registry.Fetcher{Registry: reg, HTTP: client, Origin: func(id string) string {
		if origin, ok := o.Peers[id]; ok {
			return origin
		}
		return "https://" + id
	}}
	id := protocol.Identity{Instance: o.Instance, Layer: record.Commonwealth, KeyID: "k1", Key: priv}
	c := NewCommonwealth(id, signed, reg, client, o.PerPerson, o.Delay, o.Logf)
	c.Receiver.Discover = fetcher.Discover
	c.Witness.Discover = fetcher.Discover
	defer c.Wait()

	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: c.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if o.TLSCert != "" {
			errc <- srv.ServeTLS(ln, o.TLSCert, o.TLSKey)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
