package main

import (
	"os"
	// The time zones of schedules work on systems without tzdata, such as
	// distroless and Alpine images.
	_ "time/tzdata"

	"github.com/opstruss/opstruss/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
