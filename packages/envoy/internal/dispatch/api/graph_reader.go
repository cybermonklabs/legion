package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// actingReader is the credential a read of the graph answers for: the person at the browser,
// the session a broker-issued token proved, or the person a personal token belongs to, as the
// record stores an actor (model.Actor.Recorded). Every read that reaches the node and tuple
// store takes one, and a node reserved for its author answers to that author alone - to a
// reader who is anybody else, such a node is one the graph does not hold (refs' visibility
// registry).
//
// A request whose credential does not resolve is the caller's error, answered as every gate on
// this server answers one; a route that admits an unauthenticated caller gets the zero actor,
// which proves no author (graphstore.ProvesAuthor) and so reads no author-private node.
func (s *server) actingReader(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	return actor.Recorded(), true
}

// everyReader is who a read of the graph answers for when what it reads is kept or sent where
// many readers read it rather than answered to the one person whose request made it. It proves
// no author, so a node reserved for its author is never in what everyone reads, whoever's
// request wrote it. Each path that reads as it does so on purpose, because no single acting
// person is the reader there:
//
//   - an event's payload (commentEventPayload, and messageCorrectionEvent for a message's edit
//     and delete) is appended once and read by every subscriber of the event's topics and every
//     reader of the issue's event log;
//   - a delivery's frame (finishPendingMessageDelivery) is sent to the session it targets and
//     read again by every reader of the conversation, not by the person who wrote the message;
//   - the record of a comment thread a document projects (ProjectMark, through
//     projectThreadMark from reopenComment, editComment and deleteComment, and from
//     commentAction and createCommentFor) is written into the document every reader of that
//     document reads;
//   - a comment list (answerCommentList) is kept in listAnswers under its owner and its ETag
//     and handed to the next reader whose request names the same pair, so an answer read for
//     one person would be served to another;
//   - a write's walk of the issues under an issue (readStatusChange, which plans a step for each
//     of them, and refuseSpecWriterOfMovedSubtree, which judges each of them) decides for the
//     server, not for its caller, what the write touches; no write stores an issue reserved for
//     its author (graphstore.CheckVisibilityAdmitted admits that only for the kinds the read
//     registry flags), so the walk reaches every issue whoever reads it.
var everyReader = model.Actor{}
