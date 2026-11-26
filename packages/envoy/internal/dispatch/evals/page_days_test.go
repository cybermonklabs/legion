package evals

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery/deliverytest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/measures"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The cost of a page's delivery days at ten times the stated run rate (dispatch://INFRA-2916,
// R14): a page reads the deploy runs of its days a handful of times whatever their number, writes
// them in one transaction, and logs one change of each eval.

// pageNamingRounds is how many rounds of naming a page of delivery changes makes at most, each of
// which reads its days at once (refresh.readDeliveries): the three of applyDelivery's items, their
// links and the runs that shipped those, the days owed, and nameLeft's owedRounds.
const pageNamingRounds = 3 + 1 + owedRounds

// pageTrace is a tracer (storetest.TracedStore) that counts, on the pool it traces, the reads of
// deploy runs by their creation from a moment on (applyReads: the apply runs of a day, of a list of
// days' windows, and of the days before a day its deploys can ship), and the commits of the
// transactions that wrote day rows (dayCommits): that inserted, updated or deleted one.
type pageTrace struct {
	mu         sync.Mutex
	applyReads int
	dayCommits int
	writing    map[*pgx.Conn]bool
}

func (p *pageTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.TrimSpace(data.SQL)
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case strings.Contains(statement, "delivery_runs r") && strings.Contains(statement, "coalesce(r.created_at, r.started_at) >="):
		p.applyReads++
	case strings.Contains(statement, "insert into eval_days (project_key"), strings.Contains(statement, "update eval_days "),
		strings.Contains(statement, "delete from eval_days "):
		p.writing[conn] = true
	case strings.EqualFold(statement, "commit"):
		if p.writing[conn] {
			p.dayCommits++
		}
		delete(p.writing, conn)
	case strings.EqualFold(statement, "rollback"), strings.HasPrefix(strings.ToLower(statement), "begin"):
		delete(p.writing, conn)
	}
	return ctx
}

