package ansi

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxRest bounds what a Scanner keeps of a control that a write cuts off.
const maxRest = 4 << 10

// maxCell bounds the bytes of one cell: a character and the marks that join
// it.
const maxCell = 32

// Scanner splits what a program writes to a terminal into text and
// controls, across writes: a control or a character that one write cuts
// off waits for the next, up to 4 KiB, and a string sequence such as OSC,
// which nothing shows, is dropped as it comes, whatever its length.
type Scanner struct {
	rest []byte // the start of a control or a character that the last write cut off
	str  bool   // inside a string sequence that an earlier write began
	esc  bool   // and that write ended with ESC, which may start its ST
}

// Scan calls text with each run of text in p, tabs included, and control
// with each control: an escape sequence, a C1 control, or another control
// character.
func (sc *Scanner) Scan(p []byte, text, control func(string)) {
	if len(p) == 0 {
		return
	}
	s := string(p)
	if len(sc.rest) > 0 {
		s = string(sc.rest) + s
		sc.rest = sc.rest[:0]
	}
	i := 0
	if sc.str {
		if i = sc.skip(s); sc.str {
			return
		}
	}
	start := i
	for i < len(s) {
		c := s[i]
		n, cut := 0, false
		switch {
		case c == esc:
			n, _, cut = sequence(s[i:])
		case c < 0x20 && c != '\t' || c == 0x7f:
			n = 1
		case c == 0xc2 && i+1 < len(s) && s[i+1] >= 0x80 && s[i+1] <= 0x9f:
			n = 2
		default:
			i++
			continue
		}
		if start < i {
			text(s[start:i])
		}
		if cut {
			sc.cut(s[i:])
			return
		}
		if !stringSequence(s[i:]) {
			control(s[i : i+n])
		}
		i += n
		start = i
	}
	// A character that the write cuts off waits for the rest of it.
	end := len(s)
	for j := end - 1; j >= start && j > end-utf8.UTFMax; j-- {
		if utf8.RuneStart(s[j]) {
			if !utf8.FullRuneInString(s[j:]) {
				end = j
			}
			break
		}
	}
	if start < end {
		text(s[start:end])
	}
	sc.rest = append(sc.rest, s[end:]...)
}

// stringSequence reports whether s starts with a string sequence: OSC, DCS,
// SOS, PM, or APC.
func stringSequence(s string) bool {
	return len(s) >= 2 && s[0] == esc && strings.IndexByte("]PX^_", s[1]) >= 0
}

// cut keeps seq, a control that the write cut off, for the next write; of
// a string sequence it keeps only that the next write is still in one.
func (sc *Scanner) cut(seq string) {
	if stringSequence(seq) {
		sc.str, sc.esc = true, len(seq) > 2 && seq[len(seq)-1] == esc
		return
	}
	if len(seq) <= maxRest {
		sc.rest = append(sc.rest[:0], seq...)
	}
}

// skip drops the rest of a string sequence from s, up to ST or BEL, and
// returns where the bytes after it start.
func (sc *Scanner) skip(s string) int {
	if sc.esc {
		sc.esc = false
		if s[0] == '\\' {
			sc.str = false
			return 1
		}
	}
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == 0x07:
			sc.str = false
			return i + 1
		case s[i] != esc:
		case i+1 == len(s):
			sc.esc = true
		case s[i+1] == '\\':
			sc.str = false
			return i + 2
		}
	}
	return len(s)
}

// Attributes of a style, as SGR turns them on.
const (
	bold uint8 = 1 << iota
	dim
	italic
	underline
	blink
	inverse
	hidden
	strike
)

// attrCodes are the SGR codes of the attributes, in the order of their bits.
var attrCodes = []string{"1", "2", "3", "4", "5", "7", "8", "9"}

// attrOn and attrOff are the attributes that the SGR codes up to 29 turn on
// and off.
var (
	attrOn  = [30]uint8{1: bold, 2: dim, 3: italic, 4: underline, 5: blink, 6: blink, 7: inverse, 8: hidden, 9: strike, 21: underline}
	attrOff = [30]uint8{22: bold | dim, 23: italic, 24: underline, 25: blink, 27: inverse, 28: hidden, 29: strike}
)

