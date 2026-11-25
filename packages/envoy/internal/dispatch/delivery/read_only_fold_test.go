package delivery

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// resultsHeadsRead is one entry of resultsHeadsReads: one read of pull_request_heads on the
// results path, which plan D30 makes one statement for both of its callers (headsStatement). Its
// one function takes the request's answers (requestAnswers): a writer's results read passes none
// and runs the statement over the stored rows alone, and a read-only caller's passes what its own
// request's read-throughs learned and runs it over the stored rows with those answers laid on.
type resultsHeadsRead struct {
	// read is the read as plan D30 names it: the name of its statement where the statement has
	// one (headsAttributedQuery), and else the name of its function (HeadStoredAt).
	read string
	// function is the read's one entry point, which both callers call: a function by its name, or
	// a method as Type.Method, as the two reads of an issue's linked pull requests are methods of
	// the Attributor whose rename pairs their statements read (NewAttributor).
	function string
	// statement is the name of the package-level constant or variable that holds the read's one
	// statement text: the text after its WITH list. function must reach it through the package's
	// own functions and values, and no other exported function may reach it but through function.
	statement string
	// text is that constant or variable's value: the one statement the read sends, given a
	// request's answers or none, ends with it.
	text string
}

// resultsHeadsReads is the registry of the reads of pull_request_heads on the results path: the
// five reads of plan D30, each with its one function and its one statement.
// TestEachHeadsReadOnTheResultsPathIsOneStatement holds the package to it, and
// TestTheReadOnlyFoldAnswersAsTheWritersPath asks each read of it its questions (foldQuestions).
var resultsHeadsReads = []resultsHeadsRead{
	{read: "headsAttributedQuery", function: "Attributor.HeadsAttributedTo", statement: "headsAttributedQuery", text: headsAttributedQuery},
	{read: "HeadStoredAt", function: "HeadStoredAt", statement: "headStoredAtQuery", text: headStoredAtQuery},
	{read: "MergeOfIssue", function: "Attributor.MergeOfIssue", statement: "mergeOfIssueQuery", text: mergeOfIssueQuery},
	{read: "MergeIn", function: "Attributor.MergeIn", statement: "mergeInQuery", text: mergeInQuery},
	{read: "NewestPullRequestHead", function: "NewestPullRequestHead", statement: "newestPullRequestHeadQuery", text: newestPullRequestHeadQuery},
	{read: "LandingOf", function: "LandingOf", statement: "landingQuery", text: landingQuery},
	{read: "MergedPullRequestOf", function: "MergedPullRequestOf", statement: "mergedPullRequestOfQuery", text: mergedPullRequestOfQuery},
}

// answeredReads is the functions and methods besides the reads' own that take the request's
// answers: the one that passes them to a read's statement as its parameters
// (headsStatement.query), and the branch reads, which read no pull_request_heads row and read a
// merge the request's read-throughs learned as the yes a writer's store of it would have written
// (Branches.ReadCommitOnBranch). A function is named by its name, and a method as Type.Method.
var answeredReads = []string{
	"headsStatement.query",
	"Branches.ReadCommitOnBranch",
	"Branches.readCommitOnBranch",
	"mergeLearnedOnBranch",
}

// requestAnswers is the type the reads of the results path take one request's answers in: the
// heads its read-throughs learned from GitHub and stored nowhere.
const requestAnswers = "HeadAnswers"

// answerShapes is each type that holds GitHub's answers to one results read: the heads one
// request's read-throughs learned (HeadAnswers), the default branch and the unseen pairs its
// branch reads carry (BranchCarry), and one read-through's answer as GitHub gave it (CommitPulls).
// A request makes each of them and drops it with the request. Any other type of the package whose
// fields hold one, directly or through another such type, keeps answers past the request that
// read them.
var answerShapes = []string{"HeadAnswers", "BranchCarry", "CommitPulls"}

// processHolds is each type of the package that the server holds for the process and that a
// results read reads (api's server.branches and server.commitPulls), with the fields it holds:
// what a writer's read writes there, the default branches and the compare holds, and the claims
// and the settles. A read-only read reads them and writes none of them (plan D32). Any other field
// keeps something across requests whatever type it is, such as a memory of the branch answers
// read-only reads learned, so the inventory fails on it until this list names it.
var processHolds = map[string][]string{
	"Branches":        {"mu", "known", "compares"},
	"CommitPullReads": {"mu", "notBefore"},
}

