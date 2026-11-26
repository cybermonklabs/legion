package shimwire

import (
	"errors"
	"strings"
	"testing"
)

// The one answer to a workspace read, whichever process read (the Legion reviewer's point 9 on R6
// of #5285): a long failure is cut to one answer line that keeps its end, where
// workspace.RunCommand puts the note about a stale git lock; a request that names no id is no
// answer; a read lists at most MaxWorkspaceWorkCommits
// commits and counts them all.
func TestEveryReaderAnswersAWorkspaceReadAlike(t *testing.T) {
	const note = "A git lock file is stale: remove it."
	long := "jj failed: " + strings.Repeat("x", 3*MaxAnswerErrorBytes) + " " + note
	request := WorkspaceWork{ID: "read-1", Issue: "LEGION-208", TimeoutMs: 1000}
	failed, refused := AnswerWorkspaceWork(request, func() (string, bool, []string, error) { return "", false, nil, errors.New(long) })
	if refused != nil || failed.OK || failed.ID != "read-1" {
		t.Fatalf("the answer to a failed read = %+v, %v; want a refusal of read-1", failed, refused)
	}
	if !strings.HasSuffix(failed.Error, note) || len(failed.Error) > MaxAnswerErrorBytes+200 {
		t.Fatalf("the refusal's reason (%d bytes) = %.120q…; want it cut near %d bytes, ending with the stale-lock note",
			len(failed.Error), failed.Error, MaxAnswerErrorBytes)
	}
	if err := failed.Validate(); err != nil {
		t.Fatalf("the refusal is no valid frame: %v", err)
	}

	if _, refused := AnswerWorkspaceWork(WorkspaceWork{Issue: "LEGION-208", TimeoutMs: 1000}, func() (string, bool, []string, error) {
		t.Fatal("the read ran for a request that names no id")
		return "", false, nil, nil
	}); refused == nil {
		t.Fatal("a request that names no id was answered, want it refused for the caller to log")
	}

	const commit = "f2f134541b30dfa5160a5d1ce5e17b76c967c661"
	work := make([]string, MaxWorkspaceWorkCommits+3)
	for i := range work {
		work[i] = commit
	}
	read, _ := AnswerWorkspaceWork(request, func() (string, bool, []string, error) { return commit, true, work, nil })
	if !read.OK || read.Count != len(work) || len(read.Commits) != MaxWorkspaceWorkCommits || !read.Changed || read.Head != commit {
		t.Fatalf("the answer to a read of %d commits = %+v, want all counted and %d listed", len(work), read, MaxWorkspaceWorkCommits)
	}
}
