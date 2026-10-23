package secret

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MetaName is the name OpsTruss gives sops for the _meta of a stack, which
// sops needs for what comes on stdin.
func MetaName(stack string) string { return stack + "/_meta" }

// sopsFlags are the flags of sops for the keys of encryption_keys, by
// their names in the creation rules of .sops.yaml.
var sopsFlags = map[string]string{
	"kms":                  "--kms",
	"gcp_kms":              "--gcp-kms",
	"azure_keyvault":       "--azure-kv",
	"hc_vault_transit_uri": "--hc-vault-transit",
	"pgp":                  "--pgp",
}

// Sops runs the sops program: for the files of secrets from the project
// root, where sops finds .sops.yaml, and for the sops envelope of the data
// key of a stack, with the keys of encryption_keys as flags and no
// .sops.yaml. It gets the operator's whole environment, as git does, since
// sops needs the keys and credentials that open them.
type Sops struct {
	Dir     string   // the project root
	Environ []string // the operator's environment
	Stack   string
}

// SealSops has sops encrypt the data key for the keys of to, each a
// one-key map of encryption_keys, as a JSON document whose key holds it in
// base64. The flags name the keys, and --config /dev/null keeps the rules
// of .sops.yaml, and their encrypted_regex, away from it.
func (s *Sops) SealSops(key []byte, to []map[string]string) (json.RawMessage, error) {
	plain, err := json.Marshal(map[string]string{"key": base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		return nil, err
	}
	args := []string{"--config", os.DevNull, "encrypt"}
	var kinds []string
	values := map[string][]string{}
	for _, entry := range to {
		for kind, v := range entry {
			if sopsFlags[kind] == "" {
				return nil, fmt.Errorf("sops has no flag for %s", kind)
			}
			if values[kind] == nil {
				kinds = append(kinds, kind)
			}
			values[kind] = append(values[kind], v)
		}
	}
	for _, kind := range kinds {
		args = append(args, sopsFlags[kind], strings.Join(values[kind], ","))
	}
	out, err := s.envelope(plain, "encrypt", args...)
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("sops encrypt %s gave no JSON", MetaName(s.Stack))
	}
	return out, nil
}

// OpenSops has sops decrypt what SealSops made, and returns the data key.
func (s *Sops) OpenSops(data json.RawMessage) ([]byte, error) {
	out, err := s.envelope(data, "decrypt", "--config", os.DevNull, "decrypt")
	if err != nil {
		return nil, err
	}
	var opened struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(out, &opened); err != nil {
		return nil, fmt.Errorf("the data key from sops: %w", err)
	}
	return base64.StdEncoding.DecodeString(opened.Key)
}

// envelope runs sops on the sops envelope of _meta, which goes on stdin.
func (s *Sops) envelope(stdin []byte, verb string, args ...string) ([]byte, error) {
	name := MetaName(s.Stack)
	args = append(args, "--filename-override", name, "--input-type", "json", "--output-type", "json")
	out, msg, err := s.exec(bytes.NewReader(stdin), args...)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, fmt.Errorf("sops is not in PATH, and it keeps the data key of stack %s for the keys of encryption_keys that are not age recipients; install %s or newer, for example with mise use sops", s.Stack, MinSops)
	case err != nil:
		return nil, fmt.Errorf("sops %s %s: %s", verb, name, cmp.Or(msg, err.Error()))
	}
	return out, nil
}

// treeFormats are the extensions of the files that sops reads as a tree
// of values rather than as one value of bytes.
var treeFormats = []string{".yaml", ".yml", ".json", ".env", ".ini"}

// Decrypt decrypts the sops file at path, named file in what it says, for
// a secret: a file of a format that sops reads as a tree (YAML, JSON,
// dotenv, or INI, by its extension) as JSON, and any other as its bytes.
func (s *Sops) Decrypt(path, file string) (data []byte, tree bool, err error) {
	tree = slices.Contains(treeFormats, filepath.Ext(path))
	args := []string{"decrypt"}
	if tree {
		args = append(args, "--output-type", "json")
	}
	out, msg, err := s.exec(nil, append(args, path)...)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, false, errors.New("sops is not in PATH, and it decrypts the secret; install it, for example with mise use sops")
	case err != nil:
		return nil, false, fmt.Errorf("sops decrypt %s: %s", file, cmp.Or(msg, err.Error()))
	}
	return out, tree, nil
}

// exec runs sops, and returns what it printed, and when it failed, what it
// printed to stderr.
func (s *Sops) exec(stdin io.Reader, args ...string) ([]byte, string, error) {
	cmd := exec.Command("sops", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = s.Dir, s.Environ, stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, strings.TrimSpace(stderr.String()), err
}

// MinSops is the oldest sops that OpsTruss works with: sops 3.10.0
// encrypts and decrypts what comes on stdin, which is how the sops
// envelope of _meta goes to it, and 3.9.0 brought --filename-override.
const MinSops = "3.10.0"

var sopsVersion = regexp.MustCompile(`(?m)^sops (\d+)\.(\d+)\.(\d+)`)

// SopsVersion asks the sops at path for its version, without asking the
// network for the latest one, and reports whether it is older than
// MinSops.
func SopsVersion(path string, environ []string) (version string, old bool, err error) {
	cmd := exec.Command(path, "--version", "--disable-version-check")
	cmd.Env = environ
	out, err := cmd.Output()
	m := sopsVersion.FindStringSubmatch(string(out))
	if m == nil {
		// Before 3.8.0, sops does not know --disable-version-check.
		return "", true, fmt.Errorf("%s --version gives no version it knows (%v)", path, cmp.Or(err, errors.New("no sops x.y.z line")))
	}
	version = m[1] + "." + m[2] + "." + m[3]
	min := sopsVersion.FindStringSubmatch("sops " + MinSops)
	for i := 1; i <= 3; i++ {
		a, _ := strconv.Atoi(m[i])
		b, _ := strconv.Atoi(min[i])
		if a != b {
			return version, a < b, nil
		}
	}
	return version, false, nil
}
