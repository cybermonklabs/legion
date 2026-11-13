// Package deliverytest seeds one delivery history into a Dispatch database and answers what it
// wrote. The delivery and CI measures (GET /api/v1/delivery/measures, GET /api/v1/delivery/pipeline,
// and the day parts dispatch://INFRA-2916 folds them from) count thousands of pull requests, runs
// and jobs at once, and a check that compares two computations of them compares nothing until a
// history that size exists. It is an ordinary package rather than a _test.go file, as
// legion/legiontest is, so every package whose tests need such a history can import it (Go cannot
// share test-only code across packages), and a command can write the same history through Write.
//
// Write stores the rows GitHub intake and the reconcile leave behind: the one delivery_settings
// row, two projects and the issues the pull requests name, three repositories mapped to the first
// project in repo_projects, the pull requests merged into them, the deploy workflow's push runs on
// main of the deploy repository, and the PR-checks workflow's pull_request runs of that same
// repository (the one repository whose runs Dispatch reads), each run with its jobs. Every value
// comes from one math/rand/v2 PCG source seeded by Spec.Seed and every time from Spec.Now, so one
// spec writes the same history on every run. Each table is one statement that unnests its columns
// as arrays, as legiontest writes its tables: a history of eighty thousand jobs written a statement
// per row takes minutes against a real Postgres.
//
// Containment (delivery/containment.go) ships a pull request of the deploy repository with the
// earliest-completing successful production job of a deploy run on main that GitHub created after
// the merge. It reads the merge time, the run's creation time and the run's jobs, and no commit,
// head or ancestry row, so the history writes none: the runs' creation times alone decide which
// deploy ships which pull request. Deploy runs are created about twenty times a day, so most
// merged pull requests ship within an hour or two, and many deploys ship none. In the last
// outageSpan of the history every production job fails, so the pull requests merged then are
// still waiting at its end. A pull request of the other two repositories is never tracked by a
// deploy (ComputeDeployedStatus), as in production.
//
// One database holds one such history: delivery_settings is a single row, so a second Write
// replaces the first one's settings.
package deliverytest

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// DefaultProject is the project a Spec that names none is written under.
	DefaultProject = "DELIV"

	// DefaultDays is how many days of history a Spec that asks for no number writes: more than
	// the 84 days the widest delivery window reads (api/delivery.go's maxDeliveryWindow), so a
	// window of any allowed width fits inside the history.
	DefaultDays = 120

	// pcgStream is the second word of the PCG source, fixed so Spec.Seed alone picks the history.
	// It is "delivery" in ASCII.
	pcgStream = 0x64656c6976657279

	// outageSpan is the end of the history in which every production job fails, so the pull
	// requests merged in it, about six of the deploy repository's, wait past the end, as they do
	// while production is broken.
	outageSpan = 12 * time.Hour

	// firstRunID is the id the oldest run gets. Run ids rise with creation, as GitHub's do.
	firstRunID int64 = 9_000_000_001

	// The deploy workflow's four jobs, in the order they run, and the PR-checks workflow's
	// roll-up job, which fails when any other check fails. The settings name the last three
	// deploy jobs, so every Pipeline card has a job to follow.
	buildJob        = "build / image"
	stagingApplyJob = "staging-release / apply"
	stagingE2EJob   = "staging-e2e / run"
	productionJob   = "production-release / apply"
	rollupJob       = "checks-complete"
)

// DefaultNow is the moment a Spec that names none ends its history: a fixed UTC midnight, never
// the wall clock, so two runs of one Spec write the same rows.
var DefaultNow = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// projectKey is what projects.key admits (migration 0001).
var projectKey = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// system is the actor of every row this package writes that records one.
var system = model.Actor{Kind: "system", ID: "deliverytest"}

// authors are the population authors every pull request is written by.
var authors = []string{"dev-ana", "dev-ben", "dev-chi", "dev-dai", "dev-eli"}

// deployStages are the deploy workflow's jobs, each with how long it usually takes and how widely
// that spreads (the sigma of a log-normal draw).
var deployStages = []struct {
	name   string
	median time.Duration
	sigma  float64
}{
	{buildJob, 6 * time.Minute, 0.25},
	{stagingApplyJob, 4 * time.Minute, 0.25},
	{stagingE2EJob, 16 * time.Minute, 0.3},
	{productionJob, 6 * time.Minute, 0.3},
}

// The deploy stages a run can fail or be cancelled in, by their index in deployStages.
const (
	buildStage = iota
	stagingApplyStage
	stagingE2EStage
	productionStage
)

// checkJobs are the PR-checks workflow's jobs besides the roll-up: how long each usually takes,
// how often it fails, and the share of runs that run it (the rest skip it, which no measure
// times). The two jobs most runs skip reach the slowest-jobs table's floor of
// measures.SlowestMinResults timed results only over a few days, while every other job reaches it
// in one.
var checkJobs = []struct {
	name   string
	median time.Duration
	fails  float64
	runs   float64
}{
	{"build / compile", 3 * time.Minute, 0.02, 1},
	{"build / unit", 8 * time.Minute, 0.05, 1},
	{"lint", 90 * time.Second, 0.03, 1},
	{"typecheck", 150 * time.Second, 0.02, 1},
	{"test / api", 12 * time.Minute, 0.04, 1},
	{"test / web", 9 * time.Minute, 0.03, 1},
	{"e2e / smoke", 15 * time.Minute, 0.06, 0.5},
	{"docs", 2 * time.Minute, 0.01, 0.3},
	{"security / scan", 4 * time.Minute, 0.01, 1},
}

