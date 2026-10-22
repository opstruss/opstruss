package proc

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Members returns the processes of this user in the group, other than
// zombies, from the pgrp field of /proc/<pid>/stat.
func (g Group) Members() ([]Process, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Getuid())
	var found []Process
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
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
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		open, end := strings.IndexByte(string(stat), '('), strings.LastIndexByte(string(stat), ')')
		fields := strings.Fields(string(stat[end+1:]))
		if open < 0 || end < open || len(fields) < 3 || fields[0] == "Z" || fields[2] != strconv.Itoa(int(g)) {
			continue
		}
		found = append(found, Process{PID: pid, Command: string(stat[open+1 : end])})
	}
	return found, nil
}
