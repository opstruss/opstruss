package state

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// ProbeError names the step of the probe that a backend failed.
type ProbeError struct {
	Step int
	What string
	Err  error
}

func (e *ProbeError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("probe step %d: %s", e.Step, e.What)
	}
	return fmt.Sprintf("probe step %d: %s: %v", e.Step, e.What, e.Err)
}

func (e *ProbeError) Unwrap() error { return e.Err }

// Probe tries the contract on keys of its own under _probe/<id>/, which no
// stack reads, since initialize proves none of it: conditional puts and
// deletes, a read after each write, a list as deep as inventory needs, and
// locks with their holder and a token of each lock's own, which list only
// while they are held; scan as list, when the backend gives it. What a
// failed step leaves it removes as it can.
func Probe(b Backend, h Holder) (err error) {
	dir := "_probe/" + NewID() + "/"
	// As deep under dir as a record is under its stack, so that a list that
	// stops at the first level fails here rather than hide records.
	key, lock := dir+"unit/state.json", dir+"unit/state.lock"
	first, second, stale := []byte("{\"version\": 1}\n"), []byte("{\"version\": 2}\n"), []byte("{\"version\": 3}\n")
	var made bool // the key is there, so a failed step removes it
	var token string
	defer func() {
		if err == nil {
			return
		}
		if made {
			if _, etag, err := b.Get(key); err == nil {
				_ = b.Delete(key, etag)
			}
		}
		if token != "" {
			_ = b.Unlock(lock, token)
		}
	}()
	fail := func(step int, what string, err error) error {
		return &ProbeError{Step: step, What: what, Err: err}
	}
	unexpected := func(err error) error {
		if err == nil {
			return errors.New("it succeeded")
		}
		return err
	}

	etag, err := b.Put(key, first, "")
	if err != nil {
		return fail(1, "put of a new key", err)
	}
	made = true
	got, gotTag, err := b.Get(key)
	switch {
	case err != nil:
		return fail(1, "get after put", err)
	case !bytes.Equal(got, first) || gotTag != etag:
		return fail(1, "get after put gives what put wrote, with its etag", nil)
	}
	keys, err := b.List(dir)
	switch {
	case err != nil:
		return fail(1, "list", err)
	case !slices.Contains(keys, key):
		return fail(1, fmt.Sprintf("list of the prefix gives the key %s, not %v", key, keys), nil)
	}
	// scan, when the backend gives it, is tried as list is.
	scan := func(string) (map[string][]byte, error) { return nil, errors.ErrUnsupported }
	if sc, ok := b.(Scanner); ok {
		scan = sc.Scan
	}
	items, err := scan(dir)
	scans := !errors.Is(err, errors.ErrUnsupported)
	switch {
	case !scans:
	case err != nil:
		return fail(1, "scan", err)
	case !bytes.Equal(items[key], first) || len(items) != 1:
		return fail(1, fmt.Sprintf("scan of the prefix gives the key %s with what put wrote, not %v", key, slices.Sorted(maps.Keys(items))), nil)
	}
	if _, err := b.Put(key, first, ""); !errors.Is(err, ErrConflict) {
		return fail(1, "put of a key that exists, with if_none_match, is a conflict", unexpected(err))
	}

	old := etag
	if etag, err = b.Put(key, second, old); err != nil {
		return fail(2, "put with the etag", err)
	}
	got, gotTag, err = b.Get(key)
	switch {
	case err != nil:
		return fail(2, "get after put with the etag", err)
	case !bytes.Equal(got, second) || gotTag != etag:
		return fail(2, "get after put with the etag gives what put wrote, with its new etag", nil)
	}
	if _, err := b.Put(key, stale, old); !errors.Is(err, ErrConflict) {
		return fail(2, "put with an old etag is a conflict", unexpected(err))
	}
	if err := b.Delete(key, old); !errors.Is(err, ErrConflict) {
		return fail(2, "delete with an old etag is a conflict", unexpected(err))
	}

	if err := b.Delete(key, etag); err != nil {
		return fail(3, "delete with the etag", err)
	}
	made = false
	if _, _, err := b.Get(key); !errors.Is(err, ErrNotExist) {
		return fail(3, "get after delete is not found", unexpected(err))
	}

	if token, err = b.Lock(lock, h); err != nil {
		return fail(4, "lock", err)
	}
	before := token
	_, err = b.Lock(lock, h)
	var le *LockedError
	if !errors.As(err, &le) || le.Token != token || le.Holder.Run != h.Run {
		return fail(4, "a second lock is locked, with the holder and token of the first", unexpected(err))
	}
	// A plan sees the locks that are held in what it listed.
	keys, err = b.List(dir)
	switch {
	case err != nil:
		return fail(4, "list", err)
	case !slices.Contains(keys, lock):
		return fail(4, fmt.Sprintf("list of the prefix gives the key %s of the lock that is held, not %v", lock, keys), nil)
	}
	if scans {
		items, err := scan(dir)
		switch {
		case err != nil:
			return fail(4, "scan", err)
		case items[lock] == nil:
			return fail(4, fmt.Sprintf("scan of the prefix gives the key %s of the lock that is held, not %v", lock, slices.Sorted(maps.Keys(items))), nil)
		}
	}

	if err := b.Unlock(lock, "probe-"+newToken()); !errors.Is(err, ErrNotHolder) {
		return fail(5, "unlock with another token is a conflict", unexpected(err))
	}
	if err := b.Unlock(lock, token); err != nil {
		return fail(5, "unlock with the token", err)
	}
	token = ""
	if token, err = b.Lock(lock, h); err != nil {
		return fail(5, "lock after unlock", err)
	}
	if token == before {
		// An unlock that read the token of the first lock would let this
		// one go.
		return fail(5, "a lock after unlock has a token of its own, not the one of the lock before, even with the same holder", nil)
	}
	if err := b.Unlock(lock, token); err != nil {
		return fail(5, "unlock with the token", err)
	}
	token = ""

	keys, err = b.List(dir)
	switch {
	case err != nil:
		return fail(6, "list", err)
	case len(keys) > 0:
		return fail(6, fmt.Sprintf("list after the probe is empty, not %v", keys), nil)
	}
	if scans {
		items, err := scan(dir)
		switch {
		case err != nil:
			return fail(6, "scan", err)
		case len(items) > 0:
			return fail(6, fmt.Sprintf("scan after the probe is empty, not %v", slices.Sorted(maps.Keys(items))), nil)
		}
	}
	return nil
}
