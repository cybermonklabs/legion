package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// What a task tells of news of the approved design (record.DeliveryNews, carried as
// supervise.Delivery.News and persisted as pending_task_deliveries.design_news) is taken by the
// audience it speaks to only when the turn that carries it starts, and only as the prompt that
// turn answers carried it. The send renders the news again before each prompt, so a later prompt
// can carry more than an earlier one, and the turn that starts can be the earlier one's: a
// restart's turn of a prompt an earlier daemon sent, or the late turn of a prompt whose wait ran
// out while a later send was answered busy. So the machine keeps, beside the news, the copy the
// prompt whose turn its confirmation takes carried (supervise.Delivery.Carried, persisted as
// pending_task_deliveries.carried_news), written when a prompt is acknowledged, after its
// working-copy adoption and identity mint went through.
//
// The record of what each audience took (design_news_told, 20261114082752_design_news) rises from
// that copy in the write that records the confirmation, and nowhere else a delivery reaches: the
// write that queues it, sends it or rewrites it at a send raises nothing, and the write that
// retires it unconfirmed raises nothing, so news a retired task carried stays owed and a later
// start or the reconcile tells it again. The write that takes a confirmation back puts back what
// the row it replaces says that confirmation raised, and so does the retirement of a confirmed
// delivery whose copy is still held (RetireDelivery): the turn's result clears the copy once it
// says the turn ran, which is what makes the raise stand. Each is the delivery's own write, in the
// caller's transaction, so no ordering of queues, confirmations, take-backs, retirements and
// restarts can leave a version raised that no started turn's prompt carried.
//
// The confirmation is the machine's, and a turn the agent started by itself confirms a task that
// carries news only where a prompt of that task was acknowledged (supervise.Delivery.Carried):
// after a restart, the question whether the agent's turn is the task's is never asked of a task no
// acknowledged prompt carried (supervise's askFirstNeverTakes), so an Envoy message's turn records
// no news the agent never read, a prompt whose adoption or identity mint failed among them. A
// task sent twice, its acknowledgement lost to a crash before the copy was written, is the fault
// left, and it loses nothing.
//
// The payload is read here in SQL, by its JSON names, as the kept decisions are read beside it
// (kept_decisions.go): this package imports no record, whose own tests read the claim store, so
// the shape is the one record.DeliveryNews writes and its doc names these readers. The owed
// opening is settled through the SQL function design_news_opening_settled, the one rule every
// writer of it calls.

// settleDesignNews raises or puts back the news a prompt of the delivery carried, and does so on
// two writes alone: the one that records the delivery's confirmation, which raises carried (the
// write's own copy, supervise.Delivery.Carried), and the one that takes it back, which puts back
// what the row it replaces holds (replaced.carried). Which write this is, is read from the row the
// write is about to replace (putDelivery's replacesConfirmed), as putBackARetakenConfirmation
// reads it: the write carries a confirmation and the row it replaces holds none — or there is no
// row — which is the write that records it; or the write carries none and the row it replaces
// holds one, which is a prompt refused busy after a turn of the agent's own confirmed it, a late
// refusal, a provider error, or a turn its process died in, all of which send the task again. The
// read runs before the upsert, since afterwards nothing says what the write changed, and it takes
// that row's own lock.
//
// Every other write changes no told row. A confirmed delivery written again — a write the machine
// owed, made at its next decision, or the one that clears the copy once the turn's result came —
// raises nothing a second time, which matters for the opening: the gate can open again after the
// first write, and a second clear would lose the opening that later news owes.
func settleDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery, carried []byte, replaced replacedDelivery) error {
	switch {
	case !d.ConfirmedAt.IsZero() && !replaced.confirmed:
		return tellDesignNews(ctx, tx, token, d.ID, carried, d.ConfirmedAt)
	case d.ConfirmedAt.IsZero() && replaced.confirmed:
		return untellDesignNews(ctx, tx, token, d.ID, replaced.carried)
	}
	return nil
}

