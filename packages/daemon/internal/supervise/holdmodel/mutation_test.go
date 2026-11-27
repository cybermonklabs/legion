package holdmodel

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	mutationOutput   = flag.String("hold-mutation-output", "", "run the state-write mutations and write their table to this path")
	mutationFilter   = flag.String("hold-mutation-filter", "", "run only mutants whose site matches this regexp")
	mutationParallel = flag.Int("hold-mutation-parallel", max(1, runtime.NumCPU()/2), "mutants run at once")
	mutationDepth    = flag.Int("hold-mutation-depth", 2, "state-machine depth each mutant is explored to")
	mutationDeeper   = flag.Int("hold-mutation-deeper", 0, "explore each mutant nothing caught again to this depth (0: no second pass)")
)

// The files whose transitions write state (rule 7 of the INFRA-3149 architect's
// decisions on INFRA-3904), and the one timer-selection function outside them.
// driver.go and oracles.go are the driver and the oracles, which a decision
// cannot reach, so nothing there is a transition write.
var mutationFiles = []string{"step.go", "prompt.go", "suspension.go", "transaction.go", "enabled.go", "state.go"}

// stateRoots are the names a transition writes state through: the decision,
// its row copies, the world and history it points at, the pending write's
// cause, and the cause set a stop snapshots.
var stateRoots = map[string]bool{"d": true, "r": true, "w": true, "h": true, "q": true, "i": true, "cause": true, "bits": true}

// effectCalls are the calls through which a transition reports, for the
// driver's ledger, something no row records: an eligible success, a lost
// answer, a receipt, a rebuilt machine, and a runtime stop's pause and
// launch snapshot. Each is a write of its own and is mutated like one. The
// effect vocabulary the driver's own fault enumeration is built from
// (`add`: the write attempts, commits, read-backs, sends and replacements)
// reports nothing to the ledger and is not mutated, as it was not before.
var effectCalls = map[string]bool{"put": true, "lose": true}

type stateMutation struct {
	ID, File, Func, Target, Kind, Replace string
	// Stmt is the changed statement's text, which with Func keys the row.
	Stmt             string
	Line, Start, End int
}

type mutationResult struct {
	stateMutation
	Outcome, Catcher string
	overlay          string
}

func reasonKey(m stateMutation) string {
	return m.Func + ": " + m.Stmt + " | " + m.Kind + " " + m.Target
}

// seedOnlyNotes names, for each mutant that only a seed catches, the design
// choice that seed pins rather than an invariant. A seed-only mutant with no
// note is a missing oracle and fails the run (rule 7).
var seedOnlyNotes = map[string]string{}

