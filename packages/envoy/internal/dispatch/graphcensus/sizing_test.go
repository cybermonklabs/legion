package graphcensus

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/pmdoctest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// TestMain runs the package's tests through the Dispatch harness's Main inside cmdtest.Run, which
// makes the directory the dispatch-seed binary store_drives_test.go builds is written into and
// removes it once the tests end.
func TestMain(m *testing.M) { os.Exit(cmdtest.Run(func() int { return storetest.Main(m) })) }

// sizingCensus runs the store census with its growth pass over database, as
// `envoy-dispatch census --store --sizing` runs it, and returns everything it printed.
func sizingCensus(t *testing.T, database *store.Store) string {
	t.Helper()
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), true, nil)
	if err != nil {
		t.Fatalf("take the store census with its growth pass: %v", err)
	}
	var printed bytes.Buffer
	report.Write(&printed)
	return printed.String()
}

// seedOwedDocuments writes three documents owed a block index, their debts dated an hour apart,
// and one event to stand as the database's own clock. A version carrying markdown owes its
// document an index through migration 0122's trigger, which is how a document is owed one in
// production too, so nothing here marks the rows by hand.
func seedOwedDocuments(t *testing.T, database *store.Store, clock time.Time, ages []time.Duration) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	for index, age := range ages {
		written := clock.Add(-age)
		slug := fmt.Sprintf("spec-%d", index)
		var artifactID string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
			values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}', $2)
			returning id::text
		`, slug, written).Scan(&artifactID); err != nil {
			t.Fatalf("create document %s: %v", slug, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			values ($1, 1, 'Ship it.', '[{"kind":"user","id":"alice"}]', $2)
		`, artifactID, written); err != nil {
			t.Fatalf("write version 1 of %s: %v", slug, err)
		}
	}
	// The newest event is the clock the report measures the oldest debt against. A test writes
	// it directly because this package seeds no document through the server.
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ('CORE-1', 1, 'artifact.version', '{"kind":"user","id":"alice"}', '{}', false, $1)
	`, clock); err != nil {
		t.Fatalf("record the event the backlog's age is measured against: %v", err)
	}
}

// The sizing reports the block-index backlog, so somebody holding the database and not the
// server's log can read it: how many documents are owed an index, when the oldest of those
// debts was incurred, and how old that debt is. The age follows the oldest owed document, not
// the newest, because a backlog dated by its newest arrival reads as fresh however long its
// oldest has waited.
func TestTheSizingReportsTheOwedDocumentsAndTheAgeOfTheOldest(t *testing.T) {
	database := storetest.Open(t)
	// Postgres holds a timestamp to the microsecond, so the test's own clock is rounded to it.
	clock := time.Now().UTC().Truncate(time.Microsecond)
	ages := []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour}
	seedOwedDocuments(t, database, clock, ages)

	printed := sizingCensus(t, database)
	count := fmt.Sprintf("census: sizing backlog owed_documents=%d dated_by=%s", len(ages), backlogDatedBy)
	if !strings.Contains(printed, count+"\n") {
		t.Errorf("the sizing prints no owed count; want the line\n\t%s\ngot\n%s", count, printed)
	}
	age := fmt.Sprintf("census: sizing backlog oldest_owed_at=%s oldest_owed_age_seconds=%.2f against=%s",
		refs.OwedTimeText(clock.Add(-ages[0])), ages[0].Seconds(), refs.OwedTimeText(clock))
	if !strings.Contains(printed, age+"\n") {
		t.Errorf("the sizing prints no age for the oldest owed document; want the line\n\t%s\ngot\n%s", age, printed)
	}
}

// Two runs over one unchanged database print one report, which is what the growth check holds
// the command to (the check c-growth diffs the two). An age measured against
// now() would move between the two runs, so the backlog lines carry the oldest debt's own
// timestamp and an age measured against the newest write the database recorded.
func TestTwoSizingRunsOverAnUnchangedDatabaseAgree(t *testing.T) {
	database := storetest.Open(t)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	seedOwedDocuments(t, database, clock, []time.Duration{3 * time.Hour, 2 * time.Hour, time.Hour})

	first := sizingCensus(t, database)
	second := sizingCensus(t, database)
	if first != second {
		t.Fatalf("two runs over one unchanged database print different reports:\nfirst\n%s\nsecond\n%s", first, second)
	}
	if !strings.Contains(first, "census: sizing backlog ") {
		t.Fatalf("neither run printed the backlog at all:\n%s", first)
	}
}

// A database owing nothing says so rather than printing a date no row holds, and the age is
// zero against the clock it would be measured from.
func TestTheSizingPrintsNoDateWhereNothingIsOwed(t *testing.T) {
	database := storetest.Open(t)
	printed := sizingCensus(t, database)
	for _, want := range []string{
		fmt.Sprintf("census: sizing backlog owed_documents=0 dated_by=%s\n", backlogDatedBy),
		"census: sizing backlog oldest_owed_at=none oldest_owed_age_seconds=0.00 against=none\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("the sizing of an empty database prints no line\n\t%s\ngot\n%s", want, printed)
		}
	}
}

// The sizing refuses nothing on size. Once the store is backfilled graph_tuples stays past the
// limit, so the gate is the migration census's bounded-write rule: a pending migration writes rows
// of a table above the limit only as an insert of at most pgmigrate.BoundedWriteRows literal rows
// or an update of at most that many rows by their primary key. A sizing whose graph_tuples is
// inside the limit now and projected past it within a year, and whose document_blocks, which this
// branch's migrations alter, is above it already, names that rule beside each of the two and beside
// neither graph_nodes nor comments, which stay inside; it keeps each table's and each year's
// verdict, still ends `census: sizing ok`, and refuses nothing in the store census it is part of,
// whether that census read the store or found none.
func TestASizingPastTheLimitNamesTheBoundedWriteRuleAndRefusesNothing(t *testing.T) {
	limit := pgmigrate.CensusTableLimit
	sizing := &SizingReport{
		Limit: limit,
		Tables: []TableSize{
			{Name: "graph_nodes", Rows: 500_000, Bytes: limit / 8, Held: true},
			{Name: "graph_tuples", Rows: 700_000, Bytes: limit / 2, Held: true},
			{Name: "comments", Altered: true, Rows: 20_000, Bytes: limit / 16, Held: true},
			{Name: "document_blocks", Altered: true, Rows: 4_000_000, Bytes: 2 * limit, Held: true},
		},
		Growth: []StoreTableGrowth{
			{Name: "graph_nodes", Bytes: limit / 8, Rows: 500_000, RowsADay: 100, BytesARow: measuredBytesARow,
				BytesARowSource: "measured from this database", YearBytes: limit/8 + 100*sizingYearDays*measuredBytesARow},
			{Name: "graph_tuples", Bytes: limit / 2, Rows: 700_000, RowsADay: 4_000, BytesARow: measuredBytesARow,
				BytesARowSource: "measured from this database", YearBytes: limit/2 + 4_000*sizingYearDays*measuredBytesARow, Past: true},
		},
	}
	if tuples := sizing.Growth[1]; tuples.YearBytes < limit || sizing.Growth[0].YearBytes >= limit {
		t.Fatalf("the figures put graph_tuples at %d bytes in a year and graph_nodes at %d against the limit of %d; want graph_tuples past it and graph_nodes inside",
			tuples.YearBytes, sizing.Growth[0].YearBytes, limit)
	}
	var printed bytes.Buffer
	sizing.Write(&printed)
	lines := strings.Split(strings.TrimSuffix(printed.String(), "\n"), "\n")

	bound := pgmigrate.Thousands(pgmigrate.BoundedWriteRows)
	rule := func(table, now, year string) string {
		return fmt.Sprintf("census: sizing bounded-writes table=%s now=%s year=%s: a pending migration that writes rows of %s"+
			" may write them only with bounded writes once it is above the limit (pgmigrate's census: an insert of at most %s"+
			" literal rows, or an update of at most %s rows by their primary key)", table, now, year, table, bound, bound)
	}
	want := []string{rule("graph_tuples", "inside", "past"), rule("document_blocks", "at-or-above", "unprojected")}
	var named []string
	for _, line := range lines {
		if strings.HasPrefix(line, "census: sizing bounded-writes ") {
			named = append(named, line)
		}
	}
	if !slices.Equal(named, want) {
		t.Errorf("the sizing names the bounded-write rule as\n\t%s\nwant\n\t%s\nin\n%s",
			strings.Join(named, "\n\t"), strings.Join(want, "\n\t"), printed.String())
	}
	for _, verdict := range []string{
		fmt.Sprintf("census: sizing table name=graph_tuples altered=false rows=700,000 bytes=%d limit_bytes=%d verdict=inside", limit/2, limit),
		fmt.Sprintf("census: sizing table name=document_blocks altered=true rows=4,000,000 bytes=%d limit_bytes=%d verdict=at-or-above", 2*limit, limit),
		fmt.Sprintf("census: sizing year table=graph_tuples bytes_now=%d bytes_at_a_year=%d limit_bytes=%d verdict=past",
			limit/2, sizing.Growth[1].YearBytes, limit),
	} {
		if !slices.Contains(lines, verdict) {
			t.Errorf("the sizing prints no line\n\t%s\nin\n%s", verdict, printed.String())
		}
	}
	if last := lines[len(lines)-1]; last != "census: sizing ok" {
		t.Errorf("the sizing ends %q, want census: sizing ok:\n%s", last, printed.String())
	}
	for _, line := range lines {
		if strings.Contains(line, "REFUSED") || strings.Contains(line, "refusal") {
			t.Errorf("the sizing refuses on size: %q", line)
		}
	}

	for _, census := range []struct {
		what, verdict string
		report        *StoreReport
	}{
		{"a census that found no store", "census: store absent, sizing ok", &StoreReport{Absent: true, Sizing: sizing}},
		{"a census of a store that agrees with its projections", "census: store ok",
			&StoreReport{Kinds: []KindReport{{Kind: "comment", Table: "comments", Rows: 20_000, Nodes: 20_000}}, Sizing: sizing}},
	} {
		var out bytes.Buffer
		census.report.Write(&out)
		if census.report.Refused() {
			t.Errorf("%s refuses on the sizing's sizes:\n%s", census.what, out.String())
		}
		if got := out.String(); !strings.HasSuffix(got, "census: sizing ok\n"+census.verdict+"\n") {
			t.Errorf("%s does not end with the sizing's census: sizing ok and then %q:\n%s", census.what, census.verdict, got)
		}
	}
}

// The three versions seedSizedHistory writes of each of its two documents: a callout holding two
// paragraphs beside two paragraphs of the document's own; then the same document with the
// callout's second paragraph moved out of it, the document's last paragraph dropped and a new one
// added that cites CORE-1; then the dropped paragraph brought back at the document's end.
const (
	sizedVersion1 = ":::callout{#box kind=\"note\" title=\"\"}\nInside.\n\nMoving.\n:::\n\nStays.\n\nDropped.\n"
	sizedVersion2 = ":::callout{#box kind=\"note\" title=\"\"}\nInside.\n:::\n\nMoving.\n\nStays.\n\nAdded for dispatch://CORE-1.\n"
	sizedVersion3 = sizedVersion2 + "\nDropped.\n"
)

// sizedServerURL is the server the block index reads the archived document's references against
// (text.ExtractAt). Its one reference is a dispatch:// one, which names no server.
const sizedServerURL = "https://dispatch.sizing.invalid"

// renderedBlocks is markdown as the block index takes it: the render of its parse, which a version
// stores and the index reads each block's text out of (markdown[From:To]), and each block of that
// render under the id this test gives it: a callout's own id, and a paragraph's first word after
// `p-`, since a version's markdown names no paragraph's id.
func renderedBlocks(t *testing.T, markdown string) (string, []model.ArtifactBlock) {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render %q: %v", markdown, err)
	}
	blocks := make([]model.ArtifactBlock, 0, len(offsets))
	for _, offset := range offsets {
		id := offset.ID
		if offset.Type == "paragraph" {
			id = "p-" + strings.TrimRight(strings.Fields(rendered[offset.From:offset.To])[0], ".")
		}
		blocks = append(blocks, model.ArtifactBlock{ID: id, Type: offset.Type, From: offset.From, To: offset.To})
	}
	return rendered, blocks
}

// renderedTexts is each block of markdown's render as the block index sums it, by the id
// renderedBlocks gives it.
func renderedTexts(t *testing.T, markdown string) map[string]string {
	t.Helper()
	rendered, blocks := renderedBlocks(t, markdown)
	texts := make(map[string]string, len(blocks))
	for _, block := range blocks {
		texts[block.ID] = rendered[block.From:block.To]
	}
	return texts
}

// seedSizedHistory writes, as a build before the store writes them, the history of one document
// the block index indexed three times inside the sizing window, one comment, one clear, a message
// and its reply, a delivery of each and the events beside them. The block index records a
// version's blocks in document_block_texts and the version itself in document_indexed_versions,
// and leaves the latest version's blocks in document_blocks and what they cite in block_refs; this
// writes those rows by hand as that build's index left them, each block's text exactly as the
// index sums it and each row naming no node, since that build wrote none.
//
// The comment is written by "Alice", whose canonical login is alice, anchored to the document and
// owned by it, so its write states an authored_by, an anchored_to, an owned_by and the
// recorded_as alias that keeps the raw id beside the canonical login. Its mention of the role sizer
// reached the session ses_sized, which the listener took (seedSizedRelations), and CORE-1, which
// owns the document, is routed to that role.
//
// The message is alice's on the issue, addressed to the role sizer, and the reply is the session
// ses_sized's, answering it; the delivery is the message's attempt to that session, which the
// listener took. The events stand for what the server appends beside such writes, and the outbox
// published them long ago: the message's message.created, which first addresses the role, and an
// ask's events, whose ask no row holds, since the ask terms read the events alone. The comment's
// own comment.created is still to be published, so that a test that runs the outbox over this
// history publishes it (publishTheLog); the receipt of its mention's attempt is not in the log yet,
// since the settle that appends it comes after the comment (appendTheMentionReceipt).
//
// Beside it is a second document of CORE-1, the archive: the same three versions, each written
// before the window opened and none indexed yet, so that a test can run the block index itself
// over them (indexTheArchive) without the window counting what the index writes. And beside those
// are three typed documents of CORE-1, each created before the window opened and holding one
// version written inside it and not indexed yet (seedSizedTypedDocuments), so that a test can run
// the block index itself over a Verification document whose check serves a requirement, a plan
// whose tasks hold needs, serves and a size, and a requirement whose parent attribute places it
// under the message, inside the window (indexTheTypedDocuments).
func seedSizedHistory(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank, route) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U', 'role:sizer');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	var document, refKey string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text, ref_key
	`).Scan(&document, &refKey); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	first, second, third := renderedTexts(t, sizedVersion1), renderedTexts(t, sizedVersion2), renderedTexts(t, sizedVersion3)
	type text struct {
		block string
		text  *string
	}
	versions := []struct {
		markdown string
		texts    []text
	}{{markdown: sizedVersion1}, {markdown: sizedVersion2, texts: []text{
		{"box", new(second["box"])}, {"p-Dropped", nil}, {"p-Added", new(second["p-Added"])},
	}}, {markdown: sizedVersion3, texts: []text{{"p-Dropped", new(third["p-Dropped"])}}}}
	for block, body := range first {
		versions[0].texts = append(versions[0].texts, text{block, new(body)})
	}
	// The version that last wrote each block's text, which its document_blocks row records.
	changed := map[string]int{}
	for index, version := range versions {
		number := index + 1
		rendered, _ := renderedBlocks(t, version.markdown)
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
		`, document, number, rendered); err != nil {
			t.Fatalf("write version %d of the document: %v", number, err)
		}
		for _, written := range version.texts {
			if _, err := database.Pool.Exec(ctx, `
				insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, $2, $3, $4)
			`, document, written.block, number, written.text); err != nil {
				t.Fatalf("record block %s at version %d: %v", written.block, number, err)
			}
			changed[written.block] = number
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into document_indexed_versions (artifact_id, version) values ($1::uuid, $2)
		`, document, number); err != nil {
			t.Fatalf("record version %d indexed: %v", number, err)
		}
	}
	latest, blocks := renderedBlocks(t, sizedVersion3)
	for _, block := range blocks {
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, $2, $3, $4, '', $5, $6, now())
		`, document, block.ID, refKey+"#"+block.ID, block.Type, latest[block.From:block.To], changed[block.ID]); err != nil {
			t.Fatalf("record block %s of the latest version: %v", block.ID, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'p-Added', $2, 'issue', 'CORE-1', 'mentions', 2)
	`, document, refKey+"#p-Added"); err != nil {
		t.Fatalf("record the added paragraph's reference to CORE-1: %v", err)
	}
	// An image of CORE-1 uploaded in the window, whose one version holds its bytes: the write the
	// version_bytes term counts, its mime, size and sha256 values on the image's node.
	if _, err := database.Pool.Exec(ctx, `
		with made as (
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', 'diagram-png', 'diagram.png', 'image', false, '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors)
		select made.id, 1, '\x89504e47'::bytea, 'image/png', 4, repeat('a', 64), '[{"kind":"user","id":"alice"}]' from made
	`); err != nil {
		t.Fatalf("upload the image: %v", err)
	}
	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"Alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
		returning id::text
	`, document).Scan(&comment); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into user_agent_state (login, session_id, cleared_before) values ('alice', 'ses_sized', now())
	`); err != nil {
		t.Fatalf("write the clear: %v", err)
	}
	var message string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body, target)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'Over to the sizer.', 'role:sizer')
		returning id::text
	`).Scan(&message); err != nil {
		t.Fatalf("write the message: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into messages (issue_key, author, body, in_reply_to)
		values ('CORE-1', '{"kind":"session","id":"ses_sized"}', 'On it.', $1::uuid)
	`, message); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state)
		values ($1::uuid, 1, 'btw', 'ses_sized', 'sent')
	`, message); err != nil {
		t.Fatalf("write the delivery: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at) values
			('CORE-1', 1, 'message.created', '{"kind":"user","id":"alice"}',
			 jsonb_build_object('id', $1::text, 'target', 'role:sizer'), false, now()),
			('CORE-1', 2, 'ask.opened', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false, now()),
			('CORE-1', 3, 'ask.answered', '{"kind":"user","id":"bob"}', jsonb_build_object('id', $2::text), false, now()),
			('CORE-1', 4, 'ask.edited', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false, now()),
			('CORE-1', 5, 'ask.resolved', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false, now()),
			('CORE-1', 6, 'ask.asked_of', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false, now()),
			('CORE-1', 7, 'ask.follower_added', '{"kind":"session","id":"ses_sized"}',
			 jsonb_build_object('ask_id', $2::text, 'session_id', 'ses_sized'), false, now()),
			-- The removal of an untouched scratch document, which states its node gone.
			('CORE-1', 8, 'artifact.removed', '{"kind":"user","id":"alice"}',
			 jsonb_build_object('artifact', jsonb_build_object('id', $3::text)), false, now()),
			-- A settlement's flag of a block of the spec that fails the strict read.
			('CORE-1', 9, 'block.invalid', '{"kind":"user","id":"alice"}',
			 jsonb_build_object('artifact_id', $4::text, 'block_id', 'box'), false, now())
	`, message, "6a0e5d4c-3b2a-4f19-8e7d-6c5b4a392817", "7b1f6e5d-4c3b-4a20-9f8e-7d6c5b4a3928", document); err != nil {
		t.Fatalf("write the events: %v", err)
	}
	// The comment's comment.created, as the API appends one for a person's comment on a document:
	// owned by the document and notifying, so the outbox delivers it to the route of the issue the
	// document belongs to. The receipt of the mention's attempt comes later, as the settle that
	// appends it does (appendTheMentionReceipt).
	alice := model.Actor{Kind: "user", ID: "Alice"}
	created, err := json.Marshal(model.CommentEventPayload{
		Comment:      model.Comment{ID: comment, ArtifactID: &document, Author: alice, Body: "A remark on the spec."},
		ArtifactName: "spec.md", ProjectKey: "CORE", ArtifactSlug: "spec",
	})
	if err != nil {
		t.Fatalf("encode the comment's event: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into events (artifact_id, seq, type, actor, payload, notify) values
			($1::uuid, 1, 'comment.created', '{"kind":"user","id":"Alice"}', $2::jsonb, true)
	`, document, created); err != nil {
		t.Fatalf("write the comment's event: %v", err)
	}
	var archive string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ('CORE-1', 'CORE', 'archive', 'archive.md', 'doc', false, '{"kind":"user","id":"alice"}',
		        now() - interval '40 days')
		returning id::text
	`).Scan(&archive); err != nil {
		t.Fatalf("create the archive: %v", err)
	}
	for index, markdown := range []string{sizedVersion1, sizedVersion2, sizedVersion3} {
		rendered, _ := renderedBlocks(t, markdown)
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors, created_at)
			values ($1, $2, $3, '[{"kind":"user","id":"alice"}]', now() - make_interval(days => $4))
		`, archive, index+1, rendered, sizingWindowDays+12-4*index); err != nil {
			t.Fatalf("write version %d of the archive: %v", index+1, err)
		}
	}
	seedSizedRelations(t, database, document, comment, message)
	seedSizedTypedDocuments(t, database, message)
}

// seedSizedRelations writes, beside the history seedSizedHistory writes, one row for each further
// table a term counts: a mention of the comment that resolved to a session and its attempt, which
// the listener took, two body blocks of the comment of which one is removed, a link out of the
// live one, the document's reference to CORE-1, which its added paragraph cites, a child issue
// that waits on its parent, a failed result with two screenshots and its retraction, a Keep, two
// components of one imported snapshot, one under and depending on the other, with CORE-1
// affecting the first, alice's dismissal of the message's thread, her snooze of an ask the
// session ses_asker opened before the window, so that inside the window the snooze is the one
// write of it a term counts, a copy in the spec of an ask block of a document written before the
// window, and an answered ask asked before the window whose block, Dropped, the spec's second
// version took out.
func seedSizedRelations(t *testing.T, database *store.Store, document, comment, message string) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		what, sql string
		args      []any
	}{
		{"write the mention", `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1::uuid, 'role:sizer', 'btw', 'ses_sized')`, []any{comment}},
		{"write the mention's attempt", `
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
			values ($1::uuid, 'role:sizer', 1, 'btw', 'ses_sized', 'sent')`, []any{comment}},
		{"write the dismissal", `
			insert into user_agent_thread_dismissed (login, message_id, dismissed_at) values ('alice', $1::uuid, now())`,
			[]any{message}},
		{"write the snooze", `
			with asked as (
				insert into asks (issue_key, author, question, created_at)
				values ('CORE-1', '{"kind":"session","id":"ses_asker"}', 'Which store?', now() - interval '40 days')
				returning id
			)
			insert into user_ask_snooze (login, ask_id, snoozed_until) select 'alice', id, now() + interval '1 day' from asked`,
			nil},
		{"write the comment's body blocks", `
			insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
			                            changed_revision, changed_at, removed_at)
			values ($1::uuid, 'b1', 'b1', 0, 'paragraph', 'x', 'A remark.', 1, now(), null),
			       ($1::uuid, 'b2', 'b2', null, 'paragraph', 'y', 'Gone.', 1, now(), now())`, []any{comment}},
		{"write the body block's link", `
			insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id)
			values ($1::uuid, 'b1', 'b1', 'issue', 'CORE-1')`, []any{comment}},
		{"write the document's reference", `
			insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1::text, 'issue', 'CORE-1')`,
			[]any{document}},
		{"write the child issue and its wait", `
			with child as (
				insert into issues (key, project_key, number, title, created_by, rank, parent_key)
				values ('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V', 'CORE-1')
				returning key
			)
			insert into issue_links (issue_key, kind, target_key) select key, 'blocked_by', 'CORE-1' from child`, nil},
		{"write the result and its retraction", `
			with result as (
				insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version,
				                           check_sha256, role, commit_sha, failed, verdict, screenshots, actor)
				values ('CORE-1', 'CORE-1', $1::uuid, 'c-one', 1, 'x', 'tester', repeat('a', 40), 1, 'fail',
				        '{one.png,two.png}', '{"kind":"user","id":"alice"}')
				returning id
			)
			insert into check_result_retractions (result_id, reason, actor)
			select id, 'Run against the wrong head.', '{"kind":"user","id":"alice"}' from result`, []any{document}},
		{"write the Keep", `
			insert into decision_keeps (artifact_id, block_id, text_sha256, changed_version, kept_by)
			values ($1::uuid, 'box', 'x', 1, 'alice')`, []any{document}},
		{"write a copy of another document's ask block", `
			with source as (
				insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
				values ('CORE-1', 'CORE', 'source', 'source.md', 'doc', false, '{"kind":"user","id":"alice"}',
				        now() - interval '40 days')
				returning id
			), asked as (
				insert into asks (issue_key, author, question, created_at, block_artifact_id, block_id)
				select 'CORE-1', '{"kind":"user","id":"alice"}', 'Which store does the copy name?',
				       now() - interval '40 days', id, 'copied' from source
				returning id
			)
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, 'copied', (select ref_key from artifacts where id = $1::uuid) || '#copied', 'ask', '',
			       'Which store does the copy name?', 3, now() from asked`, []any{document}},
		{"write the answered ask whose block the second version took out", `
			insert into asks (issue_key, author, question, state, answer, created_at, block_artifact_id, block_id)
			values ('CORE-1', '{"kind":"user","id":"alice"}', 'Is Dropped kept?', 'answered',
			        jsonb_build_object('user', 'bob', 'selected', jsonb_build_array(), 'text', 'No.',
			                           'at', now() - interval '40 days'),
			        now() - interval '40 days', $1::uuid, 'p-Dropped')`, []any{document}},
		{"write the components", `
			with source as (
				insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
				values ('CORE', 'o/r', 'main', 1, '{"kind":"user","id":"alice"}')
				returning id
			), snapshot as (
				insert into architecture_snapshots (project_key, commit, files) values ('CORE', 'c0ffee', '{}')
				returning id
			), placed as (
				insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
				select 'CORE', c.id, c.id, '', c.parent, snapshot.id, source.id
				from snapshot, source, (values ('root', null), ('leaf', 'root')) c (id, parent)
				returning id
			)
			insert into component_depends (project_key, from_id, to_id)
			select 'CORE', 'leaf', 'root' from placed where id = 'leaf'`, nil},
		{"write the issue's components", `
			with chosen as (insert into issue_components (issue_key, mode) values ('CORE-1', 'explicit') returning issue_key)
			insert into issue_component_members (issue_key, project_key, component_id)
			select issue_key, 'CORE', 'root' from chosen`, nil},
	} {
		if _, err := database.Pool.Exec(ctx, step.sql, step.args...); err != nil {
			t.Fatalf("%s: %v", step.what, err)
		}
	}
}

// sizedMessageMark is where the requirement document's text names the history's message, whose id
// seedSizedTypedDocuments writes there: the requirement's parent attribute places it under that
// message.
const sizedMessageMark = "<message>"

// sizedTypedDocuments are the three typed documents of CORE-1, in the order the block index
// indexes them (indexTheTypedDocuments), each with whether it is a document the issue's room holds
// once the issue converts (roleDocument): a document of one criterion and one requirement, which
// its parent attribute places under the history's message; the issue's Verification document, whose
// check serves that requirement; and the issue's plan, whose second task needs the first, serves
// the requirement and is sized in minutes, as a plan_task's size is written (pmdoc.Minutes).
var sizedTypedDocuments = []struct {
	slug, name, markdown string
	role                 bool
}{
	{"requirements-md", "requirements.md", ":::criterion{#k-tuples}\nEvery tuple has a term that counts it.\n:::\n\n" +
		":::requirement{#r-tuples parent=\"dispatch://CORE-1/message/" + sizedMessageMark + "\"}\n" +
		"The store counts every tuple its writers append.\n:::\n", false},
	{model.ArtifactSlugVerification, "verification.md", ":::check{#c-tuples serves=\"dispatch://CORE-1/artifact/requirements-md#b-r-tuples\" surface=\"cli\"}\n" +
		"The sizing sums every term.\n:::\n", true},
	{model.ArtifactSlugPlan, "plan.md", ":::plan_task{#t-census}\nWrite the census of the store.\n:::\n\n" +
		":::plan_task{#t-size needs=\"dispatch://CORE-1/artifact/plan-md#b-t-census\" serves=\"dispatch://CORE-1/artifact/requirements-md#b-r-tuples\" size=\"45m\"}\n" +
		"Size the store from the census.\n:::\n", true},
}

// seedSizedTypedDocuments writes sizedTypedDocuments as documents of CORE-1 created before the
// window opened, each with its one version, written inside it by dana and holding its text as a
// version write renders it (typedRender), which no block index has indexed yet. message is the
// history's message, which the requirement's parent attribute names.
func seedSizedTypedDocuments(t *testing.T, database *store.Store, message string) {
	t.Helper()
	ctx := context.Background()
	for _, document := range sizedTypedDocuments {
		version := typedRender(t, strings.ReplaceAll(document.markdown, sizedMessageMark, message))
		if _, err := database.Pool.Exec(ctx, `
			with made as (
				insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
				values ('CORE-1', 'CORE', $1, $2, 'doc', false, '{"kind":"user","id":"dana"}', now() - interval '40 days')
				returning id
			)
			insert into artifact_versions (artifact_id, number, markdown, authors)
			select made.id, 1, $3, '[{"kind":"user","id":"dana"}]' from made
		`, document.slug, document.name, version.markdown); err != nil {
			t.Fatalf("write %s of CORE-1 and its version 1: %v", document.slug, err)
		}
	}
}

// sizedTerm is one term of the sizing as the report prints it: its tuples a write, the writes the
// window holds, for the moved parents the versions it read and could not parse, and whether its
// rate is assumed rather than read.
type sizedTerm struct {
	tuples, writes, read, unreadable int64
	assumed                          bool
}

var (
	tuplesAWriteLine = regexp.MustCompile(`(?m)^census: sizing tuples-a-write kind=(\S+) tuples=(\d+) `)
	shareLine        = regexp.MustCompile(`(?m)^census: sizing share kind=(\S+) moved_by=\S+ writes_in_window=(\d+) share_of_tuples=\S+(?: versions_read=(\d+) versions_unreadable=(\d+))?( rate=assumed)?$`)
	tuplesADayLine   = regexp.MustCompile(`(?m)^census: sizing rows-a-day table=graph_tuples rows_a_day=(\S+)$`)
)

// sizedTerms reads every term the report printed, and fails on a term whose two lines disagree.
func sizedTerms(t *testing.T, printed string) map[string]sizedTerm {
	t.Helper()
	number := func(text string) int64 {
		if text == "" {
			return 0
		}
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			t.Fatalf("read %q out of the sizing: %v", text, err)
		}
		return value
	}
	terms := map[string]sizedTerm{}
	for _, match := range tuplesAWriteLine.FindAllStringSubmatch(printed, -1) {
		terms[match[1]] = sizedTerm{tuples: number(match[2])}
	}
	shared := 0
	for _, match := range shareLine.FindAllStringSubmatch(printed, -1) {
		term, printedTuples := terms[match[1]]
		if !printedTuples {
			t.Fatalf("the sizing prints the share of %s and no tuples a write for it:\n%s", match[1], printed)
		}
		term.writes, term.read, term.unreadable = number(match[2]), number(match[3]), number(match[4])
		term.assumed = match[5] != ""
		terms[match[1]] = term
		shared++
	}
	if shared != len(terms) {
		t.Fatalf("the sizing prints %d term(s) and the share of %d:\n%s", len(terms), shared, printed)
	}
	return terms
}

// resultWrite is every tuple the store states of one result a session recorded, naming no
// screenshot (the result_screenshot term counts those), by its kind, as the store's own writer
// appends them: graphstore.ResultFacts' nodes and tuples written through graphstore.Write in a
// transaction of its own, which is rolled back, and the tuples that write appended read back in
// the order it appended them - the visibility, state and project its node write states of the
// result's node, and each tuple ResultFacts appends: its values, its part_of and result_of its
// check, its authored_by with the alias_of a session's credential proves of its owner
// (graphstore.AuthorFacts) and its run_session. The base attributes of the author's own nodes are
// the session and person terms', and are left out. The result term is held to it, so a value or
// a tuple the writer comes to state fails the sizing until its term counts and names it.
func resultWrite(t *testing.T, database *store.Store) []string {
	t.Helper()
	ctx := context.Background()
	owner, session := "alice", "ses_sized"
	recorder := model.Actor{Kind: "session", ID: session, Owner: &owner, Act: &model.ActorAct{Session: session, SessionProven: true}}
	nodes, tuples, ok := graphstore.ResultFacts(graphstore.Result{
		ID: "00000000-0000-4000-8000-000000000001", Project: "CORE", Check: "CORE-1/verification-md#c-one",
		Verdict: "fail", Surface: "cli", Run: "go test ./...", Observed: "FAIL", Commit: strings.Repeat("a", 40),
		Role: "tester", Control: "Broke it and watched it fail.", Failed: 1, CheckVersion: 1, CheckSHA256: "x",
		Session: session, Recorder: recorder, At: time.Now().UTC(), Event: graphstore.NoEvent,
	})
	if !ok || len(nodes) == 0 || nodes[0].Kind != graphstore.KindResult {
		t.Fatalf("graphstore.ResultFacts states no result node for a session's result: %+v", nodes)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the result's write: %v", err)
	}
	defer tx.Rollback(ctx)
	var before int64
	if err := tx.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&before); err != nil {
		t.Fatalf("read the highest tuple before the result's write: %v", err)
	}
	if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
		t.Fatalf("write the result through the store: %v", err)
	}
	rows, err := tx.Query(ctx, `
		select kind from graph_tuples
		where seq > $1 and (from_kind = $2 or not kind = any($3::text[]))
		order by seq
	`, before, graphstore.KindResult, graphstoretest.BaseAttributes())
	if err != nil {
		t.Fatalf("read the tuples the result's write appended: %v", err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the tuples the result's write appended: %v", err)
	}
	return kinds
}

// writtenKind is one kind of tuple a drive of the store's writers appended: the drive, as its
// StoreDrive names it, the kind of the node the tuple is out of, and the tuple's own kind.
type writtenKind struct{ writer, from, kind string }

func compareWritten(a, b writtenKind) int {
	return cmp.Or(strings.Compare(a.writer, b.writer), strings.Compare(a.from, b.from), strings.Compare(a.kind, b.kind))
}

// The drives of the store's writers over the history seedSizedHistory wrote, each named by the
// site of the register of the store's writers it drives (lockstep.StoreWriteCallSites), and by
// what it drives that site over where the test drives one site twice.
const (
	archiveIndexDrive  = "refs/blocks.go#Queue, over the archive's three versions"
	blockBatchDrive    = "graphcensus/backfill_blocks.go#writeBlockBatch"
	blockRefBatchDrive = "graphcensus/backfill_block_refs.go#writeBlockRefBatch"
	commentBatchDrive  = "refs/comment_nodes.go#Queue, from the backfill's comment pass"
	roleBatchDrive     = "graphcensus/backfill_roles.go#writeRoleBatch"
	sessionBatchDrive  = "graphcensus/backfill_sessions.go#writeSessionBatch"
	busDeliveryDrive   = "outbox/publisher.go#recordBatch, the bus's delivery of comment.created"
	receiptDrive       = "outbox/publisher.go#recordBatch, the receipt of the comment's mention"
	fallbackDrive      = "outbox/mentions.go#recordFallbackCopies"
	objectBatchDrive   = "graphcensus/projects_backfill.go#writeObjectBatch"
	scopeBatchDrive    = "graphcensus/projects_backfill.go#writeScopeBatch"
	orphanSweepDrive   = "graphcensus/orphans.go#StateOrphanedGone"
	typedIndexDrive    = "refs/blocks.go#Queue, over a requirement, a Verification document and a plan"
)

// sweptDocument is the address of the document the orphan sweep's drive states gone: one of
// CORE-1 whose node stands behind no row, as a build before this one leaves the node of a
// document it removes.
const sweptDocument = "CORE-1/swept-md"

// seededDrives are the drives of the store's writers over the history seedSizedHistory wrote, one
// after another: the block index over the archive (refs.IndexBlocks, then graphstore.Caused, as a
// writer with no write phase runs it), the backfill's passes over the blocks, the block
// references, the comments, the roles, the sessions and the issues, documents, asks and messages,
// the same block index over the three typed documents, whose nodes and the message's the object
// pass has written by then, the backfill's fill of a node's project, the block-index tick's sweep
// of an orphaned document, and the outbox, run whole, publishing the comment's comment.created,
// then the receipt of its mention, then a held copy.
// The outbox publishes the comment and the receipt in two drives, so the delivered_to each states
// is read apart: the bus's delivery of the comment to the role sizer, which the term bus_delivery
// counts, and the delivery the mention's attempt made to ses_sized, which the term delivery counts.
//
// The terms each drive's tuples are counted by are the one thing about the writers this test
// states rather than reads: which term counts a tuple is what that term's driver says of the
// writer, and no writer says it.
func seededDrives(database *store.Store) []StoreDrive {
	ctx := context.Background()
	backfill := func(pass func(context.Context, *store.Store) (KindBackfill, error)) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if done, err := pass(ctx, database); err != nil {
				t.Fatalf("backfill the %s kind: %v", done.Kind, err)
			}
		}
	}
	passes := func(pass func(context.Context, *store.Store) ([]KindBackfill, error)) func(*testing.T) {
		return func(t *testing.T) {
			t.Helper()
			if _, err := pass(ctx, database); err != nil {
				t.Fatalf("run a backfill pass: %v", err)
			}
		}
	}
	return []StoreDrive{{
		// A version's blocks, as the index states them over the archive's three versions: the
		// blocks it inserts afresh, each with its node's base attributes, its in_document and its
		// part_of; the paragraph the second version drops, stated gone; the paragraph it moves out
		// of the callout, its old parent retracted and its new one asserted; the dropped paragraph
		// the third version brings back, stated live, with its in_document and at most a parent the
		// restore moved; and the added paragraph's reference to CORE-1. Beside them, the values
		// the index states of the blocks it places (refs' planBlockValues): a block's rank, its
		// placement, its text_parent and its text_rank at its first row, its rank again where it
		// moved among new siblings and its text_parent and text_rank where it moved in the text,
		// and the text value of each row with a text.
		Name: archiveIndexDrive, Sites: []string{"refs/blocks.go#Queue", "graphstore/write.go#Caused"},
		Database: database, Drive: func(t *testing.T) { indexTheArchive(t, database) },
		Counts: map[StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:      {"block", "block_restored", "block_gone"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:    {"block"},
			{refs.BlockNodeKind, refs.TupleInDocument}:           {"block", "block_restored"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"block", "block_restored", movedParents},
			{refs.BlockNodeKind, "mentions"}:                     {"block_ref"},
			{refs.BlockNodeKind, refs.AttributeRank}:             {"block_values", movedRanks},
			{refs.BlockNodeKind, refs.AttributePlacement}:        {"block_values"},
			{refs.BlockNodeKind, refs.TupleTextParent}:           {"block_values", movedTextPlaces},
			{refs.BlockNodeKind, refs.AttributeTextRank}:         {"block_values", movedTextPlaces},
			{refs.BlockNodeKind, graphstore.AttributeText}:       {"block_text"},
		},
	}, {
		// The backfill's passes over the rows the build before the store wrote: the blocks of the
		// window's document, which it states a block's node and membership of and leaves the
		// parent of to the index; that document's reference; the comment, as api's commentStored
		// and graphstore.QueueAuthored state one (graphstore.CommentFacts,
		// graphstore.AuthorFacts), so its kinds are the comment path's; the role sizer and its
		// page; and the sessions the census holds to a node.
		Name: blockBatchDrive, Sites: []string{"graphcensus/backfill_blocks.go#writeBlockBatch", "graphstore/write.go#Write"},
		Database: database, Drive: backfill(func(ctx context.Context, database *store.Store) (KindBackfill, error) {
			return backfillBlocks(ctx, database, refs.BlockIndexMarkingRule)
		}),
		Counts: map[StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:      {"block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:    {"block"},
			{refs.BlockNodeKind, refs.TupleInDocument}:           {"block"},
		},
	}, {
		Name: blockRefBatchDrive, Sites: []string{"graphcensus/backfill_block_refs.go#writeBlockRefBatch", "graphstore/write.go#Write"},
		Database: database, Drive: backfill(backfillBlockRefs),
		Counts: map[StoredKind][]string{{refs.BlockNodeKind, "mentions"}: {"block_ref"}},
	}, {
		Name: commentBatchDrive, Sites: []string{"refs/comment_nodes.go#Queue"},
		Database: database, Drive: backfill(backfillComments),
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.AttributeVisibility}: {"comment"},
			{graphstore.KindComment, graphstore.AttributeState}:      {"comment"},
			{graphstore.KindComment, graphstore.AttributeProject}:    {"comment"},
			{graphstore.KindComment, graphstore.TupleAuthoredBy}:     {graphstore.TupleAuthoredBy},
			{graphstore.KindComment, graphstore.TupleAnchoredTo}:     {graphstore.TupleAnchoredTo},
			{graphstore.KindComment, graphstore.TupleOwnedBy}:        {graphstore.TupleOwnedBy},
			{graphstore.KindPerson, graphstore.AttributeVisibility}:  {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:       {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.TupleRecordedAs}:      {"author_alias"},
		},
	}, {
		Name: roleBatchDrive, Sites: []string{"graphcensus/backfill_roles.go#writeRoleBatch"},
		Database: database, Drive: backfill(backfillRoles),
		Counts: map[StoredKind][]string{
			{graphstore.KindRole, graphstore.AttributeVisibility}: {graphstore.KindRole},
			{graphstore.KindRole, graphstore.AttributeState}:      {graphstore.KindRole},
			{graphstore.KindPage, graphstore.AttributeVisibility}: {graphstore.KindPage},
			{graphstore.KindPage, graphstore.AttributeState}:      {graphstore.KindPage},
			{graphstore.KindPage, graphstore.TuplePartOf}:         {graphstore.KindPage},
		},
	}, {
		Name: sessionBatchDrive, Sites: []string{"graphcensus/backfill_sessions.go#writeSessionBatch"},
		Database: database, Drive: backfill(backfillSessions),
		Counts: map[StoredKind][]string{
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}, {
		// The backfill's pass over the issues, documents, asks and messages the build before the
		// store wrote: each one's node and its visibility, state and project, as its create states
		// them, a document's part_of the issue that owns it, an issue's child_of its parent,
		// CORE-2's of CORE-1, and the mentions of each refs row a document it names holds
		// (nameArtifactNodes), which the term document_ref counts. The seeded history's are all in
		// project CORE, so none states parties.
		Name: objectBatchDrive, Sites: []string{"graphcensus/projects_backfill.go#writeObjectBatch"},
		Database: database, Drive: passes(backfillObjectNodes),
		Counts: map[StoredKind][]string{
			{graphstore.KindIssue, graphstore.AttributeVisibility}:    {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeState}:         {graphstore.KindIssue},
			{graphstore.KindIssue, graphstore.AttributeProject}:       {graphstore.KindIssue},
			{graphstore.KindArtifact, graphstore.AttributeVisibility}: {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeState}:      {graphstore.KindArtifact},
			{graphstore.KindArtifact, graphstore.AttributeProject}:    {graphstore.KindArtifact},
			{graphstore.KindAsk, graphstore.AttributeVisibility}:      {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeState}:           {graphstore.KindAsk},
			{graphstore.KindAsk, graphstore.AttributeProject}:         {graphstore.KindAsk},
			{graphstore.KindMessage, graphstore.AttributeVisibility}:  {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeState}:       {graphstore.KindMessage},
			{graphstore.KindMessage, graphstore.AttributeProject}:     {graphstore.KindMessage},
			{graphstore.KindIssue, graphstore.TupleChildOf}:           {"issue_relation"},
			{graphstore.KindArtifact, graphstore.TuplePartOf}:         {"document_part_of"},
			{graphstore.KindArtifact, graphstore.TupleMentions}:       {"document_ref"},
		},
	}, {
		// The same index over the one version of each of the three typed documents of CORE-1
		// (seedSizedTypedDocuments), each written inside the window by dana, whom no write before
		// records: a criterion and a requirement its parent attribute places under alice's message,
		// the issue's Verification document, whose check serves that requirement, and the issue's
		// plan, whose second task needs the first, serves the requirement and writes its size.
		// Every block is new, so each states its node's base attributes, its in_document and its
		// part_of, its rank, placement, text_parent and text_rank and its text; each authored
		// block - the criterion, the requirement, the check and the two tasks - states dana's
		// authored_by, counted by block_author in the role documents and by criterion outside them,
		// beside dana's node, which the first of them makes, and what an authored block writes
		// beyond a block's facts, which authored_pointer counts: the requirement's parent, the
		// check's and the second task's serves and that task's needs, each a block_refs row of its
		// kind, and that task's size in whole minutes.
		Name: typedIndexDrive, Sites: []string{"refs/blocks.go#Queue", "graphstore/write.go#Caused"},
		Database: database, Drive: func(t *testing.T) { indexTheTypedDocuments(t, database) },
		Counts: map[StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeVisibility}:    {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:         {"block"},
			{refs.BlockNodeKind, graphstore.AttributeProject}:       {"block"},
			{refs.BlockNodeKind, refs.TupleInDocument}:              {"block"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:            {"block"},
			{refs.BlockNodeKind, refs.AttributeRank}:                {"block_values"},
			{refs.BlockNodeKind, refs.AttributePlacement}:           {"block_values"},
			{refs.BlockNodeKind, refs.TupleTextParent}:              {"block_values"},
			{refs.BlockNodeKind, refs.AttributeTextRank}:            {"block_values"},
			{refs.BlockNodeKind, graphstore.AttributeText}:          {"block_text"},
			{refs.BlockNodeKind, graphstore.TupleAuthoredBy}:        {"block_author", "criterion"},
			{refs.BlockNodeKind, pmdoc.ParentAttribute}:             {authoredPointers},
			{refs.BlockNodeKind, "serves"}:                          {authoredPointers},
			{refs.BlockNodeKind, graphstore.TupleNeeds}:             {authoredPointers},
			{refs.BlockNodeKind, refs.AttributePlanSize}:            {authoredPointers},
			{graphstore.KindPerson, graphstore.AttributeVisibility}: {graphstore.KindPerson},
			{graphstore.KindPerson, graphstore.AttributeState}:      {graphstore.KindPerson},
		},
	}, {
		// The backfill's fill of the project of a node an earlier build stored with none: the
		// comment's, which the comment pass above wrote with its project, is put back as a build
		// before the project stored it - no project on its row and no project tuple in its log, as
		// the column is the projection of its tuples - and the fill states its project, in the lock
		// and fill FillScopes runs as one batch of its own (store.RunWrite), which calls no other
		// writer of the store, so the site is writeScopeBatch alone. The comment row stays, so the
		// sized window holds the writes it held.
		Name: scopeBatchDrive, Sites: []string{"graphcensus/projects_backfill.go#writeScopeBatch"},
		Database: database,
		Ready: func(t *testing.T) {
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `delete from graph_tuples where from_kind = $1 and kind = $2`,
					graphstore.KindComment, graphstore.AttributeProject); err != nil {
					return err
				}
				tag, err := tx.Exec(ctx, `update graph_nodes set project_key = null where kind = $1`, graphstore.KindComment)
				if err == nil && tag.RowsAffected() == 0 {
					err = errors.New("no comment node to put back")
				}
				return err
			}); err != nil {
				t.Fatalf("put the comment node back to no project, as an earlier build stored it: %v", err)
			}
		},
		Drive:  passes(backfillScopes),
		Counts: map[StoredKind][]string{{graphstore.KindComment, graphstore.AttributeProject}: {"comment"}},
	}, {
		// The block-index tick's sweep of a document a build before this one removed, stating
		// nothing: the node it leaves standing behind no row, stated gone as this build's removal
		// states it, which the term artifact_removed counts.
		Name: orphanSweepDrive, Sites: []string{"graphcensus/orphans.go#StateOrphanedGone"},
		Database: database,
		Ready: func(t *testing.T) {
			if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
				return graphstore.Write(ctx, tx, []graphstore.Node{graphstore.ArtifactNode(sweptDocument, "CORE", "",
					model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC().Add(-time.Hour))}, nil)
			}); err != nil {
				t.Fatalf("write the node of %s as this build's create did, before a build before this one removed its row: %v", sweptDocument, err)
			}
		},
		Drive: func(t *testing.T) {
			if err := StateOrphanedGone(ctx, database, sweptDocument); err != nil {
				t.Fatalf("state the orphaned node of %s gone: %v", sweptDocument, err)
			}
		},
		Counts: map[StoredKind][]string{{graphstore.KindArtifact, graphstore.AttributeState}: {"artifact_removed"}},
	}, {
		// The outbox's record of the batch that delivers the comment's comment.created to the role
		// sizer: the delivered_to of that delivery, which bus_delivery counts, and the holder of the
		// role, whose held_by and whose part_of the role the session term counts.
		Name: busDeliveryDrive, Sites: []string{"outbox/publisher.go#recordBatch"},
		Database: database, Drive: func(t *testing.T) { publishTheLog(t, database) },
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleDeliveredTo}: {"bus_delivery"},
			{graphstore.KindRole, graphstore.TupleHeldBy}:         {graphstore.KindSession},
			{graphstore.KindSession, graphstore.TuplePartOf}:      {graphstore.KindSession},
		},
	}, {
		// The outbox's record of the receipt of the comment's mention, a delivery to ses_sized,
		// which the term delivery counts: ses_sized claimed no role, so it takes a page of its own,
		// which the session term counts with its node.
		Name: receiptDrive, Sites: []string{"outbox/publisher.go#recordBatch"},
		Database: database, Ready: func(t *testing.T) { appendTheMentionReceipt(t, database) },
		Drive: func(t *testing.T) { publishTheLog(t, database) },
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleDeliveredTo}: {"delivery"},
			{graphstore.KindPage, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindPage, graphstore.AttributeState}:      {graphstore.KindSession},
			{graphstore.KindPage, graphstore.TuplePartOf}:         {graphstore.KindSession},
		},
	}, {
		// The outbox's fallback pass: the copy it publishes to ses_copy's own topic, and that
		// session's node, which the copy's row is the first to name.
		Name: fallbackDrive, Sites: []string{"outbox/mentions.go#recordFallbackCopies"},
		Database: database, Drive: func(t *testing.T) { publishAFallbackCopy(t, database) },
		Counts: map[StoredKind][]string{
			{graphstore.KindComment, graphstore.TupleDeliveredTo}:    {"fallback_delivery"},
			{graphstore.KindSession, graphstore.AttributeVisibility}: {graphstore.KindSession},
			{graphstore.KindSession, graphstore.AttributeState}:      {graphstore.KindSession},
		},
	}}
}

// writeTheStore runs every drive of the store's writers: the ones over the history
// seedSizedHistory wrote on database (seededDrives), graphstore's own writers run on their own
// (graphstoreDrives), the lens catalogue's writer and the backfill's passes over a run and a
// prevented_by (runAndCatalogueDrives), the boot step's ask passes (askBackfillDrives), the seed
// (seedRunDrives), the backfill's other passes, the pointer's and the rebuild's writers and the
// architecture import (internalDrives), and the drives of the sites whose packages import this
// one, which this package's external test registers (ImportedDrives). It holds them to driving
// every site the register of the store's writers lists (requireEveryListedSiteDriven). Each drive
// is the writer itself and not a copy of what it writes. It answers how many tuples of each kind
// each drive appended, read off graph_tuples by the sequences the drive's tuples took, and the
// terms each drive says count each kind it appends; and it logs, for every listed site, what each
// drive that runs it appended.
func writeTheStore(t *testing.T, database *store.Store) (map[writtenKind]int64, map[writtenKind][]string) {
	t.Helper()
	ctx := context.Background()
	drives := slices.Concat(seededDrives(database), graphstoreDrives(t), runAndCatalogueDrives(t), askBackfillDrives(t),
		seedRunDrives(t), internalDrives(t))
	if ImportedDrives == nil {
		t.Errorf("no drive of the sites whose packages import graphcensus is registered: ImportedDrives, which this package's external test sets, is nil")
	} else {
		drives = append(drives, ImportedDrives(t)...)
	}
	requireEveryListedSiteDriven(t, drives)
	appended, counted := map[writtenKind]int64{}, map[writtenKind][]string{}
	wrote := map[string]string{}
	for _, drive := range drives {
		if _, named := wrote[drive.Name]; named {
			t.Fatalf("two drives are named %s, so what each appended cannot be read apart", drive.Name)
		}
		for stored, terms := range drive.Counts {
			counted[writtenKind{writer: drive.Name, from: stored.From, kind: stored.Kind}] = terms
		}
		if drive.Ready != nil {
			drive.Ready(t)
		}
		var before int64
		if err := drive.Database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&before); err != nil {
			t.Fatalf("read the highest tuple before %s: %v", drive.Name, err)
		}
		drive.Drive(t)
		rows, err := drive.Database.Pool.Query(ctx, `
			select from_kind, kind, count(*) from graph_tuples where seq > $1 group by from_kind, kind order by from_kind, kind
		`, before)
		if err != nil {
			t.Fatalf("read the tuples %s appended: %v", drive.Name, err)
		}
		var kinds []string
		for rows.Next() {
			written := writtenKind{writer: drive.Name}
			var count int64
			if err := rows.Scan(&written.from, &written.kind, &count); err != nil {
				t.Fatalf("read a kind of tuple %s appended: %v", drive.Name, err)
			}
			appended[written] = count
			kinds = append(kinds, fmt.Sprintf("%d %s out of a %s node", count, written.kind, written.from))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the tuples %s appended: %v", drive.Name, err)
		}
		wrote[drive.Name] = "nothing"
		if len(kinds) > 0 {
			wrote[drive.Name] = strings.Join(kinds, ", ")
		}
	}
	for _, site := range slices.Sorted(maps.Keys(lockstep.StoreWriteCallSites())) {
		for _, drive := range drives {
			if slices.Contains(drive.Sites, site) {
				t.Logf("store writer %s, run by the drive %s, which appended %s", site, drive.Name, wrote[drive.Name])
			}
		}
	}
	return appended, counted
}

// indexTheArchive runs the block index over each version of the archive in turn, at the time
// that version was written, as each version's write runs it: the render's blocks under the ids
// renderedBlocks gives them, and what the render cites (text.ExtractAt). No event recorded those
// writes, so what the index states carries the sentinel, as the seed's own documents' does
// (cmd/dispatch-seed's writeDocument).
func indexTheArchive(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select v.artifact_id::text, v.number, v.markdown, v.created_at
		from artifact_versions v join artifacts a on a.id = v.artifact_id
		where a.slug = 'archive' order by v.number
	`)
	if err != nil {
		t.Fatalf("read the archive's versions: %v", err)
	}
	type version struct {
		artifact, markdown string
		number             int
		at                 time.Time
	}
	var versions []version
	for rows.Next() {
		var read version
		if err := rows.Scan(&read.artifact, &read.number, &read.markdown, &read.at); err != nil {
			t.Fatalf("read a version of the archive: %v", err)
		}
		versions = append(versions, read)
	}
	if err := rows.Err(); err != nil || len(versions) == 0 {
		t.Fatalf("read the archive's versions: %d of them, %v", len(versions), err)
	}
	for _, version := range versions {
		_, blocks := renderedBlocks(t, version.markdown)
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the index of version %d: %v", version.number, err)
		}
		_, won, write, _, err := refs.IndexBlocks(ctx, tx, version.artifact, refs.OneVersion(version.artifact, version.number, version.at), version.markdown,
			blocks, text.ExtractAt(version.markdown, sizedServerURL), nil, refs.TypedFacts{})
		if err == nil && !won {
			err = fmt.Errorf("the pass lost the revision fence, so it wrote nothing")
		}
		if err == nil {
			err = graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			tx.Rollback(ctx)
			t.Fatalf("index version %d of the archive: %v", version.number, err)
		}
	}
}

