package daemon

import (
	"bytes"
	"cmp"
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
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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

// noticeTree records a root, its child and the child's child, each with its copy row
// (putChainIssue), so the Dispatch chain the notices are routed on runs LEGION-3, LEGION-2,
// LEGION-1. closed stands the root at done, its run closed (R24 of dispatch://INFRA-3320), out of
// Legion's work.
func noticeTree(t *testing.T, pool *store.Pool, records record.Store, closed bool) {
	t.Helper()
	root, child := "LEGION-1", "LEGION-2"
	rootIssue := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", HoldsGate: true}
	if closed {
		rootIssue.Phase, rootIssue.Status = phase.Done, "done"
	}
	putChainIssue(t, pool, records, rootIssue)
	putChainIssue(t, pool, records, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &root, Title: "Child", Phase: phase.Planning, Generation: 1, Status: "in_progress"})
	putChainIssue(t, pool, records, record.Issue{Key: "LEGION-3", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root, Parent: &child, Title: "Grandchild", Phase: phase.Testing, Generation: 1, Status: "testing"})
}

// putChainIssue records issue (putOutboxIssue) and writes its row of the daemon's copy of Dispatch
// as a reading does (putCopyRow): the parent the record names and the status Dispatch last showed
// it in (DispatchStatus, else Status).
func putChainIssue(t *testing.T, pool *store.Pool, records record.Store, issue record.Issue) {
	t.Helper()
	putOutboxIssue(t, pool, records, issue)
	var parent string
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	putCopyRow(t, pool, issue.Key, parent, cmp.Or(issue.DispatchStatus, issue.Status))
}

// putCopyRow writes the copy's row of key (dispatch_issues) under parent ("" for none) at status,
// at a sequence above every value the copy holds of it, so the row reads so whatever was written
// before (seedCopyRow).
func putCopyRow(t *testing.T, pool *store.Pool, key, parent, status string) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var seq int64
		if err := tx.QueryRow(ctx, `select coalesce(max(greatest(status_seq, parent_seq)), 0) from dispatch_issues where key = $1`, key).Scan(&seq); err != nil {
			return err
		}
		seedCopyRow(t, tx, key, parent, status, seq+1)
		return nil
	}); err != nil {
		t.Fatalf("write the copy row of %s: %v", key, err)
	}
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

// routeClaim is the state of one architect claim in a routing table: the machine's state, and
// who suspended it where it is suspended.
type routeClaim struct {
	state supervise.ClaimState
	by    supervise.SuspendSource
}

var (
	holds            = routeClaim{state: supervise.StateWorking}
	launching        = routeClaim{state: supervise.StateLaunching}
	failedHere       = routeClaim{state: supervise.StateFailed}
	retiredHere      = routeClaim{state: supervise.StateRetired}
	byOperator       = routeClaim{state: supervise.StateSuspended, by: supervise.SuspendedByOperator}
	byAStop          = routeClaim{state: supervise.StateSuspended, by: supervise.SuspendedByDaemon}
	releasedWhenIdle = routeClaim{state: supervise.StateSuspended, by: supervise.SuspendedByReleased}
)

// routingChains records the issues of a routing fixture with their copy rows, and the copy rows of
// copied, issues the daemon does not record (key and parent pairs, at in_progress), and reads the
// route of every one of keys in one read, as the outbox does (readNoticeRoute).
func routingChains(t *testing.T, issues []record.Issue, copied map[string]string, keys []string) noticeRoute {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, issue := range issues {
		putChainIssue(t, pool, records, issue)
	}
	for key, parent := range copied {
		putCopyRow(t, pool, key, parent, "in_progress")
	}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, project: "legion"}
	var route noticeRoute
	if err := store.TransactWith(context.Background(), pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		route, err = runner.readNoticeRoute(ctx, tx, "LEGION", keys)
		return err
	}); err != nil {
		t.Fatalf("read the route: %v", err)
	}
	return route
}

