package cireconcile

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

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
// is that page's position in the repository, counting from zero. rest says the page comes from
// the repository's REST listing rather than from a query: GitHub gave the pass that fell back
// to the listing no cursor for the pages after the one it refused, so the pass taking this
// cursor up reads the repository on from the listing rather than asking a query for a page it
// holds no cursor for. The cycle after it begins the repository at its first page and queries
// it again.
//
// from is the number that names the page: one more than the last head of the page before it,
// and zero for a repository's first page (walk.sweep). Every page of a repository carries it,
// the page a query answered as much as the page cut from the REST listing, and a page is cut
// from that listing at the first open pull request at or above from (walk.window). A page is
// named by a number and never by the position it sat at because the pass taking the cursor up
// reads the listing again, and a pull request closed or merged below moves every position
// after it down: a window cut at that position would begin further on than the page did, the
// heads between would be read by no pass of this cycle, and the pass that closed the cycle
// would record that it had covered them. A number names the same place in the listing whatever
// has closed below it, and the page a query was refused for is named like any other: the page
// before it was answered, and every head a query answers carries a number, so the refused page
// begins above the last of them.
//
// number is the first head of the page the pass did not finish, and zero is the whole page: a
// head below it was read by the pass that left the cursor. counted says the heads of that page
// have already been counted among the repository's known heads: the pass taking the cursor up
// reads the page again, and counting it again would have the line report more open heads than
// the repository has. It is the one page it names, whichever way that page was read.
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
	from       int64
	number     int64
	counted    bool
	rest       bool
}

// ending is a walk of the order that stopped before the end of what it was given: where the
// next pass is to take the order up, and the rate limit that ended this pass, nil for the
// pass's own bound. A walk that reached the end of what it was given answers no ending at all.
//
// It is how the cursor leaves a walk. Each step of a pass answers an ending or nil, and
// reconcile, the one place that writes the Reconciler's resume, reads the cursor off it: a step
// added later says what ended it and nothing about where the next pass starts, so no way out
// can forget the cursor.
type ending struct {
	resume cursor
	at     *stop
}

// walk is one pass's read of one repository: the collaborators every step of it needs, and the
// place in the order the walk stands at, which is the cursor the endings below carry.
type walk struct {
	r          *Reconciler
	writer     Writer
	repository reconciled
	c          *counts
	// here is where the walk stands: the page of this repository's open pull requests it is
	// reading, and the head of that page it is at. It is what the next pass takes up, so it
	// moves only as the walk finishes with a head or a page.
	here cursor
	// listing is this repository's open pull requests as one REST listing, read once for the
	// walk and only by a walk that fell back to it, which is a walk GitHub refused a query of
	// (walk.restPage). Its pages are windows of it, so the listing is read once however many
	// pages the walk takes from it.
	listing []githubapp.PullRequestHead
}

// cut is the ending the pass's own bound, or its caller's cancellation, makes; stopped is the
// ending a rate limit makes. Both leave the next pass where this walk stands.
func (w *walk) cut() *ending {
	return &ending{resume: w.here}
}

func (w *walk) stopped(at *stop) *ending {
	return &ending{resume: w.here, at: at}
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
	// short records its end all the same, because endPass writes on a context detached from
	// the pass's own (duty.LeaseCall).
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
	for _, from := range order(rows, r.resume) {
		end := r.visitRepository(ctx, writer, from, c, read)
		if end == nil {
			continue
		}
		// Wherever the pass stopped, this is the one place its cursor is set: the walk carries
		// where it stood, and the pass after this one begins there.
		r.resume = end.resume
		return end.at
	}
	// The pass reached the end of the order, so every head of it has been read by this pass or
	// by one before it in this cycle, and the cycle closes.
	r.closeCycle(c)
	// A pass does not wrap round, so the pass after one that reached the end of the order
	// begins the next cycle at the start of it.
	r.resume = cursor{}
	return nil
}

