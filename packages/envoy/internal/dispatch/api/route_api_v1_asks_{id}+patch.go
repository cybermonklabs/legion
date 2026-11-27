package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPatch,
	Pattern:     "/api/v1/asks/{id}",
	Auth:        authAny,
	Description: "Reword an open ask (its author, or a credential holding the person scope); a block ask's `:::ask` block is rewritten with it. The command bar's undo makes a write back conditional by an in-process condition on the request's context, never a body member: each part it writes (question, options, multiple, urgency) must hold, when the edit takes the ask's lock, the value the turn left, an option as {label, description?}, else 409 ASK_CHANGED naming what each other part holds now, nothing written; a write back carrying no such condition is 500 GOING_CONDITION_MISSING, nothing written. The answer is the ask as the edit left it, with previous beside it: each part the edit names as the ask held it when the edit took its lock, spelled as the edit writes it. On an issue Legion runs, a reword of a block ask in its spec or Verification document by a session that is no person and no architect of the issue or of an issue above it is 403 DOCUMENT_WRITER_NOT_ARCHITECT.",
	Handler:     (*server).editAsk,
	WriteClass:  scopeProject,
	ID:          &idRoute{Kind: "ask", NotFound: "NOT_FOUND"},
	Shape:       routeShape{Status: http.StatusOK, Response: askEdited{}, Request: editAskInput{}},
})
