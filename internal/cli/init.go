package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/opstruss/opstruss/internal/state"
	"github.com/spf13/cobra"
)

// initResult is what init changed and the sources it made ready, as
// --json prints it.
type initResult struct {
	Wrote   []string    `json:"wrote"`
	Sources []sourceRow `json:"sources"`
}

func (a *app) initCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up the project here and fetch what it uses",
		Long: "Set up the project in this directory, adding only what is missing: opstruss.yaml, whose first line names " +
			"its schema for editors, and .opstruss/ in .gitignore. Then check every stack as validate does, and fetch " +
			"what the project uses, with the binaries for this machine, as uses fetch does, so that a fresh clone is " +
			"ready to plan. Running it again is safe. With --stack, also make the state of that stack. The age key of " +
			"this machine and the data key of a stack are made by the first command that needs them.",
		Example: `  mkdir shop && cd shop && opstruss init
  opstruss init --name shop
  git clone https://github.com/example/shop && cd shop && opstruss init`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := filepath.Abs(cmp.Or(a.dir, "."))
			if err != nil {
				return err
			}
			res := initResult{Wrote: []string{}, Sources: []sourceRow{}}
			if err := initProject(dir, name, &res); err != nil {
				return err
			}
			if err := initIgnore(dir, &res); err != nil {
				return err
			}
			if err := a.initSources(cmd.Context(), dir, &res); err != nil {
				return err
			}
			// Only an explicit --stack makes _meta, not $OPSTRUSS_STACK.
			if a.stack != "" {
				if err := initMeta(dir, a.stack, &res); err != nil {
					return err
				}
			}
			if a.json {
				return a.printJSON(res)
			}
			for _, w := range res.Wrote {
				fmt.Fprintf(a.stdout, "wrote %s\n", w)
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			for _, r := range res.Sources {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Source, shortCommit(r.Commit), r.Cache)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if slices.Contains(res.Wrote, model.FileName) {
				fmt.Fprintf(a.stdout, "next: add units or tasks to %s, then run opstruss plan\n", model.FileName)
			} else {
				fmt.Fprintln(a.stdout, "next: run opstruss plan")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the name of a new project; made from the directory's name when not given")
	return cmd
}

// initProject writes a minimal opstruss.yaml, at the level of this
// OpsTruss, when dir has none. Its first line names the schema that each
// command that reads the project writes, for editors that use the YAML
// language server.
func initProject(dir, name string, res *initResult) error {
	p := filepath.Join(dir, model.FileName)
	_, err := os.Stat(p)
	switch {
	case err == nil:
		if name != "" {
			return fmt.Errorf("--name names a new project, and %s already has %s", dir, model.FileName)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if name == "" {
		if name, err = projectName(filepath.Base(dir)); err != nil {
			return err
		}
	}
	probs, err := schema.Validate(schema.Project, map[string]any{"opstruss": model.Level, "name": name})
	if err != nil {
		return err
	}
	if len(probs) > 0 {
		return fmt.Errorf("--name %s: %s", name, probs[0].Message)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	modeline := "# yaml-language-server: $schema=" + model.EditorSchemaFile
	if _, err := fmt.Fprintf(f, "%s\nopstruss: %q\nname: %s\n", modeline, model.Level, name); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	res.Wrote = append(res.Wrote, model.FileName)
	return nil
}

// projectName makes a project name from the name of a directory: lower
// case, starting with a letter, with each run of other characters as one -.
func projectName(base string) (string, error) {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(base) {
		letter, digit := r >= 'a' && r <= 'z', r >= '0' && r <= '9'
		if !letter && !digit && r != '_' && r != '-' {
			gap = true
			continue
		}
		if b.Len() == 0 && !letter {
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteByte('-')
		}
		gap = false
		b.WriteRune(r)
	}
	name := b.String()
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-_")
	}
	if name == "" {
		return "", fmt.Errorf("cannot make a project name from %q; give one with --name", base)
	}
	return name, nil
}

// initIgnore keeps .opstruss/, the state and runs of the project's
// stacks, out of git.
func initIgnore(dir string, res *initResult) error {
	p := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for line := range strings.Lines(string(data)) {
		switch strings.TrimSpace(line) {
		case ".opstruss", ".opstruss/", "/.opstruss", "/.opstruss/":
			return nil
		}
	}
	text := ".opstruss/\n"
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		text = "\n" + text
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	res.Wrote = append(res.Wrote, ".gitignore")
	return nil
}

// initSources reads every stack as validate does, which fetches what the
// project uses, repairs the cache entries of its pins, completes
// opstruss.lock, and writes the schema that the first line of
// opstruss.yaml names.
func (a *app) initSources(ctx context.Context, dir string, res *initResult) error {
	lockFile := filepath.Join(dir, sources.LockName)
	before, err := os.ReadFile(lockFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	warns, err := model.ValidateWith(ctx, dir, sources.Options{Locked: a.locked, Repair: true})
	for _, w := range warns {
		fmt.Fprintf(a.stderr, "warning: %s\n", w)
	}
	if err != nil {
		return err
	}
	after, err := os.ReadFile(lockFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !bytes.Equal(before, after) {
		res.Wrote = append(res.Wrote, sources.LockName)
	}
	lock, err := sources.ReadLock(dir)
	if err != nil {
		return err
	}
	for _, key := range lock.Keys() {
		pin := lock.Sources[key]
		res.Sources = append(res.Sources, sourceRow{Name: key, Source: key, Commit: pin.Commit, Hash: pin.Hash, Cache: "ready"})
	}
	return nil
}

// initMeta makes the _meta of a stack, which the local backend otherwise
// makes with the stack's first record. It reads only the declarations:
// init has checked every stack, and loading one would write the editor
// schema of that stack alone over the one of every stack.
func initMeta(dir, stack string, res *initResult) error {
	p, err := model.Declarations(dir, stack)
	if err != nil {
		return err
	}
	b := state.NewLocal(filepath.Join(dir, ".opstruss", "state"))
	if _, _, err := b.Get(state.MetaKey(p.StackName)); err == nil {
		return nil
	} else if !errors.Is(err, state.ErrNotExist) {
		return err
	}
	s, err := state.Open(b, p.StackName, p.Name)
	if err != nil {
		return err
	}
	if err := s.EnsureMeta(); err != nil {
		return err
	}
	res.Wrote = append(res.Wrote, state.MetaKey(p.StackName))
	return nil
}
