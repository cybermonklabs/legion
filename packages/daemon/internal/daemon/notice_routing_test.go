package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// holderPublisher is the listener as a role publish meets it: a role topic in absent has no live
// holder, and the publish is refused as the listener refuses it (notify.ErrNoHolder); any other
// publish is delivered, and recorded in order with the holder it was stored for. holder names the
// session that holds every role topic, the session the listener's 200 names
// (notify.Publisher.Publish); nil names none, as a listener that does not say. keeps is whether
// the listener keeps a kept publish (PublishKept) for a claim whose holder is not live, as the
// listener of cybermonklabs/legion#4079 does: with it, a kept publish to a topic in absent is kept
// for the role's next holder and recorded in kept, and without it that publish is refused as a
// keep: false publish is, which is what a listener before #4079 answers a claim whose holder is
// gone.
type holderPublisher struct {
	mu        sync.Mutex
	absent    map[string]bool
	holder    func() string
	keeps     bool
	attempts  int
	delivered []outboxPublish
	kept      []outboxPublish
}

func (p *holderPublisher) Publish(_ context.Context, topic, message string, payload any, key string) (string, error) {
	published := outboxPublish{topic: topic, message: message, key: key, payload: payload}
	_, err := p.answer(&published)
	return published.holder, err
}

func (p *holderPublisher) PublishKept(_ context.Context, topic, message string, payload any, key string) (bool, error) {
	return p.answer(&outboxPublish{topic: topic, message: message, key: key, payload: payload, keep: true})
}

// answer is the one body of both publishes: whether the listener kept the message, and its
// refusal. A delivered publish carries the holder it was stored for.
func (p *holderPublisher) answer(published *outboxPublish) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if p.absent[published.topic] {
		if published.keep && p.keeps {
			p.kept = append(p.kept, *published)
			return true, nil
		}
		return false, fmt.Errorf("publish notice to %s: listener returned 404 Not Found: {\"reason\":\"unclaimed\"}: %w", published.topic, notify.ErrNoHolder)
	}
	if p.holder != nil {
		published.holder = p.holder()
	}
	p.delivered = append(p.delivered, *published)
	return false, nil
}

// holdBy has session hold every role topic from now on, as the listener resolves the role.
func (p *holderPublisher) holdBy(session func() string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.holder = session
}

func (p *holderPublisher) setAbsent(topics ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.absent = map[string]bool{}
	for _, topic := range topics {
		p.absent[topic] = true
	}
}

// architectTopic is the role topic of issue's architect claim.
func architectTopic(t *testing.T, issue string) string {
	t.Helper()
	return notify.RoleTopicPrefix + string(mustClaimToken(t, issue, claim.RoleArchitect))
}

// deliveredKinds is each delivered notice as "<kind> to <architect's issue>", in order.
func deliveredKinds(t *testing.T, delivered []outboxPublish) []string {
	t.Helper()
	var got []string
	for _, published := range delivered {
		owner := "?"
		for _, issue := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
			if published.topic == architectTopic(t, issue) {
				owner = issue
			}
		}
		got = append(got, string(published.payload.(record.Notice).Kind)+" to "+owner)
	}
	return got
}

func (p *holderPublisher) snapshot() (int, []outboxPublish) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts, append([]outboxPublish(nil), p.delivered...)
}

// keptCopies is every message the listener's kept lane holds for a role's next holder, in order.
func (p *holderPublisher) keptCopies() []outboxPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]outboxPublish(nil), p.kept...)
}

// noticeTree records a root, its child and the child's child, all of tree LEGION-1.
func noticeTree(t *testing.T, pool *store.Pool, records record.Store, lingers bool) {
	t.Helper()
	root, child := "LEGION-1", "LEGION-2"
	rootIssue := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
	if lingers {
		until := time.Now().Add(time.Hour)
		rootIssue.Phase, rootIssue.Status, rootIssue.LingerUntil = phase.Done, "done", &until
	}
	putOutboxIssue(t, pool, records, rootIssue)
	putOutboxIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &root, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-3", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &child, Title: "Grandchild", Phase: phase.Testing, Generation: 1, Status: "testing"})
}

func architectClaim(t *testing.T, sup *supervisor, issue string, state supervise.ClaimState) claim.Token {
	t.Helper()
	token := mustClaimToken(t, issue, claim.RoleArchitect)
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: token, Project: "legion", Tree: "LEGION-1", Issue: issue, Role: claim.RoleArchitect, State: state,
	}, ""); err != nil {
		t.Fatalf("create the architect claim of %s: %v", issue, err)
	}
	return token
}

// leasedOutboxRow enqueues row and leases it as the runner does before it executes a row, and
// returns the row as the runner then holds it: the notice executor publishes only a row it finds
// under its lease.
func leasedOutboxRow(t *testing.T, pool *store.Pool, records record.Store, row record.OutboxRow) record.OutboxRow {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		row.ID, err = records.EnqueueID(ctx, tx, row)
		return err
	}); err != nil {
		t.Fatalf("enqueue the row: %v", err)
	}
	row.LeaseToken = leaseOutboxRow(t, pool, row.ID)
	return row
}

// leaseOutboxRow leases outbox row id for a minute and returns its lease token.
func leaseOutboxRow(t *testing.T, pool *store.Pool, id int64) string {
	t.Helper()
	token := fmt.Sprintf("lease-%d", id)
	if _, err := pool.Exec(context.Background(), "update outbox set lease_token = $2, lease_until = now() + interval '1 minute' where id = $1", id, token); err != nil {
		t.Fatalf("lease outbox row %d: %v", id, err)
	}
	return token
}

func mustClaimToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("claim token of %s %s: %v", issue, role, err)
	}
	return token
}

