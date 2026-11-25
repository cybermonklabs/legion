package pgmigrate

import "testing"

// WithoutComments reads a comment where Postgres reads one and nowhere else: each row is a text
// whose comment ends, or never starts, where a reader that ends a -- comment at a newline alone,
// or that skips no quoted span, reads it otherwise.
func TestWithoutCommentsReadsACommentWherePostgresReadsOne(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"update /* the name */ projects set name = name", "update   projects set name = name"},
		{"-- the rename\nupdate projects set name = $2", " \nupdate projects set name = $2"},
		// A -- comment ends at a carriage return too: scan.l's non_newline is [^\n\r].
		{"select 1 -- c\rupdate projects set name = name", "select 1  \rupdate projects set name = name"},
		// A /* */ comment nests, so the first */ does not end it.
		{"select /* a /* b */ c */ 1; update projects set name = name", "select   1; update projects set name = name"},
		// No comment starts inside a string, a quoted identifier or a dollar quote.
		{"select '--'; update projects set name = name", "select '--'; update projects set name = name"},
		{"select 'it''s -- no comment', '/*'; update projects set name = name", "select 'it''s -- no comment', '/*'; update projects set name = name"},
		{`select "a--b" from projects`, `select "a--b" from projects`},
		{"select $$/*$$; update projects set name = name", "select $$/*$$; update projects set name = name"},
		{"select $x$ -- $x$; update projects set name = name", "select $x$ -- $x$; update projects set name = name"},
		// A dollar quote's tag takes bytes 0x80-0xff, as scan.l's ident_start does.
		{"select $é$ -- $é$; update projects set name = name", "select $é$ -- $é$; update projects set name = name"},
		// A parameter opens no dollar quote.
		{"update projects set name = $1 -- the new name", "update projects set name = $1  "},
	} {
		if got := WithoutComments(tc.text); got != tc.want {
			t.Errorf("WithoutComments(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
