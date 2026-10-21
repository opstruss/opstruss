package main

import (
	"fmt"
	"os"
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
	return testscript.Params{Dir: dir, Condition: func(cond string) (bool, error) {
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
