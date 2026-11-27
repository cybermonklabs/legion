package daemon

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// baseRead is one base's batch of merge-state reads (workflowRuntime.baseReads): the repository
// as GitHub serves it and the base branch; owed, each issue whose pull request on that base is
// owed a read (baseOwed); climb, whether a request for a climb arrived since the ladder last took
// one up (baseLadderNext); and wake, which tells a follow's wait that the batch changed, so that it
// checks again whether a climb was asked, nothing is owed any more, or a read falls due sooner
// (baseFollowWait). A wake is no request: a wait that finds none of those goes on waiting. A
// baseRead stands in baseReads from the request that starts its ladder until that ladder ends, so
// a base has one ladder at a time, and the ladder ends only once nothing is owed: every owed pull
// request was answered a state, closed, moved its head off the one its reads follow, or left for
// another ladder. GitHub's rate limit and the pace of the queries are the installation's, not the
// base's (queryGate).
type baseRead struct {
	repo  ghrepo.Repository
	base  string
	owed  map[string]baseOwed
	climb bool
	wake  chan struct{}
}

// baseOwed is one issue's place in its base's batch: head, the head its reads follow, "" until a
// read found GitHub answering `unknown` for it; climb, whether it is in a climb, which a request
// for one puts it in and which reads the head the record holds whatever head the follow reads, so
// a push to the base that arrives while the issue is followed keeps the follow and adds its own
// reads; climbed, the climb's reads of it so far, up to mergeStateLadderReads; asked, the number of
// the request that last put it there (workflowRuntime.baseAsks, never reused), so a read that took
// the issue up before a later request changes nothing that request put; alone, whether a query
// that asked for it alone failed, which has it asked for alone until GitHub answers it, so a pull
// request GitHub cannot answer in time costs no other its read; and its own ladder of reads after
// its climb or its handover, as one pull request's follow was (dispatch://INFRA-4374 R4): attempt,
// the reads on that ladder so far, wait, the wait before the next one, and due, when that one is
// due. A pull request is read on its ladder no sooner than it is due, and a climb of other pull
// requests leaves its ladder as it is.
type baseOwed struct {
	head    string
	climb   bool
	climbed int
	asked   uint64
	alone   bool
	attempt int
	wait    time.Duration
	due     time.Time
}

// baseReadKey is the key of base of repo among the runtime's base reads: GitHub compares names
// without case, and a branch name holds no colon.
func baseReadKey(repo ghrepo.Repository, base string) string {
	return strings.ToLower(repo.String()) + ":" + base
}

// queueBaseMergeStates is what the workflow asks for on the commit of a push to base of repo
// (workflow's basePush, the engine's Config.ReadBaseMergeStates): a read of GitHub's merge state
// for each of issues, the pull requests on that base the push owes a read, which a push onto the
// base can turn behind or conflicting with it while GitHub sends no event of the pull request
// itself (dispatch://INFRA-4374 R0, R4). They are read in one batch, a climb of the base's reads
// (queueBaseRead): GraphQL queries of baseReadChunk pull requests each, asked for by number,
// rather than one GET /pulls/{number} of each. It only records the issues and wakes the reader,
// so the push's own transaction waits for no GitHub call, and a runtime with no reader queues
// nothing.
func (w *workflowRuntime) queueBaseMergeStates(repo, base string, issues []string) {
	repository, err := ghrepo.Parse("the pushed repository", repo)
	if err != nil {
		w.log.Warn("github: a push to a base names no repository the daemon can read; its pull requests are read at their next event",
			"repo", repo, "base", base, "issues", issues, "error", err)
		return
	}
	owed := make(map[string]string, len(issues))
	for _, issue := range issues {
		owed[issue] = ""
	}
	w.queueBaseRead(w.repositories.serving(repository), base, owed, true)
}

