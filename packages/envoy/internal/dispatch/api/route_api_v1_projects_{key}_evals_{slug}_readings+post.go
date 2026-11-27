package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/projects/{key}/evals/{slug}/readings",
	Auth:        authAny,
	Description: fmt.Sprintf("Log one batch of readings of one eval {readings, actor?}, each reading {series, row_kind, measured_at, value or value_absent_reason, row_key?, col?, unit?, n?, n_unit?, ci_low?, ci_high?, ci_level?, attributes?, links?, source?, note?, replaces?}: stored in one transaction or not at all, under an Idempotency-Key the write requires (400 IDEMPOTENCY_KEY_REQUIRED), a repeat under the key answered the first write's answer. A body that is no JSON object of readings and actor, one holding another key or a key of actor spelled otherwise, or one naming a key twice in any object of it (the body, actor, a reading, its attributes or its links), is refused 400 INVALID_JSON naming the key and where it stands (readings[0], readings[0].attributes). A slug no eval holds registers its eval with the caller as owner, no target and state active; a reading under a computed eval's slug is stored and answered by its items, never folded into its value. A correction is a new reading naming the one it replaces. The server sets recorded_by and recorded_at, and refuses a row carrying either. measured_at lies from 2025-01-01T00:00:00Z to 24 hours after the server's time; series, row_kind, row_key and attributes.repo hold at most 512 bytes each; a link's href is an absolute http or https URL. A logged eval holds at most %[1]d series and a day row of it at most 21,626 values, and a reading's fields together take at most what the parameters of one statement of the write hold, so that the parameters of every statement of the write take at most 256 KiB. A refused batch stores nothing and answers 400 {code, error, rows, rows_omitted?}: rows names the first %[2]d refused rows by index, each by its field with its own code (READING_INVALID, READING_VALUE_OR_REASON, READING_FIELD_SERVER_SET, READING_SERIES_INVALID, READING_REPLACES_UNKNOWN, READING_ALREADY_REPLACED, MEASURED_AT_OUT_OF_RANGE, EVAL_KEY_TOO_LONG, LINK_HREF_NOT_ABSOLUTE, LINK_HREF_NOT_HTTP, LINK_REL_MISSING, READING_DAY_OVERFLOW for a value that would take a sum of its day row, its values or each weighted by its n, past what a float holds, EVAL_SERIES_LIMIT for the one row whose new series would first make the eval hold more than %[1]d, READING_DAY_SAMPLES_LIMIT for a value that would make its day row hold more than 21,626 where it held fewer, EVAL_VERSION_TOO_LARGE naming unit for a row whose new series, its unit the row's, would take more than %[3]d bytes as JSON, and READING_TOO_LARGE for a reading whose fields take more than the parameters of one statement of the write hold, naming its largest field), and rows_omitted counts the refused rows past those, absent where rows names every one; each error holds at most 1,024 bytes and each field 128 as JSON encodes them, so the answer the Idempotency-Key keeps stays under 256 KiB whatever the body. A slug that is no slug is 400 EVAL_SLUG_INVALID, one over 512 bytes 400 EVAL_KEY_TOO_LONG, a batch of no reading 400 READINGS_EMPTY, a first reading whose writer (the body's actor) would make its eval's first version take more than %[3]d bytes as JSON 400 EVAL_VERSION_TOO_LARGE naming actor, an unknown project 404 PROJECT_NOT_FOUND, and a project whose evals-md slug names a file 409 EVALS_DOCUMENT_NOT_A_DOCUMENT. The write takes no query key but session, once: any other, and session named twice, is 400 INVALID_QUERY naming it. Answers 201 {items, total, limit, offset, definition, registered}: total counts every reading the write stored, and items lists the first of them in the batch's order, limit how many, as many as keep the answer within %[4]d bytes, so that the parameters of the statement that keeps it under the Idempotency-Key take at most 256 KiB, as the parameters of every statement of the write do, and a repeat under the key is answered this answer; the ids of the readings it leaves out are read from GET /api/v1/projects/{key}/evals/{slug}/items, each reading's cols.id. One reading may take more than that as JSON (a note of some hundred kilobytes), so items may list none while total counts them.", evals.MaxEvalSeries, evals.MaxRefusedRows, evals.MaxVersionBytes, evals.MaxAnswerBytes),
	Handler:     (*server).logEvalReadings,
	// The eval registry's readings write: a batch of readings of the eval of the project its path
	// names. The project check (withinProjectScope) lets a credential scoped to one project write
	// it in any project that is not private, and in a private project only where the credential
	// names it (access.CredentialCovers); a private project it does not name, and a key no project
	// holds, are refused 403 PROJECT_SCOPE with one message. A credential that names no project
	// passes the check, and a key no project holds is then 404 PROJECT_NOT_FOUND.
	WriteClass: scopeProject,
	Shape: routeShape{
		Status: http.StatusCreated, Response: evalReadingsResponse{}, Request: evalReadingsInput{},
		// A refused batch names every refused row by its index beside the code of the first; a
		// slug that is no slug, an empty batch and a first reading whose writer would pass
		// evals.MaxVersionBytes are 400 too, a project no row holds 404 and a project whose
		// evals-md slug names a file 409, each the registry's own body (writeEvalError); every
		// other refusal is errorBody's.
		Refusals: evalRefusals,
	},
})
