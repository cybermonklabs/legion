package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	reconcileRoot       = "LEGION-600"
	reconcileChild      = "LEGION-601"
	reconcileGrandchild = "LEGION-602"
	reconcileDoc        = "artifact-600"
)

// reconcilePair is the pair of versions most cases here are owed: the spec from version 4, which
// the holders took, to version 5, which the gate stands approved at.
var reconcilePair = record.VersionPair{ArtifactID: reconcileDoc, From: 4, To: 5}

// changeSetDispatch is a Dispatch that answers the two reads the reconcile of a tree's news makes,
// and records each: what changed between two versions (answer, which defaults to one section and
// one requirement changed) and the version approved before one (before, which refuses by default,
// so a case that does not expect the review history read fails if it is made). Every other call
// reaches the nil Client it embeds.
type changeSetDispatch struct {
	dispatch.Client
	mu      sync.Mutex
	answer  func(pair record.VersionPair) (dispatch.ArtifactChanges, error)
	before  func(artifact string, version int) (int, error)
	changes []record.VersionPair
	history []int
}

func (d *changeSetDispatch) ArtifactChanges(_ context.Context, artifact string, from, to int) (dispatch.ArtifactChanges, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	pair := record.VersionPair{ArtifactID: artifact, From: from, To: to}
	d.changes = append(d.changes, pair)
	if d.answer != nil {
		return d.answer(pair)
	}
	return dispatch.ArtifactChanges{ArtifactID: artifact, From: from, To: to,
		Sections: []dispatch.SectionChange{{Heading: "Design", Change: dispatch.ChangeChanged}},
		Blocks:   []dispatch.BlockChange{{ID: "r2", Type: "requirement", Change: dispatch.ChangeChanged}}}, nil
}

func (d *changeSetDispatch) ApprovedVersionBefore(_ context.Context, artifact string, version int) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.history = append(d.history, version)
	if d.before == nil {
		return 0, errors.New("this case reads no review history")
	}
	return d.before(artifact, version)
}

// newsReconcileRig is one tree on a database of its own, gated on its spec, which stands approved at
// version 5, with the outbox that runs its design_changes rows and its starts over a fake Dispatch
// and a fake runtime. The root architect took version 5 unless a case says otherwise, so it is owed
// nothing of its own.
type newsReconcileRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records record.Store
	client  *changeSetDispatch
	runner  *outbox
	sup     *supervisor
	rt      *fake.Runtime
}

func newNewsReconcileRig(t *testing.T) *newsReconcileRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	client := &changeSetDispatch{}
	r := &newsReconcileRig{t: t, ctx: context.Background(), pool: pool, records: records, client: client, sup: sup, rt: rt}
	r.runner = &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, dispatch: client,
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), gatedSteps: config.DesignGateRootIssues.Steps(), now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + reconcileRoot}, nil
		},
		remove: func(context.Context, workspace.Workspace, sync.Locker) error { return nil },
	}
	openTree(t, pool, "LEGION", reconcileRoot)
	// The root stands in reviewing until a case records it where it needs it: its gate row needs
	// its record first.
	r.member(reconcileRoot, "", phase.Reviewing)
	r.inTx(func(tx pgx.Tx) error {
		return records.PutGate(r.ctx, tx, record.DesignGate{Issue: reconcileRoot, Step: record.DesignStepSpec, ArtifactID: reconcileDoc,
			LatestVersion: 5, ApprovedVersion: new(5), ApprovedMark: record.EventMark{Seq: 30}, EverOpened: true})
	})
	r.told(reconcileRoot, record.AudienceArchitect, 5)
	return r
}

func (r *newsReconcileRig) inTx(write func(pgx.Tx) error) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, write); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// member records key in the tree in phase at, the tree's root when it is reconcileRoot, the child
// of parent otherwise.
func (r *newsReconcileRig) member(key, parent string, at phase.Phase) record.Issue {
	r.t.Helper()
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: reconcileRoot, Title: key, Phase: at, Generation: 1, Status: "in_progress", Rank: key}
	if key != reconcileRoot {
		issue.Parent = &parent
	}
	putOutboxIssue(r.t, r.pool, r.records, issue)
	return issue
}

// told records that issue's audience took version of the spec, replacing whatever the row held
// (the rig's default for the architect among it), where a raise would keep the greater; a negative
// version is a row that knows no version of it.
func (r *newsReconcileRig) told(issue string, audience record.DesignAudience, version int) {
	r.t.Helper()
	versions := map[record.DesignStep]int{}
	if version >= 0 {
		versions[record.DesignStepSpec] = version
	}
	encoded, err := json.Marshal(versions)
	if err != nil {
		r.t.Fatal(err)
	}
	r.inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(r.ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
			values ($1, $2, $3, false, now()) on conflict (issue, audience) do update set versions = excluded.versions`,
			issue, string(audience), encoded)
		return err
	})
}

// owesOpening records that the opening is owed to issue's audience.
func (r *newsReconcileRig) owesOpening(issue string, audience record.DesignAudience) {
	r.t.Helper()
	r.inTx(func(tx pgx.Tx) error { return r.records.SetOpeningOwed(r.ctx, tx, issue, audience, true) })
}

// holder records role as the holder of issue's phase at entry 1, and its claim as the record holds
// it: in state, suspended by by when it is suspended, and given entry given.
func (r *newsReconcileRig) holder(issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource, given int64) claim.Token {
	r.t.Helper()
	token := mustClaimToken(r.t, issue, role)
	r.inTx(func(tx pgx.Tx) error {
		if role != claim.RoleArchitect {
			if err := r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: issue, Role: role, Claim: token, Entry: 1,
				EntryTask: "Work the phase.", EntryRaisedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		_, err := tx.Exec(r.ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			serving_generation, launch_failures, prompt_failures, prompt_retires, uncertain_streak, given_entry, suspended_by)
			values ($1, 'legion', $2, $3, $4, 1, 'ses', '/sessions/ses.jsonl', $5, 1, 0, 0, 0, 0, $6, $7)`,
			string(token), reconcileRoot, issue, string(role), string(state), given, string(by))
		return err
	})
	return token
}

