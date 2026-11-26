package log

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EscapeControlChars escapes the control characters in s, as Go escapes them in a quoted string: CR is `\r`,
// LF is `\n`, ESC is `\x1b`, NEL is `\u0085`. It also escapes U+2028 and U+2029, which some log viewers treat as line
// breaks, and invalid UTF-8 bytes, as `\xff`. Everything else is kept as is, so the result is still readable.
//
// Untrusted text written to a log or an error message, such as a PV's bucket name or Mountpoint's stderr, must
// go through it: a raw CR or LF there splits one record into two, and lets the text forge a record of its own.
func EscapeControlChars(s string) string {
	if strings.IndexFunc(s, needsEscape) < 0 && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case needsEscape(r):
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsEscape(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}