// TestStateWriteMutations runs each mutant of a state write against the
// state machine at -hold-mutation-depth (2 by default), the planted-oracle
// controls and the D10 restore plant, and each mutant no oracle caught again
// at -hold-mutation-deeper. The table separates the mutants an oracle named,
// the mutants only a seed caught, and the mutants only a planted control
// caught. A mutant nothing caught must then pass the equivalence check: its
// whole behaviour surface (TestModelSurface) must equal the unmutated
// model's, and a mutant whose surface differs is a gap and fails the run.
func TestStateWriteMutations(t *testing.T) {
	if *mutationOutput == "" {
		t.Skip("set -hold-mutation-output to run the state-write mutations")
	}
	pkg, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	mutants, excluded, err := collectStateMutations(pkg)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("generated=%d excluded-plant-doors=%d\n", len(mutants), excluded)
	if *mutationFilter != "" {
		re := regexp.MustCompile(*mutationFilter)
		var kept []stateMutation
		for _, m := range mutants {
			if re.MatchString(fmt.Sprintf("%s %s:%d %s %s %s", m.ID, m.File, m.Line, m.Func, m.Kind, m.Target)) {
				kept = append(kept, m)
			}
		}
		mutants = kept
	}
	if len(mutants) == 0 {
		t.Fatal("UNKNOWN: no mutant was generated, so nothing was measured")
	}
	base, err := os.MkdirTemp("", "holdmodel-mutation-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)

	results := make([]mutationResult, len(mutants))
	runAll := func(indices []int, depth int, record func(int, mutationResult)) {
		jobs := make(chan int)
		var wg sync.WaitGroup
		for range *mutationParallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					record(index, runStateMutation(pkg, base, mutants[index], depth))
				}
			}()
		}
		for _, index := range indices {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
	}
	all := make([]int, len(mutants))
	for index := range all {
		all[index] = index
	}
	// The controls first: the unmutated model must pass the very run each
	// mutant gets, at each depth, or a mutant's failure there catches nothing.
	deeperControl := make(chan error, 1)
	if *mutationDeeper > *mutationDepth {
		go func() { deeperControl <- controlModel(pkg, *mutationDeeper) }()
	}
	if err := controlModel(pkg, *mutationDepth); err != nil {
		t.Fatalf("UNKNOWN: %v", err)
	}
	fmt.Printf("control depth=%d passed\n", *mutationDepth)
	runAll(all, *mutationDepth, func(index int, r mutationResult) { results[index] = r })
	deeper := *mutationDeeper > *mutationDepth
	if deeper {
		if err := <-deeperControl; err != nil {
			// The depth-2 results stand; no depth-3 run would measure anything.
			t.Errorf("UNKNOWN: %v; no mutant is run again at depth %d", err, *mutationDeeper)
			deeper = false
		}
	}
	if deeper {
		fmt.Printf("control depth=%d passed\n", *mutationDeeper)
		var again []int
		for index, r := range results {
			if r.Outcome != "caught" && r.Outcome != "invalid" {
				again = append(again, index)
			}
		}
		fmt.Printf("deeper=%d again=%d\n", *mutationDeeper, len(again))
		unmeasured := make([]string, len(mutants))
		runAll(again, *mutationDeeper, func(index int, r mutationResult) {
			switch r.Outcome {
			case "caught":
				r.Catcher = fmt.Sprintf("depth %d: %s", *mutationDeeper, r.Catcher)
				results[index] = r
			case "timeout", "invalid":
				unmeasured[index] = r.Outcome + ": " + r.Catcher
			}
		})
		for index, why := range unmeasured {
			if why != "" {
				t.Errorf("UNKNOWN: %s gave no depth-%d result (%s)", mutants[index].ID, *mutationDeeper, why)
			}
		}
	}

	// Every mutant nothing caught must be shown equivalent by a run, not
	// argued for in prose: its behaviour surface must equal the unmutated
	// model's over the depth-2 continuations of every seed prefix.
	var survivors []int
	for index, r := range results {
		if r.Outcome == "survived" {
			survivors = append(survivors, index)
		}
	}
	if len(survivors) != 0 {
		want := filepath.Join(base, "surface-unmutated.txt")
		if out, err := runSurface(pkg, "", want); err != nil {
			t.Fatalf("UNKNOWN: the unmutated surface did not run (%v): %s", err, out)
		}
		fmt.Printf("survivors=%d equivalence-surface=%s\n", len(survivors), want)
		var mu sync.Mutex
		jobs := make(chan int)
		var wg sync.WaitGroup
		for range *mutationParallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for index := range jobs {
					got := filepath.Join(base, mutants[index].ID+"-surface.txt")
					outcome, catcher := "equivalent", ""
					if out, err := runSurface(pkg, results[index].overlay, got); err != nil {
						outcome, catcher = "unmeasured", "the surface did not run: "+firstLine(out)
					} else if diff, err := surfaceDiff(want, got); err != nil {
						outcome, catcher = "unmeasured", "the surfaces could not be compared: "+err.Error()
					} else if diff != "" {
						outcome, catcher = "survived", "behaviour differs, no oracle: "+diff
					}
					mu.Lock()
					results[index].Outcome, results[index].Catcher = outcome, catcher
					mu.Unlock()
					os.Remove(got)
				}
			}()
		}
		for _, index := range survivors {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
	}

	counts := map[string]int{}
	used := map[string]bool{}
	for _, r := range results {
		counts[r.Outcome]++
		if r.Outcome == "caught" {
			continue
		}
		t.Logf("%s %s %s:%d %s %s: %s", r.Outcome, r.ID, r.File, r.Line, r.Kind, r.Target, r.Catcher)
		switch r.Outcome {
		case "seed-only":
			key := reasonKey(r.stateMutation)
			used[key] = true
			if seedOnlyNotes[key] == "" {
				t.Errorf("%s %s:%d %s %s of %q is caught by a seed and by no oracle, and names no design choice the seed pins (seedOnlyNotes)", r.ID, r.File, r.Line, r.Func, r.Kind, r.Stmt)
			}
		case "survived":
			t.Errorf("%s %s:%d %s %s of %q changes the model's behaviour and no oracle, seed or plant caught it: %s", r.ID, r.File, r.Line, r.Func, r.Kind, r.Stmt, r.Catcher)
		case "unmeasured":
			t.Errorf("UNKNOWN: %s %s:%d %s", r.ID, r.File, r.Line, r.Catcher)
		}
	}
	if *mutationFilter == "" {
		for key := range seedOnlyNotes {
			if !used[key] {
				t.Errorf("seedOnlyNotes[%q] names no seed-only mutant: delete it", key)
			}
		}
	}
	fmt.Printf("mutants=%d caught=%d seed-only=%d plant-only=%d equivalent=%d survived=%d invalid=%d timeout=%d unmeasured=%d\n",
		len(results), counts["caught"], counts["seed-only"], counts["plant-only"], counts["equivalent"],
		counts["survived"], counts["invalid"], counts["timeout"], counts["unmeasured"])
	if err := writeMutationTable(*mutationOutput, results, excluded); err != nil {
		t.Fatal(err)
	}
	if counts["invalid"] != 0 {
		t.Errorf("%d mutants did not compile: the generator is wrong", counts["invalid"])
	}
	if counts["timeout"] != 0 {
		t.Errorf("UNKNOWN: %d mutants gave no depth-%d result in time", counts["timeout"], *mutationDepth)
	}
}

