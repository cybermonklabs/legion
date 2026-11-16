package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// roleHoldersPublished is one publication of an issue's role sessions as Dispatch received it.
type roleHoldersPublished struct {
	Issue   string
	Holders []dispatch.RoleHolder
}

// roleHoldersDispatch is a Dispatch that takes role-holder publications and records each. answers
// is what the next PUTs answer, oldest first; a PUT past the list is taken.
type roleHoldersDispatch struct {
	gatePublishDispatch
	mu      sync.Mutex
	sent    []roleHoldersPublished
	answers []error
}

func (d *roleHoldersDispatch) PutRoleHolders(_ context.Context, key string, holders []dispatch.RoleHolder) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, roleHoldersPublished{Issue: key, Holders: append(make([]dispatch.RoleHolder, 0, len(holders)), holders...)})
	if len(d.answers) == 0 {
		return nil
	}
	answer := d.answers[0]
	d.answers = d.answers[1:]
	return answer
}

// taken is every publication sent since the last call, refused ones included.
func (d *roleHoldersDispatch) taken() []roleHoldersPublished {
	d.mu.Lock()
	defer d.mu.Unlock()
	taken := d.sent
	d.sent = nil
	return taken
}

// roleHoldersRig is the daemon's record over a database of its own and the board lane that takes
// the project's role-holder publications to Dispatch.
type roleHoldersRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records *record.Postgres
	client  *roleHoldersDispatch
	runner  *outbox
	ahead   time.Duration
}

func newRoleHoldersRig(t *testing.T) *roleHoldersRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	rig := &roleHoldersRig{t: t, ctx: context.Background(), pool: pool, records: record.NewStore(), client: &roleHoldersDispatch{}}
	rig.runner = &outbox{pool: pool, records: rig.records, dispatch: rig.client, dispatchProject: gatePublishProject,
		now: rig.clock, log: quietLogger()}
	return rig
}

func (r *roleHoldersRig) clock() time.Time { return time.Now().UTC().Add(r.ahead) }

// seedClaim writes the claim of issue's role in state, with session, last changed ago before now:
// the row the daemon's supervision leaves.
func (r *roleHoldersRig) seedClaim(issue string, role claim.Role, session string, state supervise.ClaimState, ago time.Duration) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		r.t.Fatalf("the token of %s's %s: %v", issue, role, err)
	}
	if _, err := r.pool.Exec(r.ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, updated_at)
		values ($1, 'legion', $2, $2, $3, 1, $4, '', $5, 0, 0, 0, 0, now() - $6::interval)`,
		string(token), issue, string(role), session, string(state), fmt.Sprintf("%d seconds", int(ago.Seconds()))); err != nil {
		r.t.Fatalf("seed the %s claim of %s: %v", role, issue, err)
	}
	return token
}

// moveClaim puts the claim in state, as a decision of its machine does.
func (r *roleHoldersRig) moveClaim(token claim.Token, state supervise.ClaimState) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `update claims set state = $2, updated_at = now() where token = $1`, string(token), string(state)); err != nil {
		r.t.Fatalf("move %s to %s: %v", token, state, err)
	}
}

func (r *roleHoldersRig) enqueue(issue string) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.EnqueueRoleHolders(r.ctx, tx, issue, r.clock())
	}); err != nil {
		r.t.Fatalf("enqueue the role-holder publication of %s: %v", issue, err)
	}
}

// published runs the board lane over the role-holder publications alone and answers what Dispatch
// was told since the last call.
func (r *roleHoldersRig) published() []roleHoldersPublished {
	r.t.Helper()
	if drained, _ := r.runner.drain(r.ctx, "", []record.OutboxKind{record.OutboxKindRoleHoldersPublish}); !drained {
		r.t.Fatal("the role-holders lane did not drain")
	}
	return r.client.taken()
}

// queuedRoleHolders is the issues with a role_holders_publish row, and how many such rows there
// are.
func queuedRoleHolders(t *testing.T, pool *store.Pool) ([]string, int) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue from outbox where kind = $1 order by issue`, string(record.OutboxKindRoleHoldersPublish))
	if err != nil {
		t.Fatalf("read the queued role-holder publications: %v", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the queued role-holder publications: %v", err)
	}
	return issues, len(issues)
}

