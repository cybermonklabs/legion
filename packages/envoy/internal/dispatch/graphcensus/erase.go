package graphcensus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// eraseApplicationName is how the erase appears in pg_stat_activity, as the census names itself:
// an operator reading the activity of a database sees which pass holds its rows.
const eraseApplicationName = "envoy-dispatch erase"

// eraseMarker stands where the erased bytes were in a row the erase rewrites rather than
// deletes, with the node the run addressed written into it. A reader of a redacted payload
// learns three things from it: those bytes went by an erase and not by a lost write, which node
// that erase addressed, and that `graph_erasures` holds the record of the run - who ran it,
// when, and everything else it removed. It names the node and never the text, so a row it marks
// holds none of what was erased.
const eraseMarker = "[erased by envoy-dispatch erase %s:%s, recorded in graph_erasures]"

// blockExcerpt is refs.BlockExcerptRunes as a statement spells it: an excerpt this file rewrites
// is cut from its block's text exactly as the indexer cuts it (refs.IndexBlocks).
var blockExcerpt = strconv.Itoa(refs.BlockExcerptRunes)

// projectedTables are the tables that carry a node's address, and so are the rows a node is
// projected onto (migration 20261109022248 gave all three `node_kind` and `node_address`). Text
// is the column of each holding what a reader of that row sees, empty for a table that holds no
// text of its own: a block reference is one link out of a block and carries none.
//
// The erase walks all three by (node_kind, node_address) rather than by the node's kind, so a
// kind that gains a projection carries no second list here to keep in step.
var projectedTables = []struct{ Table, Text string }{
	{Table: "comments", Text: "body"},
	{Table: "document_blocks", Text: "excerpt"},
	{Table: "block_refs", Text: ""},
}

// ErasedRows is one table the erase emptied of a node's rows.
type ErasedRows struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// ErasedVersion is one document version whose markdown held the erased text, named as a reader
// addresses it. It carries no markdown: the record of an erase holds no erased text.
type ErasedVersion struct {
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Project  string `json:"project,omitempty"`
	Version  int    `json:"version"`
}

// ErasedReview is one approval whose content_sha256 covered the erased bytes. It cannot be
// rewritten - artifact_review_content_sha256_update raises on any statement that moves a stored
// digest - so the erase ends it, and names it by its document and its version.
type ErasedReview struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Version  int    `json:"version"`
	State    string `json:"state"`
}

// Operator is who ran the erase: the database role the credential is, and the account and host
// the command ran from. Only a person holding the deployment's database credential runs it, so
// this is the whole of who that was.
type Operator struct {
	DatabaseUser string `json:"database_user"`
	SystemUser   string `json:"system_user,omitempty"`
	Host         string `json:"host,omitempty"`
}

// Erasure is one run of `envoy-dispatch erase`: the node it addressed, everything that would
// go with it, and - once Applied - everything that did.
type Erasure struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Supertag string `json:"supertag"`
	// Tuples is how many tuples state something out of the node, Inbound how many state
	// something whose far end it is.
	Tuples  int64 `json:"tuples"`
	Inbound int64 `json:"inbound_tuples"`
	// Rows are the projection rows of the node and the rows that cannot exist without them.
	Rows []ErasedRows `json:"rows"`
	// RedactedRows are the rows the run rewrote where it may take no row away: a block of a
	// document that quoted the erased text, whose row projects a block node of its own, and the
	// append-only event log. Each of them holds the marker where those bytes were.
	RedactedRows []ErasedRows `json:"redacted"`
	// Versions and Reviews are the document versions whose markdown holds the erased text and
	// the approvals whose digest covered those bytes.
	Versions []ErasedVersion `json:"versions"`
	Reviews  []ErasedReview  `json:"reviews"`
	// Blockers are the rows of other things that point at this node's projection and are not
	// part of it: a reply to an erased comment, an ask handed back through it. Each refuses the
	// run, because deleting them would remove somebody else's words and rewriting them would
	// state something nobody said.
	Blockers []string `json:"-"`
	// Applied is whether this run removed anything. A run with no --confirm reads everything
	// above and rolls back, which is how the command refuses until it has listed what it would
	// remove.
	Applied bool      `json:"-"`
	RanBy   Operator  `json:"-"`
	RanAt   time.Time `json:"-"`
}

