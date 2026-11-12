package githubrest

import (
	"context"
	"fmt"
	"net/url"
)

// ChangedFile is one file GitHub lists in a change, a pull request's or a comparison's: the name
// it has after the change, and the name it had before, which GitHub fills for a rename alone.
// What the change did to the file (GitHub's "status": added, removed, modified, renamed) is read
// nowhere, so it is not decoded: READY judges a compare entry by its paths alone
// (packages/daemon/internal/api/ready.go, underLegion).
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

// PullRequestFilesLimit is the most changed files GitHub lists for a pull request: a pull request
// that reaches it may change a path the list never shows. It is GitHub's own cap on the list, not
// a page size this client chooses, so the read below cannot ask for more; what a caller can do is
// read that its list was cut and decide for the paths it never saw.
const PullRequestFilesLimit = 3000

// PullRequestPaths is every path pull request number's change touches, both names of a renamed
// file among them (ChangedPaths), and whether GitHub cut that list at PullRequestFilesLimit,
// which leaves a path the caller asks about unread.
//
// The cut is decided here rather than by each caller, so the limit and the read cannot drift
// apart: both callers judge a path a change touches — READY's by_hand merge policy
// (packages/daemon/internal/api/ready.go, mergeReadyPolicy) and the daemon's merge holds
// (packages/daemon/internal/daemon/merge_marks.go, readyMergeMarks.holding) — and a caller
// comparing the file count with a limit of its own would judge a truncated list as a complete one
// the day either changed.
func PullRequestPaths(ctx context.Context, c Client, number int) ([]string, bool, error) {
	files, err := GetPages[ChangedFile](ctx, c, fmt.Sprintf("/pulls/%d/files", number))
	if err != nil {
		return nil, false, err
	}
	return ChangedPaths(files), len(files) >= PullRequestFilesLimit, nil
}

// CompareFilesLimit is the most changed files GitHub's compare of two commits lists: "it includes
// up to 300 changed files for the entire comparison" (REST API, Compare two commits,
// https://docs.github.com/en/rest/commits/commits#compare-two-commits). A comparison that reaches
// it may change a path the list never shows.
const CompareFilesLimit = 300

// Comparison is GitHub's compare of two commits: its status (ahead when the head is the base with
// commits on top, identical, behind or diverged), and the files the comparison changes, nil when
// GitHub answered no list.
type Comparison struct {
	Status string         `json:"status"`
	Files  *[]ChangedFile `json:"files"`
}

// Compare reads GitHub's compare of base with head (GET /compare/{base}...{head}) under the
// client's repository. It asks for one commit a page: the status and the file list cover the whole
// comparison on the first page whatever the page size, and a smaller page reads less. A commit
// GitHub does not know is its 404 Answer.
func (c Client) Compare(ctx context.Context, base, head string) (Comparison, error) {
	var comparison Comparison
	if err := c.Get(ctx, fmt.Sprintf("/compare/%s...%s?per_page=1", url.PathEscape(base), url.PathEscape(head)), &comparison); err != nil {
		return Comparison{}, err
	}
	return comparison, nil
}
