package proc

import (
	"os"

	"golang.org/x/sys/unix"
)

// Members returns the processes of this user in the group, other than
// zombies, from sysctl kern.proc.pgrp.
func (g Group) Members() ([]Process, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", int(g))
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Getuid())
	var found []Process
	for i := range procs {
		p := &procs[i]
		if p.Eproc.Ucred.Uid != uid || p.Proc.P_stat == zombie || p.Eproc.Pgid != int32(g) {
			continue
		}
		found = append(found, Process{PID: int(p.Proc.P_pid), Command: unix.ByteSliceToString(p.Proc.P_comm[:])})
	}
	return found, nil
}
