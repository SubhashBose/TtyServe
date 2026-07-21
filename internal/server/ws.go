package server

import (
	"encoding/json"
	"time"

	"ttyserve/internal/session"

	"github.com/gorilla/websocket"
)

// Client->server messages are a 1-byte opcode followed by a payload.
const (
	msgInput  = '0' // payload: raw bytes typed by user
	msgResize = '1' // payload: JSON {"cols":N,"rows":N}
	msgPing   = '2' // payload: ignored (app-level keepalive)
)

// Server->client frames are a 1-byte opcode followed by a payload.
const (
	srvOutput = 'o' // payload: raw terminal output
	srvExit   = 'e' // session's stream ended (shell exit / session closed)
	srvReplay = 'r' // payload: scrollback repaint — client must NOT reply to
	// capability queries in it (they were already answered live; replying
	// again injects the responses as phantom input at the shell prompt)
	srvPong = 'p' // reply to a client '2' ping; lets the client detect a
	// dead network (protocol-level ping/pong is invisible to browser JS)
)

type resizePayload struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// serveWS attaches a websocket to a session: replays scrollback, then streams.
// closeWS sends a WebSocket close frame, then closes the connection. The
// frame travels in-band (proxies forward it like any data frame), so the
// browser fires onclose immediately instead of waiting to detect the raw TCP
// teardown — which lags through proxies. Harmless if the conn is already
// closing.
func closeWS(conn *websocket.Conn) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(2*time.Second))
	_ = conn.Close()
}

func (s *Server) serveWS(conn *websocket.Conn, cl *session.Client, sess *session.Session) {
	term := sess.Term()

	snapshot, sub, unsub, ok := term.Subscribe()
	if !ok {
		// Terminal already gone; tell the client so it stops retrying.
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{srvExit})
		closeWS(conn)
		return
	}
	// Ephemeral mode: the session dies with its socket (no reaper runs). Once
	// this subscriber is gone and none remain, discard it so its shell isn't
	// leaked. Registered before unsub so it runs after it (LIFO), seeing the
	// post-unsubscribe count.
	if !s.cfg.SessionPersistence {
		defer func() {
			if term.SubscriberCount() == 0 {
				_ = s.mgr.CloseSession(cl, sess.ID)
			}
		}()
	}
	defer unsub()

	s.mgr.ConnAttached(cl)
	defer s.mgr.ConnDetached(cl)

	// Register this connection so a revoked share can force it closed.
	connID := cl.RegConn(sess.ID, func() { closeWS(conn) })
	defer cl.UnregConn(connID)

	// sharedReadOnly: this viewer joined via a read-only share, so an owner
	// (or read-write viewer) holds authority over the terminal. readOnly:
	// whether this viewer may send input at all — also true under the global
	// readonly flag, which has no writer of its own.
	sharedReadOnly := cl.AccessReadOnly(sess.ID)
	readOnly := s.cfg.Readonly || sharedReadOnly

	// Enforce per-session viewer cap.
	if s.cfg.MaxClientsPerSession > 0 && term.SubscriberCount() > s.cfg.MaxClientsPerSession {
		closeWS(conn)
		return
	}

	// Dead-peer detection: require a pong (or any message) within pongWait.
	// 3× the ping interval forgives two lost ping cycles and doubles as the
	// window in which a connection that survives a network outage can
	// resume seamlessly (the client marks tabs "stalled" client-side well
	// before this deadline, without closing them).
	pongWait := 3 * s.cfg.PingInterval
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	conn.SetReadLimit(1 << 20)

	send := func(messageType int, data []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(messageType, data)
	}
	// sendFrame writes opcode + payload without copying the payload into a
	// prefixed buffer — output chunks can be large (up to the 1 MiB
	// coalescing cap), so the copy is worth avoiding.
	sendFrame := func(op byte, payload []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		w, err := conn.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte{op}); err != nil {
			_ = w.Close()
			return err
		}
		if len(payload) > 0 {
			if _, err := w.Write(payload); err != nil {
				_ = w.Close()
				return err
			}
		}
		return w.Close()
	}

	// Replay scrollback so a reconnecting client repaints its screen. This
	// happens before the writer goroutine starts, so there is only ever one
	// concurrent writer on the connection.
	if len(snapshot) > 0 {
		if err := sendFrame(srvReplay, snapshot); err != nil {
			closeWS(conn)
			return
		}
	}

	done := make(chan struct{})
	quit := make(chan struct{})    // closed by the reader when the conn drops
	pong := make(chan struct{}, 1) // reader requests an app-level pong reply

	// Writer: coalesced terminal output + keepalive pings -> websocket.
	// Sole writer to conn from here on. Returns promptly on quit so cleanup
	// (idle accounting, ephemeral discard) isn't delayed until the next ping.
	go func() {
		defer close(done)
		defer closeWS(conn) // in-band close frame -> prompt onclose
		ping := time.NewTicker(s.cfg.PingInterval)
		defer ping.Stop()
		for {
			select {
			case <-quit:
				return
			case <-pong:
				if err := sendFrame(srvPong, nil); err != nil {
					return
				}
			case <-sub.Notify():
				chunk, open, resync := sub.Take()
				if resync {
					// The consumer fell too far behind: repaint it from the
					// current scrollback instead of streaming stale bytes. Sent
					// as a replay frame so the client resets and repaints and
					// its query replies stay gated. The session stays live and
					// full-speed — no pause, no reconnect, no exit signal.
					if err := sendFrame(srvReplay, chunk); err != nil {
						return
					}
				} else if len(chunk) > 0 {
					if err := sendFrame(srvOutput, chunk); err != nil {
						return
					}
				}
				if !open {
					// The subscriber only closes on a genuine session end (an
					// overflow resyncs instead), so this is always a real exit.
					_ = sendFrame(srvExit, nil)
					return
				}
			case <-ping.C:
				if err := send(websocket.PingMessage, nil); err != nil {
					return
				}
				s.mgr.Touch(cl)
			}
		}
	}()

	// Reader: websocket -> terminal input / control.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		s.mgr.Touch(cl)
		if len(data) == 0 {
			continue
		}
		op := data[0]
		payload := data[1:]
		switch op {
		case msgInput:
			if !readOnly {
				_, _ = term.Write(payload)
			}
		case msgResize:
			// A read-only SHARE viewer must not reshape the terminal — that
			// would change the PTY width for the owner and other viewers.
			// The global readonly flag has no owner/writer, so there a
			// viewer's own size drives the PTY as normal.
			if sharedReadOnly {
				break
			}
			var rp resizePayload
			if err := json.Unmarshal(payload, &rp); err == nil && rp.Cols > 0 && rp.Rows > 0 {
				_ = term.Resize(rp.Rows, rp.Cols)
			}
		case msgPing:
			// App-level keepalive: answer via the writer goroutine (sole
			// writer). Coalescing to one pending pong is fine.
			select {
			case pong <- struct{}{}:
			default:
			}
		default:
			// ignore unknown opcodes
		}
	}

	close(quit) // wake the writer immediately instead of on the next ping
	conn.Close()
	<-done
}
