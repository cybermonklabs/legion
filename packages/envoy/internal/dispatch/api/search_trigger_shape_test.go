package api

import (
	"context"
	"iter"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

var (
	// dollarTag opens a dollar-quoted string: $$ or $tag$, a tag being an identifier, whose bytes
	// at or above 0x80 count as letters, as Postgres's scanner counts them (dolq_start, dolq_cont);
	// $1 is a parameter, not a quote.
	dollarTag = regexp.MustCompile(`^\$(?:(?:[A-Za-z_]|[^\x00-\x7F])(?:[A-Za-z_0-9]|[^\x00-\x7F])*)?\$`)
	// plainIdentifier is a quoted identifier's text that names what the bare word would.
	plainIdentifier = regexp.MustCompile(`^(?:[A-Za-z_]|[^\x00-\x7F])(?:[A-Za-z_0-9$]|[^\x00-\x7F])*$`)
)

// identifierByte reports whether b can continue an identifier, a byte at or above 0x80 among
// them as Postgres's scanner reads it (ident_cont), so that a name, an E before a quote or a $
// before a tag or a parameter's digits is read as part of the identifier it touches.
func identifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 || (b >= '0' && b <= '9') || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// standalone reports whether code[start:end] is a whole token: no identifier byte touches it on
// the left, or on the right when the match ends in an identifier byte.
func standalone(code string, start, end int) bool {
	if start > 0 && identifierByte(code[start-1]) {
		return false
	}
	return end >= len(code) || !identifierByte(code[end-1]) || !identifierByte(code[end])
}

// sqlSpanKind is what one span of a SQL text is to Postgres's lexer (sqlSpans).
type sqlSpanKind int

const (
	// sqlCodeSpan is text the lexer reads as tokens: names, numbers, operators, parameters and the
	// white space between them.
	sqlCodeSpan sqlSpanKind = iota
	// sqlLineComment runs from `--` to the line's end, its newline left to the code after it.
	sqlLineComment
	// sqlBlockComment is `/* */`, nested.
	sqlBlockComment
	// sqlString is a string literal: an ordinary one, whose doubled quote is one quote, an
	// E-prefixed one, its E included, whose backslash escapes, and a dollar-quoted one of any tag.
	sqlString
	// sqlQuotedIdentifier is a name in double quotes, whose doubled quote is one quote.
	sqlQuotedIdentifier
)

// sqlSpan is one span of a SQL text: a run of code, or one comment, string literal or quoted
// identifier whole, with its quotes or delimiters. One the text does not close runs to its end.
type sqlSpan struct {
	kind sqlSpanKind
	text string
}

// sqlSpans yields src as Postgres's lexer reads it, in order: each comment, string literal and
// quoted identifier whole, and each run of code between them. The spans' texts joined are src.
// maskSQL masks every span but code, and searchSplitWrittenIn writes values in for the parameters
// of code alone, so a quote inside a comment or a dollar inside a literal hides or names nothing.
func sqlSpans(src string) iter.Seq[sqlSpan] {
	return func(yield func(sqlSpan) bool) {
		codeFrom := 0 // where the run of code being read begins
		for i := 0; i < len(src); {
			start, end := i, i
			var kind sqlSpanKind
			switch rest := src[i:]; {
			case strings.HasPrefix(rest, "--"):
				kind, end = sqlLineComment, len(src)
				if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
					end = i + newline
				}
			case strings.HasPrefix(rest, "/*"):
				kind = sqlBlockComment
				for depth := 0; end < len(src); {
					switch {
					case strings.HasPrefix(src[end:], "/*"):
						depth, end = depth+1, end+2
					case strings.HasPrefix(src[end:], "*/"):
						depth, end = depth-1, end+2
					default:
						end++
					}
					if depth == 0 {
						break
					}
				}
			case src[i] == '\'':
				// An E that no identifier byte precedes opens an escape string with the quote.
				escapes := i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i == 1 || !identifierByte(src[i-2]))
				if escapes {
					start = i - 1
				}
				kind, end = sqlString, spanQuotedEnd(src, i, escapes)
			case src[i] == '"':
				kind, end = sqlQuotedIdentifier, spanQuotedEnd(src, i, false)
			case src[i] == '$' && (i == 0 || !identifierByte(src[i-1])) && dollarTag.MatchString(rest):
				tag := dollarTag.FindString(rest)
				kind, end = sqlString, len(src)
				if close := strings.Index(rest[len(tag):], tag); close >= 0 {
					end = i + len(tag) + close + len(tag)
				}
			default:
				i++
				continue
			}
			if start > codeFrom && !yield(sqlSpan{sqlCodeSpan, src[codeFrom:start]}) {
				return
			}
			if !yield(sqlSpan{kind, src[start:end]}) {
				return
			}
			i, codeFrom = end, end
		}
		if codeFrom < len(src) {
			yield(sqlSpan{sqlCodeSpan, src[codeFrom:]})
		}
	}
}

