package textcheck

import (
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

// The transaction boundary of the daemon: every transaction, every connection taken from a pool
// and every statement on a pool or a connection goes through store.Transact or
// store.TransactWith, whatever its SQL. The guard type-checks every package of the module that
// the legion binary links and lists each call outside that boundary. It parses no SQL: a read
// outside a write's transaction runs in a read-only TransactWith, where PostgreSQL itself refuses
// every write to a table or a sequence (store's TestAReadOnlyTransactionRefusesEveryWrite).

const (
	modulePath = "github.com/sjawhar/legion/daemon"
	pgxPath    = "github.com/jackc/pgx/v5"
)

// transactionOpeners are the functions of pgx that open a transaction by hand (R1).
var transactionOpeners = []string{"BeginFunc", "BeginTxFunc"}

// connectionTakers are the methods that open a transaction or take a connection (R2).
var connectionTakers = []string{"Begin", "BeginTx", "Acquire", "AcquireFunc"}

// statementRunners are the methods that run a statement (R3), a read included.
var statementRunners = []string{"Exec", "ExecParams", "ExecPrepared", "ExecBatch", "SendBatch", "CopyFrom", "CopyTo",
	"Prepare", "Deallocate", "DeallocateAll", "Query", "QueryRow", "QueryFunc"}

// allowance is one function's calls the guard admits, by rule, with their exact count.
type allowance struct {
	pkg, function, rule string
	count               int
}

// allowances is every call outside the boundary the guard admits. Each entry's count is exact: a
// second such call, a missing one, and a call in a function of the same name in another package
// all fail the guard.
var allowances = []allowance{
	// The boundary's own begin.
	{"internal/store", "TransactWith", "R2", 1},
	// The guarded pool's BeginTx, which TransactWith opens through, forwarding to pgx's pool.
	{"internal/store", "(*Pool).begin", "R2", 1},
	// The connection a migration runs on and resets before it goes back.
	{"internal/store", "(*Pool).acquire", "R2", 1},
	// The guarded pool's forwarders of their caller's SQL to pgx's pool. No production code calls
	// them, since the guard reports every caller; tests call them to seed and read rows, and the
	// guard loads no test file.
	{"internal/store", "(*Pool).Query", "R3", 1},
	{"internal/store", "(*Pool).QueryRow", "R3", 1},
	{"internal/store", "(*Pool).Exec", "R3", 1},
	// A migration commits with its schema_version row on the connection acquire took. A lost reply
	// fails the boot, and the next boot reads the row (versionRecorded) and applies nothing twice.
	// The migration registers no hook.
	{"internal/store", "(*Store).apply", "R1", 1},
	// DISCARD ALL and pgx's DeallocateAll on the migration's connection before it goes back.
	{"internal/store", "releaseMigrationConn", "R3", 2},
}

// excludedPackages are the packages the guard does not walk, each with its reason.
var excludedPackages = map[string]string{
	// The session database is one *pgx.Conn that the CLI and the launcher hold, with no second
	// connection on which Transact could read a lost commit's outcome. Its two transactions do no
	// work after their COMMIT beyond returning their answer: an import's answer is read back from
	// the committed row, so its retry answers Identical, and an append's retry meets its
	// compare-and-set, which the launcher's keeper reads as its own save landing.
	"internal/ompsessions": "one connection, and no second one to read an outcome on",
}

// boundaryGuard is the transaction-boundary guard over the module at dir.
type boundaryGuard struct {
	dir string
	// expected lists the module's packages that the binary links: `go list -deps ./cmd/legion`,
	// run in dir, kept to the module's own import paths. It is read first, and on its own.
	expected func() ([]string, error)
	// load is packages.Load. TestTheBoundaryGuardRefusesALoadOfNoPackage gives it a loader that
	// answers no package and no error.
	load func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error)
}

