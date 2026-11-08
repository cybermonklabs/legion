package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/answerloop"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// askBy is an AskReader answering that askID was opened by session.
type askBy struct{ session string }

func (a askBy) Ask(_ context.Context, askID string) (dispatch.Ask, error) {
	return dispatch.Ask{ID: askID, Author: dispatch.Actor{Kind: "session", ID: a.session}}, nil
}

// A phase worker is told to open its findings as an ask on the controller's report issue
// (skill://legion-worker/references/findings.md, "Where it goes"), an issue that is open, in no
// tree and not recorded by the daemon. When the worker's own tree is later parked and the worker
// stops, a person's answer to that ask reaches nobody: the answer loop reads the agent's
// conversation issue, finds a parked tree and leaves the decision to #2163's relay
// (answers.go:245-252), while the relay reads the issue the ask lives on, finds no recorded root
// and sends nothing (workflow/ask_mirror.go:238, workflow/parked_decision.go:27-30).
func TestADecisionOnAnAskOfAnotherIssueReachesSomeone(t *testing.T) {
	// The control is the same answer on an ask of the agent's own issue, which #2163's relay owns
	// and sends: it shows the seeding is right and the issue the ask lives on is the difference.
	for _, tc := range []struct {
		name    string
		askOn   string
		control bool
	}{
		{name: "the ask is on the agent's own issue (control)", askOn: "LEGION-4", control: true},
		{name: "the ask is on the controller's report issue", askOn: "LEGION-900"},
	} {
		t.Run(tc.name, func(t *testing.T) { decisionOnAskOf(t, tc.askOn, tc.control) })
	}
}

func decisionOnAskOf(t *testing.T, askOn string, control bool) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	// A parked tree: root LEGION-1 lingers out of the workflow, with the tester's issue under it.
	noticeTree(t, pool, records, true)
	root, child, report := "LEGION-1", "LEGION-4", askOn
	putOutboxIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: root, Parent: &root, Title: "Parked child", Phase: phase.Done, Generation: 1, Status: "backlog"})
	// The controller's report issue is in no tree and the daemon records nothing of it.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		recorded, err := records.Issue(ctx, tx, report)
		if err != nil {
			return err
		}
		if (recorded != nil) != control {
			t.Fatalf("the ask's issue %s is recorded as %#v; control %t", report, recorded, control)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, root, supervise.StateWorking)
	publisher := &holderPublisher{}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

	// What the loop makes of the hand-off: the ask's author is the stopped tester, so the person's
	// answer is the tester's decision, carried with the tester's conversation (issue LEGION-4)
	// while the ask itself is on LEGION-900. Nothing ties the two together (classify.go ownAsk).
	conv := store.AgentConversation{Session: "ses_tester", Project: "legion", Issue: child, Role: claim.RoleTester}
	text := "Use B. Why did you test A?"
	payload, err := json.Marshal(map[string]any{
		"id": 34, "type": "ask.answered", "actor": map[string]any{"kind": "user", "id": "person@example.com"},
		"issue_key": report, "project": "LEGION",
		"payload": map[string]any{"id": "a-9", "issue_key": report, "kind": record.AskQuestion, "state": "answered",
			"answer": map[string]any{"user": "person@example.com", "selected": []string{"B"}, "text": text}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handoff := intake.Handoff{
		OriginalTopic: "notifications.agent.ses_tester", TargetSession: "ses_tester",
		RoleToken: string(mustClaimToken(t, child, claim.RoleTester)), Project: "legion",
		Envelope: intake.HandoffEnvelope{EventID: "evt-1", Source: "dispatch", SourceEventID: "1",
			Topic: "notifications.agent.ses_tester", DedupeKey: "dispatch-1", PayloadSummary: report,
			Payload: string(payload), TraceID: "t"},
	}
	verdict, err := answerloop.Classify(ctx, handoff, conv, askBy{session: "ses_tester"})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Decision == nil {
		t.Fatalf("the hand-off carries no decision: %+v", verdict)
	}
	decision := *verdict.Decision
	if decision.Conversation.Issue != child || decision.Ref != "dispatch://"+report+"/ask/a-9" || !decision.Answered ||
		decision.AskKind != record.AskQuestion {
		t.Fatalf("decision = %+v; want the tester's conversation on %s with the ask on %s", decision, child, report)
	}

	// #2163's relay reads the same answer, keyed on the issue the ask lives on.
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Clock: func() time.Time { return now }}, quietLogger())
	if result, err := intake.ApplyFact(ctx, pool, "dispatch", "dispatch-34", intake.DispatchAskClosed{
		Key: report, Ask: "a-9", Kind: record.AskQuestion, State: "answered",
		Actor: dispatch.Actor{Kind: "user", ID: "person@example.com"}, Mark: record.EventMark{Seq: 7, At: now},
	}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("apply the person's answer = %#v, %v", result, err)
	}
	t.Logf("the relay queued %d parked-decision notices", len(parkedDecisions(t, pool)))

	// The loop passes the decision on, at every hand-off and across a restart.
	boot := func() routedDecisions {
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
			supervisor: sup, project: "legion", now: func() time.Time { return now }}
		return routedDecisions{outbox: runner, now: func() time.Time { return now }, log: quietLogger()}
	}
	first := boot()
	if err := first.Decide(ctx, decision); err != nil {
		t.Fatal(err)
	}
	restarted := boot()
	if err := restarted.Decide(ctx, decision); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, pool); rows != 1 {
		t.Fatalf("outbox rows for the person's answer = %d; want one notice for it, from the loop or the relay", rows)
	}
	if err := restarted.outbox.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 {
		t.Fatalf("notices delivered = %+v; want exactly one: a person's answer to a stopped agent's ask reaches someone", delivered)
	}
	t.Logf("delivered %q to %q", delivered[0].message, delivered[0].topic)
}
