//go:build !race

// A tenth static check beside the nine source guards, under the tag they carry and for the same
// saving: it reads text - the api package's parsed declarations, the bytes of every non-test .go
// file of the module and the SQL of every .sql file of it - and opens no database and starts no
// goroutine, so the race detector has nothing here to watch. Behind the tag the race build neither
// compiles it nor walks packages/envoy a second time to run it (lock_level_test.go's header weighs
// that walk).
//
// The tag is also what puts this check in the job its subject asks for. R15 of
// dispatch://INFRA-1066's spec has a test whose subject is a file's words run in a text-check job
// with a filter of its own, never inside a product suite; this one holds one SQL rule to one
// spelling in the module's source, which is words. It arrived from main inside who_waits_test.go,
// a behaviour file of this package's Postgres-backed tests, as a subtest of
// TestOneExpressionDecidesWhoWaits - the one place a module-wide walk ran in the race build. Its
// job there was envoy-go-race-api, whose filter does not name packages/envoy/** and must not: an
// edit under packages/envoy/cmd or packages/envoy/infra reaches what this check reads while
// selecting no job that ran it, so the red landed on whatever pull request next touched that
// job's own closure, or on none. Here it is the text-check job's to run by name
// (ci_job_reads.py's TEXT_CHECK_TESTS, which also keeps it out of envoy-go's plain step), once
// per run of the router, under a filter that names the whole module - the read
// this file's //ci:reads lines below declare.
//
// What it holds is one of the two tests the Go half of dispatch://INFRA-1576's
// c-one-expression-decides is written as: TestOneExpressionDecidesWhoWaitsInOneSpelling here,
// which holds that no read keeps a copy of the rule for which person an ask waits on, and
// TestOneExpressionDecidesWhoWaits in who_waits_test.go, which reads every answer for the same
// ask and the same person. That check's own `run` names the shorter name and anchors
// nothing, so one `-run TestOneExpressionDecidesWhoWaits` matches both tests; that is why the
// test here is spelled as a longer form of that name. The `!race` tag above is what puts the two
// in different jobs: the race build does not compile this file, so the text-check job runs this
// test by name while the behaviour test runs with the package's Postgres-backed tests.

package api

// main's #3429 put this check in who_waits_test.go, a behaviour file of this package, where
// the race job was the only one that ran it and the module walk under it was declared by
// nobody. It is a file of its own under `//go:build !race` now, so the text-check job runs it
// and the lines below are the declarations that hold that job's filter to what it reads.
//ci:reads packages/envoy/**/*.go !**/*_test.go `packages/envoy/internal/dispatch/api/module_source_test.go#scanModuleSources` walks packages/envoy (infra, testdata and all) and parses every .go file it finds but the test files, for the tests that ask it for the module's sources: `packages/envoy/internal/dispatch/api/lock_level_test.go#TestNoForUpdateOnOwnerTables`, `packages/envoy/internal/dispatch/api/event_insert_guard_test.go#TestOnlyTheBrokerInsertsEvents`, `packages/envoy/internal/dispatch/api/review_order_guard_test.go#TestEveryReviewReadOrdersByReviewOrder`, `packages/envoy/internal/dispatch/api/review_write_guard_test.go#TestOnlyQueueReviewWritesReviews`, `packages/envoy/internal/dispatch/api/one_expression_guard_test.go#TestOneExpressionDecidesWhoWaitsInOneSpelling`, `packages/envoy/internal/dispatch/api/issue_route_writer_guard_test.go#TestOnlyTheIssuePatchWritesTheRoute`; that last also walks packages/envoy and reads every non-test file of it as text
//ci:reads packages/envoy/**/*.sql `packages/envoy/internal/dispatch/api/one_expression_guard_test.go#sqlFilesSpellingTheRule`, which `packages/envoy/internal/dispatch/api/one_expression_guard_test.go#TestOneExpressionDecidesWhoWaitsInOneSpelling` calls, walks packages/envoy and reads every .sql file it finds, its comments blanked, for a copy of the rule

