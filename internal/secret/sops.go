package secret

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

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
