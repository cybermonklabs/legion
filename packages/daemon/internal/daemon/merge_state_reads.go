package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
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
// request on that base the push owes a read, in one batch: GraphQL queries that ask for those pull
// requests alone, by number, baseReadChunk to a query (climbBase, readBase), which every base of
// an installation makes one at a time, paced and held by that installation's gate (queryGate).
//
// GitHub can answer `unknown` for far longer: INFRA-4391's record read `unknown` for over an hour
// while GitHub answered CONFLICTING and DIRTY for the same head (dispatch://INFRA-4374 R4). So a
// climb whose fifth read still answers `unknown` is followed by more reads of the same head, each
// after a wait twice the one before, from two of the ladder's waits up to mergeStateFollowWaits of
// them - one, two, four and eight minutes, then ten minutes each - until GitHub answers a state,
// the record's head moves off the head read, or the pull request is no longer open. Those reads
// are the base's batch's (followBase): a climb of one pull request hands its head to its base
// (followOnBase), each pull request keeps its own ladder of waits there, starting from its climb or
// its handover, and is read once it is due and never before, in the same query as those due then.
// A pull request's reads end once GitHub answers it a state. The
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
// climb was asked, nothing is owed any more, or a read falls due sooner (baseFollowWait). A wake is
// no request: a wait that finds none of those goes on waiting. A baseRead stands in baseReads from
// the request that starts its ladder until that ladder ends, so a base has one ladder at a time,
// and the ladder ends only once nothing is owed: every owed pull request was answered a state,
// closed, moved its head off the one its reads follow, or left for another ladder. GitHub's rate
// limit and the pace of the queries are the installation's, not the base's (queryGate).
type baseRead struct {
	repo  ghrepo.Repository
	base  string
	owed  map[string]baseOwed
	asks  uint64
	climb bool
	wake  chan struct{}
}

// baseOwed is one issue's place in its base's batch: head, the head its reads follow, "" until a
// read found GitHub answering `unknown` for it; climb, whether it is in a climb, which a request
// for one puts it in and which reads the head the record holds whatever head the follow reads, so
// a push to the base that arrives while the issue is followed keeps the follow and adds its own
// reads; climbed, the climb's reads of it so far, up to mergeStateLadderReads; asked, the request
// that last put it there, so a read that took the issue up before a later request changes nothing
// that request put; alone, whether a query that asked for it alone failed, which has it asked for
// alone until GitHub answers it, so a pull request GitHub cannot answer in time costs no other
// its read; and its own ladder of reads after its climb or its handover, as one pull request's
// follow was (dispatch://INFRA-4374 R4): attempt, the reads on that ladder so far, wait, the wait
// before the next one, and due, when that one is due. A pull request is read on its ladder no
// sooner than it is due, and a climb of other pull requests leaves its ladder as it is.
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
// due. It only records and wakes (queueBaseReadLocked).
func (w *workflowRuntime) queueBaseRead(repo ghrepo.Repository, base string, owed map[string]string, climb bool) {
	w.mergeStateMu.Lock()
	key, starts := w.queueBaseReadLocked(repo, base, owed, climb)
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(key)
	}
}

