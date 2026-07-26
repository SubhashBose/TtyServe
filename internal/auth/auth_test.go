package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ttyserve/internal/config"
)

func testAuth(t *testing.T, mode config.PersistenceMode, users bool) *Authenticator {
	t.Helper()
	cfg := config.Default()
	cfg.SessionPersistence = true
	cfg.PersistenceMode = mode
	if users {
		cfg.Users = []config.User{{Name: "alice", Password: "apass"}, {Name: "bob", Password: "bpass"}}
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func req(user, pass string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if user != "" {
		r.SetBasicAuth(user, pass)
	}
	return r
}

// Configured users must gate access in short_term mode — the DEFAULT
// persistence mode. Previously they were ignored entirely there, so a config
// that looked authenticated served terminals to anyone, and any share link
// became a bearer token to a live shell.
func TestShortTermHonorsConfiguredUsers(t *testing.T) {
	a := testAuth(t, config.PersistShortTerm, true)

	if _, err := a.Authenticate(req("", "")); err != ErrUnauthorized {
		t.Errorf("no credentials must be rejected, got %v", err)
	}
	if _, err := a.Authenticate(req("alice", "wrong")); err != ErrUnauthorized {
		t.Errorf("wrong password must be rejected, got %v", err)
	}
	if _, err := a.Authenticate(req("nobody", "apass")); err != ErrUnauthorized {
		t.Errorf("unknown user must be rejected, got %v", err)
	}
	id, err := a.Authenticate(req("alice", "apass"))
	if err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	if id.Key == "" || id.SetCookie == nil {
		t.Fatal("expected a cookie identity for an authenticated short_term request")
	}
}

// Without users configured, short_term stays open (unchanged behaviour): the
// gate only exists when the operator asked for it.
func TestShortTermWithoutUsersStaysOpen(t *testing.T) {
	a := testAuth(t, config.PersistShortTerm, false)
	id, err := a.Authenticate(req("", ""))
	if err != nil {
		t.Fatalf("short_term without users must not require auth: %v", err)
	}
	if !strings.HasPrefix(id.Key, "cookie:") {
		t.Fatalf("expected a cookie identity, got %q", id.Key)
	}
	if strings.Contains(id.Key, "|user:") {
		t.Fatal("identity must not be user-bound when no users are configured")
	}
}

// A browser cookie outlives basic-auth credentials by weeks, so the identity
// must bind BOTH: otherwise a second user logging in on the same browser
// inherits the first user's sessions.
func TestShortTermIdentityIsPerUserNotPerBrowser(t *testing.T) {
	a := testAuth(t, config.PersistShortTerm, true)

	first, err := a.Authenticate(req("alice", "apass"))
	if err != nil {
		t.Fatal(err)
	}
	// Replay alice's cookie, but authenticated as bob (same browser).
	r := req("bob", "bpass")
	r.AddCookie(&http.Cookie{Name: a.cfg.CookieName, Value: first.SetCookie.Value})
	second, err := a.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if second.Key == first.Key {
		t.Fatal("SECURITY: a different user on the same browser cookie got the same identity")
	}

	// Same user + same cookie must be stable, or tabs would be lost on reload.
	r2 := req("alice", "apass")
	r2.AddCookie(&http.Cookie{Name: a.cfg.CookieName, Value: first.SetCookie.Value})
	again, err := a.Authenticate(r2)
	if err != nil {
		t.Fatal(err)
	}
	if again.Key != first.Key {
		t.Fatalf("same user+cookie must be a stable identity: %q vs %q", again.Key, first.Key)
	}
}

// proxy_header mode: users are an additional door. The header stays
// authoritative for identity, but a configured user list must not be ignored.
func TestProxyHeaderHonorsConfiguredUsers(t *testing.T) {
	a := testAuth(t, config.PersistProxyHeader, true)

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-User", "alice")
	if _, err := a.Authenticate(r); err != ErrUnauthorized {
		t.Errorf("proxy_header with users must require basic auth, got %v", err)
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.Header.Set("X-Forwarded-User", "alice")
	r2.SetBasicAuth("alice", "apass")
	id, err := a.Authenticate(r2)
	if err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	if id.Key != "header:alice" {
		t.Fatalf("header must remain authoritative for identity, got %q", id.Key)
	}

	// Still fails closed when the proxy forgot the header.
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.SetBasicAuth("alice", "apass")
	if _, err := a.Authenticate(r3); err != ErrNoIdentityHeader {
		t.Errorf("missing identity header must fail closed, got %v", err)
	}
}

// user mode is unchanged: credentials ARE the identity.
func TestUserModeUnchanged(t *testing.T) {
	a := testAuth(t, config.PersistByUser, true)
	if _, err := a.Authenticate(req("", "")); err != ErrUnauthorized {
		t.Errorf("user mode must require credentials, got %v", err)
	}
	id, err := a.Authenticate(req("bob", "bpass"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Key != "user:bob" {
		t.Fatalf("expected user:bob, got %q", id.Key)
	}
}
