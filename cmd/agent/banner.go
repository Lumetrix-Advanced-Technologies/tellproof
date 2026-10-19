package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The welcome banner (D-061): the Tellproof wordmark and mark, shown when the
// agent starts interactively (no arguments, or run --local) in a terminal. It
// is never shown to a pipe, a file, CI, a dumb terminal or with
// TELLPROOF_NO_BANNER set, so scripts and logs see only what they did
// before; NO_COLOR keeps the shapes and drops the colour. The packages
// install no scripts (D-054), so the first run is where it appears.

// font is a 5 by 5 block font for the wordmark.
var font = map[rune][5]string{
	'T': {"#####", "..#..", "..#..", "..#..", "..#.."},
	'E': {"#####", "#....", "####.", "#....", "#####"},
	'L': {"#....", "#....", "#....", "#....", "#####"},
	'P': {"####.", "#...#", "####.", "#....", "#...."},
	'R': {"####.", "#...#", "####.", "#..#.", "#...#"},
	'O': {".###.", "#...#", "#...#", "#...#", ".###."},
	'F': {"#####", "#....", "####.", "#....", "#...."},
}

// mark is the logo as 17 by 14 pixels, drawn two pixels per character cell
// with half blocks: B blue square, H the nut, S the brass torque stripe that
// runs from the bolt across the nut onto the base, . nothing.
var mark = [14]string{
	".BBBBBBBBBBBBBSSB",
	"BBBBHHHHHHHHHSSBB",
	"BBBHHHHHHHHHSSBBB",
	"BBBHHHHHHHHSSHBBB",
	"BBHHHHHHHHSSHHHBB",
	"BBHHHHHHBSSHHHHBB",
	"BHHHHHHBBBHHHHHHB",
	"BHHHHHHBBBHHHHHHB",
	"BBHHHHHHBHHHHHHBB",
	"BBHHHHHHHHHHHHHBB",
	"BBBHHHHHHHHHHHBBB",
	"BBBHHHHHHHHHHHBBB",
	"BBBBHHHHHHHHHBBBB",
	".BBBBBBBBBBBBBBB.",
}

type rgb struct{ r, g, b, x256 int } // x256: the nearest of the 256-colour palette

var (
	colBlue       = rgb{31, 86, 201, 26}    // --chalk
	colNut        = rgb{255, 255, 255, 231} // the mark's nut
	colBrass      = rgb{226, 173, 69, 179}  // the torque stripe
	colLetter     = rgb{88, 140, 255, 69}   // the wordmark: --chalk lifted for dark terminals
	colShadow     = rgb{110, 118, 132, 243}
	colDepth      = rgb{36, 58, 120, 24}    // the wordmark's drop shadow
	colDepthLight = rgb{178, 194, 232, 153} // the same on a light background
	colMuted      = rgb{150, 158, 172, 247}
)

// palette writes colours: truecolour, 256 colours, or none. light is a
// terminal that says (COLORFGBG) it has a light background.
type palette struct {
	mode  int // 0 none, 1 256, 2 truecolour
	light bool
}

func (p palette) fg(c rgb, s string) string {
	switch p.mode {
	case 2:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", c.r, c.g, c.b, s)
	case 1:
		return fmt.Sprintf("\x1b[38;5;%dm%s\x1b[0m", c.x256, s)
	}
	return s
}

