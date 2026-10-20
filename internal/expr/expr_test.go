package expr

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		in   string
		want []Part
	}{
		{"plain", []Part{{Text: "plain"}}},
		{"${{ a }}", []Part{{Expr: "a", IsExpr: true}}},
		{"x-${{ a }}-y", []Part{{Text: "x-"}, {Expr: "a", IsExpr: true}, {Text: "-y"}}},
		{"$${{ a }}", []Part{{Text: "${{ a }}"}}},
		{"${{ {'a': {'b': 1}} }}", []Part{{Expr: "{'a': {'b': 1}}", IsExpr: true}}},
		{"${{ '}}' + x }}", []Part{{Expr: "'}}' + x", IsExpr: true}}},
	}
	for _, tt := range tests {
		got, err := Split(tt.in)
		if err != nil {
			t.Errorf("%q: %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %#v, want %#v", tt.in, got, tt.want)
		}
	}
	for _, bad := range []string{"${{ a", "${{ }}", "${{ a } }"} {
		if _, err := Split(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestEval(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("CERT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Scope{Dir: dir, Vars: map[string]any{
		"inputs":   map[string]any{"port": int64(8080), "hosts": []any{"b", "a"}, "ratio": 80.0, "name": "web"},
		"stack":    "prod",
		"opstruss": map[string]any{"time": time.Date(2026, 10, 20, 17, 30, 0, 0, time.UTC)},
		"units":    map[string]any{"db": map[string]any{"outputs": map[string]any{"zones": map[string]any{"z": int64(1), "a": int64(2), "m": int64(3)}}}},
	}}
	tests := []struct {
		in   string
		want any
	}{
		{"${{ inputs.port }}", int64(8080)},
		{"${{ inputs.hosts }}", []any{"b", "a"}},
		{"port ${{ inputs.port }}", "port 8080"},
		{"${{ inputs.ratio }}", 80.0},
		{"${{ toJSON(inputs.hosts) }}", `["b","a"]`},
		{"${{ fromJSON('{\"n\": 80.0}').n }}", int64(80)},
		{"${{ fromYAML('a: [1, 2]').a }}", []any{int64(1), int64(2)}},
		{"${{ toYAML({'b': 1, 'a': 2}) }}", "a: 2\nb: 1\n"},
		{"${{ sha256('x') }}", "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"},
		{"${{ file('ca.pem') }}", "CERT\n"},
		{"${{ opstruss.time.strftime('%Y-%m-%d %H:%M') }}", "2026-10-20 17:30"},
		{"${{ opstruss.time.strftime('%F %H:%M', 'Asia/Jakarta') }}", "2026-10-21 00:30"},
		{"${{ inputs.name.upperAscii() }}", "WEB"},
		{"${{ [3, 1, 2].sort() }}", []any{int64(1), int64(2), int64(3)}},
		// Maps iterate in key order, from data and from literals alike.
		{"${{ units.db.outputs.zones.map(k, k) }}", []any{"a", "m", "z"}},
		{"${{ {'z': 1, 'a': 2, 'm': 3}.map(k, k) }}", []any{"a", "m", "z"}},
		{"${{ units.db.outputs.zones.transformList(k, v, k + string(v)) }}", []any{"a2", "m3", "z1"}},
		{"${{ units.db.outputs.zones.transformMap(k, v, v * 2).transformList(k, v, k + string(v)) }}", []any{"a4", "m6", "z2"}},
		{"${{ stack == 'prod' ? 'yes' : 'no' }}", "yes"},
	}
	for _, tt := range tests {
		got, err := EvalString(tt.in, s)
		if err != nil {
			t.Errorf("%s: %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %#v, want %#v", tt.in, got, tt.want)
		}
	}
	errs := map[string]string{
		"hosts ${{ inputs.hosts }}":           "is array inside text",
		"${{ kv('last') }}":                   "kv() is not supported yet",
		"${{ file('none') }}":                 "file:",
		"${{ nope.x }}":                       "undeclared reference",
		"${{ opstruss.time.strftime('%Q') }}": "unknown directive %Q",
	}
	for in, want := range errs {
		if _, err := EvalString(in, s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", in, want, err)
		}
	}
	if _, err := EvalText("${{ inputs.hosts }}", s); err == nil || !strings.Contains(err.Error(), "use toJSON()") {
		t.Errorf("an array is not an env value: %v", err)
	}
	if ok, err := EvalCondition("inputs.port > 80", s); err != nil || !ok {
		t.Errorf("condition without ${{ }}: %v %v", ok, err)
	}
}

func TestAnalyze(t *testing.T) {
	u, err := AnalyzeValue(map[string]any{
		"a": "${{ units.web.outputs.url }}",
		"b": "x ${{ secrets[inputs.to] }} ${{ [1].map(x, x + inputs.n) }}",
		"c": []any{"${{ has(units.db.outputs.host) && kv('k') != null }}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range u.Refs {
		s := strings.Join(r.Path, ".")
		if r.Dynamic {
			s += "[?]"
		}
		got = append(got, s)
	}
	want := []string{"units.web.outputs.url", "secrets[?]", "inputs.to", "inputs.n", "units.db.outputs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refs %v, want %v", got, want)
	}
	if !strings.Contains(strings.Join(u.Funcs, " "), "kv") {
		t.Errorf("funcs %v should name kv", u.Funcs)
	}
}
