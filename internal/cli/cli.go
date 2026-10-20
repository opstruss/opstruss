// Package cli implements the opstruss command line.
package cli

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
)

const usage = `Usage: opstruss <command>

Commands:
  version  Print the version
  help     Show this help
`

// Main runs the command line with the process arguments and returns the exit
// code.
func Main() int {
	return run(os.Args[1:], os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	var out string
	switch args[0] {
	case "help", "-h", "--help":
		out = usage
	case "version":
		out = fmt.Sprintf("opstruss %s %s/%s\n", version(), runtime.GOOS, runtime.GOARCH)
	default:
		fmt.Fprintf(stderr, "opstruss: unknown command %q\n\n%s", args[0], usage)
		return 1
	}
	if len(args) > 1 {
		fmt.Fprintf(stderr, "opstruss: %s takes no arguments\n", args[0])
		return 1
	}
	fmt.Fprint(stdout, out)
	return 0
}

// version returns the module version stamped by the go command: the tag for
// release builds and go install, a pseudo-version for builds in a checkout.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
