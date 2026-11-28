package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// observedAt is the fixed instant the observed rig's source reads its state at.
var observedAt = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// observedRig is a store of project LEGION, token legion, seeded (seedObserved) with an issue in
// every phase, an issue for every word of admission.held (record.HeldReasons), phases rows of
// several roles, pull requests, design gates, slots and claims, and the daemon's own state source
// over it (source), configured from stateGoldenSettingsFile as the daemon's serve configures it and
// reading the fixed clock observedAt. The record store writes with the state events on, and the
// seed writes with them off, as a daemon's records stand before a boot turns them on.
type observedRig struct {
	*stateRig
	claims *store.Store
	// scoped is the rig's record store scoped to the project (projectRecords), as the workflow
	// and the state route read and write through it.
	scoped projectRecords
	src    *source
	// keys is every issue key the seed records, in the order it records them.
	keys []string
}

func newObservedRig(t *testing.T) *observedRig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion.yaml")
	if err := os.WriteFile(path, []byte(stateGoldenSettingsFile), 0o600); err != nil {
		t.Fatalf("write legion.yaml: %v", err)
	}
	cfg, err := config.Load(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("load legion.yaml: %v", err)
	}
	r := &observedRig{stateRig: newStateRig(t)}
	if r.claims, err = store.Open(context.Background(), r.pool.Config().ConnString()); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(r.claims.Close)
	r.scoped = projectRecords{Store: r.records, project: cfg.Project}
	// No design-gate step and no admission cap, as internal/projection's
	// TestEveryIssueInTheWorkflowHasAPlace projects the fixture this seed records: the fourteen
	// words of the ladder read the same over these records with nothing promoted.
	r.src = &source{store: r.claims, records: r.records, project: cfg.Project, projectToken: "legion", runtime: cfg.Runtime.Name,
		startedAt: observedAt.Add(-time.Hour), now: func() time.Time { return observedAt }, settings: api.SettingsOf(cfg),
		experiments: cfg.ExperimentsDeclared(), stateRun: stateTestRun,
		deployment: func([]sandbox.CacheStatus) capabilities.Deployment { return capabilities.Deployment{} },
		reports:    func() map[claim.Token]capabilities.Report { return nil }}
	r.seedObserved(t)
	return r
}

// observedIssue is an issue of the seed: a labelled issue of project LEGION in tree, ranked by its
// own key, as internal/projection's lineIssue records one.
func observedIssue(key, tree, parent, status string, stands phase.Phase) record.Issue {
	issue := record.Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: key, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if parent != "" {
		issue.Parent = record.ParentOf(parent)
	}
	return issue
}

// observedClaim is the claim of issue's role, in tree, at state.
func observedClaim(t *testing.T, issue, tree string, role claim.Role, state supervise.ClaimState) supervise.Claim {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("the token of %s's %s: %v", issue, role, err)
	}
	return supervise.Claim{Token: token, Project: "legion", Tree: tree, Issue: issue, Role: role, Generation: 1,
		State: state, Session: "ses_" + string(token)}
}

