package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// mergeStateLadderReads is how many times one climb of the ladder reads GitHub's merge state for a
// head GitHub answers `unknown` for, and mergeStateLadderWait how long it waits between those
// reads. GitHub computes the merge state lazily: the first read of a head it has not looked at yet
// asks it to, and answers `unknown` while it works, which is an answer about GitHub and not about
// the head. A few seconds is the usual wait, so five reads half a minute apart cover it with room
// over.
//
// Two ladders climb. A fact of one pull request - a review round waiting on GitHub, a head move of
// an issue in implementing through merging - climbs with reads of that pull request alone
// (climbMergeState, one GET /pulls/{number} each). A push to a base climbs with reads of every pull
// request on that base the push owes a read, in one batch: one GraphQL query a page of the base's
// open pull requests, however many are owed (climbBase, readBase).
//
// GitHub can answer `unknown` for far longer: INFRA-4391's record read `unknown` for over an hour
// while GitHub answered CONFLICTING and DIRTY for the same head (dispatch://INFRA-4374 R4). So a
// climb whose fifth read still answers `unknown` is followed by more reads of the same head, each
// after a wait twice the one before, from two of the ladder's waits up to mergeStateFollowWaits of
// them - one, two, four and eight minutes, then ten minutes each - until GitHub answers a state,
// the record's head moves off the head read, or the pull request is no longer open. Those reads
// are the base's batch (followBase): a climb of one pull request hands its head to its base
// (followOnBase), so every head of a base GitHub still answers `unknown` for is read again by one
// query, and the follow ends once no pull request owed a read on that base reads `unknown`. The
// same reads follow a second read of a conflicting head whose last attempt still read no answer
// (conflict_confirm.go's conflictConfirm, queueMergeStateFollow). A request for a read that arrives
// meanwhile ends the follow's wait and starts a climb at once. The reads are held in memory, as the
// climb is: no row waits in the outbox, and a restart ends them; the daemon's start reads every
// issue's pull request again and queues a ladder again for each one GitHub still answers `unknown`
// for (readIssuesAtStart), which climbs and follows in turn. `unknown` is no answer about the head,
// so nobody is told of it. Every other answer the ladder gets is recorded, and an approval left
// standing on a head GitHub answers with a state it would not merge from tells the architect that
// owns the issue and the reviewer, once for that head and state, in the review-stuck notice the
// round writes - but only once the CI settlement standing for the head says every check GitHub
// reported on it has concluded, since until then the state says no more than that a check is
// running (workflow's reviewRound, stuckMergeState).
//
// mergeStateLadderReads and mergeStateLadderWait have two more users, the outbox's reads of one
// pull request: the merge-queue read (merge_queue_removal.go's mergeQueueRead), which reads a pull
// request GitHub reported leaving the merge queue (dispatch://INFRA-3274/spec R1), and the second
// read of a head a first read found conflicting (conflict_confirm.go's conflictConfirm,
// dispatch://INFRA-4374 R2), each as this ladder reads a merge state GitHub answers `unknown`
// for. Each makes at most mergeStateLadderReads attempts, each with at most one read, and reads
// again mergeStateLadderWait (mergeStateWait) later, or after GitHub's Retry-After when that is
// longer. So a change to either number changes those reads too. mergeStateFollowWaits is the
// longest wait between two reads after a climb, counted in the ladder's waits: twenty half
// minutes, ten minutes.
const (
	mergeStateLadderReads = 5
	mergeStateLadderWait  = 30 * time.Second
	mergeStateFollowWaits = 20
)

// The lines the reads after a climb log (followBase, followOnBase): one for each pull request each
// read answers or fails for, with its wait, and one for each pull request whose reads end because
// the record no longer holds it open at the head they read.
const (
	followReadLine = "github: a merge state read after the ladder"
	followEndLine  = "github: the merge state reads after the ladder end: the pull request is no longer open at the head they read"
)

// queueMergeState is what the workflow asks for when a fact of one pull request owes a read of
// GitHub's merge state for an issue's open pull request (workflow's askMergeState, the engine's
// Config.ReadMergeState): a review round the fact leaves waiting on GitHub's answer for the head,
// and, by the gate classify.MergeStateReadOwed, a fact that moves the head of an issue in
// implementing through merging, which can turn the head conflicting with its base
// (dispatch://INFRA-4374 R0). A push to a base asks for its pull requests' reads all at once
// instead (queueBaseMergeStates). The daemon's start asks for it too, for each issue whose start
// read GitHub answered `unknown` (readIssuesAtStart). It queues a ladder of reads for issue that
// starts with a climb (queueMergeStateFollow, with no head to follow). It only records the issue
// and wakes the reader, so the fact's own transaction waits for no GitHub call, and a second fact
// of the same issue queued before the reader takes it is one ladder rather than two. A runtime
// with no reader - the API-only daemon, and a test that drives the ladder itself - queues nothing.
func (w *workflowRuntime) queueMergeState(issue string) {
	w.queueMergeStateFollow(issue, "")
}

