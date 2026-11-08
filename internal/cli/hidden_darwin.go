package cli

import "golang.org/x/sys/unix"

// The ioctls that read and set the state of a terminal, on macOS.
const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)
