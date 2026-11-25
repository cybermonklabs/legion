package main

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// writeDocuments writes every document with its versions and indexes the latest version of each,
// the only version a block index records (refs.PlanBlocks) and the only one search, the
// related-item check and every anchor read look at.
func (r *seedRun) writeDocuments(ctx context.Context) error {
	marked, err := r.carriesNodeMarker(ctx, "artifacts")
	if err != nil {
		return err
	}
	return r.inBatches(ctx, "documents", len(r.plan.documents), batchDocuments,
		func(ctx context.Context, tx pgx.Tx, from, to int) error {
			for index := from; index < to; index++ {
				if err := r.writeDocument(ctx, tx, index, marked); err != nil {
					return fmt.Errorf("document %s: %w", r.plan.documents[index].RefKey, err)
				}
			}
			return nil
		})
}

// writeDocument writes one document, its versions and the index of its latest version. The
// latest version's markdown is the markdown the renderer wrote (RenderWithBlockOffsets), not the
// markdown the renderer was given, because refs.PlanBlocks indexes `markdown[block.From:block.To]`
// and those offsets are into that render: a row holding anything else would index the wrong
// bytes. Every document the seed writes is markdown, so its node carries no bytes values
// (graphstore.ArtifactBytes).
//
// marked is whether artifacts carries the node columns (carriesNodeMarker), which is whether the
// database is this release's or the one before it. On this release's, the document's row names its
// node, its node hangs under its issue, and each version is the projection of a version node, as
// this release's server writes them (the issue-tree spec). On the one before, each is written as
// that release's server writes it, a document's node and nothing of the tree, so the backfill this
// release runs over the data set writes the rest, and the sizing reads what it will write.
func (r *seedRun) writeDocument(ctx context.Context, tx pgx.Tx, index int, marked bool) error {
	document := r.plan.documents[index]
	var issueKey *string
	owner := ""
	if document.Issue >= 0 {
		issueKey = &r.plan.issues[document.Issue].Key
		owner = *issueKey
	}
	projectKey := r.plan.projects[document.Project].Key
	created := timeAt(index, len(r.plan.documents))
	// The document's node goes in before its row, in its project and part_of its issue where an
	// issue owns it, and the row names it, as the server's upload writes them (api's
	// storeArtifact); the seed records no event of a document, so what the node write states
	// carries the sentinel. Every node and tuple the document's write states has its head rows
	// written once, at the end (heads, below), once every row they read stands.
	refKey := graphstore.ArtifactRefKey(owner, projectKey, "", document.Slug)
	node := graphstore.ArtifactNode(refKey, projectKey, "", seedAuthor, created)
	var facts []graphstore.Tuple
	if marked && issueKey != nil {
		facts = append(facts, graphstore.ArtifactPartOf(refKey, *issueKey, seedAuthor, created))
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueWrite(b, []graphstore.Node{node}, facts)
		return nil
	}); err != nil {
		return fmt.Errorf("artifact node: %w", err)
	}
	columns := "id, issue_key, project_key, slug, name, kind, is_primary, created_by, created_at, last_seq"
	values := "$1, $2, $3, $4, $5, 'doc', $6, $7::jsonb, $8, 0"
	args := []any{document.ID, issueKey, projectKey, document.Slug,
		r.plan.vocabulary.phrase(r.random, 4), document.Primary, seedActor, created}
	if marked {
		columns += ", node_kind, node_address"
		values += ", $9, $10"
		args = append(args, graphstore.KindArtifact, refKey)
	}
	if _, err := tx.Exec(ctx, `insert into artifacts (`+columns+`) values (`+values+`)`, args...); err != nil {
		return fmt.Errorf("artifact row: %w", err)
	}

	var latest string
	var blocks []model.ArtifactBlock
	rows := make([][]any, 0, document.Versions)
	var versionNodes []graphstore.Node
	var versionFacts []graphstore.Tuple
	versionOwner := graphstore.VersionOwner{RefKey: refKey, Project: projectKey}
	for number := 1; number <= document.Versions; number++ {
		bytes, band := r.versionBytes(document, number)
		r.bands[band]++
		r.startDocument()
		markdown := r.plan.documentMarkdown(index, number, r.random, bytes)
		if number == document.Versions {
			rendered, offsets, tableRow, err := renderVersion(markdown)
			if err != nil {
				return err
			}
			if r.tableRow != "" && r.tableRow != tableRow {
				return fmt.Errorf("the table-row block id moved from %s to %s: every latest version is laid out alike, so an anchor written for one names the same row in each",
					r.tableRow, tableRow)
			}
			r.tableRow = tableRow
			latest, blocks = rendered, offsets
			markdown = rendered
		}
		versionCreated := created.Add(time.Duration(number-1) * versionStep)
		rows = append(rows, []any{
			uuidFor("artifact_version", index*versionsPerManyVersionDocument+number),
			document.ID, number, markdown, "[]", number%7 == 0, versionCreated,
		})
		if !marked {
			continue
		}
		versionNode, stated, ok := graphstore.VersionFacts(versionFacts, versionOwner, number, seedAuthor, versionCreated)
		if ok {
			versionNodes, versionFacts = append(versionNodes, versionNode), stated
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"artifact_versions"},
		[]string{"id", "artifact_id", "number", "markdown", "authors", "named", "created_at"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("version rows: %w", err)
	}

	versionAt := created.Add(time.Duration(document.Versions-1) * versionStep)
	located := text.ExtractAt(latest, serverURL)
	// No block pointers and no typed facts: a typed block's `block[]` attribute is what carries
	// a pointer, and a decision, a reading or an ask is what the typed-node pass reads, and every
	// document this tool writes is plain markdown with no typed block in it, so the lists the
	// server builds from a document's block tree are empty here.
	//
	// The index the version is planned against is the one the document holds, which is empty:
	// this is its first indexing, the rows being written here. The seed's transaction joins no
	// write phase of anyone's, so the plan's statements run at once, as a batch of their own
	// (refs.IndexBlocks). The targets the plan moves are counted by nobody: the seed appends no
	// event for a document write, which is what names them.
	_, _, blockWrite, _, err := refs.IndexBlocks(ctx, tx, document.ID, refs.OneVersion(document.ID, document.Versions, versionAt), latest, blocks, located, nil, refs.TypedFacts{})
	if err != nil {
		return fmt.Errorf("index blocks: %w", err)
	}
	// The seed records no event of a document's version, so what the index stated in the node
	// and tuple store carries the sentinel, the cause of a write no event recorded.
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, blockWrite, graphstore.NoEvent)
		return nil
	}); err != nil {
		return fmt.Errorf("record what the block index stated: %w", err)
	}
	// Each version's node, after the block nodes so the transaction's node rows stay in ascending
	// (kind, id), as the server's version write orders them (docs.WriteVersionNode).
	if len(versionNodes) > 0 {
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, versionNodes, versionFacts)
			return nil
		}); err != nil {
			return fmt.Errorf("version nodes: %w", err)
		}
	}
	// The version write records the blocks and marks the document owed the windows that stand
	// for them, which the block-index tick's window arm writes (refs.BlockIndexing.Queue), so
	// the seed writes them here as that arm would: a seeded database a server has to window is
	// one it would rewrite.
	if err := refs.WriteDocumentWindows(ctx, tx, document.ID, latest, blocks); err != nil {
		return fmt.Errorf("write the document's windows: %w", err)
	}
	// The document's own references are reconciled after its blocks, as the server's own version
	// write reconciles them (docs.indexRenderedVersion): the block mentions of a first indexing
	// date from the document's refs row for their target, so they are written while the document
	// holds no refs row yet, and take this version's time.
	replacement, err := refs.PlanReplace(ctx, tx, "artifact", document.ID, text.Refs(located), &versionAt)
	if err != nil {
		return fmt.Errorf("index references: %w", err)
	}
	// The excerpts are stored at this version number, which is the document's highest:
	// refs.MarkStaleExcerpts re-owes a mention whose excerpt_version is below
	// max(artifact_versions.number), so a seed that wrote 1 would leave every one of the
	// 28-version documents owed, which settle.go's owedWorkCounts refuses and a server started
	// on the data set would rewrite. They are queued after the reconciliation, whose rows the
	// update reads (refs.QueueRecordExcerpts).
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		refs.QueueRecordExcerpts(b, document.ID, document.Versions, refs.MentionExcerpts(latest, blocks, located))
		return nil
	}); err != nil {
		return fmt.Errorf("index references: %w", err)
	}
	// On this release's schema, what the reconciliation states of the references it writes is
	// appended at the sentinel, as the block index's is above. On the one before, the release
	// that wrote it states no mentions tuple, and its document rows name no node to state one
	// out of (refs' ArtifactMentions reads that node column), so the backfill states them.
	if marked {
		record, err := refs.ReadIndexedVersion(ctx, tx, document.ID, document.ID, document.Versions)
		if err != nil {
			return fmt.Errorf("read the version the references are stated at: %w", err)
		}
		mentions, err := replacement.ArtifactMentions(ctx, tx, record)
		if err != nil {
			return fmt.Errorf("index references: %w", err)
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueCaused(b, graphstore.Uncaused{Tuples: mentions}, graphstore.NoEvent)
			return nil
		}); err != nil {
			return fmt.Errorf("record what the references stated: %w", err)
		}
	}
	heads := slices.Concat(graphstore.HeadsOf([]graphstore.Node{node}, facts), graphstore.HeldHeads(blockWrite),
		graphstore.HeadsOf(versionNodes, versionFacts))
	if err := graphstore.WriteHeads(ctx, tx, heads); err != nil {
		return fmt.Errorf("write the head rows of the document: %w", err)
	}
	if document.Primary {
		if err := docs.RecordTaskProgressMarkdown(ctx, tx, document.ID, latest); err != nil {
			return fmt.Errorf("record task progress: %w", err)
		}
	}
	return nil
}

