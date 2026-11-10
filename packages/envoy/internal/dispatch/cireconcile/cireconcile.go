// Package cireconcile fills Envoy's CI records in from GitHub's own answers.
//
// The listener records a check run or a check suite from the webhook GitHub sends it, and the
// Legion daemon reads the settlement those records produce (`ci-ok`, `pr-title`). A webhook that
// reaches no record leaves that head short of a check, and a head short of a check never settles:
// the daemon waits for an answer that never comes. GitHub holds the same objects the webhook
// carries, so every Interval a pass reads each open pull request's head and records every
// completed check run and completed check suite the listener's record of that head lacks.
//
// A pass reads GitHub in batches, over GraphQL: one query answers twenty open pull requests of
// one repository with every check suite and every check run of each one's head commit
// (headsQuery). Reading the same heads over REST took two requests of each head and one of each
// page of each repository's open pull requests, which for 833 heads of 16 repositories measured
// 1,720 requests and 26 minutes 25 seconds -- five passes' worth of a 5-minute interval, so the
// pass never finished and the loop looked dead for the whole of it. The same read in queries is
// about 53 of them, about 318 points of the App's GraphQL budget and about 178 seconds of
// GitHub's own processing time.
//
// What a query cannot answer whole is read over REST exactly as every head was read before:
// paged, conditional on the ETags the pass kept, and recorded through the same writes. That is
// a head with more check suites than one query asks for, a head one of whose suites has more
// check runs than one query asks for, a head whose Checks fields GitHub refused or answered
// null, and every head of a page GitHub refused, gave up on, or answered past githubapp's
// 1 MiB response limit (Reconciler.sweep, Reconciler.restOfRepository).
//
// An object read either way maps through contracts.GithubCIObservation, the mapping the
// webhook's own body goes through, so a pass over a run the listener already recorded changes
// nothing: the observation is the one the webhook made, and cistore's write applies an unchanged
// observation as no change. A query's answer is mapped field by field into the shape the
// webhook's body carries before that (checkRunObject, checkSuiteObject), so a run read in a
// query and the same run delivered by webhook are one observation. The pass writes with a
// cistore writer, which binds the listener's bucket and creates none (cistore.OpenWriter), so
// one record per head is shared by both processes.
//
// GitHub budgets the App, not a process, so the pass reads as GitHub asks a poller to: serially,
// one request after another and never two at once. A query is paced by GitHub's secondary limit
// on GraphQL processing time (graphQLPace); a REST request is paced by the gap the pass sets
// itself from the work it expects (Reconciler.gap); every REST read is conditional on the ETag
// the pass kept from its last read of that same page, which costs the budget nothing when GitHub
// has not changed the page; and the shared rate limits every Dispatch process obeys
// (githubapp/ratelimit) are read before the pass starts, before each repository, before each
// query and before each head, with a limit in force ending the pass where it stands. GitHub
// budgets GraphQL apart from REST and answers a GraphQL limit in its own way, so a limit a query
// meets is recorded under githubapp.GraphQLScope and leaves every REST read of every process
// going (githubapp.GraphQLRateLimit).
//
// One pass is bound at the interval its loop runs it on (Reconciler.PassTimeout), so it always
// ends before the pass after it is due and always logs its one line. The pass walks one stable
// order -- the mapped repositories, and within each its open pull requests by number -- from the
// first repository to the last page of the last one, and keeps a cursor in it. A pass cut off by
// its bound or by a rate limit leaves the cursor at the first page or REST head it did not
// finish and the pass after it takes up there; a pass that reaches the end of the order ends
// there rather than beginning again, and leaves the cursor at the start of the order for the
// pass after it. No head starves behind the heads the order holds before it.
//
// One sweep from the start of the order to the end of it is a cycle, which takes as many passes
// as their bounds leave them, and what the Reconciler keeps of a head is forgotten only where a
// cycle closes and only when no pass of that cycle saw the head: a cut head keeps its ETags,
// since a cut is the pass ending rather than a head that failed.
//
// The pass is a read of GitHub and a write of NATS; it holds no lease of its own. Lease names the
// one a caller's loop runs it under, so one process's pass is what the App's budget pays for in a
// period however many processes run.
package cireconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// Interval is how often the running pass reads GitHub.
	Interval = 5 * time.Minute

	// Lease names the lease one pass runs under. It is not what makes the pass safe to run twice
	// -- every write it makes is a compare-and-swap that applies an observation GitHub already
	// holds, so two passes at once write one record -- it is the GitHub budget: each pass reads
	// every open pull request of every mapped repository, and N processes passing is N times those
	// reads for one result.
	Lease = "ci-reconcile"

	// RequestPace is the pause a running pass leaves between the requests it sends GitHub: two a
	// second, the pace the webhook redelivery sweep's own requests keep. A caller sets it on Pace;
	// a test sets Pace to zero and waits for nothing.
	RequestPace = 500 * time.Millisecond

	// FastestRequestPace is the shortest gap a pass will ever leave between its requests,
	// however much work it has to pace itself over: ten requests a second. GitHub's secondary
	// rate limits allow an App no more than 900 points a minute and a read of this kind costs
	// one point, so ten a second sits inside that limit with a third of it to spare for the
	// other readers the App pays for -- the listener's own reads, the webhook redelivery sweep,
	// every API call a person's click makes.
	FastestRequestPace = 100 * time.Millisecond

	// maxPages bounds one listing's pages, so a pass can never read on for ever: an answer GitHub
	// keeps reporting full past this many pages is read as a failed listing, which the next pass
	// reads again from its first page. A hundred pages is ten thousand objects of one listing,
	// far past any head or any repository's open pull requests.
	maxPages = 100
)

// Writer is the CI record store a pass writes through: the listener's bucket bound by
// cistore.OpenWriter, which creates nothing. It is an interface rather than the writer itself so
// that a caller may put its own writer in the pass's way, as every other collaborator of a
// Reconciler is its caller's to choose; *cistore.Writer satisfies it. Its two record calls answer
// whether the observation changed the head's record, which is how the pass knows a head to count.
type Writer interface {
	Record(context.Context, contracts.CIObservation) (bool, error)
	RecordSuite(context.Context, contracts.CIObservation) (bool, error)
	HasRecord(owner, repo, number, sha string) (bool, error)
}

