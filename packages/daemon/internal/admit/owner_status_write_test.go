package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tree the tests of this file run (dispatch://INFRA-3178): a root with two children, each child
// with an architect of its own and one issue under it.
const (
	ownerRoot       = "LEGION-1"
	ownerChild      = "LEGION-2"
	ownerGrandchild = "LEGION-3" // under ownerChild
	ownerSibling    = "LEGION-4"
	ownerNephew     = "LEGION-5" // under ownerSibling
)

// The session of each claims row of that tree. A write in Dispatch names its session, and the
// daemon finds the writer's claim by that session alone.
const (
	rootArchitect      = "ses-root-architect"
	childArchitect     = "ses-child-architect"
	siblingArchitect   = "ses-sibling-architect"
	rootImplementer    = "ses-root-implementer"
	childImplementer   = "ses-child-implementer"
	siblingImplementer = "ses-sibling-implementer"
)

// ownerClaim is one claims row: the claim of role on issue in tree, which session holds in state.
// turn says that the claim's agent is in a turn now.
type ownerClaim struct {
	tree, issue string
	role        claim.Role
	session     string
	state       supervise.ClaimState
	turn        bool
}

// treeClaims is every claim of the tree: the architect of the root, of the child and of the
// sibling, and an implementer on each of those three issues. The root's architect writes a status
// in a turn of its own, and the child's implementer is in a turn too, so a move that ends the
// child ends a turn in progress.
func treeClaims() []ownerClaim {
	return []ownerClaim{
		{tree: ownerRoot, issue: ownerRoot, role: claim.RoleArchitect, session: rootArchitect, state: supervise.StateWorking, turn: true},
		{tree: ownerRoot, issue: ownerChild, role: claim.RoleArchitect, session: childArchitect, state: supervise.StateWorking},
		{tree: ownerRoot, issue: ownerSibling, role: claim.RoleArchitect, session: siblingArchitect, state: supervise.StateWorking},
		{tree: ownerRoot, issue: ownerRoot, role: claim.RoleImplementer, session: rootImplementer, state: supervise.StateWorking},
		{tree: ownerRoot, issue: ownerChild, role: claim.RoleImplementer, session: childImplementer, state: supervise.StateWorking, turn: true},
		{tree: ownerRoot, issue: ownerSibling, role: claim.RoleImplementer, session: siblingImplementer, state: supervise.StateWorking},
	}
}

// seedSessionClaims inserts each claim as the daemon records it, with the session that the store
// finds a writer by (record.Store.SessionClaimsOf), and holds each name its record gives that
// session (claim_sessions, supervise.RecordedHolds), as every registration does, so a claim that
// holds its role is one the daemon publishes as its role's holder (record.Store.RoleHolders).
func seedSessionClaims(t *testing.T, pool *pgxpool.Pool, claims ...ownerClaim) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		for _, c := range claims {
			token, err := claim.NewToken(project, c.issue, c.role)
			if err != nil {
				t.Fatalf("the %s claim token of %s: %v", c.role, c.issue, err)
			}
			var turn *time.Time
			if c.turn {
				turn = &fixedNow
			}
			file := "/tmp/" + c.session + ".jsonl"
			if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
				session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, turn_started_at)
				values ($1, $2, $3, $4, $5, 1, $6, $7, $8, 0, 0, 0, 0, $9)`,
				string(token), project, c.tree, c.issue, string(c.role), c.session, file, string(c.state), turn); err != nil {
				t.Fatalf("put the %s claim of %s: %v", c.role, c.issue, err)
			}
			for _, hold := range supervise.RecordedHolds(c.session, file) {
				if _, err := tx.Exec(context.Background(), `insert into claim_sessions (project, kind, name, claim) values ($1, $2, $3, $4)`,
					project, string(hold.Kind), hold.Name, string(token)); err != nil {
					t.Fatalf("hold %s %s for the %s claim of %s: %v", hold.Kind, hold.Name, c.role, c.issue, err)
				}
			}
		}
	})
}

// ownedMember is a record of the tree: key under parent ("" for the root), in phase at, with
// status, which Dispatch shows too.
func ownedMember(key, parent, rank string, at phase.Phase, status string) *record.Issue {
	member := &record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Tree: ownerRoot, Phase: at, Generation: 1, Status: status,
		DispatchStatus: status, Rank: rank, LastDispatchSeq: 1}
	if parent == "" {
		member.HandedOver = true
	} else {
		member.Parent = new(parent)
	}
	return member
}

// ownerRig is the tree these tests run, with the handlers that a Dispatch event goes through.
type ownerRig struct {
	t         *testing.T
	pool      *pgxpool.Pool
	admission *Admission
	engine    *workflow.Engine
	// settled is the number of outbox rows the tree's settle left (seedOwnerTree), and before the
	// phase each issue stood in when a write of it was last applied (writeAt).
	settled int
	before  map[string]phase.Phase
}

// seedOwnerTree records the tree with every issue in implementing at in_progress, and the claims of
// treeClaims. The copy of Dispatch's issues holds each record's row, the root's with the legion
// label, so the engine judges each write on the Dispatch chain (R18 and R26 of
// dispatch://INFRA-3320). Each issue that keeps a slot holds one, under a cap above all of them, so
// a slot that a move gives back starts nothing the test did not seed. The project checks in
// production with the tester. shape changes the records before they are written: it can add a
// record, or delete one.
//
// The tree is then settled by one admission pass (a controller tick), as the daemon holds such a
// tree: each issue in a phase an open Dispatch child holds waits in awaiting_children (R9 and R11
// of dispatch://INFRA-3320). A test reads the issues as the settle left them, and the assertions
// read the rows queued after it (sinceSettle).
func seedOwnerTree(t *testing.T, shape func(map[string]*record.Issue)) *ownerRig {
	t.Helper()
	pool := migratedPool(t)
	rig := &ownerRig{t: t, pool: pool, admission: newAdmission(t, 10, slog.New(slog.NewTextHandler(io.Discard, nil))),
		engine: workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Steps: config.DesignGateRootIssues.Steps(), ProductionCheckRole: claim.RoleTester,
			ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return fixedNow }}, nil),
		before: map[string]phase.Phase{}}
	issues := map[string]*record.Issue{
		ownerRoot:       ownedMember(ownerRoot, "", "A", phase.Implementing, "in_progress"),
		ownerChild:      ownedMember(ownerChild, ownerRoot, "B", phase.Implementing, "in_progress"),
		ownerGrandchild: ownedMember(ownerGrandchild, ownerChild, "C", phase.Implementing, "in_progress"),
		ownerSibling:    ownedMember(ownerSibling, ownerRoot, "D", phase.Implementing, "in_progress"),
		ownerNephew:     ownedMember(ownerNephew, ownerSibling, "E", phase.Implementing, "in_progress"),
	}
	if shape != nil {
		shape(issues)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		keys := slices.Sorted(maps.Keys(issues))
		for _, key := range keys {
			if err := records.PutIssue(ctx, tx, *issues[key]); err != nil {
				t.Fatalf("put %s: %v", key, err)
			}
			putCopyTx(t, tx, *issues[key])
		}
		slot := 0
		for _, key := range keys {
			if record.KeepsSlot(*issues[key]) {
				if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: slot, AdmittedAt: fixedNow}); err != nil {
					t.Fatalf("put the slot of %s: %v", key, err)
				}
				slot++
			}
		}
		seedLifecycleTx(t, tx, ownerRoot)
	})
	seedSessionClaims(t, pool, treeClaims()...)
	if result, err := intake.ApplyFact(context.Background(), pool, "controller", "settle", intake.ControllerTick{}, rig.engine, rig.admission); err != nil || result.Refusal != nil {
		t.Fatalf("settle the tree: %+v, %v", result.Refusal, err)
	}
	rig.settled = len(effects(t, pool))
	return rig
}

// sinceSettle is every row queued after the tree's settle (seedOwnerTree).
func (r *ownerRig) sinceSettle() []effect {
	r.t.Helper()
	return effects(r.t, r.pool)[r.settled:]
}

// write applies session's write of status on key ("" for a person's write) as the next Dispatch
// event of key, and returns it.
func (r *ownerRig) write(key, status, session string) intake.DispatchIssue {
	r.t.Helper()
	return r.writeAt(key, status, session, issue(r.t, r.pool, key).LastDispatchSeq+1)
}

// writeAt applies session's write of status on key as the Dispatch event at seq. The event carries
// the whole issue, as Dispatch sends one: the record's title, rank and parent, with status.
func (r *ownerRig) writeAt(key, status, session string, seq int64) intake.DispatchIssue {
	r.t.Helper()
	return r.writeEvent(key, status, session, seq, time.Time{})
}

// writtenAt applies session's write of status on key as the next Dispatch event of key, which
// Dispatch recorded at at (intake.DispatchIssue.At), and returns it.
func (r *ownerRig) writtenAt(key, status, session string, at time.Time) intake.DispatchIssue {
	r.t.Helper()
	return r.writeEvent(key, status, session, issue(r.t, r.pool, key).LastDispatchSeq+1, at)
}

// writeEvent applies session's write of status on key as the Dispatch event at seq, recorded at
// at, zero for an event that carries no time.
func (r *ownerRig) writeEvent(key, status, session string, seq int64, at time.Time) intake.DispatchIssue {
	r.t.Helper()
	recorded := issue(r.t, r.pool, key)
	r.before[key] = recorded.Phase
	kind := "issue.updated"
	if status == record.DoneStatus {
		kind = "issue.closed"
	}
	fact := intake.DispatchIssue{Key: key, ResolvedRepo: recorded.Repo, Seq: seq, Type: kind, Status: status,
		Title: recorded.Title, Rank: recorded.Rank, Parent: deref(recorded.Parent), HandedOver: recorded.HandedOver,
		ActorSession: session, At: at}
	r.applyEvent(fmt.Sprintf("%s-%d-%s-by-%q", key, seq, status, session), fact)
	return fact
}

// echo applies the event of the daemon's own write of status on key, which Dispatch sends back
// with the daemon's actor and no session.
func (r *ownerRig) echo(key, status string) {
	r.t.Helper()
	recorded := issue(r.t, r.pool, key)
	seq := recorded.LastDispatchSeq + 1
	r.applyEvent(fmt.Sprintf("%s-%d-%s-by-the-daemon", key, seq, status), intake.DispatchIssue{Key: key, ResolvedRepo: recorded.Repo,
		Seq: seq, Type: "issue.updated", Status: status, Title: recorded.Title, Rank: recorded.Rank, Parent: deref(recorded.Parent),
		HandedOver: recorded.HandedOver, DaemonActor: true})
}

// deliverStatusWrites is the outbox's delivery of every status write the daemon queued for key:
// Dispatch took each one, and the outbox finished its row (record.Postgres.FinishOutbox), as it
// does before Dispatch's event of that write reaches the daemon. An echo of the daemon's own write
// applied while a write of the issue is still queued takes only Dispatch's view (workflow's
// dispatchIssue, keepStatus), so a test that lands the daemon's close through its echo delivers it
// first.
func (r *ownerRig) deliverStatusWrites(key string) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `delete from outbox where kind = $1 and issue = $2`,
		string(record.OutboxKindDispatchStatus), key); err != nil {
		r.t.Fatalf("deliver the queued status writes of %s: %v", key, err)
	}
}

func (r *ownerRig) applyEvent(eventID string, fact intake.DispatchIssue) {
	r.t.Helper()
	apply(r.t, r.pool, r.admission, eventID, fact, r.engine)
}

// pullRequest records key's pull request number in state.
func (r *ownerRig) pullRequest(key string, number int, state record.PullRequestState) {
	r.t.Helper()
	pr := record.PullRequest{State: state, Issue: key, Repo: "acme/widgets", Number: number, Branch: "legion/" + key,
		HeadSHA: "head-" + key, Failing: []string{}}
	if state == record.PullRequestMerged {
		pr.MergeSHA = "merge-" + key
	}
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, pr); err != nil {
			r.t.Fatalf("put the pull request of %s: %v", key, err)
		}
	})
}

// merge applies GitHub's merge of pull request number at the merge commit "merge-<key>".
func (r *ownerRig) merge(key string, number int) {
	r.t.Helper()
	if result, err := intake.ApplyFact(context.Background(), r.pool, "github", fmt.Sprintf("merged-%d", number),
		intake.PullRequestMerged{Repo: "acme/widgets", Number: number, MergeSHA: "merge-" + key}, r.engine, r.admission); err != nil || result.Refusal != nil {
		r.t.Fatalf("merge #%d: %+v, %v", number, result.Refusal, err)
	}
}

// check is key's production check, nil for none.
func (r *ownerRig) check(key string) *record.ProductionCheck {
	r.t.Helper()
	var check *record.ProductionCheck
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		var err error
		if check, err = record.NewStore().ProductionCheck(context.Background(), tx, key); err != nil {
			r.t.Fatalf("read the production check of %s: %v", key, err)
		}
	})
	return check
}

// setClaimCrossings moves the claim of role on issue to state with the times it last entered and
// left the published states (claims.published_since, unpublished_since; nil for a time no crossing
// stamped), as the store's claim writes would have stamped them.
func (r *ownerRig) setClaimCrossings(issue string, role claim.Role, state supervise.ClaimState, published, unpublished *time.Time) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = $3, published_since = $4, unpublished_since = $5
		where issue = $1 and role = $2`, issue, string(role), string(state), published, unpublished); err != nil {
		r.t.Fatalf("set the %s claim of %s to %s with its crossings: %v", role, issue, state, err)
	}
}

