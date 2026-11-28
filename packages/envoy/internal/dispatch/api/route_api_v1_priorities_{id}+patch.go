package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/priorities/{id}",
	Auth:        authAny,
	Description: fmt.Sprintf("Write one priority's title, description or owner, and move its order {title?, description?, owner?, rank?, expected?}: each field named is written and each left out is left alone. rank is {before, after}, the priority this one lands above and the one it lands below, as the issue board's own drag names them; a rank naming neither, a neighbour that is this priority, and one no live priority carries (an id the store does not hold, or one whose state is gone) are each 400 RANK_INPUT. A title with nothing in it or over %[1]d characters (UTF-16 units, counted as an issue title is, so a rename is held to the bound a create is) and a body naming nothing to write are 400 PRIORITY_INPUT, and an id no priority carries is 404 PRIORITY_NOT_FOUND. expected is {title?, description?}, the value the writer's edit of each started from, for the title and the description this same write writes; owner and rank take none. An expected naming an attribute the write does not write is 400 PRIORITY_INPUT. One that differs from the value the priority stores now (the title compared with the stored title exactly, which is stored trimmed, so a writer sends the title it read; the description exactly) is 409 PRIORITY_CHANGED, whose message names each attribute that differs and its value now, quoted and cut past %[2]d characters with a trailing …; that write writes nothing and appends no priority.updated. The compare and the write are one transaction holding the priority's row, so a write naming expected waits for any other write of the priority in flight and compares against what that one wrote. A body with no expected writes over whatever is stored.", contracts.IssueTitleMax, priorityChangedQuoteMax),
	Handler:     (*server).patchPriority,
	Successor:   successorIs(http.MethodPatch, "/priorities/{id}"),
	WriteClass:  scopeDeployment,
	ID:          &idRoute{Reason: "a priority id, a slug of its own and no uuid at all"},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.Priority{}, Request: patchPriorityInput{},
	},
})
