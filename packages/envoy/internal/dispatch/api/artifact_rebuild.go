package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// rebuildArtifactInput is the body of POST /api/v1/artifacts/{id}/rebuild and of POST
// /api/v2/artifacts/{id}/rebuilds (rebuildArtifactV2).
type rebuildArtifactInput struct {
	Markdown *string `json:"markdown"`
}

// rebuildArtifact discards only a history ygo cannot load and replaces it with one seed update
// from the latest saved markdown, or supplied markdown. It needs the person scope (its route's OAuthScopes)
// because a rebuild intentionally deletes durable history, so only a credential acting as its
// person may ask for one; a document that loads is refused unchanged. The rebuild,
// the version supplied markdown writes, its artifact.version event and the move of the open
// approval request to that version commit in one transaction, so no refusal or failure leaves a
// changed document without them. A closed issue's document is rebuilt with the close's ask states
// written into its seed (docs.Service.RebuildDocument), so a block whose ask the close withdrew
// reads resolved once the rebuild commits, not open as the latest version saved it.
func (s *server) rebuildArtifact(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input rebuildArtifactInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.rebuildDocument(w, r, actor, input)
}

// rebuildDocument rebuilds the document the path names for actor, as input asks, once the route
// has read its body: the work of POST /api/v1/artifacts/{id}/rebuild (rebuildArtifact) and POST
// /api/v2/artifacts/{id}/rebuilds (rebuildArtifactV2). It answers the rebuild's report 200.
func (s *server) rebuildDocument(w http.ResponseWriter, r *http.Request, actor model.Actor, input rebuildArtifactInput) {
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, r, apicode.NotDocument, "artifact is not a document")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	defer tx.Rollback(r.Context())
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	report, written, err := s.deps.Docs.RebuildDocument(documentCtx, artifact.ID, input.Markdown, actor)
	if errors.Is(err, docs.ErrDocumentLive) {
		writeError(w, r, apicode.DocumentLive, err.Error())
		return
	}
	if errors.Is(err, docs.ErrDocumentLoads) {
		writeError(w, r, apicode.DocumentLoads, err.Error())
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var published []*model.Event
	if written.Wrote {
		event, err := s.commitArtifactVersionEvent(ledger, ownerForArtifact(artifact), actor, artifact.ID, artifact.Name, written, nil)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		published = append(published, event)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.publish(r.Context(), appendedEvents(published...)...)
	WriteJSON(w, http.StatusOK, report)
}

func (s *server) injectArtifactSchemaFailure(w http.ResponseWriter, r *http.Request) {
	s.injectArtifactForTest(w, r, s.deps.Docs.InjectSchemaInvalidForTest)
}

// injectArtifactNewerContent appends to the document an element no build knows
// (docs.InjectUnknownElementForTest), so a browser test opens a document holding newer content.
func (s *server) injectArtifactNewerContent(w http.ResponseWriter, r *http.Request) {
	s.injectArtifactForTest(w, r, s.deps.Docs.InjectUnknownElementForTest)
}

// injectArtifactForTest runs a docs test hook on the request's document and answers 204.
func (s *server) injectArtifactForTest(w http.ResponseWriter, r *http.Request, inject func(ctx context.Context, artifactID string) error) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, r, apicode.NotDocument, "artifact is not a document")
		return
	}
	if err := inject(r.Context(), artifact.ID); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
