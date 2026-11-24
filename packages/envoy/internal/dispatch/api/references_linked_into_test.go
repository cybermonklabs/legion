package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The sizes of TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks' seed: the issue's
// own documents and the current blocks each one holds, 2,000 blocks, and the other documents and
// the blocks each one holds, 10,000 blocks, each of which mentions one of the 2,000.
const (
	linkedIntoOwnDocuments     = 40
	linkedIntoOwnBlocks        = 50
	linkedIntoLinkingDocuments = 500
	linkedIntoLinkingBlocks    = 20
)

// The references read of an issue that 10,000 blocks of other documents link into answers 200
// through the server's own route, inside its 2 s budget (referencesClosureBound), and linked_from
// lists each of the 500 documents that hold those blocks once.
//
// The seed stands for production, where 17 issues never got a 200 from the route and 57 of its 65
// statement timeouts were in its in statement (refs' linksInQuery). The issue holds its spec and
// 40 documents of 50 blocks, so its own targets are 2,042 - the issue, its 41 documents and their
// 2,000 blocks - one more than the 2,041 of the issue measured on a copy of a dispatch:local
// database seeded at production's size (block_refs 876,018 rows). There the in statement read
// 5,000 links into it in 3.44-3.48 s while it left the issue's own targets out by a test against
// an array, and in 0.27-0.36 s by the anti-join it reads now (refs' oneRowPerNode). Here 500
// documents of 20 blocks link into the issue's documents, each block mentioning one of their 2,000
// blocks: 10,000 links, 5 into each block. The server's sessions run with JIT off, as production's
// Cloud SQL runs every statement: it does not support PostgreSQL's LLVM JIT.
//
// seedLinkedInto writes the seed. One document of each side is written through the API, and the
// rest are copies of its rows written in SQL (seedDocumentCopies), so the rows the read reads are
// as the server's own writes make them. The database is then vacuumed and analyzed, as
// dispatch-seed leaves the database it seeds.
//
// Negative control: with oneRowPerNode's exclusion written back as the array, `where not
// (row(f.node_kind, f.node_id) = any(array(select row(t.kind, t.id) from targets t)))` in the in
// statement and its `own` counterpart in the out statement, the read answers 503
// REFERENCES_TIMEOUT and the test fails at its status.
func TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks(t *testing.T) {
	seed := seedLinkedInto(t)

	started := time.Now()
	response := issueReferencesRequest(t, seed.handler, seed.issue.Key, "")
	took := time.Since(started)
	t.Logf("the references read of %s, which %d blocks link into, answered %d in %s", seed.issue.Key, seed.linksInto, response.Code, took.Round(time.Millisecond))
	if response.Code != http.StatusOK {
		t.Fatalf("references of an issue %d blocks link into: status=%d after %s body=%s, want 200 inside the %s budget",
			seed.linksInto, response.Code, took.Round(time.Millisecond), response.Body.String(), referencesClosureBound)
	}
	answer := decodeBody[model.IssueReferences](t, response)
	listed := map[string]int{}
	var others []string
	for _, row := range answer.LinkedFrom {
		if row.Node.Kind != "artifact" || !seed.linking[row.Node.ID] {
			others = append(others, row.Node.Kind+" "+row.Node.ID)
			continue
		}
		listed[row.Node.ID]++
	}
	var missing, repeated int
	for id := range seed.linking {
		switch count := listed[id]; {
		case count == 0:
			missing++
		case count > 1:
			repeated++
		}
	}
	if missing != 0 || repeated != 0 || len(others) != 0 {
		t.Errorf("linked_from holds %d rows: %d of the %d linking documents missing, %d listed more than once, and %d other nodes %v; want each linking document once",
			len(answer.LinkedFrom), missing, len(seed.linking), repeated, len(others), others)
	}
}

// linkedIntoSeed is what seedLinkedInto seeded: the server that serves it, through handler, and
// its store, whose every session runs with JIT off; the issue; how many block links reach the
// issue's blocks; and the id of each document that holds those links.
type linkedIntoSeed struct {
	handler   http.Handler
	server    *server
	store     *store.Store
	issue     model.Issue
	linksInto int
	linking   map[string]bool
}