// TestEachHeadsReadOnTheResultsPathIsOneStatement is plan D30's inventory of the reads of
// pull_request_heads on the results path, and plan D32's rule that a read-only read keeps nothing
// after its request. It parses the package's non-test files, as go build selects them, and fails
// on each of these, naming it:
//
//   - a type that keeps answers past the request that read them (answerShapes), and every function
//     or method that takes one, as a parameter or as its receiver;
//   - a field of a type the server holds for the process that processHolds does not name, and a
//     field it names that the type does not hold;
//   - a function or method that takes the request's answers (requestAnswers) and that the registry
//     does not name (resultsHeadsReads, answeredReads);
//   - a name in the registry that the package does not declare;
//   - a read whose statement is not a constant or variable of the package, or is not reached from
//     the read's function, or is reached from another exported function: a second entry point.
//
// Then, in each fold history (foldHistories), on a database of its own (a subtest, which skips
// where none is set), it runs each read on a traced pool (storetest.Traced) with the history's
// request's answers, as foldQuestions asks the read, and fails where the read sends more than one
// statement, or one that does not end with the read's registered text (resultsHeadsRead.text): a
// statement only a read-only read sends, before or beside its one, fails here.
func TestEachHeadsReadOnTheResultsPathIsOneStatement(t *testing.T) {
	source := parsePackageSource(t)

	keeping := source.keepingTypes()
	for _, name := range slices.Sorted(maps.Keys(keeping)) {
		t.Errorf("%s: the type %s holds %s, so it keeps answers past the request that read them. "+
			"A read-only results read keeps nothing after its request (plan D32): pass the answers to that request's reads alone",
			source.positions.Position(source.types[name].Pos()), name, keeping[name])
	}
	for _, name := range slices.Sorted(maps.Keys(processHolds)) {
		spec, declared := source.types[name]
		if !declared {
			t.Errorf("processHolds names %s, which is no type of the package", name)
			continue
		}
		structure, isStruct := spec.Type.(*ast.StructType)
		if !isStruct {
			t.Errorf("%s: processHolds names %s, which is no struct", source.positions.Position(spec.Pos()), name)
			continue
		}
		named := map[string]bool{}
		for _, field := range processHolds[name] {
			named[field] = true
		}
		held := map[string]bool{}
		for _, field := range structure.Fields.List {
			for _, fieldName := range fieldNames(field) {
				held[fieldName] = true
				if !named[fieldName] {
					t.Errorf("%s: %s holds %s, which processHolds does not name: the server holds a %s for the process, so the field "+
						"keeps what it holds across requests. A read-only results read keeps nothing after its request (plan D32): "+
						"carry what it learns for that request alone (HeadAnswers, BranchCarry)",
						source.positions.Position(field.Pos()), name, fieldName, name)
				}
			}
		}
		for _, field := range processHolds[name] {
			if !held[field] {
				t.Errorf("processHolds names %s.%s, which %s does not hold", name, field, name)
			}
		}
	}
	registered := map[string]bool{}
	for _, name := range answeredReads {
		registered[name] = true
	}
	for _, read := range resultsHeadsReads {
		registered[read.function] = true
	}
	for _, function := range source.functions {
		if kept := function.takes(keeping, true); kept != "" {
			t.Errorf("%s: %s takes answers kept across requests, in %s (plan D32)",
				source.positions.Position(function.decl.Pos()), function.key, kept)
		}
		if registered[function.key] {
			continue
		}
		if takes := function.takes(map[string]string{requestAnswers: requestAnswers}, false); takes != "" {
			t.Errorf("%s: %s takes the request's answers as %s, and the registry names it nowhere (resultsHeadsReads, answeredReads): "+
				"it is a second copy of a writer's read, which no test holds to the writer's statement. "+
				"Run the read's one statement instead, through the read's one function",
				source.positions.Position(function.decl.Pos()), function.key, takes)
		}
	}

	for _, name := range answeredReads {
		if !source.declaresFunction(name) {
			t.Errorf("answeredReads names %s, which is no function or method of the package", name)
		}
	}
	for _, read := range resultsHeadsReads {
		if !source.declaresFunction(read.read) && !source.values[read.read] {
			t.Errorf("resultsHeadsReads names the read %s, which the package does not declare", read.read)
		}
		if !source.declaresFunction(read.function) {
			t.Errorf("the read %s names its function %s, which is no function of the package", read.read, read.function)
		}
		if !source.values[read.statement] {
			t.Errorf("the read %s names its statement %q, which is no constant or variable of the package", read.read, read.statement)
			continue
		}
		if source.declaresFunction(read.function) && !source.reaches(read.function, read.statement, "") {
			t.Errorf("%s does not run %s, the one statement of the read %s", read.function, read.statement, read.read)
		}
		for _, function := range source.functions {
			if function.key == read.function || !token.IsExported(function.decl.Name.Name) {
				continue
			}
			if source.reaches(function.key, read.statement, read.function) {
				t.Errorf("%s: %s runs %s, the one statement of the read %s, and not through %s: "+
					"it is a second entry point of the read, which a caller can pick in place of the other. "+
					"Give the read one function, which a writer calls with no answers",
					source.positions.Position(function.decl.Pos()), function.key, read.statement, read.read, read.function)
			}
		}
	}

	t.Run("each read sends its one statement given a request's answers", func(t *testing.T) {
		reads := map[string]resultsHeadsRead{}
		for _, read := range resultsHeadsReads {
			reads[read.read] = read
		}
		for _, history := range foldHistories() {
			t.Run(history.name, func(t *testing.T) {
				pool, ctx, attributor, answers := foldSetup(t, history.steps)
				recorder := &storetest.StatementRecorder{}
				traced := storetest.Traced(t, pool, recorder, nil)
				for _, question := range foldQuestions(t, history.steps) {
					before := len(recorder.Statements())
					if _, err := foldRead(ctx, traced, attributor, answers, question); err != nil {
						t.Errorf("%s, given the request's answers: %v", question, err)
						continue
					}
					read := reads[question.read]
					if sent := recorder.Statements()[before:]; len(sent) != 1 || !strings.HasSuffix(sent[0], read.text) {
						t.Errorf("%s, given the request's answers, sent %d statements:\n\t%s\nwant one, the read's own, which ends with %s (resultsHeadsReads): "+
							"both callers of a read run its one statement, and nothing beside it",
							question, len(sent), strings.Join(sent, "\n\t"), read.statement)
					}
				}
			})
		}
	})
}

