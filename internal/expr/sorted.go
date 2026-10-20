package expr

import (
	"cmp"
	"slices"
	"time"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/interpreter"
)

// Maps are always iterated in key order, by the UTF-8 bytes of the key, so
// the same values give the same result and the same digest. Go maps, and so
// CEL's own, iterate in random order; sortedMap fixes the order of maps
// that come from data, through adapter, and of map literals, through
// sortLiterals.

type sortedMap struct {
	traits.Mapper
}

func (m sortedMap) Iterator() traits.Iterator {
	var keys []ref.Val
	it := m.Mapper.Iterator()
	for it.HasNext() == types.True {
		keys = append(keys, it.Next())
	}
	slices.SortFunc(keys, compareKeys)
	return types.NewRefValList(adapter{}, keys).Iterator()
}

func compareKeys(a, b ref.Val) int {
	as, aok := a.(types.String)
	bs, bok := b.(types.String)
	if aok && bok {
		return cmp.Compare(string(as), string(bs))
	}
	return cmp.Compare(a.Type().TypeName(), b.Type().TypeName())
}

func sorted(v ref.Val) ref.Val {
	switch m := v.(type) {
	case sortedMap:
		return m
	case traits.Mapper:
		return sortedMap{m}
	}
	return v
}

// adapter turns engine values into CEL values with sorted maps.
type adapter struct{}

func (a adapter) NativeToValue(v any) ref.Val {
	switch x := v.(type) {
	case map[string]any:
		return sortedMap{types.NewStringInterfaceMap(a, x)}
	case []any:
		return types.NewDynamicList(a, x)
	case time.Time:
		return types.Timestamp{Time: x}
	}
	return types.DefaultTypeAdapter.NativeToValue(v)
}

// sortLiterals sorts the maps that expressions build: map literals, and
// what comprehensions such as transformMap return. Attributes, constants,
// and calls keep their own type, which the planner relies on; their maps
// come from data or functions, already sorted.
func sortLiterals(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
	switch x := i.(type) {
	case interpreter.InterpretableConstructor:
		if x.Type() == types.MapType {
			return sortedLiteral{x}, nil
		}
		return i, nil
	case interpreter.InterpretableAttribute, interpreter.InterpretableConst, interpreter.InterpretableCall:
		return i, nil
	}
	return sortedLiteral{i}, nil
}

type sortedLiteral struct {
	interpreter.InterpretableV2
}

func (s sortedLiteral) Eval(a interpreter.Activation) ref.Val {
	return sorted(s.InterpretableV2.Eval(a))
}

func (s sortedLiteral) Exec(f *interpreter.ExecutionFrame) ref.Val {
	return sorted(s.InterpretableV2.Exec(f))
}
