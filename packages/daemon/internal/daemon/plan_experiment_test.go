package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/experiment"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The whole daemon draws the group of each new issue of a project that declares an experiment
// whose arms set `plan`, and the group sets how the issue is done: an issue of the arm `owner`
// takes its owner's plan as an issue of no experiment does, and one of the arm `default` runs the
// project's default steps, its owner's register_plan answered PLAN_NOT_TAKEN. The rig is the
// person's handover proof's (personHandoverRig): the real Dispatch server with its events on a
// real NATS, a real Postgres, and the daemon in process on the fake runtime, which its admission
// cap of ten lets take every issue at once. The daemon draws `random` from seeded(11), so the
// draws of a run are the seed's, in the order the issues are recorded.
//
// Each subtest creates ten labelled issues one at a time and hands each to Legion as the person's
// todo, waiting until the daemon has recorded it and launched its architect, so the order of the
// draws is the order of creation. The owner of each issue gives the plan `implementer reviewer`
// twice before its gate opens, and each issue's gate is registered and approved by the person. So
// the runs tell the groups apart issue by issue: an issue that takes its owner's plan starts at
// implementing, and one that runs the project's default steps starts at planning. Then each
// issue's row of the experiment, the phase its first step started in and the source of the plan
// its run walks are read from the daemon's database, and each issue is held to the run its own
// recorded group sets (ranAsTheirGroups):
//
//   - owner: mode on at the arm a, so ten rows a/owner; each issue's plan is taken at both sends
//     and its run starts at implementing;
//   - default: mode on at the arm b, so ten rows b/default; both sends of each owner's plan are
//     answered 409 PLAN_NOT_TAKEN, since a refusal leaves the fact unspent, and each issue runs
//     the default steps from planning;
//   - shares: mode random, a and b of weight 1 each, so the ten recorded arms, in the order of
//     creation, are the ten arms experiment.Draw gives over a fresh seeded(11), both arms are
//     drawn, and each issue runs as its own arm says; its control swaps one issue's recorded
//     group, and the check must then fail;
//   - children: the settings of shares, one root handed to Legion and ten children put to todo
//     under it one at a time: admission draws the root and the workflow draws each child it
//     records, from the one source, so the eleven arms are the seed's first eleven draws;
//   - control-no-experiment: the settings of shares with no experiment, so no issue holds a row
//     of planning, the comparison of shares fails, and each issue runs as with no experiment: it
//     takes its owner's plan.
func TestTheExperimentSetsHowEachIssueIsDone(t *testing.T) {
	t.Parallel()
	t.Run("owner", func(t *testing.T) {
		issues := tenIssuesUnder(t, []config.Experiment{planningExperiment(config.ExperimentOn, "a")})
		for _, issue := range issues {
			if !issue.drawn || issue.variant != "a" || issue.plan != string(config.PlanGroupOwner) {
				t.Fatalf("%s holds the row %+v of the experiment planning; want the arm a, whose plan is owner", issue.key, issue)
			}
		}
		if err := ranAsTheirGroups(issues); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("default", func(t *testing.T) {
		issues := tenIssuesUnder(t, []config.Experiment{planningExperiment(config.ExperimentOn, "b")})
		for _, issue := range issues {
			if !issue.drawn || issue.variant != "b" || issue.plan != string(config.PlanGroupDefault) {
				t.Fatalf("%s holds the row %+v of the experiment planning; want the arm b, whose plan is default", issue.key, issue)
			}
		}
		if err := ranAsTheirGroups(issues); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("shares", func(t *testing.T) {
		declared := planningExperiment(config.ExperimentRandom, "")
		issues := tenIssuesUnder(t, []config.Experiment{declared})
		if err := drawnAsTheSeed(issues, declared, 11); err != nil {
			t.Fatal(err)
		}
		arms := map[string]int{}
		for _, issue := range issues {
			arms[issue.variant]++
		}
		if arms["a"] == 0 || arms["b"] == 0 {
			t.Fatalf("the seed drew the arms %v for the ten issues; want both arms drawn, so each group's run is read", arms)
		}
		if err := ranAsTheirGroups(issues); err != nil {
			t.Fatal(err)
		}
		// Control: the same runs, with one issue's recorded group swapped to the other arm's, must
		// fail the check, so the check reads each issue's run against its own group. The issue is
		// the first after the first of the arm a, or else of the arm b.
		swapped := slices.Clone(issues)
		k := swappable(swapped)
		if k < 0 {
			t.Fatalf("no issue after the first to swap among %+v", issues)
		}
		swapped[k].variant, swapped[k].plan = otherArm(swapped[k])
		if err := ranAsTheirGroups(swapped); err == nil {
			t.Fatalf("the check passed with %s's group swapped from %q to %q; want it to fail, since the runs tell each issue's group apart", issues[k].key, issues[k].plan, swapped[k].plan)
		}
		t.Logf("the ten issues were drawn %v, the seed's own draws in the order of creation", arms)
	})

	// Control, run by hand: with the seam taken out (engineCfg.ExperimentSource in openWorkflow),
	// the engine draws from experiment.CryptoSource, and so does admission, which draws with the
	// engine's source; the comparison fails on the first issue the two disagree on.
	t.Run("children", func(t *testing.T) {
		declared := planningExperiment(config.ExperimentRandom, "")
		issues := childrenUnder(t, declared)
		if err := drawnAsTheSeed(issues, declared, 11); err != nil {
			t.Fatal(err)
		}
		t.Logf("the root %s and its ten children were drawn %+v, the seed's own draws in the order of creation", issues[0].key, issues)
	})

	t.Run("control-no-experiment", func(t *testing.T) {
		issues := tenIssuesUnder(t, nil)
		err := drawnAsTheSeed(issues, planningExperiment(config.ExperimentRandom, ""), 11)
		if err == nil {
			t.Fatal("the comparison of the recorded arms with the seed's draws passed for a project that declares no experiment; want it to fail, since no issue holds a row of planning")
		}
		for _, issue := range issues {
			if issue.drawn {
				t.Fatalf("%s holds the row %+v of the experiment planning, which the project does not declare", issue.key, issue)
			}
		}
		if err := ranAsTheirGroups(issues); err != nil {
			t.Fatal(err)
		}
		t.Logf("with no experiment declared, the comparison failed: %v", err)
	})
}

// seeded is a random source of the shape a draw takes (experiment.Draw): each value is the next
// of one PCG seeded with seed, below the bound asked. The daemon hands its one source to its
// admission and its workflow, which draw on goroutines of their own, and a PCG is not safe to
// share, so one mutex holds it.
func seeded(seed uint64) func(uint64) (uint64, error) {
	var mu sync.Mutex
	values := rand.New(rand.NewPCG(seed, seed))
	return func(bound uint64) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		return values.Uint64N(bound), nil
	}
}

// planningExperiment is the experiment planning, drawn for each issue at mode: its arm a runs
// each issue as its owner gives it (plan owner), and its arm b runs the project's default steps
// (plan default), one weight each. onVariant is the arm mode on gives every issue.
func planningExperiment(mode config.ExperimentMode, onVariant string) config.Experiment {
	return config.Experiment{Name: "planning", Mode: mode, Unit: config.ExperimentIssue, OnVariant: onVariant,
		Variants: []config.Variant{
			{Name: "a", Weight: 1, Plan: config.PlanGroupOwner},
			{Name: "b", Weight: 1, Plan: config.PlanGroupDefault},
		}}
}

// experimentTitles are the titles of the ten issues each subtest creates, in the order it creates
// them. Dispatch refuses a new issue whose title near-duplicates one of its project, so no two
// share a word.
var experimentTitles = [...]string{
	"Ledger reconciles nightly",
	"Billing exports quarterly totals",
	"Search ranks fresher pages",
	"Onboarding emails welcome newcomers",
	"Cache evicts stale entries",
	"Uploads resume after drops",
	"Audit trail captures logins",
	"Dashboard charts weekly revenue",
	"Webhooks retry failed deliveries",
	"Profiles show avatar thumbnails",
}

// ownersPlan is the plan the owner of each issue of a subtest gives: one task whose steps are
// an implementer's and then a reviewer's, so a run that takes it starts at implementing, where the
// project's default steps start at planning.
const ownersPlan = ":::plan_task{#a steps=\"implementer reviewer\"}\nBuild the widget.\n:::\n"

// drawnIssue is what one issue of a subtest came to.
type drawnIssue struct {
	key string
	// drawn is whether the issue holds a row of the experiment planning, and variant and plan are
	// that row's arm and its plan.
	drawn         bool
	variant, plan string
	// answers are the daemon's answers to the two sends of the owner's register_plan.
	answers []planAnswer
	// firstStep is the phase the issue's run started its work in once its gate opened, and source
	// the source of the plan that run walks (record.PlanSourceOwner or record.PlanSourceDefault).
	firstStep phase.Phase
	source    string
}

// planAnswer is one answer of the daemon to register_plan: its status, and its code and sentence
// where it refused.
type planAnswer struct {
	status  int
	failure api.Failure
}

// tenIssuesUnder boots the daemon at a project that declares experiments, drawing `random` from
// seeded(11), takes ten issues through to the first step of each run, and answers what each came
// to, in the order of creation.
func tenIssuesUnder(t *testing.T, experiments []config.Experiment) []drawnIssue {
	t.Helper()
	r := newPersonHandoverRig(t, len(experimentTitles))
	r.cfg.Projects[r.cfg.Project] = config.Project{Experiments: experiments}
	r.experimentSource = seeded(11)
	r.boot()
	issues := make([]drawnIssue, len(experimentTitles))
	for i, title := range experimentTitles {
		created := r.create(asSession, title, map[string]any{"labels": []string{"legion"}})
		grant := r.admit(created.Key)
		issues[i].key = created.Key
		document := r.uploadDocument(created.Key, "plan.md", ownersPlan)
		issues[i].answers = []planAnswer{r.givePlan(created.Key, document, grant), r.givePlan(created.Key, document, grant)}
		r.registerGate(created.Key, created.PrimaryArtifactID, grant)
		r.approve(created.PrimaryArtifactID)
		settled(t, r.d, "start the first step of "+created.Key, func(s api.State) bool {
			started := s.Issues[created.Key].Phase
			return started != "" && started != phase.Admitted
		})
	}
	for i := range issues {
		r.readDrawn(&issues[i])
	}
	t.Logf("the ten issues came to %+v", issues)
	return issues
}

// childrenUnder boots the daemon at a project that declares declared, drawing `random` from
// seeded(11), hands one root to Legion as the person's todo, and then puts ten children under it
// to todo, one at a time, each by the person, waiting until the workflow has recorded each. The
// root is drawn by admission and each child by the workflow, as it records a todo child under a
// live tree, so the root takes the seed's first draw and the children the next ten. It answers
// the root and then the children, in the order of creation, each with its row of planning.
func childrenUnder(t *testing.T, declared config.Experiment) []drawnIssue {
	t.Helper()
	r := newPersonHandoverRig(t, len(experimentTitles)+1)
	r.cfg.Projects[r.cfg.Project] = config.Project{Experiments: []config.Experiment{declared}}
	r.experimentSource = seeded(11)
	r.boot()
	root := r.create(asSession, "Platform migration umbrella", map[string]any{"labels": []string{"legion"}})
	r.admit(root.Key)
	issues := []drawnIssue{{key: root.Key}}
	for _, title := range experimentTitles {
		child := r.create(asPerson, title, map[string]any{"labels": []string{"legion"}, "parent": root.Key})
		r.setStatus(child.Key, "todo")
		settled(t, r.d, "record the child "+child.Key, func(s api.State) bool {
			recorded, ok := s.Issues[child.Key]
			return ok && recorded.Tree == root.Key
		})
		issues = append(issues, drawnIssue{key: child.Key})
	}
	for i := range issues {
		r.readGroup(&issues[i])
	}
	return issues
}

// givePlan gives Legion the plan in version 1 of document as key's owner, as the architect's
// register_plan does, and answers what the daemon answered. Dispatch settles an uploaded version
// in its own time, so an answer that Dispatch does not show the version yet is asked again.
func (r *personHandoverRig) givePlan(key, document string, grant func() string) planAnswer {
	r.t.Helper()
	var answer planAnswer
	testwait.Eventually(r.t, "Dispatch to show version 1 of "+document+" to register_plan on "+key, func() bool {
		status, body := r.d.request(http.MethodPost, "/legion/v1/plans/register", api.PlanRegisterRequest{
			GrantID: grant(), Issue: key, ArtifactID: document, Version: 1,
		}, false)
		answer = planAnswer{status: status}
		if status != http.StatusOK {
			if err := json.Unmarshal(body, &answer.failure); err != nil {
				r.t.Fatalf("decode the refusal of the plan of %s from %s: %v", key, body, err)
			}
		}
		return answer.failure.Code != "VERSION_NOT_IN_DISPATCH"
	})
	return answer
}

// readDrawn reads, from the daemon's database, issue's row of the experiment planning
// (readGroup), the phase its run is at and the source of the plan in force at its generation.
func (r *personHandoverRig) readDrawn(issue *drawnIssue) {
	r.t.Helper()
	ctx := r.t.Context()
	r.readGroup(issue)
	var step string
	if err := r.pool.QueryRow(ctx, `select phase from issues where key = $1`, issue.key).Scan(&step); err != nil {
		r.t.Fatalf("read the phase of %s: %v", issue.key, err)
	}
	issue.firstStep = phase.Phase(step)
	if err := r.pool.QueryRow(ctx, `select source from issue_plans
		where issue = $1 and generation = (select generation from issues where key = $1)
		order by version desc limit 1`, issue.key).Scan(&issue.source); err != nil {
		r.t.Fatalf("read the source of the plan of %s: %v", issue.key, err)
	}
}

// readGroup reads issue's row of the experiment planning from the daemon's database: drawn is
// whether it holds one, and variant and plan are its arm and the arm's plan.
func (r *personHandoverRig) readGroup(issue *drawnIssue) {
	r.t.Helper()
	err := r.pool.QueryRow(r.t.Context(), `select variant, plan from experiment_assignments where issue_key = $1 and experiment = 'planning'`,
		issue.key).Scan(&issue.variant, &issue.plan)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		r.t.Fatalf("read the row of the experiment planning of %s: %v", issue.key, err)
	default:
		issue.drawn = true
	}
}

// drawnAsTheSeed is nil where the arm each issue recorded for declared, in the order of creation,
// is the arm experiment.Draw gives over a fresh seeded(seed), and otherwise names the first issue
// whose arm differs or which holds no row of declared.
func drawnAsTheSeed(issues []drawnIssue, declared config.Experiment, seed uint64) error {
	source := seeded(seed)
	for _, issue := range issues {
		want, err := experiment.Draw(declared, source)
		if err != nil {
			return err
		}
		if !issue.drawn {
			return fmt.Errorf("%s holds no row of the experiment %s, where the source seeded %d draws the arm %s for it", issue.key, declared.Name, seed, want)
		}
		if issue.variant != want {
			return fmt.Errorf("%s was drawn into the arm %s of the experiment %s, where the source seeded %d draws the arm %s for it", issue.key, issue.variant, declared.Name, seed, want)
		}
	}
	return nil
}

// ranAsTheirGroups is nil where each issue ran as its own recorded group sets, and otherwise names
// the first issue that did not. Each issue's owner gave its plan twice. An issue of the group
// default runs the project's default steps, from planning, on a plan of the source default, and
// its owner was answered PLAN_NOT_TAKEN at both sends. Any other issue, of the group owner or of
// none, runs as with no experiment: its owner's plan is taken at both sends, and its run starts at
// implementing on a plan of the source owner.
func ranAsTheirGroups(issues []drawnIssue) error {
	for _, issue := range issues {
		wantStep, wantSource := phase.Implementing, record.PlanSourceOwner
		want := planAnswer{status: http.StatusOK}
		if issue.plan == string(config.PlanGroupDefault) {
			wantStep, wantSource = phase.Planning, record.PlanSourceDefault
			refusal := intake.PlanNotTaken(issue.key, record.Assignment{Experiment: "planning", Variant: issue.variant, Plan: issue.plan})
			want = planAnswer{status: refusal.Status, failure: api.Failure{Code: refusal.Code, Error: refusal.Message}}
		}
		if issue.firstStep != wantStep || issue.source != wantSource {
			return fmt.Errorf("%s, of the group %q, started at %s on a plan of the source %s; want %s on a plan of the source %s",
				issue.key, issue.plan, issue.firstStep, issue.source, wantStep, wantSource)
		}
		if !slices.Equal(issue.answers, []planAnswer{want, want}) {
			return fmt.Errorf("the two sends of the owner's plan of %s, of the group %q, were answered %+v; want %+v both times", issue.key, issue.plan, issue.answers, want)
		}
	}
	return nil
}

// swappable is the index of the first issue after the first whose group is owner, or else of the
// first after the first whose group is default, or -1 where neither stands.
func swappable(issues []drawnIssue) int {
	for _, plan := range []config.PlanGroup{config.PlanGroupOwner, config.PlanGroupDefault} {
		for k := 1; k < len(issues); k++ {
			if issues[k].plan == string(plan) {
				return k
			}
		}
	}
	return -1
}

// otherArm is the arm and the group of planning that issue was not drawn into.
func otherArm(issue drawnIssue) (variant, plan string) {
	if issue.plan == string(config.PlanGroupOwner) {
		return "b", string(config.PlanGroupDefault)
	}
	return "a", string(config.PlanGroupOwner)
}
