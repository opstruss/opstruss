package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const releasesURL = "https://github.com/opstruss/opstruss/releases"

// fakeCurl serves the files under FAKE_RELEASES as the release downloads,
// with their status as -w '%{http_code}' prints it, and FAKE_LATEST as where
// releases/latest redirects.
const fakeCurl = `#!/bin/sh
out= fmt= url=
while [ $# -gt 0 ]; do
	case $1 in
	-o) out=$2; shift ;;
	-w) fmt=$2; shift ;;
	-*) ;;
	*) url=$1 ;;
	esac
	shift
done
case $url in
` + releasesURL + `/latest)
	[ "$fmt" = '%{redirect_url}' ] || exit 2
	printf %s "$FAKE_LATEST" ;;
` + releasesURL + `/download/*)
	[ "$fmt" = '%{http_code}' ] || exit 2
	f=$FAKE_RELEASES/${url#` + releasesURL + `/download/}
	if [ -f "$f" ]; then cp "$f" "$out"; printf 200; else printf 404; fi ;;
*)
	echo "unexpected url $url" >&2
	exit 3 ;;
esac
`

const fakeUname = `#!/bin/sh
case $1 in
-s) echo "$FAKE_OS" ;;
-m) echo "$FAKE_ARCH" ;;
esac
`