// smokeTimeout is how long `e2e / smoke` runs before GitHub stops it; a third of its failures are
// that timeout.
const smokeTimeout = 30 * time.Minute

// Spec is the history Write stores: the project it belongs to, the moment it ends, how many days
// it covers, and the seed of every random draw. A zero field takes its default: DefaultProject,
// DefaultNow, DefaultDays; a zero Seed is a seed like any other.
//
// OneProject, off by default, names a new issue of the project wherever the history would name
// one of the second project, so every pull request counts for the project, by its issue or, where
// it names none, by its repository: the eval registry's day rows of the project then hold every
// row the deployment-wide delivery routes count. It takes the same random draws, so the history
// is the default one with those issues moved to the project (and the project's issues numbered
// to match).
type Spec struct {
	Project    string    `json:"project"`
	Now        time.Time `json:"now"`
	Days       int       `json:"days"`
	Seed       uint64    `json:"seed"`
	OneProject bool      `json:"one_project,omitempty"`
}

// withDefaults is the spec with every zero field given its default.
func (spec Spec) withDefaults() Spec {
	if spec.Project == "" {
		spec.Project = DefaultProject
	}
	if spec.Now.IsZero() {
		spec.Now = DefaultNow
	}
	spec.Now = spec.Now.UTC()
	if spec.Days <= 0 {
		spec.Days = DefaultDays
	}
	return spec
}

// Fixture is what Write stored, in the terms a caller asserts against, and the base's own verdict
// on it: which deploys reached production and which pull requests they shipped, by
// delivery.ProductionApplies and delivery.ContainingRun over the rows written.
type Fixture struct {
	// Spec is the spec Write was given, with every default filled in.
	Spec Spec
	// Project is the project the repositories are mapped to; SecondProject is the project of the
	// few pull requests that name an issue elsewhere, whose project a reader of a pull request's
	// issue counts it for rather than its repository's.
	Project, SecondProject string
	// DeployRepo is the repository the settings name; Repos is the three repositories, the
	// deploy repository first.
	DeployRepo string
	Repos      []string
	// Settings is the delivery_settings row as Write gave it to delivery.PutSettings.
	Settings delivery.DeliverySettings
	// From and To are the two ends of the history: To is the Spec's Now, From is Days days
	// before it. Every pull request merged, and every run was created, in [From, To).
	From, To time.Time
	// Issues is every issue key written, by project; UnownedP0 is the open P0 issues with no
	// claim and no route among them, which the measures list.
	Issues    map[string][]string
	UnownedP0 []string
	// PullRequests is every pull request written, oldest merge first and those merged in one
	// instant by id, as delivery.ListPullRequestsInWindow lists them. UpdatedAt is left zero: the
	// database stamps it.
	PullRequests []delivery.DeliveryPullRequest
	// Runs is every run written, by run id, and Jobs every job of each, by run id in
	// delivery.ListRunJobs' order. A run or job still going at To carries no completion and no
	// conclusion, as GitHub reports it.
	Runs []delivery.DeliveryRun
	Jobs map[int64][]delivery.DeliveryRunJob
	// Applies is every deploy run's production apply (delivery.ProductionApplies over the deploy
	// runs on main); Deployed and Waiting are the deploy repository's pull requests a deploy
	// shipped by To and those still waiting at it (delivery.ContainingRun); ShippedNone is the
	// applies that shipped no pull request.
	Applies                        []delivery.Apply
	Deployed, Waiting, ShippedNone int
}

// Summary is one line naming the history and its counts, which Seed logs and a command prints.
func (f Fixture) Summary() string {
	jobs, deployRuns := 0, 0
	for _, run := range f.Runs {
		jobs += len(f.Jobs[run.RunID])
		if run.Kind == delivery.DeliveryRunKindDeploy {
			deployRuns++
		}
	}
	perRepo := make([]string, len(f.Repos))
	for i, repo := range f.Repos {
		count := 0
		for _, pr := range f.PullRequests {
			if pr.Repo == repo {
				count++
			}
		}
		perRepo[i] = fmt.Sprintf("%d in %s", count, repo)
	}
	return fmt.Sprintf("seeded the delivery history of %s (seed %d) from %s to %s: %d pull requests (%s; %d deployed, %d waiting at the end), "+
		"%d deploy runs (%d reached production, %d of them shipped no pull request), %d PR-check runs, %d jobs, %d issues of %s and %d of %s, %d unowned P0",
		f.Project, f.Spec.Seed, f.From.Format(time.RFC3339), f.To.Format(time.RFC3339),
		len(f.PullRequests), strings.Join(perRepo, ", "), f.Deployed, f.Waiting,
		deployRuns, len(f.Applies), f.ShippedNone, len(f.Runs)-deployRuns, jobs,
		len(f.Issues[f.Project]), f.Project, len(f.Issues[f.SecondProject]), f.SecondProject, len(f.UnownedP0))
}

