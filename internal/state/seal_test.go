package state

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSeal(t *testing.T) {
	key := bytes.Repeat([]byte{7}, KeySize)
	ct, err := Seal(key, "7f3a9c1e", map[string]any{"token": "t0k3n", "n": int64(3)})
	if err != nil || !IsSealed(ct) {
		t.Fatalf("Seal: %q %v", ct, err)
	}
	v, err := Unseal(key, "7f3a9c1e", ct)
	if err != nil || v.(map[string]any)["n"] != int64(3) {
		t.Fatalf("Unseal: %v %v", v, err)
	}
	if _, err := Unseal(key, "other", ct); err == nil {
		t.Error("a value opens for another record")
	}
	a, _ := DigestKey(key, "7f3a9c1e")
	b, _ := DigestKey(key, "7f3a9c1e")
	c, _ := DigestKey(key, "other")
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Error("DigestKey")
	}
}

func TestSealRecord(t *testing.T) {
	key := bytes.Repeat([]byte{9}, KeySize)
	asked := 0
	s := &Store{Key: func() ([]byte, error) { asked++; return key, nil }}
	r := &Record{ID: "7f3a9c1e", Inputs: map[string]any{"token": "t0k3n", "zone": "a"},
		Outputs: map[string]any{"id": "x", "pin": "1234"},
		Destroy: &Destroy{Env: map[string]string{"T": "t0k3n", "U": "u"}},
		Hidden:  map[string][]string{PartInputs: {"token"}, PartOutputs: {"pin"}, PartEnv: {"T"}},
		Pending: &Attempt{Op: "up", Inputs: map[string]any{"token": "t0k3n"}, RawOutput: "pin=1234",
			Hidden: map[string][]string{PartInputs: {"token"}}}}
	data, err := s.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "t0k3n") || strings.Contains(string(data), "1234") {
		t.Fatalf("plain text in %s", data)
	}
	if r.Inputs["token"] != "t0k3n" || r.Destroy.Env["T"] != "t0k3n" {
		t.Fatal("Encode changed the record it was given")
	}
	got, err := DecodeRecord(data, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.openRecord(got); err != nil {
		t.Fatal(err)
	}
	if got.Inputs["token"] != "t0k3n" || got.Outputs["pin"] != "1234" || got.Destroy.Env["T"] != "t0k3n" ||
		got.Pending.RawOutput != "pin=1234" || got.Hidden[PartEnv][0] != "T" || got.Sensitive != nil {
		t.Fatalf("openRecord: %+v", got)
	}
	plain := &Store{}
	if _, err := plain.Encode(&Record{ID: "1", Inputs: map[string]any{"a": "b"}}); err != nil {
		t.Errorf("a record without sensitive values needs no key: %v", err)
	}
	var keyErr *KeyError
	if _, err := plain.Encode(r); !errors.As(err, &keyErr) {
		t.Errorf("a record with sensitive values without a key: %v", err)
	}
}