// plantOn is each condition under which only a test-only plant's door runs.
// Their bodies, and writes of a plant-only field, are not transition writes.
var plantOn = map[string]bool{
	"d.skipFatalApply": true, "d.W.FatalStuck && d.FatalSeen": true, "d.badFatalClose": true,
	"d.forceFatalFailure": true, "d.intentDropsPause": true, "d.load != nil": true, "d.store != nil": true,
}

func collectStateMutations(pkg string) ([]stateMutation, int, error) {
	var all []stateMutation
	excluded := 0
	for _, name := range mutationFiles {
		path := filepath.Join(pkg, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, 0, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return nil, 0, err
		}
		text := func(n ast.Node) string {
			return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
		}
		var doors [][2]int
		ast.Inspect(file, func(n ast.Node) bool {
			if s, ok := n.(*ast.IfStmt); ok && plantOn[text(s.Cond)] {
				doors = append(doors, [2]int{fset.Position(s.Body.Pos()).Offset, fset.Position(s.Body.End()).Offset})
			}
			return true
		})
		add := func(fn string, n ast.Node, target, kind, replace string) {
			start := fset.Position(n.Pos()).Offset
			for _, door := range doors {
				if start >= door[0] && start < door[1] {
					excluded++
					return
				}
			}
			if strings.Contains(target, "FatalStuck") {
				excluded++
				return
			}
			all = append(all, stateMutation{
				File: name, Func: fn, Target: target, Kind: kind, Replace: replace,
				Stmt: strings.Join(strings.Fields(text(n)), " "),
				Line: fset.Position(n.Pos()).Line, Start: start, End: fset.Position(n.End()).Offset,
			})
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			if name == "state.go" {
				// Only the scheduler's selection lives here; the rest of
				// state.go is reading, not writing.
				if fn == "armed" {
					timerSelectionInArmed(fd, text, func(n ast.Node, target string) { add(fn, n, target, "timer: drop selection", "/* mutant */") })
				}
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncLit:
					return false // test-only plant doors are not transition writes
				case *ast.AssignStmt:
					assignmentMutants(n, text, func(node ast.Node, target, kind, replace string) { add(fn, node, target, kind, replace) })
				case *ast.IncDecStmt:
					if writesState(n.X, text) {
						target := text(n.X)
						add(fn, n, target, "drop", "_ = "+target)
						add(fn, n, target, "zero", target+" = mutationZero("+target+")")
					}
				case *ast.ExprStmt:
					callMutants(n, text, func(node ast.Node, target, kind, replace string) { add(fn, node, target, kind, replace) })
				case *ast.BinaryExpr:
					if name == "enabled.go" && n.Op == token.LEQ && callsArmed(n.Y) {
						add(fn, n, text(n), "timer: select out of order", "true")
					}
				}
				return true
			})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].Start < all[j].Start
	})
	for i := range all {
		all[i].ID = fmt.Sprintf("M%03d", i+1)
	}
	return all, excluded, nil
}