// Reconciler reads GitHub for the CI observations the listener's records lack and writes them.
// Its collaborators are fields its caller sets, as redeliver.Sweeper's are, so a test passes its
// own. One Reconciler runs one pass at a time: it remembers between passes the ETags GitHub
// answered with and the pages they belong to, and nothing guards that memory.
type Reconciler struct {
	// Pool reads repo_projects, the repositories Dispatch is mapped to.
	Pool *store.Pool
	// GitHub reads the repositories, their open pull requests and each head's check objects, as
	// the App the listener's webhook is of (R5: no credential of its own).
	GitHub *githubapp.Client
	// Limits is the GitHub rate limits every Dispatch process shares (github_rate_limits). Nil
	// shares none: the pass then stops only at GitHub's own answers, which is what a test that is
	// not about limits wants.
	Limits *ratelimit.Store
	// Open binds the CI record writer (cistore.OpenWriter), as redeliver.Sweeper.Open binds its
	// bucket. Every pass calls it and writes through the writer it answers with, binding afresh
	// rather than keeping one. It fails while NATS is not connected, and while the listener has
	// not made the bucket; a pass it fails for reads GitHub nothing and leaves the binding to a
	// later one.
	Open func() (Writer, error)
	// ReviewerAppID is the review App's id, Dispatch's DISPATCH_REVIEWER_APP_ID. A verdict check
	// (`tester`, `architect`) is recorded only when that App created it
	// (contracts.ReviewerVerdictDecision); unset, no verdict check is recorded at all.
	ReviewerAppID string
	// Logger takes the one INFO line each pass writes and the WARN lines of what it skipped. It is
	// required.
	Logger *slog.Logger
	// Pace is the pause left between requests to GitHub, and the slowest a pass ever reads: the
	// gap a pass sets itself from the work it expects is never above it (Reconciler.gap). Zero
	// waits for nothing.
	Pace time.Duration
	// PassTimeout is how long one pass may run, which its caller sets to the interval its loop
	// runs the pass on. A pass that has not covered every head by then ends where it stands,
	// logs its one line and leaves the next pass the cursor it reached. Zero is unbounded.
	PassTimeout time.Duration
	// Instance answers this process's row in the registry of live server processes, the id
	// processes.Register gave it, and "" while the process holds none yet. It is what each pass
	// records its row under (beginPass), and the registry row it names is what says later
	// whether a pass with no end time is still running (RecentPasses).
	//
	// It is a function rather than the id itself because cmd/dispatch starts the loops before it
	// registers the process: registration waits out a live peer whose signing keys do not match
	// this one's, and a loop must not wait on that. A nil Instance, and one that answers "", is
	// a pass that records no row at all.
	Instance func() string

	// rows is what the last pass learned of each repository the pass read, keyed by the canonical
	// name it read it under, which two mappings renamed onto one repository share as one entry:
	// the ETag of GitHub's answer about it, and the name and id that answer gave.
	rows map[string]rowState
	// pulls is each repository's open pull request pages as the last pass read them over REST,
	// keyed by the id GitHub gave the repository, so two rows naming one repository share one
	// listing. Only a repository a query was refused a page of is listed over REST at all
	// (Reconciler.restOfRepository), so most repositories hold no entry here.
	pulls map[int64][]pageOf[githubapp.PullRequestHead]
	// listed is how many open heads each repository's queries answered with, keyed by the id
	// GitHub gave the repository: what the pass's line reports as the heads it knows there are,
	// and what a pass that reads no head of a repository still knows of it. It is built up as a
	// repository's pages are read, over however many passes the cursor takes them, and reset
	// where a pass begins that repository at its first page.
	listed map[int64]int
	// heads is what the last pass kept of each head it read over REST (pullHead.key): a head a
	// query answered whole costs no REST request and so holds no ETag of its own.
	heads map[string]headState
	// resume is where the next pass starts: the first page or REST head the last pass did not
	// finish, and the zero cursor -- the start of the order -- after a pass that reached the end
	// of it.
	resume cursor
	// seenRows, seenRepositories and seenHeads are everything the passes of the cycle now
	// running have reached, keyed as the maps above are. They are what the pass closing the
	// cycle forgets by, and they are cleared with it.
	//
	// They are the cycle's rather than one pass's because a cycle takes as many passes as its
	// bound leaves it: a pass forgetting by what it alone saw would drop the ETags of every
	// head the passes before it read, and the cycle after would read every one of those heads
	// in full. In production 752 of 833 heads answer 304, which is what those ETags buy.
	seenRows         map[string]struct{}
	seenRepositories map[int64]struct{}
	seenHeads        map[string]struct{}
}

// cursor is one place in the order a pass walks: a mapped repository, one page of its open pull
// requests, and a pull request number within that page.
//
// As the Reconciler's resume it is where the next pass starts, and it is set only by a pass
// that was cut short -- by its own bound or by a rate limit -- at the first page or REST head
// that pass did not finish. Its zero value is the start of the order, which is where a process
// that has run no pass yet begins and where a pass that reached the end of the order leaves the
// pass after it: a pass does not wrap round, so the cursor is never a place a finished pass
// stopped at.
//
// after is GitHub's own cursor for the page -- the end cursor of the page before it, empty for
// a repository's first page -- which is the only way to ask GitHub for a page again, and page
// is that page's position in the repository, counting from zero, which is what names the heads
// of it in a REST listing when the query for it is refused. number is the first head of the
// page the pass did not finish, and zero is the whole page: a head below it was read by the
// pass that left the cursor.
//
// It lives in this process's memory beside the ETags the Reconciler keeps, and is written
// nowhere. A restart loses it and deliberately begins a new cycle at the first head of the
// order: the cursor is how a pass too big for its bound shares the order out between passes,
// not a record of what has been read, so losing it costs one cycle its fairness and nothing
// else, and a process restarts far less often than a pass runs.
type cursor struct {
	repository string
	after      string
	page       int
	number     int64
}

