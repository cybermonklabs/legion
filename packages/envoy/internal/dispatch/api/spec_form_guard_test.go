//go:build !race

// The guard is static, as lock_level_test.go's is and for the reason given there: it reads the
// module's source, so the race detector adds nothing to it.

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The route census of the form gate (spec_form.go) holds only while every way to a person's
// approval passes one of its two call sites: an approval request opens or is handed back in
// requestArtifactApproval, an approval in a person's name is judged in approvalDocuments, which
// gives the documents that approval covers, and a reopen or the repair pass opens a withdrawn
// request again in prepareRestoration. Each writer of an approval in a person's name asks
// approvalDocuments: a person's review in queueApprovalReviews, and the carry of an approval in
// carryBreaksForm, the gate that writeCarriedReview asks. This test holds the code to that census.

// asksTable names asks as a statement may write it: bare, schema-qualified, quoted, or both, as
// reviewTable names artifact_reviews.
const asksTable = `(?:"?public"?\.)?(?:"asks"|asks\b)`

var (
	askInsertStatement = regexp.MustCompile(`\b(?:insert\s+into|merge\s+into|copy)\s+` + asksTable)
	askUpdateStatement = regexp.MustCompile(`\bupdate\s+(?:only\s+)?` + asksTable + `(?:\s+(?:as\s+)?[a-z_][a-z0-9_]*)?\s+set\s+`)
	// askOpenAssignment is an assignment of the set list that writes state open, or writes it from
	// a parameter, which can be open.
	askOpenAssignment = regexp.MustCompile(`(?:^|,)\s*(?:"?[a-z_][a-z0-9_]*"?\.)?"?state"?\s*=\s*(?:'open'|\$[0-9]+)`)
	// setListEnd is where an update's set list ends.
	setListEnd = regexp.MustCompile(`\s(?:where|returning|from)\s`)
)

// opensAsk reports whether one normalised statement opens an ask: an insert, a merge or a copy into
// asks, or an update of asks whose set list writes state open or writes it from a parameter.
func opensAsk(statement string) bool {
	if askInsertStatement.MatchString(statement) {
		return true
	}
	for _, match := range askUpdateStatement.FindAllStringIndex(statement, -1) {
		set := statement[match[1]:]
		if end := setListEnd.FindStringIndex(set); end != nil {
			set = set[:end[0]]
		}
		if askOpenAssignment.MatchString(set) {
			return true
		}
	}
	return false
}

// askOpeningWriters names every declaration of sources that writes a statement that opens an ask:
// the function that holds the statement's text, or the package-level constant or variable that
// holds it, mapped to its file. A pgx.Identifier literal naming asks, which a CopyFrom writes rows
// into, counts as a copy. The statements are read as the module scans read them
// (TestNoForUpdateOnOwnerTables), with constants in place.
func askOpeningWriters(sources []moduleSource, constants map[string]string) map[string]string {
	writers := map[string]string{}
	for _, source := range sources {
		pkg := source.file.Name.Name
		for _, declared := range source.file.Decls {
			switch declaration := declared.(type) {
			case *ast.FuncDecl:
				if declaration.Body != nil && opensAskIn(declaration.Body, pkg, constants) {
					writers[declaration.Name.Name] = source.path
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for index, name := range value.Names {
						if index < len(value.Values) && opensAskIn(value.Values[index], pkg, constants) {
							writers[name.Name] = source.path
						}
					}
				}
			}
		}
	}
	return writers
}

// opensAskIn reports whether node holds the text of a statement that opens an ask, or a
// pgx.Identifier literal naming asks. Only a literal or a `+` chain is read as a statement: a bare
// name of a constant or a variable is the statement of that declaration, which names it, so the
// code that passes the name on is not named for it. A selector is walked into, since the receiver
// of a method chain (tx.QueryRow(ctx, `insert ...`).Scan) holds the call that carries the text.
func opensAskIn(node ast.Node, pkg string, constants map[string]string) bool {
	found := false
	ast.Inspect(node, func(visited ast.Node) bool {
		if found {
			return false
		}
		switch typed := visited.(type) {
		case *ast.Ident:
			return false
		case *ast.CompositeLit:
			if namesAsksIdentifier(typed) {
				found = true
				return false
			}
		case *ast.BinaryExpr:
			if typed.Op != token.ADD {
				return true
			}
			found = slices.ContainsFunc(sqlStatementsIn(typed, pkg, constants), opensAsk)
			return false
		case *ast.BasicLit:
			found = slices.ContainsFunc(sqlStatementsIn(typed, pkg, constants), opensAsk)
			return false
		}
		return true
	})
	return found
}

// namesAsksIdentifier reports whether literal is a pgx.Identifier naming asks.
func namesAsksIdentifier(literal *ast.CompositeLit) bool {
	selector, ok := literal.Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Identifier" {
		return false
	}
	return slices.ContainsFunc(literal.Elts, func(element ast.Expr) bool {
		value, ok := element.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return false
		}
		name, err := strconv.Unquote(value.Value)
		return err == nil && name == "asks"
	})
}

