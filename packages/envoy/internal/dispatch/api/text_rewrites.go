package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/reviewer"
)

// text_rewrites.go reads the `rewrite` every read of an ask, a comment, a message and an issue
// carries (model.TextRewrite, INFRA-3231's Decision 23). It is read beside the rest of what a
// read attaches, never by a write and never into an event payload:
//
//   - An ask's comes from its newest ask.edited, where that edit is a rewrite (a person's Rewrite,
//     or the reviewer's rewrite of a decision block), else from its held_texts row where the
//     reviewer held it and it posted rewritten; an edit after the rewrite that is no rewrite of
//     its own leaves the wording the agent's again, and the ask carries no rewrite. A person's
//     Rewrite that runs, or failed after the wording was last written, reads from ask_rewrites:
//     rewriting while its run is younger than askRewriteBound, failed after.
//   - A comment's, a message's and an issue's come from their held_texts row alone, where the
//     reviewer held the text and it posted rewritten; a comment edited after its post carries
//     none.

// askRewriteBound is how long a person's Rewrite may run before a read takes it for one that
// stopped, and a new request replaces it: the side question's wait, two model calls' bound (the
// detector's and the rewrite's) and a minute more.
const askRewriteBound = defaultSideQuestionWait + 2*reviewer.DefaultTimeout + time.Minute

// askRewriteStopped is the reason a run a stopped process left reads with.
const askRewriteStopped = "the rewrite did not finish"

// askRewritesStatement reads, $1 the ask ids and $2 askRewriteBound, each ask's newest ask.edited
// (the partial index events_ask_payload_id serves it), its held_texts row where it posted
// rewritten, and its ask_rewrites row, for the asks that have any of the three.
const askRewritesStatement = `
	select k.id::text, e.created_at, e.payload->'previous', e.payload->'rewrite',
		h.posted_at, h.original, h.findings,
		r.state, r.started_at, r.finished_at, r.original, coalesce(r.reason, ''),
		coalesce(r.state = 'rewriting' and r.started_at > now() - $2::interval, false)
	from unnest($1::uuid[]) as k(id)
	left join lateral (
		select created_at, payload from events
		where type = 'ask.edited' and payload->>'id' = k.id::text
		order by id desc limit 1
	) e on true
	left join held_texts h on h.id = k.id::text and h.kind = 'ask' and h.state = 'posted' and h.outcome = 'rewrite_posted'
	left join ask_rewrites r on r.ask_id = k.id
	where e.payload ? 'rewrite' or h.id is not null or r.ask_id is not null`

// queueAskRewrites queues the read of the asks' rewrites in b, into into by ask id.
func queueAskRewrites(b *pgx.Batch, askIDs []string, into map[string]model.TextRewrite) {
	if len(askIDs) == 0 {
		return
	}
	b.Queue(askRewritesStatement, askIDs, intervalOf(askRewriteBound)).Query(func(rows pgx.Rows) error {
		return scanAskRewrites(rows, into)
	})
}

// attachAskRewrites reads the asks' rewrites in one query on q and stamps each ask with its own.
func attachAskRewrites(ctx context.Context, q queryer, asks []*model.Ask) error {
	askIDs := distinctAskIDs(asks)
	if len(askIDs) == 0 {
		return nil
	}
	rewrites := map[string]model.TextRewrite{}
	rows, err := q.Query(ctx, askRewritesStatement, askIDs, intervalOf(askRewriteBound))
	if err != nil {
		return err
	}
	if err := scanAskRewrites(rows, rewrites); err != nil {
		return err
	}
	applyAskRewrites(asks, rewrites)
	return nil
}

// applyAskRewrites stamps each ask with its rewrite, where it has one.
func applyAskRewrites(asks []*model.Ask, rewrites map[string]model.TextRewrite) {
	for _, ask := range asks {
		if rewrite, held := rewrites[ask.ID]; held {
			ask.Rewrite = &rewrite
		}
	}
}