func (p *pageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// deliveryDaysOf is the UTC days report wrote delivery rows of, whatever the project, in order.
func deliveryDaysOf(report refreshed) []time.Time {
	days := map[time.Time]bool{}
	for day := range report.days {
		if day.family == walkDelivery {
			days[day.day] = true
		}
	}
	return slices.SortedFunc(maps.Keys(days), time.Time.Compare)
}

// oneApplyADay is, of each UTC day deliveries' deploy runs started on, the first of its runs that
// reached production, by run id.
func oneApplyADay(deliveries deliverytest.Fixture) []int64 {
	starts := map[int64]time.Time{}
	for _, run := range deliveries.Runs {
		starts[run.RunID] = measures.Midnight(run.StartedAt)
	}
	byDay := map[time.Time]int64{}
	for _, apply := range deliveries.Applies {
		if day := starts[apply.RunID]; byDay[day] == 0 || apply.RunID < byDay[day] {
			byDay[day] = apply.RunID
		}
	}
	return slices.Sorted(maps.Values(byDay))
}

// TestAPageOfManyDaysReadsItsDeployRunsAHandfulOfTimesInOneCommit: over thirty days of the delivery
// history, once every day is walked, the production job of one deploy run of each day completes a
// minute later, and one pass writes the days the change names, at least twenty. The pass reads
// deploy runs by their creation at least once and at most twice for each round of naming
// (pageNamingRounds): once for the windows of the round's days, however many, and once for the days
// before them their deploys can ship; where it read every deploy run created from each day it named
// on, one read of each day. It commits the days' rows in one transaction, and so logs one eval_days
// change of each eval it moved.
func TestAPageOfManyDaysReadsItsDeployRunsAHandfulOfTimesInOneCommit(t *testing.T) {
	t.Parallel()
	r := newRefresherRig(t)
	deliveries := deliverytest.Seed(t, r.database.Pool, deliverytest.Spec{Days: 30})
	r.settle(t)
	runs := oneApplyADay(deliveries)
	if len(runs) < 20 {
		t.Fatalf("the history holds a deploy that reached production on %d days, and the check needs at least twenty", len(runs))
	}
	trace := &pageTrace{writing: map[*pgx.Conn]bool{}}
	r.registry = New(storetest.TracedStore(t, r.database, trace), r.documents, r.registry.events, r.registry.attributor)
	seq := r.latestSeq(t)
	r.exec(t, "complete the production job of one deploy run of each day a minute later", `
		update delivery_run_jobs set completed_at = completed_at + interval '1 minute'
		where repo = $1 and run_id = any($2) and name = any($3) and completed_at is not null
	`, deliveries.DeployRepo, runs, deliveries.Settings.ProductionJobNames)
	report, _ := r.pass(t)
	days := deliveryDaysOf(report)
	if len(days) < 20 {
		t.Fatalf("the pass wrote the delivery rows of %d days, and the check needs at least twenty: %v", len(days), report.sorted())
	}
	if most := 2 * pageNamingRounds; trace.applyReads > most {
		t.Errorf("the pass read deploy runs by their creation %d times for the %d days it wrote, where %d rounds of naming read them at most %d times",
			trace.applyReads, len(days), pageNamingRounds, most)
	}
	if trace.applyReads == 0 {
		t.Errorf("the pass read deploy runs by their creation no time for the %d days it wrote, so the count of those reads judges nothing", len(days))
	}
	if trace.dayCommits != 1 {
		t.Errorf("the pass committed day rows in %d transactions, want the page's days in one", trace.dayCommits)
	}
	changes := r.evalDayChanges(t, seq)
	logged := map[string]int{}
	for _, change := range changes {
		logged[strings.Fields(change)[0]]++
	}
	for eval, count := range logged {
		if count > 1 {
			t.Errorf("the pass logged %d eval_days changes of %s, want one: %v", count, eval, changes)
		}
	}
	if len(logged) == 0 {
		t.Errorf("the pass logged no eval_days change, so the change moved no row and the check judges nothing")
	}
	t.Logf("%d deploy runs changed; the pass wrote %d delivery days (%d project days), read deploy runs by their creation %d times, "+
		"committed day rows in %d transactions, and logged %d eval_days changes of %d evals",
		len(runs), len(days), len(report.days), trace.applyReads, trace.dayCommits, len(changes), len(logged))
}

// dayWriteTimingEnv names the variable that runs TestAPagesDeliveryDaysAreTimed: set to 1, it
// times a page's delivery days at ten times the stated run rate, and set to statements it logs
// too where the database time of the passes of every day went (statementTimes). dayWriteDaysEnv
// names how many days of that load it writes, fourteen where it is not set: the most the pod's
// drives may load (never bigger than 14 days), and a load of more goes to a Job in the builds
// namespace.
const (
	dayWriteTimingEnv = "EVALS_DAY_WRITE_TIMING"
	dayWriteDaysEnv   = "EVALS_DAY_WRITE_DAYS"
)

// statementTimes is a tracer (storetest.TracedStore) that sums, while on, the time of each
// statement on the pool it traces by the statement's first 90 characters, its whitespace folded.
type statementTimes struct {
	mu    sync.Mutex
	on    bool
	total map[string]time.Duration
	count map[string]int
}

type statementStartedAt struct{}

func (s *statementTimes) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	shape := strings.Join(strings.Fields(data.SQL), " ")
	if len(shape) > 90 {
		shape = shape[:90]
	}
	return context.WithValue(ctx, statementStartedAt{}, statementStart{shape: shape, at: time.Now()})
}

func (s *statementTimes) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	started, ok := ctx.Value(statementStartedAt{}).(statementStart)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.on {
		s.total[started.shape] += time.Since(started.at)
		s.count[started.shape]++
	}
}

type statementStart struct {
	shape string
	at    time.Time
}

// top is the statements of the most time, at most n, each with its count and total, as lines.
func (s *statementTimes) top(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	shapes := slices.SortedFunc(maps.Keys(s.total), func(a, b string) int { return cmp.Compare(s.total[b], s.total[a]) })
	var lines []string
	for _, shape := range shapes[:min(n, len(shapes))] {
		lines = append(lines, fmt.Sprintf("%v in %d: %s", s.total[shape].Round(time.Millisecond), s.count[shape], shape))
	}
	return lines
}

// statements is how many statements s has counted while on.
func (s *statementTimes) statements() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, count := range s.count {
		total += count
	}
	return total
}

