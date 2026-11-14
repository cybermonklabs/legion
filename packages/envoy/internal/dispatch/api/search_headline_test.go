package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// searchRowShape is one row of the column shape searchQuery and searchQueryMeaning share, in the
// order scanSearchRows reads it (search.go, the select of searchPageAndTailSQL): the totals, the
// hit's kind and owner, its artifact, its ids, its score, and last its headline and the text the
// headline was cut from. The values are a document hit of an issue, which is the shape a document
// whose markdown is missing arrives in; $1 is that row's headline and $2 its text, so one call
// gives the scan a row with a headline and another gives it a row with none.
const searchRowShape = `select 1::int, 1::int, 'document'::text, 'issue'::text,
	'SRCH-1'::text, 'Navigation instruments'::text, 'open'::text,
	null::text, null::text, null::text, null::text,
	'spec'::text, 'spec.md'::text, true,
	'0f3b7c1e-8f0a-4a2b-9c6d-2b1f5a7e4c10'::text, null::text, null::text, 0.42::float8, $1::text, $2::text`

// TestSearchScanTakesARowWithANullHeadline: scanSearchRows dereferenced the headline pointer, so
// one row whose headline is null took the process down for every search on it - a nil-pointer
// panic inside the scan, which no caller recovers (dispatch://INFRA-988). ts_headline answers
// null when the text it is given is null, and the document arm of the search's text join reads
// the latest artifact_versions row alone, so a document whose latest version carries no markdown
// over an earlier one that holds the markdown the keyword leg matched is such a row. The write
// path refuses that shape - a blob version under a document artifact answers
// ARTIFACT_KIND_MISMATCH - so the row is reachable by raw SQL rather than through the API, and
// the scan holds against it anyway: a snippet of nothing is an empty snippet, and the row beside
// it still carries its marks. The display-tree reading of the same headline is asked for here
// too (scanSearchRows' trees), since it reads the headline a second time and a null one is the
// snippet tree of no text.
func TestSearchScanTakesARowWithANullHeadline(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	rows, err := database.Pool.Query(ctx, searchRowShape, nil, nil)
	if err != nil {
		t.Fatalf("query the null-headline row: %v", err)
	}
	defer rows.Close()
	response, err := scanSearchRows(rows, "astrolabe", 20, 0, true)
	if err != nil {
		t.Fatalf("scan the null-headline row: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("scanned %d results, want the one row: %+v", len(response.Results), response.Results)
	}
	if got := response.Results[0].Snippet; got != "" {
		t.Errorf("the null headline became the snippet %q, want none", got)
	}
	if got := response.Results[0].SnippetTree; len(got) != 0 {
		t.Errorf("the null headline became the snippet tree %s, want none", got)
	}
	if got := response.Results[0].Href; got == "" {
		t.Errorf("the row with no headline lost its href: %+v", response.Results[0])
	}

	marked, err := database.Pool.Query(ctx, searchRowShape, markStart+"astrolabe"+markEnd+" & sextant", "astrolabe & sextant")
	if err != nil {
		t.Fatalf("query the headline row: %v", err)
	}
	defer marked.Close()
	response, err = scanSearchRows(marked, "astrolabe", 20, 0, false)
	if err != nil {
		t.Fatalf("scan the headline row: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("scanned %d results, want the one row: %+v", len(response.Results), response.Results)
	}
	if got, want := response.Results[0].Snippet, "<mark>astrolabe</mark> &amp; sextant"; got != want {
		t.Errorf("the headline became the snippet %q, want %q", got, want)
	}
}

// TestSearchScanReadsTheSnippetOutOfTheTextTheHeadlineWasCutFrom: ts_headline drops the
// punctuation it does not count as words, so for a search of CORE-1 it answers "CORE-1` locally
// please" for the comment "Run `CORE-1` locally please", and the snippet read from that headline
// alone showed the key as prose, and as a reference, where the comment holds it in code
// (INFRA-1702). The scan reads the snippet out of the text the headline was cut from, which the
// row carries beside it, so both the HTML snippet and its tree keep the key in its code span.
func TestSearchScanReadsTheSnippetOutOfTheTextTheHeadlineWasCutFrom(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	const source = "Run `CORE-1` locally please"
	var headline string
	if err := database.Pool.QueryRow(ctx,
		`select ts_headline('english', search_text($1), websearch_to_tsquery('english', $2), $3)`,
		source, "CORE-1", headlineOptions).Scan(&headline); err != nil {
		t.Fatalf("cut the headline: %v", err)
	}
	rows, err := database.Pool.Query(ctx, searchRowShape, headline, source)
	if err != nil {
		t.Fatalf("query the headline row: %v", err)
	}
	defer rows.Close()
	response, err := scanSearchRows(rows, "CORE-1", 20, 0, true)
	if err != nil {
		t.Fatalf("scan the headline row: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("scanned %d results, want the one row: %+v", len(response.Results), response.Results)
	}
	result := response.Results[0]
	if want := "Run `<mark>CORE</mark><mark>-1</mark>` locally please"; result.Snippet != want {
		t.Errorf("the headline %q became the snippet %q, want %q", headline, result.Snippet, want)
	}
	if !strings.Contains(string(result.SnippetTree), `"inlineCode"`) {
		t.Errorf("the snippet tree holds no code span: %s", result.SnippetTree)
	}
}
