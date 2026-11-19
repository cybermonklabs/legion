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
// That copy, on a confirmed delivery, is the delivery's credit, and it is held nowhere else: the
// record of what each audience took (design_news_told, 20261119203047_design_news) is read as a
// function of the told rows and of every credit that stands (the SQL function
// design_news_told_in), so no write raises or puts back a told version for a confirmation. The
// write that records the confirmation makes the credit by writing the delivery; the write that
// takes it back, a second confirmation that replaces its copy, and the retirement of the delivery
// end it by writing or deleting the delivery, and touch no credit of any other delivery, whatever
// has been credited since. A write that fails leaves the row, and with it the credit, as the last
// write that landed left it, which the machine's next write of the same delivery finishes or
// undoes. Once the turn's result says the turn ran (supervise.Delivery.Ran), the write that records
// it folds the copy into the told rows (settleDesignNews), so the credit is final and outlives the
// delivery's row.
//
// The confirmation is the machine's, and a turn the agent started by itself confirms a task that
// carries news only where a prompt of that task was acknowledged (supervise.Delivery.Carried):
// after a restart, the question whether the agent's turn is the task's is never asked of a task no
// acknowledged prompt carried (supervise's askFirstNeverTakes), so an Envoy message's turn records
// no news the agent never read, a prompt whose adoption or identity mint failed among them. A
// task sent twice, its acknowledgement lost to a crash before the copy was written, is the fault
// left, and it loses nothing.
//
// The payload is read in SQL, by its JSON names, as the kept decisions are read beside it
// (kept_decisions.go): this package imports no record, whose own tests read the claim store, so
// the shape is the one record.DeliveryNews writes and its doc names these readers. The copy is read
// as credits by two SQL functions alone, design_news_credits for its items and design_news_openings
// for the openings it says opened, which the read of a standing credit (design_news_told_in) and
// the fold here both call. Each counts an item or an opening only while the record still holds the
// run it was rendered in (design_news_current: record.DeliveryNewsItem.Generation and
// TreeGeneration), so a copy of a run that a clear of a generation ended tells the next run
// nothing. A final credit is folded through the SQL function design_news_folded, the one write
// record.Postgres.RaiseTold makes too, and its openings through design_news_opening_told, the
// delivery's settlement of the owed opening.

// settleDesignNews folds a delivery's final credit into the told rows: on a write of a confirmed
// delivery whose turn's result said the turn ran (supervise.Delivery.Ran), what its copy carried
// (carried, the write's own) is folded (foldDesignNews). Every other write folds nothing, since a
// standing credit is the delivery's own row and is read from it (design_news_told_in). Folding
// the same copy again changes nothing, so a final delivery written again — the repair of a write
// that failed, or any later write of it before its retirement — needs no reading of the row it
// replaces.
func settleDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, d supervise.Delivery, carried []byte) error {
	if d.ConfirmedAt.IsZero() || !d.Ran || len(carried) == 0 {
		return nil
	}
	return foldDesignNews(ctx, tx, token, d.ID, carried, d.ConfirmedAt)
}

// foldDesignNews folds what carried told into the told rows: the credits of each audience it spoke
// to (design_news_credits), each counted from its item's From (design_news_folded, which writes the
// row where there is none), and each opening it said opened (design_news_openings) as owed no
// longer where the row still holds that same opening (its Seq, design_news_opening_told). An
// opening it said closed clears nothing, an item or an opening of a run the record no longer holds
// (design_news_current) folds nothing, and a copy that carried no news folds nothing.
func foldDesignNews(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, carried []byte, at time.Time) error {
	if _, err := tx.Exec(ctx, `select design_news_folded(credit.issue, credit.audience, jsonb_agg(credit.link), $2::timestamptz)
		from design_news_credits($1::jsonb) credit
		group by credit.issue, credit.audience`, carried, at); err != nil {
		return fmt.Errorf("fold what the delivery %s on %s told of the approved design: %w", delivery, token, err)
	}
	if _, err := tx.Exec(ctx, `select design_news_opening_told(opened.issue, opened.audience, opened.seq)
		from design_news_openings($1::jsonb) opened`, carried); err != nil {
		return fmt.Errorf("clear the opening the delivery %s on %s carried: %w", delivery, token, err)
	}
	return nil
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
