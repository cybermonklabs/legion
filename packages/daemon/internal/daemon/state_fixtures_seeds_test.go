package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/apifixture"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// seedStage3 is a project at Stage 3: one root in review whose Dispatch status write has failed
// twice and waits on its backoff, and a root queued behind it, on the smallest tmux file.
func seedStage3(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "minimalSettingsFile"))
	reviewing := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "LEGION-208", Phase: phase.Reviewing,
		Status: "needs_review", DispatchStatus: "needs_review", Generation: 4, LastDispatchSeq: 9,
		Repo: "sjawhar/legion-smoke", DefaultBranch: "main", GitHubRepo: "sjawhar/legion-smoke", HandedOver: true}
	queued := record.Issue{Key: "LEGION-209", Tree: "LEGION-209", Project: "LEGION", Title: "LEGION-209", Phase: phase.Admitted,
		Status: "todo", DispatchStatus: "todo", Generation: 1, LastDispatchSeq: 10, Rank: "00009",
		Repo: "sjawhar/legion-smoke", DefaultBranch: "main", GitHubRepo: "sjawhar/legion-smoke", HandedOver: true}
	reading := func(key, status string, seq int64) record.DispatchReading {
		return record.DispatchReading{Key: key, Project: "LEGION", Seq: seq, Status: record.Carry(status), Parent: record.Carry(""),
			Blockers: record.Carry([]string{}), Applied: true}
	}
	s.putRecorded(
		recorded{reviewing, reading("LEGION-208", "needs_review", 9), time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC)},
		recorded{queued, reading("LEGION-209", "todo", 10), time.Date(2126, 9, 22, 16, 2, 0, 0, time.UTC)},
	)
	s.in("the slot of LEGION-208", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2126, 9, 22, 14, 0, 0, 0, time.UTC)})
	})
	s.openTrees("LEGION", "LEGION-208")
	// The move to review wrote LEGION-208's Dispatch status (workflow's status write,
	// record.OrdinaryStatusWrite), and two attempts of it failed (the outbox's RetryOutbox).
	write := s.enqueue("LEGION-208", record.OrdinaryStatusWrite("needs_review", "in_progress", ""), time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC))
	for attempt, leasedAt := range []time.Time{time.Date(2126, 9, 22, 17, 24, 0, 0, time.UTC), time.Date(2126, 9, 22, 17, 28, 0, 0, time.UTC)} {
		row := s.lease(write, leasedAt, time.Minute)
		nextAt := time.Date(2126, 9, 22, 17, 28, 0, 0, time.UTC)
		if attempt == 1 {
			nextAt = time.Date(2126, 9, 22, 17, 32, 0, 0, time.UTC)
		}
		s.in("a failed attempt of LEGION-208's status write", func(ctx context.Context, tx pgx.Tx) error {
			return s.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, "Dispatch unavailable")
		})
	}
	stage3At := time.Date(2126, 9, 22, 17, 30, 0, 0, time.UTC)
	for _, booted := range []time.Time{time.Date(2126, 9, 18, 14, 3, 27, 0, time.UTC), time.Date(2126, 9, 19, 9, 0, 0, 0, time.UTC),
		time.Date(2126, 9, 20, 9, 0, 0, 0, time.UTC), time.Date(2126, 9, 21, 9, 0, 0, 0, time.UTC), stage3At} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	return composition{cfg: cfg, now: time.Date(2126, 9, 22, 17, 30, 30, 0, time.UTC), startedAt: stage3At}
}

