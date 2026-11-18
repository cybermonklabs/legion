package graphcensus

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// sizingCensus runs the store census with its growth pass over database, as
// `envoy-dispatch census --store --sizing` runs it, and returns everything it printed.
func sizingCensus(t *testing.T, database *store.Store) string {
	t.Helper()
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), true)
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
// the command to (dispatch://INFRA-1149's c-growth diffs the two). An age measured against
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

// The two versions seedSizedHistory writes of one document: a callout holding two paragraphs
// beside two paragraphs of the document's own, then the same document with the callout's second
// paragraph moved out of it, the document's last paragraph dropped and a new one added.
const (
	sizedVersion1 = ":::callout{#box kind=\"note\" title=\"\"}\nInside.\n\nMoving.\n:::\n\nStays.\n\nDropped.\n"
	sizedVersion2 = ":::callout{#box kind=\"note\" title=\"\"}\nInside.\n:::\n\nMoving.\n\nStays.\n\nAdded.\n"
)

// renderedTexts is each block of markdown as the block index sums it (markdown[From:To] of the
// render), under the id this test gives it: a callout's own id, and a paragraph's text after
// `p-`, since a version's markdown names no paragraph's id.
func renderedTexts(t *testing.T, markdown string) map[string]string {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("parse %q: %v", markdown, err)
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		t.Fatalf("render %q: %v", markdown, err)
	}
	texts := map[string]string{}
	for _, offset := range offsets {
		text := rendered[offset.From:offset.To]
		id := offset.ID
		if offset.Type == "paragraph" {
			id = "p-" + text
		}
		texts[id] = text
	}
	return texts
}

