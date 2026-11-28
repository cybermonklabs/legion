package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// viewArmsRequireTheScaleReadBesideUnprojectedWaits is c-view-arms' read of one issue at scale
// beside the waits no issue_links row projects: graph_edges stated with arms (the view of the step
// that deletes the four arms, which the database holds once that step has run, or the head the
// migrations state), rollbackStateUnprojectedWaits' waits stated out of rollbackScaleIssue, and the
// read held to requireTheIssueReadAtScale: the answer it gave without them, since the probe of
// issue_links answers none of them, and no Seq Scan of any relation table in its plan.
//
// Over seedRollbackScale's rows alone Postgres probes issue_links per edge whether or not the probe
// ends in `offset 0`, so no plan of that read tells the two views apart (rollbackStateUnprojectedWaits
// says why). Beside these waits it reads an unfenced probe as a hashed `= any` over the whole of
// issue_links, so this read fails naming issue_links where the view's probe has no `offset 0`
// (c-view-arms' third control), and passes where it has one.
func viewArmsRequireTheScaleReadBesideUnprojectedWaits(t *testing.T, database *store.Store, arms store.GraphEdgesArms) {
	t.Helper()
	if arms != store.GraphEdgesFourArmsDeleted {
		stateRollbackView(t, database, arms)
		defer stateRollbackView(t, database, store.GraphEdgesFourArmsDeleted)
	}
	defer rollbackStateUnprojectedWaits(t, database)()
	requireTheIssueReadAtScale(t, database)
}