// Every notice but a catch-up goes to the architect that owns its issue, by the TypeScript daemon's
// rule (record.OwningArchitect): the nearest issue at or above it whose architect claim holds its
// role (not failed or retired, and not suspended except by the idle release), else the tree root;
// a child's close or status change, and a worker-died or worker-stalled about a child's architect,
// start at its parent. A child's start is told with a child-status, whose walk starts at its
// parent, so the architect above the child hears it even while the child's own architect is
// launching. The phase-finished of a root's gate opening, the one that names no role, starts at
// the root, whose own architect hears it. A catch-up goes to the architect of its own issue alone
// (noticeArchitect). The walk stays inside the issue's tree: admission records a Dispatch
// re-parent as it comes and leaves the issue's tree as it was, so a parent that is not an issue of
// that tree — not recorded, or recorded in another tree — ends the walk at the tree's root. The
// runs are the outbox's own (holdsRole), over the claim's state alone. A case that names no kind
// is a planner's phase-finished.
func TestTheArchitectThatOwnsANotice(t *testing.T) {
	t.Parallel()
	root, child, unrecorded, elsewhere := "LEGION-1", "LEGION-2", "LEGION-77", "LEGION-50"
	tree := map[string]record.Issue{
		root:       {Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root},
		child:      {Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root},
		"LEGION-3": {Key: "LEGION-3", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &child},
		"LEGION-4": {Key: "LEGION-4", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &unrecorded},
		"LEGION-5": {Key: "LEGION-5", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &elsewhere},
	}
	for _, tc := range []struct {
		name         string
		issue        string
		kind         record.NoticeKind
		subArchitect supervise.ClaimState
		owner        string
	}{
		{name: "a child's notice with no sub-architect", issue: "LEGION-2", owner: "LEGION-1"},
		{name: "the root's own notice", issue: "LEGION-1", owner: "LEGION-1"},
		{name: "a child owned by its sub-architect", issue: "LEGION-2", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a child whose sub-architect is launching", issue: "LEGION-2", subArchitect: supervise.StateLaunching, owner: "LEGION-2"},
		{name: "a grandchild under the sub-architect's child", issue: "LEGION-3", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a child whose sub-architect retired", issue: "LEGION-2", subArchitect: supervise.StateRetired, owner: "LEGION-1"},
		{name: "a child whose sub-architect failed", issue: "LEGION-2", subArchitect: supervise.StateFailed, owner: "LEGION-1"},
		{name: "a child whose sub-architect was suspended", issue: "LEGION-2", subArchitect: supervise.StateSuspended, owner: "LEGION-1"},
		{name: "a child's close under its running sub-architect", issue: "LEGION-2", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a child's status change under its running sub-architect", issue: "LEGION-2", kind: "child-status", subArchitect: supervise.StateWorking, owner: "LEGION-1"},
		{name: "a child's start under its launching sub-architect", issue: "LEGION-2", kind: "child-status", subArchitect: supervise.StateLaunching, owner: "LEGION-1"},
		{name: "a grandchild's start under the sub-architect's child", issue: "LEGION-3", kind: "child-status", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "a grandchild's close under the sub-architect's child", issue: "LEGION-3", kind: "child-closed", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "the root's own close", issue: "LEGION-1", kind: "child-closed", owner: "LEGION-1"},
		{name: "a child whose parent is not recorded", issue: "LEGION-4", owner: "LEGION-1"},
		{name: "a child whose parent is in another tree", issue: "LEGION-5", owner: "LEGION-1"},
		{name: "the close of a child whose parent is not recorded", issue: "LEGION-4", kind: "child-closed", owner: "LEGION-1"},
		{name: "the close of a child whose parent is in another tree", issue: "LEGION-5", kind: "child-status", owner: "LEGION-1"},
		// A resting-root notice is keyed by the ancestor (dispatch://INFRA-3320, R8), so an
		// ancestor that is a member of a tree is told through its sub-architect where one runs.
		{name: "an ancestor's resting-root notice under its running sub-architect", issue: "LEGION-2", kind: "resting-root", subArchitect: supervise.StateWorking, owner: "LEGION-2"},
		{name: "an ancestor's resting-root notice with no sub-architect", issue: "LEGION-2", kind: "resting-root", owner: "LEGION-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := map[claim.Token]supervise.ClaimState{
				mustClaimToken(t, root, claim.RoleArchitect):  supervise.StateWorking,
				mustClaimToken(t, child, claim.RoleArchitect): tc.subArchitect,
			}
			kind, role := tc.kind, claim.Role("")
			switch kind {
			case "":
				kind, role = "phase-finished", claim.RolePlanner
			}
			got, err := record.OwningArchitect("legion", tree, tree[tc.issue], record.NoticeStartsAtParent(kind, role), func(token claim.Token) bool {
				return holdsRole(supervise.ClaimView{Claim: supervise.Claim{State: states[token]}})
			})
			if want := mustClaimToken(t, tc.owner, claim.RoleArchitect); err != nil || got != want {
				t.Fatalf("owner = %s, %v; want %s", got, err, want)
			}
		})
	}
}

// The executor publishes a notice to its owner's role topic and to nothing else: no issue topic,
// since every issue topic is a subject that issue's phase workers subscribe to, and no worker's role
// topic.
func TestANoticeGoesToTheOwningArchitectsRoleTopicAlone(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	publisher := &holderPublisher{}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))

	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	if err := runner.execute(context.Background(), row); err != nil {
		t.Fatalf("execute notice: %v", err)
	}
	want := architectTopic(t, "LEGION-2")
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || delivered[0].topic != want || delivered[0].key != record.OutboxKey(row.ID) {
		t.Fatalf("notice publishes = %+v, want one to %s under the row's key", delivered, want)
	}
	for _, issue := range []string{"LEGION-1", "LEGION-2", "LEGION-3"} {
		subjects := []string{"notifications.legion.legion." + issue}
		for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			subjects = append(subjects, notify.RoleTopicPrefix+string(mustClaimToken(t, issue, role)))
		}
		if slices.Contains(subjects, delivered[0].topic) {
			t.Fatalf("the notice went to %s, a subject a phase worker of %s holds", delivered[0].topic, issue)
		}
	}
}

