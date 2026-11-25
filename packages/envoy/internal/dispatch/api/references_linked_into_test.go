package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The blocks each of the issue's own documents holds and the blocks each linking document holds in
// an issue seedLinkedIntoIssue seeds; each block of a linking document mentions one of the issue's.
const (
	linkedIntoOwnBlocks     = 50
	linkedIntoLinkingBlocks = 20
)

// referencesGrowthBound is the most refs.Closure's read of the issue 10,000 blocks link into may
// take, as a multiple of its read of the issue 2,000 blocks link into, each time the median of 3
// reads that no budget cuts short (closureRead), in
// TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks. It is set from that ratio as
// measured on 2026-11-22 with the test's two issues. The anti-join the read leaves the issue's own
// targets out by (refs' oneRowPerNode) gave 2.87 and 3.46 in the implementer's pod (2 CPUs) and
// 3.59, 2.86 and 3.37 on the build farm (a Job of 12 CPUs running PostgreSQL 18). The array
// exclusion of the test's negative control gave 11.52 and 12.36 in the pod and 14.84, 14.84 and
// 14.23 on the build farm that day, and from 13.21 to 16.33 in its build-farm runs from
// 2026-11-22 to 2026-11-24. 6.5 is about the geometric middle of the anti-join's highest ratio and
// the array's lowest: 1.81 times the one, and the other 1.77 times it.
const referencesGrowthBound = 6.5

