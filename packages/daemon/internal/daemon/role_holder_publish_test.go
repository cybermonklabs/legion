package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
// the row the daemon's supervision leaves, the hold of the session's id for the claim included,
// which a registration writes before it records the session (supervise's holdRecorded).
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
	if session != "" {
		if _, err := r.pool.Exec(r.ctx, `insert into claim_sessions (project, kind, name, claim) values ('legion', 'session', $1, $2)`,
			session, string(token)); err != nil {
			r.t.Fatalf("hold %s for the %s claim of %s: %v", session, role, issue, err)
		}
	}
	return token
}

// moveClaim puts the claim in state with a statement of its own, which queues nothing: the
// executor's rows below are queued by hand (enqueue).
func (r *roleHoldersRig) moveClaim(token claim.Token, state supervise.ClaimState) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `update claims set state = $2, updated_at = now() where token = $1`, string(token), string(state)); err != nil {
		r.t.Fatalf("move %s to %s: %v", token, state, err)
	}
}

// suspendedBy records by as the source that suspended the claim, with a statement of its own that
// leaves the claim's updated_at as it stands, so a seed changed long ago stays so.
func (r *roleHoldersRig) suspendedBy(token claim.Token, by supervise.SuspendSource) {
	r.t.Helper()
	if _, err := r.pool.Exec(r.ctx, `update claims set suspended_by = $2 where token = $1`, string(token), string(by)); err != nil {
		r.t.Fatalf("record %s's suspension by %s: %v", token, by, err)
	}
}

func (r *roleHoldersRig) enqueue(issue string) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		queued, err := store.EnqueueRoleHolders(r.ctx, tx, gatePublishProject, issue, r.clock())
		if err == nil && !queued {
			r.t.Errorf("the role-holder publication of %s was refused as another project's than %s", issue, gatePublishProject)
		}
		return err
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

