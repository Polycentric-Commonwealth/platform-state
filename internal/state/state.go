// Package state is the State platform's core: it holds the State's
// identity, registry, database and public ledger, and runs the dividend
// flow with the Commonwealth.
package state

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/polycentric-commonwealth/foundation/dividend"
	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/protocol"
	"github.com/polycentric-commonwealth/foundation/record"
	"github.com/polycentric-commonwealth/foundation/registry"

	"github.com/polycentric-commonwealth/platform-state/internal/store"
)

// Config describes a State instance.
type Config struct {
	Identity     protocol.Identity
	Description  *record.Envelope // the State's signed instance description
	Registry     *registry.Registry
	Store        *store.Store
	Commonwealth string               // instance id of the Commonwealth (or the parent that settles dividends)
	Witnesses    ledger.WitnessPolicy // witnesses asked to cosign, and the policy used to check evidence
	HTTP         *http.Client         // client for other instances
	Now          func() time.Time     // defaults to time.Now
}

// State is a running State instance.
type State struct {
	Config
	Log      *ledger.Log
	Receiver *protocol.Receiver
	Sender   *protocol.Sender
	Witness  *ledger.Witness
}

// New returns a State ready to serve.
func New(cfg Config) (*State, error) {
	if cfg.Identity.Layer != record.State {
		return nil, fmt.Errorf("state: identity is a %s instance", cfg.Identity.Layer)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &State{Config: cfg}
	s.Log = ledger.NewLog(cfg.Identity, store.Ledger{S: cfg.Store})
	s.Sender = &protocol.Sender{Self: cfg.Identity, Registry: cfg.Registry, HTTP: cfg.HTTP}
	s.Receiver = &protocol.Receiver{
		Self:     cfg.Identity,
		Registry: cfg.Registry,
		Store:    store.Inbox{S: cfg.Store},
		Now:      cfg.Now,
		Handlers: map[string]protocol.Handler{dividend.SettlementType: s.receiveSettlement},
	}
	s.Witness = &ledger.Witness{Self: cfg.Identity, Registry: cfg.Registry, Store: &ledger.MemoryWitnessStore{}, Now: cfg.Now}
	return s, nil
}

// Name returns the State's human-readable name.
func (s *State) Name() string {
	if d, ok := s.Registry.Description(s.Identity.Instance); ok {
		return d.Name
	}
	return s.Identity.Instance
}

// ProtocolHandler serves the endpoints other instances use: the inbox, the
// ledger, the witness endpoint and the well-known description. Mount it at
// the root; it handles paths under /pc/ and /.well-known/.
func (s *State) ProtocolHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/pc/"+protocol.InboxPath, s.Receiver)
	mux.Handle("/pc/"+ledger.WitnessPath, s.Witness)
	mux.Handle("/pc/ledger/", http.StripPrefix("/pc/ledger", s.Log.Handler()))
	mux.HandleFunc("GET "+registry.WellKnownPath, func(w http.ResponseWriter, r *http.Request) {
		data, err := s.Description.Marshal()
		if err != nil {
			http.Error(w, "could not encode description", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	return mux
}

// ErrAlreadyReported reports a second eligibility count for one period.
var ErrAlreadyReported = errors.New("state: an eligibility count was already sent for this period")

// ReportEligibility counts the State's active residents for a period
// (YYYY-MM), sends the count to the Commonwealth and logs it. Adults are
// counted as of the first day of the period.
func (s *State) ReportEligibility(ctx context.Context, period string) (*record.Envelope, error) {
	start, err := time.Parse("2006-01", period)
	if err != nil {
		return nil, fmt.Errorf("state: period must be YYYY-MM")
	}
	if done, err := s.Store.HasCountForPeriod(period); err != nil {
		return nil, err
	} else if done {
		return nil, ErrAlreadyReported
	}
	ids, adults, err := s.Store.Snapshot(start)
	if err != nil {
		return nil, err
	}
	count := dividend.EligibilityCount{Period: period, Residents: int64(len(ids)), Adults: adults, DividendRecipients: int64(len(ids))}
	if err := count.Validate(); err != nil {
		return nil, err
	}
	env, err := s.Identity.Message(s.Commonwealth, dividend.EligibilityCountType, count)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveCount(env, ids); err != nil {
		return nil, err
	}
	return env, s.logAndSend(ctx, env)
}

// receiveSettlement is the inbox handler for dividend-settlement/1: the
// settlement must match an eligibility count this State sent.
func (s *State) receiveSettlement(_ context.Context, env *record.Envelope) error {
	if env.Issuer != s.Commonwealth {
		return fmt.Errorf("settlements are accepted only from %s", s.Commonwealth)
	}
	d, err := dividend.DecodeSettlement(env)
	if err != nil {
		return err
	}
	count, err := s.Store.Record(d.Basis)
	if errors.Is(err, store.ErrNotFound) || (err == nil && count.Direction != "sent") {
		return fmt.Errorf("no eligibility count %s was sent by this State", d.Basis)
	} else if err != nil {
		return err
	}
	if err := d.CheckAgainst(count.Envelope); err != nil {
		return err
	}
	if _, err := s.Store.SettlementForBasis(d.Basis); err == nil {
		return fmt.Errorf("a settlement for count %s was already received", d.Basis)
	}
	return s.Store.SaveSettlement(env, d)
}

// Pay pays a received settlement to every resident it covers, then sends
// and logs the confirmation.
func (s *State) Pay(ctx context.Context, settlementID string) (*record.Envelope, error) {
	st, err := s.Store.SettlementFor(settlementID)
	if err != nil {
		return nil, err
	}
	paid, total, err := s.Store.Pay(settlementID)
	if err != nil {
		return nil, err
	}
	conf := dividend.Confirmation{Settlement: settlementID, Unit: st.Unit, PaidRecipients: paid, PaidTotal: total}
	env, err := s.Identity.Message(st.Issuer, dividend.ConfirmationType, conf)
	if err != nil {
		return nil, err
	}
	if err := s.Store.SaveConfirmation(settlementID, env); err != nil {
		return nil, err
	}
	return env, s.logAndSend(ctx, env)
}

// Resend delivers a sent record again, for example after a network failure.
func (s *State) Resend(ctx context.Context, id string) error {
	r, err := s.Store.Record(id)
	if err != nil {
		return err
	}
	if r.Direction != "sent" {
		return fmt.Errorf("state: record %s was not sent by this State", id)
	}
	return s.deliver(ctx, r.Envelope)
}

// logAndSend logs a record in the public ledger, delivers it and publishes
// a new checkpoint. Delivery failures are returned but the record stays
// stored and logged, so it can be resent.
func (s *State) logAndSend(ctx context.Context, env *record.Envelope) error {
	if _, err := s.Log.Append(env); err != nil {
		return fmt.Errorf("state: logging %s: %w", env.Type, err)
	}
	sendErr := s.deliver(ctx, env)
	if _, err := s.PublishLedger(ctx); err != nil && sendErr == nil {
		return err
	}
	return sendErr
}

func (s *State) deliver(ctx context.Context, env *record.Envelope) error {
	rc, err := s.Sender.Send(ctx, env)
	switch {
	case rc != nil && rc.Status == protocol.StatusRejected:
		s.Store.SetDelivery(env.ID, "rejected: "+rc.Reason)
	case rc != nil:
		s.Store.SetDelivery(env.ID, rc.Status)
	}
	return err
}

// PublishLedger signs a checkpoint of the State's log and asks the
// witnesses to cosign it. It returns the number of cosignatures collected.
func (s *State) PublishLedger(ctx context.Context) (int, error) {
	if _, err := s.Log.Checkpoint(); err != nil {
		return 0, err
	}
	if len(s.Witnesses.Witnesses) == 0 {
		return 0, nil
	}
	return s.Log.RequestCosignatures(ctx, s.Registry, s.HTTP, s.Witnesses.Witnesses)
}

// EntryStatus is the verification result for one ledger entry.
type EntryStatus struct {
	Index     uint64
	Envelope  *record.Envelope
	Signed    error     // nil if the signature verifies against the registry
	Witnessed time.Time // zero if not yet witnessed under the policy
	Evidence  error
}

// LedgerEntries returns the newest entries of the State's log, up to limit,
// each checked against the registry and the witness policy.
func (s *State) LedgerEntries(limit int) ([]EntryStatus, error) {
	size, err := s.Log.Storage.Size()
	if err != nil {
		return nil, err
	}
	var out []EntryStatus
	for i := size; i > 0 && len(out) < limit; i-- {
		st, err := s.LedgerEntry(i - 1)
		if err != nil {
			return nil, err
		}
		out = append(out, *st)
	}
	return out, nil
}

// LedgerEntry returns one entry of the State's log, verified.
func (s *State) LedgerEntry(index uint64) (*EntryStatus, error) {
	data, err := s.Log.Storage.Entry(index)
	if err != nil {
		return nil, err
	}
	env, err := record.Parse(data)
	if err != nil {
		return nil, err
	}
	st := &EntryStatus{Index: index, Envelope: env, Signed: env.VerifyWith(s.Registry)}
	ev, err := s.Log.Evidence(index)
	if err == nil {
		st.Witnessed, err = ledger.VerifyEvidence(s.Registry, ev, s.Witnesses)
	}
	st.Evidence = err
	return st, nil
}

// Summary describes a record's payload in a few words, for lists.
func Summary(env *record.Envelope) string {
	switch env.Type {
	case dividend.EligibilityCountType:
		if c, err := dividend.DecodeCount(env); err == nil {
			return fmt.Sprintf("%s: %d recipients, %d adults", c.Period, c.DividendRecipients, c.Adults)
		}
	case dividend.SettlementType:
		if d, err := dividend.DecodeSettlement(env); err == nil {
			return fmt.Sprintf("%s: %s %s × %d = %s %s", d.Period, d.PerPerson, d.Unit, d.Recipients, d.Total, d.Unit)
		}
	case dividend.ConfirmationType:
		if c, err := dividend.DecodeConfirmation(env); err == nil {
			return fmt.Sprintf("paid %d recipients, %s %s", c.PaidRecipients, c.PaidTotal, c.Unit)
		}
	}
	return strings.TrimSuffix(env.Type, "/1")
}
