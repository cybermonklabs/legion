package daemon

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The populated state's moments. Every moment of a seed is a moment after the test's own
// database writes, because a movement is only ever written forwards from the moment its record was
// written (record.Postgres.MarkIssueMoved): so the seed is read a century ahead of the wall clock,
// at populatedReadAt.
var populatedReadAt = time.Date(2126, 9, 22, 9, 18, 0, 0, time.UTC)

// at is a moment of September 2126 in the populated seed's month.
func at(day, hour, minute, second int) time.Time {
	return time.Date(2126, 9, day, hour, minute, second, 0, time.UTC)
}

// The draws of the experiments the populated record holds: LEGION-208's tree was drawn into the
// memory group when its tree was opened, and each issue of that tree was drawn for `plan-length`
// and `planning` when the issue was (internal/experiment's DrawAndRecord).
var (
	populatedMemoryDrawnAt = at(20, 11, 4, 5)
	populatedPlanDrawnAt   = at(21, 18, 42, 11)
)

// The populated state's issue pods (the kubernetes runtime's Sandbox of each issue, named for the
// issue): LEGION-208's roles share one update round.
var populatedPods = map[string]string{
	"LEGION-207": "3b9d4e1a-2c7f-4a85-b6d0-9e1f2a3c4d57",
	"LEGION-208": "7f0c2f9a-6a4b-4f2e-9a1c-2f0d5a3b7e11",
	"LEGION-218": "c4e2a9d1-5b7f-4c38-8e6a-0d2f1b3a5c79",
	"LEGION-219": "9a1c3e5b-7d2f-4b46-a8c0-e2d4f6a8b0c1",
	"LEGION-220": "5d7f9b1c-3e5a-4c72-9b1d-4f6a8c0e2b93",
}

const podUpdateReasons = "the pod template changed: image ghcr.io/cybermonklabs/legion-worker moved"

// populatedPodLocator is one role process of issue's pod at generation, as the kubernetes runtime
// records one at its launch.
func populatedPodLocator(token claim.Token, issue string, generation uint64) *runtime.Locator {
	pod := populatedPods[issue]
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: pod + "/" + strconv.FormatUint(generation, 10),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: "legion-legion-" + strings.ToLower(issue), PodUID: pod, Container: "roles", Generation: generation},
	}
}

