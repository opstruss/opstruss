package state

import "golang.org/x/sys/unix"

// networkTypes are the magic numbers of the filesystems in statfs(2) that
// keep files on another machine, or may: fuse is often sshfs or rclone.
var networkTypes = map[int64]string{
	0x6969:     "nfs",
	0x517b:     "smb",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x5346414f: "afs",
	0x73757245: "coda",
	0x01021997: "9p",
	0x00c36400: "ceph",
	0x0bd00bd0: "lustre",
	0x65735546: "fuse",
}

// NetworkFS reports whether path is on a filesystem that keeps it on
// another machine, where flock may not lock, and names the filesystem.
func NetworkFS(path string) (string, bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", false, err
	}
	name, ok := networkTypes[int64(st.Type)]
	return name, ok, nil
}
