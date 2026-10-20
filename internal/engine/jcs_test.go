package engine

import (
	"math"
	"testing"
)

func TestCanonical(t *testing.T) {
	// The values of RFC 8785, appendix B and section 3.2.3.
	for _, tt := range []struct {
		v    any
		want string
	}{
		{333333333.33333329, "333333333.3333333"},
		{1e30, "1e+30"},
		{4.50, "4.5"},
		{2e-3, "0.002"},
		{0.000000000000000000000000001, "1e-27"},
		{1e21, "1e+21"},
		{1e20, "100000000000000000000"},
		{1e-7, "1e-7"},
		{1e-6, "0.000001"},
		{math.Copysign(0, -1), "0"},
		{int64(-42), "-42"},
		{math.MaxFloat64, "1.7976931348623157e+308"},
		{"€$\x0f\nA'B\"\\\\\"/", `"€$\u000f\nA'B\"\\\\\"/"`},
		{map[string]any{"€": "Euro Sign", "\r": "Carriage Return", "דּ": "Hebrew Letter Dalet With Dagesh",
			"1": "One", "\U0001f600": "Emoji: Grinning Face", "\u0080": "Control", "ö": "Latin Small Letter O With Diaeresis"},
			`{"\r":"Carriage Return","1":"One","` + "\u0080" + `":"Control","ö":"Latin Small Letter O With Diaeresis","€":"Euro Sign","` + "\U0001f600" + `":"Emoji: Grinning Face","` + "דּ" + `":"Hebrew Letter Dalet With Dagesh"}`},
		{[]any{nil, true, map[string]any{"b": int64(2), "a": []any{}}}, `[null,true,{"a":[],"b":2}]`},
	} {
		got, err := canonical(tt.v)
		if err != nil || string(got) != tt.want {
			t.Errorf("canonical(%v) = %s, %v; want %s", tt.v, got, err, tt.want)
		}
	}
	if _, err := canonical(math.NaN()); err == nil {
		t.Error("NaN has no JSON form")
	}
}
