package graphstore

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
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
// the deferred update trigger of comments (comments_head_marks_update) passes each over too, so none
// takes an issue's mark away.
var commentWritesNoHead = map[string]string{
	"api/comment_actions.go#reopenComment":   "sets resolved, resolved_by and resolved_at",
	"api/comment_actions.go#commentAction":   "sets resolved, resolved_by, resolved_at and a suggestion's acceptance",
	"api/comment_create.go#createCommentFor": "reopens the thread's root: resolved, resolved_by and resolved_at",
	"refs/item_blocks.go#TombstoneItem": "clears the excerpts and the recorded texts of a deleted post's blocks, every one of " +
		"which the delete's index (queueBlocks, whose head write is queueBlockHeads) took out of the post earlier in the same " +
		"batch: no current block is left whose head row holds either",
}

// commentWritesLeftTo are the writers of a conversation table that leave the head rows to a site
// their caller runs after them in the same transaction, each beside those sites, of which each names
// a head write.
var commentWritesLeftTo = map[string][]string{
	"api/comment_create.go#queueCommentRow":    {"api/comment_create.go#queueCommentCreated"},
	"api/post_history.go#clearDeletedPostBody": {"api/comment_delete.go#deleteComment"},
	"refs/item_blocks.go#queueBlocks":          {"refs/item_blocks.go#queueBlockHeads"},
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

// commentWriteSitesIn is every function of one file that spells a statement of commentWriteShape,
// by its site key, with each statement's place as "<file>:<line>".
func commentWriteSitesIn(relative string, files *token.FileSet, file *ast.File) map[string][]string {
	sites := map[string][]string{}
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
			if commentWriteShape.MatchString(statement) {
				sites[site] = append(sites[site], relative+":"+strconv.Itoa(files.Position(expr.Pos()).Line))
			}
			return false
		})
	}
	return sites
}

// commentWriteReading is one file's writers of a conversation table and its head writers.
type commentWriteReading struct {
	found map[string][]string
	heads map[string]bool
}

// readCommentWriteFile reads one file's source as the scan reads it.
func readCommentWriteFile(t *testing.T, relative string, source []byte) commentWriteReading {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, relative, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	return commentWriteReading{found: commentWriteSitesIn(relative, files, file), heads: headWriteSitesIn(relative, files, file)}
}

// readCommentWriteScan reads every Go file of the module but the tests, by its path from the
// module.
func readCommentWriteScan(t *testing.T) map[string]commentWriteReading {
	t.Helper()
	readings := map[string]commentWriteReading{}
	err := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != moduleRoot && skippedDirectory(path, entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(source, []byte("comment")) {
			return nil
		}
		relative, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		readings[filepath.ToSlash(relative)] = readCommentWriteFile(t, filepath.ToSlash(relative), source)
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's source under %s: %v", moduleRoot, err)
	}
	return readings
}

// commentWriteVerdicts are the failures of the scan's verdict over readings, with relative's
// reading replaced by replaced where relative is not "": each writer of a conversation table that
// names no head write and is on neither list, each whose named site writes none, and each list
// entry the scan does not hold or that names a head write after all. passing counts the writers
// that name a head write.
func commentWriteVerdicts(readings map[string]commentWriteReading, relative string, replaced commentWriteReading) (failures []string, passing int) {
	found, heads := map[string][]string{}, map[string]bool{}
	for file, reading := range readings {
		if file == relative {
			reading = replaced
		}
		for site, statements := range reading.found {
			found[site] = append(found[site], statements...)
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
	}
	for _, list := range []map[string]bool{keysOf(commentWritesNoHead), keysOf(commentWritesLeftTo)} {
		for _, site := range slices.Sorted(maps.Keys(list)) {
			if _, ok := found[site]; !ok {
				failures = append(failures, "a list of this test names "+site+", and no file of the module writes a conversation table there any more: take it off the list")
			}
		}
	}
	return failures, passing
}

// TestEveryWriterOfACommentWritesItsHeadRows reads every Go file of the module but the tests and
// finds each function that spells a statement writing a comment's row, the rows of its blocks or
// their texts (commentWriteShape). The head rows of a comment and of its current blocks hold those
// columns (issue_head_nodes' item_ columns, HeadRowsSQL), so each such function names a head write
// (headWriteFunctions) of what it writes, which sets dispatch.head_rows for its transaction, so the
// deferred triggers of the three tables leave the issue's mark as it is. It fails on each that names
// none, printing `<site> writes a comment, its blocks or their texts and no head row`, but for the
// writers whose statements set no value a head row holds (commentWritesNoHead) and those that leave
// the head rows to a site run after them (commentWritesLeftTo), each printed with why, of which each
// named site names a head write or the test fails printing `<site> leaves its head rows to <site>,
// which writes none`. A list entry the source holds no such writer at, or that names a head write
// after all, fails too.
//
// The same verdict over two copies goes into the log: one file with a writer planted that sets a
// comment's body and writes no head row, which the verdict finds, and the item index's head write
// taken out of a copy of its file, which the verdict names beside the writer that leaves its head
// rows to it. Such a writer leaves the rows of a comment or of its blocks behind the store and the
// issue marked, and the head read answers the stale rows; a writer of a release before has its
// issue's mark taken away by the triggers instead.
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

	// A writer planted in a copy of one file, which sets a comment's body and writes no head row.
	const plantedFile = "internal/dispatch/api/comment_actions.go"
	source, err := os.ReadFile(filepath.Join(moduleRoot, plantedFile))
	if err != nil {
		t.Fatalf("read %s: %v", plantedFile, err)
	}
	planted := append(slices.Clone(source), []byte("\nfunc plantedHeadlessCommentWriter(ctx context.Context, tx pgx.Tx) error {\n\t_, err := tx.Exec(ctx, `update comments set body = $2 where id = $1`, \"\", \"\")\n\treturn err\n}\n")...)
	plantedFailures, _ := commentWriteVerdicts(readings, plantedFile, readCommentWriteFile(t, plantedFile, planted))
	want := "api/comment_actions.go#plantedHeadlessCommentWriter writes a comment, its blocks or their texts and no head row (at " + plantedFile + ":"
	t.Logf("the copy of %s with a writer planted reads: %s", plantedFile, strings.Join(plantedFailures, "; "))
	if !slices.ContainsFunc(plantedFailures, func(failure string) bool { return strings.HasPrefix(failure, want) }) {
		t.Errorf("the copy of %s with a writer planted that writes no head row reads %q, want one beginning %q among it", plantedFile, plantedFailures, want)
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
	want = "refs/item_blocks.go#queueBlocks leaves its head rows to refs/item_blocks.go#queueBlockHeads, which writes none"
	t.Logf("the copy of %s with the head write of queueBlockHeads taken out reads: %s", indexFile, strings.Join(indexFailures, "; "))
	if !slices.Contains(indexFailures, want) {
		t.Errorf("the copy of %s with the head write taken out of queueBlockHeads reads %q, want %q among it", indexFile, indexFailures, want)
	}
}
