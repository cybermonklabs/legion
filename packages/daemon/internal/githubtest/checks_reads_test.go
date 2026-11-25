package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
)

// The fake keeps each read of a commit's check runs it answers, in the order it answered them,
// with the runs each served as they stood at that read: a run set or concluded later shows in a
// later read and not in an earlier one, and the reads of two commits are kept apart, each with its
// own commit. The control: a read the fake refused, with no token, served no run and is not kept.
func TestTheChecksFakeKeepsEachReadOfACommitsRuns(t *testing.T) {
	t.Parallel()
	c := ServeChecks(t)
	token := "Bearer " + checksMintedToken(t, c)
	const other = "fedcba9876543210fedcba9876543210fedcba98"
	read := func(sha, authorization string, want int) {
		t.Helper()
		path := "/repos/" + checksAuthRepo + "/commits/" + sha + "/check-runs"
		if status, message, body := checksRequest(t, http.MethodGet, c.URL+path, authorization); status != want {
			t.Fatalf("GET %s answered %d %q: %s, want %d", path, status, message, body, want)
		}
	}
	started := time.Now()
	read(checksAuthHead, "", http.StatusUnauthorized)
	read(checksAuthHead, token, http.StatusOK)
	c.SetCheckRun(checksAuthRepo, checksAuthHead, CheckRun{ID: 1, Name: "unit", Status: "in_progress"})
	read(checksAuthHead, token, http.StatusOK)
	read(other, token, http.StatusOK)
	c.SetCheckRun(checksAuthRepo, checksAuthHead, CheckRun{ID: 1, Name: "unit", Status: "completed", Conclusion: "success"})
	read(checksAuthHead, token, http.StatusOK)
	ended := time.Now()

	want := []string{
		checksAuthRepo + "@" + checksAuthHead + ": none",
		checksAuthRepo + "@" + checksAuthHead + ": unit in_progress",
		checksAuthRepo + "@" + other + ": none",
		checksAuthRepo + "@" + checksAuthHead + ": unit completed success",
	}
	reads := c.Reads()
	got := make([]string, len(reads))
	for i, read := range reads {
		runs := make([]string, len(read.Runs))
		for j, run := range read.Runs {
			runs[j] = strings.TrimSpace(fmt.Sprintf("%s %s %s", run.Name, run.Status, run.Conclusion))
		}
		served := strings.Join(runs, ", ")
		if served == "" {
			served = "none"
		}
		got[i] = read.Repo + "@" + read.SHA + ": " + served
		if read.At.Before(started) || read.At.After(ended) || i > 0 && read.At.Before(reads[i-1].At) {
			t.Errorf("read %d (%s) is kept at %s, want a time in order between %s and %s", i, got[i], read.At, started, ended)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the fake kept the reads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// checksMintedToken is an installation token c minted for its own App's JWT.
func checksMintedToken(t *testing.T, c *Checks) string {
	t.Helper()
	signed, err := appauth.GenerateJWT(c.App.ClientID, c.App.Key, time.Now())
	if err != nil {
		t.Fatalf("sign the App's JWT: %v", err)
	}
	status, message, body := checksRequest(t, http.MethodPost, c.URL+"/app/installations/1/access_tokens", "Bearer "+signed)
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &minted); status != http.StatusCreated || err != nil || minted.Token == "" {
		t.Fatalf("the mint with the App's JWT answered %d %q: %s", status, message, body)
	}
	return minted.Token
}
