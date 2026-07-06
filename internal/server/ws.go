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
)

type resizePayload struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// serveWS attaches a websocket to a session: replays scrollback, then streams.
func (s *Server) serveWS(conn *websocket.Conn, cl *session.Client, sess *session.Session) {
	term := sess.Term()

	snapshot, sub, unsub, ok := term.Subscribe()
	if !ok {
		// Terminal already gone; tell the client so it stops retrying.
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{srvExit})
		conn.Close()
		return
	}
	defer unsub()

	s.mgr.ConnAttached(cl)
	defer s.mgr.ConnDetached(cl)

	// Enforce per-session viewer cap.
	if s.cfg.MaxClientsPerSession > 0 && term.SubscriberCount() > s.cfg.MaxClientsPerSession {
		conn.Close()
		return
	}

	// Dead-peer detection: require a pong (or any message) within pongWait.
	pongWait := 2 * s.cfg.PingInterval
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	conn.SetReadLimit(1 << 20)

	send := func(messageType int, data []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(messageType, data)
	}

	// Replay scrollback so a reconnecting client repaints its screen. This
	// happens before the writer goroutine starts, so there is only ever one
	// concurrent writer on the connection.
	if len(snapshot) > 0 {
		if err := send(websocket.BinaryMessage, append([]byte{srvOutput}, snapshot...)); err != nil {
			conn.Close()
			return
		}
	}

	done := make(chan struct{})

	// Writer: coalesced terminal output + keepalive pings -> websocket.
	// Sole writer to conn from here on. Closing conn on exit unblocks the
	// reader loop below.
	go func() {
		defer close(done)
		defer conn.Close()
		ping := time.NewTicker(s.cfg.PingInterval)
		defer ping.Stop()
		for {
			select {
			case <-sub.Notify():
				chunk, open := sub.Take()
				if len(chunk) > 0 {
					if err := send(websocket.BinaryMessage, append([]byte{srvOutput}, chunk...)); err != nil {
						return
					}
				}
				if !open {
					_ = send(websocket.BinaryMessage, []byte{srvExit})
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
			if s.cfg.WriteEnabled {
				_, _ = term.Write(payload)
			}
		case msgResize:
			var rp resizePayload
			if err := json.Unmarshal(payload, &rp); err == nil && rp.Cols > 0 && rp.Rows > 0 {
				_ = term.Resize(rp.Rows, rp.Cols)
			}
		case msgPing:
			// app-level keepalive, no-op
		default:
			// ignore unknown opcodes
		}
	}

	conn.Close()
	<-done
}
