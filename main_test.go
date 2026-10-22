package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/cli"
	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"opstruss":  func() { os.Exit(cli.Main()) },
		"fakecloud": func() { os.Exit(fakecloud(os.Args[1:])) },
	})
}

// TestScripts runs the end-to-end tests in testdata/script: each .txtar
// file holds a project, the commands, and what they print.
func TestScripts(t *testing.T) {
	testscript.Run(t, params("testdata/script"))
}

func params(dir string) testscript.Params {
	return testscript.Params{Dir: dir, Setup: func(env *testscript.Env) error {
		// The plugins of the tests, for uses: ./testplugins/<name>.
		return copyDir("testdata/plugins", filepath.Join(env.WorkDir, "testplugins"))
	}, Condition: func(cond string) (bool, error) {
		if cond == "failpoints" {
			return failpoint.Enabled, nil
		}
		return false, fmt.Errorf("unknown condition %s", cond)
	}, Cmds: map[string]func(*testscript.TestScript, bool, []string){
		// waitfile waits up to 10 seconds for a file that a command in the
		// background makes, so a script acts after it, not after a guess.
		"waitfile": func(ts *testscript.TestScript, _ bool, args []string) {
			p := ts.MkAbs(args[0])
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(p); err == nil {
					return
				}
			}
			ts.Fatalf("%s did not appear", args[0])
		},
		// waitgrep waits up to 10 seconds until a file that a process in
		// the background writes, such as the log of a service, or one of
		// the files a glob matches, matches a regular expression.
		"waitgrep": func(ts *testscript.TestScript, _ bool, args []string) {
			if len(args) != 2 {
				ts.Fatalf("usage: waitgrep <regexp> <file or glob>")
			}
			re, err := regexp.Compile("(?m)" + args[0])
			ts.Check(err)
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				paths, _ := filepath.Glob(ts.MkAbs(args[1]))
				for _, p := range paths {
					if data, err := os.ReadFile(p); err == nil && re.Match(data) {
						return
					}
				}
			}
			ts.Fatalf("no match for %q in %s", args[0], args[1])
		},
		// webhook sends a request to the webhooks of the daemon of stack
		// default, at the address its file names, and prints the status and
		// body of the answer: webhook <method> <path> <body> [-bearer s]
		// [-github s].
		"webhook": func(ts *testscript.TestScript, _ bool, args []string) {
			if len(args) < 3 {
				ts.Fatalf("usage: webhook <method> <path> <body> [-bearer secret] [-github secret]")
			}
			var file struct {
				Listen string `json:"listen"`
			}
			data, err := os.ReadFile(ts.MkAbs(".opstruss/daemon/default.json"))
			ts.Check(err)
			ts.Check(json.Unmarshal(data, &file))
			req, err := http.NewRequest(args[0], "http://"+file.Listen+args[1], strings.NewReader(args[2]))
			ts.Check(err)
			req.Header.Set("Content-Type", "application/json")
			for i := 3; i+1 < len(args); i += 2 {
				switch args[i] {
				case "-bearer":
					req.Header.Set("Authorization", "Bearer "+args[i+1])
				case "-github":
					m := hmac.New(sha256.New, []byte(args[i+1]))
					m.Write([]byte(args[2]))
					req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
				}
			}
			resp, err := http.DefaultClient.Do(req)
			ts.Check(err)
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			_, _ = fmt.Fprintf(ts.Stdout(), "%d %s", resp.StatusCode, body)
		},
		// daemonrun fails unless the newest run of a stack ran in its
		// daemon, or with ! unless it did not: the record of the run names
		// the pid that daemon/<stack>.json names.
		"daemonrun": func(ts *testscript.TestScript, neg bool, args []string) {
			if len(args) != 1 {
				ts.Fatalf("usage: daemonrun <stack>")
			}
			root := ts.MkAbs(".opstruss")
			var daemon struct {
				PID int `json:"pid"`
			}
			data, err := os.ReadFile(filepath.Join(root, "daemon", args[0]+".json"))
			ts.Check(err)
			ts.Check(json.Unmarshal(data, &daemon))
			recs, err := filepath.Glob(filepath.Join(root, "runs", args[0], "*", "record.json"))
			ts.Check(err)
			var newest struct {
				Run     string `json:"run"`
				PID     int    `json:"pid"`
				Started string `json:"started"`
			}
			for _, p := range recs {
				var rec = newest
				data, err := os.ReadFile(p)
				ts.Check(err)
				ts.Check(json.Unmarshal(data, &rec))
				if t1, t2 := parseTime(rec.Started), parseTime(newest.Started); newest.Run == "" || t1.After(t2) {
					newest = rec
				}
			}
			if (newest.PID == daemon.PID) == neg {
				ts.Fatalf("run %s ran in pid %d, the daemon is pid %d", newest.Run, newest.PID, daemon.PID)
			}
		},
		// within runs a command n times and fails when the median takes
		// longer than a duration, for the load criteria.
		"within": func(ts *testscript.TestScript, neg bool, args []string) {
			if len(args) < 3 {
				ts.Fatalf("usage: within <runs> <duration> <command> [args...]")
			}
			n, err := strconv.Atoi(args[0])
			limit, err2 := time.ParseDuration(args[1])
			if err != nil || err2 != nil || n < 1 {
				ts.Fatalf("usage: within <runs> <duration> <command> [args...]")
			}
			var took []time.Duration
			for range n {
				began := time.Now()
				ts.Check(ts.Exec(args[2], args[3:]...))
				took = append(took, time.Since(began))
			}
			slices.Sort(took)
			median, what := took[n/2], strings.Join(args[2:], " ")
			ts.Logf("%s took %v, median %s", what, took, median.Round(time.Millisecond))
			if median > limit {
				ts.Fatalf("%s took %s in the median of %d runs, more than %s", what, median.Round(time.Millisecond), n, limit)
			}
		},
	}}
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// copyDir copies the files under from to to, with their modes.
func copyDir(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	})
}
