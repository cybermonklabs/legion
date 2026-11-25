package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// An admitted root's architect is launched with the start admission queues, which carries its
// first task (workflow.ArchitectStartTask). Once its claim is ready, the daemon also tells it what
// its tree holds.
// Each launch's ready tells it again, as the TypeScript daemon sends the catch-up at every ready of
// an active root: a relaunch in the same generation may be an architect that died during its first
// turn, or a fresh agent whose workspace was lost, and neither was woken. A ready is one fact per
// launch, so the boot's replay of one already applied tells nothing more. A catch-up the launch
// before never had delivered is dropped, so the new agent is given one catch-up, the current one.
// The notice states each design-gate step the root must hold open, whether a person must approve
// it, and that nothing has registered it yet, so the architect knows what to register and
// whether to ask for approval.
func TestARootArchitectIsToldItsTreeAtEachLaunchsReady(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy config.DesignGate
	}{
		{"the design gate armed", config.DesignGateRootIssues},
		{"the design gate off", config.DesignGateOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAdmittedTree(t, pool, 1)
			engine := testEngine(tc.policy, nil)
			// The reason is worded for a relaunch mid-tree as much as for the first launch: an
			// architect told to start over would edit its spec, and a new version closes its gate.
			// Nothing has registered the tree's gate, so the one step it must hold open is
			// unregistered; a deployment on `off` gates that step for no person.
			gates := "spec(gated=true,unregistered=true)"
			if tc.policy == config.DesignGateOff {
				gates = "spec(gated=false,unregistered=true)"
			}
			told := func(generation, launch int) string {
				return fmt.Sprintf("gen=%d launch=%d gates=%s issues=LEGION-1,LEGION-2 done= accepting=", generation, launch, gates)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
			if got, want := catchUps(t, pool), []string{told(1, 1)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the root architect's first ready, catch-ups %v; want %v", got, want)
			}

			// The first launch dies before its catch-up is delivered; the relaunch's replaces it.
			first := catchUpRows(t, pool)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got, want := catchUps(t, pool), []string{told(1, 2)}; fmt.Sprint(got) != fmt.Sprint(want) || fmt.Sprint(catchUpRows(t, pool)) == fmt.Sprint(first) {
				t.Fatalf("after a relaunch in the same generation, catch-ups %v in rows %v (before, %v); want the relaunch's own, alone", got, catchUpRows(t, pool), first)
			}

			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
			if got := catchUps(t, pool); len(got) != 1 {
				t.Fatalf("after the boot's replay of that ready, catch-ups %v; want still the one", got)
			}

			// That catch-up is delivered, and the tree is re-admitted.
			if _, err := pool.Exec(t.Context(), "delete from outbox"); err != nil {
				t.Fatalf("deliver the queued notices: %v", err)
			}
			seedAdmittedTree(t, pool, 2)
			claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 3)
			if got, want := catchUps(t, pool), []string{told(2, 3)}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("after the re-admitted tree's ready, catch-ups %v; want %v", got, want)
			}
		})
	}
}

// A root a hand-over created at this generation is said to be handed over in its architect's
// catch-up, whatever the phase, so the architect resumes rather than decomposing the tree anew; it
// is said to be carried as well when its old root's gate came with it, so the architect registers
// no spec. The same root at a later generation (closed and reopened) is a root like any other.
func TestACatchUpMarksAHandedOverRootInEveryPhase(t *testing.T) {
	for _, at := range []phase.Phase{phase.Planning, phase.Reviewing, phase.Accepting} {
		for _, gateCarried := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, gate carried %t", at, gateCarried), func(t *testing.T) {
				pool := migratedPool(t)
				root := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: at, Generation: 2, Status: "in_progress", Rank: "U"}
				seedIssue(t, pool, root)
				if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
					return record.NewStore().PutHandedOverRoot(context.Background(), tx, record.HandedOverRoot{Issue: root.Key, Generation: 2, GateCarried: gateCarried})
				}); err != nil {
					t.Fatalf("seed handed-over root: %v", err)
				}
				engine := testEngine(config.DesignGateRootIssues, nil)
				claimReady(t, pool, engine, root.Key, claim.RoleArchitect, 1)
				if got := lastCatchUp(t, pool); !got.HandedOver || got.Carried != gateCarried {
					t.Fatalf("handed-over root in %s: catch-up says handed over %t, carried %t; want true, %t", at, got.HandedOver, got.Carried, gateCarried)
				}
				if _, err := pool.Exec(t.Context(), "delete from outbox"); err != nil {
					t.Fatal(err)
				}
				root.Generation = 3
				seedIssue(t, pool, root)
				claimReady(t, pool, engine, root.Key, claim.RoleArchitect, 2)
				if got := lastCatchUp(t, pool); got.HandedOver || got.Carried {
					t.Fatalf("the root's later generation in %s: catch-up still says handed over %t, carried %t", at, got.HandedOver, got.Carried)
				}
			})
		}
	}
}