func (p palette) fgbg(f, b rgb, s string) string {
	switch p.mode {
	case 2:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm%s\x1b[0m", f.r, f.g, f.b, b.r, b.g, b.b, s)
	case 1:
		return fmt.Sprintf("\x1b[38;5;%d;48;5;%dm%s\x1b[0m", f.x256, b.x256, s)
	}
	return s
}

func (p palette) bold(s string) string {
	if p.mode == 0 {
		return s
	}
	return "\x1b[1m" + s + "\x1b[0m"
}

// markRows renders the mark as 7 text rows of 17 cells. Without colour the
// half blocks would all look the same, so it uses shades instead.
func markRows(p palette) []string {
	pixel := func(ch byte) (rgb, bool) {
		switch ch {
		case 'B':
			return colBlue, true
		case 'H':
			return colNut, true
		case 'S':
			return colBrass, true
		}
		return rgb{}, false
	}
	shade := map[byte]string{'B': "▓", 'H': "█", 'S': "░", '.': " "}
	var out []string
	for r := 0; r < len(mark); r += 2 {
		var b strings.Builder
		for c := 0; c < len(mark[r]); c++ {
			top, bot := mark[r][c], mark[r+1][c]
			if p.mode == 0 {
				ch := top
				if ch == '.' {
					ch = bot
				}
				b.WriteString(shade[ch])
				continue
			}
			tc, tok := pixel(top)
			bc, bok := pixel(bot)
			switch {
			case !tok && !bok:
				b.WriteString(" ")
			case tok && !bok:
				b.WriteString(p.fg(tc, "▀"))
			case !tok && bok:
				b.WriteString(p.fg(bc, "▄"))
			case tc == bc:
				b.WriteString(p.fg(tc, "█"))
			default:
				b.WriteString(p.fgbg(tc, bc, "▀"))
			}
		}
		out = append(out, b.String())
	}
	return out
}

// wordRows renders text in the block font with a drop shadow: 6 rows.
func wordRows(p palette, text string) []string {
	const rows = 5
	width := len(text)*6 - 1
	on := make([][]bool, rows)
	for r := range on {
		on[r] = make([]bool, width)
	}
	for i, ch := range text {
		for r := 0; r < rows; r++ {
			for c, x := range font[ch][r] {
				if x == '#' {
					on[r][i*6+c] = true
				}
			}
		}
	}
	lit := func(r, c int) bool { return r >= 0 && r < rows && c >= 0 && c < width && on[r][c] }
	var out []string
	for r := 0; r <= rows; r++ {
		var b strings.Builder
		for c := 0; c <= width; c++ {
			switch {
			case lit(r, c):
				b.WriteString(p.fg(colLetter, "█"))
			case lit(r-1, c-1):
				if p.mode == 0 {
					b.WriteString("░") // without colour a solid shadow would merge with the letters
				} else if p.light {
					b.WriteString(p.fg(colDepthLight, "█"))
				} else {
					b.WriteString(p.fg(colDepth, "█"))
				}
			default:
				b.WriteString(" ")
			}
		}
		out = append(out, b.String())
	}
	return out
}

// visible is the printed width of s: runes, without escape sequences.
func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

func pad(s string, width int) string {
	if v := visible(s); v < width {
		return s + strings.Repeat(" ", width-v)
	}
	return s
}

// bannerWide needs 80 columns: the framed wordmark beside the mark.
const bannerWide = 80

// renderBanner draws the banner for a terminal of the given width.
func renderBanner(p palette, width int, version, subtitle string) string {
	var b strings.Builder
	line := func(s string) { b.WriteString(strings.TrimRight(s, " ") + "\n") }
	tagline := p.fg(colMuted, "Signed, content-free evidence of how your AI system actually runs.")
	switch {
	case width >= bannerWide:
		inner := bannerWide - 2 // between the frame's corners
		words := wordRows(p, "TELLPROOF")
		mk := markRows(p)
		corner := func(l, r string) string {
			return " " + p.fg(colShadow, l) + strings.Repeat(" ", inner-4) + p.fg(colShadow, r)
		}
		line(corner("┌─", "─┐"))
		line(corner("│ ", " │"))
		left := []string{"   " + p.bold("Welcome to")}
		for _, w := range words {
			left = append(left, "   "+w)
		}
		sub := subtitle + " · " + version
		left = append(left, "   "+strings.Repeat(" ", max(0, 54-utf8.RuneCountInString(sub)))+p.fg(colMuted, sub))
		for i, l := range left {
			right := ""
			if i < len(mk) {
				right = mk[i]
			}
			line(" " + pad(l, 60) + right)
		}
		line(corner("│ ", " │"))
		line(corner("└─", "─┘"))
		line("  " + tagline)
	case width >= 44:
		mk := markRows(p)
		// beside the mark: 23 columns at the narrowest
		text := []string{"", p.bold("Tellproof"), p.fg(colMuted, subtitle+" · "+version), "",
			p.fg(colMuted, "Signed, content-free"), p.fg(colMuted, "evidence for AI."), ""}
		for i, m := range mk {
			line("  " + m + "  " + text[i])
		}
	default:
		line(p.bold("Tellproof") + " " + p.fg(colMuted, subtitle+" "+version))
	}
	return b.String()
}

// terminal reports whether w is an interactive terminal the banner may be
// drawn on, its width, and its colour mode.
func terminal(w io.Writer, getenv func(string) string) (ok bool, width int, p palette) {
	f, isFile := w.(*os.File)
	if !isFile || getenv("TELLPROOF_NO_BANNER") != "" || getenv("CI") != "" || getenv("TERM") == "dumb" {
		return false, 0, p
	}
	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false, 0, p
	}
	width = termWidth(f)
	if c, err := strconv.Atoi(getenv("COLUMNS")); err == nil && c > 0 {
		width = c
	}
	if width <= 0 {
		width = 80
	}
	if getenv("NO_COLOR") == "" && enableColor(f) {
		p.mode = 1
		if ct := getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
			p.mode = 2
		}
		if fgbg := getenv("COLORFGBG"); fgbg != "" {
			parts := strings.Split(fgbg, ";")
			bg := parts[len(parts)-1]
			p.light = bg == "7" || bg == "15"
		}
	}
	return true, width, p
}

