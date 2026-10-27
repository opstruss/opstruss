package sources

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/opstruss/opstruss/internal/proc"
)

type gitLeaseKey struct{}

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if file, ok := ctx.Value(gitLeaseKey{}).(*os.File); ok {
		cmd.ExtraFiles = []*os.File{file}
	}
	for _, v := range os.Environ() {
		k, _, _ := strings.Cut(v, "=")
		if !strings.HasPrefix(k, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=", "GIT_CONFIG_KEY_1=protocol.allow", "GIT_CONFIG_VALUE_1=never", "GIT_CONFIG_KEY_2=protocol.https.allow", "GIT_CONFIG_VALUE_2=always")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return proc.Group(cmd.Process.Pid).Signal(syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	b, err := gitCommand(ctx, dir, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w", args[0], err)
	}
	return b, nil
}

func initGit(ctx context.Context, dir string) error {
	_, err := gitOutput(ctx, dir, "init", "--bare", "--template=", "repo.git")
	return err
}

// ResolveRef reads a remote ref once. main, dev and latest are ordinary refs.
func (c *Cache) ResolveRef(ctx context.Context, id Identity) (string, error) {
	if id.Repository != Repository {
		return "", fmt.Errorf("unsupported repository")
	}
	if commitPattern.MatchString(id.Ref) {
		return id.Ref, nil
	}
	tmp, workCtx, cleanup, err := c.temporary(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := initGit(workCtx, tmp); err != nil {
		return "", err
	}
	b, err := gitOutput(workCtx, filepath.Join(tmp, "repo.git"), "ls-remote", "--exit-code", "https://"+id.Repository+".git", "refs/heads/"+id.Ref, "refs/tags/"+id.Ref, "refs/tags/"+id.Ref+"^{}")
	// --exit-code makes git exit with 2 when the repository has no such
	// ref, as for a ref written wrong.
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == 2 {
		return "", fmt.Errorf("%s has no branch or tag %s", id.Repository, id.Ref)
	}
	if err != nil {
		return "", fmt.Errorf("%s: cannot resolve ref: %w", id.Key(), err)
	}
	var commit, peeled string
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.HasSuffix(fields[1], "^{}") {
			peeled = fields[0]
		} else if commit == "" {
			commit = fields[0]
		}
	}
	if peeled != "" {
		commit = peeled
	}
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("%s: ref did not resolve to a commit", id.Key())
	}
	return commit, nil
}

func materialize(ctx context.Context, id Identity, tmp string) ([]treeEntry, error) {
	if !commitPattern.MatchString(id.Commit) || id.Repository != Repository {
		return nil, fmt.Errorf("invalid source commit")
	}
	if err := initGit(ctx, tmp); err != nil {
		return nil, err
	}
	repo := filepath.Join(tmp, "repo.git")
	if _, err := gitOutput(ctx, repo, "fetch", "--no-tags", "--depth=1", "https://"+id.Repository+".git", id.Commit); err != nil {
		return nil, fmt.Errorf("%s: cannot fetch pinned commit: %w", id.Key(), err)
	}
	b, err := gitOutput(ctx, repo, "ls-tree", "-rz", "--full-tree", id.Commit)
	if err != nil {
		return nil, err
	}
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return nil, err
	}
	cmd := gitCommand(ctx, repo, "cat-file", "--batch")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, err
	}
	defer func() {
		_ = in.Close()
		// A rejected blob may still fill stdout; stop the group before waiting.
		_ = cmd.Cancel()
		_ = cmd.Wait()
	}()
	reader := bufio.NewReader(out)
	var entries []treeEntry
	var total int64
	for _, line := range bytes.Split(b, []byte{0}) {
		if len(entries) >= 200000 {
			return nil, fmt.Errorf("Git tree exceeds 200000 entries")
		}
		if len(line) == 0 {
			continue
		}
		header, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || !safePath(string(name)) || string(name) == "." {
			return nil, fmt.Errorf("invalid Git tree entry")
		}
		mode, obj := fields[0], fields[2]
		if mode != "100644" && mode != "100755" && mode != "120000" && mode != "160000" {
			return nil, fmt.Errorf("unsupported Git mode %s", mode)
		}
		var payload []byte
		if mode == "160000" {
			payload = []byte(obj)
		} else {
			if _, err := io.WriteString(in, obj+"\n"); err != nil {
				return nil, err
			}
			h, err := reader.ReadString('\n')
			if err != nil {
				return nil, err
			}
			parts := strings.Fields(h)
			if len(parts) != 3 || parts[1] != "blob" {
				return nil, fmt.Errorf("invalid Git blob")
			}
			n, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil || n < 0 || n > 512<<20 {
				return nil, fmt.Errorf("Git blob exceeds 512 MiB limit")
			}
			total += n
			if total > 2<<30 {
				return nil, fmt.Errorf("Git tree exceeds 2 GiB limit")
			}
			payload = make([]byte, n)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return nil, err
			}
			if ch, err := reader.ReadByte(); err != nil || ch != '\n' {
				return nil, fmt.Errorf("invalid Git blob framing")
			}
		}
		p := filepath.Join(tree, filepath.FromSlash(string(name)))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		// No symlink is created until every regular file has been written.
		switch mode {
		case "160000":
			err = os.Mkdir(p, 0o700)
		case "120000":
			if filepath.IsAbs(string(payload)) {
				return nil, fmt.Errorf("absolute source symlink %s", name)
			}
		default:
			perm := os.FileMode(0o444)
			if mode == "100755" {
				perm = 0o555
			}
			err = os.WriteFile(p, payload, perm)
			if err == nil {
				err = os.Chmod(p, perm)
			}
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, treeEntry{string(name), mode, payloadHash(payload)})
	}
	for _, line := range bytes.Split(b, []byte{0}) {
		header, name, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || fields[0] != "120000" {
			continue
		}
		payload, err := gitOutput(ctx, repo, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, err
		}
		if err := os.Symlink(string(payload), filepath.Join(tree, filepath.FromSlash(string(name)))); err != nil {
			return nil, err
		}
	}
	err = filepath.WalkDir(tree, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o555)
		}
		return nil
	})
	if err == nil {
		err = verifyTree(tree, entries, treeHash(entries))
	}
	return entries, err
}