// queueBaseRead puts each issue of owed in the batch of base of repo, out of any other batch it
// was in, with the head its reads follow, and asks that batch's ladder for a climb (climb) or for
// a follow: a head a climb of one pull request handed over, whose own ladder of reads starts then,
// its first read two of the ladder's waits on, whatever the other owed pull requests' ladders
// stand at (baseOwed). A climb asked for an issue the batch already follows keeps the head it
// follows. A ladder that is not running starts (readBaseMergeStates); one that is is woken under
// the lock it reads its requests under, so it takes up a climb at once and a follow when it falls
// due. It only records and wakes (putLocked).
func (w *workflowRuntime) queueBaseRead(repo ghrepo.Repository, base string, owed map[string]string, climb bool) {
	if w.baseWake == nil || len(owed) == 0 {
		return
	}
	key := baseReadKey(repo, base)
	now, first := time.Now().UTC(), 2*w.mergeStateWait()
	starts := false
	w.mergeStateMu.Lock()
	for issue, head := range owed {
		var place baseOwed
		if read := w.baseReads[key]; read != nil && w.baseReadOf[issue] == key {
			place = read.owed[issue]
		}
		if head != "" {
			place.head = head
		}
		if climb {
			// A climb asked again starts again: the fact that asked is answered by reads made
			// after it.
			place.climb, place.climbed = true, 0
		} else {
			place.attempt, place.wait, place.due = 0, first, now.Add(first)
		}
		_, start := w.putLocked(repo, base, issue, place)
		starts = starts || start
	}
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(key)
	}
}

// putLocked puts issue in the batch of base of repo at place, out of any other batch it was in,
// under a new request (workflowRuntime.baseAsks), which place then names, and asks the batch's
// ladder for a climb when place is in one. A ladder that runs is woken; one that does not is to be
// started, which putLocked answers with the batch's key, and which the caller does once it has
// let the lock go (baseWake.add). Under mergeStateMu, so that each caller (queueBaseRead,
// moveBase) makes its own check and this placement in one step.
func (w *workflowRuntime) putLocked(repo ghrepo.Repository, base, issue string, place baseOwed) (string, bool) {
	key := baseReadKey(repo, base)
	read, running := w.batchLocked(repo, base)
	if w.baseReadOf[issue] != key {
		w.leaveBaseLocked(issue)
	}
	w.baseAsks++
	place.asked = w.baseAsks
	read.owed[issue] = place
	w.baseReadOf[issue] = key
	if place.climb {
		read.climb = true
	}
	if running {
		nudge(read.wake)
	}
	return key, !running
}

// batchLocked is the batch of base of repo, made when none stands, and whether one stood, which is
// whether its ladder is already running. Under mergeStateMu.
func (w *workflowRuntime) batchLocked(repo ghrepo.Repository, base string) (*baseRead, bool) {
	if w.baseReads == nil {
		w.baseReads = map[string]*baseRead{}
	}
	if w.baseReadOf == nil {
		w.baseReadOf = map[string]string{}
	}
	key := baseReadKey(repo, base)
	read, running := w.baseReads[key]
	if !running {
		read = &baseRead{repo: repo, base: base, owed: map[string]baseOwed{}, wake: make(chan struct{}, 1)}
		w.baseReads[key] = read
	}
	return read, running
}

// leaveBaseLocked takes issue out of the batch it is owed a read in, if any, under mergeStateMu:
// another batch takes it (putLocked), or the request that put it there is no longer owed a read
// (leaveBase). A batch left with no issue is woken, so its follow ends rather than wait to read
// nothing.
func (w *workflowRuntime) leaveBaseLocked(issue string) {
	key, owed := w.baseReadOf[issue]
	if !owed {
		return
	}
	delete(w.baseReadOf, issue)
	if read := w.baseReads[key]; read != nil {
		delete(read.owed, issue)
		if len(read.owed) == 0 {
			nudge(read.wake)
		}
	}
}

// ownsLocked says whether key's batch still holds issue under the request asked, under
// mergeStateMu.
func (w *workflowRuntime) ownsLocked(key, issue string, asked uint64) bool {
	read := w.baseReads[key]
	return read != nil && w.baseReadOf[issue] == key && read.owed[issue].asked == asked
}

// owns says whether key's batch still holds issue at all: a read that took it up applies no
// answer of it once a climb of its own has taken it, or another batch has.
func (w *workflowRuntime) owns(key, issue string) bool {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	return w.baseReadOf[issue] == key
}