// machine is the claim of issue's role as its machine holds it: in state, suspended by by, and
// given entry given, with a session to resume.
func (r *newsReconcileRig) machine(issue string, role claim.Role, state supervise.ClaimState, by supervise.SuspendSource, given int64) *supervise.Machine {
	r.t.Helper()
	machine, _, err := r.sup.Create(r.ctx, supervise.Claim{Token: mustClaimToken(r.t, issue, role), Project: "legion", Tree: reconcileRoot,
		Issue: issue, Role: role, State: state, Generation: 1, Session: "ses-" + string(role), SessionFile: "/tmp/" + string(role) + ".jsonl",
		SuspendedBy: by, GivenEntry: given}, "")
	if err != nil {
		r.t.Fatalf("create the %s claim of %s: %v", role, issue, err)
	}
	return machine
}

// record writes the change set of pair as Dispatch read it, or as unavailable with reason.
func (r *newsReconcileRig) record(pair record.VersionPair, reason string) {
	r.t.Helper()
	changes := record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, ReadAt: time.Now().UTC(),
		Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}}}
	if reason != "" {
		changes = record.DesignChanges{VersionPair: pair, State: record.DesignChangesUnavailable, Reason: reason, ReadAt: time.Now().UTC()}
	}
	r.inTx(func(tx pgx.Tx) error { return r.records.PutDesignChanges(r.ctx, tx, changes) })
}

// reconcile runs the tree's design_changes row as the attempt after attempts failed ones.
func (r *newsReconcileRig) reconcile(attempts int) error {
	r.t.Helper()
	return r.runner.designChanges(r.ctx, record.OutboxRow{ID: 1, Kind: record.OutboxKindDesignChanges, Issue: reconcileRoot, Attempts: attempts})
}

// recorded is the change set the record holds of pair, and whether it holds one.
func (r *newsReconcileRig) recorded(pair record.VersionPair) (record.DesignChanges, bool) {
	r.t.Helper()
	var changes record.DesignChanges
	var held bool
	r.inTx(func(tx pgx.Tx) error {
		recorded, err := r.records.DesignChangesOf(r.ctx, tx, []record.VersionPair{pair})
		changes, held = recorded[pair]
		return err
	})
	return changes, held
}

// toldOf is issue's audience row as the record holds it, the zero row where there is none.
func (r *newsReconcileRig) toldOf(issue string, audience record.DesignAudience) record.DesignTold {
	r.t.Helper()
	var found record.DesignTold
	r.inTx(func(tx pgx.Tx) error {
		rows, err := r.records.TreeTold(r.ctx, tx, reconcileRoot)
		for _, row := range rows {
			if row.Issue == issue && row.Audience == audience {
				found = row
			}
		}
		return err
	})
	return found
}