// visitRepository reads one repository of the order from the cursor the pass begins it at: the
// checks every repository is read under -- the pass's own bound, the shared rate limits, and
// what the mapped row resolves to -- and then the walk of its pages. It answers the ending that
// stopped the pass within this repository, and nil for a repository the pass read to the end
// of, which includes one it passed over.
func (r *Reconciler) visitRepository(ctx context.Context, writer Writer, from cursor, c *counts, read map[int64]struct{}) *ending {
	// The walk stands at this repository from here on, so every way out below leaves the next
	// pass where it has reached: until a page is read, that is this repository from the top.
	w := walk{r: r, writer: writer, c: c, here: from}
	// The pass's bound is checked between repositories as well as between pages and heads,
	// so a pass whose time is up ends where it stands rather than reading on through the
	// repositories after this one. What it leaves behind is the next pass's to read, from
	// this cursor.
	if r.cutShort(ctx, c) {
		return w.cut()
	}
	// Another process may have met a limit since this pass started, so the shared limits are
	// read again before each repository.
	if at := r.limited(ctx, githubapp.AppScope, githubapp.GraphQLScope); at != nil {
		return w.stopped(at)
	}
	repository, at := r.resolve(ctx, from.repository, c, read)
	if at != nil {
		return w.stopped(at)
	}
	if r.cutShort(ctx, c) {
		// The bound fell while this repository was being resolved, which is no fault of the
		// mapping's and no head of it read: the next pass reads it from the top.
		return w.cut()
	}
	if !repository.reconcile {
		// A repository the pass holds no token for, one GitHub did not answer about, or one
		// another row naming the same repository was read under. resolve has said so once.
		return nil
	}
	w.repository = repository
	return w.sweep(ctx)
}

// sweep reads one repository from the page the walk stands at to its last, and the heads of
// each of them. The ending it answers is the pass ending inside the repository -- on its own
// bound or at a rate limit -- and nil is the answer for a repository read to its last page.
//
// The pages are GitHub's own: a query answers twenty open pull requests and the cursor the page
// after them begins at, so the pass asks for one page, reads its heads, and asks for the next
// with that cursor until GitHub says there is no next page. A page GitHub will not answer a
// query for comes from the repository's REST listing instead, windowed into pages of that same
// twenty (walk.page), so the two ways of reading a repository share this one loop: one rule for
// what a page adds to the heads the repository is known to have, one check of the pass's bound
// between a page's heads, and one cursor advance.
func (w *walk) sweep(ctx context.Context) *ending {
	r, c := w.r, w.c
	if _, held := r.listed[w.repository.id]; !held || (w.here.page == 0 && !w.here.counted) {
		// The pass begins this repository at a page no pass of this cycle has counted, which
		// at its first page is the repository from the top: what the passes before it counted
		// belonged to the cycle before this one, and the count starts again.
		remember(&r.listed, w.repository.id, 0)
	}
	for {
		page, end, ok := w.page(ctx)
		if !ok {
			return end
		}
		if !w.here.counted {
			// The heads of a page are counted when a pass of this cycle first lists them and
			// never again: the pass taking a cursor up reads the page it was cut short in
			// again, and a second count of it would say the repository has heads it has not.
			r.listed[w.repository.id] += countHeads(page.heads)
			w.here.counted = true
		}
		for _, head := range page.heads {
			// The pass's bound is checked between the heads of a page as well as between the
			// pages, so a pass whose time is up stops writing here rather than writing every
			// head left on a context that is already dead -- writes that fail, under a cursor
			// moved past them, with the pass recorded as one that completed. The cursor is left
			// at the last head the pass began, which is the first the next pass is sure it must
			// read again.
			if r.cutShort(ctx, c) {
				return w.cut()
			}
			if head.sha == "" || head.number < w.here.number {
				// A pull request GitHub listed with no head commit is no head to read, and a
				// head below the cursor's number was read by the pass that left the cursor.
				continue
			}
			w.here.number = head.number
			if end := w.visitHead(ctx, head); end != nil {
				return end
			}
		}
		// The bound is checked once more after the page's heads, which is the check between
		// two pages: the check inside the loop above runs before a head, so the writes of the
		// page's last head are covered by nothing else. Without it the cursor would move past
		// a head whose writes the dead context failed -- a head the next pass skips and the
		// cycle after it reads -- and a pass cut on the last head of the order would record
		// that it completed.
		if r.cutShort(ctx, c) {
			return w.cut()
		}
		if !page.hasNextPage {
			return nil
		}
		// The page after this one begins above its last head: every head of an answered query
		// page carries a number (graphql.go), as every head of a window of the listing does,
		// so one more than the last of them names the page after it in any listing of this
		// repository, whatever has closed below. A page that answered no head at all names
		// nothing of its own, so the cursor keeps the number this page was named by: that
		// number is at or below the first head of the page after it, so a walk resuming there
		// reads heads a second time at worst and skips none.
		from := w.here.from
		if read := len(page.heads); read > 0 {
			from = page.heads[read-1].number + 1
		}
		w.here = cursor{repository: w.here.repository, after: page.endCursor, page: w.here.page + 1, from: from, rest: w.here.rest}
	}
}

