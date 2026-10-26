package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opstruss/opstruss/internal/ansi"
	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/record"
)

// renderer turns the events of one command into what it prints. With
// --json it prints each event on a line. At a terminal, each unit
// operation, task, and step that runs is a block, as the log of GitHub
// Actions shows a step: a line that starts it, the lines of its programs
// under a gutter, in their own colors, and a line with how it ended and
// the time it took; while other sources print too, each line of a program
// is led by its source, as docker compose up does. Otherwise each event is
// a line led by its source, which in GitHub Actions also groups the lines
// of each source. None of it is a contract, unlike --json.
type renderer struct {
	out, err           io.Writer
	json, terminal     bool
	github             bool // a line per event, with ::group:: around the lines of each source
	colorOut, colorErr bool

	planned map[string]map[string]string // the plan line of each address, by run, while the run goes on
	open    map[string]time.Time         // the sources between their start and their end, and when they started
	printed map[string]bool              // the sources that printed a line of a program
	labels  map[string]string            // the label of each source, as its start said
	colors  map[string]string            // the SGR color of each source's label
	recent  []string                     // the sources of the last program lines
	last    string                       // the source whose block the last line belongs to
	width   int                          // the widest label that led a line
	group   string                       // GitHub Actions: the source whose group is open
	table   *planTable                   // the plan lines of the units, until the plan is done
	shown   map[string]bool              // the sources whose block started on the terminal
	wrote   bool                         // the terminal shows something already
	tags    map[string]string            // the label that leads each line of a source while others print
	blanks  map[string]int               // the blank lines of a block that wait for a line after them
	failed  map[string]string            // the error of each unit that cannot be planned, which the table showed
	focus   string                       // the block that shows its lines as they come
	held    map[string]*heldBlock        // the lines of the other blocks, until they end
	waiting []piece                      // the blocks that ended while the focus went on, until it ends
	now     time.Time                    // the time of the event that renders
	focusAt time.Time                    // when the focus last showed a line
	ended   map[string]map[string]int    // how the operations of units ended, by operation and as the summary counts them
}

// piece is text for one writer.
type piece struct {
	w    io.Writer
	text string
}

func newRenderer(out, err io.Writer, json, terminal, github bool, colorOut, colorErr bool) *renderer {
	return &renderer{out: out, err: err, json: json, terminal: terminal && !json, github: github && !json && !terminal,
		colorOut: colorOut, colorErr: colorErr, planned: map[string]map[string]string{}, open: map[string]time.Time{},
		printed: map[string]bool{}, labels: map[string]string{}, colors: map[string]string{}, shown: map[string]bool{},
		tags: map[string]string{}, blanks: map[string]int{}, failed: map[string]string{}, ended: map[string]map[string]int{},
		held: map[string]*heldBlock{}}
}

func (r *renderer) render(ev engine.Event) []piece {
	if r.json {
		data, _ := json.Marshal(ev)
		return []piece{{r.out, string(data) + "\n"}}
	}
	if r.terminal {
		return r.block(ev)
	}
	return r.line(ev)
}

// end is what the renderer prints once the command is done.
func (r *renderer) end() []piece {
	out := append(r.flushPlan(), r.waiting...)
	r.waiting = nil
	for len(r.held) > 0 {
		// The blocks that did not end, as at an interrupt, show what they
		// held.
		r.focus = ""
		out = append(out, r.promote(r.colorOut)...)
	}
	if r.group != "" {
		r.group = ""
		out = append(out, piece{r.out, "::endgroup::\n"})
	}
	return out
}

// flushPlan prints the plan table, once the plan is done: before anything
// else of the run, and before a question at the terminal.
func (r *renderer) flushPlan() []piece {
	t := r.table
	r.table = nil
	if t == nil {
		return nil
	}
	var out []piece
	if len(t.rows) > 0 {
		s := t.print(r.colorOut)
		if r.wrote {
			s = "\n" + s
		}
		r.wrote = true
		out = append(out, piece{r.out, s})
	}
	if len(t.after) > 0 {
		out = append(append(out, t.after...), piece{r.out, "\n"})
	}
	return out
}

// writer is where ev goes: warnings and errors to stderr; and whether that
// writer shows color.
func (r *renderer) writer(ev engine.Event) (io.Writer, bool) {
	if ev.Level != "info" {
		return r.err, r.colorErr
	}
	return r.out, r.colorOut
}

