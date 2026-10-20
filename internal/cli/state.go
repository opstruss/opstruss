package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
	"github.com/spf13/cobra"
)

func (a *app) stateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Read and repair the state of a stack without touching its storage",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(a.stateList(), a.statePull(), a.statePush(), a.stateRm())
	return cmd
}

// store opens the state of the project's stack in its local backend.
func (a *app) store() (*model.Project, *state.Store, error) {
	dir, err := a.projectDir()
	if err != nil {
		return nil, nil, err
	}
	p, err := model.Load(dir, a.stackName())
	if err != nil {
		return nil, nil, err
	}
	s, err := state.Open(state.NewLocal(filepath.Join(dir, ".opstruss", "state")), p.StackName, p.Name)
	return p, s, err
}

// holder describes this command as the holder of a lock.
func (a *app) holder() (state.Holder, error) {
	host, err := state.HostID()
	if err != nil {
		return state.Holder{}, err
	}
	return state.NewHolder(state.NewRunID(), host), nil
}

type listEntry struct {
	Address   string           `json:"address"`
	ID        string           `json:"id"`
	Kind      string           `json:"kind,omitempty"`
	Orphan    bool             `json:"orphan,omitempty"`
	Pending   string           `json:"pending,omitempty"`
	Abandoned int              `json:"abandoned,omitempty"`
	Tombstone *state.Tombstone `json:"tombstone,omitempty"`
}

func (e listEntry) status() string {
	var parts []string
	switch {
	case e.Tombstone != nil && e.Tombstone.MovedTo != "":
		parts = append(parts, "moved to "+e.Tombstone.MovedTo)
	case e.Tombstone != nil:
		parts = append(parts, "forgotten")
	case e.Orphan:
		parts = append(parts, "orphan: no longer in the files")
	}
	if e.Pending != "" {
		parts = append(parts, "pending "+e.Pending)
	}
	if e.Abandoned > 0 {
		parts = append(parts, fmt.Sprintf("%d abandoned", e.Abandoned))
	}
	if len(parts) == 0 {
		return "ok"
	}
	return strings.Join(parts, ", ")
}

func (a *app) stateList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the addresses that have a record: orphans, pending and abandoned attempts, and tombstones too",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, s, err := a.store()
			if err != nil {
				return err
			}
			addrs, err := s.Inventory()
			if err != nil {
				return err
			}
			entries := []listEntry{}
			for _, addr := range addrs {
				r, _, err := s.Get(addr)
				if err != nil {
					return err
				}
				e := listEntry{Address: addr, ID: r.ID, Kind: r.Kind, Abandoned: len(r.Abandoned), Tombstone: r.Tombstone}
				if r.Pending != nil {
					e.Pending = r.Pending.Op
				}
				e.Orphan = r.Tombstone == nil && p.Units[unitName(p.StackName, addr)] == nil
				entries = append(entries, e)
			}
			if a.json {
				return a.printJSON(map[string]any{"stack": p.StackName, "records": entries})
			}
			for _, e := range entries {
				fmt.Fprintf(a.stdout, "%s\t%s\n", e.Address, e.status())
			}
			return nil
		},
	}
}

// unitName returns the unit at the start of an address in stack.
func unitName(stack, addr string) string {
	rest := strings.TrimPrefix(addr, stack+"/")
	name, _, _ := strings.Cut(rest, "/")
	name, _, _ = strings.Cut(name, "[")
	return name
}

func (a *app) statePull() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <key>",
		Short: "Print a key as stored, and its etag on stderr; key is an address without the stack, _meta, or _kv/<name>",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_, s, err := a.store()
			if err != nil {
				return err
			}
			key, _, _ := s.KeyOf(args[0])
			data, etag, err := s.Backend.Get(key)
			if errors.Is(err, state.ErrNotExist) {
				return fmt.Errorf("there is no %s in stack %s", args[0], s.Stack)
			}
			if err != nil {
				return err
			}
			if _, err := a.stdout.Write(data); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "etag %s\n", etag)
			return nil
		},
	}
}

func (a *app) statePush() *cobra.Command {
	var ifMatch string
	var force bool
	cmd := &cobra.Command{
		Use:   "push <key>",
		Short: "Replace a key with stdin, under its lock, when it still has the etag of the pull",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			if ifMatch == "" && !force {
				return errors.New("push needs --if-match <etag> from state pull, or --force, so a stale copy is refused")
			}
			_, s, err := a.store()
			if err != nil {
				return err
			}
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			key, lock, address := s.KeyOf(args[0])
			switch args[0] {
			case "_meta":
				if _, err := state.DecodeMeta(data); err != nil {
					return err
				}
			default:
				if err := s.EnsureMeta(); err != nil {
					return err
				}
				if address != "" {
					r, err := state.DecodeRecord(data, address)
					if err != nil {
						return err
					}
					if r.Address != address {
						return fmt.Errorf("the record is for %s, not %s", r.Address, address)
					}
				}
			}
			h, err := a.holder()
			if err != nil {
				return err
			}
			token, err := s.Backend.Lock(lock, h)
			if err != nil {
				return err
			}
			defer func() {
				if uerr := s.Backend.Unlock(lock, token); err == nil {
					err = uerr
				}
			}()
			_, current, err := s.Backend.Get(key)
			switch {
			case errors.Is(err, state.ErrNotExist):
				if !force {
					return fmt.Errorf("there is no %s yet; creating it needs --force", args[0])
				}
			case err != nil:
				return err
			case !force && current != ifMatch:
				return fmt.Errorf("%s changed since the pull (etag %s, not %s); pull it again", args[0], current, ifMatch)
			}
			etag, err := s.Backend.Put(key, data, current)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "etag %s\n", etag)
			return nil
		},
	}
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "the etag that state pull printed")
	cmd.Flags().BoolVar(&force, "force", false, "replace the key whatever it holds now")
	return cmd
}

func (a *app) stateRm() *cobra.Command {
	var tool bool
	cmd := &cobra.Command{
		Use:   "rm <address>",
		Short: "Forget a record without taking anything down, leaving a tombstone; on a tombstone, remove it",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (err error) {
			_, s, err := a.store()
			if err != nil {
				return err
			}
			address := s.Stack + "/" + args[0]
			h, err := a.holder()
			if err != nil {
				return err
			}
			token, err := s.Lock(address, h)
			if err != nil {
				return err
			}
			defer func() {
				if uerr := s.Unlock(address, token); err == nil {
					err = uerr
				}
			}()
			if tool {
				if err := s.ForgetTool(address, h.Run); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "%s: the tool's state moved to _forgotten; the record stays\n", address)
				return nil
			}
			msg, err := s.Forget(address, h.Run)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s: %s\n", address, msg)
			return nil
		},
	}
	cmd.Flags().BoolVar(&tool, "tool", false, "only move the tool's own state to _forgotten, and keep the record")
	return cmd
}
