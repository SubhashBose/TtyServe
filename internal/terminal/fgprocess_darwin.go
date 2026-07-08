//go:build darwin

package terminal

import (
	"golang.org/x/sys/unix"
)

// ForegroundName returns the name of the PTY's foreground process group
// leader, or "". macOS has no /proc; the name comes from the kern.proc.pid
// sysctl (comm, truncated to 16 chars like Linux's /proc comm).
func (t *Terminal) ForegroundName() string {
	pgid, err := unix.IoctlGetInt(int(t.ptmx.Fd()), unix.TIOCGPGRP)
	if err != nil || pgid <= 0 {
		return ""
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid)
	if err != nil {
		return ""
	}
	comm := make([]byte, 0, len(kp.Proc.P_comm))
	for _, c := range kp.Proc.P_comm {
		if c == 0 {
			break
		}
		comm = append(comm, byte(c))
	}
	return string(comm)
}
