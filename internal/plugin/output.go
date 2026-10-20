package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/opstruss/opstruss/internal/expr"
	"github.com/opstruss/opstruss/internal/model"
)

// collect reads what the program wrote back: the contract files in dir,
// and each declared output from them, a stream, or a file. A failed
// attempt keeps what could be read, with the reasons in OutputErr.
func (r *Run) collect(c *Call, dir string, res *Result, streams map[string]*stream) {
	var errs []error
	fail := func(what string, err error) { errs = append(errs, fmt.Errorf("%s: %w", what, err)) }

	raw, err := readMax(filepath.Join(dir, outputFile), MaxOutputs)
	res.RawOutput = raw
	var written map[string]any
	if err != nil {
		fail("OPSTRUSS_OUTPUT", err)
	} else {
		var warnings []string
		written, warnings, err = ParseOutputFile(raw, c.Outputs)
		res.Warnings = append(res.Warnings, warnings...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	res.Outputs = map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(c.Outputs)) {
		o := c.Outputs[name]
		if o.Value != nil {
			continue
		}
		v, err := output(c, o, written[name], streams)
		if err != nil {
			fail("output "+name, err)
		}
		res.Outputs[name] = v
	}
	if n := len(expr.JSON(res.Outputs)); n > MaxOutputs {
		errs = append(errs, fmt.Errorf("the outputs take %d bytes, more than %s", n, size(MaxOutputs)))
	}

	res.Changed = true
	if data, err := readMax(filepath.Join(dir, resultFile), MaxResult); err != nil {
		fail("OPSTRUSS_RESULT", err)
	} else if err := parseResult(data, res); err != nil {
		fail("OPSTRUSS_RESULT", err)
	}
	if c.Plan {
		data, err := readMax(filepath.Join(dir, planFile), MaxOutputs)
		res.Plan = data
		if err != nil {
			fail("OPSTRUSS_PLAN", err)
		}
	}
	res.OutputErr = errors.Join(errs...)
}

// output reads one declared output, typed by its declaration: what the
// program wrote to OPSTRUSS_OUTPUT, a stream, or a file relative to the
// file that declares it. A file that is not there is null.
func output(c *Call, o *model.Output, written any, streams map[string]*stream) (any, error) {
	v := written
	switch o.From {
	case "", "output":
	case "stdout", "stderr":
		data := streams[o.From].buf
		if len(data) > MaxOutputs {
			return nil, fmt.Errorf("%s holds more than %s", o.From, size(MaxOutputs))
		}
		var err error
		if v, err = Decode(data, o.Format); err != nil {
			return nil, err
		}
	case "file":
		data, err := readMax(resolve(c.Base, o.Path), MaxOutputs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if v, err = Decode(data, o.Format); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown from %s", o.From)
	}
	return (&model.Input{Type: o.Type}).Conform(v)
}

// ParseOutputFile reads OPSTRUSS_OUTPUT: lines name=value and blocks
// name<<DELIM … DELIM, as $GITHUB_OUTPUT, whose text becomes the type of
// its output, or one JSON object, whose types are kept. Names that no
// output declares are left out, with a warning.
func ParseOutputFile(data []byte, decl map[string]*model.Output) (map[string]any, []string, error) {
	text := strings.ToValidUTF8(string(data), "�")
	values := map[string]any{}
	var errs []error
	if t := strings.TrimSpace(text); strings.HasPrefix(t, "{") {
		v, err := expr.FromJSON([]byte(t))
		obj, ok := v.(map[string]any)
		if err != nil || !ok {
			return nil, nil, fmt.Errorf("OPSTRUSS_OUTPUT starts with { but is not one JSON object")
		}
		values = obj
	} else {
		lines, err := parseLines(text)
		if err != nil {
			return nil, nil, err
		}
		for k, s := range lines {
			v := any(s)
			if o := decl[k]; o != nil {
				if v, err = model.ParseText(s, o.Type); err != nil {
					errs = append(errs, fmt.Errorf("output %s: %w", k, err))
					continue
				}
			}
			values[k] = v
		}
	}
	var warnings []string
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if decl[k] == nil {
			warnings = append(warnings, fmt.Sprintf("OPSTRUSS_OUTPUT has %s, which no output declares", k))
			delete(values, k)
		}
	}
	return values, warnings, errors.Join(errs...)
}

// parseLines reads name=value lines and name<<DELIM blocks; the last value
// of a name wins.
func parseLines(text string) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		eq, hd := strings.Index(line, "="), strings.Index(line, "<<")
		if hd > 0 && (eq < 0 || hd < eq) {
			name, delim, start := line[:hd], line[hd+2:], i
			if delim == "" {
				return nil, fmt.Errorf("OPSTRUSS_OUTPUT line %d: %s<< has no delimiter", i+1, name)
			}
			var body []string
			for i++; i < len(lines) && strings.TrimSuffix(lines[i], "\r") != delim; i++ {
				body = append(body, strings.TrimSuffix(lines[i], "\r"))
			}
			if i == len(lines) {
				return nil, fmt.Errorf("OPSTRUSS_OUTPUT line %d: %s<<%s has no line %s that ends it", start+1, name, delim, delim)
			}
			out[name] = strings.Join(body, "\n")
			continue
		}
		if eq <= 0 {
			return nil, fmt.Errorf("OPSTRUSS_OUTPUT line %d is neither name=value nor name<<DELIM", i+1)
		}
		out[line[:eq]] = line[eq+1:]
	}
	return out, nil
}

// parseResult reads OPSTRUSS_RESULT: {"changed": false, "message": "…",
// "sensitive": ["<output>"]}. Without changed, the program changed
// something; fields this OpsTruss does not know are ignored.
func parseResult(data []byte, res *Result) error {
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	var r struct {
		Changed   *bool    `json:"changed"`
		Message   string   `json:"message"`
		Sensitive []string `json:"sensitive"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	if r.Changed != nil {
		res.Changed = *r.Changed
	}
	res.Message, res.Sensitive = r.Message, r.Sensitive
	return nil
}

// readMax reads a file of at most max bytes; a longer one gives its first
// max bytes and an error.
func readMax(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return data[:max], fmt.Errorf("holds more than %s", size(max))
	}
	return data, nil
}

func size(n int) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", n>>10)
}
