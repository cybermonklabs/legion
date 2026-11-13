package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A closed tree's member that a release adopts (AdoptMember, C4 of dispatch://INFRA-3107) has its
// generation cleared, and its design gates with it, so what those gates show a person approved
// past is judged before the clear (ForgetSupersededKept), as Adopt and ReenterChild judge it. A
// changes request the gate's later approval answered is deleted with the gates still standing:
// asked after the clear, it would read as the person's word again, and the catch-up the adoption
// writes, or any later task of the tree that takes the member, would send its architect to
// revise a spec the person approved. A changes request made after that approval is still the
// person's word, and the adoption keeps it for the tree that now owes it.
func TestAnAdoptedClosedTreesMemberForgetsOnlyTheChangesRequestItsGatesApprovalSuperseded(t *testing.T) {
	approvedAt := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	approved := record.EventMark{Seq: 10, At: approvedAt}
	const closedRoot, member, ref = "LEGION-500", "LEGION-501", "dispatch://LEGION-501/spec"
	for _, tc := range []struct {
		name string
		mark record.EventMark
		kept bool
	}{
		{"a changes request the approval came after", record.EventMark{Seq: 8, At: approvedAt.Add(-time.Hour)}, false},
		{"a changes request made after the approval", record.EventMark{Seq: 12, At: approvedAt.Add(time.Hour)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			pool := migratedPool(t)
			live := seedAdmittedTree(t, pool, 1)
			// The closed tree: its root finished with no linger left to run, and its member never
			// started a phase.
			seedIssue(t, pool, record.Issue{Key: closedRoot, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: closedRoot, Project: "LEGION", Title: "Closed root", Phase: phase.Done, Generation: 3, Status: record.DoneStatus, Rank: "W"})
			parent := closedRoot
			adopted := record.Issue{Key: member, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: closedRoot, Project: "LEGION", Title: "Closed tree's member", Parent: &parent, Phase: phase.Admitted,
				Generation: 3, Status: "todo", Rank: "X"}
			seedIssue(t, pool, adopted)
			records := record.NewStore()
			version := 2
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if err := records.PutGate(ctx, tx, record.DesignGate{Issue: member, Step: record.DesignStepSpec, ArtifactID: "artifact-501",
					LatestVersion: version, ApprovedVersion: &version, ApprovedMark: approved, EverOpened: true}); err != nil {
					return err
				}
				return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: member, Ref: ref,
					Decision: record.DecisionDesignChangesRequested, Actor: "user:alice", At: tc.mark.At, Seq: tc.mark.Seq})
			}); err != nil {
				t.Fatalf("keep the person's changes request beside the gate's approval: %v", err)
			}

			engine := testEngine(config.DesignGateRootIssues, nil)
			applyInTx(t, pool, func(ctx context.Context, tx pgx.Tx) error {
				return engine.AdoptMember(ctx, tx, adopted, live, intake.AdoptedChild{Issue: member, Parent: live.Key})
			})

			var kept []record.ParkedDecision
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				var err error
				kept, err = records.ParkedDecisions(ctx, tx, live.Tree)
				return err
			}); err != nil {
				t.Fatalf("read what the tree of %s keeps: %v", live.Tree, err)
			}
			if !tc.kept {
				if len(kept) != 0 {
					t.Fatalf("the tree of %s keeps %#v after the adoption: the approval at %#v came after the changes request at %#v, so the request is forgotten before the clear deletes the gate that says so",
						live.Tree, kept, approved, tc.mark)
				}
				for _, payload := range noticeRows(t, pool) {
					if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" && notice.CatchUp != nil && strings.Contains(notice.CatchUp.Decided, ref) {
						t.Fatalf("a catch-up names %q after the adoption: the person approved past that changes request", notice.CatchUp.Decided)
					}
				}
				return
			}
			if len(kept) != 1 || kept[0].Issue != member || kept[0].Seq != tc.mark.Seq || !kept[0].At.Equal(tc.mark.At) {
				t.Fatalf("the tree of %s keeps %#v after the adoption, want the changes request at %#v, which came after the approval",
					live.Tree, kept, tc.mark)
			}
		})
	}
}
