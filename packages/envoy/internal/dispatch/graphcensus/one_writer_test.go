package graphcensus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"maps"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus/censustest"
	"github.com/sjawhar/envoy/internal/dispatch/startup"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/storesmove"
)

// This file is c-one-writer of dispatch://INFRA-1098 (dispatch://INFRA-1098/spec#b-r-derived): the
// graph is the store, the same transaction writes each projection, nothing writes a projection
// without writing the store, and no reconcile script exists (R21). It proves atomicity by
// behaviour and keeps an inventory as coverage evidence: the behavioural half drives each writer
// of each kind with a failure injected between the store write and the projection write and after
// the projection write, reads nothing either left, and drives two writers of one kind at once; the
// inventory reads the Go sources and the database catalogue, and fails on a writer it finds that no
// case drove.

// moduleRoot is packages/envoy, read from this package's own directory.
const oneWriterModule = "../../.."

// namedProjections are the tables R21 names as projections of the store that no kind of the
// census holds to it on this line, each with why it is one.
var namedProjections = map[string]string{
	"user_threads": "R21 names it among the tables that answer a read, and R22 keeps it as the inbox's latency layer",
}

// storeTables are the store's own two tables.
var storeTables = []string{"graph_nodes", "graph_tuples"}

// storesMovedRecord is the progress table the boot's move of the priority store and the workflow
// store writes inside every batch transaction beside its graph_nodes and graph_tuples writes
// (storesmove/copy.go#writeBatch), and drops once the swap commits (storesmove/swap.go#dropOldTables).
// No table projects priority, step or plan after R32 (census_stores.go): the store's own node row
// projects the node's tuples, and stores_moved_seqs is the move's own record of which old tuples
// each batch copied, so every batch must commit all three together.
const storesMovedRecord = "stores_moved_seqs"

// projection is one table that projects the store: the census kinds that read it, and the
// columns of it the store states, whose update is a write of the projection.
type projection struct {
	table   string
	kinds   []string
	columns []string
	// whole says every column of the table is a projection of the store, so every update of it is
	// a write the store must hold.
	whole bool
}

// readProjections reads the tables that project the store off the census the line ships
// (graphcensus.StoreCensus) over database, and adds the ones R21 names. A kind whose row side is
// many tables' columns - the people, the sessions, the roles and their pages - is one no table
// projects, and is printed, and so is a kind held to the store's own record, as the priority, the
// run and the plan are since the stores' move (dispatch://INFRA-1098's R32).
func readProjections(t *testing.T, ctx context.Context, database *store.Store) map[string]*projection {
	t.Helper()
	report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("run the store census over the inventory's database: %v", err)
	}
	// Each relation of the schema by its kind: a table ('r', 'p') or a view ('v').
	relations := map[string]string{}
	rows, err := database.Pool.Query(ctx, `select relname, relkind::text from pg_class where relkind in ('r', 'p', 'v') and relnamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatalf("read the schema's tables: %v", err)
	}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatal(err)
		}
		relations[name] = kind
	}
	rows.Close()
	projections := map[string]*projection{}
	for _, kind := range report.Kinds {
		if strings.Contains(kind.Table, ",") {
			t.Logf("kind %s (%s): no table projects it; its row side is %s", kind.Kind, kind.NodeKind, kind.Table)
			continue
		}
		table, column, _ := strings.Cut(kind.Table, ".")
		if slices.Contains(storeTables, table) {
			t.Logf("kind %s (%s): held to the store's own record, %s", kind.Kind, kind.NodeKind, kind.Table)
			continue
		}
		if relations[table] == "v" {
			t.Logf("kind %s (%s): held to the view %s, which the store's tuples answer, so no writer writes it", kind.Kind, kind.NodeKind, kind.Table)
			continue
		}
		if relations[table] == "" {
			t.Errorf("kind %s names %s, which is no table of the inventory's database", kind.Kind, kind.Table)
			continue
		}
		held := projections[table]
		if held == nil {
			held = &projection{table: table, columns: []string{"node_kind", "node_address"}}
			if columns, listed := censustest.ProjectedColumns[table]; listed {
				held.columns = slices.Sorted(maps.Keys(columns))
			}
			projections[table] = held
		}
		held.kinds = append(held.kinds, kind.Kind)
		if column != "" && !slices.Contains(held.columns, column) {
			held.columns = append(held.columns, column)
		}
	}
	for table, why := range namedProjections {
		if kind := relations[table]; kind != "r" && kind != "p" {
			t.Errorf("%s, which R21 names a projection, is no table of the inventory's database", table)
			continue
		}
		projections[table] = &projection{table: table, kinds: []string{"(" + why + ")"}, whole: true}
	}
	// The move's record (stores_moved_seqs) is dropped by the swap on the inventory database, so
	// it does not stand in pg_class there; it stands as a table on every pre-move database and is
	// written by storesmove/copy.go#writeBatch in the same transaction as the moved nodes and
	// tuples of priority, step and plan. Holding it in the inventory makes the source scan find
	// storesmove/copy.go#writeBatch and fail unless the behavioural half drives the move for all
	// three kinds.
	projections[storesMovedRecord] = &projection{table: storesMovedRecord,
		kinds: []string{"priority", "step", "plan"}, whole: true}
	if len(projections) == 0 {
		t.Fatal("the census names no table that projects the store, so no writer can be held to it")
	}
	for _, table := range slices.Sorted(maps.Keys(projections)) {
		held := projections[table]
		columns := fmt.Sprint(held.columns)
		if held.whole {
			columns = "every column"
		}
		t.Logf("projection %s: kinds %v, the columns the store states %s", table, held.kinds, columns)
	}
	return projections
}

// projectionWrites matches the head of a statement that writes one of tables: an insert, a
// delete or an update, in any case, over any whitespace, under the schema's name or bare.
func projectionWrites(tables []string) *regexp.Regexp {
	quoted := make([]string, len(tables))
	for index, table := range tables {
		quoted[index] = regexp.QuoteMeta(table)
	}
	return regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s+(?:only\s+)?(?:public\s*\.\s*)?(` + strings.Join(quoted, "|") + `)\b`)
}

// sourceWriter is one function of the module's sources that writes a projection: the statement
// it holds, by its table and its verb, and whether the store must hold it - an insert or a
// delete, which makes or unmakes a row, or an update that sets a column the store states.
type sourceWriter struct {
	key, file, function string
	line                int
	table, verb, text   string
	// whole is the writer's literal from its statement's head to the literal's end, folded: what
	// tells two writers apart whose heads are the same up to their first parameter.
	whole string
	held  bool
}

// sourceInventory is what the scan of the Go sources found.
type sourceInventory struct {
	writers   []sourceWriter
	unheld    []sourceWriter
	runtime   []string
	reads     map[string]int
	files     int
	skipped   map[string]int
	functions map[string]bool
}

// runtimeProjectionWrites are the statements of the sources whose table a runtime value names,
// by their site, each with the projections it can reach: none for one that writes no projection.
// A site this list does not name fails the scan, since the scan cannot read which table it writes.
var runtimeProjectionWrites = map[string][]string{
	// The one-time pin of a legacy quote anchor's block over an ask's anchor alone (a comment's
	// goes through pinCommentAnchorBlock, whose literal the scan reads). It sets asks.anchor, which
	// the store does not state: the census holds an ask's row to its node and its answer only.
	"internal/dispatch/docs/anchor_blocks.go#pinAnchorBlock: update": nil,
	// A version's refresh of the anchors on its document, over asks and comments. It sets the
	// anchor column alone, which the store states of a comment and not of an ask.
	"internal/dispatch/docs/anchor_refresh.go#refreshAnchors: update": {"comments"},
	// The block index of a comment, a message and an ask, over the item's own tables, none of
	// them a projection.
	"internal/dispatch/refs/item_blocks.go#queueRecordIndexed: update":   nil,
	"internal/dispatch/refs/item_blocks.go#clearItemWindowsOwed: update": nil,
	"internal/dispatch/refs/item_blocks.go#queueStateLock: insert into":  nil,
	"internal/dispatch/refs/item_blocks.go#queueBlocks: update":          nil,
	"internal/dispatch/refs/item_blocks.go#queueBlocks: insert into":     nil,
	"internal/dispatch/refs/item_blocks.go#queueBlockRefs: delete from":  nil,
	"internal/dispatch/refs/item_blocks.go#queueBlockRefs: insert into":  nil,
	"internal/dispatch/refs/item_blocks.go#markItemBlocksOwed: update":   nil,
	// The stamp of an item's block links: comment_block_refs, message_block_refs or ask_block_refs.
	"internal/dispatch/refs/refs.go#queueStamp: update": nil,
	// The settle of one source an owed-index queue held: block_grammar_owed or refs_grammar_owed.
	"internal/dispatch/refs/rebuild.go#settleOwedStatement: delete from": nil,
	// The boot's rename pass (store.RewriteRenamedRepositories) over the stored repository fields
	// repoRenameLines lists. Of the tables nameWrite is given, two are projections, at a column the
	// store does not state: issues.repo and check_results.commit_repo. The case "the boot's rename
	// pass" drives it and reads graph_nodes and graph_tuples unchanged.
	"internal/dispatch/store/repo_rename_pass.go#nameWrite: update": {"issues", "check_results"},
	// urlWrite's URL columns and collisions' keyed tables are of no projection. collisions' delete of
	// an architecture_sources twin takes that source's components and component_depends rows with
	// it, through components_source_fkey's cascade, which this scan follows for no writer: the case
	// "the boot's rename pass" drives that delete and reads the census of those components.
	"internal/dispatch/store/repo_rename_pass.go#urlWrite: update":        nil,
	"internal/dispatch/store/repo_rename_pass.go#collisions: delete from": nil,
	// A migration runner's version table.
	"internal/pgmigrate/identity.go#writeFixes: update":  nil,
	"internal/pgmigrate/identity.go#Record: insert into": nil,
	// The pre-deploy census's rehearsal of dispatch://INFRA-1098's R32 naming, in its refusal, a
	// pending migration's insert it cannot admit: words of a report, which write nothing.
	"internal/pgmigrate/rehearsal.go#insertsBounded: insert into": nil,
}

// cutPackage is the package dispatch://INFRA-1149's cut (5a51d205) deletes; the package ships in
// dispatch://INFRA-5290 instead. This line stacks on dispatch://INFRA-1253's head, which still
// holds the package, so the scan finds its writers, and no case drives them: each still fails, and
// its failure says it leaves with the cut. This clause goes once the cut is merged, since the
// package is gone then.
const cutPackage = "internal/dispatch/grapherase/"

