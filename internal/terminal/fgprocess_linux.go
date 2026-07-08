//go:build linux

package terminal

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// ForegroundName returns the name of the PTY's foreground process group
// leader (what's "running" in the terminal right now: the shell at a prompt,
// vim, ssh, …), or "" when it cannot be determined.
func (t *Terminal) ForegroundName() string {
	var pgid int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, t.ptmx.Fd(),
		syscall.TIOCGPGRP, uintptr(unsafe.Pointer(&pgid)))
	if errno != 0 || pgid <= 0 {
		return ""
	}
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(int(pgid)) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(comm))
}
