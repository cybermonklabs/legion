package docs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackgroundQuiet is what AwaitBackgroundWrites answers of a Service's background writes: whether
// it waited for any, and the earliest moment a write it left armed for later may begin, with what
// that write is. At is zero when nothing is armed.
type BackgroundQuiet struct {
	Waited bool
	At     time.Time
	Writer string
}

// backgroundPoll is how often AwaitBackgroundWrites looks again at a write it waits for.
const backgroundPoll = 10 * time.Millisecond

// AwaitBackgroundWrites waits, within ctx, for every write the service runs in the background that
// is running now or armed to begin by soon, and answers when the first one it left armed for later
// may begin. Nothing in production calls it: a test that reads the whole database before and after
// a request (the API tests' databaseFingerprint) calls it so that no write of this service lands
// between its two reads unseen.
//
// The service's writers that run beside its callers, from its own code, and what the wait does
// with each:
//
//   - a settlement: one whose timer is due by soon or has fired and whose callback still runs
//     (settleTimer, settleCallbacks), or one a caller runs in a timer's place
//     (roomState.settling), it waits for; one armed for after soon it answers;
//   - a forced eviction (evicting), which flushes and compacts the room it closes, it waits for;
//   - the store of a room's updates its persistence has not taken or has not stored
//     (pendingUpdates, durableAppends), which a settlement's sweep of unrecorded marks leaves
//     behind it, and of the parked bytes no store has taken (roomState.parked), it waits for;
//   - a document socket (serving), whose peer's updates, the parked bytes they leave
//     (storeParked), its last browser's settlement (settleLastPeer) and the retirement of the room
//     that settlement loaded (retireSettlementRoom) write on the socket's own goroutines, it waits
//     for, so a socket a test keeps open fails the wait at ctx's end, naming it;
//   - a compaction under the tail rule, whichever path runs it - the queue's step, ygo's close of a
//     room, before or after ygo has taken the room out of its map, a retirement's or a forced
//     eviction's - folds only rows older than compactTail (compactRule). The queue's step in
//     flight it waits for. For every document with a row after its first it answers the moment the
//     oldest such row leaves compactTail, read by the queue's own check (checkCompaction,
//     compactionCheck.untilAged), which no path can fold before. A document whose rows a
//     compaction may fold already it compacts itself, under the tail rule and waiting for the
//     document's lock, once, so no path is left anything to fold until another row comes of age.
//     A document whose rows do not fold (compactUnfoldable) it fails on, naming the document and
//     why: the rows stay as stored and aged, so the next compaction at a later fold point may
//     fold them, at a moment the queue's check cannot answer.
//
// One of them can arm another - a settlement retries, a stored update arms a settlement - so it
// looks again until one look finds none (runningBackgroundWrite), and then once more, for a writer
// that moved from a count the look read into a register it had read before. Each look takes the
// service's locks on a goroutine of its own (within), so a lock held past ctx's end fails the wait
// rather than holding it. The service's other background work writes no row: the awareness relay's
// sends and a room query's answer are notifications (pg_notify), and warming the appends pool takes
// and gives back one connection. The loops cmd/dispatch starts (RunEditNotices,
// RunSettlementResumption, RunCompactionSweep and the others) run only where their caller starts
// them. A service whose process registered room sync (RegisterRoomSync) is refused: another
// process's notification can make it write at any moment.
func (s *Service) AwaitBackgroundWrites(ctx context.Context, soon time.Time) (BackgroundQuiet, error) {
	if s.roomSyncRegistered.Load() {
		return BackgroundQuiet{}, errors.New("this document service runs room sync (RegisterRoomSync): another process's notification can make it write at any moment, which no wait of this process sees")
	}
	var quiet BackgroundQuiet
	compacted := map[string]bool{}
	for {
		waited, err := s.awaitNoRunningWrite(ctx, soon)
		quiet.Waited = quiet.Waited || waited
		if err != nil {
			return quiet, err
		}
		next, err := s.nextBackgroundWrite(ctx)
		if err != nil {
			return quiet, err
		}
		room := ""
		for _, foldable := range next.foldable {
			if !compacted[foldable] {
				room = foldable
				break
			}
		}
		if room == "" {
			quiet.At, quiet.Writer = next.at, next.writer
			return quiet, nil
		}
		compacted[room] = true
		quiet.Waited = true
		result, err := s.persistence.compactRoom(ctx, room, tailRule, true)
		if err != nil {
			return quiet, fmt.Errorf("compact document %s, whose rows a background compaction may fold now: %w", room, err)
		}
		if result.outcome == compactUnfoldable {
			why := "an earlier compaction of the same fold point found that they do not fold into a state that reads back as them"
			if result.misread != nil {
				why = "they do not fold into a state that reads back as them: " + result.misread.Error()
			}
			return quiet, fmt.Errorf("document %s has rows older than %s that its compaction leaves as stored (%s), and a compaction at a later fold point may fold them at a moment this wait cannot answer", room, compactTail, why)
		}
	}
}