// ParseNode reads the `<kind>:<id>` the --node flag takes. The kind is the node's kind as
// graph_nodes keys it and holds no colon; everything after the first one is the id, so a page's
// `page:role/<topic>` and a comment's uuid are both read whole.
func ParseNode(value string) (kind, id string, err error) {
	kind, id, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found || strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" {
		return "", "", fmt.Errorf("--node takes <kind>:<id>, not %q", value)
	}
	return strings.TrimSpace(kind), strings.TrimSpace(id), nil
}

// Erase removes one node and the bytes that stand on it, in one transaction: the node's tuples,
// the tuples whose far end it is, every projection row of it and the rows that cannot exist
// without one, the `artifact_versions` rows whose markdown holds its text, and each
// `artifact_reviews` row whose `content_sha256` covered those bytes. It is the one path in this
// tree that deletes a tuple row or a node row; every other writer appends.
//
// It reads what it would remove first and refuses to remove anything until confirmed is true,
// so an operator reads every version and every review the erase would end - each by its document
// and its version - before a byte goes. A run that is not confirmed rolls its transaction back
// and has written nothing.
//
// What it did goes into `graph_erasures`: what was removed, who ran it and when, and no erased
// text. Nobody addresses that row and no read of the graph answers it.
func Erase(ctx context.Context, databaseURL, kind, id string, confirmed bool) (*Erasure, error) {
	conn, err := connect(ctx, databaseURL, eraseApplicationName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.Background())

	erasure := &Erasure{Kind: kind, ID: id, RanAt: time.Now().UTC()}
	if err := tx.QueryRow(ctx,
		`select supertag from graph_nodes where kind = $1 and id = $2 for update`,
		kind, id).Scan(&erasure.Supertag); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("the store holds no node %s:%s", kind, id)
		}
		return nil, fmt.Errorf("read node %s:%s: %w", kind, id, err)
	}
	if err := tx.QueryRow(ctx, `select current_user`).Scan(&erasure.RanBy.DatabaseUser); err != nil {
		return nil, fmt.Errorf("read the database user: %w", err)
	}
	if account, err := user.Current(); err == nil {
		erasure.RanBy.SystemUser = account.Username
	}
	if host, err := os.Hostname(); err == nil {
		erasure.RanBy.Host = host
	}

	texts, err := erasedTexts(ctx, tx, kind, id)
	if err != nil {
		return nil, err
	}
	artifacts, numbers, err := erasedVersions(ctx, tx, texts, erasure)
	if err != nil {
		return nil, err
	}
	if err := erasedReviews(ctx, tx, artifacts, numbers, erasure); err != nil {
		return nil, err
	}
	if err := erasure.countTuples(ctx, tx); err != nil {
		return nil, err
	}
	if err := erasure.readBlockers(ctx, tx, kind, id); err != nil {
		return nil, err
	}
	if !confirmed || len(erasure.Blockers) > 0 {
		return erasure, nil
	}
	if err := erasure.apply(ctx, tx, texts, artifacts, numbers); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	erasure.Applied = true
	return erasure, nil
}