// typedVersion is a version of a typed document as a version write renders it
// (pmdoc.RenderWithBlockOffsets): its markdown; its blocks with their offsets and the attributes
// each one's opening line writes, which a plan node's values are read off (refs' planBlockValues);
// the mentions located in it; and the pointers its typed blocks write (pmdoc.BlockPointers).
type typedVersion struct {
	markdown string
	blocks   []model.ArtifactBlock
	located  []text.Located
	pointers []refs.Pointer
}

// typedRender reads markdown as a version write reads it and renders it, every block holding an
// id: a typed block's is the one its opening line writes, and a paragraph takes a fresh one, which
// markdown never writes.
func typedRender(t *testing.T, markdown string) typedVersion {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	pmdoc.EnsureBlockIDs(tree)
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render %q: %v", markdown, err)
	}
	version := typedVersion{markdown: rendered, located: text.ExtractAt(rendered, sizedServerURL)}
	for _, offset := range offsets {
		version.blocks = append(version.blocks, model.ArtifactBlock{
			ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To, Attributes: offset.Attributes,
		})
	}
	for _, pointer := range pmdoc.BlockPointers(tree) {
		version.pointers = append(version.pointers, refs.Pointer{
			Block: pointer.Block, BlockType: pointer.BlockType, Kind: pointer.Attribute, Target: pointer.Target, Targets: pointer.Targets,
		})
	}
	return version
}