import (
	"fmt"
	"go/ast"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// askedOfPersonExpressionName is the identifier every read must name instead of spelling the
// rule again.
const askedOfPersonExpressionName = "askedOfPersonExpression"

// namesTheExpression reports whether a declaration or a function body mentions that identifier.
func namesTheExpression(node ast.Node) bool {
	named := false
	ast.Inspect(node, func(inner ast.Node) bool {
		if identifier, ok := inner.(*ast.Ident); ok && identifier.Name == askedOfPersonExpressionName {
			named = true
		}
		return !named
	})
	return named
}

// sqlCode is a SQL text with its comments blanked, read by PostgreSQL's lexical rules, so that no
// text a statement runs is blanked with them:
//   - a line comment (`--`) ends at its line's end: a newline or a carriage return (scan.l's
//     non_newline);
//   - a quoted string ('…') and a quoted identifier ("…"), in each of which a doubled quote stands
//     for one, are kept whole, so a `--` or a `/*` in one starts no comment; in an escape string (E'…')
//     a backslash escapes the character after it, a quote among them;
//   - a block comment nests, and ends at the `*/` that closes its outermost `/*`;
//   - a dollar-quoted string ($$…$$ or $tag$…$tag$) has no escapes and ends at its first closing
//     tag. It is the body of a function or of a DO block, which PostgreSQL runs as SQL or as
//     PL/pgSQL, so it is read as SQL of its own: its comments are blanked and each ends at the
//     body's end at the latest, and its code and its strings are kept.
//
// Each comment becomes one space, so that two tokens it stood between stay apart.
func sqlCode(text string) string {
	var code strings.Builder
	code.Grow(len(text))
	for at := 0; at < len(text); {
		rest := text[at:]
		switch {
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexAny(rest, "\n\r")
			if end < 0 {
				end = len(rest)
			}
			code.WriteByte(' ')
			at += end
		case strings.HasPrefix(rest, "/*"):
			depth, end := 1, 2
			for end < len(rest) && depth > 0 {
				switch {
				case strings.HasPrefix(rest[end:], "/*"):
					depth, end = depth+1, end+2
				case strings.HasPrefix(rest[end:], "*/"):
					depth, end = depth-1, end+2
				default:
					end++
				}
			}
			code.WriteByte(' ')
			at += end
		case rest[0] == '\'' || rest[0] == '"':
			escapes := rest[0] == '\'' && at > 0 && (text[at-1] == 'E' || text[at-1] == 'e') &&
				(at < 2 || !sqlIdentifierByte(text[at-2]))
			end := quotedEnd(rest, escapes)
			code.WriteString(rest[:end])
			at += end
		case rest[0] == '$' && (at == 0 || !sqlIdentifierByte(text[at-1])) && sqlDollarTag(rest) != "":
			tag := sqlDollarTag(rest)
			body := rest[len(tag):]
			end := strings.Index(body, tag)
			closed := end >= 0
			if !closed {
				end = len(body)
			}
			code.WriteString(tag)
			code.WriteString(sqlCode(body[:end]))
			at += len(tag) + end
			if closed {
				code.WriteString(tag)
				at += len(tag)
			}
		default:
			code.WriteByte(rest[0])
			at++
		}
	}
	return code.String()
}

// quotedEnd is the length of the quoted string or identifier rest starts with, its closing quote
// included, or all of rest when it never closes. A doubled quote is a quote in it; with escapes, a
// backslash escapes the byte after it.
func quotedEnd(rest string, escapes bool) int {
	quote := rest[0]
	for end := 1; end < len(rest); end++ {
		switch {
		case escapes && rest[end] == '\\':
			end++
		case rest[end] == quote && end+1 < len(rest) && rest[end+1] == quote:
			end++
		case rest[end] == quote:
			return end + 1
		}
	}
	return len(rest)
}

// sqlDollarTag is the opening tag of the dollar-quoted string rest starts with ($$ or $tag$, a tag
// being an identifier with no `$`), or "" when rest starts none, as `$1`, a parameter, does not.
func sqlDollarTag(rest string) string {
	for end := 1; end < len(rest); end++ {
		switch c := rest[end]; {
		case c == '$':
			return rest[:end+1]
		case end == 1 && c >= '0' && c <= '9', !sqlIdentifierByte(c):
			return ""
		}
	}
	return ""
}

// sqlIdentifierByte is whether c may stand in an unquoted SQL identifier: a letter, a digit, `_`,
// `$`, or a byte of a character past ASCII.
func sqlIdentifierByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || (c >= '0' && c <= '9') || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

// The SQL lexical rules ruleNeedle reads, as PostgreSQL's lexer has them (src/backend/parser/scan.l):
//   - sqlSpace is a character of space between two tokens (scan.l's space). In the .sql count a
//     comment between two tokens is space too, since sqlCode blanks each comment to one space.
//   - sqlIdentifierCont is a character an unquoted identifier goes on with (ident_cont): a letter,
//     `_`, a digit, `$` and any character past ASCII. Its first character is any of these but a
//     digit or `$` (ident_start).
//   - sqlIdentifier is one identifier: unquoted; quoted, in which a doubled quote stands for one;
//     or quoted with Unicode escapes (U&"…"), with the UESCAPE clause that may follow it.
const (
	sqlSpace          = `[\t\n\v\f\r ]`
	sqlIdentifierCont = `a-z0-9_$\x{80}-\x{10FFFF}`
	sqlIdentifier     = `(?:[a-z_\x{80}-\x{10FFFF}][` + sqlIdentifierCont + `]*|(?:u&)?"(?:[^"]|"")*"(?:` +
		sqlSpace + `*uescape` + sqlSpace + `*'[^']')?)`
)

// ruleNeedle is the rule's needle, `coalesce(a.asked_of`, in each spelling SQL's lexical rules give
// it: `coalesce` in any case; any space; any number of qualifiers, each an identifier of any
// spelling (an alias, a table, a schema); and the column, unquoted in any case and read whole, or
// quoted, with or without U&. It does not decode a column whose quoted name spells a character
// with a Unicode escape (U&"asked\005fof"). The count of the module's Go and the count of its .sql
// files both read it. The Go count reads a file's text as it stands, so there a SQL comment inside
// the needle is no space, and a quote that a Go string escapes (\") is no quote.
var ruleNeedle = regexp.MustCompile(`(?i)\bcoalesce` + sqlSpace + `*\(` + sqlSpace + `*(?:` + sqlIdentifier +
	sqlSpace + `*\.` + sqlSpace + `*)*(?:asked_of(?:[^` + sqlIdentifierCont + `]|$)|(?:u&)?(?-i:"asked_of"))`)

// goRuleCount is the number of copies of the rule in the text of a Go source.
func goRuleCount(text string) int {
	return len(ruleNeedle.FindAllStringIndex(text, -1))
}

// sqlRuleCount is the number of copies of the rule in a SQL text, read with its comments blanked
// (sqlCode).
func sqlRuleCount(text string) int {
	return len(ruleNeedle.FindAllStringIndex(sqlCode(text), -1))
}

// sqlFilesSpellingTheRule is each .sql file of the module that spells the rule (sqlRuleCount), as
// its path with the number of copies in it. A walk that read no migration would answer none for a
// reason other than there being none, so it fails the test instead.
func sqlFilesSpellingTheRule(t *testing.T) []string {
	t.Helper()
	copies := []string{}
	read, migrations := 0, 0
	migrationDir := filepath.Join(moduleRoot, "internal", "dispatch", "store", "migrations")
	if err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".sql") {
			return err
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		if filepath.Dir(path) == migrationDir {
			migrations++
		}
		if spelled := sqlRuleCount(string(text)); spelled > 0 {
			copies = append(copies, fmt.Sprintf("%s (%d)", filepath.ToSlash(path), spelled))
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s for .sql files: %v", moduleRoot, err)
	}
	if migrations == 0 {
		t.Fatalf("read %d .sql files and none of %s: the migrations this check holds were not seen", read, migrationDir)
	}
	return copies
}

// TestOneExpressionDecidesWhoWaitsInOneSpelling holds the six reads that name a person to the
// one constant: each of the six names it, and the SQL it holds - the expression and the three
// joins it reads over - is spelled once in the module's Go and never in its .sql files. The
// precedent for reading the source rather than the behaviour is api/lock_level_test.go.
func TestOneExpressionDecidesWhoWaitsInOneSpelling(t *testing.T) {
	t.Parallel()
	sources := moduleSources(t)
	declarations := map[string]ast.Node{}
	for _, source := range sources {
		if filepath.Base(filepath.Dir(source.path)) != "api" {
			continue
		}
		for _, declared := range source.file.Decls {
			switch node := declared.(type) {
			case *ast.FuncDecl:
				declarations[node.Name.Name+"'s query"] = node
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range value.Names {
						declarations[name.Name] = value
					}
				}
			}
		}
	}
	for _, read := range []string{
		"askReadColumns", "listInbox's query", "listOpenAsks's query", "myRepliesOwner",
		"putAskAskedOf's query", "askWaitingOnPerson's query",
	} {
		node, declared := declarations[read]
		if !declared {
			t.Fatalf("%s is no longer declared in the api package; the reads this check holds have moved", read)
		}
		if !namesTheExpression(node) {
			t.Fatalf("%s does not name %s: a read with its own copy of the rule answers a different person",
				read, askedOfPersonExpressionName)
		}
	}

	// Two spellings are each counted across the module, with _test.go files skipped by
	// moduleSources, so the needles below are not found in this file's own literals.
	//
	// The rule's needle is the prefix `coalesce(a.asked_of` in any of its spellings (ruleNeedle)
	// rather than the whole expression: a read that reintroduced the two-step
	// `coalesce(asked_of, assignee)`, under any alias, the spelling that sends a question standing
	// on an issue nobody holds to nobody, is what this count exists to catch, and the whole
	// expression would count it zero times and pass.
	//
	// The joins the rule needs are counted by the one that makes them its own, the project reached
	// through either owner: the other two left joins are ordinary beside any ask.
	copies := []string{}
	joins := []string{}
	for _, source := range sources {
		text, err := os.ReadFile(source.path)
		if err != nil {
			t.Fatalf("read %s: %v", source.path, err)
		}
		if spelled := goRuleCount(string(text)); spelled > 0 {
			copies = append(copies, fmt.Sprintf("%s (%d)", filepath.ToSlash(source.path), spelled))
		}
		joined := strings.Count(string(text), "left join projects p on p.key = coalesce(i.project_key, ar.project_key)")
		if joined > 0 {
			joins = append(joins, fmt.Sprintf("%s (%d)", filepath.ToSlash(source.path), joined))
		}
	}
	slices.Sort(copies)
	slices.Sort(joins)
	if want := []string{"../../../internal/dispatch/api/ask_rows.go (1)"}; !slices.Equal(copies, want) {
		t.Fatalf("the rule is spelled in %v, want it written once, in %v", copies, want)
	}
	if want := []string{"../../../internal/dispatch/api/ask_rows.go (1)"}; !slices.Equal(joins, want) {
		t.Fatalf("the joins the rule reads are spelled in %v, want them written once, in %v", joins, want)
	}

	// No .sql file of the module spells the rule at all. The counts above read Go, and a migration
	// is SQL Postgres runs as it stands - a view, a function, a backfill - so a copy of the rule
	// there, the two-step `coalesce(a.asked_of, i.assignee)` above all, answers a reader another
	// person while both counts above pass. Comments are blanked first (sqlCode): an applied
	// migration says what the rule was when it ran, and is never edited. The needle and the
	// blanking are first held to what they must and must not count, a Go spelling and a SQL one, so
	// a needle or a blanking that missed a copy fails here.
	for _, planted := range []struct {
		source  string
		spelled int
	}{
		{`const q = "select coalesce(asked_of, assignee) from asks"`, 1},
		{`const q = "select coalesce( a.asked_of , i.assignee ) from asks a"`, 1},
		// SQL in a raw string, under a quoted qualifier holding a doubled quote and under one past ASCII.
		{"const q = `select coalesce(\"a\"\"b\".asked_of, 'fallback') from asks as \"a\"\"b\"`", 1},
		{"const q = `select coalesce(ä.asked_of, 'fallback') from asks as ä`", 1},
	} {
		if got := goRuleCount(planted.source); got != planted.spelled {
			t.Errorf("the Go count finds %d copies of the rule in %q, want %d", got, planted.source, planted.spelled)
		}
	}
	for _, planted := range []struct {
		sql     string
		spelled int
	}{
		{"select coalesce(a.asked_of, i.assignee) from asks a", 1},
		{"SELECT COALESCE( asks.asked_of , issues.assignee ) FROM asks", 1},
		{"select '--', coalesce(asked_of, assignee) from asks", 1},
		{"select coalesce(a.\"asked_of\", i.assignee) from asks a", 1},
		{"select coalesce(public.asks.asked_of, i.assignee) from asks", 1},
		// An escape string's backslash escapes the quote after it, so its `--` is text.
		{`select E'it\'s -- text', coalesce(a.asked_of, i.assignee) from asks a`, 1},
		// A quoted identifier is no comment either.
		{`select "a--b", coalesce(asked_of, assignee) from asks`, 1},
		{"-- coalesce(a.asked_of, i.assignee)\nselect 1", 0},
		{"/* coalesce(a.asked_of, i.assignee) */ select 1", 0},
		// A block comment nests, and ends at the `*/` of its outermost `/*`.
		{"/* outer /* inner */ coalesce(a.asked_of, i.assignee) */ select 1", 0},
		// A dollar-quoted body is SQL the function runs: its comments are blanked, its code read,
		// and a comment in it ends at the body's end.
		{"create function f() returns void language plpgsql as $$ begin -- coalesce(a.asked_of, i.assignee)\nend $$", 0},
		{"create function f() returns text language sql as $body$ select coalesce(a.asked_of, i.assignee) from asks a $body$", 1},
		{"select $q$ -- $q$, coalesce(a.asked_of, i.assignee) from asks a", 1},
		// A qualifier is any identifier PostgreSQL's lexer reads (scan.l): a quoted one holding a
		// doubled quote, which stands for one quote; an unquoted one with a character past ASCII or
		// a `$` in it; and a quoted one with Unicode escapes, with or without its UESCAPE clause.
		{`select coalesce("a""b".asked_of, 'fallback') from asks as "a""b"`, 1},
		{"select coalesce(ä.asked_of, 'fallback') from asks as ä", 1},
		{"select coalesce(a$1.asked_of, 'fallback') from asks as a$1", 1},
		{`select coalesce(U&"\00e4".asked_of, 'fallback') from asks as U&"\00e4"`, 1},
		{`select coalesce(U&"!00e4" UESCAPE '!'.asked_of, 'fallback') from asks as U&"!00e4" UESCAPE '!'`, 1},
		{`select coalesce(a.U&"asked_of", 'fallback') from asks a`, 1},
		// The same text wholly inside a line comment or a nested block comment is no copy.
		{"-- coalesce(\"a\"\"b\".asked_of, 'fallback')\nselect 1", 0},
		{"-- coalesce(ä.asked_of, 'fallback')\nselect 1", 0},
		{"/* outer /* inner */ coalesce(\"a\"\"b\".asked_of, 'fallback') */ select 1", 0},
		{"/* outer /* inner */ coalesce(ä.asked_of, 'fallback') */ select 1", 0},
		// A line comment ends at a carriage return as it does at a newline (scan.l's non_newline).
		{"-- a note\rselect coalesce(a.asked_of, i.assignee) from asks a", 1},
		// A name is read whole: asked_of with a character past ASCII, a `$` or a capital letter after
		// it is another column.
		{"select coalesce(a.asked_ofä, i.assignee) from asks a", 0},
		{"select coalesce(a.asked_of$, i.assignee) from asks a", 0},
		{"select coalesce(a.asked_ofX, i.assignee) from asks a", 0},
	} {
		if got := sqlRuleCount(planted.sql); got != planted.spelled {
			t.Errorf("the .sql count finds %d copies of the rule in %q, want %d", got, planted.sql, planted.spelled)
		}
	}
	if t.Failed() {
		return
	}
	if sqlCopies := sqlFilesSpellingTheRule(t); len(sqlCopies) > 0 {
		t.Fatalf("the rule is spelled in the SQL of %v, want it in no .sql file: %s names it once, in ask_rows.go",
			sqlCopies, askedOfPersonExpressionName)
	}
}
