package cistore

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// Publisher publishes a rendered summary envelope. *bus.Client satisfies this.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// StartSummaryLoop runs the summary loop in a background goroutine until ctx is
// cancelled. Each pass emits one `pr.<n>.checks` envelope, carrying its SHA, for
// each commit of a pull request that has been quiet, none of whose recorded
// suites holds it (settlementReady) and all of whose check runs are terminal - the pull
// request's current head or not: a head whose push carried GitHub's skip-checks
// trailer runs no CI, so the settlement that stands for it is the earlier
// head's, which can settle after the new head arrives. Consumers decide which
// SHA a settlement stands for. A record a head-gated listener left terminal and
// unsettled settles only within the handover grace (due, legacyBacklog). held is
// set at each pass to how many such records the pass held back (backlogHeld).
// A record the loop does not settle - one not ready, or a head-gated
// listener's leftover it holds back - is named in one log line once it has had
// no event for heldAfter, and again only when its snapshot or its hold changes
// (logHeldLocked); each settlement published is logged too (settle).
// The store's CAS makes the loop idempotent across replicas.
//
// Passes run only at the instants start + k*tick, where start is when the loop
// started. The first runs at k = 1 and looks at every cached record; each later
// one runs only at an instant by which a cached record changed (the store's
// change feed) or a record the loop waits on fell due (its debounce ran out, its
// claim went stale, its handover grace ended, or it was held for heldAfter).
// With nothing changed and nothing due the loop arms no timer. A record is
// attempted at the first instant at or after it falls due, and looked at again
// at the next instant after each attempt, so an attempt that did not settle it
// is retried there. A store feeds one loop at a time (attachSummary): the loop
// detaches from it once its context ends, and a loop started on the store after
// that starts afresh.
func StartSummaryLoop(ctx context.Context, store *Store, pub Publisher, debounce, tick time.Duration, held *metrics.Gauge, logger *logging.Logger) {
	if tick <= 0 {
		// A non-positive tick has no grid; it panics in the caller, as time.NewTicker does.
		panic("cistore: non-positive tick for StartSummaryLoop")
	}
	wake := store.attachSummary()
	start := time.Now()
	z := newSummarizer(store, debounce)
	backlog := &backlogHeld{gauge: held}
	go func() {
		// armed is the k of the instant start + k*tick the timer is set for, 0 while it is stopped.
		armed := int64(1)
		timer := time.NewTimer(tick)
		arm := func(k int64) {
			armed = k
			timer.Reset(time.Until(start.Add(time.Duration(k) * tick)))
		}
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				store.detachSummary()
				return
			case <-wake:
				if k := int64(time.Since(start)/tick) + 1; armed == 0 || k < armed {
					arm(k)
				}
			case <-timer.C:
				passed := armed
				armed = 0
				count, next, ok := z.pass(pub, logger, time.Now().UnixMilli())
				backlog.observe(count, logger)
				if ok {
					arm(max(passed+1, gridIndexAtOrAfter(start, tick, next)))
				}
			}
		}
	}()
}

// gridIndexAtOrAfter returns the least k for which start + k*tick is at or after the wall-clock
// millisecond at, which it places on the monotonic clock by its distance from now.
func gridIndexAtOrAfter(start time.Time, tick time.Duration, at int64) int64 {
	now := time.Now()
	since := now.Add(time.Duration(at-now.UnixMilli()) * time.Millisecond).Sub(start)
	k := int64(since / tick)
	if time.Duration(k)*tick < since {
		k++
	}
	return k
}

// backlogHeld reports the records a head-gated listener left that the summary loop holds back
// (verdictHeld): its gauge is set to each pass's count, and the first pass that holds any logs the
// count once, at INFO. The loop does not wait for the CI cache to load (cmd/listener starts it
// before the cache is ready), so that first count is a floor, and the line says "at least".
type backlogHeld struct {
	gauge  *metrics.Gauge
	logged bool
}

func (b *backlogHeld) observe(held int, logger *logging.Logger) {
	b.gauge.Set(int64(held))
	if held > 0 && !b.logged {
		b.logged = true
		logger.Info("checks held back a head-gated listener's unsettled records, at least", slog.Int("records", held))
	}
}

