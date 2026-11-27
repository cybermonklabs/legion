package delivery

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"testing"
)

// A read-only caller's read of an issue's results stores nothing and keeps nothing after its
// request (plan D32), and answers what a writer's read answers: each write the writer's
// read-throughs would make is made by the writer's own code in a transaction that is rolled back,
// kept for the request (HeadAnswers), and made again before each later read of the request
// (HeadAnswers.Read). The tests hold the parts of that: no type keeps a request's answers past it,
// and every function that takes the answers is registered (TestAReadOnlyReadKeepsNothingPastItsRequest,
// here); each read of the results path reads what the writer's committed writes leave and stores
// nothing, history by history (TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave); and a write
// another session commits while a read-only read makes its writes again leaves the read where the
// committed writes leave it (TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave,
// both in read_only_histories_test.go).

// answerShapes is each type that holds GitHub's answers to one results read: the writes one
// request's read-throughs kept (HeadAnswers), the default branch and the unseen pairs its branch
// reads carry (BranchCarry), and where one read-through keeps what it learned (facts). A request
// makes each of them and drops it with the request. Any other type of the package whose fields
// hold one, directly or through another such type, keeps answers past the request that read them.
var answerShapes = []string{"HeadAnswers", "BranchCarry", "facts"}

// processHolds is each type of the package that the server holds for the process and that a
// results read reads (api's server.branches, server.commitPulls and server.linkedHeads), with the
// fields it holds: what a writer's read writes there, the default branches and the compare holds,
// and the claims and the settles. A read-only read reads them and writes none of them (plan D32).
// Any other field keeps something across requests whatever type it is, such as a memory of the
// answers read-only reads learned, so the inventory fails on it until this list names it.
var processHolds = map[string][]string{
	"Branches":        {"mu", "known", "compares"},
	"CommitPullReads": {"mu", "notBefore"},
}

// answeredReads is each function and method of the package that takes the request's answers
// (HeadAnswers) as a parameter, and what it is: a read of the results path, which
// readOnlyQuestions asks in every history of TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave
// (readAsked); a read-through that keeps its writes in the answers (keptThrough), which the
// histories' request steps make through the same writer code (readOnlyStep.keep); or the one
// function a registered read runs through (innerRead). A function that takes the answers and is
// not here is a read no history asks, and so a read that could ignore the request's writes with
// no test failing: TestAReadOnlyReadKeepsNothingPastItsRequest fails on it.
var answeredReads = map[string]string{
	"Attributor.HeadsAttributedTo":        readAsked,
	"Attributor.NewestPullRequestHead":    readAsked,
	"Attributor.MergeOfIssue":             readAsked,
	"Attributor.MergeIn":                  readAsked,
	"LandingOf":                           readAsked,
	"MergedPullRequestOf":                 readAsked,
	"Attributor.PullRequestsNotKnown":     readAsked,
	"Attributor.LinkedHeadsUnread":        readAsked,
	"Branches.ReadCommitOnBranch":         readAsked,
	"Branches.readCommitOnBranch":         innerRead,
	"readMerged":                          innerRead,
	"Attributor.AnswerCommitPulls":        keptThrough,
	"Attributor.AnswerCommitsPulls":       keptThrough,
	"Attributor.AnswerLinkedHead":         keptThrough,
	"Attributor.RefreshMergedPullRequest": keptThrough,
}

const (
	readAsked   = "a read of the results path"
	keptThrough = "a read-through that keeps its writes in the answers"
	innerRead   = "the function a registered read runs through"
)

