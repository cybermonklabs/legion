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
)

// The files whose transitions write state (rule 7 of the INFRA-3149 architect's
// decisions on INFRA-3904), and the one timer-selection function outside them.
var mutationFiles = []string{"step.go", "prompt.go", "suspension.go", "transaction.go", "enabled.go", "state.go"}

// stateRoots are the names a transition writes state through: the decision,
// its row copies, the world and history it points at, the pending write's
// cause, and the cause set a stop snapshots.
var stateRoots = map[string]bool{"d": true, "r": true, "w": true, "h": true, "q": true, "i": true, "cause": true, "bits": true}

type stateMutation struct {
	ID, File, Func, Target, Kind, Replace string
	Line, Start, End                      int
}

type mutationResult struct {
	stateMutation
	Outcome, Catcher string
}

// TestStateWriteMutations runs each mutant of a state write against the
// state machine at -hold-mutation-depth (2 by default), the planted-oracle
// controls and the D10 restore plant. A mutant is caught when any of them
// fails; the table names the oracle or seed that failed.
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
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range *mutationParallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				results[index] = runStateMutation(pkg, base, mutants[index])
			}
		}()
	}
	for index := range mutants {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	counts := map[string]int{}
	for _, r := range results {
		counts[r.Outcome]++
		if r.Outcome != "caught" {
			t.Logf("%s %s %s:%d %s %s: %s", r.Outcome, r.ID, r.File, r.Line, r.Kind, r.Target, r.Catcher)
		}
	}
	fmt.Printf("mutants=%d caught=%d plant-only=%d survived=%d invalid=%d timeout=%d\n", len(results), counts["caught"], counts["plant-only"], counts["survived"], counts["invalid"], counts["timeout"])
	if err := writeMutationTable(*mutationOutput, results, excluded); err != nil {
		t.Fatal(err)
	}
	if counts["invalid"] != 0 {
		t.Errorf("%d mutants did not compile: the generator is wrong", counts["invalid"])
	}
}

// plantOn is each condition under which only a test-only plant's door runs.
// Their bodies, and writes of a plant-only field, are not transition writes.
var plantOn = map[string]bool{
	"d.skipFatalApply": true, "d.W.FatalStuck && d.H.FatalPending": true, "d.badFatalClose": true,
	"d.forceFatalFailure": true, "d.intentDropsPause": true, "d.load != nil": true, "d.store != nil": true,
}

// oracleFuncs are the oracles and the driver's step around a transition. A
// local they write is the oracle's own reading, not transition state.
var oracleFuncs = map[string]bool{
	"execute": true, "fatalClose": true, "fatalCloseBad": true, "recoveryMutation": true,
	"recoveryAuthorized": true, "appliedInput": true, "attemptBoundBad": true, "settleOne": true,
	"settleBad": true, "checkServed": true, "checkCommit": true, "Check": true,
	"creditedOpen": true, "creditPromptOpen": true, "bootMemoryBad": true,
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
				Line: fset.Position(n.Pos()).Line, Start: start, End: fset.Position(n.End()).Offset,
			})
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			if oracleFuncs[fn] {
				continue // the oracles and the driver's step, not transitions
			}
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

func assignmentMutants(n *ast.AssignStmt, text func(ast.Node) string, add func(ast.Node, string, string, string)) {
	if n.Tok == token.DEFINE {
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
	if lit, ok := n.Rhs[0].(*ast.CompositeLit); ok && len(lit.Elts) > 1 {
		for skip, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				break // a positional literal cannot lose one member and compile
			}
			var kept []string
			for j, other := range lit.Elts {
				if j != skip {
					kept = append(kept, text(other))
				}
			}
			add(n, lhs+"."+text(kv.Key), "member left out", lhs+" "+n.Tok.String()+" "+text(lit.Type)+"{"+strings.Join(kept, ", ")+"}")
		}
	}
}