// A decision kept while nobody in a running tree could hear it (its architect's claim had ended)
// reaches the architect's next launch, a `legion claims retry` relaunch among them, in that
// launch's catch-up: a catch-up that was never delivered leaves the decision kept, so the relaunch
// whose ready drops it names it again, and only the forget of what a delivered catch-up named (the
// outbox's notice) leaves nothing for the launch after.
func TestARelaunchedArchitectsCatchUpNamesTheDecisionsKeptForIt(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	seedAdmittedTree(t, pool, 1)
	engine := testEngine(config.DesignGateRootIssues, nil)
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return record.NewStore().AddParkedDecision(ctx, tx, record.ParkedDecision{Issue: "LEGION-1", Ref: "dispatch://LEGION-1/spec",
			Decision: record.DecisionDesignApproved, Actor: "user:alice", At: at})
	}); err != nil {
		t.Fatalf("keep a decision: %v", err)
	}
	claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 1)
	first := lastCatchUp(t, pool)
	if first.Decided != "" {
		t.Fatalf("the relaunch's catch-up writes the decisions as text %q, want them as fields alone", first.Decided)
	}
	if first.Kept == nil || len(first.Kept.Decisions) != 1 || first.Kept.Decisions[0].Ref != "dispatch://LEGION-1/spec" ||
		!first.Kept.Decisions[0].At.Equal(at) || first.Kept.Decisions[0].Decision != record.DecisionDesignApproved ||
		first.Kept.Decisions[0].Actor != "user:alice" {
		t.Fatalf("the catch-up's kept record %#v, want the decision it named, at %s", first.Kept, at)
	}
	claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 2)
	if got := lastCatchUp(t, pool); got.Kept == nil || len(got.Kept.Decisions) != 1 || len(catchUps(t, pool)) != 1 {
		t.Fatalf("after a relaunch dropped that catch-up undelivered, catch-ups %v naming %#v; want one naming the decision again", catchUps(t, pool), got.Kept)
	}
	// The delivery of a catch-up forgets exactly what it named and its row claims, and nothing else
	// does.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var row int64
		if err := tx.QueryRow(ctx, "select id from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id desc limit 1").Scan(&row); err != nil {
			return err
		}
		return record.NewStore().ForgetKeptDecisions(ctx, tx, "LEGION-1", *lastCatchUp(t, pool).Kept, row)
	}); err != nil {
		t.Fatalf("forget what the delivered catch-up named: %v", err)
	}
	claimReady(t, pool, engine, "LEGION-1", claim.RoleArchitect, 3)
	if got := lastCatchUp(t, pool); got.Kept != nil {
		t.Fatalf("the launch after a delivered catch-up is told kept %#v, want nothing", got.Kept)
	}
}

func lastCatchUp(t *testing.T, pool *pgxpool.Pool) record.CatchUp {
	t.Helper()
	var got *record.CatchUp
	for _, payload := range noticeRows(t, pool) {
		if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" {
			got = notice.CatchUp
		}
	}
	if got == nil {
		t.Fatal("no catch-up queued")
	}
	return *got
}

// A catch-up names a kept approval's step as the kept-decisions task does: an approval an older
// daemon kept recorded no step, and it is the spec's, the step every deployment requires
// (record.DesignStep.OrSpec). A catch-up that keeps no approval names no step.
func TestACatchUpNamesTheStepOfAnApprovalKeptWithNone(t *testing.T) {
	kept := record.KeptDecisions{KeptApproval: record.KeptApproval{Approved: 3, Latest: 5}}
	facts, _ := keptFacts(kept)
	if want := (texts.KeptApproval{Step: "spec", Approved: 3, Latest: 5}); facts.KeptApproval != want {
		t.Errorf("the catch-up names the approval %+v, want %+v", facts.KeptApproval, want)
	}
	if task := KeptDecisionsOf("LEGION-208", kept); task.ApprovalStep != facts.Step {
		t.Errorf("the catch-up names the step %q and the kept-decisions task %q, want one step", facts.Step, task.ApprovalStep)
	}
	none, _ := keptFacts(record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-208/ask/1",
		Decision: record.DecisionAskAnswered}}})
	if none.KeptApproval != (texts.KeptApproval{}) {
		t.Errorf("a catch-up that keeps no approval names %+v, want no approval", none.KeptApproval)
	}
}

