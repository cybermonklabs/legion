package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/agents/{session_id}/answers",
	Auth:        authAny,
	Description: "What people wrote that Dispatch delivered to one session, newest first by at, then by id: each ask's current answer (replaced when it changed an earlier answer), each comment at its current body (edited_at when edited; a resolved comment stays), each message at its current body (edited_at when edited), a deleted comment or message in its place with deleted_at and deleted_by and an empty text, and each ask a person declined to answer, once however it arrived. It reads the ask.answered, comment.created, message.created, message.answered and ask.resolved events a person wrote whose published_destinations hold the session's own topic, the session's message deliveries the listener took (targeted and broadcast messages, handed to the running session or queued for it) and its comment deliveries (mentions) in those same two states; of an ask's resolutions it lists the decline alone, and it never lists an agent's or the system's words, a retraction, a withdrawal, another resolution, a hand-back, or a delivery to a role: route's topic. Each item names its relation to the session. A session owns an ask when it wrote the ask or follows it, and only an owner is told that an ask is its own: for a comment, replied_on_your_ask (a reply in the thread of an ask the session owns) over mentioned_you over replied_in_a_comment_thread over commented_on_a_document over commented_on_an_issue; for a message, replied_to_your_message over sent_you_a_message; for an answer, answered_your_ask for an owner and answered_an_ask for any other session; for a decline, declined_your_ask for an owner and declined_an_ask for any other session. A session that does not own the ask reads a reply in its thread by the reason that delivered it: the issue's route, its claim, a credit on its primary document, or a mention. Each item carries the ask and its whole question on an answer, on a decline and on a reply in an ask's thread, whoever asked it, and its owner, dispatch:// ref and href (null owner and ref, and /agents/<session_id>/live, for a direct message). A decline carries a kind of its own, declined, at the resolution's own time, its id the ask's, and its words in two parts: text, the person's own note, empty when they wrote none, and, on declined_your_ask alone, dispatch_words, the sentences Dispatch wrote for the asking agent; nobody chose an option, so it carries no selected. ?since= (RFC3339, default 72 hours ago; older than 7 days or in the future is 400 INVALID_QUERY) starts the window; ?max_chars= (1-100000, else 400 INVALID_QUERY) stops the list after the item whose words take the running sum past it, counting the UTF-16 units of text and, on a decline that carries them, of dispatch_words beside it, while total counts every item in the window; {session_id, since, total, items}. A session_id that is not a session id is 400 INVALID_SESSION_ID, as on the agent artifact routes. Not an authorization boundary: direct-message text already reaches every authenticated caller through GET /api/v1/events, and a person may read any session's list.",
	Handler:     (*server).listSessionAnswers,
	Successor:   successorIs(http.MethodGet, "/agents/{session_id}/answers"),
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: model.SessionAnswersResponse{}, Params: sessionAnswersQuery{},
	},
})