// seedOperatorClaim is an operator's spawn of an architect on an issue the workflow keeps no record
// of (internal/api's spawn route: store.Store.OpenTreeLifecycle and OpenOperatorIssueLifecycle
// under the operator's authority, then the claim's admission), idle in its tmux pane, on the
// smallest tmux file of its own project. No record moves, so its moments are the wall clock's
// month.
func seedOperatorClaim(s *seeding) composition {
	const project = "AC91849371"
	minimal := apiSettings(s.t, "minimalSettingsFile")
	body, found := strings.CutPrefix(minimal, "project: LEGION\n")
	if !found {
		s.t.Fatalf("the smallest settings file %q does not begin with its project", minimal)
	}
	cfg := apifixture.LoadSettings(s.t, "project: "+project+"\n"+body)
	spawnedAt := time.Date(2026, 9, 25, 5, 50, 0, 0, time.UTC)
	_, err := s.st.RecordBoot(s.ctx, project, spawnedAt)
	s.must("the boot", err)
	_, err = s.st.OpenTreeLifecycle(s.ctx, "ac91849371", "AC91849371-900", treelifecycle.AuthorityOperator)
	s.must("the operator's tree", err)
	_, err = s.st.OpenOperatorIssueLifecycle(s.ctx, "ac91849371", "AC91849371-900", func(context.Context, pgx.Tx) error { return nil })
	s.must("the operator's issue", err)
	token := s.token(project, "AC91849371-900", claim.RoleArchitect)
	s.claimAs(supervise.Claim{Token: token, Project: "ac91849371", Tree: "AC91849371-900", Issue: "AC91849371-900", Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateIdle, Session: "01a0d71d-aace-7448-a0d4-e401d736438f",
		Locator: &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: token, Incarnation: "1618075:268131172", Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}})
	return composition{cfg: cfg, now: spawnedAt.Add(5 * time.Minute), startedAt: spawnedAt}
}

// seedLastPause is a project that runs after a pause has ended: the operator paused it for a
// provider's outage, and the controller resumed it from its session (internal/pause's Service.Pause
// and Service.Resume), with the moments, the reason and the actors of the pause the pause routes'
// goldens answer (apifixture.Pause), on the deployment those routes serve; the store numbers it.
// Its daemon booted and its image passed the probe, and no session has reported to it, so
// every image row of its capability report is present and every live row unchecked: the state a
// boot reads. No record moves, so its moments are the wall clock's month.
func seedLastPause(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "settingsFixtureFile"))
	paused, ended := apifixture.Pause(false), apifixture.Pause(true)
	startedAt := time.Date(2026, 11, 15, 8, 30, 0, 0, time.UTC)
	for _, booted := range []time.Time{time.Date(2026, 11, 2, 14, 3, 27, 0, time.UTC), startedAt} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	_, err := pauseService(s, cfg, paused.Since).Pause(s.ctx, paused.By, paused.Reason)
	s.must("the pause", err)
	_, _, err = pauseService(s, cfg, ended.EndedAt).Resume(s.ctx, ended.EndedBy)
	s.must("the resume", err)
	// A daemon that holds a pause runs a workflow, whose three durable consumers are caught up and
	// whose store holds state events (seeding.published).
	caughtUp := ended.EndedAt.Add(19 * time.Minute)
	s.published(cfg.Project, caughtUp)
	return composition{cfg: cfg, now: ended.EndedAt.Add(20 * time.Minute), startedAt: startedAt,
		secretsIssued: true, probed: true, image: bootprobe.ImageReport{ModelFallback: "on"},
		consumers: []intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-github", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-wake", LastHandledAt: caughtUp},
		}}
}