// typedStored is one of sizedTypedDocuments as the store holds it: its id, the time its version
// was written, whether it is a document the issue's room holds once it converts, and that version
// as typedRender renders it again.
type typedStored struct {
	artifact string
	at       time.Time
	role     bool
	version  typedVersion
}

// sizedTypedVersions reads the one version of each of sizedTypedDocuments, in their order, and
// renders it again, failing the test where the render is not the text the version holds: a
// version write's render reads back as itself, so the index reads the blocks of the text stored.
func sizedTypedVersions(t *testing.T, database *store.Store) []typedStored {
	t.Helper()
	stored := make([]typedStored, 0, len(sizedTypedDocuments))
	for _, document := range sizedTypedDocuments {
		held := typedStored{role: document.role}
		var markdown string
		if err := database.Pool.QueryRow(context.Background(), `
			select a.id::text, v.created_at, v.markdown from artifacts a
			join artifact_versions v on v.artifact_id = a.id and v.number = 1
			where a.issue_key = 'CORE-1' and a.slug = $1
		`, document.slug).Scan(&held.artifact, &held.at, &markdown); err != nil {
			t.Fatalf("read version 1 of %s of CORE-1: %v", document.slug, err)
		}
		if held.version = typedRender(t, markdown); held.version.markdown != markdown {
			t.Fatalf("version 1 of %s of CORE-1 renders as %q, not as the %q it holds", document.slug, held.version.markdown, markdown)
		}
		stored = append(stored, held)
	}
	return stored
}