func scanAskRewrites(rows pgx.Rows, into map[string]model.TextRewrite) error {
	defer rows.Close()
	for rows.Next() {
		var row askRewriteRow
		if err := rows.Scan(&row.id, &row.editedAt, &row.previous, &row.rewrite, &row.postedAt, &row.heldOriginal,
			&row.findings, &row.state, &row.startedAt, &row.finishedAt, &row.runOriginal, &row.reason, &row.fresh); err != nil {
			return err
		}
		rewrite, err := row.textRewrite()
		if err != nil {
			return err
		}
		if rewrite != nil {
			into[row.id] = *rewrite
		}
	}
	return rows.Err()
}

// askRewriteRow is one row of askRewritesStatement.
type askRewriteRow struct {
	id                    string
	editedAt              *time.Time
	previous, rewrite     []byte
	postedAt              *time.Time
	heldOriginal          []byte
	findings              []byte
	state                 *string
	startedAt, finishedAt *time.Time
	runOriginal           []byte
	reason                string
	fresh                 bool
}

// textRewrite is the ask's rewrite as its three sources give it, nil for none.
func (row askRewriteRow) textRewrite() (*model.TextRewrite, error) {
	var (
		rewritten *model.TextRewrite
		// wordingAt is when an edit last wrote the ask's wording, zero where none did.
		wordingAt time.Time
	)
	switch {
	case row.editedAt != nil:
		wordingAt = *row.editedAt
		if len(row.rewrite) > 0 && string(row.rewrite) != "null" {
			var (
				record   model.AskRewriteEdit
				previous model.AskEditPrevious
			)
			if err := json.Unmarshal(row.rewrite, &record); err != nil {
				return nil, fmt.Errorf("read the rewrite of ask %s: %w", row.id, err)
			}
			if err := json.Unmarshal(row.previous, &previous); err != nil {
				return nil, fmt.Errorf("read the original of ask %s: %w", row.id, err)
			}
			rewritten = &model.TextRewrite{
				State: model.RewriteRewritten, By: record.By, At: row.editedAt.UTC(), Reason: record.Reason,
				Original: model.TextWording{Body: previous.Question, Options: previous.Options, Multiple: previous.Multiple},
			}
		}
	case row.postedAt != nil:
		held, err := heldRewrite(row.id, row.heldOriginal, row.findings, *row.postedAt)
		if err != nil {
			return nil, err
		}
		rewritten = &held
	}
	if row.state == nil || row.startedAt == nil {
		return rewritten, nil
	}
	run := model.TextRewrite{By: model.RewriteByPerson}
	if err := json.Unmarshal(row.runOriginal, &run.Original); err != nil {
		return nil, fmt.Errorf("read the original of ask %s's rewrite: %w", row.id, err)
	}
	switch {
	case *row.state == model.RewriteRewriting && row.fresh:
		run.State, run.At = model.RewriteRewriting, row.startedAt.UTC()
		return &run, nil
	case *row.state == model.RewriteRewriting && row.startedAt.After(wordingAt):
		run.State, run.At, run.Reason = model.RewriteFailed, row.startedAt.Add(askRewriteBound).UTC(), askRewriteStopped
		return &run, nil
	case *row.state == model.RewriteFailed && row.finishedAt != nil && row.finishedAt.After(wordingAt):
		run.State, run.At, run.Reason = model.RewriteFailed, row.finishedAt.UTC(), row.reason
		return &run, nil
	}
	return rewritten, nil
}

// heldRewrite is the rewrite of the held text id that posted rewritten: the reviewer's, at its
// post, with the held wording as the original and the reviewer's findings as why.
func heldRewrite(id string, original, findings []byte, postedAt time.Time) (model.TextRewrite, error) {
	var (
		text  reviewer.Text
		found []reviewer.Finding
	)
	if err := json.Unmarshal(original, &text); err != nil {
		return model.TextRewrite{}, fmt.Errorf("read held text %s's original: %w", id, err)
	}
	if err := json.Unmarshal(findings, &found); err != nil {
		return model.TextRewrite{}, fmt.Errorf("read held text %s's findings: %w", id, err)
	}
	return model.TextRewrite{
		State: model.RewriteRewritten, By: model.RewriteByReviewer, At: postedAt.UTC(),
		Original: wordingOf(text), Reason: findingsReason(found),
	}, nil
}