// rowState is what a pass learned of one reconciled repository from GitHub.
type rowState struct {
	// etag is the ETag of GET /repos/{row}, which the next pass reads conditionally on.
	etag string
	// fullName and id are what that answer gave: the name GitHub itself spells the repository
	// with, and the id two names of one repository share.
	fullName string
	id       int64
}

// headState is what a pass kept of one head: the pages of its two listings, each with the ETag
// the next pass reads it conditionally on and no items of its own (withoutItems).
type headState struct {
	runs   []pageOf[object]
	suites []pageOf[object]
	// observed says the last full answer for this head held at least one observation the rules say
	// to record. Such a head is worth reading again in full when its record has gone (the bucket's
	// TTL, or a delete), so the next pass asks HasRecord before it trusts these ETags. A head whose
	// answer held nothing to record has no record to lose, and keeps its ETags whatever the bucket
	// holds.
	observed bool
}

// object is one GitHub check run or check suite as its listing answers it, the shape
// contracts.GithubCIObservation maps.
type object = map[string]any

// stop is the rate limit that ended a pass: the scope it was found or answered under, and when
// GitHub will take a request under it again.
type stop struct {
	scope string
	until time.Time
}

// counts is what one pass did, as its one log line reports it.
type counts struct {
	// requests counts every request the pass sent GitHub, queries and REST reads together.
	requests int
	// queries counts the GraphQL queries among them, points what GitHub charged them against
	// the App's GraphQL budget (the answers' own rateLimit.cost), and restRequests the REST
	// reads: a line's three numbers say how much of the pass GraphQL answered and how much it
	// could not.
	queries      int
	points       int
	restRequests int
	// restNotModified counts the REST reads GitHub answered 304, which cost the budget nothing.
	restNotModified int
	// restHeads counts the heads read over REST, which a query could not answer whole.
	restHeads int
	// graphQLTook is how long GitHub took over this pass's last query, which is what the wait
	// before the next one is set from (graphQLPace).
	graphQLTook time.Duration
	// listPages counts the open pull request pages read over REST, heads the heads the pass
	// read either way, and headRequests the requests the heads read over REST sent.
	listPages    int
	heads        int
	headRequests int
	// notModified counts the heads every page of whose two listings answered 304.
	notModified int
	// records counts the records the pass changed, one for each head a write of the pass changed
	// the record of: a head's runs and suites all write the one record cistore.Key names, so a
	// head whose run and whose suite the pass both recorded counts once. An observation the
	// store applied as no change, which is what every observation of a head the listener already
	// recorded is, changes no record, so a pass that mended nothing reports none.
	records int
	// mappingsSkipped counts the repositories the pass skipped, one for each canonical name it
	// could not reconcile, so mappings renamed onto one repository are skipped once between
	// them; tooLarge counts the heads whose answer was past what githubapp reads.
	mappingsSkipped int
	tooLarge        int
	// pace is the gap the pass left between its requests, which it set from the work it expected
	// before it read anything (Reconciler.gap).
	pace time.Duration
	// deadline says the pass's own bound ended it, so the heads it did not reach are the next
	// pass's to read; cycle says it covered every head of the order and left the cursor back at
	// the start of it.
	deadline bool
	cycle    bool
	// firstHead and lastHead name the first and the last head the pass read whole, each as
	// `owner/repo#number`, which is the slice of the one order this pass took.
	firstHead string
	lastHead  string
}

// mark records one head the pass read whole, which the line names when it is the first or the
// last of them: two passes' lines side by side then say how far round the order each got.
func (c *counts) mark(head pullHead) {
	name := head.repository() + "#" + head.number
	if c.firstHead == "" {
		c.firstHead = name
	}
	c.lastHead = name
}

// Pass runs one reconcile pass within ctx and logs exactly one INFO line for it: the pass line
// below, or, for a pass that could not bind the CI record writer, the line saying so. It also
// records the pass in ci_reconcile_passes, which is that line in a table a person can read a day
// later, after the container log has rotated, and one thing more: how the pass ended, which the
// line does not say, since a pass its caller cancelled logs the line a pass that finished logs
// (passes.go).
//
// The pass's loop belongs to its caller, which runs it under Lease every Interval. PassTimeout is
// that same interval, and the pass is bound at it: a pass too big for one interval ends on that
// bound rather than holding its loop's lease past the pass that is due next, and the pass after
// it takes the cycle up where this one left it (reconcile).
func (r *Reconciler) Pass(ctx context.Context) {
	started := time.Now()
	var c counts
	// The context the caller handed this pass, kept before the pass's own bound goes on it.
	// Its cancellation is the process shutting down, which the row records apart from the
	// bound: the two end a pass for different reasons, and neither is a pass that finished
	// (passOutcome).
	caller := ctx
	if r.PassTimeout > 0 {
		// Every read of the pass is made within this, and so is every gap it leaves between two
		// of them, so the pass ends whatever it is waiting for. Zero is unbounded, which is what
		// a test that is not about the bound wants.
		bounded, done := context.WithTimeout(ctx, r.PassTimeout)
		defer done()
		ctx = bounded
	}
	// The row this pass leaves behind, opened before it reads anything and closed on every way
	// out below. at is the rate limit that ended the pass, read by the deferred close beside the
	// caller's context: a pass that ends at a limit records that, one its caller cancelled
	// records the cancellation, and a pass whose own bound or whose process's shutdown cut it
	// short records its end all the same, because endPass writes on a context of its own
	// (endWrite).
	var at *stop
	record := r.beginPass(ctx)
	defer func() { r.endPass(record, &c, at, caller.Err() != nil) }()
	// A limit in force ends the pass before it binds anything or reads GitHub: the App's budget is
	// shared, and a process that waits out a limit another met is why it is recorded at all.
	if at = r.limited(ctx, githubapp.AppScope); at != nil {
		r.report(&c, at, time.Since(started))
		return
	}
	// The writer is bound for this pass alone, never kept for the next: when nats.go gives up on
	// a connection the bus dials a replacement and installs it under the client, and a writer
	// held from before that would write through the connection nats.go gave up on, so every
	// later pass would read GitHub in full and record nothing until a restart. One KV bind is
	// one JetStream request every five minutes, which is nothing beside the GitHub reads the
	// pass makes, and it leaves no handle to go stale.
	writer, err := r.Open()
	if err != nil {
		// The listener makes the CI bucket and Dispatch's NATS connects in the background, and
		// every pass binds the writer afresh, so a pass before either, or one while NATS is down
		// at any later moment of the process's life, is no fault: it reads GitHub nothing, logs
		// no pass line, and a later pass binds the writer again. Its row is closed as complete
		// with every count at zero, which is what such a pass did.
		r.Logger.Info("ci reconcile pass bound no CI record writer", "error", err)
		return
	}
	at = r.reconcile(ctx, writer, &c)
	r.report(&c, at, time.Since(started))
}