// newBoundaryGuard is the guard over dir, with the go list above and packages.Load.
func newBoundaryGuard(dir string) *boundaryGuard {
	return &boundaryGuard{
		dir: dir,
		expected: func() ([]string, error) {
			list := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/legion")
			list.Dir = dir
			out, err := list.Output()
			if err != nil {
				return nil, fmt.Errorf("go list -deps ./cmd/legion: %w", err)
			}
			var own []string
			for _, path := range strings.Fields(string(out)) {
				if strings.HasPrefix(path, modulePath+"/") {
					own = append(own, path)
				}
			}
			return own, nil
		},
		load: packages.Load,
	}
}

// run reads expected, then loads exactly those patterns with overlay, then compares the two. It
// returns the guard's result lines, or an error that starts with UNKNOWN when expected is empty or
// the load falls short of it. It returns that error before it walks any call.
func (g *boundaryGuard) run(overlay map[string][]byte) ([]string, error) {
	expected, err := g.expected()
	if err != nil {
		return nil, fmt.Errorf("UNKNOWN: %w", err)
	}
	if len(expected) == 0 {
		return nil, errors.New("UNKNOWN: go list named no package of the module")
	}
	cfg := &packages.Config{
		Mode:    packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:     g.dir,
		Overlay: overlay,
	}
	loaded, err := g.load(cfg, expected...)
	errored := 0
	for _, pkg := range loaded {
		if len(pkg.Errors) > 0 {
			errored++
		}
	}
	if err != nil || errored > 0 || len(loaded) < len(expected) {
		summary := fmt.Sprintf("UNKNOWN: loaded %d of %d packages, %d with errors", len(loaded), len(expected), errored)
		if err != nil {
			summary += ": " + err.Error()
		}
		for _, pkg := range loaded {
			for _, loadErr := range pkg.Errors {
				summary += "\n  " + loadErr.Error()
			}
		}
		return nil, errors.New(summary)
	}

	tx, err := pgxTx(loaded)
	if err != nil {
		return nil, fmt.Errorf("UNKNOWN: %w", err)
	}
	calls := map[allowanceKey][]string{}
	var lines []string
	for _, pkg := range loaded {
		within := strings.TrimPrefix(pkg.PkgPath, modulePath+"/")
		if _, excluded := excludedPackages[within]; excluded {
			continue
		}
		for _, file := range pkg.Syntax {
			path := relativePath(g.dir, pkg.Fset.Position(file.Pos()).Filename)
			for _, decl := range file.Decls {
				function := declName(decl)
				ast.Inspect(decl, func(node ast.Node) bool {
					selector, ok := node.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					rule := checkedRule(pkg.TypesInfo, selector, tx)
					if rule == "" {
						return true
					}
					key := allowanceKey{pkg: within, function: function, rule: rule}
					calls[key] = append(calls[key], fmt.Sprintf("%s:%d %s in %s.%s", path,
						pkg.Fset.Position(selector.Pos()).Line, rule, pkg.Name, function))
					return true
				})
			}
		}
	}
	allowed := map[allowanceKey]bool{}
	for _, entry := range allowances {
		key := allowanceKey{pkg: entry.pkg, function: entry.function, rule: entry.rule}
		allowed[key] = true
		if found := len(calls[key]); found != entry.count {
			lines = append(lines, fmt.Sprintf("allowance %s.%s %s: allowed %d, found %d",
				filepath.Base(entry.pkg), entry.function, entry.rule, entry.count, found))
			lines = append(lines, calls[key]...)
		}
	}
	for key, sites := range calls {
		if !allowed[key] {
			lines = append(lines, sites...)
		}
	}
	slices.Sort(lines)
	return lines, nil
}

// allowanceKey is a checked call's package within the module, function and rule.
type allowanceKey struct {
	pkg, function, rule string
}

// pgxTx is pgx's Tx interface, read from the type information of a loaded package that imports
// pgx.
func pgxTx(loaded []*packages.Package) (*types.Interface, error) {
	for _, pkg := range loaded {
		for _, imported := range pkg.Types.Imports() {
			if imported.Path() != pgxPath {
				continue
			}
			if tx, ok := imported.Scope().Lookup("Tx").Type().Underlying().(*types.Interface); ok {
				return tx, nil
			}
		}
	}
	return nil, errors.New("no loaded package imports pgx, so the guard cannot tell a transaction from a pool")
}