// The outbox routes a notice without waiting on its architect's launch: the runner executes one
// row at a time, and a machine holds its lock through the runtime call its decision makes, so a
// read of the architect's claim that waited would hold every row behind it for as long as a
// Sandbox relaunch waits on the cluster (LEGION-650). The architect is launching, so the notice
// goes to its role topic, as it would once the launch returned. The publish goes through the
// outbox's hold gate (outbox.publish, supervise.Machine.HoldsItsSession), which must not wait on
// the launch either (review round 13 of #4220); its control, the gate reading the hold under the
// machine's lock, fails here with "the notice waited on its architect's launch in the runtime".
func TestANoticeIsRoutedWhileItsArchitectLaunches(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, fakeRuntime := newOutboxSupervisor(t, "legion", t.TempDir())
	rt := newStallingRuntime(t, fakeRuntime, false)
	sup.deps.Runtime = rt
	architect := architectClaim(t, sup, "LEGION-1", supervise.StateQueued)
	machine, _ := sup.Machine(architect)
	rt.stalling.Store(true)
	go func() { _ = machine.Handle(context.Background(), supervise.RequestSpawn{Claim: architect}) }()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the architect's launch never reached the runtime")
	}
	publisher := &holderPublisher{}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}, time.Now()))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}

	executed := make(chan error, 1)
	go func() { executed <- runner.execute(context.Background(), row) }()
	select {
	case err := <-executed:
		if err != nil {
			t.Fatalf("execute the notice: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notice waited on its architect's launch in the runtime")
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != architectTopic(t, "LEGION-1") {
		t.Fatalf("notice publishes = %+v, want one to the launching architect's role topic", delivered)
	}
}

// A sub-architect whose suspension by the daemon is in the runtime holds its role no longer: a
// notice that would reach it (here a notice of its issue's child, which has no architect of its
// own) goes up to the architect above it, here the tree root's, as it would once the suspension
// returned, never to the session being stopped. The routing reads the claim without waiting on
// the suspension, so it reads the stop in flight from the machine's View
// (supervise.ClaimView.Stopping) (LEGION-650).
func TestANoticeIsNotRoutedToASubArchitectBeingSuspended(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, fakeRuntime := newOutboxSupervisor(t, "legion", t.TempDir())
	rt := newStallingRuntime(t, fakeRuntime, false)
	sup.deps.Runtime = rt
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	sub := architectClaim(t, sup, "LEGION-2", supervise.StateQueued)
	sup.deps.Conns.(*fake.Conns).Register(sub, fake.NewConn())
	machine, _ := sup.Machine(sub)
	ctx := context.Background()
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: sub}); err != nil {
		t.Fatalf("spawn the sub-architect: %v", err)
	}
	generation := machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: sub, Generation: generation},
		supervise.RequestRegister{Claim: sub, Generation: generation, Session: "ses-sub", SessionFile: "/tmp/sub.jsonl"},
		supervise.RequestReady{Claim: sub, Generation: generation, Session: "ses-sub"},
	} {
		if err := machine.Handle(ctx, ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	rt.stallsSuspend.Store(true)
	go func() {
		_ = machine.Handle(ctx, supervise.RequestSuspend{Claim: sub, Reason: "its issue left its phase", By: supervise.SuspendedByDaemon})
	}()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the sub-architect's suspension never reached the runtime")
	}
	publisher := &holderPublisher{}
	row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion"}
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("execute the notice: %v", err)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != architectTopic(t, "LEGION-1") {
		t.Fatalf("notice publishes = %+v, want one to the root's role topic, none to the sub-architect being suspended", delivered)
	}
}

// A role publish with no live holder is the ordinary state while the owning architect relaunches:
// its claim has not ended, so the notice is held and retried on the outbox's backoff, logged once
// for the row rather than on every attempt. The notices behind it for the same architect wait too,
// so once the architect holds its role again every held notice arrives, in the order it was
// written.
func TestANoticeHeldForAnAbsentArchitectArrivesInOrderOnceItReturns(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	first := mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now())
	second := mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, time.Now())
	third := mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now())
	for _, row := range []record.OutboxRow{first, second, third} {
		enqueueOutbox(t, pool, records, row)
	}
	var logged bytes.Buffer
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}

	for tick := range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	attempts, delivered := publisher.snapshot()
	if len(delivered) != 0 || attempts != 3 || outboxRows(t, pool) != 3 {
		t.Fatalf("while nobody holds the architect's role: %d attempts, delivered %+v, %d rows; want the first notice tried each tick, none delivered, all three kept",
			attempts, delivered, outboxRows(t, pool))
	}
	for _, row := range []int{1, 2, 3} {
		if got := strings.Count(logged.String(), fmt.Sprintf("msg=\"outbox row waits\" row=%d kind=notice ", row)); got != 1 {
			t.Fatalf("held notice row %d was logged %d times over 3 ticks, want once:\n%s", row, got, logged.String())
		}
	}
	// The held row's error carries the listener's refusal, which is what the log line and the row's
	// last_error show an operator.
	var lastError string
	if err := pool.QueryRow(context.Background(), "select id, last_error from outbox where "+notStateRows+" order by id limit 1").Scan(&first.ID, &lastError); err != nil {
		t.Fatalf("read the first held row: %v", err)
	}
	first.LeaseToken = leaseOutboxRow(t, pool, first.ID)
	if held := runner.execute(context.Background(), first); !errors.Is(held, errNoticeWaits) || !errors.Is(held, notify.ErrNoHolder) ||
		!strings.Contains(lastError, `"reason":"unclaimed"`) {
		t.Fatalf("held error %v, last_error %q: want a wait that carries the listener's no-holder refusal", held, lastError)
	}

	publisher.setAbsent()
	for tick := range 3 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d after the architect returned: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	_, delivered = publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[phase-finished to LEGION-1 pr-blocked to LEGION-1 design-approved to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after the architect returned: delivered %v with %d rows left, want the three notices in the order written and none left", got, outboxRows(t, pool))
	}
}

