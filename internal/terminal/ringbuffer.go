package terminal

// ringBuffer is a fixed-capacity byte buffer that keeps the most recent bytes
// written to it. Used to replay recent terminal output to reconnecting clients.
type ringBuffer struct {
	data  []byte
	size  int
	start int
	full  bool
	cap   int
}

func newRingBuffer(capacity int) *ringBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &ringBuffer{
		data: make([]byte, capacity),
		cap:  capacity,
	}
}

func (r *ringBuffer) Write(p []byte) {
	if r.cap == 0 {
		return
	}
	// If the incoming chunk is larger than capacity, keep only its tail.
	if len(p) >= r.cap {
		copy(r.data, p[len(p)-r.cap:])
		r.start = 0
		r.size = r.cap
		r.full = true
		return
	}
	for _, b := range p {
		idx := (r.start + r.size) % r.cap
		r.data[idx] = b
		if r.size < r.cap {
			r.size++
		} else {
			r.start = (r.start + 1) % r.cap
		}
	}
	if r.size == r.cap {
		r.full = true
	}
}

// Snapshot returns a copy of the buffered bytes in write order.
func (r *ringBuffer) Snapshot() []byte {
	if r.size == 0 {
		return nil
	}
	out := make([]byte, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.data[(r.start+i)%r.cap]
	}
	return out
}
