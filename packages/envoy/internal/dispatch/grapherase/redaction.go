package grapherase

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// redactedEvent is one event about the node whose payload holds an erased text in a string value:
// its id and type, the payload as the redaction leaves it, and each string value that redaction
// rewrote as it stood before, whose display tree goes (treeSums).
type redactedEvent struct {
	id        int64
	kind      string
	payload   []byte
	originals []string
}

// readEvents reads the events about the node whose payload holds an erased text, and redacts each
// payload in memory (redactPayload), which apply then writes. An event about a comment is one
// whose payload is that comment's own (model.CommentEventPayload carries it as `id`) or names it
// as `comment_id` (a delivery receipt); an event about a block is a document event of its own
// document (`artifact.*` and `block.*`, whose payload names it as `artifact_id`), which carries
// its versions and their diffs. Any other event that holds the same bytes - another person's
// comment, another document's version - is about another node, and stays.
func (e *Erasure) readEvents(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	if len(read.texts) == 0 {
		return nil
	}
	var about, subject string
	switch {
	case refs.ItemKind(e.Kind) == refs.ItemComment:
		about, subject = `payload->>'id' = $1 or payload->>'comment_id' = $1`, e.ID
	case read.document != nil:
		about, subject = `payload->>'artifact_id' = $1 and (type like 'artifact.%' or type like 'block.%')`, read.document.artifact
	default:
		return nil
	}
	rows, err := tx.Query(ctx, `select id, type, payload::text from events where `+about+` order by id`, subject)
	if err != nil {
		return fmt.Errorf("read the events about %s:%s: %w", e.Kind, e.ID, err)
	}
	type stored struct {
		id            int64
		kind, payload string
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
		var event stored
		err := row.Scan(&event.id, &event.kind, &event.payload)
		return event, err
	})
	if err != nil {
		return fmt.Errorf("read the events about %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, event := range found {
		payload, originals, err := redactPayload([]byte(event.payload), read.texts, e.marker())
		if err != nil {
			return fmt.Errorf("redact the payload of event %d: %w", event.id, err)
		}
		if len(originals) == 0 {
			continue
		}
		read.events = append(read.events, redactedEvent{id: event.id, kind: event.kind, payload: payload, originals: originals})
		read.listing.events = append(read.listing.events, fmt.Sprintf("events %d %s", event.id, event.kind))
	}
	return nil
}

// redactPayload replaces each erased text with the marker inside every string value of a JSON
// payload, and answers the payload so redacted and each string value it rewrote as it stood
// before. A key is never touched: a text is erased from what a payload says, and a key is its
// shape, so an erase of `kind` leaves every "kind" key and redacts a body reading "kind". The
// payload is a jsonb's text, so its numbers are read as the literals they are and its key order
// is the one jsonb keeps whatever order it is written back in.
func redactPayload(payload []byte, texts []string, marker string) ([]byte, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, err
	}
	var originals []string
	var redact func(any) any
	redact = func(value any) any {
		switch held := value.(type) {
		case string:
			rewritten := held
			for _, text := range texts {
				rewritten = strings.ReplaceAll(rewritten, text, marker)
			}
			if rewritten != held {
				originals = append(originals, held)
			}
			return rewritten
		case []any:
			for index := range held {
				held[index] = redact(held[index])
			}
		case map[string]any:
			for key := range held {
				held[key] = redact(held[key])
			}
		}
		return value
	}
	value = redact(value)
	if len(originals) == 0 {
		return payload, nil, nil
	}
	var written bytes.Buffer
	encoder := json.NewEncoder(&written)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, nil, err
	}
	return bytes.TrimSpace(written.Bytes()), originals, nil
}

// redactEvents writes the payload of every event readEvents redacted. The event log is
// append-only - R5 redacts there rather than deleting - so the statement moves the payload and
// nothing else: the row stands with its id, its time and its actor, because what the log says
// happened and who did it is the record, and the bytes inside it are all the erase takes.
func (e *Erasure) redactEvents(ctx context.Context, tx pgx.Tx, redacted []redactedEvent) error {
	for _, event := range redacted {
		if _, err := tx.Exec(ctx, `update events set payload = $2::jsonb where id = $1`, event.id, string(event.payload)); err != nil {
			return fmt.Errorf("redact the payload of event %d: %w", event.id, err)
		}
	}
	e.redacted("events", int64(len(redacted)))
	return nil
}

