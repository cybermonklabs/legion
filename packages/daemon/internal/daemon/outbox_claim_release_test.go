package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The issue a claim release row answers: a child the daemon parked, in its root's tree, held by
// the tree's own architect session. The project's controller holds a claim of its own, on no
// issue and no tree.
const (
	claimTree              = "LEGION-1"
	claimIssue             = "LEGION-2"
	claimTreeSession       = "ses-architect"
	claimControllerSession = "ses-controller"
)

// The executor's own log lines, each of which says what became of a row nothing else would.
const (
	claimUnreadLine       = "outbox claim release left a claim Dispatch names no actor for"
	claimUnattributedLine = "outbox claim release left a claim the daemon cannot attribute to the issue's tree"
	claimGaveUpLine       = "outbox claim release gave up: the holder still holds the claim an hour after the row was written"
	claimRefusedLine      = "outbox claim release refused by Dispatch and dropped"
)

// claimReleaseDispatch is Dispatch as a claim release meets it: the issue its read answers with,
// what each DELETE gets back, and the keys released. A case that must release nothing sets
// forbidden, and the DELETE then fails the test where it is sent rather than at an assertion
// after it. It is read while a lane runs, which outboxDispatch, whose other methods it keeps, is
// not made for.
type claimReleaseDispatch struct {
	*outboxDispatch
	t  *testing.T
	mu sync.Mutex
	// shown is the issue every read answers with, its key filled in from the read.
	shown     dispatch.Issue
	forbidden bool
	// answer is what the DELETE numbered n, counting from zero, gets back; nil is the release
	// Dispatch took. A nil answer releases every claim.
	answer   func(n int) error
	released []string
	statuses []string
}

func (d *claimReleaseDispatch) GetIssue(_ context.Context, key string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	shown := d.shown
	shown.Key = key
	return shown, nil
}

func (d *claimReleaseDispatch) ReleaseClaim(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forbidden {
		d.t.Errorf("Dispatch was sent the claim release of %s; this case must send none", key)
	}
	d.released = append(d.released, key)
	if d.answer == nil {
		return nil
	}
	return d.answer(len(d.released) - 1)
}

func (d *claimReleaseDispatch) SetStatus(_ context.Context, _ string, status string) (dispatch.Issue, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statuses = append(d.statuses, status)
	return dispatch.Issue{}, nil
}

// releases is each key a DELETE was sent for, in order, and statusWrites each status written.
func (d *claimReleaseDispatch) releases() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.released)
}

func (d *claimReleaseDispatch) statusWrites() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.statuses)
}

// answersInTurn gives each DELETE the next answer; a DELETE past the last is the release Dispatch
// took.
func answersInTurn(answers ...error) func(int) error {
	return func(n int) error {
		if n < len(answers) {
			return answers[n]
		}
		return nil
	}
}

// claimConflict is one of Dispatch's two 409s on a claim release, neither of them final.
func claimConflict(code string) error {
	return &dispatch.Error{Status: http.StatusConflict, Code: code, Message: "the issue is claimed"}
}

// sessionClaim is a Dispatch claim held by an agent session, the only holder the daemon releases.
func sessionClaim(id string) *dispatch.Claim { return &dispatch.Claim{Kind: "session", ID: id} }

// claimReleaseRig is one parked issue with a claim release row of its own, drained by hand
// (RunOnce) on a clock the test holds, with the runner's log captured.
type claimReleaseRig struct {
	t      *testing.T
	pool   *pgxpool.Pool
	runner *outbox
	client *claimReleaseDispatch
	clock  *heldClock
	logs   *syncBuffer
	// row is the id of the release row the rig queued, which the unread line names.
	row int64
}

