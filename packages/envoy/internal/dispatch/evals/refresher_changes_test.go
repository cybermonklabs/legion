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

// deliveryDays reads every day of the delivery history as the refresher reads it (readDeliveryDays)
// with the deploy repository the settings name, the one repository of the history that holds runs,
// as the deploy repository: that repository's read of each day.
func (r *refresherRig) deliveryDays(t *testing.T) []DeliveryDay {
	t.Helper()
	settings, err := delivery.GetSettings(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	var days []DeliveryDay
	for day := refresherToday.AddDate(0, 0, -5); day.Before(refresherToday); day = day.AddDate(0, 0, 1) {
		reads, err := r.registry.readDeliveryDays(t.Context(), settings, []keptRepo{{name: settings.DeployRepo, deploys: true}}, []time.Time{measures.Midnight(day)}, nil)
		if err != nil {
			t.Fatalf("read the delivery rows of %s: %v", day.Format(time.DateOnly), err)
		}
		days = append(days, reads[0].days[1])
	}
	return days
}

// runStart is the UTC day the run of the history started on.
func runStart(t *testing.T, runs []delivery.DeliveryRun, id int64) time.Time {
	t.Helper()
	for _, run := range runs {
		if run.RunID == id {
			return measures.Midnight(run.StartedAt)
		}
	}
	t.Fatalf("no run %d in the history", id)
	return time.Time{}
}

func line(family, project string, day time.Time) string {
	return family + " " + project + " " + day.Format(time.DateOnly)
}

// ownerKeyOf is the owner key of the rows repo attributes where no attribution list decides their
// readers, the form owners.go documents: ':', repo, '|' and the sorted, distinct keys of the
// sharers' projects joined by ','. It is written out so an oracle does not read the key the code
// under test makes.
func ownerKeyOf(repo string, sharers ...string) string {
	return ":" + repo + "|" + strings.Join(slices.Compact(slices.Sorted(slices.Values(sharers))), ",")
}

// firstApply is the apply that first ships a pull request merged at merged: the earliest to
// complete (the lower run id of two that complete together) of the applies GitHub created strictly
// after the merge, nil where none was. It is written out, as delivery.FirstShippingApply states it,
// so an oracle does not run the code under test.
func firstApply(merged time.Time, applies []delivery.Apply) *delivery.Apply {
	var first *delivery.Apply
	for i := range applies {
		apply := &applies[i]
		if !apply.CreatedAt.After(merged) {
			continue
		}
		if first == nil || apply.CompletedAt.Before(first.CompletedAt) || apply.CompletedAt.Equal(first.CompletedAt) && apply.RunID < first.RunID {
			first = apply
		}
	}
	return first
}

// TestEachDeliveryChangeRewritesExactlyItsDays: over the delivery history, its pull requests'
// attribution inputs left unread so the issue each row names is its attribution
// (leaveInputsUnread), once every day is walked,
//   - a pull request whose issue moves to the other project rewrites its merge day in both
//     projects (the day eval_day_members held it under in the first), the start day of the
//     deploy run that ships it, in both projects, since the run counts for the projects of its
//     pull requests, and the start day of each of its PR-checks runs, in both projects, since
//     each counts for its pull request's project;
//   - a pull request merged again a day earlier rewrites the day it left and the day it joined,
//     and the start day of the deploy that ships it now;
//   - a job of a deploy run that newly reaches production rewrites the run's start day and the
//     merge days of the pull requests it now ships (delivery.FirstShippingApply over the history's
//     applies and the new one);
//   - a session group, a version of a definition and a write of day rows rewrite no day.
//
// What no issue attributes counts for its repository's owner key (owners.go, R15): a pull request
// that names no issue, and a deploy run that ships one or ships none, under the key whose sharers
// are the projects it counts for through issues. So a deploy run's day is rewritten in each owner
// key it counted for before or after the change where the key moved or the run's row in it did,
// and a key whose first input the change brings writes its zero rows through today, as one that no
// input counts for any more deletes every row it held (keyDays).
func TestEachDeliveryChangeRewritesExactlyItsDays(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.settle(t)
	r.pass(t)
	main, other := deliveries.Project, deliveries.SecondProject
	days := r.deliveryDays(t)

	// A pull request of the main project, through its issue, of another repository, which no
	// deploy tracks, so only the day eval_day_members holds names the day it leaves.
	var undeployed dayPR
	var undeployedDay time.Time
	for _, day := range days {
		for _, pr := range day.prs {
			if pr.project == main && pr.deploy == nil && !pr.via && pr.repo != deliveries.DeployRepo && undeployed.member == "" {
				undeployed, undeployedDay = pr, day.Day
			}
		}
	}
	if undeployed.member == "" {
		t.Fatalf("the history holds no undeployed pull request of %s through an issue", main)
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
	// projectOf is the project a pull request counts for through its issue, "" where it names no
	// issue the history holds.
	projectOf := func(pr delivery.DeliveryPullRequest) string {
		if pr.IssueKey != nil {
			return issueProjects[*pr.IssueKey]
		}
		return ""
	}
	// countsFor is what a pull request counts for (attribution.of): its issue's project, and its
	// repository's owner key where it names no issue the history holds.
	countsFor := func(pr delivery.DeliveryPullRequest) string {
		if project := projectOf(pr); project != "" {
			return project
		}
		return ownerKeyOf(pr.Repo)
	}
	shipper := func(pr delivery.DeliveryPullRequest) *delivery.Apply {
		if pr.Repo != deliveries.DeployRepo {
			return nil
		}
		return firstApply(*pr.MergedAt, applies)
	}
	runRepos := map[int64]string{}
	for _, run := range deliveries.Runs {
		runRepos[run.RunID] = run.Repo
	}
	// runCount is what a deploy run's start day counts it for (attribution.deploy): the projects
	// of the pull requests it ships through their issues, which its CI rows count for too; and,
	// where it ships a pull request no issue attributes or ships none, its repository's owner key
	// with those projects as its sharers, whose row of the day holds whether the run reached
	// production and those pull requests.
	type runCount struct {
		projects map[string]bool
		owner    string
		reached  bool
		unissued string
	}
	runCounts := func(runID int64) runCount {
		count := runCount{projects: map[string]bool{}, reached: slices.ContainsFunc(applies, func(a delivery.Apply) bool { return a.RunID == runID })}
		ships := false
		var unissued []string
		for _, pr := range prs {
			if apply := shipper(pr); apply == nil || apply.RunID != runID {
				continue
			}
			ships = true
			if project := projectOf(pr); project != "" {
				count.projects[project] = true
			} else {
				unissued = append(unissued, PullRequestMember(pr.Repo, pr.Number))
			}
		}
		if !ships || len(unissued) > 0 {
			slices.Sort(unissued)
			count.owner = ownerKeyOf(runRepos[runID], slices.Collect(maps.Keys(count.projects))...)
			count.unissued = strings.Join(unissued, " ")
		}
		return count
	}
	// The deployed pull request to move and the one to merge again a day earlier are two that one
	// deploy ships: once the first counts for the other project that deploy counts for both, and
	// the second's earlier merge moves it to an earlier deploy, so the deploy it leaves is written
	// in the other project only because it shipped the second (eval_day_members.shipped_by).
	var moved, redated dayPR
	var movedDay, redatedDay time.Time
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
			if moves := shipper(earlier); moves != nil && moves.RunID != deployA.RunID && !measures.Midnight(*earlier.MergedAt).Before(historyStart) {
				moved.member, movedDay = PullRequestMember(a.Repo, a.Number), measures.Midnight(*a.MergedAt)
				redated.member, redatedDay = PullRequestMember(b.Repo, b.Number), measures.Midnight(*b.MergedAt)
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
				if project != "" {
					set[line(walkDelivery, project, day)] = true
				}
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
	// runLines is the lines of a deploy run's start day day that a change writes, where the run
	// counted as then and counts as now (runCounts): each project of either, each owner key of
	// either where the key moved, and the key where the run's row in it moved.
	runLines := func(day time.Time, then, now runCount) []string {
		set := lines(day, then.projects, now.projects)
		switch {
		case then.owner != now.owner:
			for _, owner := range []string{then.owner, now.owner} {
				if owner != "" {
					set = append(set, line(walkDelivery, owner, day))
				}
			}
		case now.owner != "" && (then.reached != now.reached || then.unissued != now.unissued):
			set = append(set, line(walkDelivery, now.owner, day))
		}
		return union(set)
	}
	// counted reports whether an input of the history counts for the owner key key as it stands: a
	// deploy run whose day counts there, or a pull request that names no issue.
	counted := func(key string) bool {
		for _, run := range deliveries.Runs {
			if run.Kind == delivery.DeliveryRunKindDeploy && runCounts(run.RunID).owner == key {
				return true
			}
		}
		return slices.ContainsFunc(prs, func(pr delivery.DeliveryPullRequest) bool { return pr.MergedAt != nil && countsFor(pr) == key })
	}
	// keyDays is want, the lines a change writes, with the days of each owner key it names that the
	// change writes as the key's rows begin or end: a key that held no row before (before) writes
	// its zero rows from the first day want names of it through today, and one that no input counts
	// for any more (counted) deletes every row it held.
	keyDays := func(want []string, before map[string]DayRow) []string {
		firsts := map[string]time.Time{}
		for _, one := range want {
			rest := strings.TrimPrefix(one, walkDelivery+" ")
			at := strings.LastIndex(rest, " ")
			if key := rest[:at]; isOwner(key) {
				day, err := time.Parse(time.DateOnly, rest[at+1:])
				if err != nil {
					t.Fatalf("read the day of %q: %v", one, err)
				}
				if first, held := firsts[key]; !held || day.Before(first) {
					firsts[key] = day
				}
			}
		}
		all := slices.Clone(want)
		for key, first := range firsts {
			var held []string
			for _, row := range before {
				if row.ProjectKey == key {
					held = append(held, line(walkDelivery, key, row.Day))
				}
			}
			switch {
			case len(held) == 0:
				for day := first; !day.After(refresherToday); day = day.AddDate(0, 0, 1) {
					all = append(all, line(walkDelivery, key, day))
				}
			case !counted(key):
				all = append(all, held...)
			}
		}
		return union(all)
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
	deployedBefore := runCounts(deploy.RunID)
	r.exec(t, "make an issue of "+other, `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		values ($1, $2, 900, 'Work moved here', 'in_progress', '00000900', '{"kind":"user","id":"alice"}')`, other+"-900", other)
	r.exec(t, "move "+moved.member+" to "+other, `update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2`,
		movedPR.Repo, movedPR.Number, other+"-900")
	issueProjects[other+"-900"] = other
	movedPR.IssueKey = new(other + "-900")
	prs[at(moved.member)] = movedPR
	want := union(lines(movedDay, map[string]bool{main: true, other: true}), runLines(runDay(deploy.RunID), deployedBefore, runCounts(deploy.RunID)))
	// Its PR-checks runs count for its project, so each start day of one is written in both.
	mustMove := []string{line(walkDelivery, main, movedDay), line(walkDelivery, other, movedDay), line(walkDelivery, other, runDay(deploy.RunID))}
	for _, check := range deliveries.Runs {
		if check.Kind == delivery.DeliveryRunKindPRChecks && check.Repo == movedPR.Repo && check.PRNumber != nil && *check.PRNumber == movedPR.Number {
			checkDay := lines(measures.Midnight(check.StartedAt), map[string]bool{main: true, other: true})
			want = union(want, checkDay)
			// A run still going adds no result to the rows, which its move leaves as they were.
			if check.Conclusion != nil && (*check.Conclusion == delivery.DeliveryRunConclusionSuccess || *check.Conclusion == delivery.DeliveryRunConclusionFailure) {
				mustMove = union(mustMove, checkDay)
			}
		}
	}
	before = r.expectDays(t, "a pull request moved to "+other, before, keyDays(want, before), mustMove)

	// The pull request of the repository's project merged again a day earlier: the deploy that
	// ships it now is the first apply GitHub created after the new merge, and the one that shipped
	// it before no longer counts it among the pull requests it ships.
	redatedPR := prs[at(redated.member)]
	oldDeploy := shipper(redatedPR)
	shippedThen := runCounts(oldDeploy.RunID)
	newMerge := redatedPR.MergedAt.AddDate(0, 0, -1)
	redatedPR.MergedAt = &newMerge
	newDeploy := shipper(redatedPR)
	if newDeploy == nil || newDeploy.RunID == oldDeploy.RunID {
		t.Fatalf("%s merged a day earlier ships with %v, the run that shipped it before (%d): this case would not move its deploy", redated.member, newDeploy, oldDeploy.RunID)
	}
	deployedThen := runCounts(newDeploy.RunID)
	r.exec(t, "merge "+redated.member+" a day earlier", `
		update delivery_pull_requests set merged_at = $3, created_at = least(created_at, $3::timestamptz - interval '1 hour') where repo = $1 and number = $2
	`, redatedPR.Repo, redatedPR.Number, newMerge)
	prs[at(redated.member)] = redatedPR
	want = union(lines(redatedDay, map[string]bool{main: true}), lines(measures.Midnight(newMerge), map[string]bool{main: true}),
		runLines(runDay(newDeploy.RunID), deployedThen, runCounts(newDeploy.RunID)),
		runLines(runDay(oldDeploy.RunID), shippedThen, runCounts(oldDeploy.RunID)))
	// A change of the pull request names the start day of each of its PR-checks runs too, in its
	// project, whose rows its merge does not move.
	for _, check := range deliveries.Runs {
		if check.Kind == delivery.DeliveryRunKindPRChecks && check.Repo == redatedPR.Repo && check.PRNumber != nil && *check.PRNumber == redatedPR.Number {
			want = union(want, lines(measures.Midnight(check.StartedAt), map[string]bool{main: true}))
		}
	}
	t.Logf("%s moves from run %d (started %s) to run %d (started %s)", redated.member, oldDeploy.RunID, runDay(oldDeploy.RunID).Format(time.DateOnly),
		newDeploy.RunID, runDay(newDeploy.RunID).Format(time.DateOnly))
	before = r.expectDays(t, "a pull request merged a day earlier", before, keyDays(want, before),
		[]string{line(walkDelivery, main, redatedDay), line(walkDelivery, main, measures.Midnight(newMerge))})

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
				apply := firstApply(*pr.MergedAt, completed)
				return pr.Repo == deliveries.DeployRepo && apply != nil && apply.RunID == candidate.RunID && projectOf(pr) != ""
			}) {
				run, job = candidate, j
			}
		}
	}
	if run.RunID == 0 {
		t.Fatal("the history holds no deploy run whose failed production job, once completed, ships a pull request")
	}
	// What a run's change touches, read off the history before and after it: the run's day as it
	// counted then and counts now (runLines); and for every pull request it shipped then or ships
	// now, that pull request's merge day where it counts (countsFor), and the day of each run that
	// ships it then and now, as those runs counted then and count now.
	type shipping struct {
		by   map[string]int64
		runs map[int64]runCount
	}
	snapshot := func() shipping {
		now := shipping{by: map[string]int64{}, runs: map[int64]runCount{}}
		for _, pr := range prs {
			if apply := shipper(pr); apply != nil {
				now.by[PullRequestMember(pr.Repo, pr.Number)] = apply.RunID
			}
		}
		for _, candidate := range deliveries.Runs {
			if candidate.Kind == delivery.DeliveryRunKindDeploy {
				now.runs[candidate.RunID] = runCounts(candidate.RunID)
			}
		}
		return now
	}
	runOracle := func(id int64, then, now shipping) ([]string, int) {
		want := runLines(runDay(id), then.runs[id], now.runs[id])
		touched := 0
		for _, pr := range prs {
			member := PullRequestMember(pr.Repo, pr.Number)
			if then.by[member] != id && now.by[member] != id {
				continue
			}
			touched++
			want = union(want, lines(measures.Midnight(*pr.MergedAt), map[string]bool{countsFor(pr): true}))
			for _, other := range []int64{then.by[member], now.by[member]} {
				if other != 0 && other != id {
					want = union(want, runLines(runDay(other), then.runs[other], now.runs[other]))
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
	after := snapshot()
	want, shipped := runOracle(run.RunID, then, after)
	t.Logf("run %d (started %s) now ships %d pull requests", run.RunID, runDay(run.RunID).Format(time.DateOnly), shipped)
	before = r.expectDays(t, "a deploy run that now reaches production", before, keyDays(want, before), lines(runDay(run.RunID), after.runs[run.RunID].projects))

	// A deploy run that reached production and shipped pull requests stops reaching it (its
	// production job is run again and fails): the pull requests it shipped ship with the next
	// apply or wait, and their merge days move with them.
	var stopped delivery.Apply
	var stoppedJob delivery.DeliveryRunJob
	now := snapshot()
	// One that ships a pull request of a project merged on an earlier day than the run started,
	// so that day is written only because the run shipped it (eval_day_members.shipped_by), where
	// the history holds one; else one that ships a pull request of a project at all.
	for _, earlier := range []bool{true, false} {
		for _, apply := range applies {
			if stopped.RunID != 0 || apply.RunID == run.RunID || !slices.ContainsFunc(prs, func(pr delivery.DeliveryPullRequest) bool {
				return now.by[PullRequestMember(pr.Repo, pr.Number)] == apply.RunID && projectOf(pr) != "" &&
					(!earlier || measures.Midnight(*pr.MergedAt).Before(runDay(apply.RunID)))
			}) {
				continue
			}
			for _, j := range deliveries.Jobs[apply.RunID] {
				if slices.Contains(deliveries.Settings.ProductionJobNames, j.Name) && j.Conclusion != nil && *j.Conclusion == delivery.DeliveryJobConclusionSuccess {
					stopped, stoppedJob = apply, j
				}
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
		if now.by[PullRequestMember(pr.Repo, pr.Number)] == stopped.RunID && projectOf(pr) != "" {
			mergeDays = append(mergeDays, line(walkDelivery, projectOf(pr), measures.Midnight(*pr.MergedAt)))
		}
	}
	t.Logf("run %d (started %s) stops shipping its %d pull requests, merged on %v", stopped.RunID, runDay(stopped.RunID).Format(time.DateOnly), shipped, mergeDays)
	before = r.expectDays(t, "a deploy run that stops reaching production", before, keyDays(want, before),
		append(mergeDays, lines(runDay(stopped.RunID), now.runs[stopped.RunID].projects)...))

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
		return legionfacts.Row{Project: "EVAL", Issue: key, ClosedFacts: metrics.ClosedFacts{IssueFacts: metrics.IssueFacts{
			OpenedAt: legionHistoryDay(0), ClosedAt: closed, Root: true, Tree: key, WorkHours: hours,
			StartedAt: &started, PhaseHours: map[string]float64{}, Bars: map[string]metrics.BarPart{}, IdleByRole: map[string]float64{},
		}}}
	}
	for _, row := range []legionfacts.Row{
		fact("EVAL-1", new(history.d1.Add(10*time.Hour)), 6), fact("EVAL-2", new(history.d1.Add(12*time.Hour)), 4),
		fact("EVAL-3", new(history.d2.Add(9*time.Hour)), 3), fact("EVAL-4", nil, 2),
	} {
		history.rows[row.Issue] = row
	}
	r.writeFacts(t, history, "EVAL-1", "EVAL-2", "EVAL-3", "EVAL-4")
	history.filing = legionfacts.Filing{Project: "EVAL", IssueFiling: metrics.IssueFiling{FiledIssue: metrics.FiledIssue{
		Key: "EVAL-10", At: history.d1.Add(8 * time.Hour), Filer: "architect",
		FilerIssue: "EVAL-1", FilerPhase: "design", Class: metrics.ClassInTreeAtDesign,
	}, Root: "EVAL-1"}}
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
//   - an issue that reopens rewrites the day it had closed on (the change's t_old), and its
//     close on a later day rewrites that day alone;
//   - an issue whose close moves from one day to another in one write rewrites both, and the day
//     it left, which holds no other input, keeps the zero rows of the project's series and loses
//     its repository rows, as a whole walk over the moved close holds them, and logs an eval_days
//     change naming it, so a view of it wakes;
//   - a change of an open issue's facts rewrites no day;
//   - a filing whose class moves rewrites its filing day, and so does a write that moves the filed
//     issue's repository, which moves the repository the filing is cut by;
//   - a new P0 issue rewrites the project's unowned-p0 row of today, and a change of another issue
//     that is no P0 rewrites nothing;
//   - a reading of a logged eval rewrites no day.
func TestEachLegionChangeRewritesExactlyItsDays(t *testing.T) {
	t.Parallel()
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
	prior := before
	before = r.expectDays(t, "EVAL-3's close moved a day on", before,
		[]string{line(walkLegion, "EVAL", history.d2), line(walkLegion, "EVAL", d4)},
		[]string{line(walkLegion, "EVAL", history.d2), line(walkLegion, "EVAL", d4)})
	lost, heldRepos := 0, 0
	for key, row := range prior {
		if familyOf(row) != walkLegion || !row.Day.Equal(history.d2) || row.Repo == "" || strings.Contains(row.EvalID, SlugLegionSplitting) {
			continue
		}
		// A repository's row of that day held EVAL-3 alone: a whole walk holds none.
		heldRepos++
		if _, kept := before[key]; kept {
			t.Errorf("%s stays after its day lost its one closed issue, want it deleted", key)
		}
	}
	for _, row := range before {
		if familyOf(row) == walkLegion && row.Day.Equal(history.d2) && row.Repo == "" {
			if row.N != 0 && !strings.HasPrefix(row.Series, "filed") {
				t.Errorf("%s holds n %d after its day lost its one closed issue, want a zero row", rowKey(row), row.N)
			}
			lost++
		}
	}
	woke := slices.ContainsFunc(r.evalDayChanges(t, seq), func(change string) bool {
		return strings.HasPrefix(change, EvalID("EVAL", SlugLegionWorkTime)+" ") && strings.Contains(change, history.d2.Format(time.DateOnly))
	})
	if lost == 0 || heldRepos == 0 || !woke {
		t.Errorf("the day that lost its input holds %d all-repositories rows, held %d repository rows before, and an eval_days change naming it was logged: %t; want zero rows of the first, some of the second, and a change",
			lost, heldRepos, woke)
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

// TestAMoveOfAnAncestorRewritesTheCloseDaysBelowIt: an issue with no repository of its own resolves
// to the repository of the issue above it, so a write that moves an ancestor's repository moves
// the repository rows of every issue closed below it, though no stored fact of those issues
// changes. Over the hand-written Legion history of EVAL, with EVAL-3 (closed on the history's
// third day) below EVAL-4 (open, with no filing) and no repository of its own, so it is cut by
// EVAL-4's, acme/web, a write moving EVAL-4's repository to acme/api rewrites EVAL-3's close day,
// whose rows of acme/web are then gone and which holds rows of acme/api.
func TestAMoveOfAnAncestorRewritesTheCloseDaysBelowIt(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	history := r.seedLegionEval(t)
	r.exec(t, "put EVAL-3 below EVAL-4, with no repository of its own", `update issues set parent_key = 'EVAL-4', repo = null where key = 'EVAL-3'`)
	r.settle(t)
	r.pass(t)
	before := r.dayRows(t)
	repoRows := func(repo string) int {
		t.Helper()
		return r.count(t, `select count(*) from eval_days where eval_id = $1 and day = $2 and repo = $3`, EvalID("EVAL", SlugLegionWorkTime), history.d2, repo)
	}
	if web, api := repoRows("acme/web"), repoRows("acme/api"); web == 0 || api != 0 {
		t.Fatalf("before the move %s holds %d rows of acme/web and %d of acme/api, want EVAL-3's cut by its parent's repository, acme/web, alone", history.d2.Format(time.DateOnly), web, api)
	}
	r.exec(t, "move EVAL-4 to acme/api", `update issues set repo = 'acme/api' where key = 'EVAL-4'`)
	r.writeIssueEvent(t, "EVAL-4", "issue.updated", `{"key":"EVAL-4","status":"in_progress","changed":["repo"]}`)
	moved := []string{line(walkLegion, "EVAL", history.d2)}
	r.expectDays(t, "the repository of EVAL-3's parent moved", before, moved, moved)
	if web, api := repoRows("acme/web"), repoRows("acme/api"); web != 0 || api == 0 {
		t.Errorf("after the move %s holds %d rows of acme/web and %d of acme/api, want EVAL-3 cut by acme/api, its parent's new repository, alone", history.d2.Format(time.DateOnly), web, api)
	}
}

// TestAPullRequestsIssueMoveMovesItsPRChecksRunsWithinOnePass: a PR-checks run counts for the
// project its pull request counts for, whatever day it started on (attribution.check). Over the
// delivery history, its pull requests' attribution inputs left unread so the issue each row names
// is its attribution (leaveInputsUnread), once every day is walked, a pull request of the deploy
// repository whose issue is the first project's, and one of whose PR-checks runs started on
// another day than its merge, moves to the second project (a new issue of it): the one pass after
// the move counts each of its PR-checks runs for the second project and no more for the first, and
// the database then holds the rows, members and figures a second database holds that made the same
// move before its first walk. Then a write of repo_projects, which no rule reads, logs no change,
// and the pass after it rewrites no day.
func TestAPullRequestsIssueMoveMovesItsPRChecksRunsWithinOnePass(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.settle(t)
	r.pass(t)
	main, other := deliveries.Project, deliveries.SecondProject
	issueProject := map[string]string{}
	for project, keys := range deliveries.Issues {
		for _, key := range keys {
			issueProject[key] = project
		}
	}
	var moved delivery.DeliveryPullRequest
	var checks []string
	for _, pr := range deliveries.PullRequests {
		if moved.Number != 0 || pr.Repo != deliveries.DeployRepo || pr.IssueKey == nil || issueProject[*pr.IssueKey] != main {
			continue
		}
		var runs []string
		elsewhere := false
		for _, run := range deliveries.Runs {
			if run.Kind == delivery.DeliveryRunKindPRChecks && run.PRNumber != nil && *run.PRNumber == pr.Number {
				runs = append(runs, RunMember(run.Repo, run.RunID))
				elsewhere = elsewhere || !measures.Midnight(run.StartedAt).Equal(measures.Midnight(*pr.MergedAt))
			}
		}
		if elsewhere {
			moved, checks = pr, runs
		}
	}
	if moved.Number == 0 {
		t.Fatalf("the history holds no pull request of %s whose PR-checks run started on another day than its merge", main)
	}
	counted := `select count(*) from eval_day_members where family = $1 and row_key = any($2) and project_key = $3`
	if got, none := r.count(t, counted, KindCI, checks, main), r.count(t, counted, KindCI, checks, other); got != len(checks) || none != 0 {
		t.Fatalf("before the move %s counts %d of %s's %d PR-checks runs and %s %d, want every one in %s alone",
			main, got, PullRequestMember(moved.Repo, moved.Number), len(checks), other, none, main)
	}
	move := func(rig *refresherRig) {
		rig.exec(t, "make an issue of "+other, `
			insert into issues (key, project_key, number, title, status, rank, created_by)
			values ($1, $2, 900, 'Work moved here', 'in_progress', '00000900', '{"kind":"user","id":"alice"}')`, other+"-900", other)
		rig.exec(t, "move the pull request to "+other, `update delivery_pull_requests set issue_key = $3 where repo = $1 and number = $2`,
			moved.Repo, moved.Number, other+"-900")
	}
	move(r)
	r.pass(t)
	if got, now := r.count(t, counted, KindCI, checks, main), r.count(t, counted, KindCI, checks, other); got != 0 || now != len(checks) {
		t.Errorf("after the move %s counts %d of the %d PR-checks runs and %s %d, want every one in %s alone", main, got, len(checks), other, now, other)
	}
	walk := newRefresherRig(t)
	walk.seedHistories(t)
	walk.leaveInputsUnread(t)
	move(walk)
	walk.settle(t)
	r.expectSameAsWalk(t, "after "+PullRequestMember(moved.Repo, moved.Number)+" moved to "+other, walk)

	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("move the changes the pass left pending into the log: %v", err)
	}
	seq := r.latestSeq(t)
	r.exec(t, "write a row of repo_projects", `
		insert into repo_projects (repo, project, created_by, is_default) values ($1, $2, '{"kind":"user","id":"alice"}', true)`, deliveries.DeployRepo, other)
	if _, err := delivery.DrainPending(t.Context(), r.database.Pool); err != nil {
		t.Fatalf("move the pending changes into the log: %v", err)
	}
	changes, err := delivery.ChangesSince(t.Context(), r.database.Pool, seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("the write of repo_projects logged %+v, want no change", changes)
	}
	if report, _ := r.pass(t); len(report.days) != 0 {
		t.Errorf("the pass after the write of repo_projects rewrote %d days, want none", len(report.days))
	}
	t.Logf("%s's %d PR-checks runs moved from %s to %s in one pass", PullRequestMember(moved.Repo, moved.Number), len(checks), main, other)
}

// mergeDayEarlier merges every pull request of repo merged on day again a day earlier, as
// TestARepositoryRowThatLosesItsLastInputIsDeletedAsAWholeWalkHoldsNone moves them.
func (r *refresherRig) mergeDayEarlier(t *testing.T, repo string, day time.Time) {
	t.Helper()
	r.exec(t, "merge "+repo+"'s pull requests of "+day.Format(time.DateOnly)+" a day earlier", `
		update delivery_pull_requests
		set merged_at = merged_at - interval '1 day', created_at = least(created_at, merged_at - interval '1 day' - interval '1 hour')
		where repo = $1 and merged_at >= $2 and merged_at < $3
	`, repo, day, day.AddDate(0, 0, 1))
}

// TestARepositoryRowThatLosesItsLastInputIsDeletedAsAWholeWalkHoldsNone: over the delivery
// history, its pull requests' attribution inputs left unread so the issue each row names is its
// attribution (leaveInputsUnread), once every day is walked,
// every pull request one repository merged on one day is merged again a day earlier, so the
// repository's last pull request leaves that day. The one pass after it deletes each of the
// repository's rows of that day, in every project and owner key (owners.go) that held one, where
// it kept them as zero rows before; it logs an eval_days change spanning that day for the eval of
// each, an owner key's under each project that reads the key and none where no project does
// (migration 20261117100014's delete trigger), so a view of that repository's numbers wakes; it
// creates no repository row of a day that never held that repository's input; and the database
// then holds the delivery and CI rows, the members and the stored figures a second database holds
// that made the same move before its first walk, so the rows do not depend on whether the walk
// read the history before the move or after it. Then the settings stop making the staging gate's
// card: one pass deletes every row of that card, where a whole walk under the new settings holds
// none.
func TestARepositoryRowThatLosesItsLastInputIsDeletedAsAWholeWalkHoldsNone(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
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
	r.mergeDayEarlier(t, repo, day)
	r.pass(t)
	after := r.dayRows(t)
	changes := r.evalDayChanges(t, seq)

	evalsDeleted := map[string]bool{}
	for _, key := range held {
		if row, kept := after[key]; kept {
			t.Errorf("%s holds %s after its last input left, want it deleted", key, rowFacts(row))
			continue
		}
		evalsDeleted[before[key].EvalID] = true
	}
	// An owner key's change is logged under each project that reads the key (readerEvals).
	for _, evalID := range readerEvals(slices.Collect(maps.Keys(evalsDeleted)), r.ownerReaders(t)) {
		if !slices.ContainsFunc(changes, func(change string) bool {
			first, last, found := strings.Cut(strings.TrimPrefix(change, evalID+" "), "..")
			return strings.HasPrefix(change, evalID+" ") && found && first <= day.Format(time.DateOnly) && last >= day.Format(time.DateOnly)
		}) {
			t.Errorf("no eval_days change of %s spans %s, where its rows were deleted: %v", evalID, day.Format(time.DateOnly), changes)
		}
	}
	for key, row := range after {
		if _, was := before[key]; !was && row.Repo == repo && !row.Day.Equal(day.AddDate(0, 0, -1)) {
			t.Errorf("the pass created %s, a row of %s on a day its input never reached", key, repo)
		}
	}
	if len(held) == 0 || len(evalsDeleted) == 0 {
		t.Errorf("%s held %d rows of %s on %s and %d evals' rows were deleted, want some of each", repo, len(held), repo, day.Format(time.DateOnly), len(evalsDeleted))
	}

	walk := newRefresherRig(t)
	walk.seedHistories(t)
	walk.leaveInputsUnread(t)
	walk.mergeDayEarlier(t, repo, day)
	walk.settle(t)
	r.expectSameAsWalk(t, "after "+repo+"'s pull requests of "+day.Format(time.DateOnly)+" merged a day earlier", walk)
	t.Logf("%s's %d rows of %s were deleted in the evals %v; eval_days changes of the pass: %v",
		repo, len(held), day.Format(time.DateOnly), slices.Sorted(maps.Keys(evalsDeleted)), changes)

	settings, err := delivery.GetSettings(t.Context(), r.database.Pool)
	if err != nil {
		t.Fatalf("read the delivery settings: %v", err)
	}
	if settings.StagingE2EJobName == nil {
		t.Fatal("the history's settings make no staging gate's card, and the check needs one")
	}
	gate := EvalID(deliveries.Project, SlugStagingE2E)
	if r.count(t, `select count(*) from eval_days where eval_id = $1`, gate) == 0 {
		t.Fatalf("%s holds no row, and the check needs some", gate)
	}
	settings.StagingE2EJobName = nil
	for _, rig := range []*refresherRig{r, walk} {
		rig.putSettings(t, settings, alice)
	}
	r.pass(t)
	walk.exec(t, "forget the settings the rows stand built under", `delete from eval_days_settings`)
	walk.settle(t)
	if left := r.count(t, `select count(*) from eval_days where eval_id = $1`, gate); left != 0 {
		t.Errorf("%s holds %d rows once the settings stop making its card, want none", gate, left)
	}
	r.expectSameAsWalk(t, "the staging gate's card no longer made", walk)
}

// TestADayWithNoInputReadsZeroFromItsSeriesFirstInputDayOnAndNoDataBeforeIt: the boundary of the
// stored rows (dispatch://INFRA-2916, items 5 and 8, R12). Over the delivery history, its pull
// requests' attribution inputs left unread so the issue each row names is its attribution
// (leaveInputsUnread), once every
// day is walked, the deploy runs of the deploy repository's first deploy day and of its last are
// deleted. The pass after it leaves the main project's deploy-frequency of that repository with no
// row on the first of those days, which is no first input day of it any more, so a window over it
// reads "no data" (every statistic null) and its data_since is the next deploy day; and with the
// zero row of the last, a day after that first input day with no input, so a window over it reads
// 0 deploys a day. A second database whose runs were deleted before its first walk holds the same
// rows, members and stored figures.
func TestADayWithNoInputReadsZeroFromItsSeriesFirstInputDayOnAndNoDataBeforeIt(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries, _ := r.seedHistories(t)
	r.leaveInputsUnread(t)
	r.settle(t)
	ctx := t.Context()
	repo, main := deliveries.DeployRepo, deliveries.Project
	started := map[time.Time]bool{}
	for _, run := range deliveries.Runs {
		if run.Repo == repo && run.Kind == delivery.DeliveryRunKindDeploy {
			started[measures.Midnight(run.StartedAt)] = true
		}
	}
	deployDays := slices.SortedFunc(maps.Keys(started), time.Time.Compare)
	if len(deployDays) < 3 {
		t.Fatalf("%s's deploy runs start on %v, and the check needs three days", repo, deployDays)
	}
	first, second, last := deployDays[0], deployDays[1], deployDays[len(deployDays)-1]
	drop := func(rig *refresherRig) {
		t.Helper()
		for _, day := range []time.Time{first, last} {
			rig.exec(t, "delete the jobs of the deploy runs of "+day.Format(time.DateOnly), `
				delete from delivery_run_jobs job using delivery_runs run
				where run.repo = job.repo and run.run_id = job.run_id and run.repo = $1 and run.kind = $2 and run.started_at >= $3 and run.started_at < $4
			`, repo, string(delivery.DeliveryRunKindDeploy), day, day.AddDate(0, 0, 1))
			rig.exec(t, "delete the deploy runs of "+day.Format(time.DateOnly), `
				delete from delivery_runs where repo = $1 and kind = $2 and started_at >= $3 and started_at < $4
			`, repo, string(delivery.DeliveryRunKindDeploy), day, day.AddDate(0, 0, 1))
		}
	}
	drop(r)
	r.settle(t)
	walk := newRefresherRig(t)
	walk.seedHistories(t)
	walk.leaveInputsUnread(t)
	drop(walk)
	walk.settle(t)
	r.expectSameAsWalk(t, "the deploy runs of "+first.Format(time.DateOnly)+" and "+last.Format(time.DateOnly)+" deleted", walk)

	page, err := r.registry.Current(ctx, []string{main}, "", 100, 0)
	if err != nil {
		t.Fatalf("read the evals of %s: %v", main, err)
	}
	index := slices.IndexFunc(page.Items, func(entry Entry) bool { return entry.Definition.Slug == SlugDeployFrequency })
	if index < 0 {
		t.Fatalf("%s has no deploy-frequency eval", main)
	}
	entry, evalID := page.Items[index], EvalID(main, SlugDeployFrequency)
	firsts, err := FirstDaysIn(ctx, r.database.Pool, []Entry{entry}, repo)
	if err != nil {
		t.Fatalf("read the first days of %s: %v", evalID, err)
	}
	read := func(day time.Time) SeriesValues {
		t.Helper()
		window := Range{From: day, To: day.AddDate(0, 0, 1)}
		rows, err := DayWindowIn(ctx, r.database.Pool, []string{main}, []Entry{entry}, repo, nil, window, nil)
		if err != nil {
			t.Fatalf("read %s over %s: %v", evalID, day.Format(time.DateOnly), err)
		}
		values := SeriesValuesOf(entry, rows, firsts[evalID], window, nil)
		if len(values) == 0 {
			t.Fatalf("%s states no listed series", evalID)
		}
		return values[0]
	}
	stored := r.dayRows(t)
	row := func(day time.Time) (DayRow, bool) {
		held, found := stored[rowKey(DayRow{ProjectKey: main, EvalID: evalID, DeployRepo: repo, DayPart: measures.DayPart{Series: measures.SeriesDeploys, Day: day}})]
		return held, found
	}
	if held, found := row(first); found {
		t.Errorf("%s holds %s on %s, before its first input day %s, want no row", evalID, rowFacts(held), first.Format(time.DateOnly), second.Format(time.DateOnly))
	}
	headline := computedSources[entry.Definition.Source].headline(measures.SeriesDeploys)
	before := read(first)
	if value, stated := before.Window[headline]; !stated || value != nil {
		t.Errorf("a window over %s, before %s's first input day, states %s %s (stated %t), want it stated as no data", first.Format(time.DateOnly), evalID, headline, showValue(value), stated)
	}
	for name, value := range before.Window {
		if value != nil {
			t.Errorf("a window over %s, before %s's first input day, reads %s %g, want no data", first.Format(time.DateOnly), evalID, name, *value)
		}
	}
	if before.DataSince == nil || *before.DataSince != second.Format(time.DateOnly) {
		t.Errorf("%s's data_since is %v, want %s, its next deploy day", evalID, before.DataSince, second.Format(time.DateOnly))
	}
	if held, found := row(last); !found || held.N != 0 {
		t.Errorf("%s on %s, a day after its first input day with no input, holds %v (found %t), want its zero row", evalID, last.Format(time.DateOnly), held.N, found)
	}
	if value := read(last).Window[headline]; value == nil || *value != 0 {
		t.Errorf("a window over %s reads %s %s, want 0", last.Format(time.DateOnly), headline, showValue(value))
	}
	t.Logf("%s: no row and no data on %s, data_since %s, a zero row on %s reading %s 0", evalID, first.Format(time.DateOnly),
		second.Format(time.DateOnly), last.Format(time.DateOnly), headline)
}

// legionValues is every stored row of a Legion eval, by rowKey, without the moment and the build
// of its last write: what two databases' Legion rows compare by.
func (r *refresherRig) legionValues(t *testing.T) map[string]DayRow {
	t.Helper()
	values := map[string]DayRow{}
	for key, row := range r.dayRows(t) {
		if familyOf(row) == walkLegion {
			row.UpdatedAt, row.BuiltBy = time.Time{}, ""
			values[key] = row
		}
	}
	return values
}

// legionFigureValues is the fold key and figures of each stored figures row of a Legion eval, by
// its line.
func legionFigureValues(stored map[string]figureRow) map[string]string {
	values := map[string]string{}
	for line, row := range stored {
		evalID := line[strings.LastIndex(line, " ")+1:]
		if _, slug, _ := strings.Cut(evalID, "/"); slices.Contains(legionDaySlugs, slug) {
			values[line] = fmt.Sprintf("%d %x", row.foldKey, row.figures)
		}
	}
	return values
}

// expectSameLegionAsWalk checks that r holds the Legion day rows, the members and the stored
// figures walk holds, and that r's stored figures are the fold of its rows.
func (r *refresherRig) expectSameLegionAsWalk(t *testing.T, what string, walk *refresherRig) {
	t.Helper()
	if differ := sameValues(r.legionValues(t), walk.legionValues(t), rowFacts); len(differ) > 0 {
		t.Errorf("%s: the Legion day rows differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
	if differ := sameValues(r.memberLines(t), walk.memberLines(t), func(at string) string { return at }); len(differ) > 0 {
		t.Errorf("%s: the members differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
	stored := r.storedFigures(t)
	expectTruth(t, what, stored, r.truthFigures(t))
	if differ := sameValues(legionFigureValues(stored), legionFigureValues(walk.storedFigures(t)), func(figures string) string { return figures }); len(differ) > 0 {
		t.Errorf("%s: the stored figures differ from the whole walk's, first ones:\n\t%s", what, strings.Join(differ, "\n\t"))
	}
}

// TestALegionSeriesWhoseFirstDayMovesLandsInOneCommitAsAWholeWalkHoldsIt: over the hand-written
// Legion history of EVAL, walked two days a pass, EVAL-3, closed on the history's third day, is
// closed again on the day the history begins, two days before the project's first close: the first
// input day of the series a close has input of moves earlier, and the day between the new first day
// and the old one needs their zero rows (B). Then it is closed on the third day again, so the new
// first day loses its input, and it and the day after it hold no row of those series (E). After
// each, one pass writes every Legion day row, member and stored figure it moves in one
// transaction, logs at most one eval_days change of each eval, each with the rebuild's mark, and
// leaves the Legion walk's record as it stood and no walk with days to build; and the database
// holds the Legion rows, the members and the stored figures of a second database whose issue
// closed so before its first walk.
func TestALegionSeriesWhoseFirstDayMovesLandsInOneCommitAsAWholeWalkHoldsIt(t *testing.T) {
	t.Parallel()
	live := newRefresherRig(t)
	live.batch = 2
	history := live.seedLegionEval(t)
	live.settle(t)
	wrote := recordWrites(t, live.rig)
	record := func() string {
		t.Helper()
		var at string
		if err := live.database.Pool.QueryRow(t.Context(), `
			select xmin::text || ' ' || coalesce(next_day::text, 'to begin') from eval_days_walks where family = $1 and project_key = 'EVAL'
		`, walkLegion).Scan(&at); err != nil {
			t.Fatalf("read the Legion walk's record: %v", err)
		}
		return at
	}
	first := legionHistoryDay(0)
	for _, move := range []struct {
		name   string
		closed time.Time
	}{
		{"B, EVAL-3 closed two days before the project's first close", first.Add(14 * time.Hour)},
		{"E, EVAL-3 closed on the third day again", history.d2.Add(9 * time.Hour)},
	} {
		closeAgain := func(r *refresherRig, of *legionEval) {
			t.Helper()
			row := of.rows["EVAL-3"]
			row.ClosedAt = new(move.closed)
			of.rows["EVAL-3"] = row
			r.writeFacts(t, of, "EVAL-3")
		}
		if _, err := delivery.DrainPending(t.Context(), live.database.Pool); err != nil {
			t.Fatalf("drain the change log's outbox: %v", err)
		}
		seq, stood := live.latestSeq(t), record()
		closeAgain(live, history)
		wrote()
		report, _ := live.pass(t)
		transactions := wrote()
		if len(transactions) != 1 {
			t.Errorf("%s: the pass wrote day rows, members or figures in %d transactions, want one: %v", move.name, len(transactions), transactions)
		}
		if report.walksLeft {
			t.Errorf("%s: the pass left a walk with days to build", move.name)
		}
		if got := record(); got != stood {
			t.Errorf("%s: the Legion walk's record is %q after the pass, want it as it stood, %q", move.name, got, stood)
		}
		if _, err := delivery.DrainPending(t.Context(), live.database.Pool); err != nil {
			t.Fatalf("drain the change log's outbox: %v", err)
		}
		changes, err := delivery.ChangesSince(t.Context(), live.database.Pool, seq, 100000)
		if err != nil {
			t.Fatalf("read the delivery changes after %d: %v", seq, err)
		}
		logged := map[string]int{}
		for _, one := range changes {
			if one.Kind != delivery.ChangeKindEvalDays || one.Ref == "" {
				continue
			}
			logged[one.Ref]++
			if one.RunKind != delivery.EvalDaysWrittenByRebuild || logged[one.Ref] > 1 {
				t.Errorf("%s: eval_days change %d of %s carries run_kind %q, want one of each eval, with %q", move.name, logged[one.Ref], one.Ref, one.RunKind,
					delivery.EvalDaysWrittenByRebuild)
			}
		}
		if len(logged) == 0 {
			t.Errorf("%s: the pass logged no eval_days change", move.name)
		}
		walk := newRefresherRig(t)
		closed := walk.seedLegionEval(t)
		closeAgain(walk, closed)
		walk.settle(t)
		live.expectSameLegionAsWalk(t, move.name, walk)
		days, n := seriesDays(live.dayRows(t), "EVAL", SlugLegionWorkTime, "hours")
		t.Logf("%s: one pass wrote in the transactions %v and logged %v; work time holds rows of %v, n %v", move.name, transactions, logged, days, n)
	}
}
