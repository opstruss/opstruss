package sources

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"

	"golang.org/x/sys/unix"
)

// Cache owns immutable snapshots and their cross-process leases.
type Cache struct{ Root string }

var ErrInUse = errors.New("cache entry is in use")

func UserCache() (*Cache, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	} else if parent, err := filepath.EvalSymlinks(filepath.Dir(home)); err == nil {
		home = filepath.Join(parent, filepath.Base(home))
	}
	return &Cache{Root: filepath.Join(home, ".opstruss", "cache")}, nil
}

func cacheKey(s string) string             { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func (c *Cache) gitKey(id Identity) string { return "git-" + cacheKey(id.Repository+"@"+id.Commit) }
func (c *Cache) entry(key string) string {
	kind, hash, _ := strings.Cut(key, "-")
	return filepath.Join(c.Root, kind, hash)
}
func (c *Cache) Snapshot(id Identity) string { return c.entry(c.gitKey(id)) }

// Lease protects an entry until all its consumers have stopped.
type Lease struct {
	file *os.File
	once sync.Once
}

func (l *Lease) Close() {
	if l != nil {
		l.once.Do(func() { _ = l.file.Close() })
	}
}

func fileLock(ctx context.Context, name string, exclusive, wait bool) (*Lease, error) {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return &Lease{file: f}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		if !wait {
			_ = f.Close()
			return nil, ErrInUse
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *Cache) lock(ctx context.Context, key string, exclusive, wait bool) (*Lease, error) {
	if err := c.checkLayout(); err != nil {
		return nil, err
	}
	return fileLock(ctx, filepath.Join(c.Root, "locks", key), exclusive, wait)
}

type cacheMeta struct {
	Used    time.Time   `json:"used"`
	Entries []treeEntry `json:"entries,omitempty"`
}

func (c *Cache) metadata(key string) string { return filepath.Join(c.Root, "meta", key+".json") }
func (c *Cache) touch(key string, entries []treeEntry) error {
	return atomicJSON(c.metadata(key), cacheMeta{Used: time.Now().UTC(), Entries: entries})
}

func (c *Cache) readMeta(key string) (cacheMeta, error) {
	var m cacheMeta
	b, err := os.ReadFile(c.metadata(key))
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}

func atomicJSON(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(name, append(b, '\n'))
}

func atomicWrite(name string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".write-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), name); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// removeEntry never follows symlinks, including when making directories writable.
func removeEntry(name string) error {
	err := filepath.WalkDir(name, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o700)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(name)
}

// Prune removes only entries unused for thirty days and not leased.
func (c *Cache) Prune(ctx context.Context) ([]string, error) {
	var removed []string
	if err := c.checkLayout(); err != nil {
		return nil, err
	}
	metas, err := os.ReadDir(filepath.Join(c.Root, "meta"))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range metas {
		key := strings.TrimSuffix(e.Name(), ".json")
		kind, hash, ok := strings.Cut(key, "-")
		if !ok || (kind != "git" && kind != "bin") || len(hash) != 64 {
			continue
		}
		if _, err := hex.DecodeString(hash); err != nil {
			continue
		}
		l, err := c.lock(ctx, key, true, false)
		if err != nil {
			continue
		}
		m, err := c.readMeta(key)
		if err == nil && time.Since(m.Used) >= 30*24*time.Hour {
			err = removeEntry(c.entry(key))
			if err == nil {
				err = os.Remove(c.metadata(key))
			}
			if err == nil {
				removed = append(removed, key)
			}
		}
		l.Close()
		if err != nil {
			return removed, err
		}
	}
	// Each temporary directory has its own lock, retained outside it.
	guard, err := c.lock(ctx, "temporary", true, true)
	if err != nil {
		return removed, err
	}
	defer guard.Close()
	temps, err := os.ReadDir(filepath.Join(c.Root, "tmp"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	for _, e := range temps {
		l, err := c.lock(ctx, "tmp-"+e.Name(), true, false)
		if err != nil {
			continue
		}
		err = removeEntry(filepath.Join(c.Root, "tmp", e.Name()))
		l.Close()
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func (c *Cache) temporary(ctx context.Context) (string, context.Context, func(), error) {
	if err := os.MkdirAll(filepath.Join(c.Root, "tmp"), 0o700); err != nil {
		return "", nil, nil, err
	}
	// Hold a parent lock while publishing the temporary name and its lease.
	guard, err := c.lock(ctx, "temporary", true, true)
	if err != nil {
		return "", nil, nil, err
	}
	defer guard.Close()
	dir, err := os.MkdirTemp(filepath.Join(c.Root, "tmp"), "fetch-")
	if err != nil {
		return "", nil, nil, err
	}
	l, err := c.lock(ctx, "tmp-"+filepath.Base(dir), true, true)
	if err != nil {
		_ = removeEntry(dir)
		return "", nil, nil, err
	}
	return dir, context.WithValue(ctx, gitLeaseKey{}, l.file), func() { _ = removeEntry(dir); l.Close() }, nil
}

// Git obtains a pinned snapshot. Repair is reserved for explicit fetch.
func (c *Cache) Git(ctx context.Context, id Identity, repair, readOnly bool) (Identity, string, *Lease, error) {
retry:
	key := c.gitKey(id)
	verify := func() error {
		m, err := c.readMeta(key)
		if err != nil {
			return err
		}
		if id.Hash == "" {
			id.Hash = treeHash(m.Entries)
		}
		return verifyTree(c.entry(key), m.Entries, id.Hash)
	}
	if readOnly {
		if _, err := os.Lstat(c.entry(key)); err != nil {
			return id, "", nil, fmt.Errorf("%s: source metadata unavailable; run opstruss plugins fetch first", id.Key())
		}
	}
	l, err := c.lock(ctx, key, false, !readOnly)
	if err != nil {
		return id, "", nil, err
	}
	_, statErr := os.Lstat(c.entry(key))
	if statErr == nil {
		err = verify()
		if err == nil {
			if !readOnly {
				m, _ := c.readMeta(key)
				err = c.touch(key, m.Entries)
			}
			if err == nil {
				return id, c.entry(key), l, nil
			}
		}
		if !repair {
			l.Close()
			return id, "", nil, fmt.Errorf("%s: cache integrity: %w; run opstruss plugins fetch to repair", id.Key(), err)
		}
	}
	l.Close()
	if readOnly {
		return id, "", nil, fmt.Errorf("%s: source metadata unavailable; run opstruss plugins fetch first", id.Key())
	}
	l, err = c.lock(ctx, key, true, false)
	if err != nil {
		// A competing publisher may now share the complete entry with consumers.
		if errors.Is(err, ErrInUse) && errors.Is(statErr, os.ErrNotExist) {
			goto retry
		}
		return id, "", nil, err
	}
	defer func() {
		if l != nil {
			l.Close()
		}
	}()
	if _, err := os.Lstat(c.entry(key)); err == nil && verify() == nil {
		// Another publisher completed while the shared lease was released.
	} else {
		tmp, workCtx, cleanup, err := c.temporary(ctx)
		if err != nil {
			return id, "", nil, err
		}
		defer cleanup()
		entries, err := materialize(workCtx, id, tmp)
		if err != nil {
			return id, "", nil, err
		}
		hash := treeHash(entries)
		if id.Hash != "" && id.Hash != hash {
			return id, "", nil, fmt.Errorf("%s: fetched source hash mismatch", id.Key())
		}
		id.Hash = hash
		tree := filepath.Join(tmp, "tree")
		if err := verifyTree(tree, entries, hash); err != nil {
			return id, "", nil, err
		}
		if err := os.MkdirAll(filepath.Dir(c.entry(key)), 0o700); err != nil {
			return id, "", nil, err
		}
		if err := removeEntry(c.entry(key)); err != nil {
			return id, "", nil, err
		}
		failpoint.Hit("source-before-publish")
		if err := os.Chmod(tree, 0o700); err != nil {
			return id, "", nil, err
		}
		if err := os.Rename(tree, c.entry(key)); err != nil {
			return id, "", nil, err
		}
		if err := os.Chmod(c.entry(key), 0o555); err != nil {
			return id, "", nil, err
		}
		if err := c.touch(key, entries); err != nil {
			return id, "", nil, err
		}
		failpoint.Hit("source-after-publish")
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_SH); err != nil {
		return id, "", nil, err
	}
	out := l
	l = nil
	return id, c.entry(key), out, nil
}

func (l *Lease) File() *os.File { return l.file }

// checkLayout rejects symlinks in managed directories before writes or pruning.
func (c *Cache) checkLayout() error {
	for _, dir := range []string{filepath.Dir(c.Root), c.Root, filepath.Join(c.Root, "git"), filepath.Join(c.Root, "bin"), filepath.Join(c.Root, "tmp"), filepath.Join(c.Root, "meta"), filepath.Join(c.Root, "locks")} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("unsafe cache directory %s", dir)
		}
	}
	return nil
}