// heldUnder is the batch that holds issue and the number of the request it holds it under, "" and
// 0 when no batch does: what a climb notes as it begins, and later hands leaveBase, which takes the
// issue out only while that request stands (climbMergeState).
func (w *workflowRuntime) heldUnder(issue string) (key string, asked uint64) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	key = w.baseReadOf[issue]
	if read := w.baseReads[key]; read != nil {
		asked = read.owed[issue].asked
	}
	return key, asked
}

// leaveBase takes issue out of key's batch when the request that put it there was asked, the one a
// read took it up under: GitHub answered it a state, the record or GitHub no longer holds it open
// at the head its reads follow, or a climb of its own reads it now (climbMergeState). A later
// request stands.
func (w *workflowRuntime) leaveBase(key, issue string, asked uint64) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if w.ownsLocked(key, issue, asked) {
		w.leaveBaseLocked(issue)
	}
}

// moveBase moves issue from key's batch to the batch of base of repo at place, its own ladder
// with it, when the request that put it in key's was place.asked: GitHub's read of the pull
// request names that base now, so its reads go on there. The check and the move are one step
// under mergeStateMu: a newer request for the issue - a climb of its own, or the head that climb
// handed over to a batch - either lands before it, and the move does nothing, or after it, and
// stands. So no move decided on an older read puts back a head a newer one replaced.
func (w *workflowRuntime) moveBase(key, issue string, place baseOwed, repo ghrepo.Repository, base string) {
	w.mergeStateMu.Lock()
	destination, starts := "", false
	if w.baseWake != nil && w.ownsLocked(key, issue, place.asked) {
		destination, starts = w.putLocked(repo, base, issue, place)
	}
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(destination)
	}
}

// setOwed sets issue's place in key's batch to place, when the request that put it there was
// asked. A later request stands.
func (w *workflowRuntime) setOwed(key, issue string, asked uint64, place baseOwed) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if w.ownsLocked(key, issue, asked) {
		w.baseReads[key].owed[issue] = place
	}
}

// readBaseMergeStates runs a ladder for each base queueBaseRead started one for, until ctx ends,
// each on a goroutine of its own, so one base's waits hold up no other's.
func (w *workflowRuntime) readBaseMergeStates(ctx context.Context) {
	if w.baseWake == nil {
		return
	}
	for {
		keys, ok := w.baseWake.take(ctx)
		if !ok {
			return
		}
		for _, key := range keys {
			go w.baseLadder(store.ForConcurrentWorker(ctx), key)
		}
	}
}

// baseLadder runs key's batch ladder: a climb while a pull request is in one (climbBase), and the
// reads that follow each owed pull request on its own ladder (followBase), for as long as anything
// is owed (baseLadderNext).
func (w *workflowRuntime) baseLadder(ctx context.Context, key string) {
	for {
		climb, ok := w.baseLadderNext(ctx, key)
		if !ok {
			return
		}
		if climb {
			w.climbBase(ctx, key)
		}
		w.followBase(ctx, key)
	}
}

// baseLadderNext takes up a climb key's batch was asked for, and says whether to climb - one was
// asked, or a pull request is still in one - and whether the ladder goes on. It ends the ladder -
// takes the batch out of baseReads - only when nothing is owed or ctx has ended: an owed pull
// request is read until it is answered or leaves, and no wake, timely or late, ends its reads. It
// is read under the lock queueBaseRead records under, so a request is either taken up by this
// ladder or starts a ladder of its own.
func (w *workflowRuntime) baseLadderNext(ctx context.Context, key string) (climb, ok bool) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	read := w.baseReads[key]
	select {
	case <-read.wake:
	default:
	}
	climb, read.climb = read.climb || anyClimbing(read), false
	if ctx.Err() != nil || len(read.owed) == 0 {
		for issue := range read.owed {
			if w.baseReadOf[issue] == key {
				delete(w.baseReadOf, issue)
			}
		}
		delete(w.baseReads, key)
		return false, false
	}
	return climb, true
}

// anyClimbing says whether a pull request read owes is in a climb. Under mergeStateMu.
func anyClimbing(read *baseRead) bool {
	for _, place := range read.owed {
		if place.climb {
			return true
		}
	}
	return false
}

