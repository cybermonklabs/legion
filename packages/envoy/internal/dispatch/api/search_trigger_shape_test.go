package api

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// sqlStringEnd answers the index of the quote that closes the string literal opening at open in
// src, a doubled quote being one quote of the text, or len(src) when none closes it.
func sqlStringEnd(src string, open int) int {
	for i := open + 1; i < len(src); i++ {
		if src[i] != '\'' {
			continue
		}
		if i+1 < len(src) && src[i+1] == '\'' {
			i++
			continue
		}
		return i
	}
	return len(src)
}

// sqlCallEnd answers the index just past the parenthesis that closes the one at open in src,
// skipping string literals, or -1 when none closes it.
func sqlCallEnd(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '\'':
			i = sqlStringEnd(src, i)
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// sqlCalls answers each call of the function name in src, whole, and its top-level arguments.
func sqlCalls(src, name string) (calls []string, args [][]string) {
	opener := regexp.MustCompile(`(?i)(?:\b[a-z_]+\.)?\b` + name + `\s*\(`)
	for _, at := range opener.FindAllStringIndex(src, -1) {
		end := sqlCallEnd(src, at[1]-1)
		if end < 0 {
			continue
		}
		calls = append(calls, src[at[0]:end])
		var split []string
		inner, depth, start := src[at[1]:end-1], 0, 0
		for i := 0; i < len(inner); i++ {
			switch inner[i] {
			case '\'':
				i = sqlStringEnd(inner, i)
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					split = append(split, strings.TrimSpace(inner[start:i]))
					start = i + 1
				}
			}
		}
		args = append(args, append(split, strings.TrimSpace(inner[start:])))
	}
	return calls, args
}

// withoutCalls answers src with every call of each named function removed whole.
func withoutCalls(src string, names ...string) string {
	for _, name := range names {
		calls, _ := sqlCalls(src, name)
		for _, call := range calls {
			src = strings.ReplaceAll(src, call, "")
		}
	}
	return src
}

var (
	sqlComment = regexp.MustCompile(`--[^\n]*`)
	sqlString  = regexp.MustCompile(`'(?:[^']|'')*'`)
	newRead    = regexp.MustCompile(`(?i)\bnew\b[^;,\n]*`)
	// normalised is an argument that is wholly a call of one of the two normalising functions.
	normalised = regexp.MustCompile(`(?is)^(search_text|search_document_text)\s*\(.*\)$`)
)

// Each search trigger hands Postgres's text-search parser its text only through the shared
// normalising functions, read from the catalog as the database runs them: search_document_text for
// a column that holds ask blocks (a document's markdown, an issue's spec), which removes them and
// then applies search_text, and search_text for any other text. Removing ask blocks after
// search_text joins underscore runs that search_text broke, which the parser reads in quadratic
// time (20261121060636), and a text read raw reaches the parser with every run whole.
//
// For each function a search trigger runs, and the two functions that build a vector from the
// columns a trigger passes them (issue_search_vector, title_lexemes): no direct call of
// to_tsvector; each argument of search_vector is ” or a normalising call; a column that holds ask
// blocks appears only inside search_document_text; and once the normalising calls (and the vector
// functions this test also reads) are removed, no read of the row in any spelling is left - new.x,
// (new).x, row_to_json(new) - other than the vector it writes and a key it looks a row up by. A
// trigger this test does not know fails it, so a new search trigger is read here before it merges.
func TestEverySearchTriggerHandsTheParserOnlyNormalisedText(t *testing.T) {
	database := storetest.Open(t)
	rows, err := database.Pool.Query(context.Background(), `
		select p.proname::text, p.prosrc
		  from pg_trigger t join pg_proc p on p.oid = t.tgfoid
		 where not t.tgisinternal
		   and t.tgrelid in ('issues'::regclass, 'artifact_versions'::regclass, 'comments'::regclass, 'asks'::regclass, 'messages'::regclass)`)
	if err != nil {
		t.Fatalf("read the search triggers: %v", err)
	}
	triggers := map[string]string{}
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			t.Fatalf("read a trigger function: %v", err)
		}
		source = sqlComment.ReplaceAllString(source, "")
		if regexp.MustCompile(`(?i)\b(search_vector|to_tsvector|issue_search_vector|title_lexemes|search_text|search_document_text)\s*\(`).MatchString(source) {
			triggers[name] = source
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the search triggers: %v", err)
	}
	want := []string{"artifact_versions_issues_search", "artifact_versions_search", "asks_search", "comments_search", "issues_search", "messages_search"}
	if got := slices.Sorted(maps.Keys(triggers)); !slices.Equal(got, want) {
		t.Fatalf("the trigger functions that build a search vector are %v, want %v: read a new one here", got, want)
	}

	// The row columns that hold ask blocks, and the parameters each vector function normalises.
	const askColumn = "new.markdown"
	vectorFunctions := map[string]map[string]string{
		"issue_search_vector": {"p_spec": "search_document_text", "p_key": "search_text", "p_title": "search_text"},
		"title_lexemes":       {"title": "search_text"},
	}
	for name := range vectorFunctions {
		var source string
		if err := database.Pool.QueryRow(context.Background(),
			`select prosrc from pg_proc where proname = $1 and pronamespace = 'public'::regnamespace`, name).Scan(&source); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for parameter, wrapper := range vectorFunctions[name] {
			rest := sqlString.ReplaceAllString(withoutCalls(sqlComment.ReplaceAllString(source, ""), wrapper), "''")
			if regexp.MustCompile(`(?i)\b` + parameter + `\b`).MatchString(rest) {
				t.Errorf("%s reads %s outside %s:\n%s", name, parameter, wrapper, source)
			}
		}
	}

	for name, source := range triggers {
		if calls, _ := sqlCalls(source, "to_tsvector"); len(calls) > 0 {
			t.Errorf("%s calls the parser itself (%s): it must build its vector through search_vector or a vector function", name, calls[0])
		}
		_, args := sqlCalls(source, "search_vector")
		for _, arguments := range args {
			for _, argument := range arguments {
				if argument != "''" && !normalised.MatchString(argument) {
					t.Errorf("%s hands search_vector %q, which is neither '' nor search_text(…) or search_document_text(…)", name, argument)
				}
			}
		}
		if strings.Contains(strings.ToLower(withoutCalls(source, "search_document_text")), askColumn) {
			t.Errorf("%s reads %s, which holds ask blocks, outside search_document_text:\n%s", name, askColumn, source)
		}
		rest := sqlString.ReplaceAllString(withoutCalls(source,
			"search_document_text", "search_text", "issue_search_vector", "title_lexemes", "issue_embedding_spec"), "''")
		for _, read := range newRead.FindAllString(rest, -1) {
			read = strings.ToLower(strings.TrimSpace(read))
			if !regexp.MustCompile(`^new\.(search|title_lexemes)\s*:=|^new\.artifact_id\b|^new$`).MatchString(read) {
				t.Errorf("%s reads the row raw (%s), outside the normalising functions:\n%s", name, read, source)
			}
		}
	}
}
