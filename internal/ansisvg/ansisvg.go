// Package ansisvg replays terminal output onto a screen of cells and draws the
// screen as SVG in a window frame. It backs the README's screenshots and demo
// recording (scripts/demo): it understands what quartermaster's board and
// `tmux capture-pane -e` write, which is cursor moves, clears and SGR.
package ansisvg

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Color is a cell color: 0 is the default, 1-256 is ANSI index n-1, and
// anything above is 0x1000000|RGB.
type Color int

const rgbFlag = 0x1000000

// Cell is one character on the screen and how it was drawn. A wide character
// takes its cell and marks the next as Cont.
type Cell struct {
	R                            rune
	Fg, Bg                       Color
	Bold, Dim, Italic, Underline bool
	Invert, Cont                 bool
}

// Screen is rows of cells.
type Screen [][]Cell

func blank(w, h int) Screen {
	s := make(Screen, h)
	for i := range s {
		s[i] = make([]Cell, w)
		for j := range s[i] {
			s[i][j].R = ' '
		}
	}
	return s
}

// Replay runs s against a w×h screen. It handles cursor home and position
// (CSI H), clear (CSI J), CR, LF and SGR, and skips other sequences.
func Replay(s string, w, h int) Screen {
	scr := blank(w, h)
	var cur Cell
	row, col := 0, 0
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b && i+1 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '?' || s[j] == ':') {
				j++
			}
			if j >= len(s) {
				return scr
			}
			params, cmd := s[i+2:j], s[j]
			i = j + 1
			switch cmd {
			case 'H':
				row, col = 0, 0
				if r, c, ok := strings.Cut(params, ";"); ok {
					row, _ = strconv.Atoi(r)
					col, _ = strconv.Atoi(c)
					row, col = row-1, col-1
				}
			case 'J':
				if params == "2" {
					scr = blank(w, h)
				}
			case 'm':
				sgr(&cur, params)
			}
		case c == 0x1b:
			// Other escapes (OSC, charset): skip to the terminator.
			j := i + 1
			if j < len(s) && s[j] == ']' {
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
			}
			i = j + 1
		case c == '\r':
			col = 0
			i++
		case c == '\n':
			row, col = row+1, 0
			i++
		case c < 0x20:
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			i += size
			wide := Wide(r)
			if row >= 0 && row < h && col >= 0 && col < w {
				cell := cur
				cell.R = r
				scr[row][col] = cell
				if wide && col+1 < w {
					cont := cur
					cont.R, cont.Cont = ' ', true
					scr[row][col+1] = cont
				}
			}
			col++
			if wide {
				col++
			}
		}
	}
	return scr
}

func sgr(c *Cell, params string) {
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) == 0 {
		ps = []string{"0"}
	}
	num := func(i int) int {
		if i >= len(ps) {
			return 0
		}
		n, _ := strconv.Atoi(ps[i])
		return n
	}
	for i := 0; i < len(ps); i++ {
		switch n := num(i); {
		case n == 0:
			*c = Cell{}
		case n == 1:
			c.Bold = true
		case n == 2:
			c.Dim = true
		case n == 3:
			c.Italic = true
		case n == 4:
			c.Underline = true
		case n == 7:
			c.Invert = true
		case n == 22:
			c.Bold, c.Dim = false, false
		case n == 23:
			c.Italic = false
		case n == 24:
			c.Underline = false
		case n == 27:
			c.Invert = false
		case n >= 30 && n <= 37:
			c.Fg = Color(n - 30 + 1)
		case n >= 90 && n <= 97:
			c.Fg = Color(n - 90 + 8 + 1)
		case n == 39:
			c.Fg = 0
		case n >= 40 && n <= 47:
			c.Bg = Color(n - 40 + 1)
		case n >= 100 && n <= 107:
			c.Bg = Color(n - 100 + 8 + 1)
		case n == 49:
			c.Bg = 0
		case n == 38 || n == 48:
			var col Color
			switch num(i + 1) {
			case 5:
				col = Color(num(i+2) + 1)
				i += 2
			case 2:
				col = Color(rgbFlag | num(i+2)<<16 | num(i+3)<<8 | num(i+4))
				i += 4
			}
			if n == 38 {
				c.Fg = col
			} else {
				c.Bg = col
			}
		}
	}
}

// Wide reports a character a terminal draws two cells wide: CJK and emoji.
// The board and herdr's chrome use none, but agent output might.
func Wide(r rune) bool {
	return r >= 0x1100 && r <= 0x115F || r >= 0x2E80 && r <= 0xA4CF || r >= 0xAC00 && r <= 0xD7A3 ||
		r >= 0xF900 && r <= 0xFAFF || r >= 0xFE30 && r <= 0xFE4F || r >= 0xFF00 && r <= 0xFF60 ||
		r >= 0xFFE0 && r <= 0xFFE6 || r >= 0x1F300 && r <= 0x1F64F || r >= 0x1F900 && r <= 0x1F9FF
}

// Theme is the terminal's colors.
type Theme struct {
	Bg, Fg, Dim, Chrome string
	// ANSI is colors 0-15.
	ANSI [16]string
}