// queueBaseReadLocked is queueBaseRead's record, under mergeStateMu, so a caller can make it in
// the same step as a check of its own. It answers the batch's key and whether its ladder is to be
// started, which the caller does once it has let the lock go (baseWake.add).
func (w *workflowRuntime) queueBaseReadLocked(repo ghrepo.Repository, base string, owed map[string]string, climb bool) (string, bool) {
	key := baseReadKey(repo, base)
	if w.baseWake == nil || len(owed) == 0 {
		return key, false
	}
	read, running := w.batchLocked(repo, base)
	read.asks++
	now, first := time.Now().UTC(), 2*w.mergeStateWait()
	for issue, head := range owed {
		place, here := read.owed[issue]
		if !here || w.baseReadOf[issue] != key {
			w.leaveBaseLocked(issue)
			place = baseOwed{}
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
		read, running := w.batchLocked(repo, base)
		read.asks++
		w.leaveBaseLocked(issue)
		place.asked = read.asks
		destination = baseReadKey(repo, base)
		read.owed[issue] = place
		w.baseReadOf[issue] = destination
		if place.climb {
			read.climb = true
		}
		if running {
			nudge(read.wake)
		}
		starts = !running
	}
	w.mergeStateMu.Unlock()
	if starts {
		w.baseWake.add(destination)
	}
}

// updateOwed sets issue's place in key's batch to what change makes of it, when the request that
// put it there was asked. A later request stands.
func (w *workflowRuntime) updateOwed(key, issue string, asked uint64, change func(*baseOwed)) {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	if w.ownsLocked(key, issue, asked) {
		read := w.baseReads[key]
		place := read.owed[issue]
		change(&place)
		read.owed[issue] = place
	}
}

// queryGate is one installation's GraphQL queries, as every base's batch makes them with that
// installation's token (workflowRuntime.queryGate, keyed by githubrest.Client.Scope): GitHub
// budgets the installation, not the base, so its rate limit and the time its queries take are
// shared by every base of every repository of that owner. turn lets one query run at a time; hold
// is the moment GitHub's rate limit asked the installation's queries to wait until, and next the
// moment the last query's pace lets the next one go (run). It coordinates the batches of this
// process alone: another daemon process reading the same installation keeps a gate of its own.
type queryGate struct {
	turn chan struct{}
	mu   sync.Mutex
	hold time.Time
	next time.Time
}

// queryGate is the gate of the installation scope names, made the first time it is asked for.
func (w *workflowRuntime) queryGate(scope string) *queryGate {
	w.gateMu.Lock()
	defer w.gateMu.Unlock()
	if w.gates == nil {
		w.gates = map[string]*queryGate{}
	}
	gate := w.gates[scope]
	if gate == nil {
		gate = &queryGate{turn: make(chan struct{}, 1)}
		w.gates[scope] = gate
	}
	return gate
}

// run makes query once its turn comes and the gate's hold and pace have passed, and answers how
// long it took. After it, the gate lets no query go for as long as it took, so the installation's
// batches spend at most half of any stretch of time in GitHub's GraphQL work, inside the limit
// GitHub documents on a query's processing time (60 s of it in 60 s), and a query's rate limit,
// GraphQL's own or the secondary limit, holds every query of the installation as long as GitHub
// asks (githubrest.RetryAfter). A ctx that ends while it waits ends the wait and makes no query.
func (g *queryGate) run(ctx context.Context, query func() error) (time.Duration, error) {
	select {
	case g.turn <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	defer func() { <-g.turn }()
	g.mu.Lock()
	until := g.hold
	if g.next.After(until) {
		until = g.next
	}
	g.mu.Unlock()
	if wait := time.Until(until); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	start := time.Now()
	err := query()
	took := time.Since(start)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next = time.Now().Add(took)
	if after, _ := githubrest.RetryAfter(err); after > 0 {
		if until := time.Now().Add(after); until.After(g.hold) {
			g.hold = until
		}
	}
	return took, err
}

// queryTimeoutNear is how long a GraphQL query that failed has run when it ran into the time
// GitHub gives one query, 10 s: GitHub answers such a query 502, and on 2026-11-24 it did so 10.9 s
// after the query was sent.
const queryTimeoutNear = 9 * time.Second

// queryTimedOut says whether err, a query's failure after took, is the query running out of
// GitHub's time rather than GitHub failing it: it ran for queryTimeoutNear or longer, its own bound
// ended it, or GitHub's answer says it could not respond in time. A rate limit is none.
func queryTimedOut(err error, took time.Duration) bool {
	if err == nil {
		return false
	}
	if after, _ := githubrest.RetryAfter(err); after > 0 {
		return false
	}
	return took >= queryTimeoutNear || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(strings.ToLower(err.Error()), "couldn't respond to your request in time")
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

// climbing is how many pull requests key's batch owes are in a climb, none once it has ended.
func (w *workflowRuntime) climbing(key string) int {
	w.mergeStateMu.Lock()
	defer w.mergeStateMu.Unlock()
	read := w.baseReads[key]
	if read == nil {
		return 0
	}
	n := 0
	for _, place := range read.owed {
		if place.climb {
			n++
		}
	}
	return n
}

// climbBase reads key's batch, mergeStateLadderWait apart, while a pull request it owes is in a
// climb, as climbMergeState reads one pull request: each pull request in a climb is read up to
// mergeStateLadderReads times, until GitHub answers it a state, and then, still `unknown` or never
// answered, goes on to its own ladder of reads, the first of them two of the ladder's waits after
// its climb's last read (nextPlace). A read that fails counts as a read that answered nothing:
// every owed pull request stays owed, those requested while it ran among them. The same reads take
// the other owed pull requests already due on their ladders, and no others.
func (w *workflowRuntime) climbBase(ctx context.Context, key string) {
	for w.readBase(ctx, key) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.mergeStateWait()):
		}
	}
}

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
func (w *workflowRuntime) baseFollowWait(ctx context.Context, key string) bool {
	for {
		w.mergeStateMu.Lock()
		read := w.baseReads[key]
		stop := read.climb || len(read.owed) == 0 || anyClimbing(read)
		next := firstDue(read)
		w.mergeStateMu.Unlock()
		if stop {
			return false
		}
		wait := time.Until(next)
		if wait <= 0 {
			return ctx.Err() == nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-read.wake:
			timer.Stop()
		case <-timer.C:
			return true
		}
	}
}

