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

// A read-only caller's read of an issue's results stores nothing and keeps nothing after its
// request (plan D32), and answers what a writer's read answers: each write the writer's
// read-throughs would make is made by the writer's own code in a transaction that is rolled back,
// kept for the request (HeadAnswers), and made again before each later read of the request
// (HeadAnswers.Read). The tests here hold the three parts of that: no type keeps a request's
// answers past it (TestAReadOnlyReadKeepsNothingPastItsRequest), each read of the results path
// reads what the writer's writes would leave and stores nothing
// (TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave), and a write another session commits
// while a read-only read makes its writes again leaves the read where the committed writes leave
// it (TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave).

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

// TestAReadOnlyReadKeepsNothingPastItsRequest is plan D32's inventory: it parses the package's
// non-test files, as go build selects them, and fails on each type that keeps answers past the
// request that read them (answerShapes), each function or method that takes such a type, as a
// parameter or as its receiver, and each field of a type the server holds for the process that
// processHolds does not name, or that it names and the type does not hold.
func TestAReadOnlyReadKeepsNothingPastItsRequest(t *testing.T) {
	t.Parallel()
	types, functions, positions := parsePackageSource(t)
	keeping := keepingTypes(types)
	for _, name := range slices.Sorted(maps.Keys(keeping)) {
		t.Errorf("%s: the type %s holds %s, so it keeps answers past the request that read them. "+
			"A read-only results read keeps nothing after its request (plan D32): pass the answers to that request's reads alone",
			positions.Position(types[name].Pos()), name, keeping[name])
	}
	for _, function := range functions {
		if kept := takes(function, keeping); kept != "" {
			t.Errorf("%s: %s takes answers kept across requests, in %s (plan D32)",
				positions.Position(function.Pos()), function.Name.Name, kept)
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

// readOnlyBand is the pull request numbers of this file's fixtures, 9700 to 9799, which no other
// test of the package writes: the reads that are not keyed by an issue (PullRequestsNotKnown) and
// the stored-nothing snapshot are held to it.
const (
	readOnlyBandLow  = 9700
	readOnlyBandHigh = 9799
)

// readOnlyStep is one write a writer's read-through makes: a head and its texts
// (Attributor.writeFacts), or the settle of the read a pull request is owed (settleOwedRead).
type readOnlyStep struct {
	head   *PullRequestHead
	settle *owedRead
}

// write makes the step's write on db, with the writer's own code.
func (step readOnlyStep) write(ctx context.Context, a *Attributor, db refs.Pool) error {
	if step.head != nil {
		_, err := a.writeFacts(ctx, db, *step.head, step.head.Repo, false)
		return err
	}
	return settleOwedRead(ctx, db, step.settle.Repo, step.settle.Number, step.settle.Revision)
}

// keep makes the step's write for a read-only caller's request, as its read-throughs make it
// (keepFacts, facts.write): in a transaction of pool that holds answers' writes, rolled back, and
// kept in answers.
func (step readOnlyStep) keep(ctx context.Context, a *Attributor, pool *store.Pool, answers *HeadAnswers) error {
	f := facts{pool: pool, answers: answers}
	if step.head != nil {
		_, err := a.keepFacts(ctx, f, *step.head, step.head.Repo, false)
		return err
	}
	settle := *step.settle
	_, err := f.write(ctx, func(ctx context.Context, db refs.Pool) (bool, error) {
		return false, settleOwedRead(ctx, db, settle.Repo, settle.Number, settle.Revision)
	}, nil)
	return err
}

// readOnlyHead is a head of acme/widgets read from GitHub with every source of its issue read, at
// the event time event and a read a minute later: a title that names its issue attributes it
// (attributedTerms).
func readOnlyHead(number int, sha, title string, event time.Time) *PullRequestHead {
	read := event.Add(time.Minute)
	return &PullRequestHead{
		Repo: "acme/widgets", Number: number, HeadSHA: sha, HeadRef: "legion/work", BaseRef: "main", State: "open",
		Title: title, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		EventAt: &event, CheckedAt: &read, GitHubReadAt: &read,
		Texts: &PullRequestTexts{HeadRepo: "acme/widgets", BaseRef: "main", Title: title, CommitMessages: []string{}, MessagesRead: true},
	}
}

// readOnlyQuestions is the answer of each read of the results path a read-only caller's request
// passes its answers to, about issue, on q, in one line each: the heads the issue's checks compare
// with (HeadsAttributedTo), the newest head a check CI proves is derived at
// (NewestPullRequestHead), the branch an `after_merge` check compares with (BaseBranchMergedFor),
// the pull requests of the repository whose answer is not known (PullRequestsNotKnown, held to
// readOnlyBand) and the linked pull requests whose head is not held (LinkedHeadsUnread).
func readOnlyQuestions(ctx context.Context, q Querier, a *Attributor, issue string, answers HeadAnswers) (string, error) {
	var lines []string
	heads, err := a.HeadsAttributedTo(ctx, q, issue, answers)
	if err != nil {
		return "", err
	}
	for _, head := range heads {
		lines = append(lines, fmt.Sprintf("head #%d %s known=%t", head.Number, head.HeadSHA, head.HeadKnown))
	}
	newest, found, err := a.NewestPullRequestHead(ctx, q, issue, answers)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("newest found=%t #%d %s %s owed=%t", found, newest.Number, newest.HeadSHA, newest.State, newest.ReadOwed))
	merged, err := a.BaseBranchMergedFor(ctx, q, issue, answers)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("merged into %q", merged))
	notKnown, err := a.PullRequestsNotKnown(ctx, q, "acme/widgets", answers)
	if err != nil {
		return "", err
	}
	for _, pull := range notKnown {
		if pull.Number >= readOnlyBandLow && pull.Number <= readOnlyBandHigh {
			lines = append(lines, fmt.Sprintf("not known #%d %s", pull.Number, pull.HeadSHA))
		}
	}
	unread, err := a.LinkedHeadsUnread(ctx, q, issue, answers)
	if err != nil {
		return "", err
	}
	for _, link := range unread {
		lines = append(lines, fmt.Sprintf("linked unread %s#%d", link.Repo, link.Number))
	}
	return strings.Join(lines, "\n\t"), nil
}

// readOnlyOracle is the writer's answer to readOnlyQuestions: in a transaction of pool, rolled
// back once the reads have run in it, each step is written with the writer's own code, in order,
// as a writer's read-throughs would have stored it, and the reads run there with no answers.
func readOnlyOracle(t *testing.T, ctx context.Context, pool *store.Pool, a *Attributor, issue string, steps []readOnlyStep) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the oracle's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for index, step := range steps {
		if err := step.write(ctx, a, tx); err != nil {
			t.Fatalf("the oracle's step %d: %v", index, err)
		}
	}
	answer, err := readOnlyQuestions(ctx, tx, a, issue, HeadAnswers{})
	if err != nil {
		t.Fatalf("the oracle's reads: %v", err)
	}
	return answer
}