// runtimeWriteHead matches a literal that ends where a write's table begins, the expression after
// it in a concatenation naming that table, and formattedWriteHead a format whose verb names it.
// setList, assignedColumn and tupleAssignment read an update's set list: its assignments, one
// column's, and a list of columns set together.
var (
	runtimeWriteHead   = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s*$`)
	formattedWriteHead = regexp.MustCompile(`(?is)\b(insert\s+into|delete\s+from|update)\s+%[sv]\b`)
	setList            = regexp.MustCompile(`(?is)^\s+(?:as\s+)?(?:\w+\s+)?set\s+(.*?)(?:\bwhere\b|\bfrom\b|\breturning\b|$)`)
	assignedColumn     = regexp.MustCompile(`(?is)^\s*(?:\w+\s*\.\s*)?"?(\w+)"?\s*(?:\[[^\]]*\])?\s*=`)
	tupleAssignment    = regexp.MustCompile(`(?is)^\s*\(([\w\s,"]+)\)\s*=`)
)

// testSupportPackage reports whether a directory is a package that only tests build: one whose
// name ends in "test", which seeds for other packages' tests the rows a writer refuses to write.
func testSupportPackage(dir string) bool {
	return strings.HasSuffix(filepath.Base(dir), "test")
}

// readSourceWriters parses every Go source of the module but the tests and the test-support
// packages, and answers each function that writes a projection, with what it writes: from its
// own string literals, from a package-level constant or variable it names, and from a statement
// whose table a runtime value names (runtimeProjectionWrites). It counts the reads of each
// projection, the statements that select from it.
func readSourceWriters(t *testing.T, projections map[string]*projection) sourceInventory {
	t.Helper()
	root, err := filepath.Abs(oneWriterModule)
	if err != nil {
		t.Fatal(err)
	}
	writes := projectionWrites(slices.Sorted(maps.Keys(projections)))
	reads := regexp.MustCompile(`(?is)\b(?:from|join)\s+(?:only\s+)?(` + strings.Join(slices.Sorted(maps.Keys(projections)), "|") + `)\b`)
	inventory := sourceInventory{reads: map[string]int{}, skipped: map[string]int{}, functions: map[string]bool{}}
	seenRuntime := map[string]bool{}
	files := token.NewFileSet()
	byDirectory := map[string][]*ast.File{}
	names := map[*ast.File]string{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "testdata" || name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		relative, _ := filepath.Rel(root, path)
		names[parsed] = filepath.ToSlash(relative)
		byDirectory[filepath.Dir(path)] = append(byDirectory[filepath.Dir(path)], parsed)
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go sources: %v", err)
	}
	for directory, parsed := range byDirectory {
		// The package-level constants and variables whose literals hold a projection write, which
		// a function that names one issues.
		held := map[string][]string{}
		for _, file := range parsed {
			for _, declared := range file.Decls {
				general, ok := declared.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range value.Names {
						if index < len(value.Values) {
							held[name.Name] = append(held[name.Name], literalTexts(value.Values[index])...)
						}
					}
				}
			}
		}
		for _, file := range parsed {
			relative := names[file]
			inventory.files++
			skip := testSupportPackage(directory)
			for _, declared := range file.Decls {
				function, ok := declared.(*ast.FuncDecl)
				if !ok || function.Body == nil {
					continue
				}
				key := relative + "#" + function.Name.Name
				inventory.functions[key] = true
				var texts, copied []string
				inner := map[*ast.BinaryExpr]bool{}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch found := node.(type) {
					case *ast.BasicLit:
						if found.Kind == token.STRING {
							if text, err := strconv.Unquote(found.Value); err == nil {
								texts = append(texts, text)
							}
						}
					case *ast.Ident:
						texts = append(texts, held[found.Name]...)
					case *ast.BinaryExpr:
						if found.Op != token.ADD || inner[found] {
							return true
						}
						parts := concatenated(found, inner)
						for index, part := range parts {
							literal, ok := part.(*ast.BasicLit)
							if !ok || literal.Kind != token.STRING || index+1 >= len(parts) {
								continue
							}
							text, err := strconv.Unquote(literal.Value)
							if err != nil {
								continue
							}
							if next, ok := parts[index+1].(*ast.BasicLit); ok && next.Kind == token.STRING {
								continue
							}
							if head := runtimeWriteHead.FindStringSubmatch(text); head != nil {
								site := relative + "#" + function.Name.Name + ": " + strings.ToLower(strings.Join(strings.Fields(head[1]), " "))
								if !seenRuntime[site] && !skip {
									seenRuntime[site] = true
									inventory.runtime = append(inventory.runtime, site)
								}
							}
						}
					case *ast.CallExpr:
						// A copy names its table as a pgx.Identifier, not in a statement's text: it
						// inserts rows of that table, which the regular expression cannot read.
						if selector, ok := found.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "CopyFrom" {
							for _, argument := range found.Args {
								if table := copiedTable(argument); table != "" {
									copied = append(copied, table)
								}
							}
						}
						if selector, ok := found.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sprintf" && len(found.Args) > 0 {
							if literal, ok := found.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
								if text, err := strconv.Unquote(literal.Value); err == nil {
									if head := formattedWriteHead.FindStringSubmatch(text); head != nil {
										site := relative + "#" + function.Name.Name + ": " + strings.ToLower(strings.Join(strings.Fields(head[1]), " "))
										if !seenRuntime[site] && !skip {
											seenRuntime[site] = true
											inventory.runtime = append(inventory.runtime, site)
										}
									}
								}
							}
						}
					}
					return true
				})
				line := files.Position(function.Pos()).Line
				seen := map[string]bool{}
				for _, text := range texts {
					for _, match := range reads.FindAllStringSubmatch(text, -1) {
						inventory.reads[strings.ToLower(match[1])]++
					}
					for _, match := range writes.FindAllStringSubmatchIndex(text, -1) {
						verb := strings.ToLower(strings.Join(strings.Fields(text[match[2]:match[3]]), " "))
						table := strings.ToLower(text[match[4]:match[5]])
						writer := sourceWriter{key: key, file: relative, function: function.Name.Name, line: line, table: table, verb: verb,
							text: normalizedStatement(text[match[0]:]), whole: foldedStatement(text[match[0]:]), held: true}
						if verb == "update" {
							writer.held, writer.verb = updateHeld(projections[table], text[match[5]:])
						}
						if seen[writer.table+" "+writer.verb] {
							continue
						}
						seen[writer.table+" "+writer.verb] = true
						if skip {
							inventory.skipped[relative]++
							continue
						}
						if writer.held {
							inventory.writers = append(inventory.writers, writer)
						} else {
							inventory.unheld = append(inventory.unheld, writer)
						}
					}
				}
				for _, table := range copied {
					if projections[table] == nil || seen[table+" insert into"] {
						continue
					}
					seen[table+" insert into"] = true
					if skip {
						inventory.skipped[relative]++
						continue
					}
					// The text is the copy's statement as pgx sends it, which says no more than its
					// table, so only a case that stands for its process credits it.
					statement := foldedStatement(`copy "` + table + `"`)
					inventory.writers = append(inventory.writers, sourceWriter{key: key, file: relative, function: function.Name.Name, line: line,
						table: table, verb: "insert into", text: statement, whole: statement, held: true})
				}
			}
		}
	}
	for _, site := range inventory.runtime {
		function, _, _ := strings.Cut(site, ": ")
		file, name, _ := strings.Cut(function, "#")
		tables, listed := runtimeProjectionWrites[site]
		if !listed {
			if strings.HasPrefix(file, cutPackage) {
				t.Errorf("%s builds the table it writes at runtime, and no case drives it: it leaves with dispatch://INFRA-1149's cut (5a51d205), which deletes %s", site, cutPackage)
				continue
			}
			t.Errorf("%s builds the table it writes at runtime, and runtimeProjectionWrites does not say which projections it can reach", site)
			continue
		}
		for _, table := range tables {
			if _, projected := projections[table]; projected {
				inventory.writers = append(inventory.writers, sourceWriter{key: function, file: file, function: name, table: table,
					verb: strings.TrimPrefix(site, function+": ") + " (its table a runtime value)", held: true})
			}
		}
	}
	for site := range runtimeProjectionWrites {
		if !seenRuntime[site] {
			t.Errorf("runtimeProjectionWrites names %s, which the sources no longer hold", site)
		}
	}
	if inventory.files < 100 {
		t.Fatalf("the scan read %d Go files, which is no module: it is looking in the wrong place", inventory.files)
	}
	slices.SortFunc(inventory.writers, func(a, b sourceWriter) int { return strings.Compare(a.key+a.table+a.verb, b.key+b.table+b.verb) })
	return inventory
}

// copiedTable is the table a CopyFrom argument names, a pgx.Identifier of one string literal, or
// "" for any other argument.
func copiedTable(argument ast.Expr) string {
	literal, ok := argument.(*ast.CompositeLit)
	if !ok || len(literal.Elts) != 1 {
		return ""
	}
	if selector, ok := literal.Type.(*ast.SelectorExpr); !ok || selector.Sel.Name != "Identifier" {
		return ""
	}
	name, ok := literal.Elts[0].(*ast.BasicLit)
	if !ok || name.Kind != token.STRING {
		return ""
	}
	table, err := strconv.Unquote(name.Value)
	if err != nil {
		return ""
	}
	return strings.ToLower(table)
}

// concatenated flattens the + chain at expression into its parts, marking each inner + of it.
func concatenated(expression ast.Expr, inner map[*ast.BinaryExpr]bool) []ast.Expr {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.ADD {
		return []ast.Expr{expression}
	}
	inner[binary] = true
	return append(concatenated(binary.X, inner), concatenated(binary.Y, inner)...)
}

// literalTexts is every string literal of expression.
func literalTexts(expression ast.Expr) []string {
	var texts []string
	ast.Inspect(expression, func(node ast.Node) bool {
		if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
			if text, err := strconv.Unquote(literal.Value); err == nil {
				texts = append(texts, text)
			}
		}
		return true
	})
	return texts
}

// normalizedStatement is a writer's statement as the attribution looks for it in what a case ran:
// folded (foldedStatement), and cut at its first parameter or at 120 characters, so a statement
// whose later parts a runtime value fills is still found by the part its literal holds.
func normalizedStatement(text string) string {
	folded := foldedStatement(text)
	if index := strings.Index(folded, "$"); index > 0 {
		folded = folded[:index]
	}
	if len(folded) > 120 {
		folded = folded[:120]
	}
	return strings.TrimSpace(folded)
}

// foldedStatement is a statement's text with its whitespace folded and its case lowered, as a
// case ran it, whole.
func foldedStatement(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

// updateHeld reads what an update of projection sets, rest being the statement after the table's
// name, and answers whether the store must hold it and how the scan names it.
func updateHeld(held *projection, rest string) (bool, string) {
	if held.whole {
		return true, "update (every column of it is a projection)"
	}
	match := setList.FindStringSubmatch(rest)
	if match == nil {
		return true, "update (a set list the scan cannot read)"
	}
	var set []string
	depth := 0
	start := 0
	assignments := []string{}
	for index, character := range match[1] {
		switch character {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				assignments = append(assignments, match[1][start:index])
				start = index + 1
			}
		}
	}
	assignments = append(assignments, match[1][start:])
	stated := false
	for _, assignment := range assignments {
		// A tuple assignment, (a, b) = (...), sets each column its list names.
		if tuple := tupleAssignment.FindStringSubmatch(assignment); tuple != nil {
			for _, name := range strings.Split(tuple[1], ",") {
				name = strings.ToLower(strings.Trim(strings.TrimSpace(name), `"`))
				set = append(set, name)
				stated = stated || slices.Contains(held.columns, name)
			}
			continue
		}
		column := assignedColumn.FindStringSubmatch(assignment)
		if column == nil {
			if strings.TrimSpace(assignment) != "" {
				return true, "update (an assignment the scan cannot read: " + strings.TrimSpace(assignment) + ")"
			}
			continue
		}
		set = append(set, strings.ToLower(column[1]))
		if slices.Contains(held.columns, strings.ToLower(column[1])) {
			stated = true
		}
	}
	// A set list whose text the literal does not hold - `update issues set ` followed by columns a
	// runtime value joins - names no column this scan can read, so it may set any of them.
	if len(set) == 0 {
		return true, "update (a set list built at runtime)"
	}
	return stated, "update set " + strings.Join(set, ", ")
}