// tellDesignNews is the confirmation's own write: the turn answering the prompt that carried news
// has started, so each audience it spoke to took each version it told. Each item's step on its
// issue's audience row rises to the item's To, or stays where it stands when the row already holds
// a greater version, and the row is written where there is none, by the one raise both raises call
// (the SQL function design_news_raised, which record.Postgres.RaiseTold calls too). Each opening
// the prompt said opened is owed no longer, where the row still owes that same opening (its Seq,
// through design_news_opening_settled): a close and a reopening since the prompt was rendered owe
// a new opening, which this confirmation leaves owed. An opening the prompt said closed clears
// nothing. An item of an issue the record does not hold raises nothing, and a prompt that carried
// no news raises nothing at all.
func tellDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, carried []byte, at time.Time) error {
	if len(carried) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		select raised.issue, raised.audience, jsonb_object_agg(raised.step, raised.version), false, $2::timestamptz
		from (select item.issue, item.audience, item.step, max(item."to") as version
			from jsonb_to_recordset(coalesce($1::jsonb -> 'items', '[]'::jsonb))
				as item (issue text, audience text, step text, "to" int)
			join issues owner on owner.key = item.issue
			group by item.issue, item.audience, item.step) raised
		group by raised.issue, raised.audience
		on conflict (issue, audience) do update set
			versions = design_news_raised(design_news_told.versions, excluded.versions),
			told_at = excluded.told_at`, carried, at); err != nil {
		return fmt.Errorf("raise what the delivery %s on %s told of the approved design: %w", delivery, token, err)
	}
	if err := settleCarriedOpenings(ctx, tx, carried, false); err != nil {
		return fmt.Errorf("clear the opening the delivery %s on %s carried: %w", delivery, token, err)
	}
	return nil
}

// untellDesignNews puts back what a confirmation of this delivery raised, from the copy that
// confirmation raised (carried), on the one write that takes the confirmation back and on the
// retirement of a confirmed delivery whose turn's result never said the turn ran (RetireDelivery):
// the agent may never have read the task, so the resend or a later task tells it again and its own
// confirmation raises it then. Each item's step goes back to the item's From where the row still
// holds exactly the item's To, and is forgotten where the item's From is unknown, which is what the
// row held before; a row that has risen past To since was raised by something else, and keeps what
// it holds. Each opening the copy said opened is owed again where the row still holds that same
// opening (its Seq, through design_news_opening_settled): a close, a reopening or an opened role
// message since then settled it otherwise, and that settlement stands.
//
// The new versions are computed from the row the update has locked, in the one statement, as every
// other writer of a told row computes its write: the SET and the WHERE read the target row, never a
// value read before the lock. Under READ COMMITTED a fill, a raise or another delivery's
// confirmation of the same row that commits while this write waits for the row's lock is then
// read, since Postgres evaluates both again on the version of the row that write committed, and
// what that write wrote is kept. The items are read out of the news once, by the statement's
// `item` query, which reads the news alone and no row.
func untellDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, carried []byte) error {
	if len(carried) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `with item as (
			select * from jsonb_to_recordset(coalesce($1::jsonb -> 'items', '[]'::jsonb))
				as carried (issue text, audience text, step text, "from" int, "to" int))
		update design_news_told told set versions = (
			select (told.versions - array_agg(item.step))
				|| coalesce(jsonb_object_agg(item.step, item."from") filter (where item."from" is not null), '{}'::jsonb)
			from item
			where item.issue = told.issue and item.audience = told.audience and (told.versions ->> item.step)::int = item."to")
		where exists (select 1 from item
			where item.issue = told.issue and item.audience = told.audience and (told.versions ->> item.step)::int = item."to")`,
		carried); err != nil {
		return fmt.Errorf("put back what the delivery %s on %s told of the approved design: %w", delivery, token, err)
	}
	if err := settleCarriedOpenings(ctx, tx, carried, true); err != nil {
		return fmt.Errorf("owe again the opening the delivery %s on %s carried: %w", delivery, token, err)
	}
	return nil
}

// settleCarriedOpenings settles each opening the copy says opened (owed false at its
// confirmation, true where that confirmation is taken back) through the one rule every writer of
// the owed opening calls, design_news_opening_settled, under the number the copy rendered it under:
// the rule acts only on a row that still holds that number.
func settleCarriedOpenings(ctx context.Context, tx pgx.Tx, carried []byte, owed bool) error {
	_, err := tx.Exec(ctx, `select design_news_opening_settled(opened.issue, opened.audience, $2, opened.seq)
		from jsonb_to_recordset(coalesce($1::jsonb -> 'openings', '[]'::jsonb))
			as opened (issue text, audience text, seq int, closed boolean)
		where not coalesce(opened.closed, false)`, carried, owed)
	return err
}

// designNews is a delivery's news as the column holds it and as the delivery carries it: a task
// that carries none holds no bytes at all, which the column holds as null, so a restored delivery
// equals the one that was written. The copy a prompt carried (carried_news) is held the same way.
func designNews(news []byte) []byte {
	if len(news) == 0 || string(news) == "null" {
		return nil
	}
	return news
}