// spanQuotedEnd answers the index just past the quote that closes the one at open in src, or
// len(src) when none closes it. A doubled quote is one quote of the text, and where escapes a
// backslash takes the byte after it.
func spanQuotedEnd(src string, open int, escapes bool) int {
	quote := src[open]
	for i := open + 1; i < len(src); i++ {
		switch {
		case escapes && src[i] == '\\':
			i++
		case src[i] == quote && i+1 < len(src) && src[i+1] == quote:
			i++
		case src[i] == quote:
			return i + 1
		}
	}
	return len(src)
}

// maskSQL answers src as Postgres's lexer reads it (sqlSpans), with nothing but code left: a line
// comment goes, its newline left, and a block comment becomes a space; each string literal
// becomes an empty literal; and a quoted identifier becomes its name where that name is a plain
// identifier, so "to_tsvector" reads as to_tsvector. Every check below reads the masked text, so
// a quote inside a comment or a dollar-quoted string hides no code, and text inside a literal is
// no call.
func maskSQL(src string) string {
	var code strings.Builder
	for span := range sqlSpans(src) {
		switch span.kind {
		case sqlCodeSpan:
			code.WriteString(span.text)
		case sqlLineComment:
		case sqlBlockComment:
			code.WriteByte(' ')
		case sqlString:
			code.WriteString("''")
		case sqlQuotedIdentifier:
			if name := strings.ReplaceAll(strings.TrimSuffix(span.text[1:], `"`), `""`, `"`); plainIdentifier.MatchString(name) {
				code.WriteString(name)
			} else {
				code.WriteString("quoted_identifier")
			}
		}
	}
	return code.String()
}