// TestInstallScript runs docs/public/install.sh with sh against release
// v1.2.3 of a fake GitHub, whose binary prints its version. $DIR in want is
// the install directory of the run.
func TestInstallScript(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("docs", "public", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	latest := releasesURL + "/tag/v1.2.3"
	installed := "✓ installed  ~/.local/bin/opstruss, opstruss v1.2.3 linux/amd64"
	for _, tc := range []struct {
		name      string
		os, arch  string
		latest    string
		env       []string
		checksums func(string) string
		remove    string // a file of the release that is not there
		dir       string // OPSTRUSS_INSTALL_DIR under the temporary directory
		readOnly  bool   // the parent of dir
		inPath    bool
		old       bool
		want      []string
		lacks     []string
		fail      bool
	}{
		{name: "latest", latest: latest, want: []string{
			"OpsTruss install\n",
			"▸ release    v1.2.3, the latest, for linux/amd64\n",
			"▸ download   checksums.txt\n",
			"▸ download   opstruss_1.2.3_linux_amd64.tar.gz\n",
			"✓ verified   sha256 of opstruss_1.2.3_linux_amd64.tar.gz matches checksums.txt\n",
			installed + "\n",
			"\n~/.local/bin is not in PATH; add this line to the profile of your shell:\n\n  export PATH=\"$HOME/.local/bin:$PATH\"\n",
			"\nGet started at https://opstruss.github.io/\n",
		}, lacks: []string{"\033["}},
		{name: "version", env: []string{"OPSTRUSS_VERSION=1.2.3"}, inPath: true,
			want:  []string{"▸ release    v1.2.3, for linux/amd64\n", installed},
			lacks: []string{"not in PATH"}},
		{name: "v-version", env: []string{"OPSTRUSS_VERSION=v1.2.3"}, inPath: true,
			want: []string{"▸ release    v1.2.3, for linux/amd64\n"}},
		{name: "darwin-arm64", os: "Darwin", arch: "arm64", latest: latest,
			want: []string{"opstruss v1.2.3 darwin/arm64"}},
		{name: "linux-aarch64", arch: "aarch64", latest: latest,
			want: []string{"opstruss v1.2.3 linux/arm64"}},
		{name: "replace", latest: latest, old: true, want: []string{installed}},
		{name: "dir", latest: latest, dir: "bin",
			want: []string{"✓ installed  $DIR/opstruss, ", "\n$DIR is not in PATH;", `  export PATH="$DIR:$PATH"`}},
		{name: "read-only", latest: latest, dir: "ro/bin", readOnly: true, fail: true,
			want: []string{"✗ cannot write to $DIR; set OPSTRUSS_INSTALL_DIR to another directory\n"}},
		{name: "zsh", latest: latest, env: []string{"SHELL=/bin/zsh"},
			want: []string{"add this line to ~/.zshrc, then open a new shell:\n\n  export PATH=\"$HOME/.local/bin:$PATH\"\n"}},
		{name: "bash", latest: latest, env: []string{"SHELL=/bin/bash"},
			want: []string{"add this line to ~/.bashrc, then open a new shell:"}},
		{name: "fish", latest: latest, env: []string{"SHELL=/usr/bin/fish"},
			want: []string{"run this once in fish:\n\n  fish_add_path $HOME/.local/bin\n"}},
		{name: "color", latest: latest, env: []string{"FORCE_COLOR=1"},
			want: []string{"\033[1mOpsTruss install\033[0m\n", "\033[36m▸\033[0m release ", "\033[32m✓\033[0m installed "}},
		{name: "no-color", latest: latest, env: []string{"FORCE_COLOR=1", "NO_COLOR=1"},
			lacks: []string{"\033["}},
		{name: "only-candidates", latest: releasesURL, fail: true,
			want: []string{"✗ GitHub has no latest release yet; pick a version at https://github.com/opstruss/opstruss/releases and run:\n\n" +
				"  curl -fsSL https://opstruss.github.io/install.sh | OPSTRUSS_VERSION=X.Y.Z sh\n"}},
		{name: "no-release", env: []string{"OPSTRUSS_VERSION=9.9.9"}, fail: true,
			want: []string{"✗ found no release v9.9.9\n"}},
		{name: "no-archive", latest: latest, remove: "opstruss_1.2.3_linux_amd64.tar.gz", fail: true,
			want: []string{"✗ found no opstruss_1.2.3_linux_amd64.tar.gz in release v1.2.3\n"}},
		{name: "bad-version", env: []string{"OPSTRUSS_VERSION=1.2.3/../x"}, fail: true,
			want: []string{"✗ OPSTRUSS_VERSION is not a version: 1.2.3/../x\n"}},
		{name: "bad-checksum", latest: latest, fail: true,
			checksums: func(s string) string {
				return regexp.MustCompile(`(?m)^[0-9a-f]{64}(  opstruss_1\.2\.3_linux_amd64)`).ReplaceAllString(s, strings.Repeat("0", 64)+"$1")
			},
			want: []string{"✗ opstruss_1.2.3_linux_amd64.tar.gz does not match checksums.txt\n"}},
		{name: "no-checksum", latest: latest, fail: true,
			checksums: func(string) string { return strings.Repeat("0", 64) + "  other.tar.gz\n" },
			want:      []string{"✗ checksums.txt has no line for opstruss_1.2.3_linux_amd64.tar.gz\n"},
			lacks:     []string{"▸ download   opstruss_"}},
		{name: "windows", os: "MINGW64_NT-10.0", latest: latest, fail: true,
			want: []string{"✗ runs on Windows in WSL2"}},
		{name: "freebsd", os: "FreeBSD", latest: latest, fail: true,
			want: []string{"✗ has no build for FreeBSD\n"}},
		{name: "i686", arch: "i686", latest: latest, fail: true,
			want: []string{"✗ has no build for i686\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			home := filepath.Join(tmp, "home")
			dir := filepath.Join(home, ".local", "bin")
			env := tc.env
			if tc.dir != "" {
				dir = filepath.Join(tmp, tc.dir)
				env = append(env, "OPSTRUSS_INSTALL_DIR="+dir)
			}
			if tc.readOnly {
				if os.Geteuid() == 0 {
					t.Skip("root writes to any directory")
				}
				if err := os.Mkdir(filepath.Dir(dir), 0o555); err != nil {
					t.Fatal(err)
				}
			}
			releases := filepath.Join(tmp, "releases")
			fakeRelease(t, filepath.Join(releases, "v1.2.3"), "1.2.3", tc.checksums)
			if tc.remove != "" {
				if err := os.Remove(filepath.Join(releases, "v1.2.3", tc.remove)); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(tmp, "fakebin")
			writeExec(t, filepath.Join(bin, "curl"), fakeCurl)
			writeExec(t, filepath.Join(bin, "uname"), fakeUname)
			if tc.old {
				writeExec(t, filepath.Join(dir, "opstruss"), "#!/bin/sh\necho old\n")
			}
			path := bin + ":" + os.Getenv("PATH")
			if tc.inPath {
				path = dir + ":" + path
			}
			cmd := exec.Command("sh", script)
			cmd.Env = append([]string{
				"PATH=" + path,
				"HOME=" + home,
				"TMPDIR=" + tmp,
				"SHELL=/bin/sh", // bash sets it from the user otherwise
				"FAKE_RELEASES=" + releases,
				"FAKE_LATEST=" + tc.latest,
				"FAKE_OS=" + or(tc.os, "Linux"),
				"FAKE_ARCH=" + or(tc.arch, "x86_64"),
			}, env...)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out
			err := cmd.Run()
			if (err != nil) != tc.fail {
				t.Fatalf("err = %v, want failure %v\n%s", err, tc.fail, &out)
			}
			for _, w := range tc.want {
				if w = strings.ReplaceAll(w, "$DIR", dir); !strings.Contains(out.String(), w) {
					t.Errorf("output lacks %q:\n%s", w, &out)
				}
			}
			for _, w := range tc.lacks {
				if strings.Contains(out.String(), w) {
					t.Errorf("output has %q:\n%s", w, &out)
				}
			}
			ents, _ := os.ReadDir(dir)
			if tc.fail {
				if len(ents) > 0 {
					t.Errorf("a failed install left %d files in %s", len(ents), dir)
				}
				return
			}
			if len(ents) != 1 || ents[0].Name() != "opstruss" {
				t.Errorf("%s holds %v, want only opstruss", dir, ents)
			}
			info, err := os.Stat(filepath.Join(dir, "opstruss"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o755 {
				t.Errorf("mode %v, want 0755", info.Mode().Perm())
			}
		})
	}
}

// fakeRelease writes the archive of every platform and their checksums.txt,
// which edit may change.
func fakeRelease(t *testing.T, dir, version string, edit func(string) string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sums strings.Builder
	for _, p := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		name := fmt.Sprintf("opstruss_%s_%s.tar.gz", version, p)
		data := tarGz(t, "opstruss", fmt.Sprintf("#!/bin/sh\necho opstruss v%s %s\n", version, strings.Replace(p, "_", "/", 1)))
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(data), name)
	}
	s := sums.String()
	if edit != nil {
		s = edit(s)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, content string }{{"LICENSE", "MIT\n"}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeExec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func or(s, def string) string {
	if s != "" {
		return s
	}
	return def
}
