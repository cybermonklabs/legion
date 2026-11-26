package graphstore

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// commentWriteShape is a statement that writes a comment's row, the rows of its blocks or their
// texts, whose columns the head rows of a comment and of its blocks hold (HeadRowsSQL): an insert
// into, an update of or a delete from comments, comment_blocks or comment_block_texts, or from the
// tables an item index names for its item's kind (refs' itemTables: `tables.items`,
// `tables.blocks`, `tables.texts`), a comment's among them. A statement is read as its text with
// each operand that is no literal written `{<expression>}`.
var commentWriteShape = regexp.MustCompile(`(?i)\b(insert\s+into|update|delete\s+from)\s+((public\.)?(comments|comment_blocks|comment_block_texts)\b|\{tables\.(items|blocks|texts)\})`)

// commentWritesNoHead are the writers of a conversation table whose statements set no value the head
// rows of a comment or of its blocks hold and that name no head write, each beside what they set:
// the update trigger of comments (comments_head_marks_update) passes each over too, or the
// statements before it in its transaction claim the head rows kept, so none takes an issue's mark
// away. Their statements need no claim of their own (keptHeadRowsClaim).
var commentWritesNoHead = map[string]string{
	"api/comment_actions.go#reopenComment":   "sets resolved, resolved_by and resolved_at",
	"api/comment_actions.go#commentAction":   "sets resolved, resolved_by, resolved_at and a suggestion's acceptance",
	"api/comment_create.go#createCommentFor": "reopens the thread's root: resolved, resolved_by and resolved_at",
	"refs/item_blocks.go#TombstoneItem": "clears the excerpts and the recorded texts of a deleted post's blocks, every one of " +
		"which the delete's index (queueBlocks, whose head write is queueBlockHeads) took out of the post earlier in the same " +
		"batch: no current block is left whose head row holds either, and the delete's clear of the post's row and that index " +
		"claim the head rows kept before it, so the head mark triggers of the blocks and their texts pass it over",
}

// commentWritesLeftTo are the writers of a conversation table that leave the head rows to a site
// their caller runs after them in the same transaction, each beside those sites, of which each names
// a head write.
var commentWritesLeftTo = map[string][]string{
	"api/comment_create.go#queueCommentRow":    {"api/comment_create.go#queueCommentCreated"},
	"api/post_history.go#clearDeletedPostBody": {"api/comment_delete.go#deleteComment"},
	"refs/item_blocks.go#queueBlocks":          {"refs/item_blocks.go#queueBlockHeads"},
}

// keptHeadRowsClaim is a statement's claim that the head rows of what it writes are kept
// (KeptHeadRows), as commentWriteStatement spells the constant spliced into its text: bare in this
// package, under its package's name outside it.
var keptHeadRowsClaim = regexp.MustCompile(`\{(?:\w+\.)?KeptHeadRows\}`)

// commentStatementGoesOn is how each verb of commentWriteShape goes on after its table in a
// statement: an insert with its column list, a select or values; an update with its set, after an
// alias where it names one; a delete with its where, using or returning, after an alias where it
// names one, or with nothing. Text that names such a statement and goes on otherwise is prose - an
// error's words, `update comments anchor %s` - and writes nothing.
var commentStatementGoesOn = map[string]*regexp.Regexp{
	"insert into": regexp.MustCompile(`(?i)^\s*(\(|select\b|values\b)`),
	"update":      regexp.MustCompile(`(?i)^(\s+(as\s+)?\w+)?\s+set\b`),
	"delete from": regexp.MustCompile(`(?i)^((\s+(as\s+)?\w+)?\s+(where|using|returning)\b|\s*$)`),
}

// commentStatementWrites says whether statement holds a match of commentWriteShape that goes on as a
// statement does (commentStatementGoesOn).
func commentStatementWrites(statement string) bool {
	for _, match := range commentWriteShape.FindAllStringSubmatchIndex(statement, -1) {
		verb := strings.ToLower(strings.Join(strings.Fields(statement[match[2]:match[3]]), " "))
		if commentStatementGoesOn[verb].MatchString(statement[match[1]:]) {
			return true
		}
	}
	return false
}

