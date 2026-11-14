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
// (headsQuery). Reading the same heads over REST takes two requests of each head and one of
// each page of each repository's open pull requests, which for 833 heads of 16 repositories
// measures 1,720 requests and 26 minutes 25 seconds -- five passes' worth of a 5-minute
// interval, which no pass finishes within its bound and which leaves the loop looking dead for
// the whole of it. The same read in queries is about 53 of them, about 318 points of the App's
// GraphQL budget and about 178 seconds of GitHub's own processing time.
//
// What a query cannot answer whole is read over REST: paged, conditional on the ETags the pass
// kept, and recorded through the same writes. That is a head with more check suites than one
// query asks for, a head one of whose suites has more check runs than one query asks for, a
// head whose Checks fields GitHub refused or answered null, and every head of a page GitHub
// refused, gave up on, or answered past githubapp's 1 MiB response limit (walk.sweep,
// walk.restPage).
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
// A query's answer carries no ETag of its own -- it is one answer for twenty heads, which
// GitHub's conditional reads do not cover -- so what an ETag does for a REST read a fingerprint
// does for a query: the pass keeps a hash of the objects it mapped for each head a query
// answered whole (queryHead.fingerprint), and a head whose next answer fingerprints the same is
// written nothing at all. Without it every completed run and every completed suite of every
// head is recorded again at every pass, each one a read of that head's record from NATS, an
// unmarshal of it and a hash: some 30,000 of them for 833 heads of about 39 checks each, where
// a head read over REST is written nothing at all when its listings answer 304, which 752 of
// those 833 do.
//
// GitHub budgets the App, not a process, so the pass reads as GitHub asks a poller to: serially,
// one request after another and never two at once. A query is paced by GitHub's secondary limit
// on GraphQL processing time (graphQLPace); a REST request is paced by the gap the pass sets
// itself from the work it expects (Reconciler.gap); every REST read is conditional on the ETag
// the pass kept from its last read of that same page, which costs the budget nothing when GitHub
// has not changed the page; and the shared rate limits every Dispatch process obeys
// (githubapp/ratelimit) are read before the pass starts, before each repository, before each
// query and before each head, with a limit in force ending the pass where it stands. GitHub
// budgets GraphQL apart from REST and answers a GraphQL limit in its own way, so a primary
// limit a query meets -- the App's GraphQL points spent -- is recorded under
// githubapp.GraphQLScope and leaves every REST read of every process going, while a secondary
// limit, which GitHub shares between an installation's REST calls and its queries, is recorded
// under that installation's own scope and holds both (githubapp.GraphQLRateLimit).
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
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// Interval is how often the running pass reads GitHub.
	Interval = 5 * time.Minute

	// Lease names the lease one pass runs under (duty.CIReconcileLease, on duty's lease list). It
	// is not what makes the pass safe to run twice -- every write it makes is a compare-and-swap
	// that applies an observation GitHub already holds, so two passes at once write one record --
	// it is the GitHub budget: each pass reads every open pull request of every mapped repository,
	// and N processes passing is N times those reads for one result.
	Lease = duty.CIReconcileLease
)

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
	// (walk.restPage), so most repositories hold no entry here.
	pulls map[int64][]pageOf[githubapp.PullRequestHead]
	// listed is how many open heads each repository's listings answered with, keyed by the id
	// GitHub gave the repository: what the pass's line reports as the heads it knows there are,
	// and what a pass that reads no head of a repository still knows of it. A page counts here
	// whether a query or the REST listing answered it, by the one rule (countHeads). It is
	// built up as a repository's pages are read, over however many passes the cursor takes
	// them, and reset where a pass begins that repository at its first page.
	listed map[int64]int
	// heads is what the last pass kept of each head it read: the ETags of a head read over REST
	// (pullHead.key), and for a head a query answered whole, which costs no REST request and so
	// holds no ETag, the fingerprint of the objects that answer carried (headState).
	heads map[string]headState
	// resume is where the next pass starts: the first page or REST head the last pass did not
	// finish, and the zero cursor -- the start of the order -- after a pass that reached the end
	// of it. reconcile is the one place that writes it, from the ending the walk of the order
	// answered with.
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

// stop is the rate limit that ended a pass: the scope it was found or answered under, and when
// GitHub will take a request under it again.
type stop struct {
	scope string
	until time.Time
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

// withGraphQL is those scopes with GitHub's GraphQL budget beside them, which is what a query
// is held by: GitHub budgets GraphQL apart from REST, so a query is stopped by a limit on
// either.
func withGraphQL(headScopes []string) []string {
	held := make([]string, 0, len(headScopes)+1)
	held = append(held, headScopes...)
	return append(held, githubapp.GraphQLScope)
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
// on the first key there is: remember for a set, whose value is the empty struct.
func see[K comparable](held *map[K]struct{}, key K) {
	remember(held, key, struct{}{})
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
