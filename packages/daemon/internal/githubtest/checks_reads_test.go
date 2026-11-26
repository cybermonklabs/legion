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

// The listing reads filter, per_page and page as GitHub's own does: filter=latest, GitHub's
// default, serves the newest attempt of each check name, filter=all every attempt, a page holds
// per_page runs, 30 by default and 100 at most, a page past the end serves none, and total_count
// counts every run the filter leaves rather than the page's. The read the fake keeps names the
// three as it read them, so a rig can say what its reader asked for. The two attempts of unit
// have the lint run between them by id, so a listing that served the run set last of each name,
// or the one set first, reads differently from one that serves the newest attempt.
func TestTheChecksFakeAnswersTheListingsFilterAndPages(t *testing.T) {
	t.Parallel()
	c := ServeChecks(t)
	token := "Bearer " + checksMintedToken(t, c)
	for _, run := range []CheckRun{
		{ID: 1, Name: "unit", Status: "completed", Conclusion: "failure"},
		{ID: 2, Name: "lint", Status: "completed", Conclusion: "success"},
		{ID: 3, Name: "unit", Status: "completed", Conclusion: "success"},
	} {
		c.SetCheckRun(checksAuthRepo, checksAuthHead, run)
	}

	var kept []string
	for _, row := range []struct {
		query, served, read string
		total               int
	}{
		{query: "", total: 2, served: "lint#2, unit#3", read: "latest 30 1"},
		{query: "filter=all", total: 3, served: "unit#1, lint#2, unit#3", read: "all 30 1"},
		{query: "filter=latest&per_page=1&page=2", total: 2, served: "unit#3", read: "latest 1 2"},
		{query: "filter=all&per_page=2&page=1", total: 3, served: "unit#1, lint#2", read: "all 2 1"},
		{query: "filter=all&per_page=2&page=2", total: 3, served: "unit#3", read: "all 2 2"},
		{query: "filter=all&per_page=2&page=3", total: 3, served: "none", read: "all 2 3"},
		{query: "filter=all&per_page=1000&page=1", total: 3, served: "unit#1, lint#2, unit#3", read: "all 100 1"},
	} {
		total, served := checksListing(t, c, token, row.query)
		if total != row.total || served != row.served {
			t.Errorf("the listing %q answered total_count %d and served %s, want %d and %s", row.query, total, served, row.total, row.served)
		}
		kept = append(kept, row.read+": "+row.served)
	}

	// Each read the fake kept names the parameters it read and the runs it served, in order.
	got := make([]string, 0, len(kept))
	for _, read := range c.Reads() {
		got = append(got, fmt.Sprintf("%s %d %d: %s", read.Filter, read.PerPage, read.Page, checksRunNames(read.Runs)))
	}
	if strings.Join(got, "\n") != strings.Join(kept, "\n") {
		t.Fatalf("the fake kept the reads\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(kept, "\n"))
	}
}

// A listing parameter the fake does not model — GitHub's own check_name, status and app_id among
// them — and a value GitHub itself would refuse are answered 400 and kept with the credentials it
// refused (Refused), never passed over: a reader that asked for a part of the commit's runs and
// was served every one of them reads as a reader that asked for them all. Such a request serves
// no run and is no read. The control: the listing Envoy's poller sends, filter=all&per_page=100
// &page=1, is answered and kept.
func TestTheChecksFakeRefusesAListingParameterItDoesNotModel(t *testing.T) {
	t.Parallel()
	c := ServeChecks(t)
	token := "Bearer " + checksMintedToken(t, c)
	c.SetCheckRun(checksAuthRepo, checksAuthHead, CheckRun{ID: 1, Name: "unit", Status: "completed", Conclusion: "failure"})
	path := "/repos/" + checksAuthRepo + "/commits/" + checksAuthHead + "/check-runs"
	var want []string
	for _, row := range []struct{ query, why string }{
		{query: "check_name=unit", why: `the listing's check_name is no parameter this fake models`},
		{query: "filter=newest", why: `filter "newest" is neither latest nor all`},
		{query: "per_page=none", why: `per_page "none" is no whole number above zero`},
		{query: "page=0", why: `page "0" is no whole number above zero`},
	} {
		status, message, body := checksRequest(t, http.MethodGet, c.URL+path+"?"+row.query, token)
		if status != http.StatusBadRequest || message != row.why {
			t.Errorf("the listing %q answered %d %q: %s, want 400 %q", row.query, status, message, body, row.why)
		}
		if strings.Contains(string(body), "check_runs") {
			t.Errorf("the listing %q answered %s, want no run served", row.query, body)
		}
		want = append(want, "GET "+path+"?"+row.query+": "+row.why)
	}
	if refused := c.Refused(); strings.Join(refused, "\n") != strings.Join(want, "\n") {
		t.Errorf("the fake kept the refusals\n%s\nwant\n%s", strings.Join(refused, "\n"), strings.Join(want, "\n"))
	}
	if reads := c.Reads(); len(reads) != 0 {
		t.Errorf("the fake kept %d reads of a listing it refused, want none", len(reads))
	}

	total, served := checksListing(t, c, token, "filter=all&per_page=100&page=1")
	if total != 1 || served != "unit#1" {
		t.Fatalf("the poller's own listing answered total_count %d and served %s, want 1 and unit#1", total, served)
	}
	if reads := c.Reads(); len(reads) != 1 {
		t.Fatalf("the fake kept %d reads of the listing it answered, want 1", len(reads))
	}
}

// checksListing reads c's check runs of the commit the credential tests read, with query as the
// listing's query, and answers the total_count it counted and the runs it served as
// checksRunNames names them.
func checksListing(t *testing.T, c *Checks, token, query string) (int, string) {
	t.Helper()
	target := c.URL + "/repos/" + checksAuthRepo + "/commits/" + checksAuthHead + "/check-runs"
	if query != "" {
		target += "?" + query
	}
	status, message, body := checksRequest(t, http.MethodGet, target, token)
	if status != http.StatusOK {
		t.Fatalf("the listing %q answered %d %q: %s", query, status, message, body)
	}
	var answer struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("read the answer to the listing %q, %s: %v", query, body, err)
	}
	served := make([]CheckRun, len(answer.CheckRuns))
	for i, run := range answer.CheckRuns {
		served[i] = CheckRun{ID: run.ID, Name: run.Name}
	}
	return answer.TotalCount, checksRunNames(served)
}

// checksRunNames is runs as "<name>#<id>, …" in the order they were served, and "none" for no run
// at all, so a listing that served nothing and one that served a run read differently.
func checksRunNames(runs []CheckRun) string {
	if len(runs) == 0 {
		return "none"
	}
	named := make([]string, len(runs))
	for i, run := range runs {
		named[i] = fmt.Sprintf("%s#%d", run.Name, run.ID)
	}
	return strings.Join(named, ", ")
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
