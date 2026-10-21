package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/opstruss/opstruss/internal/state"
)

// movingHere reports whether r is marked as one end of a move whose other
// end is peer.
func movingHere(r *state.Record, peer string) bool {
	return r != nil && r.Pending != nil && r.Pending.Op == "moved" && r.Pending.Peer == peer
}

// movePair is one record that moved: moves, from one address to another.
type movePair struct{ from, to string }

// addMoves previews the entries of moved: in the files of f, the project
// or an instance of a component, whose entries name units inside it and
// come once a pass finds the instance. Each entry moves the record at its
// address and those under it, as an instance of a component moves with
// the units inside; have holds the addresses with records as the moves
// before leave them.
func (e *engine) addMoves(f *frame) {
	for _, m := range f.in.Moved {
		from, to := f.address+"/"+m.From, f.address+"/"+m.To
		pairs := []movePair{{from, to}}
		for _, a := range slices.Sorted(maps.Keys(e.have)) {
			if rest, ok := strings.CutPrefix(a, from+"/"); ok {
				pairs = append(pairs, movePair{a, to + "/" + rest})
				delete(e.have, a)
				e.have[to+"/"+rest] = true
			}
		}
		if e.have[from] {
			delete(e.have, from)
			e.have[to] = true
		}
		for _, pr := range pairs {
			e.previewMove(pr.from, pr.to)
		}
		e.pairs = append(e.pairs, pairs...)
	}
}

// pendingMove is the mark of a move that did not finish at rec, which only
// files with that entry of moved: may finish; nothing else changes the
// address meanwhile.
func pendingMove(rec *state.Record) *state.Attempt {
	if rec != nil && rec.Pending != nil && rec.Pending.Op == "moved" {
		return rec.Pending
	}
	return nil
}

// previewMoves shows the entries of moved: in order and gives each target
// the record it starts from, so a rename plans as the unit it was, not as
// a new one. A move that cannot happen fails its target, which then never
// runs as new. The entries of components come as the pass finds their
// instances.
func (e *engine) previewMoves() {
	e.moveFrom = map[string]*state.Record{}
	e.moveFailed = map[string]error{}
	e.moving = map[string]bool{}
	e.pairs = nil
	addrs, _ := e.store.Inventory()
	e.have = map[string]bool{}
	for _, a := range addrs {
		e.have[a] = true
	}
	e.addMoves(e.top)
}

// previewMove previews moving the record at from to to.
func (e *engine) previewMove(from, to string) {
	e.moving[from] = true
	if err := e.moveFailed[from]; err != nil {
		e.moveFailed[to] = err
		return
	}
	fr, _, err := e.store.Get(from)
	var tr *state.Record
	if err == nil {
		tr, _, err = e.store.Get(to)
	}
	src := e.moveFrom[from]
	if src == nil && fr != nil && fr.Tombstone == nil {
		src = fr
	}
	switch {
	case err != nil:
	case fr != nil && fr.Tombstone != nil && fr.Tombstone.MovedTo == to:
		if movingHere(tr, from) {
			cp := *tr
			cp.Pending = nil
			e.moveFrom[to] = &cp
			e.say("plan", to, "will finish the move from %s", from)
		}
		return
	case src == nil:
		return
	case tr != nil && !movingHere(tr, from):
		err = fmt.Errorf("moved: %s has a record already, so %s cannot move there", to, from)
	case src.Pending != nil && src.Pending.Op != "moved":
		err = fmt.Errorf("moved: the %s of %s at %s did not finish; finish it with files that still have that unit, then move it", src.Pending.Op, src.Pending.Holder, from)
	default:
		cp := *src
		cp.Pending = nil
		e.moveFrom[to] = &cp
		e.say("plan", to, "will move from %s", from)
		return
	}
	e.moveFailed[to] = err
	e.fail(to, err)
}

// applyMoves runs the moves of moved: in order, before any turn, so a
// chain of entries carries a record from its first address to its last.
func (e *engine) applyMoves(ctx context.Context) {
	for _, m := range e.pairs {
		from, to := m.from, m.to
		if e.moveFailed[to] != nil {
			continue
		}
		if err := e.moveFailed[from]; err != nil {
			e.moveFailed[to] = err
			continue
		}
		if err := e.move(ctx, from, to); err != nil {
			e.moveFailed[to] = err
			e.fail(to, err)
		}
	}
}

// move moves the record at from, and the tool's state of a local unit, to
// to in stages that are each safe to repeat: a pending mark at both
// addresses, the copy, a tombstone at from, then the mark at to goes. It
// holds both locks, taken in address order so two runs never wait for
// each other, and a run interrupted at any stage is finished by the next
// run whose files have the entry.
func (e *engine) move(ctx context.Context, from, to string) error {
	first, second := from, to
	if second < first {
		first, second = second, first
	}
	t1, err := e.lock(ctx, first)
	if err != nil {
		return err
	}
	defer func() { _ = e.store.Unlock(first, t1) }()
	t2, err := e.lock(ctx, second)
	if err != nil {
		return err
	}
	defer func() { _ = e.store.Unlock(second, t2) }()
	fr, fe, err := e.store.Get(from)
	if err != nil {
		return err
	}
	tr, te, err := e.store.Get(to)
	if err != nil {
		return err
	}
	switch {
	case fr != nil && fr.Tombstone != nil && fr.Tombstone.MovedTo == to:
		if !movingHere(tr, from) {
			return nil
		}
	case fr == nil || fr.Tombstone != nil:
		return nil
	default:
		if tr != nil && !movingHere(tr, from) {
			return fmt.Errorf("moved: %s has a record already, so %s cannot move there", to, from)
		}
		if p := fr.Pending; p != nil && p.Op != "moved" {
			return fmt.Errorf("moved: the %s of %s at %s did not finish; finish it with files that still have that unit, then move it", p.Op, p.Holder, from)
		}
		mark := state.Attempt{Op: "moved", Run: e.runID, Holder: e.holder}
		if !movingHere(fr, to) {
			m := mark
			m.Peer = to
			fr.Pending = &m
			if fe, err = e.store.Put(from, fr, fe); err != nil {
				return err
			}
		}
		if tr == nil {
			m := mark
			m.Peer = from
			tr = &state.Record{ID: fr.ID, Pending: &m}
			if te, err = e.store.Put(to, tr, ""); err != nil {
				return err
			}
		}
		failpoint.Hit("moved-after-pending")
		if err := e.store.MoveTool(from, to); err != nil {
			return err
		}
		cp := *fr
		cp.Pending = tr.Pending
		if te, err = e.store.Put(to, &cp, te); err != nil {
			return err
		}
		tr = &cp
		failpoint.Hit("moved-after-copy")
		tomb := &state.Record{ID: fr.ID, Tombstone: &state.Tombstone{MovedTo: to, Run: e.runID, Time: time.Now().UTC().Format(time.RFC3339)}}
		if _, err = e.store.Put(from, tomb, fe); err != nil {
			return err
		}
		failpoint.Hit("moved-after-tombstone")
	}
	tr.Pending = nil
	if _, err := e.store.Put(to, tr, te); err != nil {
		return err
	}
	e.say("unit", to, "moved from %s", from)
	return nil
}
