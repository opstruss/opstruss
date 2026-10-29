package cli

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opstruss/opstruss/internal/engine"
)

// planTable gathers the plan lines of the units at a terminal until the
// plan is done, then prints them as one table, as pulumi preview does:
// the units in a column, their changes first, then those left out, those
// only warned of, and those that cannot be planned, each hint on a line of
// its own, and a line that counts them. The status of the units prints the
// same way, as docker compose ps lists services.
type planTable struct {
	title  string // Plan or Status
	rows   []*planRow
	byAddr map[string]*planRow
	after  []piece // the notes of the run that came while the table gathered
}

type planRow struct {
	addr, mark, code, text string
	hint                   string   // what to do, after the first "; " of an error
	warns                  []string // warnings about the unit
	notes                  []string // what else a status says, as the outputs of the unit
	order                  int      // planned, left out, warned of only, cannot be planned
	kind                   string   // what the summary counts it as
	planned                bool
}

func (t *planTable) row(addr string) *planRow {
	if row := t.byAddr[addr]; row != nil {
		return row
	}
	row := &planRow{addr: addr}
	t.byAddr[addr] = row
	t.rows = append(t.rows, row)
	return row
}

// plan takes the plan line of a unit.
func (t *planTable) plan(ev engine.Event) {
	row := t.row(ev.Address)
	row.planned = true
	row.mark, row.code = planMark(ev)
	switch ev.Outcome {
	case "failure":
		row.order, row.kind = 3, "cannot be planned"
		row.text, row.hint = splitHint(strings.TrimPrefix(ev.Message, "error: "))
	case "skipped":
		row.order, row.kind, row.text = 1, "left out", ev.Message
	default:
		row.order, row.kind, row.text = 0, planKind(ev.Message), planText(ev)
		if ev.Plan["approval"] == "auto" && row.kind != "unchanged" {
			// It runs without a question, while others wait for theirs.
			row.text += " · approval: auto"
		}
	}
}

// planText is the plan line of a unit as the table shows it: without what
// its mark says already, will run and may run, and with its changes by
// action, create a, b rather than create a, create b.
func planText(ev engine.Event) string {
	msg := ev.Message
	if changes, ok := ev.Plan["changes"].([]any); ok {
		if d := describe(changes, false); d != "" {
			msg = strings.Replace(msg, "plan: "+d, "plan: "+describe(changes, true), 1)
		}
	}
	for _, p := range [][2]string{{"will run: ", ""}, {"may run: ", ""}, {"will be destroyed", "destroy"},
		{"will be forgotten", "forget"}, {"will destroy", "destroy"}} {
		if after, ok := strings.CutPrefix(msg, p[0]); ok {
			msg = p[1] + after
			break
		}
	}
	msg = strings.TrimPrefix(strings.TrimPrefix(msg, "plan: "), "plan ")
	// The table names units without their stack.
	if stack, _, ok := strings.Cut(ev.Address, "/"); ok {
		msg = strings.NewReplacer("skipped: "+stack+"/", "skipped: ").Replace(msg)
		msg = shortAfter(msg, stack)
	}
	return strings.NewReplacer("; plan: ", " · ", "; plan after ", " · after ", "; then ", " · then ").Replace(msg)
}

// shortAfter writes the units that a plan line waits for, after site,
// infra (env.URL) or after 3 instances of shard, without stack.
func shortAfter(msg, stack string) string {
	head, rest, ok := strings.Cut(msg, "plan after ")
	switch {
	case ok:
		head += "plan "
	case strings.HasPrefix(msg, "after "):
		head, rest = "", strings.TrimPrefix(msg, "after ")
	default:
		return msg
	}
	units, reads, paren := strings.Cut(rest, " (")
	names := strings.Split(units, ", ")
	for i, n := range names {
		count, unit, ok := strings.Cut(n, " instances of ")
		if !ok {
			count, unit = "", n
		} else {
			count += " instances of "
		}
		names[i] = count + strings.TrimPrefix(unit, stack+"/")
	}
	msg = head + "after " + strings.Join(names, ", ")
	if paren {
		msg += " (" + reads
	}
	return msg
}

