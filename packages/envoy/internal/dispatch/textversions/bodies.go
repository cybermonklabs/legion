package textversions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// wholeBody gives v, a version of a post whose event e cut its body, the whole body from the
// first source that holds it (decideVersions): the post's present row where no later event of
// the post changes its text (presentBody); then the post's body history, the body the first such
// later event kept (historyBody); then, for a comment.created, revision 1 of its blocks
// (firstRevisionBody). Else it keeps the event's text (bodyFromCut), which complete warns of
// where it keeps v.
//
// The history comes before the blocks because it is exact: it is the body the later edit or
// delete replaced, byte for byte, which is the body e wrote since no change of the post's text
// comes between them, where revision 1 of the blocks drops the white space that ends each block.
// The blocks stay for a comment whose later change kept no history, as a build before the body
// history wrote its edits.
func (d *decider) wholeBody(ctx context.Context, e event, v *version) error {
	body, found, err := presentBody(ctx, d.tx, e, v.Kind, v.ID)
	if err != nil {
		return err
	}
	if found {
		v.Body, v.BodyFrom = body, bodyFromRow
		return nil
	}
	if body, found, err = historyBody(ctx, d.tx, e, v.Kind, v.ID); err != nil {
		return err
	}
	if found {
		v.Body, v.BodyFrom = body, bodyFromHistory
		return nil
	}
	if e.Type == "comment.created" {
		body, ok, err := firstRevisionBody(ctx, d.tx, v.ID, e.CreatedAt)
		if err != nil {
			return err
		}
		if ok {
			v.Body, v.BodyFrom = body, bodyFromBlocks
			return nil
		}
	}
	v.BodyFrom = bodyFromCut
	return nil
}

// presentBodyQuery reads the body of post $1 from its present row in the table its kind names
// (%s), where no event after the one read, of the event's owner (issue $2, or document $3 for a
// comment on a document of no issue) at a seq above that event's ($4), is of a type that changes
// the post's text ($5, the kind's later types in postBodyReads) and names the post: the row and
// the later events are read in the statement's one snapshot, so a row that answers holds the
// text the event wrote, and no row answers where a later event changes it or the post has no row.
// The later events are read through the owner's unique (issue_key, seq) or (artifact_id, seq)
// index, from the event's seq up, not through the log: an owner's events take their ids and their
// seqs in the same order, since each append takes the owner's lock before either and holds it
// until its transaction ends (events.Broker), so they are the owner's events whose id is above
// the event's.
//
// The payload of a later event is read only where its type is one of $5, which the CASE holds
// whatever plan Postgres runs: the fill's connection caches the statement, and the plan Postgres
// may then keep for any arguments (a generic plan) can test a condition on the payload before one
// on the type, and so unpack the stored payload of each later event of the owner.
const presentBodyQuery = `
	select p.body from public.%s p
	 where p.id = $1::text::uuid
	   and not exists (
	         select from public.events l
	          where (l.issue_key = $2 and l.seq > $4 or l.artifact_id = $3::uuid and l.seq > $4)
	            and case when l.type = any($5::text[]) then l.payload->>'id' = $1 end)`

// historyBodyQuery reads, from the body history of post $1 (post_body_history, its column %s),
// the body the first event of the post after the one read that changes its text kept: the body
// that held from the event read until that change, since no other change comes between them. The
// arguments are presentBodyQuery's: the event's owner ($2 or $3), its seq ($4) and the types that
// change the post's text ($5), whose payload alone is read, under the same CASE.
//
// No column ties a history row to the event that kept it: the edit or the delete writes both in
// one transaction (api's postBodyHistoryStep and the event it appends), the row's edited_at is
// its insert's clock_timestamp() and the event's created_at the transaction's start, and two
// writes of one post can begin in either order and wait on the owner's lock, so neither time
// bounds the other. Their order does: each such write takes the post's owner's lock before it
// draws the event's seq and the row's id and holds it until it commits, so the post's events of
// these types, by seq, and its history rows, by id (an identity, drawn in the order the inserts
// ran), are in the order of their transactions. The n-th of the events kept the n-th row, where
// every one of them kept one. A comment.edited a build before the body history wrote kept none,
// so the statement answers only where the post's events of these types and its history rows are
// as many, counted in its one snapshot, in which a write's event and its row are both visible or
// neither is; with fewer rows it answers none, rather than a row another event kept, and the
// body is read from the sources after this one. The count reads every event of the owner of the
// types, from its first, through the same index.
const historyBodyQuery = `
	with changes as (
	       select count(*) filter (where l.seq <= $4) as before,
	              count(*) filter (where l.seq > $4) as after,
	              count(*) as changes
	         from public.events l
	        where (l.issue_key = $2 or l.artifact_id = $3::uuid)
	          and case when l.type = any($5::text[]) then l.payload->>'id' = $1 end)
	select h.body
	  from changes c,
	       lateral (select k.body, row_number() over (order by k.id) as n, count(*) over () as kept
	                  from public.post_body_history k
	                 where k.%s = $1::text::uuid) h
	 where c.after > 0 and h.kept = c.changes and h.n = c.before + 1`

