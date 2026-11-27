package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/priorities/{id}", Auth: authAny, Versions: onlyV2,
		Description: fmt.Sprintf("Write one priority's title, description or owner, and move its order {title?, "+
			"description?, owner?, rank?, expected?}, a bearer naming its session in actor: each field named is "+
			"written and each left out is left alone. rank is {before, after}, the priority this one lands above and "+
			"the one it lands below; a rank naming neither, a neighbour that is this priority, and one no live "+
			"priority carries are each 400 RANK_INPUT. A title with nothing in it or over %[1]d characters and a body "+
			"naming nothing to write are 400 PRIORITY_INPUT. expected is {title?, description?}, the value the "+
			"writer's edit of each started from, for the title and the description this same write writes; owner "+
			"and rank take none. An expected naming an attribute the write does not write is 400 PRIORITY_INPUT. One "+
			"that differs from the value the priority stores now (the title compared with the stored title exactly, "+
			"which is stored trimmed, so a writer sends the title it read; the description exactly) is 409 "+
			"PRIORITY_CHANGED, whose message names each attribute that differs and its value now, quoted and cut "+
			"past %[2]d characters with a trailing …; that write writes nothing and appends no priority.updated. The "+
			"compare and the write are one transaction holding the priority's row, so a write naming expected waits "+
			"for any other write of the priority in flight and compares against what that one wrote. A body with no "+
			"expected writes over whatever is stored. Answers the priority and appends priority.updated. A "+
			"credential scoped to one project is refused. 404 PRIORITY_NOT_FOUND for an id no priority carries, or "+
			"one whose priority is gone.", contracts.IssueTitleMax, priorityChangedQuoteMax),
		Handler:  s.patchPriorityV2,
		Resource: "priorities", Kind: kindItem, Key: "{id}",
		Body:     requestBody{Kind: bodyJSON, Type: patchPriorityInput{}},
		Shape:    routeShape{Status: http.StatusOK, Response: model.Priority{}},
		Scope:    scopeDeployment,
		IDs:      &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, priorityMissingRefusal, priorityInputRefusal, rankInvalidRefusal, priorityWrittenRefusal},
	}
})
