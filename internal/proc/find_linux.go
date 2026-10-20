package proc

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// find reads /proc/<pid>/environ of each process that this user owns. A
// zombie has an empty environment there, so it is not found.
func find(entry []byte) ([]Process, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	uid, self := uint32(os.Getuid()), os.Getpid()
	var found []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		// A process can end at any point; what cannot be read is gone.
		info, err := e.Info()
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		env, err := os.ReadFile("/proc/" + e.Name() + "/environ")
		if err != nil || !holds(env, entry) {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		found = append(found, Process{PID: pid, Command: string(bytes.TrimSpace(comm))})
	}
	return found, nil
}
