package api

// status_change_recovery.go is the recovery point of a keyed status change (status_change.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The members of a status change's recovery point (statusChangeRecovery), each named here once:
// every statement that writes the point or reads it names them by these constants, and each field
// of statusChangeRecovery has its member's constant as its json tag, which
// TestEachFieldOfAStatusChangesRecoveryPointIsTaggedWithItsMember holds.
const (
	// pointChanged is the entry of each step that wrote, which recoveryPointRead gathers from
	// request_key_steps.
	pointChanged = "changed"
	// pointEvents is the ids of the events the steps appended on the issue the call names.
	pointEvents = "events"
	// pointWithdrawn is the ids of the asks the named issue's own close withdrew.
	pointWithdrawn = "withdrawn"
	// pointMoved is what each step of an option changed, which the comment on the named issue
	// lists, and which recoveryPointRead gathers from request_key_steps.
	pointMoved = "moved"
	// pointFinished is the mark that every step of the call ran.
	pointFinished = "finished"
	// pointIssue is the answer's issue, written with the mark.
	pointIssue = "issue"
)

// statusChangeRecovery is the recovery point of a keyed status change (request_keys.go): what
// the steps that committed made, from which a repeat finishes the call. Each step that writes adds
// to it in its own transaction, right after its write phase, with a statement that builds the
// head's members in its SQL and adds its entry, and what a step of an option changed, as a row of
// request_key_steps (queueStatusChangeStep), and the call marks it finished, with the answer's
// issue, once its last step has run, in the transaction that writes the comment on the named issue
// (markStatusChangeFinished). Every reader reads it with the steps' entries under changed and
// their moves under moved (recoveryPointRead). Each field's json tag is the name of its member
// above, so every json.Unmarshal of the point (resumedStatusChange, refuseStatusChangeKey,
// replayRecovered) names each member as those statements do, and a member the point does not hold
// stays empty.
type statusChangeRecovery struct {
	// Changed is the entry of each step that wrote, in the order the steps committed, each with
	// its issue as its step's write answered it, which every answer from the point carries.
	Changed []statusChangeEntry `json:"changed"`
	// Events is the ids of every event the steps appended on the issue the call names, which the
	// fence of a resumed attempt counts as the call's own.
	Events []int64 `json:"events"`
	// Withdrawn is the ids of the asks that the close of the issue the call names withdrew, which
	// that close's ask sweep takes.
	Withdrawn []string `json:"withdrawn"`
	// Moved is what each step of an option changed, in the order the steps committed, which the
	// comment on the issue the call names lists (statusChangeRun.namedComment).
	Moved []statusChangeMove `json:"moved"`
	// Finished is the mark that every step of the call ran.
	Finished bool `json:"finished"`
	// Issue is the answer's issue (statusChangeRun.namedIssue), written with the mark, which a
	// replay answers.
	Issue *model.Issue `json:"issue,omitempty"`
}

// statusChangePointIssues is the keys of every issue a status change's recovery point names, which
// its refusal (refuseStatusChangeKey) and its replay (replayStatusChange) answer: the issue of each
// step's entry and of each move, and the answer's issue, each once, in the order the point holds
// them (keyedRoute.pointIssues).
func statusChangePointIssues(recovery json.RawMessage) ([]string, error) {
	var point statusChangeRecovery
	if err := json.Unmarshal(recovery, &point); err != nil {
		return nil, fmt.Errorf("read the recovery point of a status change: %w", err)
	}
	keys := make([]string, 0, len(point.Changed)+len(point.Moved)+1)
	add := func(key string) {
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, entry := range point.Changed {
		add(entry.Key)
	}
	for _, move := range point.Moved {
		add(move.Key)
	}
	if point.Issue != nil {
		add(point.Issue.Key)
	}
	return keys, nil
}

// holds reports whether the point holds the entry of key's step, which counts that step as made.
// A nil point holds none.
func (point *statusChangeRecovery) holds(key string) bool {
	return point != nil && slices.ContainsFunc(point.Changed, func(entry statusChangeEntry) bool { return entry.Key == key })
}

// closes reports whether the point holds the close of named, the issue the call names, by its own
// step: an entry of named at done, which a step writes only when it closes the issue, since the
// PATCH's gate refuses done on an issue already closed.
func (point *statusChangeRecovery) closes(named string) bool {
	return point != nil && slices.ContainsFunc(point.Changed, func(entry statusChangeEntry) bool {
		return entry.Key == named && entry.Status == "done"
	})
}

// fence is the fence of a call that sent expected_sequence sequence, with every event the point
// holds counted as the call's own. A nil point holds none.
func (point *statusChangeRecovery) fence(sequence int64) *issueWriteFence {
	own := map[int64]bool{}
	if point != nil {
		for _, id := range point.Events {
			own[id] = true
		}
	}
	return &issueWriteFence{sequence: sequence, own: own}
}

// resumedStatusChange is the recovery point that an earlier attempt of the keyed request ctx
// carries left, when this attempt took that request's key over (claimRequestKey), with the steps'
// entries (recoveryPointRead): the call this attempt finishes. It is nil for every other request:
// one with no key, a first attempt, and an attempt that took over a key with no point, which runs
// the call as a first attempt does.
func (s *server) resumedStatusChange(ctx context.Context) (*statusChangeRecovery, error) {
	claim, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim)
	if !keyed || claim.attempt == 1 {
		return nil, nil
	}
	pool, err := s.readyPool()
	if err != nil {
		return nil, err
	}
	var recovery json.RawMessage
	err = pool.QueryRow(ctx, `
		select `+recoveryPointRead+` from request_keys where login = $1 and idempotency_key = $2 and attempt = $3
	`, claim.login, claim.key, claim.attempt).Scan(&recovery)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errRequestKeyTakenOver
	}
	if err != nil {
		return nil, fmt.Errorf("read the recovery point of a status change: %w", err)
	}
	if recovery == nil {
		return nil, nil
	}
	var point statusChangeRecovery
	if err := json.Unmarshal(recovery, &point); err != nil {
		return nil, fmt.Errorf("read the recovery point of a status change: %w", err)
	}
	return &point, nil
}

