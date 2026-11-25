package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A person's spec decision that nobody inside its tree will hear goes to the controller: one
// parked-decision notice however often the undelivered row runs (its finish not committing),
// nothing to the architect, and the decision kept for the root's next run. Nobody hears it once the
// architect's claim has ended and its session holds the role no more: the close of its issue
// retired it (R24 of dispatch://INFRA-3320, the issue_close stop), or it failed on a running tree.
// An architect whose issue closed and whose close has not run yet has not ended: its notice waits
// for it while no session holds the role, as TestANoticeWaitsOrEndsByTheClaimsState holds, and is
// parked once the close retired it, which the first case holds.
func TestASpecDecisionNobodyInItsTreeHearsGoesToTheController(t *testing.T) {
	t.Parallel()
	carried := "a human approved LEGION-9's spec document at version 3, which opens the gate LEGION-1 carried; any READY it refused is released and any child waiting on it starts"
	for _, tc := range []struct {
		name   string
		state  supervise.ClaimState
		closed bool
	}{
		{"the close retired the architect", supervise.StateRetired, true},
		{"the architect's claim failed on a running tree", supervise.StateFailed, false},
	} {
		for _, decision := range []record.Notice{
			{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
			{Kind: "design-changes-requested", Version: 3, Reason: "name the rollback path", Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
			{Kind: "carried-gate-opened", Version: 3, Reason: carried, Actor: "user:alice", Ref: "dispatch://LEGION-9/spec"},
		} {
			t.Run(tc.name+"/"+string(decision.Kind), func(t *testing.T) {
				ctx := context.Background()
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				noticeTree(t, pool, records, tc.closed)
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				architectClaim(t, sup, "LEGION-1", tc.state)
				publisher := &holderPublisher{}
				publisher.setAbsent(architectTopic(t, "LEGION-1"))
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
				row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", decision, time.Now()))
				for attempt := range 2 {
					if err := runner.notice(ctx, row, decision); err != nil {
						t.Fatalf("attempt %d: %v", attempt, err)
					}
				}
				if _, delivered := publisher.snapshot(); len(delivered) != 0 {
					t.Fatalf("delivered %+v, want nothing to the architect", delivered)
				}
				kind, _ := record.SpecDecision(decision.Kind)
				want := record.ControllerNotice{Kind: record.ParkedDecisionNotice, Decision: kind, Root: "LEGION-1",
					Ref: decision.Ref, Actor: "user:alice", Version: 3, Reason: decision.Reason}
				if got := parkedDecisions(t, pool); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
					t.Fatalf("parked decisions %#v, want one %#v", got, want)
				}
				if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 1 || kept[0].Ref != decision.Ref || kept[0].Decision != kind || kept[0].Actor != "user:alice" {
					t.Fatalf("kept %#v, want the decision kept for LEGION-1's next admission", kept)
				}
			})
		}
	}
}

// A notice of the machinery to an architect the close of its issue retired (a refused round,
// here) ends undelivered with its one line: no person decided anything, so the controller is told
// nothing and nothing is kept for the root.
func TestTheMachinerysNoticeToARetiredArchitectWritesNoParkedDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateRetired)
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	notice := record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", notice, time.Now()))
	if err := runner.notice(ctx, row, notice); err != nil {
		t.Fatalf("notice: %v", err)
	}
	if got := parkedDecisions(t, pool); len(got) != 0 {
		t.Fatalf("parked decisions %#v, want none for the machinery's notice", got)
	}
	if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 0 {
		t.Fatalf("kept %#v, want nothing kept for the machinery's notice", kept)
	}
	if strings.Count(logged.String(), `msg="outbox notice finished undelivered: its architect will not hold its role again"`) != 1 ||
		strings.Contains(logged.String(), "parked decision") {
		t.Fatalf("log %q; want one finished-undelivered line and no parked decision line", logged.String())
	}
}