// newClaimReleaseRig seeds the tree and its parked issue and builds the runner over them. Every
// session the holder cases name is recorded: the tree's own architect, one that claims only in
// another tree, and the project's controller, whose claims row carries no tree at all.
func newClaimReleaseRig(t *testing.T, shown dispatch.Issue, forbidden bool, answer func(int) error) *claimReleaseRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	logs := &syncBuffer{}
	client := &claimReleaseDispatch{outboxDispatch: &outboxDispatch{}, t: t, shown: shown, forbidden: forbidden, answer: answer}
	rig := &claimReleaseRig{t: t, pool: pool, client: client, clock: microClock(), logs: logs}
	rig.runner = &outbox{pool: pool, dispatchProject: "LEGION", records: records, dispatch: client,
		log: slog.New(slog.NewJSONHandler(logs, nil)), now: rig.clock.Now}
	putOutboxIssue(t, pool, records, claimReleaseRoot())
	seedSessionClaim(t, pool, "architect-claim", "LEGION", claimTree, claimTree, claimTreeSession)
	seedSessionClaim(t, pool, "other-tree-claim", "LEGION", "LEGION-9", "LEGION-9", "ses-elsewhere")
	seedControllerClaim(t, pool, "LEGION", claimControllerSession)
	return rig
}

// records writes issue as the daemon's own record of it.
func (r *claimReleaseRig) records(issue record.Issue) {
	r.t.Helper()
	putOutboxIssue(r.t, r.pool, record.NewStore(), issue)
}

// heldByOperator opens the tree's lifecycle under the operator's authority, as an operator root
// spawn under the root's key does (treelifecycle.AuthorityOperator).
func (r *claimReleaseRig) heldByOperator() {
	r.t.Helper()
	token, err := claim.ProjectToken("LEGION")
	if err != nil {
		r.t.Fatalf("the project token of LEGION: %v", err)
	}
	if err := pgx.BeginFunc(context.Background(), r.pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().OpenTreeLifecycle(context.Background(), tx, token, claimTree, treelifecycle.AuthorityOperator)
		return err
	}); err != nil {
		r.t.Fatalf("open %s under the operator: %v", claimTree, err)
	}
}

// due queues the issue's release row, written age ago on the test's clock and due now.
func (r *claimReleaseRig) due(age time.Duration) {
	r.t.Helper()
	at := r.clock.Now().Add(-age)
	row := mustOutboxRow(r.t, claimIssue, record.ClaimRelease{}, at)
	row.CreatedAt = at
	enqueueOutbox(r.t, r.pool, record.NewStore(), row)
	if err := r.pool.QueryRow(context.Background(), "select id from outbox order by id desc limit 1").Scan(&r.row); err != nil {
		r.t.Fatalf("read back the release row's id: %v", err)
	}
}

// run drains every row due now.
func (r *claimReleaseRig) run() {
	r.t.Helper()
	if err := r.runner.RunOnce(context.Background()); err != nil {
		r.t.Fatalf("drain the outbox: %v", err)
	}
}

// left is how many rows the issue has left.
func (r *claimReleaseRig) left() int { return outboxRowsOf(r.t, r.pool, claimIssue) }

// loggedOnce holds the log to the one executor line the case names: every other line of the
// executor's must be absent, so a case that names none leaves the log quiet of all four.
func (r *claimReleaseRig) loggedOnce(text string) map[string]any {
	r.t.Helper()
	var named map[string]any
	for _, line := range []string{claimUnreadLine, claimUnattributedLine, claimGaveUpLine, claimRefusedLine} {
		got := loggedLines(r.t, r.logs.String(), line)
		want := 0
		if line == text {
			want = 1
		}
		if len(got) != want {
			r.t.Fatalf("logged %q %d times, want %d: %s", line, len(got), want, r.logs.String())
		}
		if want == 1 {
			named = got[0]
		}
	}
	return named
}

// claimReleaseRoot is the tree's root as the daemon records it, the issue every fixture here
// stands under.
func claimReleaseRoot() record.Issue {
	return record.Issue{Key: claimTree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: claimTree, Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"}
}

// parkedIssue is the issue a release row answers: the daemon took it out of the workflow without
// closing it, which is the one state that releases its claim.
func parkedIssue() record.Issue {
	parent := claimTree
	return record.Issue{Key: claimIssue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: claimTree, Parent: &parent, Title: "Child", Phase: phase.Planning,
		Generation: 1, Status: "backlog"}
}

// seedSessionClaim records session working in tree, under project, the claim row
// SessionClaimsTree and SessionClaimsAnywhere read.
func seedSessionClaim(t *testing.T, pool *pgxpool.Pool, token, project, tree, issue, session string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $4, 'architect', 1, $5, '', 'working', 0, 0, 0, 0)`,
		token, project, tree, issue, session); err != nil {
		t.Fatalf("record the claim of %s in %s: %v", session, tree, err)
	}
}

// seedControllerClaim records session as the project's controller: the one claims row with no
// issue and no tree (migration 0032_controller_claim), which SessionClaimsAnywhere accounts for
// while SessionClaimsTree, which matches on a tree, never can.
func seedControllerClaim(t *testing.T, pool *pgxpool.Pool, project, session string) {
	t.Helper()
	token, err := claim.ProjectToken(project)
	if err != nil {
		t.Fatalf("the project token of %s: %v", project, err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation,
		session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, '', '', 'controller', 1, $3, '', 'working', 0, 0, 0, 0)`,
		string(claim.ControllerToken(token)), project, session); err != nil {
		t.Fatalf("record the controller claim of %s in %s: %v", session, project, err)
	}
}

