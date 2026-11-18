// Package sourcecheck holds the checks of a Go module's own source that more than one module of
// this repository runs, each over its own tree: a module's internal/textcheck calls one with that
// module's root. It is public, as testnatsimages is, so that packages/daemon, a module outside
// github.com/sjawhar/envoy, runs the same body: Go's internal-package rule keeps that module from
// importing an internal package of this one.
// packages/daemon/internal/textcheck's envoy_helper_copies_test.go fails a copy of it there.
package sourcecheck

import (
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// CheckFileCopies fails t naming, as file:line, every line of the Go files under root, tests
// included, that holds a string literal opening a database's creation and no literal holding
// `strategy file_copy` (docs/testing.md, "A database a Go test creates"). Comments are not
// literals, so prose about the statement is not read.
func CheckFileCopies(t testing.TB, root string) {
	t.Helper()
	if offences := creationsWithoutFileCopy(t, root); len(offences) > 0 {
		t.Fatalf("%d database creations copy their source through the write-ahead log:\n\t%s\n"+
			"write `strategy file_copy` on the line that opens each one (docs/testing.md, \"A database a Go test creates\")",
			len(offences), strings.Join(offences, "\n\t"))
	}
}

// creationsWithoutFileCopy names, as file:line, every line under root that holds a string literal
// opening a database's creation and no literal holding `strategy file_copy`.
func creationsWithoutFileCopy(t testing.TB, root string) []string {
	t.Helper()
	opening := "create" + " database" // written apart so this file holds no opening of its own
	var offences []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files := token.NewFileSet()
		file := files.AddFile(path, files.Base(), len(source))
		var lex scanner.Scanner
		lex.Init(file, source, nil, 0)
		opens, copies := map[int]bool{}, map[int]bool{}
		for {
			position, kind, literal := lex.Scan()
			if kind == token.EOF {
				break
			}
			if kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(literal)
			if err != nil {
				text = literal
			}
			text = strings.ToLower(text)
			line := files.Position(position).Line
			if strings.Contains(text, opening) {
				opens[line] = true
			}
			if strings.Contains(text, "strategy file_copy") {
				copies[line] = true
			}
		}
		for line := range opens {
			if !copies[line] {
				offences = append(offences, filepath.ToSlash(path)+":"+strconv.Itoa(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read the module's Go files: %v", err)
	}
	slices.Sort(offences)
	return offences
}
