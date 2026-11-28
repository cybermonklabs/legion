package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// toldNotice is one notice row a failure wrote: its outbox kind, the notice's own kind, the
// failure it carries and its reason. inReason, on a wanted row, says the row carries no failure
// record and names the death in its reason instead.
type toldNotice struct {
	outbox, kind string
	failure      *record.NoticeFailure
	reason       string
	inReason     bool
}

// When a role's deaths for memory fail its claim, whoever is told of the failure reads what killed
// it in plain words, with the number it passed, before the runtime's own detail
// (dispatch://INFRA-1117 R9): for a phase worker, the hold its architect gets and the worker-died
// beside it (a hold its architect has not escalated is the architect's alone); for the tree's own
// architect, which nobody inside the tree is left to read, the worker-died the controller gets. Each carries the claim's record of
// its last death as failure.death, and the row to the tree architect's own role, which carries no
// failure record, names the same words in its reason (workflow's deathReason). The deaths are real
// ones: a `roles` container the kubelet ended OOMKilled under a 19Gi limit ends the role's open turn
// twice, against a launch failure limit of two, and the daemon's own hook carries the failure to
// the workflow, which writes the notices.
func TestPlainCauseTheNoticesOfAClaimItsDeathsFailSayWhatKilledIt(t *testing.T) {
	t.Parallel()
	const detail = "pod legion-legion-legion-208 (uid uid-pod-1) Failed: container roles terminated (OOMKilled, exit code 137)"
	const death = "the pod's roles container, which runs every role of this issue, was killed because it used more than its 19 GiB " +
		"memory limit. The runtime's detail: gone: " + detail
	for _, tc := range []struct {
		name string
		role claim.Role
		want []toldNotice
	}{
		{name: "a phase worker, whose deaths hold its issue", role: claim.RolePlanner, want: []toldNotice{
			{outbox: string(record.OutboxKindNotice), kind: "held"},
			{outbox: string(record.OutboxKindNotice), kind: "worker-died"},
		}},
		{name: "the tree's own architect", role: claim.RoleArchitect, want: []toldNotice{
			{outbox: string(record.OutboxKindNotice), kind: "worker-died", inReason: true},
			{outbox: string(record.OutboxKindControllerNotice), kind: "worker-died"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
				Tree: "LEGION-208", Title: "Workflow", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"}
			putOutboxIssue(t, pool, records, issue)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
			sup.OnDurableState((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger(), limits: sup.deps.Limits}).claimDurableState)
			token, err := claim.NewToken("legion", issue.Key, tc.role)
			if err != nil {
				t.Fatal(err)
			}
			machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: tc.role, State: supervise.StateQueued}, "")
			if err != nil {
				t.Fatal(err)
			}
			sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
				t.Fatalf("spawn the %s: %v", tc.role, err)
			}
			for death := 1; death <= sup.deps.Limits.LaunchFailures; death++ {
				generation := machine.Claim().Generation
				for _, ev := range []supervise.Event{
					supervise.StreamHello{Claim: token, Generation: generation},
					supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-1", SessionFile: "/tmp/ses-1.jsonl", CapabilityHash: []byte("capability")},
					supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-1"},
					supervise.StreamTurnStart{Claim: token},
				} {
					if err := machine.Handle(ctx, ev); err != nil {
						t.Fatalf("death %d: bring the %s into a turn: handle %T: %v", death, tc.role, ev, err)
					}
				}
				if err := machine.Handle(ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
					Locator: *machine.Claim().Locator, Kind: runtime.Gone, At: time.Now(), Detail: detail, OutOfMemory: true,
					Cause: runtime.DeathCause{Kind: runtime.DeathMemoryLimit, Limit: 19 << 30},
				}}); err != nil {
					t.Fatalf("death %d of the %s: %v", death, tc.role, err)
				}
			}
			if failed := machine.Claim(); failed.State != supervise.StateFailed || failed.FailureCause != supervise.CauseDied {
				t.Fatalf("after its deaths ran out the %s is %s with cause %q, want failed for its deaths", tc.role, failed.State, failed.FailureCause)
			}

			var told []toldNotice
			testwait.Eventually(t, "the failure's notices to be written", func() bool {
				told = plainCauseNotices(t, pool)
				return len(told) >= len(tc.want)
			})
			if len(told) != len(tc.want) {
				t.Fatalf("the failure wrote %+v, want the notices %+v", told, tc.want)
			}
			for i, want := range tc.want {
				got := told[i]
				if got.outbox != want.outbox || got.kind != want.kind {
					t.Errorf("notice %d is a %s %s, want a %s %s", i, got.outbox, got.kind, want.outbox, want.kind)
					continue
				}
				if want.inReason {
					if got.failure != nil || !strings.Contains(got.reason, death) {
						t.Errorf("the %s %s carries the failure %+v and the reason %q, want no failure record and %q in the reason",
							got.outbox, got.kind, got.failure, got.reason, death)
					}
					continue
				}
				if got.failure == nil || got.failure.Death != death {
					t.Errorf("the %s %s carries the failure %+v, want failure.death %q", got.outbox, got.kind, got.failure, death)
				}
			}
		})
	}
}

// plainCauseNotices are the notice rows of a claim's failure the outbox holds, the holds and the
// worker-dieds, in the order they were written, each decoded as the daemon's executor decodes it.
// A root architect's ready writes a catch-up beside them, which is no notice of the failure.
func plainCauseNotices(t *testing.T, pool *store.Pool) []toldNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select id, kind, issue, payload from outbox where kind in ($1, $2) order by id",
		string(record.OutboxKindNotice), string(record.OutboxKindControllerNotice))
	if err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	defer rows.Close()
	var told []toldNotice
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a notice row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode notice row %d: %v", row.ID, err)
		}
		var notice record.Notice
		switch p := payload.(type) {
		case record.Notice:
			notice = p
		case record.ControllerNotice:
			notice = record.Notice(p)
		default:
			t.Fatalf("notice row %d decodes as %T", row.ID, payload)
		}
		if notice.Kind == "held" || notice.Kind == "worker-died" {
			told = append(told, toldNotice{outbox: string(row.Kind), kind: string(notice.Kind), failure: notice.Failure, reason: notice.Reason})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the notice rows: %v", err)
	}
	return told
}