// seedLinkedInto writes the seed TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks
// describes, which TestTheReferencesInStatementLeavesTheIssuesOwnNodesOutByAnAntiJoin reads too,
// on a database of its own whose server reads it with JIT off: an issue whose 40 documents beside
// its spec hold 50 blocks each, and 500 other documents of 20 blocks, each block mentioning one of
// the issue's 2,000. It fails t unless the issue's documents hold 2,000 blocks and 10,000 block
// links reach them.
func seedLinkedInto(t *testing.T) linkedIntoSeed {
	t.Helper()
	ctx := context.Background()
	// JIT is off for the test database, and the pool the server reads through opens its first
	// session after that, so every session of the server reads it.
	database := storetest.Open(t)
	var name string
	if err := database.Pool.QueryRow(ctx, `select current_database()`).Scan(&name); err != nil {
		t.Fatalf("read the test database's name: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `alter database `+pgx.Identifier{name}.Sanitize()+` set jit = off`); err != nil {
		t.Fatalf("turn JIT off for the test database: %v", err)
	}
	served := storetest.Reopened(t, database, func(*pgxpool.Config) {})
	handler, _, _, registered := newRegisteredTestServer(t, testServerOptions{store: served})
	var jit string
	if err := served.Pool.QueryRow(ctx, `select current_setting('jit')`).Scan(&jit); err != nil || jit != "off" {
		t.Fatalf("the server's sessions read jit = %q (%v), want off", jit, err)
	}

	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")

	// The issue's own documents: own-1.md through the API, and own-2.md to own-40.md its copies.
	// own lists their blocks, document by document, each in its place in its document.
	ownTexts := make([]string, linkedIntoOwnBlocks)
	for index := range ownTexts {
		ownTexts[index] = "Block " + strconv.Itoa(index+1) + " of a document of the issue's own."
	}
	ownTemplate := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "own-1.md", paragraphsMarkdown(ownTexts)).Artifact
	ownTemplateBlocks := templateBlockIDs(t, served, ownTemplate.ID, ownTexts)
	type ownBlock struct{ slug, refKey, id string }
	own := make([]ownBlock, 0, linkedIntoOwnDocuments*linkedIntoOwnBlocks)
	for _, id := range ownTemplateBlocks {
		own = append(own, ownBlock{ownTemplate.Slug, ownTemplate.RefKey, id})
	}
	ownCopies := make([]seededDocument, 0, linkedIntoOwnDocuments-1)
	for number := 2; number <= linkedIntoOwnDocuments; number++ {
		slug := "own-" + strconv.Itoa(number) + "-md"
		document := seededDocument{
			id: uuid.NewString(), slug: slug, name: "own-" + strconv.Itoa(number) + ".md",
			refKey: issue.Key + "/" + slug, markdown: paragraphsMarkdown(ownTexts),
		}
		for index, text := range ownTexts {
			block := seededBlock{template: ownTemplateBlocks[index], id: uuid.NewString(), text: text}
			document.blocks = append(document.blocks, block)
			own = append(own, ownBlock{slug, document.refKey, block.id})
		}
		ownCopies = append(ownCopies, document)
	}

	// The linking documents: Linking document 1 through the API, and the 499 others its copies.
	// Block j of document k mentions the block own[(k*20+j) mod 2,000].
	linkTexts := func(document int) (texts, targets []string) {
		for index := range linkedIntoLinkingBlocks {
			target := own[(document*linkedIntoLinkingBlocks+index)%len(own)]
			texts = append(texts, "Block "+strconv.Itoa(index+1)+" cites dispatch://"+issue.Key+"/artifact/"+target.slug+"#b-"+target.id+" here.")
			targets = append(targets, target.refKey+"#"+target.id)
		}
		return texts, targets
	}
	templateTexts, _ := linkTexts(0)
	linkingTemplate := createProjectDocument(t, handler, "OPS", "Linking document 1", paragraphsMarkdown(templateTexts))
	linkingTemplateBlocks := templateBlockIDs(t, served, linkingTemplate.ID, templateTexts)
	linking := map[string]bool{linkingTemplate.ID: true}
	linkingCopies := make([]seededDocument, 0, linkedIntoLinkingDocuments-1)
	for document := 1; document < linkedIntoLinkingDocuments; document++ {
		texts, targets := linkTexts(document)
		number := strconv.Itoa(document + 1)
		copied := seededDocument{
			id: uuid.NewString(), slug: "linking-document-" + number, name: "Linking document " + number,
			refKey: "OPS/linking-document-" + number, markdown: paragraphsMarkdown(texts),
		}
		for index, text := range texts {
			copied.blocks = append(copied.blocks, seededBlock{template: linkingTemplateBlocks[index], id: uuid.NewString(), text: text, target: targets[index]})
		}
		linking[copied.id] = true
		linkingCopies = append(linkingCopies, copied)
	}

	seeded := time.Now()
	seedDocumentCopies(t, served, ownTemplate, ownCopies)
	seedDocumentCopies(t, served, linkingTemplate, linkingCopies)
	if _, err := served.Pool.Exec(ctx, `vacuum (analyze)`); err != nil {
		t.Fatalf("vacuum and analyze the seeded database: %v", err)
	}
	var ownBlocks, linksInto int
	if err := served.Pool.QueryRow(ctx, `
		select (select count(*) from document_blocks d join artifacts a on a.id = d.artifact_id where a.issue_key = $1),
		       (select count(*) from block_refs r
		        join document_blocks d on d.node_id = r.to_id
		        join artifacts a on a.id = d.artifact_id
		        where a.issue_key = $1 and r.to_kind = 'block' and r.kind = 'mentions')
	`, issue.Key).Scan(&ownBlocks, &linksInto); err != nil {
		t.Fatalf("count the seed's blocks and links: %v", err)
	}
	if want := linkedIntoOwnDocuments * linkedIntoOwnBlocks; ownBlocks != want {
		t.Fatalf("the issue's documents hold %d blocks, want %d", ownBlocks, want)
	}
	if want := linkedIntoLinkingDocuments * linkedIntoLinkingBlocks; linksInto != want {
		t.Fatalf("%d block links reach the issue's blocks, want %d", linksInto, want)
	}
	t.Logf("seed: %d own blocks, %d links into them from %d documents, copied and vacuumed in %s",
		ownBlocks, linksInto, len(linking), time.Since(seeded).Round(time.Millisecond))
	return linkedIntoSeed{handler: handler, server: registered, store: served, issue: issue, linksInto: linksInto, linking: linking}
}