// reconcile is one pass over the mapped repositories from the cursor onwards, and the stop that
// ended it early.
//
// The order is mapped()'s, which is sorted, and within a repository its open pull requests by
// number (headsQuery orders them, and the REST fallback sorts them): one order every pass
// walks, so a pass that does not reach the end of it can say where the next pass is to take it
// up. A pass starts at the cursor, reads to the end of the order and ends there -- it does not
// begin the order again -- and it ends early on its own bound or at a rate limit.
//
// The sweep those passes make together is the cycle: one walk of the order from its first
// repository to the last page of its last, however many passes it takes. The pass that reaches
// the end of the order closes it (closeCycle) and leaves the cursor at the start of the order,
// so the pass after it begins the next cycle there.
func (r *Reconciler) reconcile(ctx context.Context, writer Writer, c *counts) *stop {
	rows, err := r.mapped(ctx)
	if err != nil {
		r.Logger.Warn("ci reconcile read no repository mappings", "error", err)
		return nil
	}
	// The gap is set before the pass's first REST request and left between every one of them
	// after it: the REST work the last pass learned there is, spread over the share of this
	// pass's own bound it may spend waiting.
	c.pace = r.gap(len(rows))
	// What this pass reaches is added to what the passes of this cycle have reached already
	// (seenRows and the two beside it), which only the pass that closes the cycle reads: a
	// cycle takes as many passes as its bound leaves it, and what one of them saw is what the
	// whole of it saw. read is this pass's own, the repositories it has already read the heads
	// of, which two rows naming one repository share.
	read := make(map[int64]struct{}, len(rows))
	for _, visit := range order(rows, r.resume) {
		// The pass's bound is checked between repositories as well as between pages and heads,
		// so a pass whose time is up ends where it stands rather than reading on through the
		// repositories after this one. What it leaves behind is the next pass's to read, from
		// this cursor.
		if r.over(ctx, c) {
			r.resume = visit.cursor()
			return nil
		}
		// Another process may have met a limit since this pass started, so the shared limits are
		// read again before each repository.
		if at := r.limited(ctx, githubapp.AppScope, githubapp.GraphQLScope); at != nil {
			r.resume = visit.cursor()
			return at
		}
		repository, at := r.resolve(ctx, visit.row, c, read)
		if at != nil {
			r.resume = visit.cursor()
			return at
		}
		if r.over(ctx, c) {
			// The bound fell while this repository was being resolved, which is no fault of the
			// mapping's and no head of it read: the next pass reads it from the top.
			r.resume = visit.cursor()
			return nil
		}
		if !repository.reconcile {
			// A repository the pass holds no token for, one GitHub did not answer about, or one
			// another row naming the same repository was read under. resolve has said so once.
			continue
		}
		at, cut := r.sweep(ctx, writer, repository, visit, c)
		if cut {
			// sweep left the cursor at the page or the head it was cut short at.
			return at
		}
	}
	// The pass reached the end of the order, so every head of it has been read by this pass or
	// by one before it in this cycle, and the cycle closes.
	r.closeCycle(c)
	return nil
}

// sweep reads one repository from the page the visit names to its last, and the heads of each
// page. cut says the pass ended inside the repository -- on its own bound or at a rate limit --
// in which case the cursor is already at the page or the head the next pass is to take up, and
// the stop beside it is the limit that ended the pass, nil for the bound.
//
// The pages are GitHub's own: a query answers twenty open pull requests and the cursor the page
// after them begins at, so the pass asks for one page, reads its heads, and asks for the next
// with that cursor until GitHub says there is no next page.
func (r *Reconciler) sweep(ctx context.Context, writer Writer, repository reconciled, at visit, c *counts) (*stop, bool) {
	if _, held := r.listed[repository.id]; at.page == 0 || !held {
		// The pass begins this repository at its first page, so what the passes before it
		// counted of the repository's heads is this cycle's and is counted again here.
		remember(&r.listed, repository.id, 0)
	}
	here := cursor{repository: at.row, after: at.after, page: at.page, number: at.number}
	for {
		if r.over(ctx, c) {
			r.resume = here
			return nil, true
		}
		if stopped := r.limited(ctx, append(repository.headScopes, githubapp.GraphQLScope)...); stopped != nil {
			r.resume = here
			return stopped, true
		}
		page, err := r.query(ctx, repository, here.after, c)
		if stopped := stopAt(err, githubapp.GraphQLScope); stopped != nil {
			// A rate limit is the pass ending, never a page to read another way: reading this
			// page's heads over REST would be exactly the requests GitHub has just asked this
			// App to stop sending.
			r.resume = here
			return stopped, true
		}
		if err != nil {
			if r.over(ctx, c) {
				// The pass's own ending killed the query, which says nothing about the page.
				r.resume = here
				return nil, true
			}
			// GitHub refused the query, gave up on it, or answered past what githubapp reads.
			// The cursor of the page after this one comes only in the answer the pass did not
			// get, so there is no next page to ask for: the rest of this repository, from this
			// page's own heads onwards, is read over REST, which is how every head of it was
			// read before this change.
			r.Logger.Warn("ci reconcile read a repository over REST: GitHub did not answer the query",
				"repository", repository.owner+"/"+repository.repo, "page", here.page, "error", err)
			return r.restOfRepository(ctx, writer, repository, here, c)
		}
		r.listed[repository.id] += countHeads(page.heads)
		for _, head := range page.heads {
			if head.sha == "" || head.number < here.number {
				// A pull request GitHub listed with no head commit is no head to read, and a
				// head below the cursor's number was read by the pass that left the cursor.
				continue
			}
			here.number = head.number
			stopped, cut := r.visitHead(ctx, writer, repository, head, here, c)
			if cut {
				return stopped, true
			}
		}
		if !page.hasNextPage {
			return nil, false
		}
		here = cursor{repository: at.row, after: page.endCursor, page: here.page + 1}
	}
}