// page is the page of open pull requests the walk stands at. ok is false for a walk that reads
// this repository no further, and the ending beside it is then the pass's own -- a rate limit,
// or its bound -- or nil for a repository GitHub would answer neither a query nor a listing
// for, which the pass passes over having said so once.
//
// It comes from the query while GitHub answers the queries, and from the repository's REST
// listing from the page GitHub refuses a query for onwards (restPage). The pass reads from
// there to the end of the repository rather than that page alone, since the pages after a page
// it did not get are pages it cannot ask for.
func (w *walk) page(ctx context.Context) (queryPage, *ending, bool) {
	r, c := w.r, w.c
	if w.here.rest {
		// A pass before this one fell back to the listing within this repository and was cut
		// short there, and the cursor it left names its page by the number that page begins
		// at (cursor.from).
		return w.restPage(ctx)
	}
	if at := r.limited(ctx, w.repository.queryScopes...); at != nil {
		return queryPage{}, w.stopped(at), false
	}
	page, err := r.query(ctx, w.repository, w.here.after, c)
	if at := stopAt(err, githubapp.GraphQLScope); at != nil {
		// A rate limit is the pass ending, never a page to read another way: reading this
		// page's heads over REST would be exactly the requests GitHub has just asked this
		// App to stop sending.
		return queryPage{}, w.stopped(at), false
	}
	if err != nil {
		if r.cutShort(ctx, c) {
			// The pass's own ending killed the query, which says nothing about the page.
			return queryPage{}, w.cut(), false
		}
		// GitHub refused the query, gave up on it, or answered past what githubapp reads.
		// The cursor of the page after this one comes only in the answer the pass did not
		// get, so there is no next page to ask for: this page and the pages after it are read
		// from the listing instead, and the cursor says so from here on.
		r.Logger.Warn("ci reconcile read a repository over REST: GitHub did not answer the query",
			"repository", w.repository.owner+"/"+w.repository.repo, "page", w.here.page, "error", err)
		w.here.rest = true
		return w.restPage(ctx)
	}
	return page, nil, true
}

