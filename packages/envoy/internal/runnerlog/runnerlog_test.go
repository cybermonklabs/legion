package runnerlog

import (
	"strings"
	"testing"
)

// Say writes its prefix, then the formatted rest, the whole line escaped by Printable and ended
// by one "\n": an argument's line break prints as \n, and a "##[" that the prefix and the text
// after it make together is escaped too.
func TestSay(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, prefix, format string
		args                 []any
		want                 string
	}{
		{name: "an ordinary line", prefix: "architecture-check: ", format: "%s: %d notes, the set is valid", args: []any{"/checkout/.dispatch/architecture", 3}, want: "architecture-check: /checkout/.dispatch/architecture: 3 notes, the set is valid\n"},
		{name: "a line break in an argument", prefix: "architecture-check: ", format: "%s: holds no note", args: []any{"x\n::error::forged"}, want: `architecture-check: x\n::error::forged: holds no note` + "\n"},
		{name: "a percent sign in the prefix", prefix: "100%: ", format: "%d", args: []any{7}, want: "100%: 7\n"},
		{name: "a command of the second form across the prefix", prefix: "p#", format: "%s", args: []any{"#[error]y"}, want: `p##\x5berror]y` + "\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			Say(&out, row.prefix, row.format, row.args...)
			if got := out.String(); got != row.want {
				t.Errorf("Say(%q, %q, %q) wrote %q, want %q", row.prefix, row.format, row.args, got, row.want)
			}
		})
	}
}

// Printable leaves an ordinary name as it is, a name that is not ASCII included, escapes each
// control character, each format character, the line and paragraph separators and each byte that
// is not UTF-8 as a Go string literal writes it, and writes the "[" of every "##[" as \x5b.
func TestPrintable(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, in, want string }{
		{name: "an ordinary path", in: "/checkout/.dispatch/architecture/api.md", want: "/checkout/.dispatch/architecture/api.md"},
		{name: "a name that is not ASCII", in: "café.md", want: "café.md"},
		{name: "a line feed", in: "x\n::error::forged", want: `x\n::error::forged`},
		{name: "a carriage return", in: "y\r::warning::forged", want: `y\r::warning::forged`},
		{name: "a tab", in: "a\tb", want: `a\tb`},
		{name: "an escape", in: "a\x1b[31mb", want: `a\x1b[31mb`},
		{name: "a delete", in: "a\x7fb", want: `a\x7fb`},
		{name: "a C1 next line", in: "a\u0085b", want: `a\u0085b`},
		{name: "a byte that is not UTF-8", in: "a\xffb", want: `a\xffb`},
		{name: "a command of the second form", in: "##[", want: `##\x5b`},
		{name: "three number signs", in: "###[", want: `###\x5b`},
		{name: "two commands of the second form", in: "##[##[", want: `##\x5b##\x5b`},
		{name: "a command of the second form inside a name", in: "x##[error]y", want: `x##\x5berror]y`},
		{name: "a soft hyphen", in: "#\u00ad#[error]y", want: `#\u00ad#[error]y`},
		{name: "a zero-width joiner", in: "#\u200d#[error]y", want: `#\u200d#[error]y`},
		{name: "a byte order mark", in: "\ufeff::error::forged", want: `\ufeff::error::forged`},
		{name: "a right-to-left override", in: "a\u202eb.md", want: `a\u202eb.md`},
		{name: "a line separator", in: "x\u2028::error::y.md", want: `x\u2028::error::y.md`},
		{name: "a paragraph separator", in: "x\u2029::error::y.md", want: `x\u2029::error::y.md`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := Printable(row.in); got != row.want {
				t.Errorf("Printable(%q) = %q, want %q", row.in, got, row.want)
			}
		})
	}
}
