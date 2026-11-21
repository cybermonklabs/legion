package text

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// headRefsRow is one cut HeadRefs is held to: text cut at limit runes with ellipsis, read against
// the deployment at origin ("" where the caller knows none), is head. Note says why, where a row
// needs saying, and is read by no test.
type headRefsRow struct {
	Name     string `json:"name"`
	Text     string `json:"text"`
	Limit    int    `json:"limit"`
	Ellipsis string `json:"ellipsis"`
	Origin   string `json:"origin"`
	Head     string `json:"head"`
	Note     string `json:"note"`
}

// linkTails is a single line of 1 MiB that is nothing but link tails, `](dispatch://CORE-1)`: a
// body every one of whose addresses stands where a link's would, and none of which a `[` opens.
var linkTails = strings.Repeat("](dispatch://CORE-1)", (1<<20)/len("](dispatch://CORE-1)"))

// TestHeadRefsMatchesTheSharedHeadTable holds HeadRefs to its rule through the table the
// dashboard's headRefs is held to as well (`testdata/head-refs.json`, which
// `packages/dispatch/web/src/features/refs/head-refs.test.ts` reads), so the two copies cut every
// row alike: a reference or a code span that crosses the cut moves the cut back to its start, and
// so does a reference the head would read that the text does not write, while a text with no
// reference at the cut, a URL of anywhere else, and a reference no cut can keep whole are cut as
// HeadRunes cuts them, with the link or the code span that cut lands in closed after it.
func TestHeadRefsMatchesTheSharedHeadTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/head-refs.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []headRefsRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared head table is empty")
	}
	for _, row := range rows {
		if got := HeadRefs(row.Text, row.Limit, row.Ellipsis, row.Origin); got != row.Head {
			t.Errorf("%s: HeadRefs(%.80q, %d, %q, %q) = %.200q, want %.200q",
				row.Name, row.Text, row.Limit, row.Ellipsis, row.Origin, got, row.Head)
		}
	}
}

// TestHeadRefsReadsAWindowOfTheText holds HeadRefs to the window it reads with texts the shared
// table does not hold: a line of 1 MiB, which would make the table's file megabytes long, an
// address longer than the window, and a code span whose closing backtick is the window's last
// character, which needs the window's length counted out. The dashboard's copy reads the same
// window, holds the last of these too (`head-refs.test.ts`), and the table's own window rows, a
// link whose `)` stands past the window and code spans closing past it, hold both copies to it.
func TestHeadRefsReadsAWindowOfTheText(t *testing.T) {
	longAddress := "dispatch://CORE-1/artifact/" + strings.Repeat("x", 200)
	unit := "](dispatch://CORE-1)"
	for _, row := range []headRefsRow{
		{Name: "an address longer than the window is cut by runes", Text: "Read " + longAddress + strings.Repeat("x", 1000) + " now",
			Limit: 160, Ellipsis: "…", Head: ("Read " + longAddress + strings.Repeat("x", 1000))[:159] + "…"},
		// The window is the bound and 1,024 runes past it, so its last rune is the backtick after
		// the 1,028 x's. The window reads a span that closes there, but the text's run of two
		// backticks there may close nothing, so the span may run past the window: it counts as
		// crossing the cut, which moves back to its backtick rather than leaving it open.
		{Name: "a code span whose closing backtick ends the window counts as crossing the cut",
			Text: "See `" + strings.Repeat("x", 1028) + "`` and more", Limit: 10, Ellipsis: "…", Head: "See…"},
		// The `](` before the ellipsis is text, and stays: the line holds no `[`, so it opens no
		// link and the cut splits none. A link that crosses the cut moves it back to the link's
		// `[`, so a head can end in a `](` only where the text writes no link there.
		{Name: "a line of 1 MiB cut as a first line", Text: linkTails, Limit: 160, Ellipsis: "…", Head: strings.Repeat(unit, 7) + "](…"},
		{Name: "a line of 1 MiB cut as an excerpt", Text: linkTails, Limit: 240, Head: strings.Repeat(unit, 12)},
	} {
		if got := HeadRefs(row.Text, row.Limit, row.Ellipsis, row.Origin); got != row.Head {
			t.Errorf("%s: HeadRefs(%.80q, %d, %q, %q) = %.200q, want %.200q",
				row.Name, row.Text, row.Limit, row.Ellipsis, row.Origin, got, row.Head)
		}
	}
}

// TestHeadRefsCostsTheSameOnABodyOfAnyLength holds a cut to the window it reads: 50 cuts of a
// single line of 1 MiB - one of inline links side by side, each opening with a `[`, one of link
// tails and one of keys - take well under a second, where reading the whole line took seconds
// for each cut. Only a window holding a `[` is read through goldmark's link parser (linkRanges),
// which is the costly read, so the line of links is the one that times it, and it is timed
// first; its first cut shows the parser read the window, since a cut inside a link moves back to
// the link's `[` only where the link was read, and the address alone would leave `[x](` before
// the ellipsis. It stops at the first cut past the bound, so a cut that reads the whole line
// fails it in that cut's time rather than in all fifty's.
func TestHeadRefsCostsTheSameOnABodyOfAnyLength(t *testing.T) {
	keys := strings.Repeat("CORE-1 ", (1<<20)/len("CORE-1 "))
	const link = "[x](dispatch://CORE-1)"
	links := strings.Repeat(link, (1<<20)/len(link))
	if got, want := HeadRefs(links, 160, "…", ""), strings.Repeat(link, 7)+"…"; got != want {
		t.Fatalf("the first cut of a line of links = %q, want %q: the cut did not read the links", got, want)
	}
	for _, body := range []struct{ name, text string }{{"links", links}, {"link tails", linkTails}, {"keys", keys}} {
		began := time.Now()
		for cut := range 50 {
			HeadRefs(body.text, 160+cut*7, []string{"…", ""}[cut%2], "https://dispatch.example")
			if took := time.Since(began); took > time.Second {
				t.Fatalf("a line of %d bytes of %s: %d cuts took %s, want 50 of them well under a second",
					len(body.text), body.name, cut+1, took)
			}
		}
	}
}