// queueMergeStateFollow records that issue is owed a ladder (mergeStateQueued) and wakes the
// reader (readMergeStates): follow is the head the ladder hands to the reads of its base at once
// (followOnBase), "" for a ladder that starts with a climb (queueMergeState). A head is what the
// second read of a conflicting head asks for once its last attempt read no answer, GitHub's
// UNKNOWN or a read that failed, and released the first read (conflict_confirm.go's
// conflictConfirm, which attach hands this): reads of head that follow GitHub's merge state as the
// reads after a climb that ended on `unknown` do (followBase), the first of them after twice the
// ladder's wait, with no climb before them, since the second read's attempts were that climb
// (dispatch://INFRA-4374 R4). Their CONFLICTING answer is applied as every read's is, so it
// records a first conflicting read of the head and the second read starts again (R2).
//
// A climb takes the issue out of its base's reads (leaveBaseLocked): the climb reads it now, and
// hands it back to its base if GitHub still answers `unknown` at its last read, so no pull request
// is read by two ladders at once. An issue whose ladder already runs takes the request as a
// request for a read, and its ladder climbs again.
//
// A climb asked for before the reader takes the issue outranks a follow asked for beside it, in
// either order: the climb reads at once and follows in turn while GitHub answers `unknown`, and
// the fact that asked for it can have moved the head off the one the follow would read. Of two
// follows, the later one's head stands. A runtime with no reader records nothing.
func (w *workflowRuntime) queueMergeStateFollow(issue, follow string) {
	if w.mergeStates == nil || issue == "" {
		return
	}
	w.mergeStateMu.Lock()
	if w.mergeStateQueued == nil {
		w.mergeStateQueued = map[string]string{}
	}
	if queued, ok := w.mergeStateQueued[issue]; follow == "" || !ok || queued != "" {
		w.mergeStateQueued[issue] = follow
	}
	if follow == "" {
		w.leaveBaseLocked(issue)
	}
	w.mergeStateMu.Unlock()
	w.mergeStates.add(issue)
}

// readMergeStates runs a ladder for each issue queueMergeStateFollow queued, until ctx ends: each
// wake takes every issue queued since the last one, in the order of their keys, and runs its
// ladder on a goroutine of its own, so one issue's waits hold up no other's. The ladder starts as
// the issue's request asked (mergeStateQueued): with a climb, or by handing the head a second read
// named to its base. An issue whose ladder is still climbing starts no second one, and is marked
// as asked again instead, whichever kind of ladder it asked for: the running ladder climbs once
// more when its climb ends (mergeStateLadder), so the fact that asked is answered by a read made
// after it, which a ladder already past its last read would not be. Dropping the request outright
// left the issue standing at whatever the running ladder's last answer was, though the fact behind
// it - a check that concluded, a push, a review - is exactly what changes GitHub's answer
// (dispatch://INFRA-714 R4).
//
// mergeStateRunning is that bookkeeping: an issue is in it while its ladder runs
// (mergeStateRun). An issue a wake carries with no request standing in mergeStateQueued is skipped:
// its request was recorded while an earlier wake took the issue, and that wake took it up.
//
// It runs the reader of the bases' batches beside it for as long as it runs
// (readBaseMergeStates), since its ladders hand their follows to them.
func (w *workflowRuntime) readMergeStates(ctx context.Context) {
	go w.readBaseMergeStates(ctx)
	for {
		issues, ok := w.mergeStates.take(ctx)
		if !ok {
			return
		}
		w.mergeStateMu.Lock()
		if w.mergeStateRunning == nil {
			w.mergeStateRunning = map[string]*mergeStateRun{}
		}
		started := make(map[string]string, len(issues))
		for _, issue := range issues {
			follow, queued := w.mergeStateQueued[issue]
			if !queued {
				continue
			}
			delete(w.mergeStateQueued, issue)
			if run, running := w.mergeStateRunning[issue]; running {
				run.asked = true
				continue
			}
			w.mergeStateRunning[issue] = &mergeStateRun{}
			started[issue] = follow
		}
		w.mergeStateMu.Unlock()
		for _, issue := range issues {
			if follow, start := started[issue]; start {
				go w.mergeStateLadder(store.ForConcurrentWorker(ctx), issue, follow)
			}
		}
	}
}