func writesState(e ast.Expr, text func(ast.Node) string) bool {
	if t := text(e); strings.Contains(t, "H.Bad") || strings.HasPrefix(t, "h.Bad") {
		return false // oracle accounting, not transition state
	}
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

func runStateMutation(pkg, base string, m stateMutation) mutationResult {
	result := mutationResult{stateMutation: m}
	dir, err := os.MkdirTemp(base, m.ID+"-")
	if err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	source := filepath.Join(pkg, m.File)
	src, err := os.ReadFile(source)
	if err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	mutated := append(append(append([]byte{}, src[:m.Start]...), m.Replace...), src[m.End:]...)
	if strings.Contains(m.Replace, "mutationZero(") {
		mutated = append(mutated, "\nfunc mutationZero[T any](T, ...any) T { var zero T; return zero }\n"...)
	}
	variant := filepath.Join(dir, m.File)
	overlay := filepath.Join(dir, "overlay.json")
	spec, _ := json.Marshal(map[string]map[string]string{"Replace": {source: variant}})
	if err := os.WriteFile(variant, mutated, 0o644); err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	if err := os.WriteFile(overlay, spec, 0o644); err != nil {
		result.Outcome, result.Catcher = "invalid", err.Error()
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-vet=off", "-overlay", overlay,
		"-run", "^(TestTheHoldStateMachine|TestPlants|TestCleanRestartSendPlant)$",
		"./internal/supervise/holdmodel", "-hold-depth="+strconv.Itoa(*mutationDepth))
	cmd.Dir = filepath.Clean(filepath.Join(pkg, "..", "..", ".."))
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		result.Outcome, result.Catcher = "timeout", "no result in 15 minutes"
	case err == nil:
		result.Outcome, result.Catcher = "survived", ""
	case strings.Contains(string(out), "[build failed]") || strings.Contains(string(out), "[setup failed]"):
		result.Outcome, result.Catcher = "invalid", firstLine(string(out))
	default:
		catcher, byOracle := mutationCatcher(string(out))
		result.Outcome, result.Catcher = "caught", catcher
		if !byOracle {
			// Only a planted control went blind: the mutant weakened an
			// oracle's reach, but no oracle saw the mutant itself.
			result.Outcome = "plant-only"
		}
	}
	return result
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// mutationCatcher names what failed, sorted so the table is stable: each
// oracle a witness or a seed's mask names, a seed's verdict or a disabled
// seed event, a panic, and last any plant that went blind under the mutant.
// byOracle is whether anything but a blind plant failed.
func mutationCatcher(out string) (names string, byOracle bool) {
	oracles, plants := map[string]bool{}, map[string]bool{}
	for _, m := range witnessPattern.FindAllStringSubmatch(out, -1) {
		oracles[m[1]] = true
	}
	for _, m := range maskPattern.FindAllStringSubmatch(out, -1) {
		mask, _ := strconv.Atoi(m[1])
		for bit := uint16(1); bit != 0 && bit <= FatalClose; bit <<= 1 {
			if uint16(mask)&bit != 0 {
				oracles[invariantName(bit)] = true
			}
		}
	}
	if strings.Contains(out, " want ") {
		oracles["seed verdict"] = true
	}
	if strings.Contains(out, " disabled ") {
		oracles["seed event disabled"] = true
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
	all := append(sorted(oracles), sorted(plants)...)
	if len(all) == 0 {
		return "test failed: " + firstLine(out), true
	}
	return strings.Join(all, ", "), len(oracles) != 0
}

func writeMutationTable(path string, results []mutationResult, excluded int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Hold-model state-write mutations at depth %d\n\n", *mutationDepth)
	b.WriteString("Each row changes one place a transition writes state in `step.go`, `prompt.go`, `suspension.go`, `transaction.go` or `enabled.go`, or the timer selection in `enabled.go` and `state.go`'s `armed`. A mutant is caught when the depth-2 search, a named seed or a planted-oracle control fails; `plant-only` means only a planted control went blind.\n\n")
	fmt.Fprintf(&b, "%d further sites are test-only plant doors (a body under `%s`, or a write of the plant-only `World.FatalStuck`) and are not mutated.\n\n", excluded, "d.store != nil`, `d.load != nil`, `d.badFatalClose`, `d.skipFatalApply`, `d.intentDropsPause`, `d.forceFatalFailure`, `d.W.FatalStuck && d.H.FatalPending")
	b.WriteString("| ID | Place | Write | Mutant | Result | Caught by |\n| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | `%s:%d` %s | `%s` | %s | %s | %s |\n", r.ID, r.File, r.Line, r.Func,
			strings.ReplaceAll(r.Target, "|", `\|`), r.Kind, r.Outcome, strings.ReplaceAll(r.Catcher, "|", `\|`))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