// The executor reads the issue's set from the claims at run time and sends it whole: a claim
// counts while it holds its role (supervise.HoldsRole) and records a session. A retired, a
// failed and a suspended claim whose suspension is not the idle release's leave the set, and so
// does a claim with no session yet. The set is read when the row runs, not when it is queued, so a
// claim that ended between the two is gone.
func TestTheExecutorPublishesTheHeldSetAtRunTime(t *testing.T) {
	t.Parallel()
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

// The publication names only a session its claim holds (plan E's t-e-54): a live claim that records
// a session held for another claim, held for none, or held by no row at all — which the boot finds
// where a daemon from before the hold ran, and suspends, but not before a working claim's turn
// ends, nor after a suspension the runtime refused — is left out, so Dispatch is never told that
// its role holds a session that is not its own. So is a claim whose session's text is held for it
// only as another kind of name, or only in another project: the hold that counts is of the
// session's id, in the claim's own project. The claim holding its own session is published as
// before.
//
// The controls (the pull request records the runs): RoleHolders without its claim_sessions
// clause publishes all six sessions; without its `h.kind` predicate it publishes the one held as a
// file name, and without its `h.project` predicate the one held in another project.
func TestTheExecutorPublishesNoSessionItsClaimDoesNotHold(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-209"
	r := newRoleHoldersRig(t)
	r.seedClaim(issue, claim.RoleTester, "ses-own", supervise.StateReady, time.Minute)
	// Five live claims whose sessions the table holds for another claim, for none, not at all, for
	// the claim itself as a file name, and for the claim itself in another project.
	for _, seeded := range []struct {
		role          claim.Role
		session       string
		holder        string
		held          bool
		kind, project string
	}{
		{claim.RoleReviewer, "ses-another-claims", "legion-legion-legion-299-tester", true, "session", "legion"},
		{claim.RoleImplementer, "ses-held-for-none", "", true, "session", "legion"},
		{claim.RolePlanner, "ses-held-by-no-row", "", false, "", ""},
		{claim.RoleArchitect, "ses-held-as-a-file-name", "self", true, "file-name", "legion"},
		{claim.RoleMerger, "ses-held-in-another-project", "self", true, "session", "other"},
	} {
		token, err := claim.NewToken("legion", issue, seeded.role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(r.ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, updated_at)
			values ($1, 'legion', $2, $2, $3, 1, $4, '', 'working', 0, 0, 0, 0, now())`,
			string(token), issue, string(seeded.role), seeded.session); err != nil {
			t.Fatalf("seed the %s claim: %v", seeded.role, err)
		}
		if seeded.holder == "self" {
			seeded.holder = string(token)
		}
		if seeded.held {
			if _, err := r.pool.Exec(r.ctx, `insert into claim_sessions (project, kind, name, claim) values ($3, $4, $1, $2)`,
				seeded.session, seeded.holder, seeded.project, seeded.kind); err != nil {
				t.Fatalf("hold %s for %q as %s in %s: %v", seeded.session, seeded.holder, seeded.kind, seeded.project, err)
			}
		}
	}

	r.enqueue(issue)
	want := []roleHoldersPublished{{Issue: issue, Holders: []dispatch.RoleHolder{{Role: "tester", Session: "ses-own"}}}}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		t.Fatalf("published %+v, want %+v: only the tester holds the session it records", got, want)
	}
}

// The publication names a live claim's session only while the claim holds every name its record
// gives the session (supervise.RecordedHolds), in its own project, and none is a name no hold can
// cover (review round 14 of #4220): a claim whose session's id is held for it while its file's
// name, the id its Oh My Pi file name gives, or the Claude Code session its `<uuid>.jsonl` file
// names is not, is left out, and so is one whose file is not a clean path. The claim holding every
// name of its session is published as before.
//
// The control (the pull request records the run): RoleHolders asking for the session id's
// hold alone, as round 13 did; every one of the four claims is published beside the architect.
func TestTheExecutorPublishesASessionOnlyWhileItsClaimHoldsEveryNameOfIt(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-210"
	const uuid = "9d0c8b7a-6e5f-4a3b-8c2d-1e0f9a8b7c6d"
	r := newRoleHoldersRig(t)
	for _, seeded := range []struct {
		role    claim.Role
		session string
		file    string
		skip    supervise.HoldKind // the kind of name left unheld; "" holds every name
	}{
		{claim.RoleArchitect, "ses-whole", "/state/sessions/2026-11-17T00-00-00-000Z_ses-whole.jsonl", ""},
		{claim.RolePlanner, "ses-no-file-name", "/state/sessions/2026-11-17T00-00-00-000Z_ses-no-file-name.jsonl", supervise.HoldFileName},
		{claim.RoleImplementer, "ses-no-file-id", "/state/sessions/2026-11-17T00-00-00-000Z_ses-other-id.jsonl", supervise.HoldFileID},
		{claim.RoleTester, "ses-no-claude-stem", "/home/agent/.claude/projects/repo/" + uuid + ".jsonl", "stem"},
		{claim.RoleReviewer, "ses-unclean-file", "/state/sessions/ses-unclean-file.jsonl/..", ""},
	} {
		token, err := claim.NewToken("legion", issue, seeded.role)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(r.ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, updated_at)
			values ($1, 'legion', $2, $2, $3, 1, $4, $5, 'working', 0, 0, 0, 0, now())`,
			string(token), issue, string(seeded.role), seeded.session, seeded.file); err != nil {
			t.Fatalf("seed the %s claim: %v", seeded.role, err)
		}
		for _, h := range supervise.RecordedHolds(seeded.session, seeded.file) {
			if h.Kind == seeded.skip || seeded.skip == "stem" && h.Name == uuid {
				continue
			}
			if _, err := r.pool.Exec(r.ctx, `insert into claim_sessions (project, kind, name, claim) values ('legion', $1, $2, $3)`,
				string(h.Kind), h.Name, string(token)); err != nil {
				t.Fatalf("hold %s %s for the %s claim: %v", h.Kind, h.Name, seeded.role, err)
			}
		}
	}

	r.enqueue(issue)
	want := []roleHoldersPublished{{Issue: issue, Holders: []dispatch.RoleHolder{{Role: "architect", Session: "ses-whole"}}}}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		t.Fatalf("published %+v, want %+v: only the architect holds every name of the session it records", got, want)
	}
}

// The ready route queues the publication of its claim's issue (roleHoldersChanged) in a
// transaction of its own, and a second ready of the same issue queues no second row: the one row
// is made due again.
func TestTheReadysQueueOfTheRolePublicationKeepsOneRowAnIssue(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	w := &workflowRuntime{pool: pool, records: record.NewStore(), dispatchProject: gatePublishProject, log: quietLogger()}
	tester := supervise.Claim{Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleTester}
	for range 2 {
		if err := w.roleHoldersChanged(context.Background(), tester); err != nil {
			t.Fatalf("the ready's queue of the role-holder publication: %v", err)
		}
	}
	if issues, rows := queuedRoleHolders(t, pool); rows != 1 || issues[0] != "LEGION-208" {
		t.Fatalf("the outbox holds %d role-holder publications for %v, want the one of LEGION-208", rows, issues)
	}
}