// The references read of an issue that 10,000 blocks of other documents link into answers 200
// through the server's own route inside its 2 s budget (referencesClosureBound), and linked_from
// lists each of the 500 documents that hold those blocks once; and the read's time grows with the
// links it reads, not with the links times the issue's own targets.
//
// The seed stands for production, where 17 issues never got a 200 from the route and 57 of its 65
// statement timeouts were in its in statement (refs' linksInQuery). The issue holds its spec and
// 40 documents of 50 blocks, so its own targets are 2,042 - the issue, its 41 documents and their
// 2,000 blocks - about the size of the issue measured on a copy seeded at production's size; refs'
// oneRowPerNode's comment gives the read's times there and on this seed. Here 500 documents of 20
// blocks link into the issue's documents, each block mentioning one of their 2,000 blocks: 10,000
// links, 5 into each block. The server's sessions run with JIT off, as production's Cloud SQL runs
// every statement: it does not support PostgreSQL's LLVM JIT.
//
// The budget alone measures the machine as much as the query: the negative control below reads that
// issue in 0.9 s on the build farm, inside it, and in about 6 s in the implementer's pod. So the
// test also seeds an issue a fifth the size: its spec and 8 documents of 50 blocks, so 410 own
// targets, and 100 documents of 20 blocks that link into them, 2,000 links, 5 into each block. A
// read linear in its links reads 5 times the rows at the large issue as at the small one; a test of
// each link against every own target makes 10,000 × 2,042 comparisons at the one and 2,000 × 410 at
// the other, 25 times as many. refs.Closure, the read the route makes, reads each issue 3 times,
// the two in turn, with no budget (closureRead), and the median of the large issue's reads is at
// most referencesGrowthBound times the median of the small one's. The route then reads the large
// issue 3 times, and their median answers 200 inside the budget: one read a slow spell of the
// machine pushes past it does not fail the test, and two do. Each issue has a database and a server
// of its own (seedLinkedIntoIssue).
//
// One document of each side is written through the API, and the rest are copies of its rows
// written in SQL (seedDocumentCopies), so the rows the read reads are as the server's own writes
// make them. The database is then vacuumed and analyzed, as dispatch-seed leaves the database it
// seeds.
//
// Negative control: with oneRowPerNode's exclusion written back as the array, `where not
// (row(f.node_kind, f.node_id) = any(array(select row(x.kind, x.id) from ` + own + ` x)))`, the
// test fails at the ratio: 11.52 and 12.36 in the implementer's pod (2026-11-22), where it also
// fails at the budget, every route read answering 503 REFERENCES_TIMEOUT, and from 13.21 to 16.33
// on the build farm (2026-11-22 to 2026-11-24), where every route read answers 200 inside the
// budget: the runs referencesGrowthBound's comment names.
// TestTheReferencesInStatementLeavesTheIssuesOwnNodesOutByAnAntiJoin holds the in statement's plan
// to the anti-join, so it fails on the array whatever the machine's speed.
func TestTheReferencesReadAnswersAnIssueLinkedIntoByTenThousandBlocks(t *testing.T) {
	// Not parallel: it holds the route's reads to their 2 s budget (referencesClosureBound) and
	// refs.Closure's read times to a ratio (referencesGrowthBound), wall clock that the load of the
	// tests beside it can push past.
	small := seedLinkedIntoIssue(t, 8, 100)
	large := seedLinkedIntoIssue(t, 40, 500)
	const reads = 3

	// The growth, on reads no budget cuts short (closureRead); the two issues are read in turn, so
	// that a slow spell of the machine falls on both.
	smallTook := make([]time.Duration, 0, reads)
	largeTook := make([]time.Duration, 0, reads)
	for number := 1; number <= reads; number++ {
		smallRead, largeRead := closureRead(t, small), closureRead(t, large)
		smallTook, largeTook = append(smallTook, smallRead), append(largeTook, largeRead)
		t.Logf("closure read %d: %s for the issue %d blocks link into, %s for the issue %d blocks link into",
			number, smallRead.Round(time.Millisecond), small.links, largeRead.Round(time.Millisecond), large.links)
	}
	smallMedian, largeMedian := medianTook(smallTook), medianTook(largeTook)
	ratio := float64(largeMedian) / float64(smallMedian)
	if ratio > referencesGrowthBound {
		t.Errorf("refs.Closure's read of an issue %d blocks link into took a median %s of %d reads, %.2f times the median %s of an issue %d blocks link into; want at most %v times (referencesGrowthBound)",
			large.links, largeMedian.Round(time.Millisecond), reads, ratio, smallMedian.Round(time.Millisecond), small.links, referencesGrowthBound)
	} else {
		t.Logf("median of %d closure reads: %s for the issue %d blocks link into, %s for the issue %d blocks link into, %.2f times as long, at most %v",
			reads, smallMedian.Round(time.Millisecond), small.links, largeMedian.Round(time.Millisecond), large.links, ratio, referencesGrowthBound)
	}

	// The budget, on the route's reads of the large issue: their median, so at least 2 of the 3,
	// answers 200 inside it, a read that answers anything else counting as one past it.
	var answer *httptest.ResponseRecorder
	var inside int
	routeReads := make([]string, 0, reads)
	for range reads {
		started := time.Now()
		response := issueReferencesRequest(t, large.handler, large.key, "")
		took := time.Since(started)
		described := strconv.Itoa(response.Code) + " in " + took.Round(time.Millisecond).String()
		if response.Code != http.StatusOK {
			described += " " + strings.TrimSpace(response.Body.String())
		}
		routeReads = append(routeReads, described)
		if response.Code == http.StatusOK && took <= referencesClosureBound {
			inside++
			if answer == nil {
				answer = response
			}
		}
	}
	if inside <= reads/2 {
		t.Errorf("the route's %d reads of the references of an issue %d blocks link into answered %s; want their median to answer 200 inside the %s budget",
			reads, large.links, strings.Join(routeReads, "; "), referencesClosureBound)
	} else {
		t.Logf("the route's %d reads of the references of the issue %d blocks link into answered %s", reads, large.links, strings.Join(routeReads, "; "))
	}

	if answer == nil {
		return
	}
	references := decodeBody[model.IssueReferences](t, answer)
	listed := map[string]int{}
	var others []string
	for _, row := range references.LinkedFrom {
		if row.Node.Kind != "artifact" || !large.linking[row.Node.ID] {
			others = append(others, row.Node.Kind+" "+row.Node.ID)
			continue
		}
		listed[row.Node.ID]++
	}
	var missing, repeated int
	for id := range large.linking {
		switch count := listed[id]; {
		case count == 0:
			missing++
		case count > 1:
			repeated++
		}
	}
	if missing != 0 || repeated != 0 || len(others) != 0 {
		t.Errorf("linked_from holds %d rows: %d of the %d linking documents missing, %d listed more than once, and %d other nodes %v; want each linking document once",
			len(references.LinkedFrom), missing, len(large.linking), repeated, len(others), others)
	}
}

// linkedIntoReader is who the test's reads read as: alice, as the route records the person its
// requests name (issueReferencesRequest).
var linkedIntoReader = model.Actor{Kind: "user", ID: "alice", Proven: true}

// closureRead reads the references of issue as the route reads them, refs.Closure for alice in a
// transaction of its own on the issue's database, but with no budget (the route's boundedClosure),
// so that the read runs to its end however long it takes, and answers the time refs.Closure took.
// It fails t unless the read lists each of the issue's linking documents in linked_from.
func closureRead(t *testing.T, issue linkedIntoIssue) time.Duration {
	t.Helper()
	ctx := context.Background()
	tx, err := issue.served.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the references read of %s: %v", issue.key, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	started := time.Now()
	references, err := refs.Closure(ctx, tx, linkedIntoReader, issue.key, "")
	took := time.Since(started)
	if err != nil {
		t.Fatalf("refs.Closure's read of the references of %s: %v", issue.key, err)
	}
	if len(references.LinkedFrom) != len(issue.linking) {
		t.Fatalf("refs.Closure's read of the references of %s lists %d nodes in linked_from, want its %d linking documents",
			issue.key, len(references.LinkedFrom), len(issue.linking))
	}
	return took
}