// An issue in accepting waits for the architect that owns it alone: no worker runs there, and the
// one phase-finished notice that moved it there reaches no architect whose claim has failed: it
// passes a child's failed architect for the one above, and ends undelivered when a root's has
// failed (the outbox's notice), so a retried architect was never told. Its catch-up is its only
// wake, and it names every issue in accepting whose acceptance is that architect's, whatever each
// is waiting for: the acceptance itself is refused while an issue beneath it is still in the
// workflow (signOff), and the catch-up is what tells the architect the issue is there to be
// decided.
func TestACatchUpNamesEveryIssueWaitingForTheArchitectsAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		root, child             phase.Phase
		rootStatus, childStatus string
		// issues are the issues named with their fields, done the done ones by key, accepting
		// the ones waiting for the architect's acceptance.
		issues, done, accepting string
	}{
		{"the root accepting, its child done", phase.Accepting, phase.Done, "needs_review", "done", "LEGION-1", "LEGION-2", "LEGION-1"},
		{"the root accepting, its child parked in backlog", phase.Accepting, phase.Done, "needs_review", "backlog", "LEGION-1,LEGION-2", "", "LEGION-1"},
		{"the root accepting, its child still implementing", phase.Accepting, phase.Implementing, "needs_review", "in_progress", "LEGION-1,LEGION-2", "", "LEGION-1"},
		{"a child accepting while the root implements", phase.Implementing, phase.Accepting, "in_progress", "needs_review", "LEGION-1,LEGION-2", "", "LEGION-2"},
		{"the root and its child both accepting", phase.Accepting, phase.Accepting, "needs_review", "needs_review", "LEGION-1,LEGION-2", "", "LEGION-1,LEGION-2"},
		{"the root merging", phase.Merging, phase.Done, "retro", "done", "LEGION-1", "LEGION-2", ""},
		{"nothing accepting", phase.Reviewing, phase.Testing, "needs_review", "testing", "LEGION-1,LEGION-2", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := seedAdmittedTree(t, pool, 1)
			root.Phase, root.Status = tc.root, tc.rootStatus
			seedIssue(t, pool, root)
			parent := root.Key
			seedIssue(t, pool, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Child", Parent: &parent,
				Phase: tc.child, Generation: 1, Status: tc.childStatus, Rank: "V"})
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), root.Key, claim.RoleArchitect, 1)
			got := catchUps(t, pool)
			want := fmt.Sprintf("gen=1 launch=1 gates=spec(gated=true,unregistered=true) issues=%s done=%s accepting=%s", tc.issues, tc.done, tc.accepting)
			if len(got) != 1 || got[0] != want {
				t.Fatalf("catch-ups %v; want [%s]", got, want)
			}
		})
	}
}

// A Verification document its architect registered as criteria beside a deployment of spec alone
// holds the tree as the spec does, so the catch-up states it as a step of its own, after the
// spec and gated as the spec is, each naming its document by its dispatch:// link. A root that
// registered none is told the spec alone, as before.
func TestACatchUpStatesARegisteredVerificationDocumentAsAGatedStep(t *testing.T) {
	spec := record.CatchUpGate{Step: record.DesignStepSpec, Gated: true, Document: "dispatch://LEGION-1/spec", Artifact: "artifact-1",
		Version: 4, Approved: 4, Open: true}
	for _, tc := range []struct {
		name     string
		criteria bool
		want     []record.CatchUpGate
	}{
		{name: "a criteria row registered", criteria: true, want: []record.CatchUpGate{spec,
			{Step: record.DesignStepCriteria, Gated: true, Document: "dispatch://LEGION-1/artifact/" + dispatch.VerificationSlug,
				Artifact: "artifact-1-verification", Version: 2}}},
		{name: "no criteria row", want: []record.CatchUpGate{spec}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAdmittedTree(t, pool, 1)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepSpec, ArtifactID: "artifact-1",
				LatestVersion: 4, ApprovedVersion: new(4)})
			if tc.criteria {
				seedGate(t, pool, record.DesignGate{Issue: "LEGION-1", Step: record.DesignStepCriteria, ArtifactID: "artifact-1-verification",
					LatestVersion: 2})
			}
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), "LEGION-1", claim.RoleArchitect, 1)
			if got := lastCatchUp(t, pool).Gates; fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("the catch-up states %#v, want %#v", got, tc.want)
			}
		})
	}
}

