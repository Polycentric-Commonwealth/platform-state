package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polycentric-commonwealth/foundation/dividend"
	"github.com/polycentric-commonwealth/foundation/ledger"
	"github.com/polycentric-commonwealth/foundation/protocol"

	"github.com/polycentric-commonwealth/platform-state/internal/state"
	"github.com/polycentric-commonwealth/platform-state/internal/store"
)

// cycle is one dividend period, from count to confirmation.
type cycle struct {
	Period              string
	Recipients          int64
	Settlement          *store.Settlement
	Status, StatusClass string
	Audit, AuditClass   string
	CanPay              bool
	Resend              []string
}

// auditStatus describes the audit of a count in a few words.
func (s *Server) auditStatus(cy *cycle, countID string) error {
	a, err := s.Store.AuditForCount(countID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		cy.Audit = "Not audited yet"
		return nil
	case err != nil:
		return err
	case a.Finding == nil:
		cy.Audit, cy.AuditClass = "In progress", "wait"
		if a.Response != nil && a.Response.Delivery == "pending" {
			cy.Resend = append(cy.Resend, a.Response.Envelope.ID)
		}
	case a.Finding.Unverified == 0:
		cy.Audit, cy.AuditClass = fmt.Sprintf("Passed: all %d sampled verified", a.Finding.Sampled), "ok"
	default:
		f := a.Finding
		cy.Audit, cy.AuditClass = fmt.Sprintf("%d of %d sampled not found; repayment %s %s", f.Unverified, f.Sampled, money(f.Repayment), f.Unit), "bad"
	}
	return nil
}

func (s *Server) cycles() ([]cycle, error) {
	counts, err := s.Store.Records(dividend.EligibilityCountType, 120)
	if err != nil {
		return nil, err
	}
	var out []cycle
	for _, r := range counts {
		if r.Direction != "sent" {
			continue
		}
		c, err := dividend.DecodeCount(r.Envelope)
		if err != nil {
			return nil, err
		}
		cy := cycle{Period: c.Period, Recipients: c.DividendRecipients}
		switch {
		case strings.HasPrefix(r.Delivery, "rejected"):
			cy.Status, cy.StatusClass = "Count refused by the Commonwealth", "bad"
			out = append(out, cy)
			continue
		case r.Delivery == "pending":
			cy.Status, cy.StatusClass, cy.Resend = "Count not yet delivered", "wait", []string{r.Envelope.ID}
			out = append(out, cy)
			continue
		}
		if err := s.auditStatus(&cy, r.Envelope.ID); err != nil {
			return nil, err
		}
		st, err := s.Store.SettlementForBasis(r.Envelope.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			cy.Status, cy.StatusClass = "Awaiting settlement", "wait"
		case err != nil:
			return nil, err
		case st.Status == "received":
			cy.Settlement, cy.Status, cy.StatusClass, cy.CanPay = st, "Settled, awaiting payment", "wait", true
		default:
			cy.Settlement = st
			conf, err := s.Store.Record(st.Confirmation)
			switch {
			case err != nil:
				cy.Status, cy.StatusClass = "Paid", "ok"
			case conf.Delivery == "pending":
				cy.Status, cy.StatusClass, cy.Resend = "Paid; confirmation not yet delivered", "wait", []string{conf.Envelope.ID}
			case strings.HasPrefix(conf.Delivery, "rejected"):
				cy.Status, cy.StatusClass = "Paid; confirmation refused", "bad"
			default:
				cy.Status, cy.StatusClass = "Paid "+fmtTime(conf.Envelope.IssuedAt), "ok"
			}
		}
		out = append(out, cy)
	}
	return out, nil
}

type checkpointView struct {
	Size      uint64
	IssuedAt  string
	Witnesses []witnessView
}

type witnessView struct{ Instance, At string }

