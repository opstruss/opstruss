package main

import (
	"fmt"
	"os"
	"testing"

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
	}})
}
