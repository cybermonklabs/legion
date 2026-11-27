package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/evals",
	Auth:        authAny,
	Description: fmt.Sprintf("Register an eval or change its definition {slug, actor?, name?, question?, number_rule?, decision?, doc_block?, doc_text?, failure_modes?, unit?, direction?, target?, target_inclusive?, target_judges?, owner_role?, person?, issue_key?, kind?, source?, state?, replaced_by?, links?, series?}: the next version, written in one transaction with every field the body leaves out copied from the newest version (target: null turns the target off); a series named in series is registered or changed the same way, each series named once (one named twice is refused EVAL_SERIES_INVALID naming the second), and a version that lists or unlists a logged eval's series lists or unlists its stored day rows with it. doc_text replaces the eval's method in its section of the project's evals-md document (not with doc_block) unless that section holds it already; doc_block names a block of that document, and one the document does not hold is refused EVAL_FIELD_INVALID. Each link {rel, href, label}, its href an absolute http or https URL, is stored with the server's recorded_at, and its far side is never fetched. A slug and a series' name hold at most 512 bytes. An eval holds at most %[1]d series, listed or not, so that each of its stored rows is written in one statement of at most 256 KiB: a write that names more, or that would leave a logged eval holding more, is refused EVAL_SERIES_LIMIT naming the first series past the bound. A version of the definition, or of one series, takes at most %[2]d bytes as JSON once the fields the body leaves out are copied forward: a write that would store a larger one is refused EVAL_VERSION_TOO_LARGE naming the version's largest field as the body spells it (question, series[3].links, actor for the writer the body names), so no run of writes each within the bound leaves a version past it. source is logged: only the seeded catalogue makes an eval computed, and a write naming another source is refused EVAL_FIELD_INVALID. A series' min_n is the fewest timed results a job needs before pr-ci ranks it among its slowest jobs, so it is taken on pr-ci's job series (job:*) alone and refused EVAL_FIELD_INVALID on any other series, which no read ranks by one. On a computed eval the write sets only how the number is judged and who answers for it: target, target_inclusive, target_judges, the min_n of pr-ci's job series, owner_role, person, issue_key, links, state, replaced_by and its method (doc_text or doc_block); its code defines the rest, and a write naming any other field, or a series the eval does not hold, is refused EVAL_FIELD_COMPUTED. Answers 201 {items, total, limit, offset} where it wrote a version of the definition or of a series, and 200 where the body changed nothing, its one item {definition, series, series_total, wrote} the versions that stand: series lists the eval's series by name, as many as keep the answer within %[3]d bytes, so that the statement that keeps it under the Idempotency-Key takes at most 256 KiB, as every statement of the write does, and a repeat under the key is answered this answer, and at least one of an eval that holds any; series_total counts every series the eval holds. 400 {code, error, field} with a stable code and the one field it refuses (EVAL_SLUG_INVALID, EVAL_KEY_TOO_LONG, EVAL_FIELD_INVALID, EVAL_FIELD_COMPUTED, EVAL_SERIES_INVALID, EVAL_SERIES_LIMIT, EVAL_VERSION_TOO_LARGE, EVAL_DOC_TEXT_EMPTY, LINK_HREF_NOT_ABSOLUTE, LINK_HREF_NOT_HTTP, LINK_REL_MISSING), its error at most 1,024 bytes and its field 128 as JSON encodes them, a value of the body it names cut to fit, so the answer the Idempotency-Key keeps stays small whatever the body; 404 PROJECT_NOT_FOUND, 409 EVALS_DOCUMENT_NOT_A_DOCUMENT where the project's evals-md slug names a file.", evals.MaxEvalSeries, evals.MaxVersionBytes, evals.MaxAnswerBytes),
	Handler:     (*server).registerEval,
	// The eval registry's definition write: an eval of the project its path names, so a
	// credential scoped to another project is refused it.
	WriteClass: scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: evalRegisterResponse{}, Request: evalRegisterInput{},
		// A body that changes no field of the definition or of a series answers the versions that
		// stand, 200.
		Also: []routeAnswer{{Status: http.StatusOK, Body: evalRegisterResponse{}}},
		// The registry's refusals (writeEvalError): a field the tables would refuse, or a version
		// past evals.MaxVersionBytes, 400; a project no row holds, 404 PROJECT_NOT_FOUND; a
		// project whose evals-md slug names a file, 409 EVALS_DOCUMENT_NOT_A_DOCUMENT.
		Refusals: evalRefusals,
	},
})
