package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

// noEcho turns off the echo of the terminal f, for a line typed unseen,
// and returns what turns it back on. The line still ends at Enter, and
// Ctrl-C still interrupts, so the question waits as any other does.
func noEcho(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	t.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, setTermios, &t); err != nil {
		return nil, err
	}
	return func() { _ = unix.IoctlSetTermios(fd, setTermios, old) }, nil
}