// routingIssue is a record of a routing fixture in Legion's work, implementing, stored in tree and
// under parent ("" for none) in Dispatch; a record whose key is its tree holds its own gate, as
// the migration marks every root of the release before.
func routingIssue(key, tree, parent string) record.Issue {
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: tree, Title: key,
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", HoldsGate: key == tree}
	if parent != "" {
		issue.Parent = &parent
	}
	return issue
}

// routedTo is the key of the issue whose architect route sends notice about key to, the claims
// running as claims says (holdsRole over each, as the outbox reads its machines), or "waits" for a
// notice the router holds (errNoticeWaits), or the error.
func routedTo(t *testing.T, route noticeRoute, key string, notice record.Notice, claims map[string]routeClaim) string {
	t.Helper()
	states := map[claim.Token]routeClaim{}
	for issue, held := range claims {
		states[mustClaimToken(t, issue, claim.RoleArchitect)] = held
	}
	issue, err := route.issue(key)
	if err != nil {
		return err.Error()
	}
	token, err := noticeArchitect(route, issue, notice, func(token claim.Token) bool {
		held := states[token]
		return holdsRole(supervise.ClaimView{Claim: supervise.Claim{State: held.state, SuspendedBy: held.by}})
	})
	switch {
	case errors.Is(err, errNoticeWaits):
		return "waits"
	case err != nil:
		return err.Error()
	}
	for i := range 100 {
		if candidate := fmt.Sprintf("LEGION-%d", i); token == mustClaimToken(t, candidate, claim.RoleArchitect) {
			return candidate
		}
	}
	return string(token)
}

// The fixture of the routing tables: a root LEGION-1 with LEGION-2, LEGION-3 and LEGION-4 under it
// three deep; another root LEGION-50 with LEGION-51 under it; LEGION-5, stored in LEGION-1's tree
// and under LEGION-51 in Dispatch; LEGION-6, stored in LEGION-1's tree and under LEGION-77, which
// the daemon does not record; LEGION-7 under LEGION-88, whose row the copy lacks; and LEGION-9 at
// work under LEGION-8, which Dispatch shows on the backlog under LEGION-1.
func routingFixture(t *testing.T) noticeRoute {
	t.Helper()
	backlog := routingIssue("LEGION-8", "LEGION-1", "LEGION-1")
	backlog.Phase, backlog.Status = phase.Done, "backlog"
	issues := []record.Issue{
		routingIssue("LEGION-1", "LEGION-1", ""), routingIssue("LEGION-2", "LEGION-1", "LEGION-1"),
		routingIssue("LEGION-3", "LEGION-1", "LEGION-2"), routingIssue("LEGION-4", "LEGION-1", "LEGION-3"),
		routingIssue("LEGION-50", "LEGION-50", ""), routingIssue("LEGION-51", "LEGION-50", "LEGION-50"),
		routingIssue("LEGION-5", "LEGION-1", "LEGION-51"), routingIssue("LEGION-6", "LEGION-1", "LEGION-77"),
		routingIssue("LEGION-7", "LEGION-1", "LEGION-88"), backlog, routingIssue("LEGION-9", "LEGION-1", "LEGION-8"),
	}
	keys := make([]string, len(issues))
	for i, issue := range issues {
		keys[i] = issue.Key
	}
	return routingChains(t, issues, map[string]string{"LEGION-77": ""}, keys)
}