// populatedRecord is the populated record's issues. Each comment says what the case is there for
// and which writer of the daemon leaves such a record.
//
//   - LEGION-207 merged and closed; its tree lingers until 10:10:12, and its production check is
//     open at its second look.
//   - LEGION-208, the tree root at work in implementing, carries every optional a claim view has
//     between its architect, planner and implementer, a plan task's counts, a pull request with
//     changes requested, an approved design gate, the memory draw and the handshake of its
//     architect's entry; LEGION-209 is a child of its tree the workflow holds at admitted behind
//     two Dispatch blockers, LEGION-210 and LEGION-213, a key the daemon has no record of, and
//     LEGION-220 a child of it at work in implementing in a slot of its own, whose own architect
//     failed: its notices go to LEGION-208's architect until it runs again (workflow's
//     claimFailed).
//   - LEGION-210, held from implementing on its architect's escalation, with a failed implementer
//     nothing will wake, no architect claim, and a spec whose architect registered a version after
//     its approval.
//   - LEGION-211, a P1 root another tree (LEGION-200) handed over whose own tree has not been
//     opened, waiting in the line's starting part of its band; its implementer is released in
//     LEGION-200's tree with two failed sends. It carries a run, so no Dispatch link holds it
//     (blockers.Exempt).
//   - LEGION-212, a P0 root a person answered, holding its slot and waiting in the line's returned
//     part of its band, its spec sent back with changes, a tester retired in its tree.
//   - LEGION-214, a root whose Legion label is off, which never ran.
//   - LEGION-215, a labelled root under LEGION-217, a child of LEGION-208 its architect has not
//     released and the daemon keeps no record of: its chain reaches LEGION-208's live tree.
//   - LEGION-216, a root that has never run, queued behind LEGION-211 and blocked by LEGION-210.
//   - LEGION-218 and LEGION-219, reviewing roots whose rounds wait on GitHub's merge state: the
//     first for GitHub's first answer, its architect suspended by the operator; the second stuck on
//     `blocked` and told of it, its architect retired by the close of the run before.
func populatedRecord(s *seeding) {
	issue := func(key, tree string, ph phase.Phase, status, repo string, generation uint64, seq int64) record.Issue {
		branch := "main"
		if repo == "zeta/tools" {
			branch = "release/v7.1"
		}
		return record.Issue{
			Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: ph, Status: status, DispatchStatus: status,
			Generation: generation, LastDispatchSeq: seq, Repo: repo, DefaultBranch: branch, GitHubRepo: repo, HandedOver: true,
		}
	}
	mirror := func(key, status string, seq int64, parent string, blockers ...string) record.DispatchReading {
		if blockers == nil {
			blockers = []string{}
		}
		return record.DispatchReading{Key: key, Project: "LEGION", Seq: seq, Status: record.Carry(status), Parent: record.Carry(parent),
			Blockers: record.Carry(blockers), Applied: true}
	}
	ranked := func(reading record.DispatchReading, rank string) record.DispatchReading {
		reading.Rank = record.Carry(rank)
		return reading
	}

	// LEGION-207: merged, its tree lingering; a todo on it before the deadline takes the tree back
	// without a re-admission. The close that lingered it (workflow's leave) set LingerUntil.
	merged := issue("LEGION-207", "LEGION-207", phase.Done, "done", "acme/widgets", 2, 3)
	lingerUntil := at(22, 10, 10, 12)
	merged.LingerUntil = &lingerUntil

	// LEGION-208: the root at work.
	root := issue("LEGION-208", "LEGION-208", phase.Implementing, "in_progress", "acme/widgets", 3, 4)

	// LEGION-209: a child of LEGION-208's tree the workflow holds at admitted.
	child := issue("LEGION-209", "LEGION-208", phase.Admitted, "in_progress", "acme/widgets", 1, 5)
	child.Parent = new("LEGION-208")

	// LEGION-210: held from implementing when its architect escalated the failed implementer
	// (workflow's retryOrEscalate). A hand-over out of LEGION-200's tree made it a root of its
	// own, and its promotion cleared the hand-over's mark (admit's promotion).
	escalated := issue("LEGION-210", "LEGION-210", phase.Held, "in_progress", "zeta/tools", 1, 6)
	escalated.Hold = &record.Hold{From: phase.Implementing, Reason: record.HoldEscalated}

	// LEGION-211: a root LEGION-200's tree handed over with the phase it ran there (admit's
	// hand-over, admit.go's HandedOverFrom). Its own tree has not been opened. Dispatch shows it at
	// P1, which the record keeps from the observation that carried its rank (admit's
	// recordObservation).
	handed := issue("LEGION-211", "LEGION-211", phase.Implementing, "todo", "acme/widgets", 2, 7)
	handed.Rank, handed.HandedOverFrom, handed.Priority = "00011", "LEGION-200", record.P1

	// LEGION-212: its architect asked a person, and the person answered at 09:12, a movement of
	// the root (workflow's awaiting reconcile). It keeps the slot it took when it was started.
	// Dispatch shows it at P0, so it stands alone in the line's first band.
	answeredAt := at(22, 9, 12, 0)
	answered := issue("LEGION-212", "LEGION-212", phase.AwaitingPerson, "in_progress", "acme/widgets", 1, 8)
	answered.Priority = record.P0
	answered.Awaiting = &record.Awaiting{
		Ask: "9a8fd348-55d5-4d72-a793-3cd037f75f13", Since: record.EventMark{Seq: 14, At: at(22, 8, 5, 0)},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk,
	}

	// LEGION-214: a root of its own tree whose record says the Legion label is off.
	labelOff := issue("LEGION-214", "LEGION-214", phase.Admitted, "todo", "zeta/archive", 1, 9)
	labelOff.HandedOver = false

	// LEGION-215: a labelled root under LEGION-217, which Dispatch shows under LEGION-208: admission
	// records the chain with the parent it starts at (admit's record, admit.go; intake/facts.go's
	// Ancestry; record.Issue.Ancestry), since LEGION-217 is a child of a live tree its architect has
	// not released.
	chained := issue("LEGION-215", "LEGION-215", phase.Admitted, "todo", "sjawhar/legion", 1, 12)
	chained.Rank, chained.Parent, chained.Ancestry = "00015", new("LEGION-217"), []string{"LEGION-217", "LEGION-208"}

	// LEGION-216: a root that has never run, queued in the line's starting part behind LEGION-211,
	// which Dispatch says waits on LEGION-210.
	queued := issue("LEGION-216", "LEGION-216", phase.Admitted, "todo", "acme/gadgets", 1, 13)
	queued.Rank = "00016"

	// LEGION-218 and LEGION-219: reviewing roots. Each reviewer's completion is the newest movement
	// of its tree; LEGION-219's first read after the approval took its merge state from unread to
	// `blocked`, which moved it once more.
	waiting := issue("LEGION-218", "LEGION-218", phase.Reviewing, "needs_review", "acme/widgets", 1, 10)
	stuck := issue("LEGION-219", "LEGION-219", phase.Reviewing, "needs_review", "zeta/tools", 3, 11)

	// LEGION-220: a child of LEGION-208's tree its architect released, started in implementing in a
	// slot of its own (record.KeepsSlot).
	childAtWork := issue("LEGION-220", "LEGION-208", phase.Implementing, "in_progress", "acme/widgets", 1, 15)
	childAtWork.Parent = new("LEGION-208")

	s.putRecorded(
		recorded{merged, mirror("LEGION-207", "done", 3, ""), at(22, 8, 40, 12)},
		recorded{root, mirror("LEGION-208", "in_progress", 4, ""), at(22, 8, 58, 14)},
		recorded{child, mirror("LEGION-209", "in_progress", 5, "LEGION-208", "LEGION-210", "LEGION-213"), at(22, 7, 30, 0)},
		recorded{escalated, mirror("LEGION-210", "in_progress", 6, ""), at(22, 6, 18, 0)},
		recorded{handed, ranked(mirror("LEGION-211", "todo", 7, "", "LEGION-210"), "00011"), at(21, 9, 18, 0)},
		recorded{answered, mirror("LEGION-212", "in_progress", 8, ""), answeredAt},
		recorded{labelOff, mirror("LEGION-214", "todo", 9, ""), at(22, 7, 2, 0)},
		recorded{chained, ranked(mirror("LEGION-215", "todo", 12, "LEGION-217"), "00015"), at(22, 7, 45, 0)},
		recorded{queued, ranked(mirror("LEGION-216", "todo", 13, "", "LEGION-210"), "00016"), at(22, 8, 30, 0)},
		recorded{waiting, mirror("LEGION-218", "needs_review", 10, ""), at(22, 9, 7, 21)},
		recorded{stuck, mirror("LEGION-219", "needs_review", 11, ""), at(20, 16, 42, 58)},
		recorded{childAtWork, mirror("LEGION-220", "in_progress", 15, "LEGION-208"), at(22, 9, 4, 40)},
	)
	// LEGION-217: what Dispatch says of a child of LEGION-208 its architect has not released, which
	// the daemon keeps no record of (blockers.Mirror writes every issue fact it applies).
	s.mirrorOnly(mirror("LEGION-217", "backlog", 14, "LEGION-208"))
	// LEGION-211 is the root a hand-over created at its generation (admit's hand-over,
	// record.Store.PutHandedOverRoot), carrying its old root's gate.
	s.in("the hand-over of LEGION-211", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutHandedOverRoot(ctx, tx, record.HandedOverRoot{Issue: "LEGION-211", Generation: 2, GateCarried: true})
	})

	// The issues the daemon has started hold the cap's slots (admit's promotion, record.Store.PutSlot):
	// LEGION-211's tree has not been opened, LEGION-209 waits at admitted, LEGION-207 closed and
	// LEGION-214 never ran.
	for _, slot := range []record.Slot{
		{Issue: "LEGION-208", Index: 0, AdmittedAt: at(21, 18, 42, 11)},
		{Issue: "LEGION-210", Index: 1, AdmittedAt: at(21, 19, 5, 26)},
		{Issue: "LEGION-212", Index: 2, AdmittedAt: at(22, 8, 1, 30)},
		{Issue: "LEGION-218", Index: 3, AdmittedAt: at(22, 8, 14, 52)},
		{Issue: "LEGION-219", Index: 4, AdmittedAt: at(19, 10, 27, 44)},
		{Issue: "LEGION-220", Index: 5, AdmittedAt: at(22, 7, 52, 18)},
	} {
		s.in("the slot of "+slot.Issue, func(ctx context.Context, tx pgx.Tx) error { return s.records.PutSlot(ctx, tx, slot) })
	}
	// The promotion of each root it started opened that root's tree (record.Store.OpenTreeLifecycle).
	// LEGION-207's stays open while its tree lingers: the outbox reserves its cleanup when the linger
	// ends (outbox's reservedTreeClose).
	s.openTrees("LEGION", "LEGION-207", "LEGION-208", "LEGION-210", "LEGION-212", "LEGION-218", "LEGION-219")
	// LEGION-200's tree, which an operator opened for a root the daemon keeps no record of
	// (store.Store.OpenOperatorTreeLifecycle, the operator's spawn), handed LEGION-210 and LEGION-211
	// over; its lifecycle stands open while a claim of it does.
	_, err := s.st.OpenOperatorTreeLifecycle(s.ctx, "legion", "LEGION-200", func(context.Context, pgx.Tx) error { return nil })
	s.must("LEGION-200's operator tree", err)
	// LEGION-210's hand-over settled it at no arm of `plan-length`, the issue experiment the
	// project declared then and LEGION-200's tree held no row of (record.Store.CarryAssignments).
	s.in("LEGION-210's hand-over answers", func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.records.CarryAssignments(ctx, tx, "legion", "LEGION-210", "LEGION-200",
			[]record.DeclaredExperiment{{Experiment: "plan-length", Unit: string(config.ExperimentIssue)}})
		return err
	})
	// LEGION-208's tree was drawn into the memory group when it was opened, a tree experiment every
	// issue of the tree shows; `plan-length` and `planning` are drawn for each issue on its own, so
	// a root and its child stand in different arms (internal/experiment's DrawAndRecord,
	// record.Store.PutAssignments).
	for key, rows := range map[string][]record.Assignment{
		"LEGION-208": {
			{Experiment: config.MemoryExperimentName, Unit: string(config.ExperimentTree), Variant: config.MemoryVariantMemory, Mode: string(config.ExperimentRandom), DrawnAt: &populatedMemoryDrawnAt},
			{Experiment: "plan-length", Unit: string(config.ExperimentIssue), Variant: "short", Mode: string(config.ExperimentRandom), DrawnAt: &populatedPlanDrawnAt},
		},
		"LEGION-209": {
			{Experiment: "plan-length", Unit: string(config.ExperimentIssue), Variant: "long", Mode: string(config.ExperimentRandom), DrawnAt: &populatedPlanDrawnAt},
			{Experiment: "planning", Unit: string(config.ExperimentIssue), Variant: "b", Mode: string(config.ExperimentRandom), DrawnAt: &populatedPlanDrawnAt, Plan: string(config.PlanGroupDefault)},
		},
		"LEGION-220": {
			{Experiment: "plan-length", Unit: string(config.ExperimentIssue), Variant: "short", Mode: string(config.ExperimentRandom), DrawnAt: &populatedPlanDrawnAt},
			{Experiment: "planning", Unit: string(config.ExperimentIssue), Variant: "a", Mode: string(config.ExperimentRandom), DrawnAt: &populatedPlanDrawnAt},
		},
	} {
		s.in("the draws of "+key, func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.records.PutAssignments(ctx, tx, key, rows)
			return err
		})
	}

	// The design gates (workflow/gate.go's registration and the approvals Dispatch carries,
	// record.Store.PutGate). The deployment gates `root-issues`, the one step spec. LEGION-207's,
	// LEGION-208's, LEGION-218's and LEGION-219's specs stand approved at the version Dispatch
	// holds (LEGION-208's must: a shut gate of its tree would hold its child at design_gate, the
	// word the held ladder reads before dispatch_blocker); LEGION-211's came with its hand-over;
	// LEGION-210's architect registered a version after the one its person approved, so its gate
	// waits; LEGION-212's reviewer asked for changes at the version Dispatch holds and the architect
	// was told. LEGION-214, LEGION-215 and LEGION-216 never ran, so no architect registered theirs.
	for _, gate := range []record.DesignGate{
		{Issue: "LEGION-207", ArtifactID: "0c7e5a31-9d84-4b26-a0f3-5e81c2d47b90", LatestVersion: 4, ApprovedVersion: new(4), EverOpened: true},
		{Issue: "LEGION-208", ArtifactID: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093", LatestVersion: 7, ApprovedVersion: new(7), EverOpened: true},
		{Issue: "LEGION-210", ArtifactID: "4e8b2a61-7d93-4c05-b1f6-8a3e5c9d2074", LatestVersion: 2, ApprovedVersion: new(1), EverOpened: true},
		{Issue: "LEGION-211", ArtifactID: "e5a2c1d7-3b46-4f82-9e07-2d4a6c8b1f53", LatestVersion: 3, ApprovedVersion: new(3), EverOpened: true},
		{Issue: "LEGION-212", ArtifactID: "b71f0a94-6c3e-4d28-85af-19c7d0e3b462", LatestVersion: 2,
			ToldChangesVersion: new(2), ToldChangesReason: "name the rollback path for the schema change"},
		{Issue: "LEGION-218", ArtifactID: "f3b8d205-7c41-4e69-9a02-5d7e1c6b4f38", LatestVersion: 5, ApprovedVersion: new(5), EverOpened: true},
		{Issue: "LEGION-219", ArtifactID: "a9142c7e-0b63-4d85-8f17-3e6a0d9b2c54", LatestVersion: 2, ApprovedVersion: new(2), EverOpened: true},
	} {
		gate.Step = record.DesignStepSpec
		s.in("the gate of "+gate.Issue, func(ctx context.Context, tx pgx.Tx) error { return s.records.PutGate(ctx, tx, gate) })
	}

	// The pull requests as GitHub's last reads left them (workflow's pull request intake,
	// record.Store.PutPullRequest), each on its issue's branch (claim.IssueBranch) into its
	// repository's default branch: LEGION-208's merge state a review the repository requires holds;
	// LEGION-218's unread since the push; LEGION-219's head stood two days, every read since the first
	// after the approval answered `blocked`, the last a minute before this state, and two of the
	// fence's runs are stamped with the head.
	for _, pr := range []record.PullRequest{
		{Issue: "LEGION-207", Repo: "acme/widgets", Number: 1239, HeadSHA: "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40", State: record.PullRequestMerged},
		{Issue: "LEGION-208", Repo: "acme/widgets", Number: 1247, HeadSHA: "5b3d9e2c8f1a47d06b2e5c9f3a8d1e4b7c0f6a29",
			State: record.PullRequestOpen, MergeState: record.MergeStateBlocked},
		{Issue: "LEGION-218", Repo: "acme/widgets", Number: 1251, HeadSHA: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063",
			State: record.PullRequestOpen, HeadUpdatedAt: at(22, 8, 58, 40)},
		{Issue: "LEGION-219", Repo: "zeta/tools", Number: 642, HeadSHA: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
			State: record.PullRequestOpen, MergeState: record.MergeStateBlocked, HeadUpdatedAt: at(20, 14, 5, 33), StateReadAt: at(22, 9, 16, 12),
			CheckRuns: []record.AttemptRun{
				{Name: "test", ID: 38230245501, Head: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284"},
				{Name: "lint", ID: 38230245502, Head: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284"},
			}},
	} {
		pr.Branch, pr.Base = claim.IssueBranch(pr.Issue), "main"
		if pr.Repo == "zeta/tools" {
			pr.Base = "release/v7.1"
		}
		s.in("the pull request of "+pr.Issue, func(ctx context.Context, tx pgx.Tx) error { return s.records.PutPullRequest(ctx, tx, pr) })
	}
	// LEGION-207's check opened before the release that records the project's `production_check`
	// role, so it keeps the implementer; look 2 is open, woken by the Dispatch issue.deployed that
	// release still read (workflow's production check, record.Store.PutProductionCheck).
	s.in("LEGION-207's production check", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutProductionCheck(ctx, tx, record.ProductionCheck{
			Issue: "LEGION-207", Project: "LEGION", Tree: "LEGION-207", Repo: "acme/widgets", Number: 1239,
			MergeSHA: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60", OpenedAt: at(22, 8, 40, 12), BoundAt: time.Date(2126, 10, 6, 8, 40, 12, 0, time.UTC),
			Role: claim.RoleImplementer, Looks: 2, LookOpen: true,
			LastWakeKind: "issue.deployed", LastWakeDetail: "run 38230245571 completed 2126-09-22T09:10:44Z", LastWakeAt: new(at(22, 9, 10, 51)),
		})
	})
	// LEGION-208's run walks the project's default steps (workflow's defaultRows,
	// record.Store.PutPlan), and its implementer counts its returns on the plan's one task
	// (workflow's recordPass, record.Store.PutReturnCounts): three passes, two of them ended by a
	// review that requested changes, each pair of the moves back that made them.
	s.in("LEGION-208's plan", func(ctx context.Context, tx pgx.Tx) error {
		version := record.PlanVersion{Issue: "LEGION-208", Generation: 3, Source: record.PlanSourceDefault, GivenAt: at(21, 18, 42, 11)}
		task := record.PlanTask{Issue: "LEGION-208", Generation: 3, Task: "default", BlockText: "The project's default steps.",
			StepsByDefault: true, State: record.TaskWaiting}
		if err := s.records.PutPlan(ctx, tx, version, []record.PlanTask{task}, nil); err != nil {
			return err
		}
		return s.records.PutReturnCounts(ctx, tx, "LEGION-208", 3, "default", record.ReturnCounts{
			Pass: 3, Rounds: 2, BackwardMoves: map[string]int{"reviewing-implementing": 1, "testing-implementing": 1},
		})
	})
}

// populatedClaims writes the populated record's phases rows and claims, and the capability report
// of the one live session that has reported.
func populatedClaims(s *seeding, rows populatedRows) {
	token := func(issue string, role claim.Role) claim.Token { return s.token("LEGION", issue, role) }
	architect208 := token("LEGION-208", claim.RoleArchitect)
	planner208 := token("LEGION-208", claim.RolePlanner)
	implementer208 := token("LEGION-208", claim.RoleImplementer)

	// The phases rows each role's work writes (workflow's start, enter and completion,
	// record.Store.PutPhase).
	for _, row := range []record.PhaseRow{
		{Issue: "LEGION-207", Role: claim.RoleImplementer, Claim: token("LEGION-207", claim.RoleImplementer),
			HandoffCommit: "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40", Pass: 1, Rounds: 1},
		// Its reviewer approved the head that merged; the claim is gone, so it is no worker.
		{Issue: "LEGION-207", Role: claim.RoleReviewer, Pass: 1,
			Decision: &record.ReviewDecision{State: "approved", Head: "7d1e4f2a9c3b5e8d0a6f2c4b8e1d3a5f7c9b2e40"}},
		// The architect's entry 2, raised by the start that moved the root on at 08:58:14.
		{Issue: "LEGION-208", Role: claim.RoleArchitect, Claim: architect208, Entry: 2, EntryRaisedAt: at(22, 8, 58, 14)},
		// The planner's completion named its Dispatch handoff record.
		{Issue: "LEGION-208", Role: claim.RolePlanner, Claim: planner208,
			HandoffRecord: "6f1d2c3b-4a5e-4f60-8a7b-9c0d1e2f3a4b", HandoffVersion: 1},
		// The implementer holds the work its row names: the entry its claim was given.
		{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer208, Entry: 4, EntryRaisedAt: at(22, 8, 58, 14)},
		// The reviewer's round asked for changes; its claim is gone, so it is no worker.
		{Issue: "LEGION-208", Role: claim.RoleReviewer, Decision: &record.ReviewDecision{State: "changes_requested"}},
		// LEGION-210's architect was started on entry 1 at its promotion; no architect claim of it
		// stands now, and nothing queued a start of one, so the stall reading's second arm holds
		// (admit/architect_stall.go's architectClaimUnreachable).
		{Issue: "LEGION-210", Role: claim.RoleArchitect, Entry: 1, EntryRaisedAt: at(21, 19, 5, 26)},
		{Issue: "LEGION-210", Role: claim.RoleImplementer, Claim: token("LEGION-210", claim.RoleImplementer), Pass: 1, Rounds: 1},
		// The transition that sent LEGION-211's work back raised its implementer's row to entry 2;
		// its claim was given entry 1.
		{Issue: "LEGION-211", Role: claim.RoleImplementer, Claim: token("LEGION-211", claim.RoleImplementer), Rounds: 1, Entry: 2},
		// LEGION-220's own architect was started on its entry when the child's first phase began,
		// and its implementer holds the work of implementing's entry.
		{Issue: "LEGION-220", Role: claim.RoleArchitect, Claim: token("LEGION-220", claim.RoleArchitect), Entry: 1, EntryRaisedAt: at(22, 7, 52, 18)},
		{Issue: "LEGION-220", Role: claim.RoleImplementer, Claim: token("LEGION-220", claim.RoleImplementer), Entry: 1, EntryRaisedAt: at(22, 8, 20, 5)},
		{Issue: "LEGION-218", Role: claim.RoleImplementer, Claim: token("LEGION-218", claim.RoleImplementer),
			HandoffCommit: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063", Pass: 1},
		{Issue: "LEGION-218", Role: claim.RoleReviewer, Claim: token("LEGION-218", claim.RoleReviewer), Pass: 1, Entry: 1,
			Decision: &record.ReviewDecision{State: "approved", Head: "6c2f8a0d4b7e13952a8c0f6d3b5e9172c4a8d063"}},
		{Issue: "LEGION-219", Role: claim.RoleImplementer, Claim: token("LEGION-219", claim.RoleImplementer),
			HandoffCommit: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284", Pass: 2, Rounds: 1,
			BackwardMoves: map[string]int{"reviewing-implementing": 1}},
		// The architect and the reviewer were told of the stuck round (workflow's
		// tellStuckMergeState), which the decision's two marks record.
		{Issue: "LEGION-219", Role: claim.RoleReviewer, Claim: token("LEGION-219", claim.RoleReviewer), Pass: 2, Rounds: 1, Entry: 2,
			Decision: &record.ReviewDecision{State: "approved", Head: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284",
				ToldStuckHead: "0f5a7c2e9b48d1630a7f2c8e4b6d0915a3c7e284", ToldStuckState: record.MergeStateBlocked}},
	} {
		s.in("the phases row of "+row.Issue+" "+string(row.Role), func(ctx context.Context, tx pgx.Tx) error { return s.records.PutPhase(ctx, tx, row) })
	}

	// The pod's update round, which every claim of the pod shares (supervise's pod update round):
	// the planner answered it with the head it pushed and waits for its launch onto the
	// replacement; the implementer was asked the same round and has not answered.
	tasked := at(22, 9, 2, 0)
	round := "pod:" + populatedPods["LEGION-208"]
	implementerLocator := populatedPodLocator(implementer208, "LEGION-208", 5)
	for _, c := range []supervise.Claim{
		// The pacing holds this claim's relaunch onto this daemon's plugin: its process registered
		// at the fleet's floor.
		{Token: token("LEGION-207", claim.RoleImplementer), Project: "legion", Tree: "LEGION-207", Issue: "LEGION-207", Role: claim.RoleImplementer,
			Generation: 2, State: supervise.StateWorking, Session: "ses_implementer_207",
			Locator: populatedPodLocator(token("LEGION-207", claim.RoleImplementer), "LEGION-207", 2), PluginContract: api.ServingFloor},
		// The architect is ready in the pod: the turn it began and ended, its ready, the cause its
		// process was last found gone under and a start failure of a launch since that ready, each
		// with the time the daemon recorded it (the claim store's causeTime): the death before the
		// ready, so the running agent survived it. Its handshake is that of the entry it was last
		// queued, which a turn confirmed.
		{Token: architect208, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect, Generation: 3,
			State: supervise.StateReady, Session: "ses_architect_208", Locator: populatedPodLocator(architect208, "LEGION-208", 4), PluginContract: api.ServingFloor,
			GivenEntry: 2, Handshake: supervise.EntryHandshake{Entry: 2, Generation: 3, QueuedAt: at(22, 8, 58, 14), ConfirmedAt: at(22, 8, 58, 51)},
			Step: supervise.StepRecord{
				At: at(22, 9, 16, 58), TurnStartedAt: at(22, 9, 16, 40), Tool: "read", CallID: "toolu_arch_3",
				ArgsBytes: 74, ArgsHash: "3f7c1a9d62b40e58", StartedAt: at(22, 9, 16, 55), EndedAt: at(22, 9, 16, 58),
			},
			LastTurnStart: at(22, 9, 16, 40), LastTurnEnd: at(22, 9, 16, 58), LastReady: at(22, 9, 15, 44),
			LastDeathCause: "the pod's process was gone at the daemon's read", LastDeathAt: at(22, 9, 15, 21),
			LastStartFailure: "the pod was not ready within the worker boot timeout", LastStartFailureAt: at(22, 9, 15, 50),
			UpdateRound: round, UpdateReasons: podUpdateReasons, UpdateTaskedAt: &tasked},
		{Token: planner208, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RolePlanner, Generation: 3,
			State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByDaemon, Session: "ses_planner_208",
			Locator: populatedPodLocator(planner208, "LEGION-208", 2), PluginContract: api.ServingFloor,
			UpdateRound: round, UpdateReasons: podUpdateReasons, UpdateTaskedAt: &tasked, UpdateSignalledAt: new(at(22, 9, 6, 30)),
			UpdateHead: "4c1b8e2a6f9d3047b5e1c8a2f6d940b3e7c5a128"},
		// Two of its turns ended in a provider error in a row; it holds the task of its entry and is
		// in a call with a start and no end, the shape a stalled turn leaves behind.
		{Token: implementer208, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 3,
			State: supervise.StateWorking, Session: "ses_implementer_208", Locator: implementerLocator, PluginContract: api.ServingFloor,
			ServingGeneration: 3, GivenEntry: 4, UpdateRound: round, UpdateReasons: podUpdateReasons, UpdateTaskedAt: &tasked,
			ProviderErrorRun: 2, Pending: &supervise.Delivery{ID: "outbox:" + strconv.FormatInt(rows.running208, 10), Task: "Implement the plan's default task.", Phase: phase.Implementing,
				QueuedAt: at(22, 9, 16, 58), Generation: 3, Entry: 4},
			Step: supervise.StepRecord{
				At: at(22, 9, 17, 12), TurnStartedAt: at(22, 9, 16, 30), Tool: "bash", CallID: "toolu_impl_11",
				ArgsBytes: 128, ArgsHash: "9b2e45c0d1f8a673", StartedAt: at(22, 9, 17, 12),
			}},
		// Its implementer failed and nothing has launched it since: the cause, the sentence, the
		// moment and the counts it failed with, the death whose refused relaunches spent the budget,
		// and the sentence a launch try was refused with for the shared leak check. It is a worker
		// of the phase its hold took the issue from, with no start of that phase queued, so nothing
		// will wake it (workflow.Stranded).
		{Token: token("LEGION-210", claim.RoleImplementer), Project: "legion", Tree: "LEGION-210", Issue: "LEGION-210", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateFailed, Session: "ses_implementer_210",
			FailureCause: supervise.CauseDied, Failure: "launch failures ran out", FailedAt: at(22, 8, 47, 3),
			Budgets: supervise.Budgets{LaunchFailures: 4, Deaths: 1}, PendingCause: supervise.CauseDied,
			LeakCheckCause: "the shared public leak check could not be installed: GitHub answered the read of example-org/deployment at leak-check-v3 with HTTP 404: Not Found"},
		// The daemon released its idle pod in LEGION-200's tree, which handed it over, and it keeps
		// the task it held through the release, with the two sends that never reached a turn. Its
		// issue's own tree has not been opened, so the rule names it nowhere.
		{Token: token("LEGION-211", claim.RoleImplementer), Project: "legion", Tree: "LEGION-200", Issue: "LEGION-211", Role: claim.RoleImplementer,
			Generation: 2, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased, Session: "ses_implementer_211", GivenEntry: 1,
			Pending: &supervise.Delivery{ID: "outbox:" + strconv.FormatInt(rows.started211, 10), Task: "Implement the plan's default task.", Phase: phase.Implementing, QueuedAt: at(22, 8, 54, 20),
				Generation: 2, Entry: 1, SendFailures: 2, LastSendError: "post the prompt: dial tcp 10.42.0.17:13371: connect: connection refused"}},
		// LEGION-220's own architect failed and nothing has launched it since; the daemon starts it
		// again at the child's next phase start, so the rule names it nowhere (workflow.Stranded's
		// default case). Its implementer works on in the child's own pod.
		{Token: token("LEGION-220", claim.RoleArchitect), Project: "legion", Tree: "LEGION-208", Issue: "LEGION-220", Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateFailed, Session: "ses_architect_220",
			FailureCause: supervise.CauseDied, Failure: "launch failures ran out", FailedAt: at(22, 8, 41, 17),
			Budgets: supervise.Budgets{LaunchFailures: 4, Deaths: 1}},
		{Token: token("LEGION-220", claim.RoleImplementer), Project: "legion", Tree: "LEGION-208", Issue: "LEGION-220", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateWorking, Session: "ses_implementer_220", GivenEntry: 1,
			Locator: populatedPodLocator(token("LEGION-220", claim.RoleImplementer), "LEGION-220", 1), PluginContract: api.ServingFloor},
		// A tester of its tree retired while the issue stood in its phase; its root waits on a
		// person, whose answer resumes the tree, so the rule names it nowhere.
		{Token: token("LEGION-212", claim.RoleTester), Project: "legion", Tree: "LEGION-212", Issue: "LEGION-212", Role: claim.RoleTester,
			Generation: 1, State: supervise.StateRetired, Session: "ses_tester_212"},
		// The operator suspended LEGION-218's tree architect, whose tree runs on: only its tree's
		// admission starts it, and each start waits for the operator's resume (workflow.Stranded).
		{Token: token("LEGION-218", claim.RoleArchitect), Project: "legion", Tree: "LEGION-218", Issue: "LEGION-218", Role: claim.RoleArchitect,
			Generation: 1, State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByOperator, Session: "ses_architect_218"},
		// The live roles of the two reviewing roots, each idle in its own issue pod.
		{Token: token("LEGION-218", claim.RoleImplementer), Project: "legion", Tree: "LEGION-218", Issue: "LEGION-218", Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateIdle, Session: "ses_implementer_218",
			Locator: populatedPodLocator(token("LEGION-218", claim.RoleImplementer), "LEGION-218", 1), PluginContract: api.ServingFloor},
		{Token: token("LEGION-218", claim.RoleReviewer), Project: "legion", Tree: "LEGION-218", Issue: "LEGION-218", Role: claim.RoleReviewer,
			Generation: 1, State: supervise.StateIdle, Session: "ses_reviewer_218", GivenEntry: 1,
			Locator: populatedPodLocator(token("LEGION-218", claim.RoleReviewer), "LEGION-218", 1), PluginContract: api.ServingFloor},
		// The close of LEGION-219's run before this one retired its tree architect, and the tree
		// runs again at generation 3 with no admission start of it queued (workflow.Stranded).
		{Token: token("LEGION-219", claim.RoleArchitect), Project: "legion", Tree: "LEGION-219", Issue: "LEGION-219", Role: claim.RoleArchitect,
			Generation: 2, State: supervise.StateRetired, Session: "ses_architect_219"},
		{Token: token("LEGION-219", claim.RoleImplementer), Project: "legion", Tree: "LEGION-219", Issue: "LEGION-219", Role: claim.RoleImplementer,
			Generation: 3, State: supervise.StateIdle, Session: "ses_implementer_219",
			Locator: populatedPodLocator(token("LEGION-219", claim.RoleImplementer), "LEGION-219", 3), PluginContract: api.ServingFloor},
		{Token: token("LEGION-219", claim.RoleReviewer), Project: "legion", Tree: "LEGION-219", Issue: "LEGION-219", Role: claim.RoleReviewer,
			Generation: 3, State: supervise.StateIdle, Session: "ses_reviewer_219", GivenEntry: 2,
			Locator: populatedPodLocator(token("LEGION-219", claim.RoleReviewer), "LEGION-219", 3), PluginContract: api.ServingFloor},
	} {
		s.claimAs(c)
	}
	// The release of LEGION-211's pod in LEGION-200's tree (store.Store.RecordPodRelease), which
	// suspended its implementer as `released`. A release takes only a pod whose workspace held
	// nothing unpushed and saves no bundle, so it records no bundle bytes (store.PodRelease).
	_, _, err := s.st.RecordPodRelease(s.ctx, store.PodRelease{Project: "legion", Issue: "LEGION-211", Generation: 2,
		Roles: []string{string(claim.RoleImplementer)}, At: at(22, 8, 55, 0)})
	s.must("the release of LEGION-211's pod", err)

	// The session's capability report, from the process the locator names: two rows it proved and
	// one it did not, with the fact its check found. The store keeps it (PutCapabilityReport), and
	// the daemon's boot reads it back into the supervisor's memory (openSupervision,
	// reportsOfClaims), which is what the state reads.
	report := capabilities.Report{
		Claim: implementer208, Generation: 3, Locator: *implementerLocator,
		MeasuredAt: at(22, 9, 17, 3), ReportedAt: at(22, 9, 17, 4), ElapsedMs: 900,
		Rows: []capabilities.Row{
			{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered"},
			{Name: capabilities.DispatchEnvoyTools, OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
			{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}
	s.must("the capability report of "+string(implementer208), s.st.PutCapabilityReport(s.ctx, report))
}

// populatedRows is the outbox rows whose tasks the populated claims hold (the delivery's id names
// the row, the outbox's `outbox:<id>`).
type populatedRows struct {
	started211, running208 int64
	// runningLease is the lease the lane holds on running208.
	runningLease string
}

// populatedOutbox queues the populated record's outbox rows, each through the writer that makes
// it, with a lane's lease where one runs (record.Store.ClaimRow) and its finish where it ran
// (record.Store.FinishOutbox): the start that gave LEGION-211's implementer entry 1's task in
// LEGION-200's tree ran and finished; LEGION-208's lifecycle lane runs the start that gives its
// implementer entry 4's task, and the sweep's start of its architect behind its entry came due two
// and a half minutes ago and waits behind it (record.Store.HeldRows); LEGION-208's architect's
// catch-up waits on its row for the moment the wake pacing reserved (the outbox's start writes it
// in place of a launch, workflow.Config.Reserve).
func populatedOutbox(s *seeding) populatedRows {
	const task = "Implement the plan's default task."
	started := s.enqueue("LEGION-211", record.SuperviseRequest{Op: "start", Tree: "LEGION-200", Role: claim.RoleImplementer,
		Generation: 2, Phase: phase.Implementing, Entry: 1, Task: task}, at(22, 8, 54, 0))
	ran := s.lease(started, at(22, 8, 54, 10), time.Minute)
	s.in("the finish of LEGION-211's start", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.FinishOutbox(ctx, tx, ran.ID, ran.LeaseToken)
	})
	running := s.enqueue("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
		Generation: 3, Phase: phase.Implementing, Entry: 4, Task: task}, at(22, 9, 14, 0))
	lease := s.lease(running, at(22, 9, 14, 30), 10*time.Minute)
	s.enqueue("LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, Entry: 2}, at(22, 9, 15, 30))
	s.enqueue("LEGION-208", record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, PacedAt: at(22, 9, 18, 3), PacedFrom: at(22, 9, 16, 58),
		Reason:  "the tree of LEGION-208 at generation 3 as the daemon records it now; start or resume it as your role says",
		CatchUp: &record.CatchUp{Generation: 3, Launch: 4, Gates: []record.CatchUpGate{}, Issues: []record.CatchUpIssue{}}}, at(22, 9, 18, 3))
	return populatedRows{started211: started, running208: running, runningLease: lease.LeaseToken}
}

// populatedDeployment is the populated state's configuration and the daemon's memory beside the
// record: the boot rows and the controller's registration are written to the store here, since
// both are durable.
func populatedDeployment(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "settingsFixtureFile"))
	// Four boots of this project, the first four days before this read (store.Store.RecordBoot).
	for _, booted := range []time.Time{at(18, 14, 3, 27), at(19, 6, 12, 0), at(21, 2, 44, 9), at(22, 9, 15, 2)} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	// The daemon minted the controller's capability and its session registered with it
	// (store.Store.MintController, RegisterController).
	generation, err := s.st.MintController(s.ctx, "legion", []byte("controller-capability-hash"))
	s.must("the controller's capability", err)
	registered, err := s.st.RegisterController(s.ctx, "legion", generation, "ses_controller", []byte("controller-secret-hash"), at(22, 9, 16, 40), api.ServingFloor)
	s.must("the controller's registration", err)
	if !registered {
		s.t.Fatal("seed the controller's registration: the store did not register it")
	}
	// What GitHub answered of each name the live records give, as the refresher keeps each read
	// (repositories.keepRead) and Dispatch's answer of its webhook App (keepWebhookAnswer):
	// acme/gadgets, the review App not installed on it; acme/widgets, both role Apps installed;
	// sjawhar/legion, which the records keep under its name before a configured transfer, both
	// Apps answered on its new name cybermonklabs/legion; zeta/archive, whose implement App read
	// GitHub did not answer; and zeta/tools, both Apps installed and Dispatch's GitHub App not.
	repos := newRepositories(cfg.Project, cfg.RepositoryRenames)
	installed, missing := appInstallation{answered: true, installed: true}, appInstallation{answered: true}
	for _, read := range []struct {
		name      string
		apps      appInstallations
		readError string
	}{
		{name: "acme/gadgets", apps: appInstallations{implement: installed, review: missing}},
		{name: "acme/widgets", apps: appInstallations{implement: installed, review: installed}},
		{name: "cybermonklabs/legion", apps: appInstallations{implement: installed, review: installed}},
		{name: "sjawhar/legion", apps: appInstallations{implement: missing, review: missing}},
		{name: "zeta/archive", apps: appInstallations{review: installed},
			readError: "the implement App's installation on zeta/archive could not be read: GitHub answered 502; Legion reads it again"},
		{name: "zeta/tools", apps: appInstallations{implement: installed, review: installed}},
	} {
		repos.keepRead(ghrepo.MustParse(read.name), read.apps, read.readError)
	}
	repos.keepWebhookAnswer(ghrepo.MustParse("zeta/tools"),
		"Dispatch's GitHub App is not installed on zeta/tools: GitHub sends Legion none of its events, nothing redelivers them, and no periodic pass makes up for them, so its issues stand still until the App is installed")
	return composition{
		cfg: cfg, now: populatedReadAt, startedAt: at(22, 9, 15, 2),
		// A wave going out at its operator's pace, with one wake of each lane the machines hold:
		// LEGION-207's implementer waits for its relaunch onto this daemon's plugin, and LEGION-208's
		// planner for its launch onto the pod's replacement.
		wakes: []supervise.WaitingWake{
			{Claim: s.token("LEGION", "LEGION-207", claim.RoleImplementer), Issue: "LEGION-207", Role: claim.RoleImplementer, Kind: supervise.WakePluginRefresh, At: at(22, 9, 17, 56)},
			{Claim: s.token("LEGION", "LEGION-208", claim.RolePlanner), Issue: "LEGION-208", Role: claim.RolePlanner, Kind: supervise.WakeLaunch, At: at(22, 9, 18, 11)},
		},
		// The broker login holds a credential. The image passed its probe after two attempts the
		// pool had no room for, a gap the deployment's file decides
		// (capabilities.decided.pool-capacity).
		secretsIssued: true, probed: true,
		image: bootprobe.ImageReport{ModelFallback: "on", CapacityWaits: 2, CapacityReason: "0/3 nodes are available: 3 Insufficient cpu."},
		// acme/widgets' cache is Ready and read, and last answered its health with a repack that
		// failed for lack of space; zeta/tools' is failing; acme/legacy's, which no live record
		// names any more, keeps its cache until its retention ends.
		caches: []sandbox.CacheStatus{
			{Repo: ghrepo.MustParse("acme/widgets"), Sandbox: "legion-legion-cache-acme-widgets-d782c874", State: "ready", Ready: true,
				Address: "http://legion-legion-cache-acme-widgets-d782c874.legion.svc:8418", Claim: "cache-legion-legion-cache-acme-widgets-d782c874",
				Health: &repocache.Health{Ready: true, Repo: "acme/widgets", Head: "34a7ac80e9c3389ffbc1bbeece371ccacc043992",
					LastFetchAt: at(22, 9, 2, 30), Bytes: 6612345678, VolumeBytes: new(uint64(20 << 30)), AvailableBytes: new(uint64(107374182)),
					RepackOutOfSpaceAt: time.Date(2126, 9, 16, 2, 15, 33, 0, time.UTC)}},
			{Repo: ghrepo.MustParse("zeta/tools"), Sandbox: "legion-legion-cache-zeta-tools-1b2c3d4e", State: "failing",
				Reason: "container cache CrashLoopBackOff: back-off 5m0s restarting failed container (restarted 4 times)",
				Claim:  "cache-legion-legion-cache-zeta-tools-1b2c3d4e"},
			{Repo: ghrepo.MustParse("acme/legacy"), Sandbox: "legion-legion-cache-acme-legacy-0a1b2c3d", State: "starting",
				RetainedUntil: time.Date(2126, 9, 29, 9, 0, 0, 0, time.UTC), Claim: "cache-legion-legion-cache-acme-legacy-0a1b2c3d"},
		},
		repositories: repos,
		// The three durable consumers the daemon opens, in the order Lag answers them
		// (intake.Consumers.Lag): the Dispatch consumer is caught up; the GitHub consumer has 412
		// messages waiting and has handled none for over a minute; the listener-wake consumer's
		// info read ran out of the bound.
		consumers: []intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: at(22, 9, 17, 50)},
			{Name: "legion-go-LEGION-github", Waiting: 412, OldestAt: at(22, 9, 5, 34), OldestAge: 754 * time.Second,
				LastHandledAt: at(22, 9, 16, 41), Stalled: true},
			{Name: "legion-go-LEGION-wake", Unavailable: "read the consumer's info: context deadline exceeded"},
		},
	}
}

