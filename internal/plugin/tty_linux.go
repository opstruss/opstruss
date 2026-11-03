package plugin

import "golang.org/x/sys/unix"

// unread is the ioctl that counts the bytes that the master of a terminal
// has still to read: its input queue, on Linux.
const unread = unix.TIOCINQ
