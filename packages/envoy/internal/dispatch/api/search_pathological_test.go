package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestSavingAPathologicalBodyIsBounded holds the two defects that made saving a pathological body
// quadratic to the work each one does, which a busy machine cannot move. It held the save's time
// before, and correct code failed it on loaded runners (39 times against a bound of 32, and 72).
//
// Postgres's text-search parser rescans a whitespace-free run from every point where letters or
// digits meet an underscore, so a run of `a_` cost time quadratic in its length: 64 KiB of it saved
// in 46 s. search_text (migration 0056) breaks a run after its sixteenth underscore-joined segment.
// The parser's work reaches no count a test can read; its input does. The row's server runs on
// connections whose search_path finds a recording function ahead of each of pg_catalog's parser
// functions (recordPathologicalParserInput), so each save is read as the parser received it,
// whatever trigger or function handed it the text by a bare name: every save must hand the parser
// exactly parserCallsPerSave texts, at least as many characters as the text it saved, and no run
// of more than searchTextSegments segments. With search_text answering its input unchanged, the
// 1 KiB document of `a_` hands it a run of 507; a trigger that parses the text once more, or once
// for every so many characters of it, makes more calls. A call that does not resolve a bare name
// through the session's search_path is not recorded: the database must keep none of the kinds it
// can hold (assertParserReachedByBareName), and a call by the qualified name in a function's text
// is not held at all. A run joined again after search_text, as the ask-block removal of the
// documents trigger can join one, is held by the row of the change that moves that removal ahead
// of search_text, not by this one: no document here holds an ask block.
//
// pmdoc's Update wrote each new block with its own insert, and ygo walks a fragment from its first
// child to the index each insert or delete names, so replacing a quote of n headings with a quote
// of n rules took n²/2 steps: 3.9 to 4.3 s of CPU at 21,844 blocks, the most one write's element
// limit allows of it, against 0.68 to 0.69 s with one insert for the run (2026-11-21). Update reads
// and changes a fragment's children through one struct as it writes them, and the row reads each
// call it makes (pmdoc.RecordChildEditsForTest): every read of a fragment's children and every
// positional delete and insert, in the order ygo receives them. Each write of the replacement must
// make exactly the positional calls seedChanges and replacementChanges name, and as many reads at
// 5,461 and 21,842 blocks as at 682. A write that deletes or inserts the run one block at a time,
// or reads the quote's children once for each block, makes a call for each block.
//
// The largest save of each ladder is the most a caller can send: a mebibyte of document,
// contracts.DispatchBlockUnits characters of comment, and 21,844 blocks in the replaced quote. Each
// document's run is a code block's text, which the document stores and search indexes whole:
// written as paragraph text, the larger runs make more elements than one write may
// (TestUploadRefusesADocumentTooLargeToStore). The `a_b*` and `)_` documents leave no long run
// whatever search_text does, so for them the row holds only that each save is answered 201 and
// hands the parser its text.
//
// What no count here holds: a change inside ygo; a change in pmdoc that reaches the fragment around
// that struct, or makes more than one ygo call inside one of its methods, among them the read of
// the whole document a write makes when it would create an element this build may not create yet
// (heldInFragment), which none of these writes makes; a parser call by the qualified name in a
// function's text; and a save that costs more CPU without making another call or handing the
// parser another text or a longer run, in Go or in Postgres. The count that load cannot move for
// that last is the instructions the save executes, and this box gives a test no hardware counter
// (kernel.perf_event_paranoid is 4).
//
// Settlement is held off, so no settlement writes a document while a save is read; the test
// server's shutdown settles each document still loaded, after every count is read. No test in this
// package calls t.Parallel, so no other write runs beside the one read.
func TestSavingAPathologicalBodyIsBounded(t *testing.T) {
	database := storetest.Open(t)
	assertParserReachedByBareName(t, database)
	recorded, parsed := recordPathologicalParserInput(t, database)
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour, store: recorded})
	issue := createArtifactIssue(t, handler)
	// save runs one save and holds what it handed the parser (parserInput) against what it saved,
	// the newest text of table's column.
	save := func(t *testing.T, what, table, column string, write func()) {
		t.Helper()
		input := parsed(write)
		var saved int64
		if err := database.Pool.QueryRow(context.Background(), fmt.Sprintf(
			`select length(%s) from %s where %[1]s is not null order by created_at desc limit 1`, column, table)).Scan(&saved); err != nil {
			t.Fatalf("read the length of what %s saved: %v", what, err)
		}
		t.Logf("%s saved %d characters and handed the parser %s", what, saved, input)
		switch {
		case input.calls != parserCallsPerSave:
			t.Fatalf("%s handed the parser %d texts, want %d: search_vector parses the vector's head and its body once each, and a save that reached the parser by no bare name hands it none this row can read",
				what, input.calls, parserCallsPerSave)
		case input.characters < saved:
			t.Fatalf("%s handed the parser %d characters, fewer than the %d it saved: the text was parsed by a route this row does not read",
				what, input.characters, saved)
		case input.longest > searchTextSegments:
			t.Fatalf("%s handed the parser a run of %d underscore-joined segments, want at most %d: the text-search parser rescans a run to its end from every segment",
				what, input.longest, searchTextSegments)
		}
	}
	documents := 0
	for _, test := range []struct {
		shape string
		sizes []int
	}{
		{"a_", []int{1 << 10, 64 << 10, 1 << 20}},
		{"a_b*", []int{2 << 10, 128 << 10, 1 << 20}},
		{")_", []int{256, 16 << 10, 256 << 10}},
	} {
		t.Run("document "+test.shape, func(t *testing.T) {
			for _, size := range test.sizes {
				const fence = "```\n"
				body := fence + strings.Repeat(test.shape, (size-2*len(fence)-1)/len(test.shape)) + "\n" + fence
				documents++
				fields := map[string]string{"name": fmt.Sprintf("pathological-%d.md", documents)}
				save(t, fmt.Sprintf("a document of %s of `%s`", sizeText(size), test.shape), "artifact_versions", "markdown", func() {
					response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", fields, "body.md", "text/markdown", []byte(body), "alice")
					if response.Code != http.StatusCreated {
						t.Fatalf("upload %s of %q: status=%d body=%.200s", sizeText(size), test.shape, response.Code, response.Body.String())
					}
				})
			}
		})
	}
	// A comment's whole body is one search_text run, as a document's is, so the same shape is held
	// at the longest block a caller may send, and at a thirty-second and a quarter of it.
	t.Run("comment a_", func(t *testing.T) {
		const shape = "a_"
		for _, size := range []int{contracts.DispatchBlockUnits / 32, contracts.DispatchBlockUnits / 4, contracts.DispatchBlockUnits} {
			body := strings.Repeat(shape, size/len(shape))
			save(t, fmt.Sprintf("a comment of %s of `%s`", sizeText(size), shape), "comments", "body", func() {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": body,
				}, "alice")
				if response.Code != http.StatusCreated {
					t.Fatalf("comment of %s of %q: status=%d body=%.200s", sizeText(size), shape, response.Code, response.Body.String())
				}
			})
		}
	})
	// A quote of two rules and then many headings, written into a new document and then rewritten
	// as a quote of the same two rules and as many rules again: the first write inserts the quote
	// and its children (seedChanges), the second deletes the run of headings after the two kept
	// rules and inserts its rules in one call each (replacementChanges).
	t.Run("replacing many blocks", func(t *testing.T) {
		const kept, from, to = "> ***\n> ***\n", "> #\n", "> ***\n"
		owner := "/api/v1/issues/" + issue.Key + "/artifacts"
		// firstReads is how many reads each write made at the first size, which it must make at
		// every size.
		firstReads := map[string]int{}
		for _, blocks := range []int{682, 5_461, 21_842} {
			documents++
			name := fmt.Sprintf("many-blocks-%d.md", documents)
			for _, write := range []struct {
				kind, what  string
				body        string
				fields      func() map[string]string
				wantChanges []pmdoc.ChildEdit
			}{
				{
					kind:        "the seed",
					what:        fmt.Sprintf("writing a quote of 2 rules and %d headings into a new document", blocks),
					body:        kept + strings.Repeat(from, blocks),
					fields:      func() map[string]string { return map[string]string{"name": name} },
					wantChanges: seedChanges(blocks),
				},
				{
					kind: "the replacement",
					what: fmt.Sprintf("replacing the %d headings after its 2 kept rules with as many rules", blocks),
					body: kept + strings.Repeat(to, blocks),
					fields: func() map[string]string {
						return map[string]string{"name": name, "precondition": encodeUploadBase(t, uploadBaseNamed(t, handler, owner, name))}
					},
					wantChanges: replacementChanges(blocks),
				},
			} {
				fields := write.fields()
				var edits []pmdoc.ChildEdit
				save(t, write.what, "artifact_versions", "markdown", func() {
					edits = recordChildEdits(func() {
						response := multipartRequest(t, handler, owner, fields, "body.md", "text/markdown", []byte(write.body), "alice")
						if response.Code != http.StatusCreated {
							t.Fatalf("%s: status=%d body=%.200s", write.what, response.Code, response.Body.String())
						}
					})
				})
				t.Logf("%s made %s", write.what, childEditsText(edits))
				reads, changes := 0, []pmdoc.ChildEdit{}
				for _, edit := range edits {
					if edit.Kind == pmdoc.ChildRead {
						reads++
						continue
					}
					changes = append(changes, edit)
				}
				if !slices.Equal(changes, write.wantChanges) {
					t.Fatalf("%s deleted and inserted by %s, want %s: each call walks the fragment it names from its first child",
						write.what, childEditsText(changes), childEditsText(write.wantChanges))
				}
				first, seen := firstReads[write.kind]
				if !seen {
					firstReads[write.kind] = reads
					continue
				}
				if reads != first {
					t.Fatalf("%s read a fragment's children %d times, and %s at 682 blocks %d: a read walks every child of the fragment it reads",
						write.what, reads, write.kind, first)
				}
			}
		}
	})
}