// mergeStateRun is one running ladder's bookkeeping (workflowRuntime.mergeStateRunning): asked is
// whether a request for a read arrived since its climb began.
type mergeStateRun struct {
	asked bool
}

// mergeStateLadder runs issue's ladder: it climbs (climbMergeState) - or, when follow names a head,
// hands that head to its base's reads at once instead (queueMergeStateFollow) - hands the head a
// climb that ended on `unknown` to its base's reads (followOnBase), and climbs again for as long as
// something asked for another climb (mergeStateEnds); it leaves the issue unmarked as running when
// it returns.
func (w *workflowRuntime) mergeStateLadder(ctx context.Context, issue, follow string) {
	head := follow
	for {
		if head == "" {
			head = w.climbMergeState(ctx, issue)
		}
		if head != "" {
			w.followOnBase(ctx, issue, head)
		}
		if w.mergeStateEnds(ctx, issue) {
			return
		}
		head = ""
	}
}

// mergeStateEnds says whether issue's ladder ends here, and is the one place the issue's mark is
// taken off: it ends unless a request arrived while the climb ran and ctx is still live, and it is
// read under the lock readMergeStates marks under, so a request made during the last read is
// either marked before this reads it - and climbs - or lands after the mark is gone and starts a
// ladder of its own.
func (w *workflowRuntime) mergeStateEnds(ctx context.Context, issue string) bool {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if run := w.mergeStateRunning[issue]; run != nil && run.asked && ctx.Err() == nil {
		run.asked = false
		return false
	}
	delete(w.mergeStateRunning, issue)
	return true
}

// climbMergeState reads GitHub's merge state for issue's pull request until GitHub answers with
// one, at most mergeStateLadderReads times, mergeStateLadderWait apart. GitHub's `unknown` says it
// is still computing the state, so the climb asks again; every other answer, the read finding no
// open pull request among them, is an answer, and the climb ends on it and answers "". After the
// last `unknown` it ends too, and answers the head that read was made at, which the ladder then
// hands to its base's reads (followOnBase). A failed read ends the climb with nothing to follow:
// the issue's next event, its next handoff and the daemon's next start all read again.
func (w *workflowRuntime) climbMergeState(ctx context.Context, issue string) string {
	for read := 1; ; read++ {
		state, head, err := w.readMergeState(ctx, issue)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("read GitHub's merge state for an issue whose read was asked for; the record stands as it was until the next read",
					"issue", issue, "read", read, "error", err)
			}
			return ""
		}
		if state != record.MergeStateUnknown {
			return ""
		}
		if read >= mergeStateLadderReads {
			return head
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(w.mergeStateWait()):
		}
	}
}

// followOnBase hands head, the head of issue's pull request a climb last read `unknown` for or a
// second read of a conflict read no answer for, to the reads of the base the record holds that
// pull request on (queueBaseRead, as a follow): those reads read it again on the follow's waits, in
// one query with every other pull request of that base GitHub still answers `unknown` for, until
// GitHub answers a state (dispatch://INFRA-4374 R4). A record that no longer holds the pull request
// open at head is followed no more, and says so on the follow's end line.
func (w *workflowRuntime) followOnBase(ctx context.Context, issue, head string) {
	_, pr, err := issueRecord(ctx, w.pool, w.records, issue)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("github: hand a head GitHub answers unknown for to the reads of its base; nothing reads it again until its next event",
				"issue", issue, "head", head, "error", err)
		}
		return
	}
	if pr == nil || pr.State != record.PullRequestOpen || pr.HeadSHA != head {
		w.log.Info(followEndLine, "issue", issue, "head", head, "attempt", 0)
		return
	}
	repository, err := ghrepo.Parse("the pull request's repository", pr.Repo)
	if err == nil && pr.Base == "" {
		err = fmt.Errorf("the record holds pull request #%d on no base", pr.Number)
	}
	if err != nil {
		w.log.Warn("github: hand a head GitHub answers unknown for to the reads of its base; nothing reads it again until its next event",
			"issue", issue, "head", head, "error", err)
		return
	}
	w.queueBaseRead(w.repositories.serving(repository), pr.Base, map[string]string{issue: head}, false)
}

