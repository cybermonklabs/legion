package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestARefusedSHAIsErrNotASHA: each read that takes a commit's SHA refuses a value that is not one
// with ErrNotASHA, which a caller tells apart with errors.Is (cireconcile gives up the head's
// episode on it rather than retrying), and names the value by redact.ShapedSHA's phrase alone. A
// short SHA, which GitHub would answer at the same path, is refused too: no caller sends one.
func TestARefusedSHAIsErrNotASHA(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("GitHub was sent %s %s; want no request", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx := context.Background()
	for _, sha := range []string{mainCommit[:12], "main", credentialShaped} {
		for name, read := range map[string]func() error{
			"check runs": func() error { _, err := client.CommitCheckRuns(ctx, "ghs_test", "acme", "web", sha, 1, ""); return err },
			"check suites": func() error {
				_, err := client.CommitCheckSuites(ctx, "ghs_test", "acme", "web", sha, 1, "")
				return err
			},
			"statuses":   func() error { _, err := client.CommitStatuses(ctx, "ghs_test", "acme", "web", sha, 1, ""); return err },
			"commit":     func() error { _, err := client.HasCommit(ctx, "ghs_test", "acme", "web", sha); return err },
			"comparison": func() error { _, err := client.CommitOnBranch(ctx, "ghs_test", "acme", "web", "main", sha); return err },
			"blob": func() error {
				_, err := client.DirFiles(ctx, "ghs_test", "acme", "web", Dir{SHA: "t1", Files: []DirFile{{Name: "a.md", Path: "docs/a.md", SHA: sha, Size: 1}}})
				return err
			},
		} {
			err := read()
			if !errors.Is(err, ErrNotASHA) {
				t.Errorf("the %s read of %q answered %v, want ErrNotASHA", name, sha, err)
				continue
			}
			if strings.Contains(err.Error(), sha) || !strings.Contains(err.Error(), "is not a SHA)") {
				t.Errorf("the %s read of %q answered %q, want the value named by ShapedSHA's phrase alone", name, sha, err)
			}
		}
	}
}

// A branch's name is Dispatch's or GitHub's text, and either can hold a `/`. A target the client
// built for a branch a person saved prints as ShownBody shows it, the `/` path-escaped as it is
// sent; a comparison against a default branch GitHub named prints the branch as the rule reads it,
// which leaves a `/` as it is. Both stay readable, so the rule refuses neither: it would refuse a
// path-escaped `/` (%2F) and the whole target with it.
func TestABranchHoldingASlashPrintsReadably(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/web/git/ref/heads/feature/notes",
			strings.HasPrefix(r.URL.Path, "/repos/acme/web/compare/release/2026..."):
			serverError(w, r)
		default:
			githubtest.Unexpected(t, w, r)
		}
	})
	ctx := context.Background()
	_, err := client.Branch(ctx, "ghs_test", "acme", "web", "feature/notes")
	if want := "GET " + client.base + "/repos/acme/web/git/ref/heads/" + url.PathEscape("feature/notes") + `: status 500: {"message":"Server Error"}`; err == nil || err.Error() != want {
		t.Errorf("Branch answered %v, want %q", err, want)
	}
	_, err = client.CommitOnBranch(ctx, "ghs_test", "acme", "web", "release/2026", mainCommit)
	if want := fmt.Sprintf("GET %s/repos/acme/web/compare/release/2026...%s?per_page=1: status 500: %s", client.base, mainCommit, `{"message":"Server Error"}`); err == nil || err.Error() != want {
		t.Errorf("CommitOnBranch answered %v, want %q", err, want)
	}
}

// A note's path read from GitHub's tree answer (Dir) is held with that answer's record, and a
// path that holds no credential the read carried prints as it came: the error of a blob read
// GitHub failed names `docs/release-notes.md` whole.
func TestATreePathReadFromGitHubPrintsReadably(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/acme/web/git/trees/"):
			fmt.Fprintf(w, `{"sha":"t1","truncated":false,"tree":[{"path":"release-notes.md","mode":"100644","type":"blob","sha":%q,"size":1}]}`, mainCommit)
		case r.URL.Path == "/repos/acme/web/git/blobs/"+mainCommit:
			serverError(w, r)
		default:
			githubtest.Unexpected(t, w, r)
		}
	})
	ctx := context.Background()
	listing, err := client.Dir(ctx, "ghs_test", "acme", "web", mainCommit, "docs")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	_, err = client.DirFiles(ctx, "ghs_test", "acme", "web", listing)
	if want := fmt.Sprintf(`GET %s/repos/acme/web/git/blobs/%s (docs/release-notes.md): status 500: {"message":"Server Error"}`, client.base, mainCommit); err == nil || err.Error() != want {
		t.Errorf("DirFiles answered %v, want %q", err, want)
	}
}