// Only a live tree's architects are told their issues. A tree that lingers or has left the
// workflow is told nothing, nor is a phase worker on the root; a child's own architect is told
// the child and the issues under it, with no gate.
func TestOnlyALiveTreesArchitectsAreToldTheirIssues(t *testing.T) {
	// A lingering root set back to todo still lingers until admission takes it again: only its
	// linger, not its status, says the tree has left the workflow.
	lingering := func(root *record.Issue) {
		until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
		root.Phase, root.Status, root.LingerUntil = phase.Done, "todo", &until
	}
	for _, tc := range []struct {
		name  string
		root  func(*record.Issue)
		issue string
		role  claim.Role
		want  []string
	}{
		{"a lingering tree set back to todo", lingering, "LEGION-1", claim.RoleArchitect, nil},
		{"a tree moved to backlog", func(root *record.Issue) { root.Status = "backlog" }, "LEGION-1", claim.RoleArchitect, nil},
		{"a sub-architect", func(*record.Issue) {}, "LEGION-2", claim.RoleArchitect, []string{
			"gen=1 launch=1 gates= issues=LEGION-2 done= accepting="}},
		{"a phase worker on the root", func(*record.Issue) {}, "LEGION-1", claim.RolePlanner, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			root := seedAdmittedTree(t, pool, 1)
			tc.root(&root)
			seedIssue(t, pool, root)
			claimReady(t, pool, testEngine(config.DesignGateRootIssues, nil), tc.issue, tc.role, 1)
			if got := catchUps(t, pool); fmt.Sprint(got) != fmt.Sprint(tc.want) || len(got) != len(tc.want) {
				t.Fatalf("catch-ups %v; want %v", got, tc.want)
			}
		})
	}
}

// seedAdmittedTree records a root as admission leaves it, in_progress at generation, with one
// child, and returns the root.
func seedAdmittedTree(t *testing.T, pool *pgxpool.Pool, generation uint64) record.Issue {
	t.Helper()
	root := record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Root", Phase: phase.Admitted,
		Generation: generation, Status: "in_progress", Rank: "U"}
	parent := root.Key
	seedIssue(t, pool, root)
	seedIssue(t, pool, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-1", Project: "LEGION", Title: "Child", Parent: &parent,
		Phase: phase.Admitted, Generation: generation, Status: "todo", Rank: "V"})
	return root
}

// claimReady applies the supervision observation the workflow runtime sends when a claim is ready
// (workflowRuntime.applyTerminal), under the event id it uses: one per launch of the claim.
func claimReady(t *testing.T, pool *pgxpool.Pool, engine *Engine, issue string, role claim.Role, launch int) {
	t.Helper()
	token, err := claim.NewToken("legion", issue, role)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", fmt.Sprintf("supervise:%s:%d:ready", token, launch),
		intake.ClaimReady{Issue: issue, Role: role, Launch: uint64(launch)}, engine, admissionStub{}); err != nil {
		t.Fatalf("apply the ready of %s: %v", token, err)
	}
}

// catchUps is every catch-up notice the outbox holds, in order: the generation it names, the
// launch it was written for, the design-gate steps it states, the keys of the issues it names
// that are not done, the done ones, and the ones waiting for the architect's acceptance. No
// catch-up carries a reason.
func catchUps(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	got := []string{}
	for _, payload := range noticeRows(t, pool) {
		notice, ok := payload.(record.Notice)
		if !ok || notice.Kind != "catch-up" {
			continue
		}
		if notice.Reason != "" {
			t.Fatalf("a catch-up carries the reason %q, want none", notice.Reason)
		}
		keys := []string{}
		for _, issue := range notice.CatchUp.Issues {
			keys = append(keys, issue.Key)
		}
		steps := []string{}
		for _, gate := range notice.CatchUp.Gates {
			steps = append(steps, fmt.Sprintf("%s(gated=%t,unregistered=%t)", gate.Step, gate.Gated, gate.Unregistered))
		}
		got = append(got, fmt.Sprintf("gen=%d launch=%d gates=%s issues=%s done=%s accepting=%s", notice.CatchUp.Generation, notice.CatchUp.Launch,
			strings.Join(steps, ","), strings.Join(keys, ","), strings.Join(notice.CatchUp.Done, ","), strings.Join(notice.CatchUp.Accepting, ",")))
	}
	return got
}