// newsStarts is every news-only start the outbox holds (record.SuperviseRequest.News), oldest first,
// each with its request.
func (r *newsReconcileRig) newsStarts() []entryStartRow {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, issue, payload from outbox where kind = 'supervise'
		and coalesce((payload->>'news')::boolean, false) order by id`)
	if err != nil {
		r.t.Fatalf("read the news-only starts: %v", err)
	}
	defer rows.Close()
	var starts []entryStartRow
	for rows.Next() {
		var start entryStartRow
		var payload []byte
		if err := rows.Scan(&start.row.ID, &start.row.Issue, &payload); err != nil {
			r.t.Fatalf("read the news-only starts: %v", err)
		}
		start.row.Kind, start.row.Payload, start.row.NextAt = record.OutboxKindSupervise, json.RawMessage(payload), time.Now()
		decoded, err := record.DecodeOutboxPayload(start.row)
		if err != nil {
			r.t.Fatalf("decode news-only start %d: %v", start.row.ID, err)
		}
		start.request = decoded.(record.SuperviseRequest)
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the news-only starts: %v", err)
	}
	return starts
}

// startsTo is who each news-only start goes to, `<issue>/<role>@<phase>`, oldest first.
func startsTo(starts []entryStartRow) []string {
	to := make([]string, len(starts))
	for i, start := range starts {
		to[i] = fmt.Sprintf("%s/%s@%s", start.row.Issue, start.request.Role, start.request.Phase)
	}
	return to
}

// pendingNews is the task machine holds pending, and the news that task carries, decoded; it fails
// the test where the machine holds no task.
func pendingNews(t *testing.T, machine *supervise.Machine) (string, record.DeliveryNews) {
	t.Helper()
	machine.Wait()
	pending := machine.Claim().Pending
	if pending == nil {
		t.Fatalf("the claim %s holds no task, want the one the start delivered", machine.Claim().Token)
	}
	var news record.DeliveryNews
	if len(pending.News) > 0 {
		if err := json.Unmarshal(pending.News, &news); err != nil {
			t.Fatalf("decode the news the task carries: %v", err)
		}
	}
	return pending.Task, news
}

// heldItem is the news of the spec from version from to version 5 to issue's holder.
func heldItem(issue string, from int) record.DeliveryNewsItem {
	return newsItem(issue, record.AudienceHolder, record.DesignStepSpec, reconcileDoc, from, 5)
}

// The reconcile reads each pair of versions it needs once, whoever is owed it: two members that
// took the same version are owed the same change set, which is read from Dispatch once, recorded,
// and told to each by a news-only start of its own.
func TestAChangeSetIsReadOnceAndReusedByTwoMembers(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.holder(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}

	if !reflect.DeepEqual(r.client.changes, []record.VersionPair{reconcilePair}) {
		t.Fatalf("Dispatch was asked for %v, want the one pair both members are owed, once", r.client.changes)
	}
	if changes, held := r.recorded(reconcilePair); !held || changes.State != record.DesignChangesRead || len(changes.Sections) != 1 || len(changes.Blocks) != 1 {
		t.Fatalf("the recorded change set of %+v = %+v (held %t), want it read with its section and its requirement", reconcilePair, changes, held)
	}
	if got, want := startsTo(r.newsStarts()), []string{reconcileRoot + "/reviewer@reviewing", reconcileChild + "/implementer@implementing"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the news-only starts = %v, want %v: one for each member, from the one change set", got, want)
	}
}

// A Dispatch without the changes route answers 404 or 405 (dispatch.ErrNoChangesRoute): the route
// comes with a release, not with a wait, so the pair is recorded unavailable at once, with
// Dispatch's refusal as its reason, and the news goes, naming the versions and why the change list
// could not be read. The row finishes without a retry.
func TestAPairADispatchWithoutTheChangesRouteCannotReadIsRecordedUnavailableWithNoRetry(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.client.answer = func(record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{}, fmt.Errorf("%w: GET /api/v1/artifacts/%s/changes: 404 NOT_FOUND", dispatch.ErrNoChangesRoute, reconcileDoc)
	}

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile under a Dispatch without the route = %v, want the row finished: no retry brings the route", err)
	}
	changes, held := r.recorded(reconcilePair)
	if !held || changes.State != record.DesignChangesUnavailable || !strings.Contains(changes.Reason, "dispatch has no changes route") {
		t.Fatalf("the recorded change set = %+v (held %t), want it unavailable with Dispatch's refusal as the reason", changes, held)
	}
	if len(r.client.changes) != 1 {
		t.Fatalf("Dispatch was asked %d times, want once", len(r.client.changes))
	}
	if got := startsTo(r.newsStarts()); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts = %v, want the reviewer's: an unavailable pair is told with its reason", got)
	}
}

// A document Dispatch no longer holds answers the changes route 404 ARTIFACT_NOT_FOUND
// (dispatch.ErrDocumentNotFound): a deleted document does not come back with a wait, so its pair is
// recorded unavailable at once, as for a Dispatch without the route, with a reason that says the
// document is gone and not that the route is missing. The row finishes without a retry, and the
// news goes.
func TestAPairOfADocumentDispatchNoLongerHoldsIsRecordedUnavailableWithNoRetry(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.client.answer = func(record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{}, fmt.Errorf("%w: GET /api/v1/artifacts/%s/changes: 404 ARTIFACT_NOT_FOUND: artifact not found",
			dispatch.ErrDocumentNotFound, reconcileDoc)
	}

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile for a document Dispatch no longer holds = %v, want the row finished: no retry brings it back", err)
	}
	changes, held := r.recorded(reconcilePair)
	if !held || changes.State != record.DesignChangesUnavailable || !strings.Contains(changes.Reason, "dispatch holds no such document") ||
		strings.Contains(changes.Reason, "changes route") {
		t.Fatalf("the recorded change set = %+v (held %t), want it unavailable, its reason saying the document is gone", changes, held)
	}
	if len(r.client.changes) != 1 {
		t.Fatalf("Dispatch was asked %d times, want once", len(r.client.changes))
	}
	if got := startsTo(r.newsStarts()); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts = %v, want the reviewer's: an unavailable pair is told with its reason", got)
	}
}

// Any other failed read is ridden out on the row's backoff: the attempt returns the error, so the
// outbox retries it, and records nothing; the row's third attempt records the pair unavailable with
// the last error instead, so the news goes and a merger held for it is released.
func TestARunOfFailedReadsIsRetriedThenRecordedUnavailable(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.client.answer = func(record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{}, errors.New("dispatch: 500 INTERNAL: the database is restarting")
	}

	for attempts := range designChangesAttempts - 1 {
		if err := r.reconcile(attempts); err == nil || !strings.Contains(err.Error(), "500 INTERNAL") {
			t.Fatalf("attempt %d under a failing Dispatch = %v, want the failure returned for the outbox to retry", attempts+1, err)
		}
		if _, held := r.recorded(reconcilePair); held {
			t.Fatalf("attempt %d recorded the pair, want nothing recorded before the last attempt", attempts+1)
		}
		if starts := r.newsStarts(); len(starts) != 0 {
			t.Fatalf("attempt %d queued %v, want no news before its change set is recorded", attempts+1, startsTo(starts))
		}
	}
	if err := r.reconcile(designChangesAttempts - 1); err != nil {
		t.Fatalf("the last attempt under a failing Dispatch = %v, want the pair recorded unavailable and the row finished", err)
	}
	changes, held := r.recorded(reconcilePair)
	if !held || changes.State != record.DesignChangesUnavailable || !strings.Contains(changes.Reason, "failed 3 reads") ||
		!strings.Contains(changes.Reason, "500 INTERNAL") {
		t.Fatalf("the recorded change set = %+v (held %t), want it unavailable with the count of reads and the last error", changes, held)
	}
	if len(r.client.changes) != designChangesAttempts {
		t.Fatalf("Dispatch was asked %d times, want once an attempt, %d", len(r.client.changes), designChangesAttempts)
	}
	if got := startsTo(r.newsStarts()); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts = %v, want the reviewer's", got)
	}
}

// A change of decision blocks alone tells nobody anything of the work: each holder's told version,
// and the root architect's, rises straight to the approved one, and nothing is sent. The one
// exception is a holder owed the opening (the gate closed on that version, the holder was told so,
// and a person approved it later): it is owed a task with the opening and the words that nothing its
// issue holds changed, so its news-only start is queued, and its told version waits for that task's
// confirmation.
func TestADecisionOnlyChangeRaisesWhatEachHolderTookAndSendsNothing(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.owesOpening(reconcileChild, record.AudienceHolder)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.holder(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}, nil
	}

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}

	for _, audience := range []record.DesignAudience{record.AudienceHolder, record.AudienceArchitect} {
		if took, _ := r.toldOf(reconcileRoot, audience).Took(record.DesignStepSpec); took != 5 {
			t.Fatalf("%s's %s took %d after a decision-only change, want it raised to 5", reconcileRoot, audience, took)
		}
	}
	if took, _ := r.toldOf(reconcileChild, record.AudienceHolder).Took(record.DesignStepSpec); took != 4 {
		t.Fatalf("%s's holder, owed the opening, took %d, want 4 until the task that tells it is confirmed", reconcileChild, took)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileChild + "/implementer@implementing"}) {
		t.Fatalf("the news-only starts = %v, want the one to the holder owed the opening", got)
	}

	machine := r.machine(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{"The design gate of LEGION-600 opened", "Nothing your issue holds changed since version 4, which your phase took."} {
		if !strings.Contains(task, want) {
			t.Fatalf("the task = %q, want it to say %q", task, want)
		}
	}
	if strings.Contains(task, "Sections:") || len(news.Openings) != 1 || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileChild, 4)}) {
		t.Fatalf("the task = %q carrying %+v, want no change list, the opening and the one item", task, news)
	}
}

// The reconcile sends a member's news to the claim that holds its phase, by a news-only start of
// its own: a start of no entry and no task, naming the phase it tells, which reads what it carries
// when it runs. Run against a reviewer that was given its phase's entry and is between turns, it
// hands the reviewer its news as a task.
func TestANewsOnlyStartIsQueuedForALiveReviewer(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	want := record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer, Generation: 1, Phase: phase.Reviewing, News: true}
	if len(starts) != 1 || starts[0].row.Issue != reconcileRoot || !reflect.DeepEqual(starts[0].request, want) {
		t.Fatalf("the news-only starts = %+v, want one %+v on %s", starts, want, reconcileRoot)
	}

	machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	if !strings.Contains(task, "version 5, since version 4, which your phase last took") || !strings.Contains(task, `"Design" changed`) {
		t.Fatalf("the reviewer's task = %q, want the spec's pair and its change list", task)
	}
	if !news.NewsOnly || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the reviewer's task carries %+v, want the news-only item of the spec from 4 to 5", news)
	}
}

// A news-only start goes only where nothing else will carry the news, and never where the record
// says nothing may: not to a claim owed its phase start (that start carries the news), not to a
// retired or failed claim (the phase's own repair revives it with a start that carries the news),
// not to a claim an operator suspended, and not into a tree that lingers or to an issue not at
// work, which are owed nothing. The reconcile queues none for such a claim, and a news-only start
// that meets one when it runs finishes without acting. The live claim is the control: the same
// reconcile queues its start, and the same start hands it the news.
func TestNoNewsOnlyStartGoesWhereTheRecordSaysNoNewsMay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state supervise.ClaimState
		by    supervise.SuspendSource
		given int64
		// lingers puts the tree in its linger, and held stands the member in held.
		lingers, held bool
		queued        bool
	}{
		{name: "a live claim", state: supervise.StateIdle, given: 1, queued: true},
		{name: "a claim owed its phase start", state: supervise.StateIdle, given: 0},
		{name: "a retired claim", state: supervise.StateRetired, given: 1},
		{name: "a failed claim", state: supervise.StateFailed, given: 1},
		{name: "an operator-suspended claim", state: supervise.StateSuspended, by: supervise.SuspendedByOperator, given: 1},
		{name: "a lingering tree", state: supervise.StateIdle, given: 1, lingers: true},
		{name: "an issue not at work", state: supervise.StateIdle, given: 1, held: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			root := r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			r.holder(reconcileRoot, claim.RoleReviewer, tc.state, tc.by, tc.given)
			if tc.lingers {
				lingers := time.Now().Add(time.Hour)
				root.LingerUntil = &lingers
				putOutboxIssue(t, r.pool, r.records, root)
			}
			if tc.held {
				root.Phase, root.Hold = phase.Held, &record.Hold{From: phase.Reviewing}
				putOutboxIssue(t, r.pool, r.records, root)
			}

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			if queued := len(starts) == 1; queued != tc.queued || len(starts) > 1 {
				t.Fatalf("the news-only starts = %v, want one queued: %t", startsTo(starts), tc.queued)
			}
			if _, recorded := r.recorded(reconcilePair); recorded == (tc.lingers || tc.held) {
				t.Fatalf("the pair recorded = %t, want it recorded only where a member is owed it", recorded)
			}

			// The start as it would meet the claim when it runs, queued or not.
			machine := r.machine(reconcileRoot, claim.RoleReviewer, tc.state, tc.by, tc.given)
			row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
				Generation: 1, Phase: phase.Reviewing, News: true}, time.Now())
			if tc.held {
				row = mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
					Generation: 1, Phase: phase.Held, News: true}, time.Now())
			}
			if err := r.runner.execute(r.ctx, row); err != nil {
				t.Fatalf("run the news-only start: %v", err)
			}
			machine.Wait()
			if delivered := machine.Claim().Pending != nil; delivered != tc.queued {
				t.Fatalf("the claim holds %+v after the news-only start, want a task delivered: %t", machine.Claim().Pending, tc.queued)
			}
			if calls := r.rt.Calls(); !tc.queued && len(calls) != 0 {
				t.Fatalf("the news-only start made %d runtime calls, want none: it finishes without acting", len(calls))
			}
		})
	}
}

// A claim whose worker has not yet taken the task it holds — a delivery pending with no
// confirmation — takes no second task before it does: a news-only start queued then would only
// wait behind it, retrying on its backoff. So the reconcile queues none, and the news stays owed
// for the sweep's next reconcile or the phase's next start, which is what the root architect's
// summary says of the member (workflow.NewsAtNextStart), never that a task tells it now. A claim
// whose pending task a turn has taken is the control: its confirmation is recorded, the reconcile
// queues its start, and the summary says a task tells it now.
func TestNoNewsOnlyStartIsQueuedBehindATaskTheHolderHasNotTaken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
	}{
		{name: "a task not taken yet"},
		{name: "a task a turn took", confirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			token := r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateWorking, "", 1)
			r.inTx(func(tx pgx.Tx) error {
				_, err := tx.Exec(r.ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
					values ($1, 'outbox:7', 'Review the change.', now(), 1, 'reviewing', case when $2 then now() end)`, string(token), tc.confirmed)
				return err
			})

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			if queued := len(starts) == 1; queued != tc.confirmed || len(starts) > 1 {
				t.Fatalf("the news-only starts = %v, want one queued: %t", startsTo(starts), tc.confirmed)
			}
			if took, _ := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); took != 4 {
				t.Fatalf("the reviewer's told version = %d, want 4: the news stays owed until a task carrying it is taken", took)
			}
			var drafts []memberDraft
			r.inTx(func(tx pgx.Tx) error {
				root, err := r.records.Issue(r.ctx, tx, reconcileRoot)
				if err != nil {
					return err
				}
				drafts, err = readTreeMembers(r.ctx, tx, r.records, config.DesignGateRootIssues.Steps(), *root)
				return err
			})
			want := workflow.NewsAtNextStart
			if tc.confirmed {
				want = workflow.NewsSentNow
			}
			if len(drafts) != 1 || drafts[0].state != want {
				t.Fatalf("the summary's members = %+v, want the reviewer's issue told %q", drafts, want)
			}
		})
	}
}