// The executor reads the issue's live set from the claims at run time and sends it whole: a claim
// counts while it is registered, ready, working or idle and records a session. A retired, a
// suspended and a failed claim leave the set, and so does a claim with no session yet. The set is
// read when the row runs, not when it is queued, so a claim that ended between the two is gone.
func TestTheExecutorPublishesTheLiveSetAtRunTime(t *testing.T) {
	const issue = "LEGION-208"
	r := newRoleHoldersRig(t)
	tester := r.seedClaim(issue, claim.RoleTester, "ses-1", supervise.StateReady, time.Minute)
	r.seedClaim(issue, claim.RoleImplementer, "ses-2", supervise.StateRetired, time.Minute)
	r.seedClaim(issue, claim.RoleReviewer, "ses-3", supervise.StateSuspended, time.Minute)
	r.seedClaim(issue, claim.RolePlanner, "", supervise.StateQueued, time.Minute)

	r.enqueue(issue)
	want := []roleHoldersPublished{{Issue: issue, Holders: []dispatch.RoleHolder{{Role: "tester", Session: "ses-1"}}}}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		t.Fatalf("published %+v, want %+v", got, want)
	}

	r.enqueue(issue)
	r.moveClaim(tester, supervise.StateFailed)
	want = []roleHoldersPublished{{Issue: issue, Holders: []dispatch.RoleHolder{}}}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the tester failed, published %+v, want %+v", got, want)
	}
}

// recordingWakes is a projectRecords whose lane wake is counted: what the commit of a queued row
// wakes.
func recordingWakes(store record.Store) (projectRecords, func() int) {
	var mu sync.Mutex
	wakes := 0
	records := projectRecords{Store: store, project: gatePublishProject, log: slog.New(slog.DiscardHandler), wake: func(string, record.OutboxKind) {
		mu.Lock()
		defer mu.Unlock()
		wakes++
	}}
	return records, func() int {
		mu.Lock()
		defer mu.Unlock()
		return wakes
	}
}

// A claim whose role holder changed — here its end — queues the publication of its issue
// (Deps.RoleHolderChanged, the workflow's claimHolderChanged), on a goroutine of its own, and
// wakes the board lane once the row commits. A second change of the same issue queues no second
// row: the one row is made due again. A claim with no issue and the controller have no role to
// publish.
func TestAClaimWhoseRoleHolderChangedQueuesItsIssue(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records, wakes := recordingWakes(record.NewStore())
	w := &workflowRuntime{pool: pool, records: records, dispatchProject: gatePublishProject, log: quietLogger()}
	tester := supervise.Claim{Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleTester}

	w.claimHolderChanged(supervise.Claim{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController})
	w.claimHolderChanged(tester)
	testwait.Eventually(t, "the ended claim's publication to commit and wake its lane", func() bool { return wakes() == 1 })
	w.claimHolderChanged(tester)
	testwait.Eventually(t, "the second end's publication to commit and wake its lane", func() bool { return wakes() == 2 })

	issues, rows := queuedRoleHolders(t, pool)
	if rows != 1 || issues[0] != "LEGION-208" {
		t.Fatalf("the outbox holds %d role-holder publications for %v, want the one of LEGION-208", rows, issues)
	}
}

// Each boot queues the publication of every issue of the project that has a live claim, or a claim
// that changed in the last seven days: a daemon that starts over running sessions publishes them,
// and an end that a crash lost before its queue is published at the restart. A claim that changed
// longer ago and a claim of another project are left out.
func TestBootQueuesEveryRecentIssue(t *testing.T) {
	r := newRoleHoldersRig(t)
	r.seedClaim("LEGION-1", claim.RoleTester, "ses-1", supervise.StateIdle, 30*24*time.Hour)
	r.seedClaim("LEGION-2", claim.RoleTester, "ses-2", supervise.StateRetired, 24*time.Hour)
	r.seedClaim("LEGION-3", claim.RoleTester, "ses-3", supervise.StateRetired, 10*24*time.Hour)
	r.seedClaim("OTHER-1", claim.RoleTester, "ses-4", supervise.StateReady, time.Minute)
	r.seedClaim("LEGIONX-1", claim.RoleTester, "ses-5", supervise.StateReady, time.Minute)
	w := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: gatePublishProject, log: quietLogger()}

	if err := w.queueRoleHoldersPublish(r.ctx); err != nil {
		t.Fatalf("the boot's queue of the role-holder publications: %v", err)
	}
	if issues, _ := queuedRoleHolders(t, r.pool); !slices.Equal(issues, []string{"LEGION-1", "LEGION-2"}) {
		t.Fatalf("the boot queued %v, want LEGION-1 and LEGION-2", issues)
	}
	// A second boot, a restart loop, queues no row beside the first.
	if err := w.queueRoleHoldersPublish(r.ctx); err != nil {
		t.Fatalf("the second boot's queue: %v", err)
	}
	if issues, rows := queuedRoleHolders(t, r.pool); rows != 2 {
		t.Fatalf("after a second boot the outbox holds %v, want the same two rows", issues)
	}
}