// convertedAt records that the project's first boot on this release finished its conversion at at
// (tree_conversions, its last step, 11), the time the repair judges a claim against where no
// crossing has stamped it (record.RoleHolderAt.PublishedAt).
func (r *ownerRig) convertedAt(at time.Time) {
	r.t.Helper()
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		if err := record.RecordTreeConversionStep(context.Background(), tx, testProject, 11, &at); err != nil {
			r.t.Fatal(err)
		}
	})
}

// noticeOwner is the issue whose architect hears a notice of kind about key: the owner walk the
// outbox runs when it sends the notice (record.OwningIssue), over the Dispatch chain and the claims
// as the store holds them.
func (r *ownerRig) noticeOwner(key string, kind record.NoticeKind) string {
	r.t.Helper()
	var owner string
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		walk, err := record.ReadOwnerWalk(context.Background(), record.NewStore(), tx, testProject, []string{key})
		if err != nil {
			r.t.Fatalf("read the owner walk of %s: %v", key, err)
		}
		owner = walk.Owner(key, record.NoticeStartsAtParent(kind, claim.RoleArchitect))
	})
	return owner
}

// rowsFor is every row of rows that is of key.
func rowsFor(rows []effect, key string) []effect {
	var of []effect
	for _, row := range rows {
		if row.issue == key {
			of = append(of, row)
		}
	}
	return of
}

// payloadsOf is the payload of every row of rows that is a T.
func payloadsOf[T record.OutboxPayload](rows []effect) []T {
	var of []T
	for _, row := range rows {
		if payload, ok := row.payload.(T); ok {
			of = append(of, payload)
		}
	}
	return of
}

// rolesOf is the role of every supervise request of rows whose op is op, in order.
func rolesOf(rows []effect, op record.SuperviseOp) []claim.Role {
	var roles []claim.Role
	for _, request := range payloadsOf[record.SuperviseRequest](rows) {
		if request.Op == op {
			roles = append(roles, request.Role)
		}
	}
	return roles
}

// childNotices is every child-closed, child-status and status-reasserted notice of rows: what the
// architects hear of a move of an issue, or of its set-back.
func childNotices(rows []effect) []record.Notice {
	var notices []record.Notice
	for _, notice := range payloadsOf[record.Notice](rows) {
		switch notice.Kind {
		case "child-closed", "child-status", "status-reasserted":
			notices = append(notices, notice)
		}
	}
	return notices
}

