package deploy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polycentric-commonwealth/foundation/audit"
	"github.com/polycentric-commonwealth/foundation/dividend"

	"github.com/polycentric-commonwealth/platform-state/internal/devnet"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
)

// selfSigned writes a certificate and key for 127.0.0.1 and localhost.
func selfSigned(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pcstate test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(priv)
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return cert, key
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTwoDeployedStates runs a stand-in Commonwealth and two States, each
// initialized, attested and served as a deployment over HTTPS, as separate
// servers would be. North reports a count; the Commonwealth discovers it,
// settles and audits; North pays; South and the Commonwealth witness North's
// ledger. Then North restarts and its evidence survives.
func TestTwoDeployedStates(t *testing.T) {
	store.PasswordIterations = 1000
	base := t.TempDir()
	cert, key := selfSigned(t, base)
	addr := map[string]string{
		"commonwealth.example": freePort(t),
		"north.example":        freePort(t),
		"south.example":        freePort(t),
	}
	peers := map[string]string{}
	for id, a := range addr {
		peers[id] = "https://" + a
	}

	// The Commonwealth.
	cwDir := filepath.Join(base, "commonwealth")
	priv, err := devnet.LoadOrCreateKey(cwDir)
	if err != nil {
		t.Fatal(err)
	}
	anchor := devnet.Anchor("commonwealth.example", priv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cwDone := make(chan error, 1)
	go func() {
		cwDone <- devnet.RunCommonwealth(ctx, devnet.StandaloneOptions{
			Dir: cwDir, Instance: "commonwealth.example", BaseURL: peers["commonwealth.example"] + "/",
			Listen: addr["commonwealth.example"], TLSCert: cert, TLSKey: key, CAFile: cert, Peers: peers,
			PerPerson: "10", Delay: 100 * time.Millisecond, Logf: t.Logf,
		})
	}()

	// Two States: init, attest, add residents.
	_, anchorKey, _ := strings.Cut(anchor, "=")
	setup := func(id, name string, witnesses []string, residents int) string {
		dir := filepath.Join(base, id)
		desc, err := Init(dir, Config{
			Instance: id, Name: name, BaseURL: peers[id] + "/", Listen: addr[id], TLSCert: cert, TLSKey: key,
			Parent: "commonwealth.example", Commonwealth: "commonwealth.example",
			Anchors:   []Anchor{{Instance: "commonwealth.example", PublicKey: anchorKey}},
			Witnesses: witnesses, WitnessThreshold: len(witnesses), Peers: peers, CAFile: cert,
		})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := desc.Marshal()
		att, err := devnet.Attest("commonwealth.example", priv, data)
		if err != nil {
			t.Fatal(err)
		}
		attData, _ := att.Marshal()
		if _, err := AddAttestation(dir, attData); err != nil {
			t.Fatal(err)
		}
		if _, err := AddAttestation(dir, []byte(`{}`)); err == nil {
			t.Error("accepted a malformed attestation")
		}
		db, err := OpenStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := devnet.Seed(db, residents); err != nil {
			t.Fatal(err)
		}
		db.Close()
		return dir
	}
	northDir := setup("north.example", "North", []string{"commonwealth.example", "south.example"}, 20)
	southDir := setup("south.example", "South", []string{"commonwealth.example"}, 5)

	serve := func(dir string) (*Instance, context.CancelFunc, chan error) {
		in, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		in.Logf = t.Logf
		sctx, scancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- in.Serve(sctx) }()
		return in, scancel, done
	}
	south, stopSouth, southDone := serve(southDir)
	defer func() { stopSouth(); <-southDone; south.Close() }()
	north, stopNorth, northDone := serve(northDir)

	waitFor(t, "both States to connect", func() bool { return north.Connected() && south.Connected() })

	// North reports, the Commonwealth (which has never heard of North)
	// discovers it, settles and audits.
	if _, err := north.State.ReportEligibility(ctx, "2027-01"); err != nil {
		t.Fatalf("report: %v", err)
	}
	var settlement string
	waitFor(t, "the settlement", func() bool {
		recs, _ := north.Store.Records(dividend.SettlementType, 1)
		if len(recs) == 1 {
			settlement = recs[0].Envelope.ID
		}
		return settlement != ""
	})
	if _, err := north.State.Pay(ctx, settlement); err != nil {
		t.Fatalf("pay: %v", err)
	}
	waitFor(t, "the audit finding", func() bool {
		recs, _ := north.Store.Records(audit.FindingType, 1)
		return len(recs) == 1
	})

	// North's records are witnessed by the Commonwealth and by South, which
	// discovered North when its first checkpoint arrived.
	entries, err := north.State.LedgerEntries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("ledger has %d entries", len(entries))
	}
	for _, e := range entries {
		if e.Witnessed.IsZero() {
			t.Errorf("entry %d (%s) is not witnessed: %v", e.Index, e.Envelope.Type, e.Evidence)
		}
	}

	// Restart North: checkpoints and cosignatures come back from the
	// database, and its key is still trusted.
	stopNorth()
	if err := <-northDone; err != nil {
		t.Logf("north stopped: %v", err)
	}
	north.Close()
	north, stopNorth, northDone = serve(northDir)
	defer func() { stopNorth(); <-northDone; north.Close() }()
	for _, e := range entries {
		ev, err := north.State.Log.Evidence(e.Index)
		if err != nil || len(ev.Cosignatures) != 2 {
			t.Errorf("after restart, entry %d has no evidence with two cosignatures: %v", e.Index, err)
		}
	}
	waitFor(t, "North to reconnect", north.Connected)
	if entries, _ = north.State.LedgerEntries(10); entries[0].Witnessed.IsZero() {
		t.Errorf("after restart and reconnect, entry is not verified: %v", entries[0].Evidence)
	}

	// The well-known documents are served for others to discover.
	resp, err := north.HTTP.Get(peers["north.example"] + "/.well-known/pc-attestations")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("attestations endpoint: %d", resp.StatusCode)
	}
}