// catalogueWriter is one function of the schema whose body writes a projection, with the
// triggers that run it, on their tables, and the functions that call it. held says the store must
// hold the write, as a source writer's held says.
type catalogueWriter struct {
	function, body string
	table, verb    string
	held           bool
	triggers       []catalogueTrigger
	callers        []string
}

// catalogueTrigger is one trigger of the schema: its table, its name, the events it fires on and
// the function it runs.
type catalogueTrigger struct {
	table, name, function string
	events                []string
}

// readCatalogueWriters reads pg_proc on database, with pg_trigger and pg_class beside it: every
// function body a migration installed, each that writes a projection with the triggers that run
// it and the functions that call it, and every trigger.
func readCatalogueWriters(t *testing.T, ctx context.Context, database *store.Store, projections map[string]*projection) ([]catalogueWriter, []catalogueTrigger, int) {
	t.Helper()
	writes := projectionWrites(slices.Sorted(maps.Keys(projections)))
	rows, err := database.Pool.Query(ctx, `
		select p.proname, p.prosrc
		from pg_proc p
		join pg_namespace n on n.oid = p.pronamespace
		join pg_language l on l.oid = p.prolang
		where n.nspname = current_schema() and l.lanname not in ('internal', 'c')
		order by p.proname`)
	if err != nil {
		t.Fatalf("read pg_proc: %v", err)
	}
	bodies := map[string]string{}
	for rows.Next() {
		var name, body string
		if err := rows.Scan(&name, &body); err != nil {
			t.Fatal(err)
		}
		bodies[name] += body
	}
	rows.Close()
	triggerRows, err := database.Pool.Query(ctx, `
		select c.relname, t.tgname, p.proname, t.tgtype
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_namespace n on n.oid = c.relnamespace
		join pg_proc p on p.oid = t.tgfoid
		where not t.tgisinternal and n.nspname = current_schema()
		order by c.relname, t.tgname`)
	if err != nil {
		t.Fatalf("read pg_trigger: %v", err)
	}
	var triggers []catalogueTrigger
	for triggerRows.Next() {
		var trigger catalogueTrigger
		var kind int16
		if err := triggerRows.Scan(&trigger.table, &trigger.name, &trigger.function, &kind); err != nil {
			t.Fatal(err)
		}
		// pg_trigger.tgtype: bit 2 insert, bit 3 delete, bit 4 update, bit 5 truncate.
		for bit, event := range map[int16]string{4: "insert", 8: "delete", 16: "update", 32: "truncate"} {
			if kind&bit != 0 {
				trigger.events = append(trigger.events, event)
			}
		}
		slices.Sort(trigger.events)
		triggers = append(triggers, trigger)
	}
	triggerRows.Close()
	var writers []catalogueWriter
	for _, name := range slices.Sorted(maps.Keys(bodies)) {
		body := withoutLineComments(bodies[name])
		for _, match := range writes.FindAllStringSubmatchIndex(body, -1) {
			writer := catalogueWriter{function: name, body: bodies[name], table: strings.ToLower(body[match[4]:match[5]]),
				verb: strings.ToLower(strings.Join(strings.Fields(body[match[2]:match[3]]), " ")), held: true}
			if writer.verb == "update" {
				writer.held, writer.verb = updateHeld(projections[writer.table], body[match[5]:])
			}
			for _, trigger := range triggers {
				if trigger.function == name {
					writer.triggers = append(writer.triggers, trigger)
				}
			}
			calls := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\s*\(`)
			for _, caller := range slices.Sorted(maps.Keys(bodies)) {
				if caller != name && calls.MatchString(withoutLineComments(bodies[caller])) {
					writer.callers = append(writer.callers, caller)
				}
			}
			writers = append(writers, writer)
		}
	}
	return writers, triggers, len(bodies)
}

// withoutLineComments drops a SQL body's line comments, so a statement written in prose is none.
func withoutLineComments(body string) string {
	lines := strings.Split(body, "\n")
	for index, line := range lines {
		if at := strings.Index(line, "--"); at >= 0 {
			lines[index] = line[:at]
		}
	}
	return strings.Join(lines, "\n")
}

// keptStoreWriters is the one write of the store outside graphstore that a database holds while
// the boot's move of the priority store and the workflow store into it stands for the release
// before (dispatch://INFRA-1098's R32, storesmove): the four names of the old tables as views over
// graph_nodes and graph_tuples, which PostgreSQL updates itself, so the release before reads and
// writes its priorities, runs and plans through them, and the two triggers on graph_nodes with the
// functions of their names (graphstore.StoresMovedTriggers), which complete a run or a plan it
// writes. They are kept for one release under R22's first condition, and the boot step drops them
// once the newest announcement of another build lists stores_moved (startup.StoresMovedRetirement),
// so a database the step ran over holds none. Each is named as readStoreWritersOutsideGraphstore
// names it.
var keptStoreWriters = []string{
	"the function graph_nodes_workflow_state(), which writes graph_tuples",
	"the trigger graph_nodes_workflow_project on graph_nodes, before each update, sets the row it runs for (graph_nodes_workflow_project())",
	"the trigger graph_nodes_workflow_state on graph_nodes, after each insert, runs graph_nodes_workflow_state(), which writes graph_tuples",
	"the view priority_nodes, which PostgreSQL updates as graph_nodes",
	"the view priority_tuples, which PostgreSQL updates as graph_tuples",
	"the view workflow_nodes, which PostgreSQL updates as graph_nodes",
	"the view workflow_tuples, which PostgreSQL updates as graph_tuples",
}

// readStoreWritersOutsideGraphstore reads the catalogue of database for every write of the
// store's two tables that no statement of graphstore issues, sorted: each view over graph_nodes or
// graph_tuples that PostgreSQL can update itself, each function whose body writes either table,
// and each trigger on either table that sets the row it runs for (before each row) or runs such a
// function.
func readStoreWritersOutsideGraphstore(t *testing.T, ctx context.Context, database *store.Store) []string {
	t.Helper()
	var found []string
	views, err := database.Pool.Query(ctx, `
		select distinct v.relname::text, base.relname::text
		from pg_class v
		join pg_rewrite r on r.ev_class = v.oid
		join pg_depend d on d.classid = 'pg_rewrite'::regclass and d.objid = r.oid and d.refclassid = 'pg_class'::regclass
		join pg_class base on base.oid = d.refobjid and base.oid <> v.oid
		where v.relkind = 'v' and v.relnamespace = current_schema()::regnamespace
		  and base.relname in ('graph_nodes', 'graph_tuples') and pg_relation_is_updatable(v.oid, false) <> 0`)
	if err != nil {
		t.Fatalf("read the views over the store: %v", err)
	}
	for views.Next() {
		var view, base string
		if err := views.Scan(&view, &base); err != nil {
			t.Fatal(err)
		}
		found = append(found, "the view "+view+", which PostgreSQL updates as "+base)
	}
	views.Close()
	writes := projectionWrites(storeTables)
	bodies, err := database.Pool.Query(ctx, `
		select p.proname::text, p.prosrc from pg_proc p
		join pg_language l on l.oid = p.prolang
		where p.pronamespace = current_schema()::regnamespace and l.lanname not in ('internal', 'c')`)
	if err != nil {
		t.Fatalf("read pg_proc: %v", err)
	}
	written := map[string]string{}
	for bodies.Next() {
		var name, body string
		if err := bodies.Scan(&name, &body); err != nil {
			t.Fatal(err)
		}
		if match := writes.FindStringSubmatch(withoutLineComments(body)); match != nil {
			written[name] = strings.ToLower(match[2])
			found = append(found, "the function "+name+"(), which writes "+written[name])
		}
	}
	bodies.Close()
	triggers, err := database.Pool.Query(ctx, `
		select t.tgname::text, c.relname::text, p.proname::text, t.tgtype
		from pg_trigger t
		join pg_class c on c.oid = t.tgrelid
		join pg_proc p on p.oid = t.tgfoid
		where not t.tgisinternal and c.relname in ('graph_nodes', 'graph_tuples') and c.relnamespace = current_schema()::regnamespace`)
	if err != nil {
		t.Fatalf("read the triggers of the store: %v", err)
	}
	for triggers.Next() {
		var trigger, table, function string
		var kind int16
		if err := triggers.Scan(&trigger, &table, &function, &kind); err != nil {
			t.Fatal(err)
		}
		// pg_trigger.tgtype: bit 0 for each row, bit 1 before, then insert, delete and update.
		var events []string
		for _, event := range []struct {
			bit  int16
			name string
		}{{4, "insert"}, {8, "delete"}, {16, "update"}} {
			if kind&event.bit != 0 {
				events = append(events, event.name)
			}
		}
		if kind&3 == 3 {
			found = append(found, "the trigger "+trigger+" on "+table+", before each "+strings.Join(events, " or ")+", sets the row it runs for ("+function+"())")
		} else if target, writer := written[function]; writer {
			found = append(found, "the trigger "+trigger+" on "+table+", after each "+strings.Join(events, " or ")+", runs "+function+"(), which writes "+target)
		}
	}
	triggers.Close()
	slices.Sort(found)
	return found
}

// requireTheStoreWritersOutsideGraphstore holds database's writers of the store outside graphstore
// (readStoreWritersOutsideGraphstore) to want, printing each.
func requireTheStoreWritersOutsideGraphstore(t *testing.T, ctx context.Context, database *store.Store, what string, want []string) {
	t.Helper()
	found := readStoreWritersOutsideGraphstore(t, ctx, database)
	t.Logf("%s holds %d writers of the store outside graphstore:\n  %s", what, len(found), strings.Join(found, "\n  "))
	for _, writer := range found {
		if !slices.Contains(want, writer) {
			t.Errorf("%s holds %s, a write of the store outside graphstore that is no part of the release before's path through the stores' move", what, writer)
		}
	}
	for _, writer := range want {
		if !slices.Contains(found, writer) {
			t.Errorf("%s does not hold %s, which the stores' move keeps for the release before", what, writer)
		}
	}
}

// movedStores runs the boot's move of the priority store and the workflow store over database
// (storesmove.Move), which swaps the four tables for the views and creates the two triggers.
func movedStores(t *testing.T, ctx context.Context, database *store.Store) *store.Store {
	t.Helper()
	if result, err := storesmove.Move(ctx, database.Pool.Config().ConnString(), storesmove.Options{}); err != nil || !result.Moved {
		t.Fatalf("move the stores: moved=%v, %v; want them moved", result != nil && result.Moved, err)
	}
	return database
}

// --- the behavioural half --------------------------------------------------------------------

// injectionObjects are the test's own objects in a behavioural database, none of them a
// migration's: the point at which a write is failed, the trigger that fails a projection's write
// between the store write and it, the deferred trigger that fails the store's transaction once
// every write of it has run, and the log each projection's and the store's writes leave, which
// tells which transaction wrote what. They are installed in the behavioural databases alone, and
// the inventory reads another.
const injectionObjects = `
create table one_writer_point (point text primary key);
create table one_writer_log (
	seq bigserial primary key, tbl text not null, op text not null, depth int not null,
	txid bigint not null, application text not null, query text not null);
create function one_writer_fail_projection() returns trigger language plpgsql as $$
begin
	if exists (select 1 from one_writer_point where point = 'between') then
		raise exception 'one_writer: a failure injected between the store write and the write of %', tg_table_name;
	end if;
	return coalesce(new, old);
end $$;
create function one_writer_fail_commit() returns trigger language plpgsql as $$
begin
	if exists (select 1 from one_writer_point where point = 'after') then
		raise exception 'one_writer: a failure injected after the projection write, at the commit of the store write to %', tg_table_name;
	end if;
	return null;
end $$;
create function one_writer_logged() returns trigger language plpgsql as $$
declare
	stated text[] := tg_argv;
	moved boolean := tg_op <> 'UPDATE';
	column_name text;
begin
	if tg_op = 'UPDATE' then
		if array_length(stated, 1) is null then
			moved := true;
		else
			foreach column_name in array stated loop
				if (to_jsonb(new) -> column_name) is distinct from (to_jsonb(old) -> column_name) then
					moved := true;
				end if;
			end loop;
		end if;
	end if;
	if moved then
		insert into one_writer_log (tbl, op, depth, txid, application, query)
		values (tg_table_name, lower(tg_op), pg_trigger_depth(), txid_current(), current_setting('application_name'), current_query());
	end if;
	return null;
end $$;
`

// oneWriterRig is the behavioural half's server: the API as cmd/dispatch builds it and the
// document service beside it (writersRig) on a database whose pool reports every statement to the
// test's tracer, with the injection objects installed, the projections it holds, the tables whose
// triggers run a writer of the catalogue (logged), and the other databases a case writes on its own.
type oneWriterRig struct {
	*writersRig
	trace       *writerTrace
	projections map[string]*projection
	logged      []string
	digested    []string
	others      map[string]*store.Store
	listeners   map[*store.Store]func() []string
	application string
	// top is the whole test, which the databases and listeners a case makes outlive the case for,
	// as the census and the two writers at once read them after it.
	top *testing.T
}

// newOneWriterRig builds the rig and installs the injection objects on every projection, on the
// store's two tables and on logged.
func newOneWriterRig(t *testing.T, projections map[string]*projection, logged []string) *oneWriterRig {
	t.Helper()
	root, err := filepath.Abs(oneWriterModule)
	if err != nil {
		t.Fatal(err)
	}
	trace := &writerTrace{root: root, writes: projectionWrites(slices.Sorted(maps.Keys(projections)))}
	database := storetest.TracedStore(t, storetest.Open(t), trace)
	rig := &oneWriterRig{trace: trace, projections: projections, logged: logged, top: t,
		others: map[string]*store.Store{}, listeners: map[*store.Store]func() []string{}}
	installInjection(t, database, projections, logged)
	rig.writersRig = newWritersRigOn(t, database)
	// The event log is read by the events a failed run committed (failureReports), not by its
	// digest: an event that reports a failure is no notification of the write.
	rig.digested = slices.Concat(storeTables, slices.Sorted(maps.Keys(projections)))
	if err := database.Pool.QueryRow(context.Background(), `select current_setting('application_name')`).Scan(&rig.application); err != nil {
		t.Fatalf("read the rig's application name: %v", err)
	}
	return rig
}

// storeOf is the database a case writes: the rig's own, or one of its own named on, made once - a
// fresh one for every run of a case whose writer refuses a database it wrote before (fresh).
func (r *oneWriterRig) storeOf(t *testing.T, on string) *store.Store {
	t.Helper()
	if on == "" {
		return r.database
	}
	if held, ok := r.others[on]; ok {
		return held
	}
	database := storetest.TracedStore(r.top, storetest.Open(r.top), r.trace)
	installInjection(t, database, r.projections, r.logged)
	r.others[on] = database
	return database
}

// installInjection installs the injection objects on database: the failure and the log on every
// projection, the deferred failure and the log on the store's tables, and the log alone on logged.
func installInjection(t *testing.T, database *store.Store, projections map[string]*projection, logged []string) {
	t.Helper()
	ctx := context.Background()
	statements := []string{injectionObjects}
	for _, table := range slices.Sorted(maps.Keys(projections)) {
		held := projections[table]
		var arguments []string
		if !held.whole {
			for _, column := range held.columns {
				arguments = append(arguments, "'"+column+"'")
			}
		}
		if table == storesMovedRecord {
			var exists bool
			if err := database.Pool.QueryRow(ctx, `select to_regclass($1) is not null`, table).Scan(&exists); err != nil {
				t.Fatalf("read whether %s stands: %v", table, err)
			}
			if !exists {
				continue
			}
		}
		statements = append(statements,
			`create trigger one_writer_fail_projection before insert or update or delete on `+table+` for each row execute function one_writer_fail_projection()`,
			`create trigger one_writer_logged after insert or update or delete on `+table+` for each row execute function one_writer_logged(`+strings.Join(arguments, ", ")+`)`)
	}
	for _, table := range storeTables {
		statements = append(statements,
			`create constraint trigger one_writer_fail_commit after insert or update or delete on `+table+` deferrable initially deferred for each row execute function one_writer_fail_commit()`,
			`create trigger one_writer_logged after insert or update or delete on `+table+` for each row execute function one_writer_logged()`)
	}
	for _, table := range logged {
		statements = append(statements, `create trigger one_writer_logged after insert or update or delete on `+table+` for each row execute function one_writer_logged()`)
	}
	for _, statement := range statements {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("install the injection objects: %v\n%s", err, statement)
		}
	}
}

// writerTrace is the tracer on the behavioural databases' pools: while a case records, it keeps,
// for every statement that writes a projection, the functions of the module's own sources on the
// stack of the goroutine that issued it, and the statement's text.
type writerTrace struct {
	root   string
	writes *regexp.Regexp
	mu     sync.Mutex
	record *caseRecord
}

// caseRecord is what one run of a case issued: each projection write, with its text and the
// functions on its stack.
type caseRecord struct {
	statements []tracedWrite
}

// tracedWrite is one projection write a case issued.
type tracedWrite struct {
	table, text string
	functions   map[string]bool
}

func (w *writerTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	w.saw(data.SQL)
	return ctx
}

func (w *writerTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (w *writerTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (w *writerTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	w.saw(data.SQL)
}

func (w *writerTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// saw records one statement for the case recording, when it writes a projection.
func (w *writerTrace) saw(sql string) {
	w.mu.Lock()
	record := w.record
	w.mu.Unlock()
	if record == nil {
		return
	}
	matches := w.writes.FindAllStringSubmatch(sql, -1)
	if len(matches) == 0 {
		return
	}
	functions := map[string]bool{}
	pcs := make([]uintptr, 128)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasPrefix(frame.File, w.root+string(filepath.Separator)) && !strings.HasSuffix(frame.File, "_test.go") {
			relative, _ := filepath.Rel(w.root, frame.File)
			functions[filepath.ToSlash(relative)+"#"+frameFunction(frame.Function)] = true
		}
		if !more {
			break
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, match := range matches {
		record.statements = append(record.statements, tracedWrite{table: strings.ToLower(match[2]), text: foldedStatement(sql), functions: functions})
	}
}

// frameFunction is a frame's function as the inventory names one: without its package, its
// receiver, the closures inside it and the type parameters it was instantiated with.
func frameFunction(name string) string {
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if bracket := strings.Index(name, "["); bracket >= 0 {
		if end := strings.LastIndex(name, "]"); end > bracket {
			name = name[:bracket] + name[end+1:]
		}
	}
	for _, part := range strings.Split(name, ".")[1:] {
		closure := strings.HasPrefix(part, "func") && strings.TrimLeft(part[4:], "0123456789") == ""
		if strings.HasPrefix(part, "(") || closure || part == "deferwrap1" || strings.TrimLeft(part, "0123456789") == "" {
			continue
		}
		// A method's receiver is passed over above; the function is the first name left.
		return part
	}
	return name
}

// start begins recording a case's statements.
func (w *writerTrace) start() *caseRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record = &caseRecord{}
	return w.record
}

// stop ends the recording.
func (w *writerTrace) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record = nil
}

// writerCase is one writer of a kind as the behavioural half drives it: what it needs written
// first, the write itself, which answers its failure rather than failing the test, and, for a
// write some of whose work runs after the drive answers, the wait for that work.
type writerCase struct {
	name string
	// tables are the projections the case writes, which its plain run must write beside a store
	// write in one transaction.
	tables []string
	// on names a database of the case's own, "" for the rig's.
	on string
	// fresh gives every run of the case a database of its own, for a writer that refuses a
	// database it wrote before.
	fresh bool
	ready func(t *testing.T, r *oneWriterRig, database *store.Store)
	// each writes what every run needs afresh before it, where a run's write uses up what it
	// writes over: an unread thread a read mark marks.
	each  func(t *testing.T, r *oneWriterRig, database *store.Store)
	drive func(t *testing.T, r *oneWriterRig, database *store.Store) error
	// settle waits for the work the drive left running and answers whether it ended as a plain
	// run ends; nil for a drive whose work is over when it answers.
	settle func(t *testing.T, r *oneWriterRig, database *store.Store, within time.Duration) bool
	// again drives the writer once more over rows of its own, as two writers at once run it; nil
	// for a case no second run of which writes anything.
	again func(t *testing.T, r *oneWriterRig, database *store.Store) error
	// batches says the writer commits in transactions of its own, one batch after another: a
	// failure leaves the batches before it, each of which must hold its store write beside every
	// projection row it wrote, and the census must read no difference it did not read before.
	batches bool
	// process is the directory of the module, from packages/envoy, whose writers the case's run
	// stands for where they run in a process or on a connection the tracer does not see: the seed.
	process string
	// storeOnly says the writer writes a kind no table projects, one the move of
	// dispatch://INFRA-1098's R32 took into the store: no failure between its store write and a
	// projection write exists, so its runs inject the failure at the store write's commit alone,
	// and its plain run must write the store.
	storeOnly bool
	// storeUnwritten says the writer writes no node and no tuple, and of a projection only columns
	// the store does not state: no store write stands for an injected failure to undo, so its runs
	// inject none, and its plain run must leave graph_nodes and graph_tuples as they were, the same
	// rows counted and digested. A projection row it writes stands beside no store write, which
	// fails it.
	storeUnwritten bool
	// drives are the writers, by their inventory key, whose statement names its table with a
	// runtime value, so neither a stack nor a statement's text names them: the case drives each,
	// which its run's write of the writer's table stands for.
	drives []string
	// check reads what the plain run left once it is recorded, and fails the case on what it finds;
	// nil for a case the reads above hold whole.
	check func(t *testing.T, r *oneWriterRig, database *store.Store)
}

// caseRun is what one case's plain run left: its traced writes and the log of what its
// transactions wrote.
type caseRun struct {
	name, process string
	record        *caseRecord
	log           []loggedWrite
	application   string
	// own says the run wrote a database of the case's own, which nothing else writes.
	own    bool
	drives []string
}

// loggedWrite is one row of one_writer_log.
type loggedWrite struct {
	seq                    int64
	table, op, application string
	depth                  int
	txid                   int64
	query                  string
}

// digest is every digested table's row count and the digest of its rows on database.
func (r *oneWriterRig) digest(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	digests := map[string]string{}
	ctx := context.Background()
	for _, table := range r.digested {
		if table == storesMovedRecord {
			var exists bool
			if err := database.Pool.QueryRow(ctx, `select to_regclass($1) is not null`, table).Scan(&exists); err != nil {
				t.Fatalf("read whether %s stands: %v", table, err)
			}
			if !exists {
				continue
			}
		}
		var count int64
		var sum string
		if err := database.Pool.QueryRow(ctx, `select count(*), coalesce(md5(string_agg(md5(x::text), ',' order by md5(x::text))), '') from `+table+` x`).
			Scan(&count, &sum); err != nil {
			t.Fatalf("digest %s: %v", table, err)
		}
		digests[table] = fmt.Sprintf("%d rows, %s", count, sum)
	}
	return digests
}

// changedTables is every digested table whose digest after differs from before.
func (r *oneWriterRig) changedTables(before, after map[string]string) []string {
	var changed []string
	for _, table := range r.digested {
		if before[table] != after[table] {
			changed = append(changed, fmt.Sprintf("%s: %s before, %s after", table, before[table], after[table]))
		}
	}
	return changed
}

// setPoint sets the injection point on database, "" for none.
func setPoint(t *testing.T, database *store.Store, point string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from one_writer_point`); err != nil {
		t.Fatalf("clear the injection point: %v", err)
	}
	if point != "" {
		if _, err := database.Pool.Exec(ctx, `insert into one_writer_point (point) values ($1)`, point); err != nil {
			t.Fatalf("set the injection point %s: %v", point, err)
		}
	}
}

// logSince is every row of database's one_writer_log after seq.
func logSince(t *testing.T, database *store.Store, seq int64) []loggedWrite {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select seq, tbl, op, depth, txid, application, query from one_writer_log where seq > $1 order by seq`, seq)
	if err != nil {
		t.Fatalf("read the write log: %v", err)
	}
	defer rows.Close()
	var logged []loggedWrite
	for rows.Next() {
		var row loggedWrite
		if err := rows.Scan(&row.seq, &row.table, &row.op, &row.depth, &row.txid, &row.application, &row.query); err != nil {
			t.Fatal(err)
		}
		row.query = foldedStatement(row.query)
		logged = append(logged, row)
	}
	return logged
}

// logTop is the newest seq of database's one_writer_log.
func logTop(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var top int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(seq), 0) from one_writer_log`).Scan(&top); err != nil {
		t.Fatalf("read the write log's top: %v", err)
	}
	return top
}

// listenOn listens on every channel the schema of database notifies on, once, and answers a
// function that drains and answers what arrived since.
func (r *oneWriterRig) listenOn(t *testing.T, database *store.Store) func() []string {
	t.Helper()
	if drain, held := r.listeners[database]; held {
		return drain
	}
	ctx := context.Background()
	var channels []string
	rows, err := database.Pool.Query(ctx, `
		select distinct substring(prosrc from 'pg_notify\(\s*''([a-z_]+)''') from pg_proc
		where pronamespace = current_schema()::regnamespace and prosrc ~ 'pg_notify\(\s*'''`)
	if err != nil {
		t.Fatalf("read the channels the schema notifies on: %v", err)
	}
	for rows.Next() {
		var channel *string
		if err := rows.Scan(&channel); err != nil {
			t.Fatal(err)
		}
		if channel != nil {
			channels = append(channels, *channel)
		}
	}
	rows.Close()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the listening connection: %v", err)
	}
	r.top.Cleanup(conn.Release)
	for _, channel := range channels {
		if _, err := conn.Exec(ctx, `listen `+pgx.Identifier{channel}.Sanitize()); err != nil {
			t.Fatalf("listen on %s: %v", channel, err)
		}
	}
	t.Logf("the behavioural half listens on the %d channels the schema notifies on: %v", len(channels), channels)
	drain := func() []string {
		var arrived []string
		for {
			waited, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			notification, err := conn.Conn().WaitForNotification(waited)
			cancel()
			if err != nil {
				return arrived
			}
			arrived = append(arrived, notification.Channel+": "+notification.Payload)
		}
	}
	r.listeners[database] = drain
	return drain
}