// seedObserved records the rig's store. Its first part is internal/projection's
// TestEveryIssueInTheWorkflowHasAPlace over the real store, one issue for each of the fourteen
// words of admission.held: a member of a closed tree (LEGION-101) and of a parked tree
// (LEGION-111), a root whose label is off (LEGION-120), a root whose Dispatch chain reaches another
// live root (LEGION-140), a member of a tree whose root waits on a person (LEGION-151), a member
// under a shut design gate (LEGION-161), a child an unfinished Dispatch link holds (LEGION-171), an
// issue past its review (LEGION-172, at_work), an issue beneath it (LEGION-173, accepted_ancestor),
// an issue none of the others is true of (LEGION-174), a parent whose own children are still open
// (LEGION-181), a sub-parent in awaiting_children on its open child (LEGION-186 over LEGION-187,
// awaiting_children), a parent held before its retro for the child at work beneath it
// (LEGION-195 over LEGION-196, children_before_retro) and a root whose task was stopped in its
// merge (LEGION-197, task_stopped). Its second part is a running tree, LEGION-300, whose members
// stand in the phases the first part leaves out — planning, testing, reviewing, accepting, retro,
// verifying (LEGION-307, a merged child whose close waits for its checks) and held — and its
// third the phases rows, pull requests and claims both views of the state
// read: rows of several roles whose claims stand, a row whose claim is gone, claims no row names,
// an architect, claims on issues the workflow does not record, and the daemon's own controller's
// claim.
func (r *observedRig) seedObserved(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	opened, answeredAt, releasedAt := observedAt.Add(-time.Hour), observedAt.Add(-30*time.Minute), observedAt.Add(-45*time.Minute)

	unlabelled := observedIssue("LEGION-120", "LEGION-120", "", "todo", phase.Admitted)
	unlabelled.HandedOver = false
	underQueued := observedIssue("LEGION-140", "LEGION-140", "LEGION-130", "todo", phase.Admitted)
	underQueued.Ancestry = []string{"LEGION-130"}
	askingRoot := observedIssue("LEGION-150", "LEGION-150", "", "in_progress", phase.AwaitingPerson)
	askingRoot.Awaiting = &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 5}}
	answeredRoot := observedIssue("LEGION-190", "LEGION-190", "", "in_progress", phase.AwaitingPerson)
	answeredRoot.Awaiting = &record.Awaiting{Ask: "ask-q", AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk,
		Since: record.EventMark{Seq: 5}}
	held := observedIssue("LEGION-306", "LEGION-300", "LEGION-300", "in_progress", phase.Held)
	held.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}
	heldParent := observedIssue("LEGION-195", "LEGION-195", "", "needs_review", phase.Held)
	heldParent.Hold = &record.Hold{From: phase.Retro, Reason: record.HoldForChildren}
	stoppedTask := observedIssue("LEGION-197", "LEGION-197", "", "needs_review", phase.Held)
	stoppedTask.Hold = &record.Hold{From: phase.Merging, Reason: record.HoldTaskStopped}
	// The sub-parent holds the start of implementing while its child LEGION-187, which Dispatch
	// shows at todo under it, is open: it stands in awaiting_children with the held start the
	// phase requires (issues_children_wait_shape), holding no slot.
	waitingParent := observedIssue("LEGION-186", "LEGION-185", "LEGION-185", "in_progress", phase.AwaitingChildren)
	waitingParent.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: "Continue LEGION-186. Issue: LEGION-186. Phase: implementing.",
		Reason: "a review asked for changes, while these Dispatch children of LEGION-186 were open: LEGION-187 (todo)", Since: releasedAt}
	issues := []record.Issue{
		observedIssue("LEGION-100", "LEGION-100", "", "done", phase.Done),
		observedIssue("LEGION-101", "LEGION-100", "LEGION-100", "todo", phase.Admitted),
		observedIssue("LEGION-110", "LEGION-110", "", "backlog", phase.Admitted),
		observedIssue("LEGION-111", "LEGION-110", "LEGION-110", "todo", phase.Admitted),
		unlabelled,
		observedIssue("LEGION-130", "LEGION-130", "", "todo", phase.Admitted),
		underQueued,
		askingRoot,
		observedIssue("LEGION-151", "LEGION-150", "LEGION-150", "todo", phase.Admitted),
		observedIssue("LEGION-160", "LEGION-160", "", "todo", phase.Admitted),
		observedIssue("LEGION-161", "LEGION-160", "LEGION-160", "todo", phase.Admitted),
		observedIssue("LEGION-170", "LEGION-170", "", "todo", phase.Admitted),
		observedIssue("LEGION-171", "LEGION-170", "LEGION-170", "todo", phase.Admitted),
		observedIssue("LEGION-172", "LEGION-170", "LEGION-170", "needs_review", phase.Merging),
		observedIssue("LEGION-173", "LEGION-170", "LEGION-172", "todo", phase.Admitted),
		observedIssue("LEGION-174", "LEGION-170", "LEGION-170", "in_progress", phase.Implementing),
		observedIssue("LEGION-175", "LEGION-170", "LEGION-170", "in_progress", phase.Implementing),
		observedIssue("LEGION-180", "LEGION-180", "", "todo", phase.Admitted),
		observedIssue("LEGION-181", "LEGION-180", "LEGION-180", "todo", phase.Admitted),
		answeredRoot,
		heldParent,
		observedIssue("LEGION-196", "LEGION-195", "LEGION-195", "in_progress", phase.Implementing),
		stoppedTask,
		observedIssue("LEGION-185", "LEGION-185", "", "todo", phase.Admitted),
		waitingParent,
		observedIssue("LEGION-300", "LEGION-300", "", "in_progress", phase.Implementing),
		observedIssue("LEGION-301", "LEGION-300", "LEGION-300", "in_progress", phase.Planning),
		observedIssue("LEGION-302", "LEGION-300", "LEGION-300", "in_progress", phase.Testing),
		observedIssue("LEGION-303", "LEGION-300", "LEGION-300", "needs_review", phase.Reviewing),
		observedIssue("LEGION-304", "LEGION-300", "LEGION-300", "needs_review", phase.Accepting),
		observedIssue("LEGION-305", "LEGION-300", "LEGION-300", "in_progress", phase.Retro),
		// A merged child whose close waits for the checks of its goal: verifying holds no slot,
		// and Dispatch shows it at testing.
		observedIssue("LEGION-307", "LEGION-300", "LEGION-300", "testing", phase.Verifying),
		held,
	}
	approved := 1
	openGate := func(root string) record.DesignGate {
		return record.DesignGate{Issue: root, Step: record.DesignStepSpec, ArtifactID: "artifact-" + root, LatestVersion: 1,
			ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: opened}, EverOpened: true}
	}
	rows := []record.PhaseRow{
		// LEGION-300's architect, implementer and planner rows name claims that stand; its tester
		// row names a claim that is gone, so it shows nothing.
		{Issue: "LEGION-300", Role: claim.RoleArchitect, Claim: "legion-legion-legion-300-architect", Entry: 2},
		{Issue: "LEGION-300", Role: claim.RoleImplementer, Claim: "legion-legion-legion-300-implementer", HandoffCommit: "c0ffee",
			Pass: 2, Rounds: 1, BackwardMoves: map[string]int{"testing": 1}, HandoffRecord: "record-300", HandoffVersion: 3, Entry: 4},
		{Issue: "LEGION-300", Role: claim.RolePlanner, Claim: "legion-legion-legion-300-planner", Pass: 1},
		{Issue: "LEGION-300", Role: claim.RoleTester, Claim: "legion-legion-legion-300-tester-retired-long-ago"},
		{Issue: "LEGION-301", Role: claim.RolePlanner, Claim: "legion-legion-legion-301-planner"},
		{Issue: "LEGION-303", Role: claim.RoleImplementer, Claim: "legion-legion-legion-303-implementer", Pass: 1},
		{Issue: "LEGION-303", Role: claim.RoleReviewer, Claim: "legion-legion-legion-303-reviewer", Rounds: 2,
			Decision: &record.ReviewDecision{State: "approved", Body: "ship it", Head: "head-303"}},
		{Issue: "LEGION-303", Role: claim.RoleTester},
		{Issue: "LEGION-150", Role: claim.RoleArchitect, Claim: "legion-legion-legion-150-architect"},
		{Issue: "LEGION-175", Role: claim.RoleImplementer, Claim: "legion-legion-legion-175-implementer", Pass: 1},
		{Issue: "LEGION-175", Role: claim.RolePlanner, Claim: "legion-legion-legion-175-planner"},
		{Issue: "LEGION-306", Role: claim.RoleImplementer, Claim: "legion-legion-legion-306-implementer"},
	}
	r.seed(t, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		for _, issue := range issues {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		for _, slot := range []record.Slot{
			{Issue: "LEGION-150", Index: 0, AdmittedAt: observedAt},
			{Issue: "LEGION-160", Index: 1, AdmittedAt: observedAt},
			{Issue: "LEGION-170", Index: 2, AdmittedAt: observedAt},
			{Issue: "LEGION-175", Index: 3, AdmittedAt: observedAt},
			{Issue: "LEGION-180", Index: 4, AdmittedAt: observedAt},
			{Issue: "LEGION-190", Index: 5, AdmittedAt: observedAt},
			{Issue: "LEGION-300", Index: 6, AdmittedAt: observedAt},
			{Issue: "LEGION-302", Index: 7, AdmittedAt: observedAt},
			{Issue: "LEGION-196", Index: 8, AdmittedAt: observedAt},
		} {
			if err := records.PutSlot(ctx, tx, slot); err != nil {
				return err
			}
		}
		// The two roots that wait for their open child and for an unfinished link have given their
		// slot back for that wait, as the admission pass leaves them (admit.releaseSlotsForWaits).
		for _, key := range []string{"LEGION-170", "LEGION-180"} {
			if err := records.ReleaseSlotForWait(ctx, tx, key, releasedAt); err != nil {
				return err
			}
		}
		for _, tree := range []string{"LEGION-150", "LEGION-160", "LEGION-170", "LEGION-180", "LEGION-185", "LEGION-190", "LEGION-195", "LEGION-197", "LEGION-300"} {
			if _, err := records.OpenTreeLifecycle(ctx, tx, "legion", tree, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
		}
		shut := record.DesignGate{Issue: "LEGION-160", Step: record.DesignStepSpec, ArtifactID: "artifact-LEGION-160", LatestVersion: 2,
			ApprovedVersion: &approved, EverOpened: true}
		for _, gate := range []record.DesignGate{openGate("LEGION-150"), shut, openGate("LEGION-170"), openGate("LEGION-180"),
			openGate("LEGION-185"), openGate("LEGION-195"), openGate("LEGION-197"), openGate("LEGION-300")} {
			if err := records.PutGate(ctx, tx, gate); err != nil {
				return err
			}
		}
		for _, mirrored := range []struct{ key, status, parent string }{
			{"LEGION-130", "todo", ""}, {"LEGION-171", "todo", "LEGION-170"}, {"LEGION-180", "todo", ""},
			{"LEGION-182", "backlog", "LEGION-181"}, {"LEGION-187", "todo", "LEGION-186"}, {"LEGION-900", "todo", ""}, {"LEGION-901", "todo", ""}, {"LEGION-903", "todo", ""},
		} {
			blockers := map[string][]string{"LEGION-130": {"LEGION-903"}, "LEGION-171": {"LEGION-900"}, "LEGION-180": {"LEGION-901"}}[mirrored.key]
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: mirrored.key, Project: "LEGION", Seq: 1,
				Status: record.Carry(mirrored.status), Parent: record.Carry(mirrored.parent), Blockers: record.Carry(append([]string{}, blockers...))}); err != nil {
				return err
			}
		}
		for _, row := range rows {
			if err := records.PutPhase(ctx, tx, row); err != nil {
				return err
			}
		}
		for _, pr := range []record.PullRequest{
			{Issue: "LEGION-303", Repo: "acme/widgets", Number: 303, Branch: "legion/LEGION-303", HeadSHA: "head-303", HeadUpdatedAt: opened,
				State: record.PullRequestOpen, Failing: []string{}},
			{Issue: "LEGION-100", Repo: "acme/widgets", Number: 100, Branch: "legion/LEGION-100", HeadSHA: "head-100", HeadUpdatedAt: opened,
				State: record.PullRequestMerged, Failing: []string{}},
			{Issue: "LEGION-175", Repo: "acme/widgets", Number: 175, Branch: "legion/LEGION-175", HeadSHA: "head-175", HeadUpdatedAt: opened,
				State: record.PullRequestOpen, Failing: []string{"lint"}},
		} {
			if err := records.PutPullRequest(ctx, tx, pr); err != nil {
				return err
			}
		}
		return nil
	})
	for _, c := range []supervise.Claim{
		observedClaim(t, "LEGION-300", "LEGION-300", claim.RoleArchitect, supervise.StateIdle),
		observedClaim(t, "LEGION-300", "LEGION-300", claim.RoleImplementer, supervise.StateWorking),
		observedClaim(t, "LEGION-300", "LEGION-300", claim.RolePlanner, supervise.StateRetired),
		observedClaim(t, "LEGION-301", "LEGION-300", claim.RolePlanner, supervise.StateLaunching),
		// LEGION-302's tester and LEGION-190's architect stand with no phases row naming them.
		observedClaim(t, "LEGION-302", "LEGION-300", claim.RoleTester, supervise.StateWorking),
		observedClaim(t, "LEGION-303", "LEGION-300", claim.RoleImplementer, supervise.StateIdle),
		observedClaim(t, "LEGION-303", "LEGION-300", claim.RoleReviewer, supervise.StateReady),
		observedClaim(t, "LEGION-150", "LEGION-150", claim.RoleArchitect, supervise.StateIdle),
		observedClaim(t, "LEGION-190", "LEGION-190", claim.RoleArchitect, supervise.StateSuspended),
		observedClaim(t, "LEGION-175", "LEGION-170", claim.RoleImplementer, supervise.StateWorking),
		observedClaim(t, "LEGION-175", "LEGION-170", claim.RolePlanner, supervise.StateFailed),
		observedClaim(t, "LEGION-306", "LEGION-300", claim.RoleImplementer, supervise.StateSuspended),
		// Two claims on issues the workflow does not record, an operator's spawns.
		observedClaim(t, "LEGION-7", "LEGION-7", claim.RoleArchitect, supervise.StateLaunching),
		observedClaim(t, "LEGION-8", "LEGION-8", claim.RoleImplementer, supervise.StateWorking),
		// The daemon's own controller, which holds a claim on no issue.
		{Token: claim.ControllerToken("legion"), Project: "legion", Role: claim.RoleController, Generation: 1, State: supervise.StateReady},
	} {
		if err := r.claims.PutClaim(ctx, c); err != nil {
			t.Fatalf("write the claim %s: %v", c.Token, err)
		}
	}
	for _, issue := range issues {
		r.keys = append(r.keys, issue.Key)
	}
}