// seedChanges is every positional call writing a quote of 2 rules and blocks headings into a new
// document makes: it inserts the quote's 2 + blocks children into the quote it builds, which no
// document holds yet, and then the quote, the tail of the document. An empty heading and a rule
// have no children to insert.
func seedChanges(blocks int) []pmdoc.ChildEdit {
	return []pmdoc.ChildEdit{
		{Kind: pmdoc.ChildInsert, Index: 0, Count: 2 + blocks},
		{Kind: pmdoc.ChildInsert, Index: 0, Count: 1},
	}
}

// replacementChanges is every positional call rewriting that quote's blocks headings as rules
// makes: it deletes the run of headings at index 2, after the two kept rules, and inserts as many
// rules there.
func replacementChanges(blocks int) []pmdoc.ChildEdit {
	return []pmdoc.ChildEdit{
		{Kind: pmdoc.ChildDelete, Index: 2, Count: blocks},
		{Kind: pmdoc.ChildInsert, Index: 2, Count: blocks},
	}
}

// recordChildEdits runs write and answers every call it made to a fragment's children, reads and
// positional deletes and inserts, in the order ygo received them (pmdoc.RecordChildEditsForTest),
// restoring the recorder it found however write ends.
func recordChildEdits(write func()) []pmdoc.ChildEdit {
	var mu sync.Mutex
	var edits []pmdoc.ChildEdit
	restore := pmdoc.RecordChildEditsForTest(func(edit pmdoc.ChildEdit) {
		mu.Lock()
		defer mu.Unlock()
		edits = append(edits, edit)
	})
	defer restore()
	write()
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(edits)
}