// packageSource is the package's non-test files as TestEachHeadsReadOnTheResultsPathIsOneStatement
// reads them: every function and method in source order, every package-level constant, variable
// and type, and the names each function, method, constant and variable refers to.
type packageSource struct {
	positions *token.FileSet
	functions []packageFunction
	// values is every package-level constant and variable, by name.
	values map[string]bool
	// types is every package-level type, by name.
	types map[string]*ast.TypeSpec
	// refers is the names each function, method, constant, variable and headsStatement field a
	// literal sets (declare) refers to, by its key.
	refers map[string]map[string]bool
	// declared is the keys a name may refer to: a function, a constant or a variable of that
	// name, every method of that name, and every headsStatement field of that name a literal sets.
	declared map[string][]string
}

// packageFunction is one function or method of the package, with its key: its name, or
// Type.Method for a method.
type packageFunction struct {
	key  string
	decl *ast.FuncDecl
}

// parsePackageSource parses the non-test files go build selects for the package in the test's
// own directory. It reads every file's types before any function or value, so declare finds the
// struct type of a literal whichever file declares it.
func parsePackageSource(t *testing.T) *packageSource {
	t.Helper()
	listed, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	source := &packageSource{
		positions: token.NewFileSet(),
		values:    map[string]bool{},
		types:     map[string]*ast.TypeSpec{},
		refers:    map[string]map[string]bool{},
		declared:  map[string][]string{},
	}
	var files []*ast.File
	for _, name := range listed.GoFiles {
		file, err := parser.ParseFile(source.positions, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
		for _, decl := range file.Decls {
			if decl, isGeneral := decl.(*ast.GenDecl); isGeneral {
				for _, spec := range decl.Specs {
					if spec, isType := spec.(*ast.TypeSpec); isType {
						source.types[spec.Name.Name] = spec
					}
				}
			}
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				key := decl.Name.Name
				if decl.Recv != nil && len(decl.Recv.List) > 0 {
					key = receiverName(decl.Recv.List[0].Type) + "." + decl.Name.Name
				}
				source.functions = append(source.functions, packageFunction{key: key, decl: decl})
				source.declare(decl.Name.Name, key, decl.Body)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					spec, isValue := spec.(*ast.ValueSpec)
					if !isValue {
						continue
					}
					for _, name := range spec.Names {
						if name.Name == "_" {
							continue
						}
						source.values[name.Name] = true
						source.declare(name.Name, name.Name, spec)
					}
				}
			}
		}
	}
	return source
}

// declare records key, which name refers to, and the names node refers to as key's: every
// identifier in it, but the field and the value of a headsStatement field a literal sets. A keyed
// field of a composite literal of a package struct type, where that type declares the field a
// headsStatement, is a declaration of its own, keyed Type.field under the field's name, which
// refers to the names in the value the literal sets it to. A constructor that builds a read's
// statement once into its type, as NewAttributor builds the Attributor's from the rename pairs,
// stores the statement and runs nothing, and the method that reads the field runs it: so the
// statement is reached from that method, and not from the constructor. Every other literal,
// field and value is read as the declaration holding it.
func (s *packageSource) declare(name, key string, node ast.Node) {
	s.declared[name] = append(s.declared[name], key)
	refers := map[string]bool{}
	if node != nil {
		var visit func(ast.Node) bool
		visit = func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.Ident:
				refers[node.Name] = true
			case *ast.CompositeLit:
				typeName, named := node.Type.(*ast.Ident)
				if !named {
					return true
				}
				refers[typeName.Name] = true
				for _, element := range node.Elts {
					if field, keyed := element.(*ast.KeyValueExpr); keyed {
						if fieldName, isIdent := field.Key.(*ast.Ident); isIdent && s.holdsHeadsStatement(typeName.Name, fieldName.Name) {
							s.declare(fieldName.Name, typeName.Name+"."+fieldName.Name, field.Value)
							continue
						}
					}
					ast.Inspect(element, visit)
				}
				return false
			}
			return true
		}
		ast.Inspect(node, visit)
	}
	s.refers[key] = refers
}

// holdsHeadsStatement is whether typeName is a struct type of the package that declares field a
// headsStatement.
func (s *packageSource) holdsHeadsStatement(typeName, field string) bool {
	spec, declared := s.types[typeName]
	if !declared {
		return false
	}
	structure, isStruct := spec.Type.(*ast.StructType)
	if !isStruct {
		return false
	}
	for _, declaredField := range structure.Fields.List {
		fieldType, named := declaredField.Type.(*ast.Ident)
		if named && fieldType.Name == "headsStatement" && slices.Contains(fieldNames(declaredField), field) {
			return true
		}
	}
	return false
}

// declaresFunction is whether key is a function or method of the package.
func (s *packageSource) declaresFunction(key string) bool {
	for _, function := range s.functions {
		if function.key == key {
			return true
		}
	}
	return false
}

// reaches is whether from refers to target, directly or through the package's functions,
// methods, constants, variables and the headsStatement fields its literals set (declare), never
// through the declaration keyed avoid ("" avoids none). A name is read as every declaration of the
// package it could refer to, so a method call reaches every method of that name and a field's
// read every value a literal sets a headsStatement field of that name to: the test asks whether
// the statement is run at all, not by which path.
func (s *packageSource) reaches(from, target, avoid string) bool {
	seen := map[string]bool{from: true, avoid: true}
	next := []string{from}
	for len(next) > 0 {
		key := next[0]
		next = next[1:]
		for name := range s.refers[key] {
			for _, referred := range s.declared[name] {
				if referred == target {
					return true
				}
				if !seen[referred] {
					seen[referred] = true
					next = append(next, referred)
				}
			}
		}
	}
	return false
}