// versionStep is how far apart two versions of one document are written.
const versionStep = 36 * time.Hour

// renderVersion parses the generated markdown and renders it back with its blocks' offsets, the
// pair a version write stores and indexes together (docs' indexDocument). It also answers the
// block id of the first table's first body row, which is what an anchor on a table row names.
//
// Only a block the renderer writes through its block path carries an offset, so a table is one
// block and its rows and its cells are none, although pmdoc stamps an id on each of them
// (EnsureBlockIDsCount). That is why a table-row anchor names a block id that `document_blocks`
// holds no row for, exactly as a production anchor on a table row does.
//
// It refuses a render whose shape is not the one the seed lays out, so a change in pmdoc that
// moved a block shows up here rather than as a wrong anchor in the data set.
func renderVersion(markdown string) (string, []model.ArtifactBlock, string, error) {
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		return "", nil, "", fmt.Errorf("parse the generated markdown: %w", err)
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return "", nil, "", fmt.Errorf("render the generated markdown: %w", err)
	}
	if len(offsets) != blocksPerLatestVersion {
		return "", nil, "", fmt.Errorf("the latest version rendered %d blocks, not the %d the seed lays out",
			len(offsets), blocksPerLatestVersion)
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	tables := 0
	firstTable := ""
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
		if offset.Type == "table" {
			tables++
			if firstTable == "" {
				firstTable = offset.ID
			}
		}
	}
	if blocks[1].ID != firstParagraphBlock || blocks[1].Type != "paragraph" || tables != tablesPerVersion {
		return "", nil, "", fmt.Errorf("the latest version's blocks are not laid out as the seed's mentions expect: block 2 is %s of type %s, and it holds %d tables",
			blocks[1].ID, blocks[1].Type, tables)
	}
	rows, err := pmdoc.TableRows(tree)
	if err != nil {
		return "", nil, "", fmt.Errorf("read the table rows of the generated markdown: %w", err)
	}
	if len(rows[firstTable]) < 2 {
		return "", nil, "", fmt.Errorf("the first table of the latest version has no body row")
	}
	return rendered, blocks, rows[firstTable][1].ID, nil
}

// versionBytes is how much markdown version number of document holds, and the band the report
// counts it in. Only the latest version of a large document is large: every other version of it
// is an ordinary one. The other reading is about 4 GB of markdown, which does not fit the disk
// the measurement cluster runs on (the plan's Decision "The large documents are large only at
// their latest version").
func (r *seedRun) versionBytes(document documentPlan, number int) (int, string) {
	if document.Large && number == document.Versions {
		return largeBandMin + r.random.IntN(largeBandMax-largeBandMin), "100-660 KB"
	}
	switch draw := r.random.Float64(); {
	case draw < 0.5:
		return versionBandMin + r.random.IntN(4<<10), "1-5 KB"
	case draw < 0.8:
		return 5<<10 + r.random.IntN(5<<10), "5-10 KB"
	default:
		return 10<<10 + r.random.IntN(10<<10), "10-20 KB"
	}
}