// catchUpRows is the outbox row id of every catch-up notice the outbox holds, in order.
func catchUpRows(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id from outbox where kind = 'notice' and payload->>'kind' = 'catch-up' order by id")
	if err != nil {
		t.Fatalf("read the catch-up rows: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("scan the catch-up rows: %v", err)
	}
	return ids
}

// The catch-up a start writes in place of a launch waits its turn in the daemon's wake pacing:
// the ready carries the mark only that start sets (intake.ClaimReady.Paced), the engine takes one
// turn from the limiter it was given, and the row is due at the moment that turn comes and
// carries it (record.Notice.PacedAt), which is what the outbox reads for the wait in the line
// that wake writes and what a ready compares its now against. A launch's own ready takes no turn,
// whatever the pacing: the launch already took one at the machine's gate. An engine with no
// limiter at all — the deployment that paces nothing — writes both at once, with no moment.
func TestAPacedReadysCatchUpIsDueAtTheMomentTheWakePacingGivesIt(t *testing.T) {
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	turn := now.Add(90 * time.Second)
	for _, tc := range []struct {
		name    string
		paced   bool
		pacing  bool
		due     time.Time
		marked  time.Time
		reserve int
	}{
		{name: "a start's ready while the pacing is on", paced: true, pacing: true, due: turn, marked: turn, reserve: 1},
		{name: "a launch's ready while the pacing is on", pacing: true, due: now},
		{name: "a start's ready while the pacing is off", paced: true, due: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAdmittedTree(t, pool, 1)
			reserved := 0
			cfg := Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Repositories: testRepositories,
				Clock: func() time.Time { return now }}
			if tc.pacing {
				cfg.Reserve = func(time.Time) time.Time { reserved++; return turn }
			}
			engine := New(record.NewStore(), cfg, nil)
			token, err := claim.NewToken("legion", "LEGION-1", claim.RoleArchitect)
			if err != nil {
				t.Fatalf("claim token: %v", err)
			}
			if _, err := intake.ApplyFact(context.Background(), pool, "outbox", fmt.Sprintf("start-running:%s:1", token),
				intake.ClaimReady{Issue: "LEGION-1", Role: claim.RoleArchitect, Launch: 1, Paced: tc.paced}, engine, admissionStub{}); err != nil {
				t.Fatalf("apply the ready: %v", err)
			}

			var due time.Time
			// A catch-up the pacing reserved no moment for carries none at all (omitzero).
			var marked *time.Time
			if err := pool.QueryRow(t.Context(), `select next_at, (payload->>'paced_at')::timestamptz from outbox
				where kind = 'notice' and payload->>'kind' = 'catch-up'`).Scan(&due, &marked); err != nil {
				t.Fatalf("read the catch-up: %v", err)
			}
			var reservedAt time.Time
			if marked != nil {
				reservedAt = marked.UTC()
			}
			if !due.UTC().Equal(tc.due) || !reservedAt.Equal(tc.marked) || reserved != tc.reserve {
				t.Fatalf("the catch-up is due at %s, reserved for %s, after %d reservations; want %s, %s, %d",
					due.UTC(), reservedAt, reserved, tc.due, tc.marked, tc.reserve)
			}
		})
	}
}

// keptAnswers keeps two answers a person gives on LEGION-208 while its tree waits for its slot, so
// nobody in it hears them (record.RelayParkedDecision keeps them for its architect's start): one
// of 300 characters on ask-one and one of 1,500 on ask-two. It returns the two answers' words.
func keptAnswers(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact)) (short, long string) {
	t.Helper()
	queued := askRoot()
	queued.HandedOver, queued.Status = true, "todo"
	seedIssue(t, pool, queued)
	seedQuestion(t, pool, "ask-one", 4)
	seedQuestion(t, pool, "ask-two", 5)
	short, long = strings.Repeat("s", 300), strings.Repeat("l", 1500)
	for i, answer := range []struct{ ask, words string }{{"ask-one", short}, {"ask-two", long}} {
		apply(answer.ask, intake.DispatchAskClosed{Key: "LEGION-208", Ask: answer.ask, Kind: record.AskQuestion, State: "answered", Actor: askPerson,
			Mark: askSeq(int64(8 + i)), Words: &record.ActWords{Text: answer.words, By: askPerson.Name(), At: resumeWordsAt.Add(time.Duration(i) * time.Minute)}})
	}
	return short, long
}

