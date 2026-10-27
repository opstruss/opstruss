package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/sources"
	"github.com/spf13/cobra"
)

type sourceRow struct {
	Scope  string `json:"scope,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Cache  string `json:"cache"`
}

func (a *app) pluginsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "plugins", Short: "List the sources of the project and their pins",
		Long: "List project sources and their pins without fetching.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.listSources(cmd.Context()) }}
	cmd.AddCommand(&cobra.Command{Use: "fetch", Short: "Fetch the pinned sources and binaries",
		Long: "Prepare all project sources and binaries; repair pinned cache entries.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return a.fetchSources(cmd, "") }})
	cmd.AddCommand(&cobra.Command{Use: "update [name]", Short: "Update the pins of all sources, or of one",
		Long: "Update all sources, or a root uses declaration and its repository/ref group.", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			name := "*"
			if len(args) > 0 {
				name = args[0]
			}
			return a.fetchSources(cmd, name)
		}})
	cmd.AddCommand(&cobra.Command{Use: "prune", Short: "Remove cache entries unused for 30 days",
		Long: "Remove shared cache entries unused for 30 days, skipping active entries.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			cache, err := sources.UserCache()
			if err != nil {
				return err
			}
			removed, err := cache.Prune(cmd.Context())
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"removed": removed})
			}
			for _, key := range removed {
				fmt.Fprintln(a.stdout, "removed", key)
			}
			if len(removed) == 0 {
				fmt.Fprintln(a.stderr, "nothing to remove: every cache entry was used in the last 30 days")
			}
			return nil
		}})
	return cmd
}

func (a *app) listSources(ctx context.Context) error {
	dir, err := a.projectDir()
	if err != nil {
		return err
	}
	lock, err := sources.ReadLock(dir)
	if err != nil {
		return err
	}
	session, err := sources.Begin(ctx, dir, sources.Options{Discovery: true})
	if err != nil {
		return err
	}
	defer session.Close()
	stacks, err := model.Stacks(dir)
	if err != nil {
		return err
	}
	rows := map[string]sourceRow{}
	for _, stack := range stacks {
		p, err := model.Declarations(dir, stack)
		if err != nil {
			return err
		}
		loaded, _ := model.LoadSession(dir, stack, session)
		if loaded != nil {
			p.Packages = loaded.Packages
		}
		for _, name := range slices.Sorted(maps.Keys(p.Uses)) {
			decl := p.Uses[name]
			row := sourceRow{Scope: stack, Name: name, Source: decl.Source, Cache: "local"}
			if strings.HasPrefix(decl.Source, "opstruss.") {
				row.Kind, row.Cache = "builtin", "builtin"
			}
			if pkg := p.Packages[name]; pkg != nil {
				if pkg.Plugin != nil {
					row.Kind = "plugin"
				} else if pkg.Component {
					row.Kind = "component"
				}
			}
			rows[name+"@"+stack] = row
		}
		for _, name := range slices.Sorted(maps.Keys(p.Units)) {
			src := p.Units[name].Uses
			if strings.HasPrefix(src, "github.com/") {
				rows["units."+name+"@"+stack] = sourceRow{Scope: stack, Name: "units." + name, Source: src, Cache: "unlocked"}
			}
		}
	}
	cache := session.Cache
	for _, key := range lock.Keys() {
		pin := lock.Sources[key]
		id, _ := sources.Parse(key)
		id.Commit, id.Hash = pin.Commit, pin.Hash
		status := "missing"
		if _, err := os.Lstat(cache.Snapshot(id)); err == nil {
			status = "present"
			_, _, l, err := cache.Git(ctx, id, false, true)
			if err != nil {
				status = "corrupt"
				if errors.Is(err, sources.ErrInUse) {
					status = "busy"
				}
			} else {
				l.Close()
			}
		}
		found := false
		for name, row := range rows {
			if row.Source == key {
				row.Commit, row.Hash, row.Cache = pin.Commit, pin.Hash, status
				rows[name] = row
				found = true
			}
		}
		if !found {
			rows[key] = sourceRow{Name: key, Source: key, Commit: pin.Commit, Hash: pin.Hash, Cache: status}
		}
	}
	for name, row := range rows {
		if id, err := sources.Parse(row.Source); err == nil && row.Commit == "" {
			row.Source, row.Cache = id.Key(), "unlocked"
			rows[name] = row
		}
	}
	// An alias shows the pin of the declaration it names.
	for name, row := range rows {
		if from, _, ok := model.Alias(row.Source); ok {
			if r, ok := rows[from+"@"+row.Scope]; ok {
				row.Commit, row.Hash, row.Cache = r.Commit, r.Hash, r.Cache
				rows[name] = row
			}
		}
	}
	var out []sourceRow
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		out = append(out, rows[name])
	}
	if a.json {
		return a.printJSON(map[string]any{"sources": out})
	}
	// A declaration that every stack makes alike is one line; the stacks
	// show only where they differ. The hash and the whole commit are in
	// --json and opstruss.lock.
	type line struct {
		row    sourceRow
		stacks []string
	}
	var lines []*line
	byKey := map[sourceRow]*line{}
	for _, row := range out {
		key := row
		key.Scope = ""
		if l := byKey[key]; l != nil {
			l.stacks = append(l.stacks, row.Scope)
			continue
		}
		l := &line{row: key, stacks: []string{row.Scope}}
		byKey[key] = l
		lines = append(lines, l)
	}
	some := false
	for _, l := range lines {
		some = some || len(l.stacks) != len(stacks) && l.stacks[0] != ""
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	header := "NAME\tKIND\tSOURCE\tCOMMIT\tSTATUS"
	if some {
		header += "\tSTACKS"
	}
	fmt.Fprintln(tw, header)
	for _, l := range lines {
		r := l.row
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s", r.Name, cmp.Or(r.Kind, "-"), r.Source, cmp.Or(shortCommit(r.Commit), "-"), r.Cache)
		if some {
			fmt.Fprintf(tw, "\t%s", strings.Join(l.stacks, ", "))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

// shortCommit is the first seven characters of a commit, as git log
// --oneline shows it.
func shortCommit(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}

func (a *app) fetchSources(cmd *cobra.Command, update string) error {
	if cmd.Flags().Changed("stack") {
		return fmt.Errorf("plugins %s covers the whole project and all stacks; omit --stack", cmd.Name())
	}
	dir, err := a.projectDir()
	if err != nil {
		return err
	}
	opts := sources.Options{Locked: a.locked, Repair: update == "", Update: update == "*"}
	if update != "" && update != "*" {
		stacks, err := model.Stacks(dir)
		if err != nil {
			return err
		}
		for _, stack := range stacks {
			p, err := model.Declarations(dir, stack)
			if err != nil {
				return err
			}
			decl := p.Uses[update]
			if decl == nil {
				continue
			}
			if from, _, ok := model.Alias(decl.Source); ok && p.Uses[from] != nil {
				decl = p.Uses[from]
			}
			id, err := sources.Parse(decl.Source)
			if err != nil {
				return fmt.Errorf("%s is not a remote source; maintain its local checkout directly", update)
			}
			if !slices.Contains(opts.UpdateGroups, id.Group()) {
				opts.UpdateGroups = append(opts.UpdateGroups, id.Group())
			}
		}
		if len(opts.UpdateGroups) == 0 {
			return fmt.Errorf("uses: declares no %s; omit the name to update direct and transitive sources", update)
		}
	}
	session, err := sources.Begin(cmd.Context(), dir, opts)
	if err != nil {
		return err
	}
	defer session.Close()
	before, err := sources.ReadLock(dir)
	if err != nil {
		return err
	}
	stacks, err := model.Stacks(dir)
	if err != nil {
		return err
	}
	for _, stack := range stacks {
		if _, err := model.LoadSession(dir, stack, session); err != nil {
			return err
		}
	}
	if err := session.Finish(); err != nil {
		return err
	}
	var rows []sourceRow
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	for _, key := range slices.Sorted(maps.Keys(session.Pins())) {
		pin, old := session.Pins()[key], before.Sources[key]
		if update != "" && old == pin {
			// Update says what it left, as go get -u does.
			if id, err := sources.Parse(key); err == nil && (update == "*" || slices.Contains(opts.UpdateGroups, id.Group())) {
				fmt.Fprintf(tw, "%s\t%s\t(unchanged)\n", key, shortCommit(pin.Commit))
			}
			continue
		}
		rows = append(rows, sourceRow{Name: key, Source: key, Commit: pin.Commit, Hash: pin.Hash, Cache: "ready"})
		switch {
		case update == "":
			fmt.Fprintf(tw, "%s\t%s\tready\n", key, shortCommit(pin.Commit))
		case old.Commit == "":
			fmt.Fprintf(tw, "%s\t%s\t(new)\n", key, shortCommit(pin.Commit))
		default:
			fmt.Fprintf(tw, "%s\t%s → %s\n", key, shortCommit(old.Commit), shortCommit(pin.Commit))
		}
	}
	if a.json {
		return a.printJSON(map[string]any{"sources": rows})
	}
	return tw.Flush()
}
