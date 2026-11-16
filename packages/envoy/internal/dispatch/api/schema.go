package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// getBlockSchema answers the block schema this build serves to any authenticated caller; the
// dashboard builds it in, so only a page of an earlier build asks, with its person's cookie.
func (s *server) getBlockSchema(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(pmdoc.SchemaJSON())
}