// describe writes the changes of a plan as the plan line of the engine
// writes them, action and resource, or with each action once.
func describe(changes []any, grouped bool) string {
	var parts []string
	last := ""
	for _, c := range changes {
		m, _ := c.(map[string]any)
		action, _ := m["action"].(string)
		res, _ := m["resource"].(string)
		switch {
		case grouped && action == last && res != "":
			parts[len(parts)-1] += ", " + res
			continue
		case res != "":
			parts = append(parts, action+" "+res)
		default:
			parts = append(parts, action)
		}
		last = action
	}
	sep := ", "
	if grouped {
		sep = " · "
	}
	return strings.Join(parts, sep)
}

// status takes the status of a unit: up, pending, absent, or the state of
// its service.
func (t *planTable) status(ev engine.Event) {
	row := t.row(ev.Address)
	row.planned = true
	state, _ := ev.Outputs["state"].(string)
	row.mark, row.code = stateMark(state)
	row.kind, row.text = state, ev.Message
	run, _ := ev.Outputs["run"].(string)
	if updated, err := time.Parse(time.RFC3339, fmt.Sprint(ev.Outputs["updated"])); state == "up" && run != "" && err == nil {
		row.text = "up · " + updated.UTC().Format("2006-01-02 15:04 UTC") + " · run " + run
	}
}

// byPlugin takes the end of the status task of the plugin of a unit, which
// printed in its block above; the row of the unit is what its record says,
// and a status that failed is a warning under it.
func (t *planTable) byPlugin(ev engine.Event) {
	if ev.Outcome == "failure" {
		t.warn(engine.Event{Address: ev.Address, Message: "the status of its plugin failed, above"})
	}
}

// note takes what else the status of a unit says.
func (t *planTable) note(ev engine.Event) {
	row := t.row(ev.Address)
	row.notes = append(row.notes, ev.Message)
}

// stateMark is the mark of the state of a unit.
func stateMark(state string) (string, string) {
	switch state {
	case "up", "running":
		return "●", green
	case "pending", "starting", "restarting":
		return "!", yellow
	case "failed":
		return "✗", red
	}
	return "○", dim
}

// statusRow reports whether ev is the status of a unit, and statusNote
// whether it says more of one.
func statusRow(ev engine.Event) bool {
	return ev.Type == "unit" && ev.Status == "" && ev.Outputs["state"] != nil
}

func statusNote(ev engine.Event) bool {
	return ev.Type == "unit" && ev.Status == "" && strings.HasPrefix(ev.Message, "outputs: ")
}

// warn takes a warning about a unit.
func (t *planTable) warn(ev engine.Event) {
	row := t.row(ev.Address)
	row.warns = append(row.warns, ev.Message)
}

// splitHint parts a message as OpsTruss writes them, what happened; what to
// do, into the two.
func splitHint(s string) (string, string) {
	if what, do, ok := strings.Cut(s, "; "); ok {
		return what, do
	}
	return s, ""
}

// planKind is what a plan line counts as in the summary.
func planKind(msg string) string {
	switch {
	case strings.HasPrefix(msg, "may run"):
		return "may run"
	case strings.HasPrefix(msg, "will be destroyed"):
		return "to destroy"
	case strings.HasPrefix(msg, "will be forgotten"):
		return "to forget"
	case strings.HasPrefix(msg, "unchanged"):
		return "unchanged"
	}
	return "to run"
}

// summaryKinds are the counts of the summary in their order, with their
// colors.
var summaryKinds = []struct{ kind, code string }{
	{"to run", green}, {"may run", yellow}, {"to destroy", red}, {"to forget", red}, {"unchanged", dim},
	{"left out", dim}, {"orphan", yellow}, {"warning", yellow}, {"cannot be planned", red},
}

