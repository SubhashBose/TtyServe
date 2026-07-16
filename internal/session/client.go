package session

import (
	"strings"
	"sync"
	"time"

	"ttyserve/internal/terminal"

	"github.com/google/uuid"
)

// Session is a single terminal tab.
type Session struct {
	ID       string
	Title    string
	Created  time.Time
	terminal *terminal.Terminal

	mu sync.Mutex
	// userTitled is set once the user chooses a title (explicit title on
	// create, or a rename). It pins the title: auto cwd-tracking stops.
	userTitled bool
	// autoPs/autoDir are the components of an auto title ("<process> <dir>"),
	// kept separately so the frontend can style them differently.
	autoPs, autoDir string

	// extraArgs/extraEnv are per-session spawn parameters from the URL
	// (url_arg / url_env modes), kept so a restart respawns identically.
	// Immutable after creation.
	extraArgs, extraEnv []string

	// owner is the identity of the client that created the session. sharers
	// are clients that hold a shared-in reference (excludes the owner). Only
	// the owner may share/revoke; on owner-close all sharers are evicted.
	owner   string
	sharers map[string]*Client
}

// Term returns the underlying terminal.
func (s *Session) Term() *terminal.Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

// restartIfExited swaps in a fresh terminal if the current one's command has
// exited. Returns whether a restart happened. The check and swap are atomic
// so concurrent restart requests can't spawn two terminals.
func (s *Session) restartIfExited(spawn func() (*terminal.Terminal, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.terminal.Exited():
	default:
		return false, nil // still running
	}
	t, err := spawn()
	if err != nil {
		return false, err
	}
	s.terminal = t
	return true, nil
}

// Rename sets the tab title and pins it against auto-titling.
func (s *Session) Rename(title string) {
	s.mu.Lock()
	s.Title = title
	s.userTitled = true
	s.autoPs, s.autoDir = "", ""
	s.mu.Unlock()
}

// AutoTitle updates the title from its live components (foreground process
// name and cwd basename; either may be empty), unless the user has set a
// title themselves.
func (s *Session) AutoTitle(ps, dir string) {
	s.mu.Lock()
	if !s.userTitled && (ps != "" || dir != "") {
		s.autoPs, s.autoDir = ps, dir
		s.Title = strings.TrimSpace(ps + " " + dir)
	}
	s.mu.Unlock()
}

// Info returns the serializable view of the session.
func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionInfo{ID: s.ID, Title: s.Title, Ps: s.autoPs, Dir: s.autoDir}
}

// GetTitle returns the tab title safely.
func (s *Session) GetTitle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}

// Close terminates the session's terminal.
func (s *Session) Close() { s.Term().Close() }

// ownerID returns the identity of the creating client.
func (s *Session) ownerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// addSharerClient registers a client as a shared-in viewer.
func (s *Session) addSharerClient(c *Client) {
	s.mu.Lock()
	if s.sharers == nil {
		s.sharers = make(map[string]*Client)
	}
	s.sharers[c.ID] = c
	s.mu.Unlock()
}

// removeSharerClient unregisters a sharer (they closed their own tab).
func (s *Session) removeSharerClient(id string) {
	s.mu.Lock()
	delete(s.sharers, id)
	s.mu.Unlock()
}

// HasSharers reports whether the session currently has any shared-in viewers.
func (s *Session) HasSharers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sharers) > 0
}

// SharerCount returns how many distinct users (clients) it is shared with.
func (s *Session) SharerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sharers)
}

// takeSharers returns and clears the sharer set — used when the owner closes
// the session or revokes all shares.
func (s *Session) takeSharers() []*Client {
	s.mu.Lock()
	out := make([]*Client, 0, len(s.sharers))
	for _, c := range s.sharers {
		out = append(out, c)
	}
	s.sharers = nil
	s.mu.Unlock()
	return out
}

// Client owns a set of sessions belonging to one identity (a basic-auth user
// or a short-term cookie holder).
type Client struct {
	ID    string // identity key: username or cookie token
	mu    sync.Mutex
	sess  map[string]*Session
	order []string // session creation order, for stable tab display

	// sharedIn marks which of this client's sessions are shared-in references
	// (owned by another client); the value is that reference's read-only bit.
	// Absent for owned sessions.
	sharedIn map[string]bool

	// conns tracks live websockets by an id, each with the session it serves
	// and a closer, so a revoked share can kick the sharer's connection.
	conns      map[uint64]connSub
	nextConnID uint64

	// lastSeen is updated whenever a websocket attaches/detaches; used by the
	// reaper for short-term clients.
	lastSeen time.Time
	// activeConns counts currently attached websockets across all sessions.
	activeConns int
}

type connSub struct {
	session string
	closeFn func()
}

func newClient(id string) *Client {
	return &Client{
		ID:       id,
		sess:     make(map[string]*Session),
		sharedIn: make(map[string]bool),
		conns:    make(map[uint64]connSub),
		lastSeen: time.Now(),
	}
}

// SessionInfo is a serializable view of a session for the API.
type SessionInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Ps/Dir are the auto-title components (foreground process name, cwd
	// basename) so the frontend can style them differently. Empty for
	// user-titled tabs.
	Ps  string `json:"ps,omitempty"`
	Dir string `json:"dir,omitempty"`
	// Shared is true when this client accesses the session via a share (it is
	// not the owner); ReadOnly is true when that shared access is read-only.
	Shared   bool `json:"shared,omitempty"`
	ReadOnly bool `json:"readOnly,omitempty"`
	// SharedOut is true when this client OWNS the session and there is an
	// active share (a live link or joined viewers) — drives the "Stop
	// sharing" affordance. SharerCount is how many distinct users have
	// actually joined — drives the owner-side "someone joined" badge.
	SharedOut   bool `json:"sharedOut,omitempty"`
	SharerCount int  `json:"sharerCount,omitempty"`
}