// assertLeft holds that key left Legion's work at status as a person's move takes it out (the
// workflow's leave): its record holds status and keeps the phase it stood in before the move (R20
// of dispatch://INFRA-3320), and no status write of the daemon's is queued for it after the
// tree's settle. A done closes each of its claims, and any other status suspends each of them and
// releases its Dispatch claim. Either way its pod goes (record.IssueSuspend, compared by the tree,
// run and tree run it names, beside the fence it carries), and the architect above it hears
// child-closed for a done and child-status for the other three. The notice's reason starts with
// "<key> is <status>". This holds only that first sentence, because the notice can name more after
// it, such as a Dispatch child of key that is not done.
func (r *ownerRig) assertLeft(key, status string) {
	r.t.Helper()
	got, root := issue(r.t, r.pool, key), issue(r.t, r.pool, ownerRoot)
	if record.InLegionsWork(got) || got.Phase != r.before[key] || got.Status != status {
		r.t.Fatalf("%s after the move to %s = phase %s, status %s: want it out of Legion's work with status %s, its phase %s kept",
			key, status, got.Phase, got.Status, status, r.before[key])
	}
	rows := rowsFor(r.sinceSettle(), key)
	if writes := payloadsOf[record.StatusWrite](rows); len(writes) != 0 {
		r.t.Fatalf("status writes queued for %s after the move to %s = %+v, want none: the daemon sets nothing back over the move", key, status, writes)
	}
	op, other, kind, releases := record.SuperviseOp("suspend"), record.SuperviseOp("issue_close"), record.NoticeKind("child-status"), 1
	if status == record.DoneStatus {
		op, other, kind, releases = "issue_close", "suspend", "child-closed", 0
	}
	if got := rolesOf(rows, op); !reflect.DeepEqual(got, claim.Roles) {
		r.t.Fatalf("%s requests for %s after the move to %s = %v, want one for each role %v", op, key, status, got, claim.Roles)
	}
	if got := rolesOf(rows, other); len(got) != 0 {
		r.t.Fatalf("%s requests for %s after the move to %s = %v, want none", other, key, status, got)
	}
	var suspends []record.IssueSuspend
	for _, suspend := range payloadsOf[record.IssueSuspend](rows) {
		suspends = append(suspends, record.IssueSuspend{Tree: suspend.Tree, Generation: suspend.Generation, TreeGeneration: suspend.TreeGeneration})
	}
	if want := []record.IssueSuspend{{Tree: ownerRoot, Generation: got.Generation, TreeGeneration: root.Generation}}; !reflect.DeepEqual(suspends, want) {
		r.t.Fatalf("pod suspensions of %s after the move to %s = %+v, want %+v", key, status, suspends, want)
	}
	if got := len(payloadsOf[record.ClaimRelease](rows)); got != releases {
		r.t.Fatalf("Dispatch claim releases of %s after the move to %s = %d, want %d", key, status, got, releases)
	}
	notices := childNotices(rows)
	if len(notices) != 1 || notices[0].Kind != kind || notices[0].Role != claim.RoleArchitect || !strings.HasPrefix(notices[0].Reason, fmt.Sprintf("%s is %s", key, status)) {
		r.t.Fatalf("notices of %s after the move to %s = %+v, want one %s to the architect whose reason starts with %q", key, status, notices, kind, fmt.Sprintf("%s is %s", key, status))
	}
}

// assertSetBack holds that the write fact, of status on the issue whose record was before, is set
// back: the record keeps its phase, its status and its run, it records the event's sequence, and
// one status write of the daemon's own status over status is queued for it, at the sequence of the
// write it undoes. Nothing ends: no claim of the tree is stopped, no pod goes, and no architect is
// told, since the writer holds a claim in the tree.
func (r *ownerRig) assertSetBack(before record.Issue, status string, fact intake.DispatchIssue) {
	r.t.Helper()
	key := before.Key
	got := issue(r.t, r.pool, key)
	if got.Phase != before.Phase || got.Status != before.Status || got.Generation != before.Generation || got.LastDispatchSeq != fact.Seq {
		r.t.Fatalf("%s after the write of %s = phase %s, status %s, generation %d, seq %d: want phase %s, status %s and generation %d kept, seq %d",
			key, status, got.Phase, got.Status, got.Generation, got.LastDispatchSeq, before.Phase, before.Status, before.Generation, fact.Seq)
	}
	rows := r.sinceSettle()
	if writes, want := payloadsOf[record.StatusWrite](rowsFor(rows, key)), []record.StatusWrite{record.ReassertStatusWrite(before.Status, status, fact.Seq)}; !reflect.DeepEqual(writes, want) {
		r.t.Fatalf("status writes queued for %s after the write of %s = %+v, want the set-back alone, %+v", key, status, writes, want)
	}
	for _, op := range []record.SuperviseOp{"suspend", "issue_close", "tree_close"} {
		if roles := rolesOf(rows, op); len(roles) != 0 {
			r.t.Fatalf("%s requests after the write of %s on %s = %v, want none", op, status, key, roles)
		}
	}
	if suspends := payloadsOf[record.IssueSuspend](rows); len(suspends) != 0 {
		r.t.Fatalf("pod suspensions after the write of %s on %s = %+v, want none", status, key, suspends)
	}
	if releases := payloadsOf[record.ClaimRelease](rows); len(releases) != 0 {
		r.t.Fatalf("Dispatch claim releases after the write of %s on %s = %d, want none", status, key, len(releases))
	}
	if notices := childNotices(rows); len(notices) != 0 {
		r.t.Fatalf("notices after the write of %s on %s = %+v, want none", status, key, notices)
	}
}

// The architect that owns a child closes it in Dispatch, as a person can, and the daemon takes the
// close as it takes a person's: the child leaves the workflow at done, each of its claims closes,
// its pod goes, and the architect above it, the root's own here, hears child-closed. Nothing is
// set back. The child's implementer is in a turn, and its claim closes all the same, since a done
// ends the turn and the pod at once (supervise's issue_close).
func TestTheOwningArchitectsDoneOnAChildTakesItOutOfTheWorkflow(t *testing.T) {
	rig := seedOwnerTree(t, nil)
	rig.write(ownerChild, record.DoneStatus, rootArchitect)
	rig.assertLeft(ownerChild, record.DoneStatus)
	if owner := rig.noticeOwner(ownerChild, "child-closed"); owner != ownerRoot {
		t.Fatalf("the child-closed of %s goes to the architect of %s, want the architect above it, of %s", ownerChild, owner, ownerRoot)
	}
}

// The owning architect's backlog, icebox or triage on a child takes it out of the workflow as a
// person's does: each claim of the child is suspended with its session, for a later todo to
// resume, its pod goes, and the architect above it hears child-status.
func TestTheOwningArchitectsBacklogIceboxOrTriageOnAChildSuspendsItsClaims(t *testing.T) {
	for _, status := range []string{"backlog", "icebox", "triage"} {
		t.Run(status, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			rig.write(ownerChild, status, rootArchitect)
			rig.assertLeft(ownerChild, status)
		})
	}
}

// A sub-architect is the architect of an issue above the issues under its own, so its done on one
// of them stands while its claim was one the daemon published as its role's holder when Dispatch
// recorded the write, working or idle between turns alike (record.RoleHolderAt.PublishedAt). That
// holds of a sub-architect whose own claim is suspended after it wrote, since Dispatch judges the
// write by the holders published then (decision 24 of dispatch://INFRA-4730).
func TestASubArchitectsDoneOnAnIssueUnderItsOwnStands(t *testing.T) {
	at := fixedNow.Add(-time.Hour)
	entered, left := at.Add(-time.Hour), at.Add(5*time.Minute)
	for _, tc := range []struct {
		name                   string
		state                  supervise.ClaimState
		published, unpublished *time.Time
	}{
		{name: "its claim working", state: supervise.StateWorking, published: &entered},
		{name: "its claim idle", state: supervise.StateIdle, published: &entered},
		{name: "its own claim suspended after it wrote", state: supervise.StateSuspended, published: &entered, unpublished: &left},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			rig.setClaimCrossings(ownerChild, claim.RoleArchitect, tc.state, tc.published, tc.unpublished)
			rig.writtenAt(ownerGrandchild, record.DoneStatus, childArchitect, at)
			rig.assertLeft(ownerGrandchild, record.DoneStatus)
		})
	}
}

// The root's architect owns every issue of its tree, a grandchild under a running sub-architect
// included: the owner is the static rule of the tree's parents, not the architect that hears the
// grandchild's notices now.
func TestTheRootArchitectsDoneOnAGrandchildUnderARunningSubArchitectStands(t *testing.T) {
	rig := seedOwnerTree(t, nil)
	if owner := rig.noticeOwner(ownerGrandchild, "phase-finished"); owner != ownerChild {
		t.Fatalf("the notices of %s go to the architect of %s, want its running sub-architect, of %s: this test proves nothing", ownerGrandchild, owner, ownerChild)
	}
	rig.write(ownerGrandchild, record.DoneStatus, rootArchitect)
	rig.assertLeft(ownerGrandchild, record.DoneStatus)
}

// The exception is for a move out of the workflow alone. A status that moves the workflow, and
// todo, are the daemon's to write whoever owns the issue, so the owning architect's write of one is
// set back as any other session's of the tree.
func TestAStatusThatMovesTheWorkflowOrTodoIsSetBackFromTheOwningArchitect(t *testing.T) {
	for _, status := range []string{"in_progress", "testing", "needs_review", "retro", "todo"} {
		t.Run(status, func(t *testing.T) {
			rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
				// The write must change the status Dispatch showed, or it is an edit and no write.
				// The child has no issue under it here, so the settle leaves it testing.
				if status == "in_progress" {
					delete(issues, ownerGrandchild)
					child := issues[ownerChild]
					child.Phase, child.Status, child.DispatchStatus = phase.Testing, "testing", "testing"
				}
			})
			before := issue(t, rig.pool, ownerChild)
			fact := rig.write(ownerChild, status, rootArchitect)
			rig.assertSetBack(before, status, fact)
		})
	}
}