// runOnce drives c once over database and answers its failure: the drive's own, or a settle that
// did not end as a plain run ends within within.
func runOnce(t *testing.T, r *oneWriterRig, c writerCase, database *store.Store, within time.Duration) error {
	t.Helper()
	err := c.drive(t, r, database)
	if err == nil && c.settle != nil && !c.settle(t, r, database, within) {
		err = fmt.Errorf("its work did not end as a plain run ends within %s", within)
	}
	return err
}

// runCase runs one case: a failure injected between the store write and the projection write,
// one injected after the projection write, and then the plain run, recorded. After each failure
// it reads that nothing the write would have written stands - no node, no tuple, no projection
// row and no event but one that reports the failure - and that no event's notification arrived;
// for a writer that commits in batches, that each batch it committed holds its store write beside
// every projection row it wrote and the census reads no difference the batches did not find. A
// writer that writes no node and no tuple (storeUnwritten) has no failure injected, and its plain
// run must leave the store's rows as they were. The case's own check reads the plain run last.
func (r *oneWriterRig) runCase(t *testing.T, c writerCase) caseRun {
	t.Helper()
	run := caseRun{name: c.name, process: c.process, application: r.application, own: c.on != "", drives: c.drives}
	fixed := r.storeOf(t, c.on)
	database := func(attempt string) *store.Store {
		if c.fresh {
			return r.storeOf(t, c.on+"/"+attempt)
		}
		return fixed
	}
	readied := map[*store.Store]bool{}
	ready := func(database *store.Store) {
		if c.ready != nil && !readied[database] {
			readied[database] = true
			c.ready(t, r, database)
		}
		if c.each != nil {
			c.each(t, r, database)
		}
	}
	for _, injection := range []struct{ point, what string }{
		{"between", "a failure between the store write and the projection write"},
		{"after", "a failure after the projection write"},
	} {
		if c.storeUnwritten {
			t.Logf("%s writes no node and no tuple, so no store write stands for %s to undo, and none is injected", c.name, injection.what)
			continue
		}
		if c.storeOnly && injection.point == "between" {
			t.Logf("%s writes a kind no table projects, so no failure between its store write and a projection write exists to inject", c.name)
			continue
		}
		written := database(injection.point)
		ready(written)
		drain := r.listenOn(t, written)
		settleAll(t, written)
		drain()
		before, top, events := r.digest(t, written), logTop(t, written), eventTop(t, written)
		var differences map[string]int64
		if c.batches {
			differences = censusDifferences(t, written)
		}
		setPoint(t, written, injection.point)
		err := runOnce(t, r, c, written, 3*time.Second)
		setPoint(t, written, "")
		settleAll(t, written)
		what := c.name + ", with " + injection.what + " injected,"
		if err == nil {
			// The failure is injected at a write of a projection and at the commit of a write of
			// the store, so a run that ends untouched wrote neither inside a transaction that
			// wrote the store: what it wrote stands with nothing behind it.
			t.Errorf("%s ran to its end: the run wrote no projection row inside a transaction that wrote a node or a tuple, so no failure of the store's write could undo it", what)
			drain()
			continue
		}
		t.Logf("%s answered: %v", what, firstLineOf(err.Error()))
		if c.batches {
			committed := logSince(t, written, top)
			r.requireEveryProjectionWriteBesideAStoreWrite(t, what, committed, nil)
			for kind, count := range censusDifferences(t, written) {
				if count > differences[kind] {
					t.Errorf("%s left the census of %s at %d differences, where it read %d before the run", what, kind, count, differences[kind])
				}
			}
			t.Logf("%s left %d logged writes of the batches it committed before the failure", what, len(committed))
			continue
		}
		after := r.digest(t, written)
		if left := r.changedTables(before, after); len(left) != 0 {
			t.Errorf("%s left committed:\n  %s", what, strings.Join(left, "\n  "))
		}
		failures := failureReports(t, written, events, what)
		for _, arrived := range drain() {
			channel, _, _ := strings.Cut(arrived, ":")
			switch {
			case channel == eventsChannel && failures > 0:
				failures--
				t.Logf("%s notified %s, the publication of the event that reports its failure", what, arrived)
			case channel == eventsChannel:
				t.Errorf("%s published an event's notification: %s", what, arrived)
			default:
				t.Logf("%s woke the queue %s, which announces no write of the store or of a projection", what, arrived)
			}
		}
	}
	written := database("plain")
	ready(written)
	top := logTop(t, written)
	var stored map[string]string
	if c.storeUnwritten {
		stored = r.digest(t, written)
	}
	run.record = r.trace.start()
	err := runOnce(t, r, c, written, 60*time.Second)
	settleAll(t, written)
	r.trace.stop()
	if err != nil {
		t.Fatalf("%s failed with nothing injected: %v", c.name, err)
	}
	run.log = logSince(t, written, top)
	r.requireEveryProjectionWriteBesideAStoreWrite(t, c.name, run.log, c.tables)
	if c.storeOnly && !slices.ContainsFunc(run.log, func(row loggedWrite) bool { return slices.Contains(storeTables, row.table) }) {
		t.Errorf("%s wrote no node and no tuple", c.name)
	}
	if c.storeUnwritten {
		left := r.digest(t, written)
		for _, table := range storeTables {
			if left[table] != stored[table] {
				t.Errorf("%s changed %s: %s before the run, %s after it", c.name, table, stored[table], left[table])
				continue
			}
			t.Logf("%s left %s as it was: %s", c.name, table, left[table])
		}
		if slices.ContainsFunc(run.log, func(row loggedWrite) bool { return slices.Contains(storeTables, row.table) }) {
			t.Errorf("%s wrote a node or a tuple", c.name)
		}
	}
	if c.check != nil {
		c.check(t, r, written)
	}
	return run
}

