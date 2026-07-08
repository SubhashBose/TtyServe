package terminal

// ringBuffer is a bounded byte buffer that keeps the most recent bytes
// written to it. Used to replay recent terminal output to reconnecting
// clients. Storage is allocated lazily and grows geometrically up to cap, so
// quiet sessions don't pay the full scrollback cost up front.
//
// Invariant: while len(data) < cap the buffer has never wrapped (start == 0
// and the contents are linear in data[:size]); wrapping only happens once
// data has reached its full capacity.
type ringBuffer struct {
	data  []byte
	size  int
	start int
	cap   int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &ringBuffer{cap: capacity}
}

// grow enlarges the backing array to hold at least need bytes (clamped to
// cap), preserving contents. Only called in the linear phase (start == 0).
func (r *ringBuffer) grow(need int) {
	newSize := len(r.data) * 2
	if newSize < 4096 {
		newSize = 4096
	}
	for newSize < need {
		newSize *= 2
	}
	if newSize > r.cap {
		newSize = r.cap
	}
	nd := make([]byte, newSize)
	copy(nd, r.data[:r.size])
	r.data = nd
}

func (r *ringBuffer) Write(p []byte) {
	if r.cap == 0 || len(p) == 0 {
		return
	}
	// Chunk larger than capacity: keep only its tail.
	if len(p) >= r.cap {
		if len(r.data) < r.cap {
			r.grow(r.cap)
		}
		copy(r.data, p[len(p)-r.cap:])
		r.start, r.size = 0, r.cap
		return
	}
	if need := r.size + len(p); need > len(r.data) {
		if need > r.cap {
			r.grow(r.cap) // about to wrap: commit to full capacity
		} else {
			r.grow(need)
		}
	}
	n := len(r.data)
	pos := (r.start + r.size) % n
	c := copy(r.data[pos:], p)
	if c < len(p) {
		copy(r.data, p[c:])
	}
	if r.size+len(p) <= n {
		r.size += len(p)
	} else {
		r.start = (r.start + r.size + len(p) - n) % n
		r.size = n
	}
}

// Snapshot returns a copy of the buffered bytes in write order.
func (r *ringBuffer) Snapshot() []byte {
	if r.size == 0 {
		return nil
	}
	out := make([]byte, r.size)
	end := r.start + r.size
	if end <= len(r.data) {
		copy(out, r.data[r.start:end])
	} else {
		c := copy(out, r.data[r.start:])
		copy(out[c:], r.data[:r.size-c])
	}
	return out
}
