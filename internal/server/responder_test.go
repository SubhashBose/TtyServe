package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readRole waits for this connection's role frame and reports whether it is the
// designated query responder.
func readRole(t *testing.T, c *websocket.Conn) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(deadline)
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read role: %v", err)
		}
		if len(data) >= 2 && data[0] == srvRole {
			return data[1] == '1'
		}
	}
	t.Fatal("no role frame received")
	return false
}

// Exactly one attached viewer may answer terminal capability queries. If they
// all answer, the program consumes one reply and every surplus reply lands at
// the shell prompt as phantom input — text nobody typed, plus a bell. This is
// the regression guard for that bug.
func TestExactlyOneQueryResponderPerSession(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)

	a := dialWS(t, ts, "alice", sid)
	if !readRole(t, a) {
		t.Fatal("the first viewer must be the responder")
	}
	b := dialWS(t, ts, "alice", sid) // same user, second browser
	if readRole(t, b) {
		t.Fatal("SECURITY/CORRECTNESS: a second viewer must NOT also answer queries")
	}
	c := dialWS(t, ts, "alice", sid) // and a third
	if readRole(t, c) {
		t.Fatal("a third viewer must not answer queries either")
	}
}

// If the responder leaves, a survivor must be promoted — otherwise nobody
// answers and programs that block waiting for a reply would hang.
func TestResponderPromotedWhenItLeaves(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)

	a := dialWS(t, ts, "alice", sid)
	if !readRole(t, a) {
		t.Fatal("first viewer should be the responder")
	}
	b := dialWS(t, ts, "alice", sid)
	if readRole(t, b) {
		t.Fatal("second viewer should start silent")
	}

	a.Close() // the responder goes away

	// b must be promoted: a second role frame arrives saying "you answer now".
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = b.SetReadDeadline(deadline)
		_, data, err := b.ReadMessage()
		if err != nil {
			t.Fatalf("read after promotion: %v", err)
		}
		if len(data) >= 2 && data[0] == srvRole && data[1] == '1' {
			return // promoted
		}
	}
	t.Fatal("surviving viewer was never promoted — queries would go unanswered")
}

// A lone viewer must always be the responder, both on first connect and after
// reconnecting, or a single user's terminal would stop answering queries.
func TestSoloViewerIsAlwaysResponder(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)

	a := dialWS(t, ts, "alice", sid)
	if !readRole(t, a) {
		t.Fatal("a lone viewer must be the responder")
	}
	a.Close()
	time.Sleep(200 * time.Millisecond)

	b := dialWS(t, ts, "alice", sid)
	if !readRole(t, b) {
		t.Fatal("after the only viewer reconnects it must be the responder again")
	}
}
