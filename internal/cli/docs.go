package cli

import "regexp"

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