// moduleRelative is a source path from moduleSources (moduleRoot), relative to the module.
func moduleRelative(source string) string {
	return strings.TrimPrefix(filepath.ToSlash(source), "../../../")
}

// askOpeningWriterCensus is every production declaration that writes an open ask, by
// module-relative file. prepareRestoration reaches WriteRestoredAsks after it judges the form.
var askOpeningWriterCensus = map[string]string{
	"openApprovalAsk":     "internal/dispatch/api/reviews.go",
	"WriteRestoredAsks":   "internal/dispatch/docs/ask_row.go",
	"createAskFor":        "internal/dispatch/api/asks.go",
	"createAskBlock":      "internal/dispatch/docs/ask_blocks.go",
	"restoreRetractedAsk": "internal/dispatch/docs/ask_blocks.go",
	"writeAsks":           "cmd/dispatch-seed/write_items.go",
}

// approvalCallers is each call the gate's census rests on, by the functions that make it, one
// entry for each call: the api package's calls, and the docs package's writer of a person's review,
// the carry of an approval, beside its call of the form gate. approvalDocuments is the one function
// that gives the documents an approval in a person's name covers and judges each, so a person's
// review and the carry's gate each call it, and nothing else judges such an approval.
var approvalCallers = map[string][]string{
	"openApprovalAsk":                         {"requestArtifactApproval"},
	"renewApprovalAsk":                        {"requestArtifactApproval"},
	"docs.QueueReview(docs.ReviewKindPerson)": {"queueApprovalReviews"},
	"approvalDocuments":                       {"carryBreaksForm", "queueApprovalReviews"},
	"refuseSpecForm":                          {"approvalDocuments", "requestArtifactApproval"},
	"specFormOf":                              {"getArtifactSpecForm", "prepareRestoration", "refuseSpecForm"},
	"docs: QueueReview(ReviewKindPerson)":     {"writeCarriedReview"},
	"docs: gate":                              {"writeCarriedReview"},
}

// approvalCallerProblems names each call of approvalCallers that the api and docs packages of
// sources make from another function, or do not make from a function the census names.
func approvalCallerProblems(sources []moduleSource) []string {
	callers := map[string][]string{}
	for _, source := range sources {
		var callee func(*ast.CallExpr) string
		switch path.Dir(moduleRelative(source.path)) {
		case "internal/dispatch/api":
			callee = approvalCallee
		case "internal/dispatch/docs":
			callee = docsApprovalCallee
		default:
			continue
		}
		for _, declared := range source.file.Decls {
			function, ok := declared.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if name := callee(call); name != "" {
						callers[name] = append(callers[name], function.Name.Name)
					}
				}
				return true
			})
		}
	}
	var problems []string
	for _, callee := range slices.Sorted(maps.Keys(approvalCallers)) {
		got := slices.Sorted(slices.Values(callers[callee]))
		if want := approvalCallers[callee]; !slices.Equal(got, want) {
			problems = append(problems, callee+" is called from "+strings.Join(got, ", ")+"; the census says "+strings.Join(want, ", "))
		}
	}
	return problems
}

// docsApprovalCallee is the entry of approvalCallers that production code in the docs package
// makes. QueueReview is called only by writeCarriedReview there; its kind is derived from a
// carried person's review, so the caller census verifies the gate rather than a literal argument.
func docsApprovalCallee(call *ast.CallExpr) string {
	function, ok := call.Fun.(*ast.Ident)
	if !ok {
		return ""
	}
	switch function.Name {
	case "gate":
		return "docs: gate"
	case "QueueReview":
		return "docs: QueueReview(ReviewKindPerson)"
	}
	return ""
}

// approvalCallee is the entry of approvalCallers call makes, or "".
func approvalCallee(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch selector.Sel.Name {
	case "openApprovalAsk", "renewApprovalAsk", "refuseSpecForm", "specFormOf", "approvalDocuments":
		return selector.Sel.Name
	case "QueueReview":
		if qualifier, ok := selector.X.(*ast.Ident); !ok || qualifier.Name != "docs" {
			return ""
		}
		for _, argument := range call.Args {
			if kind, ok := argument.(*ast.SelectorExpr); ok && kind.Sel.Name == "ReviewKindPerson" {
				return "docs.QueueReview(docs.ReviewKindPerson)"
			}
		}
	}
	return ""
}

// censusOf is the writers of sources, by module-relative file.
func censusOf(t *testing.T, sources []moduleSource) map[string]string {
	t.Helper()
	constants, err := stringConstants(sources)
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string]string{}
	for name, file := range askOpeningWriters(sources, constants) {
		writers[name] = moduleRelative(file)
	}
	return writers
}

// plantedSource is code parsed as a file of the api package, which the census reads beside the
// module's own sources.
func plantedSource(t *testing.T, code string) moduleSource {
	t.Helper()
	positions := token.NewFileSet()
	const planted = "../../../internal/dispatch/api/planted.go"
	file, err := parser.ParseFile(positions, planted, "package api\n\n"+code, 0)
	if err != nil {
		t.Fatalf("parse the planted source: %v", err)
	}
	return moduleSource{path: planted, file: file, positions: positions}
}

