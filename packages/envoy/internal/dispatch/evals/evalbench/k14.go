package evalbench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/prodevents"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The k14 load (dispatch://INFRA-978, T19.9; Spec.K14) is the fixture above with two more projects
// at INFRA's size, each with INFRA's issues, events, handoffs and observations (writeProject) and
// a delivery history at INFRA's rate for each of its deploy repositories (writeDelivery), which
// k14's three writes and two first paints read:
//
//   - PAIR, with two github_workflow sources: the first names no repository and reads its default
//     repository, acme/pair-deploy; the second names acme/pair-service. Each holds INFRA's deploy
//     and pull-request runs. Its failures are its merged reverts (failure source reverts): about one
//     pull request in thirty of each deploy repository is reverted hours after it merged
//     (writeReverts), which the production signals make failures of once the deploys that ship
//     them have their rows.
//   - MIXED, with one github_workflow source naming no repository, which reads acme/mixed-deploy and
//     its runs at INFRA's rate, and one events source, whose deploys a system posted to its route:
//     as many a day as INFRA's deploy runs that reach production, about one in twelve of them
//     blamed by a posted failure (failure source events), nine in ten of those restored hours later
//     (writePosted).
//
// Each project counts its failures from failureDays before the history's end. The repositories a
// write moves to, PairSpare and MixedSpare, hold no run: a source moved there reads nothing until
// the reconcile imports the repository, and moved back it reads the rows kept under its rule. The
// base and the head paint of the fixture (k2, k8, k13, k16) load none of this, so their population
// is the same with or without it.

// The k14 projects.
const (
	PairProject  = "PAIR"
	MixedProject = "MIXED"
)

// K14Projects is the projects the k14 load adds, in the order it writes them.
var K14Projects = []string{PairProject, MixedProject}

// The k14 projects' repositories, each project's default first, and the repositories the moves of
// k14 move to, which hold no run.
var (
	PairRepos  = [3]string{"acme/pair-deploy", "acme/pair-service", "acme/pair-web"}
	MixedRepos = [3]string{"acme/mixed-deploy", "acme/mixed-api", "acme/mixed-web"}
)

const (
	PairSpare  = "acme/pair-spare"
	MixedSpare = "acme/mixed-spare"
)

// The production job names k14's rule change moves a source between: the deploy workflow's
// production apply, which about three in four deploy runs pass (deployJobsOf), and the production
// plan before it, which the runs that fail or are cancelled in the apply (about one in twelve) pass
// too, so at INFRA's rate the change moves the deploys of nearly every day.
const (
	ProductionJob = productionJob
	RuleChangeJob = productionPlanJob
)

// failureDays is how many days before the history's end each k14 project counts its failures
// from (failures_measured_since).
const failureDays = 180

// k14RunIDStride is how far apart the first run ids of the k14 deploy repositories are, after
// INFRA's (firstRunID on): more than the runs of one repository at ten times the rate (runDays a
// day of dailyRuns, 160,000), and the last of them ends before FailureDayFirstRunID.
const k14RunIDStride = 200_000

// revertShare is the share of a PAIR deploy repository's pull requests a revert reverts, and
// failedShare the share of MIXED's posted deploys a posted failure blames, nine in ten of them
// restored (restoredShare).
const (
	revertShare   = 1.0 / 30
	failedShare   = 1.0 / 12
	restoredShare = 0.9
)

// k14Fixtures is the two k14 projects as the load writes them.
var k14Fixtures = []projectFixture{
	{
		key: PairProject, name: "Two deploy sources", repos: PairRepos, stream: PairProject,
		deploys: []deployRepo{{repo: PairRepos[0], firstRunID: firstRunID + k14RunIDStride}, {repo: PairRepos[1], firstRunID: firstRunID + 2*k14RunIDStride}},
		sources: func(spec Spec) delivery.ProjectSettingsInput {
			return k14Settings(spec, PairProject, delivery.FailureSourceReverts, workflowSource(nil), workflowSource(new(PairRepos[1])))
		},
	},
	{
		key: MixedProject, name: "Deploy source and posted events", repos: MixedRepos, stream: MixedProject,
		deploys: []deployRepo{{repo: MixedRepos[0], firstRunID: firstRunID + 3*k14RunIDStride}},
		sources: func(spec Spec) delivery.ProjectSettingsInput {
			return k14Settings(spec, MixedProject, delivery.FailureSourceEvents, workflowSource(nil), delivery.SourceInput{Kind: delivery.SourceKindEvents})
		},
	},
}