// keepingTypes is every type of the package but answerShapes whose fields hold an answer shape,
// directly or through another such type, each with the shape or the type it holds.
func (s *packageSource) keepingTypes() map[string]string {
	keeping := map[string]string{}
	shapes := map[string]bool{}
	for _, shape := range answerShapes {
		shapes[shape] = true
	}
	names := slices.Sorted(maps.Keys(s.types))
	for changed := true; changed; {
		changed = false
		for _, name := range names {
			if shapes[name] || keeping[name] != "" {
				continue
			}
			for _, held := range slices.Sorted(maps.Keys(typeMentions(s.types[name].Type))) {
				if shapes[held] || keeping[held] != "" {
					keeping[name], changed = held, true
					break
				}
			}
		}
	}
	return keeping
}

// takes is the first of names that a parameter of the function mentions in its type, or its
// receiver where receiver is true, and "" where none does.
func (f packageFunction) takes(names map[string]string, receiver bool) string {
	fields := slices.Clone(f.decl.Type.Params.List)
	if receiver && f.decl.Recv != nil {
		fields = append(fields, f.decl.Recv.List...)
	}
	for _, field := range fields {
		for _, mentioned := range slices.Sorted(maps.Keys(typeMentions(field.Type))) {
			if _, named := names[mentioned]; named {
				return mentioned
			}
		}
	}
	return ""
}

// typeMentions is every name a type expression mentions, the names of a struct's fields left out:
// the types it is made of, a pointer, a slice, a map and a type argument read through.
func typeMentions(expr ast.Expr) map[string]bool {
	mentioned := map[string]bool{}
	ast.Inspect(expr, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Field:
			maps.Copy(mentioned, typeMentions(node.Type))
			return false
		case *ast.Ident:
			mentioned[node.Name] = true
		}
		return true
	})
	return mentioned
}

// receiverName is the name of a method's receiver type, without its pointer or its type
// parameters.
func receiverName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.StarExpr:
		return receiverName(expr.X)
	case *ast.IndexExpr:
		return receiverName(expr.X)
	case *ast.IndexListExpr:
		return receiverName(expr.X)
	case *ast.Ident:
		return expr.Name
	}
	return ""
}

// fieldNames is the names a struct field declares: its names, or for an embedded field the name
// of its type.
func fieldNames(field *ast.Field) []string {
	if len(field.Names) > 0 {
		names := make([]string, len(field.Names))
		for index, name := range field.Names {
			names[index] = name.Name
		}
		return names
	}
	embedded := field.Type
	if pointer, isPointer := embedded.(*ast.StarExpr); isPointer {
		embedded = pointer.X
	}
	if selector, isSelector := embedded.(*ast.SelectorExpr); isSelector {
		return []string{selector.Sel.Name}
	}
	return []string{receiverName(embedded)}
}

// foldIssue is the issue of deliveryRepo the fold histories attribute pull requests to, and
// foldLinkingIssue an issue of gadgetsRepo that links deliveryRepo's pull request 5, so its
// linked arm reads that pull request's row in every history (linkedPullRequestTerms).
const (
	foldIssue        = "HEAD-701"
	foldLinkingIssue = "GDGT-702"
)

// foldWriter names the writer of one step of a fold history.
type foldWriter string

const (
	// foldDelivery is a `pull_request` envelope's write (UpsertPullRequestHead).
	foldDelivery foldWriter = "a delivery"
	// foldReadThrough is a writer's read-through (StoreCommitPulls).
	foldReadThrough foldWriter = "a writer's read-through"
	// foldAnswered is no writer: an answer the read-only read's request learned from GitHub.
	foldAnswered foldWriter = "an answer of the request"
)

// foldStep is one step of a fold history, in time order.
type foldStep struct {
	writer foldWriter
	pulls  CommitPulls
}

// foldQuestion is one question asked of one read of resultsHeadsReads: the read's name there and
// its arguments.
type foldQuestion struct {
	read string
	args []string
}

func (q foldQuestion) String() string {
	return q.read + "(" + strings.Join(q.args, ", ") + ")"
}

// TestTheReadOnlyFoldAnswersAsTheWritersPath holds the read-only form of each read of the results
// path (resultsHeadsReads) to the writer's own path, history by history (foldHistories, plan D30).
// A history is the writers' writes, each run through the writer's own statement on the history's
// database, and then the answers one read-only request learned from GitHub, in the order it read
// them (foldSetup). The oracle is the writer's own path (foldOracle): the request's answers stored
// on the rows the writers left, as a writer's read-throughs store them, in a transaction rolled
// back once the writer's read has run in it. The read-only form runs on the rows the writers left,
// with the request's answers, and must give the oracle's answer to every question (foldQuestions).
// The request must hold one answer for each run of equal answers (foldAnswerRuns, plan D33).
func TestTheReadOnlyFoldAnswersAsTheWritersPath(t *testing.T) {
	for _, history := range foldHistories() {
		t.Run(history.name, func(t *testing.T) {
			pool, ctx, attributor, answers := foldSetup(t, history.steps)
			if held, runs := len(answers.columns.repo), foldAnswerRuns(history.steps); held != runs {
				t.Errorf("the request holds %d answers, want %d, one for each run of its answers for one pull request equal in every column "+
					"but checked_at: the writer's upserts of a run leave the row as the upsert of its last answer alone does (plan D33)", held, runs)
			}
			questions := foldQuestions(t, history.steps)
			want := foldOracle(t, ctx, pool, attributor, history.steps, questions)
			for _, question := range questions {
				got, err := foldRead(ctx, pool, attributor, answers, question)
				if err != nil {
					t.Errorf("%s, the read-only read: %v", question, err)
					continue
				}
				if got != want[question.String()] {
					t.Errorf("%s: the read-only read answered\n\t%s\nwant the writer's path's\n\t%s", question, got, want[question.String()])
				}
			}
		})
	}
}

