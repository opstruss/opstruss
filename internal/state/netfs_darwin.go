package state

import "golang.org/x/sys/unix"

// NetworkFS reports whether path is on a filesystem that keeps it on
// another machine, where flock may not lock, and names the filesystem.
func NetworkFS(path string) (string, bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", false, err
	}
	return unix.ByteSliceToString(st.Fstypename[:]), st.Flags&unix.MNT_LOCAL == 0, nil
}
