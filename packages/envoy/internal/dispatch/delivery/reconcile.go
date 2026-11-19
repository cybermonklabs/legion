// Reconcile (LEGION-567): the five-minute GitHub-App pass that backfills 28 days on first run and
// catches whatever intake's NATS consumer missed, through the same upserts intake uses -- so
// "reconcile catches a missed event" is provable as "reconcile and intake write through the same
// store functions", never two implementations that can drift. Shares its jittered-ticker loop
// with architecture.Run via duty.RunJittered (idle without GitHub App credentials, same shape as
// architecture.Importer and Intake).
package delivery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ReconcileInterval is how often reconcile re-imports GitHub facts. The Delivery page's
// freshness row promises a missed event appears "by the next five-minute reconcile".
const ReconcileInterval = 5 * time.Minute

// BackfillWindow is how far back the first reconcile pass imports, per the issue's "28-day
// backfill on first run".
const BackfillWindow = 28 * 24 * time.Hour

// reconcileOverlap extends every pass but the first back past its own last run, so a merge or run
// GitHub's search/listing index was still catching up on at the previous pass's boundary is not
// permanently missed. The shared upsert makes re-reading anything in the overlap a no-op, not a
// duplicate.
const reconcileOverlap = time.Hour

// Reconcile wires the reconcile ticker (five minutes unless TickEvery shortens it) to the
// database, the GitHub App client and the event broker the issue.deployed events go through
// (RecordDeployedEvents). A nil GitHub client (no App credentials) leaves it idle, exactly like
// architecture.Importer and Intake.
type Reconcile struct {
	pool     *store.Pool
	github   *githubapp.Client
	events   *events.Broker
	atStart  bool
	interval time.Duration
	// gitHubReads is what RecordGitHubReads set: each leased pass's attempt at reading GitHub
	// goes through it, and nil records nothing.
	gitHubReads githubreads.Recorder
}

// NewReconcile builds a Reconcile.
func NewReconcile(pool *store.Pool, github *githubapp.Client, broker *events.Broker) *Reconcile {
	return &Reconcile{pool: pool, github: github, events: broker}
}

// FirstPassAtStart makes Run run one pass as it starts, before its jittered ticks, for an
// end-to-end test that must see a pass inside its window (DISPATCH_TEST_RECONCILE_AT_START, under
// the test hooks). The pass takes the lease for the current interval as any tick's does, so a
// jittered tick inside that interval finds it served. A deployment never sets it: the jitter is
// what keeps a restart from stampeding GitHub.
func (r *Reconcile) FirstPassAtStart() *Reconcile {
	r.atStart = true
	return r
}

// TickEvery makes Run tick on interval rather than ReconcileInterval, for an end-to-end test
// whose window is seconds rather than minutes (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL, under
// the test hooks). A non-positive interval leaves the default in place. A deployment never sets
// it: five minutes is what the Delivery page's freshness row promises.
func (r *Reconcile) TickEvery(interval time.Duration) *Reconcile {
	r.interval = interval
	return r
}

// RecordGitHubReads makes each leased pass record its attempt at reading GitHub through record,
// as the newest attempt of githubreads.DeliveryReconcile, which GET /healthz answers: whether a
// GitHub read of the pass failed, and the first failure. Without it a pass records nothing.
func (r *Reconcile) RecordGitHubReads(record githubreads.Recorder) *Reconcile {
	r.gitHubReads = record
	return r
}

// tickInterval is how often the loop ticks, ReconcileInterval unless TickEvery shortened it. It
// is also the lease period one pass takes, and the two must be the same value: duty.Exclusive
// refuses a pass whose period another pass already served, so a loop ticking every second under
// five-minute lease periods would run one pass and then be refused for five minutes.
func (r *Reconcile) tickInterval() time.Duration {
	if r.interval > 0 {
		return r.interval
	}
	return ReconcileInterval
}

// HasApp reports whether the GitHub App credentials needed to do any work are configured.
func (r *Reconcile) HasApp() bool {
	return r != nil && r.github != nil
}

// ReconcileLease names the lease one reconcile pass runs under. Every pass searches and lists
// GitHub against one installation's rate limit - a backfill is tens of thousands of requests - so
// N processes reconciling is N times those calls for one result. Exported so a test that must run
// no pass of any process holds it (cmd/dispatch, TestTwoBuildsPassMessagesBothWays).
const ReconcileLease = "delivery-reconcile"

// Run re-imports delivery facts on the interval it ticks on (ReconcileInterval unless TickEvery
// shortened it) until ctx is cancelled. The first tick is jittered so a restart does not stampede
// GitHub, and a server without App credentials logs once and stops: no import can be signed.
//
// Each pass runs under ReconcileLease for one such interval, so one process reconciles
// per interval however many serve the database: the two tickers are jittered to phases of their
// own, so the second's tick lands between the first's rather than on it, and finds the interval
// reconciled. leases is required: unlike StartIntakes, which opens the lease pool itself
// because it is the boot's error-returning constructor, Run has no error to return, so it takes
// the pool cmd/dispatch has already opened (and exits at boot without).
func (r *Reconcile) Run(ctx context.Context, leases duty.Leases) {
	if r.atStart && r.HasApp() {
		r.leasedPass(ctx, leases, time.Now())
	}
	duty.RunJittered(ctx, r.tickInterval(), func() bool {
		if r.HasApp() {
			return false
		}
		slog.Info("dispatch delivery: no GitHub App key — the reconcile is idle")
		return true
	}, func(ctx context.Context, due time.Time) { r.leasedPass(ctx, leases, due) })
}

// leasedPass runs one reconcile pass under the lease, for the interval this loop ticks on and
// the tick due at due.
func (r *Reconcile) leasedPass(ctx context.Context, leases duty.Leases, due time.Time) {
	duty.Exclusive(ctx, leases, ReconcileLease, due, r.tickInterval(), r.runOnce)
}

