// Package secret holds the keys and secrets of OpsTruss: the age key of
// this machine, the envelopes that keep the data key of each stack, the
// sources of secrets, and the redaction of sensitive values.
package secret

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Identity is an age X25519 identity of this machine, with where it was
// found: a variable, or a key file.
type Identity struct {
	*age.X25519Identity
	From string
}

// Recipient is the public key of the identity, which goes into
// encryption_keys.
func (id Identity) Recipient() string { return id.X25519Identity.Recipient().String() }

// configDir is the user's configuration directory as sops finds it:
// $XDG_CONFIG_HOME, also on macOS, or else ~/.config, or ~/Library/Application
// Support on macOS.
func configDir(lookup func(string) (string, bool)) (string, error) {
	if dir, _ := lookup("XDG_CONFIG_HOME"); dir != "" {
		return dir, nil
	}
	home, _ := lookup("HOME")
	if home == "" {
		return "", errors.New("$HOME is not set")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support"), nil
	}
	return filepath.Join(home, ".config"), nil
}

// KeyFile is the file where OpsTruss keeps the age key that it makes for
// this machine: $OPSTRUSS_AGE_KEY_FILE, or opstruss/age/keys.txt in the
// user's configuration directory.
func KeyFile(lookup func(string) (string, bool)) (string, error) {
	return keyFile(lookup, "OPSTRUSS_AGE_KEY_FILE", "opstruss")
}

func keyFile(lookup func(string) (string, bool), variable, dir string) (string, error) {
	if f, _ := lookup(variable); f != "" {
		return f, nil
	}
	conf, err := configDir(lookup)
	if err != nil {
		return "", fmt.Errorf("%w, so there is no age key file; set $%s", err, variable)
	}
	return filepath.Join(conf, dir, "age", "keys.txt"), nil
}

// Identities returns the age X25519 identities of this machine, in the
// order OpsTruss tries them: $OPSTRUSS_AGE_KEY, then $OPSTRUSS_AGE_KEY_FILE
// or KeyFile, then, as sops finds its keys, $SOPS_AGE_KEY, then
// $SOPS_AGE_KEY_FILE or sops/age/keys.txt in the user's configuration
// directory. Identities of other kinds, such as those of plugins, are
// left out.
func Identities(lookup func(string) (string, bool)) ([]Identity, error) {
	var ids []Identity
	for _, tool := range []string{"OPSTRUSS", "SOPS"} {
		if v, _ := lookup(tool + "_AGE_KEY"); v != "" {
			found, err := parseIdentities(v, "$"+tool+"_AGE_KEY")
			if err != nil {
				return nil, err
			}
			ids = append(ids, found...)
		}
		file, err := keyFile(lookup, tool+"_AGE_KEY_FILE", strings.ToLower(tool))
		if err != nil {
			continue // no HOME: no file to read
		}
		data, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		found, err := parseIdentities(string(data), file)
		if err != nil {
			return nil, err
		}
		ids = append(ids, found...)
	}
	return ids, nil
}

// parseIdentities reads the X25519 identities in the text of a key file,
// which also holds comments and identities of other kinds.
func parseIdentities(text, from string) ([]Identity, error) {
	var ids []Identity
	n := 0
	for line := range strings.Lines(text) {
		n++
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "AGE-SECRET-KEY-1") {
			continue
		}
		id, err := age.ParseX25519Identity(line)
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", from, n, err)
		}
		ids = append(ids, Identity{X25519Identity: id, From: from})
	}
	return ids, nil
}

// EnsureMachineKey returns the age key of this machine, the first of
// Identities, and makes one in KeyFile, the way age-keygen writes it, when
// there is none; made says it did.
func EnsureMachineKey(lookup func(string) (string, bool), now time.Time) (key Identity, made bool, err error) {
	ids, err := Identities(lookup)
	if err != nil {
		return Identity{}, false, err
	}
	if len(ids) > 0 {
		return ids[0], false, nil
	}
	file, err := KeyFile(lookup)
	if err != nil {
		return Identity{}, false, err
	}
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Identity{}, false, err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Identity{}, false, err
	}
	text := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n", now.Format(time.RFC3339), id.Recipient(), id)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		text = "\n" + text
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return Identity{}, false, err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Identity{}, false, err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return Identity{}, false, err
	}
	if err := f.Close(); err != nil {
		return Identity{}, false, err
	}
	return Identity{X25519Identity: id, From: file}, true, nil
}

// CheckRecipient checks an entry of encryption_keys that names an age
// recipient, without printing it, since a secret key can be pasted where
// its public key belongs.
func CheckRecipient(s string) error {
	upper := strings.ToUpper(s)
	switch {
	case strings.HasPrefix(upper, "AGE-SECRET-KEY-") || strings.HasPrefix(upper, "AGE-PLUGIN-"):
		return errors.New("this is an age secret key, which never goes into a file of the project; write its public key, age1…, and keep the secret key in the key file of its machine")
	case strings.HasPrefix(s, "ssh-"):
		return errors.New("ssh recipients are not supported yet; use an age X25519 recipient, age1… from age-keygen")
	}
	if _, err := age.ParseX25519Recipient(s); err == nil {
		return nil
	}
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "age1") {
		return errors.New("this is not an age recipient, age1… from age-keygen or the public key line of a key file")
	}
	// Bech32 ends the name of the kind at the last 1: age for X25519, and
	// age1<name> for the recipients of plugins, such as age1yubikey1….
	if lower[:strings.LastIndex(lower, "1")] != "age" {
		return errors.New("recipients of age plugins and post-quantum ones are not supported yet; use an age X25519 recipient, age1… from age-keygen")
	}
	return errors.New("this is not a whole age recipient: its length or checksum is wrong")
}

// SealAge encrypts the data key for the age recipients to, ASCII-armored.
func SealAge(key []byte, to []string) (string, error) {
	rs := make([]age.Recipient, len(to))
	for i, s := range to {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return "", fmt.Errorf("age recipient %s: %w", s, err)
		}
		rs[i] = r
	}
	var b strings.Builder
	a := armor.NewWriter(&b)
	w, err := age.Encrypt(a, rs...)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(key); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	if err := a.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ErrNoIdentity is an age envelope that none of the identities opens.
var ErrNoIdentity = errors.New("no age key of this machine opens it")

// OpenAge decrypts what SealAge made with the first of ids that opens it,
// and returns which one did.
func OpenAge(data string, ids []Identity) ([]byte, Identity, error) {
	for _, id := range ids {
		r, err := age.Decrypt(armor.NewReader(strings.NewReader(data)), id.X25519Identity)
		var none *age.NoIdentityMatchError
		if errors.As(err, &none) {
			continue
		}
		if err != nil {
			return nil, Identity{}, err
		}
		key, err := io.ReadAll(io.LimitReader(r, 1<<10))
		if err != nil {
			return nil, Identity{}, err
		}
		return key, id, nil
	}
	return nil, Identity{}, ErrNoIdentity
}
