package cireconcile

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// The heads a repository is known to have are counted once a cycle, however many passes the
// cycle takes. A page two passes share -- the first cut short inside it, the second taking it
// up at the cursor -- is one page of heads, and a count that both passes added would have the
// line report more heads open than the repository has, which is the number an operator reads
// the pass's progress against.
func TestAPageTwoPassesShareCountsItsHeadsOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Twenty-five heads are two pages, and every one of them is read over REST, so the pass can
	// be cut short in the middle of the second page.
	all := heads(25)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.refuseChecks(gitHubRepository, pull.sha)
	}
	h.reconciler.PassTimeout = 3 * time.Second
	// GitHub is slower than the first pass has left at the second head of the second page.
	h.github.stall(runsOf(head(22)), time.Minute)

	h.pass()

	first := h.passLine()
	if first["deadline_reached"] != true {
		t.Fatalf("the first pass reports deadline_reached = %v, want true: %v", first["deadline_reached"], first)
	}
	if got := number(t, first, "heads_known"); got != len(all) {
		t.Fatalf("the first pass reports %d heads known, want the %d of the two pages it listed before its bound cut it short inside the second: %v",
			got, len(all), first)
	}

	h.github.stall(runsOf(head(22)), 0)
	h.reconciler.PassTimeout = 0

	h.pass()

	line := h.passLine()
	if line["cycle_complete"] != true {
		t.Fatalf("the second pass did not reach the end of the order: %v", line)
	}
	if got := number(t, line, "heads_known"); got != len(all) {
		t.Fatalf("the cycle reports %d heads known, want the %d open heads of the repository: the page the two passes shared was counted twice: %v",
			got, len(all), line)
	}
}

// A cycle that reads a repository both ways counts each of its pages once. A page is counted by
// the pass that first lists it, and that rule is the same whether the page came from a query or
// from the REST listing a refused query sends the pass to: a cycle that counted the heads of
// one page twice would have the line report more heads open than the repository has, which is
// the number an operator reads the pass's progress against, and a cycle that counted none of
// them would report fewer. Every head is read once too, however the passes of the cycle split
// the pages between them.
//
// GitHub refuses the query for the second page alone, so the cycle reads its first page from a
// query and the two after it from the listing: a cursor carrying the fallback sends the pass
// taking it up to the listing for every page left, so refusing the first page would leave no
// page of the cycle read by a query at all.
func TestACycleThatReadsARepositoryBothWaysCountsEachPageOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five heads are three pages of a query, and three windows of the REST listing.
	all := heads(45)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		)
	}
	// GitHub will not answer the query for the repository's second page, so the pass reads the
	// first from a query and falls back to the listing at the second: the cursor of the page
	// after a refused one comes only in the answer the pass did not get, so every page from
	// there on is the listing's, this pass's and the next pass's alike.
	h.github.refusePage(gitHubRepository, 1)
	h.reconciler.PassTimeout = 3 * time.Second
	// GitHub is slower than the first pass has left at the second head of the second page.
	h.github.stall(runsOf(head(22)), time.Minute)

	h.pass()

	first := h.passLine()
	if first["deadline_reached"] != true {
		t.Fatalf("the first pass reports deadline_reached = %v, want true: %v", first["deadline_reached"], first)
	}
	if got, want := number(t, first, "graphql_queries"), 2; got != want {
		t.Fatalf("the first pass asked %d queries, want the %d it sent: the page it read and the page GitHub refused: %v", got, want, first)
	}
	if got, want := number(t, first, "heads_over_rest"), 1; got != want {
		t.Fatalf("the first pass read %d heads over REST, want the %d head of the refused page it finished before GitHub held the next one past its bound: %v",
			got, want, first)
	}
	if got, want := number(t, first, "heads_known"), 2*pullsPerQuery; got != want {
		t.Fatalf("the first pass reports %d heads known, want the %d of the two pages it listed -- one by query, one from the listing: %v",
			got, want, first)
	}

	// Nothing stalls now, so the pass after the bound reads the repository out from the page
	// the cursor left it at, which is the listing's for as long as this cycle lasts.
	h.github.stall(runsOf(head(22)), 0)
	h.reconciler.PassTimeout = 0

	h.pass()

	line := h.passLine()
	if line["cycle_complete"] != true {
		t.Fatalf("the second pass did not reach the end of the order: %v", line)
	}
	if got, want := number(t, line, "graphql_queries"), 0; got != want {
		t.Errorf("the second pass asked %d queries, want %d: it took up a repository the cursor says is read from the listing: %v", got, want, line)
	}
	if got, want := number(t, line, "heads_over_rest"), len(all)-pullsPerQuery-1; got != want {
		t.Errorf("the second pass read %d heads over REST, want the %d the first pass left it: %v", got, want, line)
	}
	if got := number(t, line, "heads_known"); got != len(all) {
		t.Fatalf("the cycle reports %d heads known, want the %d open heads of the repository: a page the two passes read was counted twice, or none of them: %v",
			got, len(all), line)
	}
	for _, pull := range all {
		if state := h.recordOf(pull, pull.number); state == nil {
			t.Fatalf("head %d has no record: the cycle left a head behind where the two passes met", pull.number)
		}
	}
}