// claimFresh reports whether st carries a claim of its own generation made within its window of
// now (claimWindow: 2*debounce, longer for a claim waiting for a GitHub read), which no pass
// reclaims. A claim of an older generation is never fresh.
func claimFresh(st State, now int64, debounce time.Duration) bool {
	return st.Claim != nil &&
		st.Claim.Generation == st.Generation &&
		st.Claim.ClaimedAt >= now-claimWindow(st.Claim, debounce).Milliseconds()
}

// heldAfter is how long after its last event a record the loop does not settle waits before the
// loop names what holds it (logHeldLocked): a record that is not ready (verdictStuck), and a
// head-gated listener's leftover the loop holds back (verdictHeld). Ten minutes is the default idle
// release of an issue pod (the daemon's runtime.kubernetes.idle_release), so a head the listener
// holds is named before its worker's pod goes.
const heldAfter = 10 * time.Minute

// heldLineAt is the millisecond from which the loop names what holds st: heldAfter past its last
// event.
func heldLineAt(st State) int64 {
	return st.LastEventAt + heldAfter.Milliseconds()
}

// verdict is what a pass does with a record (schedule).
type verdict int

const (
	// verdictIdle: the record is settled, and only a change makes it otherwise.
	verdictIdle verdict = iota
	// verdictWait: the record's verdict changes at a later millisecond.
	verdictWait
	// verdictHeld: the record is unsettled, ready and a head-gated listener's leftover
	// (legacyBacklog), which only a change makes otherwise.
	verdictHeld
	// verdictStuck: the record is unsettled, not ready (settlementReady) and has been quiet for
	// heldAfter, which only a change makes otherwise: the pass names what holds it and files it
	// idle.
	verdictStuck
	// verdictAttempt: the pass tries to settle the record now.
	verdictAttempt
)

// schedule returns the verdict on st at now, in wall-clock milliseconds, and for verdictWait the
// first millisecond at which the verdict changes. A record is attempted exactly when it is due
// and carries no fresh claim, held exactly when it is unsettled, ready and legacyBacklog holds
// it, and stuck exactly when it is unsettled, not ready and quiet for heldAfter (heldLineAt); an
// unsettled record that is not ready waits until then, and so does one given up (verifyStopped)
// until its backoff runs out.
func schedule(st State, now int64, debounce time.Duration) (verdict, int64) {
	if st.SettledEmitted {
		return verdictIdle, 0
	}
	if verifyStopped(st, now) {
		// The settlement episode was given up unanswered (giveUp): the next starts once its
		// backoff runs out, and after a give-up for good only a change of the record starts one.
		if st.VerifyRetryAt == 0 {
			return verdictIdle, 0
		}
		return verdictWait, st.VerifyRetryAt
	}
	if !settlementReady(st) {
		if at := heldLineAt(st); now < at {
			return verdictWait, at
		}
		return verdictStuck, 0
	}
	if d := st.LastEventAt + debounce.Milliseconds(); now < d {
		return verdictWait, d
	}
	if legacyBacklog(st, now, debounce) {
		return verdictHeld, 0
	}
	if claimFresh(st, now, debounce) {
		at := st.Claim.ClaimedAt + claimWindow(st.Claim, debounce).Milliseconds() + 1
		if g := st.LastEventAt + (debounce + handoverGrace).Milliseconds(); st.Schema == 0 && g < at {
			at = g
		}
		return verdictWait, at
	}
	return verdictAttempt, 0
}

// queueEntry is a record's entry in the summarizer's queue: the pass at or after at (wall-clock
// milliseconds) looks at key again.
type queueEntry struct {
	at  int64
	key string
}

// deadlineQueue is a min-heap of queue entries ordered by (at, key), with one entry per key;
// index holds each queued key's position. It implements heap.Interface.
type deadlineQueue struct {
	entries []queueEntry
	index   map[string]int
}

func (q *deadlineQueue) Len() int { return len(q.entries) }

func (q *deadlineQueue) Less(i, j int) bool {
	a, b := q.entries[i], q.entries[j]
	return a.at < b.at || a.at == b.at && a.key < b.key
}

func (q *deadlineQueue) Swap(i, j int) {
	q.entries[i], q.entries[j] = q.entries[j], q.entries[i]
	q.index[q.entries[i].key] = i
	q.index[q.entries[j].key] = j
}