// Seed writes the history into the database, fails the test if it cannot, logs one line naming
// what it wrote, and answers it.
func Seed(t testing.TB, pool *store.Pool, spec Spec) Fixture {
	t.Helper()
	started := time.Now()
	fixture, err := Write(t.Context(), pool, spec)
	if err != nil {
		t.Fatalf("seed the delivery history: %v", err)
	}
	t.Logf("%s; written in %s", fixture.Summary(), time.Since(started).Round(time.Millisecond))
	return fixture
}

// Write writes the history spec names into the database pool is on and answers what it wrote.
func Write(ctx context.Context, pool *store.Pool, spec Spec) (Fixture, error) {
	spec = spec.withDefaults()
	second := spec.Project + "X"
	if !projectKey.MatchString(spec.Project) || !projectKey.MatchString(second) {
		return Fixture{}, fmt.Errorf("project %q must be a project key of at most nine characters, so %q is one too", spec.Project, second)
	}
	lower := strings.ToLower(spec.Project)
	deployRepo := "acme/" + lower + "-platform"
	s := &seeding{
		spec:       spec,
		rng:        rand.New(rand.NewPCG(spec.Seed, pcgStream)),
		from:       spec.Now.Add(-time.Duration(spec.Days) * 24 * time.Hour),
		outageFrom: spec.Now.Add(-outageSpan),
		fixture: Fixture{
			Spec: spec, Project: spec.Project, SecondProject: second, DeployRepo: deployRepo,
			Repos:  []string{deployRepo, "acme/" + lower + "-api", "acme/" + lower + "-web"},
			To:     spec.Now,
			Issues: map[string][]string{},
			Jobs:   map[int64][]delivery.DeliveryRunJob{},
			Settings: delivery.DeliverySettings{
				DeployRepo: deployRepo, DeployWorkflowPath: ".github/workflows/deploy.yml",
				ProductionJobNames: []string{productionJob}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
				PopulationAuthors: slices.Clone(authors), ExcludedRepos: []string{},
				StagingE2EJobName: new(stagingE2EJob), StagingApplyJobName: new(stagingApplyJob), PRChecksRollupJobName: new(rollupJob),
			},
		},
		issueAt: map[string]int{},
	}
	s.fixture.From = s.from
	s.pullRequests()
	s.nameIssues()
	s.unownedP0()
	s.deployRuns()
	s.checkRuns()
	s.numberRuns()
	if err := s.write(ctx, pool); err != nil {
		return Fixture{}, err
	}
	s.judge()
	return s.fixture, nil
}

// seeding is one run of Write: the spec, the one random source, the history's two edges, the
// issues and runs built so far, and the Fixture it answers.
type seeding struct {
	spec       Spec
	rng        *rand.Rand
	from       time.Time
	outageFrom time.Time
	fixture    Fixture
	prs        []delivery.DeliveryPullRequest
	issues     []issue
	// issueAt is each issue key's index in issues.
	issueAt map[string]int
	runs    []seededRun
}

// issue is one issue the history writes: what the issues table holds of it, and the latest merge
// of a pull request naming it, which its close follows.
type issue struct {
	key, project, title string
	number              int32
	priority            *int16
	opened              time.Time
	lastMerged          time.Time
	open                bool
}

// seededRun is one run with its jobs, before the run ids are given out.
type seededRun struct {
	run  delivery.DeliveryRun
	jobs []delivery.DeliveryRunJob
}

// within is a moment in [0, span), to the second, as GitHub times everything.
func (s *seeding) within(span time.Duration) time.Duration {
	return time.Duration(s.rng.Int64N(int64(span/time.Second))) * time.Second
}

// between is a duration in [low, high], to the second.
func (s *seeding) between(low, high time.Duration) time.Duration {
	return low + time.Duration(s.rng.Int64N(int64((high-low)/time.Second)+1))*time.Second
}

// count is a whole number in [low, high].
func (s *seeding) count(low, high int) int {
	return low + s.rng.IntN(high-low+1)
}

// chance is true with probability p.
func (s *seeding) chance(p float64) bool {
	return s.rng.Float64() < p
}

// lasting is a log-normal duration around median, to the second and at least one: how long one
// job or one review takes, most near the median and a few far longer.
func (s *seeding) lasting(median time.Duration, sigma float64) time.Duration {
	drawn := time.Duration(float64(median) * math.Exp(sigma*s.rng.NormFloat64())).Round(time.Second)
	return max(drawn, time.Second)
}

// part is a share of span between low and high, to the second: how far into a job it failed or
// was cancelled.
func (s *seeding) part(span time.Duration, low, high float64) time.Duration {
	return time.Duration(float64(span) * (low + (high-low)*s.rng.Float64())).Round(time.Second)
}

// sha is a commit id: forty hex digits from the random source.
func (s *seeding) sha() string {
	return fmt.Sprintf("%016x%016x%08x", s.rng.Uint64(), s.rng.Uint64(), s.rng.Uint32())
}

