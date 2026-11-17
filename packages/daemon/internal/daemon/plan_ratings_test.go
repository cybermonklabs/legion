// The whole daemon writes the plan each run of an issue runs from and the rating its retro gives
// that plan onto the real Dispatch of this package's tests (startScratchDispatchOn), and a reader
// lists the finished issues of the project with GET /api/v1/projects/{key}/plans, the read
// `dispatch plans --project` makes.
//
// The walks are the step records' (stepRecordsRig, step_records_test.go): the whole daemon booted
// over the package's broker, the real store, the real outbox and the real Dispatch client, and each
// issue walked from planning to done through the transports production has. What is stood in for
// is outside the records: GitHub's REST API (stepRecordsGitHub), and the panes, which the fake
// runtime launches and the test plays.
package daemon

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// v-records, the read by project, in the form this slice can run: three issues of one project run
// to done with no plan of their owner, the first with one failed review, and each retro rates the
// plan, 3, 4 and 5, with a reason. The read lists the three, each with the default plan, its one
// rating with its reason, its count of failed steps (1, 0 and 0), and its time: from the earliest
// start to the latest end of its records as GET /api/v1/issues/{key}/steps lists them, to the
// second. The control deletes the failed review's record from Dispatch's store, its node and its
// tuples, and the same assertion must then fail on the first issue's failed steps.
func TestTheReadOfAProjectHoldsEachRetrosRating(t *testing.T) {
	r := newStepRecordsRigWith(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 3)
	want := r.walkThreeRated()
	r.waitPlans("the three finished issues, each rated", want)

	t.Run("control", func(t *testing.T) {
		r.deleteStepRecord(want[0].steps.find("step", 4, 1).ID)
		read, err := r.readPlans()
		if err != nil {
			t.Fatalf("read the project's plans after the delete: %v", err)
		}
		err = plansHold(read, want)
		if wanted := failedStepsSay(want[0].issue, 0, 1); err == nil || !strings.Contains(err.Error(), wanted) {
			t.Fatalf("the assertion over a read without the failed review's record = %v, want an error saying %q", err, wanted)
		}
	})
}

// A plugin of the release before PlanRatingContract completes its retro with no rating, and the
// daemon takes it and writes no rating. The case is the one a deploy meets: the implementer is in
// its retro's turn when the daemon of the release before stops, and the daemon of this release
// boots on the store it left, where the claim records the contract its plugin registered,
// PlanRatingContract-1. This daemon relaunches a stale claim onto its own plugin, but it holds the
// relaunch of a claim in a turn for that turn's end and serves the turn until then
// (refreshStalePlugins, supervise's holdStaleRelaunch, api's staleClaim). So the old plugin's
// completion of the retro, which carries no rating, is taken: the issue moves to merging, the
// relaunch comes at the turn's end, the walk ends at done, and the read by project lists the issue
// with no rating. The merge's record ends after the retro's completion, and a lane runs an issue's
// records one at a time in the order they are due (outbox_lanes.go), so a rating row the
// completion had written would have run before the read holds the seven records.
//
// Before the deploy, the same completion from the same claim at this release's contract is
// refused PLAN_RATING_REQUIRED, so the contract the claim records is what decides, and the
// daemon's own view of the claim after the boot must read the old contract.
func TestAnOldPluginsRetroCompletesWithNoRatingAcrossTheDeploy(t *testing.T) {
	r := newStepRecordsRig(t)
	r.plan()
	r.implement()
	r.test()
	r.review("APPROVED", phase.Accepting)
	r.accept()
	implementer := r.worker(claim.RoleImplementer)
	implementer.take()
	if status, body := r.complete(implementer, r.retroCompletion(0, "")); status != http.StatusBadRequest || !strings.Contains(body, "PLAN_RATING_REQUIRED") {
		t.Fatalf("the retro's completion with no rating from the implementer at plugin contract %d = %d %s, want 400 PLAN_RATING_REQUIRED",
			r.d.claim(implementer.token).PluginContract, status, body)
	}

	old := api.PlanRatingContract - 1
	r.restartWith(func() {
		tag, err := r.st.Pool().Exec(context.Background(), "update claims set plugin_contract = $1 where token = $2", old, string(implementer.token))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("record %s at plugin contract %d, as the daemon of the release before left it: %d rows, %v", implementer.token, old, tag.RowsAffected(), err)
		}
	})
	implementer.waitState(supervise.StateWorking)
	if got := r.d.claim(implementer.token).PluginContract; got != old {
		t.Fatalf("the booted daemon reads %s at plugin contract %d, want %d: this case proves nothing", implementer.token, got, old)
	}

	r.finish(implementer, "", r.retroCompletion(0, ""), phase.Merging)
	// The stale claim's relaunch, which the boot held for the turn's end.
	r.awaitLaunch(implementer.token)
	r.merge()
	var steps stepRecordsRead
	r.waitSteps("the seven records of the walk whose retro gave no rating", func(got stepRecordsRead) error { steps = got; return stepsHold(got, stepRecordsSeven) })
	r.waitPlans("the finished issue whose retro gave no rating", []ratedWant{{issue: r.key, steps: steps}})
}

