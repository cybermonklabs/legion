package cireconcile

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// maxPages bounds one listing's pages, so a pass can never read on for ever: an answer GitHub
// keeps reporting full past this many pages is read as a failed listing, which the next pass
// reads again from its first page. A hundred pages is ten thousand objects of one listing,
// far past any head or any repository's open pull requests.
const maxPages = 100

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

// object is one GitHub check run or check suite as its listing answers it, the shape
// contracts.GithubCIObservation maps.
type object = map[string]any

// pullHead is one open pull request's head, as the pass reads it: the installation token and the
// shared scope its reads are budgeted under, the owner and name GitHub itself gives the
// repository, and the pull request's number and head commit.
type pullHead struct {
	token  string
	scope  string
	owner  string
	repo   string
	number string
	sha    string
}

// key is what the pass remembers this head's ETags under. It is the head's own identity, the same
// four parts cistore keys its record by.
func (h pullHead) key() string {
	return h.owner + "/" + h.repo + "#" + h.number + "@" + h.sha
}

// repository is the `owner/name` GitHub itself spells this head's repository, which every line
// the pass writes about the head names it by.
func (h pullHead) repository() string {
	return h.owner + "/" + h.repo
}

// headState is what a pass kept of one head: for a head read over REST, the pages of its two
// listings, each with the ETag the next pass reads it conditionally on and no items of its own
// (withoutItems); for a head a query answered whole, the fingerprint of what that query
// answered.
type headState struct {
	runs   []pageOf[object]
	suites []pageOf[object]
	// fingerprint is the hash of the mapped check runs and check suites the last query answered
	// for this head (queryHead.fingerprint), and zero for a head no query has answered whole --
	// a head read over REST, whose ETags do this work instead. A query answers no ETag, so
	// without it the pass writes every observation of every head it read at every pass; with
	// it, a head GitHub answers the same objects for costs the one existence read kept makes
	// and no write.
	//
	// It follows the ETags' own rules: it is kept only when every observation of the head was
	// recorded, it is dropped when a write of the head failed, when the head's record has gone
	// and when a cycle no longer lists the head (walk.visitHead, kept, closeCycle), and a head
	// a deadline or a rate limit cut short keeps the one it had, since a cut is not a failed
	// read.
	fingerprint uint64
	// observed says the last full answer for this head held at least one observation the rules say
	// to record. Such a head is worth reading again in full when its record has gone (the bucket's
	// TTL, or a delete), so the next pass asks HasRecord before it trusts these ETags. A head whose
	// answer held nothing to record has no record to lose, and keeps its ETags whatever the bucket
	// holds.
	observed bool
}

// pageOf is one page of a conditional listing as a pass keeps it: the ETag GitHub answered with,
// the items that answer held, and whether the page was full, which the next read needs to know
// whether a page follows it. A 304 carries none of the three, so a page answered 304 keeps what
// the pass that read it in full left.
type pageOf[T any] struct {
	etag  string
	items []T
	full  bool
	// modified says GitHub answered this page 200 in the read that built it, so its items are the
	// ones that read must process. A 304's items were processed by the pass that read them.
	modified bool
}

// conditionalPage is one page as GitHub answered it, whatever the listing lists: githubapp's
// ObjectPage and PullRequestPage reduced to the four things readPages needs of either.
type conditionalPage[T any] struct {
	items       []T
	etag        string
	notModified bool
	full        bool
}

// errTooManyPages ends a listing GitHub keeps reporting full (maxPages).
var errTooManyPages = fmt.Errorf("the listing did not end within %d pages", maxPages)

// readPages reads one conditional listing whole: pages 1..N, N being the pages the last pass
// kept, each conditional on that page's ETag, and then, while the last page read came back full,
// the page after it, which no pass has an ETag for. It stops at the first page that is not full.
//
// It counts each request among the REST reads of the pass, which the pace is left between, and
// against tally, the count the pass's log line reports this listing under; a page GitHub
// answers 304 is counted again among the REST reads that cost the budget nothing.
func readPages[T any](ctx context.Context, r *Reconciler, c *counts, tally *int, kept []pageOf[T], read func(ctx context.Context, page int, etag string) (conditionalPage[T], error)) ([]pageOf[T], error) {
	var pages []pageOf[T]
	for number := 1; number <= maxPages; number++ {
		var before pageOf[T]
		if number <= len(kept) {
			before = kept[number-1]
		}
		r.pace(ctx, c)
		c.restRequests++
		*tally++
		answer, err := read(ctx, number, before.etag)
		if err != nil {
			return nil, err
		}
		page := pageOf[T]{etag: answer.etag, items: answer.items, full: answer.full, modified: true}
		if answer.notModified {
			c.restNotModified++
			// A 304 should repeat the ETag it matched, and a kept ETag is better than no ETag if
			// it does not.
			page = pageOf[T]{etag: cmp.Or(answer.etag, before.etag), items: before.items, full: before.full}
		}
		pages = append(pages, page)
		if !page.full {
			return pages, nil
		}
	}
	return nil, errTooManyPages
}

