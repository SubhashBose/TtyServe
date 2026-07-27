package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ttyserve/internal/config"
)

// Authenticator resolves an HTTP request to a stable client identity string.
type Authenticator struct {
	cfg    config.Config
	users  map[string]string // name -> password
	secret []byte            // HMAC secret for short-term cookies
}

// New builds an Authenticator. A random secret is generated per process; that
// means short-term cookies are invalidated on restart, which is acceptable for
// the short-term use case.
func New(cfg config.Config) (*Authenticator, error) {
	a := &Authenticator{cfg: cfg, users: map[string]string{}}
	for _, u := range cfg.Users {
		a.users[u.Name] = u.Password
	}
	a.secret = make([]byte, 32)
	if _, err := rand.Read(a.secret); err != nil {
		return nil, fmt.Errorf("generate cookie secret: %w", err)
	}
	return a, nil
}

// ErrUnauthorized indicates the request failed basic-auth.
var ErrUnauthorized = errors.New("unauthorized")

// ErrNoIdentityHeader indicates proxy_header mode got a request without the
// configured identity header. Fail closed: a misconfigured proxy must not
// hand out anonymous or shared sessions.
var ErrNoIdentityHeader = errors.New("missing identity header (proxy_header mode expects the reverse proxy to set it)")

// Identity is the result of authenticating a request.
type Identity struct {
	// Key is the stable identity used to look up the client's sessions.
	Key string
	// SetCookie, if non-nil, must be written to the response (short-term mode
	// issuing a fresh cookie).
	SetCookie *http.Cookie
}

// Authenticate resolves the request identity according to the configured mode.
// For user mode it validates basic-auth and, on failure, returns ErrUnauthorized
// (the caller should send a 401 with WWW-Authenticate).
func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	// Persistence off: identity is a per-page ephemeral token. The index
	// page mints one (no eid param) and echoes it to the frontend, which
	// sends it back as ?eid=... on the API and websocket so all of a page's
	// requests resolve to the same client. No cookie, no cross-page sharing:
	// a reload gets a fresh identity, and the session dies with the socket.
	if !a.cfg.SessionPersistence {
		// Configured users act as a plain basic-auth gate even in ephemeral
		// mode: they control access only — identity/session semantics stay
		// fully ephemeral (per-page, dies with the socket).
		if len(a.users) > 0 {
			if !a.basicAuthOK(r) {
				return Identity{}, ErrUnauthorized
			}
		}
		eid := r.URL.Query().Get("eid")
		// Anti-fixation: the index NEVER honors a client-supplied eid — the
		// page only sends eid on API/websocket requests, so an eid on "/" can
		// only be an attacker-crafted link trying to plant a known identity
		// on the victim's page (letting the attacker attach to their
		// terminal). Also cap hostile/garbage tokens rather than stuffing
		// arbitrarily large keys into the client map.
		if r.URL.Path == "/" || eid == "" || len(eid) > 64 {
			eid = randToken(16)
		}
		return Identity{Key: "ephemeral-" + eid}, nil
	}

	switch a.cfg.PersistenceMode {
	case config.PersistByUser:
		user, _, _ := r.BasicAuth()
		if !a.basicAuthOK(r) {
			return Identity{}, ErrUnauthorized
		}
		return Identity{Key: "user:" + user}, nil

	case config.PersistShortTerm:
		// Configured users gate access in EVERY mode. Without this, `users:`
		// combined with short_term — the default persistence mode — was
		// silently ignored: a config that reads as authenticated served
		// terminals to anyone, and any share link became a bearer token to a
		// live shell for a passer-by.
		user, err := a.gateUser(r)
		if err != nil {
			return Identity{}, err
		}
		return a.shortTerm(r, user)

	case config.PersistProxyHeader:
		// Trust the reverse proxy to have authenticated the user and put a
		// stable identifier in the configured header. Spoofing is prevented
		// by deployment (bind to unix:// or loopback), not by this code.
		// Configured users are an additional door, not the identity: the
		// header stays authoritative for which sessions the request sees.
		if _, err := a.gateUser(r); err != nil {
			return Identity{}, err
		}
		v := strings.TrimSpace(r.Header.Get(a.cfg.ProxyHeaderName))
		if v == "" {
			return Identity{}, ErrNoIdentityHeader
		}
		return Identity{Key: "header:" + v}, nil

	default:
		return Identity{}, fmt.Errorf("unknown persistence mode")
	}
}

// --- Anonymous share-link guests ---
//
// A public share link ("anyone with the link") admits a visitor who has no
// credentials. They still need a stable identity to hold the shared reference
// across reloads, so the server mints one and carries it in its own signed,
// HttpOnly cookie. Two properties matter:
//
//   - The token is server-generated and signed, never taken from the request:
//     a client can no more choose its guest identity than it can forge a
//     session cookie (identity fixation).
//   - Moving the identity into an HttpOnly cookie gets the capability out of
//     the address bar, so the share token itself need not stay in the URL.
//
// The cookie alone grants nothing: access comes from the shared reference the
// guest holds, which the owner can revoke at any time.

// GuestCookieName is the cookie carrying an anonymous share-link identity.
// Distinct from the session cookie so the two can never be confused.
func (a *Authenticator) GuestCookieName() string { return a.cfg.CookieName + "_guest" }