// childEditsText writes edits as a failure names them: every call when there are at most 16, else
// the count and the first three and the last.
func childEditsText(edits []pmdoc.ChildEdit) string {
	if len(edits) <= 16 {
		return fmt.Sprintf("%d calls, %v", len(edits), edits)
	}
	return fmt.Sprintf("%d calls, %v … %v", len(edits), edits[:3], edits[len(edits)-1])
}

// searchTextSegments is the most underscore-joined segments search_text (migration 0056) leaves
// in one run before it breaks the run with a space, which bounds the parser's rescans of it.
const searchTextSegments = 16

// parserCallsPerSave is how many texts each save of this row hands the parser: its trigger calls
// search_vector once, which parses the vector's head, empty for a document or a comment, and its
// body (migration 0087), once each. A body too long for one vector would make search_vector parse
// cuts of it as well; no save here is one.
const parserCallsPerSave = 2

// parserInput is what Postgres's text-search parser was handed during one save, as
// recordPathologicalParserInput reads it: how many texts, how many characters in all, and the most
// underscore-joined segments in one run of any of them, read only where some run had more than
// searchTextSegments (0 otherwise). A segment is letters, digits, `.` and `-` ending in `_`, the
// run search_text breaks.
type parserInput struct{ calls, characters, longest int64 }

