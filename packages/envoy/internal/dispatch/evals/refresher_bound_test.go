package evals

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/logtest"
)

// windowBound is the bound of a window's read of pull requests (delivery.MaxPullRequestsPerWindow).
// A refusal names it (delivery.RefusedWindow.Bound), and the test checks the bound each refusal
// names against it.
const windowBound = delivery.MaxPullRequestsPerWindow

// TestAWindowPastTheBoundCostsItsOwnDayAlone: a window of a delivery day's read that holds more pull
// requests than the bound of a window's read is refused, and the refusal costs its own day alone
// (dispatch://INFRA-2916/comment/0d936c90-6b07-4049-bca2-7b08ea901db3). Over the delivery history
// of six days, walked whole, two windows pass the bound by one pull request:
//   - the own window of the middle day, two days after the day of the history's first deploy to
//     complete, where windowBound+1 more pull requests, of a repository that holds no run, merge;
//   - the window of the earlier rows of that first deploy's day (delivery.Deploys.Earlier): its
//     deploys ship what merged before it from the beginning of time, and windowBound+1 more pull
//     requests of the deploy repository merge the day before the history.
//
// A change of one pull request merged on each of the middle day, the day before it and the day
// after it, and one of the first deploy run, name those four days, which the pass's page reads in
// one read of several days (readDeliveries). The pass does not fail, where the refusal of the
// whole read failed it: the middle day holds no row of any eval but pr-ci, whose PR-checks runs
// the window does not decide; the first deploy's day holds no row of deploy-frequency,
// deploy-run-success or the deploy cards as read with the deploy repository, and keeps its lead
// times and its pr-ci; each of those rows was stored before, and none stands after, in any project
// or owner key. Every other row that counts something stands with its values, the other two days'
// among them, which the pass wrote. Each refused window is logged at WARN, naming the day, the
// window and the bound.
func TestAWindowPastTheBoundCostsItsOwnDayAlone(t *testing.T) {
	// Not parallel: it reads the refresher's WARN through the process's default logger
	// (logtest.SetDefault), which a test running beside it would log into.
	r := newRefresherRig(t)
	fixture := deliverytest.Seed(t, r.database.Pool, deliveryseed.Spec{Days: 6})
	r.settle(t)

	firstApply := slices.MinFunc(fixture.Applies, func(a, b delivery.Apply) int { return a.CompletedAt.Compare(b.CompletedAt) })
	at := slices.IndexFunc(fixture.Runs, func(run delivery.DeliveryRun) bool { return run.RunID == firstApply.RunID })
	if at < 0 {
		t.Fatalf("the history holds no run %d, its first deploy to complete", firstApply.RunID)
	}
	firstRun := fixture.Runs[at]
	first := measures.Midnight(firstRun.StartedAt)
	middle := first.AddDate(0, 0, 2)
	if !middle.AddDate(0, 0, 1).Before(fixture.To) {
		t.Fatalf("the history's first deploy to complete started on %s, and the day after the middle day two days on is past the history's last, %s",
			first.Format(time.DateOnly), fixture.To.AddDate(0, 0, -1).Format(time.DateOnly))
	}
	named := []time.Time{middle.AddDate(0, 0, -1), middle, middle.AddDate(0, 0, 1)}
	var touched []delivery.DeliveryPullRequest
	for _, day := range named {
		at := slices.IndexFunc(fixture.PullRequests, func(pr delivery.DeliveryPullRequest) bool {
			return pr.MergedAt != nil && measures.Midnight(*pr.MergedAt).Equal(day)
		})
		if at < 0 {
			t.Fatalf("the history merges no pull request on %s", day.Format(time.DateOnly))
		}
		touched = append(touched, fixture.PullRequests[at])
	}

	// The pull requests past the bound, each window's in one statement, logging no change: no
	// change names them, and the days the page reads are the ones the changes below name.
	r.exec(t, "stop logging the pull requests' changes", `alter table delivery_pull_requests disable trigger delivery_pull_requests_changed`)
	merge := func(what, repo string, numberedFrom int, after time.Time) {
		r.exec(t, what, `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at)
			select $1::text, $2::int + n, 'feat: a widget', 'https://github.com/' || $1::text || '/pull/' || ($2::int + n), 'octocat',
				merged - interval '1 hour', merged
			from generate_series(1, $3::int) n, lateral (select $4::timestamptz + n * interval '1 second' as merged) moment
		`, repo, numberedFrom, windowBound+1, after)
	}
	merge("merge one pull request more than the bound on the middle day", "acme/merge-train", 0, middle)
	merge("merge one pull request more than the bound of the deploy repository before the history", firstRun.Repo, 1_000_000, fixture.From.AddDate(0, 0, -1))
	r.exec(t, "log the pull requests' changes again", `alter table delivery_pull_requests enable trigger delivery_pull_requests_changed`)

	values := func() map[string]DayRow {
		kept := map[string]DayRow{}
		for key, row := range r.dayRows(t) {
			row.BuiltBy, row.UpdatedAt = "", time.Time{}
			kept[key] = row
		}
		return kept
	}
	before := values()
	ownDropped := []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugMergeToProduction, SlugFirstCommitToProduction,
		SlugOpenToProduction, SlugPROpenToMerge, SlugReworkShare, SlugStagingE2E, SlugStagingDeploy, SlugProductionDeploy}
	earlierDropped := []string{SlugDeployFrequency, SlugDeployRunSuccess, SlugStagingE2E, SlugStagingDeploy, SlugProductionDeploy}
	dropped := func(row DayRow) bool {
		slug := strings.TrimPrefix(row.EvalID, row.ProjectKey+"/")
		switch {
		case row.Day.Equal(middle):
			return slices.Contains(ownDropped, slug)
		case row.Day.Equal(first):
			return row.DeployRepo == firstRun.Repo && slices.Contains(earlierDropped, slug)
		}
		return false
	}
	droppedOn := func(rows map[string]DayRow, day time.Time) []string {
		var keys []string
		for key, row := range rows {
			if row.Day.Equal(day) && dropped(row) {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		return keys
	}
	for _, day := range []time.Time{first, middle} {
		if len(droppedOn(before, day)) == 0 {
			t.Fatalf("no row of an eval the refusal drops stands on %s before the pass, so its going proves nothing", day.Format(time.DateOnly))
		}
	}
	counting := func(rows map[string]DayRow) map[string]DayRow {
		kept := map[string]DayRow{}
		for key, row := range rows {
			if row.N > 0 && !dropped(row) {
				kept[key] = row
			}
		}
		return kept
	}

	// Each change is logged as the triggers of delivery_pull_requests and delivery_runs log it, by
	// delivery_change_log straight into the change log (migration 0100): the outbox of pending
	// changes holds no pull request or run change.
	for _, pr := range touched {
		r.exec(t, fmt.Sprintf("log a change of %s#%d", pr.Repo, pr.Number), `select delivery_change_log('pull_request', null, $1, $2, $3, $3)`,
			pr.Repo, strconv.Itoa(pr.Number), *pr.MergedAt)
	}
	logged := firstRun.StartedAt
	if firstRun.HeadCommitAt.After(logged) {
		logged = firstRun.HeadCommitAt
	}
	r.exec(t, "log a change of the first deploy run", `select delivery_change_log('run', $1, $2, $3, $4, $4)`,
		string(firstRun.Kind), firstRun.Repo, strconv.FormatInt(firstRun.RunID, 10), logged)
	recorder := &logtest.Recorder{}
	logtest.SetDefault(t, recorder)
	report, _ := r.pass(t)

	after := values()
	written := map[string]bool{}
	for day := range report.days {
		if day.family == walkDelivery {
			written[day.day.Format(time.DateOnly)] = true
		}
	}
	for _, day := range append([]time.Time{first}, named...) {
		if !written[day.Format(time.DateOnly)] {
			t.Errorf("the pass wrote no delivery day of %s, which a change names", day.Format(time.DateOnly))
		}
	}
	for _, day := range []time.Time{first, middle} {
		if left := droppedOn(after, day); len(left) > 0 {
			t.Errorf("%d rows of the evals the refusal drops stand on %s after the pass, want none, so the day reads \"no data\" for them; first ones:\n\t%s",
				len(left), day.Format(time.DateOnly), strings.Join(left[:min(len(left), 10)], "\n\t"))
		}
	}
	kept := counting(before)
	if differ := sameValues(counting(after), kept, rowFacts); len(differ) > 0 {
		t.Errorf("the rows that count something and that no refusal drops moved, first ones:\n\t%s", strings.Join(differ, "\n\t"))
	}
	keptOn := map[string]int{}
	for _, row := range kept {
		keptOn[row.Day.Format(time.DateOnly)]++
	}
	for _, day := range append([]time.Time{first}, named...) {
		if keptOn[day.Format(time.DateOnly)] == 0 {
			t.Errorf("no row that counts something and that no refusal drops stands on %s, so the comparison of its rows compares nothing", day.Format(time.DateOnly))
		}
	}

	type warning struct{ window, bound, reason string }
	warned := map[string]warning{}
	for _, record := range recorder.Records() {
		if record.Level != slog.LevelWarn {
			continue
		}
		attrs := map[string]string{}
		record.Attrs(func(attr slog.Attr) bool {
			attrs[attr.Key] = attr.Value.String()
			return true
		})
		if attrs["window"] != "" {
			warned[attrs["day"]+" "+attrs["repo"]] = warning{window: attrs["window"], bound: attrs["bound"], reason: attrs["reason"]}
		}
	}
	window := func(from, to time.Time) string {
		return "[" + from.Format(time.RFC3339Nano) + ", " + to.Format(time.RFC3339Nano) + ")"
	}
	for _, want := range []struct{ day, repo, window string }{
		{middle.Format(time.DateOnly), "", window(middle, middle.AddDate(0, 0, 1))},
		{first.Format(time.DateOnly), firstRun.Repo, window(time.Time{}, first)},
	} {
		got, found := warned[want.day+" "+want.repo]
		if !found || got.window != want.window || got.bound != strconv.Itoa(windowBound) ||
			!strings.Contains(got.reason, strconv.Itoa(windowBound)) || !strings.Contains(got.reason, want.window) {
			t.Errorf("the pass logged %+v of %s's window of repository %q, want a WARN naming the window %s and the bound %d; it logged %v",
				got, want.day, want.repo, want.window, windowBound, warned)
		}
	}
	t.Logf("the pass wrote %d delivery days; %d rows of the evals the refusals drop stood on %s and %s before it and none after, and %d rows that count something stand as they stood; it warned %v",
		len(written), len(droppedOn(before, first))+len(droppedOn(before, middle)), first.Format(time.DateOnly), middle.Format(time.DateOnly), len(kept), warned)
}