// runOnce performs one reconcile pass: the pull-request head refresh, then — where delivery is
// configured — a permission check (so a missing Actions or Pull-requests grant is named, never
// confused with a transient failure), the population pull-request search, the deploy and
// PR-checks workflows' runs and jobs, and any partial row left over from a completing fetch that
// failed earlier. Each windowed step keeps its own progress
// (delivery_reconcile_progress), so a step that fails part way through resumes where it stopped
// rather than restarting its whole window: a 28-day backfill of a busy deploy repository is tens
// of thousands of GitHub requests, well past one installation's hourly rate limit, so no single
// pass can finish one and a pass that forgets what it did could never converge. last_reconcile_at
// advances ONLY when every step that ran succeeded; any failure (a missing permission, a rate
// limit, any other GitHub or store error) is recorded by name in last_error instead, and the
// previous last_reconcile_at is left untouched -- so the freshness row can never report a healthy
// timestamp for a pass that silently did nothing.
//
// The head pass (reconcilePullRequestHeads) runs first and whatever the settings say.
// pull_request_heads is not delivery's own table: it is what a recorded check result's commit
// is compared with (api/results_comparison.go), and that reader is served in every deployment,
// delivery configured or not, so a deployment with no delivery settings refreshes its heads
// and a result recorded at a head a later push replaced stands at no basis there too. It is
// outside the deploy repository's permission check for the same reason: that check asks about
// the deploy repository's Actions and Pull-requests grants, while the head pass reads each
// row's own repository with that row's own token.
//
// Every other step stays gated on the settings and on that check, in that order: the merged
// pull-request search, the partial rows, both workflows' runs, the runs backfills and
// RecordDeployedEvents all read a setting (the population, the deploy repository, its workflow
// paths) or run against the deploy repository's own grants.
//
// The intake reads no setting for a head either, for the same reason: it binds its durable
// whether or not the settings exist and writes the head row of every `pull_request` envelope,
// leaving a merge of the population and a workflow run to the settings (Intake.Run). This pass
// re-reads only the rows that exist, so without the intake a pull request that no result's
// read-through has asked GitHub about is a row nothing writes.
//
// Each pass logs one line, `dispatch delivery: reconcile pass`, with `took`: the time from the
// start of runOnce to its return, at every return. The deferred call writes it, so no step of the
// pass comes after it. duty.RunJittered arms the next timer only after the pass returns, so this
// is the time between one pass's start and the next one's timer.
//
// It is the pass the lease runs (leasedPass), so it is one attempt at reading GitHub
// (githubreads), recorded through RecordGitHubReads' recorder when it ends: the GitHub client
// notes on it each request and each failure no caller takes as data, and the pass notes each part
// of its read it gives up on an answer the client leaves to it: the permission check, a step that
// reads GitHub and fails (recordRead), and a row of such a step it logs and skips
// (readOwedPullRequests, readFactsOf, storePullRequestFacts, completePartialPullRequest).
// An answer a step takes as data is no failure, nor is the step that reads no GitHub, nor are the
// runs backfills, which never fail the pass (the client still notes what they meet). It is
// recorded on the context the lease handed the pass, so a pass this process's shutdown cut short
// records nothing.
func (r *Reconcile) runOnce(ctx context.Context) {
	start := time.Now()
	defer func() { slog.Info("dispatch delivery: reconcile pass", "took", time.Since(start)) }()
	caller := ctx
	ctx, attempt := githubreads.Begin(ctx)
	defer githubreads.Finish(caller, githubreads.DeliveryReconcile, attempt, r.gitHubReads, nil)
	now := start
	headErr := r.reconcilePullRequestHeads(ctx, now)
	// Where the pass returns before record() below exists, the head pass's failure can only be
	// logged. At the GetSettings arm there is no delivery_settings row to write it in, or none
	// that could be read; at the splitRepo and permission arms below, r.fail does write
	// last_error into that row, but last_error holds the one failure that ended the pass and the
	// step list that would carry a second one (failures, record) does not exist yet. It is a
	// step that reads GitHub all the same, so the pass's attempt notes it, as recordRead does.
	logHeadFailure := func() {
		if headErr != nil {
			slog.Error("dispatch delivery: reconcile pull request heads", "error", headErr)
			githubreads.NoteFailuref(ctx, "reconcile pull request heads: %w", headErr)
		}
	}
	settings, err := GetSettings(ctx, r.pool)
	if err != nil {
		logHeadFailure()
		if errors.Is(err, ErrNoSettings) {
			slog.Info("dispatch delivery: delivery is not configured — its steps are idle and the pull request head pass has run")
			return
		}
		slog.Error("dispatch delivery: read settings for reconcile", "error", err)
		return
	}
	production := settings.Production()
	owner, repo, err := splitRepo(settings.DeployRepo)
	if err != nil {
		logHeadFailure()
		slog.Error("dispatch delivery: deploy_repo setting", "error", err)
		r.fail(ctx, err)
		return
	}

	// Checked first among the steps the settings gate and named distinctly from every other
	// failure below: a missing Actions or Pull-requests permission would otherwise fail every
	// GitHub call those steps make in exactly the same shape as a transient 5xx, which this
	// slice's acceptance bar (LEGION-567) requires surfacing loudly and by name, not folding into
	// a generic fetch failure.
	if _, err := r.github.DeliveryPermissions(ctx, owner, repo); err != nil {
		logHeadFailure()
		slog.Error("dispatch delivery: GitHub App permission check", "error", err)
		// The App not installed on the deploy repository, or lacking a grant, is an answer the
		// client leaves to its caller, and it ends every step below: the pass notes it.
		githubreads.NoteFailure(ctx, err)
		r.fail(ctx, err)
		return
	}

	searchScope := searchProgressScope(settings)

	var failures []string
	record := func(step string, err error) {
		if err == nil {
			return
		}
		slog.Error("dispatch delivery: "+step, "error", err)
		failures = append(failures, fmt.Sprintf("%s: %s", step, err))
	}
	// recordRead is record for a step that reads GitHub: its failure gives up part of the pass's
	// read, which the pass's attempt notes as well, whatever failed inside the step (a database
	// error inside a step that reads GitHub ends that read too). The one step that reads no
	// GitHub, the issue.deployed events, goes through record alone.
	recordRead := func(step string, err error) {
		if err != nil {
			githubreads.NoteFailuref(ctx, "%s: %w", step, err)
		}
		record(step, err)
	}

	recordRead("reconcile merged pull requests", r.reconcileMergedPullRequests(ctx, settings, searchScope, now))
	recordRead("reconcile partial pull requests", r.reconcilePartialPullRequests(ctx))
	// The head pass ran at the top of the pass, before the settings were read; its failure is
	// recorded here, so last_error names the steps in this list's order.
	recordRead("reconcile pull request heads", headErr)
	recordRead("reconcile deploy workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, production, now))
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		recordRead("reconcile PR-checks workflow runs", r.reconcileWorkflow(ctx, owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, DeliveryRunKindPRChecks, production, now))
	}
	// The runs backfills log and resume their own failures and never reach record(): a rate limit
	// one meets must not hold last_reconcile_at or set last_error while every regular step
	// succeeded. Each walks at most backfillWindowsPerPass hours a pass. The created_at backfill
	// reads deploy runs alone, since containment is the one reader of a run's creation.
	r.backfillRuns(ctx, backfillStep(DeliveryRunKindDeploy), owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, production, now)
	if settings.PRChecksWorkflowPath != settings.DeployWorkflowPath {
		r.backfillRuns(ctx, backfillStep(DeliveryRunKindPRChecks), owner, repo, settings.DeployRepo, settings.PRChecksWorkflowPath, DeliveryRunKindPRChecks, production, now)
	}
	r.backfillRuns(ctx, createdAtBackfillStep, owner, repo, settings.DeployRepo, settings.DeployWorkflowPath, DeliveryRunKindDeploy, production, now)
	// Last, over everything this pass stored and what intake stored since the previous one: the
	// issue.deployed of every pull request whose shipping run is now known. Its failure is the
	// pass's, so last_error names it, and the rows it did not reach wait for the next pass.
	if written, err := RecordDeployedEvents(ctx, r.pool, settings, r.events); err != nil {
		record("record issue.deployed events", err)
	} else if written > 0 {
		slog.Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "reconcile")
	}

	if len(failures) > 0 {
		r.fail(ctx, errors.New(strings.Join(failures, "; ")))
		return
	}
	if err := RecordReconcileSuccess(ctx, r.pool, now); err != nil {
		slog.Error("dispatch delivery: record reconcile success", "error", err)
	}
	// The change log keeps a day (ChangeRetention): a page away longer reads its window once. A
	// failed prune leaves the rows for the next pass and fails nothing else.
	if _, err := PruneChanges(ctx, r.pool); err != nil {
		slog.Error("dispatch: delivery prune changes failed", "error", err)
	}
}

// maxLastErrorLength bounds what fail() writes to delivery_settings.last_error: a reconcile pass
// with many failing items (every rate-limited run in a reconcileWorkflow pass before Rev's
// probe-driven stop-at-first-rate-limit fix, 197 of 200 requests, each contributing its own
// error text) could otherwise grow this column without bound. Generous enough to still name
// several distinct failures, not just one.
const maxLastErrorLength = 4000

// fail records a reconcile pass's failure reason in delivery_settings.last_error without
// advancing last_reconcile_at (RecordReconcileError's own contract).
func (r *Reconcile) fail(ctx context.Context, cause error) {
	message := cause.Error()
	if len(message) > maxLastErrorLength {
		message = message[:maxLastErrorLength] + fmt.Sprintf(" ... (truncated from %d bytes)", len(message))
	}
	if err := RecordReconcileError(ctx, r.pool, message); err != nil {
		slog.Error("dispatch delivery: record reconcile error", "error", err)
	}
}

// searchProgressScope names what the merged-PR search's recorded progress was measured against:
// a change to the population, the exclusions or the deploy repository (which decides how a
// result is classified) makes a window imported under the old settings say nothing about the new
// ones, so the step backfills afresh rather than resuming it.
func searchProgressScope(settings DeliverySettings) string {
	return fmt.Sprintf("authors=%s excluded=%s deploy=%s",
		strings.Join(settings.PopulationAuthors, ","), strings.Join(settings.ExcludedRepos, ","), settings.DeployRepo)
}

