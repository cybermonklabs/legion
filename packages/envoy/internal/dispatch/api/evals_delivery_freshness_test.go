package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
)

// freshnessJSON is freshness as an answer writes it, for a failure's message.
func freshnessJSON(t *testing.T, freshness any) string {
	t.Helper()
	encoded, err := json.Marshal(freshness)
	if err != nil {
		t.Fatalf("encode the freshness: %v", err)
	}
	return string(encoded)
}

// sameFreshness is whether got states want: the same instants, error and count.
func sameFreshness(got *delivery.DeliveryFreshnessView, want delivery.DeliveryFreshnessView) bool {
	sameTime := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	return got != nil && sameTime(got.LastEventAt, want.LastEventAt) && sameTime(got.LastReconcileAt, want.LastReconcileAt) &&
		equalPointers(got.LastError, want.LastError) && got.UnfetchableCount == want.UnfetchableCount
}

// TestTheDeliveryEvalsAnswerCarriesTheProjectsFreshness: the list read of a project's delivery
// evals answers delivery_freshness, its deploy sources' freshness folded by delivery.FreshnessOf,
// the one function the delivery timeline's freshness is folded by (dispatch://INFRA-2918, D2). With
// the deployment's one source it states the timeline's freshness, its error bare. With
// two it is the newest event, the oldest reconcile (null while one of them never reconciled) and
// the error of each failing source prefixed with its repository, in the sources' order, where the
// timeline keeps its own answer in the repositories' order; and its unfetchable_count counts the
// project's own unfetchable pull requests, where the timeline's counts the deployment's. Every
// other read answers null: another kind or none, a project with no deploy source, and the read
// across projects.
func TestTheDeliveryEvalsAnswerCarriesTheProjectsFreshness(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx, pool := t.Context(), database.Pool
	own := createInteractionIssue(t, handler, deliveryRuleProject, "Widgets", "The widgets.")
	widgets := delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"release / release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}
	saveDeliveryRule(t, pool, widgets)
	reconciled := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if err := delivery.RecordEventAt(ctx, pool, widgets.DeployRepo); err != nil {
		t.Fatalf("record an event: %v", err)
	}
	if err := delivery.RecordReconcileSuccess(ctx, pool, widgets.DeployRepo, reconciled); err != nil {
		t.Fatalf("record a reconcile: %v", err)
	}
	if err := delivery.RecordReconcileError(ctx, pool, widgets.DeployRepo, "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	unfetchable := func(repo string, number int, issue string) {
		t.Helper()
		pr := hubPR(number, reconciled.Add(-time.Hour))
		pr.Repo, pr.URL, pr.IssueKey = repo, "https://github.com/"+repo+"/pull/"+strconv.Itoa(number), &issue
		upsertPR(t, database, pr)
		if err := delivery.MarkPullRequestUnfetchable(ctx, pool, repo, number, "the pull request is gone"); err != nil {
			t.Fatalf("mark %s#%d unfetchable: %v", repo, number, err)
		}
	}
	unfetchable(widgets.DeployRepo, 1, own.Key)

	timeline := func() delivery.DeliveryFreshnessView {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET the timeline: status %d, body %s", response.Code, response.Body.String())
		}
		return decodeBody[delivery.DeliveryTimelineResponse](t, response).Freshness
	}
	listed := func(target string) *delivery.DeliveryFreshnessView {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", target, response.Code, response.Body.String())
		}
		return decodeBody[evalListResponse](t, response).DeliveryFreshness
	}
	deliveryEvals := "/api/v1/projects/" + deliveryRuleProject + "/evals?kind=delivery"

	one, base := listed(deliveryEvals), timeline()
	if !sameFreshness(one, base) {
		t.Fatalf("one source: delivery_freshness = %s, want the timeline's freshness %s", freshnessJSON(t, one), freshnessJSON(t, base))
	}
	if one.LastEventAt == nil || one.LastReconcileAt == nil || !one.LastReconcileAt.Equal(reconciled) ||
		one.LastError == nil || *one.LastError != "GitHub answered 502" || one.UnfetchableCount != 1 {
		t.Fatalf("one source: delivery_freshness = %s, want its event, its reconcile at %s, its bare error and its one unfetchable pull request",
			freshnessJSON(t, one), reconciled.Format(time.RFC3339))
	}
	t.Logf("one source: delivery_freshness is the timeline's %s", freshnessJSON(t, base))

	// A second source after the first: acme/gadgets, which sorts before acme/widgets, so the
	// sources' order and the repositories' differ. The save keeps acme/widgets' row and its
	// bookkeeping; each source's is then set outright.
	gadgets := widgets
	gadgets.DeployRepo = "acme/gadgets"
	if _, err := deliverytest.SaveRules(ctx, pool, deliveryRuleProject, widgets, gadgets); err != nil {
		t.Fatalf("save the two sources: %v", err)
	}
	bookkeeping := func(repo string, event, reconcile *time.Time, lastError *string) {
		t.Helper()
		if tag, err := pool.Exec(ctx, `update project_deploy_sources set last_event_at = $3, last_reconcile_at = $4, last_error = $5
			where project_key = $1 and repo = $2`, deliveryRuleProject, repo, event, reconcile, lastError); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("set the bookkeeping of %s: %v (%d rows)", repo, err, tag.RowsAffected())
		}
	}
	at := func(hour int) *time.Time { return new(reconciled.Add(time.Duration(hour) * time.Hour)) }
	bookkeeping(widgets.DeployRepo, at(1), at(0), new("GitHub answered 502"))
	bookkeeping(gadgets.DeployRepo, at(2), nil, new("the installation lacks Actions: read"))
	for _, step := range []struct {
		what                string
		event, reconcile    *time.Time
		lastError, timeline string
	}{{
		what:  "two sources, one never reconciled",
		event: at(2), reconcile: nil,
		lastError: "acme/widgets: GitHub answered 502; acme/gadgets: the installation lacks Actions: read",
		timeline:  "acme/gadgets: the installation lacks Actions: read; acme/widgets: GitHub answered 502",
	}, {
		what:  "two sources, both reconciled",
		event: at(2), reconcile: at(-1),
		lastError: "acme/widgets: GitHub answered 502",
		timeline:  "acme/widgets: GitHub answered 502",
	}} {
		if step.reconcile != nil {
			bookkeeping(gadgets.DeployRepo, at(2), step.reconcile, nil)
		}
		want := delivery.DeliveryFreshnessView{LastEventAt: step.event, LastReconcileAt: step.reconcile, LastError: &step.lastError, UnfetchableCount: 1}
		if got := listed(deliveryEvals); !sameFreshness(got, want) {
			t.Errorf("%s: delivery_freshness = %s, want %s", step.what, freshnessJSON(t, got), freshnessJSON(t, want))
		}
		// The deployment's timeline folds the same sources by repository, as it did before.
		if base := timeline(); base.LastError == nil || *base.LastError != step.timeline {
			t.Errorf("%s: the timeline's last_error = %v, want %q", step.what, base.LastError, step.timeline)
		}
	}

	// Another project's unfetchable pull request is the deployment's, not this project's.
	other := createInteractionIssue(t, handler, "OTHER", "Other", "Another project's issue.")
	unfetchable("acme/other", 2, other.Key)
	if got := listed(deliveryEvals); got == nil || got.UnfetchableCount != 1 {
		t.Errorf("delivery_freshness after another project's unfetchable pull request = %s, want unfetchable_count 1", freshnessJSON(t, got))
	}
	if base := timeline(); base.UnfetchableCount != 2 {
		t.Errorf("the timeline's unfetchable_count = %d, want the deployment's 2", base.UnfetchableCount)
	}

	for _, target := range []string{
		"/api/v1/projects/" + deliveryRuleProject + "/evals?kind=legion",
		"/api/v1/projects/" + deliveryRuleProject + "/evals",
		"/api/v1/projects/OTHER/evals?kind=delivery",
		"/api/v1/evals?kind=delivery&project=" + deliveryRuleProject,
	} {
		if got := listed(target); got != nil {
			t.Errorf("GET %s: delivery_freshness = %s, want null", target, freshnessJSON(t, got))
		}
	}
}