// text is what a program or the engine wrote, as a terminal may show it:
// clean, and without color for a writer that shows none.
func text(s string, color bool) string {
	s = ansi.Clean(s)
	if !color {
		s = ansi.Strip(s)
	}
	return s
}

// source names where an event comes from: the address, task, and step
// that its start and end share with the lines of its programs.
func source(ev engine.Event) string { return ev.Address + "\x00" + ev.Task + "\x00" + ev.Step }

// who leads a line of the event without a terminal: its address, or its
// task and step.
func who(ev engine.Event) string {
	if ev.Address == "" || (ev.Type == "task" || ev.Type == "step") && ev.Task != "" {
		if ev.Step != "" {
			return ev.Task + ": " + ev.Step
		}
		return ev.Task
	}
	return ev.Address
}

// label names the source of an event at a terminal, by its unit without
// the stack, which is the same for the whole run.
func label(ev engine.Event) string {
	task := ev.Task
	if rest, ok := strings.CutPrefix(task, ev.Address+"/"); ev.Address != "" && ok {
		// A task that a unit runs is named after the unit, as its
		// operations are: web › status as web › up.
		task = unitName(ev.Address) + " › " + rest
	} else if strings.Contains(task, "/") {
		// A task of an instance of a component, by its address.
		task = unitName(task)
	}
	if ev.Step != "" {
		// A step without a name is named after its task: build step 1.
		task += " › " + strings.TrimPrefix(ev.Step, ev.Task+" ")
	}
	switch {
	case ev.Type == "task" || ev.Type == "step" || ev.Address == "":
		return task
	case task != "":
		return unitName(ev.Address) + " › " + task
	}
	return unitName(ev.Address)
}

// unitName is an address without its stack.
func unitName(addr string) string {
	if _, rest, ok := strings.Cut(addr, "/"); ok {
		return rest
	}
	return addr
}

// line renders ev as a line, the way output without a terminal reads.
func (r *renderer) line(ev engine.Event) []piece {
	if !lineShows(ev) || r.repeatsPlan(ev) {
		return nil
	}
	w, color := r.writer(ev)
	var b strings.Builder
	switch {
	case ev.Type == "log":
		// Each line of a message, as of a log step, is led by its source,
		// which also keeps it from reading as a workflow command.
		for _, l := range lines(ev.Message) {
			fmt.Fprintf(&b, "%s | %s\n", who(ev), l)
		}
	case ev.Type == "run":
		delete(r.planned, ev.Run)
		for _, k := range slices.Sorted(maps.Keys(ev.Outputs)) {
			fmt.Fprintf(&b, "  %s: %s\n", k, expr.Text(ev.Outputs[k]))
		}
	case who(ev) != "":
		fmt.Fprintf(&b, "%s: %s\n", who(ev), ev.Message)
	default:
		fmt.Fprintln(&b, ev.Message)
	}
	var out []piece
	if r.github {
		out = r.regroup(ev)
	}
	if s := text(b.String(), color); s != "" {
		out = append(out, piece{w, s})
	}
	return out
}

// regroup opens the group of the source of a program line, closing the
// one before; any other line closes the open group, so the lines of
// OpsTruss itself stay visible while the groups are folded. A group
// follows the lines as they come, so lines of programs that run at once
// split their groups, rather than wait for one another.
func (r *renderer) regroup(ev engine.Event) []piece {
	var out []piece
	key := ""
	if ev.Type == "log" {
		key = source(ev)
	}
	if r.group != "" && r.group != key {
		out = append(out, piece{r.out, "::endgroup::\n"})
		r.group = ""
	}
	if key != "" && r.group != key {
		name := strings.ReplaceAll(ansi.Strip(cmpLabel(r.labels[key], label(ev))), "\n", " ")
		out = append(out, piece{r.out, "::group::" + name + "\n"})
		r.group = key
	}
	return out
}

func cmpLabel(known, l string) string {
	if known != "" {
		return known
	}
	return l
}

// lineShows says whether output without a terminal prints ev. The events
// that only start a run, a unit operation, or a step, and the end of a
// step that succeeded or was cancelled, pair the lines of a group for
// --json and the terminal; a line per event leaves them out.
func lineShows(ev engine.Event) bool {
	switch {
	case ev.Status == record.Running:
		return ev.Type == "task"
	case ev.Type == "step" && ev.Status == record.Finished:
		return ev.Outcome == "failure" || ev.Outcome == "skipped"
	}
	return true
}

