package state

import (
	"bytes"
	"errors"
	"fmt"
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
// stack reads, since initialize proves none of it: a conditional put and
// delete, a read after a write, locks with their holder and token, and
// list. What a failed step leaves it removes as it can.
func Probe(b Backend, h Holder) (err error) {
	dir := "_probe/" + NewID() + "/"
	key, lock := dir+"data", dir+"state.lock"
	data := []byte("{\"version\": 1}\n")
	other := "probe-" + newToken() // an etag and a token that no key has
	var etag, token string
	defer func() {
		if err != nil {
			if etag != "" {
				_ = b.Delete(key, etag)
			}
			if token != "" {
				_ = b.Unlock(lock, token)
			}
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

	if etag, err = b.Put(key, data, ""); err != nil {
		return fail(1, "put of a new key", err)
	}
	got, gotTag, err := b.Get(key)
	switch {
	case err != nil:
		return fail(1, "get after put", err)
	case !bytes.Equal(got, data) || gotTag != etag:
		return fail(1, "get after put gives what put wrote, with its etag", nil)
	}
	if _, err := b.Put(key, data, ""); !errors.Is(err, ErrConflict) {
		return fail(1, "put of a key that exists, with if_none_match, is a conflict", unexpected(err))
	}

	if _, err := b.Put(key, []byte("{\"version\": 2}\n"), other); !errors.Is(err, ErrConflict) {
		return fail(2, "put with another etag is a conflict", unexpected(err))
	}
	if err := b.Delete(key, other); !errors.Is(err, ErrConflict) {
		return fail(2, "delete with another etag is a conflict", unexpected(err))
	}

	if err := b.Delete(key, etag); err != nil {
		return fail(3, "delete with the etag", err)
	}
	etag = ""
	if _, _, err := b.Get(key); !errors.Is(err, ErrNotExist) {
		return fail(3, "get after delete is not found", unexpected(err))
	}

	if token, err = b.Lock(lock, h); err != nil {
		return fail(4, "lock", err)
	}
	_, err = b.Lock(lock, h)
	var le *LockedError
	if !errors.As(err, &le) || le.Token != token || le.Holder.Run != h.Run {
		return fail(4, "a second lock is locked, with the holder and token of the first", unexpected(err))
	}

	if err := b.Unlock(lock, other); !errors.Is(err, ErrNotHolder) {
		return fail(5, "unlock with another token is a conflict", unexpected(err))
	}
	if err := b.Unlock(lock, token); err != nil {
		return fail(5, "unlock with the token", err)
	}
	token = ""
	if token, err = b.Lock(lock, h); err != nil {
		return fail(5, "lock after unlock", err)
	}
	if err := b.Unlock(lock, token); err != nil {
		return fail(5, "unlock with the token", err)
	}
	token = ""

	keys, err := b.List(dir)
	switch {
	case err != nil:
		return fail(6, "list", err)
	case len(keys) > 0:
		return fail(6, fmt.Sprintf("list after the probe is empty, not %v", keys), nil)
	}
	return nil
}