// readOnly runs read in a repeatable-read, read-only transaction of the rig's store, the snapshot
// the state route reads in, and rolls it back.
func (r *observedRig) readOnly(t *testing.T, read func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	ctx := context.Background()
	tx, err := r.claims.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the state's snapshot: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	read(ctx, tx)
}

// Every read of all issues the state route makes in place of a read for each issue answers each
// issue what that issue's own read answers it, on a store holding an issue in every phase, an
// issue for every word of admission.held, and phases rows of several roles: AllPhases beside
// Phases, row for row, in order and in each column AllPhases reads (listedPhases), PullRequestsOf
// beside PullRequest but for what each pull request remembers of its settled heads, which
// PullRequestsOf loads none of and the state shows none of (listedPullRequest), and Gates beside
// GatesOf. An issue with no row is absent from the bulk answer, which reads as the empty answer its
// own read gives. The seed is held to its own claim first, so the comparison runs over what it
// says.
func TestTheBulkReadsAnswerEachIssueWhatItsOwnReadAnswers(t *testing.T) {
	t.Parallel()
	r := newObservedRig(t)
	r.readOnly(t, func(ctx context.Context, tx pgx.Tx) {
		issues, err := r.scoped.Issues(ctx, tx)
		if err != nil {
			t.Fatalf("read the issues: %v", err)
		}
		var stands []phase.Phase
		for _, issue := range issues {
			if !slices.Contains(stands, issue.Phase) {
				stands = append(stands, issue.Phase)
			}
		}
		for _, want := range phase.All {
			if !slices.Contains(stands, want) {
				t.Fatalf("the seed holds no issue in %s; its phases are %v", want, stands)
			}
		}
		slots, err := r.scoped.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("read the slots: %v", err)
		}
		judged, err := blockers.JudgeAdmission(ctx, tx, r.scoped, "LEGION", r.src.gatedSteps, slots, r.src.admissionCap, observedAt)
		if err != nil {
			t.Fatalf("judge the admission: %v", err)
		}
		for _, word := range record.HeldReasons {
			if !slices.ContainsFunc(judged.Block.Held, func(row record.HeldIssue) bool { return row.Reason == word }) {
				t.Fatalf("the seed holds no issue under %s; admission.held is %#v", word, judged.Block.Held)
			}
		}

		// Every key the seed records, a Dispatch issue the daemon does not record, and a key
		// nothing names.
		keys := append(slices.Clone(r.keys), "LEGION-900", "LEGION-404")
		phases, err := r.scoped.AllPhases(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("read the project's phases rows: %v", err)
		}
		pullRequests, err := r.scoped.PullRequestsOf(ctx, tx, keys)
		if err != nil {
			t.Fatalf("read the pull requests: %v", err)
		}
		gates, err := r.scoped.Gates(ctx, tx, keys)
		if err != nil {
			t.Fatalf("read the design gates: %v", err)
		}
		var withRows, withPullRequests, withGates int
		for _, key := range keys {
			own, err := r.scoped.Phases(ctx, tx, key)
			if err != nil {
				t.Fatalf("read the phases of %s: %v", key, err)
			}
			bulk, listed := phases[key]
			if len(own) == 0 {
				if listed {
					t.Errorf("%s: AllPhases lists %#v, Phases answers none", key, bulk)
				}
			} else if withRows++; !reflect.DeepEqual(bulk, listedPhases(own)) {
				t.Errorf("%s: AllPhases answers %#v, Phases answers %#v", key, bulk, own)
			}

			pr, err := r.scoped.PullRequest(ctx, tx, key)
			if err != nil {
				t.Fatalf("read the pull request of %s: %v", key, err)
			}
			bulkPR, recorded := pullRequests[key]
			if pr == nil {
				if recorded {
					t.Errorf("%s: PullRequestsOf answers %#v, PullRequest answers none", key, bulkPR)
				}
			} else if withPullRequests++; !recorded || listedPullRequest(t, bulkPR) != listedPullRequest(t, *pr) {
				t.Errorf("%s: PullRequestsOf answers %#v (%t), PullRequest answers %#v", key, bulkPR, recorded, *pr)
			}

			ownGates, err := r.scoped.GatesOf(ctx, tx, key)
			if err != nil {
				t.Fatalf("read the design gates of %s: %v", key, err)
			}
			bulkGates, gated := gates[key]
			if len(ownGates) == 0 {
				if gated {
					t.Errorf("%s: Gates answers %#v, GatesOf answers none", key, bulkGates)
				}
			} else if withGates++; !reflect.DeepEqual(bulkGates, ownGates) {
				t.Errorf("%s: Gates answers %#v, GatesOf answers %#v", key, bulkGates, ownGates)
			}
		}
		for key := range phases {
			if !slices.Contains(keys, key) {
				t.Errorf("AllPhases lists rows of %s, which no issue of the seed is", key)
			}
		}
		if withRows < 6 || withPullRequests != 3 || withGates != 8 {
			t.Fatalf("the seed compared %d issues with phases rows, %d with a pull request and %d with gates, want 6 or more, 3 and 8", withRows, withPullRequests, withGates)
		}
	})
}