// climbBase reads key's batch, mergeStateLadderWait apart, while a pull request it owes is in a
// climb, as climbMergeState reads one pull request: each pull request in a climb is read up to
// mergeStateLadderReads times, until GitHub answers it a state, and then, still `unknown` or never
// answered, goes on to its own ladder of reads, the first of them two of the ladder's waits after
// its climb's last read (nextPlace). A read that fails counts as a read that answered nothing:
// every owed pull request stays owed, those requested while it ran among them. The same reads take
// the other owed pull requests already due on their ladders, and no others. Whether a pull request
// is still in a climb is read under the lock queueBaseRead records under, so a climb asked while
// the read ran is read at the next step.
func (w *workflowRuntime) climbBase(ctx context.Context, key string) {
	for {
		w.readBase(ctx, key)
		w.mergeStateMu.Lock()
		climbing := anyClimbing(w.baseReads[key])
		w.mergeStateMu.Unlock()
		if !climbing {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-w.mergeStateAfter(w.mergeStateWait(), w.mergeStateWait()):
		}
	}
}

// The lines the reads after a climb log (followBase): one for each pull request each read answers
// or fails for, with its wait (readChunk, missed), and one for each pull request whose reads end
// because the record no longer holds it open at the head they read (readBase).
const (
	followReadLine = "github: a merge state read after the ladder"
	followEndLine  = "github: the merge state reads after the ladder end: the pull request is no longer open at the head they read"
)

// followBase reads the pull requests key's batch owes again, each on its own ladder
// (dispatch://INFRA-4374 R4): GitHub can go on answering `unknown` for an hour while it holds the
// head CONFLICTING, and a ladder that stopped at its fifth read left the record reading `unknown`
// all that while. A pull request's first read after its climb, or after it was handed over, falls
// due two of the ladder's waits (mergeStateWait, which a test shortens) on, and each next one
// twice as long after the one before, up to mergeStateFollowWaits of them; it is read once it is
// due and never before, in the same query as the others due then, later when the installation's
// gate holds or paces it (queryGate). Each read is logged with its wait and what GitHub answered
// (readBase), and applied as every read's is, so a CONFLICTING answer goes through R2's two reads
// as any read's does.
//
// A pull request's reads end when GitHub answers it a state, or when the record no longer holds
// it open at the head they follow, which is logged. The follow returns when a climb is asked or
// nothing is owed any more, before a wait or during it (baseFollowWait), since the ladder then
// climbs or ends (baseLadderNext), and when ctx ends. A read that fails is logged and followed by
// the next wait, since the record follows GitHub only while something reads it. It is held in
// memory, and a restart ends it; the daemon's start read queues a ladder for a head GitHub still
// answers `unknown` for (readIssuesAtStart), which climbs and follows in turn.
func (w *workflowRuntime) followBase(ctx context.Context, key string) {
	for w.baseFollowWait(ctx, key) {
		w.readBase(ctx, key)
	}
}

// baseFollowWait waits for the first owed pull request of key's batch to fall due, and says whether
// to read: false once a climb is asked of the batch, a pull request is in one, or nothing is owed
// in it, before the wait or during it, and false when ctx ends. A wake only has it look again: at a
// climb asked, at nothing owed, and at a pull request handed over that falls due sooner. A wake
// that arrives after the request it was sent for was taken up leaves it waiting for the same read.
// The wait runs on the ladders' clock (mergeStateAfter), given the wait that pull request's place
// on its ladder set.
func (w *workflowRuntime) baseFollowWait(ctx context.Context, key string) bool {
	for {
		w.mergeStateMu.Lock()
		read := w.baseReads[key]
		stop := read.climb || len(read.owed) == 0 || anyClimbing(read)
		next, set := firstDue(read)
		w.mergeStateMu.Unlock()
		if stop {
			return false
		}
		wait := time.Until(next)
		if wait <= 0 {
			return ctx.Err() == nil
		}
		select {
		case <-ctx.Done():
			return false
		case <-read.wake:
		case <-w.mergeStateAfter(set, wait):
			return true
		}
	}
}