// A phase worker owns no issue, so any status it writes on a child is set back: the child's own
// worker, a sibling's and the root's alike.
func TestAWorkersStatusOnAChildIsSetBack(t *testing.T) {
	for _, writer := range []struct{ name, session string }{
		{"the child's own implementer", childImplementer},
		{"a sibling's implementer", siblingImplementer},
		{"the root's implementer", rootImplementer},
	} {
		for _, status := range []string{record.DoneStatus, "backlog"} {
			t.Run(writer.name+" writes "+status, func(t *testing.T) {
				rig := seedOwnerTree(t, nil)
				before := issue(t, rig.pool, ownerChild)
				fact := rig.write(ownerChild, status, writer.session)
				rig.assertSetBack(before, status, fact)
			})
		}
	}
}

// A child's own architect does not own its own issue: an architect above it ends the child, as one
// released it. So the child's architect's done or backlog on its own issue is set back.
func TestAChildsOwnArchitectsStatusOnItsOwnIssueIsSetBack(t *testing.T) {
	for _, status := range []string{record.DoneStatus, "backlog"} {
		t.Run(status, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			before := issue(t, rig.pool, ownerChild)
			fact := rig.write(ownerChild, status, childArchitect)
			rig.assertSetBack(before, status, fact)
		})
	}
}

// A sub-architect owns nothing outside its own issue's subtree: its done on its sibling, on the
// root and on an issue under its sibling is set back.
func TestAnArchitectsStatusOnAnIssueItDoesNotOwnIsSetBack(t *testing.T) {
	for _, target := range []struct{ name, key string }{
		{"its sibling", ownerSibling},
		{"the root", ownerRoot},
		{"an issue under its sibling", ownerNephew},
	} {
		t.Run(target.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			before := issue(t, rig.pool, target.key)
			fact := rig.write(target.key, record.DoneStatus, childArchitect)
			rig.assertSetBack(before, record.DoneStatus, fact)
		})
	}
}

// The owner's move is the child's status from its sequence on. An event of the child at or below
// that sequence that the consumer delivers late, here a person's todo that would run the child
// again, teaches the record nothing and changes nothing (the sequence fence of dispatchIssue).
func TestAnOlderEventOfTheChildAfterTheOwningArchitectsMoveChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behind int64
	}{
		{name: "at the move's sequence", behind: 0},
		{name: "below the move's sequence", behind: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			move := rig.write(ownerChild, "backlog", rootArchitect)
			rig.assertLeft(ownerChild, "backlog")
			before, rows := withoutMovement(issue(t, rig.pool, ownerChild)), effects(t, rig.pool)
			rig.writeAt(ownerChild, "todo", "", move.Seq-tc.behind)
			if got := withoutMovement(issue(t, rig.pool, ownerChild)); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s after the late todo = %#v, want the record the move left, %#v", ownerChild, got, before)
			}
			if got := effects(t, rig.pool); !reflect.DeepEqual(got, rows) {
				t.Fatalf("the outbox after the late todo = %#v, want the rows the move left, %#v", got, rows)
			}
		})
	}
}

// A later move of the same child by the same architect is taken in its turn, so a backlog and then
// a done end at done. The second leave finds the child already out of Legion's work, its phase
// kept (R20 of dispatch://INFRA-3320), and repeats only what is safe to repeat: a close of each
// claim, which retires a suspended claim, and a second pod suspension, which finds no Sandbox,
// beside the architect's child-closed. It opens no production check, since the child has no merged
// pull request. No issue stands under the child, so the child-closed names nothing after its first
// sentence and is compared whole. The rows are compared without the fence each stop and pod
// suspension carries (withoutStopFences), which is the issue's run and entry as the record holds
// them.
func TestTheOwningArchitectsBacklogThenDoneEndsAtDone(t *testing.T) {
	rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { delete(issues, ownerGrandchild) })
	rig.write(ownerChild, "backlog", rootArchitect)
	rig.assertLeft(ownerChild, "backlog")
	first := effects(t, rig.pool)
	rig.write(ownerChild, record.DoneStatus, rootArchitect)
	if got := issue(t, rig.pool, ownerChild); record.InLegionsWork(got) || got.Phase != phase.Implementing || got.Status != record.DoneStatus {
		t.Fatalf("%s after the backlog and the done = phase %s, status %s, want it out of Legion's work at done, its phase implementing kept", ownerChild, got.Phase, got.Status)
	}
	rows := effects(t, rig.pool)
	if len(rows) < len(first) || !reflect.DeepEqual(rows[:len(first)], first) {
		t.Fatalf("the outbox after the done = %#v, want the backlog's rows %#v first", rows, first)
	}
	reason := ownerChild + " is done"
	var want []effect
	for _, role := range claim.Roles {
		want = append(want, effect{kind: record.OutboxKindSupervise, issue: ownerChild,
			payload: record.SuperviseRequest{Op: "issue_close", Tree: ownerRoot, Role: role, Generation: 1, Reason: reason}})
	}
	want = append(want,
		effect{kind: record.OutboxKindIssueSuspend, issue: ownerChild, payload: record.IssueSuspend{Tree: ownerRoot, Generation: 1, TreeGeneration: 1}},
		effect{kind: record.OutboxKindNotice, issue: ownerChild, payload: record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: reason}})
	if added := withoutStopFences(rows[len(first):]); !reflect.DeepEqual(added, want) {
		t.Fatalf("the rows the done added = %#v, want %#v", added, want)
	}
	if check := rig.check(ownerChild); check != nil {
		t.Fatalf("the production check of %s = %+v, want none: it has no merged pull request", ownerChild, check)
	}
}

// withoutStopFences is rows with the phase entry and the pod cleared from each claim stop and pod
// suspension (record.FenceOf), the fence the issue stood at when the row was queued, so a row is
// compared by what it does.
func withoutStopFences(rows []effect) []effect {
	cleared := slices.Clone(rows)
	for i, row := range cleared {
		switch payload := row.payload.(type) {
		case record.SuperviseRequest:
			payload.Entry, payload.PodUID = 0, ""
			cleared[i].payload = payload
		case record.IssueSuspend:
			payload.Entry, payload.PodUID = 0, ""
			cleared[i].payload = payload
		}
	}
	return cleared
}

// A merged issue stands in merging while a Dispatch child of it is not done, and queues its close
// once the last of them is done (R24 of dispatch://INFRA-3320: Dispatch closes an issue only once
// each of its children is done). The owner's done of that last child is such a departure, so the
// merged issue above it queues its close on it, as on a person's done, and closes once Dispatch
// shows it done. The owner's backlog leaves the child not done, so the merged issue goes on
// waiting, as on a person's backlog. The events go through the copy of Dispatch's issues first, as
// the daemon's consumer applies them, since the wait reads the children's statuses there.
func TestTheOwningArchitectsMoveEndsTheWaitOfAMergedAncestor(t *testing.T) {
	for _, tc := range []struct {
		status string
		closes bool
	}{
		{status: record.DoneStatus, closes: true},
		{status: "backlog", closes: false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
				child := issues[ownerChild]
				child.Phase, child.Status, child.DispatchStatus = phase.Merging, "retro", "retro"
			})
			rig.pullRequest(ownerChild, 42, record.PullRequestMerged)
			rig.write(ownerGrandchild, tc.status, rootArchitect)
			rig.assertLeft(ownerGrandchild, tc.status)
			got := issue(t, rig.pool, ownerChild)
			closes := slices.ContainsFunc(payloadsOf[record.StatusWrite](rowsFor(effects(t, rig.pool), ownerChild)), func(write record.StatusWrite) bool {
				return write.Kind == record.CloseStatusRow && write.Status == record.DoneStatus
			})
			if got.Phase != phase.Merging || (got.PendingClose != nil) != tc.closes || closes != tc.closes {
				t.Fatalf("%s after the %s of the last child under it = phase %s, pending close %+v, close queued %t; want it in merging with its close queued %t",
					ownerChild, tc.status, got.Phase, got.PendingClose, closes, tc.closes)
			}
		})
	}
}