// foldHistory is one history of the fold tests: its name, and its steps in time order.
type foldHistory struct {
	name  string
	steps []foldStep
}

// foldHistories is every history TestTheReadOnlyFoldAnswersAsTheWritersPath holds each read-only
// read to the writer's path in, and TestEachHeadsReadOnTheResultsPathIsOneStatement traces each
// read in. Each call builds them anew.
func foldHistories() []foldHistory {
	return []foldHistory{
		{"a writer's row and no answer", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(10))),
		}},
		{"an answer and no row", []foldStep{
			answered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(20))),
		}},
		// The row credits nobody; the answer is the merge, credited to the issue.
		{"an answer over a row that credits nobody", []foldStep{
			delivered(timed(head(5, sha("1")), at(1), read(10))),
			answered(timed(credited(merged(head(5, sha("1")), "release", sha("m")), foldIssue), at(2), read(20))),
		}},
		// The same event, read again later.
		{"an answer over a writer's read-through row of the same event", []foldStep{
			readThrough(timed(credited(head(5, sha("1")), foldIssue), at(1), read(10))),
			answered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(20))),
		}},
		// The row is credited to the linking issue, as a stale attribution leaves it, so both of
		// that issue's arms read it.
		{"an answer whose attribution was incomplete over a row attributed to an issue", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldLinkingIssue), at(1), read(10))),
			answered(timed(incomplete(head(5, sha("2"))), at(2), nil)),
		}},
		// A complete answer that carries no read time. ReadCommitPulls stamps every complete
		// answer, but the statement reads a missing read time from has_checked_at, not from
		// complete, so this answer keeps the row's read time, as the upsert's coalesce keeps it.
		{"a complete answer with no read time over a stored row", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(10))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(2), nil)),
		}},
		// GitHub's answer carries no updated_at. The upsert's event-time guard lets it through, and
		// the row keeps its own event time; the statement reads the missing event time from
		// has_event_at.
		{"an answer with no event time over a stored row", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldIssue), at(3), read(10))),
			answered(eventless(credited(head(5, sha("2")), foldIssue), read(20))),
		}},
		// Both carry the same times, so the number orders them.
		{"a merged pull request answered beside an open one stored", []foldStep{
			delivered(timed(credited(head(6, sha("6")), foldIssue), at(1), read(20))),
			answered(timed(credited(merged(head(5, sha("1")), "release", sha("m")), foldIssue), at(1), read(20))),
		}},
		// The second, of the pull request's next push, read no commit message: the issue stays
		// the first's, as the upsert keeps it when it stores both in turn.
		{"two answers of the request for one pull request", []foldStep{
			answered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(20))),
			answered(timed(incomplete(head(5, sha("2"))), at(2), nil)),
		}},
		// The request reads pull request 5, then pull request 6, then pull request 5 at its next
		// push. The writer upserts each answer on its own pull request's row, so pull request 6's
		// answer, whose event is the newest, never reaches pull request 5's row, which ends at the
		// second push.
		{"answers of the request for two pull requests interleaved", []foldStep{
			answered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(20))),
			answered(timed(credited(head(6, sha("6")), foldIssue), at(3), read(22))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(2), read(25))),
		}},
		// A row kept under the name the repository was renamed away from, and the answer of a read
		// asked under that name, keyed by the canonical one: the store drops the old row.
		{"an answer of a read asked under a name its repository was renamed away from", []foldStep{
			delivered(timed(credited(foldHead(gadgetsOldRepo, 9, sha("7")), foldLinkingIssue), at(1), read(10))),
			answeredUnder(gadgetsOldRepo, timed(credited(foldHead(gadgetsRepo, 9, sha("8")), foldLinkingIssue), at(2), read(20))),
		}},
		// A webhook stored the pull request's newer event; GitHub's commit-pulls answer carries an
		// older updated_at. The upsert's event-time guard refuses the answer, so the row stands.
		{"a stored row whose event is newer than the request's answer", []foldStep{
			delivered(timed(head(5, sha("1")), at(3), read(10))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(2), read(20))),
		}},
		// The request reads two commits whose answers name one pull request, the second answer
		// older: the guard refuses it laid on after the first, as it refuses the second upsert.
		{"two answers of the request out of event order", []foldStep{
			answered(timed(credited(head(5, sha("1")), foldIssue), at(3), read(20))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(2), read(25))),
		}},
		// A delivery stored the pull request at one commit. The request then reads two answers of
		// the next commit, equal but for their times: the first's event is newer than the row's and
		// the second's older. The writer's upsert of the first applies and the event-time guard
		// refuses the second, so the row ends at the first, with the time it was read.
		{"two answers equal but for their event times around a stored row's event", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldIssue), at(2), read(10))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(3), read(20))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(1), read(25))),
		}},
		// 61 answers in 14 runs (manyAnswers).
		{"many answers for one pull request", manyAnswers()},
		// Pull request 5 is stored at sha 1, and the request's read-through of another commit,
		// sha 0, names it at sha 2. The writer's upsert of that answer moves the row off sha 1,
		// so sha 1 heads no stored pull request: a HeadStoredAt given only the answers headed at
		// sha 1 would answer yes (plan D33).
		{"the moved head", []foldStep{
			delivered(timed(credited(head(5, sha("1")), foldIssue), at(1), read(10))),
			answered(timed(credited(head(5, sha("2")), foldIssue), at(2), read(20))),
		}},
	}
}