// firstDue is the soonest moment an owed pull request of read on its ladder falls due, now for
// one with no due time yet, and the wait its place set before that read (baseOwed.wait). Under
// mergeStateMu, with nothing in a climb.
func firstDue(read *baseRead) (time.Time, time.Duration) {
	var first time.Time
	var set time.Duration
	for _, place := range read.owed {
		due := place.due
		if due.IsZero() {
			due = time.Now().UTC()
		}
		if first.IsZero() || due.Before(first) {
			first, set = due, place.wait
		}
	}
	return first, set
}

// baseReading is one owed issue a read of its base's batch reads: its place in the batch and the
// pull request the record holds open.
type baseReading struct {
	issue string
	owed  baseOwed
	pr    record.PullRequest
}

// readBase reads key's batch once: each owed pull request in a climb, and each other one due on its
// ladder, none before it is due.
//
// It first reads each issue's record. One whose record no longer holds its pull request open, or
// holds it at another head than the one its reads follow with no climb asked since, leaves the
// batch unread, logged as the end of its reads when it was on its ladder. One whose record read
// fails stays owed, and is read at its next turn.
//
// It then asks GitHub's GraphQL API for the rest by number, one query at a time from one queue:
// each one a query that asked for it alone failed for in a query of its own, until GitHub answers
// it, and the others baseReadChunk to a query (queryChunk), and applies each query's answers as
// they come, so one query that fails costs the others nothing (readChunk). A query of several that
// ran out of GitHub's time (queryTimedOut) puts each of its pull requests at the front of the
// queue, one to a query, and one that fails alone is asked for alone from then on; any other
// failed query - a 5xx GitHub answers at once, a refusal, no answer - is a read that answered
// nothing, and its pull requests go on along their ladders. A rate limit stops the read where it
// stands: the gate holds every query of the installation as long as GitHub asks (queryGate), and
// the pull requests no query answered go on along their ladders, due no sooner than the limit
// ends. So does a token the implement App could not mint for a query, since no query after it
// could be made either.
func (w *workflowRuntime) readBase(ctx context.Context, key string) {
	w.mergeStateMu.Lock()
	read := w.baseReads[key]
	repo, base, owed := read.repo, read.base, maps.Clone(read.owed)
	w.mergeStateMu.Unlock()

	now := time.Now().UTC()
	var together, alone []baseReading
	for _, issue := range slices.Sorted(maps.Keys(owed)) {
		place := owed[issue]
		if !place.climb && place.due.After(now) {
			continue
		}
		_, pr, err := issueRecord(ctx, w.pool, w.records, issue)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("github: read the record of a pull request owed a merge-state read; it is read at its next turn", "issue", issue, "error", err)
			}
			w.missed(key, issue, place, 0, place.alone)
			continue
		}
		if pr == nil || pr.State != record.PullRequestOpen || (place.head != "" && pr.HeadSHA != place.head && !place.climb) {
			if !place.climb {
				w.log.Info(followEndLine, "issue", issue, "head", place.head, "attempt", place.attempt+1, "wait", place.waitBefore(w.mergeStateWait()))
			}
			w.leaveBase(key, issue, place.asked)
			continue
		}
		r := baseReading{issue: issue, owed: place, pr: *pr}
		if place.alone {
			alone = append(alone, r)
		} else {
			together = append(together, r)
		}
	}
	var queries [][]baseReading
	chunk := w.chunkSize()
	for start := 0; start < len(together); start += chunk {
		queries = append(queries, together[start:min(start+chunk, len(together))])
	}
	for _, r := range alone {
		queries = append(queries, []baseReading{r})
	}
	for len(queries) > 0 && ctx.Err() == nil {
		part := queries[0]
		outcome := w.readChunk(ctx, key, repo, base, part)
		if outcome.unminted || outcome.after > 0 {
			w.readStopped(ctx, key, queries, outcome.after)
			return
		}
		queries = queries[1:]
		switch {
		case !outcome.failed:
		case outcome.timedOut && len(part) > 1:
			singles := make([][]baseReading, 0, len(part)+len(queries))
			for _, r := range part {
				singles = append(singles, []baseReading{r})
			}
			queries = append(singles, queries...)
		default:
			for _, r := range part {
				w.sendBack(ctx, key, r, 0, len(part) == 1)
			}
		}
	}
}