func (q *deadlineQueue) Push(x any) {
	entry := x.(queueEntry)
	q.index[entry.key] = len(q.entries)
	q.entries = append(q.entries, entry)
}

func (q *deadlineQueue) Pop() any {
	last := len(q.entries) - 1
	entry := q.entries[last]
	q.entries[last] = queueEntry{}
	q.entries = q.entries[:last]
	delete(q.index, entry.key)
	return entry
}

// set queues key at at, in place of its entry if it has one.
func (q *deadlineQueue) set(key string, at int64) {
	if i, ok := q.index[key]; ok {
		q.entries[i].at = at
		heap.Fix(q, i)
		return
	}
	heap.Push(q, queueEntry{at: at, key: key})
}

// remove drops key's entry, if it has one.
func (q *deadlineQueue) remove(key string) {
	if i, ok := q.index[key]; ok {
		heap.Remove(q, i)
	}
}

// summarizer is the summary loop's view of one store: the records waiting on time, queued by the
// millisecond their verdict changes, the records it holds back, and what it last named of each
// record it holds. Every other cached record is idle until the store's change feed names it. A
// store feeds one summarizer at a time (attachSummary).
type summarizer struct {
	store    *Store
	debounce time.Duration

	// mu guards the fields below. No pass holds it, or the store's lock, while it settles a record.
	mu    sync.Mutex
	queue deadlineQueue
	held  map[string]struct{}
	// heldLogged holds, for each key whose held line was logged (logHeldLocked), the record's
	// snapshot (State.Hash) and the hold the line named, so the line is logged once for each. A
	// key's entry is dropped when its record settles or leaves the cache, and every entry when the
	// cache is emptied for a recreated bucket (takeChanges).
	heldLogged map[string]heldMark
	// verifying holds the settlements this loop has claimed and asked Dispatch to verify, by key
	// (verify.go): each waits for the record's VerifiedRev to reach its request. A wait is looked
	// at when its record changes and when it falls due (verifyWait.due), and is dropped once it
	// publishes or its claim is gone. Every wait goes when the cache is emptied for a recreated
	// bucket, whose records hold none of the claims.
	verifying map[string]verifyWait
	// verifyTimeout and verifyMaxAsks are verify.go's verifyTimeout and verifyMaxAsks, which a
	// test shortens.
	verifyTimeout time.Duration
	verifyMaxAsks int
}

// heldMark is what a held line was logged for (logHeldLocked): the record's snapshot (State.Hash)
// and each hold the snapshot does not take. Whether a check has a run id is not one: the snapshot
// takes each check's run id.
type heldMark struct {
	snapshot                    string
	overflowed, legacy, claimed bool
}

func newSummarizer(store *Store, debounce time.Duration) *summarizer {
	return &summarizer{
		store:         store,
		debounce:      debounce,
		queue:         deadlineQueue{index: map[string]int{}},
		held:          map[string]struct{}{},
		heldLogged:    map[string]heldMark{},
		verifying:     map[string]verifyWait{},
		verifyTimeout: verifyTimeout,
		verifyMaxAsks: verifyMaxAsks,
	}
}

