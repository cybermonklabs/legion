package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The issues of these tests: LEGION-208 and LEGION-300, two roots at work, and LEGION-500 at work
// under LEGION-208, which a person moves under LEGION-300; LEGION-300's primary document.
const (
	reparentOldParent = "LEGION-208"
	reparentNewParent = "LEGION-300"
	reparentIssue     = "LEGION-500"
	reparentDoc       = "artifact-300"
)

// reparentDispatch is outboxDispatch with each document version's markdown, by document id and
// version.
type reparentDispatch struct {
	outboxDispatch
	versions map[string]string
}

func (d *reparentDispatch) DocumentVersion(_ context.Context, artifactID string, version int) (string, error) {
	markdown, ok := d.versions[fmt.Sprintf("%s@%d", artifactID, version)]
	if !ok {
		return "", fmt.Errorf("no version %d of %s", version, artifactID)
	}
	return markdown, nil
}

// reparentRig is the daemon's own path of a change of parent: the handlers a Dispatch event goes
// through (the mirror, then the workflow engine), the outbox that runs the seed and the notices,
// the architect claims of all three issues, running in the supervisor and recorded in the store,
// and a publisher that records each publish.
type reparentRig struct {
	t         *testing.T
	pool      *store.Pool
	records   record.Store
	engine    *workflow.Engine
	runner    *outbox
	publisher *holderPublisher
	seq       int64
}

