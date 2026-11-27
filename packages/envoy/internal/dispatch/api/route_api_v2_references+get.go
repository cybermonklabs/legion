package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/references", Auth: authAny, Versions: onlyV2,
		Description: "Edges of one node in the reference graph: to=<dispatch ref> its backlinks or from=<dispatch ref> its " +
			"links, exactly one (400 INVALID_REFERENCE); kind=<kind>,<kind>... keeps those edge kinds, and a kind this build " +
			"does not emit is listed in unknown_kinds beside the page rather than refused. With no kind, an ask's backlinks " +
			"leave out its own replies and parts, and a document's, an ask's, a comment's or a message's leave out part_of. " +
			"Position: source_seq, the id of the event that introduced a mention. after=<source_seq> keeps the mentions above " +
			"it, structural edges carrying none, read up to the event log's safe horizon: every mention committed before the " +
			"read, unless an open transaction holds the horizon back past 1 s, when the read answers the mentions at or below " +
			"it and a later read the rest; a reader that starts from after=0 reads on from the highest source_seq it was " +
			"shown. before=<source_seq> keeps the mentions below it. sort=-created_at (the default, newest first), " +
			"created_at, source_seq or -source_seq, and with no sort an after reads up source_seq and a before alone down it. " +
			"limit 1 to 200 (default 50) and offset; {node, unknown_kinds, items, total, limit, offset}, total the exact count " +
			"of the edges the filters keep. 404 NOT_FOUND for a node nothing holds, 409 ID_AMBIGUOUS for a short id two items " +
			"carry; an after above the highest event id written, or an after or a before that is no integer of 0 or more, is " +
			"400 POSITION_INVALID naming the parameter and source_seq.",
		Handler:  s.listReferencesV2,
		Resource: "references", Kind: kindLog, Position: sourceSeqPosition,
		Query: referencesV2Query{},
		Shape: routeShape{Status: http.StatusOK, Response: referencesV2{}},
		Refusals: listRefusals(positionRefusal, referenceInvalidRefusal,
			apicode.Refusal{Condition: apicode.ReferenceNodeMissing, Code: apicode.NotFound},
			refPrefixAmbiguousRefusal),
	}
})
