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
// subscriber before it is resynced. The PTY read loop never blocks on a
// consumer; one that falls this far behind has its stale backlog discarded and
// is repainted from the current scrollback snapshot instead (see resyncTo), so
// the program never pauses and other viewers are unaffected.
const maxSubscriberBuffer = 1 << 20 // 1 MiB

// risReset is ESC c (RIS, "reset to initial state") — a full terminal reset.
// It prefixes an overflow resync so the client repaints from a clean slate
// in-band, ordered correctly with surrounding output, without an out-of-band
// terminal.reset() that could race the async write queue mid-stream.
var risReset = []byte{0x1b, 'c'}

// Subscriber is one attached output consumer. Output chunks are coalesced
// into an internal buffer and the consumer is signalled via Notify; this
// also batches many small PTY reads into fewer websocket frames.
type Subscriber struct {
	mu     sync.Mutex
	buf    []byte
	closed bool
	resync bool // next Take is an overflow resync: send it as a repaint, not a stream chunk
	notify chan struct{}
}

func newSubscriber() *Subscriber {
	return &Subscriber{notify: make(chan struct{}, 1)}
}

// Notify signals whenever output is pending or the subscriber has closed.
func (s *Subscriber) Notify() <-chan struct{} { return s.notify }

// Take returns all pending output, whether the subscriber is still open, and
// whether this batch is an overflow resync (a repaint the writer must send as
// a replay frame, not a stream chunk). A final call may return both remaining
// data and open == false.
func (s *Subscriber) Take() (data []byte, open, resync bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data = s.buf
	s.buf = nil
	resync = s.resync
	s.resync = false
	return data, !s.closed, resync
}

// push appends output without ever blocking; reports false on overflow,
// meaning the subscriber is too far behind and must be resynced (its stale
// backlog dropped and replaced with a fresh snapshot — see resyncTo).
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

// resyncTo discards the subscriber's stale backlog and replaces it with a full
// repaint (RIS reset + a fresh scrollback snapshot), flagged so the writer
// sends it as a replay frame. Used when the consumer fell too far behind:
// rather than dropping it (which forces a reconnect) or blocking the producer
// (which would stall the program and every other viewer, à la ttyd), the
// client is simply jumped to the current screen. The program never pauses.
func (s *Subscriber) resyncTo(snapshot []byte) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	buf := make([]byte, 0, len(risReset)+len(snapshot))
	buf = append(buf, risReset...)
	buf = append(buf, snapshot...)
	s.buf = buf
	s.resync = true
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

	// activity marks that output arrived since the last TakeActivity();
	// lets pollers skip idle terminals. reportedCwd is the last directory
	// the shell announced via OSC 7 — exact and container/ssh-aware, so it
	// takes precedence over the /proc guess. osc7 is owned by readLoop.
	// All three are inert unless trackCwd is set.
	trackCwd    bool
	activity    bool
	reportedCwd string
	osc7        osc7Scanner
}

// Options configures a new terminal.
type Options struct {
	Command         string
	Args            []string
	Env             []string // appended to os.Environ()
	WorkingDir      string
	ScrollbackBytes int
	Rows, Cols      uint16
	// TrackCwd enables the machinery behind directory-tracking tab titles:
	// OSC 7 scanning of output and the activity flag. Leave false when
	// titles aren't shown so the read loop does zero extra work.
	TrackCwd bool
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
		trackCwd:    opt.TrackCwd,
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
			// Scan outside the lock; the scanner is owned by this goroutine.
			var cwd string
			var cwdOK bool
			if t.trackCwd {
				cwd, cwdOK = t.osc7.feed(chunk)
			}
			var behind []*Subscriber
			var snapshot []byte
			t.mu.Lock()
			t.ring.Write(chunk)
			if t.trackCwd {
				t.activity = true
				if cwdOK {
					t.reportedCwd = cwd
				}
			}
			for _, sub := range t.subscribers {
				if !sub.push(chunk) {
					// Too far behind to stream to. Snapshot the ring (which
					// already includes this chunk) once and repaint each such
					// consumer from it — the subscriber stays attached, so the
					// program is never paused and no reconnect is forced.
					if snapshot == nil {
						snapshot = t.ring.Snapshot()
					}
					behind = append(behind, sub)
				}
			}
			t.mu.Unlock()
			for _, sub := range behind {
				sub.resyncTo(snapshot)
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

// TakeActivity reports whether output arrived since the last call, clearing
// the flag. Pollers use it to skip idle terminals entirely.
func (t *Terminal) TakeActivity() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.activity
	t.activity = false
	return a
}

// Cwd returns the terminal's current working directory: the OSC 7 path the
// shell reported, when shell integration is present (exact, and correct for
// ssh/containers), else the /proc guess for the direct child. "" when
// neither is available (process exited, or a platform without /proc — auto
// tab titles simply stay at their initial value there).
func (t *Terminal) Cwd() string {
	t.mu.Lock()
	reported := t.reportedCwd
	t.mu.Unlock()
	if reported != "" {
		return reported
	}
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