// List returns session metadata in creation order.
func (c *Client) List() []SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SessionInfo, 0, len(c.order))
	for _, id := range c.order {
		if s, ok := c.sess[id]; ok {
			info := s.Info()
			if ro, shared := c.sharedIn[id]; shared {
				info.Shared = true
				info.ReadOnly = ro
			} else {
				info.SharerCount = s.SharerCount()
				info.SharedOut = info.SharerCount > 0
			}
			out = append(out, info)
		}
	}
	return out
}

// addShared adds (or updates) a shared-in reference to another owner's
// session. readOnly is the access level granted by the share.
func (c *Client) addShared(s *Session, readOnly bool) {
	c.mu.Lock()
	if _, exists := c.sess[s.ID]; !exists {
		c.sess[s.ID] = s
		c.order = append(c.order, s.ID)
	}
	c.sharedIn[s.ID] = readOnly
	c.mu.Unlock()
}

// AccessReadOnly reports whether this client's access to a session is
// read-only (a read-only share). Owned sessions are never read-only here.
func (c *Client) AccessReadOnly(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ro, shared := c.sharedIn[id]
	return shared && ro
}

// dropShared removes a shared-in reference without touching the terminal.
func (c *Client) dropShared(id string) {
	c.mu.Lock()
	delete(c.sharedIn, id)
	if _, ok := c.sess[id]; ok {
		delete(c.sess, id)
		for i, oid := range c.order {
			if oid == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.mu.Unlock()
}

// RegConn registers a live websocket (its session and a closer) and returns
// an id to unregister it. closeConns uses these to kick connections.
func (c *Client) RegConn(sessionID string, closeFn func()) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextConnID++
	id := c.nextConnID
	c.conns[id] = connSub{session: sessionID, closeFn: closeFn}
	return id
}

func (c *Client) UnregConn(id uint64) {
	c.mu.Lock()
	delete(c.conns, id)
	c.mu.Unlock()
}

// closeConns closes all live connections serving a session (revoke eviction).
func (c *Client) closeConns(sessionID string) {
	c.mu.Lock()
	var fns []func()
	for _, cs := range c.conns {
		if cs.session == sessionID {
			fns = append(fns, cs.closeFn)
		}
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// Sessions returns the sessions themselves in creation order.
func (c *Client) Sessions() []*Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Session, 0, len(c.order))
	for _, id := range c.order {
		if s, ok := c.sess[id]; ok {
			out = append(out, s)
		}
	}
	return out
}

// SetOrder rearranges the tab order. Unknown ids are dropped; sessions not
// mentioned keep their relative order at the end.
func (c *Client) SetOrder(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := make(map[string]bool, len(ids))
	order := make([]string, 0, len(c.order))
	for _, id := range ids {
		if _, ok := c.sess[id]; ok && !seen[id] {
			order = append(order, id)
			seen[id] = true
		}
	}
	for _, id := range c.order {
		if !seen[id] {
			order = append(order, id)
		}
	}
	c.order = order
}

// SessionInfoFor returns a single session's info as this client sees it
// (including the per-client Shared/ReadOnly flags). Zero value if absent.
func (c *Client) SessionInfoFor(id string) SessionInfo {
	c.mu.Lock()
	s, ok := c.sess[id]
	ro, shared := c.sharedIn[id]
	c.mu.Unlock()
	if !ok {
		return SessionInfo{}
	}
	info := s.Info()
	if shared {
		info.Shared = true
		info.ReadOnly = ro
	} else {
		info.SharerCount = s.SharerCount()
		info.SharedOut = info.SharerCount > 0
	}
	return info
}

// IsShared reports whether the client's access to a session is a shared-in
// reference (it is not the owner).
func (c *Client) IsShared(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, shared := c.sharedIn[id]
	return shared
}

// Get returns a session by id.
func (c *Client) Get(id string) (*Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sess[id]
	return s, ok
}

// Count returns the number of sessions.
func (c *Client) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sess)
}

func (c *Client) add(s *Session) {
	c.mu.Lock()
	c.sess[s.ID] = s
	c.order = append(c.order, s.ID)
	c.mu.Unlock()
}

func (c *Client) remove(id string) {
	c.mu.Lock()
	s, ok := c.sess[id]
	_, isShared := c.sharedIn[id]
	if ok {
		delete(c.sess, id)
		delete(c.sharedIn, id)
		for i, oid := range c.order {
			if oid == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if isShared {
		// A sharer leaving: just unregister; the terminal belongs to the
		// owner and stays alive for them and other viewers.
		s.removeSharerClient(c.ID)
		return
	}
	// The owner removing: evict every sharer's durable reference, then kill
	// the terminal (which ends any sharer's live connections via the exit
	// signal). dropShared touches only each sharer's own map — no two client
	// locks are ever held at once.
	for _, sh := range s.takeSharers() {
		sh.dropShared(id)
	}
	s.Close()
}

func (c *Client) touch() {
	c.mu.Lock()
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *Client) connAdd() {
	c.mu.Lock()
	c.activeConns++
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *Client) connRemove() {
	c.mu.Lock()
	if c.activeConns > 0 {
		c.activeConns--
	}
	c.lastSeen = time.Now()
	c.mu.Unlock()
}

func (c *Client) idleSince() (time.Time, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen, c.activeConns
}

func newSessionID() string { return uuid.NewString() }