// The order held is each architect's own: a notice held for the root architect while it relaunches
// does not hold a later notice for a sub-architect that is running, which is delivered at once.
func TestANoticeHeldForOneArchitectHoldsNoOtherArchitectsNotice(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, time.Now()))
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the notices: %v", err)
	}
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[pr-blocked to LEGION-2]" || outboxRows(t, pool) != 1 {
		t.Fatalf("delivered %v with %d rows left, want the sub-architect's pr-blocked delivered and the root's design-approved still held", got, outboxRows(t, pool))
	}
}

// A sub-architect the workflow suspended (a person moved its child out of the workflow) is not
// coming back for a notice: only its child's next first phase start, after a todo (a person's, or
// the rerun_child of the architect above), or an operator's resume or delivery starts it again. A
// notice that would reach it goes to the architect above instead, and so does the child's close,
// which starts at the parent; neither waits on the suspended claim, and neither holds up the
// tree's later notices.
func TestNoNoticeWaitsOnASuspendedSubArchitect(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-2", supervise.StateSuspended)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}, time.Now()))
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-2"))
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}

	for tick := range 20 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	_, delivered := publisher.snapshot()
	if got := deliveredKinds(t, delivered); fmt.Sprint(got) != "[phase-finished to LEGION-1 child-closed to LEGION-1 phase-finished to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after 20 ticks over 40 minutes: delivered %v with %d rows left, want all three at the root architect and none left", got, outboxRows(t, pool))
	}
}

// A notice whose owning architect has ended waits on nobody: a claim that retired or failed comes
// back only through a new start (an operator's retry, a re-admission, or for a child's architect
// its child's next phase start), whose ready queues a catch-up of its own, and a lingering tree's
// architect is suspended until re-admission, which starts it with the tree's record rather than
// the notices of its close. Such a row finishes without delivery, with one log line naming the
// notice kind and the issue. A resting-root notice (dispatch://INFRA-3320, R8) is keyed by the
// ancestor whose approval a root rests on, here LEGION-1, the root of its own tree: with that
// tree's architect claim absent, retired or failed, it finishes the same way, and the ancestor's
// next catch-up names the root instead (workflow's TestRestingRootsHoldTheAncestorSignOff).
func TestANoticeForAnEndedArchitectFinishesWithoutDelivery(t *testing.T) {
	t.Parallel()
	childClosed := record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}
	restingRoot := record.Notice{Kind: "resting-root", Role: claim.RoleArchitect, Version: 3, Ref: "dispatch://LEGION-1/spec",
		Reason: "An ancestor's approval opened the spec step of LEGION-208. A person approved version 3 of the primary document of LEGION-1. " +
			"LEGION-208 rests on this approval. The checks of LEGION-208 serve these requirements of LEGION-1: dispatch://LEGION-1/spec#b-p2@v3."}
	for _, tc := range []struct {
		name string
		// state is the state of LEGION-1's architect claim, "" for no claim at all.
		state   supervise.ClaimState
		lingers bool
		issue   string
		notice  record.Notice
	}{
		{name: "the architect's claim retired", state: supervise.StateRetired, issue: "LEGION-2", notice: childClosed},
		{name: "the architect's claim failed", state: supervise.StateFailed, issue: "LEGION-2", notice: childClosed},
		// The linger's suspend of the architect is an outbox row of its own, which may not have run.
		{name: "the tree lingers before its architect's suspend ran", state: supervise.StateWorking, lingers: true, issue: "LEGION-2", notice: childClosed},
		{name: "an ancestor's resting-root notice with no architect claim", issue: "LEGION-1", notice: restingRoot},
		{name: "an ancestor's resting-root notice, its architect's claim retired", state: supervise.StateRetired, issue: "LEGION-1", notice: restingRoot},
		{name: "an ancestor's resting-root notice, its architect's claim failed", state: supervise.StateFailed, issue: "LEGION-1", notice: restingRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, tc.lingers)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			if tc.state != "" {
				architectClaim(t, sup, "LEGION-1", tc.state)
			}
			enqueueOutbox(t, pool, records, mustOutboxRow(t, tc.issue, tc.notice, time.Now()))
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
				project: "legion", now: time.Now}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the notice: %v", err)
			}
			_, delivered := publisher.snapshot()
			lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
			kind, issue := "kind="+string(tc.notice.Kind), "issue="+tc.issue
			if len(delivered) != 0 || outboxRows(t, pool) != 0 || len(lines) != 1 || !strings.Contains(lines[0], kind) || !strings.Contains(lines[0], issue) {
				t.Fatalf("delivered %+v, %d rows left, log %q; want no delivery, the row finished, one line naming %s and %s", delivered, outboxRows(t, pool),
					logged.String(), tc.notice.Kind, tc.issue)
			}
			t.Logf("finished undelivered: %s", lines[0])
		})
	}
}

