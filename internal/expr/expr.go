package expr

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
	"go.yaml.in/yaml/v3"
)

// Namespaces are the variables an expression can read; where each one may
// be read is checked from the references of the expression.
var Namespaces = []string{"inputs", "locals", "units", "tasks", "steps", "secrets", "stack", "each", "event", "opstruss", "outputs"}

// MaxFile is the most that file() reads, the limit of a value.
const MaxFile = 1 << 20

var env = sync.OnceValues(func() (*cel.Env, error) {
	opts := []cel.EnvOption{
		cel.CustomTypeAdapter(adapter{}),
		// Library versions are pinned, so upgrading cel-go never changes
		// what an expression written earlier returns.
		ext.Strings(ext.StringsVersion(5)),
		ext.Lists(ext.ListsVersion(3)),
		ext.Math(ext.MathVersion(3)),
		ext.Sets(ext.SetsVersion(0)),
		ext.TwoVarComprehensions(ext.TwoVarComprehensionsVersion(0)),
		ext.Bindings(ext.BindingsVersion(1)),
		ext.Encoders(ext.EncodersVersion(1)),
		cel.OptionalTypes(cel.OptionalTypesVersion(2)),
		unary("toJSON", cel.DynType, cel.StringType, func(v ref.Val) ref.Val {
			n, err := Native(v)
			if err != nil {
				return types.NewErr("toJSON: %v", err)
			}
			return types.String(JSON(n))
		}),
		unary("fromJSON", cel.StringType, cel.DynType, func(v ref.Val) ref.Val {
			n, err := FromJSON([]byte(v.(types.String)))
			if err != nil {
				return types.NewErr("fromJSON: %v", err)
			}
			return adapter{}.NativeToValue(n)
		}),
		unary("toYAML", cel.DynType, cel.StringType, func(v ref.Val) ref.Val {
			n, err := Native(v)
			if err != nil {
				return types.NewErr("toYAML: %v", err)
			}
			out, err := ToYAML(n)
			if err != nil {
				return types.NewErr("toYAML: %v", err)
			}
			return types.String(out)
		}),
		unary("fromYAML", cel.StringType, cel.DynType, func(v ref.Val) ref.Val {
			n, err := FromYAML([]byte(v.(types.String)))
			if err != nil {
				return types.NewErr("fromYAML: %v", err)
			}
			return adapter{}.NativeToValue(n)
		}),
		unary("sha256", cel.StringType, cel.StringType, func(v ref.Val) ref.Val {
			sum := sha256.Sum256([]byte(v.(types.String)))
			return types.String(hex.EncodeToString(sum[:]))
		}),
		cel.Function("kv",
			cel.Overload("kv_string", []*cel.Type{cel.StringType}, cel.DynType,
				cel.UnaryBinding(func(ref.Val) ref.Val { return types.NewErr("kv() is not supported yet") })),
			cel.Overload("kv_string_dyn", []*cel.Type{cel.StringType, cel.DynType}, cel.DynType,
				cel.BinaryBinding(func(ref.Val, ref.Val) ref.Val { return types.NewErr("kv() is not supported yet") }))),
		cel.Function("strftime",
			cel.MemberOverload("timestamp_strftime", []*cel.Type{cel.TimestampType, cel.StringType}, cel.StringType,
				cel.BinaryBinding(func(t, f ref.Val) ref.Val {
					return strftimeVal(t, f, types.String("UTC"))
				})),
			cel.MemberOverload("timestamp_strftime_zone", []*cel.Type{cel.TimestampType, cel.StringType, cel.StringType}, cel.StringType,
				cel.FunctionBinding(func(args ...ref.Val) ref.Val {
					return strftimeVal(args[0], args[1], args[2])
				}))),
	}
	for _, n := range Namespaces {
		opts = append(opts, cel.Variable(n, cel.DynType))
	}
	return cel.NewEnv(opts...)
})

func unary(name string, in, out *cel.Type, fn func(ref.Val) ref.Val) cel.EnvOption {
	return cel.Function(name, cel.Overload(name+"_"+in.String(), []*cel.Type{in}, out, cel.UnaryBinding(fn)))
}

