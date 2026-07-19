package session

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ttyserve/internal/config"
	"ttyserve/internal/terminal"
)

var (
	ErrTooManySessions = errors.New("session limit reached")
	ErrNotFound        = errors.New("session not found")
	ErrSharingDisabled = errors.New("session sharing is disabled")
	ErrNotOwner        = errors.New("only the session owner can do that")
	ErrShareInvalid    = errors.New("share link is invalid or has expired")
	ErrReadOnly        = errors.New("read-only access")
)

// shareGrant is a live share invitation: a token that lets another
// authenticated client attach to sessionID owned by owner.
type shareGrant struct {
	owner     string
	sessionID string
	readOnly  bool
	expires   time.Time // zero = never
	singleUse bool      // consumed on first successful accept
}

// Manager owns all clients and their sessions.
type Manager struct {
	cfg      config.Config
	defTitle string // initial tab title for new sessions
	// defPs/defDir are its auto-title components — the configured command's
	// basename and the starting directory's basename — so a new tab shows
	// the expected "<process> <dir>" immediately instead of waiting for the
	// first updater tick.
	defPs, defDir string

	mu      sync.Mutex
	clients map[string]*Client

	sharesMu sync.Mutex
	shares   map[string]*shareGrant // token -> grant

	stop     chan struct{}
	stopOnce sync.Once
}

// NewManager creates a manager and starts the reaper if needed.
func NewManager(cfg config.Config) *Manager {
	var defPs, defDir string
	if cfg.TabShowPsname {
		defPs = filepath.Base(cfg.Command)
	}
	if cfg.TabShowCwd {
		defDir = defaultTitle(cfg.WorkingDir)
	}
	defTitle := strings.TrimSpace(defPs + " " + defDir)
	switch {
	case cfg.TabTitle != "":
		defTitle = cfg.TabTitle
		defPs, defDir = "", ""
	case defTitle == "":
		defTitle = "terminal"
	}
	m := &Manager{
		cfg:      cfg,
		defTitle: defTitle,
		defPs:    defPs,
		defDir:   defDir,
		clients:  make(map[string]*Client),
		shares:   make(map[string]*shareGrant),
		stop:     make(chan struct{}),
	}
	// The reaper collects idle clients in short_term mode — and in ephemeral
	// mode, where every page load mints a fresh identity: sessions die with
	// their socket there, but the client entries would otherwise accumulate
	// in m.clients forever (every page hit, incl. bots, creates one).
	if !cfg.SessionPersistence ||
		(cfg.SessionPersistence && cfg.PersistenceMode == config.PersistShortTerm) {
		go m.reaper()
	}
	// Auto titles only matter when tabs are visible, sessions outlive a
	// page load, and neither a fixed tab-title nor PS1 titling (which is
	// client-side) is configured; otherwise skip the updater (and, via
	// TrackCwd, the per-chunk OSC 7 scanning) entirely.
	if m.autoTitleEnabled() {
		go m.titleUpdater()
	}
	return m
}

// autoTitleEnabled reports whether the server-side psname/cwd title updater
// should run under the current config.
func (m *Manager) autoTitleEnabled() bool {
	return m.cfg.SessionPersistence && m.cfg.MultiSession &&
		m.cfg.TabTitle == "" && !m.cfg.TabShowPS1 &&
		(m.cfg.TabShowPsname || m.cfg.TabShowCwd)
}

// Client returns (creating if needed) the client for an identity.
func (m *Manager) Client(id string) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[id]
	if !ok {
		c = newClient(id)
		m.clients[id] = c
	}
	return c
}

// GetClient returns an existing client without creating one.
func (m *Manager) GetClient(id string) (*Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.clients[id]
	return c, ok
}

// CreateSession spawns a new terminal/tab for a client. extraArgs/extraEnv
// are per-session additions from the URL (url_arg / url_env modes).
func (m *Manager) CreateSession(c *Client, title string, extraArgs, extraEnv []string) (*Session, error) {
	if m.cfg.MaxSessionsPerClient > 0 && c.Count() >= m.cfg.MaxSessionsPerClient {
		return nil, ErrTooManySessions
	}
	term, err := m.spawnTerminal(extraArgs, extraEnv)
	if err != nil {
		return nil, err
	}
	// An explicit title (from the API, or a configured fixed tab-title) pins
	// the tab; empty means auto (default now, then live psname/cwd).
	userTitled := title != "" || m.cfg.TabTitle != ""
	s := &Session{
		ID:         newSessionID(),
		Title:      title,
		Created:    time.Now(),
		terminal:   term,
		userTitled: userTitled,
		extraArgs:  extraArgs,
		extraEnv:   extraEnv,
		owner:      c.ID,
	}
	if title == "" {
		// Auto-titled: start with the expected components right away.
		s.Title = m.defTitle
		s.autoPs, s.autoDir = m.defPs, m.defDir
	}
	c.add(s)

	// If configured to remove sessions when the shell exits, watch for it.
	if m.cfg.CloseOnExit {
		go func() {
			<-term.Exited()
			c.remove(s.ID)
		}()
	}
	return s, nil
}