func (p parserInput) String() string {
	run := fmt.Sprintf("no run of more than %d segments", searchTextSegments)
	if p.longest > 0 {
		run = fmt.Sprintf("a run of %d segments", p.longest)
	}
	return fmt.Sprintf("%d texts of %d characters in all, %s", p.calls, p.characters, run)
}

// parserForms is every form of pg_catalog's functions that hands a text to the text-search parser
// and answers what it made of it, as the recorder shadows each (recordPathologicalParserInput):
// its parameters, what it answers, the text the parser reads, and the call of pg_catalog's own.
var parserForms = []struct{ name, params, returns, text, answer string }{
	{"to_tsvector", "config regconfig, body text", "tsvector", "body", "return pg_catalog.to_tsvector(config, body)"},
	{"to_tsvector", "body text", "tsvector", "body", "return pg_catalog.to_tsvector(body)"},
	{"to_tsvector", "config regconfig, body json", "tsvector", "body::text", "return pg_catalog.to_tsvector(config, body)"},
	{"to_tsvector", "body json", "tsvector", "body::text", "return pg_catalog.to_tsvector(body)"},
	{"to_tsvector", "config regconfig, body jsonb", "tsvector", "body::text", "return pg_catalog.to_tsvector(config, body)"},
	{"to_tsvector", "body jsonb", "tsvector", "body::text", "return pg_catalog.to_tsvector(body)"},
	{"json_to_tsvector", "config regconfig, body json, filter jsonb", "tsvector", "body::text", "return pg_catalog.json_to_tsvector(config, body, filter)"},
	{"json_to_tsvector", "body json, filter jsonb", "tsvector", "body::text", "return pg_catalog.json_to_tsvector(body, filter)"},
	{"jsonb_to_tsvector", "config regconfig, body jsonb, filter jsonb", "tsvector", "body::text", "return pg_catalog.jsonb_to_tsvector(config, body, filter)"},
	{"jsonb_to_tsvector", "body jsonb, filter jsonb", "tsvector", "body::text", "return pg_catalog.jsonb_to_tsvector(body, filter)"},
	{"ts_parse", "parser_name text, body text, out tokid integer, out token text", "setof record", "body", "return query select * from pg_catalog.ts_parse(parser_name, body)"},
	{"ts_parse", "parser_id oid, body text, out tokid integer, out token text", "setof record", "body", "return query select * from pg_catalog.ts_parse(parser_id, body)"},
}

