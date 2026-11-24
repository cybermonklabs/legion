package pgmigrate

import (
	"slices"
	"strings"
	"testing"
)

// WithoutComments blanks a comment where Postgres reads one and nowhere else, byte for byte, a
// line break kept, so the answer is as long as the text: each row is a text whose comment ends, or
// never starts, where a reader that ends a -- comment at a newline alone, or that skips no quoted
// span, reads it otherwise.
func TestWithoutCommentsReadsACommentWherePostgresReadsOne(t *testing.T) {
	for _, tc := range []struct {
		text string
		// comments are the comments of text, in order, each of which the answer blanks.
		comments []string
	}{
		{"update /* the name */ projects set name = name", []string{"/* the name */"}},
		{"-- the rename\nupdate projects set name = $2", []string{"-- the rename"}},
		// A -- comment ends at a carriage return too: scan.l's non_newline is [^\n\r].
		{"select 1 -- c\rupdate projects set name = name", []string{"-- c"}},
		// A /* */ comment nests, so the first */ does not end it; its line break stays.
		{"select /* a /* b\n */ c */ 1; update projects", []string{"/* a /* b\n */ c */"}},
		// No comment starts inside a string or a quoted identifier.
		{"select '--'; update projects set name = name", nil},
		{"select 'it''s -- no comment', '/*'; update projects", nil},
		{`select "a--b" from projects`, nil},
		// A dollar quote's tag takes bytes 0x80-0xff, as scan.l's ident_start does, so the text
		// between the two tags is the quote's body and the -- after the second is a comment.
		{"select $é$ x $é$; update projects -- why", []string{"-- why"}},
		// A parameter opens no dollar quote.
		{"update projects set name = $1 -- the new name", []string{"-- the new name"}},
		// A dollar quote's body is read as SQL: its comments are blanked, its strings stay.
		{"do $$ begin -- delete from graph_nodes\n execute '-- kept'; end $$", []string{"-- delete from graph_nodes"}},
		{"select $x$ -- $x$; update projects", []string{"-- "}},
		// A dollar quote inside a body is a string to PL/pgSQL, kept whole: the -- in it is string
		// text, though the body's own comment around it is one.
		{"do $$ begin -- why\n execute $b$ -- inner $b$; end $$", []string{"-- why"}},
		{"do $$ begin execute $b$select 1 -- note$b$; end $$", nil},
	} {
		want, rest := "", tc.text
		for _, comment := range tc.comments {
			before, after, found := strings.Cut(rest, comment)
			if !found {
				t.Fatalf("the row %q holds no comment %q after the ones before it", tc.text, comment)
			}
			blank := []byte(comment)
			for i, c := range blank {
				if c != '\n' && c != '\r' {
					blank[i] = ' '
				}
			}
			want, rest = want+before+string(blank), after
		}
		want += rest
		if got := WithoutComments(tc.text); got != want {
			t.Errorf("WithoutComments(%q) = %q, want %q", tc.text, got, want)
		}
	}
}

// Statements splits at the semicolons Postgres reads as such: none inside a string, a quoted
// identifier, a dollar quote or a comment.
func TestStatementsSplitsAtTheSemicolonsOutsideQuotesAndComments(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"alter table asks add column a text;\n-- why\nselect 1;", []string{"alter table asks add column a text", "select 1"}},
		{"select ';'; select \"a;b\"; select $$;$$ -- ;\n", []string{"select ';'", `select "a;b"`, "select $$;$$"}},
		{"alter table asks add column a text; -- c\rinsert into asks values (1);", []string{"alter table asks add column a text", "insert into asks values (1)"}},
		{"  ;\n-- only a comment\n;", nil},
	} {
		if got := Statements(tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("Statements(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}
