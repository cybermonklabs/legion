package textcheck

//ci:reads packages/envoy/internal/cipher/requests/*_test.go TestEveryBrokerTestSaysWhetherItRunsBesideTheOthers parallelMarkerOffences parses every _test.go file of the package, each build's
//ci:reads packages/envoy/internal/cipher/api/*_test.go TestEveryBrokerTestSaysWhetherItRunsBesideTheOthers parallelMarkerOffences parses every _test.go file of the package, each build's
//ci:reads packages/envoy/cmd/cipherd/*_test.go TestEveryBrokerTestSaysWhetherItRunsBesideTheOthers parallelMarkerOffences parses every _test.go file of the package, each build's

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// parallelMarkedPackages are the packages whose every top-level test says whether it runs beside
// the others (docs/testing.md, "A test in the broker's requests, api or cmd/cipherd package runs
// beside the others"), by the paths from this directory.
var parallelMarkedPackages = []string{"../cipher/requests", "../cipher/api", "../../cmd/cipherd"}

// notParallel is how a serial test's first line starts.
const notParallel = "// Not parallel:"

// citedLine is a line a comment names, which the next edit above that line makes false: a file's
// (logtest.go:36), a bare one (:85), or one in words (line 85).
var citedLine = regexp.MustCompile(`(?i):\d+|\bline \d+`)

// Every top-level test of the broker's requests, api and cmd/cipherd packages calls t.Parallel()
// first or says on its first line why it does not, so a new test cannot run alone for no reason,
// as 46 of the daemon's did by 2026-11-25, nor run beside the others with nothing that says it was
// meant to. A serial test's comment names its call or its bound, never a line, on any of the
// comment's lines (citedLine). The check reads each _test.go file whatever its build constraint,
// so cmd/cipherd's multiprocess tests are held to it too, and needs no database.
func TestEveryBrokerTestSaysWhetherItRunsBesideTheOthers(t *testing.T) {
	offences, tests := parallelMarkerOffences(t, parallelMarkedPackages...)
	if tests == 0 {
		t.Fatalf("read no top-level test in %v, so nothing was checked", parallelMarkedPackages)
	}
	if len(offences) > 0 {
		t.Fatalf("%d of the %d top-level tests break the rule:\n\t%s\n"+
			"begin each with t.Parallel(), or with a %q comment naming the call or bound that makes it serial, never a line "+
			"(docs/testing.md, \"A test in the broker's requests, api or cmd/cipherd package runs beside the others\")",
			len(offences), tests, strings.Join(offences, "\n\t"), notParallel)
	}
}

