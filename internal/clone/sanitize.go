package clone

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Scrub is the required treatment for a line of external (git/ssh) output
// before it is shown or logged: Sanitize, then apply the credential maskers,
// then Sanitize again. Sanitizing first matters: a control sequence inserted
// inside a credential ("user:\x1b[31mtoken@host") would otherwise defeat the
// string match, and the later sanitize would then reassemble the secret.
func Scrub(line string, maskers ...func(string) string) string {
	line = Sanitize(line)
	for _, m := range maskers {
		line = m(line)
	}
	return Sanitize(line)
}

// Sanitize removes terminal control sequences and control characters from a
// line of external output (git, ssh, the remote server) before it is logged
// or rendered. Only printable text plus tab survives; in particular:
//
//   - CSI sequences   ESC [ ... final byte      (colours, cursor moves, clears)
//   - OSC sequences   ESC ] ... BEL | ESC \     (title changes, OSC 52 clipboard)
//   - other ESC-introduced sequences (ESC + one byte, or DCS/APC/PM/SOS strings)
//   - C0 controls except tab, DEL, and C1 controls (U+0080..U+009F)
//   - Unicode format characters (bidi overrides/isolates, zero-width chars)
//
// are dropped. Credential masking is a plain string replacement and does not
// touch these bytes, so this must run in addition to it.
func Sanitize(s string) string {
	if !needsSanitize(s) {
		return s
	}
	// Invalid UTF-8 is never passed through: a raw 0x9b byte (8-bit CSI) is
	// not a valid encoding of U+009B, so ranging over the string would yield
	// U+FFFD and the terminal would still see the original control byte.
	// Decode byte-wise instead, mapping each invalid byte to its C1 rune if
	// it is in the C1 range (so the control-sequence logic below sees it)
	// and dropping it otherwise. Valid multi-byte characters are untouched.
	rs := decodeRunes(s)
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == 0x1b { // ESC
			i += skipEscape(rs[i+1:])
			continue
		}
		if r == 0x9b || r == 0x9d || r == 0x90 || r == 0x9e || r == 0x9f || r == 0x98 { // C1 CSI/OSC/DCS/PM/APC/SOS
			i += skipString(rs[i+1:], r == 0x9b)
			continue
		}
		if allowedRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// allowedRune is the single definition of what may reach the screen or the
// log: printable characters plus tab. This excludes C0/C1 controls and also
// Unicode format characters (bidi overrides such as U+202E, isolates such as
// U+2066, zero-width characters such as U+200B) that can reorder or hide
// text and so disguise what a remote actually sent.
func allowedRune(r rune) bool {
	return r == '\t' || unicode.IsPrint(r) || r == ' '
}

// needsSanitize is the fast path check; it must use the same predicate as
// the slow path so that nothing the slow path would drop can slip through.
func needsSanitize(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if !allowedRune(r) {
			return true
		}
	}
	return false
}

// decodeRunes decodes s as UTF-8; an invalid byte becomes the rune of the
// same value when it is a C1 control (0x80..0x9f) and is dropped otherwise.
func decodeRunes(s string) []rune {
	rs := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if c := s[i]; c >= 0x80 && c <= 0x9f {
				rs = append(rs, rune(c))
			}
			i++
			continue
		}
		rs = append(rs, r)
		i += size
	}
	return rs
}

// skipEscape returns how many runes after an ESC belong to the sequence.
func skipEscape(rest []rune) int {
	if len(rest) == 0 {
		return 0
	}
	switch rest[0] {
	case '[': // CSI: parameter/intermediate bytes 0x20..0x3f, final 0x40..0x7e
		i := 1
		for i < len(rest) && rest[i] >= 0x20 && rest[i] <= 0x3f {
			i++
		}
		if i < len(rest) {
			i++ // final byte
		}
		return i
	case ']', 'P', '^', '_', 'X': // OSC, DCS, PM, APC, SOS: until BEL or ST (ESC \)
		return 1 + skipString(rest[1:], false)
	default: // two-character escape (ESC x)
		return 1
	}
}

// skipString consumes a control string up to and including its terminator
// (BEL or ESC \ or C1 ST). For CSI (csi=true) it consumes up to the final byte.
func skipString(rest []rune, csi bool) int {
	if csi {
		i := 0
		for i < len(rest) && rest[i] >= 0x20 && rest[i] <= 0x3f {
			i++
		}
		if i < len(rest) {
			i++
		}
		return i
	}
	for i := range rest {
		switch rest[i] {
		case 0x07, 0x9c: // BEL, C1 ST
			return i + 1
		case 0x1b:
			if i+1 < len(rest) && rest[i+1] == '\\' {
				return i + 2
			}
		}
	}
	return len(rest) // unterminated: drop the remainder
}
