package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The re-parented root of these tests, which ran as a tree of its own, and its new parent, the root
// of a live tree, with the parent's primary document.
const (
	reparentedRoot = "LEGION-500"
	reparentParent = "LEGION-208"
	reparentDoc    = "artifact-208"
	reparentRan    = "it ran as a tree of its own"
)

// reparentDispatch is outboxDispatch with each document version's markdown, by document id and
// version.
type reparentDispatch struct {
	outboxDispatch
	versions map[string]string
}

func (d *reparentDispatch) DocumentVersion(_ context.Context, artifactID string, version int) (string, error) {
	markdown, ok := d.versions[fmt.Sprintf("%s@%d", artifactID, version)]
	if !ok {
		return "", fmt.Errorf("no version %d of %s", version, artifactID)
	}
	return markdown, nil
}

// The re-parent seed reads the new parent of a root that ran as a tree of its own: its labels and
// status, its primary document's approval and that version's requirement blocks. Where the parent
// carries the legion label, is not accepted, holds a person's approval of its latest version and
// marks a requirement block there, the root and the parent's tree's architect are told R1's path:
// the root can be written as a view on the parent and register its design gate on that approval
// (dispatch://INFRA-3320, R2). Where any of those fails, both are told today's words: the root's
// work reaches the parent's tree only through that tree's own issues.
func TestTheReparentSeedNamesR1sPathOnlyWhereTheParentOpensIt(t *testing.T) {
	approvedAt := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	person := dispatch.Actor{Kind: "user", ID: "alice"}
	session := dispatch.Actor{Kind: "session", ID: "session-architect"}
	const requirements = ":::requirement{#r1}\n**R1.** The first requirement.\n:::\n"
	approved := func(version, latest int, by dispatch.Actor) dispatch.Approval {
		return dispatch.Approval{IssueKey: reparentParent, State: "approved", Version: &version, LatestVersion: latest, By: by, At: approvedAt, Primary: true}
	}
	stays := fmt.Sprintf("%s is now a child of %s in Dispatch, and it stays a tree of its own in Legion: %s, so the tree of %s cannot take it in and its architect cannot release it.",
		reparentedRoot, reparentParent, reparentRan, reparentParent)
	today := stays + fmt.Sprintf(" It runs under its own architect, and its work reaches the tree of %s only through that tree's own issues.", reparentParent)
	path := stays + fmt.Sprintf(" It runs under its own architect. %s carries the legion label and marks requirement blocks in its primary document. "+
		"A person approved version 7 of that document, which is its latest version. So the architect of %s can write it as a view on %s. "+
		"Then that architect can register the design gate of %s on that approval, with no approval of its own, "+
		"as skill://legion-architect/references/design-gate.md says.", reparentParent, reparentedRoot, reparentParent, reparentedRoot)
	for _, tc := range []struct {
		name     string
		labels   []string
		status   string
		approval dispatch.Approval
		markdown string
		want     string
	}{
		{"a parent in Legion whose approved latest version marks requirement blocks", []string{"legion"}, "in_progress", approved(7, 7, person), requirements, path},
		{"a parent whose approved latest version marks no requirement block", []string{"legion"}, "in_progress", approved(7, 7, person), "No requirement here.\n", today},
		{"a parent with no legion label", []string{"design"}, "in_progress", approved(7, 7, person), requirements, today},
		{"a parent Dispatch shows in retro", []string{"legion"}, "retro", approved(7, 7, person), requirements, today},
		{"a parent whose latest version no person approved", []string{"legion"}, "in_progress", approved(7, 7, session), requirements, today},
		{"a parent approved at a version below its latest", []string{"legion"}, "in_progress", approved(6, 7, person), requirements, today},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			client := &reparentDispatch{versions: map[string]string{reparentDoc + "@7": tc.markdown, reparentDoc + "@6": tc.markdown}}
			client.issue = dispatch.Issue{Key: reparentParent, Status: tc.status, Labels: tc.labels, PrimaryArtifactID: reparentDoc}
			client.approval = tc.approval
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, dispatch: client,
				handlers: []intake.Handler{engine}, now: time.Now}
			putOutboxIssue(t, pool, records, record.Issue{Key: reparentParent, Tree: reparentParent, Project: "LEGION", Title: "parent", Phase: phase.Planning,
				Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "T", HandedOver: true, Repo: "acme/widgets",
				DefaultBranch: "main", GitHubRepo: "acme/widgets"})
			openTree(t, pool, "LEGION", reparentParent)
			// The root as tellReparented left it: told under the parent, the seed queued.
			putOutboxIssue(t, pool, records, record.Issue{Key: reparentedRoot, Tree: reparentedRoot, Project: "LEGION", Title: "a tree of its own",
				Phase: phase.Done, Generation: 2, Status: "todo", DispatchStatus: "todo", Rank: "Z", HandedOver: true, TakenBack: true,
				ToldParent: reparentParent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})

			seed := record.ReparentSeed{Parent: reparentParent, Tree: reparentParent, Ran: reparentRan}
			if err := runner.execute(context.Background(), mustOutboxRow(t, reparentedRoot, seed, time.Now())); err != nil {
				t.Fatalf("execute the re-parent seed: %v", err)
			}

			messages := reparentOutbox(t, pool, `select payload->>'body' from outbox where kind = 'dispatch_message' and issue = $1 order by id`, reparentedRoot)
			if len(messages) != 1 || messages[0] != tc.want {
				t.Fatalf("the messages on %s = %q, want one reading %q", reparentedRoot, messages, tc.want)
			}
			notices := reparentOutbox(t, pool, `select payload->>'reason' from outbox where kind = 'notice' and issue = $1
				and payload->>'kind' = 'child-status' order by id`, reparentParent)
			if len(notices) != 1 || notices[0] != tc.want {
				t.Fatalf("the child-status notices to the tree of %s = %q, want one reading %q", reparentParent, notices, tc.want)
			}

			// The same row run again is the same fact, and tells nothing again.
			if err := runner.execute(context.Background(), mustOutboxRow(t, reparentedRoot, seed, time.Now())); err != nil {
				t.Fatalf("execute the re-parent seed again: %v", err)
			}
			if again := reparentOutbox(t, pool, `select payload->>'body' from outbox where kind = 'dispatch_message' and issue = $1`, reparentedRoot); len(again) != 1 {
				t.Fatalf("the messages on %s after the same row again = %q, want the first alone", reparentedRoot, again)
			}
		})
	}
}

// reparentOutbox is the one text column query reads off the outbox rows of issue, in its order.
func reparentOutbox(t *testing.T, pool *store.Pool, query, issue string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query, issue)
	if err != nil {
		t.Fatalf("read the outbox of %s: %v", issue, err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the outbox of %s: %v", issue, err)
	}
	return texts
}