// A claim an idle pod release suspended still holds its role, and the news goes to it: the
// reconcile queues its news-only start, and the start wakes the claim as any start wakes one,
// resuming its kept session and handing it the news.
func TestANewsOnlyStartWakesAClaimAnIdleReleaseSuspended(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, 1)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
		t.Fatalf("the news-only starts = %v, want the released reviewer's", got)
	}

	machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased, 1)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the news-only start: %v", err)
	}
	if resumes := resumesOf(r.rt, machine.Claim().Token); len(resumes) != 1 {
		t.Fatalf("the start resumed the released reviewer's session %d times, want once", len(resumes))
	}
	if _, news := pendingNews(t, machine); !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the woken reviewer's task carries %+v, want the spec from 4 to 5", news)
	}
}

// The root architect is owed its own summary of the tree, and the holder form of each member in
// accepting it owns: one news-only start carries both, queued once for the tree, and a second run
// of the reconcile while that start is still to run queues no second.
func TestTheRootArchitectsStartIsQueuedOncePerTree(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)

	for run := range 2 {
		if err := r.reconcile(0); err != nil {
			t.Fatalf("reconcile %d of the tree's news: %v", run+1, err)
		}
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/architect@"}) {
		t.Fatalf("the news-only starts after two reconciles = %v, want the root architect's one", got)
	}

	machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the root architect's news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{
		"A person approved a new version of the design of LEGION-600.",
		"The approved design of LEGION-600 changed for LEGION-601, which you hold in accepting:",
		"sign LEGION-601 off with `sign_off`",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("the root architect's task = %q, want it to say %q", task, want)
		}
	}
	if len(news.Items) != 2 {
		t.Fatalf("the root architect's task carries %+v, want its own item and the accepting member's", news)
	}
}