// TestReleaseClaimRow is the claim release executor (outbox.releaseClaim) over the real store:
// which holders the daemon takes a claim from, which of its own records let it, what it makes of
// Dispatch's answers, and that an issue's release row waiting on its holder holds nothing else of
// that issue back.
func TestReleaseClaimRow(t *testing.T) {
	t.Parallel()
	t.Run("holders", testReleaseClaimHolders)
	t.Run("the record", testReleaseClaimRecord)
	t.Run("answers", testReleaseClaimAnswers)
	t.Run("its own lane", testReleaseClaimLane)
}

// Only a claim held by a session the daemon records in the issue's own tree is released. A
// person's claim is theirs, the daemon's own session is a legitimate holder, a session of another
// tree owns its own issue, the project controller's claims row carries no tree so no issue's
// claim is ever attributed to it, and a holder the daemon cannot attribute, or one Dispatch names
// no actor for, is left where it is and logged.
func testReleaseClaimHolders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shown    dispatch.Issue
		released bool
		logged   string
		holder   string
	}{
		{name: "a session of the issue's own tree", shown: dispatch.Issue{Claim: sessionClaim(claimTreeSession)}, released: true},
		{name: "a person", shown: dispatch.Issue{Claim: &dispatch.Claim{Kind: "user", ID: "alice"}}},
		{name: "the daemon's own session", shown: dispatch.Issue{Claim: sessionClaim(dispatch.DaemonSession(claimIssue))}},
		{name: "a session of another tree", shown: dispatch.Issue{Claim: sessionClaim("ses-elsewhere")}},
		{name: "the project controller's session", shown: dispatch.Issue{Claim: sessionClaim(claimControllerSession)}},
		{name: "a holder no claims row accounts for", shown: dispatch.Issue{Claim: sessionClaim("ses-stranger")},
			logged: claimUnattributedLine, holder: "session:ses-stranger"},
		{name: "a claim Dispatch names no actor for", shown: dispatch.Issue{ClaimUnread: true}, logged: claimUnreadLine},
		{name: "no claim at all", shown: dispatch.Issue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shown := tc.shown
			shown.Status = "backlog"
			rig := newClaimReleaseRig(t, shown, !tc.released, nil)
			rig.records(parkedIssue())
			rig.due(0)
			rig.run()

			want := []string{}
			if tc.released {
				want = append(want, claimIssue)
			}
			if got := rig.client.releases(); !slices.Equal(got, want) {
				t.Fatalf("claim releases = %v, want %v", got, want)
			}
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left = %d, want the release row ended", left)
			}
			line := rig.loggedOnce(tc.logged)
			if line == nil {
				return
			}
			if line["issue"] != claimIssue {
				t.Fatalf("%q names issue %v, want %s", tc.logged, line["issue"], claimIssue)
			}
			if tc.holder != "" && line["holder"] != tc.holder {
				t.Fatalf("%q names holder %v, want %s", tc.logged, line["holder"], tc.holder)
			}
			if tc.logged == claimUnreadLine && line["row"] != float64(rig.row) {
				t.Fatalf("%q names row %v, want %d", tc.logged, line["row"], rig.row)
			}
		})
	}
}