// seedSizedHistory writes, as a build before the store writes them, the history of one document
// the block index indexed twice inside the sizing window, one comment, one clear, a message and
// its reply, one delivery and three events. The block index records a version's blocks in
// document_block_texts and the version itself in document_indexed_versions; this writes those
// rows by hand, each block's text exactly as the index sums it, since this package cannot import
// the index (docs imports it).
//
// The comment is written by "Alice", whose canonical login is alice, anchored to the document and
// owned by it, so its write states an authored_by, an anchored_to, an owned_by and the
// recorded_as alias that keeps the raw id beside the canonical login.
//
// The message is alice's on the issue, addressed to the role sizer, and the reply is the session
// ses_sized's, answering it; the delivery is the message's attempt to that session, which the
// listener took. Three events stand for what the server appends beside such writes: the
// message's message.created, which first addresses the role, and an ask's ask.opened and
// ask.answered, whose ask no row holds, since the ask terms read the events alone.
func seedSizedHistory(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
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
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&document); err != nil {
		t.Fatalf("create the document: %v", err)
	}
	first, second := renderedTexts(t, sizedVersion1), renderedTexts(t, sizedVersion2)
	type text struct {
		block string
		text  *string
	}
	versions := []struct {
		markdown string
		texts    []text
	}{{markdown: sizedVersion1}, {markdown: sizedVersion2, texts: []text{
		{"box", new(second["box"])}, {"p-Dropped.", nil}, {"p-Added.", new(second["p-Added."])},
	}}}
	for block, body := range first {
		versions[0].texts = append(versions[0].texts, text{block, new(body)})
	}
	for index, version := range versions {
		number := index + 1
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, $2, $3, '[{"kind":"user","id":"alice"}]')
		`, document, number, version.markdown); err != nil {
			t.Fatalf("write version %d of the document: %v", number, err)
		}
		for _, written := range version.texts {
			if _, err := database.Pool.Exec(ctx, `
				insert into document_block_texts (artifact_id, block_id, version, text) values ($1::uuid, $2, $3, $4)
			`, document, written.block, number, written.text); err != nil {
				t.Fatalf("record block %s at version %d: %v", written.block, number, err)
			}
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into document_indexed_versions (artifact_id, version) values ($1::uuid, $2)
		`, document, number); err != nil {
			t.Fatalf("record version %d indexed: %v", number, err)
		}
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
		insert into events (issue_key, seq, type, actor, payload, notify) values
			('CORE-1', 1, 'message.created', '{"kind":"user","id":"alice"}',
			 jsonb_build_object('id', $1::text, 'target', 'role:sizer'), false),
			('CORE-1', 2, 'ask.opened', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false),
			('CORE-1', 3, 'ask.answered', '{"kind":"user","id":"bob"}', jsonb_build_object('id', $2::text), false),
			('CORE-1', 4, 'ask.edited', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false),
			('CORE-1', 5, 'ask.resolved', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false),
			('CORE-1', 6, 'ask.asked_of', '{"kind":"user","id":"alice"}', jsonb_build_object('id', $2::text), false),
			('CORE-1', 7, 'ask.follower_added', '{"kind":"session","id":"ses_sized"}',
			 jsonb_build_object('ask_id', $2::text, 'session_id', 'ses_sized'), false)
	`, message, "6a0e5d4c-3b2a-4f19-8e7d-6c5b4a392817"); err != nil {
		t.Fatalf("write the events: %v", err)
	}
	seedSizedRelations(t, database, document, comment)
}

// seedSizedRelations writes, beside the history seedSizedHistory writes, one row for each further
// table a term counts: a mention of the comment that resolved to a session, two body blocks of
// the comment of which one is removed, a link out of the live one, a child issue that waits on
// its parent, a failed result with two screenshots and its retraction, a Keep, and two components
// of one imported snapshot, one under and depending on the other, with CORE-1 affecting the first.
func seedSizedRelations(t *testing.T, database *store.Store, document, comment string) {
	t.Helper()
	ctx := context.Background()
	for _, step := range []struct {
		what, sql string
		args      []any
	}{
		{"write the mention", `
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1::uuid, 'role:sizer', 'btw', 'ses_sized')`, []any{comment}},
		{"write the comment's body blocks", `
			insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
			                            changed_revision, changed_at, removed_at)
			values ($1::uuid, 'b1', 'b1', 0, 'paragraph', 'x', 'A remark.', 1, now(), null),
			       ($1::uuid, 'b2', 'b2', null, 'paragraph', 'y', 'Gone.', 1, now(), now())`, []any{comment}},
		{"write the body block's link", `
			insert into comment_block_refs (comment_id, block_id, from_node, to_kind, to_id)
			values ($1::uuid, 'b1', 'b1', 'issue', 'CORE-1')`, []any{comment}},
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

// The sizing counts every tuple the writers append, term by term, from the database's own
// history: rows a day is the sum over the kinds of writes a day times tuples a write (R4), so a
// kind it leaves out reads the one-year verdict low. The history here holds a version that drops
// a block, which the index states gone, and moves a paragraph out of a callout, which it states
// as the retraction of one parent and the assertion of the other, beside the blocks each version
// writes afresh and the texts INFRA-1253 states, a comment with each fact its write states, the
// read marks that comment and a message move, a clear, a message with a reply, a delivery, the
// events of an ask, and a row of each further table a term counts (seedSizedRelations). Every
// kind R20 gives the five children has a term, and every term is printed, each with its writes
// inside the window or the writes it is assumed at, and the rows a day of graph_tuples is exactly
// their sum.
func TestTheSizingCountsEveryTupleTheWritersAppend(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	printed := sizingCensus(t, database)
	terms := sizedTerms(t, printed)
	// Each kind R20 gives the five children (dispatch://INFRA-1149/spec#b-r-kinds-here), beside the
	// term that counts it. A kind whose write states facts beyond its node has further terms, which
	// want below holds; requirements, plan nodes and checks are typed blocks of a document, which
	// the block term counts with their placement and the block_ref term with their serves, and a
	// version's blocks are what the block and block_text terms count beside its own node.
	kinds := []struct{ child, kind, term string }{
		{movedHere, "comments", "comment"}, {movedHere, "blocks", "block"},
		{movedHere, "block references", "block_ref"}, {movedHere, "people", "person"},
		{movedHere, "sessions", "session"}, {movedHere, "roles", "role"}, {movedHere, "a role's page", "page"},
		{movedWithInbox, "messages", "message"}, {movedWithInbox, "replies", "reply"},
		{movedWithInbox, "mentions", "mention"}, {movedWithInbox, "deliveries", "person_delivery"},
		{movedWithInbox, "delivery attempts", "delivery_attempt"}, {movedWithInbox, "threads", "participation"},
		{movedWithInbox, "read state", "read_mark"}, {movedWithInbox, "body blocks", "body_block"},
		{movedWithBlocks, "issues", "issue"}, {movedWithBlocks, "artifacts", "artifact"},
		{movedWithBlocks, "versions", "document_version"}, {movedWithBlocks, "criteria", "criterion"},
		{movedWithBlocks, "requirements", "block"}, {movedWithBlocks, "plan nodes", "block"},
		{movedWithBlocks, "checks", "block"}, {movedWithBlocks, "results", "result"},
		{movedWithBlocks, "component", "component"},
		{movedWithAsks, "decisions", "decision"}, {movedWithAsks, "readings", "reading"},
		{movedWithAsks, "asks", "ask"}, {movedWithAsks, "answers", "answer"},
		{movedWithLenses, "interactions", "interaction"}, {movedWithLenses, "lenses", "lens"},
		{movedWithLenses, "lens kinds", "lens_kind"}, {movedWithLenses, "constraints", "constraint"},
		{movedWithLenses, "risks", "risk"},
	}
	for _, kind := range kinds {
		if _, printedTerm := terms[kind.term]; !printedTerm {
			t.Errorf("the sizing prints no term for %s of %s", kind.kind, strings.TrimPrefix(kind.child, "dispatch://"))
		}
	}
	want := map[string]sizedTerm{
		"comment": {tuples: 2, writes: 1}, "authored_by": {tuples: 1, writes: 1}, "anchored_to": {tuples: 1, writes: 1},
		"owned_by": {tuples: 1, writes: 1}, "author_alias": {tuples: 1, writes: 1}, "delivery": {tuples: 1},
		// Five blocks are new at version 1 and one at version 2; version 2 drops one and rewrites
		// the callout, so seven rows of the window hold a text.
		"block": {tuples: 4, writes: 6}, "block_restored": {tuples: 4}, "block_gone": {tuples: 1, writes: 1},
		// Version 1 is the document's first indexed version, so it is compared with nothing, and
		// version 2 moved one paragraph out of the callout, which takes a rank in its new place.
		movedParents: {tuples: 2, writes: 1, read: 2}, movedRanks: {tuples: 1, writes: 1},
		// Seven rows hold a text, each a text value; six are a block's first, each a rank and a
		// placement; one deleted a block, whose text value stays as the block's node goes gone.
		"block_ref": {tuples: 2}, "block_text": {tuples: 1, writes: 7}, "block_values": {tuples: 2, writes: 6},
		// Each of the two versions writes its node and six tuples (graphstore.VersionFacts); neither
		// is a file's, and the one document is an issue's, created inside the window.
		"version": {tuples: 6, writes: 2}, "version_bytes": {tuples: 3}, "document_part_of": {tuples: 1, writes: 1},
		// The comment is alice's first, the delivery records ses_sized, and the message.created
		// event is the first to address the role sizer, which takes a page with it.
		"person": {tuples: 2, writes: 1}, "session": {tuples: 5, writes: 1}, "role": {tuples: 2, writes: 1},
		"page": {tuples: 3, writes: 1},
		// The comment and the message each move alice's read_through on their thread (migrations
		// 0095 and 0097's triggers), so she takes part in two threads and each is a read mark; the
		// reply, a session's, reaches her in the message's thread, and the ask opened reaches the
		// person it waits on.
		"message_delivery": {tuples: 1, writes: 1}, "read_mark": {tuples: 1, writes: 2}, "clear": {tuples: 1, writes: 1},
		"dismissal": {tuples: 1}, "snooze": {tuples: 1},
		"message": {tuples: 4, writes: 2}, "message_pointer": {tuples: 1, writes: 1}, "reply": {tuples: 2, writes: 1},
		// The mention is one tuple to its role and one to the session the role resolved to.
		"mention": {tuples: 1, writes: 2}, "participation": {tuples: 1, writes: 2}, "person_delivery": {tuples: 1, writes: 2},
		"ask_asked_of": {tuples: 4, writes: 1}, "body_block": {tuples: 3, writes: 2}, "body_block_gone": {tuples: 1, writes: 1},
		"body_block_ref": {tuples: 2, writes: 1}, "delivery_attempt": {tuples: 14, writes: 1},
		// CORE-2 is a child of CORE-1 and waits on it.
		"issue": {tuples: 2, writes: 2}, "issue_relation": {tuples: 1, writes: 2}, "artifact": {tuples: 3, writes: 1},
		"document_version": {tuples: 2, writes: 2}, "criterion": {tuples: 1, writes: 5 * sizingWindowDays, assumed: true},
		"result": {tuples: 16, writes: 1}, "result_screenshot": {tuples: 1, writes: 2}, "result_retraction": {tuples: 1, writes: 1},
		// The leaf's part_of the root and its depends_on it, and CORE-1's affects of the root.
		"component": {tuples: 2, writes: 2}, "component_relation": {tuples: 1, writes: 3},
		"ask": {tuples: 8, writes: 1}, "ask_edit": {tuples: 2, writes: 1}, "answer": {tuples: 5, writes: 1},
		"ask_resolved": {tuples: 1, writes: 1}, "ask_follower": {tuples: 1, writes: 1}, "keep": {tuples: 2, writes: 1},
		"decision": {tuples: 5, writes: 10 * sizingWindowDays, assumed: true},
		"reading":  {tuples: 1, writes: 10 * sizingWindowDays, assumed: true},
		// No table holds a kind of dispatch://INFRA-1950's yet, so each term reads the writes it
		// is assumed at, and the tuples its writer appends (storeKinds names each writer).
		"interaction":  {tuples: 9, writes: 10 * sizingWindowDays, assumed: true},
		"frame":        {tuples: 1, writes: 500 * sizingWindowDays, assumed: true},
		"lens":         {tuples: 10, writes: 2 * sizingWindowDays, assumed: true},
		"lens_finding": {tuples: 8, writes: 10 * sizingWindowDays, assumed: true},
		"lens_kind":    {tuples: 8, writes: 22, assumed: true},
		"constraint":   {tuples: 11, writes: 1 * sizingWindowDays, assumed: true},
		"risk":         {tuples: 11, writes: 11 * sizingWindowDays, assumed: true},
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
	// The history's two versions were written before the store held a version node, so the
	// version backfill owes each its node and six tuples once, and the year projects those beside
	// the rate (backfillVersions).
	for _, line := range []string{
		"census: sizing backfill kind=version moved_by=" + movedWithBlocks + " owed_rows=2 nodes_a_row=1 tuples_a_row=6 nodes=2 tuples=12 ",
		"census: sizing backfill-rows table=graph_tuples rows=12\n",
		"census: sizing backfill-rows table=graph_nodes rows=2\n",
	} {
		if !strings.Contains(printed, line) {
			t.Errorf("the sizing prints no line %q:\n%s", line, printed)
		}
	}
}

// The sizing counts what the block index drain (drainBlockIndexes) owes the store once, beside
// the rates: a rank, a text and a placement for every stored block, and for every block at the
// top of an issue's spec, Verification or plan document the retraction of its part_of to the
// document and its part_of to the issue. A spec of two top-level blocks and one nested, and an
// upload of one, are stored as the release before stored them; the sizing owes four blocks their
// values and two their top, and once the drain has run it owes neither.
func TestTheSizingCountsWhatTheDrainOwesEachStoredBlock(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	for _, document := range []struct {
		slug    string
		primary bool
		blocks  [][2]string
	}{
		{"spec", true, [][2]string{{"outer", ""}, {"inner", "outer"}, {"after", ""}}},
		{"notes-md", false, [][2]string{{"note", ""}}},
	} {
		var artifactID, refKey string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', $1, $1, 'doc', $2, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, document.slug, document.primary).Scan(&artifactID, &refKey); err != nil {
			t.Fatalf("create document %s: %v", document.slug, err)
		}
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
	}
	owes := func(printed string, values, tops int) {
		t.Helper()
		for _, line := range []string{
			fmt.Sprintf("census: sizing backfill kind=block_values moved_by=%s owed_rows=%d nodes_a_row=0 tuples_a_row=3 nodes=0 tuples=%d ",
				movedWithBlocks, values, 3*values),
			fmt.Sprintf("census: sizing backfill kind=block_top moved_by=%s owed_rows=%d nodes_a_row=0 tuples_a_row=2 nodes=0 tuples=%d ",
				movedWithBlocks, tops, 2*tops),
		} {
			if !strings.Contains(printed, line) {
				t.Errorf("the sizing prints no line %q:\n%s", line, printed)
			}
		}
	}
	owes(sizingCensus(t, database), 4, 2)
	if _, err := drainBlockIndexes(ctx, database); err != nil {
		t.Fatalf("run the block index drain: %v", err)
	}
	owes(sizingCensus(t, database), 0, 0)
}

// The sizing reads nothing of the store: R4 takes its readings before the branch's first
// migration, on a database that holds no graph_nodes, no graph_tuples and no node column on any
// projection. The test takes the sizing of a seeded history, then takes every object of the store
// out of the same database and takes it again: the census says the store is absent and reads no
// kind, the sizing passes, and every term reads what it read with the store there.
func TestTheSizingRunsOnADatabaseWithNoStore(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	before := sizingCensus(t, database)
	if _, err := database.Pool.Exec(context.Background(), `
		drop table graph_tuples, graph_nodes, graph_erasures cascade;
		alter table comments drop column node_kind cascade, drop column node_address cascade;
		alter table document_blocks drop column node_kind cascade, drop column node_address cascade,
			drop column parent_block_id cascade;
		alter table block_refs drop column node_kind cascade, drop column node_address cascade;
	`); err != nil {
		t.Fatalf("take the store out of the database: %v", err)
	}
	report, err := StoreCensus(context.Background(), database.Pool.Config().ConnString(), true)
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
