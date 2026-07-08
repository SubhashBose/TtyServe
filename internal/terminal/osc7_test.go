package terminal

import (
	"strings"
	"testing"
)

// feedAll runs a scanner over the input split into the given chunk sizes and
// returns the last path found.
func feedAll(t *testing.T, input string, chunkSize int) (string, bool) {
	t.Helper()
	var s osc7Scanner
	var path string
	var found bool
	data := []byte(input)
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if p, ok := s.feed(data[i:end]); ok {
			path, found = p, ok
		}
	}
	return path, found
}

func TestOSC7Basic(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"bel terminator", "\x1b]7;file://host/home/user\x07", "/home/user", true},
		{"st terminator", "\x1b]7;file://host/home/user\x1b\\", "/home/user", true},
		{"empty hostname", "\x1b]7;file:///srv/data\x07", "/srv/data", true},
		{"bare path (lenient)", "\x1b]7;/opt/work\x07", "/opt/work", true},
		{"percent decoding", "\x1b]7;file://h/dir%20with%20space\x07", "/dir with space", true},
		{"embedded in output", "ls output\x1b[31mred\x1b[0m\x1b]7;file://h/x\x07more", "/x", true},
		{"last one wins", "\x1b]7;file://h/first\x07\x1b]7;file://h/second\x07", "/second", true},
		{"relative path rejected", "\x1b]7;relative/path\x07", "", false},
		{"empty payload rejected", "\x1b]7;\x07", "", false},
		{"file:// with no path rejected", "\x1b]7;file://host\x07", "", false},
		{"other osc ignored", "\x1b]0;window title\x07", "", false},
		{"csi ignored", "\x1b[7;1H", "", false},
		{"esc esc prefix", "\x1b\x1b]7;file://h/y\x07", "/y", true},
		{"malformed st then valid", "\x1b]7;/a\x1bZ\x1b]7;/b\x07", "/b", true},
		{"no sequences", "plain text output with no escapes", "", false},
	}
	for _, c := range cases {
		got, ok := feedAll(t, c.input, len(c.input)+1)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestOSC7ChunkSplits verifies a sequence is parsed identically no matter
// where chunk boundaries fall.
func TestOSC7ChunkSplits(t *testing.T) {
	input := "before\x1b]7;file://host/home/u%C3%A9ser\x1b\\after\x1b]7;/second\x07tail"
	for size := 1; size <= len(input); size++ {
		got, ok := feedAll(t, input, size)
		if !ok || got != "/second" {
			t.Fatalf("chunk size %d: got (%q, %v), want (/second, true)", size, got, ok)
		}
	}
}

// TestOSC7UnterminatedBounded verifies an unterminated sequence is dropped at
// the payload cap without growing memory, and scanning recovers.
func TestOSC7UnterminatedBounded(t *testing.T) {
	var s osc7Scanner
	s.feed([]byte("\x1b]7;"))
	for i := 0; i < 100; i++ { // 100 x 1KiB of never-terminated payload
		if _, ok := s.feed([]byte(strings.Repeat("x", 1024))); ok {
			t.Fatal("unterminated sequence must not produce a path")
		}
	}
	if len(s.buf) > osc7MaxPayload {
		t.Fatalf("payload buffer grew to %d, cap is %d", len(s.buf), osc7MaxPayload)
	}
	// Scanner must have resynced: a following valid sequence parses.
	if p, ok := s.feed([]byte("\x1b]7;/recovered\x07")); !ok || p != "/recovered" {
		t.Fatalf("scanner did not recover after oversized sequence: (%q, %v)", p, ok)
	}
}

// TestOSC7ControlCharsRejected: a decoded path smuggling control characters
// must not become a title.
func TestOSC7ControlCharsRejected(t *testing.T) {
	for _, in := range []string{
		"\x1b]7;file://h/evil%0apath\x07",  // newline via percent-encoding
		"\x1b]7;file://h/evil%1b[2Jx\x07",  // escape char via percent-encoding
	} {
		if p, ok := feedAll(t, in, len(in)+1); ok {
			t.Errorf("control-char path accepted: %q", p)
		}
	}
}
