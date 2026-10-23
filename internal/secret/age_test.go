package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func newKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestKeyFile(t *testing.T) {
	home := map[string]string{"HOME": "/h"}
	want := "/h/.config/opstruss/age/keys.txt"
	if runtime.GOOS == "darwin" {
		want = "/h/Library/Application Support/opstruss/age/keys.txt"
	}
	for _, tt := range []struct {
		vars map[string]string
		want string
	}{
		{home, want},
		{map[string]string{"HOME": "/h", "XDG_CONFIG_HOME": "/x"}, "/x/opstruss/age/keys.txt"},
		{map[string]string{"HOME": "/h", "OPSTRUSS_AGE_KEY_FILE": "/k.txt"}, "/k.txt"},
	} {
		if got, err := KeyFile(env(tt.vars)); err != nil || got != tt.want {
			t.Errorf("%v: got %s, %v; want %s", tt.vars, got, err, tt.want)
		}
	}
	if _, err := KeyFile(env(nil)); err == nil || !strings.Contains(err.Error(), "set $OPSTRUSS_AGE_KEY_FILE") {
		t.Errorf("without HOME: %v", err)
	}
}

func TestIdentities(t *testing.T) {
	dir := t.TempDir()
	a, b, c, d := newKey(t), newKey(t), newKey(t), newKey(t)
	fileB := filepath.Join(dir, "opstruss.txt")
	fileD := filepath.Join(dir, "sops", "sops", "age", "keys.txt")
	writeFile(t, fileB, "# created: now\n# public key: "+b.Recipient().String()+"\nAGE-PLUGIN-YUBIKEY-1QQQQ\n"+b.String()+"\n")
	writeFile(t, fileD, d.String()+"\n")
	ids, err := Identities(env(map[string]string{
		"OPSTRUSS_AGE_KEY":      a.String(),
		"OPSTRUSS_AGE_KEY_FILE": fileB,
		"SOPS_AGE_KEY":          "# a comment\n" + c.String(),
		"XDG_CONFIG_HOME":       filepath.Join(dir, "sops"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, id := range ids {
		got = append(got, id.Recipient()+" "+id.From)
	}
	want := []string{
		a.Recipient().String() + " $OPSTRUSS_AGE_KEY",
		b.Recipient().String() + " " + fileB,
		c.Recipient().String() + " $SOPS_AGE_KEY",
		d.Recipient().String() + " " + fileD,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := Identities(env(map[string]string{"OPSTRUSS_AGE_KEY": "AGE-SECRET-KEY-1NOTAKEY"})); err == nil ||
		!strings.Contains(err.Error(), "$OPSTRUSS_AGE_KEY: line 1") || strings.Contains(err.Error(), "NOTAKEY") {
		t.Fatalf("a broken key: %v", err)
	}
}

func TestEnsureMachineKey(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{"XDG_CONFIG_HOME": dir}
	first, made, err := EnsureMachineKey(env(vars), time.Now())
	if err != nil || !made {
		t.Fatalf("made %v: %v", made, err)
	}
	file := filepath.Join(dir, "opstruss", "age", "keys.txt")
	if first.From != file {
		t.Fatalf("made in %s, want %s", first.From, file)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", info, err)
	}
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "# public key: "+first.Recipient()+"\n") {
		t.Fatalf("no public key line: %s", data)
	}
	again, made, err := EnsureMachineKey(env(vars), time.Now())
	if err != nil || made || again.Recipient() != first.Recipient() {
		t.Fatalf("again: %s made %v: %v", again.Recipient(), made, err)
	}

	// The key that sops reads is the key of this machine when OpsTruss has
	// none of its own.
	sops := newKey(t)
	other := t.TempDir()
	writeFile(t, filepath.Join(other, "sops.txt"), sops.String()+"\n")
	key, made, err := EnsureMachineKey(env(map[string]string{"XDG_CONFIG_HOME": other, "SOPS_AGE_KEY_FILE": filepath.Join(other, "sops.txt")}), time.Now())
	if err != nil || made || key.Recipient() != sops.Recipient().String() {
		t.Fatalf("sops key: %s made %v: %v", key.Recipient(), made, err)
	}
	if _, err := os.Stat(filepath.Join(other, "opstruss")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made a key beside the one sops reads: %v", err)
	}
}

func TestAgeEnvelope(t *testing.T) {
	a, b, c := newKey(t), newKey(t), newKey(t)
	key := bytes.Repeat([]byte{7}, 32)
	data, err := SealAge(key, []string{a.Recipient().String(), b.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(data, "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Fatalf("not armored: %s", data)
	}
	got, by, err := OpenAge(data, []Identity{{c, "c"}, {b, "b"}})
	if err != nil || !bytes.Equal(got, key) || by.From != "b" {
		t.Fatalf("opened %x by %s: %v", got, by.From, err)
	}
	if _, _, err := OpenAge(data, []Identity{{c, "c"}}); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("another key: %v", err)
	}
	if _, _, err := OpenAge(data, nil); !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("no key: %v", err)
	}
}

func TestCheckRecipient(t *testing.T) {
	id := newKey(t)
	r := id.Recipient().String()
	for _, tt := range []struct{ s, want string }{
		{r, ""},
		{id.String(), "age secret key"},
		{"ssh-ed25519 AAAA", "ssh recipients are not supported yet"},
		{"age1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t", "age plugins"},
		{r[:len(r)-2], "length or checksum"},
		{"arn:aws:kms:x", "not an age recipient"},
	} {
		err := CheckRecipient(tt.s)
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%.20s: %v, want %q", tt.s, err, tt.want)
		}
		if err != nil && strings.Contains(err.Error(), tt.s) {
			t.Errorf("the error prints what it checks: %v", err)
		}
	}
}

func TestSealSopsFlags(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sops"), "#!/bin/sh\necho \"$*\" > \"$0.args\"\necho '{\"key\": \"ENC[x]\", \"sops\": {}}'\n")
	if err := os.Chmod(filepath.Join(dir, "sops"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	s := &Sops{Dir: dir, Environ: os.Environ(), Stack: "prod"}
	out, err := s.SealSops(bytes.Repeat([]byte{1}, 32), []map[string]string{
		{"kms": "arn:a"}, {"pgp": "FP1"}, {"kms": "arn:b+arn:role"}, {"hc_vault_transit_uri": "https://v/k"},
	})
	if err != nil || string(bytes.TrimSpace(out)) != `{"key": "ENC[x]", "sops": {}}` {
		t.Fatalf("%s: %v", out, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "sops.args"))
	want := "--config /dev/null encrypt --kms arn:a,arn:b+arn:role --pgp FP1 --hc-vault-transit https://v/k " +
		"--filename-override prod/_meta --input-type json --output-type json\n"
	if string(args) != want {
		t.Fatalf("got %q\nwant %q", args, want)
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
