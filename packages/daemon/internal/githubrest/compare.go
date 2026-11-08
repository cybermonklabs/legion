package githubrest

// ChangedFile is one file GitHub lists in a pull request's change: the name it has after the
// change, and the name it had before, which GitHub fills for a rename alone. What the change did
// to the file (GitHub's "status": added, removed, modified, renamed) is read nowhere, so it is
// not decoded: READY judges the change by its paths alone, matching them against the project's
// by_hand merge rules (packages/daemon/internal/api/ready.go, mergeReadyPolicy).
type ChangedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

// ChangedPaths is every path files touch: each file's name, and a renamed file's old name too,
// since moving a file out of a path changes that path.
func ChangedPaths(files []ChangedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Filename)
		if file.PreviousFilename != "" {
			paths = append(paths, file.PreviousFilename)
		}
	}
	return paths
}
