package engine

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/state"
)

// dataKey is the data key of the stack, which encrypts its sensitive values
// and keys the hashes of them in digests: opened once a run, and made the
// first time a value needs it, plan included, so the digests of a plan and
// of up agree.
func (e *engine) dataKey() ([]byte, error) {
	e.keyOnce.Do(func() { e.key, e.keyErr = e.openKey() })
	return e.key, e.keyErr
}

func (e *engine) openKey() ([]byte, error) {
	m, _, err := e.readMeta()
	if err != nil {
		return nil, err
	}
	if m != nil && m.Key != nil {
		key, err := e.unwrapKey(m.Key)
		if err == nil && !e.writes {
			if diff := e.keysDiff(m.Key); diff != "" {
				e.warn("", "encryption_keys differs from what wraps the data key in %s (%s); the next plan, up, down, or task run wraps it again",
					state.MetaKey(e.p.StackName), diff)
			}
		}
		return key, err
	}
	token, err := e.lockMeta()
	if err != nil {
		return nil, err
	}
	defer func() { _ = e.store.UnlockMeta(token) }()
	// Another run may have made it while this one waited, and a run that
	// writes its first record makes _meta without a key.
	for range 2 {
		m, etag, err := e.readMeta()
		if err != nil {
			return nil, err
		}
		if m != nil && m.Key != nil {
			return e.unwrapKey(m.Key)
		}
		if m == nil {
			m = &state.Meta{Version: state.MetaVersion, Project: e.p.Name}
		}
		key := make([]byte, state.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if m.Key, err = e.wrapKey(key, nil); err != nil {
			return nil, err
		}
		if err := e.writeKey(m, etag, key); errors.Is(err, state.ErrConflict) {
			continue
		} else if err != nil {
			return nil, err
		}
		// Only the record and OPSTRUSS_LOG=debug say so, as Pulumi makes the
		// encryptedkey of a stack without a word: nothing is asked of the
		// operator, unlike a new age key of this machine, which is said with
		// its backup.
		e.emit(Event{Type: "diagnostic", Level: "debug", Message: fmt.Sprintf("made the data key of stack %s in %s, %s",
			e.p.StackName, state.MetaKey(e.p.StackName), wrappedFor(m.Key))})
		return key, nil
	}
	return nil, fmt.Errorf("%s changed while the data key was made; run again", state.MetaKey(e.p.StackName))
}

// syncKey wraps the data key again, at the start of a run that writes
// state, for the envelopes whose keys differ from encryption_keys. A
// machine that cannot open the data key only warns, since the run may not
// need it; one whose own keys would not open the result stops the run.
func (e *engine) syncKey() error {
	e.writes = true
	if e.p.EncryptionKeys == nil {
		return nil
	}
	meta := state.MetaKey(e.p.StackName)
	m, _, err := e.readMeta()
	if err != nil || m == nil || m.Key == nil || e.keysDiff(m.Key) == "" {
		return err
	}
	key, err := e.dataKey()
	if err != nil {
		e.warn("", "encryption_keys differs from what wraps the data key in %s, which this run cannot wrap again: %v", meta, err)
		return nil
	}
	token, err := e.lockMeta()
	if err != nil {
		return err
	}
	defer func() { _ = e.store.UnlockMeta(token) }()
	for range 2 {
		m, etag, err := e.readMeta()
		if err != nil || m == nil || m.Key == nil {
			return err
		}
		diff := e.keysDiff(m.Key)
		if diff == "" {
			return nil
		}
		if m.Key, err = e.wrapKey(key, m.Key); err != nil {
			return err
		}
		if err := e.writeKey(m, etag, key); errors.Is(err, state.ErrConflict) {
			continue
		} else if err != nil {
			return err
		}
		e.say("diagnostic", "", "wrapped the data key of stack %s again in %s (%s), %s", e.p.StackName, meta, diff, wrappedFor(m.Key))
		return nil
	}
	return fmt.Errorf("%s changed while the data key was wrapped again; run again", meta)
}

// readMeta reads _meta with its etag, or nil when the stack has none yet.
func (e *engine) readMeta() (*state.Meta, string, error) {
	data, etag, err := e.store.MetaData()
	if err != nil || data == nil {
		return nil, etag, err
	}
	m, err := state.DecodeMeta(data)
	return m, etag, err
}

// writeKey writes _meta with its data key, which this machine must open
// first, as restic opens the key of a new repository, so a list of keys
// without one of this machine, such as one with a typo, never locks it
// out.
func (e *engine) writeKey(m *state.Meta, etag string, key []byte) error {
	opened, _, err := e.openEnvelopes(m.Key)
	var closed *closedError
	switch {
	case err == nil && !bytes.Equal(opened, key):
		err = errors.New("it opens to another key")
	case errors.As(err, &closed):
		err = errors.New(closed.reason())
	}
	if err != nil {
		hint := "add the age key of this machine to encryption_keys"
		if mine, made, kerr := e.machineKey(); kerr == nil {
			hint = fmt.Sprintf("add the age key of this machine, %s (in %s%s), to encryption_keys", mine.Recipient(), mine.From, madeNow(made))
		}
		return fmt.Errorf("encryption_keys would leave the data key of stack %s closed to this machine, so %s is not written: %v; %s",
			e.p.StackName, state.MetaKey(e.p.StackName), err, hint)
	}
	data, err := state.EncodeMeta(m)
	if err != nil {
		return err
	}
	_, err = e.store.ReplaceMeta(data, etag)
	return err
}

// wrapKey puts the data key in the envelopes for encryption_keys, or for
// the age key of this machine without it. An envelope of old whose keys
// are the same is kept, so sops runs only when the keys for it change.
func (e *engine) wrapKey(key []byte, old *state.DataKey) (*state.DataKey, error) {
	ages, sops, err := e.encryptionKeys()
	if err != nil {
		return nil, err
	}
	dk := &state.DataKey{}
	switch {
	case old != nil && old.Age != nil && sameSet(old.Age.To, ages):
		dk.Age = old.Age
	case len(ages) > 0:
		data, err := secret.SealAge(key, ages)
		if err != nil {
			return nil, err
		}
		dk.Age = &state.AgeEnvelope{To: ages, Data: data}
	}
	switch {
	case old != nil && old.Sops != nil && sameSet(sopsNames(old.Sops.To), sopsNames(sops)):
		dk.Sops = old.Sops
	case len(sops) > 0:
		data, err := e.sops().SealSops(key, sops)
		if err != nil {
			return nil, err
		}
		dk.Sops = &state.SopsEnvelope{To: sops, Data: data}
	}
	return dk, nil
}

// encryptionKeys are what the data key is wrapped for: the age recipients
// and the keys for sops in encryption_keys, or, without it, the age key of
// this machine.
func (e *engine) encryptionKeys() ([]string, []map[string]string, error) {
	if e.p.EncryptionKeys == nil {
		mine, _, err := e.machineKey()
		if err != nil {
			return nil, nil, err
		}
		return []string{mine.Recipient()}, nil, nil
	}
	var ages []string
	var sops []map[string]string
	for _, k := range e.p.EncryptionKeys {
		if k.Age != "" {
			ages = append(ages, k.Age)
		} else {
			sops = append(sops, k.Sops)
		}
	}
	return ages, sops, nil
}

// machineKey is the age key of this machine, made when it has none, which
// the run says once.
func (e *engine) machineKey() (secret.Identity, bool, error) {
	mine, made, err := secret.EnsureMachineKey(e.opts.LookupEnv, time.Now())
	if made {
		e.say("diagnostic", "", "made the age key %s of this machine in %s; back it up: with the _meta of each stack, it is the only way to open their sensitive values",
			mine.Recipient(), mine.From)
	}
	return mine, made, err
}

// unwrapKey opens the data key in its envelopes, and when no key of this
// machine does, says what wraps it and the age key of this machine.
func (e *engine) unwrapKey(dk *state.DataKey) ([]byte, error) {
	key, _, err := e.openEnvelopes(dk)
	if err == nil {
		return key, nil
	}
	var closed *closedError
	if !errors.As(err, &closed) {
		return nil, err
	}
	msg := fmt.Sprintf("the data key of stack %s does not open on this machine: %s", e.p.StackName, closed.Error())
	mine, made, kerr := e.machineKey()
	switch {
	case kerr != nil:
		return nil, fmt.Errorf("%s; this machine has no age key: %w", msg, kerr)
	case slices.ContainsFunc(e.p.EncryptionKeys, func(k *model.EncryptionKey) bool { return k.Age == mine.Recipient() }):
		return nil, fmt.Errorf("%s; encryption_keys has the age key of this machine, %s (in %s): a plan, up, or down on a machine that opens the data key wraps it again for it",
			msg, mine.Recipient(), mine.From)
	}
	return nil, fmt.Errorf("%s; the age key of this machine is %s (in %s%s): add it to encryption_keys, and run plan, up, or down on a machine that opens the data key, which wraps it again",
		msg, mine.Recipient(), mine.From, madeNow(made))
}

// closedError is a data key that no key of this machine opens: what wraps
// it, and what sops said.
type closedError struct {
	dk   *state.DataKey
	meta string
	sops error
}

func (c *closedError) Error() string { return c.meta + " has it " + c.wrapped() }

// reason is the error of a data key that is not written yet.
func (c *closedError) reason() string { return "no key of this machine opens it " + c.wrapped() }

func (c *closedError) wrapped() string {
	msg := wrappedFor(c.dk)
	if c.sops != nil {
		msg += " (" + c.sops.Error() + ")"
	}
	return msg
}

// openEnvelopes opens the data key with the age keys of this machine, and
// with sops only when they cannot, and says which did.
func (e *engine) openEnvelopes(dk *state.DataKey) ([]byte, string, error) {
	meta := state.MetaKey(e.p.StackName)
	if dk.Age == nil && dk.Sops == nil {
		return nil, "", fmt.Errorf("%s has a data key in no envelope", meta)
	}
	if dk.Age != nil {
		ids, err := secret.Identities(e.opts.LookupEnv)
		if err != nil {
			return nil, "", err
		}
		key, id, err := secret.OpenAge(dk.Age.Data, ids, 1<<10)
		switch {
		case err == nil:
			return checkSize(key, meta, "with the age key in "+id.From)
		case !errors.Is(err, secret.ErrNoIdentity):
			return nil, "", fmt.Errorf("the age envelope in %s: %w", meta, err)
		}
	}
	var sopsErr error
	if dk.Sops != nil {
		key, err := e.sops().OpenSops(dk.Sops.Data)
		if err == nil {
			return checkSize(key, meta, "with sops")
		}
		sopsErr = err
	}
	return nil, "", &closedError{dk: dk, meta: meta, sops: sopsErr}
}

func checkSize(key []byte, meta, route string) ([]byte, string, error) {
	if len(key) != state.KeySize {
		return nil, "", fmt.Errorf("the data key in %s is not %d bytes", meta, state.KeySize)
	}
	return key, route, nil
}

func (e *engine) sops() *secret.Sops {
	return &secret.Sops{Dir: e.p.Dir, Environ: e.opts.Environ, Stack: e.p.StackName}
}

// keysDiff says how encryption_keys differs from the keys that wrap the
// data key, or "" when they agree or encryption_keys is not set.
func (e *engine) keysDiff(dk *state.DataKey) string {
	if e.p.EncryptionKeys == nil {
		return ""
	}
	ages, sops, _ := e.encryptionKeys()
	var had, has []string
	if dk.Age != nil {
		had = append(had, dk.Age.To...)
	}
	if dk.Sops != nil {
		had = append(had, sopsNames(dk.Sops.To)...)
	}
	has = append(append(has, ages...), sopsNames(sops)...)
	var parts []string
	for _, k := range has {
		if !slices.Contains(had, k) {
			parts = append(parts, "+"+k)
		}
	}
	for _, k := range had {
		if !slices.Contains(has, k) {
			parts = append(parts, "-"+k)
		}
	}
	return strings.Join(parts, ", ")
}

// wrappedFor says what the envelopes of the data key were wrapped for.
func wrappedFor(dk *state.DataKey) string {
	var parts []string
	if dk.Age != nil {
		parts = append(parts, "with age for "+strings.Join(dk.Age.To, ", "))
	}
	if dk.Sops != nil {
		parts = append(parts, "with sops for "+strings.Join(sopsNames(dk.Sops.To), ", "))
	}
	return strings.Join(parts, ", and ")
}

// sopsNames writes each key for sops as its kind and value: kms arn:….
func sopsNames(to []map[string]string) []string {
	var names []string
	for _, entry := range to {
		for kind, v := range entry {
			names = append(names, kind+" "+v)
		}
	}
	return names
}

func sameSet(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(s string) bool { return !slices.Contains(b, s) })
}

func madeNow(made bool) string {
	if made {
		return ", made just now"
	}
	return ""
}

// lockMeta takes the lock of _meta, waiting while another run holds it;
// writes hold it only for a moment.
func (e *engine) lockMeta() (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for delay := 5 * time.Millisecond; ; delay = min(2*delay, 200*time.Millisecond) {
		h := e.holder
		if h.Run == "" {
			// A command that reads, and starts no run.
			h = state.NewHolder(state.NewRunID(), e.host)
		}
		token, err := e.store.LockMeta(h)
		var le *state.LockedError
		if !errors.As(err, &le) || time.Now().After(deadline) {
			return token, err
		}
		time.Sleep(delay)
	}
}