// eventsChannel is the channel the event log's trigger notifies on, once per event committed.
const eventsChannel = "dispatch_events"

// eventTop is the newest id of database's event log.
func eventTop(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var top int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&top); err != nil {
		t.Fatalf("read the event log's top: %v", err)
	}
	return top
}

// failureReports reads the events a failed run committed after top: each must report the failure
// (a type ending in _failed), which is no notification of the write that failed, and it answers
// how many there are.
func failureReports(t *testing.T, database *store.Store, top int64, what string) int {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select type from events where id > $1 order by id`, top)
	if err != nil {
		t.Fatalf("read the events the run committed: %v", err)
	}
	defer rows.Close()
	reports := 0
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(kind, "_failed") {
			reports++
			t.Logf("%s committed the event %s, which reports the failure", what, kind)
			continue
		}
		t.Errorf("%s committed the event %s, a notification of the write that failed", what, kind)
	}
	return reports
}

// censusDifferences is how many differences the store census reads for each kind of database.
func censusDifferences(t *testing.T, database *store.Store) map[string]int64 {
	t.Helper()
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false, nil)
	if err != nil {
		t.Fatalf("run the census: %v", err)
	}
	differences := map[string]int64{}
	for _, kind := range report.Kinds {
		differences[kind.Kind] = kind.RowDifferences + kind.NodeDifferences
	}
	return differences
}

// settleAll waits until nothing but the test's own statement runs on database, so a write a
// request left running in the background has ended before its leftovers are read.
func settleAll(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var running int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and pid <> pg_backend_pid() and state in ('active', 'idle in transaction')`).Scan(&running); err != nil {
			t.Fatalf("read what runs on the database: %v", err)
		}
		if running == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireEveryProjectionWriteBesideAStoreWrite reads logged: every projection write it holds