// The check can fail: a test with neither first line, one with both, and a serial one whose
// comment names a line (a file's, on its first line or its second, a bare one, or one in words)
// are each named, and a parallel test and a serial one that names its call are not.
func TestTheParallelMarkerCheckNamesEachTestThatBreaksTheRule(t *testing.T) {
	dir := t.TempDir()
	source := `package planted

import "testing"

func TestRunsBeside(t *testing.T) {
	t.Parallel()
}

func TestSerialForItsCall(t *testing.T) {
	// Not parallel: it captures the process's default logger (logtest.SetDefault).
	_ = t
}

func TestNeither(t *testing.T) {
	_ = t
}

func TestBoth(t *testing.T) {
	// Not parallel: it sets the environment (t.Setenv).
	t.Parallel()
}

func TestSerialNamingALine(t *testing.T) {
	// Not parallel: it captures the process's default logger (slog.SetDefault at logtest.go:36).
	_ = t
}

func TestSerialNamingALineOnItsSecondLine(t *testing.T) {
	// Not parallel: it captures the process's default logger (logtest.SetDefault,
	// slog.SetDefault at logtest.go:36).
	_ = t
}

func TestSerialNamingABareLine(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check at :85).
	_ = t
}

func TestSerialNamingALineInWords(t *testing.T) {
	// Not parallel: it holds an upper bound on a measured time (the check on line 85).
	_ = t
}

func helper(t *testing.T) {}

func TestMain(m *testing.M) {}
`
	if err := os.WriteFile(filepath.Join(dir, "planted_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	offences, tests := parallelMarkerOffences(t, dir)
	var named []string
	for _, offence := range offences {
		named = append(named, offence[strings.LastIndex(offence, ":")+1:])
	}
	want := []string{"TestBoth", "TestNeither", "TestSerialNamingABareLine", "TestSerialNamingALine", "TestSerialNamingALineInWords", "TestSerialNamingALineOnItsSecondLine"}
	if !slices.Equal(named, want) || tests != 8 {
		t.Fatalf("the check read %d tests and named %v; want 8 tests read and %v named:\n\t%s", tests, named, want, strings.Join(offences, "\n\t"))
	}
}

// parallelMarkerOffences reads every _test.go file of dirs and answers, sorted, one line for each
// top-level test that breaks the rule, and how many top-level tests it read: a function named
// Test<something> with no receiver and one *testing.T parameter, TestMain not among them.
func parallelMarkerOffences(t *testing.T, dirs ...string) ([]string, int) {
	t.Helper()
	var offences []string
	tests := 0
	for _, dir := range dirs {
		paths, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		if len(paths) == 0 {
			t.Fatalf("%s holds no _test.go file, so it reads as checked when nothing was", dir)
		}
		for _, path := range paths {
			files := token.NewFileSet()
			file, err := parser.ParseFile(files, path, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil || !isTopLevelTest(fn) {
					continue
				}
				tests++
				where := filepath.ToSlash(path) + ":" + fn.Name.Name
				first, marker := "", ""
				if group := firstComment(file, fn.Body); group != nil {
					first = group.List[0].Text
					for _, line := range group.List {
						marker += line.Text + "\n"
					}
				}
				serial := strings.HasPrefix(first, notParallel)
				parallel := callsParallelFirst(fn)
				switch {
				case serial && parallel:
					offences = append(offences, "both t.Parallel() and "+notParallel+" "+where)
				case serial && citedLine.MatchString(marker):
					offences = append(offences, "a "+notParallel+" comment naming a line "+where)
				case serial, parallel:
				default:
					offences = append(offences, "neither t.Parallel() first nor a "+notParallel+" first line "+where)
				}
			}
		}
	}
	slices.SortFunc(offences, func(a, b string) int {
		return strings.Compare(a[strings.LastIndex(a, " ")+1:], b[strings.LastIndex(b, " ")+1:])
	})
	return offences, tests
}

// isTopLevelTest is whether fn is a test go test runs: Test, then nothing or a name that does not
// start with a lower-case letter, and one parameter of type *testing.T.
func isTopLevelTest(fn *ast.FuncDecl) bool {
	name := fn.Name.Name
	if !strings.HasPrefix(name, "Test") || name == "TestMain" {
		return false
	}
	if rest := name[len("Test"):]; rest != "" && rest[0] >= 'a' && rest[0] <= 'z' {
		return false
	}
	params := fn.Type.Params.List
	if len(params) != 1 || len(params[0].Names) > 1 {
		return false
	}
	star, ok := params[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "testing" && selector.Sel.Name == "T"
}

// callsParallelFirst is whether fn's first statement is <its *testing.T>.Parallel().
func callsParallelFirst(fn *ast.FuncDecl) bool {
	if len(fn.Body.List) == 0 || len(fn.Type.Params.List[0].Names) == 0 {
		return false
	}
	statement, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := statement.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Parallel" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == fn.Type.Params.List[0].Names[0].Name
}

// firstComment is the first comment group inside body before its first statement, or nil.
func firstComment(file *ast.File, body *ast.BlockStmt) *ast.CommentGroup {
	end := body.Rbrace
	if len(body.List) > 0 {
		end = body.List[0].Pos()
	}
	for _, group := range file.Comments {
		if group.Pos() > body.Lbrace && group.Pos() < end {
			return group
		}
	}
	return nil
}