// showBanner draws the banner on w when it is an interactive terminal.
func showBanner(w io.Writer, getenv func(string) string, subtitle string) bool {
	ok, width, p := terminal(w, getenv)
	if !ok {
		return false
	}
	fmt.Fprint(w, "\n"+renderBanner(p, width, version, subtitle)+"\n")
	return true
}

// welcome is what tellproof-agent with no arguments shows in a terminal.
func welcome(w io.Writer, getenv func(string) string) bool {
	ok, width, p := terminal(w, getenv)
	if !ok {
		return false
	}
	fmt.Fprint(w, welcomeText(p, width))
	return true
}

func welcomeText(p palette, width int) string {
	var b strings.Builder
	b.WriteString("\n" + renderBanner(p, width, version, "Evidence agent") + "\n")
	n := progName()
	cmds := [][2]string{
		{n + " init", "set up the app in this directory"},
		{n + " start", "run in the background, with a dashboard"},
		{n + " stop", "stop it; status: is it running, and where"},
		{n + " demo", "the dashboard on a synthetic week"},
		{n + " doctor", "diagnose the setup, with a fix for each"},
		{n + " check", "evaluate it against tellproof.policy.json"},
		{n + " --help", "every command and option"},
	}
	b.WriteString("  " + p.bold("Get started") + "\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "    %s  %s\n", pad(p.fg(colLetter, c[0]), 30), p.fg(colMuted, c[1]))
	}
	b.WriteString("\n  Then add " + p.fg(colLetter, "tellproof.init()") + " to your app:\n")
	b.WriteString("    " + p.fg(colLetter, `pip install "tellproof[agent]"`) + "   or   " + p.fg(colLetter, "npm install tellproof") + "\n")
	b.WriteString("  Docs: " + p.fg(colLetter, "https://lumetrix-advanced-technologies.github.io/tellproof/") + "\n\n")
	return b.String()
}

// localStatus replaces run --local's status lines under the banner.
func localStatus(w io.Writer, getenv func(string) string, agentID, otlp, dashboard, logDir string) {
	_, _, p := terminal(w, getenv)
	row := func(k, v, note string) {
		line := "  " + p.fg(colLetter, "●") + " " + pad(p.bold(k), 11) + v
		if note != "" {
			line += "  " + p.fg(colMuted, note)
		}
		fmt.Fprintln(w, line)
	}
	row("Spans", "http://"+otlp, "point your SDK here (TELLPROOF_AGENT_URL)")
	if dashboard != "" {
		row("Dashboard", dashboard, "")
	}
	row("Evidence", logDir, "signed by this agent, never sent")
	row("Agent", agentID, version)
	fmt.Fprintln(w, "\n  "+p.fg(colMuted, "In another terminal: "+progName()+" report  ·  Ctrl-C or "+progName()+" stop to stop"))
	fmt.Fprintln(w)
}

// sdkRE is the only form of TELLPROOF_SDK, which the SDKs' tellproof
// launchers set, that --version prints: a value from anywhere else could
// carry terminal escapes.
var sdkRE = regexp.MustCompile(`^tellproof [0-9A-Za-z.+-]{1,32} \((Python|Node\.js) SDK\)$`)

// sdkVersion is the SDK that started the agent as tellproof, or "".
func sdkVersion(getenv func(string) string) string {
	if v := getenv("TELLPROOF_SDK"); sdkRE.MatchString(v) {
		return v
	}
	return ""
}

// printVersion is --version: the banner and the versions in a terminal; for
// scripts, the agent's version on the first line as before, then the SDK's.
func printVersion(w io.Writer, getenv func(string) string) {
	sdk := sdkVersion(getenv)
	ok, width, p := terminal(w, getenv)
	if !ok {
		fmt.Fprintf(w, "tellproof-agent %s\n", version)
		if sdk != "" {
			fmt.Fprintln(w, sdk)
		}
		return
	}
	fmt.Fprint(w, versionText(p, width, sdk))
}

// versionText is --version in a terminal: the banner, then the versions.
func versionText(p palette, width int, sdk string) string {
	var b strings.Builder
	b.WriteString("\n" + renderBanner(p, width, version, "Evidence agent") + "\n")
	row := func(k, v string) {
		b.WriteString("  " + p.fg(colLetter, "●") + " " + pad(p.bold(k), 10) + v + "\n")
	}
	row("Agent", "tellproof-agent "+version)
	if sdk != "" {
		row("SDK", sdk)
	}
	row("Platform", runtime.GOOS+"/"+runtime.GOARCH)
	b.WriteString("\n")
	return b.String()
}
