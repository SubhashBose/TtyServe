package terminal

import (
	"strings"
	"testing"
)

// The scrollback ring keeps the last N *bytes* and knows nothing about escape
// sequences, so once it wraps a snapshot can begin in the middle of one. This
// matters for OSC 0 (set window title), which most shells emit on every prompt
// as ESC ] 0 ; <title> BEL: if the cut discards the leading "ESC ]", the
// trailing BEL is no longer a string terminator but a ground-state BEL — an
// audible bell for something that never happened.
//
// This is why the client suppresses the bell while a replay parses (see the
// term.onBell handler in app.js). If this test ever fails because snapshots
// became escape-boundary aligned, that suppression could be revisited.
func TestRingCanTruncateOSCLeavingBareBEL(t *testing.T) {
	const prompt = "\x1b]0;user@host: ~/some/dir\x07user@host:~/some/dir$ echo hello\r\nhello\r\n"
	r := newRingBuffer(200) // small so it wraps, as a real ring eventually does
	for i := 0; i < 20; i++ {
		r.Write([]byte(prompt))
	}
	snap := string(r.Snapshot())

	// Count BELs that a parser would treat as ground-state (audible) because
	// their OSC introducer fell outside the snapshot.
	bare := 0
	for i := 0; i < len(snap); i++ {
		if snap[i] != 0x07 {
			continue
		}
		esc := strings.LastIndex(snap[:i], "\x1b")
		if esc < 0 || !strings.HasPrefix(snap[esc:], "\x1b]") {
			bare++
		}
	}
	if bare == 0 {
		t.Skip("this snapshot happened to land on a clean boundary; nothing to document")
	}
	t.Logf("snapshot begins mid-sequence: %q", snap[:40])
	t.Logf("ground-state (audible) BELs a replay would produce: %d", bare)
}