// checkedRule is the rule a reference breaks, or "" for one the guard does not check: a pgx
// function that opens a transaction by hand (R1), called or not, or a method of the checked
// surface on a receiver whose static type does not implement pgx.Tx (R2, R3), called, taken as a
// method value, or named in a method expression. A method of the surface is one of the names
// above whose signature takes a context.Context first, whatever the caller passes there. A method
// of an interface is checked as one of a pool, since the guard cannot see what the interface
// holds.
func checkedRule(info *types.Info, selector *ast.SelectorExpr, tx *types.Interface) string {
	selection := info.Selections[selector]
	if selection == nil {
		function, ok := info.Uses[selector.Sel].(*types.Func)
		if ok && function.Pkg() != nil && function.Pkg().Path() == pgxPath && slices.Contains(transactionOpeners, function.Name()) {
			return "R1"
		}
		return ""
	}
	if selection.Kind() != types.MethodVal && selection.Kind() != types.MethodExpr {
		return ""
	}
	method, ok := selection.Obj().(*types.Func)
	if !ok {
		return ""
	}
	params := method.Type().(*types.Signature).Params()
	if params.Len() == 0 || params.At(0).Type().String() != "context.Context" {
		return ""
	}
	receiver := selection.Recv()
	if types.Implements(receiver, tx) || types.Implements(types.NewPointer(receiver), tx) {
		return ""
	}
	switch name := method.Name(); {
	case slices.Contains(connectionTakers, name):
		return "R2"
	case slices.Contains(statementRunners, name):
		return "R3"
	}
	return ""
}

// declName is a declaration's name as the guard reports it: a function's name, a method's
// receiver and name ((*Pool).begin), or "init" for a declaration outside any function.
func declName(decl ast.Decl) string {
	function, ok := decl.(*ast.FuncDecl)
	if !ok {
		return "init"
	}
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return function.Name.Name
	}
	receiver := function.Recv.List[0].Type
	if index, ok := receiver.(*ast.IndexExpr); ok {
		receiver = index.X
	}
	if star, ok := receiver.(*ast.StarExpr); ok {
		if ident, ok := star.X.(*ast.Ident); ok {
			return "(*" + ident.Name + ")." + function.Name.Name
		}
	}
	if ident, ok := receiver.(*ast.Ident); ok {
		return "(" + ident.Name + ")." + function.Name.Name
	}
	return function.Name.Name
}

// relativePath is file relative to dir, with forward slashes.
func relativePath(dir, file string) string {
	if rel, err := filepath.Rel(dir, file); err == nil {
		return filepath.ToSlash(rel)
	}
	return file
}

// moduleDir is the module this test belongs to, two directories above it.
func moduleDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// plantedTransaction is a file of package daemon that no build sees, which the guard loads in an
// overlay. Each line marked "reported" must be listed with its rule, and nothing else of it.
const plantedTransaction = `package daemon

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store"
)

func plantedOwner(ctx context.Context, pool *store.Pool, raw *pgxpool.Pool, conn *pgx.Conn, tx pgx.Tx, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) {
	_ = pgx.BeginFunc(ctx, raw, func(pgx.Tx) error { return nil })  // L1 reported: R1
	_, _ = pool.BeginTx(ctx, pgx.TxOptions{})                       // L2 reported: R2
	_, _ = raw.Acquire(ctx)                                         // L3 reported: R2
	_, _ = pool.Exec(ctx, "update issues set title = 'x'")          // L4 reported: R3
	_ = pool.QueryRow(ctx, "select count(*) from issues")           // L5 reported: R3, a read outside a transaction
	_ = pool.QueryRow(ctx, ` + "`" + `select "nextval"('outbox_id_seq')` + "`" + `)     // L6 reported: R3
	_, _ = conn.Query(ctx, "select 1")                              // L7 reported: R3, on a connection
	_ = q.QueryRow(ctx, "select 1")                                 // L8 reported: R3, through an interface
	exec := raw.Exec                                                // L10 reported: R3, a method value
	_ = exec
	_, _ = (*pgxpool.Pool).Exec(raw, ctx, "select 1")               // L11 reported: R3, a method expression
	_, _ = raw.Exec(struct{ context.Context }{ctx}, "select 1")     // L12 reported: R3, a context of another type
	_ = store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select count(*) from issues").Scan(new(int)) // inside the boundary
	})
	_, _ = tx.Begin(ctx)                                 // a savepoint
	_, _ = tx.Exec(ctx, "update issues set title = 'x'") // a statement in a transaction
}

func apply(ctx context.Context, raw *pgxpool.Pool) error { // a name the allowlist holds, in another package
	return pgx.BeginFunc(ctx, raw, func(pgx.Tx) error { return nil }) // L9 reported: R1
}
`