// TestAReadOnlyReadKeepsNothingPastItsRequest is plan D32's inventory: it parses the package's
// non-test files, as go build selects them, and fails on each type that keeps answers past the
// request that read them (answerShapes), each function or method that takes such a type, as a
// parameter or as its receiver, each field of a type the server holds for the process that
// processHolds does not name, or that it names and the type does not hold, and each function that
// takes the request's answers as a parameter and is not in answeredReads, or is there and is not
// declared.
func TestAReadOnlyReadKeepsNothingPastItsRequest(t *testing.T) {
	t.Parallel()
	types, functions, positions := parsePackageSource(t)
	keeping := keepingTypes(types)
	for _, name := range slices.Sorted(maps.Keys(keeping)) {
		t.Errorf("%s: the type %s holds %s, so it keeps answers past the request that read them. "+
			"A read-only results read keeps nothing after its request (plan D32): pass the answers to that request's reads alone",
			positions.Position(types[name].Pos()), name, keeping[name])
	}
	declared := map[string]bool{}
	for _, function := range functions {
		key := functionKey(function)
		declared[key] = true
		if kept := takes(function, keeping); kept != "" {
			t.Errorf("%s: %s takes answers kept across requests, in %s (plan D32)",
				positions.Position(function.Pos()), key, kept)
		}
		if takesAnswers(function) && answeredReads[key] == "" {
			t.Errorf("%s: %s takes the request's answers (HeadAnswers), and answeredReads names it nowhere: no history asks it, "+
				"so it could read the stored rows without the request's writes and no test would fail. Register it, and ask it "+
				"in readOnlyQuestions where it is a read of the results path", positions.Position(function.Pos()), key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(answeredReads)) {
		if !declared[key] {
			t.Errorf("answeredReads names %s, which is no function or method of the package", key)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(processHolds)) {
		spec, declared := types[name]
		if !declared {
			t.Errorf("processHolds names %s, which is no type of the package", name)
			continue
		}
		structure, isStruct := spec.Type.(*ast.StructType)
		if !isStruct {
			t.Errorf("%s: processHolds names %s, which is no struct", positions.Position(spec.Pos()), name)
			continue
		}
		held := map[string]bool{}
		for _, field := range structure.Fields.List {
			for _, fieldName := range fieldNames(field) {
				held[fieldName] = true
				if !slices.Contains(processHolds[name], fieldName) {
					t.Errorf("%s: %s holds %s, which processHolds does not name: the server holds a %s for the process, so the "+
						"field keeps what it holds across requests. A read-only results read keeps nothing after its request "+
						"(plan D32): carry what it learns for that request alone (HeadAnswers, BranchCarry)",
						positions.Position(field.Pos()), name, fieldName, name)
				}
			}
		}
		for _, field := range processHolds[name] {
			if !held[field] {
				t.Errorf("processHolds names %s.%s, which %s does not hold", name, field, name)
			}
		}
	}
}

// functionKey is a function's name, and a method's as Type.Name.
func functionKey(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	receiver := function.Recv.List[0].Type
	if pointer, isPointer := receiver.(*ast.StarExpr); isPointer {
		receiver = pointer.X
	}
	if named, isIdent := receiver.(*ast.Ident); isIdent {
		return named.Name + "." + function.Name.Name
	}
	return function.Name.Name
}

// takesAnswers is whether a parameter of function, its receiver left out, mentions HeadAnswers in
// its type, by value or through a pointer.
func takesAnswers(function *ast.FuncDecl) bool {
	for _, field := range function.Type.Params.List {
		if typeMentions(field.Type)["HeadAnswers"] {
			return true
		}
	}
	return false
}

// parsePackageSource parses the non-test files go build selects for the package in the test's
// own directory: every package-level type by name, and every function and method.
func parsePackageSource(t *testing.T) (map[string]*ast.TypeSpec, []*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	listed, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	positions := token.NewFileSet()
	types := map[string]*ast.TypeSpec{}
	var functions []*ast.FuncDecl
	for _, name := range listed.GoFiles {
		file, err := parser.ParseFile(positions, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				functions = append(functions, decl)
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if spec, isType := spec.(*ast.TypeSpec); isType {
						types[spec.Name.Name] = spec
					}
				}
			}
		}
	}
	return types, functions, positions
}

// keepingTypes is every type of the package but answerShapes whose fields hold an answer shape,
// directly or through another such type, each with the shape or the type it holds.
func keepingTypes(types map[string]*ast.TypeSpec) map[string]string {
	keeping := map[string]string{}
	names := slices.Sorted(maps.Keys(types))
	for changed := true; changed; {
		changed = false
		for _, name := range names {
			if slices.Contains(answerShapes, name) || keeping[name] != "" {
				continue
			}
			for _, held := range slices.Sorted(maps.Keys(typeMentions(types[name].Type))) {
				if slices.Contains(answerShapes, held) || keeping[held] != "" {
					keeping[name], changed = held, true
					break
				}
			}
		}
	}
	return keeping
}

// takes is the first of names that a parameter or the receiver of function mentions in its
// type, and "" where none does.
func takes(function *ast.FuncDecl, names map[string]string) string {
	fields := slices.Clone(function.Type.Params.List)
	if function.Recv != nil {
		fields = append(fields, function.Recv.List...)
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
	switch embedded := embedded.(type) {
	case *ast.SelectorExpr:
		return []string{embedded.Sel.Name}
	case *ast.Ident:
		return []string{embedded.Name}
	}
	return nil
}
