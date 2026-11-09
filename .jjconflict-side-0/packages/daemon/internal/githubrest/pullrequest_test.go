package githubrest

import (
	"slices"
	"testing"
)

// ChangedPaths names every path a file list touches. A rename moves a file out of its old path as
// well as into its new one, so both names are paths the change touches and READY matches both
// against the project's by_hand merge rules; a file GitHub did not rename carries no old name and
// adds no second path.
func TestChangedPathsNamesEachFileAndARenamesOldName(t *testing.T) {
	files := []ChangedFile{
		{Filename: ".legion/A-1/review.json"},
		{Filename: "src/new.go", PreviousFilename: "src/old.go"},
	}
	if paths := ChangedPaths(files); !slices.Equal(paths, []string{".legion/A-1/review.json", "src/new.go", "src/old.go"}) {
		t.Fatalf("ChangedPaths = %q, want each name and a rename's old name", paths)
	}
	if paths := ChangedPaths(nil); len(paths) != 0 {
		t.Fatalf("ChangedPaths(nil) = %q, want no paths", paths)
	}
}
