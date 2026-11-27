package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// censusStandIns is Dispatch and GitHub as the census's candidate reads read them, each GET
// counted by its path: the issues' links and results, and the pull requests a read names.
type censusStandIns struct {
	mu       sync.Mutex
	gets     map[string]int
	failing  map[string]int
	dispatch *httptest.Server
	github   *httptest.Server
}

func newCensusStandIns(t *testing.T) *censusStandIns {
	t.Helper()
	s := &censusStandIns{gets: map[string]int{}, failing: map[string]int{}}
	count := func(r *http.Request) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.gets[r.URL.Path]++
		if s.failing[r.URL.Path] > 0 {
			s.failing[r.URL.Path]--
			return true
		}
		return false
	}
	s.dispatch = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count(r)
		key := strings.TrimPrefix(r.URL.Path, "/api/v1/issues/")
		switch {
		case strings.HasSuffix(key, "/results"):
			_ = json.NewEncoder(w).Encode(map[string]any{"issue": strings.TrimSuffix(key, "/results"), "checks": []any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "project": "ACME", "title": key, "status": "done", "parent": nil, "rank": "A",
				"labels": []string{}, "last_seq": 9, "resolved_repo": "acme/widgets", "blocked_by": []string{}, "claim": nil, "children": []any{},
				"external_links": []any{}})
		}
	}))
	t.Cleanup(s.dispatch.Close)
	pulls := map[string]map[string]any{
		"/repos/acme/widgets/pulls/14":     {"number": 14, "merged": true, "merge_commit_sha": strings.Repeat("e", 40), "merged_at": "2026-11-20T10:00:00Z", "head": map[string]string{"ref": "legion/ACME-4", "sha": strings.Repeat("d", 40)}, "base": map[string]string{"ref": "main"}},
		"/repos/acme/old-widgets/pulls/51": {"number": 51, "merged": false, "head": map[string]string{"ref": "legion/ACME-5", "sha": "h51"}, "base": map[string]string{"ref": "main"}},
		"/repos/acme/widgets/pulls/52":     {"number": 52, "merged": false, "head": map[string]string{"ref": "legion/ACME-5", "sha": "h52"}, "base": map[string]string{"ref": "main"}},
	}
	s.github = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count(r) {
			http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
			return
		}
		if pull, ok := pulls[r.URL.Path]; ok {
			_ = json.NewEncoder(w).Encode(pull)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pulls") && strings.Contains(r.URL.Path, "/commits/") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	t.Cleanup(s.github.Close)
	return s
}

// count is how many GETs a stand-in was asked at path.
func (s *censusStandIns) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[path]
}