// erasedTexts is the text the node's own rows hold, which is what the erase looks for in the
// document versions and in every store it rewrites. A node whose projection holds no text - a
// block reference, or a node with no projection row at all - erases no version and redacts
// nothing, and the run then removes the node alone.
func erasedTexts(ctx context.Context, tx pgx.Tx, kind, id string) ([]string, error) {
	var texts []string
	for _, held := range projectedTables {
		if held.Text == "" {
			continue
		}
		rows, err := tx.Query(ctx, `select coalesce(`+held.Text+`, '') from `+held.Table+
			` where node_kind = $1 and node_address = $2`, kind, id)
		if err != nil {
			return nil, fmt.Errorf("read %s rows of %s:%s: %w", held.Table, kind, id, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("read %s rows of %s:%s: %w", held.Table, kind, id, err)
		}
		for _, text := range found {
			if strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts, nil
}

// erasedVersions reads every document version whose markdown holds one of the erased texts, and
// answers the (artifact, number) pairs the review read takes.
func erasedVersions(ctx context.Context, tx pgx.Tx, texts []string, erasure *Erasure) ([]string, []int32, error) {
	erasure.Versions = []ErasedVersion{}
	if len(texts) == 0 {
		return nil, nil, nil
	}
	rows, err := tx.Query(ctx, `
		select v.artifact_id::text, v.number, a.ref_key, coalesce(a.issue_key, ''), coalesce(a.project_key, '')
		from artifact_versions v join artifacts a on a.id = v.artifact_id
		where exists (
			select 1 from unnest($1::text[]) as erased(text)
			where position(erased.text in v.markdown) > 0
		)
		order by a.ref_key, v.number`, texts)
	if err != nil {
		return nil, nil, fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	defer rows.Close()
	var artifacts []string
	var numbers []int32
	for rows.Next() {
		var artifact, refKey, issue, project string
		var number int32
		if err := rows.Scan(&artifact, &number, &refKey, &issue, &project); err != nil {
			return nil, nil, fmt.Errorf("scan a version holding the erased text: %w", err)
		}
		artifacts = append(artifacts, artifact)
		numbers = append(numbers, number)
		erasure.Versions = append(erasure.Versions, ErasedVersion{
			Document: refKey, Issue: issue, Project: project, Version: int(number),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	return artifacts, numbers, nil
}

// erasedReviews reads every approval standing on one of those versions. Its content_sha256 is
// the hash of exactly the bytes about to go, so the approval cannot survive them: the trigger
// refuses to move a stored digest, and an approval over bytes nobody can reproduce is a claim
// about a text that no longer exists.
func erasedReviews(ctx context.Context, tx pgx.Tx, artifacts []string, numbers []int32, erasure *Erasure) error {
	erasure.Reviews = []ErasedReview{}
	if len(artifacts) == 0 {
		return nil
	}
	// Ordered as docs.ReviewOrder orders every read of artifact_reviews in this tree: a review
	// that committed later outranks one that committed first, since created_at is its
	// transaction's start and its id is random (api's review_order_guard_test.go). The order is
	// spelled out rather than imported, because docs' own tests read this package and an import
	// the other way would be a cycle.
	rows, err := tx.Query(ctx, `
		select artifact_reviews.id::text, a.ref_key, coalesce(a.issue_key, ''),
		       artifact_reviews.version, artifact_reviews.state
		from artifact_reviews join artifacts a on a.id = artifact_reviews.artifact_id
		where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_reviews.artifact_id and doomed.number = artifact_reviews.version
		)
		order by artifact_reviews.event_seq desc nulls last, artifact_reviews.created_at desc,
		         artifact_reviews.id desc`,
		artifacts, numbers)
	if err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var review ErasedReview
		if err := rows.Scan(&review.ID, &review.Document, &review.Issue, &review.Version, &review.State); err != nil {
			return fmt.Errorf("scan a review over the erased bytes: %w", err)
		}
		erasure.Reviews = append(erasure.Reviews, review)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	return nil
}

// countTuples reads how many tuples each side of the node holds, before any of them goes.
func (e *Erasure) countTuples(ctx context.Context, tx pgx.Tx) error {
	if err := tx.QueryRow(ctx,
		`select count(*) from graph_tuples where from_kind = $1 and from_id = $2`,
		e.Kind, e.ID).Scan(&e.Tuples); err != nil {
		return fmt.Errorf("count the tuples out of %s:%s: %w", e.Kind, e.ID, err)
	}
	if err := tx.QueryRow(ctx,
		`select count(*) from graph_tuples where to_kind = $1 and to_id = $2`,
		e.Kind, e.ID).Scan(&e.Inbound); err != nil {
		return fmt.Errorf("count the tuples into %s:%s: %w", e.Kind, e.ID, err)
	}
	return nil
}

// referencingColumn is one foreign key pointing at a projected table: the table and column that
// hold the reference, the column they hold, and whether that reference can be null.
type referencingColumn struct {
	Table, Column, Referenced string
	Required                  bool
}

// referencesTo reads every single-column foreign key pointing at one table. The composite key a
// projection carries to graph_nodes is not one of them, which is why the read binds a key of one
// column: that pair is the node's own address and goes with the node.
func referencesTo(ctx context.Context, tx pgx.Tx, table string) ([]referencingColumn, error) {
	rows, err := tx.Query(ctx, `
		select c.conrelid::regclass::text, child.attname, parent.attname, child.attnotnull
		from pg_constraint c
		join pg_attribute child on child.attrelid = c.conrelid and child.attnum = c.conkey[1]
		join pg_attribute parent on parent.attrelid = c.confrelid and parent.attnum = c.confkey[1]
		where c.contype = 'f' and c.confrelid = $1::regclass and array_length(c.conkey, 1) = 1
		order by 1, 2`, table)
	if err != nil {
		return nil, fmt.Errorf("read the foreign keys into %s: %w", table, err)
	}
	defer rows.Close()
	var references []referencingColumn
	for rows.Next() {
		var reference referencingColumn
		if err := rows.Scan(&reference.Table, &reference.Column, &reference.Referenced, &reference.Required); err != nil {
			return nil, fmt.Errorf("scan a foreign key into %s: %w", table, err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the foreign keys into %s: %w", table, err)
	}
	return references, nil
}

// projectionRows is the select of one projection's key column, which every delete against a row
// that points at it is written over.
func projectionRows(reference referencingColumn, table string) string {
	return `select held.` + reference.Referenced + ` from ` + table +
		` held where held.node_kind = $1 and held.node_address = $2`
}

// readBlockers names every row that points at this node's projection and is not part of it. A
// nullable reference is such a row: a reply whose reply_to names an erased comment is somebody
// else's words, and an ask handed back through it is the ask's own history, so the erase refuses
// rather than deleting one or rewriting the other.
func (e *Erasure) readBlockers(ctx context.Context, tx pgx.Tx, kind, id string) error {
	for _, held := range projectedTables {
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if reference.Required {
				continue
			}
			var rows int64
			if err := tx.QueryRow(ctx, `select count(*) from `+reference.Table+` child where child.`+
				reference.Column+` in (`+projectionRows(reference, held.Table)+`)`, kind, id).Scan(&rows); err != nil {
				return fmt.Errorf("count the %s.%s rows at %s:%s: %w", reference.Table, reference.Column, kind, id, err)
			}
			if rows > 0 {
				e.Blockers = append(e.Blockers, fmt.Sprintf("%d %s row(s) point at this node through %s.%s",
					rows, reference.Table, reference.Table, reference.Column))
			}
		}
	}
	return nil
}

// eraseBlockTexts deletes the whole text of every block the node projects onto, which is the
// most of the bytes an erased block holds. document_block_texts is keyed by
// (artifact_id, block_id, version) and carries a foreign key to artifacts alone (migration
// 0121), so the walk of single-column references into a projected table never names it and
// the text would outlive the node that held it. It is deleted here by the pair the block's
// own document_blocks row holds, while that row is still there, for every version of the
// block, and counted like any other table the run emptied. A node with no document_blocks
// row - a comment, a person - names no pair and deletes nothing.
func (e *Erasure) eraseBlockTexts(ctx context.Context, tx pgx.Tx) error {
	tag, err := tx.Exec(ctx, `
		delete from document_block_texts erased
		where (erased.artifact_id, erased.block_id) in (
			select held.artifact_id, held.block_id from document_blocks held
			where held.node_kind = $1 and held.node_address = $2
		)`, e.Kind, e.ID)
	if err != nil {
		return fmt.Errorf("delete the texts of the blocks of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("document_block_texts", tag.RowsAffected())
	return nil
}

// marker is eraseMarker naming the node this run addressed, which is what stands in every row
// the run rewrites.
func (e *Erasure) marker() string { return fmt.Sprintf(eraseMarker, e.Kind, e.ID) }

// placeholders is `$1 … $n` for one redaction: the marker first and one for each erased text
// after it, which is the order redactionArgs binds them in.
func placeholders(texts []string) []string {
	bound := make([]string, len(texts)+1)
	for i := range bound {
		bound[i] = "$" + strconv.Itoa(i+1)
	}
	return bound
}

// redactionArgs are those parameters' values.
func redactionArgs(marker string, texts []string) []any {
	args := make([]any, 0, len(texts)+1)
	args = append(args, marker)
	for _, text := range texts {
		args = append(args, text)
	}
	return args
}

// jsonStrings is each of those parameters as a jsonb rendered to text holds it: the value's own
// json escaping, spelled by the database rather than by this package, so what a statement looks
// for in a payload is byte for byte what that payload holds. to_jsonb wraps a string in quotes,
// which the substring cuts back off.
func jsonStrings(bound []string) []string {
	escaped := make([]string, len(bound))
	for i, value := range bound {
		rendered := "to_jsonb(" + value + "::text)::text"
		escaped[i] = "substring(" + rendered + " from 2 for length(" + rendered + ") - 2)"
	}
	return escaped
}

// redaction is one column with every erased text replaced by the marker, as a statement spells
// it: one replace() around the last, so a row holding two of those texts is rewritten by one
// pass and counted once. values is placeholders, or jsonStrings of them for a column a
// statement reads as json.
func redaction(column string, values []string) string {
	rewritten := column
	for _, text := range values[1:] {
		rewritten = "replace(" + rewritten + ", " + text + ", " + values[0] + ")"
	}
	return rewritten
}

// redact rewrites the stores that hold the erased text and that the erase may take no row out
// of: the blocks of the documents that quoted it, whose rows project block nodes of their own,
// and the append-only event log. It runs after the deletes, so a row this run has already
// removed is not rewritten first and counted as both.
func (e *Erasure) redact(ctx context.Context, tx pgx.Tx, texts []string) error {
	if len(texts) == 0 {
		return nil
	}
	if err := e.redactBlocks(ctx, tx, texts); err != nil {
		return err
	}
	return e.redactEvents(ctx, tx, texts)
}

// redactBlocks rewrites every block that held the erased text: the text itself, and the excerpt
// of the document_blocks row that text was cut for. Those rows are rewritten and not deleted
// because each projects a block node of its own - a paragraph of a document that quoted the
// node, not the node this run addressed - and a live node no row projects is a difference the
// store census reads (c-census).
//
// An excerpt is cut from the rewritten text the way the indexer cuts it (refs.IndexBlocks), so
// a reader of the excerpt and a reader of the text are told the same thing, and an excerpt that
// held only the first part of a long erased text is rewritten too. A block that rewrite does
// not reach - one whose text document_block_texts does not hold - has its excerpt rewritten by
// the statement after it.
//
// text_sha256 stands as it is. It is the digest of the bytes the version stated, which is what
// the indexer compares a later version's block against: moved to the digest of the redacted
// text, a version restating the erased bytes would read as a change and write them back in.
func (e *Erasure) redactBlocks(ctx context.Context, tx pgx.Tx, texts []string) error {
	bound := placeholders(texts)
	args := redactionArgs(e.marker(), texts)
	rewritten := redaction("held.text", bound)
	var blockTexts, recut int64
	if err := tx.QueryRow(ctx, `
		with redacted as (
			update document_block_texts held set text = `+rewritten+`
			where held.text <> `+rewritten+`
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
		args...).Scan(&blockTexts, &recut); err != nil {
		return fmt.Errorf("redact the block texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	excerpts := redaction("excerpt", bound)
	tag, err := tx.Exec(ctx, `update document_blocks set excerpt = `+excerpts+
		` where excerpt <> `+excerpts, args...)
	if err != nil {
		return fmt.Errorf("redact the block excerpts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("document_block_texts", blockTexts)
	e.redacted("document_blocks", recut+tag.RowsAffected())
	return nil
}

// redactEvents rewrites the payload of every event that held the erased text. The event log is
// append-only - R5 redacts there rather than deleting - so the statement moves the payload and
// nothing else: the row stands with its id, its time and its actor, because what the log says
// happened and who did it is the record, and the bytes inside it are all the erase takes. A
// text a payload holds is json escaped, so each is looked for as that payload spells it, and
// the whole of the payload is searched, a text written into a key going with one written into
// a value.
func (e *Erasure) redactEvents(ctx context.Context, tx pgx.Tx, texts []string) error {
	rewritten := redaction("payload::text", jsonStrings(placeholders(texts)))
	tag, err := tx.Exec(ctx, `update events set payload = (`+rewritten+`)::jsonb
		where payload::text <> `+rewritten, redactionArgs(e.marker(), texts)...)
	if err != nil {
		return fmt.Errorf("redact the event payloads holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("events", tag.RowsAffected())
	return nil
}

// apply removes everything the run read, in the one order the foreign keys allow: the whole
// text of every block of the node, then the rows that cannot exist without a projection row,
// then the projection rows, then the approvals and the versions they stood on. Then it rewrites
// what it may not remove, the blocks of the documents that quoted the node and the event log,
// and last go the tuples of both sides, the node, and the record of what the run did.
func (e *Erasure) apply(ctx context.Context, tx pgx.Tx, texts, artifacts []string, numbers []int32) error {
	if err := e.eraseBlockTexts(ctx, tx); err != nil {
		return err
	}
	for _, held := range projectedTables {
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if !reference.Required {
				continue
			}
			tag, err := tx.Exec(ctx, `delete from `+reference.Table+` child where child.`+reference.Column+
				` in (`+projectionRows(reference, held.Table)+`)`, e.Kind, e.ID)
			if err != nil {
				return fmt.Errorf("delete the %s rows of %s:%s: %w", reference.Table, e.Kind, e.ID, err)
			}
			e.removed(reference.Table, tag.RowsAffected())
		}
		tag, err := tx.Exec(ctx, `delete from `+held.Table+` where node_kind = $1 and node_address = $2`, e.Kind, e.ID)
		if err != nil {
			return fmt.Errorf("delete the %s rows of %s:%s: %w", held.Table, e.Kind, e.ID, err)
		}
		e.removed(held.Table, tag.RowsAffected())
	}
	if len(artifacts) > 0 {
		tag, err := tx.Exec(ctx, `delete from artifact_reviews where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_reviews.artifact_id and doomed.number = artifact_reviews.version)`,
			artifacts, numbers)
		if err != nil {
			return fmt.Errorf("delete the reviews over the erased bytes: %w", err)
		}
		e.removed("artifact_reviews", tag.RowsAffected())
		tag, err = tx.Exec(ctx, `delete from artifact_versions where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_versions.artifact_id and doomed.number = artifact_versions.number)`,
			artifacts, numbers)
		if err != nil {
			return fmt.Errorf("delete the versions holding the erased text: %w", err)
		}
		e.removed("artifact_versions", tag.RowsAffected())
	}
	if err := e.redact(ctx, tx, texts); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the tuples out of %s:%s: %w", e.Kind, e.ID, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_tuples where to_kind = $1 and to_id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the tuples into %s:%s: %w", e.Kind, e.ID, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_nodes where kind = $1 and id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the node %s:%s: %w", e.Kind, e.ID, err)
	}
	removed, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("render the erasure record: %w", err)
	}
	ranBy, err := json.Marshal(e.RanBy)
	if err != nil {
		return fmt.Errorf("render who ran the erase: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into graph_erasures (node_kind, node_id, removed, ran_by, ran_at)
		values ($1, $2, $3, $4, $5)`, e.Kind, e.ID, removed, ranBy, e.RanAt); err != nil {
		return fmt.Errorf("record the erase: %w", err)
	}
	return nil
}

// removed adds one table's deleted rows to the report, keeping one line per table.
func (e *Erasure) removed(table string, rows int64) {
	e.Rows = countedRows(e.Rows, table, rows)
}

// redacted adds one table's rewritten rows to the report, counted as removed counts a deleted
// one, so a run says what it rewrote as well as what it emptied.
func (e *Erasure) redacted(table string, rows int64) {
	e.RedactedRows = countedRows(e.RedactedRows, table, rows)
}

// countedRows adds one table's rows to a count of them, keeping one line per table.
func countedRows(counted []ErasedRows, table string, rows int64) []ErasedRows {
	if rows == 0 {
		return counted
	}
	for i := range counted {
		if counted[i].Table == table {
			counted[i].Rows += rows
			return counted
		}
	}
	return append(counted, ErasedRows{Table: table, Rows: rows})
}

// Write prints what the run read and what it did, the versions and the reviews named one per
// line so an operator reads every one before confirming.
func (e *Erasure) Write(w io.Writer) {
	verb := "would remove"
	if e.Applied {
		verb = "removed"
	}
	fmt.Fprintf(w, "erase %s:%s (supertag %s), %s by %s at %s\n",
		e.Kind, e.ID, e.Supertag, verb, e.RanBy.DatabaseUser, e.RanAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  tuples: %d out of the node, %d into it\n", e.Tuples, e.Inbound)
	if len(e.Rows) == 0 {
		fmt.Fprintln(w, "  rows: none")
	}
	for _, rows := range e.Rows {
		fmt.Fprintf(w, "  rows: %s %d\n", rows.Table, rows.Rows)
	}
	for _, rows := range e.RedactedRows {
		fmt.Fprintf(w, "  redacted: %s %d\n", rows.Table, rows.Rows)
	}
	fmt.Fprintf(w, "  versions holding the erased text: %d\n", len(e.Versions))
	for _, version := range e.Versions {
		fmt.Fprintf(w, "    %s version %d%s\n", version.Document, version.Version, versionOwner(version))
	}
	fmt.Fprintf(w, "  reviews over those bytes: %d\n", len(e.Reviews))
	for _, review := range e.Reviews {
		fmt.Fprintf(w, "    %s version %d %s (review %s)%s\n",
			review.Document, review.Version, review.State, review.ID, reviewIssue(review))
	}
	for _, blocker := range e.Blockers {
		fmt.Fprintf(w, "  blocked: %s\n", blocker)
	}
}

// versionOwner names the issue or the project a version's document belongs to, for the line
// that lists it.
func versionOwner(version ErasedVersion) string {
	if version.Issue != "" {
		return " of " + version.Issue
	}
	if version.Project != "" {
		return " of " + version.Project
	}
	return ""
}

func reviewIssue(review ErasedReview) string {
	if review.Issue == "" {
		return ""
	}
	return " of " + review.Issue
}
