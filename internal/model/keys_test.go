package model

import (
	"fmt"
	"strings"
	"testing"

	"filippo.io/age"
)

func TestEncryptionKeys(t *testing.T) {
	id1, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := id1.Recipient().String(), id2.Recipient().String()
	dir := write(t, map[string]string{
		"opstruss.yaml": fmt.Sprintf(`
name: app
encryption_keys:
  - %s
  - kms: arn:aws:kms:eu-west-1:111122223333:key/a+arn:aws:iam::111122223333:role/ci
stacks:
  dev: {}
  prod:
    encryption_keys: [%s]
units:
  db:
    uses: ./db
`, r1, r2),
		"db/opstruss.yaml": fmt.Sprintf("encryption_keys: [%s]\nunits:\n  x:\n    run: [x]\n", r2),
	})
	dev, err := Load(dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(dev.EncryptionKeys) != 2 || dev.EncryptionKeys[0].Age != r1 ||
		dev.EncryptionKeys[1].Sops["kms"] != "arn:aws:kms:eu-west-1:111122223333:key/a+arn:aws:iam::111122223333:role/ci" {
		t.Fatalf("dev: %+v", dev.EncryptionKeys)
	}
	prod, err := Load(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(prod.EncryptionKeys) != 1 || prod.EncryptionKeys[0].Age != r2 {
		t.Fatalf("a stack replaces the list as a whole: %+v", prod.EncryptionKeys)
	}

	secretKey := id1.String()
	tests := []struct{ keys, want string }{
		{"[]", "encryption_keys lists at least one key; without it, the data key is wrapped for the age key of the machine that makes it"},
		{"[" + secretKey + "]", "this is an age secret key, which never goes into a file of the project"},
		{"[" + strings.ToLower(secretKey) + "]", "this is an age secret key"},
		{"['ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMq']", "ssh recipients are not supported yet"},
		{"[age1yubikey1qwt50d05nh5vutpdzmlg5wn80xq5negm4uj9ghv0snvdd3yysf5yw3rhl3t]", "recipients of age plugins and post-quantum ones are not supported yet"},
		{"[age1pq1qqqqqqqqqqqqqqqqqqqqqqqq]", "recipients of age plugins and post-quantum ones are not supported yet"},
		{"[" + r1[:len(r1)-1] + "]", "this is not a whole age recipient: its length or checksum is wrong"},
		{"[example]", "this is not an age recipient"},
		{"[" + r1 + ", " + r1 + "]", "encryption_keys[1]: this key is in encryption_keys already"},
		{"[{kms: a}, {kms: a}]", "encryption_keys[1]: this key is in encryption_keys already"},
		{"['${{ inputs.key }}']", "encryption_keys are read before any expression, so they hold no ${{ }}"},
		{"[{kms: a, pgp: b}]", "an entry of encryption_keys that sops wraps for has one key: kms, gcp_kms, azure_keyvault, hc_vault_transit_uri, or pgp"},
		{"[{}]", "an entry of encryption_keys that sops wraps for has one key"},
		{"[{kmz: a}]", "kmz is not a key that sops wraps for"},
		{"[{kms: ''}]", "minLength"},
	}
	for _, tt := range tests {
		t.Run(tt.keys, func(t *testing.T) {
			dir := write(t, map[string]string{"opstruss.yaml": "name: app\nencryption_keys: " + tt.keys + "\n"})
			got := problems(t, dir, "")
			contains(t, got, tt.want)
			if strings.Contains(strings.Join(got, "\n"), secretKey[len("AGE-SECRET-KEY-1"):]) ||
				strings.Contains(strings.Join(got, "\n"), strings.ToLower(secretKey[len("AGE-SECRET-KEY-1"):])) {
				t.Fatalf("a problem prints the secret key: %v", got)
			}
		})
	}
	dir = write(t, map[string]string{"opstruss.yaml": "name: app\nstacks:\n  prod:\n    encryption_keys: []\n"})
	contains(t, problems(t, dir, "prod"), "stacks.prod.encryption_keys: encryption_keys lists at least one key")
}

func TestSecretSourceTypo(t *testing.T) {
	dir := write(t, map[string]string{"opstruss.yaml": `
name: app
secrets:
  db: {sopz: db.sops.yaml, field: password}
  token: {evn: TOKEN}
  other: {vualt: x}
stacks:
  prod:
    secrets:
      db: {sop: prod.sops.yaml}
tasks:
  secrets:
    inputs:
      x: 5
    run: [x]
`})
	got := problems(t, dir, "prod")
	contains(t, got, "secrets.db.sopz: sopz is not a source of a secret: sops, env, run, or a call of a task of a plugin, which takes a map; did you mean sops?")
	contains(t, got, "secrets.token.evn: evn is not a source of a secret: sops, env, run, or a call of a task of a plugin, which takes a map; did you mean env?")
	contains(t, got, "secrets.other.vualt: vualt is not a source of a secret: sops, env, run, or a call of a task of a plugin, which takes a map")
	contains(t, got, "stacks.prod.secrets.db.sop: sop is not a source of a secret")
	contains(t, got, "tasks.secrets.inputs.x: expected object")
	for _, p := range got {
		if strings.Contains(p, "vualt") && strings.Contains(p, "did you mean") {
			t.Fatalf("vualt is no typo of a source: %s", p)
		}
	}
}