// indexTheTypedDocuments runs the block index over the one version of each of the typed documents
// in turn (sizedTypedVersions), each in a transaction of its own, at the time that version was
// written, as a version write runs it: its render's blocks, the mentions located in it and the
// pointers its typed blocks write. No event recorded those writes, so what the index states carries
// the sentinel.
func indexTheTypedDocuments(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, typed := range sizedTypedVersions(t, database) {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the index of %s: %v", typed.artifact, err)
		}
		_, won, write, _, err := refs.IndexBlocks(ctx, tx, typed.artifact, refs.OneVersion(typed.artifact, 1, typed.at),
			typed.version.markdown, typed.version.blocks, typed.version.located, typed.version.pointers, refs.TypedFacts{})
		if err == nil && !won {
			err = errLostFence
		}
		if err == nil {
			err = graphstore.Caused(ctx, tx, write, graphstore.NoEvent)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			tx.Rollback(ctx)
			t.Fatalf("index version 1 of %s: %v", typed.artifact, err)
		}
	}
}

// publishTheLog runs the outbox until it has published every event the log holds unpublished: the
// comment's comment.created the history left, and then, once appendTheMentionReceipt has appended
// it, the receipt of its mention's attempt.
func publishTheLog(t *testing.T, database *store.Store) {
	t.Helper()
	runTheOutbox(t, database, "publish every event", `select not exists (select 1 from events where published_at is null)`)
}

