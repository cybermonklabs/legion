package admit

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// An ancestor the daemon records in no live tree when a root comes to rest on its approval gets
// no resting-root notice, so its architect's start at its admission is how it learns of that root
// (dispatch://INFRA-3320, R8): the start's task names each root that rests on its approval and is
// still in the workflow, apart from the decisions kept for it, which it names none of here, and
// before the first act of the shape the record gives the ancestor (workflow.ArchitectStartTask). A
// resting root that has left the workflow is not named. The ancestor may be one the daemon never
// recorded, or one a person moved to the backlog and then moves back to todo.
func TestAnAncestorsStartTaskAtItsAdmissionNamesTheRootsRestingOnIt(t *testing.T) {
	const (
		ancestor = "LEGION-ROOT"
		resting  = "LEGION-RESTS"
		left     = "LEGION-LEFT"
	)
	for _, tc := range []struct {
		name  string
		seed  func(*testing.T, *pgxpool.Pool)
		admit func(*testing.T, *pgxpool.Pool, *Admission)
	}{
		{"an ancestor the daemon does not record", func(*testing.T, *pgxpool.Pool) {}, func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			apply(t, pool, admission, "ancestor-todo", intake.DispatchIssue{Key: ancestor, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
				Status: "todo", Title: "ancestor", Rank: "B", HandedOver: handed}, admission.engine)
		}},
		{"an ancestor on the backlog", parkedRoot, readmitRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(io.Discard, nil)))
			tc.seed(t, pool)
			basis := record.GateBasis{Issue: ancestor, ArtifactID: "artifact-root", Version: 3, Approval: record.EventMark{Seq: 40, At: fixedNow},
				Approver: "user:alice", Serves: []string{"dispatch://LEGION-ROOT/spec#b-p2@v3"}, RegisteredAt: fixedNow}
			for _, key := range []string{resting, left} {
				seedSlotted(t, pool, key, "A")
				root := issue(t, pool, key)
				root.Phase = phase.Implementing
				if key == left {
					root.Phase, root.Status = phase.Done, "done"
				}
				putIssue(t, pool, root)
				inTx(t, pool, func(tx pgx.Tx) {
					gate := record.OpenOnBasis(record.DesignGate{Issue: key, ArtifactID: "artifact-" + key, LatestVersion: 2}, basis)
					if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
						t.Fatalf("put the gate of %s: %v", key, err)
					}
				})
			}

			tc.admit(t, pool, admission)

			var started []record.SuperviseRequest
			for _, start := range architectStartRows(t, pool) {
				if start.Tree == ancestor {
					started = append(started, start)
				}
			}
			restingText := "These roots rest on the approval of LEGION-ROOT and are still in the workflow: LEGION-RESTS (its spec step, in implementing). " +
				"The daemon refuses the sign-off and the close of LEGION-ROOT, and holds its retro where its task has no acceptance step, while any of them is in the workflow (SIGNOFF_RESTING_ROOT_IN_WORKFLOW). " +
				"Each root's own architect, or a person, moves it."
			if len(started) != 1 || started[0].Kept != nil {
				t.Fatalf("the ancestor's architect starts = %#v, want one, which names no decision", started)
			}
			task := started[0].Task
			if want := architectStart(t, pool, admission, ancestor, ""); task != want || !strings.HasPrefix(task, restingText+"\n\n") {
				t.Fatalf("the ancestor's start task = %q, want %q, the derivation over its record, which names the resting root %q first", task, want, restingText)
			}
			if !strings.Contains(task, workflow.ArchitectFreshTreeLabel) || strings.Contains(task, left) {
				t.Fatalf("the ancestor's start task = %q, want the first act of a tree nobody has architected and no word of %s, which has left the workflow", task, left)
			}
		})
	}
}
