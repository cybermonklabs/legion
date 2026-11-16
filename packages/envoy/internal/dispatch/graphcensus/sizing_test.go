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
// the block index indexed twice inside the sizing window, one comment and one clear. The block
// index records a version's blocks in document_block_texts and the version itself in
// document_indexed_versions; this writes those rows by hand, each block's text exactly as the
// index sums it, since this package cannot import the index (docs imports it).
//
// The comment is written by "Alice", whose canonical login is alice, anchored to the document and
// owned by it, so its write states an authored_by, an anchored_to, an owned_by and the
// recorded_as alias that keeps the raw id beside the canonical login.
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
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::uuid, '{"kind":"user","id":"Alice"}', 'A remark on the spec.', jsonb_build_object('artifact_id', $1::text))
	`, document); err != nil {
		t.Fatalf("write the comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into user_agent_state (login, session_id, cleared_before) values ('alice', 'ses_sized', now())
	`); err != nil {
		t.Fatalf("write the clear: %v", err)
	}
}

// sizedTerm is one term of the sizing as the report prints it: its tuples a write, the writes the
// window holds, and, for the moved parents, the versions it read and could not parse.
type sizedTerm struct {
	tuples, writes, read, unreadable int64
}

var (
	tuplesAWriteLine = regexp.MustCompile(`(?m)^census: sizing tuples-a-write kind=(\S+) tuples=(\d+) `)
	shareLine        = regexp.MustCompile(`(?m)^census: sizing share kind=(\S+) moved_by=\S+ writes_in_window=(\d+) share_of_tuples=\S+(?: versions_read=(\d+) versions_unreadable=(\d+))?$`)
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
// writes afresh and the texts INFRA-1253 states, a comment with each fact its write states, and
// the read mark that comment moves and a clear, which INFRA-1152 states. Every term is printed,
// each with its writes inside the window, and the rows a day of graph_tuples is exactly their sum.
func TestTheSizingCountsEveryTupleTheWritersAppend(t *testing.T) {
	database := storetest.Open(t)
	seedSizedHistory(t, database)
	printed := sizingCensus(t, database)
	terms := sizedTerms(t, printed)
	want := map[string]sizedTerm{
		"comment": {tuples: 2, writes: 1}, "authored_by": {tuples: 1, writes: 1}, "anchored_to": {tuples: 1, writes: 1},
		"owned_by": {tuples: 1, writes: 1}, "author_alias": {tuples: 1, writes: 1}, "delivery": {tuples: 1},
		// Five blocks are new at version 1 and one at version 2; version 2 drops one and rewrites
		// the callout, so seven rows of the window hold a text.
		"block": {tuples: 4, writes: 6}, "block_restored": {tuples: 4}, "block_gone": {tuples: 1, writes: 1},
		// Version 1 is the document's first indexed version, so it is compared with nothing, and
		// version 2 moved one paragraph out of the callout.
		movedParents: {tuples: 2, writes: 1, read: 2},
		"block_ref":  {tuples: 2}, "block_text": {tuples: 2, writes: 7},
		// The comment moves its author's read_through on its thread (migration 0095's trigger),
		// which is a read mark of its own.
		"message_delivery": {tuples: 1}, "read_mark": {tuples: 1, writes: 1}, "clear": {tuples: 1, writes: 1},
		"dismissal": {tuples: 1}, "snooze": {tuples: 1},
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