// A task-stopped notice of an owner's stop_task names the architect that made the call
// (record.Notice.StoppedBy), and that architect is answered by its own call: the executor
// finishes the row without publishing when the architect the notice's issue reaches is the one
// that stopped the task, with one log line, and publishes it to that architect when it is
// another claim. The route takes the stop from any architect of the issue's tree, so the two
// differ in both directions: the root's architect stopping a child whose sub-architect runs, and
// the child's own sub-architect stopping it.
func TestATaskStoppedNoticeReachesNoArchitectThatStoppedTheTask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, issue, stoppedBy string
		want                   []string
	}{
		{name: "a root's task stopped by the root's architect", issue: "LEGION-1", stoppedBy: "LEGION-1"},
		{name: "a child's task stopped by the root's architect, its sub-architect running", issue: "LEGION-2", stoppedBy: "LEGION-1",
			want: []string{"task-stopped to LEGION-2"}},
		{name: "a child's task stopped by its own sub-architect", issue: "LEGION-2", stoppedBy: "LEGION-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
			architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
			by := mustClaimToken(t, tc.stoppedBy, claim.RoleArchitect)
			why := "task a of " + tc.issue + " was stopped by its owner"
			enqueueOutbox(t, pool, records, mustOutboxRow(t, tc.issue, record.Notice{Kind: "task-stopped", Role: claim.RoleTester, Phase: phase.Testing,
				StoppedBy: by, Reason: why + ", the architect " + string(by)}, time.Now()))
			var logged bytes.Buffer
			publisher := &holderPublisher{}
			runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
				project: "legion", now: time.Now}

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the notice: %v", err)
			}
			_, delivered := publisher.snapshot()
			if got := deliveredKinds(t, delivered); !slices.Equal(got, tc.want) || outboxRows(t, pool) != 0 {
				t.Fatalf("delivered %v with %d rows left, want %v and the row finished", got, outboxRows(t, pool), tc.want)
			}
			if tc.want == nil && !strings.Contains(logged.String(), "its architect stopped the task itself") {
				t.Fatalf("log %q, want the line that the row finished because its architect stopped the task itself", logged.String())
			}
		})
	}
}

// A root architect the operator suspended outside a linger comes back when the operator resumes it
// (`legion claims resume` or `deliver`), so its notices are held for it, not finished: the row waits,
// logged once, and is delivered once the architect holds its role again. The fence is per
// architect, so the held rows hold no other architect's notices.
func TestANoticeForAnOperatorSuspendedRootIsHeldUntilItHoldsItsRoleAgain(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateSuspended)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "review_round_cap"}, time.Now()))
	var logged bytes.Buffer
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}

	for tick := range 2 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(2 * time.Minute)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 1 || strings.Count(logged.String(), "msg=\"outbox row waits\" row=1 kind=notice ") != 1 {
		t.Fatalf("while the suspended root holds no role: delivered %+v, %d rows, log %q; want the row held, logged once", delivered, outboxRows(t, pool), logged.String())
	}
	publisher.setAbsent()
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run after the root holds its role again: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[pr-blocked to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("after the root holds its role again: delivered %v, %d rows left; want the held pr-blocked delivered", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// An earlier notice's architect is found as its own kind routes it: a child's close starts at the
// parent. A close held for the absent root architect therefore holds the root's later notices, even
// though the closed child has a running sub-architect of its own, and when the root returns the
// close arrives first, though the later notice falls due before the held close does.
func TestAHeldChildCloseHoldsTheArchitectsLaterNotices(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateLaunching)
	architectClaim(t, sup, "LEGION-2", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	clock := time.Now()
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, "LEGION-1"))
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion",
		now: func() time.Time { return clock }}
	// The close is tried and held six times, so its backoff (1 s doubling, 32 s after the sixth)
	// outlasts the 20 s step, and the later notice, written now, falls due before the held close.
	for tick := range 6 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		clock = clock.Add(20 * time.Second)
	}
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 3}, clock))
	var closeDue, laterDue time.Time
	if err := pool.QueryRow(context.Background(), `select
		(select next_at from outbox where payload ->> 'kind' = 'child-closed'),
		(select next_at from outbox where payload ->> 'kind' = 'design-approved')`).Scan(&closeDue, &laterDue); err != nil {
		t.Fatalf("read the two rows' due times: %v", err)
	}
	if !laterDue.Before(closeDue) {
		t.Fatalf("the later notice is due at %s and the held close at %s; want the later notice due first", laterDue, closeDue)
	}
	publisher.setAbsent()
	for tick := range 8 {
		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("tick %d after the root returned: %v", tick, err)
		}
		clock = clock.Add(20 * time.Second)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[child-closed to LEGION-1 design-approved to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want the held close first, then the later notice, both to the root", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// reparent records a Dispatch re-parent of LEGION-2 as admission does: its parent changes and its
// tree does not.
func reparent(t *testing.T, pool *store.Pool, records record.Store, parent string) {
	t.Helper()
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &parent, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
}

// A child re-parented under an issue the workflow has not recorded stays in its tree: its notice
// goes to its own architect while that holds its role, else to the tree's root architect (the
// child has none here), and neither it nor the tree's other notices wait on the unrecorded parent.
// The root's own notice and a grandchild's, to the grandchild's running sub-architect, are
// delivered on the same tick.
func TestANoticeOfAChildReparentedOutsideTheRecordReachesItsTreesRoot(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	reparent(t, pool, records, "LEGION-99")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-3", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[phase-finished to LEGION-1 design-approved to LEGION-1 phase-finished to LEGION-3]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want the re-parented child's and the root's notices to the root, the grandchild's to its sub-architect, none left",
			deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// A child re-parented under another tree's root stays in its own tree: its notices go to its own
// architect while that holds its role, else, as its close does, to its own tree's root architect;
// none goes to the other tree's.
func TestANoticeOfAChildReparentedUnderAnotherTreeStaysWithItsOwnTree(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-50", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-50", Title: "Other root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	reparent(t, pool, records, "LEGION-50")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-50", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}, time.Now()))
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[phase-finished to LEGION-1 child-closed to LEGION-1]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left, want both notices to LEGION-1's architect", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
}

