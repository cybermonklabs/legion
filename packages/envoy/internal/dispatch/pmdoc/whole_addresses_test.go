package pmdoc

import (
	"strings"
	"testing"
)

// A person's address whose login holds a dot or a hyphen is stored as written: the write reads it
// as one text with no link, and the markdown it is stored as is the markdown written, so the
// stored document names the person by the address its writer wrote.
func TestABarePersonAddressIsStoredAsWritten(t *testing.T) {
	for _, markdown := range []string{
		"Ask dispatch://person/john.doe@example.com now.\n",
		"Ask dispatch://person/person-2@example.com now.\n",
	} {
		doc, written, err := ParseWritten(markdown, nil)
		if err != nil {
			t.Fatalf("ParseWritten(%q) = %v", markdown, err)
		}
		if got, want := inlineSummaries(doc), `text "`+strings.TrimSuffix(markdown, "\n")+`"`; got != want {
			t.Errorf("%q: the write reads %s, want %s", markdown, got, want)
		}
		if written.Markdown != markdown {
			t.Errorf("%q is stored as %q", markdown, written.Markdown)
		}
	}
}

// No email or URL autolink starts inside a bare `dispatch://` address, whichever reader would
// have started it: the tokenizer after a `=`, a `;` or a `_` (a `_` that a character reference
// spells included, which only the transform reads), or the transform after a `.` or a `-`, in a
// paragraph, under emphasis or in a table cell. A link the tokenizer read comes back as its source
// as written, as it showed linked; one the transform read comes back decoded. An autolink outside
// an address is made as before: an angle autolink right after one, an email after the address has
// ended at a space or a line's end, and a URL whose own text holds `dispatch://`. Go's reading of
// each row is the browser editor's engine's.
func TestNoAutolinkStartsInsideAnAddress(t *testing.T) {
	rows := []struct {
		markdown string
		want     string
	}{
		// The defect as reported: a person's login holding a dot, and one holding a hyphen and a
		// plus.
		{"Ask dispatch://person/john.doe@example.com now.\n", `text "Ask dispatch://person/john.doe@example.com now."`},
		{"Ask dispatch://person/a.b-c+d@x-y.test now.\n", `text "Ask dispatch://person/a.b-c+d@x-y.test now."`},
		{"Answer dispatch://CORE-1/ask/a1?x=foo@bar.com today.\n", `text "Answer dispatch://CORE-1/ask/a1?x=foo@bar.com today."`},
		{"See dispatch://CORE-1/a_www.example.com/x first.\n", `text "See dispatch://CORE-1/a_www.example.com/x first."`},
		{"See dispatch://CORE-1/x;https://example.com/y first.\n", `text "See dispatch://CORE-1/x;https://example.com/y first."`},
		{"See dispatch://CORE-1/a_www.example.com/x&amp;y first.\n", `text "See dispatch://CORE-1/a_www.example.com/x&amp;y first."`},
		{"See dispatch://CORE-1/x&#95;www.example.com first.\n", `text "See dispatch://CORE-1/x_www.example.com first."`},
		{
			"*dispatch://person/john.doe@example.com* and **dispatch://person/person-2@example.com**\n",
			`text "dispatch://person/john.doe@example.com" | text " and " | text "dispatch://person/person-2@example.com"`,
		},
		{"| a |\n| - |\n| dispatch://person/john.doe@example.com |\n", `text "a" || text "dispatch://person/john.doe@example.com"`},
		{"See dispatch://CORE-1/x<https://a.test> now.\n", `text "See dispatch://CORE-1/x" | text "https://a.test" link https://a.test | text " now."`},
		{"See dispatch://CORE-1. Mail a@b.cc now.\n", `text "See dispatch://CORE-1. Mail " | text "a@b.cc" link mailto:a@b.cc | text " now."`},
		// The address ends at its line's end, so the email that opens the next line is outside it.
		{"Ask dispatch://CORE-1  \na@b.cc\n", `text "Ask dispatch://CORE-1" | hardbreak "" | text "a@b.cc" link mailto:a@b.cc`},
		{
			"https://a.test/dispatch://person/john.doe@example.com\n",
			`text "https://a.test/dispatch://person/john.doe@example.com" link https://a.test/dispatch://person/john.doe@example.com`,
		},
	}
	markdowns := make([]string, len(rows))
	docs := make([]*Node, len(rows))
	for index, row := range rows {
		doc, err := Parse(row.markdown)
		if err != nil {
			t.Fatalf("Parse(%q) = %v", row.markdown, err)
		}
		markdowns[index], docs[index] = row.markdown, doc
		if got := inlineSummaries(doc); got != row.want {
			t.Errorf("%q:\n got %s\nwant %s", row.markdown, got, row.want)
		}
	}
	readings := engineReadings(t, markdowns)
	for index, row := range rows {
		if reading := readings[index]; reading == nil || !docs[index].Equal(reading) {
			t.Errorf("%q: Go reads %s, the engine %s", row.markdown, inlineSummaries(docs[index]), inlineSummaries(reading))
		}
	}
}
