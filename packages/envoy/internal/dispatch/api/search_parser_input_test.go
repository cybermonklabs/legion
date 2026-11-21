package api

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// joinedRun is a run of more underscore-joined segments (letters, digits, dots or hyphens, each
// ended by an underscore) than search_text (migration 0056) leaves: it puts a space after every
// sixteenth.
var joinedRun = regexp.MustCompile(`(?:[A-Za-z0-9.-]+_){17,}`)

// parserCall is one text the parser was handed and the PL/pgSQL call stack that handed it.
type parserCall struct{ body, stack string }

// recordParserInput connects to database with a search_path that puts, ahead of pg_catalog, a
// to_tsvector(regconfig, text) that sends each text it is given, and the PL/pgSQL call stack that
// gave it, as a notice, then answers pg_catalog.to_tsvector's vector. A notice reaches the client
// when it is raised, so a call whose subtransaction rolls back, as search_vector's does for a
// vector too long, is recorded too. Every search function calls to_tsvector by its bare name, so
// on that connection each of them hands the recorder the text it hands the parser; a path that
// does not shows as a caller that recorded no text. take answers the calls since its last call.
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
	for _, statement := range []string{
		`create schema parser_input`,
		`create function parser_input.to_tsvector(config regconfig, body text) returns tsvector
		 language plpgsql volatile as $$
		 declare
		   stack text;
		 begin
		   get diagnostics stack = pg_context;
		   raise notice using message = body, detail = stack, hint = '` + hint + `';
		   return pg_catalog.to_tsvector(config, body);
		 end $$`,
		`set search_path = parser_input, public, pg_catalog`,
	} {
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
// recorder that sees nothing passes nothing. The comment, ask and message triggers remove no ask
// block; their rows hold that a block between two runs keeps them apart there.
func TestSearchHandsTheParserNoRunJoinedAcrossAnAskBlock(t *testing.T) {
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

	// Forty blocks between runs of fifteen segments: joined, one run of 600.
	text := strings.Repeat(strings.Repeat("a_", 15)+":::ask{}:::", 40)
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
		// A statement's own call to search_vector has search_vector outermost.
		{"the related-item picker", relatedTermsSQL, []any{"PARSE", text, "", nil, "Parser input"}, []string{"search_vector"}},
		// The issue's own title, so the issue is a candidate and its spec is compared.
		{"the duplicate check", duplicateEqualLexemesQuery, []any{"PARSE", "Parser input, retitled", text, []string{}, headlineOptions}, []string{"search_vector"}},
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