// redactBlocks rewrites the text and the excerpt of every block that holds the node's block and
// quotes it (erasedRead.ancestors): a container's text is the document cut across the blocks
// inside it. Those rows are rewritten and not deleted because each projects a block node of its
// own, which the erase does not remove, and a live node no row projects is a difference the store
// census reads (c-census).
//
// An excerpt is cut from the rewritten text the way the indexer cuts it (refs.IndexBlocks), so a
// reader of the excerpt and a reader of the text are told the same thing, and an excerpt that
// held only the first part of a long erased text is rewritten too. A block that rewrite does not
// reach - one whose text document_block_texts does not hold - has its excerpt rewritten by the
// statement after it.
//
// text_sha256 stands as it is. It is the digest of the bytes the version stated, which is what
// the indexer compares a later version's block against: moved to the digest of the redacted
// text, a version restating the erased bytes would read as a change and write them back in.
func (e *Erasure) redactBlocks(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if read.document == nil || len(read.ancestors) == 0 {
		return nil
	}
	rewritten := redaction("held.text", read.texts)
	var blockTexts, recut int64
	if err := tx.QueryRow(ctx, `
		with redacted as (
			update document_block_texts held set text = `+rewritten+`
			where held.artifact_id = $3::uuid and held.block_id = any($4::text[]) and held.text <> `+rewritten+`
			returning held.artifact_id, held.block_id, held.version, held.text
		), recut as (
			update document_blocks block set excerpt = left(redacted.text, `+blockExcerpt+`)
			from redacted
			where redacted.artifact_id = block.artifact_id and redacted.block_id = block.block_id
			  and redacted.version = block.changed_version
			  and block.excerpt <> left(redacted.text, `+blockExcerpt+`)
			returning 1
		)
		select (select count(*) from redacted), (select count(*) from recut)`,
		e.marker(), read.texts, read.document.artifact, read.ancestors).Scan(&blockTexts, &recut); err != nil {
		return fmt.Errorf("redact the texts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	excerpt := redaction("held.excerpt", read.texts)
	tag, err := tx.Exec(ctx, `update document_blocks held set excerpt = `+excerpt+`
		where held.artifact_id = $3::uuid and held.block_id = any($4::text[]) and held.excerpt <> `+excerpt,
		e.marker(), read.texts, read.document.artifact, read.ancestors)
	if err != nil {
		return fmt.Errorf("redact the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("document_block_texts", blockTexts)
	e.redacted("document_blocks", recut+tag.RowsAffected())
	return nil
}

// redaction is one column with every erased text replaced by the marker, as a statement spells
// it: $1 is the marker and $2 the texts, longest first (readTexts), replaced one after the other
// so a row holding two of them is rewritten by one pass. A text holding another is replaced
// whole before the shorter one can cut it.
func redaction(column string, texts []string) string {
	rewritten := column
	for index := range texts {
		rewritten = fmt.Sprintf("replace(%s, ($2::text[])[%d], $1::text)", rewritten, index+1)
	}
	return rewritten
}

// clearExcerpts takes away the excerpt every mention of the node's document shows when it was
// cut from the node's block or from a block holding it: the excerpt is that block's first
// refs.BlockExcerptRunes runes, which hold the erased bytes in part where they do not hold them
// whole. Each such mention is marked owed an excerpt, as a version write that cannot cut one marks
// it (refs.ClearExcerpts), so the reconciliation cuts it again from the latest version left, which
// holds none of the erased text, and until then the references route shows the document's name.
func (e *Erasure) clearExcerpts(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if read.document == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `
		update refs set excerpt_text = null, excerpt_block_id = null, excerpt_version = null
		where from_kind = 'artifact' and from_id = $1::uuid::text and excerpt_block_id = any($2::text[])
		  and excerpt_text is not null`,
		read.document.artifact, append([]string{read.blockID}, read.ancestors...))
	if err != nil {
		return fmt.Errorf("clear the excerpts quoting %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("refs", tag.RowsAffected())
	return nil
}