// awaitNoRunningWrite waits until two looks in a row find no background write of the service
// running now or due by soon (runningBackgroundWrite), and answers whether it waited for one. When
// ctx ends it fails naming the writer the last look found, whether ctx ended in the pause after
// that look or inside the look after it, which waits on the service's locks (within): a wait that
// ran out of time names what it was waiting for, wherever its bound fell. A bound that ends inside
// a look after one that found nothing, or inside the first, names the look itself.
func (s *Service) awaitNoRunningWrite(ctx context.Context, soon time.Time) (bool, error) {
	waited := false
	last := ""
	for empty := 0; empty < 2; {
		writer, err := within(ctx, func() string { return s.runningBackgroundWrite(soon) })
		if err != nil {
			if last != "" {
				return waited, fmt.Errorf("%s was still running at the wait's last look, and the look after it ran past the wait's bound: %w", last, err)
			}
			return waited, fmt.Errorf("look for the document service's background writes: %w", err)
		}
		if writer == "" {
			empty, last = empty+1, ""
			continue
		}
		empty, waited, last = 0, true, writer
		if err := pause(ctx, backgroundPoll); err != nil {
			return waited, fmt.Errorf("%s did not end: %w", writer, err)
		}
	}
	return waited, nil
}

// runningBackgroundWrite names a background write of the service that runs now or is due by soon,
// and answers "" when there is none. It reads the registers a writer stands in while it is owed -
// each room's record, then the settlement timers - before the counts a writer keeps while it runs
// (settleCallbacks, evicting, serving, the compaction step), in the order every hand-off between
// two of them takes: an append arms its room's settlement before the room's count of it falls (a
// room's record, then the timers), a fired timer's callback counts itself before it drops its timer
// (the timers, then settleCallbacks), and an append that fails its room starts the room's eviction
// while the room still counts the append (failRoomLocked, refuseClosedIssueAppend,
// closeRoomOnReplacedHistory: a room's record, then evicting). So a writer that moves on while one
// look reads is in what that look reads after. A writer that moves the other way, from a count into
// a register - a socket's peer update its room has not stored when the socket ends - is in the
// register at the next look, which AwaitBackgroundWrites takes before it goes on.
func (s *Service) runningBackgroundWrite(soon time.Time) string {
	var busy string
	s.rooms.Range(func(key, value any) bool {
		room, state := key.(string), value.(*roomState)
		state.mu.Lock()
		switch {
		case state.settling > 0:
			busy = "the settlement of document " + room
		case state.pendingLocked() || state.appendingLocked() || len(state.parked) > 0:
			busy = "the store of document " + room + "'s room updates"
		}
		state.mu.Unlock()
		return busy == ""
	})
	if busy != "" {
		return busy
	}
	s.timerMu.Lock()
	for _, armed := range s.timers {
		if !armed.due.After(soon) {
			s.timerMu.Unlock()
			return fmt.Sprintf("the settlement of document %s armed for %s", armed.room, armed.due.Format(time.RFC3339Nano))
		}
	}
	s.timerMu.Unlock()
	if s.betweenBackgroundReads != nil {
		s.betweenBackgroundReads()
	}
	switch {
	case s.settleCallbacks.Load() > 0:
		return "a document settlement whose timer fired"
	case s.evicting.Load() > 0:
		return "a forced eviction of a document room"
	case s.serving.Load() > 0:
		return "a document socket, whose peer's updates, parked bytes, last browser's settlement and room retirement write on its own goroutines"
	case s.compactions.stepping():
		return "a document compaction"
	}
	return ""
}

