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
	// function is the read's one entry point, which both callers call.
	function string
	// statement is the name of the package-level constant or variable that holds the read's one
	// statement text: the text after its WITH list. function must reach it through the package's
	// own functions and values, and no other exported function may reach it but through function.
	statement string
}

// resultsHeadsReads is the registry of the reads of pull_request_heads on the results path: the
// five reads of plan D30, each with its one function and its one statement.
// TestEachHeadsReadOnTheResultsPathIsOneStatement holds the package to it, and
// TestTheReadOnlyFoldAnswersAsTheWritersPath asks each read of it its questions (foldQuestions).
var resultsHeadsReads = []resultsHeadsRead{
	{read: "headsAttributedQuery", function: "HeadsAttributedTo", statement: "headsAttributedQuery"},
	{read: "HeadStoredAt", function: "HeadStoredAt", statement: "headStoredAtQuery"},
	{read: "BaseBranchMergedFor", function: "BaseBranchMergedFor", statement: "baseBranchMergedForQuery"},
	{read: "baseBranchMergedInQuery", function: "BaseBranchMergedIn", statement: "baseBranchMergedInQuery"},
	{read: "NewestPullRequestHead", function: "NewestPullRequestHead", statement: "newestPullRequestHeadQuery"},
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

// TestEachHeadsReadOnTheResultsPathIsOneStatement is plan D30's inventory of the reads of
// pull_request_heads on the results path, and plan D32's rule that a read-only read keeps nothing
// after its request. It parses the package's non-test files, as go build selects them, and fails
// on each of these, naming it:
//
//   - a type that keeps answers past the request that read them (answerShapes), and every function
//     or method that takes one, as a parameter or as its receiver;
//   - a function or method that takes the request's answers (requestAnswers) and that the registry
//     does not name (resultsHeadsReads, answeredReads);
//   - a name in the registry that the package does not declare;
//   - a read whose statement is not a constant or variable of the package, or is not reached from
//     the read's function, or is reached from another exported function: a second entry point.
func TestEachHeadsReadOnTheResultsPathIsOneStatement(t *testing.T) {
	source := parsePackageSource(t)

	keeping := source.keepingTypes()
	for _, name := range slices.Sorted(maps.Keys(keeping)) {
		t.Errorf("%s: the type %s holds %s, so it keeps answers past the request that read them. "+
			"A read-only results read keeps nothing after its request (plan D32): pass the answers to that request's reads alone",
			source.positions.Position(source.types[name].Pos()), name, keeping[name])
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
	// refers is the names each function, method, constant and variable refers to, by its key.
	refers map[string]map[string]bool
	// declared is the keys a name may refer to: a function, a constant or a variable of that
	// name, and every method of that name.
	declared map[string][]string
}

// packageFunction is one function or method of the package, with its key: its name, or
// Type.Method for a method.
type packageFunction struct {
	key  string
	decl *ast.FuncDecl
}

// parsePackageSource parses the non-test files go build selects for the package in the test's
// own directory.
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
	for _, name := range listed.GoFiles {
		file, err := parser.ParseFile(source.positions, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
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
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						source.types[spec.Name.Name] = spec
					case *ast.ValueSpec:
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
	}
	return source
}

// declare records key, which name refers to, and the names node refers to as key's.
func (s *packageSource) declare(name, key string, node ast.Node) {
	s.declared[name] = append(s.declared[name], key)
	refers := map[string]bool{}
	if node != nil {
		ast.Inspect(node, func(node ast.Node) bool {
			if ident, isIdent := node.(*ast.Ident); isIdent {
				refers[ident.Name] = true
			}
			return true
		})
	}
	s.refers[key] = refers
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
// methods, constants and variables, never through the declaration keyed avoid ("" avoids none).
// A name is read as every declaration of the package it could refer to, so a method call reaches
// every method of that name: the test asks whether the statement is run at all, not by which
// path.
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
// path (resultsHeadsReads) to the writer's own path, history by history (plan D30). A history is
// the writers' writes, each run through the writer's own statement on the history's database, and
// then the answers one read-only request learned from GitHub, in the order it read them. The
// oracle is the writer's own path (foldOracle): the request's answers stored on the rows the
// writers left, as a writer's read-throughs store them, in a transaction rolled back once the
// writer's read has run in it. The read-only form runs on the rows the writers left, with the
// request's answers, and must give the oracle's answer to every question (foldQuestions).
func TestTheReadOnlyFoldAnswersAsTheWritersPath(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 11, 20, 9, minute, 0, 0, time.UTC) }
	read := func(minute int) *time.Time { moment := at(minute); return &moment }
	sha := func(digit string) string { return strings.Repeat(digit, 40) }
	// The heads a history writes or answers: deliveryRepo's pull request number at a commit, open,
	// credited to nobody by a read of every source and carrying no time, then changed by the
	// functions after it.
	head := func(number int, commit string) PullRequestHead {
		return foldHead(deliveryRepo, number, commit)
	}
	credited := func(head PullRequestHead, issue string) PullRequestHead {
		head.IssueKey, head.AttributionSource = &issue, "title"
		return head
	}
	incomplete := func(head PullRequestHead) PullRequestHead {
		head.AttributionComplete = false
		return head
	}
	timed := func(head PullRequestHead, event time.Time, readAt *time.Time) PullRequestHead {
		head.EventAt, head.CheckedAt = &event, readAt
		return head
	}
	merged := func(head PullRequestHead, base, commit string) PullRequestHead {
		head.State, head.BaseRef, head.MergeCommitSHA = "merged", base, commit
		return head
	}
	delivered := func(head PullRequestHead) foldStep {
		return foldStep{writer: foldDelivery, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: head.Repo}}
	}
	readThrough := func(head PullRequestHead) foldStep {
		return foldStep{writer: foldReadThrough, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: head.Repo}}
	}
	answeredUnder := func(under string, head PullRequestHead) foldStep {
		return foldStep{writer: foldAnswered, pulls: CommitPulls{Heads: []PullRequestHead{head}, storedUnder: under}}
	}
	answered := func(head PullRequestHead) foldStep {
		return answeredUnder(head.Repo, head)
	}

	for _, history := range []struct {
		name  string
		steps []foldStep
	}{
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
	} {
		t.Run(history.name, func(t *testing.T) {
			pool, ctx := deliveryTestPool(t)
			seedFoldIssues(t, ctx, pool)
			var request foldRequest
			answering := false
			for index, step := range history.steps {
				if step.writer == foldAnswered {
					answering = true
					foldAnswer(&request, step)
					continue
				}
				if answering {
					t.Fatalf("step %d, %s, comes after an answer of the request: the fold lays one request's answers "+
						"on the rows stored when its statements run, so every writer's step comes first", index, step.writer)
				}
				foldWrite(t, ctx, pool, step)
			}
			questions := foldQuestions(t, history.steps)
			want := foldOracle(t, ctx, pool, history.steps, questions)
			for _, question := range questions {
				got, err := foldRead(ctx, pool, request.answers, question)
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
// gadgetsOldRepo was renamed to, and links foldLinkingIssue to deliveryRepo's pull request 5.
func seedFoldIssues(t *testing.T, ctx context.Context, pool *store.Pool) {
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
	linked, err := LinkedPullRequests(ctx, pool, foldLinkingIssue)
	if err != nil || len(linked) != 1 || linked[0] != (LinkedPullRequest{Repo: deliveryRepo, Number: 5}) {
		t.Fatalf("the pull requests %s links = %v, %v; want %s#5 alone", foldLinkingIssue, linked, err, deliveryRepo)
	}
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
		case "headsAttributedQuery", "BaseBranchMergedFor", "NewestPullRequestHead":
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
		case "baseBranchMergedInQuery":
			questions = append(questions, foldQuestion{read: read.read, args: []string{foldLinkingIssue, deliveryRepo}})
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
// upsertHeadUnderCanonicalName drops it. Then the writer's read runs in the transaction.
func foldOracle(t *testing.T, ctx context.Context, pool *store.Pool, steps []foldStep, questions []foldQuestion) map[string]string {
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
		answer, err := foldRead(ctx, tx, HeadAnswers{}, question)
		if err != nil {
			t.Fatalf("%s, the writer's read: %v", question, err)
		}
		answers[question.String()] = answer
	}
	return answers
}

// The fold test reaches the reads, the read-only one and the writer's one its oracle runs, through
// foldRequest, foldAnswer and foldRead alone, so the histories can be run against another form of
// the reads by changing these and nothing else.

// foldRequest is the answers one read-only request learned, as the fold test hands them to the
// reads.
type foldRequest struct {
	answers HeadAnswers
}

// foldAnswer adds step, an answer the request learned from GitHub, to request, in the order the
// request read it.
func foldAnswer(request *foldRequest, step foldStep) {
	request.answers.Add(step.pulls)
}

// foldRead asks question of its read on q, with answers: the pool and the request's answers for
// the read-only read, and the oracle's transaction and none for the writer's.
func foldRead(ctx context.Context, q refs.Queryer, answers HeadAnswers, question foldQuestion) (string, error) {
	args := question.args
	switch question.read {
	case "headsAttributedQuery":
		heads, err := HeadsAttributedTo(ctx, q, args[0], answers)
		return describeAttributedHeads(heads), err
	case "HeadStoredAt":
		stored, err := HeadStoredAt(ctx, q, args[0], args[1], answers)
		return fmt.Sprint(stored), err
	case "BaseBranchMergedFor":
		branch, err := BaseBranchMergedFor(ctx, q, args[0], answers)
		return fmt.Sprintf("%q", branch), err
	case "baseBranchMergedInQuery":
		branch, err := BaseBranchMergedIn(ctx, q, args[0], args[1], answers)
		return fmt.Sprintf("%q", branch), err
	case "NewestPullRequestHead":
		head, found, err := NewestPullRequestHead(ctx, q, args[0], answers)
		return describeNewestHead(head, found), err
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
