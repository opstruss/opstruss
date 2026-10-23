package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/sources"
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
	p, err := model.LoadWith(context.Background(), dir, a.stackName(), sources.Options{Locked: a.locked})
	if err != nil {
		return nil, nil, err
	}
	s, err := state.Open(state.NewLocal(filepath.Join(dir, ".opstruss", "state")), p.StackName, p.Name)
	if err != nil {
		p.Close()
	}
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
		Short: "List the addresses that have a record: orphans, pending and abandoned attempts, and tombstones too; then the keys of kv",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, s, err := a.store()
			if err != nil {
				return err
			}
			defer p.Close()
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
				n := p.NodeAt(strings.TrimPrefix(addr, p.StackName+"/"))
				e.Orphan = r.Tombstone == nil && (n == nil || (n.Component() != nil) != (r.Kind == "component"))
				entries = append(entries, e)
			}
			kv, err := s.KVInfos()
			if err != nil {
				return err
			}
			if a.json {
				return a.printJSON(map[string]any{"stack": p.StackName, "records": entries, "kv": kv})
			}
			for _, e := range entries {
				fmt.Fprintf(a.stdout, "%s\t%s\n", e.Address, e.status())
			}
			for _, k := range kv {
				fmt.Fprintf(a.stdout, "%s\t%s\n", k.Key, kvStatus(k, time.Now()))
			}
			return nil
		},
	}
}

// kvStatus is the line of state list for a key of kv.
func kvStatus(k state.KVInfo, now time.Time) string {
	parts := []string{"kv"}
	if k.Sensitive {
		parts = append(parts, "sensitive")
	}
	if t, err := time.Parse(time.RFC3339Nano, k.Expires); err == nil {
		if t.After(now) {
			parts = append(parts, "expires "+k.Expires)
		} else {
			parts = append(parts, "expired")
		}
	}
	return strings.Join(parts, ", ")
}

func (a *app) statePull() *cobra.Command {
	return &cobra.Command{
		Use:   "pull <key>",
		Short: "Print a key as stored, and its etag on stderr; key is an address without the stack, _meta, or _kv/<name>",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, s, err := a.store()
			if err != nil {
				return err
			}
			defer p.Close()
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
			p, s, err := a.store()
			if err != nil {
				return err
			}
			defer p.Close()
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
	var tool, force bool
	var ifMatch string
	cmd := &cobra.Command{
		Use:   "rm <address|_kv/<name>>",
		Short: "Forget a record without taking anything down, leaving a tombstone; on a tombstone, remove it; or remove a key of kv",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) (err error) {
			p, s, err := a.store()
			if err != nil {
				return err
			}
			defer p.Close()
			if name, ok := strings.CutPrefix(args[0], "_kv/"); ok {
				return a.rmKV(s, name, ifMatch, force)
			}
			if ifMatch != "" || force {
				return errors.New("--if-match and --force are for a key of kv, _kv/<name>")
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
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "for _kv/<name>: the etag that state pull printed")
	cmd.Flags().BoolVar(&force, "force", false, "for _kv/<name>: remove the key whatever it holds now")
	return cmd
}

// rmKV removes the key name of kv under the lock of kv, while it still has
// the etag of the pull, as consul kv delete -cas does, so a claim that a
// run left is released without losing one written since.
func (a *app) rmKV(s *state.Store, name, ifMatch string, force bool) (err error) {
	if ifMatch == "" && !force {
		return errors.New("removing a key of kv needs --if-match <etag> from state pull, or --force, so a value written since is not lost")
	}
	if err := state.CheckKVName(name); err != nil {
		return err
	}
	key, lock, _ := s.KeyOf("_kv/" + name)
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
		return fmt.Errorf("there is no _kv/%s", name)
	case err != nil:
		return err
	case !force && current != ifMatch:
		return fmt.Errorf("_kv/%s changed since the pull (etag %s, not %s); pull it again", name, current, ifMatch)
	}
	if err := s.Backend.Delete(key, current); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "_kv/%s: removed\n", name)
	return nil
}