// readHead reads one head's check runs and check suites and records what GitHub holds that the
// listener's record of that head lacks. It answers the stop a rate limit ended the pass at, and
// nil for every other outcome: a head the pass could not read is named at WARN and the pass goes
// on to the next.
func (r *Reconciler) readHead(ctx context.Context, writer Writer, head pullHead, c *counts) *stop {
	kept := r.kept(writer, head)
	runs, err := readPages(ctx, r, c, &c.headRequests, kept.runs, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckRuns(ctx, head.token, head.owner, head.repo, head.sha, page, etag)
		return objectPage(answer), err
	})
	if err != nil {
		return r.headFailed(ctx, err, head, c)
	}
	suites, err := readPages(ctx, r, c, &c.headRequests, kept.suites, func(ctx context.Context, page int, etag string) (conditionalPage[object], error) {
		answer, err := r.GitHub.CommitCheckSuites(ctx, head.token, head.owner, head.repo, head.sha, page, etag)
		return objectPage(answer), err
	})
	if err != nil {
		return r.headFailed(ctx, err, head, c)
	}
	// Both listings are in, so this head was read: a head the pass's own ending cut off before
	// that is no head the pass read, and is the head the next pass takes the cycle up at.
	c.heads++
	// A head read over REST holds no fingerprint: these ETags are what the next pass reads it
	// conditionally on, and a hash of a query's answer is no use to a head one query could not
	// hold whole in the first place. A head a query answers again later is fingerprinted again
	// with it (walk.visitHead).
	if !r.recordHead(ctx, writer, head, runs, suites, kept, headState{
		runs:   withoutItems(runs),
		suites: withoutItems(suites),
	}, c) {
		return nil
	}
	if everyPageNotModified(runs) && everyPageNotModified(suites) {
		c.notModified++
	}
	return nil
}

// kept is what the pass remembers of one head -- the ETags of a head read over REST, the
// fingerprint of one a query answered whole -- and an empty state when neither may be used:
// the record they stand for is gone (the bucket's seven-day TTL, or a delete), so a conditional
// read would answer 304, and a fingerprint that matched would write nothing, either of which
// leaves the head with no record at all.
func (r *Reconciler) kept(writer Writer, head pullHead) headState {
	state, ok := r.heads[head.key()]
	if !ok {
		return headState{}
	}
	if !state.observed {
		// Nothing was recorded from this head's last answer, so there is no record to have lost.
		return state
	}
	has, err := writer.HasRecord(head.owner, head.repo, head.number, head.sha)
	if err != nil {
		r.Logger.Warn("ci reconcile did not read whether a head still has its CI record",
			"repository", head.repository(), "pr", head.number, "sha", head.sha, "error", err)
		return headState{}
	}
	if !has {
		return headState{}
	}
	return state
}

// headFailed reports one head's failed read, and tells a read that was cut short from one that
// failed.
//
// A cut is the pass ending rather than the head: its own bound or its caller's cancellation,
// and a rate limit GitHub answered or another process recorded. The head keeps everything the
// Reconciler holds of it, including the ETags that make the next read of it cost the App's
// budget nothing, it is named at no WARN, and it is not counted among the heads the pass read:
// the cursor names it, and the next pass reads it from the start.
//
// Every other failure is the head's own. It drops what the pass kept of that head, so the next
// pass reads it in full, and leaves the pass to the head after it.
func (r *Reconciler) headFailed(ctx context.Context, err error, head pullHead, c *counts) *stop {
	if r.cutShort(ctx, c) {
		return nil
	}
	if at := stopAt(err, head.scope); at != nil {
		return at
	}
	c.heads++
	delete(r.heads, head.key())
	var tooLarge *githubapp.ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		// GitHub's answer is past what githubapp reads at all, so this head cannot be read: it is
		// counted and named, and the pass goes on rather than failing every head behind it.
		c.tooLarge++
		r.Logger.Warn("ci reconcile skipped a head whose answer from GitHub was too large",
			"repository", head.repository(), "pr", head.number, "sha", head.sha, "error", err)
		return nil
	}
	r.Logger.Warn("ci reconcile did not read a head",
		"repository", head.repository(), "pr", head.number, "sha", head.sha, "error", err)
	return nil
}