// color is the default color, one of 256, or a 24-bit one.
type color struct {
	kind uint8 // 0 default, 1 indexed, 2 rgb
	v    uint32
}

// style is how a cell looks: what SGR sets.
type style struct {
	attr   uint8
	fg, bg color
}

// sgr is the SGR sequence that sets st from the default.
func (st style) sgr() string {
	if st == (style{}) {
		return "\x1b[0m"
	}
	b := []byte("\x1b[0")
	for i, code := range attrCodes {
		if st.attr&(1<<i) != 0 {
			b = append(append(b, ';'), code...)
		}
	}
	b = st.fg.code(b, 30, 90, "38")
	b = st.bg.code(b, 40, 100, "48")
	return string(append(b, 'm'))
}

// code adds the SGR code of c to b: base or bright for the first 16 colors,
// ext otherwise.
func (c color) code(b []byte, base, bright int, ext string) []byte {
	switch {
	case c.kind == 1 && c.v < 8:
		return strconv.AppendInt(append(b, ';'), int64(base)+int64(c.v), 10)
	case c.kind == 1 && c.v < 16:
		return strconv.AppendInt(append(b, ';'), int64(bright)+int64(c.v)-8, 10)
	case c.kind == 1:
		return strconv.AppendUint(append(append(b, ';'), ext+";5;"...), uint64(c.v), 10)
	case c.kind == 2:
		b = append(append(b, ';'), ext+";2;"...)
		b = strconv.AppendUint(b, uint64(c.v>>16), 10)
		b = strconv.AppendUint(append(b, ';'), uint64(c.v>>8&0xff), 10)
		return strconv.AppendUint(append(b, ';'), uint64(c.v&0xff), 10)
	}
	return b
}

// apply sets st as the parameters of an SGR sequence say; codes that it
// does not know change nothing.
func (st *style) apply(params string) {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		sub := strings.Split(ps[i], ":")
		switch n := number(sub[0]); {
		case n == 0:
			*st = style{}
		case n == 4 && len(sub) > 1 && number(sub[1]) == 0:
			// 4:0 is no underline; 4:1 to 4:5 are its kinds.
			st.attr &^= underline
		case n < len(attrOn):
			st.attr = (st.attr | attrOn[n]) &^ attrOff[n]
		case n >= 30 && n <= 37:
			st.fg = color{1, uint32(n - 30)}
		case n == 39:
			st.fg = color{}
		case n >= 40 && n <= 47:
			st.bg = color{1, uint32(n - 40)}
		case n == 49:
			st.bg = color{}
		case n >= 90 && n <= 97:
			st.fg = color{1, uint32(n - 90 + 8)}
		case n >= 100 && n <= 107:
			st.bg = color{1, uint32(n - 100 + 8)}
		case n == 38 || n == 48:
			var c color
			ok := false
			if len(sub) > 1 {
				c, ok, _ = extended(sub[1:], true)
			} else {
				var used int
				c, ok, used = extended(ps[i+1:], false)
				i += used
			}
			switch {
			case !ok:
			case n == 38:
				st.fg = c
			default:
				st.bg = c
			}
		}
	}
}

// extended reads the color after 38 or 48: 5;n, or 2;r;g;b, whose colon
// form may name a color space before r. It returns the color, whether args
// give one, and how many of args it took.
func extended(args []string, colon bool) (color, bool, int) {
	if len(args) == 0 {
		return color{}, false, 0
	}
	switch number(args[0]) {
	case 5:
		if len(args) < 2 {
			return color{}, false, len(args)
		}
		n := number(args[1])
		return color{1, uint32(n)}, n <= 255, 2
	case 2:
		rgb := args[1:]
		if colon && len(rgb) >= 4 {
			rgb = rgb[1:]
		}
		if len(rgb) < 3 {
			return color{}, false, len(args)
		}
		r, g, b := number(rgb[0]), number(rgb[1]), number(rgb[2])
		return color{2, uint32(r)<<16 | uint32(g)<<8 | uint32(b)}, r <= 255 && g <= 255 && b <= 255, 4
	}
	return color{}, false, 1
}

// number reads a parameter of a sequence; an empty one is 0, and a large
// one stops at 65535.
func number(s string) int {
	n := 0
	for i := 0; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		n = min(n*10+int(s[i]-'0'), 65535)
	}
	return n
}