// runsProgressScope names what a run listing's recorded progress was measured against.
func runsProgressScope(repoFull, workflowPath string) string {
	return repoFull + " " + workflowPath
}

// stepSince is where one step's window starts: the 28-day backfill when the step has no usable
// progress, else where it last imported through, minus reconcileOverlap so a merge or run
// GitHub's index was still catching up on at that boundary is re-read rather than permanently
// missed. Never earlier than the backfill window: a step whose progress is older than that (a
// server down for a month) catches up to the window the timeline actually shows rather than
// re-reading history nothing displays.
func (r *Reconcile) stepSince(ctx context.Context, step, scope string, now time.Time) (time.Time, error) {
	floor := now.Add(-BackfillWindow)
	through, err := ReconcileProgressThrough(ctx, r.pool, step, scope)
	if err != nil {
		return time.Time{}, fmt.Errorf("read %s progress: %w", step, err)
	}
	if through.IsZero() {
		return floor, nil
	}
	if since := through.Add(-reconcileOverlap); since.After(floor) {
		return since, nil
	}
	return floor, nil
}

// mergedPullRequestsStep names one installation's merged-PR search progress. Per installation
// rather than one step for the whole search: the installations are searched concurrently and
// fail independently, so one that cannot be searched must not make every other one redo what it
// already imported.
func mergedPullRequestsStep(installationID int64) string {
	return mergedPullRequestsStepPrefix + strconv.FormatInt(installationID, 10)
}

// mergedPullRequestsStepPrefix is what every installation's merged-PR step name starts with, and
// so the one place that names the family: PruneMergedPullRequestProgress selects the family with
// starts_with rather than LIKE, since the prefix's own underscores are LIKE wildcards.
const mergedPullRequestsStepPrefix = "merged_pull_requests/installation/"

// reconcileMergedPullRequests searches every population pull request merged since each
// installation's own recorded progress across every installation the App has (LEGION-294's
// population spans any repository the three authors merge into, not just the deploy
// repository), classifies each with the labels the search result already carries, and upserts
// the ones that belong -- complete, except for MergeCommitSHA, Additions, Deletions and
// FirstCommitAt, which the search response never carries.
//
// Each completed search window records that installation's progress, but only while every
// window before it in this pass succeeded: a pull request this pass could not classify (a
// deploy-repo PR GitHub's own classifier workflow has not labelled yet, IsTaskPR's "neither
// label") must be reachable again on a later pass, so progress stops at the window holding it
// while every later window is still imported. A rate limit of one installation ends the search of
// that installation only (passRateLimit): the search goes on with every other installation, and
// this step then answers the first limit it met. That limit, the search's own error and every
// per-PR classify/upsert failure are errors.Joined into one returned error -- not stringified into
// a plain errors.New, so a *githubapp.RateLimitError among them still answers errors.As for a
// caller that needs to tell it apart from an ordinary failure, the same reason
// reconcileWorkflow's own per-run aggregation keeps its typed error with %w.
func (r *Reconcile) reconcileMergedPullRequests(ctx context.Context, settings DeliverySettings, scope string, until time.Time) error {
	var mu sync.Mutex
	var failures []error
	var limited passRateLimit
	blocked := map[int64]bool{}

	since := func(installationID int64) (time.Time, error) {
		return r.stepSince(ctx, mergedPullRequestsStep(installationID), scope, until)
	}

	// The listing this search already makes is the only answer to which installations still
	// exist, so the rows of the ones that no longer do are dropped here rather than by a sweep
	// of its own.
	listed := func(installations []githubapp.Installation) {
		keep := make([]int64, 0, len(installations))
		for _, installation := range installations {
			keep = append(keep, installation.ID)
		}
		if err := PruneMergedPullRequestProgress(ctx, r.pool, keep); err != nil {
			mu.Lock()
			failures = append(failures, fmt.Errorf("prune installation progress: %w", err))
			mu.Unlock()
		}
	}

	visit := func(installation githubapp.Installation, windowUntil time.Time, prs []FetchedPullRequest) error {
		var windowFailures []error
		for _, pr := range prs {
			if pr.MergedAt == nil {
				continue
			}
			repoFull, err := r.searchResultRepo(pr)
			if err != nil {
				slog.Warn("dispatch delivery: merged-PR search result", "number", pr.Number, "error", err)
				windowFailures = append(windowFailures, fmt.Errorf("pull request #%d: %w", pr.Number, err))
				continue
			}
			if err := r.reconcilePullRequest(ctx, settings, repoFull, pr); err != nil {
				slog.Warn("dispatch delivery: reconcile pull request", "repo", repoFull, "number", pr.Number, "error", err)
				windowFailures = append(windowFailures, fmt.Errorf("%s#%d: %w", repoFull, pr.Number, err))
			}
		}
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, windowFailures...)
		if len(windowFailures) > 0 {
			blocked[installation.ID] = true
		}
		if blocked[installation.ID] {
			return nil
		}
		if err := RecordReconcileProgress(ctx, r.pool, mergedPullRequestsStep(installation.ID), scope, windowUntil); err != nil {
			failures = append(failures, fmt.Errorf("record installation %d progress: %w", installation.ID, err))
			blocked[installation.ID] = true
		}
		return nil
	}

	if err := SearchMergedPullRequestsAcrossInstallation(ctx, r.github, settings.PopulationAuthors, since, until, &limited, listed, visit); err != nil {
		mu.Lock()
		failures = append(failures, fmt.Errorf("search merged pull requests: %w", err))
		mu.Unlock()
	}
	return errors.Join(append([]error{limited.err()}, failures...)...)
}

// searchResultRepo recovers "owner/repo" from a search result's HTML URL
// (https://github.com/<owner>/<repo>/pull/<n>): GitHub's search-issues response does not carry a
// separate repository field, only the issue/PR's own URL.
func (r *Reconcile) searchResultRepo(pr FetchedPullRequest) (string, error) {
	const prefix = "https://github.com/"
	if len(pr.URL) <= len(prefix) || pr.URL[:len(prefix)] != prefix {
		return "", fmt.Errorf("pull request URL %q does not start with %s", pr.URL, prefix)
	}
	rest := pr.URL[len(prefix):]
	owner, repoTail, err := splitRepo(rest)
	if err != nil {
		return "", fmt.Errorf("pull request URL %q: %w", pr.URL, err)
	}
	// repoTail is "repo/pull/N"; keep only the repo segment.
	repoName, _, found := strings.Cut(repoTail, "/")
	if !found {
		return "", fmt.Errorf("pull request URL %q: no /pull/ segment", pr.URL)
	}
	return owner + "/" + repoName, nil
}

// reconcilePullRequest classifies and upserts one merged pull request the search found, the same
// decision handlePullRequestEnvelope makes: population membership from author (already true,
// since the search itself filtered on it)/repository/task-label, then the row. A deploy-repo PR
// the label rule cannot classify yet (IsTaskPR's "neither label" error) is logged and left for a
// later pass, exactly as intake does.
func (r *Reconcile) reconcilePullRequest(ctx context.Context, settings DeliverySettings, repoFull string, pr FetchedPullRequest) error {
	raw := RawPullRequest{Repo: repoFull, Number: pr.Number, Title: pr.Title, Author: pr.Author, Labels: pr.Labels, MergedAt: *pr.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)})
	if err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	if !isPopulation {
		return DeletePullRequest(ctx, r.pool, repoFull, pr.Number)
	}
	return UpsertPullRequest(ctx, r.pool, DeliveryPullRequest{
		Repo: repoFull, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author,
		CreatedAt: &pr.CreatedAt, MergedAt: pr.MergedAt, Rework: IsRework(pr.Title),
		Partial: true, // the search response never carries MergeCommitSHA/Additions/Deletions/FirstCommitAt
	})
}

