package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// getIssueReferences answers an issue's references read, one hop in each direction (refs.Closure):
// links, every node the issue's own content links to, and linked_from, every node that links to
// the issue or to its own content, each written [] when it has no row and never null. members are
// the documents of links, each read whole by its ref_key, at depth 1 and with its row's via, and
// truncated is false, since a read of one hop is never cut: both stay for the clients of the build
// before, which read them alone. Each member carries its kind and address: artifact and the
// document's own, or lens and the lens's where a link names the address of a document the
// pre-mortem conversion moved to a lens, its artifact then the retired document. The read runs
// under referencesClosureBound (inClosureRead); loadIssue before it and loadArtifactsByRefKey
// after it read by key on the pool, outside the bound. ?versions=latest leaves each member's
// artifact.versions holding only its latest version, under its own ETag; absent, empty or all is
// the full answer. A repeated versions is refused rather than read by its first value.
func (s *server) getIssueReferences(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	versionsValues := r.URL.Query()["versions"]
	if len(versionsValues) > 1 {
		s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be given once, as all or latest"))
		return
	}
	var latestOnly bool
	if len(versionsValues) == 1 {
		switch versionsValues[0] {
		case "", "all":
		case "latest":
			latestOnly = true
		default:
			s.writeHandlerError(w, errorf(http.StatusBadRequest, "INVALID_QUERY", "versions must be all or latest"))
			return
		}
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	closure, err := s.inClosureRead(r.Context(), who, issue.Key)
	if err != nil {
		if closureBoundHit(err) {
			s.writeHandlerError(w, errorf(http.StatusServiceUnavailable, "REFERENCES_TIMEOUT",
				"the issue's reference closure ran past its %s bound, so no reference was read; try again later", referencesClosureBound))
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	refKeys := make([]string, len(closure.Members))
	for index, member := range closure.Members {
		refKeys[index] = member.RefKey
	}
	artifacts, err := s.loadArtifactsByRefKey(r.Context(), s.deps.Store.Pool, refKeys, latestOnly)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	members := make([]model.ReferenceMember, 0, len(closure.Members))
	for _, closureMember := range closure.Members {
		artifact, ok := artifacts[closureMember.RefKey]
		if !ok {
			s.writeHandlerError(w, fmt.Errorf("reference target %q disappeared while loading closure", closureMember.RefKey))
			return
		}
		members = append(members, model.ReferenceMember{
			Artifact: *artifact,
			Kind:     closureMember.Kind,
			Address:  closureMember.Address,
			Depth:    closureMember.Depth,
			Via:      closureMember.Via,
		})
	}
	// Each row's name reaches a caller that sent Dispatch-Trees: 1 with its tree, read where it
	// stands (newIssueReferencesRead). The answer is encoded once, exactly as WriteJSON writes it,
	// trailing newline and all, so the token hashes the bytes it validates and those are the bytes
	// written; the token names the reading and the parser that read the trees too
	// (referencesETag), so a validator given for one reading never answers the other.
	trees := wantsDisplayTrees(r)
	var answer bytes.Buffer
	if err := json.NewEncoder(&answer).Encode(newIssueReferencesRead(model.IssueReferences{
		Members:    members,
		Truncated:  false,
		Links:      closure.Links,
		LinkedFrom: closure.LinkedFrom,
	}, trees)); err != nil {
		s.writeHandlerError(w, fmt.Errorf("encode the issue's references: %w", err))
		return
	}
	if answerNotModified(w, r, s.referencesETag(answer.Bytes(), latestOnly, trees)) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(answer.Bytes())
}