// workflowSource is a github_workflow source under Settings' rule, reading repo (nil: the
// project's default repository).
func workflowSource(repo *string) delivery.SourceInput {
	settings := Settings()
	return delivery.SourceInput{
		Kind: delivery.SourceKindGitHubWorkflow, Repo: repo, DeployWorkflowPath: new(settings.DeployWorkflowPath),
		ProductionJobNames: slices.Clone(settings.ProductionJobNames), PRChecksWorkflowPath: new(settings.PRChecksWorkflowPath),
		StagingE2EJobName: settings.StagingE2EJobName, StagingApplyJobName: settings.StagingApplyJobName,
		PRChecksRollupJobName: settings.PRChecksRollupJobName,
	}
}

// k14Settings is a k14 project's settings: delivery on, sources, INFRA's population, and its
// failures from failure counted from failureDays before the history's end.
func k14Settings(spec Spec, project, failure string, sources ...delivery.SourceInput) delivery.ProjectSettingsInput {
	since := spec.Now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -failureDays)
	return delivery.ProjectSettingsInput{
		Project: project, Delivery: true, Sources: sources, PopulationAuthors: slices.Clone(authors), ExcludedRepos: []string{},
		FailureSources: []string{failure}, FailuresMeasuredSince: &since,
	}
}

// saveSettings saves input through delivery.PutProjectSettings in a transaction of its own, as the
// settings route and evalfixture do, and drains the change rows it logged once it has committed.
func saveSettings(ctx context.Context, pool *store.Pool, input delivery.ProjectSettingsInput) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the delivery settings of %s: %w", input.Project, err)
	}
	defer tx.Rollback(ctx)
	if _, err := delivery.PutProjectSettings(ctx, tx, input, actor); err != nil {
		return fmt.Errorf("save the delivery settings of %s: %w", input.Project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the delivery settings of %s: %w", input.Project, err)
	}
	delivery.DrainAfterCommit(ctx, pool, "drain the change rows of the k14 projects' delivery settings")
	return nil
}

// writeK14 writes the two k14 projects (the comment above), logging each as it ends.
func writeK14(ctx context.Context, pool *store.Pool, spec Spec, step func(string)) error {
	for _, fixture := range k14Fixtures {
		history, err := writeProject(ctx, pool, spec, fixture)
		if err != nil {
			return err
		}
		prs, err := writeDelivery(ctx, pool, spec, history)
		if err != nil {
			return err
		}
		switch fixture.key {
		case PairProject:
			err = writeReverts(ctx, pool, spec, fixture, prs)
		case MixedProject:
			err = writePosted(ctx, pool, spec, fixture)
		}
		if err != nil {
			return err
		}
		step(fixture.key + "'s issues, events, delivery history and failures")
	}
	return nil
}

// writeReverts writes the merged reverts of fixture's deploy repositories (delivery_reverts): of
// each pull request merged into one, about one in thirty is reverted by a pull request of the same
// repository merged half an hour to eight hours later, before the history's end, numbered past
// every pull request of the history. The table logs no change, and its notification wakes the
// production signals' loop, which writes each revert's failure once the deploy that ships it has
// its row (prodevents/signals).
func writeReverts(ctx context.Context, pool *store.Pool, spec Spec, fixture projectFixture, prs []plannedPR) error {
	rng := fixture.rand(spec.Seed, "reverts")
	var rows [][]any
	for _, deploy := range fixture.deploys {
		number := 100_000
		for _, pr := range prs {
			if pr.repo != deploy.repo || rng.Float64() >= revertShare {
				continue
			}
			merged := pr.merged.Add(between(rng, 30*time.Minute, 8*time.Hour))
			if !merged.Before(spec.Now) {
				continue
			}
			number++
			rows = append(rows, []any{deploy.repo, int32(number), pr.repo, int32(pr.number), pr.mergeCommitSHA, merged, sha(rng)})
		}
	}
	return copyRows(ctx, pool, "delivery_reverts", []string{"repo", "number", "reverts_repo", "reverts_number",
		"reverts_merge_commit_sha", "merged_at", "merge_commit_sha"}, pgx.CopyFromRows(rows))
}

