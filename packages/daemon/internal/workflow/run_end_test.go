package workflow

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Every move of the table that leaves a step names the end of the run it leaves: the result the
// table's trigger gives it, which the record does not hold, so a move that named none would end
// its run stopped whatever it was. Each row is moved on an issue of its own, standing at the
// row's phase with an open pull request; the move's named end is read before the fact's settle,
// and the end the settle writes after it.
func TestEveryMoveOutOfAStepNamesItsEnd(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	moved := 0
	for i, row := range Table {
		from, _, _, runs := stepOf(row.From)
		if !runs {
			continue
		}
		moved++
		name := fmt.Sprintf("%s --%s--> %s", row.From, row.Trigger, row.To)
		key := fmt.Sprintf("LEGION-%d", 400+i)
		issue := record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: key, Project: "LEGION",
			Title: name, Phase: row.From, Generation: 1, Status: PhaseStatus(row.From), Rank: "U", LastDispatchSeq: 1}
		seedIssue(t, pool, issue)
		seedTreeLifecycle(t, pool, key)
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: key, Repo: "sjawhar/legion", Number: 400 + i,
			Branch: "legion/" + key, HeadSHA: "head", Failing: []string{}})
		target := phase.Phase("")
		if row.Trigger == TriggerBackward {
			target = row.To
		}
		var named StepEnd
		var found bool
		applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
			pr, err := engine.store.PullRequest(ctx, tx, key)
			if err != nil {
				return err
			}
			if err := engine.transition(ctx, tx, issue, row.Trigger, target, record.PhaseRow{}, pr, "the table test's move"); err != nil {
				return err
			}
			runs, err := factRunsOf(ctx)
			if err != nil {
				return err
			}
			named, found = runs.ends[runAt{issue: key, generation: 1, place: from}]
			return nil
		})
		want, _ := endOf(row.Trigger)
		if !found {
			t.Errorf("%s leaves a step and names no end", name)
			continue
		}
		if named.Result != want {
			t.Errorf("%s names the end %q, want %q", name, named.Result, want)
		}
		var ends []string
		for _, w := range writesAt(stepWrites(t, pool, key), from.Kind, from.Place) {
			if isEnd(w) {
				ends = append(ends, resultOf(w))
			}
		}
		if !slices.Equal(ends, []string{string(want)}) {
			t.Errorf("%s wrote the ends %v of the step it left, want one, %s", name, ends, want)
		}
	}
	if moved < 15 {
		t.Fatalf("moved %d rows of the table out of a step, want every row that leaves one", moved)
	}
}

// endWriters is every call, in the daemon's non-test code, of a writer of a run's end — the
// engine's endStep and the store's two marks of an end — by the file and the function it is in.
func endWriters(t *testing.T) map[string]int {
	t.Helper()
	writers := map[string]bool{"endStep": true, "EndStepStart": true, "EndUncountedStep": true}
	calls := map[string]int{}
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && writers[sel.Sel.Name] {
					calls[fmt.Sprintf("%s:%s:%s", filepath.ToSlash(path), fn.Name.Name, sel.Sel.Name)]++
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the daemon's code: %v", err)
	}
	return calls
}

// No way out of a step writes the end of its run itself: the fact's settle is the one caller of
// the one writer of an end (settleRuns, endStep), and that writer the one caller of the store's
// marks of an end. A call anywhere else — a new file's included — fails by name.
func TestOnlyTheSettleWritesTheEndOfARun(t *testing.T) {
	want := map[string]int{
		"../workflow/run_end.go:settleRuns:endStep":     2,
		"../workflow/steps.go:endStep:EndStepStart":     1,
		"../workflow/steps.go:endStep:EndUncountedStep": 1,
	}
	got := endWriters(t)
	for site, calls := range got {
		if want[site] != calls {
			t.Errorf("%s calls a writer of a run's end %d times, want %d: only the fact's settle ends a run", site, calls, want[site])
		}
	}
	for site, calls := range want {
		if got[site] != calls {
			t.Errorf("%s calls a writer of a run's end %d times, want %d", site, got[site], calls)
		}
	}
}

