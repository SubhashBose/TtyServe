package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ttyserve/internal/auth"
	"ttyserve/internal/config"
	"ttyserve/internal/session"

	"github.com/gorilla/websocket"
)

// newTestServer starts an in-process server with two basic-auth users and
// sharing enabled.
func newTestServer(t *testing.T) (*httptest.Server, *session.Manager) {
	t.Helper()
	cfg := config.Default()
	cfg.Command = "/bin/bash"
	cfg.SessionPersistence = true
	cfg.PersistenceMode = config.PersistByUser
	cfg.AllowSharing = true
	cfg.MultiSession = true
	cfg.Users = []config.User{{Name: "alice", Password: "apass"}, {Name: "bob", Password: "bpass"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	a, err := auth.New(cfg)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	mgr := session.NewManager(cfg)
	srv, err := New(cfg, a, mgr)
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); mgr.Shutdown() })
	return ts, mgr
}

func do(t *testing.T, ts *httptest.Server, user, method, path, body string) (int, string) {
	t.Helper()
	var rdr *strings.Reader = strings.NewReader(body)
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, map[string]string{"alice": "apass", "bob": "bpass"}[user])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// dialWS opens an authenticated websocket for a session.
func dialWS(t *testing.T, ts *httptest.Server, user, sessionID string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?session=" + sessionID
	h := http.Header{}
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.SetBasicAuth(user, map[string]string{"alice": "apass", "bob": "bpass"}[user])
	h.Set("Authorization", req.Header.Get("Authorization"))
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s as %s: %v (status %d)", sessionID, user, err, code)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// collector drains a websocket in the background, accumulating terminal
// output. gorilla marks a connection failed on ANY read error (timeouts
// included) and panics on the next read, so polling with read deadlines is not
// an option — a dedicated reader goroutine is.
type collector struct {
	mu sync.Mutex
	sb strings.Builder
}

func collect(c *websocket.Conn) *collector {
	col := &collector{}
	go func() {
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if len(data) > 1 && (data[0] == srvOutput || data[0] == srvReplay) {
				col.mu.Lock()
				col.sb.Write(data[1:])
				col.mu.Unlock()
			}
		}
	}()
	return col
}

func (col *collector) text() string {
	col.mu.Lock()
	defer col.mu.Unlock()
	return col.sb.String()
}

// waitFor reports whether s appeared in the output within d.
func (col *collector) waitFor(s string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(col.text(), s) {
			return true
		}
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

func aliceSession(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	do(t, ts, "alice", "GET", "/", "") // index creates the first session
	_, body := do(t, ts, "alice", "GET", "/sessions", "")
	var list []session.SessionInfo
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) == 0 {
		t.Fatalf("no session for alice: %v %s", err, body)
	}
	return list[0].ID
}

