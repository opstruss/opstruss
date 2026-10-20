package expr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// strftime formats t with the directives of C strftime that tools and
// shells share, such as %Y-%m-%d for date +%F.
func strftime(t time.Time, format string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i++; i == len(format) {
			return "", fmt.Errorf("%q ends with %%", format)
		}
		switch d := format[i]; d {
		case 'Y':
			b.WriteString(strconv.Itoa(t.Year()))
		case 'y':
			fmt.Fprintf(&b, "%02d", t.Year()%100)
		case 'm':
			fmt.Fprintf(&b, "%02d", int(t.Month()))
		case 'd':
			fmt.Fprintf(&b, "%02d", t.Day())
		case 'e':
			fmt.Fprintf(&b, "%2d", t.Day())
		case 'H':
			fmt.Fprintf(&b, "%02d", t.Hour())
		case 'I':
			fmt.Fprintf(&b, "%02d", (t.Hour()+11)%12+1)
		case 'M':
			fmt.Fprintf(&b, "%02d", t.Minute())
		case 'S':
			fmt.Fprintf(&b, "%02d", t.Second())
		case 'p':
			b.WriteString(t.Format("PM"))
		case 'j':
			fmt.Fprintf(&b, "%03d", t.YearDay())
		case 'a':
			b.WriteString(t.Format("Mon"))
		case 'A':
			b.WriteString(t.Format("Monday"))
		case 'b', 'h':
			b.WriteString(t.Format("Jan"))
		case 'B':
			b.WriteString(t.Format("January"))
		case 'u':
			b.WriteString(strconv.Itoa((int(t.Weekday())+6)%7 + 1))
		case 'w':
			b.WriteString(strconv.Itoa(int(t.Weekday())))
		case 'Z':
			b.WriteString(t.Format("MST"))
		case 'z':
			b.WriteString(t.Format("-0700"))
		case 's':
			b.WriteString(strconv.FormatInt(t.Unix(), 10))
		case 'F':
			b.WriteString(t.Format("2006-01-02"))
		case 'T':
			b.WriteString(t.Format("15:04:05"))
		case 'R':
			b.WriteString(t.Format("15:04"))
		case 'D':
			b.WriteString(t.Format("01/02/06"))
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '%':
			b.WriteByte('%')
		default:
			return "", fmt.Errorf("unknown directive %%%c", d)
		}
	}
	return b.String(), nil
}