// pass runs one pass at now (wall-clock milliseconds). It looks at each verify wait whose record
// changed or that is due (checkVerify), files each record the change feed names by its verdict,
// takes every queued record due by now, and attempts the due ones in key order, queueing each
// attempted record at now so the next pass looks at it again. Filing a record it holds past
// heldAfter logs what holds it, once (logHeldLocked). It returns how many records it holds back
// (verdictHeld) and, when the queue or a verify wait is not empty, the earliest millisecond it has
// to run again.
func (z *summarizer) pass(pub Publisher, logger *logging.Logger, now int64) (held int, next int64, ok bool) {
	z.mu.Lock()
	keys, reset := z.store.takeChanges()
	if reset {
		z.queue = deadlineQueue{index: map[string]int{}}
		z.held = map[string]struct{}{}
		z.heldLogged = map[string]heldMark{}
		z.verifying = map[string]verifyWait{}
	}
	var waits []string
	for key, wait := range z.verifying {
		if _, changed := keys[key]; changed || wait.due(now, z.verifyTimeout) {
			waits = append(waits, key)
		}
	}
	var batch []string
	for key := range keys {
		if _, attempt := z.fileLocked(logger, key, now); attempt {
			batch = append(batch, key)
		}
	}
	for z.queue.Len() > 0 && z.queue.entries[0].at <= now {
		batch = append(batch, heap.Pop(&z.queue).(queueEntry).key)
	}
	z.mu.Unlock()

	sort.Strings(waits)
	for _, key := range waits {
		z.mu.Lock()
		wait, waiting := z.verifying[key]
		z.mu.Unlock()
		if waiting {
			z.checkVerify(pub, logger, key, wait, now)
		}
	}

	sort.Strings(batch)
	for _, key := range batch {
		z.mu.Lock()
		st, attempt := z.fileLocked(logger, key, now)
		z.mu.Unlock()
		if !attempt {
			continue
		}
		z.settle(pub, logger, key, st, now)
		z.mu.Lock()
		delete(z.held, key)
		z.queue.set(key, now)
		z.mu.Unlock()
	}

	z.mu.Lock()
	defer z.mu.Unlock()
	if z.queue.Len() > 0 {
		next, ok = z.queue.entries[0].at, true
	}
	for _, wait := range z.verifying {
		if at := wait.dueAt(z.verifyTimeout); !ok || at < next {
			next, ok = at, true
		}
	}
	return len(z.held), next, ok
}

// fileLocked reads key's cached record and files it by its verdict at now: a wait in the queue at
// the millisecond the verdict changes, a held record in held, an idle, stuck or uncached one in
// neither. A stuck record has its held line logged (logHeldLocked) as it is filed. A held record
// is also queued at heldLineAt while that is ahead of now, a queue entry of its own since a held
// record waits on nothing else, and has its held line logged once it is not: so it stays in held,
// counted by backlogHeld, and is never attempted. For an attempt it returns the record and
// true, and files the key in neither.
func (z *summarizer) fileLocked(logger *logging.Logger, key string, now int64) (State, bool) {
	z.queue.remove(key)
	delete(z.held, key)
	st, cached := z.store.cachedState(key)
	if !cached {
		delete(z.heldLogged, key)
		return State{}, false
	}
	switch v, at := schedule(st, now, z.debounce); v {
	case verdictIdle:
		delete(z.heldLogged, key)
	case verdictWait:
		z.queue.set(key, at)
	case verdictStuck:
		z.logHeldLocked(logger, key, st)
	case verdictHeld:
		z.held[key] = struct{}{}
		if at := heldLineAt(st); now < at {
			z.queue.set(key, at)
		} else {
			z.logHeldLocked(logger, key, st)
		}
	case verdictAttempt:
		return st, true
	}
	return State{}, false
}

// heldRunningNamed is how many of a held record's checks that are not terminal its held line
// names; running_count gives how many there are.
const heldRunningNamed = 20

