package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
)

// listCIReconcilePasses answers the CI reconcile passes of the last day, newest first.
//
// The pass logs one INFO line and the container log rotates - the production pod's reached back
// 45 minutes - so the counts also live in ci_reconcile_passes, and this is how anyone reads them
// without a shell on the pod. It reads rows and nothing else: a row with no end time is read as a
// pass still running only while its process is live, the lease is held and it is the newest row
// (cireconcile.RecentPasses), and this route decides none of that itself.
//
// A pass covers repositories of many projects and names no issue, so it has no project to hold a
// viewer to: the route is a read of the whole deployment (readclass.Deployment alone), which an open
// viewer reads whole and every other viewer is refused before this runs (withinReadScope).
func (s *server) listCIReconcilePasses(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	passes, err := cireconcile.RecentPasses(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, passes)
}
