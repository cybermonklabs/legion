package cireconcile

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// head is one open pull request of the mapped repository as the tests below give it: pull
// request n at a head commit of its own, which is n written as the 40 hex characters a commit
// has. A test reads the number back out of the URLs the stand-in was asked for (headsAsked), so
// which heads a pass took, and in what order, is the stand-in's own log rather than the pass's
// word for it.
func head(number int64) fakePull {
	return fakePull{number: number, sha: fmt.Sprintf("%040x", number)}
}

// heads is the open pull requests 1..count, which a test gives a repository to fill more than
// one page of a query with: a query reads twenty heads, so 45 heads are three pages.
func heads(count int64) []fakePull {
	made := make([]fakePull, 0, count)
	for number := int64(1); number <= count; number++ {
		made = append(made, head(number))
	}
	return made
}

// headNumber is the pull request number head gave this commit, and false for a commit no head
// of these tests named, which is every other test's own head.
func headNumber(sha string) (int64, bool) {
	number, err := strconv.ParseInt(strings.TrimLeft(sha, "0"), 16, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

// runsOf is the path the stand-in lists one head's check runs at, which is the request a test
// stalls to make GitHub slower than the pass's own bound.
func runsOf(pull fakePull) string {
	return "/repos/" + gitHubRepository + "/commits/" + pull.sha + "/check-runs"
}

// suitesOf is the path the stand-in lists one head's check suites at, the second of the two
// reads a head read over REST takes: a limit answered there cuts the head short halfway
// through it.
func suitesOf(pull fakePull) string {
	return "/repos/" + gitHubRepository + "/commits/" + pull.sha + "/check-suites"
}

// headsAsked is the heads the stand-in was asked the check runs of since the request at from, in
// the order it was asked, each named by its pull request number. It is the order the pass took
// the heads it read over REST in, and it holds the head a bound cut off too: the stand-in logs a
// request before it answers it.
func (h *harness) headsAsked(from int) []int64 {
	h.t.Helper()
	var asked []int64
	for _, request := range h.github.received()[from:] {
		path, _, _ := strings.Cut(request.url, "?")
		commit, isRuns := strings.CutSuffix(path, "/check-runs")
		if !isRuns {
			continue
		}
		_, sha, named := strings.Cut(commit, "/commits/")
		if !named {
			continue
		}
		number, ok := headNumber(sha)
		if !ok {
			continue
		}
		asked = append(asked, number)
	}
	return asked
}

// text is one text field of the pass line, which every pass writes whether or not it read a
// head: an alarm reading the line must never have to tell a missing field from an empty one.
func text(t *testing.T, line map[string]any, name string) string {
	t.Helper()
	value, ok := line[name].(string)
	if !ok {
		t.Fatalf("the pass line has no %s: %v", name, line)
	}
	return value
}

// A pass that cannot read every page of every repository within its own bound ends on that
// bound cleanly: it logs the one line every pass logs, the line says the bound ended it and
// names the heads it did read, and nothing it never reached is named at WARN. The pass after it
// takes the cycle up at the page the bound cut off and reads to the last page of the order,
// where it ends -- a pass does not begin the order again -- and the pass after that one starts
// at the first page.
//
// This is what one pass in production could not do: 833 heads over 1,720 REST requests took 26
// minutes of a 5-minute interval and logged nothing until it was over, so the loop looked dead
// for 26 minutes and the pass held its lease for every one of them.
func TestAPassEndsOnItsBoundAndTheNextTakesTheCursorUpThere(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five open pull requests are three pages of twenty, twenty and five.
	h.listHeads(heads(45)...)
	h.reconciler.PassTimeout = 2 * time.Second
	// GitHub never answers the second page within what the pass has left, so the pass's own
	// bound ends it there.
	h.github.stallQuery(gitHubRepository, 1, time.Minute)

	h.pass()

	line := h.passLine()
	if line["deadline_reached"] != true {
		t.Fatalf("the pass reports deadline_reached = %v, want true: its own bound ended it: %v", line["deadline_reached"], line)
	}
	if line["cycle_complete"] != false {
		t.Errorf("the pass reports cycle_complete = %v, want false: it read one page of three: %v", line["cycle_complete"], line)
	}
	if got := number(t, line, "heads"); got != pullsPerQuery {
		t.Errorf("the pass read %d heads, want the %d of the page it finished before its bound: %v", got, pullsPerQuery, line)
	}
	if got, want := text(t, line, "first_head"), gitHubRepository+"#1"; got != want {
		t.Errorf("the pass's first_head = %q, want %q: %v", got, want, line)
	}
	if got, want := text(t, line, "last_head"), gitHubRepository+"#20"; got != want {
		t.Errorf("the pass's last_head = %q, want %q: %v", got, want, line)
	}
	// The query the bound killed is the pass ending, not a page that failed: reading its heads
	// over REST would be the round trips this pass exists to be rid of.
	if h.logged("ci reconcile read a repository over REST") {
		t.Errorf("the pass read a repository over REST, although its own bound ended it:\n%s", h.logs.String())
	}
	if h.logged("ci reconcile skipped a repository") {
		t.Errorf("the pass skipped a repository, although its own bound ended it:\n%s", h.logs.String())
	}

	// GitHub answers that page again, and the pass after it starts where this one stopped.
	h.github.stallQuery(gitHubRepository, 1, 0)
	askedBefore := len(h.github.queriesAsked())

	h.pass()

	line = h.passLine()
	if got, want := text(t, line, "first_head"), gitHubRepository+"#21"; got != want {
		t.Errorf("the second pass's first_head = %q, want %q, the first head of the page the bound cut off: %v", got, want, line)
	}
	if line["cycle_complete"] != true {
		t.Errorf("the second pass reports cycle_complete = %v, want true: it reached the last page: %v", line["cycle_complete"], line)
	}
	if asked := h.pagesAsked(askedBefore, gitHubRepository); !slices.Equal(asked, []int{1, 2}) {
		t.Fatalf("the second pass asked for pages %v, want the two the first pass did not finish", asked)
	}

	// A pass that reached the end of the order does not read the first page again: the pass
	// after it does, at the start of the next cycle.
	askedBefore = len(h.github.queriesAsked())

	h.pass()

	if asked := h.pagesAsked(askedBefore, gitHubRepository); !slices.Equal(asked, []int{0, 1, 2}) {
		t.Fatalf("the pass after a whole cycle asked for pages %v, want it to begin the order again at the first", asked)
	}
}

// A run at a page a pass's bound stopped short of is recorded by the pass after it, and a pass
// that reaches the last page of the order ends there rather than reading the first page again.
func TestAPageThePassDidNotReachIsReadByTheNextPass(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(heads(45)...)
	h.reconciler.PassTimeout = 2 * time.Second
	// The run the first pass never reaches: it is at a head of the third page.
	late := head(41)
	h.github.setCheckRuns(gitHubRepository, late.sha,
		atHead(gitHubRepository, late.sha, checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
	)
	h.github.stallQuery(gitHubRepository, 1, time.Minute)

	h.pass()

	if h.recordOf(late, 41) != nil {
		t.Fatalf("the first pass recorded a head of a page it never reached: %+v", h.recordOf(late, 41))
	}

	h.github.stallQuery(gitHubRepository, 1, 0)

	h.pass()

	state := h.recordOf(late, 41)
	if state == nil {
		t.Fatal("the pass after the cursor did not record the run at the page the bound cut off")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want the run of the page the first pass did not reach", state.Checks)
	}
}

// The gap between two REST requests is the REST work the pass expects divided into the share of
// its bound it may spend waiting, so a pass paces itself by the size of the job rather than by a
// fixed pause -- which is what made a pass of 1,681 requests take 26 minutes of a 5-minute
// interval. The work is the heads a query cannot answer whole, which are the only heads read
// over REST at all.
func TestTheGapComesFromTheWorkThePassExpects(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(head(7), head(8))
	h.github.refuseChecks(gitHubRepository, head(7).sha)
	h.github.refuseChecks(gitHubRepository, head(8).sha)
	h.reconciler.Pace = RequestPace
	h.reconciler.PassTimeout = time.Second

	// The first pass of a process knows of no work yet and reads at the floor, which is the
	// fastest it is ever allowed to read; it is the pass that learns the two heads there are.
	h.pass()

	if got, want := text(t, h.passLine(), "pace"), FastestRequestPace.String(); got != want {
		t.Fatalf("the first pass of the process paced itself at %s, want the floor %s: it knows of no work yet: %v",
			got, want, h.passLine())
	}

	h.pass()

	// Two heads read over REST are two listings each, one page apiece, under the one repository
	// whose identity the pass reads: five requests. Nine tenths of the one-second bound is
	// 900ms, and 900ms over five requests is 180ms.
	line := h.passLine()
	if got, want := text(t, line, "pace"), (180 * time.Millisecond).String(); got != want {
		t.Errorf("the pass paced itself at %s, want %s: five REST requests over nine tenths of a second: %v", got, want, line)
	}
	if got := number(t, line, "heads_known"); got != 2 {
		t.Errorf("the pass reports %d heads known, want the 2 open heads of the repository: %v", got, line)
	}
	if got := number(t, line, "heads_over_rest"); got != 2 {
		t.Errorf("the pass read %d heads over REST, want both: %v", got, line)
	}
}

// The gap never goes below the floor, however much work the pass has: the secondary limit the
// floor sits inside is the App's, and this pass is not the only reader the App pays for.
func TestTheGapNeverGoesBelowTheFloor(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(head(7), head(8), head(9), head(10), head(11), head(12))
	for _, pull := range []fakePull{head(7), head(8), head(9), head(10), head(11), head(12)} {
		h.github.refuseChecks(gitHubRepository, pull.sha)
	}
	// The pass that learns the six heads a query cannot answer waits for nothing and is bound
	// by nothing: what this test measures is the gap the pass after it sets itself, and thirteen
	// requests at the floor do not fit in the one-second bound that gives it that floor.
	h.pass()

	h.reconciler.Pace = RequestPace
	h.reconciler.PassTimeout = time.Second

	h.pass()

	// Six heads read over REST are twelve listing reads, and the repository's own identity:
	// thirteen requests, and 900ms over thirteen is 69ms, which is below the floor.
	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass paced itself at %s, want the floor %s: the arithmetic alone says 69ms: %v", got, want, line)
	}
	if got := number(t, line, "heads_known"); got != 6 {
		t.Errorf("the pass reports %d heads known, want the six whose arithmetic takes the gap below the floor: %v", got, line)
	}
}

// The gap never grows past the pause a deployment configures: a cycle with little work in it
// reads at Pace rather than minutes apart, so a head opened between two passes is still read at
// the pace the deployment asked for.
func TestTheGapNeverGrowsPastTheConfiguredPace(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.refuseChecks(gitHubRepository, headSHA)
	h.reconciler.Pace = RequestPace
	h.reconciler.PassTimeout = Interval

	// The pass that learns the one head there is.
	h.pass()

	h.pass()

	// One head over REST is three requests within a 5-minute bound, which is 90 seconds between
	// them by the arithmetic alone; the pass reads at the configured 500ms instead.
	line := h.passLine()
	if got, want := text(t, line, "pace"), RequestPace.String(); got != want {
		t.Errorf("the pass paced itself at %s, want the configured %s: %v", got, want, line)
	}
}

// A pass with no work expected of it at all paces itself at the floor rather than dividing the
// bound by it. A cycle whose heads the queries answered whole leaves the Reconciler no REST
// work to expect and the heads it listed still counted, so the answer for a process that knows
// of nothing yet is not the one that applies; a mapping emptied after such a cycle leaves no
// repository to read either, and the arithmetic that spreads the expected work over the bound
// then divides by zero. A pass panics into nothing -- its loop recovers none of it -- so the
// process would go down with it.
func TestAPassWithNoWorkExpectedOfItPacesItselfAtTheFloor(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.reconciler.Pace = RequestPace
	h.reconciler.PassTimeout = Interval

	// A cycle the query answered whole: the head is counted among the heads the pass knows
	// there are, and no head of it was read over REST, so the pass leaves no REST work behind.
	h.pass()
	if got := number(t, h.passLine(), "heads_known"); got != 1 {
		t.Fatalf("the first pass knows of %d heads, want the one the query answered: %v", got, h.passLine())
	}

	// The last mapping is taken away, as unmapping the one repository Dispatch reads does.
	if _, err := h.pool.Exec(h.ctx, `delete from repo_projects`); err != nil {
		t.Fatalf("unmap every repository: %v", err)
	}

	h.pass()

	line := h.passLine()
	if got, want := text(t, line, "pace"), FastestRequestPace.String(); got != want {
		t.Errorf("the pass paced itself at %s, want the floor %s: it expects no work at all: %v", got, want, line)
	}
}

// The pass's queries go one at a time and leave GitHub the processing time its secondary limit
// asks for: no two queries of a pass overlap, each is followed by a wait of a third of what the
// query before it took, and the query time the pass sustains is three quarters of the clock,
// which is 45 seconds of any minute.
//
// The stand-in answers every query in a fixed time, so the waits the pass leaves are the only
// thing between one query and the next and the arithmetic is the pass's own.
func TestQueriesGoOneAtATimeInsideGitHubsGraphQLBudget(t *testing.T) {
	const answersIn = 150 * time.Millisecond
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Sixty-one open pull requests are four pages, which is four queries and three waits.
	h.listHeads(heads(61)...)
	h.github.everyQueryTakes(answersIn)

	h.pass()

	asked := h.github.queriesAsked()
	if len(asked) != 4 {
		t.Fatalf("the pass asked %d queries, want the four pages of 61 open pull requests", len(asked))
	}
	for i, query := range asked {
		if query.started.IsZero() || query.ended.IsZero() {
			t.Fatalf("query %d was not timed by the stand-in: %+v", i, query)
		}
		if i == 0 {
			continue
		}
		before := asked[i-1]
		if query.started.Before(before.ended) {
			t.Errorf("query %d began at %s, before query %d ended at %s: the pass's queries overlapped",
				i, query.started, i-1, before.ended)
		}
		// The wait the pass leaves is what GitHub's budget asks of the query before it. The
		// stand-in's own answer takes a little longer than it was asked to, so the wait is
		// compared with the floor the pass's arithmetic gives, less a tenth for the clock.
		want := graphQLWait(answersIn) * 9 / 10
		if waited := query.started.Sub(before.ended); waited < want {
			t.Errorf("the pass waited %s between queries %d and %d, want at least %s: %s of query time a minute",
				waited, i-1, i, want, GraphQLTimePerMinute)
		}
	}
	if most := h.github.mostQueriesAtOnce(); most != 1 {
		t.Fatalf("the stand-in had %d queries in flight at once, want one: a pass queries serially", most)
	}

	// The rate the pass sustains, measured from the first query's start to the last one's: the
	// query time inside that span is at most three quarters of it, which is GraphQL's 45
	// seconds of every minute. The last query's own time is outside the span, since the wait
	// that pays for it comes before the query after it.
	span := asked[len(asked)-1].started.Sub(asked[0].started)
	var busy time.Duration
	for _, query := range asked[:len(asked)-1] {
		busy += query.ended.Sub(query.started)
	}
	// The two durations are multiplied out as floats: a Duration is nanoseconds in an int64,
	// and a millisecond times a minute overflows one.
	if allowed := time.Duration(float64(span) * float64(GraphQLTimePerMinute) / float64(time.Minute)); busy > allowed {
		t.Errorf("the pass kept GitHub busy %s of %s, want at most %s of it: %s of query time every %s",
			busy, span, allowed, GraphQLTimePerMinute, time.Minute)
	}
	// The same thing said as the limit itself says it: no 60-second window of the pass holds
	// more than 45 seconds of query time.
	if busiest := busiestMinute(asked); busiest > GraphQLTimePerMinute {
		t.Errorf("one minute of the pass held %s of query time, want at most %s", busiest, GraphQLTimePerMinute)
	}
}

// busiestMinute is the most query time any 60-second window of these queries holds. Each window
// begins at a query's start, which is where a window's load is heaviest: moving a window later
// can only drop a query out of it, and moving it earlier adds no query to it.
func busiestMinute(asked []fakeQuery) time.Duration {
	busiest := time.Duration(0)
	for _, from := range asked {
		window := from.started.Add(time.Minute)
		busy := time.Duration(0)
		for _, query := range asked {
			begin, end := query.started, query.ended
			if begin.Before(from.started) {
				begin = from.started
			}
			if end.After(window) {
				end = window
			}
			if end.After(begin) {
				busy += end.Sub(begin)
			}
		}
		busiest = max(busiest, busy)
	}
	return busiest
}

// requireConditional requires that every head read over REST since the request at from was read
// with the ETag GitHub answered an earlier read of that same URL with, and that the pass's own
// line says GitHub answered 304 for every head it read that way: the heads the pass went back
// over cost the App's budget nothing.
func requireConditional(t *testing.T, h *harness, from int, line map[string]any) {
	t.Helper()
	for _, request := range h.github.received()[from:] {
		if !strings.Contains(request.url, "/check-runs") && !strings.Contains(request.url, "/check-suites") {
			continue
		}
		if want := h.github.etagOf(request.url); request.ifNoneMatch != want {
			t.Errorf("a head was read with If-None-Match %q, want the ETag GitHub answered the read before it with, %q (%s)",
				request.ifNoneMatch, want, request.url)
		}
	}
	heads, notModified := number(t, line, "heads_over_rest"), number(t, line, "heads_not_modified")
	if heads == 0 || notModified != heads {
		t.Errorf("the pass had %d of the %d heads it read over REST answered 304, want every one of them: %v", notModified, heads, line)
	}
}

// A rate limit GitHub answers partway through one head's REST reads leaves the cursor at that
// head rather than past it: the head's two listings were not read whole, so the pass after the
// limit's reset reads that head again first rather than leaving it to the end of a later cycle.
// The head keeps its ETags through the cut, so that read is conditional and GitHub answers 304
// to it, which is what the reads of it that did finish before the limit are worth.
func TestARateLimitLeavesTheCursorAtTheHeadItCutShort(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHeads(head(7), head(8), head(9))
	for _, pull := range []fakePull{head(7), head(8), head(9)} {
		h.github.refuseChecks(gitHubRepository, pull.sha)
	}
	h.reconciler.PassTimeout = 2 * time.Second
	h.github.setCheckRuns(gitHubRepository, head(8).sha,
		atHead(gitHubRepository, head(8).sha, checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
	)

	// One whole cycle first, which reads every head in full and keeps the ETags GitHub answered
	// with.
	h.pass()

	if h.passLine()["cycle_complete"] != true {
		t.Fatalf("the first pass did not cover the three heads: %v", h.passLine())
	}

	// GitHub then answers the check suites of pull request 8 as an exhausted limit, which ends
	// the pass there: its check runs were read, its check suites were not, so the head was not
	// read whole.
	h.github.rateLimitNext(suitesOf(head(8)))

	h.pass()

	line := h.passLine()
	if line["rate_limited"] != true {
		t.Fatalf("the pass reports rate_limited = %v, want true: %v", line["rate_limited"], line)
	}
	if got, want := text(t, line, "last_head"), gitHubRepository+"#7"; got != want {
		t.Errorf("the pass's last_head = %q, want %q: the head the limit cut short was not read: %v", got, want, line)
	}
	if line["cycle_complete"] != false {
		t.Errorf("the pass reports cycle_complete = %v, want false: a limit ended it: %v", line["cycle_complete"], line)
	}

	// GitHub's reset passes, which takes the limit out of the ledger every Dispatch process
	// shares, and the pass after it takes the cycle up at the head the limit cut short.
	scope := githubapp.InstallationScope(h.github.installationID())
	limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
	if err != nil || !found {
		t.Fatalf("the limit GitHub answered with was recorded under %s: %v, %v", scope, found, err)
	}
	if err := h.reconciler.Limits.Clear(h.ctx, limit); err != nil {
		t.Fatalf("clear the limit as its reset does: %v", err)
	}
	sentBefore := h.github.count()

	h.pass()

	if asked := h.headsAsked(sentBefore); !slices.Equal(asked, []int64{8, 9}) {
		t.Fatalf("the pass after the reset read heads %v over REST, want it to start at 8, the head the limit cut short", asked)
	}
	requireConditional(t, h, sentBefore, h.passLine())
}

// A head whose reads were cut short keeps everything the Reconciler holds of it, ETags and all:
// a cut is the pass ending, not a head that failed, and the next pass reads that head again
// from the start. Dropping its ETags would read it in full for nothing -- in production 752 of
// 833 heads answer 304 -- and a long cycle cuts a head short at every pass.
//
// The reads of that head which did finish before the cut answer 304 after it, which is what the
// last pass of each case below proves: every head it reads is read with the ETag GitHub
// answered the read before it with, and GitHub answers each of them 304.
func TestAHeadCutShortKeepsItsETags(t *testing.T) {
	// kept is what the Reconciler holds of pull request 8's head, and whether it holds any.
	kept := func(h *harness) (headState, bool) {
		state, held := h.reconciler.heads[gitHubRepository+"#8@"+head(8).sha]
		return state, held
	}

	for _, cut := range []struct {
		name string
		cut  func(h *harness)
		undo func(h *harness)
	}{
		{
			name: "the pass's own bound",
			cut:  func(h *harness) { h.github.stall(runsOf(head(8)), time.Minute) },
			undo: func(h *harness) { h.github.stall(runsOf(head(8)), 0) },
		},
		{
			name: "a rate limit GitHub answered",
			cut:  func(h *harness) { h.github.rateLimitNext(suitesOf(head(8))) },
			undo: func(h *harness) {
				scope := githubapp.InstallationScope(h.github.installationID())
				limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
				if err != nil || !found {
					h.t.Fatalf("the limit GitHub answered with was recorded under %s: %v, %v", scope, found, err)
				}
				if err := h.reconciler.Limits.Clear(h.ctx, limit); err != nil {
					h.t.Fatalf("clear the limit as its reset does: %v", err)
				}
			},
		},
	} {
		t.Run(cut.name, func(t *testing.T) {
			h := newHarness(t, reviewerAppID)
			h.mapRepository(rowRepository)
			h.listHeads(head(7), head(8))
			h.github.refuseChecks(gitHubRepository, head(7).sha)
			h.github.refuseChecks(gitHubRepository, head(8).sha)
			h.reconciler.PassTimeout = 2 * time.Second
			h.github.setCheckRuns(gitHubRepository, head(8).sha,
				atHead(gitHubRepository, head(8).sha, checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
			)
			// The pass that reads both heads in full and keeps their ETags.
			h.pass()
			before, held := kept(h)
			if !held {
				t.Fatalf("the first pass kept nothing of the head it read in full: %v", h.passLine())
			}

			cut.cut(h)
			h.pass()

			after, held := kept(h)
			if !held {
				t.Fatalf("the head the %s cut short lost what the pass kept of it: %v", cut.name, h.passLine())
			}
			if len(after.runs) != len(before.runs) || len(after.suites) != len(before.suites) {
				t.Fatalf("the head the %s cut short kept %+v, want the %+v it had", cut.name, after, before)
			}

			// The pass after the cut is the proof of it: every head, that one included, is read
			// conditionally and GitHub answers 304.
			cut.undo(h)
			sentBefore := h.github.count()

			h.pass()

			requireConditional(t, h, sentBefore, h.passLine())
		})
	}
}

// The cycle after one that covered every head reads every head read over REST conditionally: a
// cycle is as many passes as its bound leaves it, and the pass that closes it forgets by what
// the whole cycle reached rather than by what it read itself. A pass forgetting by its own
// sight alone would drop the ETags of every head the passes before it read, and the cycle after
// it would read all of them in full -- in production, 752 heads of the 833 that answer 304
// today.
func TestASecondCycleReadsEveryHeadConditionally(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Twenty-five heads are two pages, and every one of them is read over REST, so a pass cut
	// short between the pages leaves the second page's heads to the pass after it.
	all := heads(25)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.refuseChecks(gitHubRepository, pull.sha)
	}
	h.reconciler.PassTimeout = 3 * time.Second
	// The first cycle takes two passes: the first is cut off at the second page, and the second
	// reads that page and reaches the end of the order, which closes the cycle.
	h.github.stallQuery(gitHubRepository, 1, time.Minute)

	h.pass()

	h.github.stallQuery(gitHubRepository, 1, 0)

	h.pass()

	if h.passLine()["cycle_complete"] != true {
		t.Fatalf("the second pass did not close the cycle: %v", h.passLine())
	}
	if held := len(h.reconciler.heads); held != len(all) {
		t.Fatalf("the Reconciler holds %d heads after the cycle closed, want the %d open ones", held, len(all))
	}

	h.pass()

	line := h.passLine()
	if got := number(t, line, "heads"); got != len(all) {
		t.Fatalf("the second cycle's pass read %d heads, want all %d: %v", got, len(all), line)
	}
	if got := number(t, line, "heads_not_modified"); got != len(all) {
		t.Errorf("the second cycle's pass had %d heads answered 304, want all %d: nothing changed on GitHub: %v", got, len(all), line)
	}
}