// listedPullRequest is pr as the state reads it, as JSON: without SettledHeads, which
// PullRequestsOf loads none of and the state shows none of (HeadRun.Settled is never JSON), and
// without the unexported mark of the read that made it, which JSON leaves out.
func listedPullRequest(t *testing.T, pr record.PullRequest) string {
	t.Helper()
	pr.SettledHeads = nil
	encoded, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("encode the pull request of %s: %v", pr.Issue, err)
	}
	return string(encoded)
}

// listedPhases is rows with only the columns AllPhases reads (record.Postgres.AllPhases): the
// issue, the role, the claim, the handoff commit, the pass, the handoff record and its version,
// the rounds, the counts of the moves back, the review decision and the entry.
func listedPhases(rows []record.PhaseRow) []record.PhaseRow {
	listed := make([]record.PhaseRow, len(rows))
	for i, row := range rows {
		listed[i] = record.PhaseRow{Issue: row.Issue, Role: row.Role, Claim: row.Claim, HandoffCommit: row.HandoffCommit,
			Pass: row.Pass, HandoffRecord: row.HandoffRecord, HandoffVersion: row.HandoffVersion, Rounds: row.Rounds,
			BackwardMoves: row.BackwardMoves, Decision: row.Decision, Entry: row.Entry}
	}
	return listed
}