func (s *Server) checkpoint() *checkpointView {
	cp, cosigs, ok := s.State.Log.Latest()
	if !ok {
		return nil
	}
	var payload ledger.Checkpoint
	if err := json.Unmarshal(cp.Payload, &payload); err != nil {
		return nil
	}
	v := &checkpointView{Size: payload.Size, IssuedAt: cp.IssuedAt}
	for _, c := range cosigs {
		v.Witnesses = append(v.Witnesses, witnessView{Instance: c.Issuer, At: c.IssuedAt})
	}
	return v
}

// --- Public pages ------------------------------------------------------------

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "Overview", "overview")
	var err error
	if v.ResidentCount, v.AdultCount, err = s.Store.ResidentCounts(time.Now()); err != nil {
		s.fail(w, err)
		return
	}
	settlements, err := s.Store.Settlements()
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(settlements) > 0 {
		v.Latest = &settlements[0]
	}
	if v.LedgerSize, err = s.State.Log.Storage.Size(); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "overview", v)
}

func (s *Server) dividends(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "Dividends", "dividends")
	var err error
	if v.Cycles, err = s.cycles(); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "dividends", v)
}

func (s *Server) ledgerPage(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "Public ledger", "ledger")
	var err error
	if v.Entries, err = s.State.LedgerEntries(200); err != nil {
		s.fail(w, err)
		return
	}
	v.Checkpoint = s.checkpoint()
	s.render(w, http.StatusOK, "ledger", v)
}