// heldRewritesStatement reads, $1 a held text's kind and $2 ids, each text of the kind the
// reviewer held that posted rewritten.
const heldRewritesStatement = `
	select id, original, findings, posted_at from held_texts
	where kind = $1 and id = any($2::text[]) and state = 'posted' and outcome = 'rewrite_posted'`

// queueHeldRewrites queues the read of the rewrites of the texts of kind ids name in b, into into
// by id.
func queueHeldRewrites(b *pgx.Batch, kind reviewer.Kind, ids []string, into map[string]model.TextRewrite) {
	if len(ids) == 0 {
		return
	}
	b.Queue(heldRewritesStatement, string(kind), ids).Query(func(rows pgx.Rows) error {
		return scanHeldRewrites(rows, into)
	})
}

// heldRewrites reads the rewrites of the texts of kind ids name in one query on q, by id.
func heldRewrites(ctx context.Context, q queryer, kind reviewer.Kind, ids []string) (map[string]model.TextRewrite, error) {
	rewrites := map[string]model.TextRewrite{}
	if len(ids) == 0 {
		return rewrites, nil
	}
	rows, err := q.Query(ctx, heldRewritesStatement, string(kind), ids)
	if err != nil {
		return nil, err
	}
	return rewrites, scanHeldRewrites(rows, rewrites)
}

func scanHeldRewrites(rows pgx.Rows, into map[string]model.TextRewrite) error {
	defer rows.Close()
	for rows.Next() {
		var (
			id                string
			original, finding []byte
			postedAt          time.Time
		)
		if err := rows.Scan(&id, &original, &finding, &postedAt); err != nil {
			return err
		}
		rewrite, err := heldRewrite(id, original, finding, postedAt)
		if err != nil {
			return err
		}
		into[id] = rewrite
	}
	return rows.Err()
}

// attachCommentRewrites stamps each comment the reviewer held and posted rewritten with its
// rewrite, unless the comment was edited after its post.
func attachCommentRewrites(ctx context.Context, q queryer, comments []model.Comment) error {
	ids := make([]string, len(comments))
	for i := range comments {
		ids[i] = comments[i].ID
	}
	rewrites, err := heldRewrites(ctx, q, reviewer.KindComment, ids)
	if err != nil {
		return err
	}
	for i := range comments {
		rewrite, held := rewrites[comments[i].ID]
		if !held || editedAfter(comments[i].EditedAt, rewrite.At) {
			continue
		}
		comments[i].Rewrite = &rewrite
	}
	return nil
}

// editedAfter reports whether editedAt, a row's edited_at as a read carries it, is after at.
func editedAfter(editedAt *string, at time.Time) bool {
	if editedAt == nil {
		return false
	}
	edited, err := time.Parse(time.RFC3339Nano, *editedAt)
	return err == nil && edited.After(at)
}

// attachMessageRewrites stamps each message the reviewer held and posted rewritten with its
// rewrite: every root of roots and every reply of replies.
func attachMessageRewrites(ctx context.Context, q queryer, roots []model.Message, replies map[string][]model.Message) error {
	var ids []string
	for _, root := range roots {
		ids = append(ids, root.ID)
		for _, reply := range replies[root.ID] {
			ids = append(ids, reply.ID)
		}
	}
	rewrites, err := heldRewrites(ctx, q, reviewer.KindMessage, ids)
	if err != nil || len(rewrites) == 0 {
		return err
	}
	stamp := func(message *model.Message) {
		if rewrite, held := rewrites[message.ID]; held {
			message.Rewrite = &rewrite
		}
	}
	for i := range roots {
		stamp(&roots[i])
		for j := range replies[roots[i].ID] {
			stamp(&replies[roots[i].ID][j])
		}
	}
	return nil
}