// markStatusChangeFinished writes in tx, the transaction that finishes the call
// (statusChangeRun.finish), into the recovery point of the keyed status change ctx carries the mark
// that every step of the call ran, with issue, the answer's issue, once the last step has run and
// before the call answers. A repeat that finds the mark is answered from the point, with that
// issue, and never takes the key over. Where tx writes the mark alone, its only write is to
// request_keys, which the commit step does not count (markRequestKeyCommitted); where it writes
// the comment on the named issue too, that step marks the key committed with it. The statement is
// scoped to the request's own claim, so an attempt whose key another attempt took over is refused
// (errRequestKeyTakenOver), and tx then rolls back with the comment. A request with no key marks
// nothing.
func markStatusChangeFinished(ctx context.Context, tx pgx.Tx, issue model.Issue) error {
	claim, keyed := ctx.Value(requestKeyClaimKey{}).(requestKeyClaim)
	if !keyed {
		return nil
	}
	encoded, err := json.Marshal(issue)
	if err != nil {
		return fmt.Errorf("mark a status change finished: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		update request_keys set recovery = recovery || jsonb_build_object('`+pointFinished+`', true, '`+pointIssue+`', $4::jsonb)
		where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
	`, claim.login, claim.key, claim.attempt, encoded)
	if err != nil {
		return fmt.Errorf("mark a status change finished: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errRequestKeyTakenOver
	}
	return nil
}

// queueStatusChangeStep queues on b, the batch that runs right after the write phase of one step
// of a keyed status change (docs.Ledger.AfterWrites), the statement that writes the step into the
// call's recovery point, under the request key's claim the step carries (step.claim). It adds to
// the head the key holds, which is the call's point so far, since only the attempt that holds the
// key writes it: the ids of the events on the issue the call names (step.named) after
// namedLastSeq, its last_seq as the step read it under that issue's owner lock, when the step holds
// that lock (holdsNamed), and withdrawn, the ids of the asks that the named issue's own close
// withdrew. It adds a row of request_key_steps that holds entry, the step's entry with its issue as
// the step's write answered it, and moved, what a step of an option changed (null for the named
// issue's own step), so no step rewrites the moves of the steps before it. The event ids exist, and
// the issue's last_seq and a close's closed_at are known, only once the batch has run, so the
// statement derives the ids in its own SQL, after the step's last event insert, and its callback
// reads them into events, the step's own events on the named issue (issueWriteResult.namedEvents).
// The statement is scoped to the request's own claim, so a step whose key another attempt took over
// rolls back (errRequestKeyTakenOver).
func queueStatusChangeStep(
	b *store.WriteBatch, step *issueWriteCallStep, entry statusChangeEntry, moved *statusChangeMove, namedLastSeq int,
	holdsNamed bool, withdrawn []string, events *[]int64,
) error {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	var encodedMove any
	if moved != nil {
		if encodedMove, err = json.Marshal(*moved); err != nil {
			return err
		}
	}
	claim := step.claim
	b.Queue("write the recovery point of a status change", `
		with named as (
			select coalesce(array_agg(id order by id), '{}') as ids from events
			where $4 and issue_key = $5 and seq > $6
		), head as (
			update request_keys set recovery = jsonb_build_object(
				'`+pointEvents+`', coalesce(recovery -> '`+pointEvents+`', '[]') || to_jsonb(named.ids),
				'`+pointWithdrawn+`', coalesce(recovery -> '`+pointWithdrawn+`', '[]') || coalesce(to_jsonb($8::text[]), '[]'))
			from named
			where login = $1 and idempotency_key = $2 and attempt = $3 and status is null
			returning named.ids
		), step as (
			insert into request_key_steps (login, idempotency_key, entry, moved) select $1, $2, $7::jsonb, $9::jsonb from head
		)
		select ids from head
	`, claim.login, claim.key, claim.attempt, holdsNamed, step.named, namedLastSeq, encoded, withdrawn, encodedMove,
	).QueryRow(func(row pgx.Row) error {
		err := row.Scan(events)
		if errors.Is(err, pgx.ErrNoRows) {
			return errRequestKeyTakenOver
		}
		return err
	})
	return nil
}

// replayStatusChange answers a repeat of a keyed status change from its recovery point: a call
// whose point carries the mark, or one that answered a success beside its point. It answers the
// 200 of the call with what the point stores, the entries as their steps answered them and the
// answer's issue the mark was written with, and reads nothing. No step and no fence runs again, so
// a later write of an issue the call changed, its close's ask sweep among them, changes no answer
// of a call that finished. A point with no answer's issue is a fault of the server's own, since
// every finished point this build writes holds one, and is answered 500.
func (s *server) replayStatusChange(w http.ResponseWriter, _ *http.Request, point statusChangeRecovery, _ []byte) {
	if point.Issue == nil {
		s.writeStatusChangeRefusal(w, errors.New("the recovery point of a finished status change holds no answer's issue"), point.Changed)
		return
	}
	WriteJSON(w, http.StatusOK, statusChangeAnswer{Issue: *point.Issue, Changed: point.Changed})
}
