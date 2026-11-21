package grapherase

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// restateSearch writes again the two search vectors that stand on a document's latest version,
// for each document a version of which the run deleted.
//
// document_search holds each document's latest version with markdown (migration
// 20261101182447), and 20261101182454's trigger writes it on a version's insert or on an update of
// its markdown or its search, under a guard that keeps a later version's row from an older one.
// A delete of the latest version is the one write that lowers the latest, and no trigger sees it,
// so the row the run's delete left standing on a gone version goes, and the latest version left
// is touched where its search is, which has that trigger write the row as it writes every other
// row of the table. A document with no version left keeps no row, as one that never had a version
// with markdown has none.
//
// Each row that goes is queued for the term statistics with the vector it held
// (search_term_queue), the row 20261101182522's trigger writes for a row of the table it
// replaces. That trigger fires on an insert or an update alone, since document_search had no
// delete before the erase, so without this row no queue row would carry the vector of the version
// that went, which is the vector the statistics count for the document, and
// api.ForgetErasedTermsTx, which reads the erased words from the queue rows the run wrote, could
// not take them out.
//
// An issue's own search vector holds its spec's text (issue_embedding_spec, migration
// 20261105145242), written by the trigger on the spec's version insert, so an issue whose spec
// lost a version has that vector written again by the statement that trigger runs.
func (e *Erasure) restateSearch(ctx context.Context, tx pgx.Tx, artifacts []string) error {
	if len(artifacts) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		with gone as (
			delete from document_search d
			where d.artifact_id = any($1::uuid[])
			  and not exists (select 1 from artifact_versions v where v.artifact_id = d.artifact_id and v.number = d.number)
			returning d.artifact_id, d.search
		), queued as (
			insert into search_term_queue (kind, artifact_id, old_search)
			select 'document', gone.artifact_id, coalesce(gone.search, ''::tsvector) from gone
		)
		select artifact_id::text from gone`, artifacts)
	if err != nil {
		return fmt.Errorf("delete the search rows of the versions that went: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("delete the search rows of the versions that went: %w", err)
	}
	var restated int64
	if len(stale) > 0 {
		tag, err := tx.Exec(ctx, `
			update artifact_versions v set search = v.search
			where (v.artifact_id, v.number) in (
				select distinct on (latest.artifact_id) latest.artifact_id, latest.number
				from artifact_versions latest
				where latest.artifact_id = any($1::uuid[]) and latest.markdown is not null
				order by latest.artifact_id, latest.number desc)`, stale)
		if err != nil {
			return fmt.Errorf("write the search rows of the latest versions left: %w", err)
		}
		restated = tag.RowsAffected()
	}
	e.removed("document_search", int64(len(stale))-restated)
	e.redacted("document_search", restated)
	tag, err := tx.Exec(ctx, `
		update issues i set search = issue_search_vector(i.key, i.title, issue_embedding_spec(i.key))
		from artifacts a
		where a.issue_key = i.key and a.is_primary and a.id = any($1::uuid[])`, artifacts)
	if err != nil {
		return fmt.Errorf("write the search vectors of the issues whose spec lost a version: %w", err)
	}
	e.redacted("issues", tag.RowsAffected())
	return nil
}

// restateKeywordVersions deletes the node's own rows of the keyword table (search_text_versions),
// every version of its text the keyword fill wrote, and writes again the vector of each keyword
// row of its spec issue that names a version the run deleted (textversions.RestateIssueVectors),
// by the expression the fill writes it with. It runs after the versions' delete, so that vector
// reads no markdown for the version that went and keeps the issue's key and the title its row
// holds. The fill writes no row of the table while the run holds its cursor row
// (textversions.HoldFillBack, lockOwnersThenNode), so the rows these statements reach are the
// ones the read under the run's locks listed (readSearch).
func (e *Erasure) restateKeywordVersions(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	tag, err := tx.Exec(ctx, `delete from search_text_versions where kind = $1 and id = $2`, e.Kind, e.ID)
	if err != nil {
		return fmt.Errorf("delete the keyword versions of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("search_text_versions", tag.RowsAffected())
	issue := read.document.specIssue()
	if issue == "" || len(read.numbers) == 0 {
		return nil
	}
	restated, err := textversions.RestateIssueVectors(ctx, tx, issue, read.document.artifact, read.numbers)
	if err != nil {
		return err
	}
	e.redacted("search_text_versions", restated)
	return nil
}

// forgottenEmbeddings is what forgetEmbeddings deleted that a later step of the same run writes
// again once the versions holding the erased text are gone (restateEmbeddings): the issues whose
// embedding row held it, and the documents whose windows did.
type forgottenEmbeddings struct {
	issues, documents []string
}

// itemWindows is the window kind and the owner key the windows of the node's blocks are written
// under, for a comment, a message or an ask (refs.ItemKind.WindowKind, WindowOwnerKey), and empty
// for any other node.
func (e *Erasure) itemWindows() (windowKind, ownerKey string) {
	if kind := refs.ItemKind(e.Kind); kind == refs.ItemComment || kind == refs.ItemMessage || kind == refs.ItemAsk {
		return kind.WindowKind(), kind.WindowOwnerKey(e.ID)
	}
	return "", ""
}

// forgetEmbeddings deletes the rows of the embedding store that derive from the node
// (readEmbeddings) and its item's window state, and answers what of it a later step writes
// again. Nothing else of the erase reaches that store: embeddings carries no foreign key to the
// rows its text was cut from, so no delete reaches it through one, and the text would stand in
// text_snapshot, and in the vector the embedder made of it, for as long as the row did.
//
// It deletes the rows the read under the run's locks found and no others. A comment's windows
// are written under the comment's own row (refs.WriteItemWindows) and a document's under its
// owner row (refs.WriteDocumentWindows), both of which the run took before that read, so no
// window of the node is written between the read and this delete.
//
// It runs before anything goes, while the versions the issue row is written again from still
// stand, as treeSums does.
func (e *Erasure) forgetEmbeddings(ctx context.Context, tx pgx.Tx, read erasedRead) (forgottenEmbeddings, error) {
	var forgotten forgottenEmbeddings
	windowKind, ownerKey := e.itemWindows()
	if len(read.embedded) > 0 {
		kinds := make([]string, len(read.embedded))
		ids := make([]string, len(read.embedded))
		for index, held := range read.embedded {
			kinds[index], ids[index] = held.kind, held.id
		}
		tag, err := tx.Exec(ctx, `
			delete from embeddings held
			using unnest($1::text[], $2::text[]) as doomed (kind, id)
			where held.kind = doomed.kind and held.id = doomed.id`, kinds, ids)
		if err != nil {
			return forgottenEmbeddings{}, fmt.Errorf("delete the embedding rows of %s:%s: %w", e.Kind, e.ID, err)
		}
		documents := map[string]struct{}{}
		for _, held := range read.embedded {
			switch held.kind {
			case "issue":
				forgotten.issues = append(forgotten.issues, held.id)
			case refs.DocumentWindowKind:
				owner, _, _ := strings.Cut(held.id, "#")
				documents[owner] = struct{}{}
			}
		}
		forgotten.documents = slices.Sorted(maps.Keys(documents))
		e.removed("embeddings", tag.RowsAffected()-int64(len(forgotten.issues)))
	}
	if windowKind != "" {
		tag, err := tx.Exec(ctx, `delete from embedding_window_states where kind = $1 and owner_key = $2`, windowKind, ownerKey)
		if err != nil {
			return forgottenEmbeddings{}, fmt.Errorf("delete the window state of %s:%s: %w", e.Kind, e.ID, err)
		}
		e.removed("embedding_window_states", tag.RowsAffected())
	}
	return forgotten, nil
}

// restateEmbeddings writes again what forgetEmbeddings deleted that stands for something the run
// leaves, once the versions holding the erased text are gone.
//
// An issue's row is written by the statement migration 0140's trigger runs on a version of the
// issue's spec (embeddings_enqueue_issue_spec): embeddings_enqueue of issue_embedding_text over
// the issue's title and its spec's latest version left. The row is a new one, so the queue embeds
// it afresh and no vector of the erased text stands for the issue meanwhile; each is counted as
// rewritten, and one whose issue is gone as removed.
//
// A document whose windows went is marked owed them, keeping a mark already set as refs'
// queueStand keeps it, which is how the block-index tick's window arm learns of an owner
// whose windows were removed outside the one statement that writes them (refs.DocumentsOwedWindows):
// the arm writes them from the latest version left, which holds none of the erased text either.
// A document with no markdown version left is not marked: the arm writes nothing for it
// (docs' windowDocument) and would leave the mark standing.
func (e *Erasure) restateEmbeddings(ctx context.Context, tx pgx.Tx, forgotten forgottenEmbeddings) error {
	if len(forgotten.issues) > 0 {
		tag, err := tx.Exec(ctx, `
			select embeddings_enqueue('issue', i.key, issue_embedding_text(i.title, issue_embedding_spec(i.key)))
			from issues i where i.key = any($1::text[])
			order by i.key`, forgotten.issues)
		if err != nil {
			return fmt.Errorf("write the embedding rows of the issues whose spec lost a version: %w", err)
		}
		e.redacted("embeddings", tag.RowsAffected())
		e.removed("embeddings", int64(len(forgotten.issues))-tag.RowsAffected())
	}
	if len(forgotten.documents) > 0 {
		if _, err := tx.Exec(ctx, `
			update document_block_versions owed set windows_owed_at = coalesce(owed.windows_owed_at, now())
			where owed.artifact_id::text = any($1::text[])
			  and exists (select 1 from artifact_versions left_standing
			              where left_standing.artifact_id = owed.artifact_id and left_standing.markdown is not null)`,
			forgotten.documents); err != nil {
			return fmt.Errorf("mark the documents whose windows went owed them: %w", err)
		}
	}
	return nil
}

// replaceLiveTexts writes the seed of each document whose live state the run loaded in place of
// its history (docs.ErasedLiveText's Replace), in the order readLiveTexts read them, and counts
// what went: every stored update, checkpoint and snapshot as removed, and the seed as the one
// row the run rewrote in their place.
func (e *Erasure) replaceLiveTexts(ctx context.Context, tx pgx.Tx) error {
	for _, document := range e.Documents {
		if document.live == nil {
			continue
		}
		replaced, err := document.live.Replace(ctx, tx)
		if err != nil {
			return fmt.Errorf("replace the live state of %s: %w", document.Document, err)
		}
		e.removed("doc_updates", replaced.RemovedUpdates)
		e.removed("doc_checkpoints", replaced.RemovedCheckpoints)
		e.removed("doc_snapshots", replaced.RemovedSnapshots)
		e.redacted("doc_updates", 1)
	}
	return nil
}

// treeSums is the key of the stored display tree of every whole text the run removes or rewrites:
// the node's own texts, the markdown of each version it deletes, each text of a block holding the
// node's block that the redaction rewrites (readAncestorTexts), each string of an event payload
// the redaction rewrites, and the live text of its document. text_trees keys a tree by the
// SHA-256 of its text's UTF-8 bytes (pmdoc.TextSum, migration 20261105121325) and holds the
// text's words in the tree, so each of those texts leaves a row holding the erased bytes once its
// own row is gone. It is read before anything goes, while the versions and the blocks still hold
// those texts. The text of a block holding the node's block at a version the node's block did not
// stand in (blockStoodIn) is one the run leaves, and its tree is that text's, which stays.
func (e *Erasure) treeSums(ctx context.Context, tx pgx.Tx, read erasedRead) ([][]byte, error) {
	if len(read.texts) == 0 {
		return nil, nil
	}
	whole := append([]string(nil), read.texts...)
	for _, event := range read.events {
		whole = append(whole, event.originals...)
	}
	for _, document := range e.Documents {
		if document.live != nil {
			whole = append(whole, document.live.Text)
		}
	}
	document := ""
	if read.document != nil {
		document = read.document.artifact
	}
	textBlocks := make([]string, len(read.blockTexts))
	textVersions := make([]int32, len(read.blockTexts))
	for index, text := range read.blockTexts {
		textBlocks[index], textVersions[index] = text.blockID, text.version
	}
	rows, err := tx.Query(ctx, `
		select sha256(convert_to(whole.text, 'UTF8')) from unnest($1::text[]) as whole (text)
		union
		select sha256(convert_to(v.markdown, 'UTF8')) from artifact_versions v
		  join unnest($2::uuid[], $3::int[]) as doomed (artifact, number)
		    on doomed.artifact = v.artifact_id and doomed.number = v.number
		 where v.markdown is not null
		union
		select sha256(convert_to(t.text, 'UTF8')) from document_block_texts t
		  join unnest($5::text[], $6::int[]) as rewritten (block_id, version)
		    on rewritten.block_id = t.block_id and rewritten.version = t.version
		 where t.artifact_id = nullif($4, '')::uuid and t.text is not null`,
		whole, read.artifacts, read.numbers, document, textBlocks, textVersions)
	if err != nil {
		return nil, fmt.Errorf("read the display trees of the texts of %s:%s: %w", e.Kind, e.ID, err)
	}
	sums, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, fmt.Errorf("read the display trees of the texts of %s:%s: %w", e.Kind, e.ID, err)
	}
	return sums, nil
}

// treeNeedles is, where the run cut a body an event of the node carried (erasedRead.cutBodies),
// every text node of the display tree of each of the node's own texts and of each such body, as a
// stored tree spells it: `"text":` and the node's text encoded as JSON. A body the comment held
// before an edit is held whole by no row once the edit's event cut it, so the key of its stored
// tree is the digest of a text the run never reads, and treeSums cannot name it. That tree holds
// the text nodes of the body's blocks, which comment_block_texts holds at that revision
// (readTexts), and of the head the event carried, so forgetTrees takes every tree that holds one
// of them. A tree of another text that holds one of those nodes goes too, and is made from its
// markdown at its next reading, as any tree forgetTrees deletes is. Where no event cut a body,
// every tree of the run's texts is keyed by one of the sums, and treeNeedles is empty.
func treeNeedles(read erasedRead) ([][]byte, error) {
	if len(read.cutBodies) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	var needles [][]byte
	var walk func(node any)
	walk = func(node any) {
		switch held := node.(type) {
		case []any:
			for _, child := range held {
				walk(child)
			}
		case map[string]any:
			if text, ok := held["text"].(string); ok && held["type"] == "text" && strings.TrimSpace(text) != "" {
				// json.Marshal spells the string as the stored tree does (pmdoc.DisplayTreeToStore):
				// no space after the colon, and <, > and & escaped. A string never fails to encode.
				encoded, _ := json.Marshal(text)
				if needle := append([]byte(`"text":`), encoded...); !seen[string(needle)] {
					seen[string(needle)] = true
					needles = append(needles, needle)
				}
			}
			for _, child := range held {
				walk(child)
			}
		}
	}
	for _, text := range slices.Concat(read.texts, read.cutBodies) {
		tree := pmdoc.DisplayTreeToStore(text, pmdoc.DisplayLatest)
		if tree == nil {
			continue
		}
		var decoded any
		if err := json.Unmarshal(tree, &decoded); err != nil {
			return nil, fmt.Errorf("read the display tree of a text of the erased node: %w", err)
		}
		walk(decoded)
	}
	return needles, nil
}

// deleteTreesBySumQuery deletes the stored display tree of every text whose sum is one of $1,
// through the table's key. deleteTreesHoldingQuery deletes every stored tree that holds one of
// the needles of $1 (treeNeedles), which reads every tree, so it runs only where a needle exists.
// Two statements and not one: an OR of the two would read every tree for every run.
const (
	deleteTreesBySumQuery   = `delete from text_trees where sum = any($1::bytea[])`
	deleteTreesHoldingQuery = `
		delete from text_trees t
		 where exists (select 1 from unnest($1::bytea[]) as needle (bytes) where position(needle.bytes in t.tree) > 0)`
)

// deleteTrees runs the two deletes through exec, a transaction's or a connection's, and counts
// the rows they removed.
func deleteTrees(ctx context.Context, exec func(context.Context, string, ...any) (pgconn.CommandTag, error), sums, needles [][]byte) (int64, error) {
	var removed int64
	if len(sums) > 0 {
		tag, err := exec(ctx, deleteTreesBySumQuery, sums)
		if err != nil {
			return 0, err
		}
		removed += tag.RowsAffected()
	}
	if len(needles) > 0 {
		tag, err := exec(ctx, deleteTreesHoldingQuery, needles)
		if err != nil {
			return 0, err
		}
		removed += tag.RowsAffected()
	}
	return removed, nil
}

// forgetTrees deletes the stored display tree of every text treeSums named, and every tree that
// holds a needle treeNeedles made. A reader of a text no row holds a tree for makes it from the
// markdown, as for a text written before the table existed (texttrees), so the cost of a row
// deleted here is one reading, and a text the run rewrote has the tree of what it now says made
// the first time it is read.
func (e *Erasure) forgetTrees(ctx context.Context, tx pgx.Tx, sums, needles [][]byte) error {
	removed, err := deleteTrees(ctx, tx.Exec, sums, needles)
	if err != nil {
		return fmt.Errorf("delete the display trees of the texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("text_trees", removed)
	return nil
}

// sweepTrees deletes once more, after the commit, the display trees forgetTrees deleted inside
// it. A read that began before the commit stores the trees it read on a goroutine of its own,
// detached from the request and bounded by api.StoreTreesTimeout (storeOf), so it can put the
// tree of a text the erase removed back after the commit. The sweep waits that bound out, which
// the caller passes in, then deletes those keys in a statement of its own, and counts what it
// found, so an operator reads whether the window was ever used.
//
// The keys and needles it deletes by are the ones the run held in memory (Erasure.sums and
// Erasure.needles): no digest or text node of an erased text is written anywhere to remember
// them, so a sweep that fails cannot be run again, and its error says so.
func (e *Erasure) sweepTrees(ctx context.Context, conn *pgx.Conn, bound time.Duration) error {
	if len(e.sums) == 0 && len(e.needles) == 0 {
		return nil
	}
	wait := time.NewTimer(bound)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		return fmt.Errorf("the erase committed, and its sweep of text_trees after the commit stopped "+
			"before it ran (a read that began before the commit may have stored a tree it removed; "+
			"no record keeps their keys, so the sweep cannot be run again): %w", ctx.Err())
	}
	swept, err := deleteTrees(ctx, conn.Exec, e.sums, e.needles)
	if err != nil {
		return fmt.Errorf("the erase committed, and its sweep of text_trees after the commit failed "+
			"(no record keeps their keys, so the sweep cannot be run again): %w", err)
	}
	e.Swept = swept
	return nil
}