// A member in accepting is held by the architect that owns it: a sub-architect running for its
// parent owns it, so the news goes to that sub-architect's claim, in the holder form, and the root
// architect, owed nothing of its own, is queued nothing.
func TestASubArchitectsStartIsQueuedForItsAcceptingChild(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.member(reconcileGrandchild, reconcileChild, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileChild, record.AudienceHolder, 5)
	r.told(reconcileGrandchild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.holder(reconcileChild, claim.RoleArchitect, supervise.StateWorking, "", 0)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileChild + "/architect@"}) {
		t.Fatalf("the news-only starts = %v, want the sub-architect's of %s alone", got, reconcileChild)
	}

	machine := r.machine(reconcileChild, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the sub-architect's news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	if !strings.Contains(task, "changed for LEGION-602, which you hold in accepting:") || strings.Contains(task, "Each issue of the tree") {
		t.Fatalf("the sub-architect's task = %q, want the holder form of its accepting child and no summary of the tree", task)
	}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileGrandchild, 4)}) {
		t.Fatalf("the sub-architect's task carries %+v, want the accepting child's item", news)
	}
}

// A told row that knows no version of a step is resolved into the record, once: the reconcile reads
// the version approved before the new one from Dispatch's review history and writes it into that
// row, so every later reader names the same pair. With an earlier approval the pair runs from it
// and its change set is read; with none, the pair runs from version 0, which Dispatch's route
// refuses and nothing needs to compare, so it is recorded read with no change listed and Dispatch
// is not asked.
func TestAnUnknownBaselineIsReadFromTheReviewHistoryIntoTheToldRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		before    int
		readsSets int
	}{
		{name: "an earlier approval", before: 3, readsSets: 1},
		{name: "no earlier approval", before: 0, readsSets: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, -1)
			r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
			r.client.before = func(artifact string, version int) (int, error) {
				if artifact != reconcileDoc || version != 5 {
					return 0, fmt.Errorf("asked the history of %s before %d, want %s before 5", artifact, version, reconcileDoc)
				}
				return tc.before, nil
			}

			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}

			if took, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); !known || took != tc.before {
				t.Fatalf("the holder's told version = %d (known %t), want %d written from the review history", took, known, tc.before)
			}
			if len(r.client.history) != 1 || len(r.client.changes) != tc.readsSets {
				t.Fatalf("Dispatch was asked the history %d times and the change set %d times, want once and %d", len(r.client.history), len(r.client.changes), tc.readsSets)
			}
			pair := record.VersionPair{ArtifactID: reconcileDoc, From: tc.before, To: 5}
			if changes, held := r.recorded(pair); !held || changes.State != record.DesignChangesRead || (tc.before == 0 && len(changes.Sections)+len(changes.Blocks) != 0) {
				t.Fatalf("the recorded change set of %+v = %+v (held %t), want it recorded read", pair, changes, held)
			}
			if got := startsTo(r.newsStarts()); !reflect.DeepEqual(got, []string{reconcileRoot + "/reviewer@reviewing"}) {
				t.Fatalf("the news-only starts = %v, want the reviewer's", got)
			}

			// A second reconcile reads the history no more: the row knows the version now.
			if err := r.reconcile(0); err != nil || len(r.client.history) != 1 {
				t.Fatalf("a second reconcile = %v reading the history %d times, want no error and no second read", err, len(r.client.history))
			}
		})
	}
}