// readStopped sends back each pull request of queries that key's batch still holds (sendBack)
// after the read stopped before a query answered it: a rate limit, due no sooner than after on,
// or a token the implement App could not mint, after zero.
func (w *workflowRuntime) readStopped(ctx context.Context, key string, queries [][]baseReading, after time.Duration) {
	for _, part := range queries {
		for _, r := range part {
			if w.owns(key, r.issue) {
				w.sendBack(ctx, key, r, after, r.owed.alone)
			}
		}
	}
}

// sendBack is the one way a base's read sends back a pull request it read no answer for: it goes
// on along its ladder, due no sooner than after on, read alone or in a batch as alone says
// (missed), and its round is asked again (readFailed), once, so a run of null `mergeable` reads is
// told at the time bound though no read answers and a failure that repeats asks no round twice.
func (w *workflowRuntime) sendBack(ctx context.Context, key string, r baseReading, after time.Duration, alone bool) {
	w.readFailed(ctx, r.pr)
	w.missed(key, r.issue, r.owed, after, alone)
}

// chunkOutcome is what one query of a batch came to when it answered nothing: failed, it failed;
// unminted, it was never sent, since the implement App's token could not be minted for it;
// timedOut, it ran out of GitHub's time (queryTimedOut); and after, the wait a rate limit asked,
// zero for none.
type chunkOutcome struct {
	failed   bool
	unminted bool
	timedOut bool
	after    time.Duration
}

// readChunk asks GitHub for part, owed pull requests of key's batch on base of repo, in one query
// (queryChunk), applies each answer, and has each pull request leave the batch, move to another
// base's batch or go on along its climb or its ladder:
//
// Each answer goes through applyMergeability, as one GET /pulls/{number} of the pull request would:
// the same fact, under the same event id, its CONFLICTING answer reading the base's rules, and
// stamped with the moment its own query was answered, so that an answer read before a newer one
// the record holds is dropped as older (workflow's mergeability). An issue a climb of its own, or
// another batch, took while the query ran has no answer applied. A pull request GitHub answers a
// state for, or answers closed, leaves the batch; one it answers `unknown` for on another base
// moves to that base's batch (moveBase), by GitHub's word alone, so no stale record sends it back;
// and one it answers `unknown` for here goes on along its climb or its ladder. One the answer left
// out (an error of GitHub's for that pull request alone) goes on along its climb or its ladder too,
// and so does one whose answer the record did not take (applyMergeability failed, as a write the
// database fails fails it), whatever GitHub answered: the record holds what it held before, so the
// pull request is owed a read still, and leaving the batch would leave it unread until its next
// event (dispatch://INFRA-4374 R4).
//
// A query that failed has none of part touched, and its outcome says how it failed, which its
// caller acts on (readBase).
func (w *workflowRuntime) readChunk(ctx context.Context, key string, repo ghrepo.Repository, base string, part []baseReading) chunkOutcome {
	numbers := make([]int, len(part))
	for i, r := range part {
		numbers[i] = r.pr.Number
	}
	got, err := w.queryChunk(ctx, repo, numbers)
	if got.unminted {
		w.log.Warn("github: mint the implement App token to read the merge state of pull requests on a base; they are read again",
			"repo", repo.String(), "base", base, "asked", numbers, "error", err)
		return chunkOutcome{failed: true, unminted: true}
	}
	if err != nil {
		after, _ := githubrest.RetryAfter(err)
		w.log.Warn("github: read the merge state of the open pull requests on a base whose reads were asked for; their records stand as they were, and they are read again",
			"repo", repo.String(), "base", base, "asked", numbers, "timedOut", got.timedOut, "retryAfter", after, "error", err)
		return chunkOutcome{failed: true, timedOut: got.timedOut, after: after}
	}
	w.log.Info("github: read the merge state of pull requests on a base by number", "repo", repo.String(), "base", base,
		"asked", len(numbers), "answered", len(got.answers))
	for _, r := range part {
		if !w.owns(key, r.issue) {
			continue
		}
		node, ok := got.answers[r.pr.Number]
		if !ok {
			w.log.Warn("github: GitHub answered nothing for a pull request owed a merge-state read; it is read again",
				"issue", r.issue, "repo", repo.String(), "pullRequest", r.pr.Number, "error", got.unanswered[r.pr.Number])
			w.sendBack(ctx, key, r, 0, r.owed.alone)
			continue
		}
		answer := node.githubPullRequest()
		fact, err := w.applyMergeability(ctx, got.github, r.pr, answer, got.readAt, w.mergeabilityEventID(r.pr, got.readAt))
		state := fact.MergeState
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("github: the record did not take GitHub's merge state for a pull request owed a merge-state read; it is read again",
					"issue", r.issue, "head", r.pr.HeadSHA, "repo", repo.String(), "pullRequest", r.pr.Number, "answered", string(state), "error", err)
			}
			w.missed(key, r.issue, r.owed, 0, false)
			continue
		}
		next := w.nextPlace(r.owed, r.pr.HeadSHA, got.readAt, 0)
		next.alone = false
		if !r.owed.climb {
			w.log.Info(followReadLine, "issue", r.issue, "head", r.pr.HeadSHA, "attempt", next.attempt, "wait", r.owed.waitBefore(w.mergeStateWait()),
				"answered", string(state))
		}
		switch {
		// An answer the round can decide on ends the pull request's reads; `unknown`, and a
		// `blocked` answer missing part of what the round decides it on, keep them (answerIncomplete).
		case answer.State != "open" || !answerIncomplete(fact):
			w.leaveBase(key, r.issue, r.owed.asked)
		case answer.Base.Ref != "" && answer.Base.Ref != base:
			w.log.Info("github: GitHub holds a pull request owed a merge-state read on another base; its reads go on there",
				"issue", r.issue, "repo", repo.String(), "base", base, "now", answer.Base.Ref)
			w.moveBase(key, r.issue, next, repo, answer.Base.Ref)
		default:
			w.setOwed(key, r.issue, r.owed.asked, next)
		}
	}
	return chunkOutcome{}
}

