package terminal

import (
	"testing"
	"time"
)

// TestSlowSubscriberDoesNotBlockOthers reproduces the pre-fix deadlock: one
// subscriber that never drains, while the PTY floods output. Previously the
// read loop would block on the full channel while holding the terminal mutex,
// wedging every other subscriber and Close(). Now the slow one is dropped and
// the healthy one keeps receiving.
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	term, err := New(Options{
		Command:         "/bin/sh",
		Args:            []string{"-c", "yes 0123456789abcdef | head -c 5000000; sleep 60"},
		ScrollbackBytes: 4096,
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer term.Close()

	// Slow subscriber: never drains.
	_, _, unsubSlow, ok := term.Subscribe()
	if !ok {
		t.Fatal("subscribe slow")
	}
	defer unsubSlow()

	// Healthy subscriber drains continuously and counts bytes.
	_, subFast, unsubFast, ok := term.Subscribe()
	if !ok {
		t.Fatal("subscribe fast")
	}
	defer unsubFast()

	got := 0
	deadline := time.After(5 * time.Second)
	for got < 4_000_000 {
		select {
		case <-subFast.Notify():
			data, open := subFast.Take()
			got += len(data)
			if !open && got < 4_000_000 {
				t.Fatalf("fast subscriber closed early after %d bytes", got)
			}
		case <-deadline:
			t.Fatalf("DEADLOCK: healthy subscriber starved; received only %d bytes", got)
		}
	}

	// Close must not hang either.
	done := make(chan struct{})
	go func() { term.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close() hung")
	}
}