// boundedFanOut runs fn(ctx, item) for every item in items, up to concurrency goroutines at
// once, via errgroup.WithContext(ctx) + SetLimit: the first goroutine to return a non-nil error
// cancels the shared context, and the loop checks it before starting each further item, so
// nothing new launches once that happens. An item already in flight when that happens is not
// necessarily spared either: it shares the cancelled context, so its own in-flight HTTP request
// (there is no cheap way to cancel one already sent otherwise) can itself be aborted before
// GitHub ever answers it -- it does not reliably run to completion the way an item started
// before the cancellation and already past its own HTTP call does. fn decides for itself which
// errors are the batch's own stop signal (returned) versus one item's own problem (logged and
// swallowed inside fn, nil returned) -- this helper only runs the fan-out, never interprets fn's
// result. No caller in this package returns a rate limit from fn: a limit is on one
// installation, so each caller keeps it (passRateLimit) and reads the items of every other
// installation. reconcilePartialPullRequests and SearchMergedPullRequestsAcrossInstallation's own
// searchOneInstallation loop each hand-rolled this identical
// errgroup.WithContext+SetLimit+groupCtx.Err()-before-each-Go shape separately before this
// helper existed.
func boundedFanOut[T any](ctx context.Context, concurrency int, items []T, fn func(ctx context.Context, item T) error) error {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(concurrency)
	for _, item := range items {
		if groupCtx.Err() != nil {
			break
		}
		item := item
		group.Go(func() error {
			return fn(groupCtx, item)
		})
	}
	return group.Wait()
}

// reconcilePartialConcurrency bounds how many partial pull requests reconcilePartialPullRequests
// completes at once. Each one makes sequential GitHub calls (RefreshPullRequestHead, then the
// first commit and, where that read did not page them, the commit messages); run one at a time,
// a 3,500-PR backfill (CONTRACT.md's own measured population size) serializes thousands of round
// trips end to end. GitHub documents only
// a 100-concurrent-request secondary-rate-limit ceiling and advises against running many requests
// concurrently at all, not a specific safe number below it -- 8 is this package's own
// conservative, arbitrary choice, picked to meaningfully parallelize a backfill without treating
// a single reconcile pass as free to hammer the API as hard as GitHub's documented ceiling
// allows; the shared rate limits of the server's client are what actually protect against
// running too hot, not this number: once one read meets an installation's limit, the client
// refuses each later read of that installation before it sends it (githubapp.WithLimits).
const reconcilePartialConcurrency = 8

// reconcilePartialPullRequests completes every stored partial row (written by intake when its
// completing fetch failed, or by this reconcile's own merged-PR search, which never carries the
// completing fields) with a single-pull-request fetch, via boundedFanOut
// (SetLimit(reconcilePartialConcurrency)) -- see boundedFanOut's own doc comment for the shared
// fan-out/cancellation mechanics. Each partial row is a distinct (repo, number) key, so
// concurrent upserts never race each other.
//
// A rate limit is on one installation, so it ends no completion of another (passRateLimit, the
// rule of the head pass too): completePartialPullRequest keeps the limit and answers nil, so
// boundedFanOut cancels nothing, and the rows of every other installation are completed. The step
// then answers the first limit it met, so the pass is reported failed and last_reconcile_at does
// not advance; the next pass reads again each row that this one did not complete. Any other
// per-row error (a 404, a deleted repository, a malformed answer) is only logged and skipped
// inside completePartialPullRequest: it is that one row's own problem, not a reason to stop the
// rest of the batch.
func (r *Reconcile) reconcilePartialPullRequests(ctx context.Context) error {
	partials, err := ListPartialPullRequests(ctx, r.pool)
	if err != nil {
		return fmt.Errorf("list partial pull requests: %w", err)
	}
	var limited passRateLimit
	// completePartialPullRequest answers nil on every path, so the fan-out has no failure of its
	// own to stop on.
	_ = boundedFanOut(ctx, reconcilePartialConcurrency, partials, func(ctx context.Context, pr DeliveryPullRequest) error {
		return r.completePartialPullRequest(ctx, pr, &limited)
	})
	return limited.err()
}

// reconcilePullRequestHeads is the head pass, the first step of every reconcile pass. In this
// order it reads the pull requests owed a read (readOwedPullRequests); reads again the open rows
// read longer than PullRequestHeadStaleAfter ago, at most PullRequestHeadBatch of them
// (readOpenHeads); reads the facts of each pull request that has none (readFactsLine); writes
// the answer of the statement over the stored facts to every stored row of each pull request
// (SyncAttributions); and appends the events of the queued moves (AppendAttributionEvents). The
// last two run whatever the reads answered. It remakes no keys: KeyRemakeLoop does, in each
// process. It logs one line of what it did in each pass, also where every count is 0.
//
// GitHub's `pull_request` events carry every head move, so the reads are the backstop for an
// event that never arrived — a delivery Dispatch was down for, a repository whose webhook was
// installed late — and the one way a merge that happened while Dispatch was not listening reaches
// the head row at all. The facts line is the one way a pull request stored with no facts gets
// them, and so an answer of the rule: a row a release before stored, a timeline row with no
// results row, a read whose commit messages failed. A rate limit of one installation ends no read
// of another (passRateLimit): the pass reads the rows of every other installation, and then fails
// with the first limit it met, as every other GitHub step's limit fails the pass. Any other failure
// of GitHub is logged and leaves the pull request for the next pass. Every read looks the
// installation of each repository up through the pass's own lookups (passInstallations), so a
// repository the App is not installed on costs the pass one lookup however many of its rows it
// reads. It answers the first limit and each failure of the database.
func (r *Reconcile) reconcilePullRequestHeads(ctx context.Context, now time.Time) error {
	var limited passRateLimit
	installations := &passInstallations{}
	var failures []error
	if err := r.readOwedPullRequests(ctx, now, installations, &limited); err != nil {
		failures = append(failures, err)
	}
	if err := r.readOpenHeads(ctx, now, installations, &limited); err != nil {
		failures = append(failures, err)
	}
	line, err := r.readFactsLine(ctx, installations, &limited)
	if err != nil {
		failures = append(failures, err)
	}
	synced, err := SyncAttributions(ctx, r.pool)
	if err != nil {
		failures = append(failures, err)
	}
	appended, err := AppendAttributionEvents(ctx, r.pool, r.events)
	if err != nil {
		failures = append(failures, err)
	}
	var queued int
	if err := r.pool.QueryRow(ctx, `select count(*) from pull_request_attribution_moves`).Scan(&queued); err != nil {
		failures = append(failures, fmt.Errorf("count the queued attribution moves: %w", err))
	}
	attributes := []any{"facts_left_before", line.LeftBefore, "facts_left", line.Left, "reads_left", line.ReadsLeft,
		"facts_refused", line.Refused, "changes", synced.Changes, "events", appended, "moves_queued", queued}
	if first := limited.first.Load(); first != nil {
		attributes = append(attributes, "rate_limit_scope", first.Scope)
	}
	slog.Info("dispatch delivery: pull request attribution", attributes...)
	return errors.Join(append([]error{limited.err()}, failures...)...)
}

// HeadPass runs the head pass once, at the time of the call, as runOnce runs it first
// (reconcilePullRequestHeads): for a caller outside this package that reads what one pass leaves,
// as the API's tests do.
func (r *Reconcile) HeadPass(ctx context.Context) error {
	return r.reconcilePullRequestHeads(ctx, time.Now())
}

// readOpenHeads is the open arm of the head pass: it reads each open row ListStalePullRequestHeads
// hands it by the batched query (readFactsOf), and writes GitHub's answer under the canonical name.
// A row whose read gives no answer is stamped checked, so it does not hold the front of every
// batch; one whose answer holds a value no row holds is stamped refused.
func (r *Reconcile) readOpenHeads(ctx context.Context, now time.Time, installations *passInstallations, limited *passRateLimit) error {
	stale, err := ListStalePullRequestHeads(ctx, r.pool, PullRequestHeadBatch, now)
	if err != nil {
		return err
	}
	targets := make([]factsTarget, len(stale))
	for i, ref := range stale {
		targets[i] = factsTarget{ref: ref}
	}
	return r.readFactsOf(ctx, installations, limited, targets, now, false, func(ctx context.Context, target factsTarget, outcome factsOutcome) error {
		switch outcome {
		case factsWritten:
			return nil
		case factsNoRowHolds:
			return r.stampFactsRefused(ctx, target)
		default:
			return MarkPullRequestHeadChecked(ctx, r.pool, target.ref.StoredUnder, target.ref.Number, now)
		}
	})
}

