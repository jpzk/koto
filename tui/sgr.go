package main

// sgr.go — the one place the TUI takes an escape sequence apart.
//
// Four sites filter, rewrite or strip escapes in rendered text: the padding
// trim under glamour (markdown.go), the page-ground re-assertion (theme.go),
// the colour strip for monochrome terminals (mono.go) and the shell pane's
// output scrub (vt_scrub.go). Each used to carry its own copy of the two
// things this file now owns — walking to the end of a CSI sequence, and
// splitting an SGR parameter list with the 38/48/58 extended-colour
// arguments consumed along the way. Four copies, four sets of edge cases,
// and the copy in markdown.go walked BACKWARD from a trailing 'm', which
// cannot be done: a line of prose ending in "from" reads as an SGR
// terminator from behind, and no validation after the fact repairs a guess
// (bd6d4cb — 1011 transcript lines lost mid-sentence). Forward, ESC [ is
// unambiguous, so the scanner here only ever runs forward, and the parameter
// parser checks every argument it consumes instead of stepping over it.
//
// The grammar is ECMA-48's: after ESC [, parameter and intermediate bytes
// 0x20–0x3f, then one final byte 0x40–0x7e. A CSI is an SGR when its final
// byte is 'm' AND its parameters are nothing but digits, ';' and ':' — the
// private-prefixed forms (CSI > … m is xterm's modifyOtherKeys, CSI ? … m
// exists too) are not SGR despite the final byte, and treating them as one
// would turn \x1b[>4;2m into faint text.

import "strings"

// escKind classifies what scanEsc found at an ESC.
type escKind uint8

const (
	escSGR       escKind = iota // CSI … m, pure parameters: a styling sequence
	escBadSGR                   // CSI … m without a private prefix but with bytes no SGR has (an intermediate, a space): meant as styling, honoured by no terminal
	escCSI                      // any other complete CSI (cursor motion, erase, private modes…)
	escString                   // OSC / DCS / SOS / PM / APC, through its BEL or ST terminator when present
	escTwoByte                  // ESC + one byte (RIS, DECSC, keypad…), or + two for charset designation / SS2 / SS3
	escAbort                    // ESC ESC — the first is void; end is the second, to be scanned again
	escMalformed                // a byte that can't be part of a CSI at s[end]; drop s[i:end], resume there
	escTrunc                    // the sequence runs off the end of s
)

// scanEsc classifies the escape sequence starting at s[i], which must be an
// ESC, and returns where it ends (exclusive) — so a caller's loop resumes at
// s[end:]. For escSGR and escCSI, params is the text between "\x1b[" and the
// final byte. end is always past i, and never past len(s).
//
// Where the scanner stops on a broken sequence matters as much as what it
// says about it: for escAbort and escMalformed, end is ON the byte that
// broke the sequence, not past it, so the caller judges that byte on its
// own. Consuming it as sequence body would let a follow-up sequence smuggle
// itself through — \x1b\x1b[31m read as a two-byte escape plus text would
// deliver the colour.
func scanEsc(s string, i int) (kind escKind, params string, end int) {
	n := len(s)
	if i+1 >= n {
		return escTrunc, "", n
	}
	switch s[i+1] {
	case '[':
		j := i + 2
		for j < n && s[j] >= 0x20 && s[j] <= 0x3f {
			j++
		}
		if j >= n {
			return escTrunc, "", n
		}
		if s[j] < 0x40 || s[j] > 0x7e {
			return escMalformed, "", j
		}
		params = s[i+2 : j]
		if s[j] == 'm' {
			switch {
			case isSGRParams(params):
				return escSGR, params, j + 1
			case params == "" || params[0] < '<' || params[0] > '?':
				return escBadSGR, params, j + 1
			}
		}
		return escCSI, params, j + 1
	case ']', 'P', 'X', '^', '_':
		// String sequences run to ST (ESC \). BEL ends only an OSC — an
		// xterm convenience for titles — and is plain data inside the other
		// four; taking it as a terminator there hands the rest of a DCS or
		// SOS payload to the caller as text (found by FuzzScrubVTKeepsText).
		osc := s[i+1] == ']'
		for j := i + 2; j < n; j++ {
			switch s[j] {
			case 0x07:
				if osc {
					return escString, "", j + 1
				}
			case 0x1b:
				if j+1 < n && s[j+1] == '\\' {
					return escString, "", j + 2
				}
				return escString, "", j // aborted by a new escape — hand it back
			}
		}
		return escString, "", n
	case 0x1b:
		return escAbort, "", i + 1
	case '(', ')', '*', '+', 'N', 'O': // charset designation, SS2/SS3: the selected byte rides along
		return escTwoByte, "", min(i+3, n)
	default:
		return escTwoByte, "", i + 2
	}
}

