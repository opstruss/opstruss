package engine

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/state"
)

// dataKey is the data key of the stack, which encrypts its sensitive values
// and keys the hashes of them in digests: opened once a run, through sops,
// and made the first time a value needs it, plan included, so the digests
// of a plan and of up agree.
func (e *engine) dataKey() ([]byte, error) {
	e.keyOnce.Do(func() { e.key, e.keyErr = e.openKey() })
	return e.key, e.keyErr
}

func (e *engine) openKey() ([]byte, error) {
	sops := &secret.Sops{Dir: e.p.Dir, Environ: e.opts.Environ, Stack: e.p.StackName}
	data, _, err := e.store.MetaData()
	if err != nil {
		return nil, err
	}
	if key, ok, err := unwrapKey(sops, data); ok || err != nil {
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
		data, etag, err := e.store.MetaData()
		if err != nil {
			return nil, err
		}
		if key, ok, err := unwrapKey(sops, data); ok || err != nil {
			return key, err
		}
		m := &state.Meta{Version: state.MetaVersion, Project: e.p.Name}
		if data != nil {
			if m, err = state.DecodeMeta(data); err != nil {
				return nil, err
			}
		}
		key := make([]byte, state.KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		plain, err := json.Marshal(map[string]any{"version": m.Version, "project": m.Project, "key": base64.StdEncoding.EncodeToString(key)})
		if err != nil {
			return nil, err
		}
		wrapped, err := sops.Wrap(plain)
		if err != nil {
			return nil, err
		}
		if err := sops.Check(wrapped); err != nil {
			return nil, err
		}
		_, err = e.store.ReplaceMeta(wrapped, etag)
		if errors.Is(err, state.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		e.say("diagnostic", "", "made the data key of stack %s, wrapped by sops in %s", e.p.StackName, state.MetaKey(e.p.StackName))
		return key, nil
	}
	return nil, fmt.Errorf("%s changed while the data key was made; run again", state.MetaKey(e.p.StackName))
}

// unwrapKey opens the data key in _meta, and reports whether _meta has one.
func unwrapKey(sops *secret.Sops, data []byte) ([]byte, bool, error) {
	if data == nil {
		return nil, false, nil
	}
	m, err := state.DecodeMeta(data)
	if err != nil || m.Key == "" {
		return nil, false, err
	}
	plain, err := sops.Unwrap(data)
	if err != nil {
		return nil, true, err
	}
	var opened struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(plain, &opened); err != nil {
		return nil, true, fmt.Errorf("the data key from sops: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(opened.Key)
	if err != nil || len(key) != state.KeySize {
		return nil, true, fmt.Errorf("the data key in %s is not %d bytes of base64", state.MetaKey(sops.Stack), state.KeySize)
	}
	return key, true, nil
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