// cell is one cell of a screen: a character, with the marks that join it,
// or nothing.
type cell struct {
	text  string
	style style
	wide  bool // the character takes this cell and the next
	cont  bool // the second cell of a wide character
}

// blank reports whether c shows nothing at the end of a row: no character
// but a space, no background, and no inverse.
func (c cell) blank() bool {
	return (c.text == "" || c.text == " ") && !c.cont && c.style.bg == (color{}) && c.style.attr&inverse == 0
}

// Screen is the terminal of one program: the rows that the program may
// still change, and those that leave the top of it, which it can change no
// more, as a terminal moves them to its scrollback. It knows the subset of
// controls that progress bars and redrawn areas use; the others change
// nothing.
type Screen struct {
	cols, rows int
	grid       [][]cell
	x, y       int
	wrap       bool // the last column was written: the next character goes to the next row
	style      style
	top, bot   int // the scroll region, its first and last row
	saved      struct {
		x, y  int
		style style
		ok    bool
	}
	scan Scanner
	out  []string // the rows that left the top in this Write
}

// NewScreen returns a terminal of cols × rows cells for a program to draw on.
func NewScreen(cols, rows int) *Screen {
	s := &Screen{cols: max(cols, 2), rows: max(rows, 1)}
	cells := make([]cell, s.rows*s.cols)
	s.grid = make([][]cell, s.rows)
	for y := range s.grid {
		s.grid[y] = cells[y*s.cols : (y+1)*s.cols : (y+1)*s.cols]
	}
	s.bot = s.rows - 1
	return s
}

// Write draws p and returns the rows that left the top of the screen,
// oldest first.
func (s *Screen) Write(p []byte) []string {
	s.scan.Scan(p, s.text, s.control)
	out := s.out
	s.out = nil
	return out
}

// Rows returns the rows that the program may still change, from the top to
// the last row with text.
func (s *Screen) Rows() []string {
	rows := make([]string, s.rows)
	last := -1
	for y, row := range s.grid {
		if rows[y] = render(row); rows[y] != "" {
			last = y
		}
	}
	return rows[:last+1]
}

// End returns the rows left, from the top to the last row with text, and
// empties the screen.
func (s *Screen) End() []string {
	rows := s.Rows()
	s.reset()
	return rows
}

