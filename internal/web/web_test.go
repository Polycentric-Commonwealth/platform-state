package web

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/polycentric-commonwealth/platform-state/internal/devnet"
)

const password = "correct horse"

func start(t *testing.T) (*httptest.Server, *devnet.Network) {
	t.Helper()
	net, err := devnet.Start(devnet.Options{DataDir: t.TempDir(), Residents: 12, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(net.Close)
	site, err := New(net.State, net.Store, password, false)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(site)
	t.Cleanup(srv.Close)
	return srv, net
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func get(t *testing.T, c *http.Client, u string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body(t, resp)
}

func post(t *testing.T, c *http.Client, u string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body(t, resp)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func signIn(t *testing.T, srv *httptest.Server) (*http.Client, string) {
	t.Helper()
	c := client(t)
	resp, _ := post(t, c, srv.URL+"/officials/login", url.Values{"password": {password}, "next": {"/officials"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	_, page := get(t, c, srv.URL+"/officials")
	m := csrfPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSRF token on the officials page")
	}
	return c, m[1]
}

func TestPublicPages(t *testing.T) {
	srv, _ := start(t)
	c := client(t)
	for _, path := range []string{"/", "/dividends", "/ledger", "/my-dividend", "/officials/login", "/static/style.css", "/.well-known/pc-instance"} {
		resp, _ := get(t, c, srv.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, resp.StatusCode)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no Content-Security-Policy", path)
		}
	}
	for _, path := range []string{"/ledger/0", "/ledger/abc", "/nothing"} {
		if resp, _ := get(t, c, srv.URL+path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestOfficialsAreProtected(t *testing.T) {
	srv, _ := start(t)
	c := client(t)
	resp, _ := get(t, c, srv.URL+"/officials/residents")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/officials/login") {
		t.Errorf("unauthenticated GET: %d to %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := post(t, c, srv.URL+"/officials/report", url.Values{"period": {"2027-01"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("unauthenticated POST: %d", resp.StatusCode)
	}
	if resp, page := post(t, c, srv.URL+"/officials/login", url.Values{"password": {"wrong"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "not correct") {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}

	signedIn, csrf := signIn(t, srv)
	if resp, _ := post(t, signedIn, srv.URL+"/officials/report", url.Values{"period": {"2027-01"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF token: %d", resp.StatusCode)
	}
	if resp, _ := post(t, signedIn, srv.URL+"/officials/report", url.Values{"period": {"2027-01"}, "csrf": {"forged"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST with wrong CSRF token: %d", resp.StatusCode)
	}

	// The open-redirect guard keeps sign-in inside the officials' area.
	resp, _ = post(t, client(t), srv.URL+"/officials/login", url.Values{"password": {password}, "next": {"//evil.example/"}})
	if loc := resp.Header.Get("Location"); loc != "/officials" {
		t.Errorf("redirect after sign-in went to %q", loc)
	}

	// After signing out, the session no longer works.
	post(t, signedIn, srv.URL+"/officials/logout", url.Values{"csrf": {csrf}})
	if resp, _ := get(t, signedIn, srv.URL+"/officials"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after sign-out: %d", resp.StatusCode)
	}
}

func TestDividendCycleThroughTheWeb(t *testing.T) {
	srv, net := start(t)
	c, csrf := signIn(t, srv)

	resp, _ := post(t, c, srv.URL+"/officials/report", url.Values{"period": {"2027-01"}, "csrf": {csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("report: %d", resp.StatusCode)
	}
	if resp, page := post(t, c, srv.URL+"/officials/report", url.Values{"period": {"2027-01"}, "csrf": {csrf}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, "already sent") {
		t.Errorf("second report: %d", resp.StatusCode)
	}

	var payPath string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && payPath == ""; time.Sleep(50 * time.Millisecond) {
		_, page := get(t, c, srv.URL+"/officials")
		if m := regexp.MustCompile(`action="(/officials/pay/[^"]+)"`).FindStringSubmatch(page); m != nil {
			payPath = m[1]
		}
	}
	if payPath == "" {
		t.Fatal("no Pay button appeared")
	}
	if resp, _ := post(t, c, srv.URL+payPath, url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("pay: %d", resp.StatusCode)
	}
	if _, page := get(t, client(t), srv.URL+"/dividends"); !strings.Contains(page, "306.00 XCU") || !strings.Contains(page, "Paid") {
		t.Errorf("public dividends page does not show the paid period")
	}
	if _, page := get(t, client(t), srv.URL+"/ledger"); strings.Count(page, "Witnessed") != 2 {
		t.Errorf("public ledger does not show two witnessed records")
	}

	// A resident sees their payment; a wrong date of birth reveals nothing.
	r, _ := net.Store.Resident("R-000004")
	resp, page := post(t, client(t), srv.URL+"/my-dividend", url.Values{"resident": {"r-000004"}, "birth_date": {r.BirthDate}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, r.Name) || !strings.Contains(page, "25.50 XCU") {
		t.Errorf("resident lookup: %d", resp.StatusCode)
	}
	resp, page = post(t, client(t), srv.URL+"/my-dividend", url.Values{"resident": {"R-000004"}, "birth_date": {"1900-01-01"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(page, r.Name) {
		t.Errorf("wrong date of birth: %d, leaked name: %v", resp.StatusCode, strings.Contains(page, r.Name))
	}
}

func TestResidentRegister(t *testing.T) {
	srv, _ := start(t)
	c, csrf := signIn(t, srv)
	resp, _ := post(t, c, srv.URL+"/officials/residents", url.Values{"name": {"  Ana   Moreau "}, "birth_date": {"1990-05-04"}, "csrf": {csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("register: %d", resp.StatusCode)
	}
	if _, page := get(t, c, srv.URL+"/officials/residents?q=Moreau"); !strings.Contains(page, "Ana Moreau") || !strings.Contains(page, "R-000013") {
		t.Error("registered resident not found")
	}
	for _, bad := range []url.Values{{"name": {""}, "birth_date": {"1990-01-01"}}, {"name": {"X"}, "birth_date": {"2999-01-01"}}, {"name": {"X"}, "birth_date": {"nope"}}} {
		bad.Set("csrf", csrf)
		if resp, _ := post(t, c, srv.URL+"/officials/residents", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("invalid resident %v: %d", bad, resp.StatusCode)
		}
	}
	// Names are escaped, never interpreted as HTML.
	post(t, c, srv.URL+"/officials/residents", url.Values{"name": {"<script>alert(1)</script>"}, "birth_date": {"1990-01-01"}, "csrf": {csrf}})
	if _, page := get(t, c, srv.URL+"/officials/residents"); strings.Contains(page, "<script>alert") {
		t.Error("resident name was not escaped")
	}
}
