package textcheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	// For its source alone: the row below reads server/raft.go from the module cache, and the go
	// command fetches a module into that cache only for a build that imports one of its packages
	// (moduleDir). This import makes this package's build fetch nats-server.
	_ "github.com/nats-io/nats-server/v2/server"
)

// natsServerModule is nats-server's module, whose raft.go holds the election timeouts.
const natsServerModule = "github.com/nats-io/nats-server/v2"

// timeUnits are the time package's durations a constant of the files durationConst reads may name.
var timeUnits = map[string]time.Duration{
	"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
	"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

// durationConst is the value of the package-level constant name that the Go files at paths declare,
// with the file and line it is declared at. It reads a constant expression of integer literals, the
// time package's units (time.Second and the like), time.Duration conversions, other constants of the
// same files, + - * /, and parentheses, and fails naming anything else, so a constant this cannot
// read fails the row that asks for it rather than reads as some value.
func durationConst(paths []string, name string) (time.Duration, string, error) {
	fileSet := token.NewFileSet()
	values := map[string]ast.Expr{}
	for _, path := range paths {
		file, err := parser.ParseFile(fileSet, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return 0, "", err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, ident := range value.Names {
					if i < len(value.Values) {
						values[ident.Name] = value.Values[i]
					}
				}
			}
		}
	}
	var eval func(expr ast.Expr, depth int) (int64, error)
	eval = func(expr ast.Expr, depth int) (int64, error) {
		if depth > len(values) {
			return 0, fmt.Errorf("constant %s refers to itself", name)
		}
		switch expr := expr.(type) {
		case *ast.BasicLit:
			if expr.Kind == token.INT {
				return strconv.ParseInt(strings.ReplaceAll(expr.Value, "_", ""), 0, 64)
			}
		case *ast.ParenExpr:
			return eval(expr.X, depth)
		case *ast.Ident:
			if value, ok := values[expr.Name]; ok {
				return eval(value, depth+1)
			}
		case *ast.SelectorExpr:
			if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Name == "time" {
				if unit, ok := timeUnits[expr.Sel.Name]; ok {
					return int64(unit), nil
				}
			}
		case *ast.CallExpr:
			if fun, ok := expr.Fun.(*ast.SelectorExpr); ok && len(expr.Args) == 1 && types.ExprString(fun) == "time.Duration" {
				return eval(expr.Args[0], depth)
			}
		case *ast.BinaryExpr:
			x, err := eval(expr.X, depth)
			if err != nil {
				return 0, err
			}
			y, err := eval(expr.Y, depth)
			if err != nil {
				return 0, err
			}
			switch expr.Op {
			case token.ADD:
				return x + y, nil
			case token.SUB:
				return x - y, nil
			case token.MUL:
				return x * y, nil
			case token.QUO:
				if y != 0 {
					return x / y, nil
				}
			}
		}
		return 0, fmt.Errorf("%s: %s is no constant duration this check reads", fileSet.Position(expr.Pos()), types.ExprString(expr))
	}
	value, ok := values[name]
	if !ok {
		return 0, "", fmt.Errorf("no file of %s declares the constant %s", strings.Join(paths, ", "), name)
	}
	at := fileSet.Position(value.Pos())
	nanoseconds, err := eval(value, 0)
	return time.Duration(nanoseconds), at.Filename + ":" + strconv.Itoa(at.Line), err
}

// packageFiles are the Go files of the package in dir, its tests left out.
func packageFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	return paths, nil
}

// ceilingAgainstElections compares the bus's limitCeiling, read from the Go files of busDir, with
// nats-server's maxElectionTimeoutDefault, read from raftFile, and returns an error naming both when
// they differ or either cannot be read.
func ceilingAgainstElections(busDir, raftFile string) error {
	files, err := packageFiles(busDir)
	if err != nil {
		return err
	}
	ceiling, ceilingAt, err := durationConst(files, "limitCeiling")
	if err != nil {
		return err
	}
	election, electionAt, err := durationConst([]string{raftFile}, "maxElectionTimeoutDefault")
	if err != nil {
		return err
	}
	if ceiling != election {
		return fmt.Errorf("limitCeiling is %s (%s), and nats-server's longest election timeout, maxElectionTimeoutDefault, is %s (%s)", ceiling, ceilingAt, election, electionAt)
	}
	return nil
}

// The longest limit of a start's run of reads or of writes (internal/bus limitCeiling) is
// nats-server's longest election timeout, maxElectionTimeoutDefault, read from server/raft.go of
// the nats-server version go.mod requires (moduleDir). A request still unanswered past an election
// was dropped rather than slowed, so a longer limit only holds a start longer once the leader is
// back, and a shorter one cuts a request that is waiting on an election; the bound ten seconds,
// the fixed JetStream wait this module's starts no longer wait out, is past it. The row's control
// holds its reading to planted files: a ceiling of RequestTimeout, ten seconds, differs from 9 s,
// and a ceiling it cannot read fails by name.
func TestTheLongestLimitIsNatsServersLongestElectionTimeout(t *testing.T) {
	server, err := moduleDir(filepath.Join("..", ".."), natsServerModule)
	if err != nil {
		t.Fatal(err)
	}
	raftFile := filepath.Join(server, "server", "raft.go")
	if err := ceilingAgainstElections(filepath.Join("..", "bus"), raftFile); err != nil {
		t.Fatalf("%v: a run's limit must stop growing at that election timeout", err)
	}

	planted := func(source string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "limits.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	ten := planted("package bus\n\nimport \"time\"\n\nconst (\n\tRequestTimeout = 10 * time.Second\n\tlimitCeiling   = RequestTimeout\n)\n")
	if err := ceilingAgainstElections(ten, raftFile); err == nil || !strings.Contains(err.Error(), "limitCeiling is 10s") || !strings.Contains(err.Error(), "is 9s") {
		t.Fatalf("a planted ceiling of RequestTimeout = %v, want it told apart from the election timeout, 10s against 9s", err)
	}
	unread := planted("package bus\n\nimport \"time\"\n\nconst limitCeiling = time.Duration(len(\"nine\")) * time.Second\n")
	if err := ceilingAgainstElections(unread, raftFile); err == nil || !strings.Contains(err.Error(), "is no constant duration this check reads") {
		t.Fatalf("a planted ceiling the check cannot read = %v, want a failure naming what it cannot read", err)
	}
}
