package daemon

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// openingWordsTold says whether issue, a member of the tree whose root is tree, was told the
// standing gate's opening in words that hold every one of texts. The opening reaches each holder
// once (workflow's openingDelivery): as a role message queued on issue, or, where the holder of
// issue's phase is owed news of the approved design, as the opening its told row owes, which the
// task that carries that news leads with. This rig sends no such task, so its gate sentence is
// rendered here from the record as the send renders it: RenderNews over the root's gate rows, for
// the deployment of spec alone this rig runs.
func (r *personHandoverRig) openingWordsTold(tree, issue string, texts ...string) bool {
	r.t.Helper()
	holds := func(body string) bool {
		for _, text := range texts {
			if !strings.Contains(body, text) {
				return false
			}
		}
		return true
	}
	if slices.ContainsFunc(r.roleMessages(issue), holds) {
		return true
	}
	var owed bool
	if err := store.TransactWith(r.t.Context(), r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		told, err := record.NewStore().TreeTold(ctx, tx, tree)
		owed = slices.ContainsFunc(told, func(row record.DesignTold) bool { return row.Issue == issue && row.OpeningOwed })
		return err
	}); err != nil {
		r.t.Fatalf("read the told rows of %s's tree: %v", tree, err)
	}
	return owed && holds(workflow.RenderNews(workflow.News{Audience: record.AudienceHolder, Tree: tree, Issue: issue,
		Gates: r.gates(tree), OpeningOwed: true}))
}