// stands in a transaction that wrote the store, and the run wrote each of tables.
func (r *oneWriterRig) requireEveryProjectionWriteBesideAStoreWrite(t *testing.T, what string, logged []loggedWrite, tables []string) {
	t.Helper()
	stored := map[int64]bool{}
	for _, row := range logged {
		if slices.Contains(storeTables, row.table) {
			stored[row.txid] = true
		}
	}
	wrote := map[string]bool{}
	for _, row := range logged {
		if slices.Contains(storeTables, row.table) || slices.Contains(r.logged, row.table) {
			continue
		}
		wrote[row.table] = true
		if !stored[row.txid] {
			t.Errorf("%s wrote %s (%s, at trigger depth %d) in transaction %d, which wrote no node and no tuple: %s",
				what, row.table, row.op, row.depth, row.txid, row.query)
		}
	}
	for _, table := range tables {
		if !wrote[table] {
			t.Errorf("%s wrote no row of %s, a projection it names", what, table)
		}
	}
	if tables != nil {
		t.Logf("%s wrote %v, %d logged writes in %d transactions that wrote the store", what, slices.Sorted(maps.Keys(wrote)), len(logged), len(stored))
	}
}

// firstLineOf is the first line of text.
func firstLineOf(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// send sends one request and answers its failure: an error for any answer but want, or for any
// answer that is no success where want is 0.
func (r *oneWriterRig) send(method, target string, body any, login string, want int) ([]byte, error) {
	status, answered := r.serve(method, target, body, login, "application/json")
	if status != want && (want != 0 || status < 200 || status >= 300) {
		return answered, fmt.Errorf("%s %s: status=%d body=%s", method, target, status, firstLineOf(string(answered)))
	}
	return answered, nil
}

// serve sends one request to the rig's server as writersRig.request sends it - as the person
// login names, or under the shared agent bearer with no login - and answers what it answered,
// failing nothing: a write under an injected failure answers its refusal. A body of bytes is sent
// as it is, under contentType; any other is encoded as JSON.
func (r *oneWriterRig) serve(method, target string, body any, login, contentType string) (int, []byte) {
	encoded, raw := body.([]byte)
	if !raw {
		encoded, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", contentType)
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	} else {
		request.Header.Set("Authorization", "Bearer agent-token")
	}
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

// --- the test ----------------------------------------------------------------------------------

// TestEveryGraphKindHasOneWriter is c-one-writer. It prints which database its inventory read and,
// for each writer the inventory finds, the case that drove it.
func TestEveryGraphKindHasOneWriter(t *testing.T) {
	ctx := context.Background()
	inventoryDatabase := movedStores(t, ctx, storetest.Open(t))
	var name, version string
	var migrations int
	if err := inventoryDatabase.Pool.QueryRow(ctx, `
		select current_database(), current_setting('server_version'), (select count(*) from schema_migrations)`).Scan(&name, &version, &migrations); err != nil {
		t.Fatalf("read which database the inventory reads: %v", err)
	}
	t.Logf("the inventory reads the database %s on Postgres %s: a clone of the template this tree's migrations wrote (storetest.Open), %d migrations applied, "+
		"which the boot's move of the priority store and the workflow store then swapped for its views and triggers (storesmove.Move), and no object of the test's own",
		name, version, migrations)

	projections := readProjections(t, ctx, inventoryDatabase)
	sources := readSourceWriters(t, projections)
	catalogue, triggers, bodies := readCatalogueWriters(t, ctx, inventoryDatabase, projections)
	// The tables whose triggers run a catalogue writer of a projection, the writer's own or a
	// caller's, whose writes the log keeps beside the projection row the trigger writes.
	var logged []string
	for _, writer := range catalogue {
		for _, trigger := range triggers {
			if trigger.function != writer.function && !slices.Contains(writer.callers, trigger.function) {
				continue
			}
			if _, projected := projections[trigger.table]; !projected && !slices.Contains(logged, trigger.table) {
				logged = append(logged, trigger.table)
			}
		}
	}
	slices.Sort(logged)

	rig := newOneWriterRig(t, projections, logged)
	var behaviourName string
	if err := rig.database.Pool.QueryRow(ctx, `select current_database()`).Scan(&behaviourName); err != nil {
		t.Fatal(err)
	}
	t.Logf("the behavioural half writes the database %s, another clone, holding the injection objects, and logs %v beside the projections", behaviourName, logged)
	runs := map[string]caseRun{}
	cases := oneWriterCases()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runs[c.name] = rig.runCase(t, c)
		})
	}

	t.Run("the one write of the store outside graphstore is the release before's, through the move's views and triggers, until the boot step drops them", func(t *testing.T) {
		requireTheStoreWritersOutsideGraphstore(t, ctx, inventoryDatabase, "the inventory's database "+name, keptStoreWriters)
		dropped := movedStores(t, ctx, storetest.Open(t))
		var logged bytes.Buffer
		startup.Retire(ctx, dropped.Pool, "0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e", true, slog.New(slog.NewTextHandler(&logged, nil)),
			[]startup.Retirement{startup.StoresMovedRetirement})
		var droppedName string
		if err := dropped.Pool.QueryRow(ctx, `select current_database()`).Scan(&droppedName); err != nil {
			t.Fatal(err)
		}
		t.Logf("the database %s, another clone the move swapped, then took the boot step at a boot that created it from empty, which logged:\n%s", droppedName, logged.String())
		requireTheStoreWritersOutsideGraphstore(t, ctx, dropped, "the database "+droppedName+" after the boot step", nil)
	})
	t.Run("every writer the inventory finds in the sources was driven", func(t *testing.T) {
		requireEverySourceWriterDriven(t, sources, runs)
	})
	t.Run("the boot's move is counted as a driven writer of priority, step and plan", func(t *testing.T) {
		requireTheStoresMoveDrivenForEveryKind(t, sources, runs)
	})
	t.Run("every function body and trigger the migrations installed that writes a projection writes it beside the store", func(t *testing.T) {
		requireEveryCatalogueWriterDriven(t, catalogue, triggers, bodies, runs)
	})
	t.Run("every read of a projection answers from one whose store write a case points at", func(t *testing.T) {
		requireEveryReadPointsAtAStoreWrite(t, sources, projections, runs)
	})
	t.Run("no script reconciles a projection after the fact", func(t *testing.T) {
		requireNoReconcilingScript(t, projections)
	})
	t.Run("two writers of one kind at once leave the census at zero", func(t *testing.T) {
		rig.requireTwoWritersAtOnce(t, cases)
	})
	t.Run("two moves of the priority store and the workflow store at once write each fact once", func(t *testing.T) {
		requireTwoStoresMovesAtOnce(t)
	})
	t.Run("the handoff kind is driven where this head holds its writer", func(t *testing.T) {
		requireTheHandoffWriter(t, ctx, inventoryDatabase, cases)
	})
}

// specific reports whether text says more than a writer's head, so a statement that holds it is
// that writer's and not any write of its table: a column list, or a where clause. The head is the
// verb's word and the table, not the verb as the scan names it, which holds an update's set list.
func specific(writer sourceWriter, text string) bool {
	verb, _, _ := strings.Cut(writer.verb, " ")
	if writer.verb == "insert into" || writer.verb == "delete from" {
		verb = writer.verb
	}
	return len(text) >= len(verb)+len(writer.table)+14
}

// drivenBy answers the case that drove writer, and how it was read: a statement of its table whose
// stack held the writer's function; one whose text holds the writer's own statement - its head up
// to the first parameter, or its whole literal where only that says which writer it is - as the
// tracer or the database's log saw it; or a write of its table by a process the case stands for
// (writerCase.process). A statement a write phase queued runs after its writer returned, so its
// stack names the phase and only its text names the writer.
func drivenBy(writer sourceWriter, runs map[string]caseRun) (string, string) {
	names := slices.Sorted(maps.Keys(runs))
	for _, name := range names {
		if record := runs[name].record; record != nil {
			for _, statement := range record.statements {
				if statement.table == writer.table && statement.functions[writer.key] {
					return name, "a write of " + writer.table + " issued with " + writer.function + " on its stack"
				}
			}
		}
	}
	for _, text := range []string{writer.text, writer.whole} {
		if !specific(writer, text) {
			continue
		}
		for _, name := range names {
			if record := runs[name].record; record != nil {
				for _, statement := range record.statements {
					if statement.table == writer.table && strings.Contains(statement.text, text) {
						return name, "a write of " + writer.table + " whose statement holds " + writer.function + "'s, as the tracer saw it"
					}
				}
			}
			for _, row := range runs[name].log {
				if row.table == writer.table && strings.Contains(row.query, text) {
					return name, "a write of " + writer.table + " whose statement holds " + writer.function + "'s, as the database logged it"
				}
			}
		}
	}
	for _, name := range names {
		run := runs[name]
		if run.process == "" || !strings.HasPrefix(writer.file, run.process) {
			continue
		}
		for _, row := range run.log {
			if row.table == writer.table && (run.own || row.application != run.application) {
				return name, "a write of " + writer.table + " by " + run.process + ", which runs on a connection of its own (application " + strconv.Quote(row.application) + ")"
			}
		}
	}
	for _, name := range names {
		run := runs[name]
		if !slices.Contains(run.drives, writer.key) {
			continue
		}
		written := false
		for _, row := range run.log {
			written = written || row.table == writer.table
		}
		for _, statement := range run.record.statements {
			written = written || statement.table == writer.table
		}
		if written {
			return name, "the case drives it, its statement's table a runtime value, and its run wrote " + writer.table
		}
	}
	return "", ""
}