// spawnTerminal starts a terminal. extraArgs are appended to the configured
// args; env is the fully-resolved environment for the session (the server
// builds it from the configured env with ${header.*} placeholders expanded,
// plus any URL env), stored per-session so restarts reproduce it exactly.
func (m *Manager) spawnTerminal(extraArgs, env []string) (*terminal.Terminal, error) {
	args := append(append([]string{}, m.cfg.Args...), extraArgs...)
	return terminal.New(terminal.Options{
		Command:         m.cfg.Command,
		Args:            args,
		Env:             append([]string(nil), env...),
		WorkingDir:      m.cfg.WorkingDir,
		ScrollbackBytes: m.cfg.ScrollbackBytes,
		TrackCwd:        m.autoTitleEnabled() && m.cfg.TabShowCwd,
	})
}

// RestartSession spawns a fresh terminal for a session whose command has
// exited (the close_on_exit=false case). No-op if it is still running.
func (m *Manager) RestartSession(c *Client, id string) (*Session, error) {
	s, ok := c.Get(id)
	if !ok {
		return nil, ErrNotFound
	}
	// A read-only shared viewer must not cause side effects (respawn).
	if c.AccessReadOnly(id) {
		return nil, ErrReadOnly
	}
	restarted, err := s.restartIfExited(func() (*terminal.Terminal, error) {
		return m.spawnTerminal(s.extraArgs, s.extraEnv)
	})
	if err != nil {
		return nil, err
	}
	if restarted && m.cfg.CloseOnExit {
		term := s.Term()
		go func() {
			<-term.Exited()
			c.remove(s.ID)
		}()
	}
	return s, nil
}

// CloseSession removes and terminates a session.
func (m *Manager) CloseSession(c *Client, id string) error {
	if _, ok := c.Get(id); !ok {
		return ErrNotFound
	}
	c.remove(id)
	return nil
}

// sharingEnabled reports whether tab sharing is usable: opted in and with
// persistent sessions (an ephemeral session dies with its socket).
func (m *Manager) sharingEnabled() bool {
	return m.cfg.AllowSharing && m.cfg.SessionPersistence
}

// Share mints a share token for a session the client owns. ttl <= 0 means
// the invitation never expires; singleUse consumes it on first accept.
// Returns the opaque token; the caller builds the user-facing link from it.
func (m *Manager) Share(c *Client, sessionID string, readOnly bool, ttl time.Duration, singleUse bool) (string, error) {
	if !m.sharingEnabled() {
		return "", ErrSharingDisabled
	}
	s, ok := c.Get(sessionID)
	if !ok {
		return "", ErrNotFound
	}
	// Only the owner may share — a shared-in viewer cannot re-share.
	if s.ownerID() != c.ID {
		return "", ErrNotOwner
	}
	token := shareToken()
	g := &shareGrant{owner: c.ID, sessionID: sessionID, readOnly: readOnly, singleUse: singleUse}
	if ttl > 0 {
		g.expires = time.Now().Add(ttl)
	}
	m.sharesMu.Lock()
	m.pruneSharesLocked()
	m.shares[token] = g
	m.sharesMu.Unlock()
	return token, nil
}

// AcceptShare grafts a shared session into the accepting client's tab list.
// The caller must already have authenticated the client.
func (m *Manager) AcceptShare(c *Client, token string) (*Session, error) {
	if !m.sharingEnabled() {
		return nil, ErrShareInvalid
	}
	// Hold sharesMu across the ENTIRE accept — validating the grant and
	// registering this client as a sharer must be atomic w.r.t. RevokeShare.
	// Otherwise a revoke can delete the grant and evict the current sharers
	// in the window between the token check and addSharerClient, leaving the
	// accepter with access the owner just revoked (RevokeShare keeps the
	// session, so the owner/session lookups don't catch it). sharesMu is the
	// outermost lock — nothing acquires it while holding m.mu / a client /
	// a session lock — so descending into those here is deadlock-free.
	m.sharesMu.Lock()
	defer m.sharesMu.Unlock()

	g, ok := m.shares[token]
	if ok && !g.expires.IsZero() && time.Now().After(g.expires) {
		delete(m.shares, token)
		ok = false
	}
	if !ok {
		return nil, ErrShareInvalid
	}
	owner, ok := m.GetClient(g.owner)
	if !ok {
		return nil, ErrShareInvalid
	}
	s, ok := owner.Get(g.sessionID)
	if !ok {
		return nil, ErrShareInvalid // owner closed it since
	}
	// Accepting your own share is a harmless no-op.
	if c.ID == g.owner {
		return s, nil
	}
	// Enforce the accepter's tab cap — unless they already hold this share
	// (re-accept after reload just refreshes the read-only bit).
	if _, already := c.Get(g.sessionID); !already {
		if m.cfg.MaxSessionsPerClient > 0 && c.Count() >= m.cfg.MaxSessionsPerClient {
			return nil, ErrTooManySessions
		}
	}
	c.addShared(s, g.readOnly)
	s.addSharerClient(c)
	// A single-use link is spent on first successful accept. The granted
	// access lives on as a reference in the accepter's client.
	if g.singleUse {
		delete(m.shares, token)
	}
	return s, nil
}

