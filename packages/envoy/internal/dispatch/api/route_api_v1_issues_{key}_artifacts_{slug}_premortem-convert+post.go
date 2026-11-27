package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/issues/{key}/artifacts/{slug}/premortem-convert",
	Auth:        authAny,
	Description: "Convert the issue's pre-mortem document {slug}, by slug, into nodes, once {prevented_by, dry_run?, actor?}, in one transaction: first a moved_to tuple from the document's address to the lens about to be written, and the document retired: it leaves the issue's document list, takes no new text, its address answers the lens on GET /api/v1/references?to=, and each prevented_by at a check of the document is retracted, since nobody runs a retired document's checks; then one lens of kind pre-mortem under the issue, whose verdict is the document's verdict followed by its two closing sections, What a world-class implementation looks like and The smallest first slice that proves the model, word for word; under it one risk for risk 0 and each numbered risk of the document's newest version, each with its heading, early warning and prevention; and a prevented_by tuple to each check its entry names. prevented_by holds one entry {risk, checks, reason} for each risk the document holds: its number as the document numbers it, the dispatch:// addresses of the check blocks that prevent it, of any issue of the issue's project, empty for none, and why. An entry for a number the document does not hold, a number twice, a risk with no entry, an address that names no check block that is not gone and that the caller may see, a blank reason or check, and an entry of any other shape or type are 400 ARTIFACT_INPUT, naming the entry by its position from 1 and its risk number, or the risk no entry names; an entry naming a check of another project, of a retired document or of the document {slug} itself, which the conversion retires, is 400 INVALID_POINTER, naming the entry, its risk and the check, in a dry run as in a real one, and the document stays unretired; a body that is not JSON is 400 INVALID_JSON; no document {slug} of the issue is 404 ARTIFACT_NOT_FOUND, and a document that holds no verdict, risk or closing section, or that something other than a conversion retired, 400 ARTIFACT_INPUT. Each writes nothing. With dry_run every write runs in a savepoint that is rolled back, and the answer is what the call would write, with no address. A document converted already answers 200, writes nothing and names its lens. The conversion appends one event, the document's document.retired, whose reason names the lens. Answers 200 {document {address, id, version, moved_to}, dry_run, already_converted, lens {address, kind, on, verdict}, risks [{number, address, heading, early_warning, prevention, prevented_by, reason}], summary}.",
	Handler:     (*server).convertPremortemRoute,
	// The pre-mortem conversion: the lens and the risks it writes under the issue, and the
	// document of the issue it retires, belong to the issue's project.
	WriteClass: scopeProject,
	// The pre-mortem conversion (premortem_convert.go): the map from each risk to its checks, read
	// entry by entry, and the report the command prints too.
	Shape: routeShape{
		Status: http.StatusOK, Response: PremortemReport{}, Request: premortemConvertInput{},
	},
})
