package api

import (
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/going-runs",
	Auth:        authAny,
	Description: fmt.Sprintf("History: the caller's own command-bar conversations, and nobody else's, each listed until its newest run is seven days old, the one whose newest run started last first: each row is {id (its first run's id), newest_at, runs}, its runs newest first, each as GET /api/v1/going/runs/{id} answers it, with undoable, whether Undo can still take back a change of it, undone_at, when its Undo landed (null for a run never undone), and undoable_count, undone_count and standing_count, how many of its changes Undo can still take back, took back, and tried and could not take back. A withdrawn run, one given the words of a comment or an issue message deleted since, reads stopped with its stop_reason starting %s, or %s where its own deletes alone withdrew it, as GET /api/v1/going/runs/{id} reads it, its words kept. A short answer of a pause in typing is no run and is in none. ?issue= keeps the conversations with a run whose landed requests touched that issue and ?q= those with a run whose words hold that text (any case); pages with limit (1-200, default 50) and offset; {rows, total, limit, offset}.", contracts.GoingErrorAnswerWithdrawn, contracts.GoingErrorAnswerWithdrawnByRun),
	Handler:     (*server).listMyGoingRuns,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: goingConversationsPage{}, Params: myGoingRunsQuery{}},
})