// commentWriteStatement is the text of a statement expr spells, its literals unquoted and every
// other operand of a concatenation written `{<expression>}`, and false for an expression that is no
// string literal and no concatenation with one.
func commentWriteStatement(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(expr.Value)
		return text, err == nil
	case *ast.ParenExpr:
		return commentWriteStatement(expr.X)
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, literalLeft := commentWriteStatement(expr.X)
		right, literalRight := commentWriteStatement(expr.Y)
		if !literalLeft && !literalRight {
			return "", false
		}
		if !literalLeft {
			left = "{" + types.ExprString(expr.X) + "}"
		}
		if !literalRight {
			right = "{" + types.ExprString(expr.Y) + "}"
		}
		return left + right, true
	}
	return "", false
}

// commentWriteSitesIn is every function of one file that spells a statement of commentWriteShape
// (commentStatementWrites), by its site key, with each statement's place as "<file>:<line>", and
// unkept the places of those statements that make no claim that the head rows are kept
// (keptHeadRowsClaim).
func commentWriteSitesIn(relative string, files *token.FileSet, file *ast.File) (found, unkept map[string][]string) {
	found, unkept = map[string][]string{}, map[string][]string{}
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.FuncDecl)
		if !ok || decl.Body == nil {
			continue
		}
		site := siteKey(relative, decl.Name.Name)
		ast.Inspect(decl.Body, func(node ast.Node) bool {
			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			statement, spelled := commentWriteStatement(expr)
			if !spelled {
				return true
			}
			if commentStatementWrites(statement) {
				place := relative + ":" + strconv.Itoa(files.Position(expr.Pos()).Line)
				found[site] = append(found[site], place)
				if !keptHeadRowsClaim.MatchString(statement) {
					unkept[site] = append(unkept[site], place)
				}
			}
			return false
		})
	}
	return found, unkept
}

// commentWriteReading is one file's writers of a conversation table, the places of their statements
// that claim no head rows kept, and its head writers.
type commentWriteReading struct {
	found  map[string][]string
	unkept map[string][]string
	heads  map[string]bool
}

// readCommentWriteFile reads one file's source as the scan reads it.
func readCommentWriteFile(t *testing.T, relative string, source []byte) commentWriteReading {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	found, unkept := commentWriteSitesIn(relative, files, file)
	return commentWriteReading{found: found, unkept: unkept, heads: headWriteSitesIn(relative, files, file)}
}

// readCommentWriteScan reads every Go file of the module but the tests, by its path from the
// module.
func readCommentWriteScan(t *testing.T) map[string]commentWriteReading {
	t.Helper()
	readings := map[string]commentWriteReading{}
	walkModuleSources(t, "comment", func(relative string, source []byte) {
		readings[relative] = readCommentWriteFile(t, relative, source)
	})
	return readings
}

// commentWriteVerdicts are the failures of the scan's verdict over readings, with relative's
// reading replaced by replaced where relative is not "": each writer of a conversation table that
// names no head write and is on neither list, each whose named site writes none, each that names a
// head write or leaves it to another site and writes in a statement that claims no head rows kept,
// and each list entry the scan does not hold or that names a head write after all. passing counts
// the writers that name a head write.
func commentWriteVerdicts(readings map[string]commentWriteReading, relative string, replaced commentWriteReading) (failures []string, passing int) {
	found, unkept, heads := map[string][]string{}, map[string][]string{}, map[string]bool{}
	for file, reading := range readings {
		if file == relative {
			reading = replaced
		}
		for site, statements := range reading.found {
			found[site] = append(found[site], statements...)
		}
		for site, statements := range reading.unkept {
			unkept[site] = append(unkept[site], statements...)
		}
		maps.Copy(heads, reading.heads)
	}
	for _, site := range slices.Sorted(maps.Keys(found)) {
		_, noHead := commentWritesNoHead[site]
		causes, leftTo := commentWritesLeftTo[site]
		switch {
		case (noHead || leftTo) && heads[site]:
			failures = append(failures, site+" names a head write, and a list of this test names it as a writer that writes none: take it off the list")
		case noHead:
		case leftTo:
			for _, cause := range causes {
				if !heads[cause] {
					failures = append(failures, site+" leaves its head rows to "+cause+", which writes none")
				}
			}
		case !heads[site]:
			failures = append(failures, site+" writes a comment, its blocks or their texts and no head row (at "+strings.Join(found[site], ", ")+")")
		default:
			passing++
		}
		if !noHead && (leftTo || heads[site]) && len(unkept[site]) > 0 {
			failures = append(failures, site+" writes a comment, its blocks or their texts in a statement that does not claim their "+
				"head rows kept (KeptHeadRows), so the head mark trigger of the table takes the issue's mark away as the statement "+
				"runs (at "+strings.Join(unkept[site], ", ")+")")
		}
	}
	for _, site := range slices.Concat(slices.Sorted(maps.Keys(commentWritesNoHead)), slices.Sorted(maps.Keys(commentWritesLeftTo))) {
		if _, ok := found[site]; !ok {
			failures = append(failures, "a list of this test names "+site+", and no file of the module writes a conversation table there any more: take it off the list")
		}
	}
	return failures, passing
}

