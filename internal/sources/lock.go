package sources

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/opstruss/opstruss/internal/failpoint"

	"go.yaml.in/yaml/v3"
)

const LockName = "opstruss.lock"

type Pin struct {
	Commit string `json:"commit" yaml:"commit"`
	Hash   string `json:"hash" yaml:"hash"`
}

type Lock struct {
	Version int            `json:"version" yaml:"version"`
	Sources map[string]Pin `json:"sources" yaml:"sources"`
}

func ReadLock(dir string) (*Lock, error) {
	l := &Lock{Version: 1, Sources: map[string]Pin{}}
	b, err := os.ReadFile(filepath.Join(dir, LockName))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(l); err != nil {
		return nil, fmt.Errorf("%s: %w", LockName, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s: expected one document", LockName)
	}
	if l.Version != 1 || l.Sources == nil {
		return nil, fmt.Errorf("%s: unsupported version or missing sources; upgrade OpsTruss if written by a newer version", LockName)
	}
	groups := map[string]Pin{}
	for key, pin := range l.Sources {
		id, err := Parse(key)
		if err != nil {
			return nil, err
		}
		if id.Key() != key {
			return nil, fmt.Errorf("noncanonical lock source %s", key)
		}
		id.Commit, id.Hash = pin.Commit, pin.Hash
		if err := id.Validate(); err != nil {
			return nil, err
		}
		if other, ok := groups[id.Group()]; ok && other != pin {
			return nil, fmt.Errorf("conflicting pins for %s", id.Group())
		}
		groups[id.Group()] = pin
	}
	return l, nil
}

// Options separates metadata discovery from execution and explicit updates.
type Options struct {
	Locked       bool
	Discovery    bool
	Repair       bool
	Update       bool
	UpdateGroups []string
}

// Session owns one root lock transaction and every cache lease it resolves.
type Session struct {
	Unavailable bool

	mu   sync.Mutex
	refs int

	Cache    *Cache
	Options  Options
	ctx      context.Context
	dir      string
	lock     *Lock
	original map[string]Pin
	writer   *Lease
	leases   []*Lease
	resolved map[string]Identity
	groups   map[string]Pin
	reached  map[string]bool
	binaries []binaryPin
}

type binaryPin struct {
	Name   string
	Binary Binary
}

// Acquire verifies and leases the immutable dependencies of a saved model.
// A trigger can retain metadata after its previous execution leases ended.
func (s *Session) Acquire(ctx context.Context) (*Session, error) {
	s.mu.Lock()
	copy := &Session{refs: 1, Cache: s.Cache, Options: s.Options, ctx: ctx, dir: s.dir, resolved: maps.Clone(s.resolved), binaries: slices.Clone(s.binaries)}
	s.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(copy.resolved)) {
		id := copy.resolved[key]
		_, _, lease, err := copy.Cache.Git(ctx, id, false, false)
		if err != nil {
			copy.Close()
			return nil, err
		}
		copy.leases = append(copy.leases, lease)
	}
	for _, pin := range copy.binaries {
		_, lease, err := copy.Cache.Binary(ctx, pin.Name, pin.Binary, false, false)
		if err != nil {
			copy.Close()
			return nil, err
		}
		copy.leases = append(copy.leases, lease)
	}
	return copy, nil
}

func Begin(ctx context.Context, dir string, opts Options) (*Session, error) {
	c, err := UserCache()
	if err != nil {
		return nil, err
	}
	s := &Session{refs: 1, Cache: c, Options: opts, ctx: ctx, dir: dir, resolved: map[string]Identity{}, groups: map[string]Pin{}, reached: map[string]bool{}}

	s.lock, err = ReadLock(dir)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.initializePins()

	return s, nil
}

func (s *Session) initializePins() {
	s.original = maps.Clone(s.lock.Sources)
	s.groups = map[string]Pin{}
	for key, pin := range s.lock.Sources {
		id, _ := Parse(key)
		if !s.Options.Update && !slices.Contains(s.Options.UpdateGroups, id.Group()) {
			s.groups[id.Group()] = pin
		}
	}
}

func (s *Session) ensureWriter() error {
	if s.writer != nil || s.Options.Discovery {
		return nil
	}
	writer, err := fileLock(s.ctx, filepath.Join(s.dir, ".opstruss", "sources.lock"), true, true)
	if err != nil {
		return err
	}
	lock, err := ReadLock(s.dir)
	if err != nil {
		writer.Close()
		return err
	}
	s.writer, s.lock = writer, lock
	s.initializePins()
	return nil
}

