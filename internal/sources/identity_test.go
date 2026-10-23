package sources

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestIdentity(t *testing.T) {
	for _, s := range []string{Repository + "@main", Repository + "/a@dev/feature", Repository + "/a@latest"} {
		id, err := Parse(s)
		if err != nil || id.Key() != s {
			t.Fatalf("%s: %v %v", s, id, err)
		}
	}
	for _, s := range []string{"https://" + Repository + "@v1", Repository + "-other/a@v1", Repository + "/../a@v1", Repository + "/a@--upload-pack", Repository + "/a@", Repository + "/a@a..b", "github.com/other/repo/a@v1", Repository + "/a/.git/config@v1"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestHashEncoding(t *testing.T) {
	// Published encoding fixture: JSON triples, no trailing newline.
	h := treeHash([]treeEntry{{"a", "100644", payloadHash([]byte("hello\n"))}})
	const want = "h1:3DT0nNQk+amz7wMZFkNXDrVW1vDHSyUFVEMhHsWj5Yg="
	if h != want {
		t.Fatalf("hash %s", h)
	}
	b, _ := json.Marshal([][3]string{{"a", "100644", payloadHash([]byte("hello\n"))}})
	if bytes.HasSuffix(b, []byte("\n")) {
		t.Fatal("unexpected newline")
	}
}
