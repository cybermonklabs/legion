package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// searchRowShape is one row of the column shape searchQuery and searchQueryMeaning share, in the
// order scanSearchRows reads it (search.go, the select of searchPageAndTailSQL): the totals, the
// hit's kind and owner, its artifact, its ids, its score and its headline last. The values are a
// document hit of an issue, which is the shape a document whose markdown is missing arrives in;
// $1 is that row's headline, so one call gives the scan a row with a headline and another gives
// it a row with none.
const searchRowShape = `select 1::int, 1::int, 'document'::text, 'issue'::text,
	'SRCH-1'::text, 'Navigation instruments'::text, 'open'::text,
	null::text, null::text, null::text, null::text,
	'spec'::text, 'spec.md'::text, true,
	'0f3b7c1e-8f0a-4a2b-9c6d-2b1f5a7e4c10'::text, null::text, null::text, 0.42::float8, $1::text`

// TestSearchScanTakesARowWithANullHeadline: scanSearchRows dereferenced the headline pointer, so
// one row whose headline is null took the process down for every search on it - a nil-pointer
// panic inside the scan, which no caller recovers (dispatch://INFRA-988). ts_headline answers
// null when the text it is given is null, and the document arm of the search's text join reads
// the latest artifact_versions row alone, so a document whose latest version carries no markdown
// over an earlier one that holds the markdown the keyword leg matched is such a row. The write
// path refuses that shape - a blob version under a document artifact answers
// ARTIFACT_KIND_MISMATCH - so the row is reachable by raw SQL rather than through the API, and
// the scan holds against it anyway: a snippet of nothing is an empty snippet, and the row beside
// it still carries its marks.
func TestSearchScanTakesARowWithANullHeadline(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()

	rows, err := database.Pool.Query(ctx, searchRowShape, nil)
	if err != nil {
		t.Fatalf("query the null-headline row: %v", err)
	}
	defer rows.Close()
	response, err := scanSearchRows(rows, "astrolabe", 20, 0)
	if err != nil {
		t.Fatalf("scan the null-headline row: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("scanned %d results, want the one row: %+v", len(response.Results), response.Results)
	}
	if got := response.Results[0].Snippet; got != "" {
		t.Errorf("the null headline became the snippet %q, want none", got)
	}
	if got := response.Results[0].Href; got == "" {
		t.Errorf("the row with no headline lost its href: %+v", response.Results[0])
	}

	marked, err := database.Pool.Query(ctx, searchRowShape, markStart+"astrolabe"+markEnd+" & sextant")
	if err != nil {
		t.Fatalf("query the headline row: %v", err)
	}
	defer marked.Close()
	response, err = scanSearchRows(marked, "astrolabe", 20, 0)
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
