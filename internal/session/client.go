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
}

// Term returns the underlying terminal.
func (s *Session) Term() *terminal.Terminal { return s.terminal }

// Rename sets the tab title.
func (s *Session) Rename(title string) {
	s.mu.Lock()
	s.Title = title
	s.mu.Unlock()
}

// GetTitle returns the tab title safely.
func (s *Session) GetTitle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Title
}

// Close terminates the session's terminal.
func (s *Session) Close() { s.terminal.Close() }

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