// mergeStateWait is how long the ladder waits between reads of a head GitHub is still computing
// the merge state of, the unit the reads after it wait in (followBase), and how long the outbox's
// merge-queue read and second conflict read wait before they read again (mergeQueueRead,
// conflictConfirm, which attach hands this wait): mergeStateLadderWait, which a test shortens
// through the runtime's own field.
func (w *workflowRuntime) mergeStateWait() time.Duration {
	if w.mergeStateRetry > 0 {
		return w.mergeStateRetry
	}
	return mergeStateLadderWait
}

// readMergeState reads issue's pull request from GitHub once (one GET /pulls/{number}) and applies
// what it says about merging that head into its base (applyMergeability, the same fact the
// per-issue read applies), answering GitHub's merge state as that read found it and the head the
// record held the pull request at when it read. An issue whose record holds no open pull request is
// read from nothing and answers record.MergeStateUnread and no head: the state belongs to a pull
// request, and there is none.
//
// The fact names the head GitHub answered for, which the engine records the answer against only
// while the record still stands at it (workflow's mergeability): this ladder reads GitHub with no
// transaction of its own, and a push can land while it waits. The ladder reads on, since what it
// answers is GitHub's state for the head it read, and the push the record took is itself a fact
// that asks for a read of the head it left (workflow's askMergeState).
func (w *workflowRuntime) readMergeState(ctx context.Context, issue string) (record.MergeState, string, error) {
	_, recorded, err := issueRecord(ctx, w.pool, w.records, issue)
	if err != nil {
		return record.MergeStateUnread, "", err
	}
	if recorded == nil || recorded.State != record.PullRequestOpen {
		return record.MergeStateUnread, "", nil
	}
	read, cancel := context.WithTimeout(ctx, issueRead)
	github, pull, err := w.readPullRequest(read, *recorded)
	answeredAt := time.Now().UTC()
	cancel()
	if err != nil {
		return record.MergeStateUnread, "", fmt.Errorf("read pull request %s#%d and its merge state: %w", recorded.Repo, recorded.Number, err)
	}
	// A failed apply is logged there, and the next read of the pull request applies it again.
	_ = w.applyMergeability(ctx, github, *recorded, pull, answeredAt, w.mergeabilityEventID(*recorded, answeredAt))
	return pull.mergeState(), recorded.HeadSHA, nil
}

// baseRead is one base's batch of merge-state reads (workflowRuntime.baseReads): the repository
// as GitHub serves it and the base branch; owed, each issue whose pull request on that base is
// owed a read (baseOwed); asks, the batch's count of requests, which names each one; climb,
// whether a request for a climb arrived since the ladder last took one up (baseLadderNext); and
// wake, which tells a follow's wait that the batch changed, so that it checks again whether a
// climb was asked or nothing is owed any more (baseFollowWait). A wake is no request: a wait that
// finds neither goes on waiting. A baseRead stands in baseReads from the request that starts its
// ladder until that ladder ends, so a base has one ladder at a time, and the ladder ends only once
// nothing is owed: every owed pull request was answered a state, closed, moved its head off the
// one its reads follow, or left for another ladder.
type baseRead struct {
	repo  ghrepo.Repository
	base  string
	owed  map[string]baseOwed
	asks  uint64
	climb bool
	wake  chan struct{}
}

// baseOwed is one issue's place in its base's batch: head, the head its reads follow, "" until a
// read found GitHub answering `unknown` for it; climb, whether a climb asked for it since, which
// reads the head the record holds whatever head the follow reads, so a push to the base that
// arrives while the issue is followed keeps the follow and adds its own read; and asked, the
// request that last put it there, so a read that took the issue up before a later request changes
// nothing that request put.
type baseOwed struct {
	head  string
	climb bool
	asked uint64
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
// (queueBaseRead): one GraphQL query a page of the base's open pull requests, rather than one GET
// /pulls/{number} of each. It only records the issues and wakes the reader, so the push's own
// transaction waits for no GitHub call, and a runtime with no reader queues nothing.
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
// a follow: a head a climb of one pull request handed over, which joins the follow's next read
// rather than postponing it. A climb asked for an issue the batch already follows keeps the head
// it follows (baseOwed). A ladder that is not running starts (readBaseMergeStates); one that is
// is woken under the lock it reads its requests under, so it takes up a climb at once and a follow
// at its next read. It only records and wakes (queueBaseReadLocked).
func (w *workflowRuntime) queueBaseRead(repo ghrepo.Repository, base string, owed map[string]string, climb bool) {
	w.mergeStateMu.Lock()
	key, starts := w.queueBaseReadLocked(repo, base, owed, climb)
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(key)
	}
}

