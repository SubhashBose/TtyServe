package terminal

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestRingBufferMatchesReference drives the ring with random chunk sizes and
// checks Snapshot against a trivially-correct model (append everything, keep
// the last cap bytes).
func TestRingBufferMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, capacity := range []int{0, 1, 7, 256, 4096, 100000} {
		r := newRingBuffer(capacity)
		var model []byte
		var written byte
		for i := 0; i < 200; i++ {
			n := rng.Intn(3 * (capacity + 10))
			chunk := make([]byte, n)
			for j := range chunk {
				chunk[j] = written
				written++
			}
			r.Write(chunk)
			model = append(model, chunk...)
			if capacity > 0 && len(model) > capacity {
				model = model[len(model)-capacity:]
			} else if capacity == 0 {
				model = nil
			}
			got := r.Snapshot()
			if capacity == 0 {
				if got != nil {
					t.Fatalf("cap=0: Snapshot = %d bytes, want nil", len(got))
				}
				continue
			}
			if !bytes.Equal(got, model) {
				t.Fatalf("cap=%d step=%d: snapshot mismatch (got %d bytes, want %d)",
					capacity, i, len(got), len(model))
			}
		}
	}
}

// TestRingBufferLazyAllocation checks that quiet buffers don't preallocate
// their full capacity.
func TestRingBufferLazyAllocation(t *testing.T) {
	r := newRingBuffer(256 * 1024)
	if len(r.data) != 0 {
		t.Fatalf("fresh ring allocated %d bytes", len(r.data))
	}
	r.Write([]byte("hello"))
	if len(r.data) >= 256*1024 {
		t.Fatalf("small write allocated full capacity (%d bytes)", len(r.data))
	}
}