// The record decides as much as the holder does: a claim of an issue the daemon runs belongs to
// that run, and so does a claim of an issue whose tree runs again at a later generation while
// Dispatch still shows it parked. A tree the operator holds is the operator's whole — its own
// root spawn shares the tree's key, so its session is a session of the issue's tree here — and
// the workflow may act on none of its claims. The holder is the tree's own architect session in
// each of the three, the one holder the daemon would otherwise take the claim from. An issue the
// daemon records nothing for is one it can attribute nothing of, and it says nothing either.
func testReleaseClaimRecord(t *testing.T) {
	live := parkedIssue()
	live.Status, live.Phase = "in_progress", phase.Implementing
	readmitted := parkedIssue()
	readmitted.Status, readmitted.Phase, readmitted.Generation = "todo", phase.Admitted, 2
	parked := parkedIssue()
	for _, tc := range []struct {
		name     string
		recorded *record.Issue
		// operatorHolds opens the issue's tree under the operator's authority.
		operatorHolds bool
	}{
		{name: "the daemon records the issue live in the workflow", recorded: &live},
		{name: "its tree runs again at a later generation", recorded: &readmitted},
		{name: "the operator holds the stopped issue's tree", recorded: &parked, operatorHolds: true},
		{name: "the daemon records nothing for the issue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newClaimReleaseRig(t, dispatch.Issue{Status: "backlog", Claim: sessionClaim(claimTreeSession)}, true, nil)
			if tc.recorded != nil {
				rig.records(*tc.recorded)
			}
			if tc.operatorHolds {
				rig.heldByOperator()
			}
			rig.due(0)
			rig.run()

			if got := rig.client.releases(); len(got) != 0 {
				t.Fatalf("claim releases = %v, want none", got)
			}
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left = %d, want the release row ended", left)
			}
			rig.loggedOnce("")
		})
	}
}