// queueBaseReadLocked is queueBaseRead's record, under mergeStateMu, so a caller can make it in
// the same step as a check of its own (moveBase). It answers the batch's key and whether its ladder
// is to be started, which the caller does once it has let the lock go (baseWake.add).
func (w *workflowRuntime) queueBaseReadLocked(repo ghrepo.Repository, base string, owed map[string]string, climb bool) (string, bool) {
	key := baseReadKey(repo, base)
	if w.baseWake == nil || len(owed) == 0 {
		return key, false
	}
	if w.baseReads == nil {
		w.baseReads = map[string]*baseRead{}
	}
	if w.baseReadOf == nil {
		w.baseReadOf = map[string]string{}
	}
	read, running := w.baseReads[key]
	if !running {
		read = &baseRead{repo: repo, base: base, owed: map[string]baseOwed{}, wake: make(chan struct{}, 1)}
		w.baseReads[key] = read
	}
	read.asks++
	for issue, head := range owed {
		place, here := read.owed[issue]
		if !here || w.baseReadOf[issue] != key {
			w.leaveBaseLocked(issue)
			place = baseOwed{}
		}
		if head != "" {
			place.head = head
		}
		place.climb = place.climb || climb
		place.asked = read.asks
		read.owed[issue] = place
		w.baseReadOf[issue] = key
	}
	if climb {
		read.climb = true
	}
	if running {
		nudge(read.wake)
	}
	return key, !running
}

// leaveBaseLocked takes issue out of the batch it is owed a read in, if any, under mergeStateMu: a
// climb of its own reads it now (queueMergeStateFollow), or another batch takes it (queueBaseRead).
// A batch left with no issue is woken, so its follow ends rather than wait to read nothing.
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

// leaveBase takes issue out of key's batch when the request that put it there was asked, the one a
// read took it up under: GitHub answered it a state, or the record or GitHub no longer holds it
// open at the head its reads follow. A later request stands.
func (w *workflowRuntime) leaveBase(key, issue string, asked uint64) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if w.ownsLocked(key, issue, asked) {
		w.leaveBaseLocked(issue)
	}
}

// moveBase moves issue from key's batch to the batch of base of repo, its place with it, when the
// request that put it in key's was asked: the record, or GitHub's own read of the pull request,
// holds it on that base now, so its reads go on there. The check and the move are one step under
// mergeStateMu: a newer request for the issue - a climb of its own, or the head that climb handed
// over to a batch - either lands before it, and the move does nothing, or after it, and stands.
// So no move decided on an older read puts back a head a newer one replaced.
func (w *workflowRuntime) moveBase(key, issue string, place baseOwed, repo ghrepo.Repository, base string) {
	w.mergeStateMu.Lock()
	destination, starts := "", false
	if w.ownsLocked(key, issue, place.asked) {
		destination, starts = w.queueBaseReadLocked(repo, base, map[string]string{issue: place.head}, place.climb)
	}
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(destination)
	}
}