// candidateConfig is a legion.yaml of project ACME whose postgres_dsn is dsn and whose Dispatch is
// dispatchURL, with a Dispatch token file and both GitHub Apps on a token command that releases
// one installation token of acme.
func candidateConfig(t *testing.T, dsn, dispatchURL string) string {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "dispatch-token")
	if err := os.WriteFile(token, []byte("census-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(dir, "release")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' '{\"installations\":{\"acme\":{\"installation_id\":7,\"token\":\"ghs_census\",\"expires_at\":\"%s\"}}}'\n",
		time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339))
	if err := os.WriteFile(release, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf(`project: ACME
port: 13370
postgres_dsn: %q
state_dir: %s
dispatch_url: %s
dispatch_token_file: %s
nats_urls: [nats://127.0.0.1:4222]
projects: {ACME: {}}
github_apps:
  implement: {app_id: "1", token_command: %s}
  review: {app_id: "2", token_command: %s}
`, dsn, filepath.Join(dir, "state"), dispatchURL, token, release, release)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// read is the values of the census's line label, by name.
func (c census) read(label string) map[string]string {
	fields := c.lines[label]
	values := map[string]string{}
	for i := 0; i+1 < len(fields); i += 2 {
		values[fields[i]] = fields[i+1]
	}
	return values
}

// The census reads the candidates of each PARTIAL and NONE issue, with the merge read's own stages
// and its pass of eight GETs, and resumes from its private progress file. It writes no issue, no
// outbox row and no fact. An ANSWERED issue contacts no candidate source. Each candidate issue is
// answering, completed and unread, or unfinished, and the three sum to the candidate cohort. A
// failed read and an interrupted one stay unfinished, never empty. The report names the GETs, the
// retries, the uncertain attempts, the passes observed and their lower bound, the token requests,
// and Dispatch's downstream work as UNKNOWN. A run bounded to one GET, then a run resumed for one
// GET, observe two passes, where the lower bound for two GETs is one, and the second replays no
// first read.
func TestMergeHistoryCensusResumesWithoutApplyingIt(t *testing.T) {
	t.Parallel()
	dsn, st := mergeHistoryDatabase(t, false)
	const old, renamed = "acme/old-widgets", "acme/widgets"
	execAll(t, st,
		historyIssue("ACME-1", "ACME", renamed, "done", "todo"),
		`update issues set merge_number = 11, merge_head = '`+strings.Repeat("a", 40)+`', merge_sha = '`+strings.Repeat("b", 40)+
			`', merge_base = 'main', merge_repo = 'acme/widgets', merged_at = now() where key = 'ACME-1'`,
		historyIssue("ACME-2", "ACME", old, "done", "done"),
		historyPullRequest("ACME-2", old, 12, "merged"),
		historyIssue("ACME-3", "ACME", renamed, "done", "done"),
		historyCheck("ACME-3", "ACME", renamed, 13),
		historyIssue("ACME-4", "ACME", renamed, "done", "todo"),
		historyPullRequest("ACME-4", renamed, 14, "open"),
		historyIssue("ACME-5", "ACME", old, "done", "done"),
		historyClock("ACME-5", old, 51),
		historyClock("ACME-5", renamed, 52),
		historyIssue("ACME-6", "ACME", renamed, "done", "done"),
		historyCheck("ACME-6", "ACME", renamed, 0),
		historyIssue("ACME-7", "ACME", renamed, "done", "todo"),
	)
	standIns := newCensusStandIns(t)
	config := candidateConfig(t, dsn, standIns.dispatch.URL)
	progressFile := filepath.Join(t.TempDir(), "census-progress.json")
	rows := testdb.TableRows(t, st.Pool())
	passes := 0
	run := func(maxGets string) census {
		t.Helper()
		args := []string{"--config", config, "--project", "ACME", "--repo", old, "--repo", renamed, "--read-candidates", "--progress", progressFile,
			"--github-api", standIns.github.URL}
		if maxGets != "" {
			args = append(args, "--max-gets", maxGets)
		}
		read := runCensus(t, args...)
		if read.code != 0 {
			t.Fatalf("the census's candidate read exited %d, want 0:\n%s", read.code, read.stdout)
		}
		passes++
		return read
	}

	first := run("1")
	if got := first.read("read_cost"); got["gets"] != "1" || got["passes"] != "1" {
		t.Fatalf("the run bounded to one GET cost %v, want one GET in one pass:\n%s", got, first.stdout)
	}
	resumed := run("1")
	if got := resumed.read("read_cost"); got["gets"] != "2" || got["passes"] != "2" || got["passes_lower_bound"] != "1" {
		t.Fatalf("the resumed run cost %v, want two GETs in two observed passes, whose lower bound is one:\n%s", got, resumed.stdout)
	}
	if standIns.count("/api/v1/issues/ACME-4") != 1 || standIns.count("/api/v1/issues/ACME-4/results") != 1 {
		t.Fatalf("Dispatch read ACME-4 %d times and its results %d, want each once: the resumed run replays no first read",
			standIns.count("/api/v1/issues/ACME-4"), standIns.count("/api/v1/issues/ACME-4/results"))
	}

	standIns.mu.Lock()
	standIns.failing["/repos/acme/old-widgets/pulls/51"] = 1
	standIns.mu.Unlock()
	// A pass sends at most eight GETs, and the four candidate issues need more: the reads go on,
	// pass by pass, until a pass sends none, which is where every issue is answered, unread, or
	// waiting for a retry that is not due.
	var full census
	for spent := ""; ; {
		full = run("")
		if got := full.read("read_cost")["gets"]; got == spent {
			break
		} else {
			spent = got
		}
	}
	want := map[string]map[string]string{
		"read_PARTIAL": {"candidates": "3", "answering": "1", "unread": "1", "unfinished": "1", "unfinished_active": "1", "unfinished_wait": "0"},
		"read_NONE":    {"candidates": "1", "answering": "0", "unread": "1", "unfinished": "0", "unfinished_active": "0", "unfinished_wait": "0"},
		"read_total":   {"candidates": "4", "answering": "1", "unread": "2", "unfinished": "1", "unfinished_active": "1", "unfinished_wait": "0"},
		"read_cost": {"gets": "11", "retries": "0", "uncertain": "0", "passes": strconv.Itoa(passes), "passes_lower_bound": "2", "token_requests": "3",
			"dispatch_downstream": "UNKNOWN"},
	}
	for label, values := range want {
		if got := full.read(label); !reflect.DeepEqual(got, values) {
			saved, _ := os.ReadFile(progressFile)
			t.Fatalf("the full pass printed %s %v, want %v:\n%s\nits saved progress: %s", label, got, values, full.stdout, saved)
		}
	}
	for _, path := range []string{"/api/v1/issues/ACME-1", "/api/v1/issues/ACME-2", "/api/v1/issues/ACME-3", "/repos/acme/widgets/pulls/11",
		"/repos/acme/old-widgets/pulls/12", "/repos/acme/widgets/pulls/13"} {
		if n := standIns.count(path); n != 0 {
			t.Fatalf("the census asked %s %d times, want never: an ANSWERED issue contacts no candidate source", path, n)
		}
	}
	if changed := testdb.ChangedTables(rows, testdb.TableRows(t, st.Pool())); len(changed) != 0 {
		t.Fatalf("the census's candidate reads changed the rows of %v, want none written: no issue, outbox row or fact", changed)
	}
	info, err := os.Stat(progressFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the progress file = %v, %v; want it private (0600)", info, err)
	}
}