// promoted is LEGION-208 as admission leaves it once it takes its slot: in the workflow, so its
// architect's ready writes a catch-up.
func promoted(t *testing.T, pool *pgxpool.Pool) record.Issue {
	t.Helper()
	root := askRoot()
	seedIssue(t, pool, root)
	return root
}

// startArchitect queues root's architect start as admission's promotion does (Engine.StartArchitect)
// and returns the task it carries.
func startArchitect(t *testing.T, pool *pgxpool.Pool, engine *Engine, root record.Issue) string {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return engine.StartArchitect(t.Context(), tx, root)
	})
	starts := superviseRowsOf(t, pool, root.Key, "start")
	if len(starts) == 0 || starts[len(starts)-1].Role != claim.RoleArchitect {
		t.Fatalf("the start rows %+v, want the architect's start", starts)
	}
	return starts[len(starts)-1].Task
}

// R1: a person's answers on a tree nobody heard reach its architect with their words. The start
// task that names them carries each answer's words beside its reference, each in a block of its
// own, cut to 1,000 UTF-16 units each since it carries two, with the count of the rest; a later
// launch's catch-up names them the same way where no start did. Each decision is told once:
// whichever names it first claims it, and the other names it no more.
func TestAKeptDecisionCarriesThePersonsWords(t *testing.T) {
	t.Run("in the tree's next architect start", func(t *testing.T) {
		pool, apply := askRig(t)
		short, long := keptAnswers(t, pool, apply)
		engine := testEngine(config.DesignGateRootIssues, nil)
		task := startArchitect(t, pool, engine, promoted(t, pool))
		decided, _, ok := strings.Cut(task, "\n\nkind: "+string(texts.ArchitectStart))
		if !ok {
			t.Fatalf("the start task %q holds no architect-start after the kept decisions", task)
		}
		read := readTaskOf(t, decided)
		if read.fields["kind"] != string(texts.KeptDecisions) || len(read.blocks) != 2 {
			t.Fatalf("the kept-decisions task names %v with the blocks %+v, want the two answers", read.fields, read.blocks)
		}
		var rows []string
		for _, line := range read.lines {
			if strings.HasPrefix(line, "  ") {
				rows = append(rows, line)
			}
		}
		for _, block := range read.blocks {
			var row int
			if _, err := fmt.Sscanf(block.attrs["field"], "kept[%d].answer", &row); err != nil || row >= len(rows) {
				t.Fatalf("the block %+v names no row of %q", block.attrs, rows)
			}
			switch {
			case strings.Contains(rows[row], "dispatch://LEGION-208/ask/ask-one"):
				if block.text != short || block.attrs["rest"] != "" {
					t.Fatalf("ask-one's block holds %d bytes with %v, want the 300 whole", len(block.text), block.attrs)
				}
			case strings.Contains(rows[row], "dispatch://LEGION-208/ask/ask-two"):
				if block.text != long[:1000] || block.attrs["rest"] != "500" {
					t.Fatalf("ask-two's block holds %d bytes with %v, want the first 1000 and rest 500", len(block.text), block.attrs)
				}
			default:
				t.Fatalf("the block %s's row %q names neither ask", block.attrs["field"], rows[row])
			}
		}
		claimReady(t, pool, engine, "LEGION-208", claim.RoleArchitect, 1)
		if kept := lastCatchUp(t, pool).Kept; kept != nil {
			t.Fatalf("the catch-up after the start names %+v again, want each decision told once", kept)
		}
	})
	t.Run("in a later launch's catch-up", func(t *testing.T) {
		pool, apply := askRig(t)
		short, long := keptAnswers(t, pool, apply)
		engine := testEngine(config.DesignGateRootIssues, nil)
		root := promoted(t, pool)
		claimReady(t, pool, engine, "LEGION-208", claim.RoleArchitect, 1)
		var catchUp *record.Notice
		for _, payload := range noticeRows(t, pool) {
			if notice := asNotice(payload); notice.Kind == texts.CatchUp {
				catchUp = &notice
			}
		}
		if catchUp == nil || catchUp.CatchUp.Kept == nil || len(catchUp.CatchUp.Kept.Decisions) != 2 {
			t.Fatalf("the catch-up %+v, want it to name the two answers", catchUp)
		}
		t.Logf("catch-up kept: %+v, words %+v", catchUp.CatchUp.Kept.Decisions, catchUp.Words)
		for i, mark := range catchUp.CatchUp.Kept.Decisions {
			words := catchUp.Words[fmt.Sprintf("catch_up.kept.decisions[%d].answer", i)]
			switch mark.Ref {
			case "dispatch://LEGION-208/ask/ask-one":
				if mark.Answer != short || words.Rest != 0 || words.By != askPerson.Name() {
					t.Fatalf("ask-one in the catch-up holds %d bytes with %+v, want the 300 whole by the person", len(mark.Answer), words)
				}
			case "dispatch://LEGION-208/ask/ask-two":
				if mark.Answer != long[:1000] || words.Rest != 500 || words.By != askPerson.Name() {
					t.Fatalf("ask-two in the catch-up holds %d bytes with %+v, want the first 1000 and rest 500 by the person", len(mark.Answer), words)
				}
			default:
				t.Fatalf("the catch-up names %s, want ask-one and ask-two", mark.Ref)
			}
		}
		if task := startArchitect(t, pool, engine, root); !strings.HasPrefix(task, "kind: "+string(texts.ArchitectStart)) {
			t.Fatalf("the start after the catch-up = %q, want the architect-start alone: each decision told once", task)
		}
	})
}

