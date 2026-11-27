package delivery

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

func TestADeployIsRecordedOnceForEachProductionRunAndMovedByItsReRun(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	issue := seedHeadIssue(t, ctx, pool, 880)
	var artifact string
	if err := pool.QueryRow(ctx, `insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, $2, 'deploy-check', 'deploy.md', 'doc', '{"kind":"system","id":"test"}') returning id`, issue, deliveryProject).Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
		check_version, check_sha256, role, commit_sha, verdict, actor, check_subject, check_program)
		values ($1, $1, $2, 'live', 1, 'digest', 'tester', $3, 'pass', '{"kind":"system","id":"test"}', 'production', 'delivery')`, issue, artifact, fmt.Sprintf("%040d", 100)); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for _, id := range []int64{100, 200} {
		seedDeployRun(t, ctx, pool, id, base, base.Add(time.Duration(id/100)*time.Hour), DeliveryJobConclusionSuccess)
	}
	broker := events.NewBroker()
	pass := func(want int) {
		t.Helper()
		got, err := RecordProductionDeploys(ctx, pool, settings, broker)
		if err != nil || got != want {
			t.Fatalf("record production deploys = %d, %v, want %d", got, err, want)
		}
	}
	current := func(commit string, at time.Time) {
		t.Helper()
		deploys, err := CurrentDeploys(ctx, pool, "acme/widgets")
		if err != nil || len(deploys) != 1 {
			t.Fatalf("current deploys = %+v, %v", deploys, err)
		}
		got := deploys[0]
		if got.Program != "delivery" || got.Commit != commit || !got.DeployedAt.Equal(at) || got.Source != "delivery_run" || got.RecordedBy != DeployedEventActor {
			t.Fatalf("current deploy = %+v, want %s since %s and the delivery writer", got, commit, at)
		}
	}
	countEvents := func(want int) {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `select count(*) from events where type = 'deploy.recorded' and issue_key = $1`, issue).Scan(&count); err != nil || count != want {
			t.Fatalf("deploy events = %d, %v, want %d", count, err, want)
		}
	}
	pass(2)
	current(fmt.Sprintf("%040d", 200), base.Add(2*time.Hour))
	countEvents(0)
	pass(0)
	seedDeployRun(t, ctx, pool, 300, base, base.Add(3*time.Hour), DeliveryJobConclusionSuccess)
	pass(1)
	current(fmt.Sprintf("%040d", 300), base.Add(3*time.Hour))
	countEvents(1)
	seedDeployRun(t, ctx, pool, 100, base, base.Add(4*time.Hour), DeliveryJobConclusionSuccess)
	if _, err := pool.Exec(ctx, `update delivery_runs set run_attempt = 2 where repo = 'acme/widgets' and run_id = 100`); err != nil {
		t.Fatal(err)
	}
	pass(1)
	current(fmt.Sprintf("%040d", 100), base.Add(4*time.Hour))
	countEvents(2)
	var attempt *int
	if err := pool.QueryRow(ctx, `select run_attempt from deploys where source = 'delivery_run' and run_id = 100`).Scan(&attempt); err != nil || attempt == nil || *attempt != 2 {
		t.Fatalf("rerun attempt = %v, %v, want 2", attempt, err)
	}
	seedDeployRun(t, ctx, pool, 100, base, base.Add(5*time.Hour), DeliveryJobConclusionSuccess)
	pass(1)
	current(fmt.Sprintf("%040d", 100), base.Add(4*time.Hour))
	countEvents(2)
	seedDeployRun(t, ctx, pool, 400, base, base.Add(6*time.Hour), DeliveryJobConclusionSuccess)
	if _, err := pool.Exec(ctx, `update delivery_runs set head_sha = $1 where repo = 'acme/widgets' and run_id = 400`, fmt.Sprintf("%040d", 100)); err != nil {
		t.Fatal(err)
	}
	pass(1)
	current(fmt.Sprintf("%040d", 100), base.Add(4*time.Hour))
	countEvents(2)
	pass(0)
	// A later different build must not make either already-observed same-build rerun new again.
	seedDeployRun(t, ctx, pool, 500, base, base.Add(7*time.Hour), DeliveryJobConclusionSuccess)
	pass(1)
	current(fmt.Sprintf("%040d", 500), base.Add(7*time.Hour))
	countEvents(3)
	pass(0)
	countEvents(3)
}

func TestAWorkflowRunKeepsItsKnownOrUnknownAttempt(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, attempt string
		want          *int
	}{
		{"absent", "", nil}, {"second attempt", `,"run_attempt":2`, new(2)},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			settings := seedDeliverySettings(t, ctx, pool)
			var item workflowRunItem
			body := fmt.Sprintf(`{"id":700,"head_sha":%q,"head_branch":"main","created_at":"2026-11-20T00:00:00Z"%s}`, fmt.Sprintf("%040d", 700), test.attempt)
			if err := json.Unmarshal([]byte(body), &item); err != nil {
				t.Fatal(err)
			}
			run := StoredRun("acme/widgets", DeliveryRunKindDeploy, fetchedRunFromItem(item))
			if err := UpsertRun(ctx, pool, run); err != nil {
				t.Fatal(err)
			}
			stored, err := ScanRun(pool.QueryRow(ctx, `select `+RunColumns+` from delivery_runs where repo = 'acme/widgets' and run_id = 700`))
			if err != nil {
				t.Fatal(err)
			}
			if (stored.RunAttempt == nil) != (test.want == nil) || test.want != nil && *stored.RunAttempt != *test.want {
				t.Fatalf("stored attempt = %v, want %v", stored.RunAttempt, test.want)
			}
			completed := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
			if err := UpsertRunJobs(ctx, pool, "acme/widgets", 700, []DeliveryRunJob{{
				Repo: "acme/widgets", RunID: 700, Name: settings.Production().JobNames[0], CompletedAt: &completed, Conclusion: new(DeliveryJobConclusionSuccess),
			}}, true); err != nil {
				t.Fatal(err)
			}
			if _, err := RecordProductionDeploys(ctx, pool, settings, events.NewBroker()); err != nil {
				t.Fatal(err)
			}
			deploys, err := CurrentDeploys(ctx, pool, "acme/widgets")
			if err != nil || len(deploys) != 1 {
				t.Fatalf("deploy record = %+v, %v", deploys, err)
			}
			got := deploys[0].RunAttempt
			if (got == nil) != (test.want == nil) || test.want != nil && *got != *test.want {
				t.Fatalf("deploy attempt = %v, want %v", got, test.want)
			}
			wire, err := json.Marshal(deploys[0])
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			value, present := decoded["run_attempt"]
			if !present || test.want == nil && value != nil || test.want != nil && value != float64(*test.want) {
				t.Fatalf("daemon's run_attempt field = %s, want explicit null or attempt 2", wire)
			}
		})
	}
}