func newReparentRig(t *testing.T, client *reparentDispatch) *reparentRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, issue := range []record.Issue{
		routingIssue(reparentOldParent, reparentOldParent, ""),
		routingIssue(reparentNewParent, reparentNewParent, ""),
		routingIssue(reparentIssue, reparentOldParent, reparentOldParent),
	} {
		issue.Rank = issue.Key
		putChainIssue(t, pool, records, issue)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	for _, key := range []string{reparentOldParent, reparentNewParent, reparentIssue} {
		token := architectClaim(t, sup, key, supervise.StateWorking)
		if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
			values ($1, 'legion', $2, $2, 'architect', 1, $3, $4, 'working', 0, 0, 0, 0, '')`,
			string(token), key, "ses-"+key, "/sessions/"+key+".jsonl"); err != nil {
			t.Fatalf("record the architect claim of %s: %v", key, err)
		}
	}
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	publisher := &holderPublisher{}
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, dispatch: client, notices: publisher, supervisor: sup,
		project: "legion", handlers: []intake.Handler{engine}, now: time.Now}
	return &reparentRig{t: t, pool: pool, records: records, engine: engine, runner: runner, publisher: publisher, seq: 1}
}

// move applies Dispatch's event of the moved issue under parent through the handlers in the
// daemon's order, the mirror first, and runs the outbox until nothing is due: the seed reads the
// new parent and its fact queues the notices, which the runner publishes.
func (r *reparentRig) move(parent string) {
	r.t.Helper()
	r.seq++
	fact := intake.DispatchIssue{Key: reparentIssue, ResolvedRepo: "acme/widgets", Seq: r.seq, Type: "issue.updated", Status: "in_progress",
		Title: reparentIssue, Parent: parent, Rank: reparentIssue}
	if _, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", fmt.Sprintf("move-%d", r.seq), fact,
		blockers.NewMirror(r.records, "LEGION"), r.engine); err != nil {
		r.t.Fatalf("apply the move under %s: %v", parent, err)
	}
	for range 4 {
		if err := r.runner.RunOnce(context.Background()); err != nil {
			r.t.Fatalf("run the outbox: %v", err)
		}
	}
}

// told is each reparented notice published, by the issue whose architect's role topic it reached.
func (r *reparentRig) told() map[string][]string {
	r.t.Helper()
	_, delivered := r.publisher.snapshot()
	told := map[string][]string{}
	for _, published := range delivered {
		notice := published.payload.(record.Notice)
		if notice.Kind != record.ReparentedNotice {
			continue
		}
		for _, key := range []string{reparentOldParent, reparentNewParent, reparentIssue} {
			if published.topic == architectTopic(r.t, key) {
				told[key] = append(told[key], notice.Reason)
			}
		}
	}
	return told
}

// When the Dispatch parent of an issue in Legion's work changes, the daemon tells, once and on each
// role topic, the issue's own architect, the architect above it before and the architect above it
// now (R18 of dispatch://INFRA-3320), each the same words, naming the old and the new parent and
// what goes on. A second event under the same parent tells nobody again.
func TestAReparentTellsTheOwnOldAndNewArchitectOnce(t *testing.T) {
	client := &reparentDispatch{}
	client.issue = dispatch.Issue{Key: reparentNewParent, Status: "in_progress", Labels: []string{"design"}}
	rig := newReparentRig(t, client)

	rig.move(reparentNewParent)
	told := rig.told()
	if got := slices.Sorted(maps.Keys(told)); !slices.Equal(got, []string{reparentOldParent, reparentNewParent, reparentIssue}) {
		t.Fatalf("the reparented notices reached the architects of %v, want those of %s, %s and %s", got, reparentIssue, reparentOldParent, reparentNewParent)
	}
	for key, reasons := range told {
		if len(reasons) != 1 {
			t.Fatalf("the architect of %s was told %d times, want once: %q", key, len(reasons), reasons)
		}
		if !strings.Contains(reasons[0], "changed from LEGION-208 to LEGION-300") || !strings.Contains(reasons[0], "phase implementing") {
			t.Fatalf("the architect of %s read %q, want the old and the new parent and the phase", key, reasons[0])
		}
	}

	rig.move(reparentNewParent)
	for key, reasons := range rig.told() {
		if len(reasons) != 1 {
			t.Fatalf("after a second event under the same parent the architect of %s was told %d times, want still once", key, len(reasons))
		}
	}
}

// A change of parent is told at whatever phase entry of its run the issue stands (R18 of
// dispatch://INFRA-3320), as every notice is (record.LateAt, the architect's ruling of 2026-11-21
// on dispatch://INFRA-3320): the issue moving into a new phase entry between the seed's read and
// the notices' delivery fences none of them, and none of a notice of another kind queued at the
// earlier entry either. A notice of an earlier run is what is not delivered, which
// TestALateEffectOfAnEarlierRunOrPhaseEntryDoesNothing holds.
func TestAReparentedNoticeIsToldAtWhateverPhaseEntryTheIssueStands(t *testing.T) {
	ctx := context.Background()
	client := &reparentDispatch{}
	client.issue = dispatch.Issue{Key: reparentNewParent, Status: "in_progress", Labels: []string{"design"}}
	rig := newReparentRig(t, client)
	rig.seq++
	fact := intake.DispatchIssue{Key: reparentIssue, ResolvedRepo: "acme/widgets", Seq: rig.seq, Type: "issue.updated", Status: "in_progress",
		Title: reparentIssue, Parent: reparentNewParent, Rank: reparentIssue}
	if _, err := intake.ApplyFact(ctx, rig.pool, "dispatch", "move-entry", fact, blockers.NewMirror(rig.records, "LEGION"), rig.engine); err != nil {
		t.Fatalf("apply the move under %s: %v", reparentNewParent, err)
	}
	// One pass runs the rows due when it starts: the seed, whose fact queues the notices.
	if err := rig.runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the seed: %v", err)
	}
	if told := rig.told(); len(told) != 0 {
		t.Fatalf("the seed's own pass published %v, want the notices still queued: the case's premise", told)
	}
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		// A notice of another kind, queued at the entry the notices were queued at.
		row, err := record.NewFencedRow(ctx, tx, rig.records, reparentIssue,
			record.Notice{Kind: "phase-finished", Role: claim.RoleImplementer, Phase: phase.Implementing}, time.Now())
		if err != nil {
			return err
		}
		if err := rig.records.Enqueue(ctx, tx, row); err != nil {
			return err
		}
		// The implementer's new phase entry, which raises the issue's (record.IssueFence).
		return rig.records.PutPhase(ctx, tx, record.PhaseRow{Issue: reparentIssue, Role: claim.RoleImplementer, Entry: 3, EntryRaisedAt: time.Now()})
	}); err != nil {
		t.Fatalf("queue the phase-finished notice and raise the entry of %s: %v", reparentIssue, err)
	}
	for range 3 {
		if err := rig.runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	}
	if got := slices.Sorted(maps.Keys(rig.told())); !slices.Equal(got, []string{reparentOldParent, reparentNewParent, reparentIssue}) {
		t.Fatalf("after %s entered a new phase entry the reparented notices reached the architects of %v, want those of %s, %s and %s",
			reparentIssue, got, reparentIssue, reparentOldParent, reparentNewParent)
	}
	_, delivered := rig.publisher.snapshot()
	finished := 0
	for _, published := range delivered {
		if notice := published.payload.(record.Notice); notice.Kind == "phase-finished" {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("the phase-finished notice of the earlier entry was published %d times, want once: a notice of an earlier phase entry of its run is delivered", finished)
	}
}

// The seed reads the new parent: its labels and status, its primary document's approval and that
// version's requirement blocks. Where the parent carries the legion label, is not accepted, holds a
// person's approval of its latest version and marks a requirement block there, each architect is
// told R1's path: the issue can be written as a view on the parent and register its design gate on
// that approval (dispatch://INFRA-3320, R1 and R2). Where any of those fails, the words name no
// such path.
func TestAReparentNoticeNamesR1sPathUnderAnApprovedParent(t *testing.T) {
	approvedAt := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	person := dispatch.Actor{Kind: "user", ID: "alice"}
	session := dispatch.Actor{Kind: "session", ID: "session-architect"}
	const requirements = ":::requirement{#r1}\n**R1.** The first requirement.\n:::\n"
	approved := func(version, latest int, by dispatch.Actor) dispatch.Approval {
		return dispatch.Approval{IssueKey: reparentNewParent, State: "approved", Version: &version, LatestVersion: latest, By: by, At: approvedAt, Primary: true}
	}
	path := "LEGION-300 carries the legion label and marks requirement blocks in its primary document. " +
		"A person approved version 7 of that document, which is its latest version. So the architect of LEGION-500 can write it as a view on LEGION-300. " +
		"Then that architect can register the design gate of LEGION-500 on that approval, with no approval of its own, with register_gate's basis."
	for _, tc := range []struct {
		name     string
		labels   []string
		status   string
		approval dispatch.Approval
		markdown string
		opens    bool
	}{
		{"a parent in Legion whose approved latest version marks requirement blocks", []string{"legion"}, "in_progress", approved(7, 7, person), requirements, true},
		{"a parent whose approved latest version marks no requirement block", []string{"legion"}, "in_progress", approved(7, 7, person), "No requirement here.\n", false},
		{"a parent with no legion label", []string{"design"}, "in_progress", approved(7, 7, person), requirements, false},
		{"a parent Dispatch shows in retro", []string{"legion"}, "retro", approved(7, 7, person), requirements, false},
		{"a parent whose latest version no person approved", []string{"legion"}, "in_progress", approved(7, 7, session), requirements, false},
		{"a parent approved at a version below its latest", []string{"legion"}, "in_progress", approved(6, 7, person), requirements, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &reparentDispatch{versions: map[string]string{reparentDoc + "@7": tc.markdown, reparentDoc + "@6": tc.markdown}}
			client.issue = dispatch.Issue{Key: reparentNewParent, Status: tc.status, Labels: tc.labels, PrimaryArtifactID: reparentDoc}
			client.approval = tc.approval
			rig := newReparentRig(t, client)
			rig.move(reparentNewParent)
			told := rig.told()
			if len(told) != 3 {
				t.Fatalf("the reparented notices reached %d architects, want 3: %v", len(told), told)
			}
			for key, reasons := range told {
				if len(reasons) != 1 || strings.Contains(reasons[0], path) != tc.opens {
					t.Fatalf("the architect of %s read %q; R1's path named %t, want %t", key, reasons, !tc.opens, tc.opens)
				}
			}
		})
	}
}

// A re-parent seed of INFRA-4728's shape, which a daemon of that release queued and left at this
// release's first boot, names no run: it still decodes, and its read is told as R18 says, the old
// parent read from the record (workflow's reparentRead).
func TestAReparentSeedOfTheReleaseBeforeStillDecodesAndIsTold(t *testing.T) {
	client := &reparentDispatch{}
	client.issue = dispatch.Issue{Key: reparentNewParent, Status: "in_progress"}
	rig := newReparentRig(t, client)
	putCopyRow(t, rig.pool, reparentIssue, reparentNewParent, "in_progress")
	if err := pgx.BeginFunc(context.Background(), rig.pool, func(tx pgx.Tx) error {
		issue, err := rig.records.Issue(context.Background(), tx, reparentIssue)
		if err != nil {
			return err
		}
		issue.ToldParent = reparentNewParent
		if err := rig.records.PutIssue(context.Background(), tx, *issue); err != nil {
			return err
		}
		// The row as the release before encoded it: NewOutboxRow refuses a seed of no run now, so
		// the row is enqueued as it was left, and only the decoder reads it.
		return rig.records.Enqueue(context.Background(), tx, record.OutboxRow{Kind: record.OutboxKindReparentSeed, Issue: reparentIssue,
			Payload: json.RawMessage(`{"parent":"LEGION-300","tree":"LEGION-300","ran":"it ran as a tree of its own"}`), NextAt: time.Now()})
	}); err != nil {
		t.Fatalf("leave the release before's seed: %v", err)
	}
	for range 4 {
		if err := rig.runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the outbox: %v", err)
		}
	}
	told := rig.told()
	if len(told) != 3 || !strings.Contains(told[reparentIssue][0], "changed from LEGION-208 to LEGION-300") {
		t.Fatalf("the release before's seed told %v, want each of the three architects the move from LEGION-208", told)
	}
	if rows := outboxRowsOfKind(t, rig.pool, reparentIssue, record.OutboxKindReparentSeed); rows != 0 {
		t.Fatalf("%d re-parent seeds left, want the release before's finished", rows)
	}
}