// assertCheckOpened holds that the owner's done on key, which added the rows added, opened key's
// production check on its merged pull request number: the check stands open at its first look,
// worked by the tester. Each claim of key but the tester's closes, and the tester gets the look's
// start. The pod stays for the look, so no pod suspension is queued, and no claim is suspended.
func (r *ownerRig) assertCheckOpened(key string, number int, added []effect) {
	r.t.Helper()
	if got := issue(r.t, r.pool, key); record.InLegionsWork(got) || got.Phase != r.before[key] || got.Status != record.DoneStatus {
		r.t.Fatalf("%s after the done = phase %s, status %s, want it out of Legion's work at done, its phase %s kept", key, got.Phase, got.Status, r.before[key])
	}
	check := r.check(key)
	if check == nil || !check.Open() || check.Looks != 1 || check.Role != claim.RoleTester || check.Number != number || check.MergeSHA != "merge-"+key {
		r.t.Fatalf("the production check of %s = %+v, want it open at look 1 for the tester, on #%d at merge-%s", key, check, number, key)
	}
	added = rowsFor(added, key)
	want := slices.DeleteFunc(slices.Clone(claim.Roles), func(role claim.Role) bool { return role == claim.RoleTester })
	if got := rolesOf(added, "issue_close"); !reflect.DeepEqual(got, want) {
		r.t.Fatalf("issue_close requests the done added for %s = %v, want one for each role but the tester, %v", key, got, want)
	}
	if got := rolesOf(added, "suspend"); len(got) != 0 {
		r.t.Fatalf("suspend requests the done added for %s = %v, want none", key, got)
	}
	if suspends := payloadsOf[record.IssueSuspend](added); len(suspends) != 0 {
		r.t.Fatalf("pod suspensions the done added for %s = %+v, want none while the check looks", key, suspends)
	}
	var looks []record.SuperviseRequest
	for _, request := range payloadsOf[record.SuperviseRequest](added) {
		if request.Op == "start" {
			looks = append(looks, request)
		}
	}
	if len(looks) != 1 || looks[0].Role != claim.RoleTester || looks[0].CheckLook != 1 || looks[0].Task == "" {
		r.t.Fatalf("starts the done added for %s = %+v, want the tester's first look alone", key, looks)
	}
}

// A done on a child whose pull request merged opens the child's production check, as the close at
// its merge would: whether the child still runs, or the owner already took it out of the workflow
// and the pull request merged after that move.
func TestTheOwningArchitectsDoneOnAChildWhosePullRequestMergedOpensItsCheck(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.pullRequest(ownerChild, 42, record.PullRequestOpen)
		rig.merge(ownerChild, 42)
		if got, check := issue(t, rig.pool, ownerChild), rig.check(ownerChild); !record.InLegionsWork(got) || check != nil {
			t.Fatalf("%s after its merge at phase %s = check %+v, want it still in Legion's work and no check: this test proves nothing", ownerChild, got.Phase, check)
		}
		before := effects(t, rig.pool)
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertCheckOpened(ownerChild, 42, effects(t, rig.pool)[len(before):])
	})
	t.Run("already out of the workflow", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.pullRequest(ownerChild, 42, record.PullRequestOpen)
		rig.write(ownerChild, "backlog", rootArchitect)
		rig.assertLeft(ownerChild, "backlog")
		rig.merge(ownerChild, 42)
		if got, check := issue(t, rig.pool, ownerChild), rig.check(ownerChild); got.Phase != rig.before[ownerChild] || got.Status != "backlog" || check != nil {
			t.Fatalf("%s after its merge at backlog = phase %s, status %s, check %+v, want it at backlog in its phase %s with no check", ownerChild, got.Phase, got.Status, check, rig.before[ownerChild])
		}
		before := effects(t, rig.pool)
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertCheckOpened(ownerChild, 42, effects(t, rig.pool)[len(before):])
	})
}

// A move of a child ends the child alone. Each recorded issue under it stays as it is: the one
// still in the workflow runs on, with its claims, its pod and its slot, and the ones already out
// of the workflow stay out. The notice to the architect above names each one that is not done,
// with its status, so that architect can write a status on each one the move also ends.
func TestAnIssueUnderTheChildRunsOnAfterTheOwningArchitectsDone(t *testing.T) {
	const atBacklog, done = "LEGION-6", "LEGION-7"
	rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
		issues[atBacklog] = ownedMember(atBacklog, ownerChild, "F", phase.Done, "backlog")
		issues[done] = ownedMember(done, ownerChild, "G", phase.Done, record.DoneStatus)
	})
	under := []string{ownerGrandchild, atBacklog, done}
	before := map[string]record.Issue{}
	for _, key := range under {
		before[key] = withoutMovement(issue(t, rig.pool, key))
	}
	rig.write(ownerChild, record.DoneStatus, rootArchitect)
	rig.assertLeft(ownerChild, record.DoneStatus)
	rows := effects(t, rig.pool)
	want := ownerChild + " is done. Under it, not done: " + ownerGrandchild + " (in_progress), " + atBacklog + " (backlog)"
	if notices := childNotices(rowsFor(rows, ownerChild)); len(notices) != 1 || notices[0].Reason != want {
		t.Fatalf("the notices of the done of %s = %+v, want one reading %q", ownerChild, notices, want)
	}
	for _, key := range under {
		if got := withoutMovement(issue(t, rig.pool, key)); !reflect.DeepEqual(got, before[key]) {
			t.Fatalf("%s under %s after its done = %#v, want it as it was, %#v", key, ownerChild, got, before[key])
		}
		of := rowsFor(rows, key)
		for _, op := range []record.SuperviseOp{"suspend", "issue_close", "tree_close"} {
			if roles := rolesOf(of, op); len(roles) != 0 {
				t.Fatalf("%s requests for %s under %s after its done = %v, want none", op, key, ownerChild, roles)
			}
		}
		if suspends := payloadsOf[record.IssueSuspend](of); len(suspends) != 0 {
			t.Fatalf("pod suspensions of %s under %s after its done = %+v, want none", key, ownerChild, suspends)
		}
	}
	if !slices.ContainsFunc(slots(t, rig.pool), func(slot record.Slot) bool { return slot.Issue == ownerGrandchild }) {
		t.Fatalf("slots after the done of %s = %+v, want %s still holding its own", ownerChild, slots(t, rig.pool), ownerGrandchild)
	}
}

// An issue blocked_by the child waits on it as Dispatch shows it. The owner's done finishes the
// child, so a root that waits on nothing else is admitted on that event. A backlog does not finish
// it, so that root waits on.
func TestADoneChildStartsItsDependentsAndABacklogChildKeepsThemWaiting(t *testing.T) {
	const dependent = "LEGION-9"
	for _, tc := range []struct {
		status string
		starts bool
	}{
		{status: record.DoneStatus, starts: true},
		{status: "backlog", starts: false},
	} {
		t.Run(tc.status, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			seedWaiting(t, rig.pool, dependent, "Z")
			seedMirror(t, rig.pool, dependent, "todo", ownerChild)
			seedMirror(t, rig.pool, ownerChild, "in_progress")
			rig.write(ownerChild, tc.status, rootArchitect)
			rig.assertLeft(ownerChild, tc.status)
			slotted := slices.ContainsFunc(slots(t, rig.pool), func(slot record.Slot) bool { return slot.Issue == dependent })
			waiting := slices.Contains(waitingLine(t, rig.pool, testProject, nil), dependent)
			if slotted != tc.starts || waiting == tc.starts {
				t.Fatalf("%s, blocked by %s, after its %s: slotted %t, waiting %t, want slotted %t", dependent, ownerChild, tc.status, slotted, waiting, tc.starts)
			}
		})
	}
}

// A child its merge closed has its production check open. The owner's backlog on it, which the
// architect's texts tell it not to write, gets what a person's backlog on the same record gets: the
// checking role's claim is suspended with the rest and the check's pod goes.
func TestTheOwningArchitectsMoveOfAChildItsMergeClosedIsAPersonsMove(t *testing.T) {
	type outcome struct {
		child record.Issue
		check *record.ProductionCheck
		rows  []effect
	}
	move := func(session string) (outcome, []effect) {
		rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
			delete(issues, ownerGrandchild)
			child := issues[ownerChild]
			child.Phase, child.Status, child.DispatchStatus = phase.Merging, "retro", "retro"
		})
		rig.pullRequest(ownerChild, 42, record.PullRequestOpen)
		rig.merge(ownerChild, 42)
		rig.deliverStatusWrites(ownerChild)
		rig.echo(ownerChild, record.DoneStatus)
		if got, check := issue(t, rig.pool, ownerChild), rig.check(ownerChild); got.Phase != phase.Done || got.Status != record.DoneStatus || check == nil || !check.Open() {
			t.Fatalf("%s after its merge = phase %s, status %s, check %+v, want it closed at done with its check open: this test proves nothing", ownerChild, got.Phase, got.Status, check)
		}
		before := effects(t, rig.pool)
		rig.write(ownerChild, "backlog", session)
		rows := effects(t, rig.pool)
		return outcome{child: withoutMovement(issue(t, rig.pool, ownerChild)), check: rig.check(ownerChild), rows: rows}, rows[len(before):]
	}
	person, added := move("")
	if !slices.Contains(rolesOf(rowsFor(added, ownerChild), "suspend"), claim.RoleTester) || len(payloadsOf[record.IssueSuspend](rowsFor(added, ownerChild))) != 1 {
		t.Fatalf("the rows a person's backlog on %s added = %#v, want the tester's claim suspended and the pod suspended: the comparison would be of two writes that did nothing", ownerChild, added)
	}
	if owner, _ := move(rootArchitect); !reflect.DeepEqual(owner, person) {
		t.Fatalf("the owning architect's backlog on %s, closed by its merge, left %+v, want what a person's left, %+v", ownerChild, owner, person)
	}
}