// seedDaemonController is a daemon that launches its own controller (`controller: daemon`, on the
// kubernetes file), whose claim is on no issue and no tree: the keeper admits it with no tree
// (controllerKeeper.keep, through the supervisor's Create and store.Store.AdmitClaim), and its
// session registered with the capability the daemon minted (store.Store.MintController,
// RegisterController). The state shows it as controllerLocator and on no issue's view. Beside it,
// one tree runs: LEGION-208, a root just started, its architect ready in the issue's pod, whose
// session reported its live rows. LEGION-209 is a child of that tree held at admitted behind
// LEGION-300, an issue still in progress. LEGION-210 is a root that waits behind LEGION-301, an
// issue the daemon has never observed and so cannot call finished. LEGION-211 is a root a
// hand-over of the release before carried in (record.CarriedIn), which waits on LEGION-302 too,
// but carries a run, so no Dispatch link holds it (blockers.Exempt). The two waiting roots are live
// trees of their own, so `trees` names three roots while one issue holds a slot. The records move,
// so the moments are of 2126, after the database's own clock (the populated seed's reason, `at`).
func seedDaemonController(s *seeding) composition {
	cfg := apifixture.LoadSettings(s.t, apiSettings(s.t, "settingsFixtureFile"))
	admitted := time.Date(2126, 10, 6, 18, 2, 11, 0, time.UTC)
	issue := func(key, tree string, ph phase.Phase, status string, generation uint64, seq int64) record.Issue {
		return record.Issue{Key: key, Tree: tree, Project: "LEGION", Title: key, Phase: ph, Status: status, DispatchStatus: status,
			Generation: generation, LastDispatchSeq: seq, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", HandedOver: true}
	}
	reading := func(key, status string, seq int64, parent, rank string, blockers ...string) record.DispatchReading {
		if blockers == nil {
			blockers = []string{}
		}
		r := record.DispatchReading{Key: key, Project: "LEGION", Seq: seq, Status: record.Carry(status), Parent: record.Carry(parent),
			Blockers: record.Carry(blockers), Applied: true}
		if rank != "" {
			r.Rank = record.Carry(rank)
		}
		return r
	}
	root := issue("LEGION-208", "LEGION-208", phase.Admitted, "in_progress", 1, 4)
	child := issue("LEGION-209", "LEGION-208", phase.Admitted, "todo", 1, 5)
	child.Parent = new("LEGION-208")
	blocked := issue("LEGION-210", "LEGION-210", phase.Admitted, "todo", 1, 6)
	blocked.Rank = "00010"
	// LEGION-211 runs the phase a hand-over of the release before carried it into, at the
	// generation the first boot's conversion copied onto its record with its old root's gate
	// (record.CarryHandedOverRoots).
	handed := issue("LEGION-211", "LEGION-211", phase.Implementing, "todo", 2, 7)
	handed.Rank, handed.CarriedGeneration, handed.CarriedGate = "00011", 2, true
	s.putRecorded(
		recorded{root, reading("LEGION-208", "in_progress", 4, "", ""), admitted},
		recorded{child, reading("LEGION-209", "todo", 5, "LEGION-208", "", "LEGION-300"), admitted.Add(-20 * time.Minute)},
		recorded{blocked, reading("LEGION-210", "todo", 6, "", "00010", "LEGION-301"), admitted.Add(-40 * time.Minute)},
		recorded{handed, reading("LEGION-211", "todo", 7, "", "00011", "LEGION-302"), admitted.Add(-50 * time.Minute)},
	)
	// What Dispatch says of the two blockers the daemon keeps no record of (blockers.Mirror writes
	// every issue fact it applies). LEGION-301 has no row: the daemon has never observed it.
	s.mirrorOnly(reading("LEGION-300", "in_progress", 8, "", ""))
	s.mirrorOnly(reading("LEGION-302", "todo", 9, "", ""))
	// The promotion of LEGION-208 took its slot and opened its tree (admit's promotion).
	s.in("the slot of LEGION-208", func(ctx context.Context, tx pgx.Tx) error {
		return s.records.PutSlot(ctx, tx, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: admitted})
	})
	s.openTrees("LEGION", "LEGION-208")
	for _, booted := range []time.Time{admitted.Add(-time.Hour), admitted.Add(-time.Minute)} {
		_, err := s.st.RecordBoot(s.ctx, "LEGION", booted)
		s.must("a boot", err)
	}
	// The architect is ready in its issue's pod, in a `dispatch` call whose start the daemon
	// received and whose end it has not.
	architect := s.token("LEGION", "LEGION-208", claim.RoleArchitect)
	architectLocator := controllerClaimPodLocator(architect, "legion-legion-legion-208", "4b7e1c2a-0d93-4f6e-8a51-3c2f9e7d1b04", runtime.IssuePodContainer)
	s.claimAs(supervise.Claim{Token: architect, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateReady, Session: "ses_architect_208", Locator: architectLocator, PluginContract: api.ServingFloor,
		Step: supervise.StepRecord{At: admitted.Add(50 * time.Second), TurnStartedAt: admitted.Add(40 * time.Second), Tool: "dispatch",
			CallID: "toolu_arch_7", ArgsBytes: 96, ArgsHash: "5a1c7e03b94d26f8", StartedAt: admitted.Add(50 * time.Second)}})
	// The architect's session sent its live rows on a `claims/ready` of its own (LEGION-663,
	// INFRA-927), from the process the locator names. The controller measures nothing and sends
	// none.
	s.must("the capability report of "+string(architect), s.st.PutCapabilityReport(s.ctx, capabilities.Report{
		Claim: architect, Generation: 1, Locator: *architectLocator,
		MeasuredAt: admitted.Add(45 * time.Second), ReportedAt: admitted.Add(46 * time.Second), ElapsedMs: 1200,
		Rows: []capabilities.Row{
			{Name: capabilities.Subagents, OK: true, Detail: "6 task agents discovered: deep-worker, oracle, plan-gap-analyst, plan-reviewer, thermonuclear-code-quality, thermonuclear-deep-review"},
			{Name: capabilities.WebSearch, OK: true, Detail: "web_search is active and a query answered"},
			{Name: capabilities.MCP, OK: true, Detail: "no MCP server is configured"},
			{Name: capabilities.RepositoryExtensions, OK: true, Detail: "2 extension paths discovered"},
			{Name: capabilities.DispatchEnvoyTools, OK: true, Detail: "dispatch read LEGION-208 answered; envoy_whoami is active"},
			{Name: capabilities.GitHub, Detail: "gh api user: HTTP 401: Bad credentials"},
		},
	}))
	// The daemon's controller is ready in its own pod, in a `bash` call. Its claim is on no issue,
	// so no claim view carries its call (`legion claims list` does).
	controller := claim.ControllerToken("legion")
	s.claimAs(supervise.Claim{Token: controller, Project: "legion", Role: claim.RoleController, Generation: 1,
		State: supervise.StateReady, Session: "ses_controller",
		Locator: controllerClaimPodLocator(controller, "legion-legion-controller", "9d2a6f13-7c48-4e0b-b5a9-1f8e3d6c2a57", runtime.ControllerPodContainer),
		Step: supervise.StepRecord{At: admitted.Add(55 * time.Second), TurnStartedAt: admitted.Add(52 * time.Second), Tool: "bash",
			CallID: "toolu_ctl_2", ArgsBytes: 40, ArgsHash: "8d30be61f7c2904a", StartedAt: admitted.Add(55 * time.Second)}})
	generation, err := s.st.MintController(s.ctx, "legion", []byte("controller-capability-hash"))
	s.must("the controller's capability", err)
	registered, err := s.st.RegisterController(s.ctx, "legion", generation, "ses_controller", []byte("controller-secret-hash"), admitted.Add(-30*time.Second), api.ServingFloor)
	s.must("the controller's registration", err)
	if !registered {
		s.t.Fatal("seed the controller's registration: the store did not register it")
	}
	// The daemon booted and its image passed the probe, and a daemon with a workflow opens three
	// durable consumers, which are caught up, and its store holds state events (seeding.published).
	caughtUp := admitted.Add(50 * time.Second)
	s.published(cfg.Project, caughtUp)
	return composition{cfg: cfg, now: admitted.Add(time.Minute), startedAt: admitted.Add(-time.Minute),
		secretsIssued: true, probed: true, image: bootprobe.ImageReport{ModelFallback: "on"},
		consumers: []intake.ConsumerLag{
			{Name: "legion-go-LEGION-dispatch", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-github", LastHandledAt: caughtUp},
			{Name: "legion-go-LEGION-wake", LastHandledAt: caughtUp},
		}}
}

// controllerClaimPodLocator is one role process at generation 1 in the Sandbox sandbox, whose pod
// is podUID, in its container: `roles` in an issue pod, `controller` in the controller's.
func controllerClaimPodLocator(token claim.Token, sandbox, podUID, container string) *runtime.Locator {
	return &runtime.Locator{
		Runtime: runtime.RuntimeSandbox, Claim: token, Incarnation: runtime.SandboxIncarnation(podUID, 1),
		Sandbox: &runtime.SandboxLocator{Namespace: "legion", Name: sandbox, PodUID: podUID, Container: container, Generation: 1},
	}
}