// manyAnswers is a request whose read-throughs name gadgetsRepo's pull request 9 again and
// again, over the row kept under the name gadgetsRepo was renamed away from and the row a
// delivery stored under the canonical name: runs of answers equal in every column but
// checked_at, each run unlike the one before it in the name the read was asked under, the
// head, the state, the attribution or the event time, which moves forward and back.
func manyAnswers() []foldStep {
	steps := []foldStep{
		delivered(timed(credited(foldHead(gadgetsOldRepo, 9, sha("7")), foldLinkingIssue), at(1), read(2))),
		delivered(timed(foldHead(gadgetsRepo, 9, sha("1")), at(3), read(4))),
	}
	readMinute := 30
	// run adds count answers equal to answer, each of a read asked under the name under: a
	// complete one read at the next minute, and an incomplete one unstamped, as ReadCommitPulls
	// answers them.
	run := func(count int, under string, answer PullRequestHead) {
		for range count {
			readMinute++
			answer.CheckedAt = nil
			if answer.AttributionComplete {
				answer.CheckedAt = read(readMinute)
			}
			steps = append(steps, answeredUnder(under, answer))
		}
	}
	pull9 := func(commit string) PullRequestHead {
		return credited(foldHead(gadgetsRepo, 9, commit), foldLinkingIssue)
	}
	closed := pull9(sha("3"))
	closed.State = "closed"
	run(5, gadgetsRepo, timed(pull9(sha("1")), at(5), nil))
	run(4, gadgetsOldRepo, timed(pull9(sha("1")), at(5), nil))
	run(3, gadgetsRepo, timed(pull9(sha("1")), at(5), nil))
	run(6, gadgetsRepo, timed(pull9(sha("2")), at(6), nil))
	// Older than the event the row holds: the guard refuses each.
	run(4, gadgetsRepo, timed(pull9(sha("3")), at(4), nil))
	run(5, gadgetsRepo, timed(incomplete(pull9(sha("3"))), at(7), nil))
	run(4, gadgetsRepo, timed(closed, at(8), nil))
	// Credited to nobody by a read of every source.
	run(5, gadgetsRepo, timed(foldHead(gadgetsRepo, 9, sha("4")), at(9), nil))
	run(3, gadgetsRepo, timed(pull9(sha("2")), at(8), nil))
	// The event the row holds: the guard lets each through.
	run(4, gadgetsRepo, timed(pull9(sha("4")), at(9), nil))
	// No event time: the row keeps its own.
	run(5, gadgetsRepo, pull9(sha("5")))
	run(6, gadgetsRepo, timed(merged(pull9(sha("5")), "release", sha("m")), at(10), nil))
	run(4, gadgetsOldRepo, timed(merged(pull9(sha("5")), "release", sha("m")), at(10), nil))
	run(3, gadgetsRepo, timed(pull9(sha("6")), at(9), nil))
	return steps
}

// foldSetup makes one history's database, which the test owns: the fold issues (seedFoldIssues)
// and then each writer's step, run through the writer's own statement (foldWrite). It answers the
// pool, the test's context, the Attributor of the rename pairs the fold issues seed, and the
// request's answers, added in the order the request read them. It fails where a writer's step
// comes after an answer of the request.
func foldSetup(t *testing.T, steps []foldStep) (*store.Pool, context.Context, *Attributor, HeadAnswers) {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	attributor := seedFoldIssues(t, ctx, pool)
	var answers HeadAnswers
	answering := false
	for index, step := range steps {
		if step.writer == foldAnswered {
			answering = true
			answers.Add(step.pulls)
			continue
		}
		if answering {
			t.Fatalf("step %d, %s, comes after an answer of the request: the fold lays one request's answers "+
				"on the rows stored when its statements run, so every writer's step comes first", index, step.writer)
		}
		foldWrite(t, ctx, pool, step)
	}
	return pool, ctx, attributor, answers
}

// at is minute minutes past 9:00 on 2026-11-20, in UTC: the times of the fold histories.
func at(minute int) time.Time {
	return time.Date(2026, 11, 20, 9, minute, 0, 0, time.UTC)
}

// read is at(minute) as a head's read time (checked_at).
func read(minute int) *time.Time {
	moment := at(minute)
	return &moment
}

// sha is a commit of forty times digit.
func sha(digit string) string {
	return strings.Repeat(digit, 40)
}

// head is deliveryRepo's pull request number at commit, open, credited to nobody by a read of
// every attribution source and carrying no time: a head a history writes or answers, which the
// functions after it change.
func head(number int, commit string) PullRequestHead {
	return foldHead(deliveryRepo, number, commit)
}

// credited is head credited to issue by its title.
func credited(head PullRequestHead, issue string) PullRequestHead {
	head.IssueKey, head.AttributionSource = &issue, "title"
	return head
}

// incomplete is head from a read that did not read every attribution source.
func incomplete(head PullRequestHead) PullRequestHead {
	head.AttributionComplete = false
	return head
}

// timed is head carrying the event time event and the read time readAt, nil for none.
func timed(head PullRequestHead, event time.Time, readAt *time.Time) PullRequestHead {
	head.EventAt, head.CheckedAt = &event, readAt
	return head
}

// eventless is head carrying no event time, as headRow makes it where GitHub's answer has no
// updated_at (pullRequestPayload.UpdatedAt is nil), and the read time readAt, nil for none.
func eventless(head PullRequestHead, readAt *time.Time) PullRequestHead {
	head.EventAt, head.CheckedAt = nil, readAt
	return head
}