// A notice whose architect cannot be resolved from the record (here an issue whose key makes no
// claim token) holds back no later notice of its tree, and finishes undelivered on its own attempt
// with one log line: retrying cannot change the record, and the outbox has no dead-letter path.
func TestAnUnroutableNoticeHoldsNothingBackAndFinishesOnItsOwnAttempt(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-4x", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &root, Title: "Malformed", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	clock := time.Now()
	// The unroutable row is written first but falls due after the root's notice, so the root's
	// notice meets it as an earlier notice of the tree.
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-4x", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, clock.Add(time.Minute)))
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, clock))
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[design-approved to LEGION-1]" || outboxRows(t, pool) != 1 {
		t.Fatalf("delivered %v with %d rows left, want the root's notice delivered past the unroutable one", deliveredKinds(t, delivered), outboxRows(t, pool))
	}
	clock = clock.Add(2 * time.Minute)
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if outboxRows(t, pool) != 0 || strings.Count(logged.String(), `msg="outbox notice finished undelivered: it has no architect"`) != 1 || !strings.Contains(logged.String(), "issue=LEGION-4x") {
		t.Fatalf("%d rows left, log %q; want the unroutable row finished with one line naming its issue", outboxRows(t, pool), logged.String())
	}
}

// A child moved under a child of another tree, whose own sub-architect runs, still belongs to its
// own tree: the tree's issues are read by tree, so the other tree's issues are no parents of it, and
// its notice goes to its own tree's owner.
func TestANoticeOfAChildMovedUnderAnotherTreesSubArchitectStaysWithItsOwnTree(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	other := "LEGION-50"
	putOutboxIssue(t, pool, records, record.Issue{Key: other, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: other, Title: "Other root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-51", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: other, Parent: &other, Title: "Other child", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	reparent(t, pool, records, "LEGION-51")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	architectClaim(t, sup, "LEGION-51", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := architectTopic(t, "LEGION-1")
	if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != want || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %+v with %d rows left, want the one notice to %s", delivered, outboxRows(t, pool), want)
	}
}

// A notice of a tree whose root is not recorded has no architect to go to: it finishes undelivered
// on its first attempt, with one error line, instead of waiting on a root that cannot be read.
func TestANoticeOfATreeWhoseRootIsNotRecordedFinishesUndelivered(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := "LEGION-1"
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &root, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	var logged bytes.Buffer
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, root))
	runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: time.Now}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 0 ||
		strings.Count(logged.String(), `level=ERROR msg="outbox notice finished undelivered: it has no architect"`) != 1 {
		t.Fatalf("delivered %+v with %d rows left, log %q; want the row finished undelivered with one error line", delivered, outboxRows(t, pool), logged.String())
	}
}

// A newer ready of an architect drops every catch-up the outbox holds for that architect's issue
// (DropCatchUps), a row the runner has already leased included. The runner executes a leased row
// from memory, so the notice executor reads the row under its lease in the tree's snapshot and
// publishes nothing when the row is gone: the architect is told the fresh catch-up alone, not the
// dropped one first.
func TestACatchUpDroppedUnderItsLeaseIsNotPublished(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	catchUp := func(generation uint64) record.Notice {
		return record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: fmt.Sprintf("generation %d", generation),
			CatchUp: &record.CatchUp{Generation: generation, Gates: []record.CatchUpGate{{Step: record.DesignStepSpec, Gated: true, Unregistered: true}}, Issues: []record.CatchUpIssue{}}}
	}
	clock := time.Now()
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", catchUp(1), clock))
	var leased record.OutboxRow
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		due, err := records.DueRows(ctx, tx, "LEGION", "LEGION-1", nil, clock)
		if err != nil {
			return err
		}
		if len(due) != 1 {
			return fmt.Errorf("%d rows due, want the first catch-up", len(due))
		}
		var claimed bool
		leased, claimed, err = records.ClaimRow(ctx, tx, due[0].ID, clock, time.Minute)
		if err == nil && !claimed {
			return errors.New("the first catch-up was not claimed")
		}
		return err
	}); err != nil {
		t.Fatalf("lease the first catch-up: %v", err)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := records.DropCatchUps(ctx, tx, "LEGION-1"); err != nil {
			return err
		}
		return records.Enqueue(ctx, tx, mustOutboxRow(t, "LEGION-1", catchUp(2), clock))
	}); err != nil {
		t.Fatalf("drop the first catch-up and write the second: %v", err)
	}
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
		project: "legion", now: func() time.Time { return clock }}

	if err := runner.execute(ctx, leased); err != nil {
		t.Fatalf("execute the leased first catch-up: %v", err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox: %v", err)
	}
	_, delivered := publisher.snapshot()
	// The delivery states the tree again, its reason with it (deliverNotice), so each catch-up is
	// told apart by the generation it was written for, which the row keeps.
	var got []string
	for _, published := range delivered {
		got = append(got, fmt.Sprintf("generation %d", published.payload.(record.Notice).CatchUp.Generation))
	}
	if fmt.Sprint(got) != "[generation 2]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left; want the second catch-up alone", got, outboxRows(t, pool))
	}
}

// ranPastItsLeaseLine is the lane's one line for a run that settles after the last lease it
// committed (runRow).
const ranPastItsLeaseLine = "outbox row ran past its lease; a drop of its issue's catch-ups keeps it while its claim stands, and its finish or retry fences on the token"

// lapsingAsk is the kept decision a lapsingCatchUp names.
const lapsingAsk = "dispatch://LEGION-1/ask/ask-1"

// lapsingCatchUp is a root's catch-up that names one kept decision, with the tree it states and
// the architect claim it goes to, run by an outbox whose clock the test holds (heldClock): no timer
// of the clock fires unless the test fires it, so a run's lease renewal never ticks, and advance
// moves the time every claim, drop and settle reads, so the lease a claim wrote lapses while the
// run goes on. The runner logs to logs and publishes to publisher.
type lapsingCatchUp struct {
	pool      *store.Pool
	records   record.Store
	clock     *heldClock
	publisher *hookPublisher
	logs      *testlog.Buffer
	runner    *outbox
	// row is the catch-up as its lane lists it.
	row record.OutboxRow
}

