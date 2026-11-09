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