// pullRequests draws every pull request of the history: each repository's merges spread over
// each day, about twelve a day into the deploy repository, four into the second repository and
// three into the third.
func (s *seeding) pullRequests() {
	perDay := [][2]int{{10, 14}, {2, 6}, {1, 5}}
	for day := range s.spec.Days {
		start := s.from.AddDate(0, 0, day)
		for index, repo := range s.fixture.Repos {
			for range s.count(perDay[index][0], perDay[index][1]) {
				s.pullRequest(repo, start.Add(s.within(24*time.Hour)))
			}
		}
	}
	// Numbers rise with creation within each repository, as GitHub's do.
	slices.SortStableFunc(s.prs, func(a, b delivery.DeliveryPullRequest) int {
		return cmp.Or(a.CreatedAt.Compare(*b.CreatedAt), a.MergedAt.Compare(*b.MergedAt), strings.Compare(a.Repo, b.Repo))
	})
	next := map[string]int{}
	for i := range s.prs {
		pr := &s.prs[i]
		next[pr.Repo]++
		pr.Number = next[pr.Repo]
		pr.URL = "https://github.com/" + pr.Repo + "/pull/" + strconv.Itoa(pr.Number)
	}
}

// pullRequest draws one pull request merged into repo at merged: open for about seventy minutes
// before it (a long tail to days), first committed some hours before it opened (one in ten has no
// first commit GitHub reports), and one in five a rework of earlier work.
func (s *seeding) pullRequest(repo string, merged time.Time) {
	open := min(max(s.lasting(70*time.Minute, 1.1), 3*time.Minute), 5*24*time.Hour)
	created := merged.Add(-open)
	var firstCommit *time.Time
	if !s.chance(0.1) {
		firstCommit = new(created.Add(-s.between(10*time.Minute, 8*time.Hour)))
	}
	s.prs = append(s.prs, delivery.DeliveryPullRequest{
		Repo: repo, Author: authors[s.rng.IntN(len(authors))],
		CreatedAt: &created, MergedAt: &merged, FirstCommitAt: firstCommit,
		MergeCommitSHA: new(s.sha()), Additions: new(1 + s.rng.IntN(400)), Deletions: new(s.rng.IntN(200)),
		Rework: s.chance(0.2), Sessions: []string{},
	})
}

// nameIssues gives each pull request, in creation order, the issue it names: about one in twenty
// an issue of the second project (of the project under Spec.OneProject), a little over half an
// issue of the project (a new one, or now and then one a recent pull request already named), and
// the rest none. Each issue closes half an hour after the last pull request naming it merged, and
// stays open where that falls past the history's end.
func (s *seeding) nameIssues() {
	elsewhere := s.fixture.SecondProject
	if s.spec.OneProject {
		elsewhere = s.fixture.Project
	}
	var recent []string
	for i := range s.prs {
		pr := &s.prs[i]
		var key string
		switch u := s.rng.Float64(); {
		case u < 0.05:
			key = s.newIssue(elsewhere, *pr.CreatedAt)
		case u < 0.6 && len(recent) > 0 && s.chance(0.3):
			key = recent[s.rng.IntN(len(recent))]
		case u < 0.6:
			key = s.newIssue(s.fixture.Project, *pr.CreatedAt)
			recent = append(recent, key)
			if len(recent) > 10 {
				recent = recent[1:]
			}
		}
		short := pr.Repo[strings.LastIndex(pr.Repo, "/")+1:]
		pr.Title = fmt.Sprintf("Change %d to %s", pr.Number, short)
		pr.Attribution = &delivery.AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{}}
		if key == "" {
			continue
		}
		pr.IssueKey = new(key)
		pr.Title = key + ": " + pr.Title
		pr.Attribution.TitleKeys = []string{key}
		named := &s.issues[s.issueAt[key]]
		named.lastMerged = maxTime(named.lastMerged, *pr.MergedAt)
	}
}

// newIssue writes one issue of project, opened some hours before the pull request that first
// names it, a quarter of them with no priority and the rest P1 to P3.
func (s *seeding) newIssue(project string, before time.Time) string {
	var priority *int16
	if !s.chance(0.25) {
		priority = new(int16(1 + s.rng.IntN(3)))
	}
	number := int32(len(s.fixture.Issues[project]) + 1)
	key := fmt.Sprintf("%s-%d", project, number)
	s.issueAt[key] = len(s.issues)
	s.issues = append(s.issues, issue{
		key: key, project: project, number: number, priority: priority,
		title:  fmt.Sprintf("Work item %d of %s", number, project),
		opened: before.Add(-s.between(time.Hour, 48*time.Hour)),
	})
	s.fixture.Issues[project] = append(s.fixture.Issues[project], key)
	return key
}