// RevokeShare invalidates all share links for a session the client owns and
// evicts every current sharer (durable reference removed, live connection
// closed). The terminal itself keeps running for the owner.
func (m *Manager) RevokeShare(c *Client, sessionID string) error {
	s, ok := c.Get(sessionID)
	if !ok {
		return ErrNotFound
	}
	if s.ownerID() != c.ID {
		return ErrNotOwner
	}
	m.sharesMu.Lock()
	for tok, g := range m.shares {
		if g.sessionID == sessionID {
			delete(m.shares, tok)
		}
	}
	m.sharesMu.Unlock()
	for _, sh := range s.takeSharers() {
		sh.closeConns(sessionID)
		sh.dropShared(sessionID)
	}
	return nil
}

// HasActiveLinks reports whether a session currently has any unexpired share
// link (accepted or not). Prunes expired grants as it scans, so the result
// is always exact. Touches only sharesMu — safe to call while holding no
// other lock.
func (m *Manager) HasActiveLinks(sessionID string) bool {
	m.sharesMu.Lock()
	defer m.sharesMu.Unlock()
	now := time.Now()
	found := false
	for tok, g := range m.shares {
		if !g.expires.IsZero() && now.After(g.expires) {
			delete(m.shares, tok)
			continue
		}
		if g.sessionID == sessionID {
			found = true
		}
	}
	return found
}

// pruneSharesLocked drops expired grants. Caller holds sharesMu.
func (m *Manager) pruneSharesLocked() {
	now := time.Now()
	for tok, g := range m.shares {
		if !g.expires.IsZero() && now.After(g.expires) {
			delete(m.shares, tok)
		}
	}
}

func shareToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// EnsureDefaultSession guarantees a client has at least one session and returns
// the first one. Useful for single-session mode and first connect.
func (m *Manager) EnsureDefaultSession(c *Client, extraArgs, extraEnv []string) (*Session, error) {
	list := c.List()
	if len(list) > 0 {
		s, _ := c.Get(list[0].ID)
		return s, nil
	}
	return m.CreateSession(c, "", extraArgs, extraEnv)
}

// titleUpdater keeps auto-titled sessions named "<process> <dir>" — the
// foreground process and the shell's current directory, per the tab-show-*
// options. Sessions the user has titled are never touched.
func (m *Manager) titleUpdater() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.mu.Lock()
			clients := make([]*Client, 0, len(m.clients))
			for _, c := range m.clients {
				clients = append(clients, c)
			}
			m.mu.Unlock()
			for _, c := range clients {
				// Titles only matter to attached viewers; skip the /proc
				// reads for clients nobody is watching.
				if _, active := c.idleSince(); active == 0 {
					continue
				}
				for _, s := range c.Sessions() {
					term := s.Term()
					// Directory and foreground changes always come with
					// output (command echo, prompt redraw), so terminals
					// with none since the last tick can't have changed.
					// The check is cwd-based when enabled; with cwd off
					// there's no activity source, so always refresh.
					if m.cfg.TabShowCwd && !term.TakeActivity() {
						continue
					}
					var ps, dir string
					if m.cfg.TabShowPsname {
						ps = term.ForegroundName()
					}
					if m.cfg.TabShowCwd {
						if cwd := term.Cwd(); cwd != "" {
							dir = filepath.Base(cwd)
						}
					}
					s.AutoTitle(ps, dir)
				}
			}
		}
	}
}

// defaultTitle derives the default tab title from the directory terminals
// start in: the configured working_dir, or the server's cwd when unset.
func defaultTitle(workingDir string) string {
	dir := workingDir
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if base := filepath.Base(dir); base != "" && base != "." {
		return base
	}
	return "terminal"
}

// ConnAttached / ConnDetached track live websockets for idle accounting.
func (m *Manager) ConnAttached(c *Client) { c.connAdd() }
func (m *Manager) ConnDetached(c *Client) { c.connRemove() }
func (m *Manager) Touch(c *Client)        { c.touch() }

// reaper removes short-term clients idle beyond the timeout with no active
// connections, killing all their sessions.
func (m *Manager) reaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

func (m *Manager) reapOnce() {
	cutoff := time.Now().Add(-m.cfg.IdleTimeout)
	var toKill []*Client
	m.mu.Lock()
	for id, c := range m.clients {
		last, active := c.idleSince()
		if active == 0 && last.Before(cutoff) {
			toKill = append(toKill, c)
			delete(m.clients, id)
		}
	}
	m.mu.Unlock()
	for _, c := range toKill {
		for _, info := range c.List() {
			c.remove(info.ID)
		}
	}
}

// Shutdown stops the reaper and closes every session. Safe to call repeatedly.
func (m *Manager) Shutdown() {
	m.stopOnce.Do(func() {
		close(m.stop)
		m.mu.Lock()
		clients := m.clients
		m.clients = make(map[string]*Client)
		m.mu.Unlock()
		for _, c := range clients {
			for _, info := range c.List() {
				c.remove(info.ID)
			}
		}
	})
}