// Every statement of the module that opens an ask is one of the six writers of the census, and only
// requestArtifactApproval opens or hands back an approval request, and only queueApprovalReviews
// queues a person's review, each beside its call of the form gate; a person's review and the carry
// judge the documents of an approval by one function (approvalDocuments). A scan that read nothing
// names fewer than six and fails; each control plants one writer the census must name, or one the
// patterns must leave out, or a second caller that the caller part must refuse.
func TestOnlyTheFormGateOpensOrAnswersAPersonsApproval(t *testing.T) {
	sources := moduleSources(t)
	if problems := approvalCallerProblems(sources); len(problems) > 0 {
		t.Fatalf("the calls the form gate's census rests on moved:\n\t%s", strings.Join(problems, "\n\t"))
	}
	if got := censusOf(t, sources); !reflect.DeepEqual(got, askOpeningWriterCensus) {
		t.Fatalf("the writers that open an ask are\n%v\nthe census names\n%v\nA new writer of an open ask must judge the form (spec_form.go) and join the census.", got, askOpeningWriterCensus)
	}

	withPlanted := func(name string) map[string]string {
		census := maps.Clone(askOpeningWriterCensus)
		if name != "" {
			census[name] = "internal/dispatch/api/planted.go"
		}
		return census
	}
	for _, control := range []struct {
		name, code, names string
	}{
		{name: "an update that writes state open", names: "plantedReopen",
			code: "func plantedReopen(ctx context.Context, tx pgx.Tx, id string) error {\n" +
				"\t_, err := tx.Exec(ctx, `update asks set state = 'open' where id = $1`, id)\n\treturn err\n}\n"},
		{name: "an update over two lines that writes state from a parameter", names: "plantedRestore",
			code: "func plantedRestore(ctx context.Context, tx pgx.Tx, id, state string) error {\n" +
				"\t_, err := tx.Exec(ctx, `update public.asks\n\t\tset state = $2, answer = null where id = $1`, id, state)\n\treturn err\n}\n"},
		{name: "a constant that a function passes on", names: "plantedReopenSQL",
			code: "const plantedReopenSQL = \"update asks set state = 'open' where id = $1\"\n\n" +
				"func plantedUse(ctx context.Context, tx pgx.Tx, id string) error {\n" +
				"\t_, err := tx.Exec(ctx, plantedReopenSQL, id)\n\treturn err\n}\n"},
		{name: "an identifier a copy takes", names: "plantedCopy",
			code: "var plantedCopy = pgx.Identifier{\"asks\"}\n"},
		{name: "statements that open no ask",
			code: "func plantedNarrow(ctx context.Context, tx pgx.Tx, id string) error {\n" +
				"\tif _, err := tx.Exec(ctx, `update asks set state = 'answered' where id = $1`, id); err != nil {\n\t\treturn err\n\t}\n" +
				"\t_, err := tx.Exec(ctx, `insert into asks_archive (id) values ($1)`, id)\n\treturn err\n}\n"},
	} {
		t.Run("control: "+control.name, func(t *testing.T) {
			got := censusOf(t, append(slices.Clone(sources), plantedSource(t, control.code)))
			if want := withPlanted(control.names); !reflect.DeepEqual(got, want) {
				t.Fatalf("the census with %s planted =\n%v\nwant\n%v", control.name, got, want)
			}
		})
	}
	t.Run("control: a second caller of a person's review", func(t *testing.T) {
		planted := plantedSource(t, "func plantedApprove(ctx context.Context, tx pgx.Tx, artifact model.Artifact, actor model.Actor) error {\n"+
			"\treturn docs.QueueReview(nil, nil, artifact, 1, \"approved\", docs.ReviewKindPerson, actor, nil, nil, nil)\n}\n")
		problems := approvalCallerProblems(append(slices.Clone(sources), planted))
		if len(problems) != 1 || !strings.Contains(problems[0], "plantedApprove") {
			t.Fatalf("the caller part with a second person's review planted = %q, want one problem naming plantedApprove", problems)
		}
	})
	t.Run("control: a carry in the docs package that queues a person's approval with no gate", func(t *testing.T) {
		positions := token.NewFileSet()
		const planted = "../../../internal/dispatch/docs/planted.go"
		file, err := parser.ParseFile(positions, planted, "package docs\n\n"+
			"func plantedCarry(ctx context.Context, tx pgx.Tx, artifact model.Artifact, review model.ArtifactReview) error {\n"+
			"\treturn QueueReview(nil, nil, artifact, 2, \"approved\", cmp.Or(review.Kind, ReviewKindPerson), review.Actor, nil, nil, nil)\n}\n", 0)
		if err != nil {
			t.Fatalf("parse the planted source: %v", err)
		}
		problems := approvalCallerProblems(append(slices.Clone(sources), moduleSource{path: planted, file: file, positions: positions}))
		if len(problems) != 1 || !strings.Contains(problems[0], "plantedCarry") {
			t.Fatalf("the caller part with an ungated carry planted = %q, want one problem naming plantedCarry", problems)
		}
	})
}
