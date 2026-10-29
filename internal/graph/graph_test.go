package graph

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	g, err := Build("units", map[string][]string{
		"web":   {"db", "dns"},
		"db":    nil,
		"dns":   {"db"},
		"cache": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cache", "db", "dns", "web"}; !reflect.DeepEqual(g.Order, want) {
		t.Fatalf("order %v, want %v", g.Order, want)
	}
	_, err = Build("units", map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}, "d": {"a"}})
	if err == nil || !strings.Contains(err.Error(), "units a → b → c → a form a cycle") {
		t.Fatalf("want the cycle named, got %v", err)
	}
	if _, err := Build("units", map[string][]string{"a": {"a"}}); err == nil || !strings.Contains(err.Error(), "a → a") {
		t.Fatalf("want a self cycle, got %v", err)
	}
	if _, err := Build("units", map[string][]string{"a": {"x"}}); err == nil {
		t.Fatal("want an unknown dependency error")
	}
}

func TestKeys(t *testing.T) {
	keys, err := Keys([]any{"vm", "docker", "lxc"})
	if err != nil || !reflect.DeepEqual(keys, []string{"docker", "lxc", "vm"}) {
		t.Fatalf("list keys %v, %v", keys, err)
	}
	keys, err = Keys(map[string]any{"eu-west-1": 1, "v1.2": 2})
	if err != nil || !reflect.DeepEqual(keys, []string{"eu-west-1", "v1.2"}) {
		t.Fatalf("map keys %v, %v", keys, err)
	}
	for _, bad := range []any{[]any{"Docker"}, []any{"a/b"}, []any{int64(1)}, []any{"a", "a"}, "x"} {
		if _, err := Keys(bad); err == nil {
			t.Errorf("%v: want an error", bad)
		}
	}
	if Address("prod", "image", "lxc") != "prod/image[lxc]" || Address("prod", "web", "") != "prod/web" {
		t.Fatal("address")
	}
}

// TestBuildCycles: each cycle is an error of its own, also beside another.
func TestBuildCycles(t *testing.T) {
	_, err := Build("units", map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"d"}, "d": {"e"}, "e": {"c"}, "f": {"a"}, "g": nil})
	want := "units a → b → a form a cycle\nunits c → d → e → c form a cycle"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %s", err, want)
	}
}
