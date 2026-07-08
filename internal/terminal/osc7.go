package terminal

import (
	"bytes"
	"net/url"
	"strings"
)

// osc7MaxPayload caps how many payload bytes a single OSC 7 sequence may
// carry. A stream that opens a sequence and never terminates it (malicious
// or corrupt) is discarded at this bound and scanning resyncs; memory use is
// fixed regardless of input.
const osc7MaxPayload = 4096

// osc7Scanner incrementally extracts OSC 7 ("current working directory")
// sequences — ESC ] 7 ; file://host/path BEL (or ESC \) — from a terminal
// byte stream. It is owned by the read loop and fed each chunk; sequences
// split across chunk boundaries are handled. Malformed input never fails the
// stream: the scanner simply resyncs at the next ESC.
type osc7Scanner struct {
	state int    // 0 idle, 1 ESC, 2 ESC], 3 ESC]7, 4 payload, 5 payload+ESC
	buf   []byte // payload being collected (bounded by osc7MaxPayload)
}

// feed scans chunk and returns the path of the last complete, valid OSC 7
// sequence in it, if any.
func (s *osc7Scanner) feed(chunk []byte) (path string, found bool) {
	i := 0
	for i < len(chunk) {
		b := chunk[i]
		switch s.state {
		case 0: // hunting for ESC; skip ahead in one memchr
			j := bytes.IndexByte(chunk[i:], 0x1b)
			if j < 0 {
				return path, found
			}
			i += j + 1
			s.state = 1
		case 1: // after ESC
			switch b {
			case ']':
				s.state = 2
			case 0x1b: // ESC ESC: still "after ESC"
			default:
				s.state = 0
			}
			i++
		case 2: // after ESC ]
			switch b {
			case '7':
				s.state = 3
			case 0x1b:
				s.state = 1
			default:
				s.state = 0
			}
			i++
		case 3: // after ESC ] 7
			switch b {
			case ';':
				s.state = 4
				s.buf = s.buf[:0]
			case 0x1b:
				s.state = 1
			default:
				s.state = 0
			}
			i++
		case 4: // collecting payload
			switch {
			case b == 0x07: // BEL terminator
				if p, ok := parseOSC7Path(s.buf); ok {
					path, found = p, true
				}
				s.state = 0
				i++
			case b == 0x1b: // possible ESC \ terminator
				s.state = 5
				i++
			case len(s.buf) >= osc7MaxPayload: // unterminated: give up, resync
				s.state = 0
			default:
				s.buf = append(s.buf, b)
				i++
			}
		case 5: // payload then ESC: expect '\' (ST)
			if b == '\\' {
				if p, ok := parseOSC7Path(s.buf); ok {
					path, found = p, true
				}
				s.state = 0
				i++
			} else {
				// Malformed terminator. The ESC we consumed may start a new
				// sequence: reprocess this byte in the "after ESC" state.
				s.state = 1
			}
		}
	}
	return path, found
}

// parseOSC7Path validates and extracts the directory from an OSC 7 payload.
// Accepts the spec form "file://host/path" (percent-decoded) and, leniently,
// a bare absolute path. Anything else — including paths smuggling control
// characters — is rejected.
func parseOSC7Path(payload []byte) (string, bool) {
	sp := string(payload)
	var p string
	switch {
	case strings.HasPrefix(sp, "file://"):
		u, err := url.Parse(sp)
		if err != nil {
			return "", false
		}
		p = u.Path
	case strings.HasPrefix(sp, "/"):
		p = sp
	default:
		return "", false
	}
	if p == "" {
		return "", false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return p, true
}