// callMutants covers the two writes a statement can make without an
// assignment: a copy into a state array, and the effect a transition reports
// (the only thing the driver's ledger reads).
func callMutants(n *ast.ExprStmt, text func(ast.Node) string, add func(ast.Node, string, string, string)) {
	call, ok := n.X.(*ast.CallExpr)
	if !ok {
		return
	}
	var args []string
	for _, a := range call.Args {
		args = append(args, text(a))
	}
	drop := "_ = []any{" + strings.Join(args, ", ") + "}"
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if fun.Name == "copy" && len(call.Args) == 2 && writesState(call.Args[0], text) {
			add(n, text(call.Args[0]), "drop", drop)
		}
	case *ast.SelectorExpr:
		if effectCalls[fun.Sel.Name] && stateRoots[rootName(fun.X)] {
			add(n, text(call.Fun), "drop reported effect", drop)
		}
	}
}

func assignmentMutants(n *ast.AssignStmt, text func(ast.Node) string, add func(ast.Node, string, string, string)) {
	if n.Tok == token.DEFINE {
		definedValueMutants(n, text, add)
		return
	}
	var targets []string
	for _, l := range n.Lhs {
		if writesState(l, text) {
			targets = append(targets, text(l))
		}
	}
	if len(targets) == 0 {
		return
	}
	// A dropped write still evaluates its operands, so no variable that only
	// the write used is left unused and the mutant compiles.
	var operands []string
	for _, e := range append(append([]ast.Expr{}, n.Lhs...), n.Rhs...) {
		operands = append(operands, text(e))
	}
	add(n, strings.Join(targets, ", "), "drop", "_ = []any{"+strings.Join(operands, ", ")+"}")
	if len(n.Lhs) > 1 && len(n.Lhs) == len(n.Rhs) {
		for skip := range n.Lhs {
			if !writesState(n.Lhs[skip], text) {
				continue
			}
			var lhs, rhs []string
			for j := range n.Lhs {
				if j != skip {
					lhs, rhs = append(lhs, text(n.Lhs[j])), append(rhs, text(n.Rhs[j]))
				}
			}
			add(n, text(n.Lhs[skip]), "member left out", strings.Join(lhs, ", ")+" "+n.Tok.String()+" "+strings.Join(rhs, ", "))
		}
		return
	}
	if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
		return
	}
	lhs := text(n.Lhs[0])
	if n.Tok == token.ASSIGN && !isZeroLiteral(n.Rhs[0]) {
		// The old value is passed and ignored, so a variable only it used
		// stays used and the mutant compiles.
		add(n, lhs, "zero", lhs+" = mutationZero("+lhs+", "+text(n.Rhs[0])+")")
	}
	compositeMemberMutants(n.Rhs[0], lhs, lhs+" "+n.Tok.String()+" ", text, add, n)
}

// definedValueMutants covers a value a transition builds under a state name
// and then writes into state whole (the wire request of prompt.go, the stop's
// intent of suspension.go). Each member of the value is a write of its own.
func definedValueMutants(n *ast.AssignStmt, text func(ast.Node) string, add func(ast.Node, string, string, string)) {
	if len(n.Lhs) != 1 || len(n.Rhs) != 1 || !stateRoots[rootName(n.Lhs[0])] {
		return
	}
	lhs := text(n.Lhs[0])
	compositeMemberMutants(n.Rhs[0], lhs, lhs+" := ", text, add, n)
}

// compositeMemberMutants leaves out one member of a struct literal at a time:
// a keyed member by removing it, a positional one by zeroing it, so that both
// forms of the literal compile.
func compositeMemberMutants(rhs ast.Expr, lhs, prefix string, text func(ast.Node) string, add func(ast.Node, string, string, string), n ast.Node) {
	lit, ok := rhs.(*ast.CompositeLit)
	if !ok || len(lit.Elts) < 2 || lit.Type == nil {
		return
	}
	for skip, elt := range lit.Elts {
		var kept []string
		if kv, keyed := elt.(*ast.KeyValueExpr); keyed {
			for j, other := range lit.Elts {
				if j != skip {
					kept = append(kept, text(other))
				}
			}
			add(n, lhs+"."+text(kv.Key), "member left out", prefix+text(lit.Type)+"{"+strings.Join(kept, ", ")+"}")
			continue
		}
		for j, other := range lit.Elts {
			member := text(other)
			if j == skip {
				member = "mutationZero(" + member + ")"
			}
			kept = append(kept, member)
		}
		add(n, fmt.Sprintf("%s member %d (%s)", lhs, skip, text(elt)), "member zeroed", prefix+text(lit.Type)+"{"+strings.Join(kept, ", ")+"}")
	}
}

