package main

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/opstruss/opstruss/internal/cli"
	"github.com/opstruss/opstruss/internal/failpoint"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"opstruss": func() { os.Exit(cli.Main()) },
	})
}

// TestScripts runs the end-to-end tests in testdata/script: each .txtar
// file holds a project, the commands, and what they print.
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{Dir: "testdata/script", Condition: func(cond string) (bool, error) {
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
	}})
}