// The boot itself runs the pass: a daemon that starts over a running session queues the
// publication of its issue in openWorkflow, before the outbox attaches, so the first sweep sends
// it. A boot that skips the pass, or a pass that queues nothing, leaves the session unpublished
// until its next ready or end.
func TestTheWorkflowsBootQueuesTheRolePublicationOfALiveClaim(t *testing.T) {
	pool := isolatedOutboxPool(t)
	seedSilentPhaseWorker(t, pool, "CAPTURE-701", time.Now().UTC())
	bootedTickWorkflow(t, pool)
	if issues, rows := queuedRoleHolders(t, pool); rows != 1 || issues[0] != "CAPTURE-701" {
		t.Fatalf("after the boot the outbox holds role-holder publications for %v, want the one of CAPTURE-701, whose implementer runs", issues)
	}
}

// failingRoleHolders is a record whose queue of a role-holder publication fails, as a store that
// refuses the write does.
type failingRoleHolders struct {
	record.Store
}

func (failingRoleHolders) EnqueueRoleHolders(context.Context, pgx.Tx, string, time.Time) error {
	return errors.New("the store refused the outbox write")
}

// warnings is a log handler that keeps the message of each record at Warn or above.
type warnings struct {
	mu       *sync.Mutex
	messages *[]string
}

func newWarnings() warnings { return warnings{mu: &sync.Mutex{}, messages: &[]string{}} }

func (warnings) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelWarn }
func (h warnings) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.messages = append(*h.messages, r.Message)
	return nil
}
func (h warnings) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h warnings) WithGroup(string) slog.Handler      { return h }

func (h warnings) logged() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(*h.messages)
}

// claimHolderChanged only logs a queue that fails, since it runs beside the machine's decision and
// has no caller to refuse. So the end of such a claim reaches Dispatch at the next boot alone, or
// at the next ready, end or other change on the same issue: the boot pass publishes it, because
// the claim changed less than seven days ago.
func TestBootPublishesAnEndWhoseQueueFailed(t *testing.T) {
	r := newRoleHoldersRig(t)
	token := r.seedClaim("LEGION-4", claim.RoleTester, "ses-4", supervise.StateReady, time.Minute)
	failing, wakes := recordingWakes(failingRoleHolders{Store: r.records})
	warned := newWarnings()
	w := &workflowRuntime{pool: r.pool, records: failing, dispatchProject: gatePublishProject, log: slog.New(warned)}
	r.moveClaim(token, supervise.StateRetired)

	w.claimHolderChanged(supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-4", Issue: "LEGION-4", Role: claim.RoleTester})
	testwait.Eventually(t, "the refused queue to be logged", func() bool { return len(warned.logged()) == 1 })
	if _, rows := queuedRoleHolders(t, r.pool); rows != 0 || wakes() != 0 {
		t.Fatalf("a queue the store refused left %d rows and %d wakes, want none", rows, wakes())
	}

	booted := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: gatePublishProject, log: quietLogger()}
	if err := booted.queueRoleHoldersPublish(r.ctx); err != nil {
		t.Fatalf("the boot's queue: %v", err)
	}
	if issues, _ := queuedRoleHolders(t, r.pool); !slices.Equal(issues, []string{"LEGION-4"}) {
		t.Fatalf("the boot queued %v, want LEGION-4, whose end nothing else published", issues)
	}
	want := []roleHoldersPublished{{Issue: "LEGION-4", Holders: []dispatch.RoleHolder{}}}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		t.Fatalf("published %+v, want %+v: the retired tester holds no role", got, want)
	}
}

