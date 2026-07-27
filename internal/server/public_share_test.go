package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/session"

	"github.com/gorilla/websocket"
)

// publicServer starts a server requiring auth, with public share links allowed
// up to the given ceiling.
func publicServer(t *testing.T, ceiling string) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Command = "/bin/bash"
	cfg.SessionPersistence = true
	cfg.PersistenceMode = config.PersistByUser
	cfg.AllowSharing = true
	cfg.PublicShareLinks = ceiling
	cfg.MultiSession = true
	cfg.Users = []config.User{{Name: "alice", Password: "apass"}, {Name: "bob", Password: "bpass"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	a, err := auth.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(cfg)
	srv, err := New(cfg, a, mgr)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); mgr.Shutdown() })
	return ts
}

// anon performs a request with NO credentials, carrying the given cookies.
func anon(t *testing.T, ts *httptest.Server, method, path, body string, cookies []*http.Cookie) (int, string, []*http.Cookie) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Cookies()
}

func mintShare(t *testing.T, ts *httptest.Server, sid, payload string) (string, int) {
	t.Helper()
	code, body := do(t, ts, "alice", "POST", "/sessions/"+sid+"/share", payload)
	if code != 200 {
		return "", code
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Token, code
}

// The whole point: an anonymous visitor with a public link gets in, and the
// terminal appears in their (otherwise empty) tab list.
func TestPublicLinkAdmitsAnonymousGuest(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	tok, code := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`)
	if tok == "" {
		t.Fatalf("mint public link: %d", code)
	}

	// Without the link, an anonymous request is still refused.
	if st, _, _ := anon(t, ts, "GET", "/sessions", "", nil); st != http.StatusUnauthorized {
		t.Fatalf("anonymous access without a link must be 401, got %d", st)
	}

	// Arriving on the link mints a guest cookie.
	st, _, cookies := anon(t, ts, "GET", "/?share="+tok, "", nil)
	if st != http.StatusOK {
		t.Fatalf("public link should serve the page anonymously, got %d", st)
	}
	if len(cookies) == 0 {
		t.Fatal("expected a guest cookie to be set")
	}
	st, body, _ := anon(t, ts, "POST", "/sessions/accept", `{"token":"`+tok+`"}`, cookies)
	if st != http.StatusOK {
		t.Fatalf("guest accept failed: %d %s", st, body)
	}
	st, body, _ = anon(t, ts, "GET", "/sessions", "", cookies)
	if st != http.StatusOK || !strings.Contains(body, sid) {
		t.Fatalf("guest should see the shared session: %d %s", st, body)
	}
}

// The load-bearing restriction: a share link must never become a way to get
// your own shell without credentials.
func TestGuestCannotCreateTerminals(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	tok, _ := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`)

	_, _, cookies := anon(t, ts, "GET", "/?share="+tok, "", nil)
	if st, body, _ := anon(t, ts, "POST", "/sessions", `{}`, cookies); st != http.StatusForbidden {
		t.Fatalf("SECURITY: guest could create a session: %d %s", st, body)
	}
	// The websocket path that would also spawn one.
	if st, _, _ := anon(t, ts, "GET", "/ws", "", cookies); st != http.StatusForbidden {
		t.Fatalf("SECURITY: guest could spawn a terminal via /ws, got %d", st)
	}
	// Landing on the index must not have auto-created one either.
	_, body, _ := anon(t, ts, "GET", "/sessions", "", cookies)
	var list []session.SessionInfo
	_ = json.Unmarshal([]byte(body), &list)
	for _, s := range list {
		if !s.Shared {
			t.Fatalf("SECURITY: guest owns a session it did not get from a share: %+v", s)
		}
	}
}

// A guest identity minted from one public link must not be replayable to
// accept a different, private link.
func TestGuestCannotAcceptPrivateLink(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	pub, _ := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`)
	priv, _ := mintShare(t, ts, sid, `{"readOnly":true,"singleUse":true}`) // not public

	_, _, cookies := anon(t, ts, "GET", "/?share="+pub, "", nil)
	if st, body, _ := anon(t, ts, "POST", "/sessions/accept", `{"token":"`+priv+`"}`, cookies); st == http.StatusOK {
		t.Fatalf("SECURITY: guest accepted a private share link: %s", body)
	}
}

// A private link must not admit an anonymous visitor at all.
func TestPrivateLinkStillRequiresAuth(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	priv, _ := mintShare(t, ts, sid, `{"readOnly":true}`)

	if st, _, _ := anon(t, ts, "GET", "/?share="+priv, "", nil); st != http.StatusUnauthorized {
		t.Fatalf("a non-public link must not admit an anonymous visitor, got %d", st)
	}
}

// The admin ceiling bounds what an owner may choose.
func TestPublicShareCeiling(t *testing.T) {
	// none: public links refused outright.
	ts := publicServer(t, config.PublicShareNone)
	sid := aliceSession(t, ts)
	if _, code := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`); code == 200 {
		t.Fatal("public links must be refused when the ceiling is 'none'")
	}
	// ...but ordinary sharing still works.
	if _, code := mintShare(t, ts, sid, `{"readOnly":true}`); code != 200 {
		t.Fatalf("normal sharing must still work, got %d", code)
	}

	// readonly: view-only public links allowed, control refused.
	ts2 := publicServer(t, config.PublicShareReadOnly)
	sid2 := aliceSession(t, ts2)
	if _, code := mintShare(t, ts2, sid2, `{"readOnly":true,"public":true}`); code != 200 {
		t.Fatalf("read-only public link should be allowed, got %d", code)
	}
	if _, code := mintShare(t, ts2, sid2, `{"readOnly":false,"public":true}`); code == 200 {
		t.Fatal("SECURITY: a control public link was allowed under the read-only ceiling")
	}
}