// moveOutcome is what a move leaves: every record of the tree, the slots and every queued row.
type moveOutcome struct {
	issues  map[string]record.Issue
	slots   []record.Slot
	effects []effect
}

// outcome reads what the moves on rig left.
func (r *ownerRig) outcome() moveOutcome {
	r.t.Helper()
	got := moveOutcome{issues: map[string]record.Issue{}, slots: slots(r.t, r.pool), effects: effects(r.t, r.pool)}
	rows, err := r.pool.Query(context.Background(), `select key from issues`)
	if err != nil {
		r.t.Fatalf("list the issues: %v", err)
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.t.Fatalf("list the issues: %v", err)
	}
	for _, key := range keys {
		got.issues[key] = withoutMovement(issue(r.t, r.pool, key))
	}
	return got
}

// The root left Legion's work at done while its architect's claim still runs, so the child under
// it is a root of its own now (R15 of dispatch://INFRA-3320). The root's architect is still the
// architect of the issue above the child on its Dispatch chain, and its move of the child gets what
// a person's move of that child gets on the same record, as Dispatch takes it: the repair reads the
// chain, whatever of it is in Legion's work (decision 24 of dispatch://INFRA-4730).
func TestTheOwningArchitectsMoveUnderAParentOutOfWorkIsAPersonsMove(t *testing.T) {
	for _, status := range []string{record.DoneStatus, "backlog"} {
		t.Run(status, func(t *testing.T) {
			move := func(session string) moveOutcome {
				rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
					root := issues[ownerRoot]
					root.Phase, root.Status, root.DispatchStatus = phase.Done, record.DoneStatus, record.DoneStatus
				})
				rig.write(ownerChild, status, session)
				return rig.outcome()
			}
			person := move("")
			if got := person.issues[ownerChild]; record.InLegionsWork(got) || got.Status != status {
				t.Fatalf("%s after a person's %s = phase %s, status %s, want it out of Legion's work: the comparison would be of two writes that did nothing", ownerChild, status, got.Phase, got.Status)
			}
			if owner := move(rootArchitect); !reflect.DeepEqual(owner, person) {
				t.Fatalf("the owning architect's %s on %s under a parent out of Legion's work left %+v, want what a person's left, %+v", status, ownerChild, owner, person)
			}
		})
	}
}

// The tree the moved-child case adds: a root of its own, whose architect holds a live claim, and
// the child the daemon first recorded under it, which a person then moved in Dispatch under
// ownerChild. The child's record keeps the tree it was first recorded under (issues.tree, R22 of
// dispatch://INFRA-3320), and its parent is the one the copy of Dispatch shows.
const (
	movedOldRoot     = "LEGION-6"
	movedChild       = "LEGION-7"
	oldRootArchitect = "ses-old-root-architect"
)