func (s *Server) entryPage(w http.ResponseWriter, r *http.Request) {
	index, err := strconv.ParseUint(r.PathValue("index"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	entry, err := s.State.LedgerEntry(index)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	v := s.newView(r, fmt.Sprintf("Record %d", index), "ledger")
	v.Entry = entry
	h := entryHash(entry.Envelope)
	v.Hash = ledger.EncodeHash(h)
	data, _ := json.MarshalIndent(entry.Envelope, "", "  ")
	v.JSON = string(data)
	s.render(w, http.StatusOK, "entry", v)
}

func (s *Server) myDividend(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "my-dividend", s.newView(r, "My dividend", "my-dividend"))
}

func (s *Server) myDividendLookup(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "My dividend", "my-dividend")
	id := strings.ToUpper(strings.TrimSpace(r.PostFormValue("resident")))
	res, err := s.Store.Resident(id)
	if err != nil || res.BirthDate != r.PostFormValue("birth_date") {
		time.Sleep(300 * time.Millisecond)
		v.ResidentInput = id
		v.Error = "No resident matches that number and date of birth. Check both and try again."
		s.render(w, http.StatusUnprocessableEntity, "my-dividend", v)
		return
	}
	payments, total, err := s.Store.ResidentPayments(res.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Resident, v.Payments, v.Total = res, payments, total
	s.render(w, http.StatusOK, "my-dividend", v)
}

// --- Officials ---------------------------------------------------------------

func (s *Server) dividendsView(r *http.Request) (*view, error) {
	v := s.newView(r, "Dividend cycle", "officials-dividends")
	v.Actions = v.Me != nil && v.Me.Can(store.RoleTreasurer)
	var err error
	if v.Cycles, err = s.cycles(); err != nil {
		return nil, err
	}
	for _, c := range v.Cycles {
		if c.Status == "Awaiting settlement" {
			v.Waiting = true
		}
	}
	if v.Active, _, err = s.Store.ResidentCounts(time.Now()); err != nil {
		return nil, err
	}
	v.NextPeriod = time.Now().UTC().Format("2006-01")
	if len(v.Cycles) > 0 {
		if t, err := time.Parse("2006-01", v.Cycles[0].Period); err == nil {
			v.NextPeriod = t.AddDate(0, 1, 0).Format("2006-01")
		}
	}
	return v, nil
}

func (s *Server) officialsDividends(w http.ResponseWriter, r *http.Request) {
	v, err := s.dividendsView(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "officials-dividends", v)
}

// deliveryProblem explains why a record could not be delivered.
func deliveryProblem(what string, err error) string {
	if rc, ok := protocol.IsRejected(err); ok {
		return fmt.Sprintf("The Commonwealth refused the %s: %s.", what, rc.Detail)
	}
	return fmt.Sprintf("The %s was saved and logged, but could not be delivered (%v). Use Send again once the Commonwealth is reachable.", what, err)
}

func (s *Server) dividendsError(w http.ResponseWriter, r *http.Request, msg string) {
	v, err := s.dividendsView(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Error = msg
	s.render(w, http.StatusUnprocessableEntity, "officials-dividends", v)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	env, err := s.State.ReportEligibility(r.Context(), r.PostFormValue("period"))
	switch {
	case errors.Is(err, state.ErrAlreadyReported):
		s.dividendsError(w, r, "An eligibility count was already sent for that period.")
	case err != nil && env == nil:
		s.dividendsError(w, r, "The count could not be made: "+err.Error())
	case err != nil:
		s.dividendsError(w, r, deliveryProblem("eligibility count", err))
	default:
		s.logAction(r, "reported eligibility count", r.PostFormValue("period"))
		http.Redirect(w, r, "/officials?done=reported", http.StatusSeeOther)
	}
}

func (s *Server) pay(w http.ResponseWriter, r *http.Request) {
	env, err := s.State.Pay(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrState), errors.Is(err, store.ErrNotFound):
		s.dividendsError(w, r, "That settlement cannot be paid: it may already have been paid.")
	case err != nil && env == nil:
		s.fail(w, err)
	case err != nil:
		s.logAction(r, "paid dividend", "settlement "+r.PathValue("id"))
		s.dividendsError(w, r, "Residents have been paid. "+deliveryProblem("confirmation", err))
	default:
		s.logAction(r, "paid dividend", "settlement "+r.PathValue("id"))
		http.Redirect(w, r, "/officials?done=paid", http.StatusSeeOther)
	}
}

func (s *Server) resend(w http.ResponseWriter, r *http.Request) {
	if err := s.State.Resend(r.Context(), r.PathValue("id")); err != nil {
		s.dividendsError(w, r, deliveryProblem("record", err))
		return
	}
	s.logAction(r, "resent record", r.PathValue("id"))
	http.Redirect(w, r, "/officials?done=resent", http.StatusSeeOther)
}

func (s *Server) residentsView(r *http.Request) (*view, error) {
	v := s.newView(r, "Residents", "officials-residents")
	v.Query = strings.TrimSpace(r.URL.Query().Get("q"))
	var err error
	if v.Residents, err = s.Store.Residents(v.Query, 200); err != nil {
		return nil, err
	}
	v.Active, _, err = s.Store.ResidentCounts(time.Now())
	return v, err
}

func (s *Server) residents(w http.ResponseWriter, r *http.Request) {
	v, err := s.residentsView(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "officials-residents", v)
}

func (s *Server) addResident(w http.ResponseWriter, r *http.Request) {
	id, err := s.Store.AddResident(r.PostFormValue("name"), r.PostFormValue("birth_date"))
	if errors.Is(err, store.ErrInvalidResident) {
		v, verr := s.residentsView(r)
		if verr != nil {
			s.fail(w, verr)
			return
		}
		v.Error = "The resident was not registered: " + strings.TrimPrefix(err.Error(), store.ErrInvalidResident.Error()+": ") + "."
		s.render(w, http.StatusUnprocessableEntity, "officials-residents", v)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.logAction(r, "registered resident", id)
	http.Redirect(w, r, "/officials/residents?done=registered", http.StatusSeeOther)
}

func (s *Server) residentStatus(w http.ResponseWriter, r *http.Request) {
	err := s.Store.SetResidentStatus(r.PathValue("id"), r.PostFormValue("status"))
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidResident) {
		http.Error(w, "No such resident or status.", http.StatusUnprocessableEntity)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.logAction(r, "changed resident status", r.PathValue("id")+" to "+r.PostFormValue("status"))
	http.Redirect(w, r, "/officials/residents?done=status", http.StatusSeeOther)
}

func (s *Server) officialsLedger(w http.ResponseWriter, r *http.Request) {
	v := s.newView(r, "Ledger", "officials-ledger")
	v.Checkpoint = s.checkpoint()
	v.WitnessCount = len(s.State.Witnesses.Witnesses)
	s.render(w, http.StatusOK, "officials-ledger", v)
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	s.logAction(r, "published checkpoint", "")
	if _, err := s.State.PublishLedger(r.Context()); err != nil {
		v := s.newView(r, "Ledger", "officials-ledger")
		v.Checkpoint = s.checkpoint()
		v.WitnessCount = len(s.State.Witnesses.Witnesses)
		v.Error = "The checkpoint was published, but not every witness cosigned it: " + err.Error()
		s.render(w, http.StatusBadGateway, "officials-ledger", v)
		return
	}
	http.Redirect(w, r, "/officials/ledger?done=published", http.StatusSeeOther)
}

func (s *Server) teamView(r *http.Request) (*view, error) {
	v := s.newView(r, "Officials", "officials-team")
	v.Roles = store.Roles
	var err error
	if v.Officials, err = s.Store.Officials(); err != nil {
		return nil, err
	}
	v.ActionLog, err = s.Store.Actions(100)
	return v, err
}

func (s *Server) team(w http.ResponseWriter, r *http.Request) {
	v, err := s.teamView(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "officials-team", v)
}

// teamResult shows the team page with a message, and a new password once if
// one was just set.
func (s *Server) teamResult(w http.ResponseWriter, r *http.Request, status int, notice, problem, username, password string) {
	v, err := s.teamView(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Notice, v.Error, v.NewUsername, v.NewPassword = notice, problem, username, password
	s.render(w, status, "officials-team", v)
}

func (s *Server) addOfficial(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.PostFormValue("username")))
	password, err := store.GeneratePassword()
	if err != nil {
		s.fail(w, err)
		return
	}
	err = s.Store.AddOfficial(username, r.PostFormValue("name"), r.PostFormValue("role"), password)
	if errors.Is(err, store.ErrInvalidOfficial) {
		s.teamResult(w, r, http.StatusUnprocessableEntity, "", "The account was not created: "+strings.TrimPrefix(err.Error(), store.ErrInvalidOfficial.Error()+": ")+".", "", "")
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.logAction(r, "added official", username+" as "+r.PostFormValue("role"))
	s.teamResult(w, r, http.StatusOK, "Account created.", "", username, password)
}

func (s *Server) resetOfficial(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	password, err := store.GeneratePassword()
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.SetPassword(username, password); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "No such official.", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Store.SetDisabled(username, false); err != nil {
		s.fail(w, err)
		return
	}
	s.logAction(r, "reset password", username)
	if username == me(r).Username {
		// Resetting your own password signs you out everywhere, here too.
		http.Redirect(w, r, "/officials/login", http.StatusSeeOther)
		return
	}
	s.teamResult(w, r, http.StatusOK, "Password reset. The official has been signed out everywhere.", "", username, password)
}

func (s *Server) disableOfficial(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if username == me(r).Username {
		s.teamResult(w, r, http.StatusUnprocessableEntity, "", "You cannot disable your own account. Ask another admin.", "", "")
		return
	}
	if err := s.Store.SetDisabled(username, true); errors.Is(err, store.ErrNotFound) {
		http.Error(w, "No such official.", http.StatusNotFound)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.logAction(r, "disabled official", username)
	s.teamResult(w, r, http.StatusOK, "Account disabled and signed out everywhere. Reset its password to enable it again.", "", "", "")
}