// followOwedHead has key's batch follow head for issue, which a read under the request asked found
// GitHub still answering `unknown` for: the batch's next reads read that head, and end for issue
// once the record leaves it; the climb that asked for the read is answered. A later request
// stands.
func (w *workflowRuntime) followOwedHead(key, issue string, asked uint64, head string) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if w.ownsLocked(key, issue, asked) {
		w.baseReads[key].owed[issue] = baseOwed{head: head, asked: asked}
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

// baseLadder runs key's batch ladder: a climb for a request that asked for one (climbBase), and
// otherwise, or after a climb that ended with a pull request GitHub still answers `unknown` for or
// that it could not read, a follow (followBase), for as long as anything is owed
// (baseLadderNext).
func (w *workflowRuntime) baseLadder(ctx context.Context, key string) {
	for {
		climb, ok := w.baseLadderNext(ctx, key)
		if !ok {
			return
		}
		follow := true
		if climb {
			follow = w.climbBase(ctx, key)
		}
		if follow {
			w.followBase(ctx, key)
		}
	}
}

// baseLadderNext takes up a climb key's batch was asked for, and says whether the ladder goes on.
// It ends the ladder - takes the batch out of baseReads - only when nothing is owed or ctx has
// ended: an owed pull request is read until it is answered or leaves, and no wake, timely or late,
// ends its reads. It is read under the lock queueBaseRead records under, so a request is either
// taken up by this ladder or starts a ladder of its own.
func (w *workflowRuntime) baseLadderNext(ctx context.Context, key string) (climb, ok bool) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	read := w.baseReads[key]
	select {
	case <-read.wake:
	default:
	}
	climb, read.climb = read.climb, false
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

// climbBase reads key's batch until GitHub answers a state for every owed pull request, at most
// mergeStateLadderReads times, mergeStateLadderWait apart, as climbMergeState reads one pull
// request, and says whether to follow: true when its last read still found one GitHub answers
// `unknown` for, or failed. A read that fails is logged and counts as a read that answered
// nothing: every owed pull request stays owed, those requested while it ran among them, and is
// read again on the ladder.
func (w *workflowRuntime) climbBase(ctx context.Context, key string) bool {
	for read := 1; ; read++ {
		unknown, err := w.readBase(ctx, key, nil)
		if err != nil && ctx.Err() == nil {
			w.log.Warn("github: read the merge state of the open pull requests on a base whose reads were asked for; their records stand as they were, and they are read again",
				"batch", key, "read", read, "error", err)
		}
		if err == nil && unknown == 0 {
			return false
		}
		if read >= mergeStateLadderReads {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(w.mergeStateWait()):
		}
	}
}

// followBase reads key's batch again after a climb whose last read still found a pull request
// GitHub answers `unknown` for, or for a head a climb of one pull request handed over
// (dispatch://INFRA-4374 R4): GitHub can go on answering `unknown` for an hour while it holds the
// head CONFLICTING, and a ladder that stopped at its fifth read left the record reading `unknown`
// all that while. Each read waits twice as long as the one before, from two of the ladder's waits
// (mergeStateWait, which a test shortens) up to mergeStateFollowWaits of them, and logs, for each
// owed pull request, its wait and what GitHub answered (readBase); readBase applies each answer as
// it applies every read's, so a CONFLICTING one goes through R2's two reads as any read's does. A
// head handed over meanwhile joins the next read, whose wait it does not restart.
//
// The follow ends when a read finds no owed pull request on the base reading `unknown`: GitHub
// answered each a state, or the record no longer holds it open at the head the reads follow, which
// reads nothing of it and is logged; when a climb is asked or nothing is owed any more, before a
// wait or during it (baseFollowWait), since the ladder then climbs or ends (baseLadderNext); and
// when ctx ends. A read that fails is logged and followed by the next wait, since the record
// follows GitHub only while something reads it. It is held in memory, and a restart ends it; the
// daemon's start read queues a ladder for a head GitHub still answers `unknown` for
// (readIssuesAtStart), which climbs and follows in turn.
func (w *workflowRuntime) followBase(ctx context.Context, key string) {
	wait := w.mergeStateWait()
	for attempt := 1; ; attempt++ {
		wait = min(2*wait, mergeStateFollowWaits*w.mergeStateWait())
		if !w.baseFollowWait(ctx, key, wait) {
			return
		}
		if unknown, err := w.readBase(ctx, key, &followAttempt{attempt: attempt, wait: wait}); err == nil && unknown == 0 {
			return
		}
	}
}

// baseFollowWait waits wait before a read after the climb (followBase), and says whether to make
// that read: false once a climb is asked of key's batch or nothing is owed in it, before the wait
// or during it, and false when ctx ends. A wake only has it check those two again: a head handed
// over, or a wake that arrives after the request it was sent for was taken up, leaves it waiting
// out its wait.
func (w *workflowRuntime) baseFollowWait(ctx context.Context, key string, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		w.mergeStateMu.Lock()
		read := w.baseReads[key]
		stop := read.climb || len(read.owed) == 0
		w.mergeStateMu.Unlock()
		if stop {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-read.wake:
		case <-timer.C:
			return true
		}
	}
}

// followAttempt is which read after a climb a read of a batch is, and the wait before it.
type followAttempt struct {
	attempt int
	wait    time.Duration
}

// baseReading is one owed issue a read of its base's batch reads: its place in the batch and the
// pull request the record holds open on the base.
type baseReading struct {
	issue string
	owed  baseOwed
	pr    record.PullRequest
}

