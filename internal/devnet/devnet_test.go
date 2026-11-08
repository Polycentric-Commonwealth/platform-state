package devnet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polycentric-commonwealth/foundation/amount"
	"github.com/polycentric-commonwealth/foundation/dividend"
	"github.com/polycentric-commonwealth/foundation/protocol"

	"github.com/polycentric-commonwealth/platform-state/internal/state"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
)

func start(t *testing.T, residents int) *Network {
	t.Helper()
	net, err := Start(Options{DataDir: t.TempDir(), Residents: residents, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(net.Close)
	return net
}

func waitForSettlement(t *testing.T, db *store.Store, countID string) *store.Settlement {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, err := db.SettlementForBasis(countID); err == nil {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no settlement arrived")
	return nil
}

func TestDividendFlow(t *testing.T) {
	net := start(t, 60)
	ctx := context.Background()
	st := net.State

	count, err := st.ReportEligibility(ctx, "2027-01")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := dividend.DecodeCount(count)
	if c.DividendRecipients != 60 || c.Residents != 60 || c.Adults == 0 || c.Adults >= 60 {
		t.Errorf("count = %+v, want 60 recipients with some children", c)
	}
	if r, _ := net.Store.Record(count.ID); r.Delivery != protocol.StatusAccepted {
		t.Errorf("count delivery = %q", r.Delivery)
	}
	if _, err := st.ReportEligibility(ctx, "2027-01"); !errors.Is(err, state.ErrAlreadyReported) {
		t.Errorf("second count for the same period: %v", err)
	}

	s := waitForSettlement(t, net.Store, count.ID)
	if s.Total != "1530" || s.Recipients != 60 || s.Status != "received" {
		t.Fatalf("settlement = %+v", s)
	}

	// A resident who leaves after the count is still paid for the period.
	if err := net.Store.SetResidentStatus("R-000001", "inactive"); err != nil {
		t.Fatal(err)
	}
	conf, err := st.Pay(ctx, s.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := net.Store.Record(conf.ID); r.Delivery != protocol.StatusAccepted {
		t.Errorf("confirmation delivery = %q", r.Delivery)
	}
	if _, err := st.Pay(ctx, s.RecordID); !errors.Is(err, store.ErrState) {
		t.Errorf("paying twice: %v", err)
	}
	payments, total, err := net.Store.ResidentPayments("R-000001")
	if err != nil || len(payments) != 1 || total != "25.5" {
		t.Errorf("resident payments = %+v, total %s, %v", payments, total, err)
	}

	// The State's ledger holds the count and the confirmation, both
	// verified and witnessed by the Commonwealth.
	entries, err := st.LedgerEntries(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger has %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Signed != nil || e.Evidence != nil || e.Witnessed.IsZero() {
			t.Errorf("entry %d (%s): signed %v, evidence %v", e.Index, e.Envelope.Type, e.Signed, e.Evidence)
		}
	}

	// The next month counts one fewer resident.
	next, err := st.ReportEligibility(ctx, "2027-02")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := dividend.DecodeCount(next); c.DividendRecipients != 59 {
		t.Errorf("next count = %d, want 59", c.DividendRecipients)
	}
	waitForSettlement(t, net.Store, next.ID)
}

func TestSettlementMustMatchCount(t *testing.T) {
	net := start(t, 10)
	ctx := context.Background()
	count, err := net.State.ReportEligibility(ctx, "2027-01")
	if err != nil {
		t.Fatal(err)
	}
	waitForSettlement(t, net.Store, count.ID)

	// A second settlement for the same count, from the real Commonwealth,
	// is refused.
	cw := net.Commonwealth
	s, _ := dividend.NewSettlement(count, "99")
	env, _ := cw.Identity.Message(StateID, dividend.SettlementType, s)
	_, err = cw.Sender.Send(ctx, env)
	if rc, ok := protocol.IsRejected(err); !ok || rc.Reason != protocol.ReasonRefused {
		t.Errorf("second settlement: %v", err)
	}

	// A settlement for a count the State never sent is refused.
	s.Basis = env.ID
	env2, _ := cw.Identity.Message(StateID, dividend.SettlementType, s)
	if _, err := cw.Sender.Send(ctx, env2); err == nil {
		t.Error("settlement with unknown basis accepted")
	}
}

func TestSeedIsDeterministic(t *testing.T) {
	a, _ := store.Open(t.TempDir() + "/a.db")
	b, _ := store.Open(t.TempDir() + "/b.db")
	defer a.Close()
	defer b.Close()
	Seed(a, 5)
	Seed(b, 5)
	ra, _ := a.Residents("", 10)
	rb, _ := b.Residents("", 10)
	for i := range ra {
		if ra[i].Name != rb[i].Name || ra[i].BirthDate != rb[i].BirthDate {
			t.Errorf("resident %d differs: %+v vs %+v", i, ra[i], rb[i])
		}
	}
}

func waitForFinding(t *testing.T, db *store.Store, countID string) *store.Audit {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if a, err := db.AuditForCount(countID); err == nil && a.Finding != nil {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no audit finding arrived")
	return nil
}

func TestHonestStatePassesAudit(t *testing.T) {
	net := start(t, 60)
	count, err := net.State.ReportEligibility(context.Background(), "2027-01")
	if err != nil {
		t.Fatal(err)
	}
	a := waitForFinding(t, net.Store, count.ID)
	if f := a.Finding; f.Sampled != 60 || f.Unverified != 0 || f.Repayment != "0" {
		t.Errorf("finding = %+v", f)
	}
	if a.Response == nil || a.Response.Delivery != protocol.StatusAccepted {
		t.Errorf("audit response not delivered: %+v", a.Response)
	}
	// The response held personal data, so it is not in the public ledger.
	entries, _ := net.State.LedgerEntries(10)
	for _, e := range entries {
		if e.Envelope.Type == "audit-response/1" {
			t.Error("audit response was logged")
		}
	}
	// The Commonwealth logged the request and the finding publicly.
	cwSize, _ := net.Commonwealth.Log.Storage.Size()
	if cwSize != 3 { // settlement, audit request, finding
		t.Errorf("Commonwealth ledger has %d entries, want 3", cwSize)
	}
}

func TestInflatedCountIsCaught(t *testing.T) {
	net, err := Start(Options{DataDir: t.TempDir(), Residents: 570, Ghosts: 30, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(net.Close)
	count, err := net.State.ReportEligibility(context.Background(), "2027-01")
	if err != nil {
		t.Fatal(err)
	}
	f := waitForFinding(t, net.Store, count.ID).Finding
	// 30 of 600 (5%) are invented; a sample of 500 finds about 25.
	if f.Sampled != 500 || f.Unverified < 10 || f.Unverified > 45 {
		t.Fatalf("finding = %+v", f)
	}
	if f.EstimatedExcess < 12 || f.EstimatedExcess > 54 {
		t.Errorf("estimated excess %d, want about 30", f.EstimatedExcess)
	}
	// The repayment is three times the estimated gain.
	if c, _ := amount.Cmp(f.Repayment, "0"); c <= 0 {
		t.Errorf("repayment = %s", f.Repayment)
	}
}
