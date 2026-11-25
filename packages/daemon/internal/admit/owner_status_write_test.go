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
// finds a writer by (record.Store.SessionClaimsTree).
func seedSessionClaims(t *testing.T, pool *pgxpool.Pool, claims ...ownerClaim) {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		for _, c := range claims {
			token := ownerToken(t, c.issue, c.role)
			var turn *time.Time
			if c.turn {
				turn = &fixedNow
			}
			if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
				session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, turn_started_at)
				values ($1, $2, $3, $4, $5, 1, $6, $7, $8, 0, 0, 0, 0, $9)`,
				string(token), project, c.tree, c.issue, string(c.role), c.session, "/tmp/"+c.session+".jsonl", string(c.state), turn); err != nil {
				t.Fatalf("put the %s claim of %s: %v", c.role, c.issue, err)
			}
		}
	})
}

// ownerToken is the token of the claim of role on issue, as seedSessionClaims stores it.
func ownerToken(t *testing.T, issue string, role claim.Role) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, issue, role)
	if err != nil {
		t.Fatalf("the %s claim token of %s: %v", role, issue, err)
	}
	return token
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
	// mirrored applies each event through the blockers mirror first, as the daemon does, for a
	// test whose outcome reads what Dispatch shows of a blocker.
	mirrored bool
}

// seedOwnerTree records the tree with every issue in implementing at in_progress, and the claims of
// treeClaims. Each issue that keeps a slot holds one, under a cap above all of them, so a slot that
// a move gives back starts nothing the test did not seed. The project checks in production with
// the tester. shape changes the records before they are written: it can add a record, or delete
// one.
func seedOwnerTree(t *testing.T, shape func(map[string]*record.Issue)) *ownerRig {
	t.Helper()
	pool := migratedPool(t)
	rig := &ownerRig{t: t, pool: pool, admission: newAdmission(t, 10, slog.New(slog.NewTextHandler(io.Discard, nil))),
		engine: workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
			Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, ProductionCheckRole: claim.RoleTester,
			ProductionCheckBound: 48 * time.Hour, Clock: func() time.Time { return fixedNow }}, nil)}
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
		}
		if issues[ownerRoot].Lingers() {
			return
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
	return rig
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
	recorded := issue(r.t, r.pool, key)
	kind := "issue.updated"
	if status == record.DoneStatus {
		kind = "issue.closed"
	}
	fact := intake.DispatchIssue{Key: key, ResolvedRepo: recorded.Repo, Seq: seq, Type: kind, Status: status,
		Title: recorded.Title, Rank: recorded.Rank, Parent: deref(recorded.Parent), HandedOver: recorded.HandedOver,
		ActorSession: session}
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

func (r *ownerRig) applyEvent(eventID string, fact intake.DispatchIssue) {
	r.t.Helper()
	if r.mirrored {
		applyMirrored(r.t, r.pool, r.admission, eventID, fact, r.engine)
		return
	}
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

// setClaimState moves the claim of role on issue to state.
func (r *ownerRig) setClaimState(issue string, role claim.Role, state supervise.ClaimState) {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), `update claims set state = $3 where issue = $1 and role = $2`, issue, string(role), string(state)); err != nil {
		r.t.Fatalf("set the %s claim of %s to %s: %v", role, issue, state, err)
	}
}

// noticeOwner is the issue whose architect hears a notice of kind about key: the owner walk the
// outbox runs when it sends the notice (record.OwningIssue), over the claims as the store holds them.
func (r *ownerRig) noticeOwner(key string, kind record.NoticeKind) string {
	r.t.Helper()
	about := issue(r.t, r.pool, key)
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		r.t.Fatalf("project token: %v", err)
	}
	var owner string
	inTx(r.t, r.pool, func(tx pgx.Tx) {
		walk, err := record.ReadOwnerWalk(context.Background(), record.NewStore(), tx, project, about.Tree)
		if err != nil {
			r.t.Fatalf("read the owner walk of %s: %v", about.Tree, err)
		}
		owner = walk.Owner(about, record.NoticeStartsAtParent(kind, claim.RoleArchitect))
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

// sameRoles reports whether got and want hold the same roles, each as often, in any order. A walk
// over every claim of an issue writes a row naming each stored claim, in token order, and then a
// row by the role alone for each other role (workflow's claimTargets). So the order of its rows
// follows which claims the record stores, and nothing depends on it: each row is a request of its
// own, which the outbox runs on its own.
func sameRoles(got, want []claim.Role) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// supervisedByRole is rows, each a supervise row, sorted by the role it names, for a comparison of
// the rows of a walk over every claim in any order (sameRoles).
func supervisedByRole(rows []effect) []effect {
	return slices.SortedFunc(slices.Values(rows), func(a, b effect) int {
		left, _ := a.payload.(record.SuperviseRequest)
		right, _ := b.payload.(record.SuperviseRequest)
		return strings.Compare(string(left.Role), string(right.Role))
	})
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

// assertLeft holds that key left the workflow at status as a person's move takes it out (the
// workflow's leave): its phase is done and its record holds status, and no status write of the
// daemon's is queued for it. A done closes each of its claims, and any other status suspends each
// of them and releases its Dispatch claim. Either way its pod goes (record.IssueSuspend), and the
// architect above it hears child-closed for a done and child-status for the other three. The
// notice's reason starts with "<key> is <status>". This holds only that first sentence, because
// the notice can name more after it, such as an issue under key that is still in the workflow.
func (r *ownerRig) assertLeft(key, status string) {
	r.t.Helper()
	got, root := issue(r.t, r.pool, key), issue(r.t, r.pool, ownerRoot)
	if got.Phase != phase.Done || got.Status != status || got.LingerUntil != nil {
		r.t.Fatalf("%s after the move to %s = phase %s, status %s, linger %v: want it out of the workflow at phase done with status %s",
			key, status, got.Phase, got.Status, got.LingerUntil, status)
	}
	rows := rowsFor(effects(r.t, r.pool), key)
	if writes := payloadsOf[record.StatusWrite](rows); len(writes) != 0 {
		r.t.Fatalf("status writes queued for %s after the move to %s = %+v, want none: the daemon sets nothing back over the move", key, status, writes)
	}
	op, other, kind, releases := record.SuperviseOp("suspend"), record.SuperviseOp("issue_close"), record.NoticeKind("child-status"), 1
	if status == record.DoneStatus {
		op, other, kind, releases = "issue_close", "suspend", "child-closed", 0
	}
	if got := rolesOf(rows, op); !sameRoles(got, claim.Roles) {
		r.t.Fatalf("%s requests for %s after the move to %s = %v, want one for each role %v", op, key, status, got, claim.Roles)
	}
	if got := rolesOf(rows, other); len(got) != 0 {
		r.t.Fatalf("%s requests for %s after the move to %s = %v, want none", other, key, status, got)
	}
	want := []record.IssueSuspend{{Tree: ownerRoot, Generation: got.Generation, TreeGeneration: root.Generation}}
	if suspends := payloadsOf[record.IssueSuspend](rows); !reflect.DeepEqual(suspends, want) {
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
	if got.Phase != before.Phase || got.Status != before.Status || got.Generation != before.Generation || got.LingerUntil != nil || got.LastDispatchSeq != fact.Seq {
		r.t.Fatalf("%s after the write of %s = phase %s, status %s, generation %d, linger %v, seq %d: want phase %s, status %s and generation %d kept, no linger, seq %d",
			key, status, got.Phase, got.Status, got.Generation, got.LingerUntil, got.LastDispatchSeq, before.Phase, before.Status, before.Generation, fact.Seq)
	}
	rows := effects(r.t, r.pool)
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

// A sub-architect owns the issues under its own issue, so its done on one of them stands. The
// daemon reads which claim the writer's session holds with no regard to the claim's state: a
// sub-architect whose own claim is suspended still writes as the owner, since the session in
// Dispatch is the session of that claim.
func TestASubArchitectsDoneOnAnIssueUnderItsOwnStands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
	}{
		{name: "its claim running", state: supervise.StateWorking},
		{name: "its own claim suspended", state: supervise.StateSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedOwnerTree(t, nil)
			rig.setClaimState(ownerChild, claim.RoleArchitect, tc.state)
			rig.write(ownerGrandchild, record.DoneStatus, childArchitect)
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
				if status == "in_progress" {
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
// a done end at done. The second leave finds the child already at phase done and repeats only what
// is safe to repeat: a close of each claim, which retires a suspended claim, and a second pod
// suspension, which finds no Sandbox, beside the architect's child-closed. It opens no production
// check, since the child has no merged pull request. No issue stands under the child, so the
// child-closed names nothing after its first sentence and is compared whole.
func TestTheOwningArchitectsBacklogThenDoneEndsAtDone(t *testing.T) {
	rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { delete(issues, ownerGrandchild) })
	rig.write(ownerChild, "backlog", rootArchitect)
	rig.assertLeft(ownerChild, "backlog")
	first := effects(t, rig.pool)
	rig.write(ownerChild, record.DoneStatus, rootArchitect)
	if got := issue(t, rig.pool, ownerChild); got.Phase != phase.Done || got.Status != record.DoneStatus {
		t.Fatalf("%s after the backlog and the done = phase %s, status %s, want phase done at done", ownerChild, got.Phase, got.Status)
	}
	rows := effects(t, rig.pool)
	if len(rows) < len(first) || !reflect.DeepEqual(rows[:len(first)], first) {
		t.Fatalf("the outbox after the done = %#v, want the backlog's rows %#v first", rows, first)
	}
	reason := ownerChild + " is done"
	// The close names each claim the record stores for the child by its token, and reaches each
	// other role by the role alone (workflow's claimTargets).
	stored := map[claim.Role]bool{}
	for _, c := range treeClaims() {
		if c.issue == ownerChild {
			stored[c.role] = true
		}
	}
	var closes []effect
	for _, role := range claim.Roles {
		request := record.SuperviseRequest{Op: "issue_close", Tree: ownerRoot, Role: role, Generation: 1, Reason: reason}
		if stored[role] {
			request.Claim = ownerToken(t, ownerChild, role)
		}
		closes = append(closes, effect{kind: record.OutboxKindSupervise, issue: ownerChild, payload: request})
	}
	after := []effect{
		{kind: record.OutboxKindIssueSuspend, issue: ownerChild, payload: record.IssueSuspend{Tree: ownerRoot, Generation: 1, TreeGeneration: 1}},
		{kind: record.OutboxKindNotice, issue: ownerChild, payload: record.Notice{Kind: "child-closed", Role: claim.RoleArchitect, Reason: reason}}}
	added := rows[len(first):]
	if len(added) != len(closes)+len(after) || !reflect.DeepEqual(supervisedByRole(added[:len(closes)]), supervisedByRole(closes)) ||
		!reflect.DeepEqual(added[len(closes):], after) {
		t.Fatalf("the rows the done added = %#v, want the closes %#v in any order, then %#v", added, closes, after)
	}
	if check := rig.check(ownerChild); check != nil {
		t.Fatalf("the production check of %s = %+v, want none: it has no merged pull request", ownerChild, check)
	}
}

// A merged issue stands in merging while an issue under it is in the workflow, and closes once the
// last of them leaves. The owner's move of that last issue is such a departure, so the merged
// issue above it closes on it, as on a person's move.
func TestTheOwningArchitectsMoveEndsTheWaitOfAMergedAncestor(t *testing.T) {
	for _, status := range []string{record.DoneStatus, "backlog"} {
		t.Run(status, func(t *testing.T) {
			rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
				child := issues[ownerChild]
				child.Phase, child.Status, child.DispatchStatus = phase.Merging, "retro", "retro"
			})
			rig.pullRequest(ownerChild, 42, record.PullRequestMerged)
			rig.write(ownerGrandchild, status, rootArchitect)
			rig.assertLeft(ownerGrandchild, status)
			if got := issue(t, rig.pool, ownerChild); got.Phase != phase.Done || got.Status != record.DoneStatus {
				t.Fatalf("%s after the last issue under it left = phase %s, status %s, want it closed at done", ownerChild, got.Phase, got.Status)
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
	if got := issue(r.t, r.pool, key); got.Phase != phase.Done || got.Status != record.DoneStatus {
		r.t.Fatalf("%s after the done = phase %s, status %s, want phase done at done", key, got.Phase, got.Status)
	}
	check := r.check(key)
	if check == nil || !check.Open() || check.Looks != 1 || check.Role != claim.RoleTester || check.Number != number || check.MergeSHA != "merge-"+key {
		r.t.Fatalf("the production check of %s = %+v, want it open at look 1 for the tester, on #%d at merge-%s", key, check, number, key)
	}
	added = rowsFor(added, key)
	want := slices.DeleteFunc(slices.Clone(claim.Roles), func(role claim.Role) bool { return role == claim.RoleTester })
	if got := rolesOf(added, "issue_close"); !sameRoles(got, want) {
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
		if got, check := issue(t, rig.pool, ownerChild), rig.check(ownerChild); got.Phase != phase.Implementing || check != nil {
			t.Fatalf("%s after its merge in implementing = phase %s with check %+v, want it still in implementing and no check: this test proves nothing", ownerChild, got.Phase, check)
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
		if got, check := issue(t, rig.pool, ownerChild), rig.check(ownerChild); got.Phase != phase.Done || got.Status != "backlog" || check != nil {
			t.Fatalf("%s after its merge at backlog = phase %s, status %s, check %+v, want it at backlog with no check", ownerChild, got.Phase, got.Status, check)
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
			rig.mirrored = true
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

// A tree lingers after its root left the workflow, while its architect's turn still runs. The
// architect's move of a child there gets what a person's move of that child gets on the same
// record.
func TestTheOwningArchitectsMoveInALingeringTreeIsAPersonsMove(t *testing.T) {
	until := fixedNow.Add(time.Hour)
	for _, status := range []string{record.DoneStatus, "backlog"} {
		t.Run(status, func(t *testing.T) {
			move := func(session string) moveOutcome {
				rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
					root := issues[ownerRoot]
					root.Phase, root.Status, root.DispatchStatus, root.LingerUntil = phase.Done, record.DoneStatus, record.DoneStatus, &until
				})
				rig.write(ownerChild, status, session)
				return rig.outcome()
			}
			person := move("")
			if got := person.issues[ownerChild]; got.Phase != phase.Done || got.Status != status {
				t.Fatalf("%s after a person's %s = phase %s, status %s, want it out of the workflow: the comparison would be of two writes that did nothing", ownerChild, status, got.Phase, got.Status)
			}
			if owner := move(rootArchitect); !reflect.DeepEqual(owner, person) {
				t.Fatalf("the owning architect's %s on %s in a lingering tree left %+v, want what a person's left, %+v", status, ownerChild, owner, person)
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
