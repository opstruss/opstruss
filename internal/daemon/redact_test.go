package daemon

import (
	"strings"
	"sync"
	"testing"

	"github.com/opstruss/opstruss/internal/engine"
	"github.com/opstruss/opstruss/internal/secret"
)

// serviceLines is what the log of a service gets of out and errs, written
// to its stdout and stderr at once.
func serviceLines(red *secret.Redactor, out, errs string) map[string][]string {
	var mu sync.Mutex
	got := map[string][]string{}
	s := &service{spec: &engine.Service{Redact: red, Lines: func(stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		got[stream] = append(got[stream], line)
	}}}
	var wg sync.WaitGroup
	for stream, text := range map[string]string{"stdout": out, "stderr": errs} {
		wg.Go(func() { s.copyLines(strings.NewReader(text), "", stream) })
	}
	wg.Wait()
	return got
}

// The log of a service hides a value the run knows, and the shape of a
// credential, also where a line longer than its buffer is cut; and a
// private key over several lines of one stream, but not the lines of the
// other.
func TestServiceLogHides(t *testing.T) {
	red := secret.NewRedactor()
	red.Add("KNOWN-SECRET-CANARY")
	shape := "gh" + "p_" + strings.Repeat("aB3dE5", 7)
	for _, v := range []string{"KNOWN-SECRET-CANARY", shape} {
		for _, at := range []int{1, 5, len(v) - 1} {
			long := strings.Repeat("x", secret.MaxLine-at-1) + " " + v + " tail\n"
			got := strings.Join(serviceLines(red, long, "")["stdout"], "")
			if strings.Contains(got, v) || !strings.Contains(got, secret.Mask+" tail") || strings.Count(got, "x") != secret.MaxLine-at-1 {
				t.Fatalf("%s cut %d into the buffer: ...%q", v, at, got[max(0, len(got)-60):])
			}
		}
	}
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\nAAAAAAAABG5vbmUAAAAEbm9u\n-----END OPENSSH PRIVATE KEY-----\ndone\n"
	got := serviceLines(red, key, "one\ntwo\nthree\n")
	if want := []string{secret.Mask, secret.Mask, secret.Mask, secret.Mask, "done"}; strings.Join(got["stdout"], "|") != strings.Join(want, "|") {
		t.Fatalf("stdout %q", got["stdout"])
	}
	if strings.Join(got["stderr"], "|") != "one|two|three" {
		t.Fatalf("stderr %q", got["stderr"])
	}
	// A watch source, whose lines are events, has nothing hidden.
	if got := serviceLines(nil, shape+"\n", "")["stdout"]; len(got) != 1 || got[0] != shape {
		t.Fatalf("source lines %q", got)
	}
}