// recordPathologicalParserInput answers a store on database whose connections find, ahead of
// pg_catalog's, a function of a schema of their own for each of parserForms, and a function that
// runs a save and answers what the parser was handed while it ran (parserInput). Each such
// function records the text and then calls pg_catalog's. A call resolves a bare name through the
// session's search_path when it is planned, so on those connections the text any trigger or
// plpgsql function hands the parser by a bare name passes the recorder first. What resolves the
// parser otherwise - by its qualified name in a function's text, in an expression the database
// stores already resolved, in a function that sets its own search_path, or through
// tsvector_update_trigger - is not recorded; assertParserReachedByBareName holds the database to
// having none of the last three. The recorder keeps its counts in three sequences, which no
// rollback undoes, so a call whose subtransaction rolls back, as search_vector's does for a vector
// too long, is counted too. The store is storetest.Reopened's, every pool of it closed when t
// ends, before database is dropped.
func recordPathologicalParserInput(t *testing.T, database *store.Store) (*store.Store, func(save func()) parserInput) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`create schema pathological_parser`,
		`create sequence pathological_parser.calls minvalue 0`,
		`create sequence pathological_parser.characters minvalue 0`,
		`create sequence pathological_parser.longest_run minvalue 0`,
		fmt.Sprintf(`create function pathological_parser.record(body text) returns void language plpgsql volatile strict as $$
		 declare
		   longest bigint;
		 begin
		   perform nextval('pathological_parser.calls');
		   perform setval('pathological_parser.characters', (select last_value from pathological_parser.characters) + length(body));
		   if body ~ '(?:[A-Za-z0-9.-]+_){%d}' then
		     select coalesce(max(length(run[1]) - length(replace(run[1], '_', ''))), 0) into longest
		       from regexp_matches(body, '((?:[A-Za-z0-9.-]+_)+)', 'g') as run;
		     perform setval('pathological_parser.longest_run', greatest(longest, (select last_value from pathological_parser.longest_run)));
		   end if;
		 end $$`, searchTextSegments+1),
	}
	for _, form := range parserForms {
		statements = append(statements, fmt.Sprintf(`create function pathological_parser.%s(%s) returns %s language plpgsql volatile strict as $$
		 begin
		   perform pathological_parser.record(%s);
		   %s;
		 end $$`, form.name, form.params, form.returns, form.text, form.answer))
	}
	for _, statement := range statements {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("record the parser's input: %s: %v", statement, err)
		}
	}
	recorded := storetest.Reopened(t, database, func(config *pgxpool.Config) {
		config.ConnConfig.RuntimeParams["search_path"] = "pathological_parser, pg_catalog, public"
	})
	return recorded, func(save func()) parserInput {
		if _, err := database.Pool.Exec(ctx, `select setval('pathological_parser.calls', 0, true),
			setval('pathological_parser.characters', 0, true), setval('pathological_parser.longest_run', 0, true)`); err != nil {
			t.Fatalf("reset the parser's recorder: %v", err)
		}
		save()
		var input parserInput
		if err := database.Pool.QueryRow(ctx, `select (select last_value from pathological_parser.calls),
			(select last_value from pathological_parser.characters), (select last_value from pathological_parser.longest_run)`).Scan(&input.calls, &input.characters, &input.longest); err != nil {
			t.Fatalf("read the parser's recorder: %v", err)
		}
		return input
	}
}