// The end rule (runOver, mergeCanLand) reads four inputs: the issue's record, its pull request, its
// hand-over fence and a tree's linger close. A write of one of them that no settle reads leaves the
// run it ends unended. So:
//   - each store writer of the first three keeps the issue among those its transaction touched
//     (noteIssueWrite), which the settle reads; the linger close is named to the fact itself
//     (closeLinger), which refuses a ctx no fact opened (errOutsideFact);
//   - each call of those writers is in a package whose writes run in a fact or the boot's
//     reconcile, both of which settle (workflow, admit), or is listed here: the outbox's three
//     drops of a fence, each in a transaction that settles as a fact does (its settling).
//
// A writer that stops noting the issue, or a call added anywhere else, fails by name.
func TestEveryWriterOfTheEndRulesInputsIsSettled(t *testing.T) {
	writers := map[string]bool{"PutIssue": true, "PutPullRequest": true, "PutHandover": true, "DeleteHandover": true, "DeleteHandoverAt": true}
	noting := map[string]bool{}
	calls := map[string]int{}
	settles := map[string]bool{}
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case strings.HasPrefix(slashed, "../record/") && writers[fn.Name.Name] && sel.Sel.Name == "noteIssueWrite":
					noting[fn.Name.Name] = true
				case writers[sel.Sel.Name] && !strings.HasPrefix(slashed, "../workflow/") && !strings.HasPrefix(slashed, "../admit/") &&
					!strings.HasPrefix(slashed, "../record/"):
					calls[fmt.Sprintf("%s:%s:%s", slashed, fn.Name.Name, sel.Sel.Name)]++
				case sel.Sel.Name == "settling":
					settles[fmt.Sprintf("%s:%s", slashed, fn.Name.Name)] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the daemon's code: %v", err)
	}
	for writer := range writers {
		if !noting[writer] {
			t.Errorf("record's %s writes an input of the end rule and notes no issue (noteIssueWrite): the settle would not read the run it ends", writer)
		}
	}
	// The outbox's three drops of a fence, and the function that runs each in a settling
	// transaction: handOverRule.refused's drop runs in finishRefused's, and the drop of a cancelled
	// fence in the transaction of handOverHolds's own read.
	want := map[string]string{
		"../daemon/outbox.go:refused:DeleteHandoverAt":       "../daemon/outbox.go:finishRefused",
		"../daemon/outbox.go:dropHandover:DeleteHandoverAt":  "../daemon/outbox.go:dropHandover",
		"../daemon/outbox.go:handOverHolds:DeleteHandoverAt": "../daemon/outbox.go:handOverHolds",
	}
	for site, n := range calls {
		settled, listed := want[site]
		if !listed || n != 1 {
			t.Errorf("%s writes an input of the end rule %d times outside a fact: write it in a fact, or in a transaction that settles as one does, and list it here", site, n)
			continue
		}
		if !settles[settled] {
			t.Errorf("%s runs %s's write in no settling transaction", settled, site)
		}
	}
	for site := range want {
		if calls[site] != 1 {
			t.Errorf("%s calls a writer of an input of the end rule %d times, want 1", site, calls[site])
		}
	}
}

// A hand-over fence an earlier linger of the tree left can stand beside a member whose merge waits
// in the tree's linger now (record.Handover outlives its linger). It is that earlier linger's, so
// it hands the member to no readmit: the unmerged close of the member's pull request after this
// linger's deadline ends the merge stopped. Only a fence of this tree at the generation of the
// linger the merge waits in keeps the merge open (mergeCanLand).
func TestAnUnmergedCloseEndsAMergeWhoseFenceIsAnEarlierLingers(t *testing.T) {
	pool := migratedPool(t)
	root := stepRoot(t, pool, phase.Reviewing)
	root.Generation = 2
	seedIssue(t, pool, root)
	engine := stepEngine(nil)
	mergingChild(t, pool, engine)
	apply := applyFacts(t, pool, engine)
	apply("park-root", intake.DispatchIssue{Key: root.Key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated",
		Status: "backlog", Title: "root", Rank: "U"})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return engine.store.PutHandover(t.Context(), tx, record.Handover{Issue: "LEGION-209", Tree: root.Tree, Generation: root.Generation - 1})
	})
	if _, end := runOf(stepWrites(t, pool, "LEGION-209"), "merge", 0); isEnd(end) {
		t.Fatalf("the park ended LEGION-209's merge %+v; a merge that awaits its landing in the linger stays open", end)
	}
	parked := issueRecord(t, pool, root.Key)
	engine.cfg.Clock = func() time.Time { return parked.LingerUntil.Add(time.Second) }
	apply("close-unmerged", intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 43, HeadSHA: "head-43"})
	start, end := runOf(stepWrites(t, pool, "LEGION-209"), "merge", 0)
	if !isEnd(end) || end.ID != start.ID {
		t.Fatalf("after the unmerged close past the deadline LEGION-209's merge is %+v then %+v, want its start and its end", start, end)
	}
	assertEnd(t, end, "merge", 0, 3, "stopped", nil)
}