// plantedMark is a planted line's mark: its name and the rule it must be reported under.
var plantedMark = regexp.MustCompile(`// (L[0-9]+) reported: (R[0-9])`)

// TestNoTransactionOrStatementBypassesTheBoundary runs the guard over the module with the planted
// file added. The production code must add no line; the planted file must add exactly its twelve
// marked lines. A real call outside the boundary fails by being listed, a guard blind to a
// receiver kind, a method, a method value, a method expression, a context of another type or a
// package fails by a missing marked line, and a guard that reaches into the boundary fails by
// listing the TransactWith body, the savepoint or the statement in a transaction.
func TestNoTransactionOrStatementBypassesTheBoundary(t *testing.T) {
	dir := moduleDir(t)
	planted := filepath.Join(dir, "internal", "daemon", "planted_transaction.go")
	if _, err := os.Stat(planted); err == nil {
		t.Fatalf("%s exists on disk; the planted file is an overlay only", planted)
	}
	var want []string
	for number, line := range strings.Split(plantedTransaction, "\n") {
		mark := plantedMark.FindStringSubmatch(line)
		if mark == nil {
			continue
		}
		function := "plantedOwner"
		if mark[1] == "L9" {
			function = "apply"
		}
		want = append(want, "internal/daemon/planted_transaction.go:"+strconv.Itoa(number+1)+" "+mark[2]+" in daemon."+function)
	}
	if len(want) != 12 {
		t.Fatalf("the planted file marks %d lines, want 12", len(want))
	}
	slices.Sort(want)

	start := time.Now()
	lines, err := newBoundaryGuard(dir).run(map[string][]byte{planted: []byte(plantedTransaction)})
	t.Logf("the guard ran in %s", time.Since(start).Round(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) == 0 {
		t.Fatal("the guard listed no call, not even the planted ones: it reads nothing")
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("the guard listed:\n  %s\nwant exactly the planted lines:\n  %s", strings.Join(lines, "\n  "), strings.Join(want, "\n  "))
	}
	t.Logf("the guard listed:\n  %s", strings.Join(lines, "\n  "))
}

// A guard that loaded nothing has not passed: given a loader that answers no package and no
// error, it refuses with UNKNOWN before it compares anything.
func TestTheBoundaryGuardRefusesALoadOfNoPackage(t *testing.T) {
	g := newBoundaryGuard(moduleDir(t))
	g.load = func(*packages.Config, ...string) ([]*packages.Package, error) { return nil, nil }
	expected, err := g.expected()
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) <= 1 {
		t.Fatalf("go list named %d packages of the module, want more than one", len(expected))
	}
	lines, err := g.run(nil)
	if lines != nil {
		t.Fatalf("the guard over no package listed %v, want nil", lines)
	}
	if want := fmt.Sprintf("UNKNOWN: loaded 0 of %d packages, 0 with errors", len(expected)); err == nil || err.Error() != want {
		t.Fatalf("the guard over no package = %v, want %q", err, want)
	}
}