// visitHead reads and records one head of a page: from the query's own answer where it could
// answer the head whole, and over REST where it could not. cut says the pass ended at this
// head, in which case the cursor names it and the next pass reads it again from the start.
func (r *Reconciler) visitHead(ctx context.Context, writer Writer, repository reconciled, head queryHead, here cursor, c *counts) (*stop, bool) {
	pull := pullHead{
		token:  repository.token,
		scope:  repository.scope,
		owner:  repository.owner,
		repo:   repository.repo,
		number: strconv.FormatInt(head.number, 10),
		sha:    head.sha,
	}
	if !head.overflow {
		// The query answered this head whole, so there is nothing to ask GitHub and nothing
		// the pass's bound can cut short: the writes alone are left.
		c.heads++
		_, changed, _ := r.record(ctx, writer, pull, pagesOf(head.runs), pagesOf(head.suites))
		if changed {
			c.records++
		}
		c.mark(pull)
		return nil, false
	}
	// The query could not answer this head whole, so it is read over REST, paged and
	// conditional on the ETags the pass kept of it. Its reads are budgeted under the
	// installation rather than under GraphQL, so the shared limits are read again for them.
	if stopped := r.limited(ctx, repository.headScopes...); stopped != nil {
		r.resume = here
		return stopped, true
	}
	r.Logger.Info("ci reconcile reads a head over REST",
		"repository", pull.repository(), "pr", pull.number, "sha", pull.sha, "why", head.why)
	see(&r.seenHeads, pull.key())
	if stopped := r.readHead(ctx, writer, pull, c); stopped != nil {
		r.resume = here
		return stopped, true
	}
	if r.over(ctx, c) {
		// The bound fell while this head was being read, so it was not read whole and the next
		// pass takes the cycle up at it. A head that was read whole just as the bound fell is
		// read again by that pass, which costs it the head's two conditional requests and
		// nothing more: the honest place to take a cycle up is the last head the pass finishing
		// it is sure of.
		r.resume = here
		return nil, true
	}
	c.restHeads++
	c.mark(pull)
	return nil, false
}

// restOfRepository reads one repository's open pull requests over REST and every head of it
// from the refused page onwards, which is what a pass does with a page GitHub would not answer
// the query for. It is the read every head of every repository took before this change: the
// repository's open pull requests page by page, each conditional on its own ETag, and each
// head's check runs and check suites the same way.
//
// The heads of the refused page are named by where the page sits in the repository: GitHub's
// own order for the query and the order this listing is sorted into are both the pull request
// number ascending, so the page counting from zero holds the heads at those positions of the
// listing. The pass reads from there to the end of the repository rather than that page alone,
// since the pages after a page it did not get are pages it cannot ask for.
func (r *Reconciler) restOfRepository(ctx context.Context, writer Writer, repository reconciled, from cursor, c *counts) (*stop, bool) {
	pulls, at, err := r.openPullRequests(ctx, repository.id, repository.owner, repository.repo, repository.token, repository.scope, c)
	if at != nil {
		r.resume = from
		return at, true
	}
	if err != nil {
		if r.over(ctx, c) {
			r.resume = from
			return nil, true
		}
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile read no open pull requests of a repository",
			"repository", repository.owner+"/"+repository.repo, "error", err)
		return nil, false
	}
	window := from.page * pullsPerQuery
	if window > len(pulls) {
		window = len(pulls)
	}
	pulls = pulls[window:]
	r.listed[repository.id] += len(pulls)
	here := from
	for _, pull := range pulls {
		if pull.HeadSHA == "" || pull.Number < here.number {
			continue
		}
		here.number = pull.Number
		head := queryHead{number: pull.Number, sha: pull.HeadSHA, overflow: true, why: "GitHub did not answer the query for the head's page"}
		stopped, cut := r.visitHead(ctx, writer, repository, head, here, c)
		if cut {
			return stopped, true
		}
	}
	return nil, false
}

// countHeads is the open pull requests of one page that have a head commit to read, which is
// what the pass's line reports as the heads it knows there are.
func countHeads(heads []queryHead) int {
	known := 0
	for _, head := range heads {
		if head.sha != "" {
			known++
		}
	}
	return known
}

// pagesOf is the objects one query answered for a head, as record takes them: one page, read in
// full, so every object of it is one the pass records. A query's answer has no pages of its own
// and no ETag -- it is one answer for twenty heads, which GitHub's conditional reads do not
// cover -- so the one page here stands for the whole of what GitHub said about that head.
func pagesOf(items []object) []pageOf[object] {
	if len(items) == 0 {
		return nil
	}
	return []pageOf[object]{{items: items, modified: true}}
}

// closeCycle ends the cycle this pass has just walked the last of and leaves the next pass at
// the start of the order. What no pass of the cycle reached is forgotten here and nowhere else:
// a pass forgetting by its own sight alone would drop the ETags of every head the passes before
// it read, and the cycle after it would read all of those heads over REST in full.
func (r *Reconciler) closeCycle(c *counts) {
	c.cycle = true
	forget(r.rows, r.seenRows)
	forget(r.pulls, r.seenRepositories)
	forget(r.listed, r.seenRepositories)
	forget(r.heads, r.seenHeads)
	r.seenRows, r.seenRepositories, r.seenHeads = nil, nil, nil
	r.resume = cursor{}
}