// logHeldLocked logs `checks held a head`, the line that names what holds st, the record cached
// under key, from the record itself: the head, its generation and its check count; each check that
// is not terminal as name:status (sorted, the first heldRunningNamed, and running_count); each
// suite that holds it (suiteHolds) as `<id> app=<app id> status=<status>` (sorted by id); whether
// no check has a run id (no_run_id); and the hold Hash does not take: overflowed, no schema (a
// head-gated listener's leftover, legacy), and a claim held. It is WARN where the record holds a
// check and every one is terminal (every_check_finished, as terminal reads it), and INFO
// otherwise, which a long job and a lost completion both give. It logs once for each snapshot and
// hold (heldLogged): a record filed again with the snapshot and hold it logged logs nothing, so no
// tick or pass logs it twice, and a change of the hold alone logs once more. Callers hold z.mu.
func (z *summarizer) logHeldLocked(logger *logging.Logger, key string, st State) {
	mark := heldMark{snapshot: st.Hash(), overflowed: st.Overflowed, legacy: st.Schema == 0, claimed: st.Claim != nil}
	if z.heldLogged[key] == mark {
		return
	}
	z.heldLogged[key] = mark
	noRunID := !hasCheckRunID(st)

	running := make([]string, 0)
	for name, check := range st.Checks {
		switch classify(check) {
		case catRunning, catQueued:
			if check.Name != "" {
				name = check.Name
			}
			running = append(running, name+":"+check.Status)
		}
	}
	sort.Strings(running)
	runningCount := len(running)
	if runningCount > heldRunningNamed {
		running = running[:heldRunningNamed]
	}
	suiteIDs := make([]string, 0, len(st.Suites))
	for id, suite := range st.Suites {
		if suiteHolds(suite) {
			suiteIDs = append(suiteIDs, id)
		}
	}
	sort.Strings(suiteIDs)
	suites := make([]string, len(suiteIDs))
	for i, id := range suiteIDs {
		suite := st.Suites[id]
		suites[i] = fmt.Sprintf("%s app=%s status=%s", id, suite.AppID, suite.Status)
	}
	everyCheckFinished := len(st.Checks) > 0 && runningCount == 0
	level := slog.LevelInfo
	if everyCheckFinished {
		level = slog.LevelWarn
	}
	logger.LogEvent(level, "checks held a head",
		slog.String("owner", st.Owner),
		slog.String("repo", st.Repo),
		slog.String("number", st.Number),
		slog.String("sha", st.SHA),
		slog.Uint64("generation", st.Generation),
		slog.Int("checks", len(st.Checks)),
		slog.Bool("every_check_finished", everyCheckFinished),
		slog.Any("running", running),
		slog.Int("running_count", runningCount),
		slog.Any("suites", suites),
		slog.Bool("overflowed", st.Overflowed),
		slog.Bool("no_run_id", noRunID),
		slog.Bool("legacy", mark.legacy),
		slog.Bool("claimed", mark.claimed),
	)
}

// settle tries to settle the record cached under key at now: it reclaims a stale claim, claims the
// record, and publishes and marks its settlement (publish). With verify on it publishes nothing
// yet: it writes the claim's verify request and leaves the claim waiting for Dispatch's answer
// (ask, checkVerify), which publishes it.
func (z *summarizer) settle(pub Publisher, logger *logging.Logger, key string, cached State, now int64) {
	store, debounce := z.store, z.debounce
	if cached.Claim != nil {
		staleBefore := now - claimWindow(cached.Claim, debounce).Milliseconds()
		claimGeneration := cached.Claim.Generation
		reclaimed, err := store.ReclaimSettlement(key, claimGeneration, staleBefore)
		if err != nil {
			logger.Warn("checks reclaim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
			return
		}
		if !reclaimed {
			return
		}
	}

	state, claimed, err := store.ClaimSettlement(key, cached.Hash(), cached.Generation, now, debounce)
	if err != nil {
		logger.Warn("checks claim failed", slog.String("error", err.Error()), slog.String("sha", cached.SHA))
		return
	}
	if !claimed {
		return
	}
	if store.verify {
		z.ask(logger, key, state, now, verifyWait{generation: state.Generation, hash: state.Hash(), since: now})
		return
	}
	z.publish(pub, logger, key, state, 0, now)
}

// publishOutcome is how a publication ended (publish): it ended for good -- published, or refused
// or failed in a way that lets the claim go or never settles the head (publishEnded); the
// authorization point could not read the record, and the claim is still held (publishUnread); or
// the authorization point refused it on a fresh read of the record (publishRefused).
type publishOutcome int

const (
	publishEnded publishOutcome = iota
	publishUnread
	publishRefused
)

// publish publishes the settlement of state, whose claim this loop holds, and marks it, releasing
// the claim when the publication fails, and answers how it ended. requested is the revision of
// the claim's verify request, zero where verify is off; the authorization point (ClaimStillHeld)
// reads the record fresh and refuses unless it still holds the claim and, for a request, its
// answer. A publication that succeeded logs `checks settlement published`, one INFO line naming
// the head, the generation and snapshot it settled, whether it superseded an earlier settlement of
// the record, and the verify request it was published on, so a settlement that never became a role
// message is shown to have left the listener; a publication refused or failed logs no such line.
func (z *summarizer) publish(pub Publisher, logger *logging.Logger, key string, state State, requested uint64, now int64) publishOutcome {
	store := z.store
	env, err := settlementEnvelope(state, contracts.NowMillis())
	if err != nil {
		logger.Error("checks payload failed", slog.String("error", err.Error()))
		return publishEnded
	}
	if err := env.Validate(); err != nil {
		logger.Error("checks invalid envelope", slog.String("error", err.Error()))
		if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
			logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
		}
		return publishEnded
	}
	snapshot := state.Hash()
	held, err := store.ClaimStillHeld(key, state.Generation, snapshot, requested, now, z.debounce)
	if err != nil {
		logger.Warn("checks claim verification failed", slog.String("error", err.Error()), slog.String("sha", state.SHA))
		return publishUnread
	}
	if !held {
		return publishRefused
	}
	if err := pub.Publish(env); err != nil {
		if errors.Is(err, bus.ErrRefused) {
			// NATS refuses this settlement at every attempt, so the commit never settles from here: its
			// record is marked overflowed, as one past its bounds is, and the refusal logged once.
			logger.Error("checks settlement refused",
				slog.String("error", err.Error()),
				slog.String("topic", env.Topic),
				slog.String("sha", state.SHA),
			)
			if err := store.markOverflowed(key); err != nil {
				logger.Warn("checks overflow not recorded", slog.String("error", err.Error()), slog.String("sha", state.SHA))
			}
			return publishEnded
		}
		logger.Warn("checks publish failed",
			slog.String("error", err.Error()),
			slog.String("topic", env.Topic),
			slog.String("sha", state.SHA),
		)
		if _, releaseErr := store.ReleaseClaim(key, state.Generation); releaseErr != nil {
			logger.Warn("checks release failed", slog.String("error", releaseErr.Error()), slog.String("sha", state.SHA))
		}
		return publishEnded
	}
	logger.Info("checks settlement published",
		slog.String("owner", state.Owner),
		slog.String("repo", state.Repo),
		slog.String("number", state.Number),
		slog.String("sha", state.SHA),
		slog.Uint64("generation", state.Generation),
		slog.String("snapshot", snapshot),
		slog.Bool("superseded", state.EmittedCount > 0),
		slog.Uint64("verify_request", requested),
	)
	if _, err := store.MarkSettled(key, state.Generation); err != nil {
		logger.Warn("checks mark-settled failed", slog.String("error", err.Error()))
	}
	return publishEnded
}

