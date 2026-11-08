// Package devnet runs a State instance together with a small local
// Commonwealth on this machine, for development and demonstrations. The
// Commonwealth attests the State's key, settles every eligibility count at a
// fixed per-person amount, and witnesses the State's ledger.
//
// Everything is created fresh: new keys, a new database and self-signed TLS
// certificates.
package devnet

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"time"

	"github.com/polycentric-commonwealth/foundation/dividend"
	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/protocol"
	"github.com/polycentric-commonwealth/foundation/record"
	"github.com/polycentric-commonwealth/foundation/registry"

	"github.com/polycentric-commonwealth/platform-state/internal/state"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
)

// Instance identifiers used by the dev network.
const (
	CommonwealthID = "commonwealth.example"
	StateID        = "state.example"
)

// Options configures the dev network.
type Options struct {
	DataDir         string        // directory for the State's database
	StateName       string        // defaults to "Example State"
	PerPerson       string        // dividend per person, defaults to "25.50"
	Residents       int           // sample residents to register, defaults to 60
	SettlementDelay time.Duration // how long the Commonwealth takes to settle
	Logf            func(format string, args ...any)
}

// Network is a running dev network.
type Network struct {
	State        *state.State
	Commonwealth *Commonwealth
	Store        *store.Store
	HTTP         *http.Client
	StateBaseURL string // the State's protocol server, for other instances

	servers []*httptest.Server
}

// Close stops the servers and closes the database.
func (n *Network) Close() {
	for _, s := range n.servers {
		s.Close()
	}
	n.Commonwealth.wg.Wait()
	n.Store.Close()
}

type node struct {
	id      protocol.Identity
	pub     ed25519.PublicKey
	desc    *registry.Description
	signed  *record.Envelope
	server  *httptest.Server
	handler http.Handler
}

func newNode(instance string, layer record.Layer, name, parent string, validFrom string) (*node, error) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		return nil, err
	}
	n := &node{id: protocol.Identity{Instance: instance, Layer: layer, KeyID: "k1", Key: priv}, pub: pub}
	n.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.handler.ServeHTTP(w, r)
	}))
	n.server.StartTLS()
	n.desc = &registry.Description{
		Instance: instance, Layer: layer, Name: name, Parent: parent,
		Endpoints: map[string]string{"base": n.server.URL + "/pc/"},
		Keys:      []registry.Key{{ID: "k1", PublicKey: registry.EncodePublicKey(pub), ValidFrom: validFrom}},
	}
	n.signed, err = registry.NewDescription(n.desc, "k1", priv)
	return n, err
}

// Start launches the dev network.
func Start(opts Options) (*Network, error) {
	if opts.StateName == "" {
		opts.StateName = "Example State"
	}
	if opts.PerPerson == "" {
		opts.PerPerson = "25.50"
	}
	if opts.Residents == 0 {
		opts.Residents = 60
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	validFrom := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Format(time.RFC3339)

	cw, err := newNode(CommonwealthID, record.Commonwealth, "Commonwealth (development)", "", validFrom)
	if err != nil {
		return nil, err
	}
	st, err := newNode(StateID, record.State, opts.StateName, CommonwealthID, validFrom)
	if err != nil {
		cw.server.Close()
		return nil, err
	}
	net := &Network{servers: []*httptest.Server{cw.server, st.server}, StateBaseURL: st.server.URL + "/"}
	fail := func(err error) (*Network, error) {
		for _, s := range net.servers {
			s.Close()
		}
		if net.Store != nil {
			net.Store.Close()
		}
		return nil, err
	}

	attestation, err := registry.NewAttestation(CommonwealthID, record.Commonwealth, "k1", cw.id.Key,
		&registry.Attestation{Subject: StateID, SubjectLayer: record.State,
			Keys: []registry.AttestedKey{{ID: "k1", PublicKey: registry.EncodePublicKey(st.pub)}}})
	if err != nil {
		return fail(err)
	}
	newRegistry := func() (*registry.Registry, error) {
		reg, err := registry.New(registry.Anchor{Instance: CommonwealthID, PublicKey: registry.EncodePublicKey(cw.pub)})
		if err != nil {
			return nil, err
		}
		for _, d := range []*record.Envelope{cw.signed, st.signed} {
			if err := reg.AddDescription(d); err != nil {
				return nil, err
			}
		}
		return reg, reg.AddAttestation(attestation)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cw.server.Certificate())
	pool.AddCert(st.server.Certificate())
	net.HTTP = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	db, err := store.Open(filepath.Join(opts.DataDir, "state.db"))
	if err != nil {
		return fail(err)
	}
	net.Store = db
	if err := Seed(db, opts.Residents); err != nil {
		return fail(err)
	}

	stReg, err := newRegistry()
	if err != nil {
		return fail(err)
	}
	policy := ledger.WitnessPolicy{Witnesses: []string{CommonwealthID}, Threshold: 1}
	net.State, err = state.New(state.Config{
		Identity: st.id, Description: st.signed, Registry: stReg, Store: db,
		Commonwealth: CommonwealthID, Witnesses: policy, HTTP: net.HTTP,
	})
	if err != nil {
		return fail(err)
	}
	st.handler = net.State.ProtocolHandler()

	cwReg, err := newRegistry()
	if err != nil {
		return fail(err)
	}
	net.Commonwealth = newCommonwealth(cw.id, cw.signed, cwReg, net.HTTP, opts)
	cw.handler = net.Commonwealth.handler()
	return net, nil
}

// Commonwealth is the dev network's stand-in for the Commonwealth platform.
type Commonwealth struct {
	Identity    protocol.Identity
	Description *record.Envelope
	Registry    *registry.Registry
	Log         *ledger.Log
	Receiver    *protocol.Receiver
	Sender      *protocol.Sender
	Witness     *ledger.Witness

	perPerson string
	delay     time.Duration
	http      *http.Client
	logf      func(string, ...any)

	mu          sync.Mutex
	settlements map[string]*record.Envelope
	wg          sync.WaitGroup
}

func newCommonwealth(id protocol.Identity, desc *record.Envelope, reg *registry.Registry, client *http.Client, opts Options) *Commonwealth {
	c := &Commonwealth{
		Identity: id, Description: desc, Registry: reg,
		Log:       ledger.NewLog(id, nil),
		Sender:    &protocol.Sender{Self: id, Registry: reg, HTTP: client},
		Witness:   &ledger.Witness{Self: id, Registry: reg, Store: &ledger.MemoryWitnessStore{}},
		perPerson: opts.PerPerson, delay: opts.SettlementDelay, http: client, logf: opts.Logf,
		settlements: map[string]*record.Envelope{},
	}
	c.Receiver = &protocol.Receiver{Self: id, Registry: reg, Store: protocol.NewMemoryStore(), Handlers: map[string]protocol.Handler{
		dividend.EligibilityCountType: c.receiveCount,
		dividend.ConfirmationType:     c.receiveConfirmation,
	}}
	return c
}

func (c *Commonwealth) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/pc/"+protocol.InboxPath, c.Receiver)
	mux.Handle("/pc/"+ledger.WitnessPath, c.Witness)
	mux.Handle("/pc/ledger/", http.StripPrefix("/pc/ledger", c.Log.Handler()))
	return mux
}