// factsTarget is one pull request a pass reads the facts of: its ref, under the canonical name,
// and the stored name of its timeline row where it has no results row ("" where it has one).
type factsTarget struct {
	ref      PullRequestRef
	timeline string
}

// factsOutcome is what the read of one target came to (readFactsOf).
type factsOutcome int

const (
	// factsWritten: GitHub's answer is written, or the refresh of a too-large answer stored it.
	factsWritten factsOutcome = iota
	// factsNoAnswer: GitHub gave nothing for the pull request, or the App cannot read its
	// repository. That is no proof that it does not exist.
	factsNoAnswer
	// factsAbsent: GitHub proved that no such pull request exists (pullRequestAbsent).
	factsAbsent
	// factsNotProved: GitHub answered the pull request absent, and gave no proof of it.
	factsNotProved
	// factsNoRowHolds: GitHub's answer holds a value no row holds (errNoRowHolds).
	factsNoRowHolds
)

// readFactsOf reads the facts of targets by the batched query, FactsBatchSize pull requests a
// query (readFactsBatchWithin), and settles each target it reads with what its read came to. It
// groups the targets by installation (passInstallations.repositoryToken of the canonical name,
// githubapp.Client.TokenScope), and reads the groups in parallel and each group's queries in
// sequence, within the installation's budget for the pass (passInstallations.factsBudget). It
// writes each answer under the canonical name and drops the row under ref.StoredUnder
// (upsertHeadUnderCanonicalName). With proveAbsence, an answer of absence goes to
// pullRequestAbsent, which asks GitHub once more with the query's token. A rate limit goes to
// limited and ends the reads of its installation, and the other installations go on. Any other
// failure of GitHub is logged and ends the reads of its installation for this pass. A target of a
// query that failed is not settled. It answers each failure of the database. The attempt of the
// pass notes each read of GitHub that this function logs and gives up (githubreads).
func (r *Reconcile) readFactsOf(
	ctx context.Context, installations *passInstallations, limited *passRateLimit, targets []factsTarget,
	now time.Time, proveAbsence bool, settle func(context.Context, factsTarget, factsOutcome) error,
) error {
	var (
		mu       sync.Mutex
		failures []error
	)
	fail := func(err error) {
		if err != nil {
			mu.Lock()
			failures = append(failures, err)
			mu.Unlock()
		}
	}
	type group struct {
		scope, token string
		targets      []factsTarget
	}
	var groups []*group
	byScope := map[string]*group{}
	for _, target := range targets {
		token, err := installations.repositoryToken(ctx, r.github, target.ref.Owner, target.ref.Repo)
		switch {
		case err == nil:
		case repositoryUnreadable(err):
			fail(settle(ctx, target, factsNoAnswer))
			continue
		case limited.keep(err):
			continue
		default:
			slog.Warn("dispatch delivery: mint the token of a read of a pull request's facts; the next pass reads it",
				"repo", target.ref.Owner+"/"+target.ref.Repo, "number", target.ref.Number, "error", err)
			githubreads.NoteFailuref(ctx, "read the facts of %s PR #%d: %w", target.ref.Owner+"/"+target.ref.Repo, target.ref.Number, err)
			continue
		}
		scope := r.github.TokenScope(token)
		g, found := byScope[scope]
		if !found {
			g = &group{scope: scope, token: token}
			byScope[scope] = g
			groups = append(groups, g)
		}
		g.targets = append(g.targets, target)
	}
	var wait sync.WaitGroup
	for _, g := range groups {
		budget := installations.factsBudget(g.scope)
		wait.Go(func() {
			for start := 0; start < len(g.targets); start += FactsBatchSize {
				chunk := g.targets[start:min(start+FactsBatchSize, len(g.targets))]
				refs := make([]PullRequestRef, len(chunk))
				for i, target := range chunk {
					refs[i] = target.ref
				}
				answers, err := readFactsBatchWithin(ctx, r.pool, r.github, installations, g.token, refs, now, budget)
				for _, target := range chunk {
					answer, found := answers[target.ref]
					switch {
					case found && answer.Head != nil:
						_, written := upsertHeadUnderCanonicalName(ctx, r.pool, *answer.Head, target.ref.StoredUnder)
						switch {
						case errors.Is(written, errNoRowHolds):
							fail(settle(ctx, target, factsNoRowHolds))
						case written != nil:
							fail(written)
						default:
							fail(settle(ctx, target, factsWritten))
						}
					case found && answer.Stored:
						fail(settle(ctx, target, factsWritten))
					case found && answer.Absent:
						outcome := factsNotProved
						if proveAbsence {
							absent, askErr := pullRequestAbsent(ctx, r.github, &pullRequestRefusal{err: ErrPullRequestNotFound,
								owner: target.ref.Owner, repo: target.ref.Repo, number: target.ref.Number, token: g.token})
							if askErr != nil && !limited.keep(askErr) {
								slog.Warn("dispatch delivery: ask GitHub whether a repository holds a pull request it answered absent; the next pass reads it again",
									"repo", target.ref.Owner+"/"+target.ref.Repo, "number", target.ref.Number, "error", askErr)
								githubreads.NoteFailuref(ctx, "ask GitHub whether %s holds PR #%d: %w", target.ref.Owner+"/"+target.ref.Repo, target.ref.Number, askErr)
							}
							if absent {
								outcome = factsAbsent
							}
						}
						fail(settle(ctx, target, outcome))
					case !found && err == nil:
						fail(settle(ctx, target, factsNoAnswer))
					}
				}
				if err != nil {
					if !errors.Is(err, errFactsBudgetSpent) && !limited.keep(err) {
						slog.Warn("dispatch delivery: read the facts of pull requests; the next pass reads them again",
							"installation", g.scope, "pull_requests", len(chunk), "error", err)
						first := chunk[0].ref
						githubreads.NoteFailuref(ctx, "read the facts of %d pull requests under %s, the first %s PR #%d: %w",
							len(chunk), g.scope, first.Owner+"/"+first.Repo, first.Number, err)
					}
					return
				}
			}
		})
	}
	wait.Wait()
	return errors.Join(failures...)
}

// stampFactsRefused records that the last read of target's facts was refused: attribution_refused_at
// on each results row of the pull request, under each name the renaming rule reads as its canonical
// name, or facts_refused_at on its timeline row where it has no results row. The line reads a
// refused pull request after every other, the oldest refusal first, and no refusal ends its read.
func (r *Reconcile) stampFactsRefused(ctx context.Context, target factsTarget) error {
	if target.timeline != "" {
		if _, err := r.pool.Exec(ctx, `update delivery_pull_requests set facts_refused_at = clock_timestamp() where repo = $1 and number = $2`,
			target.timeline, target.ref.Number); err != nil {
			return fmt.Errorf("record the refused read of %s#%d: %w", target.timeline, target.ref.Number, err)
		}
		return nil
	}
	return r.stampResultsRows(ctx, target.ref, "attribution_refused_at")
}

// stampResultsRows sets column, attribution_refused_at or attribution_absent_at, to now on each
// results row of ref's pull request, under each name the renaming rule reads as its canonical name.
// Neither column is a fact of the head, so the write owes no read.
func (r *Reconcile) stampResultsRows(ctx context.Context, ref PullRequestRef, column string) error {
	canonical := ref.Owner + "/" + ref.Repo
	if _, err := r.pool.Exec(ctx, `update pull_request_heads set `+column+` = clock_timestamp()
		where number = $2 and lower(`+refs.CanonicalRepoSQL("repo")+`) = lower($1)`, canonical, ref.Number); err != nil {
		return fmt.Errorf("record %s of %s#%d: %w", column, canonical, ref.Number, err)
	}
	return nil
}

