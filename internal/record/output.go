package record

// An output in the last event of a run, in its record, and in the units
// that status reports is written out as opstruss outputs --json writes
// one: its value, which a sensitive output does not have, so that a
// hidden value never reads as one; whether it is sensitive; and its type,
// when what gives it declares one.

// OutputOf is one output in that shape.
func OutputOf(value any, sensitive bool, typ string) map[string]any {
	o := map[string]any{"sensitive": sensitive}
	if !sensitive {
		o["value"] = value
	}
	if typ != "" {
		o["type"] = typ
	}
	return o
}

// OutputValue is the value of an output in that shape, and false for a
// sensitive one, which has none.
func OutputValue(o any) (any, bool) {
	m, _ := o.(map[string]any)
	v, ok := m["value"]
	return v, ok
}