// receiveCount accepts an eligibility count and settles it shortly after,
// as the real Commonwealth would once its Commons Fund calculation runs.
func (c *Commonwealth) receiveCount(_ context.Context, env *record.Envelope) error {
	if _, err := dividend.DecodeCount(env); err != nil {
		return err
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		time.Sleep(c.delay)
		if err := c.settle(env); err != nil {
			c.logf("devnet: commonwealth could not settle %s: %v", env.ID, err)
		}
	}()
	return nil
}

func (c *Commonwealth) settle(count *record.Envelope) error {
	s, err := dividend.NewSettlement(count, c.perPerson)
	if err != nil {
		return err
	}
	env, err := c.Identity.Message(count.Issuer, dividend.SettlementType, s)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.settlements[env.ID] = env
	c.mu.Unlock()
	if _, err := c.Log.Append(env); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Sender.Send(ctx, env); err != nil {
		return err
	}
	if _, err := c.Log.Checkpoint(); err != nil {
		return err
	}
	_, err = c.Log.RequestCosignatures(ctx, c.Registry, c.http, []string{count.Issuer})
	return err
}

func (c *Commonwealth) receiveConfirmation(_ context.Context, env *record.Envelope) error {
	conf, err := dividend.DecodeConfirmation(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	s, ok := c.settlements[conf.Settlement]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("no settlement %s was sent", conf.Settlement)
	}
	return conf.CheckAgainst(s)
}

var (
	givenNames  = []string{"Amara", "Bao", "Camila", "Dmitri", "Eitan", "Farah", "Gustavo", "Hana", "Ines", "Jomo", "Kaito", "Leila", "Mateo", "Nia", "Olu", "Priya", "Quinn", "Rafael", "Sana", "Tomas", "Uma", "Viktor", "Wanjiru", "Xin", "Yara", "Zeynep"}
	familyNames = []string{"Abara", "Bergstrom", "Chen", "Delacroix", "Esposito", "Fernandes", "Gallagher", "Haddad", "Ivanova", "Johansson", "Kowalski", "Lindqvist", "Mensah", "Nakamura", "Okafor", "Petrova", "Quispe", "Rahman", "Silva", "Tanaka", "Underwood", "Varga", "Wiśniewska", "Yilmaz", "Zhou"}
)

// Seed registers n fictional residents with a spread of ages, including
// children. The same n always gives the same people.
func Seed(db *store.Store, n int) error {
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < n; i++ {
		name := givenNames[r.IntN(len(givenNames))] + " " + familyNames[r.IntN(len(familyNames))]
		birth := time.Date(1940+r.IntN(84), time.Month(1+r.IntN(12)), 1+r.IntN(28), 0, 0, 0, 0, time.UTC)
		if _, err := db.AddResident(name, birth.Format("2006-01-02")); err != nil {
			return err
		}
	}
	return nil
}
