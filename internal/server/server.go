package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/session"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFS embed.FS

// staticAsset is one embedded frontend file, pre-hashed and pre-compressed
// at startup so reloads can be answered with 304s or gzip bodies.
type staticAsset struct {
	body  []byte
	gz    []byte // nil when compression doesn't help
	etag  string
	ctype string
}

// Server ties config, auth, and the session manager to HTTP handlers.
type Server struct {
	cfg      config.Config
	auth     *auth.Authenticator
	mgr      *session.Manager
	upgrader websocket.Upgrader
	tmpl     *template.Template
	static   map[string]*staticAsset
	assetVer string // combined hash of all static assets, for cache busting

	// favicon is the custom icon loaded from cfg.Favicon when it is a file
	// path (nil = use the embedded default). faviconHref is what the page's
	// <link rel="icon"> points at: either the /favicon endpoint or, for
	// data: URIs, the URI itself.
	favicon     *staticAsset
	faviconHref template.URL
}

// New constructs a Server.
func New(cfg config.Config, a *auth.Authenticator, mgr *session.Manager) (*Server, error) {
	tmplData, err := webFS.ReadFile("web/index.html")
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("index").Parse(string(tmplData))
	if err != nil {
		return nil, err
	}
	static, err := loadStatic()
	if err != nil {
		return nil, err
	}
	// Version stamp for asset URLs: any asset change changes every URL, so
	// a fresh page can never run against stale cached scripts. Hash in
	// sorted order — map iteration is random and the stamp must be stable
	// across restarts of the same binary.
	names := make([]string, 0, len(static))
	for name := range static {
		names = append(names, name)
	}
	sort.Strings(names)
	ver := sha256.New()
	for _, name := range names {
		ver.Write([]byte(static[name].etag))
	}
	s := &Server{
		cfg:      cfg,
		auth:     a,
		mgr:      mgr,
		tmpl:     tmpl,
		static:   static,
		assetVer: hex.EncodeToString(ver.Sum(nil)[:6]),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Same-origin by default; extra origins (or "*") can be allowed
			// via the allow_origins config option. This matters because the
			// websocket is authenticated by cookie in short_term mode.
			CheckOrigin: func(r *http.Request) bool { return originAllowed(cfg, r) },
		},
	}

	// Resolve the favicon. A data: URI goes straight into the page's <link>
	// (same formats HTML/CSS accept); a file path is read once here, failing
	// fast on a bad config; empty uses the embedded default icon.
	switch {
	case strings.HasPrefix(cfg.Favicon, "data:"):
		s.faviconHref = template.URL(cfg.Favicon)
	case cfg.Favicon != "":
		body, err := os.ReadFile(cfg.Favicon)
		if err != nil {
			return nil, fmt.Errorf("favicon: %w", err)
		}
		ctype := mime.TypeByExtension(path.Ext(cfg.Favicon))
		if ctype == "" {
			ctype = http.DetectContentType(body)
		}
		sum := sha256.Sum256(body)
		s.favicon = &staticAsset{
			body:  body,
			etag:  fmt.Sprintf("%q", hex.EncodeToString(sum[:8])),
			ctype: ctype,
		}
		s.faviconHref = template.URL("favicon?v=" + hex.EncodeToString(sum[:6]))
	default:
		s.faviconHref = template.URL("favicon?v=" + s.assetVer)
	}
	return s, nil
}

// loadStatic reads every embedded static file once, computing its ETag and a
// gzipped copy. The frontend is a fixed set of small files, so holding both
// forms in memory is cheap (~½ MB) and makes reloads fast even without a
// caching proxy in front.
func loadStatic() (map[string]*staticAsset, error) {
	entries, err := webFS.ReadDir("web/static")
	if err != nil {
		return nil, err
	}
	assets := make(map[string]*staticAsset, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := webFS.ReadFile("web/static/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		gz := buf.Bytes()
		if len(gz) >= len(body) {
			gz = nil
		}
		ctype := mime.TypeByExtension(path.Ext(e.Name()))
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		assets[e.Name()] = &staticAsset{
			body:  body,
			gz:    gz,
			etag:  fmt.Sprintf("%q", hex.EncodeToString(sum[:8])),
			ctype: ctype,
		}
	}
	return assets, nil
}

// handleStatic serves the pre-processed embedded assets with ETag
// revalidation (304 on reload) and gzip.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	a, ok := s.static[strings.TrimPrefix(r.URL.Path, "/static/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, a)
}