// readOnlyStored is the rows of readOnlyBand in each table a writer's read-through writes, as
// text: the heads, their texts, the reads owed, and the branch answers of acme/widgets, so a read
// that stored anything changes it.
func readOnlyStored(t *testing.T, ctx context.Context, pool *store.Pool) string {
	t.Helper()
	var stored string
	if err := pool.QueryRow(ctx, `
		select concat_ws(' | ',
			(select string_agg(concat_ws(',', repo, number, head_sha, state, event_at, checked_at, github_read_at), ';' order by repo, number)
				from pull_request_heads where number between $1 and $2),
			(select string_agg(concat_ws(',', repo, number, head_sha, title, updated_at, commit_messages::text), ';' order by repo, number)
				from pull_request_texts where number between $1 and $2),
			(select string_agg(concat_ws(',', repo, number, revision, reason), ';' order by repo, number)
				from pull_request_reads_owed where number between $1 and $2),
			(select string_agg(concat_ws(',', repo, branch, sha, on_branch), ';' order by repo, branch, sha)
				from branch_commits where sha like 'f97%'))
	`, readOnlyBandLow, readOnlyBandHigh).Scan(&stored); err != nil {
		t.Fatalf("read the stored rows of the read-only band: %v", err)
	}
	return stored
}

// readOnlySHA is a commit of this file's fixtures: f97 and then digits, 40 in all.
func readOnlySHA(digits string) string {
	return "f97" + digits + strings.Repeat("0", 37-len(digits))
}

// TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave holds each read of the results path a
// read-only caller's request passes its answers to (readOnlyQuestions) to the writer's own path,
// history by history. A history is the rows the writers stored, and then the writes one read-only
// request's read-throughs kept (readOnlyStep.keep). The oracle is the writer's own path
// (readOnlyOracle): those writes made on the stored rows in a transaction rolled back once the
// writer's reads have run in it. The read-only form reads on the pool with the request's answers,
// and must give the oracle's answer; and the rows of every table a read-through writes must be
// what the writers stored, so the request stored nothing.
//
// The control: in every history where the request's writes change an answer (matters), the
// reads with no answers on the pool, which are a read that dropped the request's writes, must not
// give the oracle's answer, so a read that ignored its answers fails here.
func TestAReadOnlyReadReadsTheRowsTheWritersWritesLeave(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	a := unrenamed()
	base := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	for _, history := range []struct {
		name    string
		issue   int
		stored  func(issue string) []*PullRequestHead
		owes    []int
		steps   func(issue string, owed map[int][]owedRead) []readOnlyStep
		matters bool
	}{{
		name: "an answer moves a stored head", issue: 971,
		stored: func(issue string) []*PullRequestHead {
			return []*PullRequestHead{readOnlyHead(9711, readOnlySHA("11"), issue+" the work", base)}
		},
		steps: func(issue string, _ map[int][]owedRead) []readOnlyStep {
			return []readOnlyStep{{head: readOnlyHead(9711, readOnlySHA("12"), issue+" the work", base.Add(time.Hour))}}
		},
		matters: true,
	}, {
		name: "an older answer moves nothing", issue: 972,
		stored: func(issue string) []*PullRequestHead {
			return []*PullRequestHead{readOnlyHead(9721, readOnlySHA("21"), issue+" the work", base.Add(time.Hour))}
		},
		steps: func(issue string, _ map[int][]owedRead) []readOnlyStep {
			return []readOnlyStep{{head: readOnlyHead(9721, readOnlySHA("22"), issue+" the work", base)}}
		},
	}, {
		name: "an answer of a pull request no row holds", issue: 973,
		steps: func(issue string, _ map[int][]owedRead) []readOnlyStep {
			return []readOnlyStep{{head: readOnlyHead(9731, readOnlySHA("31"), issue+" the work", base)}}
		},
		matters: true,
	}, {
		name: "a merged answer names the branch it merged into", issue: 974,
		stored: func(issue string) []*PullRequestHead {
			return []*PullRequestHead{readOnlyHead(9741, readOnlySHA("41"), issue+" the work", base)}
		},
		steps: func(issue string, _ map[int][]owedRead) []readOnlyStep {
			merged := readOnlyHead(9741, readOnlySHA("41"), issue+" the work", base.Add(time.Hour))
			merged.State, merged.BaseRef, merged.MergeCommitSHA = "merged", "release", readOnlySHA("49")
			merged.Texts.BaseRef = "release"
			return []readOnlyStep{{head: merged}}
		},
		matters: true,
	}, {
		name: "an answer settles the read its pull request is owed", issue: 975,
		stored: func(issue string) []*PullRequestHead {
			return []*PullRequestHead{readOnlyHead(9751, readOnlySHA("51"), issue+" the work", base)}
		},
		owes: []int{9751},
		steps: func(issue string, owed map[int][]owedRead) []readOnlyStep {
			steps := []readOnlyStep{{head: readOnlyHead(9751, readOnlySHA("52"), issue+" the work", base.Add(time.Hour))}}
			for _, read := range owed[9751] {
				steps = append(steps, readOnlyStep{settle: &read})
			}
			return steps
		},
		matters: true,
	}, {
		name: "an answer whose messages are not read is not known", issue: 976,
		steps: func(issue string, _ map[int][]owedRead) []readOnlyStep {
			unread := readOnlyHead(9761, readOnlySHA("61"), "the work of nobody yet", base)
			unread.Texts.CommitMessages, unread.Texts.MessagesRead, unread.CheckedAt = nil, false, nil
			return []readOnlyStep{{head: unread}}
		},
		matters: true,
	}} {
		t.Run(history.name, func(t *testing.T) {
			issue := seedHeadIssue(t, ctx, pool, history.issue)
			if history.stored != nil {
				for _, head := range history.stored(issue) {
					if _, err := a.writeFacts(ctx, pool, *head, head.Repo, false); err != nil {
						t.Fatalf("store #%d: %v", head.Number, err)
					}
				}
			}
			for _, number := range history.owes {
				if err := a.owePullRequestRead(ctx, pool, "acme/widgets", number, owedFailure, ""); err != nil {
					t.Fatalf("owe a read of #%d: %v", number, err)
				}
			}
			steps := history.steps(issue, a.owedReadsIn(ctx, pool, "acme/widgets"))
			before := readOnlyStored(t, ctx, pool)

			var answers HeadAnswers
			for index, step := range steps {
				if err := step.keep(ctx, a, pool, &answers); err != nil {
					t.Fatalf("keep the request's step %d: %v", index, err)
				}
			}
			want := readOnlyOracle(t, ctx, pool, a, issue, steps)
			got, err := readOnlyQuestions(ctx, pool, a, issue, answers)
			if err != nil {
				t.Fatalf("the read-only reads: %v", err)
			}
			t.Logf("the read-only reads answered\n\t%s", got)
			if got != want {
				t.Errorf("the read-only reads answered\n\t%s\nwant the writer's path's\n\t%s", got, want)
			}
			if after := readOnlyStored(t, ctx, pool); after != before {
				t.Errorf("the read-only request stored rows: before\n\t%s\nafter\n\t%s", before, after)
			}
			dropped, err := readOnlyQuestions(ctx, pool, a, issue, HeadAnswers{})
			if err != nil {
				t.Fatalf("the reads with no answers: %v", err)
			}
			if history.matters && dropped == want {
				t.Errorf("CONTROL: the reads with no answers answered the writer's path's\n\t%s\nso this history cannot tell a read "+
					"that drops the request's writes from one that makes them", want)
			}
			if !history.matters && dropped != want {
				t.Errorf("the request's writes change nothing here, yet the reads with no answers answered\n\t%s\nwant\n\t%s", dropped, want)
			}
		})
	}
	t.Run("a merge the request learned is on its base branch", func(t *testing.T) {
		var answers HeadAnswers
		merged := readOnlyHead(9742, readOnlySHA("42"), "HEAD-974 the work", base.Add(time.Hour))
		merged.State, merged.BaseRef, merged.MergeCommitSHA = "merged", "release", readOnlySHA("48")
		merged.Texts.BaseRef = "release"
		if err := (readOnlyStep{head: merged}).keep(ctx, a, pool, &answers); err != nil {
			t.Fatalf("keep the merge: %v", err)
		}
		if !mergeLearnedOnBranch(answers, "Acme/Widgets", "release", readOnlySHA("48")) {
			t.Error("the merge the request kept is not on its base branch, want the yes a writer's store of it writes")
		}
		if mergeLearnedOnBranch(answers, "acme/widgets", "main", readOnlySHA("48")) {
			t.Error("the merge the request kept is on main, want it on its base branch alone")
		}
		var stored int
		if err := pool.QueryRow(ctx, `select count(*) from branch_commits where sha = $1`, readOnlySHA("48")).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != 0 {
			t.Errorf("the request stored %d branch_commits rows of the merge, want none", stored)
		}
	})
}

// TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave holds a read-only read to
// the committed writes while another session writes the same pull request. The row stores #N at
// A. The request kept a write of #N at a newer head B (its read-through ran first). Then a
// writer's session writes #N at C and holds its transaction open, so the read-only read, which
// makes B's write again first, waits on that session's lock of the row
// (storetest.WaitForLockWaiters names it waiting); then the session commits. The oracle is the
// writes committed in the order they were made: B, then C, in a transaction rolled back. The read
// must answer the oracle's heads in both orders of the two event times: C newer than B, where the
// guard keeps C, and C older, where it keeps B.
//
// The control is a request whose kept write is B written over the row with no guard, on a pull
// request of its own: where C is newer, it reads B where the committed writes leave C, and the
// comparison must name the difference, so a read that ignored a write committed in its lock window
// fails here.
func TestAReadOnlyReadWithAConcurrentWriteReadsWhatTheCommittedWritesLeave(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	a := unrenamed()
	base := time.Date(2026, 11, 25, 7, 0, 0, 0, time.UTC)
	// race stores #number at A, keeps the request's write of B (planted: B over the row with no
	// guard), and answers what the committed writes leave (the oracle) and what the read-only reads
	// answer while C's session holds the row and then commits.
	race := func(t *testing.T, issue string, number int, concurrentAt time.Time, planted bool) (got, want string, shas map[string]string) {
		t.Helper()
		shas = map[string]string{
			"A": readOnlySHA(fmt.Sprint(number) + "1"), "B": readOnlySHA(fmt.Sprint(number) + "2"),
			"C": readOnlySHA(fmt.Sprint(number) + "3"),
		}
		stored := readOnlyHead(number, shas["A"], issue+" the work", base)
		if _, err := a.writeFacts(ctx, pool, *stored, stored.Repo, false); err != nil {
			t.Fatalf("store A: %v", err)
		}
		kept := readOnlyHead(number, shas["B"], issue+" the work", base.Add(2*time.Hour))
		concurrent := readOnlyHead(number, shas["C"], issue+" the work", concurrentAt)
		var answers HeadAnswers
		if planted {
			answers.writes = append(answers.writes, func(ctx context.Context, db refs.Pool) error {
				if _, err := db.Exec(ctx, `update pull_request_heads set head_sha = $3, event_at = $4 where repo = $1 and number = $2`,
					kept.Repo, kept.Number, kept.HeadSHA, *kept.EventAt); err != nil {
					return err
				}
				_, err := db.Exec(ctx, `update pull_request_texts set head_sha = $3, updated_at = $4 where repo = $1 and number = $2`,
					kept.Repo, kept.Number, kept.HeadSHA, *kept.EventAt)
				return err
			})
		} else if err := (readOnlyStep{head: kept}).keep(ctx, a, pool, &answers); err != nil {
			t.Fatalf("keep B: %v", err)
		}
		want = readOnlyOracle(t, ctx, pool, a, issue, []readOnlyStep{{head: kept}, {head: concurrent}})

		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("open the concurrent writer's transaction: %v", err)
		}
		defer func() { _ = holder.Rollback(context.WithoutCancel(ctx)) }()
		if _, err := a.writeFacts(ctx, holder, *concurrent, concurrent.Repo, false); err != nil {
			t.Fatalf("write C in the concurrent session: %v", err)
		}
		type answered struct {
			text string
			err  error
		}
		done := make(chan answered, 1)
		go func() {
			text, err := readOnlyQuestions(ctx, pool, a, issue, answers)
			done <- answered{text, err}
		}()
		storetest.WaitForLockWaiters(t, holder, 1)
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("commit C: %v", err)
		}
		result := <-done
		if result.err != nil {
			t.Fatalf("the read-only reads: %v", result.err)
		}
		return result.text, want, shas
	}
	for _, row := range []struct {
		name   string
		issue  int
		number int
		// concurrent is C's event time, B's being base plus two hours and A's base.
		concurrent time.Time
		want       string
	}{
		{name: "the concurrent write is newer", issue: 977, number: 9771, concurrent: base.Add(3 * time.Hour), want: "C"},
		{name: "the concurrent write is older", issue: 978, number: 9781, concurrent: base.Add(time.Hour), want: "B"},
	} {
		t.Run(row.name, func(t *testing.T) {
			issue := seedHeadIssue(t, ctx, pool, row.issue)
			got, want, shas := race(t, issue, row.number, row.concurrent, false)
			if !strings.Contains(want, fmt.Sprintf("head #%d %s ", row.number, shas[row.want])) {
				t.Fatalf("SETUP: the committed writes leave\n\t%s\nwant #%d at %s", want, row.number, row.want)
			}
			t.Logf("the read-only reads, waited on the concurrent write, answered\n\t%s", got)
			if got != want {
				t.Errorf("the read-only reads answered\n\t%s\nwant what the committed writes leave\n\t%s", got, want)
			}
		})
	}
	t.Run("control: a write that ignores the guard reads past the committed write", func(t *testing.T) {
		issue := seedHeadIssue(t, ctx, pool, 979)
		got, want, shas := race(t, issue, 9791, base.Add(3*time.Hour), true)
		t.Logf("the planted request answered\n\t%s\nthe committed writes leave\n\t%s", got, want)
		if got == want {
			t.Errorf("CONTROL: a request whose write ignores the guard answered the committed writes'\n\t%s\nso this test "+
				"cannot tell a read that overrides a write committed in its lock window", want)
		}
		if !strings.Contains(got, fmt.Sprintf("head #9791 %s ", shas["B"])) {
			t.Errorf("CONTROL: the planted request answered\n\t%s\nwant #9791 at B, the write that overrides the committed C", got)
		}
	})
}