// postBodyRead is how the whole body of a kind of post whose event cut it is read: its present
// row (presentBodyQuery) and its body history (historyBodyQuery), each statement over the kind's
// own table or column, and the event types that change the post's text after the event read,
// which both statements take as $5. Both take their arguments from the same value (arguments),
// so the statements and the arguments they are given always agree.
type postBodyRead struct {
	present, history string
	laterTypes       []string
}

// postBodyReads is the read of each kind of post's whole body, over the table of its rows and
// the column of post_body_history that names it, with the event types that change the post's
// text after the event that created it: its edit and its delete. A post's present row holds the
// text of one of its events only where no committed event of these types of the same post comes
// after that one (presentBody); and each such event kept the body it replaced in the post's
// history (historyBody). Each kind's statements are given its own types alone.
var postBodyReads = map[string]postBodyRead{
	kindComment: postBodyReadOf("comments", "comment_id", "comment.edited", "comment.deleted"),
	kindMessage: postBodyReadOf("messages", "message_id", "message.edited", "message.deleted"),
}

// postBodyReadOf is the read of the whole body of a post whose rows are in table, whose body
// history post_body_history keys by column, and whose text the event types laterTypes change
// after the event that wrote it.
func postBodyReadOf(table, column string, laterTypes ...string) postBodyRead {
	return postBodyRead{
		present:    fmt.Sprintf(presentBodyQuery, table),
		history:    fmt.Sprintf(historyBodyQuery, column),
		laterTypes: laterTypes,
	}
}

// arguments is what r's statements take to read the whole body of post id, whose event e cut
// it: the post's id, e's owner, e's seq and the types that change the post's text.
func (r postBodyRead) arguments(e event, id string) []any {
	return []any{id, e.IssueKey, e.ArtifactID, e.Seq, r.laterTypes}
}

// presentBody reads the whole body of post id of kind, whose event e cut it, from the post's
// present row where that row holds the text e wrote: where no committed event of the post after
// e changes its text (presentBodyQuery). found is false where a later event changes it, and where
// the post has no row.
func presentBody(ctx context.Context, tx pgx.Tx, e event, kind, id string) (string, bool, error) {
	read := postBodyReads[kind]
	body, found, err := readBody(ctx, tx, read.present, read.arguments(e, id))
	if err != nil {
		return "", false, fmt.Errorf("read %s %s: %w", kind, id, err)
	}
	return body, found, nil
}

// historyBody reads the whole body of post id of kind, whose event e cut it, from the post's body
// history: the body the first committed event of the post after e that changes its text kept,
// where each of the post's events of those types kept one (historyBodyQuery). found is false where
// no later event changes the post's text, and where its history does not hold a body for each.
func historyBody(ctx context.Context, tx pgx.Tx, e event, kind, id string) (string, bool, error) {
	read := postBodyReads[kind]
	body, found, err := readBody(ctx, tx, read.history, read.arguments(e, id))
	if err != nil {
		return "", false, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	return body, found, nil
}

// readBody runs sql, a statement that answers one body or none, with arguments in tx: the body,
// and found false where it answers none.
func readBody(ctx context.Context, tx pgx.Tx, sql string, arguments []any) (string, bool, error) {
	var body string
	err := tx.QueryRow(ctx, sql, arguments...).Scan(&body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return body, true, nil
}

// firstRevisionQuery reads the blocks of revision 1 of comment $1, the comment's first indexing,
// in the order of their ids' numbers (b2 before b10), which the first indexing mints in body
// order: each block's text and whether its block was inserted by the transaction whose start is
// $2.
const firstRevisionQuery = `
	select t.text, b.created_at = $2
	  from public.comment_block_texts t
	  join public.comment_blocks b on b.comment_id = t.comment_id and b.block_id = t.block_id
	 where t.comment_id = $1::uuid and t.revision = 1
	 order by length(t.block_id), t.block_id`

// firstRevisionBody is comment id's first body as revision 1 of its blocks holds it: their
// texts, in their order, joined by one blank line. ok is false unless every block of revision 1
// was inserted by the transaction that started at createdAt, the comment's own (a comment is
// indexed in the transaction that writes it, so revision 1 is then its first body), and holds a
// text. A block's text keeps the body's words and drops only the white space that ends it
// (itemblocks.Split), so the body differs from the one the comment was written with only in the
// white space between blocks, which changes no lexeme of its vector.
func firstRevisionBody(ctx context.Context, tx pgx.Tx, id string, createdAt time.Time) (string, bool, error) {
	rows, err := tx.Query(ctx, firstRevisionQuery, id, createdAt)
	if err != nil {
		return "", false, fmt.Errorf("read the first revision of comment %s: %w", id, err)
	}
	var texts []string
	whole := true
	for rows.Next() {
		var text *string
		var own bool
		if err := rows.Scan(&text, &own); err != nil {
			rows.Close()
			return "", false, fmt.Errorf("read the first revision of comment %s: %w", id, err)
		}
		if text == nil || !own {
			whole = false
			continue
		}
		texts = append(texts, *text)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("read the first revision of comment %s: %w", id, err)
	}
	if !whole || len(texts) == 0 {
		return "", false, nil
	}
	return strings.Join(texts, "\n\n"), true, nil
}
