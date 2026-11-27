package api

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
)

// TestARecordWriteMarksNoViewAndTheRefreshersWriteMarksTheEvalsViewsOfItsProjects
// (dispatch://INFRA-978, R13): through the change log, a deploy row of ALPHA's repository
// that ships ALPHA-1's and OMEGA-1's pull requests, a failure posted on it and that failure's
// reject each log one production_events change and recompute no view at all: no view reads the
// record, and touchedBy's production_events arm marks none. The refresher's pass that follows each
// writes the failure evals' day rows of the projects the deploy counts for, ALPHA (its repository's
// owner, and ALPHA-1's) and OMEGA (OMEGA-1's), and their eval_days changes recompute the evals
// views of those two projects and of no other (DELTA's, whose failure measures are on and which
// the deploy does not count for, among them), and the timeline, which folds the failure evals' day
// rows of the one delivery-on project (evals.Store.DeploymentFailures) and so is marked by a write
// of them in any project. Each of the two evals pages is sent the failure evals the write moved,
// and no other page is sent anything: with three delivery-on projects the timeline folds none.
func TestARecordWriteMarksNoViewAndTheRefreshersWriteMarksTheEvalsViewsOfItsProjects(t *testing.T) {
	rig := newHubRig(t, nil)
	seedHubSettings(t, rig.database)
	ctx, pool := t.Context(), rig.database.Pool
	now := rig.clock.Now()
	yesterday := measures.Midnight(now).AddDate(0, 0, -1)
	since := yesterday.AddDate(0, 0, -1)
	if _, err := pool.Exec(ctx, `insert into projects (key, name, default_assignee, next_number, default_repo)
		values ('ALPHA', 'Alpha', 'alice', 2, 'acme/alpha-app'), ('OMEGA', 'Omega', 'alice', 2, 'acme/omega-app'),
		       ('DELTA', 'Delta', 'alice', 1, 'acme/delta-app')`); err != nil {
		t.Fatalf("seed the projects: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into issues (key, project_key, number, title, status, rank, repo, created_by, created_at)
		values ('ALPHA-1', 'ALPHA', 1, 'Alpha work', 'in_progress', '00000001', 'acme/alpha-app', '{"kind":"system","id":"test"}', $1),
		       ('OMEGA-1', 'OMEGA', 1, 'Omega work', 'in_progress', '00000001', 'acme/alpha-app', '{"kind":"system","id":"test"}', $1)`,
		since); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, project := range []string{"ALPHA", "OMEGA", "DELTA"} {
		saveProjectDelivery(t, pool, delivery.ProjectSettingsInput{
			Project: project, Delivery: true,
			Sources: []delivery.SourceInput{{
				Kind: delivery.SourceKindGitHubWorkflow, DeployWorkflowPath: new(".github/workflows/deploy.yml"),
				ProductionJobNames: []string{"deploy / production"}, PRChecksWorkflowPath: new(".github/workflows/pr-checks.yml"),
			}},
			PopulationAuthors: []string{"octocat"}, ExcludedRepos: []string{},
			FailureSources: delivery.FailureSources, FailuresMeasuredSince: &since,
		})
	}
	for number, issue := range []string{"ALPHA-1", "OMEGA-1"} {
		pr := hubPR(number+1, yesterday.Add(8*time.Hour))
		pr.Repo, pr.URL, pr.IssueKey = "acme/alpha-app", fmt.Sprintf("https://github.com/acme/alpha-app/pull/%d", number+1), new(issue)
		upsertPR(t, rig.database, pr)
	}
	registry := rig.hub.server.registry()
	refreshEvalDays(t, registry, rig.database, now)

	frames := map[string]DeliveryLiveView{
		"t": presetView(deliveryLiveTimeline, 7, ""),
		"a": evalsLiveView("ALPHA", DeliveryLiveWindow{Preset: new(28)}, ""),
		"o": evalsLiveView("OMEGA", DeliveryLiveWindow{Preset: new(28)}, ""),
		"d": evalsLiveView("DELTA", DeliveryLiveWindow{Preset: new(28)}, ""),
	}
	subs := []string{"a", "d", "o", "t"}
	conns := map[string]*fakeLiveConn{}
	subOf := map[string]string{}
	for _, sub := range []string{"t", "a", "o", "d"} {
		spec, refusal := parseDeliveryLiveView(frames[sub])
		if refusal != nil {
			t.Fatalf("read the view of %s: %+v", sub, refusal)
		}
		subOf[spec.key] = sub
		conns[sub] = newFakeLiveConn()
		rig.hub.Subscribe(conns[sub], subscribeFrame(sub, frames[sub]))
		conns[sub].snapshot(t, sub)
		if sub != "t" {
			conns[sub].freshness(t, "the verdict after the snapshot of "+sub)
		}
	}
	rig.settle(t)

	// step writes, and requires the change kinds the write logged and the views recomputed after
	// it: each named one once its recompute has begun, and every one once the hub has gone quiet.
	// It answers how many change rows the write logged.
	step := func(what string, write func(), wantKinds, wantRecomputed []string) int {
		t.Helper()
		before, mark := latestChange(t, rig.database), rig.mark()
		write()
		// The refresher's day rows wait in the change log's outbox; the hub drains before it reads,
		// and so does this read.
		if _, err := delivery.DrainPending(ctx, pool); err != nil {
			t.Fatalf("%s: drain the change log's outbox: %v", what, err)
		}
		changes, err := delivery.ChangesSince(ctx, pool, before, 1000)
		if err != nil {
			t.Fatalf("%s: read the changes it logged: %v", what, err)
		}
		var kinds []string
		for _, change := range changes {
			if !slices.Contains(kinds, change.Kind) {
				kinds = append(kinds, change.Kind)
			}
		}
		if !slices.Equal(kinds, wantKinds) {
			t.Fatalf("%s logged the kinds %v, want %v", what, kinds, wantKinds)
		}
		rig.hub.catchUpNow()
		for _, sub := range wantRecomputed {
			rig.waitFor(t, mark, "the recompute of "+sub+" after "+what, recomputeOfView(t, frames[sub]))
		}
		rig.waitNoRecompute(t, viewMaxWait)
		recomputed := []string{}
		rig.mu.Lock()
		for _, note := range rig.notes[mark:] {
			if note.what == "recompute begin" && !slices.Contains(recomputed, subOf[note.view]) {
				recomputed = append(recomputed, subOf[note.view])
			}
		}
		rig.mu.Unlock()
		slices.Sort(recomputed)
		if !slices.Equal(recomputed, wantRecomputed) {
			t.Errorf("%s recomputed the views of %v, want %v", what, recomputed, wantRecomputed)
		}
		t.Logf("%s logged %d changes of %v and recomputed the views of %v", what, len(changes), kinds, recomputed)
		return len(changes)
	}
	// sent reads the one delta each sub of want was sent, requiring the evals it upserts, and
	// requires that every other sub was sent nothing.
	sent := func(what string, want map[string][]string) {
		t.Helper()
		for _, sub := range subs {
			slugs, ok := want[sub]
			if !ok {
				conns[sub].none(t, 100*time.Millisecond, what)
				continue
			}
			project := *frames[sub].Project
			ids := make([]string, len(slugs))
			for i, slug := range slugs {
				ids[i] = evals.EvalID(project, slug)
			}
			got := liveUpsertIDs(t, conns[sub].delta(t, sub), "items")
			slices.Sort(got)
			slices.Sort(ids)
			if !slices.Equal(got, ids) {
				t.Errorf("%s sent %s the evals %v, want %v", what, sub, got, ids)
			}
		}
	}

	record := []string{delivery.ChangeKindProductionEvents}
	days := []string{delivery.ChangeKindEvalDays}
	withTimeline := []string{"a", "o", "t"}
	refresh := func() { refreshEvalDays(t, registry, rig.database, rig.clock.Now()) }

	var deploy, failure prodevents.Event
	logged := step("the GitHub adapter's deploy row", func() {
		finished := yesterday.Add(10 * time.Hour)
		deploy = writeProductionEvent(t, pool, prodevents.Event{
			ProjectKey: "ALPHA", Kind: prodevents.KindDeploy, Status: prodevents.StatusSucceeded, Source: prodevents.SourceGitHub,
			SourceRef: prodevents.GitHubSourceRef(1), StartedAt: new(finished.Add(-10 * time.Minute)), FinishedAt: &finished,
			Repo: "acme/alpha-app", PullRequests: []string{"acme/alpha-app#1", "acme/alpha-app#2"},
		})
	}, record, nil)
	sent("the deploy row", nil)
	step("the refresher's pass after the deploy row", refresh, days, withTimeline)
	sent("the refresher's pass after the deploy row", map[string][]string{
		"a": {evals.SlugChangeFailRate, evals.SlugDeploymentReworkRate},
		"o": {evals.SlugChangeFailRate, evals.SlugDeploymentReworkRate},
	})

	logged += step("a failure posted on the deploy", func() {
		failure = writeProductionEvent(t, pool, prodevents.Event{
			ProjectKey: "ALPHA", Kind: prodevents.KindFailure, Source: prodevents.SourceAPI, SourceRef: "page-1",
			DeployID: new(deploy.ID), Signal: new(prodevents.SignalAPI), DetectedAt: new(yesterday.Add(11 * time.Hour)),
		})
	}, record, nil)
	sent("the failure row", nil)
	step("the refresher's pass after the failure", refresh, days, withTimeline)
	sent("the refresher's pass after the failure", map[string][]string{
		"a": {evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery},
		"o": {evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery},
	})

	logged += step("the failure's reject", func() {
		if _, err := prodevents.SetState(ctx, pool, "ALPHA", failure.ID, prodevents.StateChange{
			State: prodevents.StateRejected, Reason: "the page was a test", Actor: model.Actor{Kind: "user", ID: "alice"}, At: rig.clock.Now(),
		}); err != nil {
			t.Fatalf("reject the failure: %v", err)
		}
	}, record, nil)
	sent("the reject", nil)
	step("the refresher's pass after the reject", refresh, days, withTimeline)
	sent("the refresher's pass after the reject", map[string][]string{
		"a": {evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery},
		"o": {evals.SlugChangeFailRate, evals.SlugFailedDeployRecovery},
	})
	if logged != 3 {
		t.Errorf("the deploy, the failure and the reject logged %d production_events changes, want one each", logged)
	}
}