// firstDue is the soonest moment an owed pull request of read on its ladder falls due, now for
// one with no due time yet. Under mergeStateMu, with nothing in a climb.
func firstDue(read *baseRead) time.Time {
	var first time.Time
	for _, place := range read.owed {
		due := place.due
		if due.IsZero() {
			due = time.Now().UTC()
		}
		if first.IsZero() || due.Before(first) {
			first = due
		}
	}
	return first
}

// baseReading is one owed issue a read of its base's batch reads: its place in the batch and the
// pull request the record holds open.
type baseReading struct {
	issue string
	owed  baseOwed
	pr    record.PullRequest
}

// readBase reads key's batch once: each owed pull request in a climb, and each other one due on its
// ladder, none before it is due. It answers how many pull requests the batch owes in a climb after
// the read, which climbBase reads again.
//
// It first reads each issue's record. One whose record no longer holds its pull request open, or
// holds it at another head than the one its reads follow with no climb asked since, leaves the
// batch unread, logged as the end of its reads when it was on its ladder.
//
// It then asks GitHub's GraphQL API for the rest by number: each one a query that asked for it alone
// failed for in a query of its own, until GitHub answers it, and the others baseReadChunk to a
// query (queryChunk), and applies each query's answers as they come, so one query that fails costs
// the others nothing (readChunk). A query of several that ran out of GitHub's time
// (queryTimedOut) is asked again one pull request at a time, and each that fails alone is asked for
// alone from then on; any other failed query - a 5xx GitHub answers at once, a refusal, no answer -
// is a read that answered nothing, and its pull requests go on along their ladders. A rate limit
// stops the read where it stands: the gate holds every query of the installation as long as GitHub
// asks (queryGate), and the pull requests no query answered go on along their ladders, due no sooner
// than the limit ends.
func (w *workflowRuntime) readBase(ctx context.Context, key string) int {
	w.mergeStateMu.Lock()
	read := w.baseReads[key]
	repo, base, owed := read.repo, read.base, maps.Clone(read.owed)
	w.mergeStateMu.Unlock()

	now := time.Now().UTC()
	var reading []baseReading
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
		reading = append(reading, baseReading{issue: issue, owed: place, pr: *pr})
	}
	if len(reading) == 0 {
		return w.climbing(key)
	}
	github, err := implementClient(ctx, w.tokens, w.githubAPI, repo)
	if err != nil {
		w.log.Warn("github: mint the implement App token to read the merge state of pull requests on a base; they are read again",
			"repo", repo.String(), "base", base, "error", err)
		for _, r := range reading {
			w.missed(key, r.issue, r.owed, 0, r.owed.alone)
		}
		return w.climbing(key)
	}
	var queries [][]baseReading
	var together, alone []baseReading
	for _, r := range reading {
		if r.owed.alone {
			alone = append(alone, r)
		} else {
			together = append(together, r)
		}
	}
	chunk := w.chunkSize()
	for start := 0; start < len(together); start += chunk {
		queries = append(queries, together[start:min(start+chunk, len(together))])
	}
	for _, r := range alone {
		queries = append(queries, []baseReading{r})
	}
	for i, part := range queries {
		if ctx.Err() != nil {
			return w.climbing(key)
		}
		outcome := w.readChunk(ctx, key, github, repo, base, part)
		if outcome.after > 0 {
			w.limited(key, queries[i:], outcome.after)
			return w.climbing(key)
		}
		if !outcome.failed {
			continue
		}
		if !outcome.timedOut || len(part) == 1 {
			for _, r := range part {
				w.missed(key, r.issue, r.owed, 0, len(part) == 1)
			}
			continue
		}
		for j, r := range part {
			single := w.readChunk(ctx, key, github, repo, base, []baseReading{r})
			if single.after > 0 {
				w.limited(key, append([][]baseReading{part[j:]}, queries[i+1:]...), single.after)
				return w.climbing(key)
			}
			if single.failed {
				w.missed(key, r.issue, r.owed, 0, true)
			}
		}
	}
	return w.climbing(key)
}

