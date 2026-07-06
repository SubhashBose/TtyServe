package terminal

import (
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/creack/pty"
)

// maxSubscriberBuffer caps how many bytes may queue for a single slow
// subscriber before it is forcibly dropped. The PTY read loop never blocks on
// a consumer; a dropped client simply reconnects and is repainted from the
// scrollback ring, so no correctness is lost.
const maxSubscriberBuffer = 1 << 20 // 1 MiB

// Subscriber is one attached output consumer. Output chunks are coalesced
// into an internal buffer and the consumer is signalled via Notify; this
// also batches many small PTY reads into fewer websocket frames.
type Subscriber struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	notify chan struct{}
}

func newSubscriber() *Subscriber {
	return &Subscriber{notify: make(chan struct{}, 1)}
}

// Notify signals whenever output is pending or the subscriber has closed.
func (s *Subscriber) Notify() <-chan struct{} { return s.notify }

// Take returns all pending output and whether the subscriber is still open.
// A final call may return both remaining data and open == false.
func (s *Subscriber) Take() (data []byte, open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data = s.buf
	s.buf = nil
	return data, !s.closed
}

// push appends output without ever blocking; reports false on overflow,
// meaning the subscriber is too slow and should be dropped.
func (s *Subscriber) push(p []byte) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return true
	}
	if len(s.buf)+len(p) > maxSubscriberBuffer {
		s.mu.Unlock()
		return false
	}
	s.buf = append(s.buf, p...)
	s.mu.Unlock()
	s.signal()
	return true
}

func (s *Subscriber) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.signal()
}

func (s *Subscriber) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Terminal wraps a single PTY + child process. Output is fanned out to any
// number of subscribers and retained in a scrollback ring buffer so a
// reconnecting client can repaint its screen.
type Terminal struct {
	ptmx *os.File
	cmd  *exec.Cmd

	mu          sync.Mutex
	subscribers map[int]*Subscriber
	nextSubID   int
	ring        *ringBuffer
	closed      bool
	exited      chan struct{}
	exitErr     error
}

// Options configures a new terminal.
type Options struct {
	Command         string
	Args            []string
	Env             []string // appended to os.Environ()
	WorkingDir      string
	ScrollbackBytes int
	Rows, Cols      uint16
}

// New spawns the command attached to a new PTY.
func New(opt Options) (*Terminal, error) {
	cmd := exec.Command(opt.Command, opt.Args...)
	cmd.Env = append(os.Environ(), opt.Env...)
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	if opt.WorkingDir != "" {
		cmd.Dir = opt.WorkingDir
	}

	rows, cols := opt.Rows, opt.Cols
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}

	t := &Terminal{
		ptmx:        ptmx,
		cmd:         cmd,
		subscribers: make(map[int]*Subscriber),
		ring:        newRingBuffer(opt.ScrollbackBytes),
		exited:      make(chan struct{}),
	}
	go t.readLoop()
	go t.wait()
	return t, nil
}

func (t *Terminal) wait() {
	err := t.cmd.Wait()
	t.mu.Lock()
	t.exitErr = err
	t.mu.Unlock()
	close(t.exited)
	t.Close()
}

// readLoop pumps PTY output into the ring buffer and all subscribers.
// It never blocks on a consumer: pushes are buffered, and a subscriber that
// exceeds its buffer cap is dropped (it will reconnect and be repainted).
func (t *Terminal) readLoop() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			var dropped []*Subscriber
			t.mu.Lock()
			t.ring.Write(chunk)
			for id, sub := range t.subscribers {
				if !sub.push(chunk) {
					delete(t.subscribers, id)
					dropped = append(dropped, sub)
				}
			}
			t.mu.Unlock()
			for _, sub := range dropped {
				sub.close()
			}
		}
		if err != nil {
			return
		}
	}
}

// Subscribe registers a new consumer and returns the current scrollback
// snapshot (write it to the client before draining the subscriber), the
// subscriber, and an unsubscribe function.
func (t *Terminal) Subscribe() (snapshot []byte, sub *Subscriber, unsub func(), ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, nil, func() {}, false
	}
	id := t.nextSubID
	t.nextSubID++
	sub = newSubscriber()
	t.subscribers[id] = sub
	snapshot = t.ring.Snapshot()
	unsub = func() {
		t.mu.Lock()
		s, exists := t.subscribers[id]
		if exists {
			delete(t.subscribers, id)
		}
		t.mu.Unlock()
		if exists {
			s.close()
		}
	}
	return snapshot, sub, unsub, true
}

// SubscriberCount returns how many clients are attached.
func (t *Terminal) SubscriberCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.subscribers)
}

// Write sends bytes to the PTY (keyboard input).
func (t *Terminal) Write(p []byte) (int, error) {
	return t.ptmx.Write(p)
}

// Resize changes the PTY window size.
func (t *Terminal) Resize(rows, cols uint16) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Cwd returns the child process's current working directory, or "" when it
// cannot be determined (process exited, or a platform without /proc such as
// macOS/Windows — auto tab titles simply stay at their initial value there).
func (t *Terminal) Cwd() string {
	if t.cmd.Process == nil {
		return ""
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(t.cmd.Process.Pid) + "/cwd")
	if err != nil {
		return ""
	}
	return cwd
}

// Exited returns a channel closed when the child process exits.
func (t *Terminal) Exited() <-chan struct{} {
	return t.exited
}

// Close terminates the process and releases the PTY. Safe to call repeatedly.
func (t *Terminal) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	subs := t.subscribers
	t.subscribers = make(map[int]*Subscriber)
	t.mu.Unlock()

	for _, s := range subs {
		s.close()
	}
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	err := t.ptmx.Close()
	if err == io.EOF {
		return nil
	}
	return err
}
