package expr

import (
	"errors"
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
		"${{ kv('last') }}":                   "kv() reads the memory of a stack, which is only open in a run",
		"${{ file('none') }}":                 "file:",
		"${{ nope.x }}":                       "undeclared reference",
		"${{ opstruss.time.strftime('%Q') }}": "unknown directive %Q",
	}
	for in, want := range errs {
		if _, err := EvalString(in, s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", in, want, err)
		}
	}
	mem := map[string]any{"last": int64(3), "pr-1": "a", "pr-2": "b"}
	ks := s.WithKV(func(key string) (any, error) {
		if p, ok := strings.CutSuffix(key, "*"); ok {
			out := map[string]any{}
			for k, v := range mem {
				if strings.HasPrefix(k, p) {
					out[k] = v
				}
			}
			return out, nil
		}
		return mem[key], nil
	}, nil)
	for in, want := range map[string]any{
		"${{ kv('last') + 1 }}":                 int64(4),
		"${{ kv('none') }}":                     nil,
		"${{ kv('none', 7) }}":                  int64(7),
		"${{ kv('last', 7) }}":                  int64(3),
		"${{ kv('pr-*').map(k, k).join(',') }}": "pr-1,pr-2",
	} {
		if got, err := EvalString(in, ks); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v (%v), want %#v", in, got, err, want)
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

func TestMarks(t *testing.T) {
	s := (&Scope{Vars: map[string]any{
		"inputs": map[string]any{"token": "t0k3n", "name": "web"},
		"steps":  map[string]any{"a": map[string]any{"outputs": map[string]any{"key": "k", "id": "1"}}},
	}}).WithMarks("inputs", map[string]any{"token": "t0k3n", "name": "web"}, map[string]any{"token": true}).
		WithMarks("steps", map[string]any{"a": map[string]any{"outputs": map[string]any{"key": "k", "id": "1"}}},
			map[string]any{"a": map[string]any{"outputs": map[string]any{"key": true}}})
	for src, want := range map[string]bool{
		"inputs.token":                       true,
		"inputs.name":                        false,
		"inputs":                             true,
		"inputs['name'] + 'x'":               false,
		"base64.encode(bytes(inputs.token))": true,
		"steps.a.outputs.id":                 false,
		"steps.a.outputs.key.size()":         true,
		"steps.a":                            true,
	} {
		if got := Sensitive(src, s); got != want {
			t.Errorf("Sensitive(%q) = %v", src, got)
		}
	}
	v, m, err := EvalMarked(map[string]any{"url": "https://${{ inputs.name }}", "auth": "Bearer ${{ inputs.token }}",
		"list": []any{"a", "${{ inputs.token }}"}}, s)
	if err != nil {
		t.Fatal(err)
	}
	marks := m.(map[string]any)
	if marks["url"] != nil || marks["auth"] != true || marks["list"] != true || v.(map[string]any)["auth"] != "Bearer t0k3n" {
		t.Errorf("EvalMarked: %v %v", v, m)
	}
	if text, sensitive, err := EvalTextMarked("${{ inputs.name }}", s); err != nil || text != "web" || sensitive {
		t.Errorf("EvalTextMarked: %q %v %v", text, sensitive, err)
	}
	if MarkAt(true, "x", "y") != true || MarkAt(nil, "x") != nil || MarkAt(map[string]any{"a": map[string]any{}}, "a") != nil {
		t.Error("MarkAt")
	}
	if s.With("inputs", map[string]any{}).Marks["inputs"] != nil {
		t.Error("With keeps no marks for the name it sets")
	}
}

func TestLater(t *testing.T) {
	made := map[string]int{}
	value := func(name string, v any, err error) any {
		return Later(func() (any, error) {
			made[name]++
			return v, err
		})
	}
	s := &Scope{Vars: map[string]any{
		"secrets": map[string]any{
			"token":  value("token", "t0k3n", nil),
			"restic": value("restic", map[string]any{"s3": "k1", "b2": "k2"}, nil),
			"broken": value("broken", nil, errors.New("sops: no key")),
			"unread": value("unread", "x", nil),
		},
		"inputs": map[string]any{"to": "b2"},
	}}
	for src, want := range map[string]any{
		"${{ secrets.token }}":                 "t0k3n",
		"Bearer ${{ secrets.token }}":          "Bearer t0k3n",
		"${{ secrets.restic[inputs.to] }}":     "k2",
		"${{ secrets['restic'].s3 }}":          "k1",
		"${{ has(secrets.token) }}":            true,
		"${{ size(secrets.token) }}":           int64(5),
		"${{ toJSON(secrets.restic) }}":        `{"b2":"k2","s3":"k1"}`,
		"${{ secrets.token == 't0k3n' }}":      true,
		"${{ [secrets.token, 'x'].join('') }}": "t0k3nx",
	} {
		got, err := EvalString(src, s)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v, %v; want %#v", src, got, err, want)
		}
	}
	if _, err := EvalString("${{ secrets.broken }}", s); err == nil || !strings.Contains(err.Error(), "sops: no key") {
		t.Errorf("a value that fails to be made fails what reads it: %v", err)
	}
	if made["token"] != 1 || made["restic"] != 1 || made["unread"] != 0 {
		t.Errorf("each value is made once, and only when read: %v", made)
	}
	if !ReadsLater("${{ secrets.token }}", s) || ReadsLater("${{ inputs.to }}", s) || !ReadsLater(map[string]any{"a": "${{ secrets }}"}, s) {
		t.Error("ReadsLater")
	}
	v, err := Made(map[string]any{"a": []any{s.Vars["secrets"].(map[string]any)["token"]}, "b": "plain"})
	if err != nil || !reflect.DeepEqual(v, map[string]any{"a": []any{"t0k3n"}, "b": "plain"}) {
		t.Errorf("Made: %v %v", v, err)
	}
	if _, err := Made(s.Vars["secrets"]); err == nil {
		t.Error("Made fails with the value that fails")
	}
}
