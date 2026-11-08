package cli

import "golang.org/x/sys/unix"

// The ioctls that read and set the state of a terminal, on Linux.
const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)
