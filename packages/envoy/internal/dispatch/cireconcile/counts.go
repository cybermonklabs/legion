package cireconcile

import (
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// counts is what one pass did, as its one log line reports it.
type counts struct {
	// queries counts the GraphQL queries the pass asked, points what GitHub charged them
	// against the App's GraphQL budget (the answers' own rateLimit.cost), and restRequests the
	// REST reads: a line's three numbers say how much of the pass GraphQL answered and how much
	// it could not, and the two of them together are every request the pass sent, which is what
	// the line reports as requests (report).
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
	// unchanged counts the heads a query answered the very objects the pass had already
	// recorded for, which are written nothing at all: the counterpart for a head read in a
	// query of notModified for a head read over REST (walk.visitHead).
	unchanged int
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
	// firstHead and lastHead are the first and the last head the pass read whole, which is the
	// slice of the one order this pass took. They are kept as the head's own parts and spelled
	// `owner/repo#number` for the line alone (report): the pass marks every head it reads, and
	// building that name for each of them would be a string a head thrown away at once.
	firstHead headName
	lastHead  headName
}

// headName is one head as the pass's line names it: the repository in the casing GitHub itself
// gives it, and the pull request's number.
type headName struct {
	owner  string
	repo   string
	number string
}

// String is the name the line carries, and "" for a head the pass never marked, which is what
// a pass that read no head at all reports.
func (n headName) String() string {
	if n.number == "" {
		return ""
	}
	return n.owner + "/" + n.repo + "#" + n.number
}

// mark records one head the pass read whole, which the line names when it is the first or the
// last of them: two passes' lines side by side then say how far round the order each got.
func (c *counts) mark(head pullHead) {
	name := headName{owner: head.owner, repo: head.repo, number: head.number}
	if c.firstHead.number == "" {
		c.firstHead = name
	}
	c.lastHead = name
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

// countPullHeads is countHeads for the REST listing's own shape: the open pull requests of it
// that have a head commit to read. The two listings are counted by the one rule, so the heads
// the pass says it knows there are mean the same thing whichever way the page was read.
func countPullHeads(pulls []githubapp.PullRequestHead) int {
	known := 0
	for _, pull := range pulls {
		if pull.HeadSHA != "" {
			known++
		}
	}
	return known
}

// known is the open heads this cycle's listings answered, which is what the pass's line reports
// as the heads there are to read: each page is counted by the pass that first lists it and by
// no other, so a page two passes share counts once (cursor.counted). A pull request GitHub
// listed with no head commit is no head to read, and counts for nothing here either
// (countHeads).
func (r *Reconciler) known() int {
	heads := 0
	for _, listed := range r.listed {
		heads += listed
	}
	return heads
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
// at all (heads_over_rest), how many heads a query answered the objects the pass had already
// recorded for and so wrote nothing of (heads_unchanged), the gap it left between its REST
// requests (pace), whether its own bound ended it (deadline_reached) and whether it reached the
// end of the order and left the cursor at the start of it (cycle_complete). One line is then
// enough to see a pass that no longer fits its interval, a pass whose queries have stopped
// answering, and where the pass after it will take up. A field added to the line is written
// after the fields already there, which leaves the order of those unchanged.
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
		"requests", c.queries+c.restRequests,
		"pace", c.pace.String(),
		"first_head", c.firstHead.String(),
		"last_head", c.lastHead.String(),
		"cycle_complete", c.cycle,
		"deadline_reached", c.deadline,
		"graphql_queries", c.queries,
		"graphql_points", c.points,
		"rest_requests", c.restRequests,
		"rest_not_modified", c.restNotModified,
		"heads_over_rest", c.restHeads,
		"heads_unchanged", c.unchanged,
		"duration", took.String())
	r.Logger.Info("ci reconcile pass", attrs...)
}
