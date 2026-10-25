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
}

// piece is text for one writer.
type piece struct {
	w    io.Writer
	text string
}

func newRenderer(out, err io.Writer, json, terminal, github bool, colorOut, colorErr bool) *renderer {
	return &renderer{out: out, err: err, json: json, terminal: terminal && !json, github: github && !json && !terminal,
		colorOut: colorOut, colorErr: colorErr, planned: map[string]map[string]string{}, open: map[string]time.Time{},
		printed: map[string]bool{}, labels: map[string]string{}, colors: map[string]string{}}
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
	if r.group == "" {
		return nil
	}
	r.group = ""
	return []piece{{r.out, "::endgroup::\n"}}
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

// label names the source of an event at a terminal.
func label(ev engine.Event) string {
	task := ev.Task
	if ev.Step != "" {
		task += " › " + ev.Step
	}
	switch {
	case ev.Type == "task" || ev.Type == "step" || ev.Address == "":
		return task
	case task != "":
		return ev.Address + " › " + task
	}
	return ev.Address
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
		fmt.Fprintf(&b, "%s | %s\n", who(ev), ev.Message)
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
// compose colors its services.
var palette = []string{"36", "35", "34", "33", "32", "96", "95", "94"}

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
	if r.repeatsPlan(ev) {
		return nil
	}
	w, color := r.writer(ev)
	key := source(ev)
	switch {
	case ev.Status == record.Running:
		if ev.Type == "run" {
			return nil
		}
		r.open[key] = eventTime(ev)
		r.labels[key] = label(ev)
		r.last = key
		return []piece{{w, r.header(key, color) + "\n"}}
	case ev.Type == "log":
		return r.program(ev, key, w, color)
	case ev.Type == "run":
		delete(r.planned, ev.Run)
		r.last = ""
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(ev.Outputs)) {
			fmt.Fprintf(&b, "  %s: %s\n", k, text(expr.Text(ev.Outputs[k]), color))
		}
		return []piece{{w, b.String()}}
	case ev.Type == "plan":
		return []piece{{w, r.planLine(ev, key, color) + "\n"}}
	case ev.Status == record.Finished:
		return []piece{{w, r.endLine(ev, key, color) + "\n"}}
	}
	r.last = ""
	msg := text(ev.Message, color)
	switch ev.Level {
	case "warn":
		msg = paint(color, yellow, msg)
	case "error":
		msg = paint(color, red, msg)
	}
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

// color is the color of the label of a source.
func (r *renderer) color(key string) string {
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
	}
	r.printed[key] = true
	r.recent = append(r.recent, key)
	if len(r.recent) > 8 {
		r.recent = r.recent[1:]
	}
	msg := text(ev.Message, color)
	if r.others(key) {
		l := shorten(text(r.labels[key], false), maxLabel)
		r.width = max(r.width, utf8.RuneCountInString(l))
		r.last = ""
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

// endLine is the line that ends the block of a source.
func (r *renderer) endLine(ev engine.Event, key string, color bool) string {
	glyph, code := "✓", green
	switch ev.Outcome {
	case "failure":
		glyph, code = "✗", red
	case "skipped":
		glyph, code = "○", dim
	case "cancelled":
		glyph, code = "■", yellow
	}
	line := paint(color, code, glyph) + " " + text(cmpLabel(r.labels[key], label(ev)), color)
	// The label names the operation of a unit already: up: done is done.
	msg := strings.TrimPrefix(ev.Message, ev.Task+": ")
	if msg != ev.Outcome && (ev.Outcome != "failure" || msg != "failed") {
		line += ": " + text(msg, color)
	}
	return line + r.took(key, ev, color)
}

// planLine is the plan line of a unit, marked by its changes as terraform
// plan marks them, which ends the plan of its plugin when one ran.
func (r *renderer) planLine(ev engine.Event, key string, color bool) string {
	glyph, code := planMark(ev)
	return paint(color, code, glyph) + " " + text(ev.Address, color) + "  " + text(ev.Message, color) + r.took(key, ev, color)
}

// planMark is the mark of a plan line: + for what is made or run, ~ for
// what changes, ± for what is replaced, - for what goes, = for nothing,
// and ✗ for a unit that cannot be planned.
func planMark(ev engine.Event) (string, string) {
	switch ev.Outcome {
	case "failure":
		return "✗", red
	case "skipped":
		return "○", dim
	}
	rank := map[string]int{"run": 1, "create": 1, "update": 2, "replace": 3, "delete": 4}
	top := 0
	if changes, ok := ev.Plan["changes"].([]any); ok {
		for _, c := range changes {
			if m, ok := c.(map[string]any); ok {
				a, _ := m["action"].(string)
				top = max(top, rank[a])
			}
		}
	}
	switch top {
	case 1:
		return "+", green
	case 2:
		return "~", yellow
	case 3:
		return "±", yellow
	case 4:
		return "-", red
	}
	return "=", dim
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

// shorten cuts the middle of s to n runes.
func shorten(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	half := (n - 1) / 2
	return string(runes[:half]) + "…" + string(runes[len(runes)-(n-1-half):])
}