func writesState(e ast.Expr, text func(ast.Node) string) bool {
	return stateRoots[rootName(e)]
}

func rootName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return rootName(e.X)
	case *ast.IndexExpr:
		return rootName(e.X)
	case *ast.ParenExpr:
		return rootName(e.X)
	case *ast.StarExpr:
		return rootName(e.X)
	case *ast.SliceExpr:
		return rootName(e.X)
	}
	return ""
}

func isZeroLiteral(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Value == "0"
	case *ast.Ident:
		return e.Name == "false" || e.Name == "nil" || e.Name == "NoWrite"
	case *ast.CompositeLit:
		return len(e.Elts) == 0
	}
	return false
}

func callsArmed(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "armed"
}

func timerSelectionInArmed(fd *ast.FuncDecl, text func(ast.Node) string, add func(ast.Node, string)) {
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if a, ok := n.(*ast.AssignStmt); ok && a.Tok == token.ASSIGN && len(a.Lhs) == 1 && text(a.Lhs[0]) == "t" {
			add(a, text(a.Rhs[0]))
		}
		return true
	})
}

var maskPattern = regexp.MustCompile(`invariant mask (\d+)`)
var witnessPattern = regexp.MustCompile(`model_test\.go:\d+: ([A-Za-z0-9-]+): `)
var plantPattern = regexp.MustCompile(`missed (?:plant|scheduler plant) ?([A-Za-z0-9-]*)`)

// A seed's own failure, as replaySeed reports it: the verdict that failed, or
// the event the mutant disabled, at that seed step.
var verdictPattern = regexp.MustCompile(`model_test\.go:\d+: (.+?\[\d+\]) want (\S+), got `)
var disabledPattern = regexp.MustCompile(`model_test\.go:\d+: (.+?\[\d+\]) disabled (\S+): `)

