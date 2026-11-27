package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/block-links", Auth: authAny, Versions: onlyV2,
		Description: "Links from blocks of one document to blocks of another: from=<document ref> and to=<document " +
			"ref> (dispatch://KEY/spec, dispatch://KEY/artifact/<slug> or dispatch://PROJECT/artifact/<slug>; one " +
			"document may be both), each link with both block nodes, its edge kind, and when and in which version of " +
			"its document the source block first held it, newest first; a link counts only while both its blocks are " +
			"current blocks of their documents. {from, to, items, total, limit, offset}, from and to the two document " +
			"nodes; limit 1 to 200 (default 50) and offset. A from or a to that names no document reference is 400 " +
			"INVALID_REFERENCE, and one naming a document that does not exist 404 ARTIFACT_NOT_FOUND.",
		Handler:  s.getBlockLinksV2,
		Resource: "block-links", Kind: kindCollection,
		Query:    blockLinksV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: blockLinksV2{}},
		Refusals: listRefusals(documentMissingRefusal, referenceInvalidRefusal),
	}
})
