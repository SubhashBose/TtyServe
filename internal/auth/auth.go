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
		eid := r.URL.Query().Get("eid")
		// Cap hostile/garbage tokens: an over-long eid gets a fresh identity
		// instead of stuffing an arbitrarily large key into the client map.
		if eid == "" || len(eid) > 64 {
			eid = randToken(16)
		}
		return Identity{Key: "ephemeral-" + eid}, nil
	}

	switch a.cfg.PersistenceMode {
	case config.PersistByUser:
		user, pass, ok := r.BasicAuth()
		if !ok {
			return Identity{}, ErrUnauthorized
		}
		want, exists := a.users[user]
		// Constant-time comparison to avoid leaking via timing.
		userOK := exists && subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
		if !userOK {
			return Identity{}, ErrUnauthorized
		}
		return Identity{Key: "user:" + user}, nil

	case config.PersistShortTerm:
		return a.shortTerm(r)

	case config.PersistProxyHeader:
		// Trust the reverse proxy to have authenticated the user and put a
		// stable identifier in the configured header. Spoofing is prevented
		// by deployment (bind to unix:// or loopback), not by this code.
		v := strings.TrimSpace(r.Header.Get(a.cfg.ProxyHeaderName))
		if v == "" {
			return Identity{}, ErrNoIdentityHeader
		}
		return Identity{Key: "header:" + v}, nil

	default:
		return Identity{}, fmt.Errorf("unknown persistence mode")
	}
}

// shortTerm validates an existing signed cookie or mints a new one. In both
// cases a Set-Cookie is returned: refreshing the cookie on every request gives
// sliding expiration, so an actively-used browser never drops the cookie while
// the server still holds sessions for it. (Without this, MaxAge would expire
// the cookie after idle_timeout even during continuous use, silently assigning
// the user a new identity.)
func (a *Authenticator) shortTerm(r *http.Request) (Identity, error) {
	if c, err := r.Cookie(a.cfg.CookieName); err == nil {
		if token, ok := a.verifyCookie(c.Value); ok {
			return Identity{Key: "cookie:" + token, SetCookie: a.buildCookie(c.Value)}, nil
		}
	}
	token := randToken(24)
	return Identity{Key: "cookie:" + token, SetCookie: a.buildCookie(a.signCookie(token))}, nil
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
		Name:     a.cfg.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(life / time.Second),
	}
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