// R1: the controller's parked-decision notice of a person's answer on a parked tree carries the
// first 4,000 UTF-16 units of an answer of 5,000, the count of the rest and the ask's reference.
func TestAParkedDecisionCarriesThePersonsWords(t *testing.T) {
	pool, apply := askRig(t)
	parked := askRoot()
	until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	parked.Phase, parked.Status, parked.LingerUntil = phase.Done, "backlog", &until
	seedIssue(t, pool, parked)
	seedQuestion(t, pool, "ask-q", 4)
	answer := strings.Repeat("a", 5000)
	apply("answered", intake.DispatchAskClosed{Key: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, State: "answered", Actor: askPerson,
		Mark: askSeq(8), Words: &record.ActWords{Text: answer, By: askPerson.Name(), At: resumeWordsAt}})
	var relayed []record.Notice
	for _, payload := range noticeRows(t, pool) {
		if notice := asNotice(payload); notice.Kind == texts.ParkedDecision {
			relayed = append(relayed, notice)
		}
	}
	if len(relayed) != 1 {
		t.Fatalf("parked-decision notices %+v, want one", relayed)
	}
	notice := relayed[0]
	t.Logf("notice: ref %s, answer of %d bytes, words %+v", notice.Ref, len(notice.Answer), notice.Words)
	if notice.Answer != answer[:4000] || notice.Words["answer"].Rest != 1000 || notice.Ref != "dispatch://LEGION-208/ask/ask-q" {
		t.Fatalf("the notice carries %d bytes of the answer with %+v at %s, want the first 4000, rest 1000 and the ask's reference",
			len(notice.Answer), notice.Words["answer"], notice.Ref)
	}
}

// A catch-up that names one kept decision carries one act: R1 lets it carry up to 4,000 UTF-16
// units of it ("up to 4,000 characters of one act, or up to 1,000 characters of each act where it
// carries several", dispatch://INFRA-3686/spec). The kept-decisions task of the same one decision
// carries it whole (KeptDecisionsOf); the catch-up cuts an answer of 1,500 to its first 1,000.
func TestACatchUpWithOneKeptDecisionCarriesItUpToFourThousand(t *testing.T) {
	at := time.Date(2026, 11, 14, 22, 44, 10, 0, time.UTC)
	answer := strings.Repeat("w", 1500)
	kept := record.KeptDecisions{Decisions: []record.DecisionMark{{Ref: "dispatch://LEGION-208/ask/ask-q", At: at, Issue: "LEGION-208",
		Decision: record.DecisionAskAnswered, Actor: "user:alice", Answer: answer, AnswerAt: at}}}

	facts, words := keptFacts(kept)
	if got := facts.Decisions[0].Answer; got != answer {
		t.Errorf("the catch-up carries %d units of the one kept answer with rest %d, want all 1500: it carries one act",
			len(got), words["catch_up.kept.decisions[0].answer"].Rest)
	}
	if task := KeptDecisionsOf("LEGION-208", kept); task.Kept[0].Answer.Text != answer {
		t.Errorf("the kept-decisions task carries %d units of the one kept answer, want all 1500", len(task.Kept[0].Answer.Text))
	}
}