// unownedP0 writes two open P0 issues of the project with no claim and no route, which the
// measures list as unowned and judge the P0 target by.
func (s *seeding) unownedP0() {
	for _, age := range []time.Duration{72 * time.Hour, 20 * time.Hour} {
		number := int32(len(s.fixture.Issues[s.fixture.Project]) + 1)
		key := fmt.Sprintf("%s-%d", s.fixture.Project, number)
		s.issueAt[key] = len(s.issues)
		s.issues = append(s.issues, issue{
			key: key, project: s.fixture.Project, number: number, priority: new(int16(0)),
			title: fmt.Sprintf("Production incident %d", number), opened: s.spec.Now.Add(-age), open: true,
		})
		s.fixture.Issues[s.fixture.Project] = append(s.fixture.Issues[s.fixture.Project], key)
		s.fixture.UnownedP0 = append(s.fixture.UnownedP0, key)
	}
}

// deployRuns draws the deploy workflow's push runs on main of the deploy repository, eighteen to
// twenty-two a day.
func (s *seeding) deployRuns() {
	for day := range s.spec.Days {
		start := s.from.AddDate(0, 0, day)
		for range s.count(18, 22) {
			s.deployRun(start.Add(s.within(24 * time.Hour)))
		}
	}
}

// deployRun draws one deploy run GitHub created at created. Its four jobs run one after another,
// and a job that fails or is cancelled leaves the jobs after it skipped: about three in four runs
// reach production, the production job fails in one in twenty and is cancelled in one in thirty,
// and the rest stop in an earlier stage. In the outage at the end of the history every production
// job fails.
func (s *seeding) deployRun(created time.Time) {
	started := created.Add(s.between(5*time.Second, time.Minute))
	if !started.Before(s.spec.Now) {
		return
	}
	failed, cancelled, timedOut := -1, -1, false
	switch u := s.rng.Float64(); {
	case !created.Before(s.outageFrom):
		failed = productionStage
	case u < 0.04:
		failed = buildStage
	case u < 0.07:
		failed = stagingApplyStage
	case u < 0.15:
		failed, timedOut = stagingE2EStage, s.chance(0.25)
	case u < 0.20:
		failed = productionStage
	case u < 0.23:
		cancelled = stagingE2EStage
	case u < 0.26:
		cancelled = productionStage
	}
	conclusion := delivery.DeliveryRunConclusionSuccess
	jobs := make([]delivery.DeliveryRunJob, 0, len(deployStages))
	at := started.Add(s.between(5*time.Second, 20*time.Second))
	stopped := false
	for stage, step := range deployStages {
		if stopped {
			jobs = append(jobs, job(step.name, at, at, delivery.DeliveryJobConclusionSkipped))
			continue
		}
		begun := at.Add(s.between(2*time.Second, 20*time.Second))
		took := s.lasting(step.median, step.sigma)
		switch stage {
		case cancelled:
			at = begun.Add(s.part(took, 0.2, 0.8))
			jobs = append(jobs, job(step.name, begun, at, delivery.DeliveryJobConclusionCancelled))
			conclusion, stopped = delivery.DeliveryRunConclusionCancelled, true
		case failed:
			result := delivery.DeliveryJobConclusionFailure
			at = begun.Add(s.part(took, 0.3, 1))
			if timedOut {
				result, at = delivery.DeliveryJobConclusionTimedOut, begun.Add(45*time.Minute)
			}
			jobs = append(jobs, job(step.name, begun, at, result))
			conclusion, stopped = delivery.DeliveryRunConclusionFailure, true
		default:
			at = begun.Add(took)
			jobs = append(jobs, job(step.name, begun, at, delivery.DeliveryJobConclusionSuccess))
		}
	}
	s.addRun(delivery.DeliveryRun{
		Kind: delivery.DeliveryRunKindDeploy, HeadSHA: s.sha(),
		HeadCommitAt: created.Add(-s.between(10*time.Second, 3*time.Minute)),
		StartedAt:    started, CreatedAt: &created, CompletedAt: new(at.Add(s.between(3*time.Second, 15*time.Second))),
		Conclusion: &conclusion, HeadBranch: new("main"), Event: new("push"),
	}, jobs)
}

// checkRuns draws the PR-checks workflow's pull_request runs of the deploy repository, fifty-five
// to sixty-five a day.
func (s *seeding) checkRuns() {
	var numbers []int
	for _, pr := range s.prs {
		if pr.Repo == s.fixture.DeployRepo {
			numbers = append(numbers, pr.Number)
		}
	}
	for day := range s.spec.Days {
		start := s.from.AddDate(0, 0, day)
		for range s.count(55, 65) {
			s.checkRun(start.Add(s.within(24*time.Hour)), numbers[s.rng.IntN(len(numbers))])
		}
	}
}