// The repair takes a session's done, backlog, icebox or triage as a person's move where the session
// held a claim the daemon published as a role holder (its state and suspension source holding its
// role, supervise.HoldsRole, its session held) when Dispatch recorded the write
// (record.RoleHolderAt.PublishedAt), as the architect of an issue above the written issue on its
// Dispatch chain, the parent's and the grandparent's alike, whether or not the written issue or the
// issues above it are in Legion's work, so that architect's backlog and then done ends at done
// (decision 24 of dispatch://INFRA-4730, INFRA-3178's R3). It reads the chain the copy of Dispatch
// holds, never the tree an issue's record stores, so the architect above a child moved under it in
// Dispatch takes it whatever tree the child was first recorded under. It sets back the write of the
// issue's own architect, of a sibling's architect, of another role of the parent and of an
// architect whose claim had failed, or was suspended by any source but the idle release, when the
// write was made, as every other write of an agent of the tree. A claim no crossing has stamped
// since this release began is published at any time where it is published now; where it is not,
// it was published only before the project's conversion (tree_conversions.converted_at), and never
// where no conversion is recorded.
//
// A write on an issue whose chain is unknown is held (record.Issue.HeldSessionWrites), and so is
// each later session write, until the chain is known; then the list is judged in Dispatch's order,
// at the pass that knows the chain (here the workflow's own call, Engine.JudgeHeldStatusWrites) or
// at once where a later session write meets a known chain with writes still held. The architect
// above's done then the issue's own architect's backlog end at done with one write of done at the
// backlog's sequence; its backlog then its done end at done with nothing written; two writes the
// repair sets back end at the record's own status with one write of it. A person's later write
// clears the list and stands, and the daemon's own echo leaves it.
//
// Negative controls, run when this was written, each failing this test and then reverted: with the
// repair taking the architect's write only while the written issue is in Legion's work, "an issue
// out of Legion's work: the architect's backlog and then done" ends at backlog; with it reading the
// issue's stored tree (issues.tree) instead of the Dispatch chain, the architect above the moved
// child is set back; with every claim whose published_since is null judged by the conversion's
// time, the claim published since before this release and suspended after the write is set back;
// with that judgment made of a claim published now too, the working claim written after the
// conversion is set back; and with a claim no crossing stamped never published, the suspended
// claim written before the conversion is set back.
func TestTheRepairTakesTheWriteOfAnArchitectAboveOnTheChain(t *testing.T) {
	// The claim is judged as it stood when Dispatch recorded the write (intake.DispatchIssue.At),
	// by the times it last entered and left the published states, whatever state it stands in
	// when the write is applied.
	at := fixedNow.Add(-time.Hour)
	before, after := func(d time.Duration) *time.Time { v := at.Add(-d); return &v }, func(d time.Duration) *time.Time { v := at.Add(d); return &v }
	for _, tc := range []struct {
		state                  supervise.ClaimState
		published, unpublished *time.Time
		stands                 bool
	}{
		{state: supervise.StateRegistered, published: before(time.Hour), stands: true},
		{state: supervise.StateReady, published: before(time.Hour), stands: true},
		{state: supervise.StateWorking, published: before(time.Hour), stands: true},
		{state: supervise.StateIdle, published: before(time.Hour), stands: true},
		{state: supervise.StateFailed, published: before(time.Hour), unpublished: before(5 * time.Minute)},
	} {
		t.Run("the parent's architect, its claim "+string(tc.state), func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			rig.setClaimCrossings(ownerRoot, claim.RoleArchitect, tc.state, tc.published, tc.unpublished)
			before := issue(t, rig.pool, ownerChild)
			fact := rig.writtenAt(ownerChild, record.DoneStatus, rootArchitect, at)
			if tc.stands {
				rig.assertLeft(ownerChild, record.DoneStatus)
				return
			}
			rig.assertSetBack(before, record.DoneStatus, fact)
		})
	}
	for _, tc := range []struct {
		name                   string
		state                  supervise.ClaimState
		published, unpublished *time.Time
		// converted is when the project's conversion ended, nil where none is recorded.
		converted *time.Time
		// untimed is a write whose event carries no Dispatch time, judged by the claim as it
		// stands when the write is applied.
		untimed bool
		stands  bool
	}{
		{name: "written before its suspension and applied after it", state: supervise.StateSuspended,
			published: before(time.Hour), unpublished: after(5 * time.Minute), stands: true},
		{name: "published since before this release, written before its suspension and after the conversion", state: supervise.StateSuspended,
			unpublished: after(5 * time.Minute), converted: before(2 * time.Hour), stands: true},
		{name: "written after its suspension", state: supervise.StateSuspended,
			published: before(time.Hour), unpublished: before(5 * time.Minute)},
		{name: "left its published states after the write and back before it is applied", state: supervise.StateWorking,
			published: after(10 * time.Minute), unpublished: after(5 * time.Minute), stands: true},
		{name: "entered and left its published states both after the write", state: supervise.StateSuspended,
			published: after(5 * time.Minute), unpublished: after(10 * time.Minute)},
		{name: "not published when the write was made, registered before it is applied", state: supervise.StateRegistered,
			published: after(5 * time.Minute)},
		{name: "left its published states before the write, registered again before it is applied", state: supervise.StateRegistered,
			published: after(5 * time.Minute), unpublished: before(10 * time.Minute)},
		{name: "a write with no Dispatch time, suspended before it is applied", state: supervise.StateSuspended,
			published: before(time.Hour), unpublished: after(5 * time.Minute), untimed: true},
		{name: "working with no crossing stamped, written before the conversion", state: supervise.StateWorking,
			converted: after(5 * time.Minute), stands: true},
		{name: "working with no crossing stamped, written after the conversion", state: supervise.StateWorking,
			converted: before(5 * time.Minute), stands: true},
		{name: "suspended with no crossing stamped, written before the conversion", state: supervise.StateSuspended,
			converted: after(5 * time.Minute), stands: true},
		{name: "suspended with no crossing stamped, written after the conversion", state: supervise.StateSuspended,
			converted: before(5 * time.Minute)},
		{name: "suspended with no crossing stamped and no conversion recorded", state: supervise.StateSuspended},
	} {
		t.Run("the parent's architect, "+tc.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			rig.setClaimCrossings(ownerRoot, claim.RoleArchitect, tc.state, tc.published, tc.unpublished)
			if tc.converted != nil {
				rig.convertedAt(*tc.converted)
			}
			before := issue(t, rig.pool, ownerChild)
			writtenAt := at
			if tc.untimed {
				writtenAt = time.Time{}
			}
			fact := rig.writtenAt(ownerChild, record.DoneStatus, rootArchitect, writtenAt)
			if tc.stands {
				rig.assertLeft(ownerChild, record.DoneStatus)
				return
			}
			rig.assertSetBack(before, record.DoneStatus, fact)
		})
	}
	t.Run("the parent's architect, a sub-architect", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.write(ownerGrandchild, "icebox", childArchitect)
		rig.assertLeft(ownerGrandchild, "icebox")
	})
	t.Run("the grandparent's architect", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.write(ownerGrandchild, "triage", rootArchitect)
		rig.assertLeft(ownerGrandchild, "triage")
	})
	t.Run("an issue out of Legion's work: the architect's backlog and then done", func(t *testing.T) {
		rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { delete(issues, ownerGrandchild) })
		rig.write(ownerChild, "backlog", rootArchitect)
		rig.assertLeft(ownerChild, "backlog")
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertOutAt(ownerChild, record.DoneStatus)
	})
	t.Run("an issue out of Legion's work: a person's backlog and then the architect's done", func(t *testing.T) {
		rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { delete(issues, ownerGrandchild) })
		rig.write(ownerChild, "backlog", "")
		rig.assertLeft(ownerChild, "backlog")
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertOutAt(ownerChild, record.DoneStatus)
	})
	t.Run("an issue under a parent out of Legion's work", func(t *testing.T) {
		rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
			root := issues[ownerRoot]
			root.Phase, root.Status, root.DispatchStatus = phase.Done, record.DoneStatus, record.DoneStatus
		})
		rig.write(ownerChild, "backlog", rootArchitect)
		rig.assertOutAt(ownerChild, "backlog")
	})
	t.Run("a child moved under another architect's issue, its stored tree the old one", func(t *testing.T) {
		rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
			old := ownedMember(movedOldRoot, "", "F", phase.Implementing, "in_progress")
			old.Tree = movedOldRoot
			moved := ownedMember(movedChild, ownerChild, "G", phase.Implementing, "in_progress")
			moved.Tree = movedOldRoot
			issues[movedOldRoot], issues[movedChild] = old, moved
		})
		seedSessionClaims(t, rig.pool, ownerClaim{tree: movedOldRoot, issue: movedOldRoot, role: claim.RoleArchitect,
			session: oldRootArchitect, state: supervise.StateWorking})
		if got := issue(t, rig.pool, movedChild); got.Tree != movedOldRoot || deref(got.Parent) != ownerChild {
			t.Fatalf("%s = tree %s, parent %s; want the tree it was first recorded under, %s, and the parent Dispatch shows, %s: this case proves nothing",
				movedChild, got.Tree, deref(got.Parent), movedOldRoot, ownerChild)
		}
		rig.write(movedChild, record.DoneStatus, childArchitect)
		rig.assertOutAt(movedChild, record.DoneStatus)
	})
	for _, writer := range []struct{ name, session string }{
		{"the issue's own architect", childArchitect},
		{"a sibling's architect", siblingArchitect},
		{"another role of the parent", rootImplementer},
	} {
		t.Run(writer.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			before := issue(t, rig.pool, ownerChild)
			fact := rig.write(ownerChild, "backlog", writer.session)
			rig.assertSetBack(before, "backlog", fact)
		})
	}
	t.Run("held on an unknown chain: the architect above's done, then the issue's own architect's backlog", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		done := rig.write(ownerChild, record.DoneStatus, rootArchitect)
		backlog := rig.write(ownerChild, "backlog", childArchitect)
		rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq},
			record.HeldSessionWrite{Session: childArchitect, Status: "backlog", Seq: backlog.Seq})
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, record.DoneStatus, record.ReassertStatusWrite(record.DoneStatus, "backlog", backlog.Seq))
	})
	t.Run("held on an unknown chain: the architect above's done, then on the known chain the issue's own architect's backlog", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		done := rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq})
		rig.findChainTop()
		backlog := rig.write(ownerChild, "backlog", childArchitect)
		rig.assertJudged(ownerChild, record.DoneStatus, record.ReassertStatusWrite(record.DoneStatus, "backlog", backlog.Seq))
	})
	t.Run("held on an unknown chain: the architect above's backlog, then its done", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		rig.write(ownerChild, "backlog", rootArchitect)
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, record.DoneStatus)
	})
	t.Run("held on an unknown chain: a worker's backlog, then the architect above's done", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		rig.write(ownerChild, "backlog", childImplementer)
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, record.DoneStatus)
	})
	t.Run("held on an unknown chain: a worker's done, then the issue's own architect's backlog", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		before := issue(t, rig.pool, ownerChild)
		rig.loseChainTop()
		rig.write(ownerChild, record.DoneStatus, childImplementer)
		backlog := rig.write(ownerChild, "backlog", childArchitect)
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, before.Status, record.ReassertStatusWrite(before.Status, "backlog", backlog.Seq))
		if got := issue(t, rig.pool, ownerChild); got.Phase != before.Phase || got.Generation != before.Generation {
			t.Fatalf("%s after two writes the repair sets back = phase %s, generation %d; want phase %s and generation %d kept", ownerChild, got.Phase, got.Generation, before.Phase, before.Generation)
		}
	})
	t.Run("held on an unknown chain: a person's later write clears the list and stands", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.write(ownerChild, "backlog", "")
		rig.assertHeld(ownerChild)
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, "backlog")
	})
	t.Run("held on an unknown chain: the daemon's own echo leaves the list", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		done := rig.write(ownerChild, record.DoneStatus, rootArchitect)
		before := issue(t, rig.pool, ownerChild)
		rig.echo(ownerChild, before.Status)
		rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq})
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, record.DoneStatus, record.ReassertStatusWrite(record.DoneStatus, before.Status, done.Seq))
	})
	t.Run("held on an unknown chain: the architect above's done written before its suspension, judged after it", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.loseChainTop()
		done := rig.writtenAt(ownerChild, record.DoneStatus, rootArchitect, at)
		rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq, At: at})
		rig.setClaimCrossings(ownerRoot, claim.RoleArchitect, supervise.StateSuspended, before(time.Hour), after(5*time.Minute))
		rig.findChainTop()
		rig.judgeHeld(ownerChild)
		rig.assertJudged(ownerChild, record.DoneStatus)
	})
	// The legion label of the chain stands above a gap the copy cannot read past, so whether the
	// write is anyone's to make is not known either, and the write is held with every other
	// (sessionStatusWrite). Negative control, run when this was written: with the label read
	// before the chain's knowledge, the unlabelled child's write is taken out of Legion's work.
	t.Run("held on an unknown chain: an unlabelled child whose parent's row the copy lacks", func(t *testing.T) {
		rig := seedOwnerTree(t, nil)
		rig.putCopyRow(record.DispatchReading{Key: ownerChild, Project: testProject, Parent: record.Carry(missingTop)})
		if chain := rig.chainOf(ownerChild); chain.Known() || len(chain.Links) != 0 {
			t.Fatalf("the chain of %s = %+v, want it unknown at its parent, with no link read: this case proves nothing", ownerChild, chain)
		}
		done := rig.write(ownerChild, record.DoneStatus, rootArchitect)
		rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq})
	})
}

