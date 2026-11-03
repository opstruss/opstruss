package plugin

import (
	"os"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TTY is the size of a terminal that a program gets of its own.
type TTY struct{ Cols, Rows int }

// terminal is the pseudo-terminal of one program: the program writes its
// stdout and stderr to the slave, and the call reads them from the master
// as one stream, in the order that a terminal shows them.
type terminal struct {
	master, slave *os.File
}

// openTerminal opens a terminal of the given size. The program does not
// get it as its controlling terminal: /dev/tty stays the operator's.
func openTerminal(size TTY) (*terminal, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	err = pty.Setsize(master, &pty.Winsize{Cols: uint16(size.Cols), Rows: uint16(size.Rows)})
	fd := -1
	if err == nil {
		// The master that pty.Open gives blocks a read that Close cannot
		// end; a copy that does not block reads through the poller of Go,
		// as a pipe does, so drain can give up on it.
		fd, err = unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	}
	_ = master.Close()
	if err == nil {
		err = syscall.SetNonblock(fd, true)
	}
	if err != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		_ = slave.Close()
		return nil, err
	}
	return &terminal{master: os.NewFile(uintptr(fd), "/dev/ptmx"), slave: slave}, nil
}

// settle waits, up to d, until the call has read what the program wrote,
// then closes the copy of the slave that the call kept while the program
// ran: macOS drops what is still queued for the master once the last copy
// of the slave goes.
func (t *terminal) settle(d time.Duration) {
	defer func() { _ = t.slave.Close() }()
	rc, err := t.master.SyscallConn()
	if err != nil {
		return
	}
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n := 0
		if rc.Control(func(fd uintptr) { n, err = unix.IoctlGetInt(int(fd), unread) }) != nil || err != nil || n == 0 {
			return
		}
	}
}

// close closes the slave of a terminal whose program did not start.
func (t *terminal) close() {
	if t != nil {
		_ = t.slave.Close()
	}
}
