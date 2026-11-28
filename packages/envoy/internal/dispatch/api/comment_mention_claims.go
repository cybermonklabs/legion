package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// mentionClaimRefresh is how often the mention sender writes the claims of the sends waiting on
// its lanes again (refreshHeldClaims). It is well inside model.DeliveryClaimLapse, so a claim this
// sender holds never lapses while the process is alive however long its lane is held, and a
// process that stops or dies writes none again, so every claim it still held lapses within one
// lease and the delivery sweep takes those attempts.
const mentionClaimRefresh = model.DeliveryClaimLapse / 3

// mentionClaim is one attempt the sender holds and the claim it last wrote on it.
type mentionClaim struct {
	key       mentionKey
	claimedAt time.Time
}

func (m mentionSend) claim() mentionClaim {
	return mentionClaim{key: m.key(), claimedAt: m.claimed}
}

// mentionClaimState is what the mention sender records of one attempt it holds
// (mentionSender.held).
type mentionClaimState struct {
	// claimed is the claim this sender holds on the attempt, as it last wrote it, so one statement
	// writes the claims of every attempt it holds again: every send that has not started, and a
	// send in flight until it ends (claimedDuring). A try that claims the attempt for itself takes that
	// claim out of the statements' reach, as each matches only the claim recorded here, and park
	// records the try's; a try that reads the rule alone (sendCommentMention) writes no claim, so
	// its send's row still carries the one recorded here, and a refresh that writes it meanwhile
	// records what it wrote (refreshHeldClaims).
	claimed time.Time
	// unsure is that a refresh named claimed and got no answer, so the row may carry a claim this
	// sender wrote and never learned; the next refresh or try re-reads it (refreshHeldClaims,
	// claimKnown), and a claim recorded from an answer ends it.
	unsure bool
	// lost counts the refreshes whose answer was lost while they named this attempt. A re-read
	// applies what it read only while the count is the one it began under: a refresh that lost
	// its answer after the read took the row may have written a claim the read never saw.
	lost uint64
	// trying is that a try of the attempt is in flight (mentionSender.markTrying), from before the try
	// until its park records its claim or the send ends (claimedDuring), which the re-read of the unsure
	// claims leaves alone (rereadUnsure).
	trying bool
}

// markTrying marks key's try in flight: the one writer of this sender's own its row can have
// beside the refresh, which rereadUnsure leaves alone while it runs. The mark stays until the try's
// park clears it, under the lock it records the try's claim under (park), or the send ends
// (claimedDuring). A send that only reads its claim (claimKnown) writes nothing, and is not marked.
func (m *mentionSender) markTrying(key mentionKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, mine := m.held[key]; mine {
		state.trying = true
		m.held[key] = state
	}
}