// over reports whether the pass's time is up: its own bound (PassTimeout) has passed, or the
// caller whose loop runs it cancelled it. The pass ends where it stands on either, and logs the
// one line every pass logs.
//
// It is also what tells a failed read that is the pass's own ending from one that is the
// repository's or the head's own fault: a request the ending killed is named at no WARN, since
// one for every repository and head the ending cut off would be a storm of them, and it costs
// what the pass kept of that head nothing.
func (r *Reconciler) over(ctx context.Context, c *counts) bool {
	if ctx.Err() == nil {
		return false
	}
	// Only the pass's own bound is the deadline the line reports: a cancelled caller is the
	// process going down, which is not a pass that ran out of its interval. The row says which
	// of the two it was, read off the caller's own context rather than from here, so every way
	// out of a cancelled pass records it and not only the ones that come through here (Pass,
	// passOutcome).
	c.deadline = c.deadline || errors.Is(ctx.Err(), context.DeadlineExceeded)
	return true
}

// visit is one repository a pass reads and where in it this visit begins: the page of its open
// pull requests the cursor named, and the first head of that page the cursor left unfinished.
// The zero page and the zero number are the whole of the repository, which is every repository
// of a pass but the one it takes up in the middle of.
type visit struct {
	row    string
	after  string
	page   int
	number int64
}

// cursor is where a pass that ends at this visit, before it has read anything of it, leaves the
// next pass to start.
func (v visit) cursor() cursor {
	return cursor{repository: v.row, after: v.after, page: v.page, number: v.number}
}

// order is the repositories a pass reads, in the order it reads them: mapped()'s own order from
// the cursor onwards and no further. A pass ends at the last repository rather than beginning
// the order again, so the order holds each repository once and the pass that reaches the end of
// it closes the cycle.
//
// A cursor naming something the order no longer holds strands no pass: a repository that has
// dropped out of the mapping leaves the pass starting at the first repository after it, and a
// cursor every mapped repository sorts before -- the repository it named was the last and has
// gone -- is a cursor into a cycle that cannot be finished, so the pass begins a fresh one at
// the first repository rather than reading nothing at all.
func order(rows []string, at cursor) []visit {
	if len(rows) == 0 {
		return nil
	}
	start := 0
	for start < len(rows) && rows[start] < at.repository {
		start++
	}
	if start == len(rows) {
		start = 0
	}
	visits := make([]visit, 0, len(rows)-start)
	for _, row := range rows[start:] {
		visits = append(visits, visit{row: row})
	}
	if rows[start] == at.repository {
		visits[0].after, visits[0].page, visits[0].number = at.after, at.page, at.number
	}
	return visits
}

// reconciled is one mapped repository as a pass resolved it: the token its reads are made with
// and the scopes they are budgeted under, and the owner, name and id GitHub itself gives it.
// reconcile is false for a repository the pass reads no head of -- one it holds no token for,
// one GitHub did not answer about, one another row naming the same repository was read under --
// which the pass passes over, resolve having said why once.
type reconciled struct {
	owner      string
	repo       string
	id         int64
	token      string
	scope      string
	headScopes []string
	reconcile  bool
}

// resolve reads one mapped row: the installation token it is read with, and the name and id
// GitHub gives it. It answers the stop a rate limit ended the pass at, and
// counts and names at WARN each repository it skips, every one of which is a repository nothing
// within this pass can mend.
//
// read is the repositories this pass has read the heads of already, by the id GitHub gives
// them, which is this pass's own: two rows naming one repository are read once within a pass,
// while the cycle's own sets, which say what may be forgotten when the cycle closes, hold
// everything every pass of the cycle reached.
func (r *Reconciler) resolve(ctx context.Context, row string, c *counts, read map[int64]struct{}) (reconciled, *stop) {
	owner, name, ok := splitRepo(row)
	if !ok {
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository mapping that is not owner/name", "repository", row)
		return reconciled{}, nil
	}
	see(&r.seenRows, row)
	token, err := r.GitHub.RepositoryToken(ctx, owner, name)
	if at := stopAt(err, githubapp.AppScope); at != nil {
		return reconciled{}, at
	}
	if err != nil {
		if r.over(ctx, c) {
			// The pass's own ending killed this read, which says nothing about the repository.
			return reconciled{}, nil
		}
		// The App is not installed on it, or its token is refused the repository: neither
		// mends itself within a pass, so the repository is named once and the pass goes on.
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository it holds no token for", "repository", row, "error", err)
		return reconciled{}, nil
	}
	// Every read made with this token is budgeted under its installation, so that scope is read
	// beside the App's before each of this repository's head reads, and is what a limit GitHub
	// answers one of those reads with is named by.
	scope := r.GitHub.TokenScope(token)
	state, at, err := r.identify(ctx, row, owner, name, token, scope, c)
	if at != nil {
		return reconciled{}, at
	}
	if err != nil {
		if r.over(ctx, c) {
			return reconciled{}, nil
		}
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub did not answer for", "repository", row, "error", err)
		delete(r.rows, row)
		return reconciled{}, nil
	}
	remember(&r.rows, row, state)
	if _, done := read[state.id]; done {
		// Two rows name one repository -- an old name and the one it was renamed to -- and
		// GitHub answers both with one id, so its heads are read once.
		return reconciled{}, nil
	}
	read[state.id] = struct{}{}
	see(&r.seenRepositories, state.id)
	// Every record is keyed by the name GitHub itself gives the repository, never by the row:
	// repo_projects.repo is stored lower-cased, while the webhook's key comes from GitHub's own
	// casing and cistore.Key does not lower-case, so a pass keyed from the row would write a
	// second record of the same head and settle it on a second subject.
	ownerOf, nameOf, ok := splitRepo(state.fullName)
	if !ok {
		c.mappingsSkipped++
		r.Logger.Warn("ci reconcile skipped a repository GitHub named oddly", "repository", row, "full_name", state.fullName)
		return reconciled{}, nil
	}
	return reconciled{
		owner:      ownerOf,
		repo:       nameOf,
		id:         state.id,
		token:      token,
		scope:      scope,
		headScopes: scopes(scope),
		reconcile:  true,
	}, nil
}

