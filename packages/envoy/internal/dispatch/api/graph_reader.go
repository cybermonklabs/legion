package api

import (
	"context"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// readerKey is the request-context key of the reader a layer already resolved (withReader).
type readerKey struct{}

// withReader puts a resolved reader into a request context, for readerFromContext to read back:
// a layer that resolves the request's reader before the handler runs (withinReadScope) hands it
// on this way, so the handler's readerOf resolves nothing a second time.
func withReader(ctx context.Context, reader model.Actor) context.Context {
	return context.WithValue(ctx, readerKey{}, reader)
}

// readerFromContext is the reader withReader put into ctx, if any.
func readerFromContext(ctx context.Context) (model.Actor, bool) {
	reader, ok := ctx.Value(readerKey{}).(model.Actor)
	return reader, ok
}

// resolveReader is the reader a read of the request answers for: the person at the browser, the
// session a broker-issued token proved, or the person a personal token belongs to, as a read binds
// an actor (model.Actor.Reader), with what that credential may see (access.Resolve). Every read of
// the node and tuple store binds its viewer (refs.VisibilityArg), so a node of a project the
// caller does not see, like a node reserved for another author, is one the graph does not hold.
//
// A reader already in r's context is answered as it is. Otherwise the caller is resolved
// (optionalActor) and access.Resolve runs once. A credential that does not resolve answers its own
// error, the one every gate on this server answers; a failed Resolve answers the read's error.
//
// A server with no store - a router's test server, or one whose database is not configured -
// answers the caller with the closed viewer (closedViewer): it never reads, and fails closed, so a
// read that binds it answers nothing and no write reaches an object its caller may not see.
func (s *server) resolveReader(r *http.Request) (model.Actor, error) {
	if reader, ok := readerFromContext(r.Context()); ok {
		return reader, nil
	}
	actor, _, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, err
	}
	return s.readerFor(r.Context(), actor)
}

// readerFor is actor as a read answers for it (model.Actor.Reader), with what its credential sees
// now (access.Resolve): the one resolution behind resolveReader, and the one a live socket makes
// again of the caller its upgrade resolved (feedEvents, the delivery hub's access re-check).
func (s *server) readerFor(ctx context.Context, actor model.Actor) (model.Actor, error) {
	reader := actor.Reader()
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		reader.Viewer = closedViewer()
		return reader, nil
	}
	viewer, err := access.Resolve(ctx, s.deps.Store.Pool, actor)
	if err != nil {
		return model.Actor{}, err
	}
	reader.Viewer = &viewer
	return reader, nil
}

// closedViewer is a viewer that sees no project and no party, is neither open nor all, and reads
// as though some project were private: the one refs.VisibilityArg binds for a reader with no
// viewer, made a value so a reader that holds it says so. Each call answers its own, so no reader
// can change another's.
func closedViewer() *model.Viewer {
	return &model.Viewer{Sees: []string{}, AnyPrivate: true}
}

// readerOf is resolveReader for a handler: a caller whose credential does not resolve is refused
// as every gate on this server refuses one (writeAuthenticationError), and a failed Resolve is the
// server's error, which that answers as writeHandlerError does.
func (s *server) readerOf(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	reader, err := s.resolveReader(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	return reader, true
}

// readerAs is actor - the writer a handler records, or the person a credential acts as - as a read
// binds it: actor kept as the author, with the viewer of the request's credential (resolveReader),
// so what a write answers, or a read made for that actor, shows only what the caller may see. The
// writer itself stays as it is: a handler records actor and hands this one only to its reads.
//
// It resolves on the pool, so a handler calls it before it opens a transaction or holds a
// connection: inside either, access.Resolve waits on a second connection (store.ErrNestedAcquire).
// A caller that does not resolve is refused as readerOf refuses one.
func (s *server) readerAs(w http.ResponseWriter, r *http.Request, actor model.Actor) (model.Actor, bool) {
	reader, ok := s.readerOf(w, r)
	if !ok {
		return model.Actor{}, false
	}
	actor = actor.Reader()
	actor.Viewer = reader.Viewer
	return actor, true
}

// everyReader is who a read of the graph answers for when what it reads is kept or sent where
// many readers read it rather than answered to the one person whose request made it: the server's
// own viewer (model.ServerViewer), which sees every project and every party and proves no author,
// so a node reserved for its author is never in what everyone reads, whoever's request wrote it.
// Each read it makes reads content that belongs to one owner, one project or one conversation,
// and is gated per viewer where it leaves the server. TestEveryServerReadIsNamed holds every use
// to serverReads, which says why no single caller is its reader:
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
//     one person would be served to another.
var everyReader = model.ServerReader
