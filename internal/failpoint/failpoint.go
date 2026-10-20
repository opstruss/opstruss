//go:build !failpoints

// Package failpoint marks the points where tests stop OpsTruss as a crash
// would, as gofail does in etcd. Only builds with the failpoints tag do
// anything at them.
package failpoint

// Enabled reports whether this build stops at failpoints.
const Enabled = false

// Hit marks the named point.
func Hit(string) {}

// Fail marks the named point, where a test can make an operation fail.
func Fail(string) error { return nil }