// A notice about an issue goes to its own architect while that claim holds its role, else up the
// Dispatch chain to the nearest architect in Legion's work whose claim holds its role, else to its
// gate root's (R18 of dispatch://INFRA-3320, record.OwningIssue): a claim that failed, retired,
// was never made or was suspended by the operator or by a stop holds no role, and one the idle
// release suspended or that is launching does. The chain is the copy's, never the stored tree: an
// issue stored in one tree and standing under another's issue in Dispatch is that issue's
// architects', one under an issue the daemon does not record is a root of its own, an issue on
// the backlog between two in work is passed over, and a walk that meets a chain the copy cannot
// show to its top waits, unless the issue's own architect hears it. A catch-up goes to its own
// issue's architect alone, and a reparented notice to the architect it names.
func TestANoticeGoesToTheIssuesOwnArchitectFirst(t *testing.T) {
	t.Parallel()
	route := routingFixture(t)
	phaseFinished := record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}
	for _, tc := range []struct {
		name   string
		issue  string
		notice record.Notice
		claims map[string]routeClaim
		owner  string
	}{
		{"its own architect holds its role", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": holds, "LEGION-2": holds, "LEGION-1": holds}, "LEGION-3"},
		{"its own architect is launching", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": launching, "LEGION-1": holds}, "LEGION-3"},
		{"its own architect failed, the one above holds", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": failedHere, "LEGION-2": holds, "LEGION-1": holds}, "LEGION-2"},
		{"its own retired, the one above released for being idle", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": retiredHere, "LEGION-2": releasedWhenIdle, "LEGION-1": holds}, "LEGION-2"},
		{"its own suspended by the operator", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": byOperator, "LEGION-1": holds}, "LEGION-1"},
		{"its own suspended by a stop", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-3": byAStop, "LEGION-1": holds}, "LEGION-1"},
		{"no claim of its own, the one above failed", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-2": failedHere, "LEGION-1": holds}, "LEGION-1"},
		{"no architect on the chain holds its role", "LEGION-3", phaseFinished, map[string]routeClaim{"LEGION-1": failedHere}, "LEGION-1"},
		{"three deep, past a stopped architect to an idle one", "LEGION-4", phaseFinished, map[string]routeClaim{"LEGION-3": byAStop, "LEGION-2": releasedWhenIdle, "LEGION-1": holds}, "LEGION-2"},
		{"stored in one tree, under another tree's issue", "LEGION-5", phaseFinished, map[string]routeClaim{"LEGION-51": holds, "LEGION-1": holds}, "LEGION-51"},
		{"stored in one tree, under another tree's failed architect", "LEGION-5", phaseFinished, map[string]routeClaim{"LEGION-51": failedHere, "LEGION-50": holds, "LEGION-1": holds}, "LEGION-50"},
		{"under an issue the daemon does not record", "LEGION-6", phaseFinished, map[string]routeClaim{"LEGION-1": holds}, "LEGION-6"},
		{"on a chain the copy cannot show to its top", "LEGION-7", phaseFinished, map[string]routeClaim{"LEGION-1": holds}, "waits"},
		{"on such a chain, its own architect holding its role", "LEGION-7", phaseFinished, map[string]routeClaim{"LEGION-7": holds, "LEGION-1": holds}, "LEGION-7"},
		{"past a parent on the backlog", "LEGION-9", phaseFinished, map[string]routeClaim{"LEGION-1": holds}, "LEGION-1"},
		{"a catch-up, its own architect failed", "LEGION-3", record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, CatchUp: &record.CatchUp{}},
			map[string]routeClaim{"LEGION-3": failedHere, "LEGION-2": holds}, "LEGION-3"},
		{"a reparented notice", "LEGION-3", record.Notice{Kind: record.ReparentedNotice, To: "LEGION-50", Reason: "moved"},
			map[string]routeClaim{"LEGION-3": holds, "LEGION-50": holds}, "LEGION-50"},
		{"an ancestor's resting-root notice under its running sub-architect", "LEGION-2", record.Notice{Kind: "resting-root", Role: claim.RoleArchitect},
			map[string]routeClaim{"LEGION-2": holds, "LEGION-1": holds}, "LEGION-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := routedTo(t, route, tc.issue, tc.notice, tc.claims); got != tc.owner {
				t.Fatalf("a %s notice about %s with claims %v goes to %s, want %s", tc.notice.Kind, tc.issue, tc.claims, got, tc.owner)
			}
		})
	}
}

// A notice of a child's status or close, and a worker-died or a worker-stalled about an issue's own
// architect, starts at its parent on the Dispatch chain (R18 of dispatch://INFRA-3320,
// record.NoticeStartsAtParent): the architect above the issue hears it though the issue's own holds
// its role, past one above that does not, else the gate root of the issue above; a notice about
// the issue's phase worker goes to its own architect. An issue with nothing above it in Legion's
// work, a root or one under an issue the daemon does not record, hears its own. The walk follows
// the chain, not the stored tree, past a parent on the backlog, and waits on a chain the copy
// cannot show to its top.
func TestAChildStatusNoticeStartsAtTheParent(t *testing.T) {
	t.Parallel()
	route := routingFixture(t)
	childStatus := record.Notice{Kind: "child-status", Role: claim.RoleArchitect, Reason: "started"}
	childClosed := record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "done"}
	for _, tc := range []struct {
		name   string
		issue  string
		notice record.Notice
		claims map[string]routeClaim
		owner  string
	}{
		{"a child's status, its own architect holding its role", "LEGION-3", childStatus, map[string]routeClaim{"LEGION-3": holds, "LEGION-2": holds, "LEGION-1": holds}, "LEGION-2"},
		{"a child's close past a failed architect above it", "LEGION-3", childClosed, map[string]routeClaim{"LEGION-3": holds, "LEGION-2": failedHere, "LEGION-1": holds}, "LEGION-1"},
		{"a worker-died about its own architect", "LEGION-3", record.Notice{Kind: "worker-died", Role: claim.RoleArchitect},
			map[string]routeClaim{"LEGION-3": failedHere, "LEGION-2": holds}, "LEGION-2"},
		{"a worker-stalled about its own architect, nobody above holding its role", "LEGION-3", record.Notice{Kind: "worker-stalled", Role: claim.RoleArchitect},
			map[string]routeClaim{"LEGION-3": holds}, "LEGION-1"},
		{"a worker-died about its planner", "LEGION-3", record.Notice{Kind: "worker-died", Role: claim.RolePlanner},
			map[string]routeClaim{"LEGION-3": holds, "LEGION-2": holds}, "LEGION-3"},
		{"a root's own status", "LEGION-1", childStatus, map[string]routeClaim{"LEGION-1": holds}, "LEGION-1"},
		{"an issue under one the daemon does not record", "LEGION-6", childStatus, map[string]routeClaim{"LEGION-6": holds, "LEGION-1": holds}, "LEGION-6"},
		{"stored in one tree, under another tree's issue", "LEGION-5", childClosed, map[string]routeClaim{"LEGION-5": holds, "LEGION-51": holds, "LEGION-1": holds}, "LEGION-51"},
		{"past a parent on the backlog", "LEGION-9", childClosed, map[string]routeClaim{"LEGION-9": holds, "LEGION-1": holds}, "LEGION-1"},
		{"on a chain the copy cannot show to its top", "LEGION-7", childStatus, map[string]routeClaim{"LEGION-7": holds, "LEGION-1": holds}, "waits"},
		// A child's start is told with a child-status, whose walk starts at its parent, so the
		// architect above the child hears it even while the child's own architect is launching.
		{"a child's start, its own architect launching", "LEGION-2", childStatus, map[string]routeClaim{"LEGION-2": launching, "LEGION-1": holds}, "LEGION-1"},
		{"a grandchild's start under its parent's architect", "LEGION-3", childStatus, map[string]routeClaim{"LEGION-2": holds, "LEGION-1": holds}, "LEGION-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := routedTo(t, route, tc.issue, tc.notice, tc.claims); got != tc.owner {
				t.Fatalf("a %s notice about %s's %s with claims %v goes to %s, want %s", tc.notice.Kind, tc.issue, tc.notice.Role, tc.claims, got, tc.owner)
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
// back only through a new start (an operator's retry, a return, or for a child's architect its
// child's next phase start), whose ready queues a catch-up of its own. Such a row finishes without
// delivery, with one log line naming the notice kind and the issue. A resting-root notice
// (dispatch://INFRA-3320, R8) is keyed by the ancestor whose approval a root rests on, here
// LEGION-1, a root: with its architect claim absent, retired or failed, it finishes the same way,
// and the ancestor's next catch-up names the root instead (workflow's
// TestRestingRootsHoldTheAncestorSignOff).
func TestANoticeForAnEndedArchitectFinishesWithoutDelivery(t *testing.T) {
	t.Parallel()
	childClosed := record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}
	restingRoot := record.Notice{Kind: "resting-root", Role: claim.RoleArchitect, Version: 3, Ref: "dispatch://LEGION-1/spec",
		Reason: "An ancestor's approval opened the spec step of LEGION-208. A person approved version 3 of the primary document of LEGION-1. " +
			"LEGION-208 rests on this approval. The checks of LEGION-208 serve these requirements of LEGION-1: dispatch://LEGION-1/spec#b-p2@v3."}
	for _, tc := range []struct {
		name string
		// state is the state of LEGION-1's architect claim, "" for no claim at all.
		state  supervise.ClaimState
		issue  string
		notice record.Notice
	}{
		{name: "the architect's claim retired", state: supervise.StateRetired, issue: "LEGION-2", notice: childClosed},
		{name: "the architect's claim failed", state: supervise.StateFailed, issue: "LEGION-2", notice: childClosed},
		{name: "an ancestor's resting-root notice with no architect claim", issue: "LEGION-1", notice: restingRoot},
		{name: "an ancestor's resting-root notice, its architect's claim retired", state: supervise.StateRetired, issue: "LEGION-1", notice: restingRoot},
		{name: "an ancestor's resting-root notice, its architect's claim failed", state: supervise.StateFailed, issue: "LEGION-1", notice: restingRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
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
// another claim. The route takes the stop from the issue's own architect or one above it on the
// Dispatch chain, so the two differ in both directions: the root's architect stopping a child
// whose sub-architect runs, and the child's own sub-architect stopping it.
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

// A notice for an architect whose claim is suspended — by the operator, or by a stop of its issue,
// which keeps its claims suspended until Dispatch shows it in work again (R20 of
// dispatch://INFRA-3320) — waits for it: the row is kept, and nothing is sent. One for a claim
// that failed or retired, or that this daemon does not supervise, ends undelivered (R18). A notice
// whose owner walk meets a chain the copy cannot show to its top waits too, whoever runs above the
// gap, and is delivered once a reading of Dispatch writes the missing row. A notice starts and
// resumes no claim a stop suspended: one queued at an entry the return then raises waits through
// it, and is delivered once the return's start brings the claim back, since a notice's delivery
// reads its run alone (the architect's ruling of 2026-11-21 on dispatch://INFRA-3320). Its
// delivery wakes only a claim an idle release suspended, which keeps its phase and task.
func TestANoticeWaitsOrEndsByTheClaimsState(t *testing.T) {
	t.Parallel()
	childClosed := record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: "LEGION-2 is done"}
	for _, tc := range []struct {
		name  string
		claim *routeClaim
		waits bool
	}{
		{name: "suspended by the operator", claim: &byOperator, waits: true},
		{name: "suspended by a stop of its issue", claim: &byAStop, waits: true},
		{name: "failed", claim: &failedHere},
		{name: "retired", claim: &retiredHere},
		{name: "not supervised"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			noticeTree(t, pool, records, false)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			if tc.claim != nil {
				if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-1", claim.RoleArchitect), Project: "legion",
					Tree: "LEGION-1", Issue: "LEGION-1", Role: claim.RoleArchitect, State: tc.claim.state, SuspendedBy: tc.claim.by}, ""); err != nil {
					t.Fatalf("create the architect claim of LEGION-1: %v", err)
				}
			}
			row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-2", childClosed, time.Now()))
			publisher := &holderPublisher{}
			publisher.setAbsent(architectTopic(t, "LEGION-1"))
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
			err := runner.execute(context.Background(), row)
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered %+v, want nothing sent to an architect that holds no role", delivered)
			}
			if tc.waits != errors.Is(err, errNoticeWaits) {
				t.Fatalf("executing the close of LEGION-2 for LEGION-1's architect, %s, = %v; waits %t", tc.name, err, tc.waits)
			}
			if !tc.waits && err != nil {
				t.Fatalf("executing the close of LEGION-2 for LEGION-1's architect, %s, = %v; want it finished undelivered", tc.name, err)
			}
		})
	}

	t.Run("on a chain the copy cannot show to its top", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		putChainIssue(t, pool, records, routingIssue("LEGION-1", "LEGION-1", ""))
		// LEGION-3's parent LEGION-2 is recorded, and the copy holds no row of it yet.
		putOutboxIssue(t, pool, records, routingIssue("LEGION-2", "LEGION-1", "LEGION-1"))
		putChainIssue(t, pool, records, routingIssue("LEGION-3", "LEGION-1", "LEGION-2"))
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-3", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))
		publisher := &holderPublisher{}
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}
		if err := runner.execute(context.Background(), row); !errors.Is(err, errNoticeWaits) || !errors.Is(err, record.ErrChainUnknown) {
			t.Fatalf("executing a notice of LEGION-3 whose parent's row the copy lacks = %v, want a wait on the unknown chain", err)
		}
		putCopyRow(t, pool, "LEGION-2", "LEGION-1", "in_progress")
		if err := runner.execute(context.Background(), row); err != nil {
			t.Fatalf("executing the notice once the chain is known: %v", err)
		}
		if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != architectTopic(t, "LEGION-1") {
			t.Fatalf("delivered %+v once the chain is known, want the notice to LEGION-1's architect", delivered)
		}
	})

	// stoppedOrReleased is LEGION-1's architect claim suspended by by, with the session a resume
	// brings back, and the runner that delivers its notices, with the store a wake is taken in.
	stoppedOrReleased := func(t *testing.T, by supervise.SuspendSource) (*store.Pool, record.Store, *fake.Runtime, *supervise.Machine, *outbox, *holderPublisher) {
		t.Helper()
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		st, err := store.Open(ctx, pool.Config().ConnString())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		records := record.NewStore()
		noticeTree(t, pool, records, false)
		sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
		machine, _, err := sup.Create(ctx, supervise.Claim{Token: mustClaimToken(t, "LEGION-1", claim.RoleArchitect), Project: "legion", Tree: "LEGION-1",
			Issue: "LEGION-1", Role: claim.RoleArchitect, State: supervise.StateSuspended, SuspendedBy: by, Generation: 1,
			Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"}, "")
		if err != nil {
			t.Fatalf("create the architect claim of LEGION-1: %v", err)
		}
		machine.Wait()
		if by == supervise.SuspendedByReleased {
			// The rig's supervisor writes its claims to a store of its own (outboxClaimStore), and
			// a delivery wake takes nothing for a claim the record holds no row of
			// (store.Store.TakeDeliveryWake), so the released claim's row goes into st as a
			// daemon's supervisor writes it (recordedReleasedClaim).
			if err := st.PutClaim(ctx, machine.Claim()); err != nil {
				t.Fatalf("record the released architect's claim of LEGION-1: %v", err)
			}
		}
		// The suspended claim's session is gone, so the listener finds no holder of its role.
		publisher := &holderPublisher{}
		publisher.setAbsent(architectTopic(t, "LEGION-1"))
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, trees: st,
			project: "legion", now: time.Now}
		return pool, records, rt, machine, runner, publisher
	}
	resumes := func(rt *fake.Runtime) int {
		count := 0
		for _, call := range rt.Calls() {
			if call.Method == "Resume" || call.Method == "Spawn" {
				count++
			}
		}
		return count
	}

	t.Run("of an earlier entry, for a claim a stop suspended, until the return starts it", func(t *testing.T) {
		ctx := context.Background()
		pool, records, rt, machine, runner, publisher := stoppedOrReleased(t, supervise.SuspendedByDaemon)
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
		queued, _ := record.FenceOf(mustDecode(t, row))
		// The return raises the issue's phase entry (workflow.ReturnToWork) in the run it stands in.
		architect := rolePhaseRow(t, pool, "LEGION-1", claim.RoleArchitect)
		architect.Issue, architect.Role = "LEGION-1", claim.RoleArchitect
		architect.Entry++
		var now record.Fence
		if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
			if err := records.PutPhase(ctx, tx, architect); err != nil {
				return err
			}
			var err error
			now, err = record.ReadFence(ctx, tx, records, "LEGION-1", false)
			return err
		}); err != nil {
			t.Fatalf("raise the entry of LEGION-1: %v", err)
		}
		if now.Generation != queued.Generation || now.Entry <= queued.Entry {
			t.Fatalf("LEGION-1 stands at %+v and its notice was queued at %+v, want a later entry of the same run", now, queued)
		}
		if err := runner.execute(ctx, row); !errors.Is(err, errNoticeWaits) {
			t.Fatalf("executing the notice while the stop holds LEGION-1's architect = %v, want it to wait", err)
		}
		if _, delivered := publisher.snapshot(); len(delivered) != 0 || resumes(rt) != 0 || machine.Claim().State != supervise.StateSuspended {
			t.Fatalf("delivered %+v, %d launches, claim %s; want nothing sent and the claim left suspended: a notice starts no claim a stop suspended",
				delivered, resumes(rt), machine.Claim().State)
		}
		// The return's start brings the claim back by rule, as the start executor hands it.
		if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: machine.Claim().Token, Reason: "the return's start"}); err != nil {
			t.Fatalf("resume LEGION-1's architect: %v", err)
		}
		machine.Wait()
		// The resumed session holds the role again once its relaunch has claimed it.
		publisher.setAbsent()
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("executing the notice once the return started LEGION-1's architect: %v", err)
		}
		if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[design-approved to LEGION-1]" {
			t.Fatalf("delivered %v once the return started the claim, want the notice of the earlier entry to LEGION-1's architect", deliveredKinds(t, delivered))
		}
	})

	t.Run("for a claim an idle release suspended, which its delivery wakes", func(t *testing.T) {
		ctx := context.Background()
		pool, records, rt, machine, runner, publisher := stoppedOrReleased(t, supervise.SuspendedByReleased)
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
		if err := runner.execute(ctx, row); !errors.Is(err, errNoticeWaits) {
			t.Fatalf("executing the notice for the released architect of LEGION-1 = %v, want it to wait for the woken claim", err)
		}
		machine.Wait()
		// The woken session holds the role again once its relaunch has claimed it.
		publisher.setAbsent()
		if _, delivered := publisher.snapshot(); len(delivered) != 0 || resumes(rt) != 1 || machine.Claim().State == supervise.StateSuspended {
			t.Fatalf("delivered %+v, %d launches, claim %s; want nothing sent yet and the released claim woken by the delivery", delivered, resumes(rt), machine.Claim().State)
		}
		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("executing the notice once the claim was woken: %v", err)
		}
		if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[design-approved to LEGION-1]" {
			t.Fatalf("delivered %v once the claim was woken, want the notice to LEGION-1's architect", deliveredKinds(t, delivered))
		}
	})
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

// A notice of an issue the daemon does not record has no architect to go to: it finishes
// undelivered on its first attempt, with one error line, instead of waiting on a record nothing
// will write.
func TestANoticeOfAnIssueTheDaemonDoesNotRecordFinishesUndelivered(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	noticeTree(t, pool, records, false)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
	enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-40", record.Notice{Kind: "phase-finished", Role: claim.RolePlanner, Phase: phase.Planning}, time.Now()))
	var logged bytes.Buffer
	publisher := &holderPublisher{}
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
		if _, err := records.DropCatchUps(ctx, tx, "LEGION-1", clock); err != nil {
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
	var got []string
	for _, published := range delivered {
		got = append(got, published.payload.(record.Notice).Reason)
	}
	if fmt.Sprint(got) != "[generation 2]" || outboxRows(t, pool) != 0 {
		t.Fatalf("delivered %v with %d rows left; want the second catch-up alone", got, outboxRows(t, pool))
	}
}
