package model

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCatalog(t *testing.T) {
	plugin := func(run string) string { return "name: a\ntasks:\n  up:\n    run: [" + run + "]\n" }
	files := func(marker, run string, more ...string) map[string]string {
		m := map[string]string{
			"opstruss.yaml":                          "name: app\nunits:\n  a:\n    uses: ./catalog/plugins/a\n",
			"catalog/lib/opstruss.sh":                "",
			"catalog/plugins/a/opstruss.plugin.yaml": plugin(run),
		}
		if marker != "" {
			m["catalog/opstruss.catalog.yaml"] = marker
		}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	const shared = "shared: [lib]\n"
	const unsafe = "a shared path is relative to the root of the catalog, without .., globs, or a leading /"
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"a program of the plugin", files(shared, "./up.sh"), ""},
		{"a program in a shared path", files(shared, "../../lib/opstruss.sh"), ""},
		{"a program by name", files(shared, "sh"), ""},
		{"a program of another package", files(shared, "../b/up.sh"),
			"catalog/plugins/a/opstruss.plugin.yaml:4:11: tasks.up.run[0]: ../b/up.sh leads outside its package and the shared paths of its catalog"},
		{"a program outside the catalog", files(shared, "../../../x.sh"), "../../../x.sh leads outside its package"},
		{"a program of a step", files(shared, "x", "catalog/plugins/a/opstruss.plugin.yaml",
			"name: a\ntasks:\n  up:\n    run: [./up.sh]\n  build:\n    steps:\n      - run: [../b/build.sh]\n"),
			"tasks.build.steps[0].run[0]: ../b/build.sh leads outside"},
		{"a program of stop", files(shared, "x", "catalog/plugins/a/opstruss.plugin.yaml",
			"name: a\ntasks:\n  up:\n    run: [./up.sh]\n    stop: {run: [../b/stop.sh]}\n"),
			"tasks.up.stop.run[0]: ../b/stop.sh leads outside"},
		{"no catalog", files("", "../b/up.sh"), ""},
		{"no shared paths", files("{}\n", "../../lib/opstruss.sh"), "leads outside its package"},
		{"extension keys", files("shared: [lib]\nx-site: {title: Catalog}\n", "./up.sh"), ""},
		{"a shared file", files("shared: [lib/opstruss.sh]\n", "../../lib/opstruss.sh"), ""},
		{"..", files("shared: [../lib]\n", "./up.sh"), "catalog/opstruss.catalog.yaml:1:10: shared[0]: " + unsafe},
		{"a glob", files("shared: [lib/*]\n", "./up.sh"), "shared[0]: " + unsafe},
		{"an absolute path", files("shared: [/lib]\n", "./up.sh"), "shared[0]: " + unsafe},
		{"an unclean path", files("shared: [./lib]\n", "./up.sh"), "shared[0]: " + unsafe},
		{"a missing path", files("shared: [nope]\n", "./up.sh"), "shared[0]: nope does not exist"},
		{"an unknown key", files("plugins: [a]\n", "./up.sh"), "catalog/opstruss.catalog.yaml:1:1: plugins: unknown key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := problems(t, write(t, tt.files), "")
			if tt.want == "" {
				if len(got) > 0 {
					t.Fatalf("unexpected problems:\n%s", strings.Join(got, "\n"))
				}
				return
			}
			contains(t, got, tt.want)
		})
	}
}

func TestCatalogOnce(t *testing.T) {
	// The problems of opstruss.catalog.yaml are reported in it, once,
	// and not again where its packages are used.
	got := problems(t, write(t, map[string]string{
		"opstruss.yaml":                          "name: app\nunits:\n  a:\n    uses: ./catalog/plugins/a\n  b:\n    uses: ./catalog/plugins/b\n",
		"catalog/opstruss.catalog.yaml":          "shared: [nope]\n",
		"catalog/plugins/a/opstruss.plugin.yaml": "name: a\ntasks:\n  up:\n    run: [./up.sh]\n",
		"catalog/plugins/b/opstruss.plugin.yaml": "name: b\ntasks:\n  up:\n    run: [./up.sh]\n",
	}), "")
	if len(got) != 1 || !strings.HasPrefix(got[0], "catalog/opstruss.catalog.yaml:1:10: shared[0]: nope does not exist") {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
}

func TestCatalogLinks(t *testing.T) {
	link := func(target string) []string {
		dir := write(t, map[string]string{
			"opstruss.yaml":                          "name: app\nunits:\n  a:\n    uses: ./catalog/plugins/a\n  c:\n    uses: ./catalog/components/c\n",
			"catalog/opstruss.catalog.yaml":          "shared: [lib]\n",
			"catalog/lib/opstruss.sh":                "",
			"catalog/lib/units.yaml":                 "units:\n  shared:\n    run: [x]\n",
			"catalog/plugins/a/opstruss.plugin.yaml": "name: a\ntasks:\n  up:\n    run: [./up.sh]\n",
			"catalog/plugins/b/up.sh":                "",
			"catalog/components/c/opstruss.yaml":     "include: [units.yaml]\nunits:\n  own:\n    run: [x]\n",
		})
		for _, l := range [][2]string{{target, "catalog/plugins/a/up.sh"}, {"../../lib/units.yaml", "catalog/components/c/units.yaml"}} {
			if err := os.Symlink(l[0], filepath.Join(dir, l[1])); err != nil {
				t.Fatal(err)
			}
		}
		return problems(t, dir, "")
	}
	if got := link("../../lib/opstruss.sh"); len(got) > 0 {
		t.Fatalf("links to shared paths:\n%s", strings.Join(got, "\n"))
	}
	got := link("../b/up.sh")
	if !slices.ContainsFunc(got, func(s string) bool {
		return strings.Contains(s, "units.a.uses: symlink plugins/a/up.sh leads outside its package and the shared paths of the catalog")
	}) {
		t.Fatalf("a link to another package:\n%s", strings.Join(got, "\n"))
	}
}