// checkRun draws one PR-checks run of pull request number that started at started: its nine
// checks run side by side, each failing at its own rate, and the roll-up job follows them and
// fails when any failed. One run in twenty is cancelled partway, which cancels every check still
// running and the roll-up.
func (s *seeding) checkRun(started time.Time, number int) {
	if !started.Before(s.spec.Now) {
		return
	}
	created := started.Add(-s.between(0, 30*time.Second))
	jobs := make([]delivery.DeliveryRunJob, 0, len(checkJobs)+1)
	last, anyFailed := started, false
	for _, check := range checkJobs {
		begun := started.Add(s.between(0, 90*time.Second))
		if !s.chance(check.runs) {
			jobs = append(jobs, job(check.name, begun, begun, delivery.DeliveryJobConclusionSkipped))
			continue
		}
		took := s.lasting(check.median, 0.35)
		result, ended := delivery.DeliveryJobConclusionSuccess, begun.Add(took)
		if s.chance(check.fails) {
			anyFailed = true
			result, ended = delivery.DeliveryJobConclusionFailure, begun.Add(s.part(took, 0.3, 1))
			if check.name == "e2e / smoke" && s.chance(1.0/3) {
				result, ended = delivery.DeliveryJobConclusionTimedOut, begun.Add(smokeTimeout)
			}
		}
		jobs = append(jobs, job(check.name, begun, ended, result))
		last = maxTime(last, ended)
	}
	conclusion := delivery.DeliveryRunConclusionSuccess
	var rollup delivery.DeliveryRunJob
	if s.chance(0.05) {
		stop := started.Add(s.part(last.Sub(started), 0.1, 0.9))
		for i := range jobs {
			if *jobs[i].Conclusion == delivery.DeliveryJobConclusionSkipped || !jobs[i].CompletedAt.After(stop) {
				continue
			}
			jobs[i] = job(jobs[i].Name, minTime(*jobs[i].StartedAt, stop), stop, delivery.DeliveryJobConclusionCancelled)
		}
		conclusion, last = delivery.DeliveryRunConclusionCancelled, stop
		rollup = job(rollupJob, stop, stop, delivery.DeliveryJobConclusionCancelled)
	} else {
		begun := last.Add(s.between(2*time.Second, 10*time.Second))
		result := delivery.DeliveryJobConclusionSuccess
		if anyFailed {
			conclusion, result = delivery.DeliveryRunConclusionFailure, delivery.DeliveryJobConclusionFailure
		}
		last = begun.Add(s.between(3*time.Second, 15*time.Second))
		rollup = job(rollupJob, begun, last, result)
	}
	jobs = append(jobs, rollup)
	s.addRun(delivery.DeliveryRun{
		Kind: delivery.DeliveryRunKindPRChecks, PRNumber: new(number), HeadSHA: s.sha(),
		HeadCommitAt: created.Add(-s.between(time.Minute, 30*time.Minute)),
		StartedAt:    started, CreatedAt: &created, CompletedAt: new(last.Add(s.between(2*time.Second, 10*time.Second))),
		Conclusion: &conclusion, HeadBranch: new(fmt.Sprintf("feature/%d", number)), Event: new("pull_request"),
	}, jobs)
}

// job is one job of a run as GitHub reports it once it has finished.
func job(name string, started, completed time.Time, conclusion delivery.DeliveryJobConclusion) delivery.DeliveryRunJob {
	return delivery.DeliveryRunJob{Name: name, StartedAt: &started, CompletedAt: &completed, Conclusion: &conclusion}
}

// addRun keeps a run as the database holds it at the history's end: a job GitHub had not started
// by then is not listed yet, a job still going has no completion and no conclusion, and so has
// the run while any of its jobs, or the run itself, is still going.
func (s *seeding) addRun(run delivery.DeliveryRun, jobs []delivery.DeliveryRunJob) {
	now := s.spec.Now
	listed := jobs[:0]
	finished := run.CompletedAt.Before(now)
	for _, one := range jobs {
		if !one.StartedAt.Before(now) {
			finished = false
			continue
		}
		if !one.CompletedAt.Before(now) {
			one.CompletedAt, one.Conclusion, finished = nil, nil, false
		}
		listed = append(listed, one)
	}
	if !finished {
		run.CompletedAt, run.Conclusion = nil, nil
	}
	s.runs = append(s.runs, seededRun{run: run, jobs: listed})
}

// numberRuns gives out the run ids, rising with creation, and fills in each run's repository and
// URL and each job's run.
func (s *seeding) numberRuns() {
	slices.SortStableFunc(s.runs, func(a, b seededRun) int {
		return cmp.Or(a.run.Created().Compare(b.run.Created()), a.run.StartedAt.Compare(b.run.StartedAt), strings.Compare(a.run.HeadSHA, b.run.HeadSHA))
	})
	repo := s.fixture.DeployRepo
	for i := range s.runs {
		seeded := &s.runs[i]
		id := firstRunID + int64(i)
		seeded.run.Repo, seeded.run.RunID = repo, id
		seeded.run.URL = "https://github.com/" + repo + "/actions/runs/" + strconv.FormatInt(id, 10)
		for j := range seeded.jobs {
			seeded.jobs[j].Repo, seeded.jobs[j].RunID = repo, id
		}
		jobs := slices.Clone(seeded.jobs)
		slices.SortStableFunc(jobs, func(a, b delivery.DeliveryRunJob) int {
			return cmp.Or(a.StartedAt.Compare(*b.StartedAt), strings.Compare(a.Name, b.Name))
		})
		s.fixture.Runs = append(s.fixture.Runs, seeded.run)
		s.fixture.Jobs[id] = jobs
	}
}