func strftimeVal(t, f, zone ref.Val) ref.Val {
	loc, err := time.LoadLocation(string(zone.(types.String)))
	if err != nil {
		return types.NewErr("strftime: %v", err)
	}
	s, err := strftime(t.(types.Timestamp).In(loc), string(f.(types.String)))
	if err != nil {
		return types.NewErr("strftime: %v", err)
	}
	return types.String(s)
}

var envs sync.Map

// envFor returns the environment whose file() reads relative to dir, the
// directory of the file that wrote the expression.
func envFor(dir string) (*cel.Env, error) {
	if e, ok := envs.Load(dir); ok {
		return e.(*cel.Env), nil
	}
	base, err := env()
	if err != nil {
		return nil, err
	}
	e, err := base.Extend(unary("file", cel.StringType, cel.StringType, readFile(dir)))
	if err != nil {
		return nil, err
	}
	envs.Store(dir, e)
	return e, nil
}

type programKey struct{ src, dir string }

var programs sync.Map

// program compiles src for the file in dir.
func program(src, dir string) (cel.Program, error) {
	key := programKey{src, dir}
	if p, ok := programs.Load(key); ok {
		return p.(cel.Program), nil
	}
	e, err := envFor(dir)
	if err != nil {
		return nil, err
	}
	checked, iss := e.Compile(src)
	if iss.Err() != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", src, iss.Err())
	}
	p, err := e.Program(checked, cel.CustomDecoratorV2(sortLiterals))
	if err != nil {
		return nil, err
	}
	programs.Store(key, p)
	return p, nil
}