// What the daemon makes of Dispatch's answer to the DELETE: the holder still holding the claim
// is a wait the row rides out until the hour it is given runs out; a contended one, which
// Dispatch applied nothing for and judged nobody by, is a wait of its own the hour never ends; a
// refusal no retry repairs ends the row with that refusal logged; and an outage, whatever its
// shape, is ridden out and the next attempt releases the claim.
func testReleaseClaimAnswers(t *testing.T) {
	// A claim release's backoff climbs to retryBackoff's cap, so one advance of the cap brings
	// every attempt but the one the hour ends on due again.
	const capped = 64 * time.Second
	for _, tc := range []struct {
		name string
		// age is how old the row already is at its first attempt.
		age      time.Duration
		answers  []error
		attempts int
		// between is how far the clock moves before each attempt after the first.
		between time.Duration
		deletes int
		settled string
		logged  string
		holder  string
		refusal map[string]any
	}{
		{name: "a 409 ISSUE_CLAIMED, then the holder is gone", answers: []error{claimConflict(dispatch.IssueClaimed)},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row waits"},
		// A contended release applied nothing and judged nobody, so it is a routine retry rather
		// than a failure, and no evidence about the holder: it never spends the hour.
		{name: "a 409 CLAIM_CONTENDED, then Dispatch applies it", answers: []error{claimConflict(dispatch.ClaimContended)},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row waits"},
		{name: "a 409 ISSUE_CLAIMED that never clears", answers: []error{claimConflict(dispatch.IssueClaimed), claimConflict(dispatch.IssueClaimed)},
			attempts: 2, between: time.Hour, deletes: 2, settled: "outbox row waits",
			logged: claimGaveUpLine, holder: "session:" + claimTreeSession},
		{name: "a row already older than an hour at its first attempt", age: 2 * time.Hour,
			answers: []error{claimConflict(dispatch.IssueClaimed)}, attempts: 1, deletes: 1,
			logged: claimGaveUpLine, holder: "session:" + claimTreeSession},
		{name: "a 404", answers: []error{&dispatch.Error{Status: http.StatusNotFound, Code: "ISSUE_NOT_FOUND"}},
			attempts: 1, deletes: 1, logged: claimRefusedLine,
			refusal: map[string]any{"status": float64(http.StatusNotFound), "code": "ISSUE_NOT_FOUND"}},
		{name: "a 503", answers: []error{&dispatch.Error{Status: http.StatusServiceUnavailable, Code: "UNAVAILABLE"}},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
		{name: "a closed connection", answers: []error{errors.New("read tcp 10.0.0.1:443: connection reset by peer")},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
		{name: "a 401", answers: []error{&dispatch.Error{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED"}},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
		{name: "a 403", answers: []error{&dispatch.Error{Status: http.StatusForbidden, Code: "FORBIDDEN"}},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
		{name: "a 408", answers: []error{&dispatch.Error{Status: http.StatusRequestTimeout, Code: "TIMEOUT"}},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
		{name: "a 429", answers: []error{&dispatch.Error{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED"}},
			attempts: 2, between: capped, deletes: 2, settled: "outbox row failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newClaimReleaseRig(t, dispatch.Issue{Status: "backlog", Claim: sessionClaim(claimTreeSession)},
				false, answersInTurn(tc.answers...))
			rig.records(parkedIssue())
			rig.due(tc.age)
			for attempt := range tc.attempts {
				if attempt > 0 {
					rig.clock.advance(tc.between)
				}
				rig.run()
			}

			if got := rig.client.releases(); len(got) != tc.deletes {
				t.Fatalf("claim releases = %v, want %d", got, tc.deletes)
			}
			if left := rig.left(); left != 0 {
				t.Fatalf("rows left = %d, want the release row ended", left)
			}
			for _, settled := range []string{"outbox row waits", "outbox row failed"} {
				lines := loggedLines(t, rig.logs.String(), settled)
				if (len(lines) > 0) != (settled == tc.settled) {
					t.Fatalf("logged %q %d times, want %t: %s", settled, len(lines), settled == tc.settled, rig.logs.String())
				}
			}
			line := rig.loggedOnce(tc.logged)
			if line == nil {
				return
			}
			if line["issue"] != claimIssue {
				t.Fatalf("%q names issue %v, want %s", tc.logged, line["issue"], claimIssue)
			}
			if tc.holder != "" && line["holder"] != tc.holder {
				t.Fatalf("%q names holder %v, want %s", tc.logged, line["holder"], tc.holder)
			}
			for key, want := range tc.refusal {
				if line[key] != want {
					t.Fatalf("%q names %s %v, want %v", tc.logged, key, line[key], want)
				}
			}
		})
	}
}

// A release waiting on a holder that has not stopped waits on a lane of its own
// (record.LaneClaim), so it delays nothing else of its issue: with the release held on its
// backoff, the same issue's status write and its notice run on the board lane and land.
func testReleaseClaimLane(t *testing.T) {
	pool := isolatedOutboxPool(t)
	noticeTree(t, pool, record.NewStore(), false)
	putOutboxIssue(t, pool, record.NewStore(), parkedIssue())
	seedSessionClaim(t, pool, "architect-claim", "LEGION", claimTree, claimTree, claimTreeSession)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, claimTree, supervise.StateWorking)
	clock := microClock()
	publisher := &holderPublisher{}
	client := &claimReleaseDispatch{outboxDispatch: &outboxDispatch{}, t: t,
		shown:  dispatch.Issue{Status: "backlog", Claim: sessionClaim(claimTreeSession)},
		answer: func(int) error { return claimConflict(dispatch.IssueClaimed) }}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", dispatch: client,
		notices: publisher, supervisor: sup, project: "legion"}
	runner.records = wakingRecords(runner)
	runLanes(t, runner, clock)

	commitRows(t, pool, runner.records, "release", mustOutboxRow(t, claimIssue, record.ClaimRelease{}, clock.Now()))
	backoff := retryBackoff(0)
	testwait.Eventually(t, "the release waited on its holder and its lane armed the backoff", func() bool {
		live, _ := clock.armed(backoff)
		return live == 1 && len(client.releases()) == 1
	})

	commitRows(t, pool, runner.records, "board",
		mustOutboxRow(t, claimIssue, record.StatusWrite{ObservedStatus: "backlog", Status: "icebox"}, clock.Now()),
		mustOutboxRow(t, claimIssue, record.Notice{Kind: "pr-blocked", Role: claim.RoleArchitect, Reason: "max_fix_attempts"}, clock.Now()))
	testwait.Eventually(t, "the status write and the notice ran while the release waited", func() bool {
		_, delivered := publisher.snapshot()
		return len(client.statusWrites()) == 1 && len(delivered) == 1
	})
	if left := outboxRowsOf(t, pool, claimIssue); left != 1 {
		t.Fatalf("rows left on %s = %d, want the waiting release alone", claimIssue, left)
	}
	if got := client.statusWrites(); !slices.Equal(got, []string{"icebox"}) {
		t.Fatalf("statuses written = %v, want the one icebox write", got)
	}
}
