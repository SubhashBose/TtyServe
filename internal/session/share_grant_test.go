package session

import (
	"sync"
	"testing"
	"time"

	"ttyserve/internal/config"
)

func grantTestManager(t *testing.T) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.Command = "/bin/bash"
	cfg.AllowSharing = true
	cfg.SessionPersistence = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	m := NewManager(cfg)
	t.Cleanup(m.Shutdown)
	return m
}

func grantCount(m *Manager) int {
	m.sharesMu.Lock()
	defer m.sharesMu.Unlock()
	return len(m.shares)
}

// forceSweep runs the prune with the rate-limit cleared, standing in for "some
// time has passed" so tests don't have to sleep grantSweepInterval.
func forceSweep(m *Manager) {
	m.sharesMu.Lock()
	m.lastGrantSweep = time.Time{}
	m.pruneSharesLocked()
	m.sharesMu.Unlock()
}

// Grants for a session that no longer exists must be reclaimed. Session
// teardown happens in Client.remove(), which cannot reach the share map, so
// without the sweep in pruneSharesLocked every closed session would leave its
// non-expiring grants resident for the life of the process.
func TestGrantsFreedWhenSessionCloses(t *testing.T) {
	m := grantTestManager(t)
	c := m.Client("alice")
	s, err := m.CreateSession(c, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Distinct (single-use) grants so reuse doesn't collapse them.
	for i := 0; i < 4; i++ {
		if _, err := m.Share(c, s.ID, false, 0, true, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := grantCount(m); n != 4 {
		t.Fatalf("expected 4 grants, got %d", n)
	}
	if err := m.CloseSession(c, s.ID); err != nil {
		t.Fatal(err)
	}
	forceSweep(m)
	if n := grantCount(m); n != 0 {
		t.Fatalf("grants for a closed session were not reclaimed: %d remain", n)
	}
}

// The per-session cap must hold, evicting the oldest link rather than growing
// without bound. Uses single-use grants: those are never reused, so each call
// genuinely adds an entry.
func TestGrantCapEvictsOldest(t *testing.T) {
	m := grantTestManager(t)
	c := m.Client("alice")
	s, err := m.CreateSession(c, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var first, last string
	for i := 0; i < maxGrantsPerSession+10; i++ {
		tok, err := m.Share(c, s.ID, false, 0, true, false)
		if err != nil {
			t.Fatalf("share %d: %v", i, err)
		}
		if i == 0 {
			first = tok
		}
		last = tok
	}
	if n := grantCount(m); n != maxGrantsPerSession {
		t.Fatalf("cap not enforced: %d grants (want %d)", n, maxGrantsPerSession)
	}
	// The oldest was evicted; the newest survives.
	if _, err := m.AcceptShare(m.Client("bob"), first); err == nil {
		t.Fatal("the oldest grant should have been evicted at the cap")
	}
	if _, err := m.AcceptShare(m.Client("carol"), last); err != nil {
		t.Fatalf("the newest grant must still be valid: %v", err)
	}
}

// An interchangeable request (permanent, multi-use, same access) reuses the
// existing token instead of minting a duplicate.
func TestGrantReuseOnlyWhenInterchangeable(t *testing.T) {
	m := grantTestManager(t)
	c := m.Client("alice")
	s, err := m.CreateSession(c, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	a, _ := m.Share(c, s.ID, false, 0, false, false)
	b, _ := m.Share(c, s.ID, false, 0, false, false)
	if a != b {
		t.Fatal("a permanent multi-use re-share should reuse the same token")
	}
	if n := grantCount(m); n != 1 {
		t.Fatalf("reuse should not add a grant, have %d", n)
	}

	// A different access level is NOT interchangeable.
	ro, _ := m.Share(c, s.ID, true, 0, false, false)
	if ro == a {
		t.Fatal("read-only and read-write links must be distinct")
	}

	// Single-use must always be fresh: reusing one would let the first
	// recipient spend the second recipient's link.
	s1, _ := m.Share(c, s.ID, false, 0, true, false)
	s2, _ := m.Share(c, s.ID, false, 0, true, false)
	if s1 == s2 {
		t.Fatal("single-use links must never be reused")
	}

	// An expiring link must be fresh too, or the requester would silently
	// inherit an earlier expiry.
	e1, _ := m.Share(c, s.ID, false, time.Hour, false, false)
	e2, _ := m.Share(c, s.ID, false, time.Hour, false, false)
	if e1 == e2 {
		t.Fatal("expiring links must not be reused")
	}
	if e1 == a || e2 == a {
		t.Fatal("an expiring link must not reuse the permanent one")
	}
}

// Revoking clears every grant, so re-sharing afterwards issues a genuinely new
// token — the old link must stay dead rather than being resurrected by reuse.
func TestReshareAfterRevokeIssuesNewToken(t *testing.T) {
	m := grantTestManager(t)
	c := m.Client("alice")
	s, err := m.CreateSession(c, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := m.Share(c, s.ID, false, 0, false, false)
	if err := m.RevokeShare(c, s.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := m.Share(c, s.ID, false, 0, false, false)
	if fresh == old {
		t.Fatal("re-share after revoke must not reissue the revoked token")
	}
	if _, err := m.AcceptShare(m.Client("bob"), old); err == nil {
		t.Fatal("the revoked token must stay invalid")
	}
}

// The dead-session sweep made pruneSharesLocked descend into m.mu and client
// locks while holding sharesMu. That is only safe because sharesMu is the
// outermost lock and nothing acquires it while holding the inner ones — this
// hammers all the share paths concurrently so a future inversion shows up as a
// timeout here rather than a wedged server.
func TestShareConcurrencyNoDeadlock(t *testing.T) {
	m := grantTestManager(t)
	owner := m.Client("alice")
	var sessions []*Session
	for i := 0; i < 3; i++ {
		s, err := m.CreateSession(owner, "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			viewer := m.Client([]string{"bob", "carol", "dave"}[n%3])
			for {
				select {
				case <-done:
					return
				default:
				}
				s := sessions[n%len(sessions)]
				tok, err := m.Share(owner, s.ID, n%2 == 0, 0, n%3 == 0, false)
				if err == nil {
					_, _ = m.AcceptShare(viewer, tok)
				}
				_ = m.HasActiveLinks(s.ID)
				if n%4 == 0 {
					_ = m.RevokeShare(owner, s.ID)
				}
				m.sharesMu.Lock()
				m.lastGrantSweep = time.Time{} // force the nested-lock sweep
				m.sharesMu.Unlock()
			}
		}(i)
	}

	time.Sleep(1500 * time.Millisecond)
	close(done)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: share operations did not finish (lock-order inversion?)")
	}
}

// Guests are the one identity whose keyspace is unbounded in EVERY mode: each
// anonymous visitor (and each crawler following a public link) mints one. So
// they must be reaped even in `user` mode, which otherwise runs no reaper —
// while real users there keep their sessions, as that mode promises.
func TestGuestsReapedButUsersAreNot(t *testing.T) {
	cfg := config.Default()
	cfg.Command = "/bin/bash"
	cfg.AllowSharing = true
	cfg.SessionPersistence = true
	cfg.PersistenceMode = config.PersistByUser // no general reaper in this mode
	cfg.Users = []config.User{{Name: "alice", Password: "pw"}}
	cfg.PublicShareLinks = config.PublicShareAll
	cfg.IdleTimeout = time.Millisecond
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	m := NewManager(cfg)
	defer m.Shutdown()

	if m.reapAll {
		t.Fatal("user mode must not reap every client")
	}
	owner := m.Client("user:alice")
	if _, err := m.CreateSession(owner, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	guest := m.Client(GuestIDPrefix + "abc123")
	if !guest.IsGuest() || owner.IsGuest() {
		t.Fatal("IsGuest must distinguish guests from real users")
	}

	time.Sleep(5 * time.Millisecond) // both are now idle past IdleTimeout
	m.reapOnce()

	if _, ok := m.GetClient(GuestIDPrefix + "abc123"); ok {
		t.Fatal("an idle guest must be reaped even in user mode")
	}
	if _, ok := m.GetClient("user:alice"); !ok {
		t.Fatal("SECURITY/UX: a real user must NOT be reaped in user mode — their sessions must survive")
	}
	if owner.Count() != 1 {
		t.Fatalf("the user's session must survive, have %d", owner.Count())
	}
}