// restPage is the page the walk stands at taken from the repository's open pull requests as one
// REST listing: the window of twenty open pull requests that page begins at, each head of it
// marked to be read over REST, and whether a page follows it. The listing is read once for the
// walk, page by page and each page conditional on its own ETag, and every head it names is read
// as every other head a query could not answer is (visitHeadOverREST).
//
// The window begins at the first open pull request of the listing at or above the number the
// cursor names, which is where the page the walk stands at begins (walk.window). The cursor
// carries that number for every page: a pass cut short in this listing left it, and the page
// a query answered before a refusal named the page after it, which is the page the refusal
// fell on (walk.sweep, cursor.from). Nothing here reads the listing by position, so whatever
// stands between the read that wrote the number and this one -- a pass's bound, a rate limit,
// or only the time the pages before it took -- costs the walk no head: a pull request closed
// or merged below the number moves no page of this listing.
func (w *walk) restPage(ctx context.Context) (queryPage, *ending, bool) {
	r, c, repository := w.r, w.c, w.repository
	if w.listing == nil {
		pulls, at, err := r.openPullRequests(ctx, repository.id, repository.owner, repository.repo, repository.token, repository.scope, c)
		if at != nil {
			return queryPage{}, w.stopped(at), false
		}
		if err != nil {
			if r.cutShort(ctx, c) {
				return queryPage{}, w.cut(), false
			}
			c.mappingsSkipped++
			r.Logger.Warn("ci reconcile read no open pull requests of a repository",
				"repository", repository.owner+"/"+repository.repo, "error", err)
			return queryPage{}, nil, false
		}
		w.listing = pulls
	}
	from := w.window()
	to := min(from+pullsPerQuery, len(w.listing))
	page := queryPage{heads: make([]queryHead, 0, to-from), hasNextPage: to < len(w.listing)}
	for _, pull := range w.listing[from:to] {
		page.heads = append(page.heads, queryHead{number: pull.Number, sha: pull.HeadSHA, overflow: refusedPage})
	}
	return page, nil, true
}

// window is where in the listing the page the walk stands at begins: the first open pull
// request of the listing at or above the number the cursor names.
//
// The listing is sorted by number ascending, and a number names the same place in it however
// many pull requests below have closed or merged since that number was written. The number is
// one above the last head the walk read, so the window begins at the first head still open
// above that one -- no pull request of the listing need carry the number itself -- and that
// head is the first this cycle has not read.
//
// Zero is the start of the listing, which is a repository read over REST from its first page:
// every pull request number is above zero, so no page of the listing is skipped for it.
func (w *walk) window() int {
	at, _ := slices.BinarySearchFunc(w.listing, w.here.from, func(pull githubapp.PullRequestHead, number int64) int {
		return cmp.Compare(pull.Number, number)
	})
	return at
}

// refusedPage is why a head of a page GitHub would not answer the query for is read over REST,
// which the head's own line carries beside the reasons one query could not answer a head whole
// (queryHead.overflow).
const refusedPage = "GitHub did not answer the query for the head's page"

// visitHead reads and records one head of a page: from the query's own answer where it could
// answer the head whole, and over REST where it could not. The ending it answers is the pass
// ending at this head, in which case the next pass reads the head again from the start.
//
// A head the query answered the objects the pass already recorded for is written nothing: the
// fingerprint of a query's answer is what the ETag of a listing is to a REST read, and a pass
// that wrote every head again would be 30,000 reads, unmarshals and hashes of records nothing
// had changed. The rules it is kept and dropped by are the ETags' (headState.fingerprint).
func (w *walk) visitHead(ctx context.Context, head queryHead) *ending {
	r, c := w.r, w.c
	pull := w.repository.head(head.number, head.sha)
	if head.overflow != "" {
		return w.visitHeadOverREST(ctx, pull, head.overflow)
	}
	// The query answered this head whole, so there is nothing to ask GitHub and nothing
	// the pass's bound can cut short: the writes alone are left.
	c.heads++
	// This cycle has reached the head, which is what keeps its fingerprint when the cycle
	// closes. A head read in a query is marked here and one read over REST in visitHeadOverREST,
	// so every head of the cycle is in the set whichever way it was read (closeCycle).
	see(&r.seenHeads, pull.key())
	// kept is what the pass holds of the head, and nothing at all when the record those writes
	// went to has gone: the fingerprint then stands for observations no record holds any more,
	// and the head is written again.
	kept := r.kept(w.writer, pull)
	if kept.fingerprint == head.fingerprint {
		c.unchanged++
		c.mark(pull)
		return nil
	}
	// The fingerprint is kept only where every write landed, exactly as the ETags of a head
	// read over REST are (recordHead): a write that did not land must not be hidden behind it,
	// and the next pass writes this head again.
	r.recordHead(ctx, w.writer, pull, pagesOf(head.runs), pagesOf(head.suites), kept, headState{fingerprint: head.fingerprint}, c)
	c.mark(pull)
	return nil
}

