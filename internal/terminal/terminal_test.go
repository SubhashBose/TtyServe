package terminal

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingBasicAndWrap(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("abc"))
	if got := string(r.Snapshot()); got != "abc" {
		t.Fatalf("got %q", got)
	}
	r.Write([]byte("defghij")) // total 10 bytes into cap 8 -> keep last 8
	if got := string(r.Snapshot()); got != "cdefghij" {
		t.Fatalf("wrap: got %q", got)
	}
	// Oversized single write keeps only its tail.
	r.Write([]byte("0123456789ABCDEF"))
	if got := string(r.Snapshot()); got != "89ABCDEF" {
		t.Fatalf("oversize: got %q", got)
	}
}

func TestRingZeroCap(t *testing.T) {
	r := newRingBuffer(0)
	r.Write([]byte("data"))
	if len(r.Snapshot()) != 0 {
		t.Fatal("expected empty snapshot for zero-cap ring")
	}
}

func TestSubscriberCoalesceAndClose(t *testing.T) {
	s := newSubscriber()
	if !s.push([]byte("hello ")) || !s.push([]byte("world")) {
		t.Fatal("push failed unexpectedly")
	}
	<-s.Notify()
	data, open := s.Take()
	if !open || string(data) != "hello world" {
		t.Fatalf("got %q open=%v", data, open)
	}
	// Close with pending data: one Take drains it and reports closed.
	s.push([]byte("tail"))
	s.close()
	<-s.Notify()
	data, open = s.Take()
	if open || string(data) != "tail" {
		t.Fatalf("after close: got %q open=%v", data, open)
	}
	// push after close is a no-op that must not report overflow.
	if !s.push([]byte("ignored")) {
		t.Fatal("push after close should not signal overflow")
	}
}

func TestSubscriberOverflowReportsDrop(t *testing.T) {
	s := newSubscriber()
	big := []byte(strings.Repeat("x", maxSubscriberBuffer))
	if !s.push(big) {
		t.Fatal("filling to cap should succeed")
	}
	if s.push([]byte("y")) {
		t.Fatal("exceeding cap must report overflow so caller drops the subscriber")
	}
	// The consumer never blocks the producer: producer already returned.
	data, open := s.Take()
	if !open || !bytes.Equal(data, big) {
		t.Fatal("buffered data should be intact")
	}
}
