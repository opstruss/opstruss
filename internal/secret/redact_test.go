package secret

import (
	"errors"
	"io/fs"
	"testing"
)

func TestRedactor(t *testing.T) {
	r := NewRedactor()
	if short := r.Add("s3cr3t-token"); short {
		t.Fatal("s3cr3t-token is long enough")
	}
	if short := r.Add("abc"); !short {
		t.Fatal("abc is too short to hide")
	}
	r.Add("line-one\nline-two\n}")
	r.Add(`pa"ss\word`)
	r.Add(`to<"ken&`)
	r.Add("abcd")
	r.Add("abcd1234")
	r.Add(map[string]any{"user": "admin-user", "port": int64(5432), "tls": true})

	for _, tt := range []struct{ in, want string }{
		{"token=s3cr3t-token;", "token=***;"},
		{"abc stays", "abc stays"},
		{"a line-one b line-two }", "a *** b *** }"},
		{`{"k": "line-one\nline-two"}`, `{"k": "***\n***"}`},
		{`{"p": "pa\"ss\\word"}`, `{"p": "***"}`},
		{`output p: "pa\"ss\\word" is not an integer`, `output p: "***" is not an integer`},
		{`output t: "to<\"ken&" is not an integer`, `output t: "***" is not an integer`},
		{`{"t": "to\u003c\"ken\u0026"}`, `{"t": "***"}`},
		{"pa\"ss\\word<&>", "***<&>"},
		{"abcd1234 abcd", "*** ***"},
		{"admin-user on 5432 true", "*** on *** true"},
	} {
		if got := r.String(tt.in); got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}

	r.AddDerived("Bearer s3cr3t-token")
	r.AddDerived("czNjcjN0LXRva2Vu")
	if got := r.String("Authorization: Bearer s3cr3t-token czNjcjN0LXRva2Vu"); got != "Authorization: Bearer *** ***" {
		t.Errorf("derived values: %q", got)
	}

	v := r.Value(map[string]any{"a": []any{"x s3cr3t-token", int64(5432)}, "s3cr3t-token": true})
	m := v.(map[string]any)
	if m["a"].([]any)[0] != "x ***" || m["a"].([]any)[1] != Mask || m["***"] != true {
		t.Errorf("Value: %v", v)
	}
	if !r.Contains(map[string]any{"a": []any{"s3cr3t-token"}}) || r.Contains("nothing here") {
		t.Error("Contains")
	}

	err := r.Err(&fs.PathError{Op: "open", Path: "s3cr3t-token", Err: fs.ErrNotExist})
	if err.Error() != "open ***: file does not exist" || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Err: %v", err)
	}
}