// A pass taking up a repository it reads over REST starts at the pull request the cursor names
// and not at the place that pull request sat in the listing the pass before it read. Each of
// the two passes reads the listing of its own, and a pull request closed or merged between them
// moves every position after it down: a window cut at the position the page sat at would begin
// further on than the page did, the heads between would be read by no pass of this cycle, and
// the pass closing the cycle would record that it had covered them. One merge below the cursor
// is enough for that, and a number names the same place in the listing however many have closed
// below it.
func TestARepositoryReadOverRESTIsTakenUpAtTheNumberTheCursorNames(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five heads are three windows of the REST listing.
	all := heads(45)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		)
	}
	// GitHub answers no query for this repository's first page, so every page of it is read
	// from the REST listing, by this pass and by the pass that takes its cursor up.
	h.github.refusePage(gitHubRepository, 0)
	h.reconciler.PassTimeout = 3 * time.Second
	// GitHub is slower than the first pass has left at the second head of the second window.
	h.github.stall(runsOf(head(22)), time.Minute)

	h.pass()

	first := h.passLine()
	if first["deadline_reached"] != true {
		t.Fatalf("the first pass reports deadline_reached = %v, want true: %v", first["deadline_reached"], first)
	}
	if got, want := number(t, first, "heads_over_rest"), 21; got != want {
		t.Fatalf("the first pass read %d heads over REST, want the %d it finished before GitHub held the next one past its bound: %v",
			got, want, first)
	}

	// Five pull requests below the cursor are merged between the two passes, so every head the
	// second pass has left to read sits five places earlier in the listing than it did. The
	// head the cursor names is the number it was and five places from where it was.
	h.github.stall(runsOf(head(22)), 0)
	h.listHeads(all[5:]...)
	h.reconciler.PassTimeout = 0
	askedBefore := len(h.github.received())

	h.pass()

	var want []int64
	for number := int64(22); number <= int64(len(all)); number++ {
		want = append(want, number)
	}
	if asked := h.headsAsked(askedBefore); !slices.Equal(asked, want) {
		t.Fatalf("the pass after the bound read heads %v, want every head from the one the cursor names to the last, %v: it took the listing up at the position the cursor sat at rather than at the head it named",
			asked, want)
	}
	for _, pull := range all[21:] {
		if state := h.recordOf(pull, pull.number); state == nil {
			t.Fatalf("head %d has no record: the cycle left it behind where the two passes met", pull.number)
		}
	}
	line := h.passLine()
	if line["cycle_complete"] != true {
		t.Fatalf("the second pass reports cycle_complete = %v, want true: it read the repository out to its last head: %v", line["cycle_complete"], line)
	}
}

// pullsPath is where the stand-in lists the mapped repository's open pull requests, which is
// the read a test answers with a rate limit to end a pass in the listing a refused query sent
// it to.
const pullsPath = "/repos/" + gitHubRepository + "/pulls"