// ratedWant is what the read by project must list of one issue: its rating and reason (none for
// a rating of 0), its count of failed steps, and its records as GET steps read them once its walk
// was done, from which its time and the id of its retro come.
type ratedWant struct {
	issue  string
	rating int
	reason string
	failed int
	steps  stepRecordsRead
}

// walkThreeRated walks the rig's three issues to done side by side, one step of each issue in
// turn, so no pane the daemon launches waits on the other issues' whole walks to register: the
// first issue's first review requests changes, and the retros rate the plan 3, 4 and 5. It answers
// what the read by project must list of each, its records read back once its walk is done.
func (r *stepRecordsRig) walkThreeRated() []ratedWant {
	r.t.Helper()
	want := []ratedWant{
		{issue: r.issues[0].key, rating: 3, reason: "The plan fit the work; the review sent it back once.", failed: 1},
		{issue: r.issues[1].key, rating: 4, reason: "The plan fit the work.", failed: 0},
		{issue: r.issues[2].key, rating: 5, reason: "The plan was the work, step for step.", failed: 0},
	}
	each := func(step func()) {
		for _, w := range want {
			r.on(w.issue)
			step()
		}
	}
	each(r.plan)
	each(r.implement)
	each(r.test)
	r.on(want[0].issue)
	r.review("CHANGES_REQUESTED", phase.Implementing)
	r.implement()
	r.test()
	each(func() { r.review("APPROVED", phase.Accepting) })
	each(r.accept)
	for _, w := range want {
		r.on(w.issue)
		r.rate(w.rating, w.reason)
	}
	each(r.merge)
	for place, w := range want {
		records := stepRecordsSeven
		if w.failed != 0 {
			records = stepRecordsTen
		}
		r.on(w.issue)
		r.waitSteps(fmt.Sprintf("the %d records of the walk of %s", len(records), w.issue), func(got stepRecordsRead) error {
			want[place].steps = got
			return stepsHold(got, records)
		})
	}
	return want
}

// projectPlansRead is the read of a project's finished issues, as Dispatch answers it.
type projectPlansRead struct {
	Project string              `json:"project"`
	Issues  []finishedIssueRead `json:"issues"`
}

// finishedIssueRead is one finished issue of the read.
type finishedIssueRead struct {
	Issue    string    `json:"issue"`
	Title    string    `json:"title"`
	ClosedAt time.Time `json:"closed_at"`
	Plan     *struct {
		Source     string  `json:"source"`
		ArtifactID *string `json:"artifact_id"`
		Version    *int    `json:"version"`
		Tasks      []struct {
			ID    string   `json:"id"`
			Steps []string `json:"steps"`
		} `json:"tasks"`
	} `json:"plan"`
	Ratings []struct {
		Rating  int        `json:"rating"`
		Reason  string     `json:"reason"`
		StepID  string     `json:"step_id"`
		EndedAt *time.Time `json:"ended_at"`
	} `json:"ratings"`
	StartedAt       *time.Time `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at"`
	DurationSeconds *int64     `json:"duration_seconds"`
	FailedSteps     int        `json:"failed_steps"`
}

// failedStepsSay is how plansHold names an issue whose count of failed steps is got, not want.
func failedStepsSay(issue string, got, want int) string {
	return fmt.Sprintf("%s lists failed_steps %d, want %d", issue, got, want)
}

