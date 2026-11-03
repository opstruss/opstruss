package plugin

import "golang.org/x/sys/unix"

// unread is the ioctl that counts the bytes that the master of a terminal
// has still to read: the output queue of the terminal, on macOS.
const unread = unix.TIOCOUTQ