// assertParserReachedByBareName fails t where the database holds a call of the parser that a
// session's search_path cannot route through the recorder (recordPathologicalParserInput): an
// expression the database keeps already resolved - a column's default or generated value, an
// index's expression or predicate, a check constraint, the body of a SQL function written BEGIN
// ATOMIC, a policy, a rule - that names one of pg_catalog's parser functions by its id; a trigger
// that runs tsvector_update_trigger; and a function that sets its own search_path, under which a
// bare name it or anything it calls resolves past the recorder. It reads the stored expressions
// as their node trees, where a function call is `:funcid` and the function's id. Objects of
// pg_catalog and information_schema are left out.
func assertParserReachedByBareName(t *testing.T, database *store.Store) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		with parser as (
			select oid from pg_proc
			 where pronamespace = 'pg_catalog'::regnamespace
			   and proname in ('to_tsvector', 'json_to_tsvector', 'jsonb_to_tsvector', 'ts_parse', 'ts_debug',
			                   'ts_headline', 'tsvector_update_trigger', 'tsvector_update_trigger_column')
		), ours(namespace) as (
			select oid from pg_namespace where nspname not in ('pg_catalog', 'information_schema') and nspname !~ '^pg_toast'
		), stored(what, tree) as (
			select format('the default or generated value of %s.%s', d.adrelid::regclass, a.attname), d.adbin::text
			  from pg_attrdef d join pg_attribute a on (a.attrelid, a.attnum) = (d.adrelid, d.adnum)
			  join pg_class c on c.oid = d.adrelid where c.relnamespace in (select namespace from ours)
			union all
			select format('index %s', i.indexrelid::regclass), coalesce(i.indexprs::text, '') || coalesce(i.indpred::text, '')
			  from pg_index i join pg_class c on c.oid = i.indexrelid where c.relnamespace in (select namespace from ours)
			union all
			select format('constraint %s', c.conname), c.conbin::text
			  from pg_constraint c where c.conbin is not null and c.connamespace in (select namespace from ours)
			union all
			select format('the SQL body of %s', p.oid::regprocedure), p.prosqlbody::text
			  from pg_proc p where p.prosqlbody is not null and p.pronamespace in (select namespace from ours)
			union all
			select format('policy %s on %s', p.polname, p.polrelid::regclass), coalesce(p.polqual::text, '') || coalesce(p.polwithcheck::text, '')
			  from pg_policy p
			union all
			select format('rule %s on %s', r.rulename, r.ev_class::regclass), coalesce(r.ev_action::text, '') || coalesce(r.ev_qual::text, '')
			  from pg_rewrite r join pg_class c on c.oid = r.ev_class where c.relnamespace in (select namespace from ours)
		)
		select 0, what from stored where exists (select 1 from parser where stored.tree ~ (':funcid ' || parser.oid || '\M'))
		union all
		select 0, format('trigger %s on %s runs %s', t.tgname, t.tgrelid::regclass, t.tgfoid::regproc)
		  from pg_trigger t where t.tgfoid in (select oid from parser)
		union all
		select 0, format('%s sets its own search_path', p.oid::regprocedure)
		  from pg_proc p where p.pronamespace in (select namespace from ours)
		   and exists (select 1 from unnest(p.proconfig) as setting where setting like 'search_path=%')
		union all
		select (select count(*) from parser) * 1000000 + (select count(*) from stored), ''`)
	if err != nil {
		t.Fatalf("read the calls of the parser the database keeps: %v", err)
	}
	var found []string
	var read int64
	for rows.Next() {
		var counts int64
		var what string
		if err := rows.Scan(&counts, &what); err != nil {
			t.Fatalf("read a call of the parser the database keeps: %v", err)
		}
		if what == "" {
			read = counts
			continue
		}
		found = append(found, what)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the calls of the parser the database keeps: %v", err)
	}
	if parsers, trees := read/1000000, read%1000000; parsers == 0 || trees == 0 {
		t.Fatalf("read %d parser functions and %d stored expressions: nothing was read", parsers, trees)
	}
	if len(found) > 0 {
		slices.Sort(found)
		t.Fatalf("the database calls the parser where a session's search_path cannot route the call through this row's recorder:\n%s",
			strings.Join(found, "\n"))
	}
}

// sizeText writes a size in bytes as a failure names it.
func sizeText(size int) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size >= 1<<10:
		return fmt.Sprintf("%d KiB", size>>10)
	}
	return fmt.Sprintf("%d B", size)
}

// Ordinary text searches as it did before search_text: a title, a document, a comment, an ask
// with its answer and a message are each found by a word they hold, with the same snippet marks,
// and a word joined to others by underscores is still found by itself.
func TestSearchFindsOrdinaryAndUnderscoredTextAfterSearchText(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedSearchCorpus(t, handler)
	for _, test := range []struct{ query, kind, id string }{
		{"astrolabe", "document", corpus.primaryArtifactID},
		{"sextant", "comment", corpus.commentID},
		{"alidade", "ask", corpus.askID},
		{"compass calibration", "message", corpus.messageID},
		{"navigation instruments", "issue", corpus.issueKey},
	} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(test.query)).Results
		found := false
		for _, result := range results {
			if result.Kind == test.kind && result.ID == test.id {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: no %s %s among %d results", test.query, test.kind, test.id, len(results))
		}
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+corpus.issueKey+"/messages", map[string]any{
		"body": "set LEGION_E2E_MODEL_GATEWAY_URL and the pod_safety_overlay_name_for_each_role_in_the_bundle_of_prompts_the_daemon_snapshots_at_boot_time first",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create underscored message: status=%d body=%s", message.Code, message.Body.String())
	}
	for _, query := range []string{"legion_e2e_model_gateway_url", "gateway", "snapshots", "boot"} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(query)).Results
		found := false
		for _, result := range results {
			if result.Kind == "message" && strings.Contains(result.Snippet, "<mark>") {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: the underscored message was not found with a marked snippet", query)
		}
	}
}

// A document whose markdown makes more elements than one write may - 1 MiB of `)_`, 262,144
// italic spans, held a gigabyte while it was saved (LEGION-481) - is refused as too large to store,
// naming the limit, on a new document (SeedText) and on a replacement (ReplaceText), never a 500.
func TestUploadRefusesADocumentTooLargeToStore(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	body := strings.Repeat(")_", (1<<20)/2)
	// A new document (SeedText): refused, and nothing is created.
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "too-large.md"}, "body.md", "text/markdown", []byte(body), "alice")
	if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
		t.Fatalf("seed of a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 65536-element limit, as the document service words it", response.Code, response.Body.String())
	}
	if listing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice"); strings.Contains(listing.Body.String(), "too-large") {
		t.Fatalf("the refused seed left an artifact behind: %.300s", listing.Body.String())
	}
	// A replacement of a stored document (ReplaceText): refused the same way, and the document keeps
	// its version.
	if first := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "body.md", "text/markdown", []byte("# Notes\n"), "alice"); first.Code != http.StatusCreated {
		t.Fatalf("seed notes.md: status=%d body=%.300s", first.Code, first.Body.String())
	}
	response = multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md", "precondition": encodeUploadBase(t, uploadBaseNamed(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", "notes.md"))}, "body.md", "text/markdown", []byte(body), "alice")
	if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
		t.Fatalf("replacement by a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 65536-element limit, as the document service words it", response.Code, response.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/notes-md/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Notes\n"`) {
		t.Fatalf("notes.md after the refused replacement: status=%d body=%.300s, want its first version", text.Code, text.Body.String())
	}
}

