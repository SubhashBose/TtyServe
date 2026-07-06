package server

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/session"

	"github.com/gorilla/websocket"
)

//go:embed web
var webFS embed.FS

// Server ties config, auth, and the session manager to HTTP handlers.
type Server struct {
	cfg      config.Config
	auth     *auth.Authenticator
	mgr      *session.Manager
	upgrader websocket.Upgrader
	tmpl     *template.Template
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
	return &Server{
		cfg:  cfg,
		auth: a,
		mgr:  mgr,
		tmpl: tmpl,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Same-origin by default; extra origins (or "*") can be allowed
			// via the allow_origins config option. This matters because the
			// websocket is authenticated by cookie in short_term mode.
			CheckOrigin: func(r *http.Request) bool { return originAllowed(cfg, r) },
		},
	}, nil
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
	staticFS, _ := fs.Sub(webFS, "web/static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/sessions", s.handleSessions)     // GET list, POST create
	mux.HandleFunc("/api/sessions/", s.handleSessionItem) // PATCH rename, DELETE close
	mux.HandleFunc("/ws", s.handleWS)
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
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	if id.SetCookie != nil {
		http.SetCookie(w, id.SetCookie)
	}
	return s.mgr.Client(id.Key), true
}

type pageData struct {
	Title              string
	MultiSession       bool
	TabBarPosition     string
	WriteEnabled       bool
	SessionPersistence bool
	PersistenceMode    string
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
	// Guarantee at least one session exists on first load.
	if _, err := s.mgr.EnsureDefaultSession(cl); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.Execute(w, pageData{
		Title:              s.cfg.Title,
		MultiSession:       s.cfg.MultiSession,
		TabBarPosition:     s.cfg.TabBarPosition,
		WriteEnabled:       s.cfg.WriteEnabled,
		SessionPersistence: s.cfg.SessionPersistence,
		PersistenceMode:    string(s.cfg.PersistenceMode),
	})
}

// GET /api/sessions -> list; POST /api/sessions -> create.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, cl.List())
	case http.MethodPost:
		if !s.cfg.MultiSession && cl.Count() >= 1 {
			http.Error(w, "multi-session disabled", http.StatusForbidden)
			return
		}
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sess, err := s.mgr.CreateSession(cl, capTitle(strings.TrimSpace(body.Title)))
		if err == session.ErrTooManySessions {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, session.SessionInfo{ID: sess.ID, Title: sess.GetTitle()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// PATCH /api/sessions/{id} {title} -> rename; DELETE -> close.
func (s *Server) handleSessionItem(w http.ResponseWriter, r *http.Request) {
	cl, ok := s.resolve(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	sess, exists := cl.Get(id)
	if !exists {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Title) == "" {
			http.Error(w, "title required", http.StatusBadRequest)
			return
		}
		sess.Rename(capTitle(strings.TrimSpace(body.Title)))
		writeJSON(w, http.StatusOK, session.SessionInfo{ID: sess.ID, Title: sess.GetTitle()})
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
		// No session specified: use/create the default.
		var err error
		sess, err = s.mgr.EnsureDefaultSession(cl)
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