// medianTook is the middle of an odd number of read times.
func medianTook(times []time.Duration) time.Duration {
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// linkedIntoIssue is an issue seedLinkedIntoIssue seeded: the server that reads its database,
// through handler, and the store it reads through, whose every session runs with JIT off; the
// issue's key; the number of block links into its blocks; and the uuids of the documents that hold
// those links.
type linkedIntoIssue struct {
	handler http.Handler
	server  *server
	served  *store.Store
	key     string
	links   int
	linking map[string]bool
}

// seedLinkedIntoIssue seeds, in a database of its own, an issue of project CORE that holds
// ownDocuments documents of linkedIntoOwnBlocks blocks, and linkingDocuments documents of project
// OPS of linkedIntoLinkingBlocks blocks, each block of which mentions one of the issue's blocks,
// the issue's blocks in turn; and it starts a server on that database. It fails t unless the
// issue's documents hold ownDocuments × linkedIntoOwnBlocks blocks and the block links into them
// are linkingDocuments × linkedIntoLinkingBlocks.
func seedLinkedIntoIssue(t *testing.T, ownDocuments, linkingDocuments int) linkedIntoIssue {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	served := storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.ConnConfig.RuntimeParams["jit"] = "off"
	})
	handler, _, _, registered := newRegisteredTestServer(t, testServerOptions{store: served})
	var jit string
	if err := served.Pool.QueryRow(ctx, `select current_setting('jit')`).Scan(&jit); err != nil || jit != "off" {
		t.Fatalf("the server's sessions read jit = %q (%v), want off", jit, err)
	}

	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	issue := createReferenceAPIIssue(t, handler, "CORE")

	// The issue's own documents: own-1.md through the API, and the others its copies. own lists
	// their blocks, document by document, each in its place in its document.
	ownTexts := make([]string, linkedIntoOwnBlocks)
	for index := range ownTexts {
		ownTexts[index] = "Block " + strconv.Itoa(index+1) + " of a document of the issue's own."
	}
	ownTemplate := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "own-1.md", paragraphsMarkdown(ownTexts)).Artifact
	ownTemplateBlocks := templateBlockIDs(t, served, ownTemplate.ID, ownTexts)
	type ownBlock struct{ slug, refKey, id string }
	own := make([]ownBlock, 0, ownDocuments*linkedIntoOwnBlocks)
	for _, id := range ownTemplateBlocks {
		own = append(own, ownBlock{ownTemplate.Slug, ownTemplate.RefKey, id})
	}
	ownCopies := make([]seededDocument, 0, ownDocuments-1)
	for number := 2; number <= ownDocuments; number++ {
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

	// The linking documents: Linking document 1 through the API, and the others its copies.
	// Block j of document k mentions the block own[(k*linkedIntoLinkingBlocks+j) mod len(own)].
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
	linkingCopies := make([]seededDocument, 0, linkingDocuments-1)
	for document := 1; document < linkingDocuments; document++ {
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
	if want := ownDocuments * linkedIntoOwnBlocks; ownBlocks != want {
		t.Fatalf("the issue's documents hold %d blocks, want %d", ownBlocks, want)
	}
	if want := linkingDocuments * linkedIntoLinkingBlocks; linksInto != want {
		t.Fatalf("%d block links reach the issue's blocks, want %d", linksInto, want)
	}
	t.Logf("seed: %d own blocks, %d links into them from %d documents, copied and vacuumed in %s",
		ownBlocks, linksInto, len(linking), time.Since(seeded).Round(time.Millisecond))
	return linkedIntoIssue{handler: handler, server: registered, served: served, key: issue.Key, links: linksInto, linking: linking}
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
			insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by, created_at, project_key, ref_key, last_seq, session_id, role, retired_at)
			select d.id::uuid, a.issue_key, d.slug, d.name, a.kind, a.is_primary, a.created_by, a.created_at, a.project_key, d.ref_key, a.last_seq, a.session_id, a.role, a.retired_at
			from seeded_documents d cross join artifacts a where a.id = $1`, []any{template.ID}},
		{"the store's nodes", `
			insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
			select n.kind, d.ref_key, n.supertag, n.visibility, n.state, n.attributes, n.made_at, n.made_by, n.source_event, n.project_key, n.parties
			from seeded_documents d join graph_nodes n on n.kind = 'artifact' and n.id = $1::text
			union all
			select n.kind, b.node_id, n.supertag, n.visibility, n.state, n.attributes, n.made_at, n.made_by, n.source_event, n.project_key, n.parties
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