// waitBefore is the wait the read of place now made on its ladder followed: its ladder's wait, two
// of unit for a pull request read before its ladder had one.
func (place baseOwed) waitBefore(unit time.Duration) time.Duration {
	if place.wait <= 0 {
		return 2 * unit
	}
	return place.wait
}

// nextPlace is place after a read of it at at that left it owed, the head it follows being head: in
// a climb, one more of the climb's reads, and after its mergeStateLadderReads-th a ladder of its
// own, its first read two of the ladder's waits on; on its ladder, that ladder's next step, twice
// the wait before, up to mergeStateFollowWaits of the ladder's waits; and either way due no sooner
// than after, a rate limit's wait.
func (w *workflowRuntime) nextPlace(place baseOwed, head string, at time.Time, after time.Duration) baseOwed {
	unit := w.mergeStateWait()
	if head != "" {
		place.head = head
	}
	if place.climb {
		if place.climbed++; place.climbed < mergeStateLadderReads {
			return place
		}
		place.climb, place.climbed = false, 0
		place.attempt, place.wait = 0, 2*unit
	} else {
		place.attempt, place.wait = place.attempt+1, min(2*place.waitBefore(unit), mergeStateFollowWaits*unit)
	}
	place.due = at.Add(max(place.wait, after))
	return place
}

// missed has issue's place in key's batch go on along its climb or its ladder after a read that
// answered nothing for it, or whose answer the record did not take, asked for alone from then on
// when alone says so, and logs that read when it was on its ladder.
func (w *workflowRuntime) missed(key, issue string, place baseOwed, after time.Duration, alone bool) {
	next := w.nextPlace(place, "", time.Now().UTC(), after)
	next.alone = alone
	if !place.climb {
		w.log.Warn(followReadLine, "issue", issue, "head", place.head, "attempt", next.attempt, "wait", place.waitBefore(w.mergeStateWait()),
			"answered", "no answer")
	}
	w.setOwed(key, issue, place.asked, next)
}
