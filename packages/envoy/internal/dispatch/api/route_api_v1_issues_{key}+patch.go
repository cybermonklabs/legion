package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/issues/{key}",
	Auth:        authAny,
	Description: fmt.Sprintf("Update title (over %d characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the issue holds when the write commits rather than replacing them; not with labels), route, parent, rank, assignee (the email of a person who has signed in, or null; anyone may reassign), advances, or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue). advances is the issue's whole list of its own priorities, which replaces the list it holds ([] clears it; an inherited entry is no relation of the issue and no write states or retracts one): more than %d ids, counted before duplicates are removed, an id no priority carries, and one whose priority is gone are each 400 ADVANCES_INPUT. advances_revision is the advances_revision of the issue read the write answers: a write whose advances_revision is not the revision the issue holds is 409 ADVANCES_REVISION naming the one it holds, and a write that names no revision may assert or retract one relation, so one that would change two or more is 409 ADVANCES_REVISION too, naming the revision held. A closed issue refuses advances (409 ISSUE_CLOSED). Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution kind withdrawn naming the closer; reopening it opens exactly those asks again. expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer that sequence. With DISPATCH_PARENT_STATUS_RULE on, a status or a parent that would leave a parent out of line with its children is 409 PARENT_STATUS_CONFLICT {code, error, issues: [{key, title, status, relation}]}, relation parent or child, issues naming each issue that holds the rule.", contracts.IssueTitleMax, contracts.MaxIssueAdvances),
	Handler:     (*server).patchIssue,
	WriteClass:  scopeProject,
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Issue{}, Request: updateIssueInput{}, Advice: writeAdvice{},
		Refusals: []routeAnswer{
			// An acceptance write that asks for the open-child check (refuse_open_foreign_children)
			// over an open child of another project (refuseOpenForeignChildren).
			{Status: http.StatusConflict, Body: foreignChildOpenError{}},
			// With DISPATCH_PARENT_STATUS_RULE on, a status or a parent that breaks the rule.
			{Status: http.StatusConflict, Body: parentStatusConflictBody{}},
		},
	},
})