// claimKnown is item with the claim its row carries, read before its try acts on it when its
// recorded claim is unsure: a refresh named that claim and got no answer, so the row may carry one
// the refresh wrote (mentionSender's doc). The read answers a row's claim only while it is this
// sender's own - at or past the claim recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL) - and this goroutine's is the one try of the attempt, so no other
// try's claim can be among them. The claim read is recorded unless one was recorded from an
// answer meanwhile, which item then carries instead; no claim read leaves the record as it is,
// the row's claim being no longer this sender's to carry, and the try finds it so. ok is false
// when the read fails: the claim is still unknown, and the send makes no try.
func (m *mentionSender) claimKnown(item mentionSend) (_ mentionSend, ok bool) {
	key := item.key()
	m.mu.Lock()
	began := m.held[key]
	m.mu.Unlock()
	if !began.unsure {
		return item, true
	}
	read, err := m.reread(m.ctx, []mentionClaim{item.claim()})
	if err != nil {
		slog.Error("dispatch: read the claim of a comment mention a lost refresh may have written",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
		return item, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.held[key]
	switch {
	case !state.claimed.Equal(item.claimed):
		item.claimed = state.claimed
	case state.lost != began.lost:
		// Another refresh lost its answer after the read: the claim stays unknown, and the try
		// carries the one recorded, which the row's claim, if this sender's, is at or past.
	default:
		state.unsure = false
		if len(read) == 1 {
			item.claimed = read[0].claimedAt
			state.claimed = item.claimed
		}
		m.held[key] = state
	}
	return item, true
}

// keepClaims writes the claims of the sends waiting on the lanes again every claimRefresh
// (mentionClaimRefresh), until the lifetime ends or wait runs. Nothing is written again after
// that, which is what hands a stopped process's queue to the delivery sweep a lease later.
func (m *mentionSender) keepClaims() {
	defer m.done.Done()
	refresh := time.NewTicker(m.claimRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-m.stopped:
			return
		case <-refresh.C:
			m.refreshHeldClaims(m.ctx)
		}
	}
}

// refreshHeldClaims writes the claim of every attempt this sender holds again, in one statement,
// and records what that statement wrote: the next refresh and the lane's own send both read the
// claim this sender last took. The statement moves only a row that still carries the claim this
// sender recorded, so it never moves the claim a try has taken for itself out from under that
// try's settle, and it goes on writing the row of a send in flight whose try reads the rule alone
// and writes none (held).
//
// The statement runs without the lock, so for its length a row carries a newer claim than the map
// records. That gap is safe: the row's claim only moves forward, so the delivery sweep, which
// reads committed rows past a lease, sees it live sooner, never later, and the lane's bound send
// accepts any claim at or past the one it carries within a lease (model.DeliveryClaimSinceSQL).
// Its result is recorded only for a key whose claim the map still holds as the snapshot took it,
// so a claim written meanwhile - a keyed repeat's (queueLocked) - is never set back.
//
// A statement that answers an error may have committed all the same, so it leaves every claim it
// named that the map still holds unknown (mentionClaimState.unsure), never assumed, and the
// refresh re-reads those rows at once (rereadUnsure), leaving a send whose try is in flight to
// its park, which records the claim that try took; the next refresh or try re-reads whatever is
// still unknown before it acts on it (claimKnown). A read proves a row's claim this sender's own
// while it is less than a lease after the claim recorded (model.DeliveryClaimSinceSQL), and a lost
// refresh wrote at most one claim past the one recorded, less than a lease after it, since it
// writes only while the claim it names is live and every refresh after it names the recorded one,
// which the row no longer carries.
func (m *mentionSender) refreshHeldClaims(ctx context.Context) {
	m.rereadUnsure(ctx)
	m.mu.Lock()
	queued := make([]mentionClaim, 0, len(m.held))
	snapshot := make(map[mentionKey]time.Time, len(m.held))
	for key, state := range m.held {
		queued = append(queued, mentionClaim{key: key, claimedAt: state.claimed})
		snapshot[key] = state.claimed
	}
	m.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	refreshed, err := m.refresh(ctx, queued)
	if err != nil {
		slog.Error("dispatch: write the claims of the queued comment mentions again; the claims it named are unknown until read",
			"attempts", len(queued), "error", err)
	}
	m.mu.Lock()
	if err != nil {
		for key, claimed := range snapshot {
			if state, mine := m.held[key]; mine && state.claimed.Equal(claimed) {
				state.unsure = true
				state.lost++
				m.held[key] = state
			}
		}
		m.mu.Unlock()
		m.rereadUnsure(ctx)
		return
	}
	defer m.mu.Unlock()
	for _, claim := range refreshed {
		if state, mine := m.held[claim.key]; mine && state.claimed.Equal(snapshot[claim.key]) {
			state.claimed, state.unsure = claim.claimedAt, false
			m.held[claim.key] = state
		}
	}
}

// rereadUnsure reads the claims the rows of the unsure attempts carry, for those no try is in
// flight for, and records each (mentionSender's doc). The read answers a row's claim only while
// it is this sender's own - at or past the claim recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL) - and a claim is recorded only while its attempt is no try's and
// its record is the one the read named, so a try's own claim, which a try that started after the
// read began may have taken, is never recorded here: that try records its own (park), or ends
// (claimedDuring). An attempt it answers no row for is no longer this sender's to write, and stays as it
// is. A read that fails leaves every one unsure for the next refresh.
func (m *mentionSender) rereadUnsure(ctx context.Context) {
	m.mu.Lock()
	var unsure []mentionClaim
	lost := map[mentionKey]uint64{}
	for key, state := range m.held {
		if state.unsure && !state.trying {
			unsure = append(unsure, mentionClaim{key: key, claimedAt: state.claimed})
			lost[key] = state.lost
		}
	}
	m.mu.Unlock()
	if len(unsure) == 0 {
		return
	}
	read, err := m.reread(ctx, unsure)
	if err != nil {
		slog.Error("dispatch: read the claims of the comment mentions a lost refresh may have written",
			"attempts", len(unsure), "error", err)
		return
	}
	rows := make(map[mentionKey]time.Time, len(read))
	for _, claim := range read {
		rows[claim.key] = claim.claimedAt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, claim := range unsure {
		state, mine := m.held[claim.key]
		if !mine || !state.unsure || state.trying || !state.claimed.Equal(claim.claimedAt) || state.lost != lost[claim.key] {
			continue
		}
		state.unsure = false
		if row, ok := rows[claim.key]; ok {
			state.claimed = row
		}
		m.held[claim.key] = state
	}
}

// mentionClaimArrays is claims as the four arrays the sender's statements match a comment_deliveries
// row against: its comment, target and attempt, and the claim this sender wrote on it.
func mentionClaimArrays(claims []mentionClaim) (comments, targets []string, attempts []int32, claimed []time.Time) {
	comments = make([]string, len(claims))
	targets = make([]string, len(claims))
	attempts = make([]int32, len(claims))
	claimed = make([]time.Time, len(claims))
	for i, claim := range claims {
		comments[i], targets[i] = claim.key.comment, claim.key.target
		attempts[i], claimed[i] = int32(claim.key.attempt), claim.claimedAt
	}
	return comments, targets, attempts, claimed
}

// refreshMentionClaims is the mention sender's refresh: one statement writes the claim of every
// attempt the sender holds, and answers the claims it wrote. A row that no longer carries the
// claim this sender recorded is left alone and not answered - a try has taken it for itself, or
// another sender has, or it has settled - so refreshHeldClaims leaves that send's recorded claim
// as it was. So is a row whose recorded claim has lapsed: the attempt is free for any sender to
// take (model.DeliveryClaimFreeSQL), the lane's own send among them, and a claim written on it
// would be one more than a lease past the claim recorded, which no re-read of a lost answer can
// tell from another sender's (rereadMentionClaims). Every claim this statement writes is
// therefore less than a lease after the claim it names.
func (s *server) refreshMentionClaims(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(queued)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		update comment_deliveries d set claimed_at = now()
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as queued(comment_id, target, attempt, claimed_at)
		where d.comment_id = queued.comment_id and d.target = queued.target and d.attempt = queued.attempt
		  and d.state = 'pending' and d.claimed_at = queued.claimed_at
		  and `+model.DeliveryClaimLiveSQL("queued.claimed_at")+`
		returning d.comment_id::text, d.target, d.attempt, d.claimed_at
	`, comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return collectMentionClaims(rows)
}

// rereadMentionClaims is the mention sender's reread: one statement reads, and writes nothing,
// the claim of every attempt named whose row is pending and carries a claim of this sender's own:
// at or past the claim the sender recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL), since another sender takes a claim only once the one on the row
// has lapsed, and so writes one more than a lease past it. The sender asks it for attempts whose
// recorded claim a refresh named and got no answer for (mentionClaimState.unsure), and records
// what it answers.
func (s *server) rereadMentionClaims(ctx context.Context, unsure []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(unsure)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select d.comment_id::text, d.target, d.attempt, d.claimed_at
		from comment_deliveries d
		join unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as recorded(comment_id, target, attempt, claim)
			on d.comment_id = recorded.comment_id and d.target = recorded.target and d.attempt = recorded.attempt
		where d.state = 'pending' and `+model.DeliveryClaimSinceSQL("recorded.claim"), comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return collectMentionClaims(rows)
}

// collectMentionClaims reads the claims a statement of the sender's answers.
func collectMentionClaims(rows pgx.Rows) ([]mentionClaim, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionClaim, error) {
		var claim mentionClaim
		err := row.Scan(&claim.key.comment, &claim.key.target, &claim.key.attempt, &claim.claimedAt)
		return claim, err
	})
}