// A phase start carries the news its role is owed, read from the record when the start runs: the
// reviewer's start carries what the tester was owed and never took — its delivery was dropped when
// the tester completed — since the told row is the issue's, not a session's. The control is the
// same start while the pair's change set is not recorded yet: it carries its own text alone and
// the news stays owed.
func TestAPhaseStartIntoReviewingCarriesTheNewsTheTesterNeverTook(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool
	}{
		{name: "the change set recorded", recorded: true},
		{name: "the change set not recorded yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.member(reconcileRoot, "", phase.Reviewing)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateQueued, "", 0)
			if tc.recorded {
				r.record(reconcilePair, "")
			}
			const own = "Review the change. Issue: LEGION-600. Phase: reviewing."
			row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
				Generation: 1, Phase: phase.Reviewing, Entry: 1, Task: own}, time.Now())

			if err := r.runner.execute(r.ctx, row); err != nil {
				t.Fatalf("run the reviewer's start: %v", err)
			}
			machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleReviewer))
			if !found {
				t.Fatal("the reviewer's start created no claim")
			}
			task, news := pendingNews(t, machine)
			if !tc.recorded {
				if task != own || len(news.Items) != 0 {
					t.Fatalf("the reviewer's task = %q carrying %+v, want its own text alone: an unrecorded pair stays owed", task, news)
				}
				return
			}
			if !strings.HasPrefix(task, own+"\n\nThe approved design of LEGION-600 changed for LEGION-600, which you hold in reviewing:") ||
				!strings.Contains(task, "version 5, since version 4, which your phase last took") {
				t.Fatalf("the reviewer's task = %q, want its own text, a blank line, then the news the tester never took", task)
			}
			if news.Task != own || news.NewsOnly || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
				t.Fatalf("the reviewer's task carries %+v, want its own text and the spec from 4 to 5", news)
			}
		})
	}
}

