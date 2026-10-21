package builtin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/opstruss/opstruss/internal/plugin"
)

// fromDir is p from dir, when p is relative.
func fromDir(dir string, p any) string {
	s := p.(string)
	if filepath.IsAbs(s) {
		return s
	}
	return filepath.Join(dir, s)
}

// read reads a file of at most the size of outputs, in a format.
func read(dir string, in map[string]any) *Result {
	f, err := os.Open(fromDir(dir, in["path"]))
	if err != nil {
		return failed(err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, plugin.MaxOutputs+1))
	if err != nil {
		return failed(err)
	}
	if len(data) > plugin.MaxOutputs {
		return failed(fmt.Errorf("%s is larger than %d bytes", in["path"], plugin.MaxOutputs))
	}
	v, err := plugin.Decode(data, in["format"].(string))
	if err != nil {
		return failed(err)
	}
	return &Result{Outputs: map[string]any{"content": v}}
}

// write writes content in a format, only when the file holds something
// else or has another mode, through a temporary file and a rename, so a
// reader never sees half of it. The directory must exist.
func write(dir string, in map[string]any) *Result {
	p := fromDir(dir, in["path"])
	data, err := plugin.Encode(in["content"], in["format"].(string))
	if err != nil {
		return failed(fmt.Errorf("content: %w", err))
	}
	mode := fs.FileMode(0o644)
	if m, ok := in["mode"].(string); ok {
		n, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			return failed(fmt.Errorf("mode: %w", err))
		}
		mode = fs.FileMode(n)
	}
	out := map[string]any{"path": p}
	old, err := os.ReadFile(p)
	switch {
	case err == nil:
		info, err := os.Stat(p)
		if err != nil {
			return failed(err)
		}
		_, modeSet := in["mode"].(string)
		if bytes.Equal(old, data) && (!modeSet || info.Mode().Perm() == mode) {
			return &Result{Outputs: out}
		}
		if !modeSet {
			mode = info.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return failed(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-"+filepath.Base(p)+"-*")
	if err != nil {
		return failed(err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		return failed(err)
	}
	return &Result{Outputs: out, Changed: true}
}
