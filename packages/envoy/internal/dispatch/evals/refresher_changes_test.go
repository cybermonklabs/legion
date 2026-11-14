package evals

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/legionfacts"
)

// The checks of what each change touches (refresher_work.go's list, the oracle of
// dispatch://INFRA-2916's k12): each makes one change to one input after the refresher has walked
// every day, makes one pass, and reads back which days the pass wrote and which rows' updated_at
// moved (expectDays).

// deliveryDays reads every day of the delivery history as ReadDeliveryDay reads it.
func (r *refresherRig) deliveryDays(t *testing.T) []DeliveryDay {
	t.Helper()
	settings, err := delivery.GetSettings(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	var days []DeliveryDay
	for day := refresherToday.AddDate(0, 0, -5); day.Before(refresherToday); day = day.AddDate(0, 0, 1) {
		read, err := r.registry.ReadDeliveryDay(t.Context(), settings, day)
		if err != nil {
			t.Fatalf("read the delivery rows of %s: %v", day.Format(time.DateOnly), err)
		}
		days = append(days, read)
	}
	return days
}

// runStart is the UTC day the run of the history started on.
func runStart(t *testing.T, runs []delivery.DeliveryRun, id int64) time.Time {
	t.Helper()
	for _, run := range runs {
		if run.RunID == id {
			return utcMidnight(run.StartedAt)
		}
	}
	t.Fatalf("no run %d in the history", id)
	return time.Time{}
}

func line(family, project string, day time.Time) string {
	return family + " " + project + " " + day.Format(time.DateOnly)
}

// TestEachDeliveryChangeRewritesExactlyItsDays: over the delivery history, once every day is
// walked,
//   - a pull request whose issue moves to the other project rewrites its merge day in both
//     projects (the day eval_day_members held it under in the first) and the start day of the
//     deploy run that ships it, in both projects, since the run counts for the projects of its
//     pull requests;
//   - a pull request merged again a day earlier rewrites the day it left and the day it joined,
//     and the start day of the deploy that ships it now;
//   - a job of a deploy run that newly reaches production rewrites the run's start day and the
//     merge days of the pull requests it now ships (delivery.FirstShippingApply over the history's
//     applies and the new one);
//   - a session group, a version of a definition and a write of day rows rewrite no day.
func TestEachDeliveryChangeRewritesExactlyItsDays(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	main, other := deliveries.Project, deliveries.SecondProject
	days := r.deliveryDays(t)

	// A pull request of the main project, through its issue, that a deploy ships; one of another
	// repository, which no deploy tracks, so only the day eval_day_members holds names the day it
	// leaves; and one through its repository, after the history's first day.
	var moved, undeployed, redated dayPR
	var movedDay, undeployedDay, redatedDay time.Time
	for _, day := range days {
		for _, pr := range day.prs {
			switch {
			case pr.project != main:
			case pr.deploy == nil && !pr.via && pr.repo != deliveries.DeployRepo && undeployed.member == "":
				undeployed, undeployedDay = pr, day.Day
			case pr.deploy == nil:
			case !pr.via && moved.member == "":
				moved, movedDay = pr, day.Day
			case pr.via && redated.member == "" && day.Day.After(refresherToday.AddDate(0, 0, -4)):
				redated, redatedDay = pr, day.Day
			}
		}
	}
	if moved.member == "" || redated.member == "" || undeployed.member == "" {
		t.Fatalf("the history holds no deployed pull request of %s through an issue (%q), none through its repository after its first day (%q), or no undeployed one through an issue (%q)",
			main, moved.member, redated.member, undeployed.member)
	}

	// The history as the cases move it, which each case's oracle reads: every pull request's issue
	// and merge, the projects of the issues, and the deploys that reached production.
	issueProjects := map[string]string{}
	for project, keys := range deliveries.Issues {
		for _, key := range keys {
			issueProjects[key] = project
		}
	}
	prs := slices.Clone(deliveries.PullRequests)
	applies := slices.Clone(deliveries.Applies)
	at := func(member string) int {
		index := slices.IndexFunc(prs, func(pr delivery.DeliveryPullRequest) bool { return PullRequestMember(pr.Repo, pr.Number) == member })
		if index < 0 {
			t.Fatalf("no pull request %s in the history", member)
		}
		return index
	}
	// Every repository of the history is mapped to the main project.
	projectOf := func(pr delivery.DeliveryPullRequest) string {
		if pr.IssueKey != nil {
			if project, found := issueProjects[*pr.IssueKey]; found {
				return project
			}
		}
		return main
	}
	shipper := func(pr delivery.DeliveryPullRequest) *delivery.Apply {
		if pr.Repo != deliveries.DeployRepo {
			return nil
		}
		return delivery.FirstShippingApply(*pr.MergedAt, applies)
	}
	// runDayProjects is every project a deploy run's start day counts it for: the projects of the
	// pull requests it ships, or the deploy repository's where it ships none, and the deploy
	// repository's for its CI rows.
	runDayProjects := func(runID int64) map[string]bool {
		projects := map[string]bool{main: true}
		for _, pr := range prs {
			if apply := shipper(pr); apply != nil && apply.RunID == runID {
				projects[projectOf(pr)] = true
			}
		}
		return projects
	}
	// The deployed pull request to move and the one to merge again a day earlier are two that one
	// deploy ships: once the first counts for the other project that deploy counts for both, and
	// the second's earlier merge moves it to an earlier deploy, so the deploy it leaves is written
	// in the other project only because it shipped the second (eval_day_members.shipped_by).
	moved.member, redated.member = "", ""
	historyStart := refresherToday.AddDate(0, 0, -5)
	for _, a := range prs {
		deployA := shipper(a)
		if moved.member != "" || deployA == nil || a.IssueKey == nil || issueProjects[*a.IssueKey] != main {
			continue
		}
		for _, b := range prs {
			deployB := shipper(b)
			if moved.member != "" || b.Number == a.Number && b.Repo == a.Repo || projectOf(b) != main || deployB == nil || deployB.RunID != deployA.RunID {
				continue
			}
			earlier := b
			earlier.MergedAt = new(b.MergedAt.AddDate(0, 0, -1))
			if moves := shipper(earlier); moves != nil && moves.RunID != deployA.RunID && !utcMidnight(*earlier.MergedAt).Before(historyStart) {
				moved.member, movedDay = PullRequestMember(a.Repo, a.Number), utcMidnight(*a.MergedAt)
				redated.member, redatedDay = PullRequestMember(b.Repo, b.Number), utcMidnight(*b.MergedAt)
			}
		}
	}
	if moved.member == "" {
		t.Fatalf("no deploy of the history ships a pull request of %s through an issue and another that a day earlier ships with another deploy", main)
	}
	runDay := func(runID int64) time.Time { return runStart(t, deliveries.Runs, runID) }
	lines := func(day time.Time, projects ...map[string]bool) []string {
		set := map[string]bool{}
		for _, some := range projects {
			for project := range some {
				set[line(walkDelivery, project, day)] = true
			}
		}
		return slices.Sorted(maps.Keys(set))
	}
	union := func(sets ...[]string) []string {
		all := map[string]bool{}
		for _, set := range sets {
			for _, one := range set {
				all[one] = true
			}
		}
		return slices.Sorted(maps.Keys(all))
	}

	// The undeployed pull request moved to the other project: the day it merged on is written in
	// the project that counts it now and, through eval_day_members alone, in the one it left.
	before := r.dayRows(t)
	undeployedPR := prs[at(undeployed.member)]
	r.exec(t, "make an issue of "+other, `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ($1, $2, 901, 'Other work moved here', 'in_progress', '00000901', '{"kind":"user","id":"alice"}')`, other+"-901", other)
	r.exec(t, "move "+undeployed.member+" to "+other, `update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2`,
		undeployedPR.Repo, undeployedPR.Number, other+"-901")
	issueProjects[other+"-901"] = other
	undeployedPR.IssueKey = new(other + "-901")
	prs[at(undeployed.member)] = undeployedPR
	both := []string{line(walkDelivery, main, undeployedDay), line(walkDelivery, other, undeployedDay)}
	before = r.expectDays(t, "an undeployed pull request moved to "+other, before, both, both)

	// The deployed pull request moved to the other project, through a new issue of it.
	movedPR := prs[at(moved.member)]
	deploy := shipper(movedPR)
	if deploy == nil {
		t.Fatalf("%s is deployed by the day's read and by no apply of the history", moved.member)
	}
	deployedBefore := runDayProjects(deploy.RunID)
	r.exec(t, "make an issue of "+other, `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ($1, $2, 900, 'Work moved here', 'in_progress', '00000900', '{"kind":"user","id":"alice"}')`, other+"-900", other)
	r.exec(t, "move "+moved.member+" to "+other, `update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2`,
		movedPR.Repo, movedPR.Number, other+"-900")
	issueProjects[other+"-900"] = other
	movedPR.IssueKey = new(other + "-900")
	prs[at(moved.member)] = movedPR
	want := union(lines(movedDay, map[string]bool{main: true, other: true}), lines(runDay(deploy.RunID), deployedBefore, runDayProjects(deploy.RunID)))
	before = r.expectDays(t, "a pull request moved to "+other, before, want,
		[]string{line(walkDelivery, main, movedDay), line(walkDelivery, other, movedDay), line(walkDelivery, other, runDay(deploy.RunID))})

	// The pull request of the repository's project merged again a day earlier: the deploy that
	// ships it now is the first apply GitHub created after the new merge, and the one that shipped
	// it before no longer counts it among the pull requests it ships.
	redatedPR := prs[at(redated.member)]
	oldDeploy := shipper(redatedPR)
	shippedThen := runDayProjects(oldDeploy.RunID)
	newMerge := redatedPR.MergedAt.AddDate(0, 0, -1)
	redatedPR.MergedAt = &newMerge
	newDeploy := shipper(redatedPR)
	if newDeploy == nil || newDeploy.RunID == oldDeploy.RunID {
		t.Fatalf("%s merged a day earlier ships with %v, the run that shipped it before (%d): this case would not move its deploy", redated.member, newDeploy, oldDeploy.RunID)
	}
	deployedThen := runDayProjects(newDeploy.RunID)
	r.exec(t, "merge "+redated.member+" a day earlier", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour') where repo = $1 and number = $2
	`, redatedPR.Repo, redatedPR.Number, newMerge)
	prs[at(redated.member)] = redatedPR
	want = union(lines(redatedDay, map[string]bool{main: true}), lines(utcMidnight(newMerge), map[string]bool{main: true}),
		lines(runDay(newDeploy.RunID), deployedThen, runDayProjects(newDeploy.RunID)),
		lines(runDay(oldDeploy.RunID), shippedThen, runDayProjects(oldDeploy.RunID)))
	t.Logf("%s moves from run %d (started %s) to run %d (started %s)", redated.member, oldDeploy.RunID, runDay(oldDeploy.RunID).Format(time.DateOnly),
		newDeploy.RunID, runDay(newDeploy.RunID).Format(time.DateOnly))
	before = r.expectDays(t, "a pull request merged a day earlier", before, want,
		[]string{line(walkDelivery, main, redatedDay), line(walkDelivery, main, utcMidnight(newMerge))})

	// A deploy run of the outage, whose production job failed, now completes it: the run ships the
	// pull requests merged before its creation that it completes first.
	var run delivery.DeliveryRun
	var job delivery.DeliveryRunJob
	for _, candidate := range deliveries.Runs {
		if run.RunID != 0 || candidate.Kind != delivery.DeliveryRunKindDeploy || slices.ContainsFunc(applies, func(a delivery.Apply) bool { return a.RunID == candidate.RunID }) {
			continue
		}
		for _, j := range deliveries.Jobs[candidate.RunID] {
			if !slices.Contains(deliveries.Settings.ProductionJobNames, j.Name) || j.CompletedAt == nil || j.Conclusion == nil ||
				*j.Conclusion != delivery.DeliveryJobConclusionFailure {
				continue
			}
			// The first such run that ships a pull request once its job completes.
			completed := append(slices.Clone(applies), delivery.Apply{RunID: candidate.RunID, CompletedAt: *j.CompletedAt, CreatedAt: candidate.Created()})
			if slices.ContainsFunc(prs, func(pr delivery.DeliveryPullRequest) bool {
				apply := delivery.FirstShippingApply(*pr.MergedAt, completed)
				return pr.Repo == deliveries.DeployRepo && apply != nil && apply.RunID == candidate.RunID
			}) {
				run, job = candidate, j
			}
		}
	}
	if run.RunID == 0 {
		t.Fatal("the history holds no deploy run whose failed production job, once completed, ships a pull request")
	}
	// What a run's change touches, read off the history before and after it: the run's day in the
	// projects it counted for then and counts for now; and for every pull request it shipped then
	// or ships now, that pull request's merge day and the day of each run that ships it then and
	// now, in those runs' projects then and now.
	type shipping struct {
		by       map[string]int64
		projects map[int64]map[string]bool
	}
	snapshot := func() shipping {
		now := shipping{by: map[string]int64{}, projects: map[int64]map[string]bool{}}
		for _, pr := range prs {
			if apply := shipper(pr); apply != nil {
				now.by[PullRequestMember(pr.Repo, pr.Number)] = apply.RunID
			}
		}
		for _, candidate := range deliveries.Runs {
			if candidate.Kind == delivery.DeliveryRunKindDeploy {
				now.projects[candidate.RunID] = runDayProjects(candidate.RunID)
			}
		}
		return now
	}
	runOracle := func(id int64, then, now shipping) ([]string, int) {
		want := lines(runDay(id), then.projects[id], now.projects[id])
		touched := 0
		for _, pr := range prs {
			member := PullRequestMember(pr.Repo, pr.Number)
			if then.by[member] != id && now.by[member] != id {
				continue
			}
			touched++
			want = union(want, lines(utcMidnight(*pr.MergedAt), map[string]bool{projectOf(pr): true}))
			for _, other := range []int64{then.by[member], now.by[member]} {
				if other != 0 && other != id {
					want = union(want, lines(runDay(other), then.projects[other], now.projects[other]))
				}
			}
		}
		return want, touched
	}

	then := snapshot()
	r.exec(t, "complete the production job of run "+fmt.Sprint(run.RunID), `
		update delivery_run_jobs set conclusion = 'success' where repo = $1 and run_id = $2 and name = $3
	`, run.Repo, run.RunID, job.Name)
	applies = append(applies, delivery.Apply{RunID: run.RunID, CompletedAt: *job.CompletedAt, CreatedAt: run.Created()})
	want, shipped := runOracle(run.RunID, then, snapshot())
	t.Logf("run %d (started %s) now ships %d pull requests", run.RunID, runDay(run.RunID).Format(time.DateOnly), shipped)
	before = r.expectDays(t, "a deploy run that now reaches production", before, want, []string{line(walkDelivery, main, runDay(run.RunID))})

	// A deploy run that reached production and shipped pull requests stops reaching it (its
	// production job is run again and fails): the pull requests it shipped ship with the next
	// apply or wait, and their merge days move with them.
	var stopped delivery.Apply
	var stoppedJob delivery.DeliveryRunJob
	now := snapshot()
	for _, apply := range applies {
		// One that ships a pull request merged on an earlier day than the run started, so that
		// day is written only because the run shipped it (eval_day_members.shipped_by).
		if stopped.RunID != 0 || apply.RunID == run.RunID || !slices.ContainsFunc(prs, func(pr delivery.DeliveryPullRequest) bool {
			return now.by[PullRequestMember(pr.Repo, pr.Number)] == apply.RunID && utcMidnight(*pr.MergedAt).Before(runDay(apply.RunID))
		}) {
			continue
		}
		for _, j := range deliveries.Jobs[apply.RunID] {
			if slices.Contains(deliveries.Settings.ProductionJobNames, j.Name) && j.Conclusion != nil && *j.Conclusion == delivery.DeliveryJobConclusionSuccess {
				stopped, stoppedJob = apply, j
			}
		}
	}
	if stopped.RunID == 0 {
		t.Fatal("the history holds no apply that ships a pull request")
	}
	r.exec(t, "fail the production job of run "+fmt.Sprint(stopped.RunID), `
		update delivery_run_jobs set conclusion = 'failure' where repo = $1 and run_id = $2 and name = $3
	`, deliveries.DeployRepo, stopped.RunID, stoppedJob.Name)
	applies = slices.DeleteFunc(applies, func(a delivery.Apply) bool { return a.RunID == stopped.RunID })
	want, shipped = runOracle(stopped.RunID, now, snapshot())
	var mergeDays []string
	for _, pr := range prs {
		if now.by[PullRequestMember(pr.Repo, pr.Number)] == stopped.RunID {
			mergeDays = append(mergeDays, line(walkDelivery, projectOf(pr), utcMidnight(*pr.MergedAt)))
		}
	}
	t.Logf("run %d (started %s) stops shipping its %d pull requests, merged on %v", stopped.RunID, runDay(stopped.RunID).Format(time.DateOnly), shipped, mergeDays)
	before = r.expectDays(t, "a deploy run that stops reaching production", before, want, append(mergeDays, line(walkDelivery, main, runDay(stopped.RunID))))

	for _, kind := range []string{"session_group", "eval_versions", "eval_days"} {
		r.exec(t, "log a "+kind+" change", `select delivery_change_log($1, null, $2, $3, null, null)`, kind, main, EvalID(main, SlugDeployFrequency))
		before = r.expectDays(t, "a "+kind+" change", before, nil, nil)
	}
}

// memberPR is the repository and the number a pull request's row key names.
func memberPR(t *testing.T, member string) (string, int) {
	t.Helper()
	rest, ok := strings.CutPrefix(member, "pull_request:")
	repo, number, found := strings.Cut(rest, "#")
	var n int
	if _, err := fmt.Sscan(number, &n); !ok || !found || err != nil {
		t.Fatalf("%q is no pull request's row key", member)
	}
	return repo, n
}

// legionEval is the hand-written Legion history of project EVAL the Legion change checks run
// over: EVAL-1 and EVAL-2 close on the 2nd (EVAL-1 in acme/web, EVAL-2 in acme/api), EVAL-3 on
// the 3rd alone, EVAL-4 stays open, and EVAL-10 (in acme/api) is a filing of the 2nd by EVAL-1's
// architect; the first claim observation falls on the 1st.
type legionEval struct {
	rows    map[string]legionfacts.Row
	filing  legionfacts.Filing
	d1, d2  time.Time
	actor   string
	project string
}

// The days of the hand-written Legion history: the history runs from refresherToday's eighth day
// before it.
func legionHistoryDay(n int) time.Time {
	return refresherToday.AddDate(0, 0, n-9)
}

func (r *refresherRig) seedLegionEval(t *testing.T) *legionEval {
	t.Helper()
	history := &legionEval{rows: map[string]legionfacts.Row{}, d1: legionHistoryDay(2), d2: legionHistoryDay(3),
		actor: `{"kind":"system","id":"refresher-test"}`, project: "EVAL"}
	r.exec(t, "map the repositories", `insert into repo_projects (repo, project, created_by, is_default) values ('acme/web', 'EVAL', $1, false), ('acme/api', 'EVAL', $1, false)`, history.actor)
	for _, issue := range []struct {
		number   int
		repo     any
		priority any
	}{{1, "acme/web", nil}, {2, "acme/api", nil}, {3, "acme/web", nil}, {4, "acme/web", nil}, {10, "acme/api", 2}} {
		r.exec(t, fmt.Sprintf("the issue EVAL-%d", issue.number), `
			insert into issues (key, project_key, number, title, status, rank, repo, priority, created_by, created_at)
			values ($1, 'EVAL', $2, 'An issue', 'in_progress', $3, $4, $5, $6::jsonb, $7)
		`, fmt.Sprintf("EVAL-%d", issue.number), issue.number, fmt.Sprintf("%08d", issue.number), issue.repo, issue.priority, history.actor, legionHistoryDay(0))
	}
	r.exec(t, "the first claim observation", `
		insert into legion_observations (project, issue_key, field, value, observed_at) values ('EVAL', 'EVAL-1', 'claim.implementer', '"working"', $1)
	`, legionHistoryDay(1).Add(-time.Hour))
	fact := func(key string, closed *time.Time, hours float64) legionfacts.Row {
		started := legionHistoryDay(1)
		return legionfacts.Row{
			Project: "EVAL", Issue: key, OpenedAt: legionHistoryDay(0), ClosedAt: closed, RootKey: key, WorkHours: hours,
			StartedAt: &started, HoursByPhase: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{},
		}
	}
	for _, row := range []legionfacts.Row{
		fact("EVAL-1", new(history.d1.Add(10*time.Hour)), 6), fact("EVAL-2", new(history.d1.Add(12*time.Hour)), 4),
		fact("EVAL-3", new(history.d2.Add(9*time.Hour)), 3), fact("EVAL-4", nil, 2),
	} {
		history.rows[row.Issue] = row
	}
	r.writeFacts(t, history, "EVAL-1", "EVAL-2", "EVAL-3", "EVAL-4")
	history.filing = legionfacts.Filing{Project: "EVAL", Issue: "EVAL-10", FiledAt: history.d1.Add(8 * time.Hour), FilerRole: "architect",
		FilerIssue: "EVAL-1", FilerPhase: "design", Class: metrics.ClassInTreeAtDesign, RootKey: "EVAL-1"}
	if err := legionfacts.WriteFilings(t.Context(), r.database.Pool, legionfacts.FilingsWrite{Project: "EVAL", Filings: []legionfacts.Filing{history.filing}, Stated: []string{"EVAL-10"}}); err != nil {
		t.Fatalf("write the filing: %v", err)
	}
	return history
}

// writeFacts writes the stored facts of keys as history holds them, as the Legion facts consumer
// writes the rows of the issues its pass computed.
func (r *refresherRig) writeFacts(t *testing.T, history *legionEval, keys ...string) {
	t.Helper()
	var rows []legionfacts.Row
	for _, key := range keys {
		rows = append(rows, history.rows[key])
	}
	if _, err := legionfacts.WriteProject(t.Context(), r.database.Pool, legionfacts.ProjectWrite{Project: "EVAL", Rows: rows, Stated: keys}); err != nil {
		t.Fatalf("write the facts of %v: %v", keys, err)
	}
}

// writeIssueEvent writes one event of the issue, as a person's write of it.
func (r *refresherRig) writeIssueEvent(t *testing.T, issue, kind, payload string) {
	t.Helper()
	r.exec(t, "write a "+kind+" of "+issue, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1), $2, '{"kind":"user","id":"alice"}', $3::jsonb, false, now())
	`, issue, kind, payload)
}

// TestEachLegionChangeRewritesExactlyItsDays: over a hand-written Legion history of EVAL, once
// every day is walked,
//   - an issue that reopens rewrites the day it had closed on (eval_day_members' day), and its
//     close on a later day rewrites that day alone;
//   - an issue whose close moves from one day to another in one write rewrites both, and the day
//     it left, which holds no other input, becomes zero rows and logs an eval_days change naming
//     it, so a view of it wakes;
//   - a change of an open issue's facts rewrites no day;
//   - a filing whose class moves rewrites its filing day, and so does a write that moves the filed
//     issue's repository, which moves the repository the filing is cut by;
//   - a new P0 issue rewrites the project's unowned-p0 row of today, and a change of another issue
//     that is no P0 rewrites nothing;
//   - a reading of a logged eval rewrites no day.
func TestEachLegionChangeRewritesExactlyItsDays(t *testing.T) {
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.settle(t)
	r.pass(t)
	d3, d4 := legionHistoryDay(4), legionHistoryDay(5)
	before := r.dayRows(t)
	if _, n := seriesDays(before, "EVAL", SlugLegionWorkTime, "hours"); n[history.d1] != 2 || n[history.d2] != 1 || n[d3] != 0 {
		t.Fatalf("after the walk legion-work-time holds n %v, want 2 on %s, 1 on %s and a zero row on %s", n,
			history.d1.Format(time.DateOnly), history.d2.Format(time.DateOnly), d3.Format(time.DateOnly))
	}

	reopened := history.rows["EVAL-1"]
	reopened.ClosedAt = nil
	history.rows["EVAL-1"] = reopened
	r.writeFacts(t, history, "EVAL-1")
	before = r.expectDays(t, "EVAL-1 reopened", before, []string{line(walkLegion, "EVAL", history.d1)}, []string{line(walkLegion, "EVAL", history.d1)})

	reopened.ClosedAt = new(d3.Add(15 * time.Hour))
	history.rows["EVAL-1"] = reopened
	r.writeFacts(t, history, "EVAL-1")
	before = r.expectDays(t, "EVAL-1 closed again", before, []string{line(walkLegion, "EVAL", d3)}, []string{line(walkLegion, "EVAL", d3)})

	seq := r.latestSeq(t)
	moved := history.rows["EVAL-3"]
	moved.ClosedAt = new(d4.Add(9 * time.Hour))
	history.rows["EVAL-3"] = moved
	r.writeFacts(t, history, "EVAL-3")
	before = r.expectDays(t, "EVAL-3's close moved a day on", before,
		[]string{line(walkLegion, "EVAL", history.d2), line(walkLegion, "EVAL", d4)},
		[]string{line(walkLegion, "EVAL", history.d2), line(walkLegion, "EVAL", d4)})
	lost, zeroedRepos := 0, 0
	for _, row := range before {
		if familyOf(row) != walkLegion || !row.Day.Equal(history.d2) {
			continue
		}
		switch {
		case row.Repo == "":
			if row.N != 0 && !strings.HasPrefix(row.Series, "filed") {
				t.Errorf("%s holds n %d after its day lost its one closed issue, want a zero row", rowKey(row), row.N)
			}
			lost++
		case !strings.Contains(row.EvalID, SlugLegionSplitting):
			// A repository's row of that day had input, so it stays, as a zero row.
			if row.N != 0 {
				t.Errorf("%s holds n %d after its day lost its one closed issue, want a zero row", rowKey(row), row.N)
			}
			zeroedRepos++
		}
	}
	woke := slices.ContainsFunc(r.evalDayChanges(t, seq), func(change string) bool {
		return strings.HasPrefix(change, EvalID("EVAL", SlugLegionWorkTime)+" ") && strings.Contains(change, history.d2.Format(time.DateOnly))
	})
	if lost == 0 || zeroedRepos == 0 || !woke {
		t.Errorf("the day that lost its input holds %d all-repositories rows and %d repository rows, and an eval_days change naming it was logged: %t; want zero rows of both and a change",
			lost, zeroedRepos, woke)
	}

	open := history.rows["EVAL-4"]
	open.WorkHours = 5
	history.rows["EVAL-4"] = open
	r.writeFacts(t, history, "EVAL-4")
	before = r.expectDays(t, "an open issue's facts moved", before, nil, nil)

	history.filing.Class = metrics.ClassOutOfTree
	if err := legionfacts.WriteFilings(t.Context(), r.database.Pool, legionfacts.FilingsWrite{Project: "EVAL", Filings: []legionfacts.Filing{history.filing}, Stated: []string{"EVAL-10"}}); err != nil {
		t.Fatalf("write the filing: %v", err)
	}
	before = r.expectDays(t, "a filing's class moved", before, []string{line(walkLegion, "EVAL", history.d1)}, []string{line(walkLegion, "EVAL", history.d1)})

	r.exec(t, "move EVAL-10 to acme/web", `update issues set repo = 'acme/web' where key = 'EVAL-10'`)
	r.writeIssueEvent(t, "EVAL-10", "issue.updated", `{"key":"EVAL-10","priority":2,"status":"in_progress","changed":["repo"]}`)
	before = r.expectDays(t, "the filed issue's repository moved", before, []string{line(walkLegion, "EVAL", history.d1)}, []string{line(walkLegion, "EVAL", history.d1)})
	if got := r.count(t, `select count(*) from eval_days where eval_id = $1 and day = $2 and repo = 'acme/web' and series = $3 and n = 1`,
		EvalID("EVAL", SlugLegionSplitting), history.d1, "filed:"+metrics.ClassOutOfTree); got != 1 {
		t.Errorf("the filing's day holds %d acme/web rows of its class, want the filing cut by its issue's new repository", got)
	}

	r.exec(t, "open a P0 issue", `
		insert into issues (key, project_key, number, title, status, rank, priority, created_by)
		values ('EVAL-50', 'EVAL', 50, 'Broken', 'triage', '00000050', 0, '{"kind":"user","id":"alice"}')`)
	r.writeIssueEvent(t, "EVAL-50", "issue.created", `{"key":"EVAL-50","priority":0,"status":"triage","changed":["title","status","priority"]}`)
	before = r.expectDays(t, "a P0 issue opened", before, []string{line(SlugUnownedP0, "EVAL", refresherToday)}, []string{line(SlugUnownedP0, "EVAL", refresherToday)})
	if count := r.day(t, EvalID("EVAL", SlugUnownedP0), SeriesUnownedP0, "", refresherToday); count.LastValue == nil || *count.LastValue != 1 {
		t.Errorf("the unowned-p0 row of today holds %v, want 1", count.LastValue)
	}
	r.writeIssueEvent(t, "EVAL-4", "issue.updated", `{"key":"EVAL-4","status":"in_progress","changed":["title"]}`)
	before = r.expectDays(t, "another issue retitled", before, nil, nil)

	r.write(t, "build-minutes", alice, row(t, map[string]any{"series": "minutes", "row_kind": "run", "row_key": "run-1", "value": 3.0, "measured_at": r.now}))
	r.expectDays(t, "a reading of a logged eval", r.dayRows(t), nil, nil)
}

// TestARepoProjectsChangeReattributesWithinOnePass: once every day is walked, the deploy
// repository's mapping moves to the second project and LEGION gains a default repository, and the
// one pass after both writes moves every row they decide: the CI runs and the pull requests that
// name no issue count for the second project and no more for the first, whose days that lost
// their input hold zero rows, and LEGION's Legion rows are cut by the repository its issues now
// resolve to.
func TestARepoProjectsChangeReattributesWithinOnePass(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, legion := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	main, other := deliveries.Project, deliveries.SecondProject
	checks := `select coalesce(sum(n), 0) from eval_days where project_key = $1 and eval_id = $2 and series = $3 and repo = '' and day < $4`
	legionRepo := `select count(*) from eval_days where project_key = $1 and eval_id = $2 and repo = 'acme/legion' and n > 0`
	mainChecks := r.count(t, checks, main, EvalID(main, SlugPRCI), measures.SeriesMinutes, refresherToday)
	if mainChecks == 0 || r.count(t, checks, other, EvalID(other, SlugPRCI), measures.SeriesMinutes, refresherToday) != 0 ||
		r.count(t, legionRepo, legion.Project, EvalID(legion.Project, SlugLegionWorkTime)) != 0 {
		t.Fatalf("before the mapping moved, %s counts %d timed CI results and %s some, or %s holds acme/legion rows", main, mainChecks, other, legion.Project)
	}

	seq := r.latestSeq(t)
	r.exec(t, "map the deploy repository to "+other, `update repo_projects set project = $2 where repo = $1`, deliveries.DeployRepo, other)
	r.exec(t, "give "+legion.Project+" a default repository", `
		insert into repo_projects (repo, project, created_by, is_default) values ('acme/legion', $1, '{"kind":"user","id":"alice"}', true)`, legion.Project)
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	var logged []string
	for _, change := range changes {
		logged = append(logged, change.Kind+" "+change.Repo)
	}
	if !slices.Equal(logged, []string{"settings " + deliveries.DeployRepo, "settings acme/legion"}) {
		t.Errorf("the two mapping writes logged %v, want a settings change naming each repository", logged)
	}

	r.batch = 100
	report, next := r.pass(t)
	if next.Equal(r.now) {
		t.Errorf("the pass left a walk with days to build, want the whole history rebuilt in one pass")
	}
	if got := r.count(t, checks, other, EvalID(other, SlugPRCI), measures.SeriesMinutes, refresherToday); got != mainChecks {
		t.Errorf("after the pass %s counts %d timed CI results, want the %d %s counted before", other, got, mainChecks, main)
	}
	if got := r.count(t, checks, main, EvalID(main, SlugPRCI), measures.SeriesMinutes, refresherToday); got != 0 {
		t.Errorf("after the pass %s still counts %d timed CI results of a repository mapped elsewhere", main, got)
	}
	firstCheck := r.count(t, `select count(*) from eval_days where project_key = $1 and eval_id = $2 and series = $3 and repo = '' and n = 0 and day < $4`,
		main, EvalID(main, SlugPRCI), measures.SeriesMinutes, refresherToday)
	if firstCheck == 0 {
		t.Errorf("%s's CI days that lost their input hold no zero row", main)
	}
	via := r.count(t, `select coalesce(sum(via_repository), 0) from eval_days where project_key = $1 and eval_id = $2 and repo = ''`,
		other, EvalID(other, SlugMergeToProduction))
	if via == 0 {
		t.Errorf("after the pass %s counts no deployed pull request that reached it through the repository", other)
	}
	if got := r.count(t, legionRepo, legion.Project, EvalID(legion.Project, SlugLegionWorkTime)); got == 0 {
		t.Errorf("after the pass %s holds no Legion row cut by acme/legion, the repository its issues resolve to now", legion.Project)
	}
	families := map[string]int{}
	for day := range report.days {
		families[day.family+" "+day.project]++
	}
	t.Logf("one pass rewrote %v; %s now counts %d timed CI results and %d deployed pull requests through the repository, and %s %d zero CI days",
		families, other, mainChecks, via, main, firstCheck)
}

// TestARepositoryRowThatLosesItsLastInputBecomesAZeroRow: once every day is walked, every pull
// request one repository merged on one day is merged again a day earlier, so the repository's last
// pull request leaves that day. The one pass after it keeps each of the repository's rows of that
// day, in every project that held one, as a zero row (n 0 and a zero value, its updated_at moved),
// rather than deleting it, which would log no eval_days change and leave a view of that
// repository's numbers holding the old ones; it logs an eval_days change spanning that day for the
// eval of each; and it creates no repository row of a day that never held that repository's
// input. A row whose series has a first input day counts a zero as a value.
func TestARepositoryRowThatLosesItsLastInputBecomesAZeroRow(t *testing.T) {
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.settle(t)
	r.pass(t)
	// The third repository's pull requests are tracked by no deploy, so moving them moves the pull
	// request family's rows of the two days alone. The day is a day of the history after its first,
	// so the day before it is one of the history too.
	repo := deliveries.Repos[2]
	historyStart := refresherToday.AddDate(0, 0, -5)
	before := r.dayRows(t)
	var day time.Time
	var held []string
	for candidate := historyStart.AddDate(0, 0, 1); candidate.Before(refresherToday) && day.IsZero(); candidate = candidate.AddDate(0, 0, 1) {
		for _, row := range before {
			if row.Repo == repo && row.Day.Equal(candidate) && row.N > 0 {
				day = candidate
			}
		}
	}
	if day.IsZero() {
		t.Fatalf("no day of the history after its first holds a row of %s with input", repo)
	}
	for key, row := range before {
		if row.Repo == repo && row.Day.Equal(day) {
			held = append(held, key)
		}
	}
	slices.Sort(held)

	seq := r.latestSeq(t)
	r.exec(t, "merge "+repo+"'s pull requests of "+day.Format(time.DateOnly)+" a day earlier", `
		update delivery_pull_requests
		set merged_at = merged_at - interval '1 day', created_at = least(created_at, merged_at - interval '1 day' - interval '1 hour')
		where repo = $1 and merged_at >= $2 and merged_at < $3
	`, repo, day, day.AddDate(0, 0, 1))
	r.pass(t)
	after := r.dayRows(t)
	changes := r.evalDayChanges(t, seq)

	evalsZeroed := map[string]bool{}
	for _, key := range held {
		row, kept := after[key]
		switch {
		case !kept:
			t.Errorf("%s was deleted, want a zero row", key)
		case row.N != 0 || valueOr(row.Sum) != 0 || valueOr(row.Num) != 0 || valueOr(row.Den) != 0 || row.Max != nil ||
			len(row.Samples) != 0 || row.ViaRepository != 0 || slices.ContainsFunc(slices.Collect(maps.Values(row.Counts)), func(count float64) bool { return count != 0 }):
			t.Errorf("%s holds %s after its last input left, want a zero row", key, rowFacts(row))
		case before[key].N == 0:
			// A row already zero (a lead time of pull requests no deploy ships) is left as it is.
		case !row.UpdatedAt.After(before[key].UpdatedAt):
			t.Errorf("%s kept its updated_at %s, want it rewritten as a zero row", key, row.UpdatedAt)
		default:
			evalsZeroed[row.EvalID] = true
		}
	}
	for evalID := range evalsZeroed {
		if !slices.ContainsFunc(changes, func(change string) bool {
			first, last, found := strings.Cut(strings.TrimPrefix(change, evalID+" "), "..")
			return strings.HasPrefix(change, evalID+" ") && found && first <= day.Format(time.DateOnly) && last >= day.Format(time.DateOnly)
		}) {
			t.Errorf("no eval_days change of %s spans %s, where its rows became zero rows: %v", evalID, day.Format(time.DateOnly), changes)
		}
	}
	for key, row := range after {
		if _, was := before[key]; !was && row.Repo == repo && !row.Day.Equal(day.AddDate(0, 0, -1)) {
			t.Errorf("the pass created %s, a row of %s on a day its input never reached", key, repo)
		}
	}
	if len(held) == 0 || len(evalsZeroed) == 0 {
		t.Errorf("%s held %d rows of %s on %s and %d evals' rows became zero rows, want some of each", repo, len(held), repo, day.Format(time.DateOnly), len(evalsZeroed))
	}
	t.Logf("%s's %d rows of %s became zero rows in the evals %v; eval_days changes of the pass: %v",
		repo, len(held), day.Format(time.DateOnly), slices.Sorted(maps.Keys(evalsZeroed)), changes)
}