// TestEveryWriterOfACommentWritesItsHeadRows reads every Go file of the module but the tests and
// finds each function that spells a statement writing a comment's row, the rows of its blocks or
// their texts (commentWriteShape), prose that names such a statement left out
// (commentStatementGoesOn). The head rows of a comment and of its current blocks hold those columns
// (issue_head_nodes' item_ columns, HeadRowsSQL), so each such function names a head write
// (headWriteFunctions) of what it writes. It fails on each that names none, printing `<site> writes
// a comment, its blocks or their texts and no head row`, but for the writers whose statements set no
// value a head row holds (commentWritesNoHead) and those that leave the head rows to a site run after
// them (commentWritesLeftTo), each printed with why, of which each named site names a head write or
// the test fails printing `<site> leaves its head rows to <site>, which writes none`. A list entry the
// source holds no such writer at, or that names a head write after all, fails too.
//
// The head mark triggers of the three tables (issue_head_items_drop) fire inside the statement that
// writes the row, unless the transaction has claimed the head rows kept by then, so every statement
// of a writer that names a head write, or leaves it to another site, makes that claim itself
// (KeptHeadRows, keptHeadRowsClaim): the test fails on each that does not, printing `<site> writes a
// comment, its blocks or their texts in a statement that does not claim their head rows kept`.
//
// The same verdict over three copies goes into the log: one file with two writers planted, one that
// sets a comment's body and writes no head row and one that sets it with no claim and writes the head
// row, which the verdict finds both of; the item index's head write taken out of a copy of its file,
// which the verdict names beside the writer that leaves its head rows to it; and the claim taken out
// of the item index's statements in another copy, which the verdict names at that writer. A writer
// with no head write leaves the rows of a comment or of its blocks behind the store and the issue
// marked, and the head read answers the stale rows; a statement with no claim takes away the mark of
// an issue whose rows its own writer keeps, so the head read walks the tuples until the backfill
// fills it again; a writer of a release before has its issue's mark taken away by the triggers, as
// it should.
func TestEveryWriterOfACommentWritesItsHeadRows(t *testing.T) {
	readings := readCommentWriteScan(t)
	for _, site := range slices.Sorted(maps.Keys(commentWritesNoHead)) {
		t.Logf("writer %s sets no value a head row holds: %s", site, commentWritesNoHead[site])
	}
	for _, site := range slices.Sorted(maps.Keys(commentWritesLeftTo)) {
		t.Logf("writer %s leaves its head rows to %s", site, strings.Join(commentWritesLeftTo[site], ", "))
	}
	failures, passing := commentWriteVerdicts(readings, "", commentWriteReading{})
	for _, failure := range failures {
		t.Error(failure)
	}
	if passing == 0 {
		t.Fatalf("found no writer of a conversation table that names a head write under %s: the walk read nothing worth checking", moduleRoot)
	}
	t.Logf("read %d writers of a conversation table that name a head write, %d that set no value a head row holds, %d that leave their head rows to another site",
		passing, len(commentWritesNoHead), len(commentWritesLeftTo))

	// Two writers planted in a copy of one file, which set a comment's body: one writes no head row,
	// and one writes it and claims no head rows kept.
	const plantedFile = "internal/dispatch/api/comment_actions.go"
	source, err := os.ReadFile(filepath.Join(moduleRoot, plantedFile))
	if err != nil {
		t.Fatalf("read %s: %v", plantedFile, err)
	}
	planted := append(slices.Clone(source), []byte("\nfunc plantedHeadlessCommentWriter(ctx context.Context, tx pgx.Tx) error {\n\t_, err := tx.Exec(ctx, `update comments set body = $2 where id = $1`, \"\", \"\")\n\treturn err\n}\n"+
		"\nfunc plantedUnkeptCommentWriter(ctx context.Context, tx pgx.Tx) error {\n\tif _, err := tx.Exec(ctx, `update comments set body = $2 where id = $1`, \"\", \"\"); err != nil {\n\t\treturn err\n\t}\n"+
		"\treturn graphstore.WriteHeads(ctx, tx, []graphstore.NodeKey{{Kind: graphstore.KindComment, ID: \"\"}})\n}\n")...)
	plantedFailures, _ := commentWriteVerdicts(readings, plantedFile, readCommentWriteFile(t, plantedFile, planted))
	t.Logf("the copy of %s with two writers planted reads: %s", plantedFile, strings.Join(plantedFailures, "; "))
	for _, want := range []string{
		"api/comment_actions.go#plantedHeadlessCommentWriter writes a comment, its blocks or their texts and no head row (at " + plantedFile + ":",
		"api/comment_actions.go#plantedUnkeptCommentWriter writes a comment, its blocks or their texts in a statement that does not claim their head rows kept",
	} {
		if !slices.ContainsFunc(plantedFailures, func(failure string) bool { return strings.HasPrefix(failure, want) }) {
			t.Errorf("the copy of %s with two writers planted reads %q, want one beginning %q among it", plantedFile, plantedFailures, want)
		}
	}

	// The head write taken out of a copy of the item index's file.
	const indexFile = "internal/dispatch/refs/item_blocks.go"
	source, err = os.ReadFile(filepath.Join(moduleRoot, indexFile))
	if err != nil {
		t.Fatalf("read %s: %v", indexFile, err)
	}
	headWrite := []byte("\tgraphstore.QueueHeads(b, nodes)\n")
	if !bytes.Contains(source, headWrite) {
		t.Fatalf("%s holds no line %q to take out", indexFile, headWrite)
	}
	indexFailures, _ := commentWriteVerdicts(readings, indexFile, readCommentWriteFile(t, indexFile, bytes.Replace(source, headWrite, nil, 1)))
	want := "refs/item_blocks.go#queueBlocks leaves its head rows to refs/item_blocks.go#queueBlockHeads, which writes none"
	t.Logf("the copy of %s with the head write of queueBlockHeads taken out reads: %s", indexFile, strings.Join(indexFailures, "; "))
	if !slices.Contains(indexFailures, want) {
		t.Errorf("the copy of %s with the head write taken out of queueBlockHeads reads %q, want %q among it", indexFile, indexFailures, want)
	}

	// The claim taken out of the item index's statements in another copy of its file.
	claim := []byte("`+graphstore.KeptHeadRows+`")
	if !bytes.Contains(source, claim) {
		t.Fatalf("%s holds no claim %q to take out", indexFile, claim)
	}
	unkeptFailures, _ := commentWriteVerdicts(readings, indexFile, readCommentWriteFile(t, indexFile, bytes.ReplaceAll(source, claim, []byte("true"))))
	want = "refs/item_blocks.go#queueBlocks writes a comment, its blocks or their texts in a statement that does not claim their head rows kept"
	t.Logf("the copy of %s with the claim taken out of its statements reads: %s", indexFile, strings.Join(unkeptFailures, "; "))
	if !slices.ContainsFunc(unkeptFailures, func(failure string) bool { return strings.HasPrefix(failure, want) }) {
		t.Errorf("the copy of %s with the claim taken out of its statements reads %q, want one beginning %q among it", indexFile, unkeptFailures, want)
	}
}
