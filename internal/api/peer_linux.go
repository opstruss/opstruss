package api

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID returns the user of the process at the other end of a Unix
// socket, from SO_PEERCRED.
func peerUID(nc net.Conn) (int, error) {
	uc, ok := nc.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a Unix socket")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}