// visitHeadOverREST reads and records one head over REST: paged, and conditional on the ETags
// the pass kept of it. Both the heads one query could not answer whole (visitHead) and every
// head of a page GitHub would not answer the query for at all (restPage) are read here, and why
// names which of the two the head's own line reports. The ending it answers is the pass ending
// at this head, as visitHead's is.
func (w *walk) visitHeadOverREST(ctx context.Context, pull pullHead, why string) *ending {
	r, c := w.r, w.c
	// These reads are budgeted under the installation rather than under GraphQL, so the shared
	// limits are read again for them.
	if at := r.limited(ctx, w.repository.headScopes...); at != nil {
		return w.stopped(at)
	}
	r.Logger.Info("ci reconcile reads a head over REST",
		"repository", pull.repository(), "pr", pull.number, "sha", pull.sha, "why", why)
	see(&r.seenHeads, pull.key())
	if at := r.readHead(ctx, w.writer, pull, c); at != nil {
		return w.stopped(at)
	}
	if r.cutShort(ctx, c) {
		// The bound fell while this head was being read, so it was not read whole and the next
		// pass takes the cycle up at it. A head that was read whole just as the bound fell is
		// read again by that pass, which costs it the head's two conditional requests and
		// nothing more: the honest place to take a cycle up is the last head the pass finishing
		// it is sure of.
		return w.cut()
	}
	c.restHeads++
	c.mark(pull)
	return nil
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

// closeCycle ends the cycle this pass has just walked the last of. What no pass of the cycle
// reached is forgotten here and nowhere else: a pass forgetting by its own sight alone would
// drop the ETags of every head the passes before it read, and the cycle after it would read all
// of those heads over REST in full.
func (r *Reconciler) closeCycle(c *counts) {
	c.cycle = true
	forget(r.rows, r.seenRows)
	forget(r.pulls, r.seenRepositories)
	forget(r.listed, r.seenRepositories)
	forget(r.heads, r.seenHeads)
	r.seenRows, r.seenRepositories, r.seenHeads = nil, nil, nil
}

// cutShort reports whether the pass's time is up: its own bound (PassTimeout) has passed, or the
// caller whose loop runs it cancelled it. The pass ends where it stands on either, and logs the
// one line every pass logs.
//
// It is also what tells a failed read that is the pass's own ending from one that is the
// repository's or the head's own fault: a request the ending killed is named at no WARN, since
// one for every repository and head the ending cut off would be a storm of them, and it costs
// what the pass kept of that head nothing.
func (r *Reconciler) cutShort(ctx context.Context, c *counts) bool {
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

// order is the repositories a pass reads, in the order it reads them and each as the cursor the
// pass begins it at: mapped()'s own order from the cursor onwards and no further. A pass ends at
// the last repository rather than beginning the order again, so the order holds each repository
// once and the pass that reaches the end of it closes the cycle.
//
// Every repository of a pass but the one it takes up in the middle of begins at the whole of
// that repository, which is the zero page and the zero number of its cursor.
//
// A cursor naming something the order no longer holds strands no pass: a repository that has
// dropped out of the mapping leaves the pass starting at the first repository after it, and a
// cursor every mapped repository sorts before -- the repository it named was the last and has
// gone -- is a cursor into a cycle that cannot be finished, so the pass begins a fresh one at
// the first repository rather than reading nothing at all.
func order(rows []string, at cursor) []cursor {
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
	visits := make([]cursor, 0, len(rows)-start)
	for _, row := range rows[start:] {
		visits = append(visits, cursor{repository: row})
	}
	if rows[start] == at.repository {
		visits[0] = at
	}
	return visits
}
