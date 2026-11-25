package githubtest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Refs answers the three refs requests as GitHub does, through the daemon's own client: the list
// under a prefix names each branch under it and no other, in name order; the exact read finds a
// branch it holds and answers 404 for one it does not; a delete answers 204 and the branch is gone,
// and a delete of a branch it does not hold answers GitHub's 422. A planned answer is answered
// once, in its order, and removes the branch where it says so. Each request is kept.
func TestRefsServesTheListTheExactReadAndTheDeleteAsGitHubDoes(t *testing.T) {
	ctx := context.Background()
	refs := NewRefs()
	const repo = "acme/widgets"
	refs.Set(repo, "legion-beside/LEGION-208/3/a/fix-1/tester", "1111")
	refs.Set(repo, "legion-beside/LEGION-208/3/a/fix-1/implementer", "2222")
	refs.Set(repo, "legion-beside/LEGION-208/3/a/fix-2/tester", "3333")
	refs.Set(repo, "legion/LEGION-208", "4444")
	refs.Set("acme/other", "legion-beside/LEGION-208/3/a/fix-1/tester", "5555")
	client := githubrest.Client{Token: "t", API: refs.Serve(t) + "/repos/" + repo}

	type ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	listed, err := githubrest.GetPages[ref](ctx, client, "/git/matching-refs/heads/legion-beside/LEGION-208/3/a/fix-1/")
	if err != nil {
		t.Fatalf("list fix 1's branches: %v", err)
	}
	var names []string
	for _, r := range listed {
		names = append(names, r.Ref+"@"+r.Object.SHA)
	}
	if want := []string{"refs/heads/legion-beside/LEGION-208/3/a/fix-1/implementer@2222", "refs/heads/legion-beside/LEGION-208/3/a/fix-1/tester@1111"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("the list under fix 1's prefix = %v, want %v", names, want)
	}
	if none, err := githubrest.GetPages[ref](ctx, client, "/git/matching-refs/heads/legion-beside/LEGION-999/"); err != nil || len(none) != 0 {
		t.Fatalf("the list under a prefix of no branch = %v, %v; want an empty list", none, err)
	}

	var found ref
	if err := client.Get(ctx, "/git/ref/heads/legion-beside/LEGION-208/3/a/fix-1/tester", &found); err != nil || found.Object.SHA != "1111" {
		t.Fatalf("the exact read of the tester's branch = %+v, %v; want it at 1111", found, err)
	}
	if err := client.Delete(ctx, "/git/refs/heads/legion-beside/LEGION-208/3/a/fix-1/tester"); err != nil {
		t.Fatalf("delete the tester's branch: %v", err)
	}
	if status := statusOf(client.Get(ctx, "/git/ref/heads/legion-beside/LEGION-208/3/a/fix-1/tester", &found)); status != http.StatusNotFound {
		t.Fatalf("the exact read of the deleted branch answered %d, want 404", status)
	}
	if status := statusOf(client.Delete(ctx, "/git/refs/heads/legion-beside/LEGION-208/3/a/fix-1/tester")); status != http.StatusUnprocessableEntity {
		t.Fatalf("a delete of the deleted branch answered %d, want GitHub's 422", status)
	}

	refs.Fail(http.MethodDelete, repo, "legion-beside/LEGION-208/3/a/fix-2/tester", http.StatusInternalServerError, false)
	if status := statusOf(client.Delete(ctx, "/git/refs/heads/legion-beside/LEGION-208/3/a/fix-2/tester")); status != http.StatusInternalServerError {
		t.Fatalf("the planned delete answered %d, want 500", status)
	}
	if got := refs.Branches(repo, "legion-beside/LEGION-208/3/a/fix-2/"); len(got) != 1 {
		t.Fatalf("fix 2's branches after the planned 500 = %v, want the branch kept", got)
	}
	refs.Fail(http.MethodDelete, repo, "legion-beside/LEGION-208/3/a/fix-2/tester", http.StatusUnprocessableEntity, true)
	if status := statusOf(client.Delete(ctx, "/git/refs/heads/legion-beside/LEGION-208/3/a/fix-2/tester")); status != http.StatusUnprocessableEntity {
		t.Fatalf("the planned delete answered %d, want 422", status)
	}
	if got := refs.Branches(repo, "legion-beside/LEGION-208/3/a/fix-2/"); len(got) != 0 {
		t.Fatalf("fix 2's branches after the planned 422 that removes = %v, want none", got)
	}
	if got := refs.Branches("acme/other", "legion-beside/"); len(got) != 1 {
		t.Fatalf("the other repository's branches = %v, want its one untouched", got)
	}
	if got := len(refs.Requests()); got != 8 {
		t.Fatalf("Refs kept %d requests, want the 8 it answered: %+v", got, refs.Requests())
	}
}

// statusOf is the status of GitHub's answer err carries, 0 for none.
func statusOf(err error) int {
	var answer *githubrest.Answer
	if errors.As(err, &answer) {
		return answer.Status
	}
	return 0
}