// armedWrite is what nextBackgroundWrite answers: the earliest moment a write the service leaves
// armed may begin and what it is, at the zero time when nothing is armed, and the documents whose
// rows a compaction may fold already.
type armedWrite struct {
	at       time.Time
	writer   string
	foldable []string
}

// compactableRoomsSQL names every document with a row after its first: the documents a compaction
// may one day fold. Which of them it may fold, and from when, is the queue's own check's to read
// (checkCompaction).
const compactableRoomsSQL = `select artifact_id::text from doc_updates group by artifact_id having count(*) > 1`

// nextBackgroundWrite answers the earliest moment a write the service leaves armed may begin, and
// what it is: the settlement timer due first, or the first compaction that may fold a document's
// rows, for every document with a row after its first, resident or not, since a close's
// compaction can still run once ygo has taken a room out of its map. It lists the documents a
// compaction may fold already.
func (s *Service) nextBackgroundWrite(ctx context.Context) (armedWrite, error) {
	var next armedWrite
	take := func(when time.Time, what string) {
		if next.at.IsZero() || when.Before(next.at) {
			next.at, next.writer = when, what
		}
	}
	timers, err := within(ctx, func() []settleTimer {
		s.timerMu.Lock()
		defer s.timerMu.Unlock()
		return slices.Collect(maps.Values(s.timers))
	})
	if err != nil {
		return next, fmt.Errorf("read the document settlements armed: %w", err)
	}
	for _, armed := range timers {
		take(armed.due, fmt.Sprintf("the settlement of document %s armed for %s", armed.room, armed.due.Format(time.RFC3339Nano)))
	}
	rows, err := s.store.Pool.Query(ctx, compactableRoomsSQL)
	if err != nil {
		return next, fmt.Errorf("list the documents a compaction may fold: %w", err)
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return next, fmt.Errorf("list the documents a compaction may fold: %w", err)
	}
	for _, room := range rooms {
		// From asked, which comes before the check's now(), so the moment reads no later than it is.
		asked := time.Now()
		check, err := s.checkCompaction(ctx, room)
		if err != nil {
			return next, fmt.Errorf("read when a compaction may fold document %s: %w", room, err)
		}
		switch {
		case check.later == 0:
		case check.aged:
			next.foldable = append(next.foldable, room)
			take(asked, fmt.Sprintf("a compaction of document %s, which may fold its rows older than %s now", room, compactTail))
		default:
			take(asked.Add(check.untilAged), fmt.Sprintf("a compaction of document %s, which may fold its rows older than %s", room, compactTail))
		}
	}
	return next, nil
}

// within runs look on a goroutine of its own and answers what it answers, or ctx's cause when ctx
// ends first. A look takes the service's locks (timerMu, each room's state.mu), and a holder that
// stalls in a slow call keeps one past any bound, so the wait never blocks on one past ctx; a look
// ctx left behind ends on its own once the lock is let go.
func within[T any](ctx context.Context, look func() T) (T, error) {
	answer := make(chan T, 1)
	go func() { answer <- look() }()
	select {
	case value := <-answer:
		return value, nil
	case <-ctx.Done():
		var zero T
		return zero, context.Cause(ctx)
	}
}

// pause waits for d, answering ctx's error when ctx ends first.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