// The run ids of the timed load: a day's deploy runs are timedDeployRuns + day*1000 + n, its
// PR-checks runs timedCheckRuns + day*1000 + n, below deliverytest's.
const (
	timedDeployRuns int64 = 7_000_000_000
	timedCheckRuns  int64 = 7_500_000_000
)

// TestAPagesDeliveryDaysAreTimed logs how long a refresher pass takes for each delivery day it
// writes, at ten times the stated run rate (evalbench's TenTimes: 200 deploy runs a day of the
// deploy workflow's four jobs and 600 PR-checks runs a day of 40 jobs, 24,000 jobs) over the days
// dayWriteDaysEnv names, beside deliverytest's five days: once every day is walked, five rounds
// each of one pass after the production job of one deploy run of one day moves, and one after one
// of each day's moves. Each round's per-day time is the pass's time over the delivery days it
// wrote, and the marginal one the difference of the two passes' times over the difference of their
// days: what one more day adds to a page, whatever the pass's own cost. It is skipped unless
// dayWriteTimingEnv is 1 or statements.
func TestAPagesDeliveryDaysAreTimed(t *testing.T) {
	// Not parallel: it times a page's writes, which other tests' load would move; it runs only
	// when dayWriteTimingEnv asks for it.
	if mode := os.Getenv(dayWriteTimingEnv); mode != "1" && mode != "statements" {
		t.Skipf("set %s=1 to time a page's delivery days at ten times the stated run rate", dayWriteTimingEnv)
	}
	days := 14
	if given := os.Getenv(dayWriteDaysEnv); given != "" {
		parsed, err := strconv.Atoi(given)
		if err != nil || parsed < 2 || parsed > 100 {
			t.Fatalf("%s=%q is no number of days from 2 to 100", dayWriteDaysEnv, given)
		}
		days = parsed
	}
	r := newRefresherRig(t)
	deliveries := deliverytest.Seed(t, r.database.Pool, deliverytest.Spec{Days: 5})
	settings, repo := deliveries.Settings, deliveries.DeployRepo
	var numbers []int32
	for _, pr := range deliveries.PullRequests {
		if pr.Repo == repo {
			numbers = append(numbers, int32(pr.Number))
		}
	}
	first := refresherToday.AddDate(0, 0, -days)
	production := settings.ProductionJobNames[0]
	began := time.Now()
	r.exec(t, "write the deploy runs of ten times the stated run rate", `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at,
			conclusion, url, head_branch, event, jobs_complete)
		select $1, id, 'deploy', null, md5(id::text), moment.at - interval '2 minutes', moment.at, moment.at, moment.at + interval '30 minutes',
			'success', 'https://example.invalid/runs/' || id, 'main', 'push', true
		from generate_series(0, $3::int - 1) as d cross join generate_series(0, 199) as n
		cross join lateral (select $4::bigint + d * 1000 + n as id, $2::timestamptz + make_interval(days => d, secs => n * 432) as at) as moment
	`, repo, first, days, timedDeployRuns)
	r.exec(t, "write their jobs", `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select r.repo, r.run_id, job.name, r.started_at + job.offset_at, r.started_at + job.offset_at + interval '5 minutes', 'success'
		from delivery_runs r
		cross join (values ('build / image', interval '0 minutes'), ($4, interval '6 minutes'), ($5, interval '12 minutes'),
			($6, interval '24 minutes')) as job(name, offset_at)
		where r.repo = $1 and r.run_id >= $2 and r.run_id < $3
	`, repo, timedDeployRuns, timedCheckRuns, *settings.StagingApplyJobName, *settings.StagingE2EJobName, production)
	r.exec(t, "write the PR-checks runs of ten times the stated run rate", `
		insert into delivery_runs (repo, run_id, kind, pr_number, head_sha, head_commit_at, started_at, created_at, completed_at,
			conclusion, url, head_branch, event, jobs_complete)
		select $1, id, 'pr_checks', ($5::int[])[1 + (d * 600 + n) % cardinality($5::int[])], md5(id::text), moment.at - interval '1 minute',
			moment.at, moment.at, moment.at + interval '20 minutes', 'success', 'https://example.invalid/runs/' || id, 'feature',
			'pull_request', true
		from generate_series(0, $3::int - 1) as d cross join generate_series(0, 599) as n
		cross join lateral (select $4::bigint + d * 1000 + n as id, $2::timestamptz + make_interval(days => d, secs => n * 144) as at) as moment
	`, repo, first, days, timedCheckRuns, numbers)
	r.exec(t, "write their jobs", `
		insert into delivery_run_jobs (repo, run_id, name, started_at, completed_at, conclusion)
		select r.repo, r.run_id, 'check-' || lpad(k::text, 2, '0'), r.started_at + make_interval(secs => k * 10),
			r.started_at + make_interval(secs => k * 10 + 300), case when k = 7 and r.run_id % 10 = 0 then 'failure' else 'success' end
		from delivery_runs r cross join generate_series(1, 40) as k
		where r.repo = $1 and r.run_id >= $2 and r.run_id < $3
	`, repo, timedCheckRuns, timedCheckRuns+1_000_000_000)
	r.exec(t, "analyze the load", `analyze`)
	loaded := time.Since(began)
	passes, _ := r.settle(t)
	t.Logf("%d days at ten times the stated run rate (%d deploy runs, %d PR-checks runs and %d jobs) loaded in %v and walked in %d passes, %v",
		days, r.count(t, `select count(*) from delivery_runs where kind = 'deploy'`), r.count(t, `select count(*) from delivery_runs where kind = 'pr_checks'`),
		r.count(t, `select count(*) from delivery_run_jobs`), loaded.Round(time.Millisecond), passes, (time.Since(began) - loaded).Round(time.Millisecond))
	var times *statementTimes
	if os.Getenv(dayWriteTimingEnv) == "statements" {
		times = &statementTimes{total: map[string]time.Duration{}, count: map[string]int{}}
		r.registry = New(storetest.TracedStore(t, r.database, times), r.documents, r.registry.events, r.registry.attributor)
	}
	// pass moves the production job of the deploy run n of each of the first count days of the load
	// by shift seconds, makes one pass, and answers its time and the delivery days it wrote; with
	// times, the statements of a pass of every day are summed.
	pass := func(count, n, shift int) (time.Duration, int) {
		t.Helper()
		runs := make([]int64, count)
		for d := range count {
			runs[d] = timedDeployRuns + int64(d*days/count)*1000 + int64(n)
		}
		r.exec(t, "move the production job of a deploy run of each day", `
			update delivery_run_jobs set completed_at = completed_at + make_interval(secs => $4)
			where repo = $1 and run_id = any($2) and name = $3
		`, repo, runs, production, shift)
		if times != nil {
			times.mu.Lock()
			times.on = count == days
			times.mu.Unlock()
		}
		started := time.Now()
		report, _ := r.pass(t)
		return time.Since(started), len(deliveryDaysOf(report))
	}
	var perDay, marginal, ones, manys []time.Duration
	for round := range 5 {
		shift := 60
		if round%2 == 1 {
			shift = -60
		}
		one, oneDays := pass(1, 3+round, shift)
		many, manyDays := pass(days, 3+round, shift)
		ones, manys = append(ones, one), append(manys, many)
		perDay = append(perDay, many/time.Duration(max(manyDays, 1)))
		if manyDays > oneDays {
			marginal = append(marginal, (many-one)/time.Duration(manyDays-oneDays))
		}
		t.Logf("round %d: a pass of %d delivery days took %v, one of %d days %v", round, manyDays, many.Round(time.Millisecond), oneDays,
			one.Round(time.Millisecond))
	}
	t.Logf("at ten times the stated run rate over %d days: a pass of every day's change took a median %v, %v a delivery day; one of one day's "+
		"%v; one more day adds a median %v (marginal %v, per day %v)", days, median(manys).Round(time.Millisecond),
		median(perDay).Round(100*time.Microsecond), median(ones).Round(time.Millisecond), median(marginal).Round(100*time.Microsecond),
		marginal, perDay)
	if len(marginal) == 0 {
		t.Errorf("no round's pass of every day's change wrote more delivery days than its pass of one day's, so the times measure no day's cost")
	}
	if times != nil {
		times.mu.Lock()
		times.on = false
		times.mu.Unlock()
		t.Logf("the database time of the five passes of every day's change, by statement:\n\t%s", strings.Join(times.top(15), "\n\t"))
	}
}