// FactsLine is what one facts line counted (readFactsLine): the pull requests with no facts
// before its reads (LeftBefore) and after them (Left), the pull requests a pass still reads after
// them (ReadsLeft), and the part of those whose last read was refused (Refused).
type FactsLine struct{ LeftBefore, Left, ReadsLeft, Refused int }

// factsEntry is one pull request of the facts line, with what the line orders it by.
type factsEntry struct {
	target                                               factsTarget
	hasResults, open, unread, textsCurrent, messagesRead bool
	known, hasFacts, likelyWrong                         bool
	fallback                                             *string
	refusedAt, newest                                    *time.Time
}

// rank is the place of the entry's kind in the line's order: open rows, then rows whose answer is
// not known, then results rows no read of GitHub wrote, then rows whose fallback differs from the
// answer of their title and branch alone, then the rest, then rows whose messages are not read
// though a higher source decides their answer, and last the rows refused before.
func (e factsEntry) rank() int {
	switch {
	case e.refusedAt != nil:
		return 6
	case e.open:
		return 0
	case e.textsCurrent && !e.known:
		return 1
	case e.hasResults && e.unread:
		return 2
	case e.likelyWrong:
		return 3
	case e.textsCurrent && !e.messagesRead:
		return 5
	default:
		return 4
	}
}

// readFactsLine reads the facts of the pull requests a pass still reads, the facts line: each
// results row that is open or merged, that GitHub has not proved absent, and whose texts are not
// current or whose messages are not read, and each timeline row that is not unfetchable and has no
// results row under any name (factsLineJudged). It takes each pull request once, under its
// canonical name, and reads it by the batched query in the line's order (factsEntry.rank), within
// the installation's budget of the pass, which the open arm spent first (readFactsOf). So it reads
// at most FactsQueriesPerPass × FactsBatchSize pull requests of each installation.
//
// It writes each answer under the canonical name and drops the row under an old name. A write
// that checkHeadRow refuses stamps the pull request refused, and the line goes on. An answer of
// absence goes to pullRequestAbsent: its proof sets attribution_absent_at on the results rows, or
// marks the timeline row unfetchable (MarkPullRequestUnfetchable), and no proof stamps it refused.
// GitHub giving nothing for a pull request stamps it refused too. No single refusal ends a read: a
// refused pull request stays in the line, after every other.
func (r *Reconcile) readFactsLine(ctx context.Context, installations *passInstallations, limited *passRateLimit) (FactsLine, error) {
	entries, err := r.factsEntries(ctx)
	if err != nil {
		return FactsLine{}, err
	}
	var line FactsLine
	for _, entry := range entries {
		if !entry.hasFacts {
			line.LeftBefore++
		}
		if entry.refusedAt != nil {
			line.Refused++
		}
	}
	line.Left, line.ReadsLeft = line.LeftBefore, len(entries)
	if len(entries) == 0 {
		return line, nil
	}
	if err := r.markLikelyWrong(ctx, entries); err != nil {
		return line, err
	}
	slices.SortStableFunc(entries, compareFactsEntries)
	targets := make([]factsTarget, len(entries))
	for i, entry := range entries {
		targets[i] = entry.target
	}
	readErr := r.readFactsOf(ctx, installations, limited, targets, time.Now(), true, r.settleFacts)
	counts, err := ReadAttributionCounts(ctx, r.pool)
	if err != nil {
		return line, errors.Join(readErr, err)
	}
	line.Left, line.ReadsLeft, line.Refused = counts.FactsLeft, counts.ReadsLeft, counts.FactsRefused
	return line, readErr
}

// settleFacts acts on what the facts line's read of target came to (readFactsOf).
func (r *Reconcile) settleFacts(ctx context.Context, target factsTarget, outcome factsOutcome) error {
	switch outcome {
	case factsWritten:
		return nil
	case factsAbsent:
		if target.timeline != "" {
			if err := MarkPullRequestUnfetchable(ctx, r.pool, target.timeline, target.ref.Number, "GitHub proved that no such pull request exists"); err != nil {
				return fmt.Errorf("mark %s#%d unfetchable: %w", target.timeline, target.ref.Number, err)
			}
			return nil
		}
		return r.stampResultsRows(ctx, target.ref, "attribution_absent_at")
	default:
		return r.stampFactsRefused(ctx, target)
	}
}