// repeatsPlan says whether ev is the turn of a unit that ended as the plan
// line of its run already said, such as unchanged, with nothing of that
// unit between them, which is shown once; after another line, such as
// waiting for a lock, the end shows again. The event itself stays in
// --json and the record.
func (r *renderer) repeatsPlan(ev engine.Event) bool {
	if ev.Address == "" || ev.Level != "info" {
		return false
	}
	switch ev.Type {
	case "plan":
		if r.planned[ev.Run] == nil {
			r.planned[ev.Run] = map[string]string{}
		}
		r.planned[ev.Run][ev.Address] = ev.Message
	case "unit":
		line, ok := r.planned[ev.Run][ev.Address]
		delete(r.planned[ev.Run], ev.Address)
		return ok && line == ev.Message
	}
	return false
}

// SGR codes of the terminal renderer.
const (
	bold   = "1"
	dim    = "2"
	red    = "31"
	green  = "32"
	yellow = "33"
)

// palette colors the labels of sources, one after another, as docker
// compose colors its services; green, yellow, and red say how something
// went, so no label has them.
var palette = []string{"36", "35", "34", "96", "95", "94"}

// paint wraps s in an SGR code when color is on.
func paint(color bool, code, s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// maxLabel is the widest label that leads a line; a longer one loses its
// middle.
const maxLabel = 40

// block renders ev at a terminal.
func (r *renderer) block(ev engine.Event) []piece {
	r.now = eventTime(ev)
	r.count(ev)
	if r.repeatsPlan(ev) {
		return nil
	}
	var out []piece
	if r.table != nil && !keepsTable(ev) {
		out = r.flushPlan()
	}
	more := r.blockEvent(ev)
	if len(more) > 0 {
		r.wrote = true
	}
	return append(out, more...)
}

func (r *renderer) blockEvent(ev engine.Event) []piece {
	w, color := r.writer(ev)
	key := source(ev)
	switch {
	case ev.Status == record.Running:
		if ev.Type == "run" {
			return nil
		}
		// A block shows once its programs print; the line that ends it, or
		// the plan table, says how it went.
		r.open[key] = eventTime(ev)
		r.labels[key] = label(ev)
		r.tag(ev, key)
		return nil
	case ev.Type == "log":
		var out []piece
		for _, l := range lines(ev.Message) {
			ev.Message = l
			out = append(out, r.program(ev, key, w, color)...)
		}
		return out
	case ev.Type == "run":
		delete(r.planned, ev.Run)
		r.last = ""
		var b strings.Builder
		b.WriteString(r.summary(color))
		for _, k := range slices.Sorted(maps.Keys(ev.Outputs)) {
			fmt.Fprintf(&b, "  %s: %s\n", k, text(expr.Text(ev.Outputs[k]), color))
		}
		return []piece{{w, b.String()}}
	case ev.Type == "plan":
		return r.planLine(ev, key, color)
	case ev.Type == "diagnostic" && ev.Level == "warn" && ev.Address != "" && r.table != nil:
		r.table.warn(ev)
		return nil
	case statusRow(ev):
		if r.table == nil {
			r.table = &planTable{title: "Status", byAddr: map[string]*planRow{}}
		}
		r.table.status(ev)
		return nil
	case statusNote(ev) && r.table != nil:
		r.table.note(ev)
		return nil
	case ev.Type == "task" && ev.Status == record.Finished && ev.Address != "" && ev.Task == ev.Address+"/status":
		if r.table == nil {
			r.table = &planTable{title: "Status", byAddr: map[string]*planRow{}}
		}
		r.table.byPlugin(ev)
		return r.closeBlock(key, color, piece{w, r.endLine(ev, key, color) + "\n"})
	case ev.Status == record.Finished:
		if _, started := r.open[key]; !started && interruptedBefore(ev) {
			// A unit that an interrupt kept from starting is counted in
			// the summary; a line for each says nothing more.
			return nil
		}
		return r.closeBlock(key, color, piece{w, r.endLine(ev, key, color) + "\n"})
	}
	msg := text(ev.Message, color)
	switch ev.Level {
	case "warn":
		msg = paint(color, yellow, msg)
	case "error":
		msg = paint(color, red, msg)
	}
	if k := r.owner(ev, key); k != "" && ev.Status == "" {
		// What OpsTruss says of a block stays in it: a warning or an error,
		// and dim, any other line, as that a service runs.
		if ev.Level == "info" {
			msg = paint(color, dim, msg)
		}
		r.printed[k] = true
		return r.blockLine(k, w, paint(color, dim, "│")+" "+msg+"\n", color)
	}
	if r.table != nil && ev.Address == "" && ev.Status == "" && ev.Type != "approval" {
		// A note of the run while the plan is gathered, as that this machine
		// got an age key, comes under the plan table, as terraform plan
		// prints its warnings after the plan, rather than between the
		// blocks of the plans of plugins.
		r.table.after = append(r.table.after, piece{w, msg + "\n"})
		return nil
	}
	r.last = ""
	if ev.Type == "approval" {
		msg = paint(color, bold, msg)
	}
	if l := label(ev); l != "" {
		return []piece{{w, text(l, color) + ": " + msg + "\n"}}
	}
	return []piece{{w, msg + "\n"}}
}

// header is the line that starts the block of a source.
func (r *renderer) header(key string, color bool) string {
	return paint(color, bold, paint(color, r.color(key), "▸ "+text(r.labels[key], color)))
}

// color is the color of the label of a source: one per unit, which its
// operations and the lines of its service share, and one per task, which
// its steps share.
func (r *renderer) color(key string) string {
	addr, rest, _ := strings.Cut(key, "\x00")
	task, _, _ := strings.Cut(rest, "\x00")
	switch {
	case addr != "":
		key = addr
	case task != "":
		key = "\x00" + task
	}
	if c, ok := r.colors[key]; ok {
		return c
	}
	c := palette[len(r.colors)%len(palette)]
	r.colors[key] = c
	return c
}

// program renders a line of a program: under the gutter of its block
// while its source is the only one that prints, otherwise led by its
// label.
func (r *renderer) program(ev engine.Event, key string, w io.Writer, color bool) []piece {
	if r.labels[key] == "" {
		r.labels[key] = label(ev)
		r.tag(ev, key)
	}
	msg := text(ev.Message, color)
	blank := strings.TrimSpace(ansi.Strip(msg)) == ""
	if k := r.owner(ev, key); k != "" {
		// A line of a block: blank lines that start it show nothing, and
		// those that end it neither.
		if !r.printed[k] && blank {
			return nil
		}
		r.printed[k] = true
		gutter := paint(color, dim, "│")
		if blank {
			r.blanks[k]++
			return nil
		}
		var out []piece
		for ; r.blanks[k] > 0; r.blanks[k]-- {
			out = append(out, r.blockLine(k, w, gutter+"\n", color)...)
		}
		return append(out, r.blockLine(k, w, gutter+" "+msg+"\n", color)...)
	}
	// A line outside any block, as of a service: led by its source while
	// others print too, as docker compose up does.
	if !r.printed[key] && blank {
		return nil
	}
	r.printed[key] = true
	r.recent = append(r.recent, key)
	if len(r.recent) > 8 {
		r.recent = r.recent[1:]
	}
	if r.others(key) {
		r.last = ""
		if blank {
			return nil
		}
		l := r.tags[key]
		pad := strings.Repeat(" ", r.width-utf8.RuneCountInString(l))
		return []piece{{w, paint(color, r.color(key), l) + pad + " " + paint(color, dim, "│") + " " + msg + "\n"}}
	}
	var out []piece
	if r.last != key {
		out = append(out, piece{w, r.header(key, color) + "\n"})
		r.last = key
	}
	return append(out, piece{w, paint(color, dim, "│") + " " + msg + "\n"})
}

// maxHeldBlock bounds the lines that a block holds while another shows; a
// block past it shows what it held, and holds again.
const maxHeldBlock = 1 << 20

// heldBlock is what a block holds while another one shows.
type heldBlock struct {
	pieces []piece
	bytes  int
}

// owner is the open block that ev belongs to: that of its own source, or,
// for an event of a unit without a task, such as a warning about it, the
// open operation of that unit; empty for an event outside any block.
func (r *renderer) owner(ev engine.Event, key string) string {
	if _, ok := r.open[key]; ok {
		return key
	}
	if ev.Address != "" && ev.Task == "" {
		for _, op := range []string{"up", "destroy", "plan"} {
			k := ev.Address + "\x00" + op + "\x00"
			if _, ok := r.open[k]; ok {
				return k
			}
		}
	}
	return ""
}

// blockLine shows a line of the block of k, or holds it. One block shows
// at a time, as its programs print; the others hold their lines and show
// whole once they end, so the lines of programs that run at once do not
// mix, as turbo run --log-order=grouped keeps the logs of each task
// together.
func (r *renderer) blockLine(k string, w io.Writer, line string, color bool) []piece {
	if r.focus == "" {
		r.focus = k
	}
	if r.focus == k {
		var out []piece
		if r.last != k {
			out = append(out, piece{w, r.header(k, color) + "\n"})
			r.last = k
			r.shown[k] = true
		}
		r.focusAt = r.now
		return append(out, piece{w, line})
	}
	h := r.held[k]
	if h == nil {
		h = &heldBlock{}
		r.held[k] = h
	}
	h.pieces = append(h.pieces, piece{w, line})
	h.bytes += len(line)
	if h.bytes > maxHeldBlock {
		return r.release(k, color)
	}
	return nil
}

// release shows what the block of k held, under its header.
func (r *renderer) release(k string, color bool) []piece {
	h := r.held[k]
	delete(r.held, k)
	if h == nil {
		return nil
	}
	r.last, r.shown[k] = k, true
	return append([]piece{{r.out, r.header(k, color) + "\n"}}, h.pieces...)
}

// closeBlock ends the block of k with its last line. A block that held
// its lines shows them whole, after the block that shows ends, so a block
// does not cut another; while that one is quiet for a second, at once.
// Once the block that showed ends, the next one shows.
func (r *renderer) closeBlock(k string, color bool, last piece) []piece {
	if r.focus == k {
		out := append([]piece{last}, r.waiting...)
		r.waiting = nil
		if len(out) > 1 {
			r.last = ""
		}
		r.focus = ""
		return append(out, r.promote(color)...)
	}
	block := append(r.release(k, color), last)
	if r.focus != "" && r.now.Sub(r.focusAt) < time.Second {
		r.waiting = append(r.waiting, block...)
		r.last = r.focus
		return nil
	}
	r.last = ""
	return block
}

// promote gives the terminal to the block that started first of those that
// hold lines, once the one that showed ended.
func (r *renderer) promote(color bool) []piece {
	next := ""
	for k := range r.held {
		if next == "" || r.open[k].Before(r.open[next]) {
			next = k
		}
	}
	if next == "" {
		return nil
	}
	r.focus = next
	return r.release(next, color)
}

// tag sets the label that leads the lines of a source while others
// print, as docker compose names its services: the unit alone for an
// operation of a unit, which every line of it is; the label otherwise. The
// column of these labels is as wide as the widest of the sources that
// started, so the lines of one burst line up.
func (r *renderer) tag(ev engine.Event, key string) {
	l := r.labels[key]
	if ev.Address != "" && ev.Step == "" && (ev.Task == "" || ev.Task == "plan" || ev.Task == "up" || ev.Task == "destroy") {
		l = unitName(ev.Address)
	}
	l = shorten(ansi.Strip(l), maxLabel)
	r.tags[key] = l
	r.width = max(r.width, utf8.RuneCountInString(l))
}

// count notes how an operation of a unit ended, for the summary of the
// run: done, unchanged, failed, skipped, or cancelled.
func (r *renderer) count(ev engine.Event) {
	if ev.Status != record.Finished || ev.Address == "" || (ev.Task != "up" && ev.Task != "destroy") {
		return
	}
	if ev.Type != "unit" && ev.Type != "approval" {
		return
	}
	kind := map[string]string{"success": "done", "failure": "failed", "skipped": "skipped", "cancelled": "cancelled"}[ev.Outcome]
	switch {
	case ev.Message == "unchanged" || ev.Message == "already destroy":
		kind = "unchanged"
	case interruptedBefore(ev):
		// Skipped after a unit that the interrupt stopped: the interrupt
		// stopped it too.
		kind = "cancelled"
	}
	if r.ended[ev.Task] == nil {
		r.ended[ev.Task] = map[string]int{}
	}
	r.ended[ev.Task][kind]++
	if stack, _, ok := strings.Cut(ev.Address, "/"); ok {
		r.ended["\x00stack"] = map[string]int{stack: 1}
	}
}

// summary is the line that counts how the operations of units ended, at
// the end of a run that has any, as terraform apply ends with Apply
// complete!
func (r *renderer) summary(color bool) string {
	stack := ""
	for k := range r.ended["\x00stack"] {
		stack = k
	}
	var b strings.Builder
	for _, op := range []string{"up", "destroy"} {
		counts := r.ended[op]
		if len(counts) == 0 {
			continue
		}
		var parts []string
		for _, k := range []struct{ kind, code string }{{"done", green}, {"unchanged", dim}, {"skipped", dim}, {"cancelled", yellow}, {"failed", red}} {
			if n := counts[k.kind]; n > 0 {
				parts = append(parts, paint(color, k.code, fmt.Sprintf("%d %s", n, k.kind)))
			}
		}
		title := map[string]string{"up": "Up", "destroy": "Destroy"}[op] + " of stack " + text(stack, color) + ":"
		fmt.Fprintf(&b, "\n%s %s\n", paint(color, bold, title), strings.Join(parts, ", "))
	}
	r.ended = map[string]map[string]int{}
	return b.String()
}

// others reports whether a source other than key is active: one between
// its start and its end that printed lines of its own, which leaves out a
// task around its steps, or one among the sources of the last program
// lines that has not ended, as a service, which prints outside any start
// and end.
func (r *renderer) others(key string) bool {
	for k := range r.open {
		if k != key && r.printed[k] {
			return true
		}
	}
	return slices.ContainsFunc(r.recent, func(k string) bool { return k != key })
}

// interruptedBefore reports whether ev ends an operation of a unit that
// an interrupt kept from starting: cancelled, or skipped after a unit it
// needs that was.
func interruptedBefore(ev engine.Event) bool {
	return ev.Address != "" && (ev.Task == "up" || ev.Task == "destroy") &&
		(ev.Outcome == "cancelled" || ev.Outcome == "skipped" && strings.HasSuffix(ev.Message, " is cancelled"))
}

// outcomeMark is the mark of how a block ended.
func outcomeMark(outcome string) (string, string) {
	switch outcome {
	case "failure":
		return "✗", red
	case "skipped":
		return "○", dim
	case "cancelled":
		return "■", yellow
	}
	return "✓", green
}

// endLine is the line that ends the block of a source.
func (r *renderer) endLine(ev engine.Event, key string, color bool) string {
	glyph, code := outcomeMark(ev.Outcome)
	line := paint(color, code, glyph) + " " + text(cmpLabel(r.labels[key], label(ev)), color)
	// The label names the operation of a unit already: up: done is done.
	msg := strings.TrimPrefix(ev.Message, ev.Task+": ")
	switch {
	case ev.Outcome == "failure" && msg == r.failed[ev.Address]:
		// The plan table showed the error.
		msg = "cannot be planned"
	case ev.Type == "approval":
		// The plan table showed the change; why it stopped is what is new.
		if i := strings.LastIndex(msg, "; "); i >= 0 {
			msg = msg[i+2:]
		}
	}
	if msg != ev.Outcome && (ev.Outcome != "failure" || msg != "failed") {
		line += ": " + text(msg, color)
	}
	return line + r.took(key, ev, color)
}

// planLine takes the plan line of a unit into the table, and ends the
// block of the plan of its plugin, when one showed, with the time it took.
func (r *renderer) planLine(ev engine.Event, key string, color bool) []piece {
	var out []piece
	if r.shown[key] || r.held[key] != nil {
		glyph, code := outcomeMark(ev.Outcome)
		out = r.closeBlock(key, color, piece{r.out, paint(color, code, glyph) + " " + text(r.labels[key], color) + r.took(key, ev, color) + "\n"})
	} else {
		r.took(key, ev, color)
	}
	delete(r.shown, key)
	if r.table == nil {
		r.table = &planTable{title: "Plan", byAddr: map[string]*planRow{}}
	}
	r.table.plan(ev)
	if ev.Outcome == "failure" {
		r.failed[ev.Address] = strings.TrimPrefix(ev.Message, "error: ")
	}
	return out
}

// took is the time from the start of the source of ev to ev, which ends
// it, or nothing when it did not start.
func (r *renderer) took(key string, ev engine.Event, color bool) string {
	start, ok := r.open[key]
	delete(r.open, key)
	r.recent = slices.DeleteFunc(r.recent, func(k string) bool { return k == key })
	r.last = ""
	if !ok || start.IsZero() {
		return ""
	}
	end := eventTime(ev)
	if end.IsZero() {
		return ""
	}
	return "  " + paint(color, dim, duration(end.Sub(start)))
}

func eventTime(ev engine.Event) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, ev.Time)
	return t
}

// duration writes d as 0.4s, 12.3s, or 1m4s.
func duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// lines are the lines of a message, without the line end that closes the
// last.
func lines(msg string) []string {
	return strings.Split(strings.TrimSuffix(strings.TrimSuffix(msg, "\n"), "\r"), "\n")
}

// shorten cuts the middle of s to n runes.
func shorten(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	half := (n - 1) / 2
	return string(runes[:half]) + "…" + string(runes[len(runes)-(n-1-half):])
}