// What becomes of a row is decided by Dispatch's answer alone. An outage is retried: a transport
// failure, a 5xx, a 400 with no code (from whatever stands in front of Dispatch), and the 404 that
// a Dispatch without the route answers under its router's code
// NOT_FOUND, so the daemon and Dispatch deploy in either order. A refusal that no retry changes
// ends the row: the route's 404 ISSUE_NOT_FOUND, and a 400 with Dispatch's code, which names a set
// that Dispatch will never take. A 400 ends the row with one ERROR line that names the issue and
// Dispatch's code, and nothing else: the issue's next ready or end of a claim queues a fresh
// publication, which reads the live set again.
func TestRoleHoldersExecutorRetriesARefusal(t *testing.T) {
	const issue = "LEGION-208"
	r := newRoleHoldersRig(t)
	logs := &syncBuffer{}
	r.runner.log = slog.New(slog.NewTextHandler(logs, nil))
	r.seedClaim(issue, claim.RoleTester, "ses-1", supervise.StateWorking, time.Minute)
	r.client.answers = []error{
		fmt.Errorf("publish the role holders of %s: %w", issue, errors.New("Dispatch is down")),
		&dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "no route for PUT /api/v1/issues/" + issue + "/role-holders"},
		&dispatch.Error{Status: http.StatusInternalServerError, Code: "INTERNAL", Message: "internal server error"},
		&dispatch.Error{Status: http.StatusBadRequest, Message: "<html>400 Bad Request</html>"},
		&dispatch.Error{Status: http.StatusBadRequest, Code: "INVALID_ROLE_HOLDER", Message: "the session of tester is not a session id."},
		&dispatch.Error{Status: http.StatusNotFound, Code: dispatch.IssueNotFound, Message: "no issue " + issue},
	}
	r.enqueue(issue)

	sent := roleHoldersPublished{Issue: issue, Holders: []dispatch.RoleHolder{{Role: "tester", Session: "ses-1"}}}
	for _, step := range []struct {
		what    string
		rows    int
		after   time.Duration
		requeue bool
	}{
		{what: "a Dispatch that is down", rows: 1},
		{what: "the router's 404 NOT_FOUND of a Dispatch with no such route", rows: 1, after: time.Minute},
		{what: "a 500", rows: 1, after: time.Minute},
		{what: "a 400 with no code, from whatever stands in front of Dispatch", rows: 1, after: time.Minute},
		{what: "the 400 that refuses the set", rows: 0, after: time.Minute},
		{what: "the 404 that names the issue as unknown, on the row the next ready queued", rows: 0, requeue: true},
		{what: "a publication Dispatch takes, on the row the next end queued", rows: 0, requeue: true},
	} {
		r.ahead += step.after
		if step.requeue {
			r.enqueue(issue)
		}
		if got := r.published(); !reflect.DeepEqual(got, []roleHoldersPublished{sent}) {
			t.Fatalf("after %s, published %+v, want %+v", step.what, got, sent)
		}
		if rows := outboxRows(t, r.pool); rows != step.rows {
			t.Fatalf("after %s the outbox holds %d rows, want %d", step.what, rows, step.rows)
		}
	}
	if answers := len(r.client.answers); answers != 0 {
		t.Fatalf("%d of the answers were never given, so the rows never met them", answers)
	}

	var refused, failed []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "role-holders publish refused by Dispatch") {
			refused = append(refused, line)
		}
		if strings.Contains(line, `msg="outbox row failed"`) {
			failed = append(failed, line)
		}
	}
	if len(failed) != 4 {
		t.Errorf("logged %d failed rows, want the four retried answers:\n%s", len(failed), logs.String())
	}
	if len(refused) != 2 {
		t.Fatalf("logged %d refusals, want the 400 and the ISSUE_NOT_FOUND once each:\n%s", len(refused), logs.String())
	}
	if line := refused[0]; !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "issue="+issue) || !strings.Contains(line, "code=INVALID_ROLE_HOLDER") {
		t.Errorf("the 400's line is %q, want one ERROR that names the issue and Dispatch's code", line)
	}
	if line := refused[1]; !strings.Contains(line, "level=WARN") || !strings.Contains(line, "code="+dispatch.IssueNotFound) {
		t.Errorf("the ISSUE_NOT_FOUND line is %q, want a WARN that names the code", line)
	}
}