// mutantOverlay writes the mutated file and the overlay that replaces the
// original with it, and returns the overlay's path.
func mutantOverlay(pkg, dir string, m stateMutation) (string, error) {
	source := filepath.Join(pkg, m.File)
	src, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	mutated := append(append(append([]byte{}, src[:m.Start]...), m.Replace...), src[m.End:]...)
	if strings.Contains(m.Replace, "mutationZero(") {
		mutated = append(mutated, "\nfunc mutationZero[T any](T, ...any) T { var zero T; return zero }\n"...)
	}
	variant := filepath.Join(dir, m.File)
	overlay := filepath.Join(dir, "overlay.json")
	spec, _ := json.Marshal(map[string]map[string]string{"Replace": {source: variant}})
	if err := os.WriteFile(variant, mutated, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(overlay, spec, 0o644); err != nil {
		return "", err
	}
	return overlay, nil
}

// runSurface writes the behaviour surface of the tree, mutated by the given
// overlay or unmutated when it is empty.
func runSurface(pkg, overlay, out string) (string, error) {
	args := []string{"test", "-count=1", "-vet=off", "-timeout=2h"}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	args = append(args, "-run", "^TestModelSurface$", "./internal/supervise/holdmodel", "-hold-surface-output="+out)
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Clean(filepath.Join(pkg, "..", "..", ".."))
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func runStateMutation(pkg, base string, m stateMutation, depth int) mutationResult {
	result := mutationResult{stateMutation: m}
	dir, err := os.MkdirTemp(base, m.ID+"-")
	if err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	overlay, err := mutantOverlay(pkg, dir, m)
	if err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	result.overlay = overlay
	out, err, timedOut := runModel(pkg, overlay, depth)
	switch {
	case timedOut:
		result.Outcome, result.Catcher = "timeout", "no result in "+depthLimit(depth).String()
	case err == nil:
		result.Outcome, result.Catcher = "survived", ""
	case strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]"):
		result.Outcome, result.Catcher = "invalid", firstLine(out)
	default:
		result.Outcome, result.Catcher = mutationCatcher(out)
	}
	return result
}

// depthLimit is how long one run at depth may take. A deeper search takes
// longer than go test's default 10 minutes; its timeout is no catch: the
// mutant was not measured.
func depthLimit(depth int) time.Duration {
	if depth > 2 {
		return 2 * time.Hour
	}
	return 15 * time.Minute
}

// runModel runs the state machine to depth, the planted-oracle controls and
// the D10 restore plant, on the tree mutated by overlay, or unmutated when
// overlay is empty.
func runModel(pkg, overlay string, depth int) (out string, err error, timedOut bool) {
	limit := depthLimit(depth)
	ctx, cancel := context.WithTimeout(context.Background(), limit+time.Minute)
	defer cancel()
	args := []string{"test", "-count=1", "-vet=off", "-timeout=" + limit.String()}
	if overlay != "" {
		args = append(args, "-overlay", overlay)
	}
	args = append(args, "-run", "^(TestTheHoldStateMachine|TestPlants|TestCleanRestartSendPlant)$",
		"./internal/supervise/holdmodel", "-hold-depth="+strconv.Itoa(depth))
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = filepath.Clean(filepath.Join(pkg, "..", "..", ".."))
	b, err := cmd.CombinedOutput()
	out = string(b)
	return out, err, ctx.Err() != nil || strings.Contains(out, "panic: test timed out after")
}

// controlModel is the run every mutant gets, on the unmutated model. A
// mutant's failure is a catch only where this passes: at 87bd6c73 the
// unmutated model failed at depth 3, and 49 of one shard's 51 depth-3 rows
// named that model's own deadline violation as a catch of theirs.
func controlModel(pkg string, depth int) error {
	out, err, timedOut := runModel(pkg, "", depth)
	if timedOut || err != nil {
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "model_test.go:") || strings.HasPrefix(line, "FAIL") {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
		return fmt.Errorf("the unmutated model fails the mutants' run at depth %d (timed out: %v; %v): %s", depth, timedOut, err, strings.Join(lines[:min(len(lines), 5)], " | "))
	}
	return nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// mutationCatcher names what failed, sorted so the table is stable: each
// oracle a witness or a mask names, then any seed verdict or disabled seed
// event, then any planted control that went blind. The outcome is the
// strongest of the three: an oracle named the mutant, only a seed caught it,
// or only a control's reach changed.
func mutationCatcher(out string) (outcome, names string) {
	oracles, seeds, plants := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, m := range witnessPattern.FindAllStringSubmatch(out, -1) {
		oracles[m[1]] = true
	}
	for _, m := range maskPattern.FindAllStringSubmatch(out, -1) {
		mask, _ := strconv.Atoi(m[1])
		for bit := uint16(1); bit != 0 && bit <= lastInvariant; bit <<= 1 {
			if uint16(mask)&bit != 0 {
				oracles[invariantName(bit)] = true
			}
		}
	}
	// TestPlants replays the seeds too, so each failure is counted once.
	verdicts, disabled := map[string]bool{}, map[string]bool{}
	for _, m := range verdictPattern.FindAllStringSubmatch(out, -1) {
		verdicts[m[1]+" :"+m[2]] = true
	}
	for _, m := range disabledPattern.FindAllStringSubmatch(out, -1) {
		disabled[m[1]+" "+m[2]] = true
	}
	for kind, set := range map[string]map[string]bool{"seed verdict": verdicts, "seed event disabled": disabled} {
		if len(set) == 0 {
			continue
		}
		list := make([]string, 0, len(set))
		for name := range set {
			list = append(list, name)
		}
		sort.Strings(list)
		more := ""
		if len(list) > 3 {
			more = fmt.Sprintf(" and %d more", len(list)-3)
			list = list[:3]
		}
		seeds[kind+" ("+strings.Join(list, "; ")+more+")"] = true
	}
	if len(verdicts) == 0 && strings.Contains(out, " want ") {
		seeds["seed verdict"] = true
	}
	if len(disabled) == 0 && strings.Contains(out, " disabled ") {
		seeds["seed event disabled"] = true
	}
	if strings.Contains(out, "panic:") {
		oracles["panic"] = true
	}
	for _, m := range plantPattern.FindAllStringSubmatch(out, -1) {
		plants["plant blind: "+m[1]] = true
	}
	if strings.Contains(out, "missed the second request after a restore") {
		plants["plant blind: D10-restore-without-fence"] = true
	}
	sorted := func(set map[string]bool) []string {
		var list []string
		for name := range set {
			list = append(list, name)
		}
		sort.Strings(list)
		return list
	}
	all := append(append(sorted(oracles), sorted(seeds)...), sorted(plants)...)
	// A planted control that went blind names the oracle whose reach the
	// mutant changed, which is more than a seed verdict says.
	switch {
	case len(oracles) != 0:
		return "caught", strings.Join(all, ", ")
	case len(plants) != 0:
		return "plant-only", strings.Join(all, ", ")
	case len(seeds) != 0:
		return "seed-only", strings.Join(all, ", ")
	}
	return "caught", "test failed: " + firstLine(out)
}

var mutationSections = []struct{ outcome, title, note string }{
	{"caught", "Caught by an oracle", "The search, from a named seed prefix, reported the invariant below."},
	{"seed-only", "Caught by a seed and by no oracle", "The depth-2 search ran and reported no invariant; a named seed failed. Each row names the design choice that seed pins (`seedOnlyNotes`)."},
	{"plant-only", "Caught by a planted control going blind", "No oracle and no seed failed, but a planted defect the run injects stopped being caught, so the mutant changed an oracle's reach."},
	{"equivalent", "Equivalent: no oracle, and the run shows no difference", "The whole behaviour surface of the mutant (memory, store, world, pending write, ledger, enabled events and effects over the depth-2 continuations of every seed prefix) is byte for byte the unmutated model's."},
	{"survived", "Survived: a gap", "Nothing caught the mutant and its behaviour surface differs from the unmutated model's."},
	{"invalid", "Did not compile", "The generator is wrong."},
	{"timeout", "Unmeasured", "No result in time. A timeout is no catch."},
	{"unmeasured", "Unmeasured", "The equivalence check did not run."},
}

func writeMutationTable(path string, results []mutationResult, excluded int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Hold-model state-write mutations at depth %d\n\n", *mutationDepth)
	fmt.Fprintf(&b, "Each row changes one place a transition writes state or reports an effect, in `%s`, or the timer selection in `enabled.go` and `state.go`'s `armed`. A mutant is caught when the depth-%d search, a named seed or a planted-oracle control fails", strings.Join(mutationFiles, "`, `"), *mutationDepth)
	if *mutationDeeper > *mutationDepth {
		fmt.Fprintf(&b, ", or, for a mutant no oracle caught there, the same at depth %d (`depth %d:` in the last column)", *mutationDeeper, *mutationDeeper)
	}
	b.WriteString(". A mutant nothing caught is then run through the equivalence check.\n\n")
	fmt.Fprintf(&b, "%d further sites are test-only plant doors (a body under `%s`, or a write of the plant-only `World.FatalStuck`) and are not mutated.\n", excluded, "d.store != nil`, `d.load != nil`, `d.badFatalClose`, `d.skipFatalApply`, `d.intentDropsPause`, `d.forceFatalFailure`, `d.W.FatalStuck && d.FatalSeen")
	counts := map[string]int{}
	for _, r := range results {
		counts[r.Outcome]++
	}
	b.WriteString("\n| Result | Rows |\n| --- | --- |\n")
	for _, s := range mutationSections {
		if counts[s.outcome] != 0 {
			fmt.Fprintf(&b, "| %s | %d |\n", s.title, counts[s.outcome])
		}
	}
	for _, s := range mutationSections {
		if counts[s.outcome] == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s (%d)\n\n%s\n\n", s.title, counts[s.outcome], s.note)
		b.WriteString("| ID | Place | Write | Mutant | Caught by, or what the run shows |\n| --- | --- | --- | --- | --- |\n")
		for _, r := range results {
			if r.Outcome != s.outcome {
				continue
			}
			last := r.Catcher
			if r.Outcome == "seed-only" {
				if note := seedOnlyNotes[reasonKey(r.stateMutation)]; note != "" {
					last += "; " + note
				}
			}
			fmt.Fprintf(&b, "| %s | `%s:%d` %s | `%s` | %s | %s |\n", r.ID, r.File, r.Line, r.Func,
				strings.ReplaceAll(r.Target, "|", `\|`), r.Kind, strings.ReplaceAll(last, "|", `\|`))
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
