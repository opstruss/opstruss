package state

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// Local is the local backend: a key is a file under its root, normally
// .opstruss/state/. Locks are flock on the lock file, which the system
// releases when the process dies, so a local lock is never stale.
type Local struct {
	root string

	mu    sync.Mutex
	held  map[string]*heldLock
	paths sync.Map // lock path → *sync.Mutex, because flock on some file systems does not exclude two descriptors of one process
}

type heldLock struct {
	token string
	f     *os.File
	mu    *sync.Mutex
}

// NewLocal returns the local backend whose keys live under root.
func NewLocal(root string) *Local {
	return &Local{root: root, held: map[string]*heldLock{}}
}

func (l *Local) path(key string) (string, error) {
	if key == "" || path.IsAbs(key) || path.Clean(key) != key || strings.HasPrefix(key, "../") || key == ".." {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return filepath.Join(l.root, filepath.FromSlash(key)), nil
}

// ETag is the etag of content: a hash, so any change changes it.
func ETag(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func (l *Local) Get(key string) ([]byte, string, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", ErrNotExist
	}
	if err != nil {
		return nil, "", err
	}
	return data, ETag(data), nil
}

// backup names the copy of the previous content that a put keeps.
func backup(p string) string {
	if strings.HasSuffix(p, ".json") {
		return strings.TrimSuffix(p, ".json") + ".backup.json"
	}
	return p + ".backup"
}

// Put writes through a temporary file, fsync, and rename, then fsyncs the
// directory, so a crash never leaves half a record. The previous content
// of a record or _meta goes to its backup first. Callers hold the lock of
// the key, so the etag check and the rename cannot interleave with another
// writer; a new key is created with link, which fails if it exists.
func (l *Local) Put(key string, data []byte, ifMatch string) (string, error) {
	p, err := l.path(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	tmp, err := writeTemp(filepath.Dir(p), data)
	if err != nil {
		return "", err
	}
	// After a rename or a link the temporary name is gone or redundant.
	defer func() { _ = os.Remove(tmp) }()
	if ifMatch == "" {
		if err := os.Link(tmp, p); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return "", ErrConflict
			}
			return "", err
		}
		return ETag(data), syncDir(filepath.Dir(p))
	}
	old, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	if ETag(old) != ifMatch {
		return "", ErrConflict
	}
	if keepsBackup(key) {
		if err := replace(backup(p), old); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, p); err != nil {
		return "", err
	}
	return ETag(data), syncDir(filepath.Dir(p))
}

func keepsBackup(key string) bool {
	return path.Base(key) == "state.json" || path.Base(key) == "_meta"
}

// Delete removes a key, keeping a record's last content as its backup. A
// lock file is never deleted, because flock locks the open file, not its
// path.
func (l *Local) Delete(key string, ifMatch string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotExist
	}
	if err != nil {
		return err
	}
	if ETag(old) != ifMatch {
		return ErrConflict
	}
	if keepsBackup(key) {
		if err := replace(backup(p), old); err != nil {
			return err
		}
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p))
}

// List returns the keys under prefix, without backups and temporary files.
func (l *Local) List(prefix string) ([]string, error) {
	base, err := l.path(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, err
	}
	var keys []string
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") || strings.Contains(name, ".backup") {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	slices.Sort(keys)
	return keys, err
}

// Lock takes the lock key without waiting.
func (l *Local) Lock(key string, h Holder) (string, error) {
	p, err := l.path(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	m, _ := l.paths.LoadOrStore(p, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	if !mu.TryLock() {
		holder := readHolder(f)
		_ = f.Close()
		return "", &LockedError{Key: key, Holder: holder}
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		mu.Unlock()
		holder := readHolder(f)
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "", &LockedError{Key: key, Holder: holder}
		}
		return "", err
	}
	h.Version = HolderVersion
	if err := writeHolder(f, h); err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		mu.Unlock()
		return "", err
	}
	token := newToken()
	l.mu.Lock()
	l.held[key] = &heldLock{token: token, f: f, mu: mu}
	l.mu.Unlock()
	return token, nil
}

// Held reports whether the lock key is held, and by whom, without taking
// it: a probe that writes nothing.
func (l *Local) Held(key string) (Holder, bool, error) {
	p, err := l.path(key)
	if err != nil {
		return Holder{}, false, err
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return Holder{}, false, nil
	}
	if err != nil {
		return Holder{}, false, err
	}
	defer func() { _ = f.Close() }()
	if m, ok := l.paths.Load(p); ok {
		mu := m.(*sync.Mutex)
		if !mu.TryLock() {
			return readHolder(f), true, nil
		}
		defer mu.Unlock()
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return readHolder(f), true, nil
	}
	if err != nil {
		return Holder{}, false, err
	}
	return Holder{}, false, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// writeHolder writes who holds the lock into the lock file, where a run
// that waits reads it. It is not synced: flock is the lock, and what the
// file says only names the holder to those who wait.
func writeHolder(f *os.File, h Holder) error {
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err = f.WriteAt(data, 0)
	return err
}

func readHolder(f *os.File) Holder {
	var h Holder
	buf := make([]byte, 4096)
	n, _ := f.ReadAt(buf, 0)
	_ = json.Unmarshal(buf[:n], &h)
	return h
}

func (l *Local) Unlock(key, token string) error {
	l.mu.Lock()
	h := l.held[key]
	if h == nil || h.token != token {
		l.mu.Unlock()
		return ErrNotHolder
	}
	delete(l.held, key)
	l.mu.Unlock()
	err := h.f.Truncate(0)
	if uerr := syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN); err == nil {
		err = uerr
	}
	if cerr := h.f.Close(); err == nil {
		err = cerr
	}
	h.mu.Unlock()
	return err
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeTemp(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// replace writes data to p atomically.
func replace(p string, data []byte) error {
	tmp, err := writeTemp(filepath.Dir(p), data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
