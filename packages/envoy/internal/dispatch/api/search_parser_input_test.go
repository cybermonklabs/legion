package api

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// joinedRun is a run of more underscore-joined segments (letters, digits, dots or hyphens, each
// ended by an underscore) than search_text (migration 0056) leaves: it puts a space after every
// sixteenth.
var joinedRun = regexp.MustCompile(`(?:[A-Za-z0-9.-]+_){17,}`)

// parserCall is one text the parser was handed and the PL/pgSQL call stack that handed it.
type parserCall struct{ body, stack string }

// parserEntry is one form of one of pg_catalog's text-search functions that takes text, json or
// jsonb: its name, its input types in order, each OUT parameter as `name type`, what it answers,
// whether it answers a set, and whether it is strict.
type parserEntry struct {
	name, returns  string
	inputs, outs   []string
	retset, strict bool
}

// parserEntryNames are the entry points of the text-search parser that parserEntries must find:
// a read that misses one of them has read the wrong catalog.
var parserEntryNames = []string{"to_tsvector", "json_to_tsvector", "jsonb_to_tsvector", "to_tsquery", "plainto_tsquery",
	"phraseto_tsquery", "websearch_to_tsquery", "ts_parse", "ts_debug", "ts_headline"}

// parserEntries reads from pg_proc every form of every function of pg_catalog that takes text,
// json or jsonb and answers a tsvector or a tsquery, or whose name begins ts_: every way a caller
// hands text to the parser by a function's name, and a few text-search functions that do not
// parse it (ts_delete, ts_lexize, ts_rewrite, ts_stat, ts_token_type), which no search path calls.
// It is the one list the parser-input recorder shadows and the shape test refuses calls of.
func parserEntries(t *testing.T, ctx context.Context, database *store.Store) []parserEntry {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select p.proname::text, format_type(p.prorettype, null), p.proretset, p.proisstrict,
		       array(select format_type(a.t, null) from unnest(p.proargtypes::oid[]) with ordinality as a(t, n) order by a.n),
		       coalesce(array(select quote_ident(p.proargnames[a.n]) || ' ' || format_type(a.t, null)
		                        from unnest(p.proallargtypes, p.proargmodes) with ordinality as a(t, m, n)
		                       where a.m = 'o' order by a.n), '{}')
		  from pg_proc p
		 where p.pronamespace = 'pg_catalog'::regnamespace
		   and exists (select 1 from unnest(p.proargtypes::oid[]) a(t) where a.t in ('text'::regtype, 'json'::regtype, 'jsonb'::regtype))
		   and (p.prorettype in ('tsvector'::regtype, 'tsquery'::regtype) or p.proname ~ '^ts_')
		 order by 1`)
	if err != nil {
		t.Fatalf("read the parser's entry points: %v", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (parserEntry, error) {
		var entry parserEntry
		return entry, row.Scan(&entry.name, &entry.returns, &entry.retset, &entry.strict, &entry.inputs, &entry.outs)
	})
	if err != nil {
		t.Fatalf("read the parser's entry points: %v", err)
	}
	for _, name := range parserEntryNames {
		if !slices.ContainsFunc(entries, func(entry parserEntry) bool { return entry.name == name }) {
			t.Fatalf("pg_proc answered %d entry points of the parser and no %s: the read cannot see the parser", len(entries), name)
		}
	}
	return entries
}

// recordParserInput connects to database with a search_path that puts, ahead of pg_catalog, a
// function of the same signature for each of parserEntries. Each sends every text, json or jsonb
// it is given, and the PL/pgSQL call stack that gave it, as a notice, then answers what
// pg_catalog's own answers. A notice reaches the client when it is raised, so a call whose
// subtransaction rolls back, as search_vector's does for a vector too long, is recorded too. A
// call by a bare name resolves through the search_path, so on that connection every path that
// hands text to the parser by a bare name hands it to the recorder first; a call qualified with
// pg_catalog, and text @@ query, an operator, pass it by, which the shape test refuses. take
// answers the calls since its last call.
func recordParserInput(t *testing.T, ctx context.Context, database *store.Store) (conn *pgx.Conn, take func() []parserCall) {
	t.Helper()
	const hint = "parser input"
	var calls []parserCall
	config := database.Pool.Config().ConnConfig.Copy()
	config.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		if notice.Hint == hint {
			calls = append(calls, parserCall{body: notice.Message, stack: notice.Detail})
		}
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	statements := []string{`create schema parser_input`}
	for _, entry := range parserEntries(t, ctx, database) {
		var params, args, records []string
		for i, input := range entry.inputs {
			arg := fmt.Sprintf("a%d", i+1)
			params, args = append(params, arg+" "+input), append(args, arg)
			if input == "text" || input == "json" || input == "jsonb" {
				records = append(records, fmt.Sprintf("raise notice using message = %s::text, detail = parser_input_stack, hint = '%s';", arg, hint))
			}
		}
		for _, out := range entry.outs {
			params = append(params, "out "+out)
		}
		returns, answer := entry.returns, fmt.Sprintf("return pg_catalog.%s(%s);", entry.name, strings.Join(args, ", "))
		if entry.retset {
			answer = fmt.Sprintf("return query select * from pg_catalog.%s(%s);", entry.name, strings.Join(args, ", "))
			returns = "setof " + returns
		}
		strict := ""
		if entry.strict {
			strict = " strict"
		}
		statements = append(statements, fmt.Sprintf(`create function parser_input.%s(%s) returns %s language plpgsql volatile%s as $$
		 declare
		   parser_input_stack text;
		 begin
		   get diagnostics parser_input_stack = pg_context;
		   %s
		   %s
		 end $$`, entry.name, strings.Join(params, ", "), returns, strict, strings.Join(records, "\n\t\t   "), answer))
	}
	statements = append(statements, `set search_path = parser_input, public, pg_catalog`)
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			t.Fatalf("record the parser's input: %s: %v", statement, err)
		}
	}
	return conn, func() []parserCall {
		taken := calls
		calls = nil
		return taken
	}
}

// stackFunction reads the function a line of a PL/pgSQL call stack names.
var stackFunction = regexp.MustCompile(`function ([a-z_]+)\(`)

// callerFunctions names the PL/pgSQL functions of a recorded call stack below the recorder,
// innermost first: search_vector, artifact_versions_search.
func callerFunctions(stack string) []string {
	var names []string
	for _, line := range strings.Split(stack, "\n")[1:] {
		if match := stackFunction.FindStringSubmatch(line); match != nil {
			names = append(names, match[1])
		}
	}
	return names
}

// No search path hands Postgres's text-search parser a run of more than sixteen underscore-joined
// segments from a text of ask blocks written inside underscore runs. The parser reads such a run
// again from each letter in it, so its time grows with the square of the run, and search_text
// breaks every run after its sixteenth segment. The document and issue vectors, the related-item
// picker and the duplicate check removed the ask blocks after search_text, which joined the runs
// on each side of every block into one that nothing broke (migration 20261121060636 has the
// times). Each removes them first now, through search_document_text.
//
// The test reads the text where the parser receives it (recordParserInput), not search_text's
// answer, so a path that applies search_text and then joins its runs fails here, named by the
// function that called the parser. Each row names the functions that must hand the parser a text
// of its shape, outermost first in the call stack, and fails when one of them handed none: a
// recorder that sees nothing passes nothing. Every row's text also holds a plain run of 600
// segments, so a path that hands the parser its text raw fails too, the comment, ask and message
// triggers among them, which remove no ask block.
func TestSearchHandsTheParserNoRunJoinedAcrossAnAskBlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	const actor = `{"kind":"user","id":"alice"}`
	var specID, notesID string
	if err := database.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name, default_assignee) values ('PARSE', 'Parse', 'alice') returning key),
		i as (insert into issues (key, project_key, number, title, created_by, rank)
		      select 'PARSE-1', key, 1, 'Parser input', '`+actor+`', 'A' from p returning key),
		a as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		      select key, 'PARSE', slug, slug || '.md', 'doc', slug = 'spec', '`+actor+`'
		        from i cross join (values ('spec'), ('notes')) s(slug) returning slug, id)
		select (select id::text from a where slug = 'spec'), (select id::text from a where slug = 'notes')
	`).Scan(&specID, &notesID); err != nil {
		t.Fatalf("seed the project, the issue and its documents: %v", err)
	}

	conn, take := recordParserInput(t, ctx, database)

	// Forty blocks between runs of fifteen segments, which a removal after search_text joins into
	// one run of 600; then a plain run of 600, which reaches the parser whole if a path reads the
	// text raw, though it removes no ask block.
	text := strings.Repeat(strings.Repeat("a_", 15)+":::ask{}:::", 40) + "\n\n" + strings.Repeat("a_", 600)
	const insertVersion = `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, $2, '[]')`
	for _, row := range []struct {
		name      string
		statement string
		args      []any
		callers   []string
	}{
		{"a document version", insertVersion, []any{notesID, text}, []string{"artifact_versions_search"}},
		{"a version of an issue's spec", insertVersion, []any{specID, text}, []string{"artifact_versions_search", "artifact_versions_issues_search"}},
		{"a retitle of an issue with that spec", `update issues set title = $1 where key = 'PARSE-1'`, []any{"Parser input, retitled"}, []string{"issues_search"}},
		{"a comment", `insert into comments (issue_key, author, body) values ('PARSE-1', '` + actor + `', $1)`, []any{text}, []string{"comments_search"}},
		{"an ask", `insert into asks (issue_key, author, question) values ('PARSE-1', '` + actor + `', $1)`, []any{text}, []string{"asks_search"}},
		{"a message", `insert into messages (issue_key, author, body) values ('PARSE-1', '{"kind":"session","id":"s1"}', $1)`, []any{text}, []string{"messages_search"}},
		// A statement's own call to search_vector has search_vector outermost. Both statements
		// take the reader's viewer last; the server's own reader sees every project of the seed.
		{"the related-item picker", relatedTermsSQL, []any{"PARSE", text, "", nil, "Parser input", relatedViewer(everyReader)}, []string{"search_vector"}},
		// The issue's own title, so the issue is a candidate and its spec is compared.
		{"the duplicate check", duplicateEqualLexemesQuery, []any{"PARSE", "Parser input, retitled", text, []string{}, headlineOptions, refs.VisibilityArg(everyReader)}, []string{"search_vector"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			take()
			if _, err := conn.Exec(ctx, row.statement, row.args...); err != nil {
				t.Fatalf("run it: %v", err)
			}
			recorded := take()
			handed := map[string]bool{}
			for _, call := range recorded {
				if !strings.Contains(call.body, "a_a_") {
					continue
				}
				callers := callerFunctions(call.stack)
				if len(callers) > 0 {
					handed[callers[len(callers)-1]] = true
				}
				if run := joinedRun.FindString(call.body); run != "" {
					slices.Reverse(callers)
					t.Errorf("%s handed the parser a run of %d underscore-joined segments, more than search_text's 16",
						strings.Join(callers, " > "), strings.Count(run, "_"))
				}
			}
			for _, caller := range row.callers {
				if !handed[caller] {
					t.Errorf("%s handed the parser no text of the row's shape in the %d calls recorded: the record cannot see that path", caller, len(recorded))
				}
			}
		})
	}
}
