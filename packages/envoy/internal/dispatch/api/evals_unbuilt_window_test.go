package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliveryseed"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
)

// walkedEvals is whether item is a computed eval whose day rows a walk builds over the history: a
// delivery, CI or Legion eval, every computed eval but unowned-p0, whose one row of a day is
// written on that day.
func walkedEvals(item evalItem) bool {
	source := item.Definition.Source
	return strings.HasPrefix(source, evals.SourceComputedPrefix) && source != evals.SourceListUnownedP0
}

// requireNoNumber fails unless every walked eval (walkedEvals) of items answers no value over its
// window and its compare range, every statistic null, and answers how many it checked.
func requireNoNumber(t *testing.T, what string, items []evalItem) int {
	t.Helper()
	checked := 0
	for _, item := range items {
		if !walkedEvals(item) {
			continue
		}
		checked++
		for _, series := range item.Series {
			for name, stats := range map[string]evals.Statistics{"window": series.Window, "compare": series.Compare, "change": series.Change} {
				for statistic, value := range stats {
					if value != nil {
						t.Errorf("%s: %s's %s.%s over the %s is %v, want no value: the range holds days the walk has not built",
							what, item.Definition.EvalID, series.Series.Series, statistic, name, *value)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s: the answer holds no eval whose rows a walk builds, and the check needs them", what)
	}
	return checked
}

// listItems is the evals a list body holds.
func listItems(t *testing.T, body json.RawMessage) []evalItem {
	t.Helper()
	var list evalListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("read the evals body: %v", err)
	}
	items := make([]evalItem, len(list.Items))
	for index, item := range list.Items {
		items[index] = item.evalItem
	}
	return items
}

// rangeQuery is the query of a read over the UTC days [from, to), and the compare range
// [compareFrom, compareTo) where compareTo is not the zero time.
func rangeQuery(from, to, compareFrom, compareTo time.Time) string {
	query := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	if !compareTo.IsZero() {
		query.Set("compare_from", compareFrom.Format(time.RFC3339))
		query.Set("compare_to", compareTo.Format(time.RFC3339))
	}
	return query.Encode()
}

// TestAnEvalsPageOverDaysTheWalkHasNotBuiltGetsNoNumberUntilItHas (R12, dispatch://INFRA-2916's
// k12): over thirty days of the delivery history, the refresher's walks stop twelve days before the
// history's last day (a refresh as of that day), so the stored rows are whole through it alone. A
// page subscribed to the project's evals over the 28 days ending on the history's last day gets,
// for every eval whose rows a walk builds, no number over its window: the window holds days before
// today the walk has not built, and a fold of the days it has would be a part of the window read as
// the whole. So do the list route, the read across projects and the one eval's read of that window,
// and of a compare range that runs past the built days, while their window of built days answers
// its values. The walks go on six days, the window still running past them, and the page is sent
// no number; once they have built every day, the page gets one, its body the route's. The hub reads
// its evals view through the list route's own function (readProjectEvalList), which states every
// eval's values through evals.Built.Withhold, so this is the view's one case of the rule.
func TestAnEvalsPageOverDaysTheWalkHasNotBuiltGetsNoNumberUntilItHas(t *testing.T) {
	t.Parallel()
	handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{})
	fixture := deliverytest.Seed(t, database.Pool, deliveryseed.Spec{Seed: 2916, OneProject: true, Days: 30})
	end := fixture.To.Add(-time.Minute)
	today := measures.Midnight(end)
	walked := today.AddDate(0, 0, -12)
	page := &refreshedEvalsPage{conn: newFakeLiveConn(), fixture: fixture, now: walked.Add(12 * time.Hour),
		registry: evals.New(database, deps.Docs, deps.Events, deps.Attributor)}
	page.rig = hubOn(t, handler, database, deps, newStillHubClock(), nil)
	page.refresh(t)
	page.frame = evalsLiveView(fixture.Project, evalsDays(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1)), "")
	page.rig.hub.Subscribe(page.conn, subscribeFrame("e", page.frame))
	page.held = page.conn.snapshot(t, "e").Body
	page.rig.settle(t)
	page.rig.quiesce(t, page.frame)
	page.take(t)
	page.requireAtRoute(t, "the snapshot over days the walk has not built")
	checked := requireNoNumber(t, "the page's snapshot, the walks through "+walked.Format(time.DateOnly), listItems(t, page.held))

	// The routes' reads of the same days, and of the built days against a compare range past them.
	project := "/api/v1/projects/" + fixture.Project + "/evals"
	past := rangeQuery(today.AddDate(0, 0, -27), today.AddDate(0, 0, 1), time.Time{}, time.Time{})
	requireNoNumber(t, "the cross-project read", listItems(t, getBody(t, handler, "/api/v1/evals?project="+fixture.Project+"&"+past)))
	detail := evalGet[evalDetailResponse](t, handler, project+"/"+evals.SlugDeployFrequency+"?"+past)
	requireNoNumber(t, "deploy-frequency's read", []evalItem{detail.Items[0].evalItem})
	compared := evalGet[evalListResponse](t, handler, project+"?"+rangeQuery(walked.AddDate(0, 0, -6), walked.AddDate(0, 0, 1), walked.AddDate(0, 0, -3), walked.AddDate(0, 0, 4)))
	deploys := seriesValues(t, findEval(t, compared.Items, fixture.Project, evals.SlugDeployFrequency), measures.SeriesDeploys)
	if deploys.Window["deploys"] == nil || *deploys.Window["deploys"] <= 0 || deploys.Compare["deploys"] != nil || deploys.Change["deploys"] != nil {
		t.Errorf("deploy-frequency over the built week against a compare range past them reads deploys %v against %v, change %v; want a count, then no value twice",
			showFigure(deploys.Window["deploys"]), showFigure(deploys.Compare["deploys"]), showFigure(deploys.Change["deploys"]))
	}

	page.now = today.AddDate(0, 0, -6).Add(12 * time.Hour)
	page.refresh(t)
	page.rig.quiesce(t, page.frame)
	sent := page.take(t)
	page.requireAtRoute(t, "the walks six days on")
	requireNoNumber(t, "the page, the walks through "+today.AddDate(0, 0, -6).Format(time.DateOnly), listItems(t, page.held))

	page.now = end
	page.refresh(t)
	page.rig.quiesce(t, page.frame)
	sent += page.take(t)
	page.requireAtRoute(t, "the walks through the history's last day")
	var held evalListResponse
	if err := json.Unmarshal(page.held, &held); err != nil {
		t.Fatalf("read the evals body: %v", err)
	}
	settled := seriesValues(t, findEval(t, held.Items, fixture.Project, evals.SlugDeployFrequency), measures.SeriesDeploys)
	if settled.Window["deploys"] == nil || *settled.Window["deploys"] <= 0 {
		t.Errorf("once the walks built every day the page reads deploy-frequency's deploys %s, want the window's count", showFigure(settled.Window["deploys"]))
	}
	t.Logf("%d walked evals answered no number while the walks stood at %s and %s; %d deltas later the page reads %s deploys, the route's body",
		checked, walked.Format(time.DateOnly), today.AddDate(0, 0, -6).Format(time.DateOnly), sent, showFigure(settled.Window["deploys"]))
}
