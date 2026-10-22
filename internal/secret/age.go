// Package secret holds what OpsTruss shares with sops: the age key that
// sops reads, and the rules of .sops.yaml that pick the keys wrapping the
// data key of each stack.
package secret

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
)

// KeyFile is the age key file that sops reads: $SOPS_AGE_KEY_FILE, or
// sops/age/keys.txt in the user's configuration directory, which sops
// takes from $XDG_CONFIG_HOME on macOS too.
func KeyFile(lookup func(string) (string, bool)) (string, error) {
	if f, _ := lookup("SOPS_AGE_KEY_FILE"); f != "" {
		return f, nil
	}
	dir, _ := lookup("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := lookup("HOME")
		if home == "" {
			return "", errors.New("$HOME is not set, so there is no age key file; set $SOPS_AGE_KEY_FILE")
		}
		dir = filepath.Join(home, ".config")
		if runtime.GOOS == "darwin" {
			dir = filepath.Join(home, "Library", "Application Support")
		}
	}
	return filepath.Join(dir, "sops", "age", "keys.txt"), nil
}

// AgeKey is the age identity whose recipient goes into .sops.yaml.
type AgeKey struct {
	Recipient string `json:"recipient"`
	File      string `json:"file,omitempty"` // the key file; empty when the identity came from $SOPS_AGE_KEY
	Created   bool   `json:"created"`
}

// FindAgeKey returns the first X25519 identity that sops reads, from
// $SOPS_AGE_KEY or the key file, with no recipient when neither has one.
func FindAgeKey(lookup func(string) (string, bool)) (AgeKey, error) {
	if v, _ := lookup("SOPS_AGE_KEY"); v != "" {
		r, err := firstRecipient(v)
		if err != nil {
			return AgeKey{}, fmt.Errorf("$SOPS_AGE_KEY: %w", err)
		}
		if r != "" {
			return AgeKey{Recipient: r}, nil
		}
	}
	file, err := KeyFile(lookup)
	if err != nil {
		return AgeKey{}, err
	}
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return AgeKey{}, err
	}
	r, err := firstRecipient(string(data))
	if err != nil {
		return AgeKey{}, fmt.Errorf("%s: %w", file, err)
	}
	return AgeKey{Recipient: r, File: file}, nil
}

// EnsureAgeKey returns the key of FindAgeKey, and adds a new one to the key
// file, the way age-keygen writes it, when sops reads none.
func EnsureAgeKey(lookup func(string) (string, bool), now time.Time) (AgeKey, error) {
	key, err := FindAgeKey(lookup)
	if err != nil || key.Recipient != "" {
		return key, err
	}
	file := key.File
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return AgeKey{}, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return AgeKey{}, err
	}
	r := id.Recipient().String()
	text := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", now.Format(time.RFC3339), r, id)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		text = "\n" + text
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return AgeKey{}, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return AgeKey{}, err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return AgeKey{}, err
	}
	if err := f.Close(); err != nil {
		return AgeKey{}, err
	}
	return AgeKey{Recipient: r, File: file, Created: true}, nil
}

// firstRecipient returns the recipient of the first X25519 identity in the
// text of a key file, or "" when it has none; identities of other kinds,
// such as plugins, are left to sops.
func firstRecipient(text string) (string, error) {
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "AGE-SECRET-KEY-1") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return "", err
		}
		return id.Recipient().String(), nil
	}
	return "", nil
}
