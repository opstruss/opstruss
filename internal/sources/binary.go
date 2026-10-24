package sources

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"

	"golang.org/x/sys/unix"
)

// Binary pins the downloaded payload, including an archive when used.
type Binary struct {
	URL    string `json:"url" yaml:"url"`
	SHA256 string `json:"sha256" yaml:"sha256"`
}

var binName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

func (b Binary) Validate() error {
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/opstruss/plugins/releases/download/") {
		return fmt.Errorf("binary URL must be an HTTPS release asset of %s", Repository)
	}
	rel := strings.TrimPrefix(u.Path, "/opstruss/plugins/releases/download/")
	if len(strings.Split(rel, "/")) != 2 || !safePath(rel) {
		return fmt.Errorf("invalid binary release asset path")
	}
	sum, err := hex.DecodeString(b.SHA256)
	if err != nil || len(sum) != 32 || strings.ToLower(b.SHA256) != b.SHA256 {
		return fmt.Errorf("binary sha256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

const binaryLimit = 512 << 20

// executable validates every archive entry, extracting only the named root file.
func executable(name string, b Binary, payload []byte) ([]byte, error) {
	if payloadHash(payload) != b.SHA256 {
		return nil, fmt.Errorf("binary payload checksum mismatch")
	}
	if !strings.HasSuffix(b.URL, ".tar.gz") {
		return payload, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(io.LimitReader(gz, binaryLimit+1))
	seen := map[string]bool{}
	var exe []byte
	var size int64
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if count >= 4096 || h.Size < 0 || h.Size > binaryLimit || size+h.Size > binaryLimit {
			return nil, fmt.Errorf("binary archive exceeds 4096 entries or 512 MiB limit")
		}
		size += h.Size
		if !safePath(h.Name) || h.Name == "." || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Mode&0o6000 != 0 {
			return nil, fmt.Errorf("unsafe or duplicate binary archive entry %q", h.Name)
		}
		seen[h.Name] = true
		if h.Name == name {
			if h.Mode&0o111 == 0 {
				return nil, fmt.Errorf("archive %s is not executable", name)
			}
			exe, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
		}
	}
	if exe == nil {
		return nil, fmt.Errorf("archive must contain executable %s at its root", name)
	}
	return exe, nil
}

func download(ctx context.Context, b Binary) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || req.URL.Scheme != "https" || req.URL.User != nil {
			return fmt.Errorf("unsafe binary redirect")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("binary download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binary download returned HTTP %d", resp.StatusCode)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, binaryLimit+1))
	if err != nil {
		return nil, fmt.Errorf("binary download interrupted")
	}
	if len(payload) > binaryLimit {
		return nil, fmt.Errorf("binary download exceeds 512 MiB limit")
	}
	return payload, nil
}

// Binary prepares an isolated executable directory and holds its lease.
func (c *Cache) Binary(ctx context.Context, name string, b Binary, repair, readOnly bool) (string, *Lease, error) {
	if !binName.MatchString(name) || path.Base(name) != name {
		return "", nil, fmt.Errorf("invalid binary name %q", name)
	}
	if err := b.Validate(); err != nil {
		return "", nil, err
	}
retry:
	key := "bin-" + cacheKey(Platform()+"/"+name+"/"+b.SHA256+"/"+fmt.Sprint(strings.HasSuffix(b.URL, ".tar.gz")))
	dir := c.entry(key)
	verify := func() error {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm() != 0o555 {
			return fmt.Errorf("unsafe binary cache directory")
		}
		info, err = os.Lstat(filepath.Join(dir, "exec"))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm() != 0o555 {
			return fmt.Errorf("unsafe executable cache directory")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 2 {
			return fmt.Errorf("unexpected binary cache entries")
		}
		for _, p := range []string{filepath.Join(dir, "payload"), filepath.Join(dir, "exec", name)} {
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() || st.Size() > binaryLimit || st.Mode().Perm()&0o222 != 0 {
				return fmt.Errorf("unsafe binary cache file")
			}
			if p == filepath.Join(dir, "exec", name) && st.Mode().Perm() != 0o555 {
				return fmt.Errorf("binary cache executable mode mismatch")
			}
		}
		execs, err := os.ReadDir(filepath.Join(dir, "exec"))
		if err != nil {
			return err
		}
		if len(execs) != 1 {
			return fmt.Errorf("unexpected executable cache entries")
		}
		payload, err := os.ReadFile(filepath.Join(dir, "payload"))
		if err != nil {
			return err
		}
		expected, err := executable(name, b, payload)
		if err != nil {
			return err
		}
		actual, err := os.ReadFile(filepath.Join(dir, "exec", name))
		if err != nil {
			return err
		}
		if !bytes.Equal(expected, actual) {
			return fmt.Errorf("binary executable checksum mismatch")
		}
		return nil
	}
	if readOnly {
		if _, err := os.Lstat(dir); err != nil {
			return "", nil, fmt.Errorf("%s: binary unavailable; run opstruss plugins fetch first", name)
		}
	}
	l, err := c.lock(ctx, key, false, !readOnly)
	if err != nil {
		return "", nil, err
	}
	_, statErr := os.Lstat(dir)
	if statErr == nil {
		err = verify()
		if err == nil {
			if !readOnly {
				err = c.touch(key, nil)
			}
			if err == nil {
				return filepath.Join(dir, "exec"), l, nil
			}
		}
		if !repair {
			l.Close()
			return "", nil, fmt.Errorf("%s: binary cache integrity: %w; run opstruss plugins fetch to repair", name, err)
		}
	}
	l.Close()
	if readOnly {
		return "", nil, fmt.Errorf("%s: binary unavailable; run opstruss plugins fetch first", name)
	}
	l, err = c.lock(ctx, key, true, false)
	if err != nil {
		if errors.Is(err, ErrInUse) && errors.Is(statErr, os.ErrNotExist) {
			goto retry
		}
		return "", nil, err
	}
	defer func() {
		if l != nil {
			l.Close()
		}
	}()
	if verify() != nil {
		tmp, workCtx, cleanup, err := c.temporary(ctx)
		if err != nil {
			return "", nil, err
		}
		defer cleanup()
		payload, err := download(workCtx, b)
		if err != nil {
			return "", nil, err
		}
		exe, err := executable(name, b, payload)
		if err != nil {
			return "", nil, err
		}
		tree := filepath.Join(tmp, "bin")
		if err := os.MkdirAll(filepath.Join(tree, "exec"), 0o700); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(tree, "payload"), payload, 0o444); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(filepath.Join(tree, "payload"), 0o444); err != nil {
			return "", nil, err
		}
		if err := os.WriteFile(filepath.Join(tree, "exec", name), exe, 0o555); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(filepath.Join(tree, "exec", name), 0o555); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(filepath.Join(tree, "exec"), 0o555); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(tree, 0o555); err != nil {
			return "", nil, err
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
			return "", nil, err
		}
		if err := removeEntry(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", nil, err
		}
		failpoint.Hit("binary-before-publish")
		if err := os.Chmod(tree, 0o700); err != nil {
			return "", nil, err
		}
		if err := os.Rename(tree, dir); err != nil {
			return "", nil, err
		}
		if err := os.Chmod(dir, 0o555); err != nil {
			return "", nil, err
		}
		if err := c.touch(key, nil); err != nil {
			return "", nil, err
		}
		failpoint.Hit("binary-after-publish")
	}
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_SH); err != nil {
		return "", nil, err
	}
	out := l
	l = nil
	return filepath.Join(dir, "exec"), out, nil
}