// R1 bounds "up to 1,000 characters of each act where it carries several", and only an act whose
// words the text carries counts (the architect's answer for dispatch://INFRA-3686): an approval
// carries none. A person approves the spec of a parked tree and answers one of its asks with 3,000
// characters, so the kept-decisions task and the catch-up that name both decisions carry the
// words of one act, and carry that answer whole, up to 4,000 (keptBound).
func TestKeptDecisionsOfOneWordedActCarryItUpToFourThousand(t *testing.T) {
	at := time.Date(2026, 11, 14, 22, 44, 10, 0, time.UTC)
	answer := strings.Repeat("w", 3000)
	kept := record.KeptDecisions{Decisions: []record.DecisionMark{
		{Ref: "dispatch://LEGION-208/spec", At: at, Issue: "LEGION-208", Decision: record.DecisionDesignApproved, Actor: "user:alice"},
		{Ref: "dispatch://LEGION-208/ask/ask-q", At: at, Issue: "LEGION-208", Decision: record.DecisionAskAnswered, Actor: "user:alice",
			Answer: answer, AnswerAt: at},
	}}

	got := KeptDecisionsOf("LEGION-208", kept).Kept[1].Answer
	if got == nil {
		t.Fatal("the kept-decisions task carries no block of the answer")
	}
	if got.Text != answer || got.Rest != 0 {
		t.Errorf("the kept-decisions task carries %d units of the answer with rest %d, want all 3000: it carries the words of one act",
			len(got.Text), got.Rest)
	}
	facts, words := keptFacts(kept)
	if carried := facts.Decisions[1].Answer; carried != answer {
		t.Errorf("the catch-up carries %d units of the answer with rest %d, want all 3000: it carries the words of one act",
			len(carried), words["catch_up.kept.decisions[1].answer"].Rest)
	}
}

// R2 has a daemon text name each Dispatch item by its dispatch:// link. A root's catch-up states
// each design-gate step it holds, and names the step's document by the bare artifact id alone
// (catch_up.gates[0].artifact), where the root architect's start task names the same document as
// dispatch://KEY/spec (gateSteps).
func TestACatchUpNamesItsGateDocumentByALink(t *testing.T) {
	const artifact = "0b8f1c2e-5d6a-4f7b-9c3d-2e1f0a9b8c7d"
	pool := migratedPool(t)
	root := askRoot()
	seedIssue(t, pool, root)
	seedGate(t, pool, record.DesignGate{Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: artifact, LatestVersion: 2})

	var facts texts.CatchUpFacts
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		facts, err = treeCatchUp(context.Background(), tx, record.NewStore(), []record.DesignStep{record.DesignStepSpec}, root)
		return err
	})
	payload, err := json.Marshal(texts.CatchUpRecord{NoticeHead: texts.NoticeHead{Kind: texts.CatchUp, Issue: root.Key}, CatchUp: &facts})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "dispatch://"+root.Key+"/spec") {
		t.Errorf("the catch-up names the spec step's document by no dispatch:// link: %s", payload)
	}
}

// R2 has a daemon text name each Dispatch item by its dispatch:// link. A kept approval names the
// step whose document a person approved, the version approved and the version reached since, and
// names that document by its step word alone (approval_step: spec in the kept-decisions task,
// kept.step in the catch-up), where the catch-up's gates name the same document by its link.
func TestAKeptApprovalNamesItsDocumentByALink(t *testing.T) {
	kept := record.KeptDecisions{KeptApproval: record.KeptApproval{Step: record.DesignStepSpec, Approved: 2, Latest: 3}}
	if text := KeptDecisionsTask("LEGION-208", kept); !strings.Contains(text, "dispatch://LEGION-208/spec") {
		t.Errorf("the kept-decisions task names the approved document by its step alone, and by no dispatch:// link:\n%s", text)
	}
}
