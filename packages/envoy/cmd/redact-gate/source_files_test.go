package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestSourceFilesWalksTheDirectoriesGoWalks: the walk names every production .go file go's ./...
// pattern walks, a vendor directory's own files among them, and none below a vendor directory,
// in testdata, under a name starting with "." or "_", or in a nested module.
func TestSourceFilesWalksTheDirectoriesGoWalks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":            "module example.com/m\n",
		"a.go":              "package m\n",
		"a_test.go":         "package m\n",
		"_a.go":             "package m\n",
		".a.go":             "package m\n",
		"b/b.go":            "//go:build ignore\n\npackage main\n",
		"vendor/v.go":       "package vendor\n",
		"vendor/x/w.go":     "package x\n",
		"testdata/t.go":     "package t\n",
		"_under/u.go":       "package u\n",
		".hidden/h.go":      "package h\n",
		"nested/go.mod":     "module example.com/nested\n",
		"nested/n.go":       "package n\n",
		"deep/er/e.go":      "package er\n",
		"deep/er/notes.txt": "not Go\n",
	})
	files, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"a.go", "b/b.go", "deep/er/e.go", "vendor/v.go"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the walk named %q, want %q", got, want)
	}
}