// The held phase's retry hands the role the work of the entry it is still on (ResendEntry), and
// its task carries the news the role is owed as any phase start's does (R5).
func TestAHeldRestartsTaskCarriesTheNews(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleImplementer, supervise.StateIdle, "", 1)
	r.record(reconcilePair, "")
	// The worker confirmed the entry's last task and its turn ended: it holds that entry as given
	// and no task, which is what the retry's mark is for.
	machine := r.machine(reconcileRoot, claim.RoleImplementer, supervise.StateIdle, "", 1)
	const retry = "Retry the phase. Reason: the operator retried it."
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleImplementer,
		Generation: 1, Phase: phase.Implementing, Entry: 1, ResendEntry: true, Task: retry}, time.Now())

	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("run the held restart: %v", err)
	}
	task, news := pendingNews(t, machine)
	if !strings.HasPrefix(task, retry+"\n\n") || !strings.Contains(task, "version 5, since version 4, which your phase last took") {
		t.Fatalf("the restart's task = %q, want its own text and the news after it", task)
	}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the restart's task carries %+v, want the spec from 4 to 5", news)
	}
}

// A news-only start names the phase of the member it tells, and a member that has left that phase
// has a new holder whose own start carries the news: the start finishes without acting, handing
// the old phase's claim nothing. The same start once the member stands in that phase again is the
// control: it hands the claim its news.
func TestANewsOnlyStartForAMemberWhosePhaseMovedFinishesWithoutActing(t *testing.T) {
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Testing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.record(reconcilePair, "")
	machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleReviewer,
		Generation: 1, Phase: phase.Reviewing, News: true}, time.Now())

	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("run the news-only start of a phase the member left: %v", err)
	}
	machine.Wait()
	if pending := machine.Claim().Pending; pending != nil || len(r.rt.Calls()) != 0 {
		t.Fatalf("the reviewer's claim holds %+v and the runtime saw %d calls, want nothing: the start finishes without acting",
			pending, len(r.rt.Calls()))
	}

	root.Phase = phase.Reviewing
	putOutboxIssue(t, r.pool, r.records, root)
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("run the news-only start once the member stands in reviewing: %v", err)
	}
	if _, news := pendingNews(t, machine); !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the reviewer's task carries %+v, want the spec from 4 to 5", news)
	}
}

// A member in accepting is held by the architect that owns it, here the tree's root architect: the
// reconcile queues the news-only start on that architect's claim, and the start hands it the holder
// form of the member's news — the change list and the accepting answer, sign-off or a move back —
// and no summary of the tree, since the root architect took the approved version itself.
func TestANewsOnlyStartForAnAcceptingMemberReachesItsOwningArchitectInTheHolderForm(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Implementing)
	r.member(reconcileChild, reconcileRoot, phase.Accepting)
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)

	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	starts := r.newsStarts()
	if got := startsTo(starts); !reflect.DeepEqual(got, []string{reconcileRoot + "/architect@"}) {
		t.Fatalf("the news-only starts = %v, want the owning root architect's alone", got)
	}

	machine := r.machine(reconcileRoot, claim.RoleArchitect, supervise.StateIdle, "", 0)
	if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
		t.Fatalf("run the architect's news-only start: %v", err)
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{"changed for LEGION-601, which you hold in accepting:", "sign LEGION-601 off with `sign_off`",
		"move it back to planning, implementing or testing"} {
		if !strings.Contains(task, want) {
			t.Fatalf("the architect's task = %q, want the holder form to say %q", task, want)
		}
	}
	if strings.Contains(task, "Each issue of the tree") {
		t.Fatalf("the architect's task = %q, want no summary of the tree: the architect took the approved version", task)
	}
	if !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileChild, 4)}) {
		t.Fatalf("the architect's task carries %+v, want the accepting member's item", news)
	}
}

// A merger released before it reads what changed could merge it, so its start waits while its
// member is owed news whose change set is not recorded yet; once the pair is recorded the same row
// runs, and the merger's task carries the news.
func TestTheMergersStartWaitsWhileItsMembersChangeSetIsUnrecorded(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	const own = "Merge it."
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleMerger,
		Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}, time.Now())

	if err := r.runner.execute(r.ctx, row); !errors.Is(err, wait.ErrWaiting) {
		t.Fatalf("the merger's start with its news unrecorded = %v, want it held (wait.ErrWaiting)", err)
	}
	if spawns := len(r.rt.CallsOf("Spawn")); spawns != 0 {
		t.Fatalf("the runtime spawned %d sessions while the merger's news was unrecorded, want none", spawns)
	}

	r.record(reconcilePair, "")
	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the merger's start once its news is recorded = %v, want it run", err)
	}
	machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if !found {
		t.Fatal("the merger's start created no claim")
	}
	task, news := pendingNews(t, machine)
	if !strings.HasPrefix(task, own+"\n\n") || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 4)}) {
		t.Fatalf("the merger's task = %q carrying %+v, want its own text and the spec from 4 to 5", task, news)
	}
}

