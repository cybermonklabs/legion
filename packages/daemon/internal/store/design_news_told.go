package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// What a task tells of news of the approved design (record.DeliveryNews, carried as
// supervise.Delivery.News and persisted as pending_task_deliveries.design_news) is taken by the
// audience it speaks to only when the turn that carries it starts. So the record of what each
// audience took (design_news_told, 20261113212423_design_news) rises in the write that records that
// delivery's confirmation, and nowhere else a delivery reaches: the write that queues it, sends it
// or rewrites it at a send raises nothing, and the write that retires it unconfirmed raises nothing,
// so news a retired task carried stays owed and a later start or the reconcile tells it again. The
// write that takes a confirmation back puts back what that confirmation raised. Each is the
// delivery's own write, in the caller's transaction, so no ordering of queues, confirmations,
// take-backs, retirements and restarts can leave a version raised that no started turn carried.
//
// The payload is read here in SQL, by its JSON names, as the kept decisions are read beside it
// (kept_decisions.go): this package imports no record, whose own tests read the claim store, so
// the shape is the one record.DeliveryNews writes and its doc names these readers.

// settleDesignNews raises or puts back what the delivery's news tells, and does so on two writes
// alone: the one that records the delivery's confirmation, and the one that takes it back. Which
// write this is, is read from the row the write is about to replace, as putBackARetakenConfirmation
// reads it: the write carries a confirmation and the row it replaces holds none — or there is no row
// — which is the write that records it; or the write carries none and the row it replaces holds
// one, which is a prompt refused busy after a turn of the agent's own confirmed it, a late refusal,
// or a turn its process died in, all of which send the task again. So it runs before the upsert,
// since afterwards nothing says what the write changed, and the read takes that row's own lock.
//
// Every other write changes no told row. A confirmed delivery written again — a write the machine
// owed, made at its next decision — raises nothing a second time, which matters for the opening: the
// gate can open again after the first write, and a second clear would lose the opening that later
// news owes. A task that carries no news has nothing to raise.
func settleDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery, news []byte) error {
	if len(news) == 0 {
		return nil
	}
	var confirmed bool
	switch err := tx.QueryRow(ctx, `select confirmed_at is not null from pending_task_deliveries
		where claim_token = $1 for update`, string(token)).Scan(&confirmed); {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read whether the delivery %s holds was confirmed: %w", token, err)
	}
	switch {
	case !d.ConfirmedAt.IsZero() && !confirmed:
		return tellDesignNews(ctx, tx, token, d.ID, news, d.ConfirmedAt)
	case d.ConfirmedAt.IsZero() && confirmed:
		return untellDesignNews(ctx, tx, token, d.ID, news)
	}
	return nil
}

// tellDesignNews is the confirmation's own write: the turn carrying this delivery has started, so
// each audience it spoke to took each version it told. Each item's step on its issue's audience row
// rises to the item's To, or stays where it stands when the row already holds a greater version,
// and the row is written where there is none (record.Postgres.RaiseTold, the same raise); each
// audience it carried the opening to is owed it no longer. An item of an issue the record does not
// hold raises nothing.
func tellDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, news []byte, at time.Time) error {
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		select raised.issue, raised.audience, jsonb_object_agg(raised.step, raised.version), false, $2::timestamptz
		from (select item.issue, item.audience, item.step, max(item."to") as version
			from jsonb_to_recordset(coalesce($1::jsonb -> 'items', '[]'::jsonb))
				as item (issue text, audience text, step text, "to" int)
			join issues owner on owner.key = item.issue
			group by item.issue, item.audience, item.step) raised
		group by raised.issue, raised.audience
		on conflict (issue, audience) do update set
			versions = design_news_told.versions || (
				select jsonb_object_agg(told.key,
					greatest(told.value::int, coalesce((design_news_told.versions ->> told.key)::int, told.value::int)))
				from jsonb_each_text(excluded.versions) told),
			told_at = excluded.told_at`, news, at); err != nil {
		return fmt.Errorf("raise what the delivery %s on %s told of the approved design: %w", delivery, token, err)
	}
	if _, err := tx.Exec(ctx, `update design_news_told set opening_owed = false
		from jsonb_to_recordset(coalesce($1::jsonb -> 'openings', '[]'::jsonb)) as opened (issue text, audience text)
		where design_news_told.issue = opened.issue and design_news_told.audience = opened.audience`, news); err != nil {
		return fmt.Errorf("clear the opening the delivery %s on %s carried: %w", delivery, token, err)
	}
	return nil
}

// untellDesignNews puts back what this delivery's confirmation raised, on the one write that takes
// that confirmation back: the agent never read the task, so the resend tells it again and its own
// confirmation raises it then. Each item's step goes back to the item's From where the row still
// holds exactly the item's To, and is forgotten where the item's From is unknown, which is what the
// row held before; a row that has risen past To since was raised by something else, and keeps what
// it holds. Each audience the delivery carried the opening to is owed it again.
func untellDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, news []byte) error {
	if _, err := tx.Exec(ctx, `with lowered as (
			select told.issue, told.audience,
				(told.versions - array_agg(item.step))
					|| coalesce(jsonb_object_agg(item.step, item."from") filter (where item."from" is not null), '{}'::jsonb) as versions
			from design_news_told told
			join jsonb_to_recordset(coalesce($1::jsonb -> 'items', '[]'::jsonb))
				as item (issue text, audience text, step text, "from" int, "to" int)
				on told.issue = item.issue and told.audience = item.audience and (told.versions ->> item.step)::int = item."to"
			group by told.issue, told.audience, told.versions)
		update design_news_told set versions = lowered.versions
		from lowered where design_news_told.issue = lowered.issue and design_news_told.audience = lowered.audience`,
		news); err != nil {
		return fmt.Errorf("put back what the delivery %s on %s told of the approved design: %w", delivery, token, err)
	}
	if _, err := tx.Exec(ctx, `insert into design_news_told (issue, audience, opening_owed)
		select distinct opened.issue, opened.audience, true
		from jsonb_to_recordset(coalesce($1::jsonb -> 'openings', '[]'::jsonb)) as opened (issue text, audience text)
		join issues owner on owner.key = opened.issue
		on conflict (issue, audience) do update set opening_owed = true`, news); err != nil {
		return fmt.Errorf("owe again the opening the delivery %s on %s carried: %w", delivery, token, err)
	}
	return nil
}

// designNews is a delivery's news as the column holds it and as the delivery carries it: a task
// that carries none holds no bytes at all, which the column holds as null, so a restored delivery
// equals the one that was written.
func designNews(news []byte) []byte {
	if len(news) == 0 || string(news) == "null" {
		return nil
	}
	return news
}
