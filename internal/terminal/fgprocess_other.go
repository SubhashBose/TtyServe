//go:build !linux && !darwin

package terminal

// ForegroundName is unsupported on this platform; auto tab titles fall back
// to their other sources.
func (t *Terminal) ForegroundName() string { return "" }