// The observed view (`?view=observed`, source.ObservedState) is the part of the full state
// (source.State) Dispatch observes, read in one snapshot over the seeded store: both list the same
// issues; each issue's key, phase, tree, slot index, wait, architect's claim state and each worker
// role's claim state are the full view's; and the daemon block, the settings and the admission
// block are the full state's own, the admission block byte for byte, since a reader takes an
// issue's place in the line from its index in `admission.waiting`. Both reads answer the same
// claims beside their view, which the route places its caller by.
func TestTheObservedViewIsTheFullStatesObservedPart(t *testing.T) {
	t.Parallel()
	r := newObservedRig(t)
	var full api.State
	var observed api.State
	var fullClaims, observedClaims []supervise.Claim
	r.readOnly(t, func(ctx context.Context, tx pgx.Tx) {
		var err error
		if full, fullClaims, err = r.src.State(ctx, tx); err != nil {
			t.Fatalf("read the full state: %v", err)
		}
		if observed, observedClaims, err = r.src.ObservedState(ctx, tx); err != nil {
			t.Fatalf("read the observed view: %v", err)
		}
	})

	sameJSON(t, "the daemon block", full.Daemon, observed.Daemon)
	sameJSON(t, "the settings", full.Settings, observed.Settings)
	sameJSON(t, "the admission block", full.Admission, observed.Admission)
	tokens := func(claims []supervise.Claim) []claim.Token {
		listed := make([]claim.Token, len(claims))
		for i, current := range claims {
			listed[i] = current.Token
		}
		return listed
	}
	if got, want := tokens(observedClaims), tokens(fullClaims); len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("the observed read answers the claims %v, the full read %v: the route places its caller by the claims of the snapshot it serves", got, want)
	}

	fullKeys, observedKeys := slices.Sorted(maps.Keys(full.Issues)), slices.Sorted(maps.Keys(observed.Issues))
	if !slices.Equal(fullKeys, observedKeys) {
		t.Fatalf("the observed view lists %v, the full state %v", observedKeys, fullKeys)
	}
	var architects, workers, slotted, waiting, unrecorded, treeless int
	for _, key := range fullKeys {
		want, got := full.Issues[key], observed.Issues[key]
		if got.Key != want.Key || got.Phase != want.Phase || got.Tree != want.Tree {
			t.Errorf("%s: the observed view reads key %q, phase %q, tree %q; the full state %q, %q, %q", key, got.Key, got.Phase, got.Tree, want.Key, want.Phase, want.Tree)
		}
		if (got.Slot == nil) != (want.Slot == nil) || got.Slot != nil && got.Slot.Index != want.Slot.Index {
			t.Errorf("%s: the observed view reads slot %+v, the full state %+v", key, got.Slot, want.Slot)
		}
		sameJSON(t, key+"'s waitingOn", want.WaitingOn, got.WaitingOn)
		if (got.Architect == nil) != (want.Architect == nil) || got.Architect != nil && got.Architect.State != want.Architect.State {
			t.Errorf("%s: the observed view reads architect %+v, the full state %+v", key, got.Architect, want.Architect)
		}
		wantWorkers := map[claim.Role]string{}
		for role, worker := range want.Workers {
			wantWorkers[role] = worker.Claim.State
		}
		gotWorkers := map[claim.Role]string{}
		for role, worker := range got.Workers {
			gotWorkers[role] = worker.Claim.State
		}
		if !reflect.DeepEqual(gotWorkers, wantWorkers) {
			t.Errorf("%s: the observed view reads workers %v, the full state %v", key, gotWorkers, wantWorkers)
		}
		if want.Architect != nil {
			architects++
		}
		workers += len(want.Workers)
		if want.Slot != nil {
			slotted++
		}
		if want.WaitingOn != nil {
			waiting++
		}
		if want.Phase == api.Unrecorded {
			unrecorded++
		}
		if want.Tree == "" {
			treeless++
		}
	}
	// The comparison is over every shape the view carries, so a view that dropped one is seen.
	if architects < 3 || workers < 9 || slotted < 5 || waiting < 2 || unrecorded != 2 || treeless != 2 || len(observed.Admission.Waiting) == 0 || len(observed.Admission.Held) == 0 {
		t.Fatalf("the full state holds %d architects, %d workers, %d slots, %d waits, %d unrecorded and %d tree-less issues, %d waiting and %d held: the seed shows too little to compare",
			architects, workers, slotted, waiting, unrecorded, treeless, len(observed.Admission.Waiting), len(observed.Admission.Held))
	}
}