// settlementEnvelope is the checks envelope that settles state, issued at issuedAt.
func settlementEnvelope(state State, issuedAt int64) (contracts.Envelope, error) {
	sum := renderSummary(state)
	payload, err := json.Marshal(sum)
	if err != nil {
		return contracts.Envelope{}, err
	}
	return contracts.Envelope{
		EventID:       id.New(),
		Source:        "github",
		SourceEventID: id.New(),
		Topic:         contracts.GithubSubject(state.Owner, state.Repo, "pr."+state.Number+".checks"),
		DedupeKey: fmt.Sprintf(
			"github.checks.%s/%s.pr.%s.%s.g%d",
			state.Owner,
			state.Repo,
			state.Number,
			state.SHA,
			state.Generation,
		),
		IssuedAt:       issuedAt,
		PayloadSummary: settledPayloadSummary(sum),
		Payload:        string(payload),
		TraceID:        id.New(),
	}, nil
}

// settledPayloadSummary is the settlement's one line. A settlement that names a commit status adds
// how many statuses passed and failed, and the contexts of the failed ones, after the failing
// check runs; one that names none reads as before the record held statuses.
func settledPayloadSummary(sum Summary) string {
	summary := fmt.Sprintf(
		"checks settled on %s#%s @ %s: %d passed, %d failed, %d cancelled, %d skipped",
		sum.Repo, sum.Number, sha7(sum.SHA), sum.Passed.Count, sum.Failed.Count,
		sum.Cancelled.Count, sum.Skipped.Count,
	)
	if sum.Failed.Count > 0 {
		summary += "; failing: " + strings.Join(sum.Failed.Checks, ", ")
	}
	if len(sum.Statuses) > 0 {
		var failing []string
		for _, status := range sum.Statuses {
			if statusFailed(status.State) {
				failing = append(failing, status.Context)
			}
		}
		summary += fmt.Sprintf("; statuses: %d passed, %d failed", len(sum.Statuses)-len(failing), len(failing))
		if len(failing) > 0 {
			summary += "; failing statuses: " + strings.Join(failing, ", ")
		}
	}
	if sum.SupersededSettlement == "true" {
		summary += " (re-settled)"
	}
	return contracts.OneLineSummary(summary)
}

func sha7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
