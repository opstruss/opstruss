package cli

import (
	"regexp"
	"slices"

	"github.com/spf13/cobra"
)

// docsSite is where the documentation of each version is published.
const docsSite = "https://opstruss.github.io/"

var release = regexp.MustCompile(`^v(\d+)\.(\d+)\.\d+(\+.*)?$`)

// docsURL is the address of path in the documentation of the OpsTruss
// that version names: /v<major>.<minor>/ for a release, and /dev/, the
// documentation of main, for a pre-release or any other build.
func docsURL(version, path string) string {
	m := release.FindStringSubmatch(version)
	if m == nil {
		return docsSite + "dev/" + path
	}
	return docsSite + "v" + m[1] + "." + m[2] + "/" + path
}

// LinkedPaths are the paths of the documentation that the commands under
// root link: the page that the help of each ends with, "" for the home
// page. The site keeps each of them as long as it keeps the version.
func LinkedPaths(root *cobra.Command) []string {
	var paths []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		paths = append(paths, docsPath(c))
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	slices.Sort(paths)
	return slices.Compact(paths)
}
