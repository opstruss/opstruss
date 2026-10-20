package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/opstruss/opstruss/internal/expr"
)

// canonical writes v as canonical JSON (RFC 8785): keys in the order of
// their UTF-16 code units, no white space, strings with the fewest escapes,
// and numbers as ECMAScript writes them. v holds the engine's types; other
// values go through encoding/json first.
func canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	err := writeCanonical(&b, v)
	return b.Bytes(), err
}

func writeCanonical(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case string:
		writeString(b, x)
	case int64:
		return writeNumber(b, float64(x))
	case float64:
		return writeNumber(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b))) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		n, err := expr.FromJSON(data)
		if err != nil {
			return err
		}
		return writeCanonical(b, n)
	}
	return nil
}

func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// writeNumber writes f as ECMAScript's Number.prototype.toString does:
// the shortest digits that read back as f, in plain notation for exponents
// from -7 to 20 and in exponent notation otherwise.
func writeNumber(b *bytes.Buffer, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%v has no JSON form", f)
	}
	if f == 0 {
		b.WriteByte('0')
		return nil
	}
	if f < 0 {
		b.WriteByte('-')
		f = -f
	}
	mant, exp, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
	digits := strings.Replace(mant, ".", "", 1)
	e, _ := strconv.Atoi(exp)
	n, k := e+1, len(digits)
	switch {
	case k <= n && n <= 21:
		b.WriteString(digits + strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		b.WriteString(digits[:n] + "." + digits[n:])
	case -6 < n && n <= 0:
		b.WriteString("0." + strings.Repeat("0", -n) + digits)
	default:
		if k > 1 {
			digits = digits[:1] + "." + digits[1:]
		}
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		fmt.Fprintf(b, "%se%s%d", digits, sign, abs(n-1))
	}
	return nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
