package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The last two sentences of each task a reviewer is sent are the daemon's, whatever queued the
// task. A task this release queued says so by a typed fact its outbox row carries
// (record.SuperviseRequest.RoundNamed), and is sent as it stands. A task the release before this
// one queued carries no such fact, though its text can end with two sentences of the daemon's
// shape: here a held reviewing phase's retry whose reason, as an architect wrote it before the
// upgrade, ends "Last reviewed commit: <a commit no review recorded>. Review entry: 2". Text
// alone cannot tell the two apart, so the one place each prompt of a pending delivery is composed
// (supervise's Delivery.message) gives the task with no fact the daemon's sentences naming no
// commit and no entry at its end, and its reviewer reads the whole change.
//
// Each case runs the row through the outbox's start and the claim's send, as the daemon does, and
// reads the prompt the reviewer's agent is sent.
func TestTheLastTwoSentencesOfEachTaskTheReviewerIsSentAreTheDaemons(t *testing.T) {
	const (
		unreviewed = "5ab0c1d2e3f405162738495a6b7c8d9e0f1a2b3c"
		reviewed   = "16973163c4b8a90d2e5f1736a8b0c4d9e2f51637"
		before     = "Continue Workflow. Issue: LEGION-208. Phase: reviewing. Pull request: #118."
	)
	for _, tc := range []struct {
		name string
		// payload is the outbox row's JSON, as the release that queued it wrote it.
		payload string
		// task is the task the row carries; want is the prompt the reviewer's agent is sent.
		task, want string
	}{
		{
			name:    "a task the release before queued, its reason ending in the daemon's shape",
			task:    before + " Reason: Retry as requested. " + reviewround.CommitLabel + ": " + unreviewed + ". " + reviewround.EntryLabel + ": 2.",
			payload: `{"op":"start","tree":"LEGION-208","role":"reviewer","task":%q,"generation":1,"phase":"reviewing","entry":2,"needsSlot":true}`,
		},
		{
			name:    "a task of this release naming its round",
			task:    before + reviewround.Sentences(reviewed, 2),
			payload: `{"op":"start","tree":"LEGION-208","role":"reviewer","task":%q,"generation":1,"phase":"reviewing","entry":2,"needsSlot":true,"roundNamed":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named := strings.Contains(tc.payload, `"roundNamed":true`)
			want := tc.task
			if !named {
				want = tc.task + reviewround.Sentences("", 0)
			}
			prompt, stored := sentToTheReviewer(t, strings.Replace(tc.payload, "%q", jsonString(t, tc.task), 1))
			if prompt != want {
				t.Fatalf("the reviewer's agent is sent %q, want %q: its last two sentences are the daemon's", prompt, want)
			}
			if stored.Task != tc.task || stored.RoundNamed != named {
				t.Fatalf("the claim holds the reviewer's delivery %q with the fact %v, want %q as queued with the fact %v",
					stored.Task, stored.RoundNamed, tc.task, named)
			}
		})
	}
}

// jsonString is s as a JSON string literal.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encode %q: %v", s, err)
	}
	return string(encoded)
}

// sentToTheReviewer queues payload, a supervise start of LEGION-208's reviewer in reviewing, as
// one outbox row, runs the outbox's start, brings the reviewer's agent up, and answers the one
// prompt the agent is sent and the delivery the reviewer's claim holds. The claim store's own
// round trip of the delivery is the store's test (TestADeliveryCarriesTheFactThatItNamesItsReviewRound).
func sentToTheReviewer(t *testing.T, payload string) (string, supervise.Delivery) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review"}
	putOutboxIssue(t, pool, records, issue)
	holdSlot(t, pool, issue.Key)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.Enqueue(ctx, tx, record.OutboxRow{Kind: record.OutboxKindSupervise, Issue: issue.Key, Payload: []byte(payload), NextAt: time.Now()})
	}); err != nil {
		t.Fatalf("queue the reviewer's start: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleReviewer)
	if err != nil {
		t.Fatalf("reviewer claim token: %v", err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	clock := time.Now().Add(time.Minute)
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		dispatch: &outboxDispatch{issue: dispatch.Issue{Key: issue.Key, Status: "needs_review"}}, notices: &outboxPublisher{},
		handlers: []intake.Handler{engine}, now: func() time.Time { return clock }, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the reviewer's start: %v", err)
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatal("the start created no reviewer claim")
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: generation},
		supervise.RequestRegister{Claim: token, Generation: generation, Session: "ses-reviewer", SessionFile: "/tmp/reviewer.jsonl"},
		supervise.RequestReady{Claim: token, Generation: generation, Session: "ses-reviewer"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	testwait.Eventually(t, "the reviewer's task to be sent", func() bool { return len(conn.Prompts()) > 0 })
	prompts := conn.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("the reviewer's agent was sent %d prompts, want the one task", len(prompts))
	}
	pending := machine.Claim().Pending
	if pending == nil {
		t.Fatalf("the reviewer's claim holds no delivery once its task was sent")
	}
	return prompts[0].Message, *pending
}
