package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues",
	Auth:        authAny,
	Description: fmt.Sprintf("Create an issue (native, or from a GitHub owner/repo#n ref); assignee defaults to the creating person, the personal token's owner, the parent's assignee when the parent is of the issue's own project, or the project's default assignee. Every create names its project, a create from a ref included: external with no project, or an empty one, is 400 INVALID_ISSUE. A title over %d characters is 400 CAP_EXCEEDED. advances is the priorities the new issue advances, a list of priority ids: more than %d of them, counted before duplicates are removed, an id no priority carries, and one whose priority is gone are each 400 ADVANCES_INPUT, and the create writes nothing; a create carries no advances_revision and is never refused for one, since the new issue holds no relation yet. With DISPATCH_PARENT_STATUS_RULE on, a parent that the new issue would leave out of line with its children (a done parent) is 409 PARENT_STATUS_CONFLICT {code, error, issues: [{key, title, status, relation}]}, issues naming each issue that holds the rule. When Legion runs the new issue, because it or an issue of its own project above it carries the label legion, a create under a parent by a session that is no person and no architect of the parent or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT. A create under a parent of another project is judged as a root of the new issue's own project, which this check does not judge. After the write commits, a related-item check searches the project by meaning and by its rarest words and waits at most 5 seconds; the response's advice.suggestions says whether it found related items (outcome found, none or not_made with the reason in missing), how it searched (search, search_note), the three items most like the new issue, one per owner, with each ask item's answer state, words to search with by hand (search_query), and under force the near copies forced past (title_matches), or the likely duplicate an unforced create was told (likely_duplicate).", contracts.IssueTitleMax, contracts.MaxIssueAdvances),
	Handler:     (*server).createIssue,
	// The issue create is the one write of a project's own objects whose project its address
	// does not name: createIssue reads it from the body and makes the check itself.
	WriteClass: scopeProjectInBody,
	Shape: routeShape{
		Status: http.StatusCreated, Response: model.Issue{}, Request: createIssueInput{}, Advice: writeAdvice{},
		// A ref whose external issue already has a Dispatch issue answers that issue, 200.
		Also: []routeAnswer{{Status: http.StatusOK, Body: model.Issue{}}},
		// A 409 is a possible duplicate, or, with DISPATCH_PARENT_STATUS_RULE on, a create under a
		// done parent (parent_status_rule.go).
		Refusals: []routeAnswer{
			{Status: http.StatusConflict, Body: issueDuplicateError{}},
			{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
		},
	},
})