// judge is the base's own verdict on what was written: every deploy run's production apply, the
// deploy repository's pull requests each apply shipped first, and the applies that shipped none.
func (s *seeding) judge() {
	var deploys []delivery.DeliveryRun
	for _, run := range s.fixture.Runs {
		if run.Kind == delivery.DeliveryRunKindDeploy {
			deploys = append(deploys, run)
		}
	}
	s.fixture.Applies = delivery.ProductionApplies(deploys, s.fixture.Jobs, s.fixture.Settings.Production()).Applies
	shipped := map[int64]int{}
	for _, pr := range s.fixture.PullRequests {
		if pr.Repo != s.fixture.DeployRepo {
			continue
		}
		if apply := delivery.ContainingRun(pr, s.fixture.DeployRepo, s.fixture.Applies); apply != nil {
			s.fixture.Deployed++
			shipped[apply.RunID]++
			continue
		}
		s.fixture.Waiting++
	}
	for _, apply := range s.fixture.Applies {
		if shipped[apply.RunID] == 0 {
			s.fixture.ShippedNone++
		}
	}
}

// write stores the history, one statement per table: the settings through delivery.PutSettings,
// the one write of that row, and every other table as arrays unnested into one insert, so a
// history of eighty thousand jobs costs a handful of round trips. The tables go in the order
// their foreign keys need: projects, repositories and issues before the pull requests that name
// them, and the runs before their jobs.
func (s *seeding) write(ctx context.Context, pool *store.Pool) error {
	exec := func(what, statement string, args ...any) error {
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			return fmt.Errorf("write the %s of %s: %w", what, s.spec.Project, err)
		}
		return nil
	}
	actor := fmt.Sprintf(`{"kind":%q,"id":%q}`, system.Kind, system.ID)
	if _, err := delivery.PutSettings(ctx, pool, s.fixture.Settings, system); err != nil {
		return fmt.Errorf("write the delivery settings of %s: %w", s.spec.Project, err)
	}
	projects := []string{s.fixture.Project, s.fixture.SecondProject}
	nextNumbers := []int32{int32(len(s.fixture.Issues[s.fixture.Project]) + 1), int32(len(s.fixture.Issues[s.fixture.SecondProject]) + 1)}
	if err := exec("projects", `
		insert into projects (key, name, next_number)
		select key, key, next from unnest($1::text[], $2::int[]) as seeded(key, next)`,
		projects, nextNumbers); err != nil {
		return err
	}
	if err := exec("repositories", `
		insert into repo_projects (repo, project, created_by, is_default)
		select repo, $2, $3::jsonb, repo = $4 from unnest($1::text[]) as seeded(repo)`,
		s.fixture.Repos, s.fixture.Project, actor, s.fixture.DeployRepo); err != nil {
		return err
	}
	if err := s.writeIssues(exec, actor); err != nil {
		return err
	}
	if err := s.writePullRequests(exec); err != nil {
		return err
	}
	return s.writeRuns(exec)
}

// writeIssues stores every issue. An issue closes half an hour after the last pull request
// naming it merged, and is still open where that falls at or past the history's end.
func (s *seeding) writeIssues(exec func(what, statement string, args ...any) error, actor string) error {
	count := len(s.issues)
	keys, projects, titles, statuses, ranks := make([]string, count), make([]string, count), make([]string, count), make([]string, count), make([]string, count)
	numbers := make([]int32, count)
	priorities := make([]*int16, count)
	opened := make([]time.Time, count)
	closed := make([]*time.Time, count)
	for i, one := range s.issues {
		keys[i], projects[i], titles[i], numbers[i], priorities[i], opened[i] = one.key, one.project, one.title, one.number, one.priority, one.opened
		ranks[i] = fmt.Sprintf("%08d", one.number)
		statuses[i] = "triage"
		if one.open {
			continue
		}
		if at := one.lastMerged.Add(30 * time.Minute); at.Before(s.spec.Now) {
			statuses[i], closed[i] = "done", &at
			continue
		}
		statuses[i] = "in_progress"
	}
	return exec("issues", `
		insert into issues (key, project_key, number, title, status, rank, priority, created_by, created_at, closed_at)
		select key, project, number, title, status, rank, priority, $1::jsonb, opened, closed
		from unnest($2::text[], $3::text[], $4::int[], $5::text[], $6::text[], $7::text[], $8::smallint[],
			$9::timestamptz[], $10::timestamptz[])
			as seeded(key, project, number, title, status, rank, priority, opened, closed)`,
		actor, keys, projects, numbers, titles, statuses, ranks, priorities, opened, closed)
}

