package engine

import (
	"testing"

	"github.com/opstruss/opstruss/internal/model"
	"github.com/opstruss/opstruss/internal/state"
)

func TestKeysDiff(t *testing.T) {
	dk := &state.DataKey{
		Age:  &state.AgeEnvelope{To: []string{"age1a", "age1b"}},
		Sops: &state.SopsEnvelope{To: []map[string]string{{"kms": "arn:x"}}},
	}
	keys := func(entries ...*model.EncryptionKey) *engine {
		return &engine{p: &model.Project{File: model.File{EncryptionKeys: entries}}}
	}
	for _, tt := range []struct {
		e    *engine
		want string
	}{
		{keys(), ""}, // without encryption_keys, _meta stays as it is
		{keys(&model.EncryptionKey{Age: "age1b"}, &model.EncryptionKey{Sops: map[string]string{"kms": "arn:x"}}, &model.EncryptionKey{Age: "age1a"}), ""},
		{keys(&model.EncryptionKey{Age: "age1a"}, &model.EncryptionKey{Age: "age1c"}), "+age1c, -age1b, -kms arn:x"},
		{keys(&model.EncryptionKey{Age: "age1a"}, &model.EncryptionKey{Age: "age1b"}, &model.EncryptionKey{Sops: map[string]string{"pgp": "FP"}}), "+pgp FP, -kms arn:x"},
	} {
		if got := tt.e.keysDiff(dk); got != tt.want {
			t.Errorf("keysDiff = %q, want %q", got, tt.want)
		}
	}
	if got := wrappedFor(dk); got != "with age for age1a, age1b, and with sops for kms arn:x" {
		t.Errorf("wrappedFor = %q", got)
	}
}