// writePosted writes the deploys a system posted to fixture's route and the failures it posted
// that blame them (production_events, source api): over the last runDays days, as many deploys a
// day as the deploy runs of one deploy repository that reach production (three in four of
// dailyRuns' deploys), each finished at a moment of its day and started five to thirty minutes
// before; about one in twelve blamed by a failure detected five minutes to three hours after it
// finished, nine in ten of those restored a log-normal two hours later, before the history's end.
// As the delivery history is, the rows are written with the record's change trigger off for the
// transaction's length: they are history no reader is owed as a change.
func writePosted(ctx context.Context, pool *store.Pool, spec Spec, fixture projectFixture) error {
	rng := fixture.rand(spec.Seed, "posted")
	deploysADay, _ := spec.dailyRuns()
	deploysADay = max(1, deploysADay*3/4)
	posted := `{"kind":"system","id":"evalbench-poster"}`
	start := spec.Now.Add(-runDays * 24 * time.Hour)
	var deploys, failures [][]any
	for day := range runDays {
		from := start.AddDate(0, 0, day)
		for range deploysADay {
			finished := from.Add(within(rng, 24*time.Hour))
			if !finished.Before(spec.Now) {
				continue
			}
			id := uuidOf(rng)
			ref := fmt.Sprintf("evalbench-deploy-%d", len(deploys)+1)
			deploys = append(deploys, []any{id, fixture.key, prodevents.KindDeploy, "succeeded", prodevents.SourceAPI, ref,
				finished.Add(-between(rng, 5*time.Minute, 30*time.Minute)), finished, "", []string{sha(rng)}, []string{}, "", posted})
			if rng.Float64() >= failedShare {
				continue
			}
			detected := finished.Add(between(rng, 5*time.Minute, 3*time.Hour))
			if !detected.Before(spec.Now) {
				continue
			}
			var restoredAt *time.Time
			var restoredBy *string
			if restored := detected.Add(max(lasting(rng, 2*time.Hour, 0.8), time.Minute).Round(time.Second)); rng.Float64() < restoredShare && restored.Before(spec.Now) {
				restoredAt, restoredBy = &restored, &posted
			}
			failures = append(failures, []any{uuidOf(rng), fixture.key, prodevents.KindFailure, "", prodevents.SourceAPI,
				fmt.Sprintf("evalbench-failure-%d", len(failures)+1), "", []string{}, []string{}, "", posted,
				id, prodevents.SignalAPI, detected, prodevents.StateCounted, restoredAt, restoredBy})
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the posted deploys of %s: %w", fixture.key, err)
	}
	defer tx.Rollback(ctx)
	triggers := [][2]string{{"production_events", "production_events_changed"}}
	if err := setTriggers(ctx, tx, triggers, false); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"production_events"}, []string{"id", "project_key", "kind", "status", "source",
		"source_ref", "started_at", "finished_at", "repo", "commits", "pull_requests", "url", "actor"}, pgx.CopyFromRows(deploys)); err != nil {
		return fmt.Errorf("copy the posted deploys of %s: %w", fixture.key, err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"production_events"}, []string{"id", "project_key", "kind", "status", "source",
		"source_ref", "repo", "commits", "pull_requests", "url", "actor", "deploy_id", "signal", "detected_at", "state",
		"restored_at", "restored_by"}, pgx.CopyFromRows(failures)); err != nil {
		return fmt.Errorf("copy the posted failures of %s: %w", fixture.key, err)
	}
	if err := setTriggers(ctx, tx, triggers, true); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the posted deploys of %s: %w", fixture.key, err)
	}
	return nil
}

// uuidOf is a version 4 UUID drawn from rng, so a second load with the seed writes the same ids.
func uuidOf(rng *rand.Rand) pgtype.UUID {
	var id pgtype.UUID
	for i := range id.Bytes {
		id.Bytes[i] = byte(rng.Uint32())
	}
	id.Bytes[6] = id.Bytes[6]&0x0f | 0x40
	id.Bytes[8] = id.Bytes[8]&0x3f | 0x80
	id.Valid = true
	return id
}