// merged is head merged into base by the merge commit commit.
func merged(head PullRequestHead, base, commit string) PullRequestHead {
	head.State, head.BaseRef, head.MergeCommitSHA = "merged", base, commit
	return head
}

// delivered is a delivery's step that writes head.
func delivered(head PullRequestHead) foldStep {
	return foldStep{writer: foldDelivery, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: head.Repo}}
}

// readThrough is a writer's read-through's step that stores head.
func readThrough(head PullRequestHead) foldStep {
	return foldStep{writer: foldReadThrough, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: head.Repo}}
}

// answeredUnder is an answer of the request, head, of a read asked under the name under.
func answeredUnder(under string, head PullRequestHead) foldStep {
	return foldStep{writer: foldAnswered, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: under}}
}

// answered is an answer of the request, head, of a read asked under its repository's own name.
func answered(head PullRequestHead) foldStep {
	return answeredUnder(head.Repo, head)
}

// foldHead is pull request number of repo at commit, open, credited to nobody by a read of every
// attribution source and carrying no time.
func foldHead(repo string, number int, commit string) PullRequestHead {
	return PullRequestHead{
		Repo: repo, Number: number, HeadSHA: commit, HeadRef: "legion/work", BaseRef: "main", State: "open",
		AttributionSource: "none", AttributionComplete: true,
		Title: fmt.Sprintf("Pull request %d", number), URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
	}
}

// seedFoldIssues writes foldIssue, of deliveryRepo, and foldLinkingIssue, of gadgetsRepo, which
// gadgetsOldRepo was renamed to, and links foldLinkingIssue to deliveryRepo's pull request 5. It
// answers the Attributor of the rename pairs the database then holds, as a process builds its own
// at boot (renamesOf).
func seedFoldIssues(t *testing.T, ctx context.Context, pool *store.Pool) *Attributor {
	t.Helper()
	if key := seedHeadIssue(t, ctx, pool, 701); key != foldIssue {
		t.Fatalf("seeded %s, want %s", key, foldIssue)
	}
	seedRenamedGadgets(t, ctx, pool)
	if key := seedGadgetsIssue(t, ctx, pool, 702); key != foldLinkingIssue {
		t.Fatalf("seeded %s, want %s", key, foldLinkingIssue)
	}
	link := "https://github.com/" + deliveryRepo + "/pull/5"
	if _, err := pool.Exec(ctx, `
		insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'url')
	`, foldLinkingIssue, link); err != nil {
		t.Fatalf("link %s to %s: %v", foldLinkingIssue, link, err)
	}
	attributor := NewAttributor(renamesOf(t, ctx, pool))
	linked, err := attributor.LinkedPullRequests(ctx, pool, foldLinkingIssue)
	if err != nil || len(linked) != 1 || linked[0] != (LinkedPullRequest{Repo: deliveryRepo, Number: 5}) {
		t.Fatalf("the pull requests %s links = %v, %v; want %s#5 alone", foldLinkingIssue, linked, err, deliveryRepo)
	}
	return attributor
}

// foldWrite runs a writer's step through the writer's own statement.
func foldWrite(t *testing.T, ctx context.Context, pool *store.Pool, step foldStep) {
	t.Helper()
	switch step.writer {
	case foldDelivery:
		for _, head := range step.pulls.Heads {
			if err := UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
				t.Fatalf("%s of %s#%d: %v", step.writer, head.Repo, head.Number, err)
			}
		}
	case foldReadThrough:
		if _, err := StoreCommitPulls(ctx, pool, nil, step.pulls); err != nil {
			t.Fatalf("%s: %v", step.writer, err)
		}
	default:
		t.Fatalf("%s writes nothing", step.writer)
	}
}

// foldQuestions is every question the fold test asks of each read of resultsHeadsReads in a
// history: the heads of each issue, whether each commit of each repository a step names heads a
// stored pull request, the merged branch of each issue and of foldLinkingIssue in deliveryRepo,
// and the newest head of each issue. A read it has no question for fails the test.
func foldQuestions(t *testing.T, steps []foldStep) []foldQuestion {
	t.Helper()
	var questions []foldQuestion
	for _, read := range resultsHeadsReads {
		switch read.read {
		case "headsAttributedQuery", "MergeOfIssue", "NewestPullRequestHead":
			for _, issue := range []string{foldIssue, foldLinkingIssue} {
				questions = append(questions, foldQuestion{read: read.read, args: []string{issue}})
			}
		case "HeadStoredAt":
			asked := map[[2]string]bool{}
			for _, step := range steps {
				for _, head := range step.pulls.Heads {
					if commit := [2]string{head.Repo, head.HeadSHA}; !asked[commit] {
						asked[commit] = true
						questions = append(questions, foldQuestion{read: read.read, args: commit[:]})
					}
				}
			}
		case "MergeIn":
			questions = append(questions, foldQuestion{read: read.read, args: []string{foldLinkingIssue, deliveryRepo}})
		case "LandingOf", "MergedPullRequestOf":
			asked := map[[2]string]bool{}
			for _, step := range steps {
				for _, head := range step.pulls.Heads {
					argument := head.HeadRef
					if read.read == "MergedPullRequestOf" {
						argument = fmt.Sprint(head.Number)
					}
					key := [2]string{head.Repo, argument}
					if !asked[key] {
						asked[key] = true
						questions = append(questions, foldQuestion{read: read.read, args: key[:]})
					}
				}
			}
		default:
			t.Fatalf("the fold test asks nothing of the read %s", read.read)
		}
	}
	return questions
}