// readBase reads key's batch once and answers how many owed pull requests GitHub still answers
// `unknown` for, or could not be read.
//
// It first reads each owed issue's record. One whose record no longer holds its pull request open,
// or holds it at another head than the one its reads follow with no climb asked since, leaves the
// batch unread, logged as the follow's end when follow names a read after a climb; one the record
// holds on another base moves to that base's batch (moveBase).
//
// It then reads the merge state of every open pull request on the base from GitHub in one GraphQL
// query a page (queryBase), and applies each owed pull request's answer through applyMergeability,
// as one GET /pulls/{number} of it would be applied: the same fact, under the same event id, its
// CONFLICTING answer reading the base's rules, and stamped with the moment its own page was
// answered, so that an answer read before a newer one the record holds is dropped as older
// (workflow's mergeability) however long the later pages took. An issue a climb of its own, or
// another batch, took while the query ran has no answer applied. An owed pull request GitHub does
// not list open on the base is read alone (readPullRequest): it may have closed, or moved to
// another base with its head unchanged, which GitHub sends no event of that the daemon reads, and
// that read records the base GitHub holds it on.
//
// A pull request GitHub answers a state for, or answers closed, leaves the batch; one it answers
// `unknown` for on another base moves to that base's batch; and one it answers `unknown` for here
// is followed at the head the record holds. A pull request on the base that is owed no read is
// answered and left alone. A read after a climb logs each owed pull request's answer with its
// wait, as the follow's lines.
func (w *workflowRuntime) readBase(ctx context.Context, key string, follow *followAttempt) (int, error) {
	w.mergeStateMu.Lock()
	read := w.baseReads[key]
	repo, base, owed := read.repo, read.base, maps.Clone(read.owed)
	w.mergeStateMu.Unlock()

	var reading []baseReading
	for _, issue := range slices.Sorted(maps.Keys(owed)) {
		place := owed[issue]
		_, pr, err := issueRecord(ctx, w.pool, w.records, issue)
		if err != nil {
			return len(owed), err
		}
		switch {
		case pr == nil || pr.State != record.PullRequestOpen || (place.head != "" && pr.HeadSHA != place.head && !place.climb):
			if follow != nil {
				w.log.Info(followEndLine, "issue", issue, "head", place.head, "attempt", follow.attempt, "wait", follow.wait)
			}
			w.leaveBase(key, issue, place.asked)
		case pr.Base != base:
			w.log.Info("github: the record holds a pull request owed a read on another base; its reads go on there",
				"issue", issue, "repo", repo.String(), "base", base, "recorded", pr.Base)
			w.moveBase(key, issue, place, repo, pr.Base)
		default:
			reading = append(reading, baseReading{issue: issue, owed: place, pr: *pr})
		}
	}
	if len(reading) == 0 {
		return 0, nil
	}
	numbers := make([]int, len(reading))
	for i, r := range reading {
		numbers[i] = r.pr.Number
	}
	github, pulls, pages, err := w.queryBase(ctx, repo, base, numbers)
	if err != nil {
		if follow != nil {
			for _, r := range reading {
				w.log.Warn(followReadLine, "issue", r.issue, "head", r.pr.HeadSHA, "attempt", follow.attempt, "wait", follow.wait,
					"answered", "no answer", "error", err)
			}
		}
		return len(reading), err
	}
	w.log.Info("github: read the merge state of the open pull requests on a base", "repo", repo.String(), "base", base,
		"owed", len(reading), "pages", pages)
	unknown := 0
	for _, r := range reading {
		if !w.owns(key, r.issue) {
			continue
		}
		client, answer, readAt := github, githubPullRequest{}, time.Time{}
		if listed, ok := pulls[r.pr.Number]; ok {
			answer, readAt = listed.pull.githubPullRequest(base), listed.readAt
		} else {
			w.log.Info("github: a pull request owed a read is not among the open pull requests GitHub lists on its base; it is read alone",
				"issue", r.issue, "repo", repo.String(), "base", base, "pullRequest", r.pr.Number)
			bounded, cancel := context.WithTimeout(ctx, issueRead)
			alone, pull, err := w.readPullRequest(bounded, r.pr)
			readAt = time.Now().UTC()
			cancel()
			if err != nil {
				if follow != nil {
					w.log.Warn(followReadLine, "issue", r.issue, "head", r.pr.HeadSHA, "attempt", follow.attempt, "wait", follow.wait,
						"answered", "no answer", "error", err)
				}
				unknown++
				continue
			}
			client, answer = alone, pull
		}
		// A failed apply is logged there, and the next read of the pull request applies it again.
		_ = w.applyMergeability(ctx, client, r.pr, answer, readAt, w.mergeabilityEventID(r.pr, readAt))
		state := answer.mergeState()
		if follow != nil {
			w.log.Info(followReadLine, "issue", r.issue, "head", r.pr.HeadSHA, "attempt", follow.attempt, "wait", follow.wait,
				"answered", string(state))
		}
		switch {
		case answer.State != "open" || state != record.MergeStateUnknown:
			w.leaveBase(key, r.issue, r.owed.asked)
		case answer.Base.Ref != "" && answer.Base.Ref != base:
			w.moveBase(key, r.issue, baseOwed{head: r.pr.HeadSHA, asked: r.owed.asked}, repo, answer.Base.Ref)
		default:
			unknown++
			w.followOwedHead(key, r.issue, r.owed.asked, r.pr.HeadSHA)
		}
	}
	return unknown, nil
}