// TestASourcesBookkeepingMovesItsProjectsDeliveryEvalsView: the write of a deploy source's
// bookkeeping logs no change, so its delivery_freshness notification is what marks the evals view
// of the source's project's delivery evals (applyFreshness): the view is sent the delta that sets
// its new delivery_freshness, which lands on the route's body, and the project's view of another
// kind, which answers none, is sent nothing.
func TestASourcesBookkeepingMovesItsProjectsDeliveryEvalsView(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	frames := map[string]DeliveryLiveView{
		"d": evalsLiveView(deliveryRuleProject, DeliveryLiveWindow{Preset: new(28)}, "kind=delivery"),
		"l": evalsLiveView(deliveryRuleProject, DeliveryLiveWindow{Preset: new(28)}, "kind=legion"),
	}
	conns := map[string]*fakeLiveConn{}
	snapshots := map[string]json.RawMessage{}
	for _, sub := range []string{"d", "l"} {
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		snapshots[sub] = conns[sub].snapshot(t, sub).Body
	}
	rig.waitConnected(t)
	var held evalListResponse
	if err := json.Unmarshal(snapshots["d"], &held); err != nil {
		t.Fatalf("decode the delivery view's snapshot: %v", err)
	}
	if held.DeliveryFreshness == nil || held.DeliveryFreshness.LastError != nil {
		t.Fatalf("the delivery view's snapshot holds delivery_freshness %s, want its source's, with no error", freshnessJSON(t, held.DeliveryFreshness))
	}

	if err := delivery.RecordReconcileError(t.Context(), rig.database.Pool, "acme/widgets", "GitHub answered 502"); err != nil {
		t.Fatalf("record a reconcile error: %v", err)
	}
	delta := conns["d"].delta(t, "d")
	if _, set := delta.Set["delivery_freshness"]; !set {
		t.Fatalf("the bookkeeping's delta sets %v, want delivery_freshness among them", delta.Set)
	}
	applied := applyLiveDelta(t, evalsView, snapshots["d"], delta)
	var moved evalListResponse
	if err := json.Unmarshal(applied, &moved); err != nil {
		t.Fatalf("decode the delivery view's body after the delta: %v", err)
	}
	if moved.DeliveryFreshness == nil || moved.DeliveryFreshness.LastError == nil || *moved.DeliveryFreshness.LastError != "GitHub answered 502" {
		t.Fatalf("after the delta delivery_freshness = %s, want last_error \"GitHub answered 502\"", freshnessJSON(t, moved.DeliveryFreshness))
	}
	route := mustEncodeLive(t, evalsView, getBody(t, rig.handler, evalsTarget(t, deliveryRuleProject, applied, "kind=delivery")))
	if got, want := liveComparable(t, mustEncodeLive(t, evalsView, applied)), liveComparable(t, route); !reflect.DeepEqual(got, want) {
		t.Fatalf("the delta does not land on the route's body:\napplied: %v\nGET:     %v", got, want)
	}
	conns["l"].none(t, time.Second, "the legion view, which answers no delivery freshness")
	conns["d"].none(t, time.Second, "the bookkeeping's one delta")
}
