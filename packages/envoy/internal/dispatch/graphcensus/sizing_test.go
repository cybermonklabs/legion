package graphcensus

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
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
// over them (indexTheArchive) without the window counting what the index writes.
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
			 jsonb_build_object('ask_id', $2::text, 'session_id', 'ses_sized'), false, now())
	`, message, "6a0e5d4c-3b2a-4f19-8e7d-6c5b4a392817"); err != nil {
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
}

// seedSizedRelations writes, beside the history seedSizedHistory writes, one row for each further
// table a term counts: a mention of the comment that resolved to a session and its attempt, which
// the listener took, two body blocks of the comment of which one is removed, a link out of the
// live one, a child issue that waits on its parent, a failed result with two screenshots and its
// retraction, a Keep, two components of one imported snapshot, one under and depending on the
// other, with CORE-1 affecting the first, alice's dismissal of the message's thread, and her
// snooze of an ask the session ses_asker opened before the window, so that inside the window
// the snooze is the one write of it a term counts.
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
)

// seededDrives are the drives of the store's writers over the history seedSizedHistory wrote, one
// after another: the block index over the archive (refs.IndexBlocks, then graphstore.Caused, as a
// writer with no write phase runs it), the backfill's five passes, and the outbox, run whole,
// publishing the comment's comment.created, then the receipt of its mention, then a held copy.
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
	return []StoreDrive{{
		// A version's blocks, as the index states them over the archive's three versions: the
		// blocks it inserts afresh, each with its node's base attributes, its in_document and its
		// part_of; the paragraph the second version drops, stated gone; the paragraph it moves out
		// of the callout, its old parent retracted and its new one asserted; the dropped paragraph
		// the third version brings back, stated live, with its in_document and at most a parent the
		// restore moved; and the added paragraph's reference to CORE-1.
		Name: archiveIndexDrive, Sites: []string{"refs/blocks.go#Queue", "graphstore/write.go#Caused"},
		Database: database, Drive: func(t *testing.T) { indexTheArchive(t, database) },
		Counts: map[StoredKind][]string{
			{refs.BlockNodeKind, graphstore.AttributeVisibility}: {"block"},
			{refs.BlockNodeKind, graphstore.AttributeState}:      {"block", "block_restored", "block_gone"},
			{refs.BlockNodeKind, refs.TupleInDocument}:           {"block", "block_restored"},
			{refs.BlockNodeKind, graphstore.TuplePartOf}:         {"block", "block_restored", movedParents},
			{refs.BlockNodeKind, "mentions"}:                     {"block_ref"},
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
// (graphstoreDrives), the seed (seedRunDrives), and the drives of the sites whose packages import
// this one, which this package's external test registers (ImportedDrives). It holds them to
// driving every site the register of the store's writers lists (requireEveryListedSiteDriven).
// Each drive is the writer itself and not a copy of what it writes. It answers how many tuples of
// each kind each drive appended, read off graph_tuples by the sequences the drive's tuples took,
// and the terms each drive says count each kind it appends; and it logs, for every listed site,
// what each drive that runs it appended.
func writeTheStore(t *testing.T, database *store.Store) (map[writtenKind]int64, map[writtenKind][]string) {
	t.Helper()
	ctx := context.Background()
	drives := slices.Concat(seededDrives(database), graphstoreDrives(t), seedRunDrives(t))
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
		_, won, write, err := refs.IndexBlocks(ctx, tx, version.artifact, version.number, version.at, version.markdown,
			blocks, text.ExtractAt(version.markdown, sizedServerURL), nil)
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
// and a row of each further table a term counts (seedSizedRelations). Every term is printed with
// the tuples a write this test states for it and its writes inside the window or the writes it is
// assumed at, none of them zero, and the rows a day of graph_tuples is exactly their sum.
func TestTheSizingCountsEveryTupleTheWritersAppend(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	appended, countedBy := writeTheStore(t, database)
	printed := sizingCensus(t, database)
	terms := sizedTerms(t, printed)
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
		"comment": {tuples: 2, writes: 1}, "authored_by": {tuples: 1, writes: 1}, "anchored_to": {tuples: 1, writes: 1},
		"owned_by": {tuples: 1, writes: 1}, "author_alias": {tuples: 1, writes: 1},
		// The mention's attempt to ses_sized, the comment.created the bus delivered to the role
		// sizer, and the copy the fallback pass published to ses_copy.
		"delivery": {tuples: 1, writes: 1}, "bus_delivery": {tuples: 1, writes: 1}, "fallback_delivery": {tuples: 1, writes: 1},
		// Five blocks are new at version 1 and one at version 2; version 2 drops one and rewrites
		// the callout, and version 3 brings the dropped one back, so eight rows of the window hold
		// a text.
		"block": {tuples: 4, writes: 6}, "block_restored": {tuples: 4, writes: 1}, "block_gone": {tuples: 1, writes: 1},
		// Version 1 is the document's first indexed version, so it is compared with nothing,
		// version 2 moved one paragraph out of the callout, and version 3 moved none.
		movedParents: {tuples: 2, writes: 1, read: 3},
		"block_ref":  {tuples: 2, writes: 1}, "block_text": {tuples: 2, writes: 8},
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
		"message": {tuples: 4, writes: 2}, "message_pointer": {tuples: 1, writes: 1}, "reply": {tuples: 2, writes: 1},
		// The mention is one tuple to its role and one to the session the role resolved to.
		"mention": {tuples: 1, writes: 2}, "participation": {tuples: 1, writes: 2}, "person_delivery": {tuples: 1, writes: 2},
		"ask_asked_of": {tuples: 4, writes: 1}, "body_block": {tuples: 3, writes: 2}, "body_block_gone": {tuples: 1, writes: 1},
		// The attempts are the mention's and the message's.
		"body_block_ref": {tuples: 2, writes: 1}, "delivery_attempt": {tuples: 14, writes: 2},
		// CORE-2 is a child of CORE-1 and waits on it. The archive was written before the window.
		"issue": {tuples: 2, writes: 2}, "issue_relation": {tuples: 1, writes: 2}, "artifact": {tuples: 3, writes: 1},
		"document_version": {tuples: 2, writes: 3}, "criterion": {tuples: 1, writes: 5 * sizingWindowDays, assumed: true},
		"result": {tuples: 16, writes: 1}, "result_screenshot": {tuples: 1, writes: 2}, "result_retraction": {tuples: 1, writes: 1},
		// The leaf's part_of the root and its depends_on it, and CORE-1's affects of the root.
		"component": {tuples: 2, writes: 2}, "component_relation": {tuples: 1, writes: 3},
		"ask": {tuples: 8, writes: 1}, "ask_edit": {tuples: 2, writes: 1}, "answer": {tuples: 5, writes: 1},
		"ask_resolved": {tuples: 1, writes: 1}, "ask_follower": {tuples: 1, writes: 1}, "keep": {tuples: 2, writes: 1},
		"decision": {tuples: 5, writes: 10 * sizingWindowDays, assumed: true},
		"reading":  {tuples: 1, writes: 10 * sizingWindowDays, assumed: true},
		// No table holds a kind of the lenses child's yet, so each term reads the writes it
		// is assumed at.
		"interaction":  {tuples: 4, writes: 10 * sizingWindowDays, assumed: true},
		"frame":        {tuples: 1, writes: 500 * sizingWindowDays, assumed: true},
		"lens":         {tuples: 6, writes: 2 * sizingWindowDays, assumed: true},
		"lens_finding": {tuples: 4, writes: 10 * sizingWindowDays, assumed: true},
		"lens_kind":    {tuples: 8, writes: 11, assumed: true},
		"constraint":   {tuples: 5, writes: 1 * sizingWindowDays, assumed: true},
		"risk":         {tuples: 4, writes: 11 * sizingWindowDays, assumed: true},
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
