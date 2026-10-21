package secret

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SopsConfig is the file in the project root where sops finds its rules.
const SopsConfig = ".sops.yaml"

// MetaName is the name OpsTruss gives sops for the _meta of a stack, so
// the rules of .sops.yaml pick the keys that wrap its data key.
func MetaName(stack string) string { return stack + "/_meta" }

// metaRegex is the path_regex of the rule that init adds: the _meta of
// every stack.
const metaRegex = `(^|/)_meta$`

// HasRule reports whether a creation rule of the .sops.yaml in dir
// matches name, as sops picks one: a rule without path_regex, or one whose
// path_regex matches.
func HasRule(dir, name string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, SopsConfig))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var conf struct {
		CreationRules []struct {
			PathRegex string `yaml:"path_regex"`
		} `yaml:"creation_rules"`
	}
	if err := yaml.Unmarshal(data, &conf); err != nil {
		return false, fmt.Errorf("%s: %w", SopsConfig, err)
	}
	for _, r := range conf.CreationRules {
		if r.PathRegex == "" {
			return true, nil
		}
		re, err := regexp.Compile(r.PathRegex)
		if err != nil {
			return false, fmt.Errorf("%s: path_regex %q: %w", SopsConfig, r.PathRegex, err)
		}
		if re.MatchString(name) {
			return true, nil
		}
	}
	return false, nil
}

// AddMetaRule adds the rule for the _meta of every stack, wrapped for
// recipient, at the end of the creation rules in the .sops.yaml in dir,
// and makes the file when there is none. Sops takes the first rule that
// matches, so a rule of the user's that already matches some _meta keeps
// it, and the comments and other rules stay.
func AddMetaRule(dir, recipient string) error {
	p := filepath.Join(dir, SopsConfig)
	data, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", SopsConfig, err)
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		// A file of comments only.
		*root = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: root.HeadComment, FootComment: root.FootComment}
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s is not a map", SopsConfig)
	}
	var rules *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "creation_rules" {
			rules = root.Content[i+1]
		}
	}
	switch {
	case rules == nil:
		rules = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content, str("creation_rules"), rules)
	case rules.Kind == yaml.ScalarNode && rules.Tag == "!!null":
		*rules = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	case rules.Kind != yaml.SequenceNode:
		return fmt.Errorf("creation_rules in %s is not a list", SopsConfig)
	}
	rules.Content = append(rules.Content, &yaml.Node{
		Kind: yaml.MappingNode, Tag: "!!map",
		HeadComment: "The data key of each OpsTruss stack, in <stack>/_meta.",
		Content:     []*yaml.Node{str("path_regex"), str(metaRegex), str("age"), str(recipient)},
	})
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(p); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(p, out.Bytes(), mode)
}

func str(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

// Sops runs the sops program on the _meta of a stack, which holds its
// data key, and on the files of secrets: from the project root, where sops
// finds .sops.yaml, with the operator's whole environment, as for git,
// since sops needs the keys and credentials that open them. OpsTruss names
// what is encrypted in _meta itself, so a rule needs only the keys.
type Sops struct {
	Dir     string   // the project root
	Environ []string // the operator's environment
	Stack   string
}

// Wrap encrypts the key field of plain, _meta as JSON, with the keys of the
// rule in .sops.yaml that matches the _meta of the stack.
func (s *Sops) Wrap(plain []byte) ([]byte, error) {
	return s.run(plain, "encrypt", "--encrypted-regex", "^key$")
}

// Unwrap decrypts _meta as sops wrapped it.
func (s *Sops) Unwrap(wrapped []byte) ([]byte, error) {
	return s.run(wrapped, "decrypt")
}

func (s *Sops) run(stdin []byte, args ...string) ([]byte, error) {
	name := MetaName(s.Stack)
	args = append(args, "--filename-override", name, "--input-type", "json", "--output-type", "json")
	out, msg, err := s.exec(bytes.NewReader(stdin), args...)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, errors.New("sops is not in PATH, and the data key of the stack is kept by sops; install it, for example with mise use sops")
	case err != nil && (strings.Contains(msg, "no matching creation rules found") || strings.Contains(msg, "has no creation rules")):
		return nil, fmt.Errorf("no rule in %s matches %s, which holds the data key of the stack; opstruss init adds one", SopsConfig, name)
	case err != nil:
		return nil, fmt.Errorf("sops %s %s: %s", args[0], name, cmp.Or(msg, err.Error()))
	}
	return out, nil
}

// treeFormats are the extensions of the files that sops reads as a tree
// of values rather than as one value of bytes.
var treeFormats = []string{".yaml", ".yml", ".json", ".env", ".ini"}

// Decrypt decrypts the sops file at path, named file in what it says, for
// a secret: a file of a format that sops reads as a tree (YAML, JSON,
// dotenv, or INI, by its extension) as JSON, and any other as its bytes.
func (s *Sops) Decrypt(path, file string) (data []byte, tree bool, err error) {
	tree = slices.Contains(treeFormats, filepath.Ext(path))
	args := []string{"decrypt"}
	if tree {
		args = append(args, "--output-type", "json")
	}
	out, msg, err := s.exec(nil, append(args, path)...)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return nil, false, errors.New("sops is not in PATH, and it decrypts the secret; install it, for example with mise use sops")
	case err != nil:
		return nil, false, fmt.Errorf("sops decrypt %s: %s", file, cmp.Or(msg, err.Error()))
	}
	return out, tree, nil
}

// exec runs sops, and returns what it printed, and when it failed, what it
// printed to stderr.
func (s *Sops) exec(stdin io.Reader, args ...string) ([]byte, string, error) {
	cmd := exec.Command("sops", args...)
	cmd.Dir, cmd.Env, cmd.Stdin = s.Dir, s.Environ, stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, strings.TrimSpace(stderr.String()), err
}

// MinSops is the oldest sops that OpsTruss works with: sops 3.10.0
// encrypts and decrypts what comes on stdin, which is how _meta goes to
// it, and 3.9.0 brought --filename-override.
const MinSops = "3.10.0"

var sopsVersion = regexp.MustCompile(`(?m)^sops (\d+)\.(\d+)\.(\d+)`)

// SopsVersion asks the sops at path for its version, without asking the
// network for the latest one, and reports whether it is older than
// MinSops.
func SopsVersion(path string, environ []string) (version string, old bool, err error) {
	cmd := exec.Command(path, "--version", "--disable-version-check")
	cmd.Env = environ
	out, err := cmd.Output()
	m := sopsVersion.FindStringSubmatch(string(out))
	if m == nil {
		// Before 3.8.0, sops does not know --disable-version-check.
		return "", true, fmt.Errorf("%s --version gives no version it knows (%v)", path, cmp.Or(err, errors.New("no sops x.y.z line")))
	}
	version = m[1] + "." + m[2] + "." + m[3]
	min := sopsVersion.FindStringSubmatch("sops " + MinSops)
	for i := 1; i <= 3; i++ {
		a, _ := strconv.Atoi(m[i])
		b, _ := strconv.Atoi(min[i])
		if a != b {
			return version, a < b, nil
		}
	}
	return version, false, nil
}
