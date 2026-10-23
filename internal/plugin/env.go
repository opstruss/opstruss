package plugin

import (
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
)

// Names of the contract files in the directory of a call.
const (
	outputFile = "output"
	resultFile = "result"
	planFile   = "plan"
	inputsFile = "inputs.json"
	stateFile  = "state.json"
)

// clean is the environment every program starts from: what tools need to
// find programs, the user, the locale, the terminal, proxies, certificates,
// and the SSH agent, and nothing else of the operator's.
func clean(operator map[string]string) map[string]string {
	base := map[string]string{}
	for k, v := range operator {
		if basic(k) {
			base[k] = v
		}
	}
	return base
}

func basic(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LANG", "TZ", "TMPDIR", "TERM", "SSH_AUTH_SOCK",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy":
		return true
	}
	return strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "SSL_CERT_")
}

// passes reports whether a name or pattern in pass lets the operator's
// variable name through. The variables of the contract never pass.
func passes(pass []string, name string) bool {
	if model.ReservedEnv(name) {
		return false
	}
	for _, p := range pass {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// environment builds the environment of a call: the clean base, what pass
// lets through, and env, then the facts and contract files of its kind of
// program, which only the engine sets; a program that lives outside the
// run gets neither the facts of the run nor its files. Every file goes
// through write, which says where it is.
func (r *Run) environment(c *Call, write func(name string, data []byte) (string, error), inRun bool) (map[string]string, error) {
	env := maps.Clone(r.base)
	for k, v := range r.operator {
		if passes(c.Pass, k) {
			env[k] = v
		}
	}
	for k, v := range c.Env {
		if model.ReservedEnv(k) {
			return nil, fmt.Errorf("env: cannot set %s, which comes from OpsTruss", k)
		}
		if !v.File {
			env[k] = v.Text
			continue
		}
		p, err := write("env-"+k, []byte(v.Text))
		if err != nil {
			return nil, err
		}
		env[k] = p
	}
	if r.Passed != nil {
		for _, k := range slices.Sorted(maps.Keys(r.operator)) {
			if _, set := c.Env[k]; !set && passes(c.Pass, k) {
				r.Passed(k, r.operator[k])
			}
		}
	}

	env["OPSTRUSS_CONTRACT"] = Contract
	env["OPSTRUSS_PROJECT"] = r.Project
	env["OPSTRUSS_STACK"] = r.Stack
	if u := c.Unit; u != nil {
		env["OPSTRUSS_ADDRESS"] = u.Address
		env["OPSTRUSS_STATE_KEY"] = u.StateKey
	}
	if !inRun {
		// A program outside a run gets its inputs as INPUT_* only.
		for name, v := range c.Inputs {
			if s := expr.Text(v); v != nil && len(s) <= MaxInputEnv {
				env[model.InputEnv(name)] = s
			}
		}
		return env, nil
	}
	files := map[string]string{"OPSTRUSS_OUTPUT": outputFile, "OPSTRUSS_RESULT": resultFile}
	if c.Plan {
		files["OPSTRUSS_PLAN"] = planFile
	}
	for k, name := range files {
		p, err := write(name, nil)
		if err != nil {
			return nil, err
		}
		env[k] = p
	}
	if c.Inputs != nil {
		p, err := write(inputsFile, []byte(expr.JSON(c.Inputs)))
		if err != nil {
			return nil, err
		}
		env["OPSTRUSS_INPUTS"] = p
		for name, v := range c.Inputs {
			if s := expr.Text(v); v != nil && len(s) <= MaxInputEnv {
				env[model.InputEnv(name)] = s
			}
		}
	}
	env["OPSTRUSS_RUN"] = r.ID
	env["OPSTRUSS_RUN_PARENTS"] = strings.Join(r.Parents, ",")
	env["OPSTRUSS_TASK"] = c.Task
	env["OPSTRUSS_REASON"] = c.Reason
	env["OPSTRUSS_TEMP"] = r.Temp
	if !c.Secret {
		env["OPSTRUSS_ARTIFACTS"] = r.Artifacts
	}
	if c.Digest != "" {
		env["OPSTRUSS_DIGEST"] = c.Digest
	}
	if c.PluginDir != "" {
		env["OPSTRUSS_PLUGIN_DIR"] = c.PluginDir
	}
	if u := c.Unit; u != nil {
		if u.Workdir != "" {
			if err := os.MkdirAll(u.Workdir, 0o700); err != nil {
				return nil, err
			}
			env["OPSTRUSS_WORKDIR"] = u.Workdir
		}
		if u.StateDir != "" {
			if err := os.MkdirAll(u.StateDir, 0o700); err != nil {
				return nil, err
			}
			env["OPSTRUSS_STATE_DIR"] = u.StateDir
		}
		if u.State != nil {
			p, err := write(stateFile, []byte(expr.JSON(u.State)))
			if err != nil {
				return nil, err
			}
			env["OPSTRUSS_STATE"] = p
		}
	}
	return env, nil
}
