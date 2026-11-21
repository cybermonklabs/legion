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
// the post changes its text (presentBody), then, for a comment.created, revision 1 of its blocks
// (firstRevisionBody). Else it keeps the event's text (bodyFromCut), which complete warns of
// where it keeps v.
func (d *decider) wholeBody(ctx context.Context, e event, v *version) error {
	body, found, err := presentBody(ctx, d.tx, e, v.Kind, v.ID)
	if err != nil {
		return err
	}
	if found {
		v.Body, v.BodyFrom = body, bodyFromRow
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
// the post's text ($5, the kind's later types in presentBodyReads) and names the post: the row
// and the later events are read in the statement's one snapshot, so a row that answers holds the
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

// presentRowQuery reads the body of post $1 from its present row in the table its kind names
// (%s), for a kind whose text no event changes after the one that wrote it (presentBodyReads
// gives it no later types), so the row holds the text of each of its events and no later event
// is read.
const presentRowQuery = `select p.body from public.%s p where p.id = $1::text::uuid`

// presentBodyRead is how presentBody reads the present body of a kind of post: its statement,
// and the event types that change the post's text after the event read (presentBodyReads), which
// the statement takes as $5 (presentBodyQuery); with none, the statement reads the row alone
// (presentRowQuery). presentBody takes the statement's arguments from the same value (arguments),
// so the statement and the arguments it is given always agree.
type presentBodyRead struct {
	sql        string
	laterTypes []string
}

// presentBodyReads is the read of each kind of post's present body, over the table of its rows,
// with the event types that change the post's text after the event that created it. A post's
// present row holds the text of one of its events only where no committed event of these types
// of the same post comes after that one (presentBody). Each kind's statement is given its own
// types alone, and a kind with none reads its row alone (presentRowQuery). A comment's is
// comment.edited, which INFRA-1523's edit and delete types join once their rules come; no build
// edits a message, so a message has none yet, and its types go here once one does.
var presentBodyReads = map[string]presentBodyRead{
	kindComment: presentBodyReadOf("comments", "comment.edited"),
	kindMessage: presentBodyReadOf("messages"),
}

// presentBodyReadOf is the read of the present body of a post whose rows are in table and whose
// text the event types laterTypes change after the event that wrote it: presentRowQuery where
// there are none, else presentBodyQuery.
func presentBodyReadOf(table string, laterTypes ...string) presentBodyRead {
	if len(laterTypes) == 0 {
		return presentBodyRead{sql: fmt.Sprintf(presentRowQuery, table)}
	}
	return presentBodyRead{sql: fmt.Sprintf(presentBodyQuery, table), laterTypes: laterTypes}
}

// arguments is what r's statement takes to read the present body of post id, whose event e cut
// it: the post's id and, where the statement reads the later events, e's owner, e's seq and the
// types that change the post's text.
func (r presentBodyRead) arguments(e event, id string) []any {
	if len(r.laterTypes) == 0 {
		return []any{id}
	}
	return []any{id, e.IssueKey, e.ArtifactID, e.Seq, r.laterTypes}
}

// presentBody reads the whole body of post id of kind, whose event e cut it, from the post's
// present row where that row holds the text e wrote: where no committed event of the post after
// e changes its text (presentBodyQuery), which, for a kind whose text no later event changes, is
// wherever the post has a row (presentRowQuery). found is false where a later event changes it,
// and where the post has no row.
func presentBody(ctx context.Context, tx pgx.Tx, e event, kind, id string) (string, bool, error) {
	read := presentBodyReads[kind]
	var body string
	err := tx.QueryRow(ctx, read.sql, read.arguments(e, id)...).Scan(&body)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read %s %s: %w", kind, id, err)
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
