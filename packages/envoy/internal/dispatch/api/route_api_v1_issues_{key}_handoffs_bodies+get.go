package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/handoffs/bodies",
	Auth:        authAny,
	Description: fmt.Sprintf("The bodies of the issue's handoffs ?ids= names, comma-separated, 1 to %[1]d whole uuids, in one answer: {bodies: [{id, body}], left: [id]}. bodies holds one entry for each id answered, in the order asked, body the exact text written, or null for an id that is no handoff of this issue, another issue's handoff included. The answer adds each entry's size as sent to a count - its body as a JSON string, each <, > and & written as six bytes, and its field_trees where it carries them - and stops before the first entry that would take the count past %[2]d bytes: that id and each id after it are left, in the order asked, for a later request. The first id asked is always answered, whatever its size. With Dispatch-Trees: 1 each entry also carries field_trees, the tree of each text its card draws, as GET /api/v1/handoffs/{id} carries them, counted beside its body; a first entry whose body and trees together pass the limit is sent with its body and no trees. Refusals, in this order: ids absent, empty or given twice, an empty id, or an id asked twice, 400 INVALID_QUERY; more than %[1]d ids, 400 INVALID_QUERY; an id that is not a uuid, 400 HANDOFF_ID_INPUT; a key no issue has, 404 NOT_FOUND, as GET /api/v1/issues/{key} answers it. A closed issue's read too. The route writes nothing, and reads the issue and the bodies in one read-only transaction.", contracts.HandoffBodiesMaxIDs, contracts.HandoffBodiesMaxBytes),
	Handler:     (*server).getHandoffBodies,
	Read:        readEntry{Writes: readWritesNothing},
	// The bodies of several handoffs, each entry carrying field_trees as handoffRead does.
	Shape: routeShape{
		Status: http.StatusOK, Response: handoffBodies{}, Params: handoffBodiesQuery{},
	},
})
