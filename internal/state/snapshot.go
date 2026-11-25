package state

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
)

// Snapshot is the keys of one scope as one read found them, for what reads
// many records at once, such as a plan and destroy: the scan of a backend
// that gives one, or its list, with each record read when it is first
// wanted, so a key that the list does not have is never read. It is no
// transaction and has no etags: nothing is written from it, as writes take
// the etag that Get gives under the lock.
type Snapshot struct {
	store  *Store
	global *Snapshot // that of the Global store, read with this one

	mu   sync.Mutex
	keys map[string]bool
	data map[string][]byte // what scan gave, and each record read since
}

// Snapshot reads the scope of the store, and that of its Global store.
func (s *Store) Snapshot() (*Snapshot, error) {
	sn, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if s.Global != nil {
		if sn.global, err = s.Global.snapshot(); err != nil {
			return nil, err
		}
	}
	return sn, nil
}

func (s *Store) snapshot() (*Snapshot, error) {
	keys, data, err := s.scan(s.Stack + "/")
	if err != nil {
		return nil, err
	}
	sn := &Snapshot{store: s, keys: map[string]bool{}, data: map[string][]byte{}}
	for _, k := range keys {
		sn.keys[k] = true
	}
	maps.Copy(sn.data, data)
	return sn, nil
}

// scan gives the keys under prefix, and their data too from a backend that
// scans them in one operation. From one that cannot, data is nil, and the
// caller gets the keys it needs.
func (s *Store) scan(prefix string) ([]string, map[string][]byte, error) {
	if sc, ok := s.Backend.(Scanner); ok {
		items, err := sc.Scan(prefix)
		if err == nil {
			return slices.Sorted(maps.Keys(items)), items, nil
		}
		if !errors.Is(err, errors.ErrUnsupported) {
			return nil, nil, err
		}
	}
	keys, err := s.Backend.List(prefix)
	return keys, nil, err
}

// Global is the snapshot of the global scope, or nil.
func (sn *Snapshot) Global() *Snapshot { return sn.global }

// at is the snapshot that holds address: Global for an address of its scope.
func (sn *Snapshot) at(address string) *Snapshot {
	if sn.global != nil && strings.HasPrefix(address, sn.global.store.Stack+"/") {
		return sn.global
	}
	return sn
}

func (sn *Snapshot) has(key string) bool {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	return sn.keys[key]
}

// Inventory is the addresses of the scope with a record, tombstones
// included, as Store.Inventory gives them.
func (sn *Snapshot) Inventory() []string {
	sn.mu.Lock()
	keys := slices.Sorted(maps.Keys(sn.keys))
	sn.mu.Unlock()
	return inventory(sn.store.Stack, keys)
}

// Get returns the record at address as the snapshot has it, or nil when
// there is none.
func (sn *Snapshot) Get(address string) (*Record, error) {
	sn = sn.at(address)
	if err := sn.store.checkAddress(address); err != nil {
		return nil, err
	}
	key := RecordKey(address)
	sn.mu.Lock()
	listed := sn.keys[key]
	data, read := sn.data[key]
	sn.mu.Unlock()
	if !listed {
		return nil, nil
	}
	if !read {
		d, _, err := sn.store.Backend.Get(key)
		if errors.Is(err, ErrNotExist) {
			// Gone since the list: it is not in the scope now.
			sn.mu.Lock()
			delete(sn.keys, key)
			sn.mu.Unlock()
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		sn.mu.Lock()
		sn.data[key] = d
		sn.mu.Unlock()
		data = d
	}
	return sn.store.decode(data, address)
}

// Held reports whether another holder has the lock of an address, and who,
// as Store.Held does, for a plan that only looks. A plugin backend lists a
// lock only while it is held, so one that the snapshot does not have is
// free without another operation; the local backend keeps its lock files,
// so it looks with flock.
func (sn *Snapshot) Held(address string, h Holder) (Holder, bool, error) {
	sn = sn.at(address)
	if _, local := sn.store.Backend.(*Local); !local && !sn.has(LockKey(address)) {
		return Holder{}, false, nil
	}
	return sn.store.Held(address, h)
}