func (s *Session) Resolve(source string) (Identity, string, error) {
	id, err := Parse(source)
	if err != nil {
		return id, "", err
	}
	if err := s.ensureWriter(); err != nil {
		return id, "", err
	}
	s.reached[id.Key()] = true
	if old, ok := s.resolved[id.Key()]; ok {
		return old, filepath.Join(s.Cache.Snapshot(old), filepath.FromSlash(old.Path)), nil
	}
	pin, ok := s.groups[id.Group()]
	if !ok {
		if s.Options.Locked && !s.Options.Update && !slices.Contains(s.Options.UpdateGroups, id.Group()) {
			return id, "", fmt.Errorf("%s requires a pin change in %s; run plugins fetch or update without --locked", id.Key(), LockName)
		}
		if s.Options.Discovery {
			s.Unavailable = true
			return id, "", fmt.Errorf("%s: metadata unavailable; run opstruss plugins fetch first", id.Key())
		}
		commit, err := s.Cache.ResolveRef(s.ctx, id)
		if err != nil {
			return id, "", err
		}
		pin.Commit = commit
		// A ref update to an already pinned commit must retain its expected hash.
		for _, old := range s.original {
			if old.Commit == commit {
				pin.Hash = old.Hash
				break
			}
		}
	}
	id.Commit, id.Hash = pin.Commit, pin.Hash
	id, root, l, err := s.Cache.Git(s.ctx, id, s.Options.Repair, s.Options.Discovery)
	if err != nil {
		if s.Options.Discovery {
			s.Unavailable = true
		}
		return id, "", err
	}
	s.leases = append(s.leases, l)
	s.groups[id.Group()] = Pin{Commit: id.Commit, Hash: id.Hash}
	if !s.Options.Discovery {
		s.lock.Sources[id.Key()] = s.groups[id.Group()]
	}
	s.resolved[id.Key()] = id
	dir, err := Within(root, filepath.Join(root, filepath.FromSlash(id.Path)))
	return id, dir, err
}

// Owner finds provenance inherited by relative paths inside a remote tree.
func (s *Session) Owner(name string) (*Identity, string, error) {
	for _, key := range slices.Sorted(maps.Keys(s.resolved)) {
		id := s.resolved[key]
		root := s.Cache.Snapshot(id)
		rel, err := filepath.Rel(root, name)
		if err == nil && filepath.IsLocal(rel) {
			real, err := Within(root, name)
			if err != nil {
				return nil, "", err
			}
			rel, _ = filepath.Rel(root, real)
			copy := id
			copy.Path = filepath.ToSlash(rel)
			return &copy, root, nil
		}
	}
	return nil, "", nil
}

func (s *Session) Finish() error {
	if s.Options.Discovery {
		return nil
	}
	if s.Options.Update && s.writer == nil {
		if err := s.ensureWriter(); err != nil {
			return err
		}
	}
	// Updating a group moves every known subdirectory of that group together.
	for key := range s.lock.Sources {
		id, _ := Parse(key)
		if pin, ok := s.groups[id.Group()]; ok {
			s.lock.Sources[key] = pin
		}
	}
	if s.Options.Update {
		for key := range s.lock.Sources {
			if !s.reached[key] {
				delete(s.lock.Sources, key)
			}
		}
	}
	if maps.Equal(s.original, s.lock.Sources) {
		s.writer.Close()
		s.writer = nil
		return nil
	}
	if s.Options.Locked {
		return fmt.Errorf("--locked forbids changing %s", LockName)
	}
	b, err := yaml.Marshal(s.lock)
	if err != nil {
		return err
	}
	failpoint.Hit("sources-before-lock")
	if err := atomicWrite(filepath.Join(s.dir, LockName), b); err != nil {
		return err
	}
	failpoint.Hit("sources-after-lock")
	s.writer.Close()
	s.writer = nil
	return nil
}

func (s *Session) Pins() map[string]Pin { return maps.Clone(s.lock.Sources) }

// Retain transfers an additional lifetime to a run, service or snapshot.
func (s *Session) Retain() func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	s.refs++
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(s.Close) }
}
func (s *Session) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs--
	if s.refs != 0 {
		return
	}
	s.writer.Close()
	s.writer = nil
	for _, l := range s.leases {
		l.Close()
	}
	s.leases = nil
}

func (s *Session) AddBinary(name string, b Binary, l *Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases = append(s.leases, l)
	pin := binaryPin{name, b}
	if !slices.Contains(s.binaries, pin) {
		s.binaries = append(s.binaries, pin)
	}
}

// Keys returns a deterministic inventory, without fetching or touching usage times.
func (l *Lock) Keys() []string { return slices.Sorted(maps.Keys(l.Sources)) }

func (s *Session) Context() context.Context { return s.ctx }

// Files are inherited by programs so a killed parent cannot expose active code to prune.
func (s *Session) Files() []*os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var files []*os.File
	for _, l := range s.leases {
		files = append(files, l.file)
	}
	return files
}
