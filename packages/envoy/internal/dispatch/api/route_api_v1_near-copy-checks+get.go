package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/near-copy-checks",
	Auth:        authAny,
	Description: fmt.Sprintf("List one project's near-copy gate answers, one row for each create the gate answered (an external create runs no gate and has none), recorded when the create was refused or once it was written: the rows of ?project= (required; 400 INVALID_PROJECT when it is not a key) within [from, to) (RFC3339, each optional; 400 INVALID_QUERY when from is not before to), oldest first, at most ?limit= (1 to %d, default %d), after the row whose id ?after= names (400 INVALID_QUERY when it is no row of the project). A window should end some time before now: a row written in its create's transaction holds that transaction's start as created_at and is seen only once the transaction commits, so a reader that pages up to now and resumes with ?after= can miss it. {project, from, to} echo the query (null when absent), checks are the rows, and next_after is the id to pass as ?after= for the next page, null on the last. A row is {id, created_at, project, actor_kind, actor_id, outcome, reason, force, title, spec_sha256, parent, issue, candidates, refuse_score, nudge_score, build_commit, read_ms, read_stage}, with no spec: GET /api/v1/near-copy-checks/{id} reads one row with it. outcome is refused (the gate refused it and nothing was written; every 409 POSSIBLE_DUPLICATE has this row, written before the answer), forced (written past a refusal with force), nudged (written, its closest open issue named as a likely duplicate), passed (written with nothing named) or not_made (no check that could stop the create was made, and it went in unchecked); reason is bound, failed or unrecorded for not_made and null otherwise; force is whether the create asked for it; spec_sha256 is the sha256 of the spec as sent, on every row; issue is the key written, null for a refusal; candidates are the issues the gate read at or above nudge_score, at most %d, best first, each {key, title, score}; refuse_score and nudge_score are the build's bars and build_commit the commit it was built from, null for an unstamped build; read_ms is how long the create waited on the gate's read, in milliseconds, its rollback included, and read_stage the step the read was in when it answered or ended (connect, terms, keys, or score, which every answered read ends in), both null on a row written before they were recorded. A refusal is answered only once its row is known to be written: otherwise it refuses nothing, and the create goes in as not_made, reason unrecorded, writing its row in its own transaction. The row of every other create that went in is written after the commit, on a context of its own bounded to %s, and tried once more in the background for up to %s when a timeout or a lock stopped it, so one whose row still could not be written has none and is otherwise unchanged.", nearCopyChecksPageMax, nearCopyChecksPageMax, duplicateCandidateLimit, nearCopyRecordTimeout, relatedRecordRetryTimeout),
	Handler:     (*server).getNearCopyChecks,
	Read:        readEntry{Writes: readWritesNothing, AsOf: asOfNoHistory},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.NearCopyChecksPage{}, Params: nearCopyChecksQuery{},
	},
})
