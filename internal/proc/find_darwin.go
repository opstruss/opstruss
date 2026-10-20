package proc

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// zombie is SZOMB in sys/proc.h: a process that ended and is not reaped.
const zombie = 5

// find lists this user's processes with sysctl kern.proc.uid and reads
// each one's environment with kern.procargs2.
func find(entry []byte) ([]Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var found []Process
	for i := range procs {
		p := &procs[i].Proc
		pid := int(p.P_pid)
		if pid == self || pid == 0 || p.P_stat == zombie {
			continue
		}
		// A process can end at any point; what cannot be read is gone.
		args, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil {
			continue
		}
		if exe, env := procargs(args); holds(env, entry) {
			found = append(found, Process{PID: pid, Command: filepath.Base(exe)})
		}
	}
	return found, nil
}

// procargs splits kern.procargs2: argc, the executable path, padding,
// argc arguments, then the environment up to an empty string. Every string
// ends with NUL.
func procargs(b []byte) (exe string, env []byte) {
	if len(b) < 4 {
		return "", nil
	}
	argc := int(int32(binary.NativeEndian.Uint32(b)))
	path, rest, _ := bytes.Cut(b[4:], []byte{0})
	rest = bytes.TrimLeft(rest, "\x00")
	for ; argc > 0 && len(rest) > 0; argc-- {
		_, rest, _ = bytes.Cut(rest, []byte{0})
	}
	if end := bytes.Index(rest, []byte{0, 0}); end >= 0 {
		rest = rest[:end+1]
	}
	return string(path), rest
}
