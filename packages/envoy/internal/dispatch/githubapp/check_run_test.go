package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// checkRunCommit is the commit the check runs below were made for.
const checkRunCommit = "0123456789abcdef0123456789abcdef01234567"

// CheckRun reads one check run by its id and answers its id, its name and its commit, the three
// facts a job's result is held to.
func TestCheckRunReadsTheNameAndCommitOfOneRun(t *testing.T) {
	client, fake := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"id":97,"name":"go-checks (linux, 1.27)","head_sha":%q,"status":"in_progress"}`, checkRunCommit)
	})

	run, err := client.CheckRun(context.Background(), "installation-token", "acme", "web", 97)
	if err != nil {
		t.Fatalf("CheckRun: %v", err)
	}
	if sent := fake.last(t); sent.method != http.MethodGet || sent.path != "/repos/acme/web/check-runs/97" || sent.query != "" {
		t.Fatalf("CheckRun asked for %s %s?%s, want GET /repos/acme/web/check-runs/97", sent.method, sent.path, sent.query)
	}
	if want := (CheckRun{ID: 97, Name: "go-checks (linux, 1.27)", HeadSHA: checkRunCommit}); run != want {
		t.Fatalf("CheckRun = %+v, want %+v", run, want)
	}
}

// GitHub's 404 for a check run is ErrRepositoryUnreadable, as every repository read's is: the
// installation cannot see the run, which says nothing of whose it is.
func TestCheckRunGitHubCannotShowIsUnreadable(t *testing.T) {
	client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})

	if _, err := client.CheckRun(context.Background(), "installation-token", "acme", "web", 97); !errors.Is(err, ErrRepositoryUnreadable) {
		t.Fatalf("CheckRun of a run GitHub answers 404 for: %v, want ErrRepositoryUnreadable", err)
	}
}

// An answer that is not the run asked for, or that names no name or no whole commit, is an
// error and never a run: a result held to a run must be held to that run's own facts.
func TestCheckRunRefusesAnAnswerThatIsNotTheRun(t *testing.T) {
	for name, body := range map[string]string{
		"another id":     fmt.Sprintf(`{"id":98,"name":"go-checks","head_sha":%q}`, checkRunCommit),
		"no name":        fmt.Sprintf(`{"id":97,"name":"","head_sha":%q}`, checkRunCommit),
		"a short commit": `{"id":97,"name":"go-checks","head_sha":"0123456"}`,
		"no commit":      `{"id":97,"name":"go-checks"}`,
		"not JSON":       `<html>`,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := startPollGitHub(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, body) })
			if run, err := client.CheckRun(context.Background(), "installation-token", "acme", "web", 97); err == nil {
				t.Fatalf("CheckRun of %s = %+v, want an error", body, run)
			}
		})
	}
}

// A client with no App key answers ErrNoAppKey, as every read of this package does.
func TestCheckRunWithNoAppKey(t *testing.T) {
	var client *Client
	if _, err := client.CheckRun(context.Background(), "installation-token", "acme", "web", 97); !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("CheckRun on a nil client: %v, want ErrNoAppKey", err)
	}
}
