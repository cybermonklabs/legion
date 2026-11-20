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
//
// A document event's payload carries the whole document, so it holds the erased block's text
// beside every sibling's: it is redacted where the erased text stands as a block of its own
// (redactBlockQuote) and nowhere else, which leaves a sibling's line in a version's markdown or
// in a diff as it was written. A comment's own event carries that comment's body and nothing
// else of anybody's, so every one of those bytes goes wherever it stands in what a person wrote.
//
// An event about the node whose payload holds an erased text in its structural values alone
// (structuralKey) is not redacted, so it is none of these: the run leaves it whole, and the
// search for the rows it leaves lists it (readLeft), so the listing says what the run does.
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
		payload, originals, err := redactPayload([]byte(event.payload), read.texts, e.marker(), read.document != nil)
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
// payload that carries what a person wrote, and answers the payload so redacted and each string
// value it rewrote as it stood before. A key is never touched: a text is erased from what a
// payload says, and a key is its shape. A value under a structural key is never touched either
// (structuralKey), since it says what the payload is about and not what anybody wrote: an erase of
// `kind` leaves every "kind" key and the kind each one names, and redacts a body reading "kind";
// an erase of `sent` leaves a receipt's state reading "sent"; and an erase of a text that is part
// of a session's id leaves that id whole. The payload is a jsonb's text, so its numbers are read
// as the literals they are and its key order is the one jsonb keeps whatever order it is written
// back in.
//
// quoted says the payload quotes a document, whose blocks are other nodes': a text is then
// replaced only where it stands as a block of its own (redactBlockQuote).
func redactPayload(payload []byte, texts []string, marker string, quoted bool) ([]byte, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, err
	}
	var originals []string
	// key is the key the value stands under, and the key of the array that holds it for an
	// element of one: an array under a structural key is a list of structural values.
	var redact func(value any, key string) any
	redact = func(value any, key string) any {
		switch held := value.(type) {
		case string:
			if structuralKey(key) {
				return held
			}
			rewritten := held
			if quoted {
				rewritten = redactBlockQuote(held, texts, marker)
			} else {
				for _, text := range texts {
					rewritten = strings.ReplaceAll(rewritten, text, marker)
				}
			}
			if rewritten != held {
				originals = append(originals, held)
			}
			return rewritten
		case []any:
			for index := range held {
				held[index] = redact(held[index], key)
			}
		case map[string]any:
			for name := range held {
				held[name] = redact(held[name], name)
			}
		}
		return value
	}
	value = redact(value, "")
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

// structuralNames and structuralSuffixes are the keys of an event payload whose values are its
// structure rather than what a person wrote: what names a thing, where it is addressed, which of
// a closed set of states or kinds it is, and when it happened. They are the names the payloads
// the erase reads carry, and one rule at every depth of every event type: the comment's own
// events (model.CommentEventPayload, with the actors, anchors, mentions, attempts and references
// inside it), its delivery receipts (model.CommentDeliveryEventPayload) and a document's events
// (docs.ArtifactVersionEventPayload with its model.Version, model.Artifact,
// model.ArtifactReviewEventPayload, model.ArtifactHumanEditedEventPayload and the block events).
//
// The rule names the structure and not the content because the two failures are not alike. A
// key it does not know is read as content and redacted, so a field a later build adds that holds
// what a person wrote loses the erased bytes as every other does; at worst such a field that is
// structure takes the marker, which breaks a reader of that one value. A list of content keys
// would leave the bytes standing in every content field a later build adds, which is the failure
// an erase exists to prevent.
var structuralNames = map[string]bool{
	// Identifiers: a node's, the comment a reply replies to or answers, a person's login, a
	// session's, a service's, a project's, a digest and where it was read from.
	"id": true, "reply_to": true, "answers": true, "owner": true, "editors": true, "session": true,
	"service": true, "project": true, "sha256": true, "content_sha256_source": true,
	// Addresses: a delivery's target, an issue's route, a reference, a node's address, a slug.
	"target": true, "route": true, "ref": true, "node": true, "slug": true,
	// Enumerations: a kind, a type, a state, a status, a role, how a mention is delivered, whose
	// turn it is, a credential's scopes, a version's media type, whom an ask waits on.
	"kind": true, "type": true, "state": true, "status": true, "role": true, "delivery": true,
	"turn": true, "scopes": true, "mime": true, "waiting_on": true,
	// A time.
	"at": true,
}

var structuralSuffixes = []string{
	"_id", "_key", "_slug", "_route", "_type", "_kinds", "_state", "_status", "_waiting_on",
	"_logins", "_sha256", "_at",
}

