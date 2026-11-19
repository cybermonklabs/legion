// Package runnerlog writes the lines a command prints into the log of a GitHub Actions runner,
// such as cmd/architecture-check's, so that no text the command takes from a file name, a file or
// an argument starts a line of its own or names a workflow command.
//
// A GitHub runner ends a line of the log at \n and at \r, and reads a workflow command in two
// forms: a line that starts with "::" once its leading white space is trimmed, and a "##["
// anywhere in a line. Say writes each line as the command's fixed prefix and the rest, all of it
// escaped by Printable, so every line starts with the prefix, no line break of the text reaches
// the log, and no line holds a "##[".
package runnerlog

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Say writes one line to w: prefix, then format filled with args, the whole escaped by Printable,
// then "\n". prefix is the command's own fixed text, such as "architecture-check: ", which starts
// no workflow command and holds nothing Printable escapes, so it is written as it is and every
// line Say writes starts with it whatever the format. No print site can leave an argument
// unescaped, a "##[" that the prefix, the format text and an argument make together is escaped
// too, and Printable keeps the rest of the line on it, so no text of an argument starts a line.
func Say(w io.Writer, prefix, format string, args ...any) {
	fmt.Fprintln(w, Printable(prefix+fmt.Sprintf(format, args...)))
}

// Printable is s as a command prints it in a line of a runner's log. A file name or a front-matter
// value in a pull request is its author's text, and a GitHub runner reads workflow commands from
// the log: it ends a line at \n and at \r alike, and reads a command in two forms, a line that
// starts with "::" once its leading white space is trimmed, and a "##[" anywhere in a line.
//
// Printable writes each control character (Unicode category Cc: C0, DEL and C1) as a Go string
// literal writes it, `\n`, `\r` or `\x1b`, so no line break of s reaches the log and no text of s
// starts a line of its own: every line Say writes starts with its prefix.
//
// Then it writes the "[" of every "##[" as `\x5b`, so "###[" prints as `###\x5b` and "##[##[" as
// `##\x5b##\x5b`. No escape holds a "#" or a "[", so no "##[" is left, and that keeps out every
// command of the second form, since only a literal "##[" names one. The runner finds "##[" with a
// culture-sensitive search, which can pass over an ignorable character such as U+034F or U+FE0F
// as if it were not there. But it then reads the command name a fixed three UTF-16 code units after the
// start of the match, and accepts only a name of its registered commands, compared without regard
// to case. So in a match through an ignorable character the name starts inside the match, at a
// "#", a "[" or that character, and no command the runner defines has that name. In actions/runner
// at commit 8cfa1049f86781d141056082d73802d56cd2b433, ActionCommand.TryParse reads the name at
// cmdIndex = prefixIndex + Prefix.Length, and ActionCommandManager keeps the registered commands
// in _registeredCommands, a set built with StringComparer.OrdinalIgnoreCase.
//
// Printable also writes each format character (Cf: the bidi controls, the zero-width joiner and
// non-joiner, the soft hyphen, the byte order mark) as `\u202e`, `\u200d`, `\u00ad` or `\ufeff`,
// the line separator U+2028 (Zl) and the paragraph separator U+2029 (Zp) as `\u2028` and
// `\u2029`, and each byte that is not UTF-8 as `\xNN`. That is for a reader of the log, not for
// the runner: a format character is invisible or reorders the text around it, so it can make one
// name read as another, and the runner does not end a line at U+2028 or U+2029 (it reads lines
// with StreamReader.ReadLine), but a log viewer can show a line break there. Text that is valid
// UTF-8 and holds none of these is s itself.
func Printable(s string) string {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, escaped) {
		var b strings.Builder
		for i := 0; i < len(s); {
			r, size := utf8.DecodeRuneInString(s[i:])
			if (r == utf8.RuneError && size == 1) || escaped(r) {
				// strconv quotes a lone byte that is not UTF-8 as \xNN, and r as its escape.
				var quoted [12]byte // `"\U0010ffff"`, the longest a rune quotes to
				escape := strconv.AppendQuote(quoted[:0], s[i:i+size])
				b.Write(escape[1 : len(escape)-1])
			} else {
				b.WriteString(s[i : i+size])
			}
			i += size
		}
		s = b.String()
	}
	return strings.ReplaceAll(s, "##[", `##\x5b`)
}

// escaped reports whether Printable writes r as an escape: a control character (Unicode category
// Cc), such as a line break or a terminal escape, a format character (Cf), which is invisible or
// reorders text, or the line separator U+2028 (Zl) or the paragraph separator U+2029 (Zp), at
// which a log viewer can show a line break.
func escaped(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}