// print writes the table, for a writer that shows color or not.
func (t *planTable) print(color bool) string {
	rows := slices.Clone(t.rows)
	for _, row := range rows {
		if row.planned || len(row.warns) == 0 {
			continue
		}
		// A unit that only a warning names, such as an orphan, is a row
		// of its own.
		row.mark, row.code, row.order = "!", yellow, 2
		row.text, row.hint = splitHint(row.warns[0])
		row.warns = row.warns[1:]
		row.kind = "warning"
		if strings.HasPrefix(row.text, "orphan") {
			row.kind = "orphan"
		}
	}
	slices.SortStableFunc(rows, func(a, b *planRow) int { return cmp.Compare(a.order, b.order) })
	counts := map[string]int{}
	shown := rows[:0:0]
	var kinds []struct{ kind, code string }
	for _, row := range rows {
		if counts[row.kind] == 0 {
			kinds = append(kinds, struct{ kind, code string }{row.kind, row.code})
		}
		counts[row.kind]++
		// The summary counts what does not change, as pulumi preview
		// leaves the same resources out.
		if row.kind != "unchanged" || len(row.warns) > 0 {
			shown = append(shown, row)
		}
	}
	width := 0
	for _, row := range shown {
		width = max(width, utf8.RuneCountInString(unitName(row.addr)))
	}
	under := strings.Repeat(" ", width+6)
	var b strings.Builder
	for _, row := range shown {
		name := unitName(row.addr)
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(name))
		fmt.Fprintf(&b, "  %s %s%s  %s\n", paint(color, row.code, row.mark), text(name, color), pad, text(row.text, color))
		if row.hint != "" {
			b.WriteString(under + paint(color, dim, "→ "+text(row.hint, color)) + "\n")
		}
		for _, w := range row.warns {
			b.WriteString(under + paint(color, yellow, "! "+text(w, color)) + "\n")
		}
		for _, n := range row.notes {
			b.WriteString(under + paint(color, dim, text(n, color)) + "\n")
		}
	}
	var parts []string
	order := summaryKinds
	if t.title == "Status" {
		// The states of the units count from up to absent, then any other
		// as the table shows it.
		order = nil
		for _, k := range []string{"up", "running", "starting", "restarting", "pending", "failed", "stopped", "absent"} {
			for _, s := range kinds {
				if s.kind == k {
					order = append(order, s)
				}
			}
		}
		for _, s := range kinds {
			if !slices.ContainsFunc(order, func(o struct{ kind, code string }) bool { return o.kind == s.kind }) {
				order = append(order, s)
			}
		}
	}
	for _, s := range order {
		n := counts[s.kind]
		if n == 0 {
			continue
		}
		part := fmt.Sprintf("%d %s", n, s.kind)
		if n > 1 && (s.kind == "orphan" || s.kind == "warning") {
			part += "s"
		}
		parts = append(parts, paint(color, s.code, part))
	}
	stack, _, _ := strings.Cut(rows[0].addr, "/")
	if len(shown) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "%s %s\n\n", paint(color, bold, t.title+" of stack "+text(stack, color)+":"), strings.Join(parts, ", "))
	return b.String()
}

// keepsTable reports whether ev leaves the table of the plan or the
// status of the units open: a line the table gathers, a warning, a line of
// a program, the plan of a plugin, or a task that a unit runs, as the
// status of its plugin. Anything else, such as the end of the run or an
// operation of a unit, comes after the table.
func keepsTable(ev engine.Event) bool {
	switch {
	case ev.Type == "plan", statusRow(ev), statusNote(ev):
		return true
	case ev.Type == "diagnostic":
		return true
	case ev.Type == "log":
		// A line of up or destroy comes after the plan, also from an older
		// OpsTruss, whose operations have no start.
		return ev.Task != "up" && ev.Task != "destroy"
	case ev.Address != "" && ev.Task == "plan":
		return true
	}
	return (ev.Type == "task" || ev.Type == "step") && ev.Address != ""
}

// planMark is the mark of a plan line, as terraform plan marks changes: +
// for what is made or runs, ~ for what changes, ± for what is replaced, -
// for what goes, ? for what may run once the units before it ran, = for
// nothing, ○ for a unit left out, and ✗ for one that cannot be planned.
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
	// Without changes, a plan line that waits for the outputs of another
	// unit, or one of an older OpsTruss, says what it will do.
	switch msg := ev.Message; {
	case strings.HasPrefix(msg, "will run"):
		return "+", green
	case strings.HasPrefix(msg, "may run"):
		return "?", yellow
	case strings.HasPrefix(msg, "will be destroyed"), strings.HasPrefix(msg, "will be forgotten"):
		return "-", red
	case strings.HasPrefix(msg, "error"):
		return "✗", red
	}
	return "=", dim
}