// A spec decision that names no person relays nothing: gates.design off opened the gate, or the
// release before this one queued the notice, which then carries no ref either. The log line names
// the notice's ref, empty for the old release's notice, which is how A5 tells the two apart.
func TestASpecDecisionThatNamesNoPersonIsNotRelayed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		notice record.Notice
		ref    string
	}{
		{"gates.design off opened the gate", record.Notice{Kind: "design-approved", Version: 3, Ref: "dispatch://LEGION-1/spec"}, "ref=dispatch://LEGION-1/spec"},
		{"the release before this one queued it", record.Notice{Kind: "design-approved", Version: 3}, `ref=""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, true)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateRetired)
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", tc.notice, time.Now()))
			if err := runner.notice(ctx, row, tc.notice); err != nil {
				t.Fatalf("notice: %v", err)
			}
			if got := parkedDecisions(t, pool); len(got) != 0 {
				t.Fatalf("parked decisions %#v, want none: no person made the decision", got)
			}
			if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 0 {
				t.Fatalf("kept %#v, want nothing kept", kept)
			}
			var line string
			for candidate := range strings.SplitSeq(logged.String(), "\n") {
				if strings.Contains(candidate, `msg="outbox parked decision not relayed: it names no person"`) {
					line = candidate
				}
			}
			if line == "" || !strings.Contains(line, tc.ref) || !strings.Contains(line, "root=LEGION-1") || !strings.Contains(line, "key=legion-outbox:") {
				t.Fatalf("log %q; want the not-relayed line naming %s, the root and the key", logged.String(), tc.ref)
			}
		})
	}
}

// Under a deployment that gates more than one step, a parked root keeps a decision for each
// document a person reviewed: the decisions document and the spec are two references, so the
// newest-per-reference rule the keep follows (record.Store.AddParkedDecision) keeps both rather
// than letting the second replace the first. The spec step's reference is the one every release
// before the step list wrote, so a one-step deployment keeps exactly what it kept before. The root
// is closed and its architect retired by the close, so nobody hears either; each row is finished
// once its executor ends it, as the runner finishes it, so the second is not held behind the first.
func TestGatedStepsKeepADecisionForEachDocumentAPersonReviewed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateRetired)
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: time.Now}
	decisions := record.Notice{Kind: "design-approved", Version: 2, Actor: "user:alice", Ref: "dispatch://LEGION-1/artifact/decisions-md"}
	spec := record.Notice{Kind: "design-approved", Version: 4, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"}
	for _, decision := range []record.Notice{decisions, spec} {
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", decision, time.Now()))
		if err := runner.notice(ctx, row, decision); err != nil {
			t.Fatalf("relay the approval of %s: %v", decision.Ref, err)
		}
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			return records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			t.Fatalf("finish the relayed approval of %s: %v", decision.Ref, err)
		}
	}
	kept := keptDecisions(t, pool, records, "LEGION-1")
	refs := make([]string, 0, len(kept))
	for _, decision := range kept {
		refs = append(refs, decision.Ref)
	}
	slices.Sort(refs)
	want := []string{"dispatch://LEGION-1/artifact/decisions-md", "dispatch://LEGION-1/spec"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("kept decisions %v, want one of each document: %v", refs, want)
	}
	told := parkedDecisions(t, pool)
	if len(told) != 2 || told[0].Ref != decisions.Ref || told[0].Version != 2 || told[1].Ref != spec.Ref || told[1].Version != 4 {
		t.Fatalf("parked-decision notices %#v, want one for each document at its own version", told)
	}
}

// A person's changes request that a newer one of the same document replaced is still the person's
// words, and the workflow tells it when no approval followed it — a late one, or one replayed from
// a message the intake gave up on, however late — but where nobody in its tree hears it, it relays
// nothing: the gate of the document records the newer request as the one told
// (record.DesignGate.ToldChangesVersion and ToldChangesReason), so the older notice reaching the
// controller would replace the newer decision kept under the document's ref, and the claim a start
// holds on it. Here alice asked for changes at version 3 ("cut the migration"), that request was
// relayed and kept, and a start claimed it; her earlier request at version 2 then reaches the root,
// whose architect the close of its issue retired, so nobody hears either (as
// TestASpecDecisionNobodyInItsTreeHearsGoesToTheController's first case). Each row is finished once
// its executor ends it, as the runner finishes it, so the second is not held behind the first. The
// newer decision stays kept, at its own time, and claimed, and the controller has the one notice.
func TestALateChangesRequestLeavesTheNewerParkedDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	told := 3
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepSpec, ArtifactID: "artifact-1",
			LatestVersion: 3, ToldChangesVersion: &told, ToldChangesReason: "cut the migration", ToldChangesMark: record.EventMark{Seq: 12}})
	}); err != nil {
		t.Fatalf("seed the gate the newer request was told on: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateRetired)
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
		supervisor: sup, project: "legion", now: time.Now}
	relay := func(notice record.Notice) {
		t.Helper()
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", notice, time.Now()))
		if err := runner.notice(ctx, row, notice); err != nil {
			t.Fatalf("relay the changes request at version %d: %v", notice.Version, err)
		}
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			return records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
		}); err != nil {
			t.Fatalf("finish the relayed changes request at version %d: %v", notice.Version, err)
		}
	}
	const ref = "dispatch://LEGION-1/spec"
	relay(record.Notice{Kind: "design-changes-requested", Version: 3, Reason: "cut the migration", Actor: "user:alice", Ref: ref})
	if _, err := pool.Exec(ctx, `update parked_decisions set claimed_by = (select max(id) from outbox) where issue = 'LEGION-1'`); err != nil {
		t.Fatalf("claim the kept decision: %v", err)
	}
	newer := keptDecisions(t, pool, records, "LEGION-1")
	if len(newer) != 1 || newer[0].ClaimedBy == 0 {
		t.Fatalf("kept before the late request = %+v, want the newer request kept and claimed", newer)
	}

	relay(record.Notice{Kind: "design-changes-requested", Version: 2, Reason: "name the rollback path", Actor: "user:alice", Ref: ref})

	if kept := keptDecisions(t, pool, records, "LEGION-1"); len(kept) != 1 || !kept[0].At.Equal(newer[0].At) || kept[0].ClaimedBy != newer[0].ClaimedBy {
		t.Fatalf("kept after the late request = %+v, want the newer %+v as it stood, claimed", kept, newer[0])
	}
	if told := parkedDecisions(t, pool); len(told) != 1 || told[0].Version != 3 {
		t.Fatalf("parked-decision notices = %+v, want the one of the request at version 3", told)
	}
}

// The parked-decision notice goes where every controller notice goes: the outbox's next run
// publishes it on the project's controller topic, as "parked-decision on <issue>", with the run and
// the phase entry of its issue its row was queued at (record.Fence).
func TestAParkedDecisionIsPublishedOnTheControllerTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, true)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateRetired)
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	decision := record.Notice{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", decision, time.Now()))
	for run := range 2 {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	_, delivered := publisher.snapshot()
	want := record.Notice{Kind: record.ParkedDecisionNotice, Decision: record.DecisionDesignApproved, Root: "LEGION-1", Ref: "dispatch://LEGION-1/spec", Actor: "user:alice", Version: 3,
		Generation: record.FirstFence.Generation, Entry: record.FirstFence.Entry}
	if len(delivered) != 1 || delivered[0].topic != notify.ControllerTopic("legion") || delivered[0].message != "parked-decision on LEGION-1" ||
		!reflect.DeepEqual(delivered[0].payload, want) {
		t.Fatalf("delivered %+v, want one %#v on %s as parked-decision on LEGION-1", delivered, want, notify.ControllerTopic("legion"))
	}
}

// A decision kept for a root is held by the delivery the claim queued the start's task as, and
// only then: a start that finishes without acting (here one of an earlier generation) leaves it
// KEPT for the next start to name, and a decision kept on a named reference since the naming is
// left behind by the hold, which names the row the start read, not the reference. The hold is the
// claim store's own write, so this runs through the daemon's own claim store.
func TestOnlyADeliveredStartHoldsTheDecisionsItNamed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	spec, ask, at := "dispatch://LEGION-1/spec", "dispatch://LEGION-1/ask/ask-1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, kept := range []record.ParkedDecision{
		{Issue: "LEGION-1", Ref: spec, Decision: record.DecisionDesignApproved, Actor: "user:alice", At: at},
		{Issue: "LEGION-1", Ref: ask, Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at},
	} {
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error { return records.AddParkedDecision(ctx, tx, kept) }); err != nil {
			t.Fatalf("keep the decision: %v", err)
		}
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = keptClaimStore{Store: outboxTreeStore(t, pool, issue.Key)}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	start := func(generation uint64, id int64) record.OutboxRow {
		row := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: generation,
			Task: "a person decided on the tree", Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: spec, At: at}, {Ref: ask, At: at}}}}, time.Now())
		row.ID = id
		return row
	}
	if err := runner.execute(ctx, start(1, 57)); err != nil {
		t.Fatalf("the start of an earlier generation: %v", err)
	}
	if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != 2 {
		t.Fatalf("kept after a start that finished without acting = %#v, want both decisions left for the next start", left)
	}
	// A person decides again on the reference the start named, before that start is delivered.
	newer := record.ParkedDecision{Issue: "LEGION-1", Ref: spec, Decision: record.DecisionDesignChangesRequested, Actor: "user:bob", At: at.Add(time.Hour)}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error { return records.AddParkedDecision(ctx, tx, newer) }); err != nil {
		t.Fatalf("keep the newer decision: %v", err)
	}
	if err := runner.execute(ctx, start(2, 58)); err != nil {
		t.Fatalf("the start of this generation: %v", err)
	}
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, found := sup.Machine(token)
	if !found {
		t.Fatal("the start created no architect claim")
	}
	if pending := machine.Claim().Pending; pending == nil || pending.ID != "outbox:58" {
		t.Fatalf("the architect's pending task = %#v, want the start's delivery", pending)
	}
	// The answer nothing replaced is held by the delivery, to be told at its confirmation; the
	// newer decision on the specification's reference, kept after the start named the older one,
	// is left KEPT for the next start or catch-up to name.
	left := keptDecisions(t, pool, records, "LEGION-1")
	if len(left) != 2 {
		t.Fatalf("kept after the delivered start = %#v, want both rows standing: the delivery holds what it names", left)
	}
	if left[0].Ref != spec || left[0].Actor != "user:bob" || !left[0].At.Equal(newer.At) || left[0].HeldBy.Claim != "" {
		t.Fatalf("the newest row = %#v, want the decision kept since the start named the older one, held by nothing", left[0])
	}
	if left[1].Ref != ask || left[1].HeldBy.Claim != string(token) || left[1].HeldBy.Delivery != "outbox:58" {
		t.Fatalf("the row the start named = %#v, want it held by the delivery the claim took", left[1])
	}
}

// A start row still queued keeps the decisions it claimed: a ready of the root's architect before
// that start is delivered (the start refused while another task is pending, a ready between its
// delivery and the claim's write, a crash in that gap) writes a catch-up that names none of them,
// and the start's delivery then holds them, so each is told once, at that delivery's confirmation,
// and no catch-up names them meanwhile.
func TestACatchUpLeavesOutWhatAQueuedStartClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, root)
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	answer := record.ParkedDecision{Issue: root.Key, Ref: "dispatch://LEGION-1/ask/ask-1", Decision: record.DecisionAskAnswered, Actor: "user:alice",
		At: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.AddParkedDecision(ctx, tx, answer); err != nil {
			return err
		}
		task, kept, err := engine.ParkedDecisionTask(ctx, tx, root)
		if err != nil {
			return err
		}
		return engine.EnqueueNaming(ctx, tx, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
			Generation: 1, Task: task, Kept: kept}, kept, time.Now())
	}); err != nil {
		t.Fatalf("queue the start naming the answer: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "ready-before-the-start", intake.ClaimReady{Issue: root.Key, Role: claim.RoleArchitect, Launch: 1}, engine); err != nil {
		t.Fatalf("apply the architect's ready: %v", err)
	}
	var decided string
	if err := pool.QueryRow(ctx, "select coalesce(payload->'catch_up'->>'decided', '') from outbox where kind = 'notice' and payload->>'kind' = 'catch-up'").Scan(&decided); err != nil {
		t.Fatalf("read the catch-up: %v", err)
	}
	if strings.Contains(decided, answer.Ref) {
		t.Fatalf("the catch-up names %s, which the queued start claims: %q", answer.Ref, decided)
	}
	var start record.OutboxRow
	if err := pool.QueryRow(ctx, "select id, kind, issue, payload from outbox where kind = 'supervise'").Scan(&start.ID, &start.Kind, &start.Issue, &start.Payload); err != nil {
		t.Fatalf("read the queued start: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = keptClaimStore{Store: outboxTreeStore(t, pool, root.Key)}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("deliver the start: %v", err)
	}
	left := keptDecisions(t, pool, records, root.Key)
	if len(left) != 1 || left[0].HeldBy.Delivery != fmt.Sprintf("%s%d", outboxDeliveryPrefix, start.ID) {
		t.Fatalf("kept after the start's delivery = %#v, want the one row held by that delivery", left)
	}
}

// A start that claimed a kept decision and then serves an earlier generation (its resume kept
// failing while the tree was parked and re-admitted again) finishes without acting, and its
// finish releases the claim. The new generation's start named nothing the old start claimed, and
// its architect's ready wrote a catch-up that left the claimed decision out too, so once the claim
// is released the decision must be handed to that running architect: some start or catch-up of
// the root names it. Otherwise it waits, kept and unclaimed, for the architect's next relaunch or
// the tree's next re-admission, while the architect works the tree without the person's decision.
func TestADecisionAStaleStartReleasesReachesTheRunningArchitect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 2, Status: "in_progress", HoldsGate: true}
	// Its copy row too: the catch-up the release hands the decision to is written on the chain the
	// copy shows.
	putChainIssue(t, pool, records, root)
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	answer := record.ParkedDecision{Issue: root.Key, Ref: "dispatch://LEGION-1/ask/ask-1", Decision: record.DecisionAskAnswered, Actor: "user:alice",
		At: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.AddParkedDecision(ctx, tx, answer); err != nil {
			return err
		}
		task, kept, err := engine.ParkedDecisionTask(ctx, tx, root)
		if err != nil {
			return err
		}
		return engine.EnqueueNaming(ctx, tx, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect,
			Generation: 2, Task: task, Kept: kept}, kept, time.Now())
	}); err != nil {
		t.Fatalf("queue generation 2's start naming the answer: %v", err)
	}
	var stale record.OutboxRow
	if err := pool.QueryRow(ctx, "select id, kind, issue, payload from outbox where kind = 'supervise'").Scan(&stale.ID, &stale.Kind, &stale.Issue, &stale.Payload); err != nil {
		t.Fatalf("read the queued start: %v", err)
	}
	// The tree is parked and re-admitted while that start waits out its backoff: generation 3's start
	// names nothing (the answer is claimed), and its architect comes up and is told its tree.
	root.Generation = 3
	putOutboxIssue(t, pool, records, root)
	if _, err := intake.ApplyFact(ctx, pool, "outbox", "generation-3-ready", intake.ClaimReady{Issue: root.Key, Role: claim.RoleArchitect, Launch: 1}, engine); err != nil {
		t.Fatalf("apply generation 3's architect ready: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	if err := runner.execute(ctx, stale); err != nil {
		t.Fatalf("run generation 2's start: %v", err)
	}
	// The lane finishes the row it ran (Store.FinishOutbox), which releases what it claimed.
	if _, err := pool.Exec(ctx, "delete from outbox where id = $1", stale.ID); err != nil {
		t.Fatalf("finish generation 2's start: %v", err)
	}
	if left := keptDecisions(t, pool, records, root.Key); len(left) != 1 {
		t.Fatalf("kept after the stale start finished = %#v, want the answer still kept", left)
	}
	var naming int
	if err := pool.QueryRow(ctx, "select count(*) from outbox where strpos(payload::text, $1) > 0", answer.Ref).Scan(&naming); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if naming == 0 {
		t.Fatalf("after the stale start released %s, no start or catch-up of %s names it: generation 3's running architect is never told the person's decision", answer.Ref, root.Key)
	}
}

// A start row whose task named kept decisions, delivered and then retired once the architect's
// turn on it ended, is not delivered again when the row runs again (a ClaimReady that failed after
// the forget, or a finish that did not commit): the delivered mark its forget wrote stops it, so the
// architect is told each decision once.
func TestARetriedStartDeliversItsTaskOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	ref, at := "dispatch://LEGION-1/ask/ask-1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: issue.Key, Ref: ref, Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at})
	}); err != nil {
		t.Fatalf("keep the decision: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleArchitect,
		State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the architect claim: %v", err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: 1,
		Task: "a person decided on the tree: " + ref, Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: ref, At: at}}}}, time.Now())
	start.ID = 71
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("deliver the start: %v", err)
	}
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	testwait.Eventually(t, "the start's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the task's turn: %v", err)
	}
	endTurn(ctx, t, machine, token)
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("the delivery after its turn ended = %#v, want it retired", pending)
	}
	prompts := len(conn.Prompts())
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("run the start row again: %v", err)
	}
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("the retried start queued %#v, want no second delivery", pending)
	}
	if got := len(conn.Prompts()) - prompts; got != 0 {
		t.Fatalf("the retried start prompted the architect %d more times, want none", got)
	}
}

// A start row whose task named kept decisions is delivered, and the row runs again: its finish was
// lost (a crash, or a settlement that did not commit). If the architect's turn on that task ends
// and the delivery is retired before the retry, the retry must not deliver the task again: the
// architect was told each decision once already, and the confirmation of that turn told it.
// (Before decisions were held by the delivery, the injected failure was the start's own forget;
// a delivered start now forgets nothing, so the row's lost finish is the failure that remains.)
func TestAStartWhoseForgetFailedIsNotDeliveredAgainAfterItsTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-1", Title: "Root", Phase: phase.Admitted, Generation: 1, Status: "in_progress"}
	putOutboxIssue(t, pool, records, issue)
	ref, at := "dispatch://LEGION-1/ask/ask-1", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: issue.Key, Ref: ref, Decision: record.DecisionAskAnswered, Actor: "user:alice", At: at})
	}); err != nil {
		t.Fatalf("keep the decision: %v", err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = keptClaimStore{Store: outboxTreeStore(t, pool, issue.Key)}
	token, err := claim.NewToken("legion", issue.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: token, Project: "legion", Tree: issue.Tree, Issue: issue.Key, Role: claim.RoleArchitect,
		State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the architect claim: %v", err)
	}
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(token, conn)
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-1"}, nil
		},
	}
	start := mustOutboxRow(t, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleArchitect, Generation: 1,
		Task: "a person decided on the tree: " + ref, Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: ref, At: at}}}}, time.Now())
	start.ID = 72
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("the start's first run: %v", err)
	}
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	testwait.Eventually(t, "the start's task to be acknowledged", func() bool {
		p := machine.Claim().Pending
		return p != nil && !p.DeliveredAt.IsZero()
	})
	if err := machine.Handle(ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("start the task's turn: %v", err)
	}
	endTurn(ctx, t, machine, token)
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("the delivery after its turn ended = %#v, want it retired", pending)
	}
	prompts := len(conn.Prompts())
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("retry the start row: %v", err)
	}
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil {
		t.Fatalf("the retried start queued %#v again after the architect's turn on it ended; want each decision told once", pending)
	}
	if got := len(conn.Prompts()) - prompts; got != 0 {
		t.Fatalf("the retried start prompted the architect %d more times, want none", got)
	}
	if left := keptDecisions(t, pool, records, issue.Key); len(left) != 0 {
		t.Fatalf("kept after the retried start = %#v, want the told decision forgotten", left)
	}
}

// A catch-up forgets the decisions it named when it is published to its architect, and leaves them
// when it finishes undelivered, so the next catch-up names them again.
func TestOnlyADeliveredCatchUpForgetsTheDecisionsItNamed(t *testing.T) {
	t.Parallel()
	spec, at := "dispatch://LEGION-1/spec", time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		state     supervise.ClaimState
		absent    bool
		stillKept int
	}{
		{"published to its architect", supervise.StateWorking, false, 0},
		{"finished undelivered: its architect will not hold its role again", supervise.StateFailed, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: "LEGION-1", Ref: spec,
					Decision: record.DecisionDesignApproved, Actor: "user:alice", At: at})
			}); err != nil {
				t.Fatalf("keep the decision: %v", err)
			}
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", tc.state)
			publisher := &holderPublisher{}
			if tc.absent {
				publisher.setAbsent(architectTopic(t, "LEGION-1"))
			}
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			catchUp := record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: "the tree of LEGION-1", CatchUp: &record.CatchUp{
				Generation: 1, Launch: 1, Decided: "a person decided on the tree",
				Kept: &record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: spec, At: at}}}}}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", catchUp, time.Now()))
			if err := runner.notice(ctx, row, catchUp); err != nil {
				t.Fatalf("run the catch-up: %v", err)
			}
			if left := keptDecisions(t, pool, records, "LEGION-1"); len(left) != tc.stillKept {
				t.Fatalf("kept after the catch-up %s = %#v, want %d", tc.name, left, tc.stillKept)
			}
		})
	}
}

// A re-admission's start that finds the root's architect still running (its claim was mid-launch
// when the linger's suspend ran) both delivers its task, which names the decisions kept for the
// root, and tells the running architect its tree as a ready would (the start-running catch-up).
// The architect is told each kept decision once: the catch-up does not name again what the task it
// rides beside already names, or the architect reads one person's decision as two and may act on
// it twice.
func TestAStartThatFindsTheRootArchitectRunningNamesEachKeptDecisionOnce(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208",
		Title: "Workflow", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, root)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	// The hold of what a start's task names is the claim store's own write, so this runs through
	// the daemon's own claim store: the catch-up the start applies must find those rows held.
	sup.deps.Store = keptClaimStore{Store: outboxTreeStore(t, pool, root.Key)}
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	admission := admit.New(records, engine, 2, "legion", quietLogger())
	runner := &outbox{
		dispatchProject: "LEGION",
		pool:            pool, records: records, supervisor: sup, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(),
		handlers: []intake.Handler{engine, admission}, now: time.Now, log: quietLogger(),
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	if err := runner.execute(ctx, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: 1}, time.Now())); err != nil {
		t.Fatalf("start the architect: %v", err)
	}
	token, err := claim.NewToken("legion", root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	machine, _ := sup.Machine(token)
	launch := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: launch},
		supervise.RequestRegister{Claim: token, Generation: launch, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: token, Generation: launch, Session: "ses-architect"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	if _, err := pool.Exec(ctx, "delete from outbox"); err != nil {
		t.Fatalf("deliver the generation 1 notices: %v", err)
	}

	// A person answered on the parked tree; the re-admission at generation 2 names it in the start's
	// task, as admission writes it (workflow.Engine.ParkedDecisionTask), while the architect still runs.
	answer := record.ParkedDecision{Issue: root.Key, Ref: "dispatch://LEGION-208/ask/ask-1", Decision: record.DecisionAskAnswered, Actor: "user:alice",
		At: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	root.Generation = 2
	putOutboxIssue(t, pool, records, root)
	var task string
	var kept *record.KeptDecisions
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := records.AddParkedDecision(ctx, tx, answer); err != nil {
			return err
		}
		var err error
		task, kept, err = engine.ParkedDecisionTask(ctx, tx, root)
		return err
	}); err != nil {
		t.Fatalf("keep the answer and name it: %v", err)
	}
	if kept == nil || !strings.Contains(task, answer.Ref) {
		t.Fatalf("the start task = %q naming %#v, want it to name %s", task, kept, answer.Ref)
	}
	start := mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree, Role: claim.RoleArchitect, Generation: 2, Task: task, Kept: kept}, time.Now())
	if err := runner.execute(ctx, start); err != nil {
		t.Fatalf("start the running architect for generation 2: %v", err)
	}
	if pending := machine.Claim().Pending; pending == nil || !strings.Contains(pending.Task, answer.Ref) {
		t.Fatalf("the architect's pending task = %#v, want the start's task naming %s", pending, answer.Ref)
	}
	rows, err := pool.Query(ctx, "select coalesce(payload->'catch_up'->>'decided', '') from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id")
	if err != nil {
		t.Fatalf("read the catch-ups: %v", err)
	}
	decided, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the catch-ups: %v", err)
	}
	if len(decided) != 1 {
		t.Fatalf("catch-ups after the generation 2 start = %q, want the one the running architect is told its tree by", decided)
	}
	if strings.Contains(decided[0], answer.Ref) {
		t.Fatalf("the catch-up names %s again beside the start task that names it: %q; want each kept decision told once", answer.Ref, decided[0])
	}
}

// parkedDecisions is every parked-decision controller notice in the outbox, oldest first.
func parkedDecisions(t *testing.T, pool *store.Pool) []record.ControllerNotice {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox
		where kind = $1 and payload->>'kind' = $2 order by id`, string(record.OutboxKindControllerNotice), string(record.ParkedDecisionNotice))
	if err != nil {
		t.Fatalf("read the parked decisions: %v", err)
	}
	defer rows.Close()
	var got []record.ControllerNotice
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		// The run and the phase entry a parked-decision notice names are its row's fence
		// (record.Fence), not what it tells the controller.
		notice := payload.(record.ControllerNotice)
		notice.Generation, notice.Entry = 0, 0
		got = append(got, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the parked decisions: %v", err)
	}
	return got
}

// keptDecisions is every decision kept for root's architect now: of the issues the Dispatch chain
// shows owed to it (record.KeptMembers, Store.ParkedDecisionsOf).
func keptDecisions(t *testing.T, pool *store.Pool, records record.Store, root string) []record.ParkedDecision {
	t.Helper()
	ctx := context.Background()
	var kept []record.ParkedDecision
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		members, err := record.KeptMembers(ctx, tx, records, root)
		if err != nil {
			return err
		}
		kept, err = records.ParkedDecisionsOf(ctx, tx, members)
		return err
	}); err != nil {
		t.Fatalf("read the kept decisions of %s: %v", root, err)
	}
	return kept
}
