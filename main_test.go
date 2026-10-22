package main

import (
	"bufio"
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
		"lines":     func() { os.Exit(lines(os.Args[1:])) },
	})
}

// TestScripts runs the end-to-end tests in testdata/script: each .txtar
// file holds a project, the commands, and what they print.
func TestScripts(t *testing.T) {
	testscript.Run(t, params("testdata/script"))
}

// TestUseCases runs the projects of real use in testdata/usecases: each
// .txtar file holds a whole project as its users write it, with fakes for
// the tools and services it talks to.
func TestUseCases(t *testing.T) {
	testscript.Run(t, params("testdata/usecases"))
}

func params(dir string) testscript.Params {
	return testscript.Params{Dir: dir, Setup: func(env *testscript.Env) error {
		// How long the load scripts run: 30 seconds in make criteria, ten
		// minutes in the daily load workflow.
		env.Setenv("LOAD_SECONDS", cmp.Or(os.Getenv("OPSTRUSS_LOAD_SECONDS"), "30"))
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
		// rss fails when the daemon of stack default takes more than max MiB
		// of memory: rss <max>.
		"rss": func(ts *testscript.TestScript, _ bool, args []string) {
			var file struct {
				PID int `json:"pid"`
			}
			data, err := os.ReadFile(ts.MkAbs(".opstruss/daemon/default.json"))
			ts.Check(err)
			ts.Check(json.Unmarshal(data, &file))
			// Linux tells it in /proc, macOS through ps.
			var kib int
			if status, err := os.ReadFile("/proc/" + strconv.Itoa(file.PID) + "/status"); err == nil {
				for _, l := range strings.Split(string(status), "\n") {
					if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
						kib, err = strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), " kB"))
						ts.Check(err)
					}
				}
			} else {
				out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(file.PID)).Output()
				ts.Check(err)
				kib, err = strconv.Atoi(strings.TrimSpace(string(out)))
				ts.Check(err)
			}
			limit, err := strconv.Atoi(args[0])
			ts.Check(err)
			ts.Logf("the daemon takes %d MiB", kib/1024)
			if kib/1024 > limit {
				ts.Fatalf("the daemon takes %d MiB, more than %d", kib/1024, limit)
			}
		},
		// burst sends n requests with a bearer token to a webhook of the
		// daemon of stack default within a second, three times with a pause
		// between for the runs to finish, and fails when one gets another
		// answer than 202 or 503, or on Linux when the median of the three
		// 99th percentiles takes longer than max, as within takes the median
		// of plans: burst <path> <n> <secret> <max> <pause>.
		"burst": func(ts *testscript.TestScript, _ bool, args []string) {
			if len(args) != 5 {
				ts.Fatalf("usage: burst <path> <n> <secret> <max> <pause>")
			}
			var file struct {
				Listen string `json:"listen"`
			}
			data, err := os.ReadFile(ts.MkAbs(".opstruss/daemon/default.json"))
			ts.Check(err)
			ts.Check(json.Unmarshal(data, &file))
			n, err := strconv.Atoi(args[1])
			ts.Check(err)
			limit, err := time.ParseDuration(args[3])
			ts.Check(err)
			pause, err := time.ParseDuration(args[4])
			ts.Check(err)
			type result struct {
				code int
				took time.Duration
			}
			var p99s []time.Duration
			for round := range 3 {
				if round > 0 {
					time.Sleep(pause)
				}
				results := make(chan result, n)
				for i := range n {
					go func() {
						time.Sleep(time.Duration(i) * time.Second / time.Duration(n))
						req, _ := http.NewRequest("POST", "http://"+file.Listen+args[0], strings.NewReader("{}"))
						req.Header.Set("Authorization", "Bearer "+args[2])
						began := time.Now()
						resp, err := http.DefaultClient.Do(req)
						if err != nil {
							results <- result{0, time.Since(began)}
							return
						}
						_ = resp.Body.Close()
						results <- result{resp.StatusCode, time.Since(began)}
					}()
				}
				codes := map[int]int{}
				var took []time.Duration
				for range n {
					r := <-results
					codes[r.code]++
					took = append(took, r.took)
				}
				slices.Sort(took)
				p99 := took[len(took)*99/100]
				p99s = append(p99s, p99)
				_, _ = fmt.Fprintf(ts.Stdout(), "202 %d 503 %d other %d p99 %s slowest %v\n", codes[202], codes[503], n-codes[202]-codes[503], p99, took[len(took)-3:])
				if codes[202]+codes[503] != n {
					ts.Fatalf("answers %v", codes)
				}
			}
			slices.Sort(p99s)
			median := p99s[1]
			_, _ = fmt.Fprintf(ts.Stdout(), "median p99 %s\n", median)
			// Go on macOS holds back accept while a fork runs, as macOS has no
			// SOCK_CLOEXEC, so the time is a criterion on Linux only.
			if median > limit && runtime.GOOS == "linux" {
				ts.Fatalf("the median p99 is %s, more than %s", median, limit)
			}
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

// lines writes rate lines a second for seconds seconds, numbered, as a
// service whose output a watch reads, keeping to the rate by the clock
// rather than by a ticker that drops ticks on a slow machine, and then
// writes how many it wrote to the file emitted: lines <rate> <seconds>.
func lines(args []string) int {
	rate, _ := strconv.Atoi(args[0])
	secs, _ := strconv.Atoi(args[1])
	w := bufio.NewWriter(os.NewFile(1, "stdout"))
	began, total, i := time.Now(), rate*secs, 0
	for i < total {
		due := min(total, int(time.Since(began).Seconds()*float64(rate)))
		for ; i < due; i++ {
			_, _ = fmt.Fprintf(w, "line %d error\n", i)
		}
		_ = w.Flush()
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.WriteFile("emitted", []byte(strconv.Itoa(total)+"\n"), 0o600)
	time.Sleep(time.Hour)
	return 0
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
