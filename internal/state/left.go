package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func leftDir(backend, key string) (string, error) {
	if strings.HasPrefix(key, probePrefix) {
		return "", nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".opstruss", "left-locks", leftHash(backend+"\x00"+key)), nil
}

func leftHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// LeaveLock records an abandoned acquisition so another process on this
// host can release it while the original process is still running.
func LeaveLock(backend, key, token string) error {
	dir, err := leftDir(backend, key)
	if err != nil || dir == "" {
		return err
	}
	p := filepath.Join(dir, leftHash(token))
	for {
		// Cleanup may remove the key directory while it is empty.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		err := os.Mkdir(p, 0o700)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}

// LeftLock reports whether this host gave up this acquisition.
func LeftLock(backend, key, token string) bool {
	dir, err := leftDir(backend, key)
	if err != nil || dir == "" || token == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, leftHash(token)))
	return err == nil && info.IsDir()
}

// ForgetLock removes the marker for the acquisition that was released.
func ForgetLock(backend, key, token string) {
	if dir, err := leftDir(backend, key); err == nil && dir != "" {
		if os.Remove(filepath.Join(dir, leftHash(token))) == nil {
			_ = os.Remove(dir)
		}
	}
}

// LeftLockCleanup captures the markers that a successful acquisition
// invalidates. Take the snapshot before asking the backend so a delayed
// cleanup cannot remove a marker for a later acquisition.
func LeftLockCleanup(backend, key string) func() {
	dir, err := leftDir(backend, key)
	if err != nil || dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return nil
	}
	return func() {
		for _, entry := range entries {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
		_ = os.Remove(dir)
	}
}
