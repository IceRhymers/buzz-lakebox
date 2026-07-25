// Package shellquote provides the one sanctioned way to interpolate
// untrusted or payload data into a POSIX shell command/script string.
package shellquote

import "strings"

// Single single-quotes s for safe interpolation into a POSIX shell
// script, escaping any embedded single quote via the standard
// close-quote/escaped-quote/reopen-quote trick ('\”). Single-quoted
// shell strings preserve embedded newlines literally, so multi-line
// values (e.g. system_prompt) need no further escaping.
//
// This is the ONLY approved way to interpolate untrusted/payload data
// into shell text. Do not hand-roll quoting elsewhere; for trusted
// static literals (e.g. fixed "$HOME"-relative paths that must expand),
// use a double-quoted literal instead — see internal/deployflow.dquote.
func Single(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