// writePullRequests stores every pull request as a complete row (partial false), with the
// attribution inputs the completing fetch reads: the issue key its title names, and no cited
// issue, branch key or commit key. The answer's PullRequests are then put in merge order.
func (s *seeding) writePullRequests(exec func(what, statement string, args ...any) error) error {
	count := len(s.prs)
	repos, titles, urls, authorsOf, shas := make([]string, count), make([]string, count), make([]string, count), make([]string, count), make([]string, count)
	numbers, additions, deletions := make([]int32, count), make([]int32, count), make([]int32, count)
	created, merged := make([]time.Time, count), make([]time.Time, count)
	firstCommits := make([]*time.Time, count)
	rework := make([]bool, count)
	issueKeys := make([]*string, count)
	for i, pr := range s.prs {
		repos[i], numbers[i], titles[i], urls[i], authorsOf[i], shas[i] = pr.Repo, int32(pr.Number), pr.Title, pr.URL, pr.Author, *pr.MergeCommitSHA
		created[i], merged[i], firstCommits[i] = *pr.CreatedAt, *pr.MergedAt, pr.FirstCommitAt
		additions[i], deletions[i], rework[i], issueKeys[i] = int32(*pr.Additions), int32(*pr.Deletions), pr.Rework, pr.IssueKey
	}
	if err := exec("pull requests", `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, first_commit_at,
			merge_commit_sha, additions, deletions, rework, issue_key, sessions, partial,
			attribution_title_keys, attribution_cited_issues, attribution_branch_keys, attribution_commit_keys)
		select repo, number, title, url, author, created, merged, first_commit, sha, additions, deletions, rework, issue,
			'{}', false, case when issue is null then '{}'::text[] else array[issue] end, '{}', '{}', '{}'
		from unnest($1::text[], $2::int[], $3::text[], $4::text[], $5::text[], $6::timestamptz[], $7::timestamptz[],
			$8::timestamptz[], $9::text[], $10::int[], $11::int[], $12::bool[], $13::text[])
			as seeded(repo, number, title, url, author, created, merged, first_commit, sha, additions, deletions, rework, issue)`,
		repos, numbers, titles, urls, authorsOf, created, merged, firstCommits, shas, additions, deletions, rework, issueKeys); err != nil {
		return err
	}
	s.fixture.PullRequests = slices.Clone(s.prs)
	slices.SortStableFunc(s.fixture.PullRequests, func(a, b delivery.DeliveryPullRequest) int {
		return cmp.Or(a.MergedAt.Compare(*b.MergedAt), strings.Compare(a.Repo+"#"+strconv.Itoa(a.Number), b.Repo+"#"+strconv.Itoa(b.Number)))
	})
	return nil
}

// writeRuns stores every run and then every job. A run whose jobs had all finished by the
// history's end carries jobs_complete, as delivery.UpsertRunJobs records a listing GitHub gave
// after the run ended.
func (s *seeding) writeRuns(exec func(what, statement string, args ...any) error) error {
	count := len(s.fixture.Runs)
	ids := make([]int64, count)
	kinds, shas, urls := make([]string, count), make([]string, count), make([]string, count)
	prNumbers := make([]*int32, count)
	headAt, started, created := make([]time.Time, count), make([]time.Time, count), make([]time.Time, count)
	completed := make([]*time.Time, count)
	conclusions, branches, events := make([]*string, count), make([]*string, count), make([]*string, count)
	complete := make([]bool, count)
	var jobRuns []int64
	var jobNames []string
	var jobStarted, jobCompleted []*time.Time
	var jobConclusions []*string
	for i, run := range s.fixture.Runs {
		ids[i], kinds[i], shas[i], urls[i] = run.RunID, string(run.Kind), run.HeadSHA, run.URL
		if run.PRNumber != nil {
			prNumbers[i] = new(int32(*run.PRNumber))
		}
		headAt[i], started[i], created[i], completed[i] = run.HeadCommitAt, run.StartedAt, *run.CreatedAt, run.CompletedAt
		if run.Conclusion != nil {
			conclusions[i] = new(string(*run.Conclusion))
		}
		branches[i], events[i], complete[i] = run.HeadBranch, run.Event, run.CompletedAt != nil
		for _, one := range s.fixture.Jobs[run.RunID] {
			jobRuns, jobNames = append(jobRuns, run.RunID), append(jobNames, one.Name)
			jobStarted, jobCompleted = append(jobStarted, one.StartedAt), append(jobCompleted, one.CompletedAt)
			var conclusion *string
			if one.Conclusion != nil {
				conclusion = new(string(*one.Conclusion))
			}
			jobConclusions = append(jobConclusions, conclusion)
		}
	}
	if err := exec("runs", `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at,
			completed_at, conclusion, url, head_branch, event, jobs_complete)
		select $1, id, kind, pr, sha, head_at, started, created, completed, conclusion, url, branch, event, complete
		from unnest($2::bigint[], $3::text[], $4::int[], $5::text[], $6::timestamptz[], $7::timestamptz[], $8::timestamptz[],
			$9::timestamptz[], $10::text[], $11::text[], $12::text[], $13::text[], $14::bool[])
			as seeded(id, kind, pr, sha, head_at, started, created, completed, conclusion, url, branch, event, complete)`,
		s.fixture.DeployRepo, ids, kinds, prNumbers, shas, headAt, started, created, completed, conclusions, urls,
		branches, events, complete); err != nil {
		return err
	}
	return exec("jobs", `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select $1, run, name, started, completed, conclusion
		from unnest($2::bigint[], $3::text[], $4::timestamptz[], $5::timestamptz[], $6::text[])
			as seeded(run, name, started, completed, conclusion)`,
		s.fixture.DeployRepo, jobRuns, jobNames, jobStarted, jobCompleted, jobConclusions)
}

// maxTime is the later of two moments.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// minTime is the earlier of two moments.
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