// newLapsingCatchUp builds the tree, the architect claim and the kept decision as
// TestAReadyLeavesTheCatchUpThatIsPublishingWhatItNamed does, and queues the catch-up of launch 1
// that names and claims the decision.
func newLapsingCatchUp(t *testing.T) *lapsingCatchUp {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	clock := &heldClock{at: time.Now().UTC().Truncate(time.Microsecond)}
	keepDecision(t, pool, records, "LEGION-1", lapsingAsk, time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	keptCatchUp(t, pool, records, "LEGION-1", 1, clock.Now())
	rows := catchUpRows(t, pool, "LEGION-1")
	if naming := catchUpsNaming(t, pool, "LEGION-1", 0, lapsingAsk); len(rows) != 1 || namingRef(naming) != 1 {
		t.Fatalf("the catch-ups of LEGION-1 name %s: %v, want the one catch-up to name it", lapsingAsk, naming)
	}
	logs := &testlog.Buffer{}
	publisher := &hookPublisher{}
	return &lapsingCatchUp{pool: pool, records: records, clock: clock, publisher: publisher, logs: logs, row: rows[0],
		runner: &outbox{log: slog.New(slog.NewJSONHandler(logs, nil)), pool: pool, dispatchProject: "LEGION", records: records,
			notices: publisher, supervisor: sup, project: "legion", now: clock.Now, clock: clock}}
}

// drop is the architect's next ready while the catch-up's claim stands past its lease: the clock
// moves on by more than outboxLease from the claim, past the lease the claim wrote, and the ready
// queues the catch-up of launch at that moment (workflow.QueueCatchUp), which first drops the
// root's queued catch-ups (DropCatchUps). It answers an error rather than failing the test, so it
// can run inside the run, and it answers one when the catch-up holds no claim whose lease has
// lapsed by then, since the drop would then not meet a run past its lease.
func (l *lapsingCatchUp) drop(launch uint64) error {
	ctx := context.Background()
	l.clock.advance(outboxLease + time.Second)
	now := l.clock.Now()
	var until time.Time
	if err := l.pool.QueryRow(ctx, "select lease_until from outbox where id = $1 and lease_token <> ''", l.row.ID).Scan(&until); err != nil {
		return fmt.Errorf("read the claim of the catch-up %d, which a drop past its lease must meet: %w", l.row.ID, err)
	}
	if !until.Before(now) {
		return fmt.Errorf("the catch-up %d is leased until %s, and the drop is at %s: want its lease lapsed",
			l.row.ID, until.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	}
	return store.Transact(ctx, l.pool, func(ctx context.Context, tx pgx.Tx) error {
		stored, err := l.records.Issue(ctx, tx, "LEGION-1")
		if err != nil || stored == nil {
			return fmt.Errorf("read LEGION-1: %w", err)
		}
		return workflow.QueueCatchUp(ctx, tx, l.records, config.DesignGateRootIssues.Steps(), *stored, launch, "", now)
	})
}

// runDue runs every row of the outbox due at the clock's time (RunOnce), as the lanes and the
// sweep would.
func (l *lapsingCatchUp) runDue(t *testing.T) {
	t.Helper()
	if err := l.runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the outbox's due rows: %v", err)
	}
}

// toldOnce fails t unless the kept decision was named by exactly one published catch-up, nothing
// is left kept, and the outbox holds no row, every due row having run. The count is read first:
// a decision told twice is the fault these tests stand against.
func (l *lapsingCatchUp) toldOnce(t *testing.T) {
	t.Helper()
	var told int
	for _, published := range l.publisher.published() {
		if notice, ok := published.payload.(record.Notice); ok && notice.CatchUp != nil && strings.Contains(notice.CatchUp.Decided, lapsingAsk) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("%s was told %d times by the catch-ups published, want once: a run past its lease still tells what its row "+
			"claims, so a drop of the root's catch-ups keeps that row, and the fresh catch-up names nothing of it", lapsingAsk, told)
	}
	if left := keptDecisions(t, l.pool, l.records, "LEGION-1"); len(left) != 0 {
		t.Fatalf("kept after every due row ran = %s, want none: the telling forgets what it named", keptRefs(left))
	}
	if rows := outboxRows(t, l.pool); rows != 0 {
		t.Fatalf("%d rows left in the outbox after every due row ran, want none", rows)
	}
}

// loggedTheRunPastItsLease fails t unless the lane logged once, at Warn, that the catch-up's run
// settled after its lease, naming the row, its kind, its issue, the lease and the moment it ended.
func (l *lapsingCatchUp) loggedTheRunPastItsLease(t *testing.T) {
	t.Helper()
	lines := loggedMessages(t, l.logs.String(), ranPastItsLeaseLine)
	if len(lines) != 1 {
		t.Fatalf("the lane logged %d lines of a run past its lease, want one for row %d: %v", len(lines), l.row.ID, lines)
	}
	line := lines[0]
	if line["msg"] != ranPastItsLeaseLine || line["level"] != "WARN" || line["row"] != float64(l.row.ID) ||
		line["kind"] != string(record.OutboxKindNotice) || line["issue"] != "LEGION-1" {
		t.Fatalf("the line of the run past its lease = %v, want it at WARN naming row %d, kind %s and issue LEGION-1",
			line, l.row.ID, record.OutboxKindNotice)
	}
	for _, attr := range []string{"lease_until", "ended"} {
		if _, ok := line[attr]; !ok {
			t.Fatalf("the line of the run past its lease = %v, want it to carry %s", line, attr)
		}
	}
}

// toldMarkHook is the record store with one operation run inside the mark that records a catch-up's
// telling (record.KeptToldSource, the outbox's toldKept): the publish has delivered, and the mark
// is not yet written. The hook runs once, on a transaction of its own, before the first such mark
// is written; the mark is then written as the store writes it.
type toldMarkHook struct {
	record.Store
	mu      sync.Mutex
	hook    func() error
	hookErr error
}

func (s *toldMarkHook) MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error) {
	if source == record.KeptToldSource {
		s.mu.Lock()
		hook := s.hook
		s.hook = nil
		s.mu.Unlock()
		if hook != nil {
			if err := hook(); err != nil {
				s.mu.Lock()
				s.hookErr = err
				s.mu.Unlock()
			}
		}
	}
	return s.Store.MarkProcessed(ctx, tx, source, eventID)
}

