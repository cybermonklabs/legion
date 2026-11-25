package api

import (
	"context"
	"maps"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
// viewer to: the route is a read of the whole deployment (readclass.Deployment), which an open
// viewer reads and every other viewer is refused before this runs (withinReadScope). The one thing
// of a project a pass names is the repository its cursor rests in, which the route holds to the
// reader (leaveOutHiddenCursors).
func (s *server) listCIReconcilePasses(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	reader, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	passes, err := cireconcile.RecentPasses(r.Context(), s.deps.Store.Pool)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.leaveOutHiddenCursors(r.Context(), reader, passes); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, passes)
}

// leaveOutHiddenCursors writes each pass's cursor as reader reads it: a cursor resting in a
// repository reader may not see by the GitHub proxy's rule (repositoriesHidden, over
// refs.RepositoryClause: a private project reader does not see owns it) is the zero cursor, the
// start of the order, which is what a pass that reached the end of the order leaves. So the
// repository's name, and the page and pull request numbers of it the cursor holds, reach no such
// reader, and the pass reads as one that left no repository behind, as the App-installation
// route answers such a repository as one the App is not installed on. One statement is made for
// every repository the passes name together, and none for a reader that sees every object (R9).
func (s *server) leaveOutHiddenCursors(ctx context.Context, reader model.Actor, passes []cireconcile.PassRecord) error {
	named := map[string]bool{}
	for _, pass := range passes {
		if pass.Cursor != nil && pass.Cursor.Repository != "" {
			named[pass.Cursor.Repository] = true
		}
	}
	hidden, err := s.repositoriesHidden(ctx, reader, slices.Sorted(maps.Keys(named)))
	if err != nil {
		return err
	}
	for _, pass := range passes {
		if pass.Cursor != nil && hidden[pass.Cursor.Repository] {
			*pass.Cursor = cireconcile.PassCursor{}
		}
	}
	return nil
}
