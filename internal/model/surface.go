package model

import "slices"

// Surface lists the names that a project cannot take, the fields of the
// event of each trigger, and what steps.<id> gives, one fact a line, for
// the surface test that keeps every line of a release.
func Surface() []string {
	var lines []string
	lists := map[string][]string{"command": Commands, "standard-task": StandardTasks, "builtin-task": BuiltinTasks,
		"builtin-flag": BuiltinFlags, "hook": Hooks}
	for list, names := range lists {
		for _, n := range names {
			lines = append(lines, "name "+list+" "+n)
		}
	}
	for kind := range EventFields {
		// A watch with batch gives every field that a watch may give.
		for _, f := range triggerEvent(kind, &Watch{Batch: "1s"}) {
			lines = append(lines, "event "+kind+" "+f)
		}
	}
	for _, f := range stepFields {
		lines = append(lines, "variable steps.<id>."+f)
	}
	slices.Sort(lines)
	return slices.Compact(lines)
}
