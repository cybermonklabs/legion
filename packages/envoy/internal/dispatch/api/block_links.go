package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// getBlockLinks lists the links from blocks of the document ?from= names to blocks of the
// document ?to= names (one document may be both), newest first: each with both block nodes, its
// edge kind, and when and in which version of its document the source block first held it. A link
// counts only while both of its blocks are current blocks of their documents.
func (s *server) getBlockLinks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	documents, err := s.blockLinkDocuments(r.Context(), [2]string{query.Get("from"), query.Get("to")}, apicode.NotFound)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	links, err := refs.BlockLinks(r.Context(), s.deps.Store.Pool, documents[0].ID, documents[1].ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, model.BlockLinks{From: documents[0], To: documents[1], Links: links})
}

// blockLinkDocuments is the document nodes the from and the to of a block-links read name, in
// that order, each read by blockLinksDocument; a reference that names no document is answered
// with notFound, which is NOT_FOUND under /api/v1 and ARTIFACT_NOT_FOUND under /api/v2.
func (s *server) blockLinkDocuments(ctx context.Context, raws [2]string, notFound apicode.Code) ([2]model.GraphNode, error) {
	var documents [2]model.GraphNode
	for index, param := range [2]string{"from", "to"} {
		raw := strings.TrimSpace(raws[index])
		document, found, err := s.blockLinksDocument(ctx, param, raw)
		if err != nil {
			return documents, err
		}
		if !found {
			return documents, errorf(notFound, "no document %s", raw)
		}
		documents[index] = document
	}
	return documents, nil
}

// blockLinksDocument is the document node the reference raw, given as the query parameter
// param, names: a reference to an issue's or a project's document, parsed as GET
// /api/v1/references parses one (text.ParseRef, whatever the length of its name). Anything else
// is 400 INVALID_REFERENCE, and found is false for a document that does not exist.
func (s *server) blockLinksDocument(ctx context.Context, param, raw string) (model.GraphNode, bool, error) {
	ref, ok := text.ParseRef(raw, s.deps.ServerURL)
	if !ok || ref.Kind != "artifact" || ref.Session != "" {
		return model.GraphNode{}, false, errorf(apicode.InvalidReference, "%s must name a document: dispatch://KEY/spec, dispatch://KEY/artifact/<slug> or dispatch://PROJECT/artifact/<slug>", param)
	}
	return refs.Node(ctx, s.deps.Store.Pool, "artifact", text.TargetID(ref), true)
}