// structuralKey reports whether a value under key is structure (structuralNames), which the
// redaction never rewrites. The top-level value of a payload stands under no key and is content.
func structuralKey(key string) bool {
	if structuralNames[key] {
		return true
	}
	for _, suffix := range structuralSuffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
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

// redactedBlockText is the text one block of the node's document held at one version, as the
// redaction leaves it.
type redactedBlockText struct {
	blockID string
	version int32
	text    string
}

// redactedExcerpt is the excerpt of one block of the node's document, as the redaction leaves
// it, with the node that block projects, which names it on the listing.
type redactedExcerpt struct {
	blockID, nodeID, excerpt string
}

// redactBlocks writes back the text and the excerpt of every block that holds the node's block
// and quotes it, as the read under the run's locks left them (readAncestors). Those rows are
// rewritten and not deleted because each projects a block node of its own, which the erase does
// not remove, and a live node no row projects is a difference the store census reads (c-census).
//
// It writes the rows the listing named and no others: the redaction was computed from the same
// read, so a block of the same document holding the erased words where the node's own block does
// not stand is none of these.
//
// An excerpt is cut from the rewritten text the way the indexer cuts it (refs.IndexBlocks), so a
// reader of the excerpt and a reader of the text are told the same thing, and an excerpt that
// held only the first part of a long erased text is rewritten too.
//
// text_sha256 stands as it is. It is the digest of the bytes the version stated, which is what
// the indexer compares a later version's block against: moved to the digest of the redacted
// text, a version restating the erased bytes would read as a change and write them back in.
func (e *Erasure) redactBlocks(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if read.document == nil {
		return nil
	}
	if len(read.blockTexts) > 0 {
		blocks := make([]string, len(read.blockTexts))
		versions := make([]int32, len(read.blockTexts))
		texts := make([]string, len(read.blockTexts))
		for index, held := range read.blockTexts {
			blocks[index], versions[index], texts[index] = held.blockID, held.version, held.text
		}
		tag, err := tx.Exec(ctx, `
			update document_block_texts held set text = redacted.text
			from unnest($2::text[], $3::int[], $4::text[]) as redacted (block_id, version, text)
			where held.artifact_id = $1::uuid and held.block_id = redacted.block_id and held.version = redacted.version`,
			read.document.artifact, blocks, versions, texts)
		if err != nil {
			return fmt.Errorf("redact the texts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
		}
		e.redacted("document_block_texts", tag.RowsAffected())
	}
	if len(read.excerpts) > 0 {
		blocks := make([]string, len(read.excerpts))
		excerpts := make([]string, len(read.excerpts))
		for index, held := range read.excerpts {
			blocks[index], excerpts[index] = held.blockID, held.excerpt
		}
		tag, err := tx.Exec(ctx, `
			update document_blocks held set excerpt = redacted.excerpt
			from unnest($2::text[], $3::text[]) as redacted (block_id, excerpt)
			where held.artifact_id = $1::uuid and held.block_id = redacted.block_id`,
			read.document.artifact, blocks, excerpts)
		if err != nil {
			return fmt.Errorf("redact the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
		}
		e.redacted("document_blocks", tag.RowsAffected())
	}
	return nil
}

// blockFraming are the bytes a line of markdown can carry before a block's own text begins: the
// indent and the marker of a list item, a quote or a heading, the pipe of a table row, and the
// space, `+` or `-` a line of a diff opens with.
const blockFraming = " \t>-*+#.)|0123456789"

// redactBlockQuote replaces each erased text with the marker in a text that quotes a whole
// document - a version's markdown, a diff of one, the text of a block that holds other blocks -
// where that text stands as a block of its own, and leaves it standing anywhere else.
//
// A block's stored text is the slice of its document's markdown the block was rendered into
// (refs.IndexBlocks), so the erased block stands in such a quote at a line of its own, after
// whatever framing the line carries, and ends a line. Words of it inside a longer line are
// another block's text: a sibling that repeats them is a node of its own, which R5 has the run
// list and leave for an erase of that node.
//
// Two blocks of one document whose whole text is the same cannot be told apart in a quote of
// that document, and both read as the erased one. A text read from a block's excerpt rather than
// from its stored text (readTexts) is the first refs.BlockExcerptRunes runes of a long block, so
// it ends a line only where the block does, and a quote that holds no more than those runes of
// it keeps them.
func redactBlockQuote(quote string, texts []string, marker string) string {
	redacted := quote
	for _, text := range texts {
		if text == "" {
			continue
		}
		var written strings.Builder
		rest := redacted
		for {
			at := strings.Index(rest, text)
			if at < 0 {
				break
			}
			end := at + len(text)
			if standsAsBlock(rest, at, end) {
				written.WriteString(rest[:at])
				written.WriteString(marker)
			} else {
				written.WriteString(rest[:end])
			}
			rest = rest[end:]
		}
		if written.Len() == 0 {
			continue
		}
		written.WriteString(rest)
		redacted = written.String()
	}
	return redacted
}

// standsAsBlock reports whether quote[at:end] stands as a block of its own: its line holds
// nothing but framing before it (blockFraming), and it ends the quote or its line.
func standsAsBlock(quote string, at, end int) bool {
	for index := at - 1; index >= 0; index-- {
		if quote[index] == '\n' {
			break
		}
		if strings.IndexByte(blockFraming, quote[index]) < 0 {
			return false
		}
	}
	return end == len(quote) || quote[end] == '\n' || quote[end] == '\r'
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