// handleFavicon serves the configured icon (a file read at startup), or the
// embedded default. Also mounted at /favicon.ico for clients that probe it.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	a := s.favicon
	if a == nil {
		a = s.static["favicon.svg"]
	}
	if a == nil {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, a)
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, a *staticAsset) {
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("ETag", a.etag)
	// Requests carrying the content-hash ?v= stamp may be cached forever:
	// any content change changes the URL (the HTML referencing it is
	// no-store). Bare requests (e.g. /favicon.ico probes) get
	// cache-but-revalidate, where the ETag makes revalidation a cheap 304.
	if r.URL.Query().Has("v") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, no-cache")
	}
	h.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == a.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := a.body
	if a.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// originAllowed permits requests with no Origin header (non-browser clients),
// same-host origins, and anything in cfg.AllowOrigins ("*" allows all).
func originAllowed(cfg config.Config, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, a := range cfg.AllowOrigins {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Static assets (xterm.js etc.) under /static/.
	mux.HandleFunc("/static/", s.handleStatic)

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/sessions", s.handleSessions)     // GET list, POST create
	mux.HandleFunc("/sessions/", s.handleSessionItem) // PATCH rename, DELETE close, POST {id}/restart
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/favicon", s.handleFavicon)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// resolve authenticates and returns the client, writing any cookie. On auth
// failure it writes the response and returns ok=false.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) (*session.Client, bool) {
	id, err := s.auth.Authenticate(r)
	if err == auth.ErrUnauthorized {
		s.auth.WriteUnauthorized(w)
		return nil, false
	}
	if err == auth.ErrNoIdentityHeader {
		// No basic-auth challenge here: the proxy, not the browser, is
		// supposed to supply the identity.
		http.Error(w, err.Error(), http.StatusForbidden)
		return nil, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	// The cookie carries no Path attribute, so the browser scopes it to the
	// directory of the URL it requested — including any proxy mount prefix
	// the server never sees. That only works when Set-Cookie is emitted on
	// endpoints directly under the app root ("/", "/sessions", "/ws"): item
	// paths like /sessions/<id> have a deeper directory and would mint
	// shadow cookies, so they never set one.
	if id.SetCookie != nil {
		switch r.URL.Path {
		case "/", "/sessions", "/ws":
			http.SetCookie(w, id.SetCookie)
		}
	}
	cl := s.mgr.Client(id.Key)
	// Any authenticated request counts as activity: the reaper should only
	// collect clients with no connection AND no requests for idle_timeout.
	// (Normally the websocket keeps clients alive; this covers the windows
	// where the socket is down but the page is still polling.)
	s.mgr.Touch(cl)
	return cl, true
}

// urlSpawnParams extracts per-session spawn parameters from the request
// query when url_arg / url_env is enabled. Parameter order is preserved
// (url.Values would randomize it). In url_arg mode "?a&b=5" yields args
// ["a", "b=5"]; in url_env mode env ["a=", "b=5"].
func (s *Server) urlSpawnParams(r *http.Request) (args, env []string) {
	if !s.cfg.URLArg && !s.cfg.URLEnv {
		return nil, nil
	}
	for _, tok := range strings.Split(r.URL.RawQuery, "&") {
		if tok == "" {
			continue
		}
		k, v, _ := strings.Cut(tok, "=")
		ku, err1 := url.QueryUnescape(k)
		vu, err2 := url.QueryUnescape(v)
		if err1 != nil || err2 != nil {
			continue
		}
		// Skip query params the frontend uses internally, so they never leak
		// into the spawned command (eid = ephemeral identity, share = accept
		// token).
		if ku == "eid" || ku == "share" {
			continue
		}
		// url-arg and url-env are independent — both may be enabled, in which
		// case each parameter becomes an argument AND an env var.
		if s.cfg.URLArg {
			arg := ku
			if strings.Contains(tok, "=") {
				arg = ku + "=" + vu
			}
			args = append(args, arg)
		}
		if s.cfg.URLEnv && ku != "" {
			env = append(env, ku+"="+vu)
		}
	}
	return args, env
}

// spawnParams builds the args and full environment for a new session from
// this request: the configured env with ${header.NAME} placeholders expanded
// against request headers, followed by any URL env (url_env mode).
func (s *Server) spawnParams(r *http.Request) (args, env []string) {
	args, urlEnv := s.urlSpawnParams(r)
	env = config.ExpandHeaderEnv(s.cfg.Env, r.Header.Get)
	env = append(env, urlEnv...)
	return args, env
}

type pageData struct {
	Title              string
	MultiSession       bool
	TabBarPosition     string
	Readonly           bool
	SessionPersistence bool
	PersistenceMode    string
	FontSize           int
	EnableGraphics     bool
	DisableHyperlink   bool
	MiddleclickPaste   bool
	ClipboardWrite     bool
	Bell               string
	TabShowPS1         bool
	AutoRespawn        bool
	CloseOnExit        bool
	DOMRenderer        bool
	AllowSharing       bool
	// PingSeconds lets the client derive the server's dead-peer deadline
	// (3× ping-interval) for its own liveness give-up limit.
	PingSeconds int
	// Sessions is inlined into the page so the frontend can build tabs and
	// open websockets immediately, without a follow-up API round trip.
	Sessions []session.SessionInfo
	// V is the asset version appended to static URLs (cache busting).
	V string
	// EphemeralID is the page identity token in persistence-off mode, echoed
	// back by the frontend as ?eid=... so all its requests share a client.
	EphemeralID string
	// Favicon is a pre-vetted href (endpoint URL or data: URI); typed URL so
	// the template doesn't filter the data: scheme.
	Favicon template.URL
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	// Guarantee at least one session exists on first load — but not in
	// ephemeral mode: there each page load is a fresh identity with no
	// reaper, so pre-spawning here would leak a shell for every page hit
	// (bots, prefetch). The frontend creates the session on demand instead,
	// and it's discarded when the socket closes.
	if s.cfg.SessionPersistence {
		args, env := s.spawnParams(r)
		if _, err := s.mgr.EnsureDefaultSession(cl, args, env); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	// In ephemeral mode, hand the frontend the page's identity token so its
	// API/websocket requests resolve to this same client.
	ephemeralID := ""
	if !s.cfg.SessionPersistence {
		ephemeralID = strings.TrimPrefix(cl.ID, "ephemeral-")
	}
	// The page embeds the client's session list and mints cookies: it is
	// personalized state and must never be served from any cache — a stale
	// copy carries dead session ids and the frontend would abandon the
	// client's real sessions.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.Execute(w, pageData{
		Title:              s.cfg.Title,
		MultiSession:       s.cfg.MultiSession,
		TabBarPosition:     s.cfg.TabBarPosition,
		Readonly:           s.cfg.Readonly,
		SessionPersistence: s.cfg.SessionPersistence,
		PersistenceMode:    string(s.cfg.PersistenceMode),
		FontSize:           s.cfg.FontSize,
		EnableGraphics:     s.cfg.EnableGraphics,
		DisableHyperlink:   s.cfg.DisableHyperlink,
		MiddleclickPaste:   s.cfg.MiddleclickPaste,
		ClipboardWrite:     s.cfg.ClipboardWrite,
		Bell:               s.cfg.Bell,
		TabShowPS1:         s.cfg.TabShowPS1,
		AutoRespawn:        s.cfg.AutoRespawn,
		CloseOnExit:        s.cfg.CloseOnExit,
		DOMRenderer:        s.cfg.DOMRenderer,
		AllowSharing:       s.cfg.AllowSharing && s.cfg.SessionPersistence,
		PingSeconds:        int(s.cfg.PingInterval / time.Second),
		Sessions:           s.sessionList(cl),
		V:                  s.assetVer,
		EphemeralID:        ephemeralID,
		Favicon:            s.faviconHref,
	})
}

// maxBodyBytes bounds JSON request bodies (titles cap at 64 runes and order
// lists are small); without it a huge POST body is a memory-DoS vector on
// endpoints that are unauthenticated in short_term mode.
const maxBodyBytes = 64 << 10

// shareState fills in SharedOut for owned sessions from the manager's live
// share links (a link exists even before anyone accepts). List() already set
// it from accepted sharers; this ORs in outstanding links.
func (s *Server) shareState(cl *session.Client, infos []session.SessionInfo) []session.SessionInfo {
	for i := range infos {
		if !infos[i].Shared && !infos[i].SharedOut {
			infos[i].SharedOut = s.mgr.HasActiveLinks(infos[i].ID)
		}
	}
	return infos
}

func (s *Server) sessionList(cl *session.Client) []session.SessionInfo {
	return s.shareState(cl, cl.List())
}

func (s *Server) oneInfo(cl *session.Client, id string) session.SessionInfo {
	out := s.shareState(cl, []session.SessionInfo{cl.SessionInfoFor(id)})
	return out[0]
}

// GET /sessions -> list; POST /sessions -> create.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.sessionList(cl))
	case http.MethodPost:
		if !s.cfg.MultiSession && cl.Count() >= 1 {
			http.Error(w, "multi-session disabled", http.StatusForbidden)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		args, env := s.spawnParams(r)
		sess, err := s.mgr.CreateSession(cl, capTitle(strings.TrimSpace(body.Title)), args, env)
		if err == session.ErrTooManySessions {
			http.Error(w, "maximum allowed session limit reached", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, sess.Info())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// PATCH /sessions/{id} {title} -> rename; DELETE -> close.
func (s *Server) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	id, action, hasAction := strings.Cut(rest, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	// PUT /sessions/order {order: [ids]} -> rearrange tabs. Session ids
	// are UUIDs, so "order" cannot collide with one.
	if id == "order" && !hasAction {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Order []string `json:"order"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "order required", http.StatusBadRequest)
			return
		}
		cl.SetOrder(body.Order)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// POST /sessions/accept {token} -> attach a session shared with us.
	// "accept" cannot collide with a UUID session id.
	if id == "accept" && !hasAction {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
			http.Error(w, "token required", http.StatusBadRequest)
			return
		}
		sess, err := s.mgr.AcceptShare(cl, strings.TrimSpace(body.Token))
		if err != nil {
			code := http.StatusBadRequest
			if err == session.ErrTooManySessions {
				code = http.StatusForbidden
			}
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, http.StatusOK, s.oneInfo(cl, sess.ID))
		return
	}
	sess, exists := cl.Get(id)
	if !exists {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if hasAction {
		// POST /sessions/{id}/restart: respawn an exited command
		// (close_on_exit=false keeps the session around after exit).
		if action == "restart" && r.Method == http.MethodPost {
			if _, err := s.mgr.RestartSession(cl, id); err != nil {
				code := http.StatusInternalServerError
				if err == session.ErrReadOnly {
					code = http.StatusForbidden
				}
				http.Error(w, err.Error(), code)
				return
			}
			writeJSON(w, http.StatusOK, sess.Info())
			return
		}
		// POST /sessions/{id}/share {readOnly, ttl} -> mint a share link.
		// DELETE /sessions/{id}/share -> revoke all shares of the session.
		if action == "share" {
			switch r.Method {
			case http.MethodPost:
				var body struct {
					ReadOnly  bool   `json:"readOnly"`
					TTL       string `json:"ttl"` // Go duration; "" = never expire
					SingleUse bool   `json:"singleUse"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				var ttl time.Duration
				if body.TTL != "" {
					d, err := time.ParseDuration(body.TTL)
					if err != nil {
						http.Error(w, "bad ttl", http.StatusBadRequest)
						return
					}
					ttl = d
				}
				token, err := s.mgr.Share(cl, id, body.ReadOnly, ttl, body.SingleUse)
				if err != nil {
					code := http.StatusForbidden
					if err == session.ErrNotFound {
						code = http.StatusNotFound
					}
					http.Error(w, err.Error(), code)
					return
				}
				writeJSON(w, http.StatusOK, map[string]string{"token": token})
				return
			case http.MethodDelete:
				if err := s.mgr.RevokeShare(cl, id); err != nil {
					http.Error(w, err.Error(), http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		// The tab title lives on the shared session object, so only the owner
		// may rename it — a sharer renaming would change the owner's title.
		if cl.IsShared(id) {
			http.Error(w, "only the owner can rename this tab", http.StatusForbidden)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
			http.Error(w, "title required", http.StatusBadRequest)
			return
		}
		sess.Rename(capTitle(strings.TrimSpace(body.Title)))
		writeJSON(w, http.StatusOK, sess.Info())
	case http.MethodDelete:
		_ = s.mgr.CloseSession(cl, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// GET /ws?session=ID -> upgrade and bridge.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	sid := r.URL.Query().Get("session")
	var sess *session.Session
	if sid != "" {
		sess, ok = cl.Get(sid)
		if !ok {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
	} else {
		// No session specified: use/create the default. The /ws query carries
		// protocol fields, not URL spawn params, so only the configured env
		// (with header placeholders expanded) applies here.
		var err error
		env := config.ExpandHeaderEnv(s.cfg.Env, r.Header.Get)
		sess, err = s.mgr.EnsureDefaultSession(cl, nil, env)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	s.serveWS(conn, cl, sess)
}

// capTitle bounds user-supplied tab titles to a sane length (rune-safe).
func capTitle(t string) string {
	const max = 64
	if r := []rune(t); len(r) > max {
		return string(r[:max])
	}
	return t
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