// seedPopulatedRecord is the populated record the populated and paused states share, written in
// the order its writers need: the issues, the outbox rows the claims' tasks name, the claims, the
// deployment's boots and controller, the intake's give-ups and the pod's hold. It answers the
// daemon's composition of it and the outbox rows the claims hold.
func seedPopulatedRecord(s *seeding) (composition, populatedRows) {
	populatedRecord(s)
	rows := populatedOutbox(s)
	populatedClaims(s, rows)
	c := populatedDeployment(s)
	populatedIntake(s)
	populatedPodHolds(s)
	return c, rows
}

// seedPopulated is the populated state: the project running, unpaused.
func seedPopulated(s *seeding) composition {
	c, _ := seedPopulatedRecord(s)
	return c
}

// populatedPodHolds writes what the idle rule recorded of the pod it kept past the idle period
// (internal/daemon's recordIdleHold, store.Store.RecordPodHold), keyed on the claim token's
// spelling of the project as the idle pass writes it: LEGION-219's pod is quiet, and its
// reviewer's last report named an async job still running, so the first pass of the rule that held
// the pod for background work came the evening before and its newest a minute before this state.
// The two passes are one streak, the pod's standing hold, with the kind the report named.
func populatedPodHolds(s *seeding) {
	for _, pass := range []time.Time{at(21, 22, 4, 0), at(22, 9, 17, 0)} {
		s.must("a pass of the idle rule over LEGION-219's pod", s.st.RecordPodHold(s.ctx, store.PodHold{
			Project: "legion", Issue: "LEGION-219", Reason: workflow.IdleHoldBackgroundWork.String(), Detail: "async-job", At: pass,
		}))
	}
}

