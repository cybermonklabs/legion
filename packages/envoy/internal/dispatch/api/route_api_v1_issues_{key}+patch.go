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
	Description: fmt.Sprintf("Update title (over %d characters is 400 CAP_EXCEEDED), status, priority, labels (or labels_add, which adds to the labels the issue holds when the write commits rather than replacing them; not with labels), route, parent, rank, assignee (the email of a person who has signed in, or null; anyone may reassign), advances, or components ({mode: inherit|explicit|none, ids, reason}; allowed on a closed issue). advances is the issue's whole list of its own priorities, which replaces the list it holds ([] clears it; an inherited entry is no relation of the issue and no write states or retracts one): more than %d ids, counted before duplicates are removed, an id no priority carries, and one whose priority is gone are each 400 ADVANCES_INPUT. advances_revision is the advances_revision of the issue read the write answers: a write whose advances_revision is not the revision the issue holds is 409 ADVANCES_REVISION naming the one it holds, and a write that names no revision may assert or retract one relation, so one that would change two or more is 409 ADVANCES_REVISION too, naming the revision held. A closed issue refuses advances (409 ISSUE_CLOSED). Closing the issue (status done) withdraws every open ask it holds, each recorded resolved with resolution kind withdrawn naming the closer; reopening it opens exactly those asks again. expected_sequence makes the write conditional: 409 ISSUE_CHANGED when the issue's last event is no longer that sequence. With DISPATCH_PARENT_STATUS_RULE on, a status or a parent that would leave a parent out of line with its children is 409 PARENT_STATUS_CONFLICT {code, error, issues: [{key, title, status, relation}]}, relation parent or child, issues naming each issue that holds the rule. On an issue Legion runs, a close, a reopen and a labels or parent write that takes the issue out of Legion are 403 DOCUMENT_WRITER_NOT_ARCHITECT from a session that is no person, no architect of the issue or of an issue above it, and not the session of the project's Legion daemon. A move under a parent, after which Legion runs the issue, is 403 DOCUMENT_WRITER_NOT_ARCHITECT from a session that is no person and no architect of the new parent or of an issue above it. A move under a parent of another project is judged as a root of the issue's own project, which this check does not judge. When Legion runs the issue, or an issue under it, before the move, the move is 403 DOCUMENT_WRITER_NOT_ARCHITECT too, wherever the new parent is, from a session that is no person and no architect of each such issue or of an issue above it where it stands: the issue itself when Legion runs it, and else each issue under it with the label legion that it reaches through parents of its own project. An issue's chain ends at the first parent of another project, as at a root. Neither check of a move passes the session of the project's Legion daemon.", contracts.IssueTitleMax, contracts.MaxIssueAdvances),
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