// The observed view is one snapshot, as the full state is: its daemon block's stateSeq, stateId and
// stateRun are read in the transaction its issues and its admission block are read in. A write that
// commits after the snapshot was taken — an issue's phase moved through the record store, which
// queues its state row, so state_publication.last_seq moves at its commit — is in neither the
// issue's phase nor stateSeq of a view built on that snapshot, and in both of a view built on a
// snapshot taken after it.
func TestTheObservedViewIsOneSnapshot(t *testing.T) {
	t.Parallel()
	r := newObservedRig(t)
	ctx := context.Background()
	move := func(to phase.Phase) int64 {
		t.Helper()
		r.commit(t, r.scoped, func(ctx context.Context, tx pgx.Tx, records record.Store) error {
			issue, err := records.Issue(ctx, tx, "LEGION-301")
			if err != nil {
				return err
			}
			issue.Phase = to
			return records.PutIssue(ctx, tx, *issue)
		})
		var seq int64
		if err := r.pool.QueryRow(ctx, `select last_seq from state_publication where project = 'LEGION'`).Scan(&seq); err != nil {
			t.Fatalf("read the project's last state sequence: %v", err)
		}
		return seq
	}
	before := move(phase.Testing)

	tx, err := r.claims.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the state's snapshot: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The snapshot is taken at the transaction's first statement, as the route's own is.
	var listed int
	if err := tx.QueryRow(ctx, `select count(*) from issues`).Scan(&listed); err != nil {
		t.Fatalf("take the snapshot: %v", err)
	}
	after := move(phase.Reviewing)
	if after <= before {
		t.Fatalf("the second move left the project's last state sequence at %d, want it above %d", after, before)
	}

	read := func(tx pgx.Tx) (phase.Phase, int64) {
		t.Helper()
		observed, _, err := r.src.ObservedState(ctx, tx)
		if err != nil {
			t.Fatalf("read the observed view: %v", err)
		}
		if observed.Daemon.StateSeq == nil || observed.Daemon.StateID == "" || observed.Daemon.StateRun != stateTestRun {
			t.Fatalf("the observed view's daemon block = %+v, want its stateSeq, stateId and stateRun", observed.Daemon)
		}
		return observed.Issues["LEGION-301"].Phase, *observed.Daemon.StateSeq
	}
	if stands, seq := read(tx); stands != phase.Testing || seq != before {
		t.Fatalf("the view built on the snapshot taken before the move reads LEGION-301 at %s and stateSeq %d, want %s and %d: the move is in both or in neither",
			stands, seq, phase.Testing, before)
	}
	r.readOnly(t, func(_ context.Context, tx pgx.Tx) {
		if stands, seq := read(tx); stands != phase.Reviewing || seq != after {
			t.Fatalf("the view built on a snapshot taken after the move reads LEGION-301 at %s and stateSeq %d, want %s and %d",
				stands, seq, phase.Reviewing, after)
		}
	})
}

// sameJSON fails the test unless want and got encode to the same bytes.
func sameJSON(t *testing.T, what string, want, got any) {
	t.Helper()
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode %s of the full state: %v", what, err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode %s of the observed view: %v", what, err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("%s: the observed view reads %s, the full state %s", what, gotJSON, wantJSON)
	}
}