// plansHold says whether read lists exactly the issues of want, newest close first, each closed,
// with the default plan, its rating with its reason on its retro's record (or none), its count of
// failed steps, and its time: the earliest start and the latest end of the records of its steps,
// and their difference in whole seconds. The error names the first issue that is missing or wrong.
func plansHold(read projectPlansRead, want []ratedWant) error {
	if len(read.Issues) != len(want) {
		return fmt.Errorf("the read lists %d finished issues, want %d", len(read.Issues), len(want))
	}
	for place := 1; place < len(read.Issues); place++ {
		if before, after := read.Issues[place-1], read.Issues[place]; after.ClosedAt.After(before.ClosedAt) {
			return fmt.Errorf("%s closed at %s is listed after %s closed at %s, want the newest close first", after.Issue, after.ClosedAt, before.Issue, before.ClosedAt)
		}
	}
	for _, w := range want {
		at := slices.IndexFunc(read.Issues, func(listed finishedIssueRead) bool { return listed.Issue == w.issue })
		if at < 0 {
			return fmt.Errorf("the read lists no %s", w.issue)
		}
		got := read.Issues[at]
		switch plan := got.Plan; {
		case got.ClosedAt.IsZero():
			return fmt.Errorf("%s is listed with no close", w.issue)
		case plan == nil:
			return fmt.Errorf("%s lists no plan, want the default plan", w.issue)
		case plan.Source != "default" || plan.ArtifactID != nil || plan.Version != nil || len(plan.Tasks) != 1 ||
			plan.Tasks[0].ID != "default" || !slices.Equal(plan.Tasks[0].Steps, workflow.DefaultSteps()):
			return fmt.Errorf("%s lists the plan %+v, want source default with no document and the one task default of %v", w.issue, *plan, workflow.DefaultSteps())
		}
		retro := w.steps.find("retro", 0, 1)
		switch {
		case w.rating == 0 && len(got.Ratings) != 0:
			return fmt.Errorf("%s lists the ratings %+v, want none", w.issue, got.Ratings)
		case w.rating != 0 && len(got.Ratings) != 1:
			return fmt.Errorf("%s lists the ratings %+v, want the one rating %d", w.issue, got.Ratings, w.rating)
		case w.rating != 0 && (got.Ratings[0].Rating != w.rating || got.Ratings[0].Reason != w.reason):
			return fmt.Errorf("%s lists the rating %d %q, want %d %q", w.issue, got.Ratings[0].Rating, got.Ratings[0].Reason, w.rating, w.reason)
		case w.rating != 0 && (got.Ratings[0].StepID != retro.ID || got.Ratings[0].EndedAt == nil || retro.EndedAt == nil ||
			!got.Ratings[0].EndedAt.Equal(*retro.EndedAt)):
			return fmt.Errorf("%s lists its rating on the record %s ended %v, want the retro's record %s ended %v", w.issue,
				got.Ratings[0].StepID, got.Ratings[0].EndedAt, retro.ID, retro.EndedAt)
		case got.FailedSteps != w.failed:
			return fmt.Errorf("%s", failedStepsSay(w.issue, got.FailedSteps, w.failed))
		}
		started, ended := w.steps.span()
		seconds := int64(ended.Sub(started) / time.Second)
		switch {
		case got.StartedAt == nil || got.EndedAt == nil || got.DurationSeconds == nil:
			return fmt.Errorf("%s lists started_at %v, ended_at %v and duration_seconds %v, want each", w.issue, got.StartedAt, got.EndedAt, got.DurationSeconds)
		case !got.StartedAt.Equal(started) || !got.EndedAt.Equal(ended) || *got.DurationSeconds != seconds:
			return fmt.Errorf("%s lists %s to %s, %d seconds, want %s to %s, %d seconds, from its records", w.issue, got.StartedAt, got.EndedAt,
				*got.DurationSeconds, started, ended, seconds)
		}
	}
	return nil
}

// span is the earliest start and the latest end of the records of read.
func (read stepRecordsRead) span() (started, ended time.Time) {
	for _, record := range read.records() {
		if record.StartedAt != nil && (started.IsZero() || record.StartedAt.Before(started)) {
			started = *record.StartedAt
		}
		if record.EndedAt != nil && record.EndedAt.After(ended) {
			ended = *record.EndedAt
		}
	}
	return started, ended
}

// readPlans is GET /api/v1/projects/{key}/plans of the run's project as a person of the project
// reads it, or the error that the read met.
func (r *stepRecordsRig) readPlans() (projectPlansRead, error) {
	var read projectPlansRead
	if err := r.personRead("/api/v1/projects/"+r.cfg.Project+"/plans", &read); err != nil {
		return projectPlansRead{}, err
	}
	return read, nil
}

// waitPlans waits for the read by project to hold want, and fails with plansHold's last answer.
func (r *stepRecordsRig) waitPlans(what string, want []ratedWant) {
	r.t.Helper()
	var last error
	if !testwait.Holds(stepRecordsWait, time.Second, func() bool {
		read, err := r.readPlans()
		if err != nil {
			last = err
			return false
		}
		last = plansHold(read, want)
		return last == nil
	}) {
		r.t.Fatalf("%s never reached the read of %s: %v", what, r.cfg.Project, last)
	}
}