// openPullRequestsQuery is the one GraphQL query a read of a base's batch makes, a page at a time:
// the open pull requests on base of the repository, each with its number, its head and GitHub's
// lazily computed mergeability and merge state for that head, which a GET /pulls/{number} answers
// as `mergeable` and `mergeable_state` (graphQLPull.githubPullRequest).
const openPullRequestsQuery = `query($owner: String!, $name: String!, $base: String!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: OPEN, baseRefName: $base, first: 100, after: $cursor) {
      nodes { number headRefOid headRefName mergeable mergeStateStatus }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// graphQLPull is one open pull request as openPullRequestsQuery answers it.
type graphQLPull struct {
	Number           int    `json:"number"`
	HeadRefOid       string `json:"headRefOid"`
	HeadRefName      string `json:"headRefName"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
}

// githubPullRequest is p as GET /pulls/{number} answers the same pull request on base, the shape
// applyMergeability reads: GitHub's GraphQL `mergeable` MERGEABLE, CONFLICTING and UNKNOWN are the
// REST `mergeable` true, false and null, and its `mergeStateStatus` is the REST `mergeable_state`
// in capitals (CLEAN, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNSTABLE, UNKNOWN). A state outside
// those reads as unknown, as a REST one does (githubPullRequest.mergeState).
func (p graphQLPull) githubPullRequest(base string) githubPullRequest {
	pull := githubPullRequest{Number: p.Number, State: "open", MergeableState: strings.ToLower(p.MergeStateStatus)}
	pull.Base.Ref, pull.Head.Ref, pull.Head.SHA = base, p.HeadRefName, p.HeadRefOid
	switch p.Mergeable {
	case "MERGEABLE":
		mergeable := true
		pull.Mergeable = &mergeable
	case "CONFLICTING":
		mergeable := false
		pull.Mergeable = &mergeable
	}
	return pull
}

// listedPull is one pull request a page of openPullRequestsQuery answered, and the moment that
// page's answer was in hand, which its fact is stamped with (readBase).
type listedPull struct {
	pull   graphQLPull
	readAt time.Time
}

// queryBase reads the open pull requests on base of repo from GitHub's GraphQL API with the
// implement App's installation token for its owner, a page of a hundred at a time, until it has
// read every pull request of numbers or GitHub names no next page, and answers the REST client the
// same token built (the base's rules a CONFLICTING answer reads, readBaseRules), each pull request
// it read by number with the moment its page was answered, and how many pages it read. Each page
// is bounded by issueRead, as a REST read of one pull request is.
func (w *workflowRuntime) queryBase(ctx context.Context, repo ghrepo.Repository, base string, numbers []int) (githubrest.Client, map[int]listedPull, int, error) {
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repo)
	if err != nil {
		return githubrest.Client{}, nil, 0, fmt.Errorf("mint the implement App token for %s: %w", repo.Owner(), err)
	}
	query := githubrest.Client{Token: github.Token, API: githubrest.GraphQLEndpoint(w.githubAPI)}
	wanted := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		wanted[number] = true
	}
	read := map[int]listedPull{}
	var cursor *string
	for pages := 1; ; pages++ {
		var answer struct {
			Repository *struct {
				PullRequests struct {
					Nodes    []graphQLPull `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		err := query.GraphQL(bounded, openPullRequestsQuery,
			map[string]any{"owner": repo.Owner(), "name": repo.Name(), "base": base, "cursor": cursor}, &answer)
		readAt := time.Now().UTC()
		cancel()
		if err != nil {
			return github, nil, pages, fmt.Errorf("read the open pull requests on %s of %s, page %d: %w", base, repo, pages, err)
		}
		if answer.Repository == nil {
			return github, nil, pages, fmt.Errorf("read the open pull requests on %s of %s: GitHub answered no repository", base, repo)
		}
		for _, pull := range answer.Repository.PullRequests.Nodes {
			if wanted[pull.Number] {
				read[pull.Number] = listedPull{pull: pull, readAt: readAt}
			}
		}
		page := answer.Repository.PullRequests.PageInfo
		if len(read) == len(wanted) || !page.HasNextPage {
			return github, read, pages, nil
		}
		if page.EndCursor == "" || (cursor != nil && *cursor == page.EndCursor) {
			return github, nil, pages, fmt.Errorf("read the open pull requests on %s of %s: GitHub named a next page after page %d with no new cursor", base, repo, pages)
		}
		next := page.EndCursor
		cursor = &next
	}
}