// isSGRParams reports whether s could be the parameter text of an SGR: only
// digits, ';' and ':' (the sub-parameter separator) between "\x1b[" and the
// final 'm'. This is the precondition forEachSGRAttr assumes, and the one
// thing that separates a real escape from a line of prose that happens to
// end in the letter 'm'.
func isSGRParams(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}

// sgrAttr is one parsed SGR parameter. code is the attribute number, or -1
// for a field that isn't a number at all (a colon sub-parameter form, or
// garbage). For the extended colours (38/48/58) sub is the colour-space
// selector that followed — 5 indexed, 2 truecolor — and args the numbers
// consumed with it. raw is the parameter's own text, arguments included, so
// a consumer can re-emit it unchanged. bad marks anything that isn't a
// well-formed attribute: a non-numeric field, or an extended colour whose
// arguments are missing, non-numeric, or under an unknown selector.
type sgrAttr struct {
	code  int
	sub   int
	args  [3]int
	nargs int
	raw   string
	bad   bool
}

// forEachSGRAttr walks an SGR parameter list, calling fn once per attribute
// with that attribute's arguments already consumed; fn returns false to stop
// early. Reports whether every attribute was well-formed. The empty list is
// the bare reset (ECMA-48's default parameter) and yields one attr, code 0.
//
// It allocates nothing: themeFrame runs it on every SGR of every frame.
func forEachSGRAttr(params string, fn func(sgrAttr) bool) (ok bool) {
	ok = true
	for start := 0; start <= len(params); {
		field, next := sgrField(params, start)
		a := sgrAttr{raw: field}
		if n, num := sgrNumber(field); !num {
			a.code, a.bad = -1, true
		} else {
			a.code = n
			if n == 38 || n == 48 || n == 58 {
				a, next = sgrExtended(params, a, start, next)
			}
		}
		if a.bad {
			ok = false
		}
		if !fn(a) {
			return ok
		}
		start = next
	}
	return ok
}

// sgrExtended consumes the arguments of an extended-colour introducer
// (38/48/58): a selector — 5 for indexed, 2 for truecolor — then one or
// three numbers. A selector it doesn't know is consumed and the attribute
// marked bad; so is one whose numbers are missing or aren't numbers. Every
// argument is CHECKED, not stepped over: this parser is handed candidate
// spans, and a span that is not an SGR must come back bad rather than pass
// on the strength of an index nobody read.
func sgrExtended(params string, a sgrAttr, start, next int) (sgrAttr, int) {
	if next > len(params) { // the introducer is the last field
		a.bad = true
		return a, next
	}
	sel, selNext := sgrField(params, next)
	rawEnd := selNext - 1
	next = selNext
	n, num := sgrNumber(sel)
	a.sub = n
	want := 0
	switch {
	case num && n == 5:
		want = 1
	case num && n == 2:
		want = 3
	default:
		a.bad = true
	}
	for k := 0; k < want; k++ {
		if next > len(params) {
			a.bad = true
			break
		}
		f, fNext := sgrField(params, next)
		v, vnum := sgrNumber(f)
		if !vnum {
			a.bad = true
		}
		a.args[k] = v
		a.nargs++
		rawEnd = fNext - 1
		next = fNext
	}
	a.raw = params[start:rawEnd]
	return a, next
}

// sgrField returns the parameter field starting at params[start] and the
// start of the one after it — len(params)+1 when this was the last, so a
// loop `for start := 0; start <= len(params); start = next` visits every
// field exactly once, including the single empty field an empty list is.
func sgrField(params string, start int) (field string, next int) {
	if j := strings.IndexByte(params[start:], ';'); j >= 0 {
		return params[start : start+j], start + j + 1
	}
	return params[start:], len(params) + 1
}

