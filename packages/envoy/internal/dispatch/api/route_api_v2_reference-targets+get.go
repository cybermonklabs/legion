package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/reference-targets", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("What many dispatch:// references point at, in one read: ref=<dispatch ref> once per "+
			"reference, at most %d of them; more than that is 400 CAP_EXCEEDED naming the cap. One member per ref in "+
			"the request's order (a reference named twice answered twice), each {ref, kind, "+
			"name, gone}: ref as the request wrote it, kind the kind of node it names, name the words shown for the target "+
			"in place of its text (an issue's title, a document's name, an ask's question, a comment's or a message's body, "+
			"a component's title, a block's text), and gone whether nothing answers to that reference now, once an id it "+
			"writes short is resolved, which carries no name. A member naming a comment or a message, or a block of "+
			"one, also carries edited_at where the post's author edited it, and deleted_at and deleted_by where its "+
			"author deleted it: a deleted post's member is not gone and its name is \"\", and a member naming a block "+
			"of it is gone, with no name and no text_tree. A caller that sent Dispatch-Trees: 1 is also given text_tree "+
			"on each member whose kind is block: the block's whole text as the tree a reader is shown, links and references "+
			"kept and nothing cut, a typed block's being of what stands between its opening line and its closing one, read "+
			"by the block's kind as GET /api/v1/reference-targets reads it. No ref is 400 INVALID_REFERENCE, as is a "+
			"reference the grammar cannot read, naming it; one that reads but names nothing is answered gone, never "+
			"refused. An item id or a "+
			"block id written short (8 characters or more, never a whole uuid) is resolved in the scope the reference "+
			"names, as GET /api/v2/references resolves its to=, and ref still echoes it as written: a prefix that nothing "+
			"carries is answered gone, and a prefix that two or more items carry answers that member gone with ambiguous: "+
			"true, which no other member carries, and every other member is answered; this read refuses no prefix. limit 1 "+
			"to %d (default %d, so one read of at most the cap is one page) and offset page the members; {items, total, "+
			"limit, offset}, total the count of refs named. The answer carries an ETag over the page, each tree and each "+
			"ambiguous included; If-None-Match answers 304. This answers no edges: GET /api/v2/references reads one "+
			"node's edges.", referenceTargetsMax, referenceTargetsMax, referenceTargetsMax),
		Handler:  s.listReferenceTargetsV2,
		Resource: "reference-targets", Kind: kindCollection,
		Query: referenceTargetsV2Query{},
		List:  listBounds{Default: referenceTargetsMax, Max: referenceTargetsMax},
		Shape: routeShape{
			Status: http.StatusOK, Response: page[model.ReferenceTarget]{},
			Also: []routeAnswer{{Status: http.StatusNotModified}},
		},
		Refusals: listRefusals(referenceInvalidRefusal, valuePastBoundRefusal),
	}
})