func shareToken(t *testing.T, ts *httptest.Server, sid string, readOnly bool) string {
	t.Helper()
	payload := `{"readOnly":false}`
	if readOnly {
		payload = `{"readOnly":true}`
	}
	code, body := do(t, ts, "alice", "POST", "/sessions/"+sid+"/share", payload)
	if code != 200 {
		t.Fatalf("share: %d %s", code, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Token == "" {
		t.Fatalf("bad share response: %s", body)
	}
	return out.Token
}

// A read-only shared viewer must not be able to inject input, even with a
// hand-crafted websocket frame that bypasses the UI's local input blocking.
// This is the security boundary for view-only sharing: enforcement must be
// server-side.
func TestReadOnlyShareCannotInjectInput(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)
	tok := shareToken(t, ts, sid, true)
	if code, body := do(t, ts, "bob", "POST", "/sessions/accept", `{"token":"`+tok+`"}`); code != 200 {
		t.Fatalf("accept: %d %s", code, body)
	}

	alice := dialWS(t, ts, "alice", sid)
	bob := dialWS(t, ts, "bob", sid)
	aliceOut := collect(alice)
	collect(bob)
	time.Sleep(600 * time.Millisecond) // let the shell settle

	// Bob (read-only) sends raw input frames directly.
	const marker = "BOB_INJECTED_MARKER"
	if err := bob.WriteMessage(websocket.BinaryMessage,
		append([]byte{msgInput}, []byte("echo "+marker+"\n")...)); err != nil {
		t.Fatalf("bob write: %v", err)
	}
	if aliceOut.waitFor(marker, 1500*time.Millisecond) {
		t.Fatalf("SECURITY: read-only sharer's input reached the PTY; owner saw %q", aliceOut.text())
	}

	// Sanity: alice's own input still works, proving the terminal is live and
	// the test isn't passing because nothing works at all.
	const ok = "ALICE_OK_MARKER"
	if err := alice.WriteMessage(websocket.BinaryMessage,
		append([]byte{msgInput}, []byte("echo "+ok+"\n")...)); err != nil {
		t.Fatalf("alice write: %v", err)
	}
	if !aliceOut.waitFor(ok, 3*time.Second) {
		t.Fatalf("owner input did not reach the PTY; got %q", aliceOut.text())
	}
}

// A read-only viewer must not resize the PTY either: that would reshape the
// terminal for the owner and every other viewer.
func TestReadOnlyShareCannotResize(t *testing.T) {
	ts, mgr := newTestServer(t)
	sid := aliceSession(t, ts)
	tok := shareToken(t, ts, sid, true)
	if code, _ := do(t, ts, "bob", "POST", "/sessions/accept", `{"token":"`+tok+`"}`); code != 200 {
		t.Fatal("accept failed")
	}
	alice := dialWS(t, ts, "alice", sid)
	aliceOut := collect(alice)
	time.Sleep(400 * time.Millisecond)
	// Owner sets a known size.
	_ = alice.WriteMessage(websocket.BinaryMessage, append([]byte{msgResize}, []byte(`{"cols":100,"rows":30}`)...))
	time.Sleep(300 * time.Millisecond)

	bob := dialWS(t, ts, "bob", sid)
	collect(bob)
	_ = bob.WriteMessage(websocket.BinaryMessage, append([]byte{msgResize}, []byte(`{"cols":20,"rows":5}`)...))
	time.Sleep(500 * time.Millisecond)

	// Ask the shell for its width; it must still be the owner's 100.
	_ = alice.WriteMessage(websocket.BinaryMessage, append([]byte{msgInput}, []byte("echo W=$(tput cols)\n")...))
	if !aliceOut.waitFor("W=100", 3*time.Second) {
		t.Fatalf("SECURITY: read-only sharer changed the owner's PTY width; owner saw %q", aliceOut.text())
	}
	_ = mgr
}

// A read-WRITE shared viewer is expected to be able to type — the counterpart
// check, so the read-only tests above can't pass trivially.
func TestReadWriteShareCanType(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)
	tok := shareToken(t, ts, sid, false)
	if code, _ := do(t, ts, "bob", "POST", "/sessions/accept", `{"token":"`+tok+`"}`); code != 200 {
		t.Fatal("accept failed")
	}
	alice := dialWS(t, ts, "alice", sid)
	bob := dialWS(t, ts, "bob", sid)
	aliceOut := collect(alice)
	collect(bob)
	time.Sleep(600 * time.Millisecond)

	const marker = "BOB_RW_MARKER"
	_ = bob.WriteMessage(websocket.BinaryMessage, append([]byte{msgInput}, []byte("echo "+marker+"\n")...))
	if !aliceOut.waitFor(marker, 3*time.Second) {
		t.Fatalf("read-write sharer's input did not reach the PTY; got %q", aliceOut.text())
	}
}

// A user who was never granted a share must not be able to open a websocket to
// someone else's session by guessing/knowing its id.
func TestNonSharerCannotAttachWebsocket(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?session=" + sid
	h := http.Header{}
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.SetBasicAuth("bob", "bpass")
	h.Set("Authorization", req.Header.Get("Authorization"))
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err == nil {
		c.Close()
		t.Fatal("SECURITY: non-sharer attached to another user's session websocket")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("expected 404 for foreign session, got %d", code)
	}
}

// After the owner revokes, a previously-accepted viewer must lose access: the
// durable reference is gone, so a fresh websocket is refused.
func TestRevokeBlocksReconnect(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := aliceSession(t, ts)
	tok := shareToken(t, ts, sid, false)
	if code, _ := do(t, ts, "bob", "POST", "/sessions/accept", `{"token":"`+tok+`"}`); code != 200 {
		t.Fatal("accept failed")
	}
	bob := dialWS(t, ts, "bob", sid) // works before revoke
	_ = bob

	if code, body := do(t, ts, "alice", "DELETE", "/sessions/"+sid+"/share", ""); code != 204 {
		t.Fatalf("revoke: %d %s", code, body)
	}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?session=" + sid
	h := http.Header{}
	req, _ := http.NewRequest("GET", ts.URL, nil)
	req.SetBasicAuth("bob", "bpass")
	h.Set("Authorization", req.Header.Get("Authorization"))
	c, resp, err := websocket.DefaultDialer.Dial(u, h)
	if err == nil {
		c.Close()
		t.Fatal("SECURITY: revoked viewer reconnected to the session")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after revoke, got %v", resp)
	}
}