// The ready of a claim of another project's issue — the operator's spawn of it, which runs under
// this daemon's token — queues no publication of that issue's role sessions (claim.KeyOfProject):
// a row keyed by the issue is run only by the other project's daemon, against that daemon's own
// claims. So the route leaves the row that daemon holds of the issue as it stands, its lease and
// its due time unchanged, while the ready of this project's claim beside it queues its own. Every
// ready of such a spawn takes this path, so it is an ordinary one and logs no warning.
func TestTheReadyOfAClaimOfAnotherProjectsIssueQueuesNoRolePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	var logs syncBuffer
	w := &workflowRuntime{pool: pool, records: record.NewStore(), dispatchProject: gatePublishProject, log: jsonLogger(&logs)}
	const theirs = "OTHERPROJECT-9"
	// The other project's daemon is sending its issue's publication: it holds the row's lease.
	if _, err := pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
		values ($1, $2, jsonb_build_object('issue', $2::text), 0, now() + interval '1 minute', '', 'otherproject-lease', now() + interval '5 minutes')`,
		string(record.OutboxKindRoleHoldersPublish), theirs); err != nil {
		t.Fatalf("queue the other project's daemon's publication of %s: %v", theirs, err)
	}
	held := func() string {
		t.Helper()
		var row string
		if err := pool.QueryRow(ctx, `select string_agg(concat_ws(' ', id, next_at, attempts, lease_token, lease_until), '; ')
			from outbox where kind = $1 and issue = $2`, string(record.OutboxKindRoleHoldersPublish), theirs).Scan(&row); err != nil {
			t.Fatalf("read the publication rows of %s: %v", theirs, err)
		}
		return row
	}
	before := held()
	spawn := supervise.Claim{Token: "legion-legion-otherproject-9-architect", Project: "legion", Tree: theirs, Issue: theirs, Role: claim.RoleArchitect}
	if err := w.roleHoldersChanged(ctx, spawn); err != nil {
		t.Fatalf("the ready of the spawn of %s: %v", theirs, err)
	}
	if after := held(); after != before {
		t.Errorf("after the ready of the spawn of %s its publication rows are %q, want the other project's daemon's row as it stood, %q", theirs, after, before)
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("the ready of the spawn of %s logged a warning, want none on this ordinary path; the log:\n%s", theirs, logs.String())
	}
	tester := supervise.Claim{Token: "legion-legion-legion-208-tester", Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleTester}
	if err := w.roleHoldersChanged(ctx, tester); err != nil {
		t.Fatalf("the ready of the tester of LEGION-208: %v", err)
	}
	if issues, _ := queuedRoleHolders(t, pool); !slices.Equal(issues, []string{"LEGION-208", theirs}) {
		t.Fatalf("the outbox holds the role-holder publications of %v, want LEGION-208's, which the tester's ready queued, and the other project's daemon's", issues)
	}
}

// Each boot queues the publication of every issue of the project that has a claim that holds its
// role, or a claim that changed in the last seven days: a daemon that upgrades from a release that
// published nothing starts over sessions Dispatch has never been told of, one that upgrades from a
// release that left a claim the idle release suspended out of the set finds that claim's role
// unnamed, and a roll forward finds the claims that release ended. A claim that changed longer ago
// and holds no role, and a claim of another project, are left out: of the suspensions changed
// longer ago, only those whose source keeps the role (suspensionsKeepTheirRole) are read, one issue
// for each source.
func TestBootQueuesEveryRecentIssue(t *testing.T) {
	t.Parallel()
	r := newRoleHoldersRig(t)
	r.seedClaim("LEGION-1", claim.RoleTester, "ses-1", supervise.StateIdle, 30*24*time.Hour)
	r.seedClaim("LEGION-2", claim.RoleTester, "ses-2", supervise.StateRetired, 24*time.Hour)
	r.seedClaim("LEGION-3", claim.RoleTester, "ses-3", supervise.StateRetired, 10*24*time.Hour)
	r.seedClaim("OTHER-1", claim.RoleTester, "ses-4", supervise.StateReady, time.Minute)
	r.seedClaim("LEGIONX-1", claim.RoleTester, "ses-5", supervise.StateReady, time.Minute)
	want := []string{"LEGION-1", "LEGION-2"}
	for i, tc := range suspensionsKeepTheirRole {
		issue := fmt.Sprintf("LEGION-%d", 40+i)
		r.suspendedBy(r.seedClaim(issue, claim.RoleArchitect, "ses-"+string(tc.by), supervise.StateSuspended, 30*24*time.Hour), tc.by)
		if tc.keeps {
			want = append(want, issue)
		}
	}
	slices.Sort(want)
	w := &workflowRuntime{pool: r.pool, records: r.records, dispatchProject: gatePublishProject, log: quietLogger()}

	if err := w.queueRoleHoldersPublish(r.ctx); err != nil {
		t.Fatalf("the boot's queue of the role-holder publications: %v", err)
	}
	issues, _ := queuedRoleHolders(t, r.pool)
	if slices.Sort(issues); !slices.Equal(issues, want) {
		t.Fatalf("the boot queued %v, want %v", issues, want)
	}
	// A second boot, a restart loop, queues no row beside the first.
	if err := w.queueRoleHoldersPublish(r.ctx); err != nil {
		t.Fatalf("the second boot's queue: %v", err)
	}
	if issues, rows := queuedRoleHolders(t, r.pool); rows != len(want) {
		t.Fatalf("after a second boot the outbox holds %v, want the same %d rows", issues, len(want))
	}
}

// suspensionsKeepTheirRole is whether a claim suspended by each source holds its role, which is
// whether Dispatch's set keeps naming its session (supervise.HoldsRole): the idle release's
// suspension, which a project pause's stop makes too, keeps the session, the phase and the task,
// and the next wake resumes the same session, so the role is still the claim's; every other source
// stops the agent until a rule or a resume starts it again, and the role with it. One row for each
// source, so a change of what a source does is one row's edit;
// TestEverySuspensionSourceHasARowOfItsOwn holds the table to the sources supervise declares.
var suspensionsKeepTheirRole = []struct {
	by    supervise.SuspendSource
	keeps bool
}{
	{supervise.SuspendedByReleased, true},
	{supervise.SuspendedByOperator, false},
	{supervise.SuspendedByController, false},
	{supervise.SuspendedByDaemon, false},
	{supervise.SuspendedByExit, false},
	{supervise.SuspendedByUnknown, false},
}

func TestEverySuspensionSourceHasARowOfItsOwn(t *testing.T) {
	t.Parallel()
	var rows []supervise.SuspendSource
	for _, tc := range suspensionsKeepTheirRole {
		rows = append(rows, tc.by)
	}
	declared := supervise.SuspendSources()
	slices.Sort(rows)
	slices.Sort(declared)
	if !slices.Equal(rows, declared) {
		t.Fatalf("suspensionsKeepTheirRole has rows for %v, want one for each source supervise declares, %v", rows, declared)
	}
}

// heldRoleIssue is the issue whose architect the held-role tests write, in its own tree.
const heldRoleIssue = "LEGION-330"

// heldRoleRig is the role-holder publication end to end over one database: the daemon's store,
// whose claim writes queue the publication in their own transactions as a daemon with a workflow
// has them do (store.PublishRoleHolders, store's putClaim), and the board lane that sends it
// (roleHoldersRig). Its claim is heldRoleIssue's architect, whose sessions ses-1 and ses-2 are
// both held for it, as the registration of each holds it.
type heldRoleRig struct {
	*roleHoldersRig
	st    *store.Store
	token claim.Token
}

func newHeldRoleRig(t *testing.T) *heldRoleRig {
	t.Helper()
	r := &heldRoleRig{roleHoldersRig: newRoleHoldersRig(t)}
	r.st = outboxTreeStore(t, r.pool)
	r.st.PublishRoleHolders(nil)
	token, err := claim.NewToken("legion", heldRoleIssue, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the token of %s's architect: %v", heldRoleIssue, err)
	}
	r.token = token
	for _, session := range []string{"ses-1", "ses-2"} {
		if _, err := r.pool.Exec(r.ctx, `insert into claim_sessions (project, kind, name, claim) values ('legion', 'session', $1, $2)`,
			session, string(token)); err != nil {
			t.Fatalf("hold %s for %s: %v", session, token, err)
		}
	}
	return r
}

// step writes the architect's claim in state, suspended by by, recording session, as a machine's
// claim write does (store.PutClaim), and holds what follows to the step's want: whether the write
// queued the issue's publication (moves), and the session the set Dispatch is then sent names as
// the architect ("" for none). A write that queues nothing is read through a publication queued
// by hand, as the ready of a resumed claim queues one (roleHoldersChanged), so every step reads the
// set the claims hold at that step, and not only the sets a claim write sent.
func (r *heldRoleRig) step(name string, state supervise.ClaimState, by supervise.SuspendSource, session string, moves bool, architect string) {
	r.t.Helper()
	if err := r.st.PutClaim(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: heldRoleIssue, Issue: heldRoleIssue,
		Role: claim.RoleArchitect, Generation: 1, State: state, Session: session, SuspendedBy: by}); err != nil {
		r.t.Fatalf("%s: write the claim: %v", name, err)
	}
	if _, rows := queuedRoleHolders(r.t, r.pool); (rows == 1) != moves {
		r.t.Errorf("%s queued %d role-holder publications, want a publication queued: %t", name, rows, moves)
	}
	if !moves {
		r.enqueue(heldRoleIssue)
	}
	want := []roleHoldersPublished{{Issue: heldRoleIssue, Holders: []dispatch.RoleHolder{}}}
	if architect != "" {
		want[0].Holders = []dispatch.RoleHolder{{Role: "architect", Session: architect}}
	}
	if got := r.published(); !reflect.DeepEqual(got, want) {
		r.t.Errorf("%s: Dispatch was sent %+v, want %+v", name, got, want)
	}
}

// A suspension keeps its claim's role in the set Dispatch is sent only where its source does
// (suspensionsKeepTheirRole). The idle release's changes no holder, so it queues nothing, and the
// set names the session through the suspension and through the resume over the kept session —
// queued, launching, the agent's hello and its registration: an idle release is not the loss of a
// role, and the set does not empty and fill again around it. Every other source ends the role: the
// suspension queues the set without the session, and the first step of the resume over the session
// the claim kept, a queued claim or a launching one, queues the set with it again, before the agent
// registers, since a claim queued or launched over its session holds its role
// (supervise.HoldsRole). Each source is resumed both ways: through the queue, and straight into the
// launch.
//
// The control is the code before this change, which read a role as held only while a process ran
// (supervise.HoldsCapability): there the idle release queues the empty set, and neither a queued
// nor a launching claim queues the set back, so every row fails there.
func TestASuspensionKeepsItsClaimsRoleOnlyWhereItsSourceDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range suspensionsKeepTheirRole {
		for _, resume := range []supervise.ClaimState{supervise.StateQueued, supervise.StateLaunching} {
			t.Run(string(tc.by)+" resumed from "+string(resume), func(t *testing.T) {
				t.Parallel()
				r := newHeldRoleRig(t)
				r.step("the registration", supervise.StateRegistered, "", "ses-1", true, "ses-1")
				r.step("a turn's end", supervise.StateIdle, "", "ses-1", false, "ses-1")
				suspended := ""
				if tc.keeps {
					suspended = "ses-1"
				}
				r.step("the suspension", supervise.StateSuspended, tc.by, "ses-1", !tc.keeps, suspended)
				r.step("the resume's first step, "+string(resume)+" over the kept session", resume, "", "ses-1", !tc.keeps, "ses-1")
				if resume == supervise.StateQueued {
					r.step("the launch that runs the queued resume", supervise.StateLaunching, "", "ses-1", false, "ses-1")
				}
				r.step("the resumed agent's hello", supervise.StateShimConnected, "", "ses-1", false, "ses-1")
				r.step("the resumed agent's registration", supervise.StateRegistered, "", "ses-1", false, "ses-1")
			})
		}
	}
}

// A claim the idle release suspended holds its role until the claim ends or its role moves to
// another session, and each of those changes queues its publication: a failure and a retirement
// send the set without the session; a suspension by any other source over the released claim,
// which changes its source alone (supervise's suspendedAgain: a linger, a leave, a park, an
// operator's or the controller's suspend), sends the set without it too, one row for each such
// source (suspensionsKeepTheirRole); a session the claim forgot, as a fresh start forgets it,
// sends the set without it; and a resume whose agent registers under another session sends the
// set naming the new session in the old one's place, with no empty set between them.
//
// The control is the code before this change, under which the release had already sent the empty
// set: there no end of a released claim queues a publication, and the resume names no session
// until the new one registers, so every row fails there.
func TestAReleasedClaimKeepsItsRoleUntilItEndsOrMoves(t *testing.T) {
	t.Parallel()
	type write struct {
		name      string
		state     supervise.ClaimState
		by        supervise.SuspendSource
		session   string
		moves     bool
		architect string
	}
	type change struct {
		name   string
		writes []write
	}
	changes := []change{
		{"failed", []write{{"the failure", supervise.StateFailed, "", "ses-1", true, ""}}},
		{"retired", []write{{"the retirement", supervise.StateRetired, "", "ses-1", true, ""}}},
		{"session forgotten", []write{{"the fresh start that forgets the session", supervise.StateQueued, "", "", true, ""}}},
		{"another session", []write{
			{"the launch that resumes the kept session", supervise.StateLaunching, "", "ses-1", false, "ses-1"},
			{"the registration under another session", supervise.StateRegistered, "", "ses-2", true, "ses-2"},
		}},
	}
	for _, source := range suspensionsKeepTheirRole {
		if source.keeps {
			continue
		}
		changes = append(changes, change{"suspended again by " + string(source.by), []write{
			{"the suspension by " + string(source.by) + " over the released claim", supervise.StateSuspended, source.by, "ses-1", true, ""},
		}})
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newHeldRoleRig(t)
			r.released()
			for _, w := range tc.writes {
				r.step(w.name, w.state, w.by, w.session, w.moves, w.architect)
			}
		})
	}
}

// released puts the rig's architect where the idle release leaves it: registered under ses-1,
// idle, then suspended by the release, with whatever those writes queued sent, so each write after
// it is read on its own. It holds the release itself to nothing
// (TestASuspensionKeepsItsClaimsRoleOnlyWhereItsSourceDoes does), so a test that starts from it
// fails on its own write and not on the release.
func (r *heldRoleRig) released() {
	r.t.Helper()
	r.step("the registration", supervise.StateRegistered, "", "ses-1", true, "ses-1")
	r.step("a turn's end", supervise.StateIdle, "", "ses-1", false, "ses-1")
	if err := r.st.PutClaim(r.ctx, supervise.Claim{Token: r.token, Project: "legion", Tree: heldRoleIssue, Issue: heldRoleIssue,
		Role: claim.RoleArchitect, Generation: 1, State: supervise.StateSuspended, Session: "ses-1",
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		r.t.Fatalf("the idle release's suspension: %v", err)
	}
	r.published()
}

// The resume of a released claim brings back the session the claim kept, and the claim holds its
// role in every state the resume passes through (supervise.HoldsRole): queued; launch_uncertain,
// which a launch a crash cut short leaves; launching; shim_connected, the state the shim's hello
// writes (supervise's helloed); registered, the state the agent's registration writes; and ready.
// So no write of the resume queues a publication, and the set Dispatch is sent names the kept
// session after each. One row per state, each written over the released claim on its own.
//
// The control is the code before this change, which read a role as held only while a process ran
// (supervise.HoldsCapability): there the release had already sent the empty set, so the four
// states before the registration leave it empty, and the registration and the ready each queue the
// set back, so every row fails there.
func TestAResumeOfTheKeptSessionKeepsTheRoleInEveryStateOfIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
	}{
		{"queued", supervise.StateQueued},
		{"launch_uncertain", supervise.StateLaunchUncertain},
		{"launching", supervise.StateLaunching},
		{"the hello, shim_connected", supervise.StateShimConnected},
		{"the registration, registered", supervise.StateRegistered},
		{"ready", supervise.StateReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newHeldRoleRig(t)
			r.released()
			r.step("the resume's write of "+string(tc.state)+" over the kept session", tc.state, "", "ses-1", false, "ses-1")
		})
	}
}

// The boot itself runs the pass: a daemon that starts over a running session queues the
// publication of its issue in openWorkflow, before the outbox attaches, so the first sweep sends
// it. The session is seeded with a statement of its own, as a release that published nothing
// leaves it, so no claim write queued its row. A boot that skips the pass, or a pass that queues
// nothing, leaves the session unpublished until the next change of a claim on its issue.
func TestTheWorkflowsBootQueuesTheRolePublicationOfALiveClaim(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	seedSilentPhaseWorker(t, pool, "CAPTURE-701", time.Now().UTC())
	if issues, rows := queuedRoleHolders(t, pool); rows != 0 {
		t.Fatalf("control: the seed left role-holder publications for %v, want none, so only the boot can queue one", issues)
	}
	bootedTickWorkflow(t, pool)
	if issues, rows := queuedRoleHolders(t, pool); rows != 1 || issues[0] != "CAPTURE-701" {
		t.Fatalf("after the boot the outbox holds role-holder publications for %v, want the one of CAPTURE-701, whose implementer runs", issues)
	}
}

// An end whose claim write failed with an error the daemon owes (here a broken connection,
// supervise's writeOwed) is not lost: the claim's next decision makes the write, and that write
// queues the issue's publication in its own transaction (store's putClaim), so the ended session
// leaves Dispatch's set at the repair, with no restart and no other claim's change. Until then the
// store holds the claim live, so nothing is queued, and nothing should be: a publication would read
// the session as holding its role. Each end is driven from an idle claim through its machine over
// the real store: a stop and a suspension.
func TestARepairedEndIsPublishedWhenItsWriteLands(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		state supervise.ClaimState
		event func(claim.Token) supervise.Event
	}{
		{supervise.StateRetired, func(token claim.Token) supervise.Event {
			return supervise.RequestStop{Claim: token, By: supervise.SuspendedByOperator}
		}},
		{supervise.StateSuspended, func(token claim.Token) supervise.Event {
			return supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByOperator}
		}},
	} {
		t.Run(string(end.state), func(t *testing.T) {
			rig := newOwedWriteRig(t, unmeasuredClaimWrite, owedRole{claim.RoleTester, supervise.StateIdle})
			// The store of a daemon with a workflow, which publishes (store.PublishRoleHolders).
			rig.st.PublishRoleHolders(nil)
			client := &roleHoldersDispatch{}
			lane := &outbox{pool: rig.pool, records: record.NewStore(), dispatch: client, dispatchProject: gatePublishProject,
				now: func() time.Time { return time.Now().UTC() }, log: quietLogger()}
			published := func() []roleHoldersPublished {
				t.Helper()
				if drained, _ := lane.drain(rig.ctx, "", []record.OutboxKind{record.OutboxKindRoleHoldersPublish}); !drained {
					t.Fatal("the role-holders lane did not drain")
				}
				return client.taken()
			}
			// The boot over the running tester publishes it (queueRoleHoldersPublish).
			booted := &workflowRuntime{pool: rig.pool, records: record.NewStore(), dispatchProject: gatePublishProject, log: quietLogger()}
			if err := booted.queueRoleHoldersPublish(rig.ctx); err != nil {
				t.Fatalf("the boot's queue of the role-holder publications: %v", err)
			}
			live := []roleHoldersPublished{{Issue: owedIssue, Holders: []dispatch.RoleHolder{{Role: "tester", Session: "ses_tester"}}}}
			if got := published(); !reflect.DeepEqual(got, live) {
				t.Fatalf("the booted tester published %+v, want %+v", got, live)
			}

			rig.refuse(claim.RoleTester, brokenConnection())
			if err := rig.handle(claim.RoleTester, end.event(rig.token(claim.RoleTester))); err == nil {
				t.Fatal("the end whose claim write the store refused returned no error")
			}
			if held, stored := rig.held(claim.RoleTester), rig.stored(claim.RoleTester); held != end.state || stored != supervise.StateIdle {
				t.Fatalf("control: after the refused end memory holds %s and the row %s, want %s over the idle row", held, stored, end.state)
			}
			if issues, rows := queuedRoleHolders(t, rig.pool); rows != 0 {
				t.Fatalf("the refused end queued role-holder publications for %v, want none while the row is live", issues)
			}

			rig.answers()
			if err := rig.handle(claim.RoleTester, rig.nudge(claim.RoleTester)); err != nil {
				t.Fatalf("the decision after the database answered again = %v, want none", err)
			}
			if stored := rig.stored(claim.RoleTester); stored != end.state {
				t.Fatalf("the repair left the row %s, want %s", stored, end.state)
			}
			ended := []roleHoldersPublished{{Issue: owedIssue, Holders: []dispatch.RoleHolder{}}}
			if got := published(); !reflect.DeepEqual(got, ended) {
				t.Fatalf("after the repair landed the lane published %+v, want %+v: the ended tester holds no role", got, ended)
			}
		})
	}
}

// What becomes of a row is decided by Dispatch's answer alone. An outage is retried as a failure:
// a transport failure, a 404 NOT_FOUND "not found" that a handler answers for a row it did not find
// (Dispatch's writeHandlerError), a 5xx, and a 400 with no code (from whatever stands in front of
// Dispatch). The 404 that a Dispatch without the route answers under its router's code NOT_FOUND,
// "no route for …", is a wait, so the daemon and Dispatch deploy in either order with no ERROR
// line while they do (TestARoleHoldersPublicationWaitsForADispatchThatHasNoRouteForIt). A refusal
// that no retry changes ends the row: the route's 404 ISSUE_NOT_FOUND, and a 400 with Dispatch's
// code, which names a set that Dispatch will never take. A 400 ends the row with one ERROR line
// that names the issue and Dispatch's code, and nothing else: the issue's next ready or end of a
// claim queues a fresh publication, which reads the live set again.
func TestRoleHoldersExecutorRetriesARefusal(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-208"
	r := newRoleHoldersRig(t)
	logs := &syncBuffer{}
	// At DEBUG, since the router's 404 meets the row at its second attempt, where a wait logs DEBUG.
	r.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.seedClaim(issue, claim.RoleTester, "ses-1", supervise.StateWorking, time.Minute)
	r.client.answers = []error{
		fmt.Errorf("publish the role holders of %s: %w", issue, errors.New("Dispatch is down")),
		noRouteError("no route for PUT /api/v1/issues/" + issue + "/role-holders"),
		&dispatch.Error{Status: http.StatusNotFound, Code: oauth2calltest.Said("NOT_FOUND"), Message: "not found"},
		&dispatch.Error{Status: http.StatusInternalServerError, Code: oauth2calltest.Said("INTERNAL"), Message: "internal server error"},
		&dispatch.Error{Status: http.StatusBadRequest, Message: "<html>400 Bad Request</html>"},
		&dispatch.Error{Status: http.StatusBadRequest, Code: oauth2calltest.Said("INVALID_ROLE_HOLDER"), Message: "the session of tester is not a session id."},
		&dispatch.Error{Status: http.StatusNotFound, Code: oauth2calltest.Said(dispatch.IssueNotFound), Message: "no issue " + issue},
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
		{what: "a handler's 404 NOT_FOUND for a row it did not find", rows: 1, after: time.Minute},
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

	var refused, failed, waited []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "role-holders publish refused by Dispatch") {
			refused = append(refused, line)
		}
		if strings.Contains(line, `msg="outbox row failed"`) {
			failed = append(failed, line)
		}
		if strings.Contains(line, `msg="outbox row waits"`) {
			waited = append(waited, line)
		}
	}
	if len(failed) != 4 {
		t.Errorf("logged %d failed rows, want the four retried failures:\n%s", len(failed), logs.String())
	}
	if len(waited) != 1 || !strings.Contains(waited[0], "no route for PUT") {
		t.Errorf("logged the waits %q, want one, the router's 404:\n%s", waited, logs.String())
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

// A Dispatch older than the route answers each publication with its router's 404 NOT_FOUND "no
// route for …" (routes/router.go, staticHandler), and the daemon and Dispatch deploy in either
// order. So the row waits rather than fails: it logs `outbox row waits` and no ERROR line, at INFO
// on its first run, DEBUG on the next six, and WARN from its seventh retry, when its backoff has
// reached its cap of about a minute (waitWarnAttempts); each run leaves the row in the outbox with
// its next run ahead of it. Once Dispatch serves the route, the next run lands the live set and
// ends the row. The stand-in answers as the router does, through the daemon's real HTTP client,
// so the client's reading of that body is what decides.
func TestARoleHoldersPublicationWaitsForADispatchThatHasNoRouteForIt(t *testing.T) {
	t.Parallel()
	const issue = "LEGION-208"
	r := newRoleHoldersRig(t)
	logs := &syncBuffer{}
	r.runner.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r.seedClaim(issue, claim.RoleTester, "ses-1", supervise.StateWorking, time.Minute)

	var mu sync.Mutex
	routed, asked := false, 0
	var landed [][]dispatch.RoleHolder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		w.Header().Set("Content-Type", "application/json")
		if !routed || req.Method != http.MethodPut || req.URL.Path != "/api/v1/issues/"+issue+"/role-holders" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": "NOT_FOUND", "error": "no route for " + req.Method + " " + req.URL.Path, "hint": "GET /api/v1 lists every route",
			})
			return
		}
		var body struct {
			Holders []dispatch.RoleHolder `json:"holders"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		landed = append(landed, body.Holders)
		_ = json.NewEncoder(w).Encode(map[string]any{"issue": issue, "holders": body.Holders})
	}))
	t.Cleanup(server.Close)
	r.runner.dispatch = dispatch.New(server.URL, dispatch.BearerTransport("agent-token", nil))
	run := func() {
		t.Helper()
		if drained, _ := r.runner.drain(r.ctx, "", []record.OutboxKind{record.OutboxKindRoleHoldersPublish}); !drained {
			t.Fatal("the role-holders lane did not drain")
		}
	}
	r.enqueue(issue)

	const tries = waitWarnAttempts + 1
	for try := 1; try <= tries; try++ {
		run()
		var attempts int
		var nextAt time.Time
		if err := r.pool.QueryRow(r.ctx, `select attempts, next_at from outbox where kind = $1 and issue = $2`,
			string(record.OutboxKindRoleHoldersPublish), issue).Scan(&attempts, &nextAt); err != nil {
			t.Fatalf("after try %d, read the row: %v", try, err)
		}
		if attempts != try || !nextAt.After(r.clock()) {
			t.Fatalf("after try %d the row has %d attempts and runs next at %s, want %d attempts and a run after %s", try, attempts, nextAt, try, r.clock())
		}
		mu.Lock()
		reached := asked
		mu.Unlock()
		if reached != try {
			t.Fatalf("after try %d the stand-in was asked %d times, want once a try", try, reached)
		}
		r.ahead += 2 * time.Minute
	}

	mu.Lock()
	routed = true
	mu.Unlock()
	run()
	if want := [][]dispatch.RoleHolder{{{Role: "tester", Session: "ses-1"}}}; !reflect.DeepEqual(landed, want) {
		t.Fatalf("once the route was served the stand-in took %+v, want %+v", landed, want)
	}
	if rows := outboxRows(t, r.pool); rows != 0 {
		t.Fatalf("after the publication landed the outbox holds %d rows, want none", rows)
	}

	var levels []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "level=ERROR") {
			t.Errorf("logged an ERROR line while Dispatch had no route: %s", line)
		}
		if strings.Contains(line, `msg="outbox row waits"`) {
			for _, field := range strings.Fields(line) {
				if level, ok := strings.CutPrefix(field, "level="); ok {
					levels = append(levels, level)
				}
			}
		}
	}
	want := []string{"INFO", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "DEBUG", "WARN"}
	if !slices.Equal(levels, want) {
		t.Fatalf("the waits logged at %v, want %v:\n%s", levels, want, logs.String())
	}
}