// recordHead writes one head's objects and then keeps or drops what the pass holds of that
// head, which is the one tail both reads of a head end in: the head read over REST (readHead)
// and the head a query answered whole (walk.visitHead).
//
// hold is what the read itself has to keep -- the ETags of the two listings, or the fingerprint
// of the query's answer -- and kept is what the pass held of the head before this read, which
// is where the observed flag carries on from. It answers whether every write landed: one that
// did not must be hidden behind neither an ETag nor a fingerprint, so the pass then holds
// nothing of the head and the next reads it in full and writes it again.
func (r *Reconciler) recordHead(ctx context.Context, writer Writer, head pullHead, runs, suites []pageOf[object], kept, hold headState, c *counts) bool {
	wanted, changedRecord, ok := r.record(ctx, writer, head, runs, suites)
	if changedRecord {
		// A head's runs and suites all write the one record cistore.Key names, so a head whose
		// writes changed that record counts once here however many of them changed it. This is
		// before the ok check: a record a write changed was changed whether or not a later write
		// of the same head failed.
		c.records++
	}
	if !ok {
		delete(r.heads, head.key())
		return false
	}
	// Once a head's answer has held something to record, it holds it in every later answer too,
	// so the record it was written to is worth asking after at every pass.
	hold.observed = kept.observed || wanted > 0
	remember(&r.heads, head.key(), hold)
	return true
}

// listing is one of the two listings a head's record is written from, as recordListing walks
// either of them: the event name contracts.GithubCIObservation maps its objects under, the
// store call an observation of it is written with, how a WARN about one of them names it, and
// whether its observations go through the review App's rule, which reads a check's name and so
// is the runs' alone.
type listing struct {
	event string
	write func(context.Context, Writer, contracts.CIObservation) (bool, error)
	// what and label are how a failed write names this listing and the object it could not
	// write: `did not record a check run` with the run's own name under `check`, `did not
	// record a check suite` with the suite's id under `suite`.
	what    string
	label   string
	nameOf  func(contracts.CIObservation) string
	verdict bool
}

// checkRuns and checkSuites are those two listings. Each is built once rather than at every
// head, and each writes through a plain function of the writer rather than a method value of
// it, which would be an allocation for every head the pass records.
var (
	checkRuns = listing{
		event:   "check_run",
		write:   recordRun,
		what:    "check run",
		label:   "check",
		nameOf:  func(observation contracts.CIObservation) string { return observation.CheckName },
		verdict: true,
	}
	checkSuites = listing{
		event:  "check_suite",
		write:  recordSuite,
		what:   "check suite",
		label:  "suite",
		nameOf: func(observation contracts.CIObservation) string { return observation.SuiteID },
	}
)

func recordRun(ctx context.Context, writer Writer, observation contracts.CIObservation) (bool, error) {
	return writer.Record(ctx, observation)
}

func recordSuite(ctx context.Context, writer Writer, observation contracts.CIObservation) (bool, error) {
	return writer.RecordSuite(ctx, observation)
}

// record writes every observation the rules say to record from the pages GitHub answered 200
// with, under the pull request's number and the owner and name GitHub itself gave. wanted counts
// the observations those rules named, whether or not the record already held them.
// changedRecord answers whether any of those writes changed this head's record, which is the one
// record of the head (cistore.Key) and so is what the pass counts once; a pass over what the
// listener already holds changes none. ok is false when a write failed and a later pass might
// make it land, which is every failure but a refusal (refused); it is what makes the pass drop
// this head's ETags.
func (r *Reconciler) record(ctx context.Context, writer Writer, head pullHead, runs, suites []pageOf[object]) (wanted int, changedRecord, ok bool) {
	wantedRuns, changedRuns, runsOK := r.recordListing(ctx, writer, head, runs, checkRuns)
	wantedSuites, changedSuites, suitesOK := r.recordListing(ctx, writer, head, suites, checkSuites)
	return wantedRuns + wantedSuites, changedRuns || changedSuites, runsOK && suitesOK
}

