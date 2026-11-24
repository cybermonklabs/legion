package githubrest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// The open pull request on a branch is the one whose head is that branch of the repository itself.
// GitHub's head=<owner>:<branch> filter names an owner only, and the owner of a repository can hold
// a fork of it under another name, whose branches may carry any name, an issue's or a task's among
// them. A listed pull request whose head is a branch of another repository, or of one GitHub no
// longer has (a deleted fork: head.repo is null), is not the branch's; the repository's own, in
// whatever case GitHub spells its name, is (the control).
//
// The fault: the read takes the first listed pull request whose head.ref is the branch, so a
// fork's is taken as the task's, recorded, and moves the task. The control: the read without its
// test of the head's repository.
func TestTheOpenPullRequestOnABranchIsOneWhoseHeadIsInTheRepository(t *testing.T) {
	const branch = "legion/LEGION-208.impl"
	var listed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" || r.URL.Query().Get("head") != "acme:"+branch || r.URL.Query().Get("state") != "open" {
			t.Errorf("read %s?%s, want the open pull requests on acme:%s", r.URL.Path, r.URL.RawQuery, branch)
		}
		_, _ = w.Write([]byte(listed))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	repository := ghrepo.MustParse("acme/widgets")
	const (
		fork = `{"number":9,"state":"open","head":{"ref":"` + branch + `","sha":"fork","repo":{"full_name":"acme/widgets-fork"}}}`
		gone = `{"number":10,"state":"open","head":{"ref":"` + branch + `","sha":"gone","repo":null}}`
		own  = `{"number":2,"state":"open","head":{"ref":"` + branch + `","sha":"own","repo":{"full_name":"Acme/Widgets"}}}`
	)

	listed = "[" + fork + "," + gone + "]"
	if pull, found, err := client.OpenPullRequestOn(context.Background(), repository, branch); err != nil || found {
		t.Fatalf("the open pull request on %s, with a fork's and a deleted fork's listed = #%d, %t, %v; want none: neither head is a branch of acme/widgets",
			branch, pull.Number, found, err)
	}
	listed = "[" + fork + "," + gone + "," + own + "]"
	if pull, found, err := client.OpenPullRequestOn(context.Background(), repository, branch); err != nil || !found || pull.Number != 2 {
		t.Fatalf("the open pull request on %s, with acme/widgets' own listed after a fork's = #%d, %t, %v; want #2", branch, pull.Number, found, err)
	}
}