// The missing top of the chain the held-write cases lose and find again: ownerRoot's parent in
// the copy of Dispatch, an issue the copy holds no row of until a reading of it arrives.
const missingTop = "LEGION-99"

// loseChainTop makes the copy of Dispatch name missingTop as ownerRoot's parent, a row it does not
// hold, so every chain of the tree is unknown (R16 of dispatch://INFRA-3320), while the rows it
// reads below the gap, the root's legion label among them, stand.
func (r *ownerRig) loseChainTop() {
	r.t.Helper()
	r.putCopyRow(record.DispatchReading{Key: ownerRoot, Project: testProject, Parent: record.Carry(missingTop)})
	if chain := r.chainOf(ownerChild); chain.Known() {
		r.t.Fatalf("the chain of %s = %+v, want it unknown: these cases prove nothing", ownerChild, chain)
	}
}

// findChainTop writes missingTop's row into the copy with no parent, as a reading of it would, and
// with no event, so no pass runs: the chains of the tree are known again.
func (r *ownerRig) findChainTop() {
	r.t.Helper()
	r.putCopyRow(record.DispatchReading{Key: missingTop, Project: testProject, Status: record.Carry("backlog"),
		Parent: record.Carry(""), Labels: record.Carry([]string{})})
	if chain := r.chainOf(ownerChild); !chain.Known() {
		r.t.Fatalf("the chain of %s = %+v, want it known", ownerChild, chain)
	}
}

// putCopyRow writes reading into the copy of Dispatch's issues at a sequence above every one the
// copy holds for its key.
func (r *ownerRig) putCopyRow(reading record.DispatchReading) {
	r.t.Helper()
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		if err := tx.QueryRow(context.Background(), `select coalesce(max(greatest(status_seq, parent_seq, labels_seq)), 0) + 1 from dispatch_issues where key = $1`,
			reading.Key).Scan(&reading.Seq); err != nil {
			r.t.Fatalf("read the copy's sequences of %s: %v", reading.Key, err)
		}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, reading); err != nil {
			r.t.Fatalf("write the copy's row of %s: %v", reading.Key, err)
		}
	})
}

// chainOf is key's Dispatch chain as the copy holds it.
func (r *ownerRig) chainOf(key string) record.Chain {
	r.t.Helper()
	var chain record.Chain
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		chains, err := record.ReadChains(context.Background(), tx, record.NewStore(), testProject, []string{key})
		if err != nil {
			r.t.Fatalf("read the chain of %s: %v", key, err)
		}
		chain = chains.Chain(key)
	})
	return chain
}

// judgeHeld has the workflow judge the session writes key holds, in a transaction of its own
// (store.Transact), as admission's pass calls it (workflow's Engine.JudgeHeldStatusWrites), and
// fails unless the chain was known to judge them on.
func (r *ownerRig) judgeHeld(key string) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) error {
		ctx = r.engine.OpenFact(ctx)
		held, err := record.NewStore().Issue(ctx, tx, key)
		if err != nil || held == nil {
			return fmt.Errorf("read %s: %v, %v", key, held, err)
		}
		chains, err := record.ReadChainsOf(ctx, tx, record.NewStore(), testProject, []string{key})
		if err != nil {
			return fmt.Errorf("read the chain of %s: %w", key, err)
		}
		known, err := r.engine.JudgeHeldStatusWrites(ctx, tx, chains, *held)
		if err != nil || !known {
			return fmt.Errorf("judge the writes %s holds = known %t, %v; want them judged on its known chain", key, known, err)
		}
		if err := r.engine.SettleFact(ctx, tx); err != nil {
			return fmt.Errorf("settle: %w", err)
		}
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

// assertHeld holds that key's record holds want, in order, and that nothing of them was taken or
// set back yet: the record keeps the status the tree's settle left it at, and no status write of
// the daemon's is queued for it since.
func (r *ownerRig) assertHeld(key string, want ...record.HeldSessionWrite) {
	r.t.Helper()
	got := issue(r.t, r.pool, key)
	if !reflect.DeepEqual(got.HeldSessionWrites, want) && (len(want) > 0 || len(got.HeldSessionWrites) > 0) {
		r.t.Fatalf("session writes %s holds = %+v, want %+v", key, got.HeldSessionWrites, want)
	}
	if len(want) == 0 {
		return
	}
	if !record.InLegionsWork(got) || got.Status != "in_progress" {
		r.t.Fatalf("%s while it holds writes = phase %s, status %s; want it still in Legion's work at in_progress", key, got.Phase, got.Status)
	}
	if writes := payloadsOf[record.StatusWrite](rowsFor(r.sinceSettle(), key)); len(writes) != 0 {
		r.t.Fatalf("status writes queued for %s while it holds writes = %+v, want none", key, writes)
	}
}

// assertJudged holds that key's held writes were judged: its record holds none, stands at status,
// out of Legion's work where status takes it out, and the status writes queued for it since the
// tree's settle are want, in order: at most the one write of the record's final status.
func (r *ownerRig) assertJudged(key, status string, want ...record.StatusWrite) {
	r.t.Helper()
	got := issue(r.t, r.pool, key)
	if len(got.HeldSessionWrites) != 0 || got.Status != status || record.OutOfWorkflow(status) && record.InLegionsWork(got) {
		r.t.Fatalf("%s once its held writes are judged = phase %s, status %s, held %+v; want status %s and none held", key, got.Phase, got.Status, got.HeldSessionWrites, status)
	}
	writes := payloadsOf[record.StatusWrite](rowsFor(r.sinceSettle(), key))
	if len(writes) != 0 || len(want) != 0 {
		if !reflect.DeepEqual(writes, want) {
			r.t.Fatalf("status writes queued for %s once its held writes are judged = %+v, want %+v", key, writes, want)
		}
	}
}

// A session's write on an issue whose Dispatch chain the copy cannot read to its top is held, and
// so is each later one (record.Issue.HeldSessionWrites), until the first admission pass whose copy
// knows the chain judges them in Dispatch's order and clears them (decision 24 of
// dispatch://INFRA-4730): here the pass the event of the missing link runs, which leaves the
// writes to it. The architect above's done is taken, and the issue's own architect's later backlog
// is set back to the done the first left, with one write of done at the backlog's sequence. The
// daemon's own echo of the issue in between leaves the list.
//
// Negative control, run when this was written: with the pass dropping the held writes unjudged,
// the issue stays in Legion's work and this test fails.
func TestAHeldStatusWriteIsJudgedAtThePassThatKnowsItsChain(t *testing.T) {
	rig := seedOwnerTree(t, nil)
	rig.loseChainTop()
	done := rig.write(ownerChild, record.DoneStatus, rootArchitect)
	backlog := rig.write(ownerChild, "backlog", childArchitect)
	rig.echo(ownerChild, issue(t, rig.pool, ownerChild).Status)
	rig.assertHeld(ownerChild, record.HeldSessionWrite{Session: rootArchitect, Status: record.DoneStatus, Seq: done.Seq},
		record.HeldSessionWrite{Session: childArchitect, Status: "backlog", Seq: backlog.Seq})
	rig.applyEvent("the-missing-top", intake.DispatchIssue{Key: missingTop, ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated",
		Status: "backlog", Title: missingTop, Rank: "Z"})
	rig.assertJudged(ownerChild, record.DoneStatus, record.ReassertStatusWrite(record.DoneStatus, "in_progress", backlog.Seq))
	if notices := childNotices(rowsFor(rig.sinceSettle(), ownerChild)); len(notices) != 1 || notices[0].Kind != "child-closed" {
		t.Fatalf("notices of %s once its held writes are judged = %+v, want the child-closed of the done alone", ownerChild, notices)
	}
}

// assertOutAt holds that a write took key out of Legion's work, or kept it out, at status: its
// record holds status out of Legion's work, and no status write of the daemon's is queued for it
// after the tree's settle, so nothing sets the write back.
func (r *ownerRig) assertOutAt(key, status string) {
	r.t.Helper()
	if got := issue(r.t, r.pool, key); record.InLegionsWork(got) || got.Status != status {
		r.t.Fatalf("%s after the write of %s = phase %s, status %s; want it out of Legion's work at %s", key, status, got.Phase, got.Status, status)
	}
	if writes := payloadsOf[record.StatusWrite](rowsFor(r.sinceSettle(), key)); len(writes) != 0 {
		r.t.Fatalf("status writes queued for %s after the write of %s = %+v, want none: the daemon sets nothing back over the move", key, status, writes)
	}
}