// TokyoNight is the palette the README's images use.
var TokyoNight = Theme{
	Bg: "#1a1b26", Fg: "#c0caf5", Dim: "#565f89", Chrome: "#16161e",
	ANSI: [16]string{
		"#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#a9b1d6",
		"#414868", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#c0caf5",
	},
}

func (t Theme) color(c Color, def string) string {
	switch {
	case c == 0:
		return def
	case c&rgbFlag != 0:
		return fmt.Sprintf("#%06x", int(c)&0xffffff)
	case c <= 16:
		return t.ANSI[c-1]
	case c <= 232:
		// The 6×6×6 cube.
		n := int(c) - 17
		v := func(x int) int {
			if x == 0 {
				return 0
			}
			return 55 + 40*x
		}
		return fmt.Sprintf("#%02x%02x%02x", v(n/36), v(n/6%6), v(n%6))
	default:
		g := 8 + 10*(int(c)-233)
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
}

// Metrics of the drawing, in px.
const (
	CellW   = 8.4 // 0.6em: a monospace advance at 14px
	CellH   = 20.0
	padX    = 20.0
	padY    = 16.0
	chromeH = 34.0
)

// Size is the drawing's width and height for a w×h screen.
func Size(w, h int) (float64, float64) {
	return float64(w)*CellW + 2*padX, chromeH + float64(h)*CellH + 2*padY
}

// SVG draws the screen in a window with a title bar. Each run of same-styled
// text is placed at its column and stretched to its cells, so a fallback font
// can't shift the columns.
func SVG(scr Screen, title string, t Theme) string {
	w, h := len(scr[0]), len(scr)
	width, height := Size(w, h)
	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%g" height="%g" viewBox="0 0 %g %g">`+"\n", width, height, width, height)
	fmt.Fprintf(&s, `<rect width="%g" height="%g" rx="10" fill="%s"/>`+"\n", width, height, t.Bg)
	fmt.Fprintf(&s, `<path d="M0 10a10 10 0 0 1 10-10h%g a10 10 0 0 1 10 10v%gh-%gz" fill="%s"/>`+"\n", width-20, chromeH-10, width, t.Chrome)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&s, `<circle cx="%g" cy="%g" r="6" fill="%s"/>`+"\n", 20+float64(i)*20, chromeH/2, c)
	}
	fmt.Fprintf(&s, `<text x="%g" y="%g" fill="%s" font-family="ui-sans-serif, -apple-system, 'Segoe UI', Helvetica, Arial, sans-serif" font-size="13" text-anchor="middle">%s</text>`+"\n",
		width/2, chromeH/2+4.5, t.Dim, html.EscapeString(title))
	fmt.Fprintf(&s, `<g font-family="'JetBrains Mono', 'JetBrainsMono Nerd Font', ui-monospace, SFMono-Regular, Menlo, Consolas, 'DejaVu Sans Mono', monospace" font-size="14" xml:space="preserve">`+"\n")
	for y, line := range scr {
		top := chromeH + padY + float64(y)*CellH
		for x := 0; x < w; {
			st := line[x]
			end := x + 1
			for end < w && sameStyle(line[end], st) {
				end++
			}
			fg, bg := t.color(st.Fg, t.Fg), t.color(st.Bg, "")
			if st.Dim && st.Fg == 0 {
				fg = t.Dim
			}
			if st.Invert {
				fg, bg = t.color(st.Bg, t.Bg), t.color(st.Fg, t.Fg)
			}
			if bg != "" {
				fmt.Fprintf(&s, `<rect x="%g" y="%g" width="%g" height="%g" fill="%s"/>`+"\n", padX+float64(x)*CellW, top, float64(end-x)*CellW, CellH, bg)
			}
			// Draw each run of text between wide characters' spacer cells.
			for a := x; a < end; {
				b := a
				var text strings.Builder
				cells := 0
				for b < end && !line[b].Cont {
					text.WriteRune(line[b].R)
					cells++
					if Wide(line[b].R) {
						cells++
					}
					b++
				}
				str := strings.TrimRight(text.String(), " ")
				cells -= len([]rune(text.String())) - len([]rune(str))
				if strings.TrimSpace(str) != "" {
					attrs := ""
					if st.Bold {
						attrs += ` font-weight="bold"`
					}
					if st.Italic {
						attrs += ` font-style="italic"`
					}
					if st.Underline {
						attrs += ` text-decoration="underline"`
					}
					if st.Dim && st.Fg != 0 {
						attrs += ` fill-opacity="0.6"`
					}
					fmt.Fprintf(&s, `<text x="%g" y="%g" fill="%s"%s textLength="%g" lengthAdjust="spacingAndGlyphs">%s</text>`+"\n",
						padX+float64(a)*CellW, top+CellH*0.72, fg, attrs, float64(cells)*CellW, html.EscapeString(str))
				}
				for b < end && line[b].Cont {
					b++
				}
				a = b
			}
			x = end
		}
	}
	s.WriteString("</g>\n</svg>\n")
	return s.String()
}

func sameStyle(a, b Cell) bool {
	a.R, b.R = 0, 0
	a.Cont, b.Cont = false, false
	return a == b
}
