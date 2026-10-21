package cli

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/schema"
	"github.com/opstruss/opstruss/internal/secret"
	"github.com/opstruss/opstruss/internal/state"
	"github.com/spf13/cobra"
)

// initResult is what init changed, as --json prints it.
type initResult struct {
	Wrote  []string       `json:"wrote"`
	AgeKey *secret.AgeKey `json:"age_key,omitempty"`
}

func (a *app) initCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up the project in this directory: opstruss.yaml, .gitignore, and the age key and .sops.yaml rule that wrap the data key of each stack",
		Long: "Set up the project in this directory, adding only what is missing: opstruss.yaml, .opstruss/ in .gitignore, " +
			"and a rule in .sops.yaml for the _meta of every stack, with an age key in the file sops reads when there is none. " +
			"With --stack, also make that stack's _meta.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir, err := filepath.Abs(cmp.Or(a.dir, "."))
			if err != nil {
				return err
			}
			res := initResult{Wrote: []string{}}
			if err := initProject(dir, name, &res); err != nil {
				return err
			}
			if err := initIgnore(dir, &res); err != nil {
				return err
			}
			if err := a.initSops(dir, &res); err != nil {
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
			switch k := res.AgeKey; {
			case k == nil:
			case k.Created:
				fmt.Fprintf(a.stdout, "made the age key %s in %s; back it up: with each stack's _meta, it is the only way to open the stack's sensitive values\n", k.Recipient, k.File)
			default:
				fmt.Fprintf(a.stdout, "the rule for _meta uses the age key %s\n", k.Recipient)
			}
			if len(res.Wrote) == 0 {
				fmt.Fprintln(a.stdout, "nothing to do: the project is set up")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the name of a new project; made from the directory's name when not given")
	return cmd
}

// initProject writes a minimal opstruss.yaml, at the level of this
// OpsTruss, when dir has none.
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
	if _, err := fmt.Fprintf(f, "opstruss: %q\nname: %s\n", model.Level, name); err != nil {
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

// initSops adds the rule for _meta to .sops.yaml when the _meta of a stack
// matches no rule, with the operator's age key, made when there is none.
func (a *app) initSops(dir string, res *initResult) error {
	stacks, err := model.Stacks(dir)
	if err != nil {
		return err
	}
	missing := false
	for _, s := range stacks {
		ok, err := secret.HasRule(dir, secret.MetaName(s))
		if err != nil {
			return err
		}
		missing = missing || !ok
	}
	if !missing {
		return nil
	}
	key, err := secret.EnsureAgeKey(a.lookupEnv, time.Now())
	if err != nil {
		return err
	}
	if err := secret.AddMetaRule(dir, key.Recipient); err != nil {
		return err
	}
	res.Wrote = append(res.Wrote, secret.SopsConfig)
	res.AgeKey = &key
	return nil
}

// initMeta makes the _meta of a stack, which the local backend otherwise
// makes with the stack's first record.
func initMeta(dir, stack string, res *initResult) error {
	p, err := model.Load(dir, stack)
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