// render writes a row as Clean leaves a line: its text, with an SGR sequence
// where the style changes and a reset at the end, without the blank cells
// at its end.
func render(row []cell) string {
	end := len(row)
	for end > 0 && row[end-1].blank() {
		end--
	}
	var b strings.Builder
	cur := style{}
	for _, c := range row[:end] {
		if c.cont {
			continue
		}
		if c.style != cur {
			b.WriteString(c.style.sgr())
			cur = c.style
		}
		if c.text == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.text)
		}
	}
	if cur != (style{}) {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func (s *Screen) reset() {
	for y := range s.grid {
		s.blankRow(s.grid[y], cell{})
	}
	s.x, s.y, s.wrap, s.style = 0, 0, false, style{}
	s.top, s.bot = 0, s.rows-1
	s.saved.ok = false
}

// erased is a cell that an erase leaves: blank, with the background of the
// style, as xterm erases.
func (s *Screen) erased() cell { return cell{style: style{bg: s.style.bg}} }

func (s *Screen) blankRow(row []cell, c cell) {
	for x := range row {
		row[x] = c
	}
}

// text draws a run of text at the cursor.
func (s *Screen) text(t string) {
	for i := 0; i < len(t); {
		r, n := utf8.DecodeRuneInString(t[i:])
		ch := t[i : i+n]
		if r == utf8.RuneError && n == 1 {
			ch = "�"
		}
		if r == '\t' {
			s.x, s.wrap = min((s.x/8+1)*8, s.cols-1), false
		} else {
			s.put(ch, runeWidth(r, 0))
		}
		i += n
	}
}

// put writes one character of w cells at the cursor; one of no width joins
// the character before it.
func (s *Screen) put(ch string, w int) {
	if w == 0 {
		s.join(ch)
		return
	}
	if s.wrap || w == 2 && s.x == s.cols-1 {
		// The row is full: the character starts the next, as autowrap does.
		s.x, s.wrap = 0, false
		s.lineFeed()
	}
	row := s.grid[s.y]
	s.unwide(row, s.x)
	row[s.x] = cell{text: ch, style: s.style, wide: w == 2}
	if w == 2 {
		s.unwide(row, s.x+1)
		row[s.x+1] = cell{style: s.style, cont: true}
	}
	if s.x += w; s.x >= s.cols {
		s.x, s.wrap = s.cols-1, true
	}
}

// unwide blanks the other half of a wide character at x, which a character
// written there splits.
func (s *Screen) unwide(row []cell, x int) {
	switch {
	case row[x].cont && x > 0:
		row[x-1] = cell{}
	case row[x].wide && x+1 < len(row):
		row[x+1] = cell{}
	}
}

// join adds a mark of no width to the character before the cursor.
func (s *Screen) join(ch string) {
	x := s.x
	if !s.wrap {
		x--
	}
	row := s.grid[s.y]
	if x >= 0 && row[x].cont {
		x--
	}
	if x < 0 || row[x].text == "" || len(row[x].text)+len(ch) > maxCell {
		return
	}
	row[x].text += ch
}

// lineFeed moves the cursor down a row, scrolling the region at its last
// row.
func (s *Screen) lineFeed() {
	switch {
	case s.y == s.bot:
		s.scrollUp(1)
	case s.y < s.rows-1:
		s.y++
	}
}

// scrollUp moves the rows of the region up n rows; those that leave the
// top of the screen are final, and those that leave a region below the top
// are dropped, as a terminal drops them.
func (s *Screen) scrollUp(n int) {
	n = min(n, s.bot-s.top+1)
	gone := make([][]cell, n)
	copy(gone, s.grid[s.top:s.top+n])
	for _, row := range gone {
		if s.top == 0 {
			s.out = append(s.out, render(row))
		}
		s.blankRow(row, s.erased())
	}
	copy(s.grid[s.top:], s.grid[s.top+n:s.bot+1])
	copy(s.grid[s.bot-n+1:], gone)
}

// scrollDown moves the rows of the region down n rows; those that leave
// its bottom are dropped.
func (s *Screen) scrollDown(n int) {
	n = min(n, s.bot-s.top+1)
	gone := make([][]cell, n)
	copy(gone, s.grid[s.bot-n+1:s.bot+1])
	for _, row := range gone {
		s.blankRow(row, s.erased())
	}
	copy(s.grid[s.top+n:], s.grid[s.top:s.bot-n+1])
	copy(s.grid[s.top:], gone)
}

// control does what a control says.
func (s *Screen) control(c string) {
	switch {
	case c[0] == esc && len(c) >= 2 && c[1] == '[':
		s.csi(c)
	case c[0] == esc && len(c) == 2:
		s.escape(c[1])
	case c == "\r":
		s.x, s.wrap = 0, false
	case c == "\n", c == "\v", c == "\f":
		s.wrap = false
		s.lineFeed()
	case c == "\b":
		s.x, s.wrap = max(s.x-1, 0), false
	}
}

// escape does what ESC and one byte say.
func (s *Screen) escape(b byte) {
	switch b {
	case '7':
		s.saved.x, s.saved.y, s.saved.style, s.saved.ok = s.x, s.y, s.style, true
	case '8':
		s.restore()
	case 'D':
		s.wrap = false
		s.lineFeed()
	case 'E':
		s.x, s.wrap = 0, false
		s.lineFeed()
	case 'M':
		s.wrap = false
		switch {
		case s.y == s.top:
			s.scrollDown(1)
		case s.y > 0:
			s.y--
		}
	case 'c':
		s.reset()
	}
}

func (s *Screen) restore() {
	if s.saved.ok {
		s.x, s.y, s.style = min(s.saved.x, s.cols-1), min(s.saved.y, s.rows-1), s.saved.style
	} else {
		s.x, s.y, s.style = 0, 0, style{}
	}
	s.wrap = false
}

// csi does what a CSI sequence says.
func (s *Screen) csi(c string) {
	if len(c) < 3 {
		return
	}
	final, params := c[len(c)-1], c[2:len(c)-1]
	if final < 0x40 || final > 0x7e || strings.ContainsAny(params, "<=>? !\"#$%&'()*+,-./") {
		// A private sequence, one with intermediate bytes, or one that a
		// byte that cannot be in it ended.
		return
	}
	if final == 'm' {
		s.style.apply(params)
		return
	}
	var args []int
	for _, p := range strings.Split(params, ";") {
		head, _, _ := strings.Cut(p, ":")
		args = append(args, number(head))
	}
	arg := func(i, def int) int {
		if i < len(args) && args[i] > 0 {
			return args[i]
		}
		return def
	}
	n := arg(0, 1)
	row := s.grid[s.y]
	s.wrap = false
	switch final {
	case 'A', 'F':
		limit := 0
		if s.y >= s.top {
			limit = s.top
		}
		s.y = max(s.y-n, limit)
		if final == 'F' {
			s.x = 0
		}
	case 'B', 'E':
		limit := s.rows - 1
		if s.y <= s.bot {
			limit = s.bot
		}
		s.y = min(s.y+n, limit)
		if final == 'E' {
			s.x = 0
		}
	case 'C':
		s.x = min(s.x+n, s.cols-1)
	case 'D':
		s.x = max(s.x-n, 0)
	case 'G':
		s.x = min(n, s.cols) - 1
	case 'H', 'f':
		s.y, s.x = min(n, s.rows)-1, min(arg(1, 1), s.cols)-1
	case 'd':
		s.y = min(n, s.rows) - 1
	case 'J':
		s.eraseDisplay(arg(0, 0))
	case 'K':
		s.eraseLine(row, arg(0, 0))
	case 'L', 'M':
		if s.y < s.top || s.y > s.bot {
			return
		}
		top := s.top
		s.top = s.y
		if final == 'L' {
			s.scrollDown(n)
		} else {
			s.scrollUpWithin(n)
		}
		s.top, s.x = top, 0
	case '@':
		n = min(n, s.cols-s.x)
		copy(row[s.x+n:], row[s.x:s.cols-n])
		s.blankRow(row[s.x:s.x+n], s.erased())
		s.mend(row)
	case 'P':
		n = min(n, s.cols-s.x)
		copy(row[s.x:], row[s.x+n:])
		s.blankRow(row[s.cols-n:], s.erased())
		s.mend(row)
	case 'X':
		n = min(n, s.cols-s.x)
		s.blankRow(row[s.x:s.x+n], s.erased())
		s.mend(row)
	case 'S':
		s.scrollUp(n)
	case 'T':
		s.scrollDown(n)
	case 'r':
		top, bot := arg(0, 1)-1, min(arg(1, s.rows), s.rows)-1
		if top < bot {
			s.top, s.bot = top, bot
			s.x, s.y = 0, 0
		}
	case 's':
		s.escape('7')
	case 'u':
		s.restore()
	}
}

// scrollUpWithin is scrollUp for rows that DL deletes, which are dropped
// even at the top of the screen.
func (s *Screen) scrollUpWithin(n int) {
	out := s.out
	s.scrollUp(n)
	s.out = out
}

func (s *Screen) eraseDisplay(mode int) {
	switch mode {
	case 0:
		s.eraseLine(s.grid[s.y], 0)
		for _, row := range s.grid[s.y+1:] {
			s.blankRow(row, s.erased())
		}
	case 1:
		for _, row := range s.grid[:s.y] {
			s.blankRow(row, s.erased())
		}
		s.eraseLine(s.grid[s.y], 1)
	case 2, 3:
		// The screen goes without leaving rows, as xterm clears it.
		for _, row := range s.grid {
			s.blankRow(row, s.erased())
		}
	}
}

func (s *Screen) eraseLine(row []cell, mode int) {
	switch mode {
	case 0:
		s.blankRow(row[s.x:], s.erased())
	case 1:
		s.blankRow(row[:s.x+1], s.erased())
	case 2:
		s.blankRow(row, s.erased())
	}
	s.mend(row)
}

// mend blanks the halves of wide characters that an edit of row split.
func (s *Screen) mend(row []cell) {
	for x := range row {
		switch {
		case row[x].cont && (x == 0 || !row[x-1].wide):
			row[x] = cell{style: style{bg: row[x].style.bg}}
		case row[x].wide && (x+1 == len(row) || !row[x+1].cont):
			row[x] = cell{style: style{bg: row[x].style.bg}}
		}
	}
}