// mapped is every repository repo_projects maps to a project, each named as the repository is
// called today: the row's own `owner/name`, lower-cased with any `.git` stripped, or, where the
// row names a rename, what its renamed_to names. They are read in one order so two passes read
// GitHub in the same order, and each distinct name comes back once, since two rows naming one
// repository are one repository to read.
//
// The canonical name is read rather than the row's own because a pass must never ask GitHub
// about a name a repository was renamed away from: GitHub answers 404 for that name's
// installation, so RepositoryToken fails for it and the row is skipped with `ci reconcile
// skipped a repository it holds no token for`, a WARN every pass would log for ever about a
// name no one can read by design. The rule is the server's one renaming rule spliced in rather
// than written again here (refs.CanonicalRepoSQL), so this read and every other reader of the
// rule cannot come to different answers.
func (r *Reconciler) mapped(ctx context.Context) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `select distinct `+refs.CanonicalRepoSQL("mapping.repo")+` as repository
		from repo_projects mapping order by repository`)
	if err != nil {
		return nil, fmt.Errorf("read the repositories mapped to projects: %w", err)
	}
	defer rows.Close()
	var repositories []string
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, fmt.Errorf("read a mapped repository: %w", err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the repositories mapped to projects: %w", err)
	}
	return repositories, nil
}

// identify resolves one row against GitHub, conditionally: the name GitHub gives the repository
// and its id. A 304 says the answer the last pass kept still stands. scope is what this read is
// budgeted under, which is the token's installation: GitHub records a limit it answers this
// read with under that scope, so the pass names the same one.
func (r *Reconciler) identify(ctx context.Context, row, owner, name, token, scope string, c *counts) (rowState, *stop, error) {
	kept := r.rows[row]
	r.pace(ctx, c)
	c.requests++
	c.restRequests++
	answer, err := r.GitHub.RepositoryIdentity(ctx, token, owner, name, kept.etag)
	if at := stopAt(err, scope); at != nil {
		return rowState{}, at, nil
	}
	if err != nil {
		return rowState{}, nil, err
	}
	if answer.NotModified {
		c.restNotModified++
		if kept.id == 0 {
			// Nothing is remembered to stand for the 304, which only a kept ETag can draw, so
			// this cannot happen; reading the row in full next pass is the honest answer to it.
			return rowState{}, nil, errors.New("GitHub answered 304 for a repository this pass remembers nothing of")
		}
		kept.etag = cmp.Or(answer.ETag, kept.etag)
		return kept, nil, nil
	}
	if answer.FullName == "" || answer.ID == 0 {
		return rowState{}, nil, fmt.Errorf("GitHub named the repository %q with id %d", answer.FullName, answer.ID)
	}
	return rowState{etag: answer.ETag, fullName: answer.FullName, id: answer.ID}, nil, nil
}

// openPullRequests is every open pull request of one repository, read page by page and each page
// conditional on the ETag the last pass kept for it. scope is what these reads are budgeted
// under, as identify's is.
func (r *Reconciler) openPullRequests(ctx context.Context, id int64, owner, name, token, scope string, c *counts) ([]githubapp.PullRequestHead, *stop, error) {
	pages, err := readPages(ctx, r, c, &c.listPages, r.pulls[id], func(ctx context.Context, page int, etag string) (conditionalPage[githubapp.PullRequestHead], error) {
		answer, err := r.GitHub.OpenPullRequests(ctx, token, owner, name, page, etag)
		return pullRequestPage(answer), err
	})
	if at := stopAt(err, scope); at != nil {
		return nil, at, nil
	}
	if err != nil {
		// The listing is read again from its first page next pass.
		delete(r.pulls, id)
		return nil, nil, err
	}
	remember(&r.pulls, id, pages)
	var pulls []githubapp.PullRequestHead
	for _, page := range pages {
		pulls = append(pulls, page.items...)
	}
	// The pass reads a repository's heads in pull request number order, whatever order GitHub
	// listed them in: the cursor a pass leaves is a number in this order, and an order that
	// moved between passes would leave the next pass starting somewhere else.
	slices.SortFunc(pulls, func(a, b githubapp.PullRequestHead) int {
		return cmp.Compare(a.Number, b.Number)
	})
	return pulls, nil, nil
}

// limited reads the shared GitHub rate limits and reports the limit in force on the scopes it is
// given, nil when none is. A read that fails leaves the pass to GitHub's own answers, as
// githubapp.Client's own read before every request does: the limits are the database's, and
// GitHub still answers each process's requests with the limit.
func (r *Reconciler) limited(ctx context.Context, under ...string) *stop {
	if r.Limits == nil {
		return nil
	}
	limit, found, err := r.Limits.InForce(ctx, under...)
	if err != nil {
		r.Logger.Warn("ci reconcile did not read the shared GitHub rate limits", "scopes", under, "error", err)
		return nil
	}
	if !found {
		return nil
	}
	return &stop{scope: limit.Scope, until: limit.Until}
}

// stopAt is the stop a GitHub error ends the pass at, and nil for every other error.
// githubapp.Client.request records GitHub's own limit in the shared ledger itself, so the pass
// records nothing here; scope is what the request was budgeted under, which a limit GitHub
// answered with rather than one already held does not carry.
func stopAt(err error, scope string) *stop {
	limited, ok := githubapp.AsRateLimit(err)
	if !ok {
		return nil
	}
	at := &stop{scope: limited.Scope, until: time.Now().Add(limited.Wait)}
	if at.scope == "" {
		at.scope = scope
	}
	return at
}

// scopes is the shared limits a read with an installation token is held by: the App's, and the
// installation's when this process minted the token (githubapp.Client.TokenScope answers nothing
// for a token it did not mint).
func scopes(tokenScope string) []string {
	if tokenScope == "" {
		return []string{githubapp.AppScope}
	}
	return []string{githubapp.AppScope, tokenScope}
}