// The review history is read under the bound a change set is: a merger whose record knows no
// version it took waits while Dispatch keeps failing to say which version came before, each attempt
// of the design_changes row returning the failure for its retry, and the row's third attempt fills
// the step with 0 and records the pair from 0 unavailable, naming the failed read. That releases the
// merger within the row's bounded attempts, and its task says the change list could not be read and
// why, never that the version is a first approval.
func TestThreeFailedHistoryReadsLeaveThePairUnavailableAndReleaseTheMerger(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Merging)
	r.told(reconcileRoot, record.AudienceHolder, -1)
	r.holder(reconcileRoot, claim.RoleMerger, supervise.StateQueued, "", 0)
	r.client.before = func(string, int) (int, error) { return 0, errors.New("dispatch: 502 BAD_GATEWAY") }
	const own = "Merge it."
	row := mustOutboxRow(t, reconcileRoot, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleMerger,
		Generation: 1, Phase: phase.Merging, Entry: 1, Task: own}, time.Now())
	unread := record.VersionPair{ArtifactID: reconcileDoc, From: 0, To: 5}

	for attempts := range designChangesAttempts - 1 {
		if err := r.reconcile(attempts); err == nil || !strings.Contains(err.Error(), "502 BAD_GATEWAY") {
			t.Fatalf("attempt %d under a failing review history = %v, want the failure returned for the outbox to retry", attempts+1, err)
		}
		if _, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); known {
			t.Fatalf("attempt %d wrote a taken version, want none before the last attempt", attempts+1)
		}
		if err := r.runner.execute(r.ctx, row); !errors.Is(err, wait.ErrWaiting) {
			t.Fatalf("the merger's start after attempt %d = %v, want it held (wait.ErrWaiting)", attempts+1, err)
		}
	}
	if err := r.reconcile(designChangesAttempts - 1); err != nil {
		t.Fatalf("the last attempt under a failing review history = %v, want the pair recorded unavailable and the row finished", err)
	}
	if took, known := r.toldOf(reconcileRoot, record.AudienceHolder).Took(record.DesignStepSpec); !known || took != 0 {
		t.Fatalf("the merger's told version = %d (known %t), want 0 filled", took, known)
	}
	changes, held := r.recorded(unread)
	if !held || changes.State != record.DesignChangesUnavailable ||
		!strings.Contains(changes.Reason, "the version this issue last took could not be read from the review history") ||
		!strings.Contains(changes.Reason, "502 BAD_GATEWAY") {
		t.Fatalf("the recorded change set of %+v = %+v (held %t), want it unavailable, naming the failed history read", unread, changes, held)
	}
	if len(r.client.history) != designChangesAttempts || len(r.client.changes) != 0 {
		t.Fatalf("Dispatch was asked the history %d times and a change set %d times, want %d and none", len(r.client.history),
			len(r.client.changes), designChangesAttempts)
	}

	if err := r.runner.execute(r.ctx, row); err != nil {
		t.Fatalf("the merger's start once the pair is recorded = %v, want it released", err)
	}
	machine, found := r.sup.Machine(mustClaimToken(t, reconcileRoot, claim.RoleMerger))
	if !found {
		t.Fatal("the merger's start created no claim")
	}
	task, news := pendingNews(t, machine)
	for _, want := range []string{"version 5, since a version the daemon could not read",
		"The change list could not be read: the version this issue last took could not be read from the review history"} {
		if !strings.Contains(task, want) {
			t.Fatalf("the merger's task = %q, want it to say %q", task, want)
		}
	}
	if strings.Contains(task, "first version a person approved") || !reflect.DeepEqual(news.Items, []record.DeliveryNewsItem{heldItem(reconcileRoot, 0)}) {
		t.Fatalf("the merger's task = %q carrying %+v, want no first approval and the item from the filled 0", task, news)
	}
}

// The entry sweep's tick is the backstop for news no fact queued a reconcile for: a tree owed news
// with no design_changes row queued gets one, a tree owed nothing gets none, and the next tick
// queues no second beside the one still to run.
func TestTheSweepQueuesTheReconcileOfATreeOwedNewsAndOfNoOther(t *testing.T) {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	const quiet = "LEGION-700"
	putOutboxIssue(t, r.pool, r.records, record.Issue{Key: quiet, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: quiet, Title: quiet, Phase: phase.Reviewing, Generation: 1, Status: "in_progress", Rank: quiet})
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: quiet, Step: record.DesignStepSpec, ArtifactID: "artifact-700",
			LatestVersion: 5, ApprovedVersion: new(5), EverOpened: true}); err != nil {
			return err
		}
		if err := r.records.RaiseTold(r.ctx, tx, quiet, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, time.Now()); err != nil {
			return err
		}
		return r.records.RaiseTold(r.ctx, tx, quiet, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 5}, time.Now())
	})
	sweeper := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(), dispatchProject: "LEGION",
		log: quietLogger()}
	queued := func() map[string]int {
		t.Helper()
		rows, err := r.pool.Query(r.ctx, `select issue from outbox where kind = 'design_changes'`)
		if err != nil {
			t.Fatalf("read the design_changes rows: %v", err)
		}
		issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the design_changes rows: %v", err)
		}
		got := map[string]int{}
		for _, issue := range issues {
			got[issue]++
		}
		return got
	}
	if got := queued(); len(got) != 0 {
		t.Fatalf("design_changes rows before the tick = %v, want none: the case's own premise", got)
	}

	for tick := range 2 {
		if err := sweeper.queueOwedDesignNews(r.ctx); err != nil {
			t.Fatalf("tick %d of the sweep: %v", tick+1, err)
		}
		if got, want := queued(), map[string]int{reconcileRoot: 1}; !reflect.DeepEqual(got, want) {
			t.Fatalf("design_changes rows after tick %d = %v, want %v: the tree owed news gets one, the tree owed nothing none", tick+1, got, want)
		}
	}
}