// foldOracle is the writer's path's answer to each question, by its String. In one transaction,
// rolled back once the reads have run, each answer of the request is stored on the rows the
// writers left, in the order the request read them, as a writer's read-through stores it: through
// upsertHeadRow, and with the row a read asked under an old name kept under that name dropped, as
// upsertHeadUnderCanonicalName drops it. Then the writer's read runs in the transaction, through
// attributor as the read-only read's does.
func foldOracle(t *testing.T, ctx context.Context, pool *store.Pool, attributor *Attributor, steps []foldStep, questions []foldQuestion) map[string]string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the oracle's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for index, step := range steps {
		if step.writer != foldAnswered {
			continue
		}
		for _, head := range step.pulls.Heads {
			if _, err := upsertHeadRow(ctx, tx, head); err != nil {
				t.Fatalf("the oracle's step %d, %s of %s#%d: %v", index, step.writer, head.Repo, head.Number, err)
			}
			if !strings.EqualFold(step.pulls.storedUnder, head.Repo) {
				if _, err := tx.Exec(ctx, `delete from pull_request_heads where repo = $1 and number = $2`,
					step.pulls.storedUnder, head.Number); err != nil {
					t.Fatalf("the oracle's step %d, drop %s#%d: %v", index, step.pulls.storedUnder, head.Number, err)
				}
			}
		}
	}
	answers := map[string]string{}
	for _, question := range questions {
		answer, err := foldRead(ctx, tx, attributor, HeadAnswers{}, question)
		if err != nil {
			t.Fatalf("%s, the writer's read: %v", question, err)
		}
		answers[question.String()] = answer
	}
	return answers
}

// foldAnswerRuns is how many answers a request that read the answers of steps holds: one for each
// run of its answers for one pull request equal in every column of HeadAnswers but checked_at,
// renamed_from among them. The writer's upserts of such a run leave the row as the upsert of its
// last answer alone does, so HeadAnswers.Add lays the run on as that one answer (plan D33).
func foldAnswerRuns(steps []foldStep) int {
	runs := 0
	last := map[string]string{}
	for _, step := range steps {
		if step.writer != foldAnswered {
			continue
		}
		for _, head := range step.pulls.Heads {
			renamedFrom := "-"
			if !strings.EqualFold(step.pulls.storedUnder, head.Repo) {
				renamedFrom = step.pulls.storedUnder
			}
			pull := fmt.Sprintf("%s#%d", head.Repo, head.Number)
			answer := fmt.Sprintf("%s %s %s %s %t %s %s", head.HeadSHA, head.BaseRef, head.State,
				foldColumn(head.IssueKey), head.AttributionComplete, foldColumn(head.EventAt), renamedFrom)
			if answer != last[pull] {
				runs++
			}
			last[pull] = answer
		}
	}
	return runs
}

// foldColumn is a column's value as text, and "-" for null.
func foldColumn[T any](value *T) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprint(*value)
}

// foldRead asks question of its read on q, through attributor, with answers: the pool and the
// request's answers for the read-only read, and the oracle's transaction and none for the
// writer's.
func foldRead(ctx context.Context, q refs.Queryer, attributor *Attributor, answers HeadAnswers, question foldQuestion) (string, error) {
	args := question.args
	switch question.read {
	case "headsAttributedQuery":
		heads, err := attributor.HeadsAttributedTo(ctx, q, args[0], answers)
		return describeAttributedHeads(heads), err
	case "HeadStoredAt":
		stored, err := HeadStoredAt(ctx, q, args[0], args[1], answers)
		return fmt.Sprint(stored), err
	case "MergeOfIssue":
		merged, err := attributor.MergeOfIssue(ctx, q, args[0], answers)
		return fmt.Sprintf("%+v", merged), err
	case "MergeIn":
		merged, err := attributor.MergeIn(ctx, q, args[0], args[1], answers)
		return fmt.Sprintf("%+v", merged), err
	case "NewestPullRequestHead":
		head, found, err := NewestPullRequestHead(ctx, q, args[0], answers)
		return describeNewestHead(head, found), err
	case "LandingOf":
		merged, err := LandingOf(ctx, q, args[0], args[1], answers)
		return fmt.Sprintf("%+v", merged), err
	case "MergedPullRequestOf":
		var number int
		if _, err := fmt.Sscan(args[1], &number); err != nil {
			return "", err
		}
		merged, found, err := MergedPullRequestOf(ctx, q, args[0], number, answers)
		return fmt.Sprintf("%+v, %t", merged, found), err
	}
	return "", fmt.Errorf("no read answers %s", question.read)
}

// describeAttributedHeads is heads in short, in their order: each pull request's number, head,
// two times and linked repository.
func describeAttributedHeads(heads []AttributedHead) string {
	described := make([]string, 0, len(heads))
	for _, head := range heads {
		described = append(described, fmt.Sprintf("#%d at %.7s read %s updated %s linked %q",
			head.Number, head.HeadSHA, describeFoldTime(head.CheckedAt), describeFoldTime(head.EventAt), head.LinkedRepo))
	}
	return "[" + strings.Join(described, "; ") + "]"
}

// describeNewestHead is NewestPullRequestHead's answer in short.
func describeNewestHead(head NewestHead, found bool) string {
	if !found {
		return "no head"
	}
	return fmt.Sprintf("%s#%d at %.7s", head.Repo, head.Number, head.HeadSHA)
}

// describeFoldTime is a stored time in UTC, and "-" for none.
func describeFoldTime(moment *time.Time) string {
	if moment == nil {
		return "-"
	}
	return moment.UTC().Format(time.TimeOnly)
}