// splitRepo is the owner and the name of an `owner/name`, and false for anything that is not
// one: no slash, or a side of it empty.
func splitRepo(full string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(full, "/")
	if !found || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

// gap is the pause this pass leaves between its REST requests: the REST work it expects to do,
// spread over the share of its own bound it may spend waiting.
//
// The work is what the last pass learned there is -- the pages of each listing of each head
// the queries could not answer whole, and one read of each mapped repository's identity -- so a
// pass paces itself by the size of the job rather than by a fixed pause, which is what made a
// pass of 1,681 requests take 26 minutes of a 5-minute interval. Most of a pass is queries now,
// which GitHub's own secondary limit on GraphQL time paces instead (graphQLPace), so the REST
// work left is the handful of heads one query cannot hold and the gap is usually the ceiling.
// The share is nine tenths of the bound, the tenth left over being the round trips themselves.
//
// The gap is clamped at both ends. FastestRequestPace is the floor whatever the arithmetic says,
// because the secondary limit it sits inside is the App's and this pass is not the only reader
// the App pays for; Pace is the ceiling, so the pass is never slower than the pause a deployment
// configures and the work only ever buys GitHub more room, never less.
func (r *Reconciler) gap(rows int) time.Duration {
	if r.Pace <= 0 {
		// A caller that waits for nothing, which is what every test of this package is.
		return 0
	}
	if r.PassTimeout <= 0 {
		// An unbounded pass has no budget to spread its requests over, so it keeps the pause its
		// deployment configured.
		return r.Pace
	}
	work := r.restWork()
	if r.known() == 0 && work == 0 {
		// The first pass of a process knows of no work yet, and the floor is the safe answer to
		// that: it is the fastest this pass is ever allowed to read, which is inside GitHub's
		// secondary limit however much work there turns out to be, and the pass after it knows
		// the real count and paces itself by it.
		return FastestRequestPace
	}
	expected := work + rows
	return min(max(r.PassTimeout*9/10/time.Duration(expected), FastestRequestPace), r.Pace)
}

// known is the open heads the last cycle's queries listed, which is what the pass's line
// reports as the heads there are to read. A pull request GitHub listed with no head commit is
// no head to read, and counts for nothing here either (countHeads).
func (r *Reconciler) known() int {
	heads := 0
	for _, listed := range r.listed {
		heads += listed
	}
	return heads
}

// restWork is the REST requests the last pass learned this one will make: every head a query
// could not answer whole is read again over REST, one request for each page of each of its two
// listings, and a head whose pages the pass no longer holds is one request for each listing,
// which is the least a read of it can take.
func (r *Reconciler) restWork() int {
	requests := 0
	for _, state := range r.heads {
		requests += max(len(state.runs), 1) + max(len(state.suites), 1)
	}
	return requests
}

// pace leaves the pause between REST requests GitHub asks a poller for, which is the gap this
// pass set itself (counts.pace). It waits before each REST request but the pass's first, so a
// pass of one such request takes no longer than that request. A query is paced by GitHub's
// GraphQL budget instead and is no part of this count (graphQLPace).
func (r *Reconciler) pace(ctx context.Context, c *counts) {
	if c.pace <= 0 || c.restRequests == 0 {
		return
	}
	timer := time.NewTimer(c.pace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// remember writes one entry of a map the Reconciler keeps between passes, making the map on the
// first entry there is.
func remember[K comparable, V any](held *map[K]V, key K, value V) {
	if *held == nil {
		*held = make(map[K]V, 1)
	}
	(*held)[key] = value
}

// see adds one key to a set the Reconciler keeps across the passes of one cycle, making the set
// on the first key there is.
func see[K comparable](held *map[K]struct{}, key K) {
	if *held == nil {
		*held = make(map[K]struct{}, 1)
	}
	(*held)[key] = struct{}{}
}

// forget drops every entry of held the cycle that has just closed did not reach, so what the
// Reconciler keeps follows the open pull requests there are rather than every one the process
// has ever read.
func forget[K comparable, V any](held map[K]V, seen map[K]struct{}) {
	maps.DeleteFunc(held, func(key K, _ V) bool {
		_, ok := seen[key]
		return !ok
	})
}

// report writes the one INFO line a pass logs. Its field names are fixed: an alarm counts a
// pass's records and its rate limits by them.
//
// Beside the counts of what the pass did, the line says how the pass read GitHub, how it paced
// itself and how far round the cycle it reached: the heads it read of the heads it knows there
// are (heads, heads_known), the first and the last of them (first_head, last_head), every
// request it sent (requests) and how those split between the queries it asked and the points
// they cost (graphql_queries, graphql_points) and the REST reads it made (rest_requests), how
// many of those REST reads GitHub answered 304 (rest_not_modified) and how many heads took one
// at all (heads_over_rest), the gap it left between its REST requests (pace), whether its own
// bound ended it (deadline_reached) and whether it reached the end of the order and left the
// cursor at the start of it (cycle_complete). One line is then enough to see a pass that no
// longer fits its interval, a pass whose queries have stopped answering, and where the pass
// after it will take up. Each new field is written after the fields that were there before it,
// which keeps those in the order they have always been logged in.
func (r *Reconciler) report(c *counts, at *stop, took time.Duration) {
	attrs := []any{
		"list_pages", c.listPages,
		"heads", c.heads,
		"head_requests", c.headRequests,
		"heads_not_modified", c.notModified,
		"records", c.records,
		"mappings_skipped", c.mappingsSkipped,
		"heads_too_large", c.tooLarge,
		"rate_limited", at != nil,
	}
	if at != nil {
		attrs = append(attrs, "rate_limit_scope", at.scope, "rate_limit_until", at.until.UTC().Format(time.RFC3339))
	}
	attrs = append(attrs,
		"heads_known", r.known(),
		"requests", c.requests,
		"pace", c.pace.String(),
		"first_head", c.firstHead,
		"last_head", c.lastHead,
		"cycle_complete", c.cycle,
		"deadline_reached", c.deadline,
		"graphql_queries", c.queries,
		"graphql_points", c.points,
		"rest_requests", c.restRequests,
		"rest_not_modified", c.restNotModified,
		"heads_over_rest", c.restHeads,
		"duration", took.String())
	r.Logger.Info("ci reconcile pass", attrs...)
}
