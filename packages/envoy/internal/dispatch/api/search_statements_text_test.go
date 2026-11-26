package api

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// searchStatementsAtBase holds the statements base 5d3e9b84 composed, by the variable that names
// each: the text main runs since its visibility predicate (#3178) entered every search statement,
// #3512 dated a comment and a message by their edit (coalesce(edited_at, created_at)) and #3125
// cut each headline from the text window it selects beside it (src.text).
const searchStatementsAtBase = "testdata/search-statements-5d3e9b84.json"

// A search that binds no `to` - every /api/v1 search, of one project or of every project, and a
// /api/v2 search naming no to - and every related-item check run the statements whose plans were
// measured (searchProjectScope, searchMeaningCommentArmJoined): byte for byte the text base
// 5d3e9b84 composed, which testdata holds as that base's own code wrote it. The date filter's end
// and the opener member that binds it are in the statements a `to` binds alone (searchWindow).
func TestTheStatementsOfASearchWithNoToAreTheTextOfItsBase(t *testing.T) {
	data, err := os.ReadFile(searchStatementsAtBase)
	if err != nil {
		t.Fatalf("read the statements of 5d3e9b84: %v", err)
	}
	var atBase map[string]string
	if err := json.Unmarshal(data, &atBase); err != nil {
		t.Fatalf("decode the statements of 5d3e9b84: %v", err)
	}
	for name, statement := range map[string]string{
		"searchQuery":                        searchQuery,
		"searchQueryMeaning":                 searchQueryMeaning,
		"searchQueryMeaningScoped":           searchQueryMeaningScoped,
		"searchQueryMeaningJoined":           searchQueryMeaningJoined,
		"relatedKeywordQuery":                relatedKeywordQuery,
		"relatedKeywordExcludingQuery":       relatedKeywordExcludingQuery,
		"relatedMeaningQuery":                relatedMeaningQuery,
		"relatedMeaningScopedQuery":          relatedMeaningScopedQuery,
		"relatedMeaningJoinedQuery":          relatedMeaningJoinedQuery,
		"relatedMeaningExcludingQuery":       relatedMeaningExcludingQuery,
		"relatedMeaningScopedExcludingQuery": relatedMeaningScopedExcludingQuery,
		"relatedMeaningJoinedExcludingQuery": relatedMeaningJoinedExcludingQuery,
	} {
		base, held := atBase[name]
		if !held {
			t.Fatalf("%s holds no text of %s", searchStatementsAtBase, name)
		}
		if statement != base {
			t.Errorf("%s is not the text 5d3e9b84 ran (%s)", name, firstDifference(statement, base))
		}
	}
}

// firstDifference names where got first parts from want: the byte offset and the text of each
// from there.
func firstDifference(got, want string) string {
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	excerpt := func(text string) string {
		return text[at:min(len(text), at+80)]
	}
	return fmt.Sprintf("from byte %d: %q where 5d3e9b84 has %q", at, excerpt(got), excerpt(want))
}