// populatedIntake writes the intake's three open give-ups as its consumers write them
// (internal/intake's giveup.go), each in the order of its delivery and in its own transaction:
//
//   - the Dispatch consumer gave up LEGION-213's issue event at its bound: its first delivery
//     failed at 06:30:04, its twenty-first at 07:31:04, over an hour later, and that delivery's
//     transaction noted the failure and gave the message up (record.Postgres.NoteFailure, then
//     GiveUpIntakeMessage, recordFailure's transaction);
//   - the GitHub consumer's review event of LEGION-208's pull request failed three deliveries, and
//     the consumer, made again, moved past it: the sweep listed it at its 07:50 pass and gave it up
//     at the next, 07:55 (ListPassedFailures, SweepPassedFailures);
//   - a Dispatch message about LEGION-214 did not decode, and was given up at its first delivery
//     (RecordUndecodable).
//
// The project's controller is registered, so each give-up is counted in a controller notice due a
// minute later (record's countGiveUps), which the outbox's controller-notice lane publishes then
// (ClaimRow, FinishOutbox). The records' ids are the clone's sequence's, which an insert's
// conflict consumes too.
func populatedIntake(s *seeding) {
	publish := func(given record.IntakeFailure, at time.Time) {
		s.t.Helper()
		if given.NoticeID == 0 {
			s.t.Fatalf("seed the give-up of %s's message %d: no notice counted it, though a controller is registered", given.Consumer, given.StreamSeq)
		}
		row := s.lease(given.NoticeID, at, time.Minute)
		s.in("the publish of the intake-gave-up notice", func(ctx context.Context, tx pgx.Tx) error {
			return s.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
		})
	}

	bound := record.IntakeMessage{Project: "LEGION", Consumer: "legion-go-LEGION-dispatch",
		Subject: "notifications.dispatch.issue.LEGION-213.issue.updated", StreamSeq: 6044801, StoredAt: at(22, 6, 30, 0),
		Payload: []byte(`{"source":"dispatch","event_id":"dispatch-6044801","key":"LEGION-213"}`), EventSource: "dispatch", EventID: "dispatch-6044801"}
	const boundError = "apply the issue event: read LEGION-213 from Dispatch: context deadline exceeded"
	s.in("the first failed delivery of LEGION-213's event", func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.records.NoteFailure(ctx, tx, bound, 1, boundError, at(22, 6, 30, 4))
		return err
	})
	var boundGiven record.IntakeFailure
	s.in("the give-up of LEGION-213's event at its bound", func(ctx context.Context, tx pgx.Tx) error {
		failure, err := s.records.NoteFailure(ctx, tx, bound, 21, boundError, at(22, 7, 31, 4))
		if err != nil {
			return err
		}
		given, gaveUp, err := s.records.GiveUpIntakeMessage(ctx, tx, "LEGION", failure.ID, record.GiveUpBound, boundError, at(22, 7, 31, 4))
		if err == nil && !gaveUp {
			s.t.Fatalf("seed the give-up of LEGION-213's event: record %d was no open failure", failure.ID)
		}
		boundGiven = given
		return err
	})
	publish(boundGiven, at(22, 7, 32, 5))

	passed := record.IntakeMessage{Project: "LEGION", Consumer: "legion-go-LEGION-github",
		Subject: "notifications.github.acme.widgets.pr.1247.review", StreamSeq: 812004, StoredAt: at(22, 6, 2, 0),
		Payload: []byte(`{"source":"github","event_id":"github-9b2c71e0","repository":"acme/widgets","number":1247}`), EventSource: "github", EventID: "github-9b2c71e0"}
	const passedError = "apply the review event: read acme/widgets#1247 from GitHub: 502 Bad Gateway"
	for _, failed := range []struct {
		delivered int
		at        time.Time
	}{{1, at(22, 6, 2, 3)}, {3, at(22, 6, 20, 0)}} {
		s.in("a failed delivery of LEGION-208's review event", func(ctx context.Context, tx pgx.Tx) error {
			_, err := s.records.NoteFailure(ctx, tx, passed, failed.delivered, passedError, failed.at)
			return err
		})
	}
	var listed []record.OpenFailure
	s.in("the sweep's pass that listed the review event", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		listed, err = s.records.ListPassedFailures(ctx, tx, "LEGION", passed.Consumer, passed.StreamSeq)
		return err
	})
	if len(listed) != 1 {
		s.t.Fatalf("seed the sweep of the GitHub consumer: it listed %+v, want the one open failure", listed)
	}
	var swept []record.IntakeFailure
	s.in("the sweep's give-up of the review event", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		swept, err = s.records.SweepPassedFailures(ctx, tx, "LEGION", []int64{listed[0].ID}, at(22, 7, 50, 0), at(22, 7, 55, 0))
		return err
	})
	if len(swept) != 1 {
		s.t.Fatalf("seed the sweep of the GitHub consumer: it gave up %+v, want the one failure it listed", swept)
	}
	publish(swept[0], at(22, 7, 56, 1))

	undecodable := record.IntakeMessage{Project: "LEGION", Consumer: "legion-go-LEGION-dispatch",
		Subject: "notifications.dispatch.issue.LEGION-214.issue.updated", StreamSeq: 6044896, StoredAt: at(22, 8, 40, 12),
		Payload: []byte(`{"source":"dispatch","event_id":`)}
	var undecodableGiven record.IntakeFailure
	s.in("the give-up of LEGION-214's message that did not decode", func(ctx context.Context, tx pgx.Tx) error {
		given, recorded, err := s.records.RecordUndecodable(ctx, tx, undecodable, 1, "decode Envoy envelope: unexpected end of JSON input", at(22, 8, 40, 13))
		if err == nil && !recorded {
			s.t.Fatal("seed the give-up of LEGION-214's message: it was recorded as given up already")
		}
		undecodableGiven = given
		return err
	})
	publish(undecodableGiven, at(22, 8, 41, 14))
}