// requireEverySourceWriterDriven prints, for each writer the scan of the sources found, the case
// that drove it, and fails on one no case drove, and on each of cutPackage's, naming it as leaving
// with that cut. It prints the writes the store need not hold, and the test-support packages it
// read past.
func requireEverySourceWriterDriven(t *testing.T, sources sourceInventory, runs map[string]caseRun) {
	t.Logf("the scan read %d Go files of packages/envoy and found %d writes of a projection the store must hold, %d it need not, and %d whose table a runtime value names",
		sources.files, len(sources.writers), len(sources.unheld), len(sources.runtime))
	for _, file := range slices.Sorted(maps.Keys(sources.skipped)) {
		t.Logf("read past %s: a test-support package, which no deployment builds, holding %d writes of a projection", file, sources.skipped[file])
	}
	for _, writer := range sources.unheld {
		t.Logf("not a write the store holds: %s:%d %s %s: %s", writer.file, writer.line, writer.function, writer.table, writer.verb)
	}
	if len(sources.writers) == 0 {
		t.Fatal("the scan found no writer of a projection, so no writer was held to a case")
	}
	undriven := 0
	for _, writer := range sources.writers {
		if strings.HasPrefix(writer.file, cutPackage) {
			undriven++
			t.Errorf("no case drives %s (%s %s, %s:%d): it leaves with dispatch://INFRA-1149's cut (5a51d205), which deletes %s", writer.key, writer.verb, writer.table, writer.file, writer.line, cutPackage)
			continue
		}
		name, how := drivenBy(writer, runs)
		if name == "" {
			undriven++
			t.Errorf("no case drove %s (%s %s, %s:%d): add a failure-injection case that runs it", writer.key, writer.verb, writer.table, writer.file, writer.line)
			t.Logf("  its statement, as the attribution looks for it: %s", writer.whole)
			for _, near := range nearestWrites(writer, runs) {
				t.Logf("  a write of %s a case ran: %s", writer.table, near)
			}
			continue
		}
		t.Logf("driven: %s (%s %s) by the case %q, read as %s", writer.key, writer.verb, writer.table, name, how)
	}
	t.Logf("%d of %d writers were driven", len(sources.writers)-undriven, len(sources.writers))
}

// requireTheStoresMoveDrivenForEveryKind holds the inventory and the behavioural half to driving
// the boot's move (internal/dispatch/storesmove/copy.go#writeBatch) for each of the three kinds
// R32 moves into the store: priority, step and plan.
func requireTheStoresMoveDrivenForEveryKind(t *testing.T, sources sourceInventory, runs map[string]caseRun) {
	const moveWriterKey = "internal/dispatch/storesmove/copy.go#writeBatch"
	var found []sourceWriter
	for _, writer := range sources.writers {
		if writer.key == moveWriterKey {
			found = append(found, writer)
		}
	}
	if len(found) == 0 {
		t.Fatalf("the source inventory did not list %s as a writer", moveWriterKey)
	}
	for _, kind := range []string{"priority", "step", "plan"} {
		caseName := "the boot's move of " + kind + " (storesmove/copy.go#writeBatch)"
		run, ok := runs[caseName]
		if !ok {
			t.Errorf("no behavioural case %q drove %s for kind %s", caseName, moveWriterKey, kind)
			continue
		}
		single := map[string]caseRun{caseName: run}
		for _, writer := range found {
			name, how := drivenBy(writer, single)
			if name == "" {
				t.Errorf("case %q did not drive %s (%s %s) for kind %s", caseName, writer.key, writer.verb, writer.table, kind)
				continue
			}
			t.Logf("driven for kind %s: %s (%s %s) by the case %q, read as %s", kind, writer.key, writer.verb, writer.table, name, how)
		}
		// Every transaction of the move that wrote graph_tuples must also have written
		// stores_moved_seqs in that same transaction, so a batch that wrote its tuples through a
		// pooled connection instead of its transaction fails here even if its failure-injection run
		// somehow escaped.
		recorded := map[int64]bool{}
		for _, row := range run.log {
			if row.table == storesMovedRecord {
				recorded[row.txid] = true
			}
		}
		for _, row := range run.log {
			if row.table == "graph_tuples" && !recorded[row.txid] {
				t.Errorf("%s (%s): wrote graph_tuples in transaction %d without writing %s in that transaction: %s",
					caseName, moveWriterKey, row.txid, storesMovedRecord, row.query)
			}
		}
	}
}

// nearestWrites is, for a writer no case drove, up to three distinct statements of its table the
// cases ran, each with its case, so a reader sees whether the writer ran under another text.
func nearestWrites(writer sourceWriter, runs map[string]caseRun) []string {
	var near []string
	seen := map[string]bool{}
	add := func(name, text string) {
		if len(near) < 3 && !seen[text] {
			if len(text) > 300 {
				text = text[:300] + "..."
			}
			seen[text] = true
			near = append(near, strconv.Quote(name)+": "+text)
		}
	}
	verb, _, _ := strings.Cut(writer.whole, " ")
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		if record := runs[name].record; record != nil {
			for _, statement := range record.statements {
				if statement.table == writer.table && strings.HasPrefix(statement.text, verb) {
					add(name, statement.text)
				}
			}
		}
		for _, row := range runs[name].log {
			if row.table == writer.table && strings.HasPrefix(row.query, verb) {
				add(name+" (logged)", row.query)
			}
		}
	}
	return near
}

// requireEveryCatalogueWriterDriven reads the catalogue's writers of a projection: each runs only
// from triggers whose every event a case drove, in a transaction that wrote the store beside the
// projection row it wrote, or it is a writer no case can stand behind. It prints every function
// body it accepted, and each write the store need not hold.
func requireEveryCatalogueWriterDriven(t *testing.T, catalogue []catalogueWriter, triggers []catalogueTrigger, bodies int, runs map[string]caseRun) {
	t.Logf("the catalogue holds %d function bodies and %d triggers; %d writes of a projection stand in them", bodies, len(triggers), len(catalogue))
	if bodies == 0 || len(triggers) == 0 {
		t.Fatal("the catalogue read no function body or no trigger, so nothing was checked")
	}
	for _, writer := range catalogue {
		if !writer.held {
			t.Logf("not a write the store holds: the function %s writes %s: %s", writer.function, writer.table, writer.verb)
			continue
		}
		reaching := writer.triggers
		for _, caller := range writer.callers {
			for _, trigger := range triggers {
				if trigger.function == caller {
					reaching = append(reaching, trigger)
				}
			}
		}
		if len(reaching) == 0 {
			t.Errorf("the function %s writes %s (%s), and no trigger runs it: a writer of a projection outside every transaction a case drives\n%s", writer.function, writer.table, writer.verb, writer.body)
			continue
		}
		accepted := true
		for _, trigger := range reaching {
			for _, event := range trigger.events {
				name := triggeredWrite(trigger, event, writer.table, runs)
				if name == "" {
					accepted = false
					t.Errorf("the function %s writes %s (%s), run by the trigger %s on %s for each %s, and no case drove that %s of %s with the store written in its transaction\n%s",
						writer.function, writer.table, writer.verb, trigger.name, trigger.table, event, event, trigger.table, writer.body)
					continue
				}
				t.Logf("the trigger %s on %s, for each %s, runs %s, which wrote %s in the transaction of the case %q, beside its store write", trigger.name, trigger.table, event, writer.function, writer.table, name)
			}
		}
		if accepted {
			t.Logf("accepted: the function %s, which writes %s (%s), run from %d triggers:\n%s", writer.function, writer.table, writer.verb, len(reaching), writer.body)
		}
	}
}

// triggeredWrite is the case whose plain run wrote table at a trigger's depth in a transaction
// that wrote trigger's table by event and wrote the store, "" for none.
func triggeredWrite(trigger catalogueTrigger, event, table string, runs map[string]caseRun) string {
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		byTransaction := map[int64][]loggedWrite{}
		for _, row := range runs[name].log {
			byTransaction[row.txid] = append(byTransaction[row.txid], row)
		}
		for _, rows := range byTransaction {
			fired, wrote, stored := false, false, false
			for _, row := range rows {
				fired = fired || (row.table == trigger.table && row.op == event && row.depth == 1)
				wrote = wrote || (row.table == table && row.depth > 1)
				stored = stored || slices.Contains(storeTables, row.table)
			}
			if fired && wrote && stored {
				return name
			}
		}
	}
	return ""
}

// requireEveryReadPointsAtAStoreWrite holds each projection a read answers from to a store write
// the test can point at: a case whose plain run wrote that projection in a transaction that wrote
// the store. A projection no read answers from is printed.
func requireEveryReadPointsAtAStoreWrite(t *testing.T, sources sourceInventory, projections map[string]*projection, runs map[string]caseRun) {
	read := 0
	for _, table := range slices.Sorted(maps.Keys(projections)) {
		sites := sources.reads[table]
		if sites == 0 {
			t.Logf("no statement of the sources reads %s", table)
			continue
		}
		read++
		pointed := ""
		for _, name := range slices.Sorted(maps.Keys(runs)) {
			stored := map[int64]bool{}
			for _, row := range runs[name].log {
				if slices.Contains(storeTables, row.table) {
					stored[row.txid] = true
				}
			}
			for _, row := range runs[name].log {
				if row.table == table && stored[row.txid] {
					pointed = name
				}
			}
			if pointed != "" {
				break
			}
		}
		if pointed == "" {
			t.Errorf("%d statements of the sources read %s, and no case wrote it beside a store write, so no read of it answers from a projection whose store write the test can point at", sites, table)
			continue
		}
		t.Logf("%d statements of the sources read %s, whose store write the case %q points at", sites, table, pointed)
	}
	if read == 0 {
		t.Fatal("no statement of the sources reads a projection, so nothing was checked")
	}
}

// reconcileExtensions are the files a script that reconciles a projection could be written in.
var reconcileExtensions = []string{".sh", ".bash", ".sql", ".ts", ".js", ".mjs", ".py", ".yaml", ".yml", ".tf"}

// testMaterial matches a path that is a test's own material rather than a script a deployment
// runs: a test file, a test directory, a fixture, an acceptance driver, a migration.
var testMaterial = regexp.MustCompile(`(?:^|/)(?:__tests__|__recordings__|test|tests|testdata|fixtures|e2e|acceptance|migrations)/|[._]test\.[a-z]+$|\.e2e\.ts$|\.spec\.ts$`)

// scriptExclusion is a part of the checkout the script search leaves out, with why.
type scriptExclusion struct {
	name, reason string
	holds        func(name string) bool
}

// scriptExclusions are the parts of the checkout the script search leaves out, each printed with
// its reason: no shipped build runs them, so a write of a projection there is no writer of a
// deployment's database.
var scriptExclusions = []scriptExclusion{{
	name:   "test material and migrations (" + testMaterial.String() + ")",
	reason: "a test's own material, an end-to-end tree or an acceptance driver runs against a database the test made, and a migration is the schema's own history",
	holds:  testMaterial.MatchString,
}, {
	name:   "scripts/search-measure/",
	reason: "a measuring fixture: it loads a captured corpus into a database of its own to measure search, and no shipped build runs it",
	holds:  func(name string) bool { return strings.HasPrefix(name, "scripts/search-measure/") },
}}