// releaseMentionClaims is the mention sender's release: one statement gives back the claims the
// sender recorded on the attempts it names, so an attempt no lane took, or none started, is
// pending and unclaimed. Each is scoped to that claim, so an attempt another sender has since
// taken keeps that sender's claim. A row that carries a claim a lost refresh wrote, which a failed
// re-read left unknown, is not given back: its claim lapses a lease after it was written, as a
// killed process's claims do, and the delivery sweep then takes the attempt.
func (s *server) releaseMentionClaims(ctx context.Context, held []mentionClaim) error {
	comments, targets, attempts, claimed := mentionClaimArrays(held)
	_, err := s.deps.Store.Pool.Exec(ctx, `
		update comment_deliveries d set claimed_at = null
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as held(comment_id, target, attempt, claimed_at)
		where d.comment_id = held.comment_id and d.target = held.target and d.attempt = held.attempt
		  and d.state = 'pending' and d.claimed_at = held.claimed_at
	`, comments, targets, attempts, claimed)
	return err
}

// claimQueuedMention takes the claim on an owed attempt a keyed repeat is about to put back on a
// lane (replayComment), so the delivery sweep leaves it to this process exactly as it leaves the
// attempt a comment of this process's own queued. ok is false when another sender holds it: that
// sender owns the send, and nothing is queued for it here.
func (s *server) claimQueuedMention(ctx context.Context, key mentionKey) (claimed time.Time, ok bool, err error) {
	err = s.deps.Store.Pool.QueryRow(ctx, `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and `+model.DeliveryClaimFreeSQL+`
		returning claimed_at
	`, key.comment, key.target, key.attempt).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return claimed, true, nil
}
