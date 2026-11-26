package textcheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// capTableWrite is a statement that writes the admission cap's table: an insert into it, an
// update or a delete of it, a truncate, an alter, a drop, or a copy into it.
var capTableWrite = regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from|truncate(\s+table)?|alter\s+table|drop\s+table|copy)\s+(if\s+exists\s+)?project_admission_cap_changes\b`)

// capWriter is one place that writes the admission cap's table, or reaches the one function that
// does: the file, relative to the daemon module, and the function it stands in.
type capWriter struct {
	file, function string
}

func (w capWriter) String() string { return w.file + ": " + w.function }

// capWriters reads every Go file of the daemon module under root that is no test, and answers each
// function that writes project_admission_cap_changes in an SQL string, each function that calls
// RecordAdmissionCapChangeTx, and each function that builds an intake.AdmissionCapSet, the fact
// whose admission is that call's one caller. extra is source to read beside the module's, by the
// file name it would have, which is how the control below adds a writer without touching a file.
func capWriters(t *testing.T, root string, extra map[string]string) (sql, recorders, facts []capWriter) {
	t.Helper()
	sources := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == "node_modules" || entry.Name() == "testdata") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sources[filepath.ToSlash(rel)] = string(text)
		return nil
	})
	if err != nil {
		t.Fatalf("read the daemon module under %s: %v", root, err)
	}
	if len(sources) < 100 {
		t.Fatalf("read %d Go files under %s, want the daemon module's hundreds: the walk read the wrong tree", len(sources), root)
	}
	for name, text := range extra {
		sources[name] = text
	}
	for name, text := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), name, text, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			here := capWriter{file: name, function: functionName(fn)}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.BasicLit:
					if node.Kind == token.STRING {
						if value, err := strconv.Unquote(node.Value); err == nil && capTableWrite.MatchString(value) {
							sql = append(sql, here)
						}
					}
				case *ast.CallExpr:
					if calledName(node.Fun) == "RecordAdmissionCapChangeTx" {
						recorders = append(recorders, here)
					}
				case *ast.CompositeLit:
					if sel, ok := node.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "AdmissionCapSet" {
						facts = append(facts, here)
					}
				}
				return true
			})
		}
	}
	for _, list := range []*[]capWriter{&sql, &recorders, &facts} {
		slices.SortFunc(*list, func(a, b capWriter) int { return strings.Compare(a.String(), b.String()) })
		*list = slices.Compact(*list)
	}
	return sql, recorders, facts
}

// calledName is the name a call calls: the function's own, or the selector's.
func calledName(fun ast.Expr) string {
	switch fun := fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// Every change of the cap takes one recorded path (dispatch://INFRA-5616/spec#b-r5-one-path): one
// function writes project_admission_cap_changes, RecordAdmissionCapChangeTx; one function calls it,
// admission's setAdmissionCap, which runs on the fact intake.AdmissionCapSet; and only the cap
// routes, through changeAdmissionCap's callers, build that fact. The migrations write the table in
// one file, the migration that creates it. A second writer of any of the four fails this test,
// naming it.
//
// The control adds one writer beside the module's own files, a function that deletes the table's
// rows, and requires the same reading to name it: a scan that could not see a writer would pass
// on the module and fail there.
func TestEveryWriterOfTheAdmissionCapsTableIsTheOnePath(t *testing.T) {
	module, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	sql, recorders, facts := capWriters(t, module, nil)
	wantSQL := []capWriter{{file: "internal/store/admission_cap.go", function: "RecordAdmissionCapChangeTx"}}
	wantRecorders := []capWriter{{file: "internal/admit/admit.go", function: "Admission.setAdmissionCap"}}
	wantFacts := []capWriter{
		{file: "internal/api/admission_cap.go", function: "server.operatorProjectAdmissionCap"},
		{file: "internal/api/admission_cap.go", function: "server.operatorProjectAdmissionCapClear"},
		{file: "internal/api/admission_cap.go", function: "server.projectAdmissionCap"},
		{file: "internal/api/admission_cap.go", function: "server.projectAdmissionCapClear"},
	}
	for _, check := range []struct {
		what      string
		got, want []capWriter
	}{
		{"the writers of project_admission_cap_changes in SQL", sql, wantSQL},
		{"the callers of RecordAdmissionCapChangeTx", recorders, wantRecorders},
		{"the builders of intake.AdmissionCapSet", facts, wantFacts},
	} {
		if !slices.Equal(check.got, check.want) {
			t.Errorf("%s = %v, want %v alone", check.what, check.got, check.want)
		}
	}

	migrations, err := filepath.Glob(filepath.Join(module, "internal", "store", "migrations", "*.sql"))
	if err != nil || len(migrations) < 100 {
		t.Fatalf("the migrations read %d files (%v), want the store's hundreds", len(migrations), err)
	}
	var writingMigrations []string
	for _, path := range migrations {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(text), "project_admission_cap_changes") {
			writingMigrations = append(writingMigrations, filepath.Base(path))
		}
	}
	if !slices.Equal(writingMigrations, []string{"20261124040825_admission_cap.up.sql"}) {
		t.Errorf("the migrations that name project_admission_cap_changes = %v, want the one that creates it alone", writingMigrations)
	}

	t.Run("the control: a second writer is named", func(t *testing.T) {
		sql, _, _ := capWriters(t, module, map[string]string{"internal/admit/sneak.go": `package admit

func sneak(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "delete from project_admission_cap_changes where project = $1", "LEGION")
	return err
}
`})
		want := append(slices.Clone(wantSQL), capWriter{file: "internal/admit/sneak.go", function: "sneak"})
		slices.SortFunc(want, func(a, b capWriter) int { return strings.Compare(a.String(), b.String()) })
		if !slices.Equal(sql, want) {
			t.Fatalf("with a second writer, the writers in SQL = %v, want %v", sql, want)
		}
	})
}