// reconcilingScripts is every file of files under root, of reconcileExtensions and outside
// scriptExclusions, that writes a projection, each with the statement it holds, and how many
// scripts it read and how many each exclusion left out. An update counts only where it sets a
// column the store states, as in the sources (updateHeld): a script that moves an issue's event
// counter writes no projection.
func reconcilingScripts(root string, files []string, writes *regexp.Regexp, projections map[string]*projection) ([]string, int, map[string]int) {
	var found []string
	searched := 0
	excluded := map[string]int{}
	for _, name := range files {
		if !slices.Contains(reconcileExtensions, filepath.Ext(name)) {
			continue
		}
		left := false
		for _, exclusion := range scriptExclusions {
			if exclusion.holds(name) {
				excluded[exclusion.name]++
				left = true
				break
			}
		}
		if left {
			continue
		}
		content, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		searched++
		text := string(content)
		for _, match := range writes.FindAllStringSubmatchIndex(text, -1) {
			verb := strings.ToLower(strings.Join(strings.Fields(text[match[2]:match[3]]), " "))
			if held := projections[strings.ToLower(text[match[4]:match[5]])]; verb == "update" && held != nil {
				rest := text[match[5]:]
				if end := strings.IndexByte(rest, ';'); end >= 0 {
					rest = rest[:end]
				}
				if stated, _ := updateHeld(held, rest); !stated {
					continue
				}
			}
			start := strings.LastIndexByte(text[:match[0]], '\n') + 1
			end := len(text)
			if next := strings.IndexByte(text[match[0]:], '\n'); next >= 0 {
				end = match[0] + next
			}
			line := strings.Count(text[:match[0]], "\n") + 1
			found = append(found, fmt.Sprintf("%s:%d: %s", name, line, strings.TrimSpace(text[start:end])))
		}
	}
	return found, searched, excluded
}

// requireNoReconcilingScript searches every script of the repository, outside its test material,
// for a statement that writes a projection, first over a scratch tree holding a planted
// reconciling script, which the search must find, so the absence it reports over the checkout is
// one it could have found.
func requireNoReconcilingScript(t *testing.T, projections map[string]*projection) {
	writes := projectionWrites(slices.Sorted(maps.Keys(projections)))
	planted := t.TempDir()
	script := "scripts/reconcile-comments.sh"
	if err := os.MkdirAll(filepath.Join(planted, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planted, script), []byte("#!/usr/bin/env bash\npsql \"$DATABASE_URL\" -c \"update comments set node_kind = 'comment', node_address = id::text where node_kind is null\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, searched, _ := reconcilingScripts(planted, []string{script}, writes, projections)
	if len(found) != 1 || searched != 1 {
		t.Fatalf("the search over the planted tree %s found %v in %d files, want the planted script's one statement", planted, found, searched)
	}
	t.Logf("the search finds the planted reconciling script: %s", found[0])
	root, err := filepath.Abs(filepath.Join(oneWriterModule, "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("list the checkout's files: %v", err)
	}
	var files []string
	for _, name := range strings.Split(string(listed), "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	found, searched, excluded := reconcilingScripts(root, files, writes, projections)
	for _, exclusion := range scriptExclusions {
		t.Logf("the search leaves out %s, %d scripts: %s", exclusion.name, excluded[exclusion.name], exclusion.reason)
	}
	t.Logf("the search read %d scripts of the checkout outside those", searched)
	if searched == 0 {
		t.Fatal("the search read no script of the checkout, so it proves nothing")
	}
	for _, statement := range found {
		t.Errorf("a script writes a projection after the fact: %s", statement)
	}
}

// requireTwoWritersAtOnce runs, for each projection the rig's own cases write, two of its cases
// that can run again at once - one case twice where only one can - and then reads the census of
// every kind the projection holds at zero differences.
func (r *oneWriterRig) requireTwoWritersAtOnce(t *testing.T, cases []writerCase) {
	ctx := context.Background()
	ran := 0
	for _, table := range slices.Sorted(maps.Keys(r.projections)) {
		if table == storesMovedRecord {
			// Two moves at once run over a pre-move database of their own (requireTwoStoresMovesAtOnce).
			continue
		}
		var writers []writerCase
		for _, c := range cases {
			if c.again != nil && c.on == "" && slices.Contains(c.tables, table) {
				writers = append(writers, c)
			}
		}
		if len(writers) == 0 {
			t.Errorf("no case of the rig's own that can run again writes %s, so no two writers of it were run at once", table)
			continue
		}
		pair := []writerCase{writers[0], writers[0]}
		if len(writers) > 1 {
			pair[1] = writers[1]
		}
		errs := make([]error, 2)
		var wait sync.WaitGroup
		for index, c := range pair {
			wait.Add(1)
			go func() {
				defer wait.Done()
				errs[index] = c.again(t, r, r.database)
			}()
		}
		wait.Wait()
		settleAll(t, r.database)
		for index, err := range errs {
			if err != nil {
				t.Errorf("%s, run beside %s on %s, failed: %v", pair[index].name, pair[1-index].name, table, err)
			}
		}
		ran++
		report, err := graphcensus.StoreCensus(ctx, r.database.Pool.Config().ConnString(), false, nil)
		if err != nil {
			t.Fatalf("run the census after two writers of %s: %v", table, err)
		}
		for _, kind := range report.Kinds {
			if base, _, _ := strings.Cut(kind.Table, "."); base != table {
				continue
			}
			if kind.RowDifferences+kind.NodeDifferences != 0 {
				t.Errorf("after %s and %s wrote %s at once, the census of %s reads %d row and %d node differences: %v %v",
					pair[0].name, pair[1].name, table, kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
				continue
			}
			t.Logf("after %s and %s wrote %s at once, the census of %s reads %d rows and %d nodes, zero differences", pair[0].name, pair[1].name, table, kind.Kind, kind.Rows, kind.Nodes)
		}
	}
	if ran == 0 {
		t.Fatal("no two writers were run at once")
	}
}

// requireTwoStoresMovesAtOnce runs two storesmove.Move calls at once against one database seeded
// with the release before's priority, step and plan rows, holding the first runner on the
// stores-moved advisory lock (Options.HoldFile) until the second is waiting on pg_locks, then
// releasing it. Exactly one runner moves the stores, the second finds them moved and writes
// nothing, each fact of priority, step and plan is written once, and the store census reads zero
// differences for all three kinds.
func requireTwoStoresMovesAtOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	seedStoresMoveCorpus(t, database)
	hold := filepath.Join(t.TempDir(), "release")
	url := database.Pool.Config().ConnString()

	type outcome struct {
		result *storesmove.Result
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := storesmove.Move(ctx, url, storesmove.Options{Batch: 3, HoldFile: hold})
		first <- outcome{result, err}
	}()
	waitForLock(t, database, `select exists (select 1 from pg_locks where locktype = 'advisory' and granted and objid = hashtext('stores-moved'))`)

	second := make(chan outcome, 1)
	go func() {
		result, err := storesmove.Move(ctx, url, storesmove.Options{Batch: 3})
		second <- outcome{result, err}
	}()
	waitForLock(t, database, `select exists (select 1 from pg_locks where locktype = 'advisory' and not granted and objid = hashtext('stores-moved'))`)

	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatalf("release the first move: %v", err)
	}
	one, two := <-first, <-second
	if one.err != nil {
		t.Fatalf("the first move failed: %v", one.err)
	}
	if two.err != nil {
		t.Fatalf("the second move failed: %v", two.err)
	}
	if !one.result.Moved || one.result.Copied == 0 {
		t.Errorf("the first move answered %+v, want it to have moved the stores", one.result)
	}
	if two.result.Moved || two.result.Copied != 0 || two.result.SwapCopied != 0 {
		t.Errorf("the second move answered %+v, want it to have moved nothing once the first released stores-moved", two.result)
	}
	report, err := graphcensus.StoreCensus(ctx, url, false, nil)
	if err != nil {
		t.Fatalf("run the store census after two moves at once: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind != "priority" && kind.Kind != "step" && kind.Kind != "plan" {
			continue
		}
		if kind.Rows == 0 || kind.Nodes == 0 {
			t.Errorf("after two moves at once, the census of %s reads %d rows and %d nodes, want both above zero", kind.Kind, kind.Rows, kind.Nodes)
		}
		if kind.RowDifferences+kind.NodeDifferences != 0 {
			t.Errorf("after two moves at once, the census of %s reads %d row and %d node differences: %v %v",
				kind.Kind, kind.RowDifferences, kind.NodeDifferences, kind.ReportedRows, kind.ReportedNodes)
			continue
		}
		t.Logf("after two moves at once, the census of %s reads %d rows and %d nodes, zero differences", kind.Kind, kind.Rows, kind.Nodes)
	}
	var duplicates int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from (
			select from_kind, from_id, kind, to_kind, to_id, at, asserted
			from graph_tuples
			where from_kind in ('priority', 'step', 'plan') or (from_kind = 'issue' and kind = 'advances')
			group by from_kind, from_id, kind, to_kind, to_id, at, asserted
			having count(*) > 1
		) dup`).Scan(&duplicates); err != nil {
		t.Fatalf("check for duplicate tuples after two moves at once: %v", err)
	}
	if duplicates != 0 {
		t.Errorf("two moves at once wrote %d duplicate facts of priority, step, plan or advances, want 0", duplicates)
	}
	var nodes int
	if err := database.Pool.QueryRow(ctx, `select count(*) from graph_nodes where kind in ('priority', 'step', 'plan')`).Scan(&nodes); err != nil {
		t.Fatalf("count moved nodes: %v", err)
	}
	t.Logf("two moves at once wrote %d nodes of priority, step and plan, %d tuples copied by the first runner and %d by the second, zero duplicates",
		nodes, one.result.Copied, two.result.Copied)
}

// waitForLock polls query, a select of one boolean over database, until it answers true.
func waitForLock(t *testing.T, database *store.Store, query string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var ready bool
		if err := database.Pool.QueryRow(context.Background(), query).Scan(&ready); err != nil {
			t.Fatalf("wait for lock (%s): %v", query, err)
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", query)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireTheHandoffWriter reads whether this head holds the handoff kind, which
// dispatch://INFRA-1336 creates: where the store's types hold it, a case must drive its writer,
// and where they do not, the head holds no such writer and it is printed.
func requireTheHandoffWriter(t *testing.T, ctx context.Context, database *store.Store, cases []writerCase) {
	var held bool
	if err := database.Pool.QueryRow(ctx, `select exists (select 1 from graph_nodes where kind = 'supertag' and id = 'handoff')`).Scan(&held); err != nil {
		t.Fatalf("read whether the store's types hold handoff: %v", err)
	}
	if !held {
		t.Logf("handoff: not created at this head (dispatch://INFRA-1336), so no writer of it is driven")
		return
	}
	for _, c := range cases {
		if strings.Contains(c.name, "handoff") {
			t.Logf("handoff: the case %q drives its writer", c.name)
			return
		}
	}
	t.Errorf("the store's types hold handoff, which dispatch://INFRA-1336 creates, and no case drives its writer")
}
