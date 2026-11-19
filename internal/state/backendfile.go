package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// BackendFileVersion is the version of backend.json that this OpsTruss
// writes.
const BackendFileVersion = 1

// BackendFileName is the file in the local directory of a scope,
// .opstruss/state/<scope>/, that names the backend this checkout keeps the
// scope's state in. It is not a key of the state.
const BackendFileName = "backend.json"

// BackendFile is backend.json: the plugin backend of a scope as this
// checkout last opened it, with its source and pin, its with: and pass:
// as written, so no secret, and the location it gave. No file means the
// local backend, the only one before 1.1.
type BackendFile struct {
	Version  int            `json:"version"`
	Uses     string         `json:"uses"`
	Commit   string         `json:"commit,omitempty"`
	Hash     string         `json:"hash,omitempty"`
	With     map[string]any `json:"with,omitempty"`
	Pass     []string       `json:"pass,omitempty"`
	Location string         `json:"location"`
}

func backendFilePath(root, scope string) string {
	return filepath.Join(root, scope, BackendFileName)
}

// ReadBackendFile reads backend.json of scope under root, .opstruss/state,
// or nil when there is none.
func ReadBackendFile(root, scope string) (*BackendFile, error) {
	p := backendFilePath(root, scope)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	what := filepath.Join(scope, BackendFileName)
	if err := CheckVersion(data, "version", what, BackendFileVersion); err != nil {
		return nil, err
	}
	var f BackendFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return &f, nil
}

// WriteBackendFile writes backend.json of scope under root at once.
func WriteBackendFile(root, scope string, f *BackendFile) error {
	f.Version = BackendFileVersion
	data, err := encodeJSON(f)
	if err != nil {
		return err
	}
	p := backendFilePath(root, scope)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return replace(p, data)
}