// seedPaused is the populated record ten seconds after the controller paused the project for a
// provider's outage (internal/pause's Service.Pause, store.Store.Pause), at the moment the
// populated state is read: the pause has asked every live role of the project to stop, and no stop
// has ended yet, so it waits on each of them. The start that gave LEGION-208's implementer its task
// finished before the pause (record.Store.FinishOutbox), so no lane holds a row of the project.
// The rows the pause holds are the starts and notices due under it (record.Store.PauseHeldRows):
// the sweep's start of LEGION-208's architect, which names its claim, and the catch-up of that
// architect whose paced moment came three seconds into the pause, which names none, since a notice
// names no claim.
func seedPaused(s *seeding) composition {
	c, rows := seedPopulatedRecord(s)
	s.in("the finish of LEGION-208's start", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.FinishOutbox(ctx, tx, rows.running208, rows.runningLease)
	})
	_, err := pauseService(s, c.cfg, populatedReadAt).Pause(s.ctx, store.PauseActor{Kind: store.PauseByController, Session: "ses_controller"},
		"the model provider is refusing every turn")
	s.must("the pause", err)
	c.now = populatedReadAt.Add(10 * time.Second)
	return c
}

// pauseService is the daemon's pause service of cfg's project over the seeded store, at the moment
// at (internal/pause.New, which the daemon builds with its workflow).
func pauseService(s *seeding, cfg config.Config, at time.Time) *pause.Service {
	s.t.Helper()
	pauses, err := pause.New(pause.Options{Store: s.st, Records: s.records, Project: cfg.Project, Config: cfg,
		Log: quietLogger(), Now: func() time.Time { return at }})
	s.must("the pause service", err)
	return pauses
}
