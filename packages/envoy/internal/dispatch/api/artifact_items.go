package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) documentOwnerForHandler(w http.ResponseWriter, r *http.Request, who model.Actor) (owner, bool) {
	_, documentOwner, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return owner{}, false
	}
	return documentOwner, true
}

func (s *server) listArtifactAsks(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r, who)
	if !ok {
		return
	}
	state, err := parseAskListState(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	asks, _, err := s.queryOwnerAsks(r.Context(), s.deps.Store.Pool, who, owner, state)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	reads := shapedAsks(asks, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, reads)
}

func (s *server) createArtifactAsk(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r, who)
	if !ok {
		return
	}
	// The ask's related-item check reads as this same reader (createAskFor's readerOf).
	s.createAskFor(w, r.WithContext(withReader(r.Context(), who)), owner)
}

// listArtifactComments answers an unlinked document's comment list: the same list as an issue's,
// under the same validator and keeping the same answer (answerCommentList), scoped to the one
// document the route names rather than to an issue.
func (s *server) listArtifactComments(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r, who)
	if !ok {
		return
	}
	s.answerCommentList(w, r, owner, "", r.PathValue("id"))
}

func (s *server) createArtifactComment(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r, who)
	if !ok {
		return
	}
	s.createCommentFor(w, r, owner)
}

func (s *server) listArtifactEvents(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r, who)
	if !ok {
		return
	}
	options, err := parseEventListOptions(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events, err := s.readEvents(r.Context(), who, owner, options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	attachEventTrees(events, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, events)
}