// recordListing writes one of those two listings, page by page and object by object, and
// answers for that listing what record answers for the head.
func (r *Reconciler) recordListing(ctx context.Context, writer Writer, head pullHead, pages []pageOf[object], of listing) (wanted int, changedRecord, ok bool) {
	ok = true
	for _, page := range pages {
		if !page.modified {
			continue
		}
		for _, raw := range page.items {
			observation, mapped := contracts.GithubCIObservation(of.event, raw, head.owner, head.repo, head.number)
			if !mapped {
				continue
			}
			// Only a completed object is recorded. cistore refuses to settle a head while any
			// check its record holds is queued or running, and nothing ever takes a check out of
			// a record, so one run left queued at that commit -- an attempt GitHub abandoned, a
			// job nobody ran -- would stop ci-ok ever reaching the daemon for that head, which is
			// the failure this pass exists to mend, and a suite that is not completed holds the
			// head exactly as a queued run does (cistore's settlementReady). The listener records
			// a running run because the event completing it follows; a pass reading a listing has
			// no such promise.
			if observation.Status != "completed" {
				continue
			}
			if of.verdict && !r.recordVerdict(observation, head) {
				continue
			}
			wanted++
			changed, err := of.write(ctx, writer, observation)
			if err != nil {
				if !refused(err) {
					ok = false
				}
				r.Logger.Warn("ci reconcile did not record a "+of.what,
					of.label, of.nameOf(observation), "repository", head.repository(),
					"pr", head.number, "sha", head.sha, "error", err)
				continue
			}
			if changed {
				changedRecord = true
			}
		}
	}
	return wanted, changedRecord, ok
}

// recordVerdict reports whether a check run is one to record under the review App's rule, which
// every run but a verdict check is: a verdict check (`tester`, `architect`) is recorded only
// when the review App created it, and none at all while DISPATCH_REVIEWER_APP_ID is unset
// (contracts.ReviewerVerdictDecision). A run the rule drops is named at WARN, since a verdict a
// person can see on the pull request reaching no record is a thing to be able to read.
func (r *Reconciler) recordVerdict(observation contracts.CIObservation, head pullHead) bool {
	switch contracts.ReviewerVerdictDecision(observation.CheckName, observation.AppID, r.ReviewerAppID) {
	case contracts.VerdictNoReviewerApp:
		r.Logger.Warn("ci reconcile dropped a verdict check: DISPATCH_REVIEWER_APP_ID is not set",
			"check", observation.CheckName, "app_id", observation.AppID,
			"repository", head.repository(), "pr", head.number)
		return false
	case contracts.VerdictUntrusted:
		r.Logger.Warn("ci reconcile ignored a verdict check another App created",
			"check", observation.CheckName, "app_id", observation.AppID,
			"expected_app_id", r.ReviewerAppID, "repository", head.repository(), "pr", head.number)
		return false
	}
	return true
}

// refused reports whether a failed write is one every later pass would meet again: cistore
// refuses a write that would take the record or its settlement past its bound with
// bus.ErrTooLarge and writes the record marked Overflowed, so the head never settles on the
// checks it could not hold and the listener's own writes leave it exactly there. Such a write
// is named at WARN once, as any other failed write is, but it does not cost the head its
// ETags: reading the head in full at every pass would buy nothing.
func refused(err error) bool {
	return errors.Is(err, bus.ErrTooLarge)
}

// objectPage is githubapp's answer as readPages takes it.
func objectPage(answer githubapp.ObjectPage) conditionalPage[object] {
	return conditionalPage[object]{
		items:       answer.Objects,
		etag:        answer.ETag,
		notModified: answer.NotModified,
		full:        answer.Full,
	}
}

// pullRequestPage is githubapp's answer to the open pull request listing as readPages takes it.
func pullRequestPage(answer githubapp.PullRequestPage) conditionalPage[githubapp.PullRequestHead] {
	return conditionalPage[githubapp.PullRequestHead]{
		items:       answer.PullRequests,
		etag:        answer.ETag,
		notModified: answer.NotModified,
		full:        answer.Full,
	}
}

// everyPageNotModified reports whether every page of a listing answered 304, which is GitHub
// saying the pass has already read all of it.
func everyPageNotModified[T any](pages []pageOf[T]) bool {
	for _, page := range pages {
		if page.modified {
			return false
		}
	}
	return len(pages) > 0
}

// withoutItems is these pages as the pass remembers them between passes: each page's ETag and
// whether it was full, and none of its items. A page the pass kept is a page the next read
// either sends that ETag for and is answered 304 on -- which record skips, since such a page is
// not modified -- or reads in full and answers with items of its own, so the items held here
// are never read again. A head's check runs carry each run's output.text, which GitHub caps at
// 65,535 characters, so keeping them would hold megabytes a head for as long as the head is open.
func withoutItems[T any](pages []pageOf[T]) []pageOf[T] {
	kept := make([]pageOf[T], len(pages))
	for i, page := range pages {
		kept[i] = pageOf[T]{etag: page.etag, full: page.full}
	}
	return kept
}
