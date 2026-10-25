package ansi

// Forced reads what the environment says of color: NO_COLOR that is not
// empty turns it off, as no-color.org asks, and FORCE_COLOR or
// CLICOLOR_FORCE that is neither empty nor 0 turns it on. Set is false when
// the environment says nothing.
func Forced(getenv func(string) string) (on, set bool) {
	if getenv("NO_COLOR") != "" {
		return false, true
	}
	for _, k := range []string{"FORCE_COLOR", "CLICOLOR_FORCE"} {
		if v := getenv(k); v != "" && v != "0" {
			return true, true
		}
	}
	return false, false
}

// Color says whether output to a writer is in color: as the environment
// says, if it does (Forced); else not with TERM=dumb; else at a terminal,
// and in GitHub Actions, whose log shows ANSI.
func Color(getenv func(string) string, terminal bool) bool {
	if on, set := Forced(getenv); set {
		return on
	}
	switch {
	case getenv("TERM") == "dumb":
		return false
	case terminal:
		return true
	}
	return getenv("GITHUB_ACTIONS") == "true"
}