// The page a query was refused for is named by a number like every other page, and the pass
// taking the cursor up begins there. The pass that met the refusal read no head of that page
// -- the listing it fell back to answered a rate limit -- so the only thing that can name it
// is the page the query did answer before it: every head a query answers carries a number, so
// the refused page begins above the last of them. A page taken at the position it sat at
// would begin further on than the page did, since each pull request merged below it moves
// every position after it down, and the heads between would be read by no pass of this cycle
// while the pass that closed the cycle recorded that it had covered them.
func TestAPageAQueryWasRefusedForIsTakenUpAtItsNumber(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five heads are three pages of a query, and three windows of the REST listing.
	all := heads(45)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		)
	}
	// GitHub refuses the query for the second page and answers the listing the pass falls back
	// to with a rate limit, so the pass ends having read the first page and no head of the
	// second, and leaves the cursor at the page the refusal fell on.
	h.github.refusePage(gitHubRepository, 1)
	h.github.rateLimitNext(pullsPath)

	h.pass()

	first := h.passLine()
	if first["rate_limited"] != true {
		t.Fatalf("the first pass reports rate_limited = %v, want true: the listing it fell back to answered a limit: %v", first["rate_limited"], first)
	}
	if got, want := number(t, first, "heads"), pullsPerQuery; got != want {
		t.Fatalf("the first pass read %d heads, want the %d its one answered query held: the limit ended it before a head of the refused page: %v",
			got, want, first)
	}

	// The limit is cleared as its reset clears it, and five pull requests below the refused
	// page are merged between the two passes: every head left to read sits five places earlier
	// in the listing than it did, and carries the number it always had.
	scope := githubapp.InstallationScope(h.github.installationID())
	limit, found, err := h.reconciler.Limits.Get(h.ctx, scope)
	if err != nil || !found {
		t.Fatalf("the limit GitHub answered with was recorded under %s: %v, %v", scope, found, err)
	}
	if err := h.reconciler.Limits.Clear(h.ctx, limit); err != nil {
		t.Fatalf("clear the limit as its reset does: %v", err)
	}
	h.listHeads(all[5:]...)
	askedBefore := len(h.github.received())

	h.pass()

	var want []int64
	for number := int64(pullsPerQuery) + 1; number <= int64(len(all)); number++ {
		want = append(want, number)
	}
	if asked := h.headsAsked(askedBefore); !slices.Equal(asked, want) {
		t.Fatalf("the pass after the limit read heads %v, want every head from %d, where the refused page begins, to the last: %v. It cut the listing at the position that page sat at, which the merges below moved, rather than at the number that names it",
			asked, pullsPerQuery+1, want)
	}
	if state := h.recordOf(head(pullsPerQuery+1), pullsPerQuery+1); state == nil {
		t.Fatalf("head %d, the first of the page GitHub refused the query for, has no record: no pass of the cycle read it", pullsPerQuery+1)
	}
	for _, pull := range all {
		if state := h.recordOf(pull, pull.number); state == nil {
			t.Fatalf("head %d has no record: the cycle left it behind where the two passes met", pull.number)
		}
	}
	line := h.passLine()
	if got, want := number(t, line, "heads"), len(all)-pullsPerQuery; got != want {
		t.Fatalf("the second pass read %d heads, want the %d the first pass left it: with the %d of the first pass that is every head of the repository read once: %v",
			got, want, pullsPerQuery, line)
	}
	if line["cycle_complete"] != true {
		t.Fatalf("the second pass reports cycle_complete = %v, want true: it read the repository out to its last head: %v", line["cycle_complete"], line)
	}
}

// The same page, named the same way, inside one pass that nothing cuts short. A pull request
// below the refused page is merged while the pages before it are read, so the listing the pass
// falls back to at the refusal is shorter below that page than the pages the queries answered
// were: the page sits five places earlier in the listing than its position in the repository,
// and nothing but its number still finds it. The pages of a query are read one after another,
// and GitHub answers each of them from the pull requests open at the moment it is asked.
func TestARefusedPageIsReadFromItsNumberWithinOnePass(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five heads are three pages of a query, and three windows of the REST listing.
	all := heads(45)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		)
	}
	// GitHub refuses the query for the second page, so the pass reads the first from a query
	// and everything from the second on from the REST listing.
	h.github.refusePage(gitHubRepository, 1)
	// Five pull requests below the refused page are merged as the last head of the first page
	// is recorded, which is after the query that answered that page and before the query
	// GitHub refuses: the listing read next holds forty heads where the query's pages held
	// forty-five.
	h.mergeWhenRecorded(head(pullsPerQuery), all[5:])
	askedBefore := len(h.github.received())

	h.pass()

	var want []int64
	for number := int64(pullsPerQuery) + 1; number <= int64(len(all)); number++ {
		want = append(want, number)
	}
	if asked := h.headsAsked(askedBefore); !slices.Equal(asked, want) {
		t.Fatalf("the pass read heads %v over REST, want every head from %d, where the page GitHub refused begins, to the last: %v. It cut the listing at the position that page sat at before the merges, rather than at the number that names it",
			asked, pullsPerQuery+1, want)
	}
	if state := h.recordOf(head(pullsPerQuery+1), pullsPerQuery+1); state == nil {
		t.Fatalf("head %d, the first of the page GitHub refused the query for, has no record: the pass read the listing past it", pullsPerQuery+1)
	}
	line := h.passLine()
	if got := number(t, line, "heads"); got != len(all) {
		t.Fatalf("the pass read %d heads, want the %d of the repository: the %d its query answered and the %d from the refused page on: %v",
			got, len(all), pullsPerQuery, len(all)-pullsPerQuery, line)
	}
	if line["cycle_complete"] != true {
		t.Fatalf("the pass reports cycle_complete = %v, want true: nothing cut it short and it read the order out: %v", line["cycle_complete"], line)
	}
}