func readFile(dir string) func(ref.Val) ref.Val {
	return func(v ref.Val) ref.Val {
		name := string(v.(types.String))
		if !filepath.IsAbs(name) {
			name = filepath.Join(dir, name)
		}
		f, err := os.Open(name)
		if err != nil {
			return types.NewErr("file: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return types.NewErr("file: %v", err)
		}
		if len(data) > MaxFile {
			return types.NewErr("file: %s is larger than 1 MiB", v)
		}
		return types.String(strings.ToValidUTF8(string(data), string(utf8.RuneError)))
	}
}

// Scope is the data an expression reads, and the directory of the file
// that wrote it.
type Scope struct {
	Vars map[string]any
	Dir  string
}

// With returns a copy of s in which name holds v.
func (s *Scope) With(name string, v any) *Scope {
	vars := make(map[string]any, len(s.Vars)+1)
	for k, x := range s.Vars {
		vars[k] = x
	}
	vars[name] = v
	return &Scope{Vars: vars, Dir: s.Dir}
}

func (s *Scope) activation() map[string]any {
	a := make(map[string]any, len(Namespaces))
	for _, n := range Namespaces {
		if v, ok := s.Vars[n]; ok {
			a[n] = v
		} else {
			a[n] = map[string]any{}
		}
	}
	return a
}

// Eval evaluates the source of one expression.
func Eval(src string, s *Scope) (any, error) {
	p, err := program(src, s.Dir)
	if err != nil {
		return nil, err
	}
	out, _, err := p.Eval(s.activation())
	if err != nil {
		return nil, fmt.Errorf("${{ %s }}: %w", src, err)
	}
	return Native(out)
}

// EvalString evaluates the expressions in str. A string that is exactly
// one expression keeps the expression's type; inside text, numbers and
// booleans become text, and arrays, objects, and null are refused.
func EvalString(str string, s *Scope) (any, error) {
	parts, err := Split(str)
	if err != nil {
		return nil, err
	}
	if len(parts) == 1 && parts[0].IsExpr {
		return Eval(parts[0].Expr, s)
	}
	var b strings.Builder
	for _, p := range parts {
		if !p.IsExpr {
			b.WriteString(p.Text)
			continue
		}
		v, err := Eval(p.Expr, s)
		if err != nil {
			return nil, err
		}
		switch v.(type) {
		case nil, []any, map[string]any:
			return nil, fmt.Errorf("${{ %s }} is %s inside text: write the whole value as one ${{ }}, or use toJSON()", p.Expr, TypeName(v))
		}
		b.WriteString(Text(v))
	}
	return b.String(), nil
}

// EvalText evaluates a value that is always a string, such as an env value
// or an argv item.
func EvalText(str string, s *Scope) (string, error) {
	v, err := EvalString(str, s)
	if err != nil {
		return "", err
	}
	switch v.(type) {
	case nil, []any, map[string]any:
		return "", fmt.Errorf("%q is %s, and env values and argv items are strings: use toJSON()", str, TypeName(v))
	}
	return Text(v), nil
}

// EvalValue evaluates every expression in a string, map, or list.
func EvalValue(v any, s *Scope) (any, error) {
	switch x := v.(type) {
	case string:
		return EvalString(x, s)
	case map[string]any:
		out := make(map[string]any, len(x))
		for _, k := range sortedKeys(x) {
			e, err := EvalValue(x[k], s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = e
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := EvalValue(e, s)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// EvalCondition evaluates a condition key: a boolean, or an expression
// that may be written without ${{ }}.
func EvalCondition(v any, s *Scope) (bool, error) {
	var r any
	var err error
	switch x := v.(type) {
	case bool:
		return x, nil
	case string:
		if strings.Contains(x, "${{") {
			r, err = EvalString(x, s)
		} else {
			r, err = Eval(x, s)
		}
	default:
		return false, fmt.Errorf("a condition is a boolean or an expression, not %s", TypeName(v))
	}
	if err != nil {
		return false, err
	}
	b, ok := r.(bool)
	if !ok {
		return false, fmt.Errorf("the condition %q is %s, not a boolean", v, TypeName(r))
	}
	return b, nil
}

// ConditionSource returns the CEL source of a condition key.
func ConditionSource(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	if strings.Contains(s, "${{") {
		return Whole(s)
	}
	return s, true
}

// Native turns a CEL value into the engine's types: map[string]any, []any,
// string, bool, nil, int64, and float64. Timestamps become RFC 3339 text.
func Native(v ref.Val) (any, error) {
	switch x := v.(type) {
	case types.Null:
		return nil, nil
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return int64(x), nil
	case types.Uint:
		if uint64(x) <= math.MaxInt64 {
			return int64(x), nil
		}
		return float64(x), nil
	case types.Double:
		return float64(x), nil
	case types.String:
		return string(x), nil
	case types.Bytes:
		return string(x), nil
	case types.Timestamp:
		return x.Time.UTC().Format(time.RFC3339Nano), nil
	case types.Duration:
		return x.String(), nil
	case traits.Mapper:
		out := map[string]any{}
		it := x.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			ks, ok := k.(types.String)
			if !ok {
				return nil, fmt.Errorf("map key %v is not a string", k)
			}
			e, err := Native(x.Get(k))
			if err != nil {
				return nil, err
			}
			out[string(ks)] = e
		}
		return out, nil
	case traits.Lister:
		n := int64(x.Size().(types.Int))
		out := make([]any, n)
		for i := range n {
			e, err := Native(x.Get(types.Int(i)))
			if err != nil {
				return nil, err
			}
			out[i] = e
		}
		return out, nil
	}
	if o, ok := v.(interface{ GetValue() ref.Val }); ok {
		return Native(o.GetValue())
	}
	return nil, fmt.Errorf("a value of type %s cannot leave an expression", v.Type())
}

// TypeName names the type of a value the way schemas do.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case int64:
		return "integer"
	case float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Text writes a value as text: strings as they are, numbers and booleans
// as text, others as JSON.
func Text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return JSON(v)
}

// JSON writes compact JSON with sorted keys.
func JSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// FromJSON reads JSON with numbers as int64 when whole, float64 otherwise.
func FromJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("more than one JSON value")
	}
	return jsonNumbers(v), nil
}

// ToYAML writes a value as YAML, maps in key order.
func ToYAML(v any) ([]byte, error) { return yaml.Marshal(v) }

// FromYAML reads YAML 1.2 into the engine's types.
func FromYAML(data []byte) (any, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return normalize(v)
}

func jsonNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return wholeNumber(f)
	case map[string]any:
		for k, e := range x {
			x[k] = jsonNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = jsonNumbers(e)
		}
	}
	return v
}

func wholeNumber(f float64) any {
	if f == math.Trunc(f) && f >= -(1<<63) && f < 1<<63 {
		return int64(f)
	}
	return f
}

func normalize(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[k] = n
		}
		return x, nil
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("key %v is not a string", k)
			}
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		for i, e := range x {
			n, err := normalize(e)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		return wholeNumber(x), nil
	}
	return v, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