// NewGuestToken mints a fresh anonymous identity and the cookie that carries it.
func (a *Authenticator) NewGuestToken() (string, *http.Cookie) {
	token := randToken(24)
	c := a.buildCookie(a.signCookie(token))
	c.Name = a.GuestCookieName()
	return token, c
}

// ClearGuestCookie expires the anonymous identity, so a guest who signs in
// stops presenting it. Without this the guest cookie lingers and would silently
// take over again the moment the browser drops its cached credentials.
func (a *Authenticator) ClearGuestCookie() *http.Cookie {
	c := a.buildCookie("")
	c.Name = a.GuestCookieName()
	c.MaxAge = -1
	return c
}

// GuestToken returns the identity token from a valid guest cookie.
func (a *Authenticator) GuestToken(r *http.Request) (string, bool) {
	c, err := r.Cookie(a.GuestCookieName())
	if err != nil {
		return "", false
	}
	return a.verifyCookie(c.Value)
}

// gateUser enforces configured users as a plain access gate, returning the
// authenticated name ("" when no users are configured, i.e. the gate is off).
// Used by the modes whose identity comes from somewhere else (cookie, proxy
// header) so that `users:` never silently does nothing.
func (a *Authenticator) gateUser(r *http.Request) (string, error) {
	if len(a.users) == 0 {
		return "", nil
	}
	if !a.basicAuthOK(r) {
		return "", ErrUnauthorized
	}
	user, _, _ := r.BasicAuth()
	return user, nil
}

// shortTerm validates an existing signed cookie or mints a new one. In both
// cases a Set-Cookie is returned: refreshing the cookie on every request gives
// sliding expiration, so an actively-used browser never drops the cookie while
// the server still holds sessions for it. (Without this, MaxAge would expire
// the cookie after idle_timeout even during continuous use, silently assigning
// the user a new identity.)
func (a *Authenticator) shortTerm(r *http.Request, user string) (Identity, error) {
	// The cookie identifies the BROWSER. When users are configured the identity
	// must also include the authenticated user, because the two have very
	// different lifetimes: the cookie lives 30+ days while basic-auth
	// credentials are dropped when the browser closes. Keyed on the cookie
	// alone, a second user logging in on the same browser would inherit the
	// first user's sessions.
	key := func(token string) string {
		if user == "" {
			return "cookie:" + token
		}
		return "cookie:" + token + "|user:" + user
	}
	if c, err := r.Cookie(a.cfg.CookieName); err == nil {
		if token, ok := a.verifyCookie(c.Value); ok {
			return Identity{Key: key(token), SetCookie: a.buildCookie(c.Value)}, nil
		}
	}
	token := randToken(24)
	return Identity{Key: key(token), SetCookie: a.buildCookie(a.signCookie(token))}, nil
}

func (a *Authenticator) buildCookie(value string) *http.Cookie {
	// The cookie must outlive idle_timeout by a wide margin: an open
	// websocket keeps the server-side client alive indefinitely but never
	// refreshes the browser cookie (only HTTP requests do). With
	// MaxAge == idle_timeout, a page left open past the timeout would
	// silently lose its cookie and get a fresh identity — and lose its
	// tabs — on the next reload. Session lifetime is enforced server-side
	// by the reaper regardless, so a long cookie changes nothing else.
	life := a.cfg.IdleTimeout
	if min := 30 * 24 * time.Hour; life < min {
		life = min
	}
	return &http.Cookie{
		Name:  a.cfg.CookieName,
		Value: value,
		// No Path attribute: per RFC 6265, the browser then scopes the
		// cookie to the directory of the URL *it* requested — which includes
		// any proxy mount prefix the server never sees (/code/proxy/7681/ →
		// Path=/code/proxy/7681). Automatic scoping keeps the token away
		// from sibling apps on the same host. Requires that Set-Cookie is
		// only emitted on requests whose browser-side directory is the app
		// root — the index and /ws — which resolve() enforces.
		HttpOnly: true,
		Secure: a.cfg.CookieSecure,
		// Lax, not Strict: Strict cookies are withheld from top-level
		// navigations in tabs that were originally opened cross-site — and
		// reloads inherit that initiator, so such a tab would silently lose
		// its identity on every refresh. Lax still withholds the cookie on
		// cross-site fetches and non-GET requests (the CSRF-relevant ones),
		// and the websocket is guarded by the same-host Origin check.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(life / time.Second),
	}
}

// basicAuthOK validates the request's basic-auth credentials against the
// configured users, in constant time.
func (a *Authenticator) basicAuthOK(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	want, exists := a.users[user]
	if !exists {
		// Compare against a dummy anyway so unknown usernames take the same
		// time as wrong passwords (no username enumeration via timing).
		subtle.ConstantTimeCompare([]byte(pass), []byte("no-such-user-dummy"))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
}

// WriteUnauthorized emits a 401 with a basic-auth challenge.
func (a *Authenticator) WriteUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", a.cfg.AuthRealm))
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

// --- cookie signing ---

func (a *Authenticator) signCookie(token string) string {
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(token))
	sig := mac.Sum(nil)
	return token + "." + hex.EncodeToString(sig)
}

func (a *Authenticator) verifyCookie(value string) (string, bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	token, sigHex := parts[0], parts[1]
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(token))
	expected := mac.Sum(nil)
	if !hmac.Equal(sig, expected) {
		return "", false
	}
	return token, true
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