// A guest holding a read-only public link must not be able to type — the same
// server-side gate that protects authenticated read-only sharers.
func TestGuestReadOnlyCannotInjectInput(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	tok, _ := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`)
	_, _, cookies := anon(t, ts, "GET", "/?share="+tok, "", nil)
	if st, _, _ := anon(t, ts, "POST", "/sessions/accept", `{"token":"`+tok+`"}`, cookies); st != 200 {
		t.Fatal("guest accept failed")
	}

	alice := dialWS(t, ts, "alice", sid)
	aliceOut := collect(alice)

	// Dial the guest websocket with the guest cookie.
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?session=" + sid
	h := http.Header{}
	var jar []string
	for _, c := range cookies {
		jar = append(jar, c.Name+"="+c.Value)
	}
	h.Set("Cookie", strings.Join(jar, "; "))
	guest, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("guest ws dial: %v (%d)", err, code)
	}
	defer guest.Close()

	const marker = "GUEST_INJECTED"
	_ = guest.WriteMessage(websocket.BinaryMessage, append([]byte{msgInput}, []byte("echo "+marker+"\n")...))
	if aliceOut.waitFor(marker, 1500*time.Millisecond) {
		t.Fatalf("SECURITY: read-only guest's input reached the PTY: %q", aliceOut.text())
	}
}

// Referrer-Policy must be set on every response. A share link carries its token
// in the URL, so without this a click on a link in terminal output would hand
// the token to a third-party site in the Referer header.
func TestReferrerPolicyHeader(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	for _, path := range []string{"/", "/healthz", "/static/app.js", "/sessions"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.SetBasicAuth("alice", "apass")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got := resp.Header.Get("Referrer-Policy")
		resp.Body.Close()
		if got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q, want no-referrer", path, got)
		}
	}
}

// A guest cookie satisfies resolve() everywhere else, so no ordinary endpoint
// ever answers 401 — without a dedicated route a guest could never reach the
// browser's credential prompt and would be stuck anonymous. /login skips the
// guest fallback on purpose.
func TestGuestCanSignIn(t *testing.T) {
	ts := publicServer(t, config.PublicShareAll)
	sid := aliceSession(t, ts)
	tok, _ := mintShare(t, ts, sid, `{"readOnly":true,"public":true}`)
	_, _, cookies := anon(t, ts, "GET", "/?share="+tok, "", nil)

	// Ordinary endpoints must NOT challenge (that is the trap being fixed).
	req, _ := http.NewRequest("POST", ts.URL+"/sessions", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Fatal("a guest request should not carry a challenge; that is what /login is for")
	}

	// /login with no credentials must challenge, even holding a guest cookie.
	req, _ = http.NewRequest("GET", ts.URL+"/login", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("/login must challenge a guest: %d %q",
			resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// With credentials it signs in, clears the guest identity, and redirects.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	req, _ = http.NewRequest("GET", ts.URL+"/login", nil)
	req.SetBasicAuth("bob", "bpass")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 after signing in, got %d", resp.StatusCode)
	}
	// The redirect must stay RELATIVE, or a reverse-proxy mount prefix (which
	// the server never sees) would be dropped and the user sent to the wrong app.
	if loc := resp.Header.Get("Location"); loc != "./" {
		t.Fatalf("Location must be relative to survive a proxy prefix, got %q", loc)
	}
	var cleared bool
	for _, c := range resp.Cookies() {
		if strings.Contains(c.Name, "guest") && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("signing in must expire the guest cookie")
	}

	// And bob can now do what a guest could not: create his own terminal.
	if code, body := do(t, ts, "bob", "POST", "/sessions", `{}`); code != http.StatusCreated {
		t.Fatalf("signed-in user must be able to create a terminal: %d %s", code, body)
	}
}
