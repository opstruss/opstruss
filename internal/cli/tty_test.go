package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
	"github.com/opstruss/opstruss/internal/ansi"
)

// A run on a terminal gives a program with tty a terminal of its own: the
// terminal shows the rows that the program ended with under the header of
// its block, and none of the frames that it drew before them.
func TestTTYOnTerminal(t *testing.T) {
	dir := t.TempDir()
	yaml := `name: frames
tasks:
  show:
    tty: true
    run: |
      printf 'a\nb\n'
      sleep 0.3
      printf '\033[2Aa2\nb2\n'
`
	if err := os.WriteFile(filepath.Join(dir, "opstruss.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 60, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	screen := ansi.NewScreen(60, 200)
	read := make(chan []string)
	go func() {
		var gone []string
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			gone = append(gone, screen.Write(b[:n])...)
			if err != nil {
				break
			}
		}
		read <- append(gone, screen.End()...)
	}()
	environ := append(os.Environ(), "HOME="+t.TempDir(), "TERM=xterm-256color", "NO_COLOR=1")
	code := start([]string{"-C", dir, "show"}, nil, false, slave, slave, environ)
	_ = slave.Close()
	rows := <-read
	got := strings.Join(rows, "\n")
	if code != 0 || !strings.Contains(got, "▸ show\n│ a2\n│ b2\n✓ show") || strings.Contains(got, "│ a\n") {
		t.Fatalf("exit %d, terminal:\n%s", code, got)
	}
}
