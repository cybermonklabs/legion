package testtypes

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// CheckPackage reads the files the build compiles and no other. A test file and a file a build
// constraint leaves out are no part of the package the build makes, so a test that read them
// would hold the daemon to code it never runs. Each of the two here declares A again, so reading
// either one fails the check on the redeclaration rather than passing unseen.
func TestCheckPackageReadsTheFilesTheBuildCompilesAndNoOther(t *testing.T) {
	dir := t.TempDir()
	for name, source := range map[string]string{
		"a.go":      "package p\n\nconst A = 1\n",
		"a_test.go": "package p\n\nconst A = 2\n",
		"never.go":  "//go:build never\n\npackage p\n\nconst A = 3\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The files import nothing, so the check needs no importer.
	fset := token.NewFileSet()
	checked, err := CheckPackage(fset, nil, "example.com/p", dir)
	if err != nil {
		t.Fatalf("CheckPackage: %v", err)
	}
	var read []string
	for _, file := range checked.Files {
		read = append(read, filepath.Base(fset.Position(file.Pos()).Filename))
	}
	if !slices.Equal(read, []string{"a.go"}) {
		t.Errorf("CheckPackage read %v, want a.go alone: the build compiles no test file and no file its constraints leave out", read)
	}
	if checked.Types.Path() != "example.com/p" {
		t.Errorf("CheckPackage checked the package as %q, want the path it was given", checked.Types.Path())
	}
	if a, ok := checked.Types.Scope().Lookup("A").(*types.Const); !ok || a.Val().ExactString() != "1" {
		t.Errorf("the checked package's A is %v, want the constant 1 that a.go declares", checked.Types.Scope().Lookup("A"))
	}
}