// compareFactsEntries orders the facts line: by rank, the oldest refusal first among the refused,
// and then the newest first, by the time GitHub last updated the pull request and then its number.
func compareFactsEntries(a, b factsEntry) int {
	if c := cmp.Compare(a.rank(), b.rank()); c != 0 {
		return c
	}
	if a.refusedAt != nil && b.refusedAt != nil {
		if c := a.refusedAt.Compare(*b.refusedAt); c != 0 {
			return c
		}
	}
	switch {
	case a.newest != nil && b.newest == nil:
		return -1
	case a.newest == nil && b.newest != nil:
		return 1
	case a.newest != nil && b.newest != nil:
		if c := b.newest.Compare(*a.newest); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(b.target.ref.Number, a.target.ref.Number); c != 0 {
		return c
	}
	return strings.Compare(a.target.ref.Owner+"/"+a.target.ref.Repo, b.target.ref.Owner+"/"+b.target.ref.Repo)
}

// factsEntries reads the entries of the facts line, each under its canonical name, with what the
// line orders it by (attributionStatements.factsLine).
func (r *Reconcile) factsEntries(ctx context.Context) ([]factsEntry, error) {
	statements, err := attribution()
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, statements.factsLine)
	if err != nil {
		return nil, fmt.Errorf("read the facts line: %w", err)
	}
	defer rows.Close()
	var entries []factsEntry
	for rows.Next() {
		var entry factsEntry
		var canonical, storedUnder, timeline string
		if err := rows.Scan(&canonical, &entry.target.ref.Number, &storedUnder, &entry.hasResults, &timeline,
			&entry.open, &entry.unread, &entry.textsCurrent, &entry.messagesRead, &entry.known, &entry.hasFacts,
			&entry.fallback, &entry.refusedAt, &entry.newest); err != nil {
			return nil, fmt.Errorf("read the facts line: %w", err)
		}
		owner, repo, err := splitRepo(canonical)
		if err != nil {
			slog.Warn("dispatch delivery: a pull request of the facts line names no repository GitHub can have", "repo", canonical, "number", entry.target.ref.Number, "error", err)
			continue
		}
		entry.target.ref.Owner, entry.target.ref.Repo, entry.target.ref.StoredUnder = owner, repo, storedUnder
		if !entry.hasResults {
			entry.target.timeline = timeline
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the facts line: %w", err)
	}
	return entries, nil
}

// markLikelyWrong marks each entry with a results row and no current texts whose fallback differs
// from the answer of the statement over the keys of its title and branch alone
// (attributionStatements.titleAndBranchAnswers): the rows whose stored issue is most likely wrong,
// which the line reads before the rest.
func (r *Reconcile) markLikelyWrong(ctx context.Context, entries []factsEntry) error {
	var repos []string
	var numbers []int
	for _, entry := range entries {
		if entry.hasResults && !entry.textsCurrent {
			repos, numbers = append(repos, entry.target.ref.Owner+"/"+entry.target.ref.Repo), append(numbers, entry.target.ref.Number)
		}
	}
	if len(repos) == 0 {
		return nil
	}
	statements, err := attribution()
	if err != nil {
		return err
	}
	rows, err := r.pool.Query(ctx, statements.titleAndBranchAnswers, repos, numbers)
	if err != nil {
		return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
	}
	defer rows.Close()
	answers := map[PullRequestKey]*string{}
	for rows.Next() {
		var key PullRequestKey
		var issue *string
		if err := rows.Scan(&key.Repo, &key.Number, &issue); err != nil {
			return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
		}
		answers[key] = issue
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the answers of the titles and branches of the facts line: %w", err)
	}
	for i := range entries {
		entry := &entries[i]
		guess, found := answers[PullRequestKey{Repo: entry.target.ref.Owner + "/" + entry.target.ref.Repo, Number: entry.target.ref.Number}]
		entry.likelyWrong = found && !equalIssue(guess, entry.fallback)
	}
	return nil
}

// equalIssue reports whether two issue keys, each nil for nobody, name one issue.
func equalIssue(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// passRateLimit is the first rate limit that one step of a reconcile pass met. It is the one rule
// of a limit for each step that reads the pull requests of every installation: the head pass
// (reconcilePullRequestHeads), the merged search (reconcileMergedPullRequests) and the completions
// (reconcilePartialPullRequests). A limit is on one installation (githubapp.InstallationScope): a
// client that holds the shared limits, as the server's does (githubapp.WithLimits), refuses each
// later request of that installation before it sends it. So a step does not stop at a limit: it
// goes on with the rows of every other installation, at no call to the limited one, and fails
// with the limit once its reads are done. A limit on one installation then holds no other
// installation's rows back a pass (review of cybermonklabs/legion#4209 at ff73c7358d, B3, whose
// intake slots this pass is the other side of).
type passRateLimit struct {
	first atomic.Pointer[githubapp.RateLimitError]
}

// keep records err's rate limit where it is the first that the step met, and answers whether err
// is one. Each step that reads the rows of every installation has its own passRateLimit.
func (p *passRateLimit) keep(err error) bool {
	limited, ok := githubapp.AsRateLimit(err)
	if ok {
		p.first.CompareAndSwap(nil, limited)
	}
	return ok
}

// err is the failure of a step that met a rate limit, nil for one that met none. The caller's
// record names the step (runOnce).
func (p *passRateLimit) err() error {
	if first := p.first.Load(); first != nil {
		return fmt.Errorf("a rate limit ended the reads of an installation, and the reads of each installation no limit held went on: %w", first)
	}
	return nil
}

// readOwedPullRequests reads from GitHub the pull requests owed a read (pull_request_reads_owed,
// owePullRequestRead): every failure first, at most PullRequestHeadBatch of them a pass, and then
// the refusals, at most RefusedReadsPerPass, each kind the row read the fewest times first and
// then the one owed longest (listOwedPullRequestReads). So outside a GitHub outage a table full of
// refusals, which forged envelopes can make, takes no read from a failure, which an outage or a
// slow read leaves; during an outage the refusals a pass reads fail, become failures, and go
// before the real failure in the passes after it (pull_request_reads_owed.go).
// These come before the stored rows: nothing else writes the head of a pull request that has no
// row, or reads a merged or closed one again. The read writes the row, creating it where none
// exists, through the refresh every stored row takes, and recordOwedReadOutcome settles the owed
// read once GitHub answers for the pull request, or once one answer of GitHub, asked with the
// token of the read GitHub refused, shows the repository readable and the pull request absent
// (pullRequestAbsent, one GraphQL request for each 404 or 410), and otherwise counts the read
// under the kind it gave, with the word of GitHub's refusal for a refusal (refusalOf). So a pull
// request owed a failure is read every pass until GitHub answers, and one GitHub refuses with no
// such answer is read again by a later pass, with no age
// that ends it. Both lists are taken before either is read, so a row a read of this pass moves to
// the other kind is not read again in it. A rate limit, of the read or of the question whether the
// pull request exists, is kept for the pass (limited); the read's counts the read as a failure, and
// the question's leaves the refusal owed. The refresh stamps the row read, so the stored rows' list
// below does not take a row this has just read.
func (r *Reconcile) readOwedPullRequests(ctx context.Context, now time.Time, installations *passInstallations, limited *passRateLimit) error {
	read := func(ctx context.Context, owed owedRead) error {
		_, refused, err := RefreshPullRequestHead(ctx, r.pool, r.github, installations, owed.Repo, owed.Number, now)
		outcome, refusedAs := owedReadAnswered, ""
		switch {
		case err != nil:
			outcome = owedReadFailed
		case refused != nil:
			absent, askErr := pullRequestAbsent(ctx, r.github, refused)
			if askErr != nil && !limited.keep(askErr) {
				slog.Warn("dispatch delivery: ask GitHub whether a repository holds a pull request it refused; the read stays owed",
					"repo", owed.Repo, "number", owed.Number, "error", askErr)
				githubreads.NoteFailuref(ctx, "ask GitHub whether %s holds PR #%d: %w", owed.Repo, owed.Number, askErr)
			}
			if !absent {
				outcome, refusedAs = owedReadRefused, refusalOf(refused.err)
			}
		}
		if recordErr := recordOwedReadOutcome(ctx, r.pool, owed, outcome, refusedAs); recordErr != nil {
			slog.Warn("dispatch delivery: record how a pull request's owed read ended; the next pass reads it again",
				"repo", owed.Repo, "number", owed.Number, "error", recordErr)
		}
		if err != nil && !limited.keep(err) {
			slog.Warn("dispatch delivery: read a pull request owed a read; the next pass reads it again",
				"repo", owed.Repo, "number", owed.Number, "reason", owed.Reason, "owed_at", owed.OwedAt, "error", err)
			// The pass gives up this read until the next pass, so the attempt of the pass notes it.
			githubreads.NoteFailuref(ctx, "read the owed head of %s PR #%d: %w", owed.Repo, owed.Number, err)
		}
		return nil
	}
	failures, err := listOwedPullRequestReads(ctx, r.pool, owedFailure, PullRequestHeadBatch)
	if err != nil {
		return err
	}
	refusals, err := listOwedPullRequestReads(ctx, r.pool, owedRefusal, RefusedReadsPerPass)
	if err != nil {
		return err
	}
	// read answers nil on every path, so the fan-outs have no failure of their own to stop on.
	for _, owed := range [][]owedRead{failures, refusals} {
		_ = boundedFanOut(ctx, reconcilePartialConcurrency, owed, read)
	}
	return nil
}

// completePartialPullRequest is reconcilePartialPullRequests' per-row body, run as its own
// boundedFanOut goroutine. It reads the pull request once (RefreshPullRequestHead), which stores
// its results row and its texts, and completes the timeline row from that same read: the first
// commit's time (completeFetchedPullRequest) and the sessions (completePullRequest, which pages
// the messages only where the read did not read them). It answers nil on every path. It keeps a
// rate limit for the step (limited.keep), so boundedFanOut cancels nothing and the rows of every
// other installation are completed. It logs every other error, since one row's own failure must
// not stop the completion of any other row.
//
// A refusal marks the row unfetchable only where pullRequestAbsent proves that the repository is
// readable and holds no such pull request. One 404, or a repository that the App cannot read, is
// no such proof, so the row stays partial and the next pass reads it again.
func (r *Reconcile) completePartialPullRequest(ctx context.Context, pr DeliveryPullRequest, limited *passRateLimit) error {
	// settle ends the completion of the row at err: it keeps a rate limit for the step. It logs
	// any other failure with message, and the attempt of the pass notes that failure. The row
	// stays partial for the next pass.
	settle := func(message string, err error) error {
		if !limited.keep(err) {
			slog.Warn(message, "repo", pr.Repo, "number", pr.Number, "error", err)
			githubreads.NoteFailuref(ctx, "complete partial pull request %s #%d: %w", pr.Repo, pr.Number, err)
		}
		return nil
	}
	fetched, refused, err := RefreshPullRequestHead(ctx, r.pool, r.github, nil, pr.Repo, pr.Number, time.Now())
	if err != nil {
		return settle("dispatch delivery: complete partial pull request", err)
	}
	if refused != nil {
		absent, askErr := pullRequestAbsent(ctx, r.github, refused)
		if askErr != nil {
			return settle("dispatch delivery: ask GitHub whether a repository holds a partial pull request it refused; the row stays partial", askErr)
		}
		if !absent {
			slog.Warn("dispatch delivery: GitHub refused the read of a partial pull request; the next pass reads it again",
				"repo", pr.Repo, "number", pr.Number, "error", refused.err)
			// GitHub refuses the repository again in each pass, so the attempt of the pass notes
			// that refusal. A pull request not found and an App not installed are data, not failures.
			if errors.Is(refused.err, githubapp.ErrRepositoryUnreadable) {
				githubreads.NoteFailuref(ctx, "complete partial pull request %s #%d: %w", pr.Repo, pr.Number, refused.err)
			}
			return nil
		}
		if markErr := MarkPullRequestUnfetchable(ctx, r.pool, pr.Repo, pr.Number, refused.err.Error()); markErr != nil {
			slog.Warn("dispatch delivery: mark pull request unfetchable", "repo", pr.Repo, "number", pr.Number, "error", markErr)
		}
		return nil
	}
	full, err := completeFetchedPullRequest(ctx, r.github, fetched.Token, fetched.Owner, fetched.Repo, fetched.Payload)
	if err != nil {
		return settle("dispatch delivery: complete partial pull request", err)
	}
	if full.MergedAt == nil {
		return nil
	}
	var messages []string
	if fetched.Texts != nil && fetched.Texts.MessagesRead {
		messages = fetched.Texts.CommitMessages
		if messages == nil {
			messages = []string{}
		}
	}
	if err := completePullRequest(ctx, r.pool, r.github, fetched.Owner, fetched.Repo, pr.Repo, pr.Number, full, messages); err != nil {
		return settle("dispatch delivery: complete partial pull request; it stays partial for the next pass", err)
	}
	return nil
}

// runsWindow bounds how many runs one window of a run listing hands to reconcileWorkflow's
// visitor, and so how much work a failed pass redoes. Unlike a search window, every completed
// run in it costs its own jobs request, so 100 runs is about ten listing pages plus a hundred
// jobs requests -- a couple of minutes -- where GitHub's own 1,000-result cap would be most of
// an hour and a rate limit part way through would throw all of it away.
const runsWindow = 100

// runsStep names one workflow kind's run-listing progress.
func runsStep(kind DeliveryRunKind) string {
	return "runs/" + string(kind)
}

// completedRunIDs is the ids of runs that have concluded: the only runs whose jobs a pass lists,
// so the only ones its skip-set queries need to ask about.
func completedRunIDs(runs []FetchedRun) []int64 {
	ids := make([]int64, 0, len(runs))
	for _, run := range runs {
		if run.CompletedAt != nil {
			ids = append(ids, run.RunID)
		}
	}
	return ids
}

// reconcileWorkflow walks kind's workflow runs created since this step's own recorded progress,
// in windows, and upserts each run plus (for a concluded run) its jobs. Each completed window
// records the step's progress, but only while every window before it in this pass succeeded: a
// run whose jobs this pass could not fetch must be reachable again on a later pass, so progress
// stops at the window holding it while every later window is still imported.
//
// Returns a combined error naming every run reconcileRun failed on, rather than only logging
// it: a rate-limited or otherwise-failed per-run fetch (most often the jobs listing, since the
// runs listing above it already succeeded) must not let this pass report itself healthy.
// Reads which of a window's completed runs already carry a jobs-unfetchable mark in one bulk
// query (ListUnfetchableRunIDs) per window, instead of reconcileRun running its own
// RunJobsUnfetchable point query once per completed run on every pass -- the overwhelming
// majority of runs were never marked, and this package already bulk-fetches this way one
// function away (ListPartialPullRequests feeding reconcilePartialPullRequests).
func (r *Reconcile) reconcileWorkflow(ctx context.Context, owner, repo, repoFull, workflowPath string, kind DeliveryRunKind, production Production, until time.Time) error {
	step, scope := runsStep(kind), runsProgressScope(repoFull, workflowPath)
	since, err := r.stepSince(ctx, step, scope, until)
	if err != nil {
		return err
	}

	var failures []string
	attempted := 0
	blocked := false

	visit := func(windowUntil time.Time, runs []FetchedRun) error {
		jobsUnfetchable, err := ListUnfetchableRunIDs(ctx, r.pool, repoFull, completedRunIDs(runs))
		if err != nil {
			blocked = true
			return fmt.Errorf("list unfetchable %s run jobs: %w", kind, err)
		}
		windowFailed := false
		for _, run := range runs {
			attempted++
			err := r.reconcileRun(ctx, owner, repo, repoFull, kind, production, run, jobsUnfetchable)
			if err == nil {
				continue
			}
			slog.Warn("dispatch delivery: reconcile run", "repo", repoFull, "run_id", run.RunID, "error", err)
			failures = append(failures, fmt.Sprintf("run %d: %s", run.RunID, err))
			windowFailed = true
			// A rate limit is a global condition on this installation token's budget, not one
			// run's own problem: Rev's probe showed the un-fixed loop sending 197 of 200
			// requests after the first rate-limited one, each adding its own failure text to
			// last_error (57 KB by the end). Stop the whole walk here instead, preserving the
			// typed error so a caller checking errors.As for it still can, unlike every other
			// per-run failure joined into one string.
			if limited, ok := githubapp.AsRateLimit(err); ok {
				blocked = true
				return fmt.Errorf("%d of %d %s runs attempted before a rate limit stopped the rest: %s: %w",
					len(failures), attempted, kind, strings.Join(failures, "; "), limited)
			}
		}
		if windowFailed {
			blocked = true
		}
		if blocked {
			return nil
		}
		if err := RecordReconcileProgress(ctx, r.pool, step, scope, windowUntil); err != nil {
			blocked = true
			failures = append(failures, fmt.Sprintf("record %s progress: %s", kind, err))
		}
		return nil
	}

	if listErr := ListWorkflowRuns(ctx, r.github, owner, repo, workflowPath, since, until, runsWindow, visit); listErr != nil {
		return fmt.Errorf("list %s workflow runs: %w", kind, listErr)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d %s runs failed: %s", len(failures), attempted, kind, strings.Join(failures, "; "))
	}
	return nil
}

// reconcileRun upserts one run -- GitHub's workflow-runs listing already carries the head
// commit's timestamp (head_commit.timestamp, read into FetchedRun.HeadCommitAt by
// github_runs.go's fetchedRunFromItem), unlike the live webhook envelope intake.go handles, which
// re-verifies the whole run against GitHub rather than trusting the envelope -- and, once it has
// concluded, its jobs. A run whose jobs listing already carries a jobs-unfetchable mark
// (jobsUnfetchable, reconcileWorkflow's own bulk ListUnfetchableRunIDs read, from an earlier
// pass's permanent 404) is skipped rather than re-fetched: the same treatment
// completePartialPullRequest gives an already-unfetchable pull request.
func (r *Reconcile) reconcileRun(ctx context.Context, owner, repo, repoFull string, kind DeliveryRunKind, production Production, run FetchedRun, jobsUnfetchable map[int64]bool) error {
	if err := UpsertRun(ctx, r.pool, StoredRun(repoFull, kind, run)); err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	if run.CompletedAt == nil {
		return nil
	}
	if jobsUnfetchable[run.RunID] {
		return nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, r.github, owner, repo, run.RunID)
	if err != nil {
		if errors.Is(err, ErrRunNotFound) || errors.Is(err, githubapp.ErrNoInstallation) {
			if markErr := MarkRunJobsUnfetchable(ctx, r.pool, repoFull, run.RunID, err.Error()); markErr != nil {
				slog.Warn("dispatch delivery: mark run jobs unfetchable", "repo", repoFull, "run_id", run.RunID, "error", markErr)
			}
			return nil
		}
		return fmt.Errorf("list jobs: %w", err)
	}
	jobs := RunJobs(repoFull, run.RunID, fetchedJobs, production)
	return UpsertRunJobs(ctx, r.pool, repoFull, run.RunID, jobs, JobsComplete(jobs))
}

// mapRunConclusionPtr adapts mapRunConclusion (which intake.go calls with the envelope's raw
// string, always present) to FetchedRun.Conclusion's pointer (nil while a run is still in
// progress).
func mapRunConclusionPtr(raw *string) *DeliveryRunConclusion {
	if raw == nil {
		return nil
	}
	return mapRunConclusion(*raw)
}
