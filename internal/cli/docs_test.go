package cli

import "testing"

func TestDocsURL(t *testing.T) {
	for _, tt := range []struct{ version, want string }{
		{"v0.1.0", "https://opstruss.github.io/v0.1/guides/ci"},
		{"v0.1.3", "https://opstruss.github.io/v0.1/guides/ci"},
		{"v1.12.0+dirty", "https://opstruss.github.io/v1.12/guides/ci"},
		{"v0.2.0-rc.1", "https://opstruss.github.io/dev/guides/ci"},
		{"v0.1.1-0.20261027063000-41d990212345", "https://opstruss.github.io/dev/guides/ci"},
		{"(devel)", "https://opstruss.github.io/dev/guides/ci"},
	} {
		if got := docsURL(tt.version, "guides/ci"); got != tt.want {
			t.Errorf("docsURL(%q) = %q, want %q", tt.version, got, tt.want)
		}
	}
}