// sgrNumber parses one decimal parameter. An empty field is 0 — the default
// parameter, a bare reset. Anything that isn't plain digits, or is absurdly
// large, is not a parameter.
func sgrNumber(f string) (int, bool) {
	// A parameter is at most four digits, leading zeros included (audit
	// 2026-09-11 L122). The value check below bounds the NUMBER, which
	// `ESC[0000…0m` never exceeds — so a field of a hundred thousand zeros
	// parsed as a valid SGR 0, and the sanitizers, which keep pure SGR, passed
	// the whole raw field through to be re-scanned and re-emitted on every
	// frame and then parsed again by the operator's terminal. A zero-width
	// payload that costs CPU, allocations and frame bandwidth at every hop.
	// No real SGR parameter needs more than four digits (the largest is 107,
	// and truecolor components reach 255).
	// The EMPTY field stays valid: ECMA-48's default parameter is 0, and a
	// bare `ESC[m` is the reset every styled span ends with.
	if len(f) > 4 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1000 {
			return 0, false
		}
	}
	return n, true
}

// --- 2026-09-11 M141: not every "pure SGR" is styling ------------------------

// sgrApproved filters an SGR parameter list down to the attributes untrusted
// output may set, and reports whether anything survived. Same policy as
// daemon/sanitize.go's function of the same name — the daemon module cannot be
// imported here, and the two sanitizers have been deliberate mirrors since they
// were written; this one is expressed on forEachSGRAttr rather than re-parsing.
//
// The codes it exists to remove are not styling but deception: 8 (conceal)
// makes a warning, or the attribution quoting a peer's report, invisible while
// the text still reads as complete; 7 (reverse) is koto's OWN vocabulary for
// "this is the UI, not content" — the bars, the chip pair, the tree cursor —
// and in mono mode the only signal they have; 5 and 6 (blink) manufacture
// urgency the renderer never asked for. Their resets (25, 27, 28) go with them,
// because a lone "reverse off" inside a row koto is drawing reversed escapes
// the highlight as effectively as a "reverse on" imitates it.
//
// An allowlist, not a denylist: the parameter space is open, and a code nobody
// here has heard of should not render until somebody decides it is styling. A
// malformed attribute — which for 38/48/58 means a missing or unknown extended
// form — drops the WHOLE sequence, because resyncing past it would reinterpret
// that form's arguments as codes in their own right (`38;7` would leave a 7).
func sgrApproved(params string) (string, bool) {
	var out []string
	bad := false
	ok := forEachSGRAttr(params, func(a sgrAttr) bool {
		if a.bad {
			return false
		}
		for k := 0; k < a.nargs; k++ {
			if a.args[k] > 255 { // not a colour component
				bad = true
				return false
			}
		}
		if a.code == 38 || a.code == 48 || a.code == 58 || sgrStyleCode(a.code) {
			out = append(out, a.raw)
		}
		return true
	})
	if !ok || bad || len(out) == 0 {
		return "", false
	}
	return strings.Join(out, ";"), true
}

// sgrStyleCode reports whether a single SGR parameter is one of the styling
// attributes untrusted output may set: weights, decorations, and the basic and
// bright colour pairs. See sgrApproved for what is missing and why.
func sgrStyleCode(n int) bool {
	switch {
	case n == 0, // reset
		n == 1, n == 2, // bold, dim
		n == 3, n == 4, // italic, underline
		n == 9,           // strikethrough
		n == 21, n == 22, // double underline, normal intensity
		n == 23, n == 24, n == 29, // no italic / no underline / no strike
		n == 26,          // proportional spacing
		n == 53, n == 55, // overline, no overline
		n == 39, n == 49, n == 59: // default fg / bg / underline colour
		return true
	case n >= 30 && n <= 37, n >= 40 && n <= 47: // basic fg/bg
		return true
	case n >= 90 && n <= 97, n >= 100 && n <= 107: // bright fg/bg
		return true
	}
	return false
}