// sqlCallEnd answers the index just past the parenthesis that closes the one at open in code,
// which maskSQL has left with no literal or comment, or -1 when none closes it.
func sqlCallEnd(code string, open int) int {
	depth := 0
	for i := open; i < len(code); i++ {
		switch code[i] {
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

// sqlCalls answers each call of the function name in code, whole, and its top-level arguments.
func sqlCalls(code, name string) (calls []string, args [][]string) {
	opener := regexp.MustCompile(`(?i)` + name + `\s*\(`)
	for _, at := range opener.FindAllStringIndex(code, -1) {
		if !standalone(code, at[0], at[0]+len(name)) {
			continue
		}
		end := sqlCallEnd(code, at[1]-1)
		if end < 0 {
			continue
		}
		calls = append(calls, code[at[0]:end])
		var split []string
		inner, depth, start := code[at[1]:end-1], 0, 0
		for i, b := range []byte(inner) {
			switch b {
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

// withoutCalls answers code with every call of each named function removed whole.
func withoutCalls(code string, names ...string) string {
	for _, name := range names {
		calls, _ := sqlCalls(code, name)
		for _, call := range calls {
			code = strings.ReplaceAll(code, call, "")
		}
	}
	return code
}

// normalisedCall reports whether argument is wholly one call of search_text or
// search_document_text: the call's own parenthesis closes it.
func normalisedCall(argument string) bool {
	opener := regexp.MustCompile(`(?i)^(search_text|search_document_text)\s*\(`).FindStringIndex(argument)
	return opener != nil && sqlCallEnd(argument, opener[1]-1) == len(argument)
}

var (
	// assignTarget is a trigger's write of the vector or the title lexemes, and returnNew its
	// return of the row: neither reads a column. rowRead is any other mention of the row.
	assignTarget = regexp.MustCompile(`(?i)\bnew\s*\.\s*(search|title_lexemes)\s*:=`)
	returnNew    = regexp.MustCompile(`(?i)\breturn\s+new\b`)
	rowRead      = regexp.MustCompile(`(?i)new(\s*\.\s*\w+)?`)
	// callName is any call in masked code, an identifier holding letters, digits, _, $ and any
	// character past ASCII, and subquery a select in it.
	callName = regexp.MustCompile(`(?i)((?:[a-z_]|[^\x00-\x7F])(?:[a-z_0-9$]|[^\x00-\x7F])*)\s*\(`)
	subquery = regexp.MustCompile(`(?i)\bselect\b`)
	// markdownWord is any mention of a markdown column, and markdownNullTest a null test of one
	// through a table alias - v.markdown is null, v.markdown is not null - which hands the parser
	// nothing: it tells a version with text from an image, as the triggers' own `when` does.
	markdownWord     = regexp.MustCompile(`(?i)\bmarkdown\b`)
	markdownNullTest = regexp.MustCompile(`(?i)\b[a-z_][a-z_0-9]*\s*\.\s*markdown\s+is\s+(not\s+)?null\b`)
)

// argumentCalls are the only calls a trigger may make inside the argument of a normaliser or a
// vector function: none of them parses text.
var argumentCalls = []string{"coalesce", "ask_search_suffix", "issue_embedding_spec"}

// rowColumnsRead are the columns of `new` a search trigger may read outside the normalising
// functions: the vector it writes, the artifact_id it looks a row up by, and the number it
// compares versions by. None is text the parser reads.
var rowColumnsRead = []string{"new.search", "new.artifact_id", "new.number"}

// markdownOutsideNormaliser reports whether code, as maskSQL leaves it, reads a markdown column
// other than inside search_document_text or in a null test through an alias (markdownNullTest).
func markdownOutsideNormaliser(code string) bool {
	return markdownWord.MatchString(markdownNullTest.ReplaceAllString(withoutCalls(code, "search_document_text"), " "))
}

// rawRowReads answers each read of the row in code, as maskSQL leaves it, that is not inside a
// normalising function, a write of the vector, the row's return, or a column of rowColumnsRead.
func rawRowReads(code string) []string {
	rest := withoutCalls(code, "search_document_text", "search_text", "issue_search_vector", "title_lexemes", "issue_embedding_spec")
	rest = returnNew.ReplaceAllString(assignTarget.ReplaceAllString(rest, "assigned :="), "return")
	var reads []string
	for _, at := range rowRead.FindAllStringIndex(rest, -1) {
		if !standalone(rest, at[0], at[0]+len("new")) {
			continue
		}
		read := strings.ToLower(strings.Join(strings.Fields(rest[at[0]:at[1]]), ""))
		if !slices.Contains(rowColumnsRead, read) {
			reads = append(reads, rest[at[0]:min(len(rest), at[1]+40)])
		}
	}
	return reads
}

// callsIn answers the name of each call in code that stands as a whole token, lower case.
func callsIn(code string) []string {
	var names []string
	for _, at := range callName.FindAllStringSubmatchIndex(code, -1) {
		if standalone(code, at[2], at[3]) {
			names = append(names, strings.ToLower(code[at[2]:at[3]]))
		}
	}
	return names
}

// Each search trigger hands Postgres's text-search parser its text only through the shared
// normalising functions, read from the catalog as the database runs them: search_document_text for
// a column that holds ask blocks (a document's markdown, an issue's spec), which removes them and
// then applies search_text, and search_text for any other text. Removing ask blocks after
// search_text joins underscore runs that search_text broke, which the parser reads in quadratic
// time (20261121060636), and a text read raw reaches the parser with every run whole.
//
// The parser's entry points are read from pg_proc (parserEntries), the list the parser-input test
// shadows: to_tsvector and its json forms, the tsquery builders, ts_parse, ts_debug, ts_headline
// and the rest of pg_catalog's text-search functions that take text. The test reads the source
// of every trigger function in the database that calls one of them, a normaliser or a vector
// function, as maskSQL leaves it: code alone, comments of either kind and string literals of
// every kind taken out. The set must be the six it knows, so a new one is read here before it
// merges. In each: no call of an entry point, quoted or qualified; each argument of search_vector
// a literal or wholly one normalising call; inside the argument of each normaliser and vector
// function no subquery and no call but coalesce, ask_search_suffix and issue_embedding_spec;
// markdown only inside search_document_text, or in a null test through an alias, which reads no
// text (markdownOutsideNormaliser); and once those calls are removed, no read through `new` -
// new.x, (new).x, row_to_json(new) - but the vector it writes, the artifact_id it looks a row up by
// and the number it compares versions by (rawRowReads). It reads search_document_text's own body
// too, which must be one search_text call, and each parameter of the two vector functions those
// triggers call, which must appear only inside its normaliser. A read of the row through another
// name, such as a variable assigned from new, is beyond it, and so is text @@ query, an operator;
// a raw read of new for either still fails here. TestTheSearchTriggerShapeReadsTheRowsItAllows
// holds the two allowances to exactly that.
func TestEverySearchTriggerHandsTheParserOnlyNormalisedText(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	entries := map[string]bool{}
	for _, entry := range parserEntries(t, ctx, database) {
		entries[entry.name] = true
	}
	normalisers := []string{"search_vector", "issue_search_vector", "title_lexemes", "search_text", "search_document_text"}
	rows, err := database.Pool.Query(ctx, `
		select distinct p.proname::text, p.prosrc
		  from pg_trigger t join pg_proc p on p.oid = t.tgfoid
		 where not t.tgisinternal`)
	if err != nil {
		t.Fatalf("read the trigger functions: %v", err)
	}
	triggers := map[string]string{}
	read := 0
	for rows.Next() {
		var name, source string
		if err := rows.Scan(&name, &source); err != nil {
			t.Fatalf("read a trigger function: %v", err)
		}
		read++
		code := maskSQL(source)
		for _, call := range callsIn(code) {
			if entries[call] || slices.Contains(normalisers, call) {
				triggers[name] = code
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the trigger functions: %v", err)
	}
	want := []string{"artifact_versions_issues_search", "artifact_versions_search", "asks_search", "comments_search", "issues_search", "messages_search"}
	if got := slices.Sorted(maps.Keys(triggers)); !slices.Equal(got, want) {
		t.Fatalf("of the %d trigger functions in the database, those that reach the parser are %v, want %v: read a new one here", read, got, want)
	}

	source := func(name string) string {
		var source string
		if err := database.Pool.QueryRow(ctx,
			`select prosrc from pg_proc where proname = $1 and pronamespace = 'public'::regnamespace`, name).Scan(&source); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return maskSQL(source)
	}
	body := strings.TrimSpace(source("search_document_text"))
	if selected, ok := strings.CutPrefix(strings.ToLower(body), "select"); !ok || !normalisedCall(strings.TrimSpace(selected)) ||
		!strings.HasPrefix(strings.TrimSpace(selected), "search_text") {
		t.Errorf("search_document_text answers %q, which is not one search_text(…) call: a change made after search_text can join its runs again", body)
	}
	vectorFunctions := map[string]map[string]string{
		"issue_search_vector": {"p_spec": "search_document_text", "p_key": "search_text", "p_title": "search_text"},
		"title_lexemes":       {"title": "search_text"},
	}
	for name, parameters := range vectorFunctions {
		text := source(name)
		for parameter, wrapper := range parameters {
			rest := withoutCalls(text, wrapper)
			if regexp.MustCompile(`(?i)\b` + parameter + `\b`).MatchString(rest) {
				t.Errorf("%s reads %s outside %s:\n%s", name, parameter, wrapper, text)
			}
		}
	}
	for name, code := range triggers {
		for _, call := range callsIn(code) {
			if entries[call] {
				t.Errorf("%s calls the parser itself (%s): it must build its vector through search_vector or a vector function", name, call)
			}
		}
		for _, wrapper := range []string{"search_text", "search_document_text", "issue_search_vector", "title_lexemes"} {
			_, args := sqlCalls(code, wrapper)
			for _, arguments := range args {
				argument := strings.Join(arguments, ", ")
				if subquery.MatchString(argument) {
					t.Errorf("%s hands %s a subquery (%s): its argument may only read the row", name, wrapper, argument)
				}
				for _, call := range callsIn(argument) {
					if !slices.Contains(argumentCalls, call) {
						t.Errorf("%s calls %s inside %s's argument (%s): only %v may stand there", name, call, wrapper, argument, argumentCalls)
					}
				}
			}
		}
		_, args := sqlCalls(code, "search_vector")
		for _, arguments := range args {
			for _, argument := range arguments {
				if argument != "''" && !normalisedCall(argument) {
					t.Errorf("%s hands search_vector %q, which is neither a literal nor one call of search_text or search_document_text", name, argument)
				}
			}
		}
		if markdownOutsideNormaliser(code) {
			t.Errorf("%s reads markdown, which holds ask blocks, outside search_document_text:\n%s", name, code)
		}
		for _, read := range rawRowReads(code) {
			t.Errorf("%s reads the row raw (%s), outside the normalising functions:\n%s", name, read, code)
		}
	}
}

// The two allowances of TestEverySearchTriggerHandsTheParserOnlyNormalisedText's rules are a
// markdown column's null test through an alias, and new.number. Neither lets the parser see raw
// text: markdown handed to anything but search_document_text, a bare markdown, and a read of any
// other column of new still fail.
func TestTheSearchTriggerShapeReadsTheRowsItAllows(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		code            string
		markdown, reads bool
	}{
		{"a null test of an alias's markdown", "if not exists (select 1 from artifact_versions v where v.markdown is not null) then", false, false},
		{"an is-null test of an alias's markdown", "where v . markdown is null", false, false},
		{"markdown inside search_document_text", "search_vector('', search_document_text(new.markdown))", false, false},
		{"a comparison of new.number", "where v.artifact_id = new.artifact_id and v.number > new.number", false, false},
		{"markdown handed to to_tsvector", "to_tsvector('english', v.markdown)", true, false},
		{"markdown handed to search_text", "search_vector('', search_text(v.markdown))", true, false},
		{"markdown compared, not null-tested", "where v.markdown is not distinct from ''", true, false},
		{"a bare markdown", "select markdown from artifact_versions", true, false},
		{"new.markdown outside a normaliser", "if new.markdown is not null then", false, true},
		{"another column of new", "where v.created_at > new.created_at", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			code := maskSQL(test.code)
			if got := markdownOutsideNormaliser(code); got != test.markdown {
				t.Errorf("markdownOutsideNormaliser(%q) = %v, want %v", test.code, got, test.markdown)
			}
			if got := len(rawRowReads(code)) > 0; got != test.reads {
				t.Errorf("rawRowReads(%q) = %v, want a read refused: %v", test.code, rawRowReads(code), test.reads)
			}
		})
	}
}