// limited has each pull request of queries that key's batch still holds go on along its ladder
// after a rate limit stopped the read before a query answered it, due no sooner than after on.
func (w *workflowRuntime) limited(key string, queries [][]baseReading, after time.Duration) {
	for _, part := range queries {
		for _, r := range part {
			if w.owns(key, r.issue) {
				w.missed(key, r.issue, r.owed, after, r.owed.alone)
			}
		}
	}
}

// chunkOutcome is what one query of a batch came to when it answered nothing: failed, it failed;
// timedOut, it ran out of GitHub's time (queryTimedOut); and after, the wait a rate limit asked,
// zero for none.
type chunkOutcome struct {
	failed   bool
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
// out (an error of GitHub's for that pull request alone) goes on along its climb or its ladder too.
//
// A query that failed has none of part touched, and its outcome says how it failed, which its
// caller acts on (readBase).
func (w *workflowRuntime) readChunk(ctx context.Context, key string, github githubrest.Client, repo ghrepo.Repository, base string, part []baseReading) chunkOutcome {
	numbers := make([]int, len(part))
	for i, r := range part {
		numbers[i] = r.pr.Number
	}
	answers, unanswered, readAt, timedOut, err := w.queryChunk(ctx, github, repo, numbers)
	if err != nil {
		after, _ := githubrest.RetryAfter(err)
		w.log.Warn("github: read the merge state of the open pull requests on a base whose reads were asked for; their records stand as they were, and they are read again",
			"repo", repo.String(), "base", base, "asked", numbers, "timedOut", timedOut, "retryAfter", after, "error", err)
		return chunkOutcome{failed: true, timedOut: timedOut, after: after}
	}
	w.log.Info("github: read the merge state of pull requests on a base by number", "repo", repo.String(), "base", base,
		"asked", len(numbers), "answered", len(answers))
	for _, r := range part {
		if !w.owns(key, r.issue) {
			continue
		}
		node, ok := answers[r.pr.Number]
		if !ok {
			w.log.Warn("github: GitHub answered nothing for a pull request owed a merge-state read; it is read again",
				"issue", r.issue, "repo", repo.String(), "pullRequest", r.pr.Number, "error", unanswered[r.pr.Number])
			w.missed(key, r.issue, r.owed, 0, r.owed.alone)
			continue
		}
		answer := node.githubPullRequest()
		// A failed apply is logged there, and the next read of the pull request applies it again.
		_ = w.applyMergeability(ctx, github, r.pr, answer, readAt, w.mergeabilityEventID(r.pr, readAt))
		state := answer.mergeState()
		next := w.nextPlace(r.owed, r.pr.HeadSHA, readAt, 0)
		next.alone = false
		if !r.owed.climb {
			w.log.Info(followReadLine, "issue", r.issue, "head", r.pr.HeadSHA, "attempt", next.attempt, "wait", r.owed.waitBefore(w.mergeStateWait()),
				"answered", string(state))
		}
		switch {
		case answer.State != "open" || state != record.MergeStateUnknown:
			w.leaveBase(key, r.issue, r.owed.asked)
		case answer.Base.Ref != "" && answer.Base.Ref != base:
			w.log.Info("github: GitHub holds a pull request owed a merge-state read on another base; its reads go on there",
				"issue", r.issue, "repo", repo.String(), "base", base, "now", answer.Base.Ref)
			w.moveBase(key, r.issue, next, repo, answer.Base.Ref)
		default:
			w.updateOwed(key, r.issue, r.owed.asked, func(place *baseOwed) { *place = next })
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
// answered nothing for it, asked for alone from then on when alone says so, and logs that read when
// it was on its ladder.
func (w *workflowRuntime) missed(key, issue string, place baseOwed, after time.Duration, alone bool) {
	next := w.nextPlace(place, "", time.Now().UTC(), after)
	next.alone = alone
	if !place.climb {
		w.log.Warn(followReadLine, "issue", issue, "head", place.head, "attempt", next.attempt, "wait", place.waitBefore(w.mergeStateWait()),
			"answered", "no answer")
	}
	w.updateOwed(key, issue, place.asked, func(owed *baseOwed) { *owed = next })
}

// baseReadChunk is how many pull requests one GraphQL query of a base's batch asks for, each by
// number (chunkQuery). GitHub ends a query that runs 10 s, and its merge state costs GitHub time
// for each pull request asked: on 2026-11-24, against cybermonklabs/legion's 408 open pull
// requests, a query of every open pull request on main, a hundred to a page with its merge state,
// answered 502 after about 11 s three times out of three; its twenty oldest asked for by number
// answered in 7.5 and 8.1 s and once 502 after 10.9 s; ten in 3.6 to 4.9 s; and five in 1.4 to
// 4.9 s over twelve queries. A query that fails all the same is asked again one pull request at a
// time (readBase).
const baseReadChunk = 5

// chunkSize is baseReadChunk, or the size a test sets (workflowRuntime.baseChunk).
func (w *workflowRuntime) chunkSize() int {
	if w.baseChunk > 0 {
		return w.baseChunk
	}
	return baseReadChunk
}

// pullRequestFields is what a read of a base's batch asks GitHub of each pull request: its
// number, whether it is open, its head, the base it sits on now, and GitHub's lazily computed
// mergeability and merge state for that head, which a GET /pulls/{number} answers as `state`,
// `head`, `base`, `mergeable` and `mergeable_state` (graphQLPull.githubPullRequest).
const pullRequestFields = "number state headRefOid headRefName baseRefName mergeable mergeStateStatus"

// chunkQuery is the GraphQL query of numbers, pull requests of the repository $owner/$name: each
// asked for by its number under the alias p<number>, so one that GitHub cannot answer leaves the
// others answered.
func chunkQuery(numbers []int) string {
	var query strings.Builder
	query.WriteString("query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n")
	for _, number := range numbers {
		fmt.Fprintf(&query, "    p%d: pullRequest(number: %d) { %s }\n", number, number, pullRequestFields)
	}
	query.WriteString("  }\n}")
	return query.String()
}

// graphQLPull is one pull request as chunkQuery answers it.
type graphQLPull struct {
	Number           int    `json:"number"`
	State            string `json:"state"`
	HeadRefOid       string `json:"headRefOid"`
	HeadRefName      string `json:"headRefName"`
	BaseRefName      string `json:"baseRefName"`
	Mergeable        string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
}

// githubPullRequest is p as GET /pulls/{number} answers the same pull request, the shape
// applyMergeability reads: GitHub's GraphQL `state` OPEN is the REST `open`, and CLOSED and MERGED
// are its `closed`; its `mergeable` MERGEABLE, CONFLICTING and UNKNOWN are the REST `mergeable`
// true, false and null; and its `mergeStateStatus` is the REST `mergeable_state` in capitals
// (CLEAN, BLOCKED, BEHIND, DIRTY, DRAFT, HAS_HOOKS, UNSTABLE, UNKNOWN). A state outside those reads
// as unknown, as a REST one does (githubPullRequest.mergeState).
func (p graphQLPull) githubPullRequest() githubPullRequest {
	pull := githubPullRequest{Number: p.Number, State: "closed", MergeableState: strings.ToLower(p.MergeStateStatus), Merged: p.State == "MERGED"}
	if p.State == "OPEN" {
		pull.State = "open"
	}
	pull.Base.Ref, pull.Head.Ref, pull.Head.SHA = p.BaseRefName, p.HeadRefName, p.HeadRefOid
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

// queryChunk asks GitHub's GraphQL API for numbers, pull requests of repo, with the implement App's
// installation token github carries, in one query (chunkQuery) bounded by issueRead, as a REST read
// of one pull request is, made through that installation's gate (queryGate.run: its rate-limit
// hold, its pace and one query at a time). It answers each pull request GitHub answered by number,
// GitHub's word on each it did not (an error of the answer's for that pull request alone, which
// leaves the others answered), the moment the answer was in hand, and whether a query that failed
// ran out of GitHub's time (queryTimedOut). A query GitHub refused or answered with no repository
// at all is an error.
func (w *workflowRuntime) queryChunk(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, numbers []int) (map[int]graphQLPull, map[int]string, time.Time, bool, error) {
	query := githubrest.Client{Token: github.Token, API: githubrest.GraphQLEndpoint(w.githubAPI)}
	var answer struct {
		Repository map[string]*graphQLPull `json:"repository"`
	}
	var readAt time.Time
	took, err := w.queryGate(github.Scope).run(ctx, func() error {
		bounded, cancel := context.WithTimeout(ctx, issueRead)
		defer cancel()
		err := query.GraphQL(bounded, chunkQuery(numbers), map[string]any{"owner": repo.Owner(), "name": repo.Name()}, &answer)
		readAt = time.Now().UTC()
		return err
	})
	var partial githubrest.GraphQLErrors
	if err != nil && !(errors.As(err, &partial) && answer.Repository != nil) {
		return nil, nil, readAt, queryTimedOut(err, took), fmt.Errorf("read pull requests %v of %s: %w", numbers, repo, err)
	}
	if answer.Repository == nil {
		return nil, nil, readAt, false, fmt.Errorf("read pull requests %v of %s: GitHub answered no repository", numbers, repo)
	}
	answers, unanswered := map[int]graphQLPull{}, map[int]string{}
	for _, number := range numbers {
		if node := answer.Repository[fmt.Sprintf("p%d", number)]; node != nil {
			answers[number] = *node
		} else {
			unanswered[number] = "no answer"
		}
	}
	for _, entry := range partial {
		if len(entry.Path) >= 2 {
			var number int
			if alias, ok := entry.Path[1].(string); ok {
				if _, err := fmt.Sscanf(alias, "p%d", &number); err == nil {
					if _, left := unanswered[number]; left {
						unanswered[number] = entry.Message
					}
				}
			}
		}
	}
	return answers, unanswered, readAt, false, nil
}