// Every route that writes a caller's markdown refuses markdown past the element limit as 413
// CAP_EXCEEDED naming the limit, and writes nothing: a spec at issue creation, an issue's and a
// project's document upload as JSON, and an edit's insert and replace.
func TestEveryMarkdownWriteRefusesMarkdownPastTheElementLimit(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Element limit", "Before.\n")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "DOCS", "name": "Docs"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	heavy := strings.Repeat(")_", 200_000)
	for _, write := range []struct {
		name, path string
		body       any
	}{
		{"a spec at issue creation", "/api/v1/issues", map[string]string{"project": "TEST", "title": "Too heavy", "spec": heavy}},
		{"an issue's document upload", "/api/v1/issues/" + issue.Key + "/artifacts", map[string]string{"name": "heavy.md", "content": heavy}},
		{"a project's document upload", "/api/v1/projects/DOCS/artifacts", map[string]string{"name": "heavy.md", "content": heavy}},
		{"an edit's insert", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": heavy}},
		}},
		{"an edit's replace", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/edits", map[string]any{
			"ops": []map[string]string{{"op": "replace", "find": "Before.", "with": heavy}},
		}},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, write.path, write.body, "alice")
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), "more than 65536 elements") {
			t.Errorf("%s past the element limit: status=%d body=%.300s, want 413 CAP_EXCEEDED naming the limit", write.name, response.Code, response.Body.String())
		}
	}
	if listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice"); strings.Contains(listed.Body.String(), "Too heavy") {
		t.Errorf("the refused issue was created: %.300s", listed.Body.String())
	}
	for _, path := range []string{"/api/v1/issues/" + issue.Key + "/artifacts", "/api/v1/projects/DOCS/artifacts"} {
		if listed := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice"); strings.Contains(listed.Body.String(), "heavy") {
			t.Errorf("a refused upload left a document behind at %s: %.300s", path, listed.Body.String())
		}
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); text != "Before.\n" {
		t.Errorf("the spec after the refused edits = %q, want it unchanged", text)
	}
}