func (s *toldMarkHook) ranHook(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hookErr != nil {
		t.Fatalf("the operation run inside the told mark failed: %v", s.hookErr)
	}
	if s.hook != nil {
		t.Fatal("the operation armed for the told mark never ran: the row marked no telling")
	}
}

// A lane runs the row it claimed to its settle whatever its lease says: nothing cancels a row
// mid-effect, and the renewal that keeps the lease live can be held back (a database it cannot
// reach, a process the scheduler stalls). A catch-up that names a kept decision and outlives its
// lease that way still tells the decision once. A drop of the root's catch-ups past the lapse (the
// architect's next ready, workflow.QueueCatchUp) keeps the row while its claim stands, so the
// fresh catch-up the drop's transaction writes names nothing of it, whether the drop comes after
// the run read its row and before its publish, or after the publish and before the mark of the
// telling commits (toldKept). The lane logs such a run once, at Warn, when it settles past its
// lease. A run that stops mid-row before its publish, as a daemon that stops leaves it, leaves its
// claim to lapse: the sweep after the lapse runs the rows, and the decision is still told once.
// The clock is held (heldClock), so no renewal ticks, and the drop moves it past the claim's lease.
func TestAKeptDecisionIsToldOnceWhenACatchUpOutlivesItsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("a drop after the read and before the publish", func(t *testing.T) {
		l := newLapsingCatchUp(t)
		// The hook runs inside the publish, after the run read its row under its claim (stillOwed)
		// and found nothing newer (catchUpSuperseded).
		l.publisher.hook = func() error { return l.drop(2) }
		if err := l.runner.runRow(ctx, l.row); err != nil {
			t.Fatalf("run the catch-up on its lane: %v", err)
		}
		l.publisher.ranHook(t)
		l.runDue(t)
		l.toldOnce(t)
		l.loggedTheRunPastItsLease(t)
	})
	t.Run("a drop after the publish and before the told mark commits", func(t *testing.T) {
		l := newLapsingCatchUp(t)
		marks := &toldMarkHook{Store: l.records, hook: func() error { return l.drop(2) }}
		l.runner.records = marks
		if err := l.runner.runRow(ctx, l.row); err != nil {
			t.Fatalf("run the catch-up on its lane: %v", err)
		}
		marks.ranHook(t)
		l.runDue(t)
		l.toldOnce(t)
		l.loggedTheRunPastItsLease(t)
	})
	t.Run("a run that stops mid-row before its publish", func(t *testing.T) {
		l := newLapsingCatchUp(t)
		// The lane's claim, as runRow takes it, and then nothing: the daemon stopped mid-row.
		if err := store.Transact(ctx, l.pool, func(ctx context.Context, tx pgx.Tx) error {
			_, claimed, err := l.records.ClaimRow(ctx, tx, l.row.ID, l.clock.Now(), outboxLease)
			if err == nil && !claimed {
				return fmt.Errorf("outbox row %d was not claimed", l.row.ID)
			}
			return err
		}); err != nil {
			t.Fatalf("claim the catch-up as its lane does: %v", err)
		}
		if err := l.drop(2); err != nil {
			t.Fatalf("the architect's next ready past the claim's lease: %v", err)
		}
		// The sweep after the lapse runs every due row.
		l.runDue(t)
		l.toldOnce(t)
	})
}

// A catch-up whose run outlives its lease and whose publish the architect's session never confirmed
// (a listener's receipt timeout) is queued again as a copy that takes over the row's claims
// (reholdNotice, MoveKeptClaims). A drop of the root's catch-ups inside that publish, past the
// lapse, keeps the row while its claim stands, so the re-hold has the claims to move, the fresh
// catch-up the drop's transaction writes names nothing of them, and the copy tells the decision
// once. The fresh catch-up is of the launch the architect stands at, as CatchUpTheTreesOwed writes
// one, so nothing supersedes the row and the re-hold queues its copy rather than hand the
// decisions over (catchUpSuperseded). A drop that deleted the row would release its claims to the
// fresh catch-up and leave the copy the text without them, and both would tell the decision: the
// re-hold door of cybermonklabs/legion#2743's section "Named, not done".
func TestAKeptDecisionIsToldOnceWhenAReHeldCatchUpOutlivesItsLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	l := newLapsingCatchUp(t)
	l.publisher.answer = fmt.Errorf("publish notice: listener returned 504 Gateway Timeout: %w",
		notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses-architect"})
	l.publisher.hook = func() error { return l.drop(1) }
	if err := l.runner.runRow(ctx, l.row); err != nil {
		t.Fatalf("run the catch-up on its lane: %v", err)
	}
	l.publisher.ranHook(t)
	// The copy and the fresh catch-up are delivered; the clock moves past the copy's re-hold delay.
	l.publisher.mu.Lock()
	l.publisher.answer = nil
	l.publisher.mu.Unlock()
	l.clock.advance(noticeReholdDelays[0] + time.Second)
	l.runDue(t)
	l.toldOnce(t)
	copied := false
	for _, published := range l.publisher.published() {
		copied = copied || published.key == record.OutboxKey(l.row.ID)
	}
	if !copied {
		t.Fatalf("no catch-up was published under the key of row %d: the re-hold queued no copy of it", l.row.ID)
	}
	l.loggedTheRunPastItsLease(t)
}
