package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// fakesops stands in for the sops program where a test has no cloud. It
// encrypts what comes on stdin for the keys of --kms, --gcp-kms,
// --azure-kv, --hc-vault-transit, and --pgp, and decrypts it again when
// $FAKESOPS_KEYS, a comma-separated list, names one of those keys, as the
// access to a key of a KMS would:
//
//	fakesops [--config <file>] encrypt <key flags> --filename-override <name> --input-type json --output-type json
//	fakesops [--config <file>] decrypt --filename-override <name> --input-type json --output-type json
//	fakesops --version
//
// It encrypts nothing for real: each value becomes ENC[fake,<base64>], and
// the keys go into sops.fake. Encrypting also needs access to each key.
// Each call goes to $FAKESOPS_LOG as one line: the verb and the keys.
func fakesops(args []string) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "fakesops: "+format+"\n", a...)
		return code
	}
	if slices.Contains(args, "--version") {
		fmt.Println("sops 3.13.3 (fakesops)")
		return 0
	}
	flags := map[string]string{"--kms": "kms", "--gcp-kms": "gcp_kms", "--azure-kv": "azure_keyvault",
		"--hc-vault-transit": "hc_vault_transit_uri", "--pgp": "pgp"}
	var verb string
	var keys []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "encrypt" || a == "decrypt":
			verb = a
		case flags[a] != "" && i+1 < len(args):
			for _, k := range strings.Split(args[i+1], ",") {
				keys = append(keys, flags[a]+"="+k)
			}
			i++
		case strings.HasPrefix(a, "--") && i+1 < len(args):
			i++ // --config, --filename-override, --input-type, --output-type
		default:
			return fail(1, "unknown argument %s", a)
		}
	}
	access := strings.Split(os.Getenv("FAKESOPS_KEYS"), ",")
	can := func(key string) bool {
		_, k, _ := strings.Cut(key, "=")
		return slices.Contains(access, k)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail(1, "%v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fail(1, "stdin: %v", err)
	}
	out := map[string]any{}
	switch verb {
	case "encrypt":
		if len(keys) == 0 {
			return fail(1, "no keys")
		}
		for _, k := range keys {
			if !can(k) {
				return fail(128, "AccessDeniedException: no access to %s", k)
			}
		}
		for name, v := range doc {
			b, _ := json.Marshal(v)
			out[name] = "ENC[fake," + base64.StdEncoding.EncodeToString(b) + "]"
		}
		out["sops"] = map[string]any{"fake": keys}
	case "decrypt":
		meta, _ := doc["sops"].(map[string]any)
		list, _ := meta["fake"].([]any)
		for _, k := range list {
			keys = append(keys, fmt.Sprint(k))
		}
		if !slices.ContainsFunc(keys, can) {
			return fail(128, "Failed to get the data key required to decrypt the SOPS file.")
		}
		for name, v := range doc {
			s, _ := v.(string)
			enc, ok := strings.CutPrefix(s, "ENC[fake,")
			if name == "sops" || !ok {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(enc, "]"))
			if err != nil {
				return fail(1, "%s: %v", name, err)
			}
			var value any
			if err := json.Unmarshal(b, &value); err != nil {
				return fail(1, "%s: %v", name, err)
			}
			out[name] = value
		}
	default:
		return fail(1, "usage: fakesops [--config <file>] encrypt|decrypt [flags]")
	}
	if log := os.Getenv("FAKESOPS_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(1, "%v", err)
		}
		_, _ = fmt.Fprintf(f, "%s %s\n", verb, strings.Join(keys, " "))
		_ = f.Close()
	}
	b, err := json.Marshal(out)
	if err != nil {
		return fail(1, "%v", err)
	}
	fmt.Println(string(b))
	return 0
}