// paragraphsMarkdown is the markdown of a document of texts, one paragraph each.
func paragraphsMarkdown(texts []string) string {
	return strings.Join(texts, "\n\n") + "\n"
}

// templateBlockIDs is the id of the block the API wrote for each of texts, in their order, in the
// document id, a document of those paragraphs alone. Each block's excerpt is its whole text, its
// text_sha256 the SHA-256 of that text, and it sits at the document's top level, which is how
// seedDocumentCopies writes a copy's blocks; a block that is not so fails the test.
func templateBlockIDs(t *testing.T, database *store.Store, id string, texts []string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, excerpt,
		       text_sha256 = encode(sha256(convert_to(excerpt, 'UTF8')), 'hex') and parent_block_id is null
		from document_blocks where artifact_id = $1
	`, id)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", id, err)
	}
	defer rows.Close()
	places := make(map[string]int, len(texts))
	for index, text := range texts {
		places[text] = index
	}
	ids := make([]string, len(texts))
	for rows.Next() {
		var blockID, excerpt string
		var copiable bool
		if err := rows.Scan(&blockID, &excerpt, &copiable); err != nil {
			t.Fatalf("scan a block of %s: %v", id, err)
		}
		index, found := places[excerpt]
		if !found || !copiable || ids[index] != "" {
			t.Fatalf("block %s of %s, %q, is not one top-level block of a paragraph its document was written with, whose excerpt is its text and whose text_sha256 is that text's SHA-256", blockID, id, excerpt)
		}
		ids[index] = blockID
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", id, err)
	}
	for index, blockID := range ids {
		if blockID == "" {
			t.Fatalf("%s holds no block of its paragraph %q", id, texts[index])
		}
	}
	return ids
}

// seededDocument is a copy seedDocumentCopies writes of a document the API wrote: its uuid, slug,
// name and ref_key, the markdown of its one version, and its blocks.
type seededDocument struct {
	id, slug, name, refKey, markdown string
	blocks                           []seededBlock
}

// seededBlock is a block of a seededDocument: the template's block in its place, its own id, its
// text, and the node id of the block it mentions, where it mentions one.
type seededBlock struct {
	template, id, text, target string
}

// seedDocumentCopies writes copies, each a copy of the rows the API wrote for template, in one
// transaction. Each copy takes the template's rows with its own uuid, slug, name, ref_key and
// markdown, its own block ids, and each block's own text and the target it mentions: the artifacts
// row; the store's nodes of the document and its blocks, which the rows of its blocks and their
// links name, and its tuples about them (graph_nodes, graph_tuples); the version, with no render
// stamp, since a copy holds no document state of its own (doc_updates) to have rendered it from,
// as a version an upload writes without one does; the rows the version's triggers write, left as
// the template's write left them (document_block_versions, refs_grammar_owed);
// document_indexed_versions; the blocks and their texts, a block's node id being its document's
// ref_key, '#' and its block id; and each block's links (block_refs) and the document's mention of
// each target (refs). It writes no event.
func seedDocumentCopies(t *testing.T, database *store.Store, template model.Artifact, copies []seededDocument) {
	t.Helper()
	ctx := context.Background()
	documents := make([][]any, 0, len(copies))
	var blocks [][]any
	for _, document := range copies {
		documents = append(documents, []any{document.id, document.slug, document.name, document.refKey, document.markdown})
		for _, block := range document.blocks {
			var target any
			if block.target != "" {
				target = block.target
			}
			blocks = append(blocks, []any{document.id, document.refKey, block.template, block.id, document.refKey + "#" + block.id, block.text, target})
		}
	}
	steps := []struct {
		what, sql string
		args      []any
	}{
		{"the documents", `
			insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by, created_at, project_key, ref_key, last_seq, session_id, role)
			select d.id::uuid, a.issue_key, d.slug, d.name, a.kind, a.is_primary, a.created_by, a.created_at, a.project_key, d.ref_key, a.last_seq, a.session_id, a.role
			from seeded_documents d cross join artifacts a where a.id = $1`, []any{template.ID}},
		{"the store's nodes", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
			select n.kind, d.ref_key, n.supertag, n.visibility, n.state, n.attributes, n.made_at, n.made_by, n.source_event
			from seeded_documents d join graph_nodes n on n.kind = 'artifact' and n.id = $1::text
			union all
			select n.kind, b.node_id, n.supertag, n.visibility, n.state, n.attributes, n.made_at, n.made_by, n.source_event
			from seeded_blocks b join graph_nodes n on n.kind = 'block' and n.id = $1::text || '#' || b.template_block`, []any{template.RefKey}},
		{"the store's tuples", `
			insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, legacy_source_seq, asserted)
			select g.from_kind, d.ref_key, g.kind, g.to_kind, g.to_id, g.at, g.who, g.source_event, g.legacy_source_seq, g.asserted
			from seeded_documents d join graph_tuples g on g.from_kind = 'artifact' and g.from_id = $1::text
			union all
			select g.from_kind, b.node_id, g.kind, g.to_kind,
			       case when g.to_id = $1::text then b.ref_key when g.to_kind = 'block' then b.target else g.to_id end,
			       g.at, g.who, g.source_event, g.legacy_source_seq, g.asserted
			from seeded_blocks b join graph_tuples g on g.from_kind = 'block' and g.from_id = $1::text || '#' || b.template_block`, []any{template.RefKey}},
		{"the versions", `
			insert into artifact_versions (artifact_id, number, markdown, content, mime, size, sha256, authors, named, summary, created_at, doc_update_version)
			select d.id::uuid, v.number, d.markdown, v.content, v.mime, v.size, v.sha256, v.authors, v.named, v.summary, v.created_at, v.doc_update_version
			from seeded_documents d cross join artifact_versions v where v.artifact_id = $1`, []any{template.ID}},
		{"the block index's version", `
			insert into document_block_versions (artifact_id, version, windows_owed_at, store_version, store_owed_at, store_owed_rule)
			select d.id::uuid, v.version, v.windows_owed_at, v.store_version, v.store_owed_at, v.store_owed_rule
			from seeded_documents d cross join document_block_versions v where v.artifact_id = $1
			on conflict (artifact_id) do update set version = excluded.version, windows_owed_at = excluded.windows_owed_at,
				store_version = excluded.store_version, store_owed_at = excluded.store_owed_at, store_owed_rule = excluded.store_owed_rule`, []any{template.ID}},
		{"the grammar index's debts", `
			delete from refs_grammar_owed o using seeded_documents d
			where o.id = d.id::uuid and o.kind not in (select t.kind from refs_grammar_owed t where t.id = $1)`, []any{template.ID}},
		{"the indexed versions", `
			insert into document_indexed_versions (artifact_id, version)
			select d.id::uuid, v.version from seeded_documents d cross join document_indexed_versions v where v.artifact_id = $1`, []any{template.ID}},
		{"the blocks", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, created_at, node_kind, node_address, parent_block_id)
			select b.document::uuid, b.block_id, b.node_id, t.type, encode(sha256(convert_to(b.text, 'UTF8')), 'hex'), b.text,
			       t.changed_version, t.changed_at, t.created_at, t.node_kind, case when t.node_kind is not null then b.node_id end, t.parent_block_id
			from seeded_blocks b join document_blocks t on t.artifact_id = $1 and t.block_id = b.template_block`, []any{template.ID}},
		{"the blocks' texts", `
			insert into document_block_texts (artifact_id, block_id, version, text)
			select b.document::uuid, b.block_id, t.version, b.text
			from seeded_blocks b join document_block_texts t on t.artifact_id = $1 and t.block_id = b.template_block`, []any{template.ID}},
		{"the blocks' links", `
			insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_at, created_version, source_seq, to_types, node_kind, node_address)
			select b.document::uuid, b.block_id, b.node_id, r.to_kind, b.target, r.kind, r.created_at, r.created_version, r.source_seq, r.to_types,
			       r.node_kind, case when r.node_kind is not null then b.node_id end
			from seeded_blocks b join block_refs r on r.artifact_id = $1 and r.block_id = b.template_block`, []any{template.ID}},
		{"the documents' mentions", `
			insert into refs (from_kind, from_id, to_kind, to_id, kind, created_at, source_seq, excerpt_block_id, excerpt_text, excerpt_version)
			select r.from_kind, b.document, r.to_kind, b.target, r.kind, r.created_at, r.source_seq, b.block_id, b.text, r.excerpt_version
			from seeded_blocks b join refs r on r.from_kind = 'artifact' and r.from_id = $1::text and r.excerpt_block_id = b.template_block`, []any{template.ID}},
	}
	err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			create temp table seeded_documents (id text, slug text, name text, ref_key text, markdown text) on commit drop;
			create temp table seeded_blocks (document text, ref_key text, template_block text, block_id text, node_id text, text text, target text) on commit drop
		`); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"seeded_documents"}, []string{"id", "slug", "name", "ref_key", "markdown"}, pgx.CopyFromRows(documents)); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"seeded_blocks"}, []string{"document", "ref_key", "template_block", "block_id", "node_id", "text", "target"}, pgx.CopyFromRows(blocks)); err != nil {
			return err
		}
		for _, step := range steps {
			if _, err := tx.Exec(ctx, step.sql, step.args...); err != nil {
				return fmt.Errorf("copy %s: %w", step.what, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("copy %s %d times: %v", template.RefKey, len(copies), err)
	}
}
