package session

import (
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

	// extraArgs/extraEnv are per-session spawn parameters from the URL
	// (url_arg / url_env modes), kept so a restart respawns identically.
	// Immutable after creation.
	extraArgs, extraEnv []string
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
	s.mu.Unlock()
}

// AutoTitle updates the title to follow the shell's cwd, unless the user has
// set a title themselves.
func (s *Session) AutoTitle(title string) {
	s.mu.Lock()
	if !s.userTitled && title != "" {
		s.Title = title
	}
	s.mu.Unlock()
}

// GetTitle returns the tab title safely.
func (s *Session) GetTitle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}

// Close terminates the session's terminal.
func (s *Session) Close() { s.Term().Close() }

// Client owns a set of sessions belonging to one identity (a basic-auth user
// or a short-term cookie holder).
type Client struct {
	ID    string // identity key: username or cookie token
	mu    sync.Mutex
	sess  map[string]*Session
	order []string // session creation order, for stable tab display

	// lastSeen is updated whenever a websocket attaches/detaches; used by the
	// reaper for short-term clients.
	lastSeen time.Time
	// activeConns counts currently attached websockets across all sessions.
	activeConns int
}

func newClient(id string) *Client {
	return &Client{
		ID:       id,
		sess:     make(map[string]*Session),
		lastSeen: time.Now(),
	}
}

// SessionInfo is a serializable view of a session for the API.
type SessionInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// List returns session metadata in creation order.
func (c *Client) List() []SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]SessionInfo, 0, len(c.order))
	for _, id := range c.order {
		if s, ok := c.sess[id]; ok {
			out = append(out, SessionInfo{ID: s.ID, Title: s.GetTitle()})
		}
	}
	return out
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
	if s, ok := c.sess[id]; ok {
		delete(c.sess, id)
		for i, oid := range c.order {
			if oid == id {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		s.Close()
		return
	}
	c.mu.Unlock()
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