// A pull request merged below a page a query has already answered moves no page of the query
// either. The pass asks GitHub for the page after a cursor GitHub itself gave it, and that
// cursor is keyed by the last node of the page it came in rather than by how far into the
// result that page sat (cursor.after), so GitHub answers the heads above that key whatever has
// merged below. A cursor that was an offset would answer the twenty heads at the old position
// of the page, which the merges have moved down, and the heads between would be read by no pass
// of this cycle while the pass that closed the cycle recorded that it had covered them.
//
// Nothing is refused and nothing cuts the pass short here: this is one pass reading one
// repository by query alone, with five pull requests below the second page merged between the
// query that answered the first page and the query that asks for the second.
func TestAMergeBelowAQueryCursorMovesNoPageOfTheQuery(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	// Forty-five heads are three pages of a query.
	all := heads(45)
	h.listHeads(all...)
	for _, pull := range all {
		h.github.setCheckRuns(gitHubRepository, pull.sha,
			atHead(gitHubRepository, pull.sha, checkRun(100+pull.number, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z")),
		)
	}
	// Five pull requests below the second page are merged as the last head of the first page is
	// recorded, which is after the query that answered that page and before the query that asks
	// for the one after it: GitHub answers that second query from forty open pull requests where
	// the first was answered from forty-five.
	h.mergeWhenRecorded(head(pullsPerQuery), all[5:])

	h.pass()

	for _, pull := range all {
		if state := h.recordOf(pull, pull.number); state == nil {
			t.Errorf("head %d has no record: the query after the merges answered a page cut at the position the one before it ended at, and the heads between were read by nobody",
				pull.number)
		}
	}
	line := h.passLine()
	if got := number(t, line, "heads"); got != len(all) {
		t.Fatalf("the pass read %d heads, want the %d open when it began, each once: %v", got, len(all), line)
	}
	if line["cycle_complete"] != true {
		t.Fatalf("the pass reports cycle_complete = %v, want true: nothing cut it short and it read the order out: %v", line["cycle_complete"], line)
	}
}

// mergeWhenRecorded merges the pull requests of the mapped repository that are not in left the
// moment the pass records a check run of head at: the stand-in answers the repository's open
// pull requests with left from then on.
//
// It is how a test puts a merge inside one pass, between the query that answered a page and
// the read that follows it. Nothing else the harness holds happens there: a query's page costs
// no request of its own per head, so the writes of its heads are the one thing a test can
// reach between two queries.
func (h *harness) mergeWhenRecorded(at fakePull, left []fakePull) {
	open := h.reconciler.Open
	number := strconv.FormatInt(at.number, 10)
	h.reconciler.Open = func() (Writer, error) {
		writer, err := open()
		if err != nil {
			return nil, err
		}
		return &mergingWriter{Writer: writer, at: number, merge: func() { h.listHeads(left...) }}, nil
	}
}

// mergingWriter is the CI record writer with merge run after the check runs of one head are
// recorded (mergeWhenRecorded).
type mergingWriter struct {
	Writer
	at    string
	merge func()
}

func (w *mergingWriter) Record(ctx context.Context, observation contracts.CIObservation) (bool, error) {
	changed, err := w.Writer.Record(ctx, observation)
	if observation.Number == w.at {
		w.merge()
	}
	return changed, err
}