// appendTheMentionReceipt appends the receipt of the comment's mention's attempt to ses_sized, as
// the mention's settle appends one (api's completeCommentDelivery) once the listener took the
// send: owned by the comment's document and notifying nobody.
func appendTheMentionReceipt(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var comment, document string
	if err := database.Pool.QueryRow(ctx, `select id::text, artifact_id::text from comments`).Scan(&comment, &document); err != nil {
		t.Fatalf("read the comment whose mention the receipt settles: %v", err)
	}
	receipt, err := json.Marshal(model.CommentDeliveryEventPayload{
		CommentID: comment, Target: "role:sizer", Attempt: 1, Delivery: "btw",
		SessionID: new("ses_sized"), State: model.DeliveryStateSent,
	})
	if err != nil {
		t.Fatalf("encode the mention's receipt: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into events (artifact_id, seq, type, actor, payload, notify)
		values ($1::uuid, 2, 'comment.delivery', '{"kind":"user","id":"Alice"}', $2::jsonb, false)
	`, document, receipt); err != nil {
		t.Fatalf("append the mention's receipt: %v", err)
	}
}

// publishAFallbackCopy holds the comment's copy for the session ses_copy as a fallback row, as a
// batch holds one back for a mention it waits on no longer (outbox's holdFallbackCopy), and runs
// the outbox until its fallback pass has published it. No attempt of the comment names ses_copy,
// which the pass reads as a mention that delivered nothing, so the copy goes.
func publishAFallbackCopy(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into comment_fallback_copies (comment_id, destination, event_id, session_id)
		select (e.payload->>'id')::uuid, $1, e.id, 'ses_copy' from events e where e.type = 'comment.created'
	`, contracts.LegacyAgentSubject("ses_copy")); err != nil {
		t.Fatalf("hold the comment's copy for ses_copy: %v", err)
	}
	runTheOutbox(t, database, "publish the held copy",
		`select exists (select 1 from comment_fallback_copies where published and settled_at is not null)`)
}

// runTheOutbox runs the real outbox over database (outbox.New, Run), every role held by
// censusRoleHolder and every send acknowledged at once, until settled answers true, and then
// stops it and waits for it to stop, so nothing it writes lands after this returns.
func runTheOutbox(t *testing.T, database *store.Store, what, settled string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		outbox.New(outbox.Deps{
			Store: database, Publisher: censusStream{}, Broker: events.NewBroker(),
			Listen: database.Pool.Listener, RoleClaims: censusRoles{}, Sessions: censusRoles{},
		}).Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("the outbox did not stop")
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var ok bool
		if err := database.Pool.QueryRow(context.Background(), settled).Scan(&ok); err != nil {
			t.Fatalf("read whether the outbox did %s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox did not %s within 30s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The sizing counts every tuple the writers append, term by term: rows a day is the sum over the
// terms of writes a day times tuples a write (R4), so a kind it leaves out reads the one-year
// verdict low. The test takes its list of kinds from the writers themselves: it drives every site
// the register of the store's writers lists (writeTheStore), the real site each time, and
// requires each kind of tuple each drive appended to be counted by a term the sizing prints (each
// drive's Counts), naming the drive and the kind where none does. The outbox's record of a batch
// is driven twice, once over the bus's delivery of a comment and once over the receipt of its
// mention, so the delivered_to of a comment event the bus delivers, that of a mention's attempt
// and that of a published fallback copy are each held to a term of their own.
//
// The history holds at least one write of every term, so that the sum over the terms sees each:
// a version that drops a block, which the index states gone, moves a paragraph out of a callout,
// which it states as the retraction of one parent and the assertion of the other, and brings the
// dropped block back, beside the blocks each version writes afresh, a reference and the texts
// the blocks child moves; a comment with each fact its write states, its mention's delivery and the
// bus's and a fallback copy's delivery of it; the read marks that comment and a message move, a
// clear, a dismissal and a snooze; a message with a reply and a delivery; the events of an ask;
// and a row of each further table a term counts (seedSizedRelations); and three typed documents
// whose one version each the block index indexes inside the window (typedIndexDrive): a
// Verification document whose check serves a requirement, a plan whose tasks hold needs, serves and
// a size, and the requirement, whose parent attribute places it under the message, so the term
// authored_pointer, which counts those pointers and that size, reads them. Every term is printed
// with the tuples a write this test states for it and its writes inside the window or the writes
// it is assumed at, none of them zero, and the rows a day of graph_tuples is exactly their sum.
func TestTheSizingCountsEveryTupleTheWritersAppend(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	appended, countedBy := writeTheStore(t, database)
	printed := sizingCensus(t, database)
	terms := sizedTerms(t, printed)
	resultKinds := resultWrite(t, database)
	// Each block of the three typed documents is a block's first row inside the window, each with a
	// text, and each block of the two of them the issue's room holds once it converts - its
	// Verification document and its plan - takes the one author its version lists.
	var typedBlocks, typedRoomBlocks int64
	for _, typed := range sizedTypedVersions(t, database) {
		typedBlocks += int64(len(typed.version.blocks))
		if typed.role {
			typedRoomBlocks += int64(len(typed.version.blocks))
		}
	}
	for _, written := range slices.SortedFunc(maps.Keys(appended), compareWritten) {
		counting, stated := countedBy[written]
		if !stated {
			t.Errorf("%s appends %s tuples out of a %s node, and no term of the sizing counts them",
				written.writer, written.kind, written.from)
			continue
		}
		for _, term := range counting {
			if _, printedTerm := terms[term]; !printedTerm {
				t.Errorf("%s appends %s tuples out of a %s node, which term %s counts, and the sizing prints no term %s",
					written.writer, written.kind, written.from, term, term)
			}
		}
	}
	for _, written := range slices.SortedFunc(maps.Keys(countedBy), compareWritten) {
		if appended[written] == 0 {
			t.Errorf("this test says %s appends %s tuples out of a %s node, which term %s counts, and it appended none",
				written.writer, written.kind, written.from, strings.Join(countedBy[written], " and "))
		}
	}
	want := map[string]sizedTerm{
		"comment": {tuples: 3, writes: 1}, "authored_by": {tuples: 1, writes: 1}, "anchored_to": {tuples: 1, writes: 1},
		"owned_by": {tuples: 1, writes: 1}, "author_alias": {tuples: 1, writes: 1},
		// The comment is anchored to the document: its anchored_in and the part_of a comment
		// answering nothing takes to that block, at most their retractions and their restatements.
		"anchor_block": {tuples: 6, writes: 1},
		// The mention's attempt to ses_sized, the comment.created the bus delivered to the role
		// sizer, and the copy the fallback pass published to ses_copy.
		"delivery": {tuples: 1, writes: 1}, "bus_delivery": {tuples: 1, writes: 1}, "fallback_delivery": {tuples: 1, writes: 1},
		// Five blocks of the window document are new at version 1 and one at version 2; version 2
		// drops one and rewrites the callout, and version 3 brings the dropped one back, so eight
		// of its rows hold a text; and every block of the three typed documents is new.
		"block": {tuples: 5, writes: 6 + typedBlocks}, "block_restored": {tuples: 4, writes: 1}, "block_gone": {tuples: 1, writes: 1},
		// Version 1 is the window document's first indexed version, so it is compared with nothing,
		// version 2 moved one paragraph out of the callout, which takes a rank and a place in the
		// text in its new place, and version 3 moved none; each typed document's one version is its
		// first indexed one too.
		movedParents: {tuples: 2, writes: 1, read: 6}, movedRanks: {tuples: 1, writes: 1}, movedTextPlaces: {tuples: 3, writes: 1},
		// The window document's eight rows and each typed block's first hold a text, each a text
		// value; its six first rows and each typed block's, each a rank, a placement, a text_parent
		// and a text_rank; one deleted a block, whose text value stays as the block's node goes gone.
		// The added paragraph's reference to CORE-1 is the one link block_ref counts.
		"block_ref": {tuples: 2, writes: 1}, "block_text": {tuples: 1, writes: 8 + typedBlocks}, "block_values": {tuples: 4, writes: 6 + typedBlocks},
		// The requirement's parent, the check's and the second task's serves and that task's needs,
		// each a block_refs row of its kind, and that task's size.
		authoredPointers: {tuples: 2, writes: 5},
		// The document's reference to CORE-1 (seedSizedRelations). The document is CORE-1's spec,
		// whose six first rows each take the one author their version lists, as do the first rows of
		// the Verification document and the plan.
		"document_ref": {tuples: 2, writes: 1}, "block_author": {tuples: 1, writes: 6 + typedRoomBlocks},
		// Each of the window's seven versions writes its node and six tuples
		// (graphstore.VersionFacts): the window document's three, the image's one, which states its
		// three bytes on the image's node, and the typed documents' one each; the window document
		// and the image are an issue's, created inside the window, and the typed documents were
		// created before it. The window document is CORE-1's spec, so each of its three versions is
		// taken to hold a definition_version, and so is the version of the Verification document and
		// the plan's.
		"version": {tuples: 6, writes: 7}, "version_bytes": {tuples: 3, writes: 1}, "document_part_of": {tuples: 1, writes: 2},
		"query_version": {tuples: 1, writes: 5},
		// The scratch document removed in the window held one version.
		"version_removed": {tuples: 1, writes: 1},
		// The comment is alice's first, the delivery records ses_sized and the published copy
		// ses_copy, and the message.created event is the first to address the role sizer, which
		// takes a page with it.
		"person": {tuples: 2, writes: 1}, "session": {tuples: 5, writes: 2}, "role": {tuples: 2, writes: 1},
		"page": {tuples: 3, writes: 1},
		// The comment and the message each move alice's read_through on their thread (migrations
		// 0095 and 0097's triggers), so she takes part in two threads and each is a read mark; the
		// reply, a session's, reaches her in the message's thread, and the ask opened reaches the
		// person it waits on.
		"message_delivery": {tuples: 1, writes: 1}, "read_mark": {tuples: 1, writes: 2}, "clear": {tuples: 1, writes: 1},
		"dismissal": {tuples: 1, writes: 1}, "snooze": {tuples: 1, writes: 1},
		"message": {tuples: 3, writes: 2}, "message_relation": {tuples: 2, writes: 2}, "message_pointer": {tuples: 1, writes: 1},
		"reply": {tuples: 2, writes: 1},
		// The mention is one tuple to its role and one to the session the role resolved to.
		"mention": {tuples: 1, writes: 2}, "participation": {tuples: 1, writes: 2}, "person_delivery": {tuples: 1, writes: 2},
		"ask_asked_of": {tuples: 4, writes: 1}, "body_block": {tuples: 3, writes: 2}, "body_block_gone": {tuples: 1, writes: 1},
		// The attempts are the mention's and the message's.
		"body_block_ref": {tuples: 2, writes: 1}, "delivery_attempt": {tuples: 14, writes: 2},
		// CORE-2 is a child of CORE-1 and waits on it. The archive was written before the window.
		"issue": {tuples: 3, writes: 2}, "issue_relation": {tuples: 1, writes: 2}, "artifact": {tuples: 3, writes: 2},
		// CORE-1 and CORE-2 are each taken to state a filed_from and a duplicate_of and to be born
		// converted, each with its room and its decisions query; CORE-1's spec is a query node too.
		"issue_origin": {tuples: 2, writes: 2}, "issue_room": {tuples: 21, writes: 2}, "query_node": {tuples: 4, writes: 3},
		// The scratch document removed in the window.
		"artifact_removed": {tuples: 1, writes: 1},
		// The criterion in requirements.md, outside the three role documents, whose version lists
		// one author (dana).
		"criterion": {tuples: 1, writes: 1},
		"result":    {tuples: int64(len(resultKinds)), writes: 1}, "result_screenshot": {tuples: 1, writes: 2}, "result_retraction": {tuples: 1, writes: 1},
		// Each component's node is in its project. The leaf's part_of the root and its depends_on
		// it, and CORE-1's affects of the root.
		"component": {tuples: 3, writes: 2}, "component_relation": {tuples: 1, writes: 3},
		// No table keeps a component an import dropped or a source's removal took.
		"component_retired": {tuples: 3, writes: sizingWindowDays / 7, assumed: true},
		// The ask's part_of the document holding its block beside its anchored_to, an answer's
		// decided_by beside its answered_by, and a resolution's value and its retraction.
		"ask": {tuples: 3, writes: 1}, "ask_opened": {tuples: 8, writes: 1}, "ask_edit": {tuples: 2, writes: 1}, "answer": {tuples: 6, writes: 1},
		"ask_resolved": {tuples: 3, writes: 1}, "ask_follower": {tuples: 1, writes: 1}, "keep": {tuples: 2, writes: 1},
		// The copy of the source document's ask block, the spec's block the settlement flagged, and
		// the version that took out the answered ask's block; no table dates a moved_to.
		"ask_copy": {tuples: 1, writes: 1}, "ask_invalid": {tuples: 2, writes: 1},
		"ask_removal": {tuples: 2, writes: 1}, "block_move": {tuples: 1, writes: 1 * sizingWindowDays, assumed: true},
		"decision": {tuples: 5, writes: 10 * sizingWindowDays, assumed: true},
		"reading":  {tuples: 1, writes: 10 * sizingWindowDays, assumed: true},
		// No table holds a kind of the lenses child's yet, so each term reads the writes it
		// is assumed at, and the tuples its writer appends (storeKinds names each writer).
		"interaction":      {tuples: 11, writes: 10 * sizingWindowDays, assumed: true},
		"frame":            {tuples: 1, writes: 500 * sizingWindowDays, assumed: true},
		"interaction_gone": {tuples: 1, writes: 10 * sizingWindowDays, assumed: true},
		"lens":             {tuples: 11, writes: 2 * sizingWindowDays, assumed: true},
		"document_move":    {tuples: 1, writes: 1 * sizingWindowDays, assumed: true},
		"lens_finding":     {tuples: 8, writes: 10 * sizingWindowDays, assumed: true},
		"lens_kind":        {tuples: 8, writes: 22, assumed: true},
		"constraint":       {tuples: 11, writes: 1 * sizingWindowDays, assumed: true},
		"risk":             {tuples: 11, writes: 11 * sizingWindowDays, assumed: true},
		"node_state":       {tuples: 1, writes: 10 * sizingWindowDays, assumed: true},
		"pointer_retarget": {tuples: 2, writes: 1 * sizingWindowDays, assumed: true},
		// No table holds a lens, so R7's delete of one with its finding reads the writes it is
		// assumed at: each one's state gone and the retraction of the lens's part_of, has_kind and
		// authored_by and of the finding's part_of and authored_by.
		"node_delete": {tuples: 7, writes: 1 * sizingWindowDays, assumed: true},
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		got, printedTerm := terms[name]
		switch {
		case !printedTerm:
			t.Errorf("the sizing prints no term %s, whose writer appends %d tuple(s) a write", name, want[name].tuples)
		case got != want[name]:
			t.Errorf("the sizing prints term %s as %+v, want %+v", name, got, want[name])
		}
	}
	// The result term's driver names every kind of tuple its writer states (resultWrite), as the
	// count above holds its tuples to that writer's.
	resultDriver := regexp.MustCompile(`(?m)^census: sizing tuples-a-write kind=result .* driver=(.*)$`).FindStringSubmatch(printed)
	if resultDriver == nil {
		t.Fatalf("the sizing prints no driver of the result term:\n%s", printed)
	}
	for _, kind := range resultKinds {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(kind) + `\b`).MatchString(resultDriver[1]) {
			t.Errorf("the result term's driver %q names no %s, which graphstore.ResultFacts states of a session's result", resultDriver[1], kind)
		}
	}
	// A term with no write adds nothing to the sum, so the sum below would read the same with the
	// term left out of it: every term needs a write for that check to see it.
	for _, name := range slices.Sorted(maps.Keys(terms)) {
		if terms[name].writes == 0 {
			t.Errorf("the seeded history gives term %s no write, so the sum of the terms cannot tell whether it counts %s", name, name)
		}
	}
	var tuples int64
	for name, term := range terms {
		if _, wanted := want[name]; !wanted {
			t.Errorf("the sizing prints term %s, which this test does not know", name)
		}
		tuples += term.writes * term.tuples
	}
	match := tuplesADayLine.FindStringSubmatch(printed)
	if wantADay := fmt.Sprintf("%.2f", float64(tuples)/sizingWindowDays); match == nil || match[1] != wantADay {
		t.Errorf("the sizing projects graph_tuples at %v rows a day, want %s: the sum of every term's writes times its tuples, %d, over %d days\n%s",
			match, wantADay, tuples, sizingWindowDays, printed)
	}
	// The history's ten versions, the window document's three, the image's one, the archive's
	// three and the typed documents' one each, were written before the store held a version node,
	// so the version backfill owes each its node and six tuples once. No pass of this build's rule
	// has stated a document of this store but the typed documents, so the backfill's marking owes
	// every block of a document it would mark its rank, text and placement (unstatedBlocksOwed). The
	// year projects every backfill's rows beside the rate: each table's backfill-rows line is the
	// sum of the backfill lines' nodes or tuples.
	if line := "census: sizing backfill kind=version moved_by=" + movedWithBlocks + " owed_rows=10 nodes_a_row=1 tuples_a_row=6 nodes=10 tuples=60 "; !strings.Contains(printed, line) {
		t.Errorf("the sizing prints no line %q:\n%s", line, printed)
	}
	var backfillNodes, backfillTuples int64
	for _, owed := range regexp.MustCompile(`(?m)^census: sizing backfill kind=\S+ .* nodes=(\d+) tuples=(\d+) `).FindAllStringSubmatch(printed, -1) {
		nodes, _ := strconv.ParseInt(owed[1], 10, 64)
		tuples, _ := strconv.ParseInt(owed[2], 10, 64)
		backfillNodes, backfillTuples = backfillNodes+nodes, backfillTuples+tuples
	}
	for _, line := range []string{
		fmt.Sprintf("census: sizing backfill-rows table=graph_tuples rows=%d\n", backfillTuples),
		fmt.Sprintf("census: sizing backfill-rows table=graph_nodes rows=%d\n", backfillNodes),
	} {
		if !strings.Contains(printed, line) {
			t.Errorf("the sizing prints no line %q:\n%s", line, printed)
		}
	}
}

// The sizing counts what the backfill's marking (markStaleDocuments) owes the store once, beside
// the rates: a rank, a text and a placement for every block of a document the marking would mark -
// one whose row holds no mark and no pass of this build's rule has stated at its version - and for
// every such block at the top of an issue's spec, Verification or plan document the retraction of
// its part_of to the document and its part_of to the issue.
//
// Four documents are stored as the release before stored them: a spec of two top-level blocks and
// one nested, and an upload of one, which no pass of this rule has stated; a plan of two top-level
// blocks a pass of this rule stated at its version; and a Verification document of one top-level
// block that holds the backfill's mark already. The sizing owes the spec's and the upload's four
// blocks their values and the spec's two their top. Once a pass of this rule has stated the spec
// it owes the upload's block its values and no top. On a database the migration that adds the two
// columns of that record has not reached, which the sizing takes its readings on too, no row can
// say it is stated, and the sizing owes every stored block: seven values and five tops.
func TestTheSizingCountsWhatTheMarkingOwesEachUnstatedBlock(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	documents := map[string]string{}
	for _, document := range []struct {
		slug                      string
		primary                   bool
		blocks                    [][2]string
		statedVersion, statedRule *int
		marked                    bool
	}{
		{slug: "spec", primary: true, blocks: [][2]string{{"outer", ""}, {"inner", "outer"}, {"after", ""}}},
		{slug: "notes-md", blocks: [][2]string{{"note", ""}}},
		{slug: "plan-md", blocks: [][2]string{{"step", ""}, {"next", ""}}, statedVersion: new(1), statedRule: new(refs.BlockIndexMarkingRule)},
		{slug: "verification-md", blocks: [][2]string{{"check", ""}}, marked: true},
	} {
		var artifactID, refKey string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', $1, $1, 'doc', $2, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, document.slug, document.primary).Scan(&artifactID, &refKey); err != nil {
			t.Fatalf("create document %s: %v", document.slug, err)
		}
		documents[document.slug] = artifactID
		for _, block := range document.blocks {
			var parent *string
			if block[1] != "" {
				parent = &block[1]
			}
			if _, err := database.Pool.Exec(ctx, `
				insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at, parent_block_id)
				values ($1::uuid, $2, $3, 'paragraph', '', $2, 1, now(), $4)
			`, artifactID, block[0], refKey+"#"+block[0], parent); err != nil {
				t.Fatalf("write block %s of %s: %v", block[0], document.slug, err)
			}
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into document_block_versions (artifact_id, version, store_version, stated_version, stated_rule, store_owed_at)
			values ($1::uuid, 1, 1, $2, $3, case when $4 then now() end)
		`, artifactID, document.statedVersion, document.statedRule, document.marked); err != nil {
			t.Fatalf("write the block index row of %s: %v", document.slug, err)
		}
	}
	owes := func(when string, values, tops int) {
		t.Helper()
		printed := sizingCensus(t, database)
		for _, line := range []string{
			fmt.Sprintf("census: sizing backfill kind=block_values moved_by=%s owed_rows=%d nodes_a_row=0 tuples_a_row=5 nodes=0 tuples=%d ",
				movedWithBlocks, values, 5*values),
			fmt.Sprintf("census: sizing backfill kind=block_top moved_by=%s owed_rows=%d nodes_a_row=0 tuples_a_row=2 nodes=0 tuples=%d ",
				movedWithBlocks, tops, 2*tops),
		} {
			if !strings.Contains(printed, line) {
				t.Errorf("%s the sizing prints no line %q:\n%s", when, line, printed)
			}
		}
	}
	owes("with the spec and the upload unstated,", 4, 2)
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set stated_version = version, stated_rule = $2 where artifact_id = $1::uuid
	`, documents["spec"], refs.BlockIndexMarkingRule); err != nil {
		t.Fatalf("state the spec by this build's rule: %v", err)
	}
	owes("with the spec stated,", 1, 0)
	if _, err := database.Pool.Exec(ctx, `
		alter table document_block_versions drop column stated_version, drop column stated_rule
	`); err != nil {
		t.Fatalf("take the stated record's columns out of the database: %v", err)
	}
	owes("before the migration that adds the stated record,", 7, 5)
}

// The sizing needs nothing of the store: R4 takes its readings before the branch's first
// migration, on a database that holds no graph_nodes, no graph_tuples and no node column on any
// projection. The test takes the sizing of a seeded history, then takes every object of the store
// out of the same database and takes it again: the census says the store is absent and reads no
// kind, the sizing passes, and every term reads what it read with the store there.
func TestTheSizingRunsOnADatabaseWithNoStore(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	before := sizingCensus(t, database)
	if _, err := database.Pool.Exec(context.Background(), `
		drop table graph_tuples, graph_nodes cascade;
		alter table comments drop column node_kind cascade, drop column node_address cascade;
		alter table document_blocks drop column node_kind cascade, drop column node_address cascade,
			drop column parent_block_id cascade;
		alter table block_refs drop column node_kind cascade, drop column node_address cascade;
	`); err != nil {
		t.Fatalf("take the store out of the database: %v", err)
	}
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), true, nil)
	if err != nil {
		t.Fatalf("take the sizing of a database with no store: %v", err)
	}
	var printed bytes.Buffer
	report.Write(&printed)
	after := printed.String()
	if !report.Absent || len(report.Kinds) != 0 || report.Refused() {
		t.Fatalf("the sizing of a database with no store reads absent=%t, %d kind(s) and refused=%t, want the store absent, no kind and a pass:\n%s",
			report.Absent, len(report.Kinds), report.Refused(), after)
	}
	for _, line := range []string{"census: store absent (", "census: sizing table name=graph_nodes held=false\n", "census: store absent, sizing ok\n"} {
		if !strings.Contains(after, line) {
			t.Errorf("the sizing of a database with no store prints no line %q:\n%s", line, after)
		}
	}
	if was, now := sizedTerms(t, before), sizedTerms(t, after); len(now) == 0 || fmt.Sprint(was) != fmt.Sprint(now) {
		t.Errorf("with the store taken out the sizing reads %+v, want what it read with the store there, %+v", now, was)
	}
}

// The two versions TestTheSizingReadsAVersionOfATypeOnlyTheStoreDeclares writes of its document: a
// claim, a typed block the store declares and the file this binary embeds does not, holding two
// paragraphs beside one of the document's own; then the same document with the claim's second
// paragraph moved out of it.
const (
	claimVersion1 = ":::claim{#c1}\nInside.\n\nMoving.\n:::\n\nStays.\n"
	claimVersion2 = ":::claim{#c1}\nInside.\n:::\n\nMoving.\n\nStays.\n"
)

// The sizing's replay parses each version against the block schema the store's type nodes build,
// as the block index that indexed it did, and not against the file this binary embeds: a type the
// store holds and the file lacks would read as unreadable, and the replay would count none of the
// moves of a version holding one. The store here declares claim, which blocks.json does not, in a
// supertag node as a type migration writes one, and the process holds the embedded schema, as the
// census subcommand does. A document's two versions, both indexed inside the window, hold a claim,
// and version 2 moves a paragraph out of it: the replay reads both and counts that one move.
func TestTheSizingReadsAVersionOfATypeOnlyTheStoreDeclares(t *testing.T) {
	// The installed schema is the process's, and the sizing installs the store's: the hook puts
	// back the one installed before the test once it is over.
	var installed pmdoc.BlockSchema
	if err := json.Unmarshal(pmdoc.SchemaJSON(), &installed); err != nil {
		t.Fatalf("decode the installed block schema: %v", err)
	}
	pmdoctest.UseBlockSchema(t, installed)
	if pmdoc.IsTypedBlock("claim") {
		t.Fatalf("the installed block schema declares claim, so the store would declare nothing the file lacks")
	}
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event)
		values ($1, 'claim', $1, 'project', 'live',
			jsonb_build_object('block_type', true, 'content', $2::text, 'render', 'host', 'version', $3::int, 'position', 99),
			now(), '{"kind": "system", "id": "graphcensus-sizing-test"}', 0)
	`, schemanodes.KindSupertag, string(pmdoc.BlockContentParagraphs), pmdoc.BuiltSchemaVersion()); err != nil {
		t.Fatalf("declare the typed block claim in the store: %v", err)
	}

	// Each version is stored as the block index takes it, the render of its parse, and each of its
	// blocks' texts as the index sums them, which only a schema declaring claim can give: the test
	// renders them under one and puts the embedded schema back before the census runs.
	declaring, err := json.Marshal(pmdoctest.BlockPointerSchema(pmdoc.BlockAttributes{}))
	if err != nil {
		t.Fatalf("encode a block schema declaring claim: %v", err)
	}
	restore, err := pmdoc.UseManifestForTest(declaring)
	if err != nil {
		t.Fatalf("use a block schema declaring claim: %v", err)
	}
	var rendered []string
	var texts []map[string]string
	for _, markdown := range []string{claimVersion1, claimVersion2} {
		render, _ := renderedBlocks(t, markdown)
		rendered = append(rendered, render)
		texts = append(texts, renderedTexts(t, markdown))
	}
	restore()

	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'claims', 'claims.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&document); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	// The block index records each block a version writes, drops or changes the text of, at that
	// version, and every block of the document's first.
	held := map[string]string{}
	for index, markdown := range rendered {
		number := index + 1
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
		`, document, number, markdown); err != nil {
			t.Fatalf("write version %d of the document: %v", number, err)
		}
		written := map[string]*string{}
		for block, text := range texts[index] {
			if earlier, wrote := held[block]; !wrote || earlier != text {
				written[block] = new(text)
			}
		}
		for block := range held {
			if _, kept := texts[index][block]; !kept {
				written[block] = nil
			}
		}
		for _, block := range slices.Sorted(maps.Keys(written)) {
			if _, err := database.Pool.Exec(ctx, `
				insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, $2, $3, $4)
			`, document, block, number, written[block]); err != nil {
				t.Fatalf("record block %s at version %d: %v", block, number, err)
			}
		}
		held = texts[index]
		if _, err := database.Pool.Exec(ctx, `
			insert into document_indexed_versions (artifact_id, version) values ($1::uuid, $2)
		`, document, number); err != nil {
			t.Fatalf("record version %d indexed: %v", number, err)
		}
	}

	printed := sizingCensus(t, database)
	got := sizedTerms(t, printed)[movedParents]
	if want := (sizedTerm{tuples: 2, writes: 1, read: 2}); got != want {
		t.Errorf("the sizing counts %d move(s) over the claim document's %d version(s) read, %d of them unreadable; want versions 1 and 2 read, neither unreadable, and the 1 move version 2 makes of a paragraph out of its claim counted, since the store's type nodes declare claim\n%s",
			got.writes, got.read, got.unreadable, printed)
	}
}
