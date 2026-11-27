package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/projects/{key}/related",
	Auth:        authAny,
	Description: fmt.Sprintf("Run the related-item check an issue create runs, over ?title= and ?text= (checked as the title, a newline and the text; either alone is enough), without writing anything: no issue, ask, suggestion or related_checks record. ?exclude_issue= leaves that issue of the project and everything it holds out of the results and out of the word statistics (400 EXCLUDE_ISSUE_INPUT when it is not an issue of the project); ?exclude_issues=, repeated, leaves each of up to %d issues of the project out of the results alone, so a draft can leave out a whole subtree without changing how rare its words are (400 EXCLUDE_ISSUE_INPUT for a longer list or for a key the project does not hold); ?keywords_only=true skips meaning search. It waits at most 5 seconds and answers the create's advice.suggestions shape: outcome found, none or not_made (a blank text is 200 not_made, the reason in missing), search, search_note, search_query and related, with up to 3 open issues before any item of another kind and everything a done issue owns last. This form answers no other_projects: its caller is the dashboard's new-issue dialog, which runs the check for each burst of typing and draws the items of the asking project alone. Title and text over %d characters together are 400 CAP_EXCEEDED; POST takes a longer text. An unknown project is 404 PROJECT_NOT_FOUND.", relatedExcludeLimit, contracts.SearchQueryMax),
	Handler:     (*server).getRelated,
	Weight:      concurrentReadWeight,
	// The reads whose handler makes the server write on its own context, or whose class a first
	// reading of the handler would get wrong.
	Read: readEntry{Writes: readWritesNothing, WriteNote: "When the project has no search_term_totals row, the related-item check starts a refresh of the project's term statistics on the server's own context (ensureSearchTermStats, once per project and process): it inserts and updates search_term_totals, deletes and inserts search_term_stats, and deletes the project's search_term_queue rows. The statistics are a cache of what the project holds, which every related check reads and the first to find them missing rebuilds; nothing else is written."},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Suggestions{}, Params: relatedQuery{},
	},
})
