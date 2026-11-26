package codesurvival

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// legion is the repository the captured answers of testdata/github are of.
const legion = "cybermonklabs/legion"

// The commits of the captured commits page (testdata/github/commits-page.txt): the merge
// 604ff097 of the side branch a5b9b863 into its first parent ec386297, the side branch's other
// two commits, and the first parents of ec386297 and of d28a0b6f, which the page does not list.
const (
	mergeSHA       = "604ff0971f8b7afd4368134ab35cd1c1648775c4"
	firstParentSHA = "ec3862972a80940126bc43622a2f3cd313eafb92"
	sideTipSHA     = "a5b9b8634f40c4333158a76cc624310bbe933f7d"
	sideMidSHA     = "55beb54088ce8f5277e7acd30526cb57b2c630cd"
	sideBaseSHA    = "d28a0b6fd517963ab23fff5c5339a7431669968e"
	grandParentSHA = "d7528aa821a9a62ab7322fb46223640a79c8142a"
	sideParentSHA  = "6026a4512831dfce2bca92004e20665527b9c497"
)

// captured is one answer of testdata/github: GitHub's status, headers and body.
type captured struct {
	status int
	header http.Header
	body   []byte
}

// capture reads testdata/github/<name>, as `gh api -i` printed it: a status line, the headers
// up to a blank line, and the body.
func capture(t *testing.T, name string) captured {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatalf("read the capture %s: %v", name, err)
	}
	line, rest, _ := bytes.Cut(raw, []byte("\n"))
	fields := strings.Fields(string(line))
	if len(fields) < 2 {
		t.Fatalf("the capture %s starts %q, want a status line", name, line)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("the capture %s's status line %q: %v", name, line, err)
	}
	answer := captured{status: status, header: http.Header{}}
	for {
		line, rest, _ = bytes.Cut(rest, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			break
		}
		key, value, found := strings.Cut(string(line), ": ")
		if !found {
			t.Fatalf("the capture %s holds the header line %q", name, line)
		}
		answer.header.Add(key, value)
	}
	answer.body = rest
	return answer
}

// serve answers w as GitHub answered the capture, with its headers but its Link, which named
// GitHub's own origin; next is the Link of the fake's next page, "" for none.
func (c captured) serve(w http.ResponseWriter, next string) {
	for key, values := range c.header {
		switch http.CanonicalHeaderKey(key) {
		case "Link", "Content-Length", "Transfer-Encoding", "Content-Encoding":
			continue
		}
		w.Header()[http.CanonicalHeaderKey(key)] = values
	}
	if next != "" {
		w.Header().Set("Link", `<`+next+`>; rel="next"`)
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body)
}

// fakeGitHub is the GitHub the tests' reads reach: the App's installation lookup and token mint,
// the repository and its branch, and each route a test plans with handle. Every request it is
// sent is recorded as "<METHOD> <path>[?<query>]", for a test to count what the reads asked for.
type fakeGitHub struct {
	t   *testing.T
	mux *http.ServeMux
	url string

	mu       sync.Mutex
	requests []string
	// installation is the status the installation lookup answers in place of the installation
	// (404: the App is not installed), 0 for the installation; contents is the Contents
	// permission the installation names.
	installation int
	contents     string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, mux: http.NewServeMux(), contents: "read"}
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		status, contents := f.installation, f.contents
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
			return
		}
		writeJSON(t, w, map[string]any{"id": 1, "app_slug": "dispatch-test", "permissions": map[string]string{"contents": contents}})
	})
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{"token": "ghs_survival", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	})
	f.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { githubtest.Unexpected(t, w, r) })
	f.url = githubtest.Server(t, githubtest.Spec{Record: f.record, Handle: f.mux.ServeHTTP})
	return f
}

func (f *fakeGitHub) record(r *http.Request) {
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	f.mu.Lock()
	f.requests = append(f.requests, line)
	f.mu.Unlock()
}

// install makes the installation lookup answer status in place of the installation (0 for the
// installation), and the installation name contents as its Contents permission.
func (f *fakeGitHub) install(status int, contents string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installation, f.contents = status, contents
}

// handle plans one route of the fake.
func (f *fakeGitHub) handle(pattern string, handler http.HandlerFunc) {
	f.mux.HandleFunc(pattern, handler)
}

// asked is every request the fake was sent whose line starts with prefix, in order.
func (f *fakeGitHub) asked(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []string
	for _, line := range f.requests {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// count is how many requests the fake was sent whose line is line.
func (f *fakeGitHub) count(line string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		if request == line {
			n++
		}
	}
	return n
}

// sleeps records the waits a reader made between two tries, which take no time.
type sleeps struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleeps) made() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// newReader is a GitHub reader of the fake, through an App client signed with a key made for the
// test, whose waits between tries are recorded and take no time. pool is the database the
// attribution reads, nil for a test that attributes nothing.
func newReader(t *testing.T, fake *fakeGitHub, pool *store.Pool) (*GitHub, *sleeps) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.survival", PEM: pemText}, fake.url)
	if err != nil || client == nil {
		t.Fatalf("new App client: %v", err)
	}
	reader := NewGitHub(client, pool, delivery.NewAttributor(refs.Renames{}))
	waits := &sleeps{}
	reader.sleep = waits.sleep
	return reader, waits
}

// repository is repo read open with branch at tip, as Open answers it.
func repository(repo, branch, tip string) Repository {
	owner, name, _ := strings.Cut(repo, "/")
	return Repository{Repo: repo, Owner: owner, Name: name, Branch: branch, Tip: tip}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode the fake's answer: %v", err)
	}
}

// listed is one entry of a commits list page, as GitHub writes it.
func listed(sha string, committed time.Time, parents ...string) map[string]any {
	entries := make([]map[string]string, len(parents))
	for i, parent := range parents {
		entries[i] = map[string]string{"sha": parent}
	}
	return map[string]any{
		"sha":     sha,
		"commit":  map[string]any{"committer": map[string]any{"name": "GitHub", "date": committed.Format(time.RFC3339)}},
		"parents": entries,
	}
}

func shasOf(chain []survival.Commit) []string {
	shas := make([]string, len(chain))
	for i, commit := range chain {
		shas[i] = commit.SHA
	}
	return shas
}

// The chain is the first-parent walk from the tip of every commit GitHub lists, oldest first, with
// each commit's parents and committer time: the side branch's commits a merge brought in, which
// GitHub lists between the merge and its first parent, are no part of it, and neither is a commit
// GitHub lists that no first parent of the walk reaches. The list is read page by page as GitHub's
// Link header names the next, from the tip Open read.
func TestTheChainReadWalksFirstParentsAndIgnoresOtherCommits(t *testing.T) {
	fake := newFakeGitHub(t)
	page := capture(t, "commits-page.txt")
	off := "0ff0000000000000000000000000000000000001"
	grandParentAt := time.Date(2026, 11, 25, 9, 12, 0, 0, time.UTC)
	// GitHub's own Link names the repository by its id, so the second page is read wherever the
	// first page's Link sends the read.
	checkQuery := func(r *http.Request) {
		query := r.URL.Query()
		if query.Get("sha") != mergeSHA || query.Get("since") != "2026-11-20T00:00:00Z" || query.Get("per_page") != "100" {
			t.Errorf("the commits list was asked for %q, want sha=%s, since=2026-11-20T00:00:00Z and per_page=100", r.URL.RawQuery, mergeSHA)
		}
	}
	fake.handle("GET /repos/cybermonklabs/legion/commits", func(w http.ResponseWriter, r *http.Request) {
		checkQuery(r)
		page.serve(w, fake.url+"/repositories/1146927990/commits?"+r.URL.RawQuery+"&page=2")
	})
	fake.handle("GET /repositories/1146927990/commits", func(w http.ResponseWriter, r *http.Request) {
		checkQuery(r)
		if r.URL.Query().Get("page") != "2" {
			t.Errorf("the commits list's next page was asked for %q, want the page=2 its Link named", r.URL.RawQuery)
		}
		writeJSON(t, w, []map[string]any{
			listed(off, grandParentAt.Add(time.Hour), grandParentSHA),
			listed(grandParentSHA, grandParentAt, "1111111111111111111111111111111111111111"),
		})
	})
	reader, _ := newReader(t, fake, nil)

	chain, err := reader.Chain(testwait.Context(t), repository(legion, "main", mergeSHA), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("the chain read: %v", err)
	}
	want := []survival.Commit{
		{SHA: grandParentSHA, Parents: []string{"1111111111111111111111111111111111111111"}, CommittedAt: grandParentAt},
		{SHA: firstParentSHA, Parents: []string{grandParentSHA}, CommittedAt: time.Date(2026, 11, 25, 14, 29, 4, 0, time.UTC)},
		{SHA: mergeSHA, Parents: []string{firstParentSHA, sideTipSHA}, CommittedAt: time.Date(2026, 11, 26, 0, 4, 44, 0, time.UTC)},
	}
	if !reflect.DeepEqual(chain, want) {
		t.Fatalf("the chain is %+v, want the first parents %v from the tip, oldest first, with neither the side branch's %s, %s and %s nor %s",
			chain, shasOf(want), sideTipSHA[:8], sideMidSHA[:8], sideBaseSHA[:8], off[:8])
	}
	if got := reader.Counts().Chain; got != 2 {
		t.Fatalf("the chain read counted %d pages, want the 2 it read", got)
	}
	if first, next := fake.asked("GET /repos/cybermonklabs/legion/commits"), fake.asked("GET /repositories/1146927990/commits"); len(first) != 1 || len(next) != 1 {
		t.Fatalf("the commits list was asked for %v and %v, want its two pages once each", first, next)
	}
}

// A chain read asks GitHub for the commits since its day less three days (ChainSince) and its walk
// stops at the first commit dated before that: an older commit GitHub lists anyway is no part of
// the chain, and neither is a commit dated after the since that only a commit dated before it
// reaches.
func TestTheChainReadStopsAtTheFirstDayLessThreeDays(t *testing.T) {
	oldest := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	since := ChainSince(oldest)
	if want := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC); !since.Equal(want) {
		t.Fatalf("ChainSince(%s) = %s, want %s: the day less three days", oldest.Format(time.DateOnly), since, want)
	}
	if got := ChainSince(oldest.Add(17 * time.Hour)); !got.Equal(since) {
		t.Fatalf("ChainSince of a time of day %s = %s, want the day's %s", oldest.Add(17*time.Hour), got, since)
	}
	sha := func(n int) string { return fmt.Sprintf("%040x", n) }
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got != "2026-09-07T00:00:00Z" {
			t.Errorf("the commits list was asked since %q, want 2026-09-07T00:00:00Z", got)
		}
		writeJSON(t, w, []map[string]any{
			listed(sha(6), at(9, 12, 8, 0), sha(5)),
			listed(sha(5), at(9, 10, 8, 0), sha(4)),
			listed(sha(4), at(9, 7, 0, 0), sha(3)),
			listed(sha(3), at(9, 6, 23, 59), sha(2)),
			listed(sha(2), at(9, 8, 12, 0), sha(1)),
			listed(sha(1), at(9, 5, 12, 0), sha(0)),
		})
	})
	reader, _ := newReader(t, fake, nil)

	chain, err := reader.Chain(testwait.Context(t), repository(widgets, "main", sha(6)), since)
	if err != nil {
		t.Fatalf("the chain read: %v", err)
	}
	if got, want := shasOf(chain), []string{sha(4), sha(5), sha(6)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the chain is %v, want %v: the walk stops at %s, dated 23:59 the day before the since", got, want, sha(3))
	}
}

// The pull request facts come from the closed pull requests list of the repository, of every base,
// newest update first, read page by page until a page's last pull request was updated before D0,
// and a pull request is a commit's when it merged as that commit (merge_commit_sha). A closed pull
// request that never merged is no change, whatever commit GitHub names for it, and neither is a
// commit no pull request merged as.
func TestThePullRequestFactsMatchByMergeCommit(t *testing.T) {
	page := capture(t, "pulls-page.txt")
	var captured []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(page.body, &captured); err != nil || len(captured) != 3 {
		t.Fatalf("decode the captured pulls page: %d pull requests, %v", len(captured), err)
	}
	byNumber := map[int]int{}
	for i, pull := range captured {
		byNumber[pull.Number] = i
	}
	const (
		merged5641 = "6e2e4def26e632374d3f33aafc3c4f4b0fcee3c9"
		merged4592 = "4a9390dc2a2fed31c628df87d02d08c0643d1a45"
		unmerged   = "7e57000000000000000000000000000000000001"
		noPull     = "7e57000000000000000000000000000000000002"
	)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/cybermonklabs/legion/pulls", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("state") != "closed" || query.Has("base") || query.Get("sort") != "updated" ||
			query.Get("direction") != "desc" || query.Get("per_page") != "100" {
			t.Errorf("the pulls list was asked for %q, want state=closed, sort=updated, direction=desc and per_page=100, and no base", r.URL.RawQuery)
		}
		switch query.Get("page") {
		case "":
			page.serve(w, fake.url+"/repos/cybermonklabs/legion/pulls?"+r.URL.RawQuery+"&page=2")
		case "2":
			w.Header().Set("Link", `<`+fake.url+`/repos/cybermonklabs/legion/pulls?page=3>; rel="next"`)
			writeJSON(t, w, []map[string]any{{
				"number": 4500, "html_url": "https://github.com/cybermonklabs/legion/pull/4500", "title": "closed, never merged",
				"user": map[string]any{"login": "alice"}, "head": map[string]any{"ref": "spike", "repo": map[string]any{"full_name": legion}},
				"merge_commit_sha": unmerged, "merged_at": nil, "updated_at": "2026-11-26T01:00:00Z",
			}, {
				"number": 4400, "html_url": "https://github.com/cybermonklabs/legion/pull/4400", "title": "updated before D0",
				"user": map[string]any{"login": "bob"}, "head": map[string]any{"ref": "old", "repo": map[string]any{"full_name": legion}},
				"merge_commit_sha": "7e57000000000000000000000000000000000003", "merged_at": "2026-11-20T01:00:00Z", "updated_at": "2026-11-25T23:59:59Z",
			}})
		default:
			t.Errorf("the pulls list was read past the page whose last pull request was updated before D0: %s", r.URL.RawQuery)
			writeJSON(t, w, []any{})
		}
	})
	reader, _ := newReader(t, fake, nil)
	chain := []survival.Commit{{SHA: noPull}, {SHA: merged4592}, {SHA: unmerged}, {SHA: merged5641}}

	pulls, err := reader.PullRequests(testwait.Context(t), repository(legion, "main", merged5641), time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), chain)
	if err != nil {
		t.Fatalf("the pull request facts: %v", err)
	}
	want := map[string]PullRequest{
		merged5641: {
			Number: 5641, URL: "https://github.com/cybermonklabs/legion/pull/5641", Title: captured[byNumber[5641]].Title,
			Body: captured[byNumber[5641]].Body, HeadRef: "fix/sandbox-await-pod-gone-wait", HeadRepo: legion,
			Author: "sjawhar-agent[bot]", MergedAt: time.Date(2026, 11, 26, 10, 48, 55, 0, time.UTC),
		},
		merged4592: {
			Number: 4592, URL: "https://github.com/cybermonklabs/legion/pull/4592", Title: captured[byNumber[4592]].Title,
			Body: captured[byNumber[4592]].Body, HeadRef: "legion/INFRA-4728", HeadRepo: legion,
			Author: "legion-implementer[bot]", MergedAt: time.Date(2026, 11, 26, 10, 37, 0, 0, time.UTC),
		},
	}
	if !reflect.DeepEqual(pulls, want) {
		t.Fatalf("the pull requests of the chain are %+v, want #5641 and #4592 by their merge commits, and neither #4714, merged as no commit of the chain, nor #4500, which never merged", pulls)
	}
	if got := reader.Counts().Pulls; got != 2 {
		t.Fatalf("the pulls list counted %d pages, want the 2 it read", got)
	}
}

// A pull request merged into another branch, one of a stack, is the change of the commit it
// merged as where that commit is on the chain: the list names no base, so PR #1284 of
// cybermonklabs/legion, whose base was legion/LEGION-208-4b-predicates and whose merge commit
// fb96b8da is on main's first-parent chain, is matched, and a stacked pull request whose merge
// commit is on no commit of the chain is not.
func TestAPullRequestOfAnotherBaseIsMatchedByItsMergeCommit(t *testing.T) {
	const (
		stacked  = "fb96b8da5256494583eea9494b3404c7bb7df86c"
		offChain = "7e57000000000000000000000000000000000004"
		onMain   = "7e57000000000000000000000000000000000005"
	)
	pull := func(number int, base, head, sha, merged string) map[string]any {
		return map[string]any{
			"number": number, "html_url": fmt.Sprintf("https://github.com/cybermonklabs/legion/pull/%d", number), "title": head,
			"user": map[string]any{"login": "legion-implementer[bot]"}, "head": map[string]any{"ref": head, "repo": map[string]any{"full_name": legion}},
			"base": map[string]any{"ref": base}, "merge_commit_sha": sha, "merged_at": merged, "updated_at": merged,
		}
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/cybermonklabs/legion/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("base") {
			t.Errorf("the pulls list was asked for %q, which names a base", r.URL.RawQuery)
			writeJSON(t, w, []any{})
			return
		}
		writeJSON(t, w, []map[string]any{
			pull(1300, "main", "legion/LEGION-208-4b", onMain, "2026-09-25T01:00:00Z"),
			pull(1292, "legion/LEGION-208-4b-supervise", "legion/LEGION-208-4b-workspace-loss", offChain, "2026-09-25T00:39:18Z"),
			pull(1284, "legion/LEGION-208-4b-predicates", "legion/LEGION-208-4b-spawnspec", stacked, "2026-09-25T00:39:16Z"),
			pull(1100, "main", "legion/old", "7e57000000000000000000000000000000000006", "2026-09-01T00:00:00Z"),
		})
	})
	reader, _ := newReader(t, fake, nil)
	chain := []survival.Commit{{SHA: stacked}, {SHA: onMain}}

	pulls, err := reader.PullRequests(testwait.Context(t), repository(legion, "main", onMain), time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), chain)
	if err != nil {
		t.Fatalf("the pull request facts: %v", err)
	}
	if got, ok := pulls[stacked]; !ok || got.Number != 1284 || got.HeadRef != "legion/LEGION-208-4b-spawnspec" || len(pulls) != 2 || pulls[onMain].Number != 1300 {
		t.Fatalf("the pull requests of the chain are %+v, want #1284 by its merge commit %s though its base is another branch, #1300 by %s, and not #1292, whose merge commit is on no commit of the chain", pulls, stacked, onMain)
	}
	if asked := fake.asked("GET /repos/cybermonklabs/legion/pulls"); len(asked) != 1 {
		t.Fatalf("the pulls list was read %d times, want once: %v", len(asked), asked)
	}
}

// A commit of one parent is read through the commit route, asking for the diff, and its patch is
// the diff as the replay reads it, counted on that route.
func TestAPatchOfOneParentReadsTheCommitRoute(t *testing.T) {
	answer := capture(t, "diff-answer.txt")
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/cybermonklabs/legion/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha") != firstParentSHA || r.Header.Get("Accept") != "application/vnd.github.diff" || r.URL.RawQuery != "" {
			t.Errorf("the commit route was asked for %s?%s with Accept %q, want %s with the diff media type", r.PathValue("sha"), r.URL.RawQuery, r.Header.Get("Accept"), firstParentSHA)
		}
		answer.serve(w, "")
	})
	reader, _ := newReader(t, fake, nil)

	read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), survival.Commit{SHA: firstParentSHA, Parents: []string{grandParentSHA}})
	if err != nil {
		t.Fatalf("the patch read: %v", err)
	}
	files, err := survival.ParseDiff(answer.body)
	if err != nil {
		t.Fatalf("parse the captured diff: %v", err)
	}
	if read.Route != RouteDiff || read.Bytes != 14635 || !reflect.DeepEqual(read.Patch, survival.Patch{Files: files}) {
		t.Fatalf("the patch read through %s, %d bytes, %d files, %d withheld; want the commit route's 14635 bytes as %d files",
			read.Route, read.Bytes, len(read.Patch.Files), read.Patch.Withheld, len(files))
	}
	if counts := reader.Counts(); counts.Diff != 1 || counts.Compare != 0 || counts.Files != 0 || counts.Bytes != 14635 {
		t.Fatalf("the read counted %+v, want one diff request of 14635 bytes", counts)
	}
}

// A commit of two parents is read through the compare route from its first parent, asking for the
// diff: the patch the merge brought onto the branch.
func TestAPatchOfTwoParentsReadsTheCompareRoute(t *testing.T) {
	diff, err := os.ReadFile(filepath.Join("..", "delivery", "survival", "testdata", "diffs", "merge-compare.diff"))
	if err != nil {
		t.Fatalf("read T2's compare-route capture: %v", err)
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/cybermonklabs/legion/compare/{range}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("range") != firstParentSHA+"..."+mergeSHA || r.Header.Get("Accept") != "application/vnd.github.diff" {
			t.Errorf("the compare route was asked for %s with Accept %q, want %s...%s with the diff media type", r.PathValue("range"), r.Header.Get("Accept"), firstParentSHA, mergeSHA)
		}
		w.Header().Set("Content-Type", "application/vnd.github.diff; charset=utf-8")
		_, _ = w.Write(diff)
	})
	reader, _ := newReader(t, fake, nil)

	read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), survival.Commit{SHA: mergeSHA, Parents: []string{firstParentSHA, sideTipSHA}})
	if err != nil {
		t.Fatalf("the patch read: %v", err)
	}
	files, err := survival.ParseDiff(diff)
	if err != nil {
		t.Fatalf("parse the compare-route capture: %v", err)
	}
	if read.Route != RouteCompare || !reflect.DeepEqual(read.Patch, survival.Patch{Files: files}) {
		t.Fatalf("the merge's patch read through %s as %d files, want the compare route's %d", read.Route, len(read.Patch.Files), len(files))
	}
	if counts := reader.Counts(); counts.Compare != 1 || counts.Diff != 0 || counts.Files != 0 {
		t.Fatalf("the read counted %+v, want one compare request", counts)
	}
}

// filesPages are the two pages of a commit's files[] the fallback reads: T2's captured page of 100
// files, three of them with changes and no patch, and a second page of one file.
func filesPages(t *testing.T) (first, second []byte, entries []survival.FilesEntry) {
	t.Helper()
	first, err := os.ReadFile(filepath.Join("..", "delivery", "survival", "testdata", "diffs", "files-page.json"))
	if err != nil {
		t.Fatalf("read T2's files[] page: %v", err)
	}
	second = []byte(`{"sha":"1ffc8dec7e789c9cb2374f10a8d69069edff73ed","files":[{"filename":"docs/after.md","status":"modified","changes":2,"patch":"@@ -1 +1 @@\n-old\n+new"}]}`)
	for _, page := range [][]byte{first, second} {
		var decoded struct {
			Files []survival.FilesEntry `json:"files"`
		}
		if err := json.Unmarshal(page, &decoded); err != nil {
			t.Fatalf("decode a files[] page: %v", err)
		}
		entries = append(entries, decoded.Files...)
	}
	return first, second, entries
}

// serveFiles plans the commit route of sha on the fake: the diff, answered by diff (a request for
// it fails the test where diff is nil), and the files[] pages, its first with a Link to its second.
func serveFiles(t *testing.T, fake *fakeGitHub, sha string, diff func(w http.ResponseWriter)) {
	t.Helper()
	first, second, _ := filesPages(t)
	fake.handle("GET /repos/cybermonklabs/legion/commits/"+sha, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.RawQuery == "" && r.Header.Get("Accept") == "application/vnd.github.diff" && diff != nil {
			diff(w)
			return
		}
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			t.Errorf("the files[] route of %s was asked for the diff", sha)
		}
		switch {
		case query.Get("per_page") == "100" && query.Get("page") == "1":
			w.Header().Set("Link", `<`+fake.url+`/repos/cybermonklabs/legion/commits/`+sha+`?per_page=100&page=2>; rel="next"`)
			_, _ = w.Write(first)
		case query.Get("per_page") == "100" && query.Get("page") == "2":
			_, _ = w.Write(second)
		default:
			t.Errorf("the files[] route of %s was asked for %q, want per_page=100 and a page", sha, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// A patch GitHub refuses -- a 406, a 422, a 5xx whose body names the diff -- or answers past 32
// MiB is read once through the commit's files[], every page of it, in the replay's form with the
// files GitHub withheld counted (survival.FilesPatch). The refused route is not asked again.
func TestARefusalOrAnOversizeAnswerReadsFilesOnce(t *testing.T) {
	_, _, entries := filesPages(t)
	files, withheld := survival.FilesPatch(entries)
	if withheld != 3 {
		t.Fatalf("the two files[] pages withhold %d files, want the captured page's 3", withheld)
	}
	refusals := []struct {
		name    string
		parents []string
		refuse  func(w http.ResponseWriter)
	}{{
		name: "a 406",
		refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprint(w, `{"message":"Sorry, this diff is unavailable.","status":"406"}`)
		},
	}, {
		name:    "a 422 of the compare route",
		parents: []string{firstParentSHA, sideTipSHA},
		refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Sorry, the diff exceeded the maximum number of files (300).","errors":[{"resource":"Commit","field":"diff","code":"too_large"}],"status":"422"}`)
		},
	}, {
		name: "a 502 whose body names the diff",
		refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message":"Server Error: Sorry, this diff is taking too long to generate.","status":"502"}`)
		},
	}, {
		name: "a 500 that says the patch is too large",
		refuse: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"The patch is too large to generate.","status":"500"}`)
		},
	}, {
		name: "an answer past 32 MiB",
		refuse: func(w http.ResponseWriter) {
			chunk := bytes.Repeat([]byte("+a line of an enormous diff\n"), 1<<15)
			for written := 0; written <= 32<<20; written += len(chunk) {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		},
	}}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			commit := survival.Commit{SHA: mergeSHA, Parents: tc.parents}
			refused := "GET /repos/cybermonklabs/legion/commits/" + mergeSHA
			if len(tc.parents) > 1 {
				refused = "GET /repos/cybermonklabs/legion/compare/" + tc.parents[0] + "..." + mergeSHA
				fake.handle(refused, func(w http.ResponseWriter, _ *http.Request) { tc.refuse(w) })
				serveFiles(t, fake, mergeSHA, nil)
			} else {
				serveFiles(t, fake, mergeSHA, tc.refuse)
			}
			reader, waits := newReader(t, fake, nil)

			read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), commit)
			if err != nil {
				t.Fatalf("the patch read GitHub refused: %v, want the files[] read in its place", err)
			}
			if read.Route != RouteFiles || !reflect.DeepEqual(read.Patch, survival.Patch{Files: files, Withheld: 3}) {
				t.Fatalf("the refused patch read through %s as %d files, %d withheld; want files[]'s %d files with 3 withheld",
					read.Route, len(read.Patch.Files), read.Patch.Withheld, len(files))
			}
			filesRoute := "GET /repos/cybermonklabs/legion/commits/" + mergeSHA + "?per_page=100&page="
			if fake.count(refused) != 1 || fake.count(filesRoute+"1") != 1 || fake.count(filesRoute+"2") != 1 {
				t.Fatalf("GitHub was asked %v, want the refused route once and each files[] page once", fake.asked("GET /repos/"))
			}
			if got := waits.made(); len(got) != 0 {
				t.Fatalf("the read waited %v, want no wait: a refusal is no failure to read again", got)
			}
			counts := reader.Counts()
			if counts.Files != 2 || counts.Diff+counts.Compare != 1 {
				t.Fatalf("the read counted %+v, want the refused request and the two files[] pages", counts)
			}
		})
	}
}

// A failure that is no refusal fails the read, and reads no files[] in its place: a status GitHub
// gives for no diff (a 404), a 5xx that names no diff, asked again until the tries are spent, and
// a refusal whose files[] GitHub then answers with a failure.
func TestAFailureThatIsNoRefusalFailsTheRun(t *testing.T) {
	failures := []struct {
		name string
		// answer is the diff route's answer, and files the files[] route's, nil where the read
		// must not reach it; tries is how many times the diff route is asked.
		answer func(w http.ResponseWriter)
		files  func(w http.ResponseWriter)
		tries  int
	}{{
		name: "a 404",
		answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found","status":"404"}`)
		},
		tries: 1,
	}, {
		name: "a 500 that names no diff",
		answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Server Error","status":"500"}`)
		},
		tries: 5,
	}, {
		name: "a refusal whose files[] GitHub cannot find",
		answer: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotAcceptable)
			fmt.Fprint(w, `{"message":"Sorry, this diff is unavailable.","status":"406"}`)
		},
		files: func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found","status":"404"}`)
		},
		tries: 1,
	}}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			route := "GET /repos/cybermonklabs/legion/commits/" + firstParentSHA
			fake.handle(route, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.RawQuery == "":
					tc.answer(w)
				case tc.files != nil:
					tc.files(w)
				default:
					t.Errorf("a failure that is no refusal read the files[] route: %s", r.URL.RawQuery)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			reader, _ := newReader(t, fake, nil)

			read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), survival.Commit{SHA: firstParentSHA, Parents: []string{grandParentSHA}})
			if err == nil {
				t.Fatalf("the read answered %d files through %s, want a failure", len(read.Patch.Files), read.Route)
			}
			if _, unreadable := Unreadable(err); unreadable {
				t.Fatalf("the read failed with %v, which names the repository unreadable; want a failure of the run", err)
			}
			filesPages := 0
			if tc.files != nil {
				filesPages = 1
			}
			if fake.count(route) != tc.tries || fake.count(route+"?per_page=100&page=1") != filesPages {
				t.Fatalf("GitHub was asked %v, want the diff route %d times and the files[] route %d", fake.asked(route), tc.tries, filesPages)
			}
		})
	}
}

// The budget is GitHub's rate limit as each answer names it, read from every read the reader
// makes: X-RateLimit-Limit, X-RateLimit-Remaining and X-RateLimit-Reset. An answer of the same
// reset window that arrives after a later one, as a worker's can, never raises what is left, and
// an answer of a later window replaces it. The bytes counted are the bytes GitHub answered.
func TestTheBudgetIsReadFromEachAnswer(t *testing.T) {
	commits, pulls, diff := capture(t, "commits-page.txt"), capture(t, "pulls-page.txt"), capture(t, "diff-answer.txt")
	reset := time.Unix(1795694313, 0).UTC()
	stale := http.Header{"X-Ratelimit-Limit": {"15000"}, "X-Ratelimit-Remaining": {"14990"}, "X-Ratelimit-Reset": {"1795694313"}}
	later := http.Header{"X-Ratelimit-Limit": {"15000"}, "X-Ratelimit-Remaining": {"14999"}, "X-Ratelimit-Reset": {"1795697913"}}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/cybermonklabs/legion/commits", func(w http.ResponseWriter, _ *http.Request) { commits.serve(w, "") })
	fake.handle("GET /repos/cybermonklabs/legion/pulls", func(w http.ResponseWriter, _ *http.Request) { pulls.serve(w, "") })
	fake.handle("GET /repos/cybermonklabs/legion/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("sha") {
		case firstParentSHA:
			diff.serve(w, "")
		case sideBaseSHA:
			captured{status: http.StatusOK, header: stale, body: []byte("")}.serve(w, "")
		default:
			captured{status: http.StatusOK, header: later, body: []byte("")}.serve(w, "")
		}
	})
	reader, _ := newReader(t, fake, nil)
	ctx := testwait.Context(t)
	repo := repository(legion, "main", mergeSHA)
	if got := reader.Budget(); got != (Budget{}) {
		t.Fatalf("a reader that has read nothing holds the budget %+v, want none", got)
	}

	if _, err := reader.Chain(ctx, repo, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("the chain read: %v", err)
	}
	if got, want := reader.Budget(), (Budget{Limit: 15000, Remaining: 13933, Reset: reset}); got != want {
		t.Fatalf("the budget after the commits page is %+v, want the page's own %+v", got, want)
	}
	if _, err := reader.PullRequests(ctx, repo, time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), nil); err != nil {
		t.Fatalf("the pull request facts: %v", err)
	}
	if got := reader.Budget().Remaining; got != 13931 {
		t.Fatalf("the budget after the pulls page holds %d requests, want the page's own 13931", got)
	}
	if _, err := reader.Patch(ctx, repo, survival.Commit{SHA: firstParentSHA, Parents: []string{grandParentSHA}}); err != nil {
		t.Fatalf("the patch read: %v", err)
	}
	if got := reader.Budget().Remaining; got != 13929 {
		t.Fatalf("the budget after the diff holds %d requests, want the diff answer's own 13929", got)
	}
	if _, err := reader.Patch(ctx, repo, survival.Commit{SHA: sideBaseSHA, Parents: []string{sideParentSHA}}); err != nil {
		t.Fatalf("the patch read: %v", err)
	}
	if got := reader.Budget().Remaining; got != 13929 {
		t.Fatalf("the budget after an answer of the same window naming more left holds %d requests, want the 13929 a later answer named", got)
	}
	if _, err := reader.Patch(ctx, repo, survival.Commit{SHA: sideMidSHA, Parents: []string{sideBaseSHA}}); err != nil {
		t.Fatalf("the patch read: %v", err)
	}
	if got, want := reader.Budget(), (Budget{Limit: 15000, Remaining: 14999, Reset: time.Unix(1795697913, 0).UTC()}); got != want {
		t.Fatalf("the budget after an answer of the next window is %+v, want that window's %+v", got, want)
	}
	counts := reader.Counts()
	if want := int64(len(commits.body) + len(pulls.body) + len(diff.body)); counts.Bytes != want {
		t.Fatalf("the reads counted %d bytes, want the %d GitHub answered", counts.Bytes, want)
	}
	if counts.Chain != 1 || counts.Pulls != 1 || counts.Diff != 3 || counts.Requests() != 5 {
		t.Fatalf("the reads counted %+v, want a chain page, a pulls page and three diffs", counts)
	}
}

// A read that fails in a way that may pass -- a 5xx naming no diff, a transport error, a timeout --
// is read again up to four more times, 1, 2, 4 and 8 s apart, and one that still fails fails the
// run. A rate limit is the budget's: it comes back at once, read no second time.
func TestATransientErrorIsReadAgainAndThenFailsTheRun(t *testing.T) {
	answer := capture(t, "diff-answer.txt")
	commit := survival.Commit{SHA: firstParentSHA, Parents: []string{grandParentSHA}}
	diffRoute := "GET /repos/cybermonklabs/legion/commits/" + firstParentSHA

	t.Run("a 502, then the diff", func(t *testing.T) {
		fake := newFakeGitHub(t)
		var tries atomic.Int32
		fake.handle(diffRoute, func(w http.ResponseWriter, _ *http.Request) {
			if tries.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprint(w, `{"message":"Server Error"}`)
				return
			}
			answer.serve(w, "")
		})
		reader, waits := newReader(t, fake, nil)
		read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), commit)
		if err != nil || read.Route != RouteDiff || len(read.Patch.Files) != 3 {
			t.Fatalf("a patch GitHub answered 502 and then whole: %d files through %s, %v; want the diff's 3", len(read.Patch.Files), read.Route, err)
		}
		if got := waits.made(); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
			t.Fatalf("the read waited %v, want one second before its second try", got)
		}
		if got := reader.Counts().Diff; got != 2 {
			t.Fatalf("the read counted %d diff requests, want both tries", got)
		}
	})

	t.Run("a connection cut mid-answer, then the diff", func(t *testing.T) {
		fake := newFakeGitHub(t)
		var tries atomic.Int32
		fake.handle(diffRoute, func(w http.ResponseWriter, _ *http.Request) {
			if tries.Add(1) == 1 {
				// The answer's head and part of its body, and then the connection closes: Go's
				// transport asks again on its own only for a request no answer had begun.
				connection, buffered, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("hijack the connection: %v", err)
					return
				}
				fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.github.diff\r\nContent-Length: %d\r\n\r\n", len(answer.body))
				_, _ = buffered.Write(answer.body[:100])
				_ = buffered.Flush()
				_ = connection.Close()
				return
			}
			answer.serve(w, "")
		})
		reader, waits := newReader(t, fake, nil)
		read, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), commit)
		if err != nil || len(read.Patch.Files) != 3 {
			t.Fatalf("a patch whose first try lost its connection: %d files, %v; want the diff's 3", len(read.Patch.Files), err)
		}
		if got := waits.made(); !reflect.DeepEqual(got, []time.Duration{time.Second}) {
			t.Fatalf("the read waited %v, want one second before its second try", got)
		}
		if got := fake.count(diffRoute); got != 2 {
			t.Fatalf("GitHub was asked %d times, want both tries", got)
		}
	})

	t.Run("five timeouts", func(t *testing.T) {
		saved := githubapp.DiffTimeout
		githubapp.DiffTimeout = 50 * time.Millisecond
		t.Cleanup(func() { githubapp.DiffTimeout = saved })
		fake := newFakeGitHub(t)
		fake.handle(diffRoute, func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.RawQuery != "" {
				t.Errorf("a patch that timed out read the files[] route: %s", r.URL.RawQuery)
				return
			}
			<-r.Context().Done()
		})
		reader, waits := newReader(t, fake, nil)
		_, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), commit)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a patch GitHub never answered: %v, want the read's own deadline", err)
		}
		if got := len(fake.asked(diffRoute)); got != 5 {
			t.Fatalf("GitHub was asked %d times, want five tries", got)
		}
		if got, want := waits.made(), []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}; !reflect.DeepEqual(got, want) {
			t.Fatalf("the read waited %v between its tries, want %v", got, want)
		}
	})

	for _, limit := range []struct {
		name   string
		status int
		header map[string]string
	}{
		{name: "a 403 naming the rate limit", status: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "15000", "X-RateLimit-Reset": "1795694313"}},
		{name: "a 429", status: http.StatusTooManyRequests, header: map[string]string{"Retry-After": "60"}},
	} {
		t.Run(limit.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			fake.handle(diffRoute, func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range limit.header {
					w.Header().Set(key, value)
				}
				w.WriteHeader(limit.status)
				fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 1."}`)
			})
			reader, waits := newReader(t, fake, nil)
			_, err := reader.Patch(testwait.Context(t), repository(legion, "main", mergeSHA), commit)
			if _, limited := githubapp.AsRateLimit(err); !limited {
				t.Fatalf("a patch GitHub limited: %v, want its *githubapp.RateLimitError", err)
			}
			if got := len(fake.asked(diffRoute)); got != 1 {
				t.Fatalf("GitHub was asked %d times, want once: a limit is the budget's, not a failure to read again", got)
			}
			if got := waits.made(); len(got) != 0 {
				t.Fatalf("the read waited %v after a rate limit, want no wait", got)
			}
		})
	}
}

// Open reads the repository's default branch and its tip through the App, and a repository the
// App cannot read -- not installed, no Contents read, a repository GitHub answers 404 for -- is
// reported unreadable with the reason, where a rate limit is not.
func TestARepositoryTheAppCannotReadIsUnreadableWithItsReason(t *testing.T) {
	tip := strings.Repeat("ab", 20)
	cases := []struct {
		name      string
		prepare   func(fake *fakeGitHub)
		repo      int
		reason    error
		rateLimit bool
	}{
		{name: "a readable repository", prepare: func(*fakeGitHub) {}},
		{name: "a repository the App is not installed on", prepare: func(f *fakeGitHub) { f.install(http.StatusNotFound, "read") }, reason: githubapp.ErrNoInstallation},
		{name: "an installation with no Contents read", prepare: func(f *fakeGitHub) { f.install(0, "none") }, reason: githubapp.ErrNoContentsRead},
		{name: "a repository GitHub answers 404 for", prepare: func(*fakeGitHub) {}, repo: http.StatusNotFound, reason: githubapp.ErrRepositoryUnreadable},
		{name: "a rate limit", prepare: func(*fakeGitHub) {}, repo: http.StatusForbidden, rateLimit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGitHub(t)
			tc.prepare(fake)
			fake.handle("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
				switch tc.repo {
				case 0:
					writeJSON(t, w, map[string]any{"full_name": widgets, "default_branch": "trunk"})
				case http.StatusForbidden:
					w.Header().Set("X-RateLimit-Remaining", "0")
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
				default:
					w.WriteHeader(tc.repo)
					fmt.Fprint(w, `{"message":"Not Found"}`)
				}
			})
			fake.handle("GET /repos/acme/widgets/git/ref/heads/trunk", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, map[string]any{"ref": "refs/heads/trunk", "object": map[string]any{"sha": tip, "type": "commit"}})
			})
			reader, _ := newReader(t, fake, nil)

			repo, err := reader.Open(testwait.Context(t), widgets)
			reason, unreadable := Unreadable(err)
			switch {
			case tc.rateLimit:
				if _, limited := githubapp.AsRateLimit(err); !limited || unreadable {
					t.Fatalf("Open under a rate limit: %v (unreadable %v), want the limit and no unreadable repository", err, unreadable)
				}
			case tc.reason != nil:
				if !unreadable || !errors.Is(err, tc.reason) || reason == "" || !strings.Contains(reason, tc.reason.Error()) {
					t.Fatalf("Open: %v, reason %q (unreadable %v); want the repository unreadable for %q", err, reason, unreadable, tc.reason)
				}
			default:
				if err != nil {
					t.Fatalf("Open of a readable repository: %v", err)
				}
				if want := (Repository{Repo: widgets, Owner: "acme", Name: "widgets", Branch: "trunk", Tip: tip}); repo != want {
					t.Fatalf("Open answered %+v, want %+v", repo, want)
				}
			}
		})
	}
}

// seedProject writes project key with defaultRepo as its default repository, none where it is "".
func seedProject(t *testing.T, ctx context.Context, pool *store.Pool, key, defaultRepo string) {
	t.Helper()
	var repo any
	if defaultRepo != "" {
		repo = defaultRepo
	}
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, default_repo) values ($1, $1, 'alice', $2)
		on conflict (key) do update set default_repo = excluded.default_repo
	`, key, repo); err != nil {
		t.Fatalf("seed the project %s: %v", key, err)
	}
}

// seedIssue writes the issue <project>-<number>, with repo as its own repository and parent as its
// parent, each none where it is "", and answers its key.
func seedIssue(t *testing.T, ctx context.Context, pool *store.Pool, project string, number int, repo, parent string) string {
	t.Helper()
	key := fmt.Sprintf("%s-%d", project, number)
	var ownRepo, parentKey any
	if repo != "" {
		ownRepo = repo
	}
	if parent != "" {
		parentKey = parent
	}
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank, repo, parent_key)
		values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1, $4, $5)
	`, key, project, number, ownRepo, parentKey); err != nil {
		t.Fatalf("seed the issue %s: %v", key, err)
	}
	return key
}

// storeHead writes pull request number of repo as pull_request_heads holds a merged one an
// attribution read of GitHub judged under the current rule, credited to issue (none for "").
func storeHead(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int, issue string) {
	t.Helper()
	checked := time.Now()
	head := delivery.PullRequestHead{
		Repo: repo, Number: number, HeadSHA: fmt.Sprintf("%040x", 0xbeef0000+number), HeadRef: "stored/" + strconv.Itoa(number),
		BaseRef: "main", State: "merged", AttributionSource: "none", AttributionComplete: true,
		URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, number), EventAt: &checked, CheckedAt: &checked,
	}
	if issue != "" {
		head.IssueKey, head.AttributionSource = &issue, "branch"
	}
	if err := delivery.UpsertPullRequestHead(ctx, pool, nil, head); err != nil {
		t.Fatalf("store the head of %s#%d: %v", repo, number, err)
	}
}

// merged is a merged pull request of widgets, numbered number, landed as the commit made of it.
func merged(number int, title, headRef string) (string, PullRequest) {
	return fmt.Sprintf("%040x", 0xc0de0000+number), PullRequest{
		Number: number, URL: fmt.Sprintf("https://github.com/%s/pull/%d", widgets, number), Title: title,
		HeadRef: headRef, HeadRepo: widgets, Author: "legion-implementer[bot]", MergedAt: firstDay,
	}
}

// chainOf is a chain of one commit for each merged pull request of pulls, in number order, after
// a commit that merged none.
func chainOf(pulls map[string]PullRequest) []survival.Commit {
	chain := []survival.Commit{{SHA: strings.Repeat("0", 40)}}
	numbers := make([]int, 0, len(pulls))
	shas := map[int]string{}
	for sha, pull := range pulls {
		numbers = append(numbers, pull.Number)
		shas[pull.Number] = sha
	}
	for len(numbers) > 0 {
		least := 0
		for i, number := range numbers {
			if number < numbers[least] {
				least = i
			}
		}
		chain = append(chain, survival.Commit{SHA: shas[numbers[least]]})
		numbers = append(numbers[:least], numbers[least+1:]...)
	}
	return chain
}

// issuesOf is the issue each change of chain was given, by its pull request's number.
func issuesOf(t *testing.T, chain []survival.Commit, pulls map[string]PullRequest) map[int]string {
	t.Helper()
	issues := map[int]string{}
	for _, commit := range chain {
		pull, ok := pulls[commit.SHA]
		if !ok {
			if commit.Change != (survival.ChangeFacts{}) {
				t.Fatalf("the commit %s merged no pull request and has the change %+v, want none", commit.SHA, commit.Change)
			}
			continue
		}
		if commit.Change.PR != pull.Number || commit.Change.HeadRef != pull.HeadRef || commit.Change.Author != pull.Author {
			t.Fatalf("the commit of #%d has the change %+v, want #%d's number, head branch and author", pull.Number, commit.Change, pull.Number)
		}
		issues[pull.Number] = commit.Change.IssueKey
	}
	return issues
}

// commitMessages answers the commits of a pull request, page by page: messages[i] is the message
// of commit i, and a page holds 100.
func commitMessages(t *testing.T, w http.ResponseWriter, r *http.Request, messages []string) {
	t.Helper()
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 || r.URL.Query().Get("per_page") != "100" {
		t.Errorf("a pull request's commits were asked for %q, want per_page=100 and a page", r.URL.RawQuery)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	from, to := min((page-1)*100, len(messages)), min(page*100, len(messages))
	commits := make([]map[string]any, 0, to-from)
	for _, message := range messages[from:to] {
		commits = append(commits, map[string]any{"commit": map[string]any{"message": message}})
	}
	writeJSON(t, w, commits)
}

// A change's issue is the one Dispatch's attribution gives its pull request. Where
// pull_request_heads holds the pull request judged under the current rule, its stored issue is
// taken, credited or not, and no request is made. Every other pull request runs attribution's own
// path: the sources its facts carry first, and its commit messages read only when those name no
// issue of this repository; an issue of another repository is dropped, from any source. A row an
// older rule judged is judged again.
func TestAChangesIssueIsAttributionsAnswer(t *testing.T) {
	database := storetest.Open(t)
	pool, ctx := database.Pool, testwait.Context(t)
	seedProject(t, ctx, pool, "ACME", widgets)
	seedProject(t, ctx, pool, "OTHR", "acme/elsewhere")
	stored := seedIssue(t, ctx, pool, "ACME", 1, "", "")
	titled := seedIssue(t, ctx, pool, "ACME", 2, "", "")
	inCommits := seedIssue(t, ctx, pool, "ACME", 3, "", "")
	elsewhere := seedIssue(t, ctx, pool, "OTHR", 1, "", "")
	storeHead(t, ctx, pool, widgets, 11, stored)
	storeHead(t, ctx, pool, widgets, 15, stored)
	if _, err := pool.Exec(ctx, `update pull_request_heads set attribution_rule = $1 where repo = $2 and number = 15`, delivery.AttributionRuleVersion-1, widgets); err != nil {
		t.Fatalf("make #15 a row an older rule judged: %v", err)
	}
	storeHead(t, ctx, pool, widgets, 16, "")

	pulls := map[string]PullRequest{}
	for _, pull := range []struct {
		number         int
		title, headRef string
	}{
		{11, titled + ": the title names another issue than the stored one", "feature/stored"},
		{12, "no key in the title", "feature/" + titled},
		{13, elsewhere + ": an issue of another repository", "feature/elsewhere"},
		{14, elsewhere + ": an issue of another repository, and none in the commits", "feature/none"},
		{15, titled + ": judged by an older rule", "feature/rejudged"},
		{16, titled + ": judged and credited to nobody", "feature/nobody"},
	} {
		sha, facts := merged(pull.number, pull.title, pull.headRef)
		pulls[sha] = facts
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("number") {
		case "13":
			commitMessages(t, w, r, []string{"chore: tidy", "fix(" + inCommits + "): the work"})
		case "14":
			commitMessages(t, w, r, []string{"chore: tidy", "refs " + elsewhere})
		default:
			t.Errorf("the commit messages of #%s were read, want them read only where sources 1 to 4 name no issue of this repository", r.PathValue("number"))
			w.WriteHeader(http.StatusNotFound)
		}
	})
	reader, _ := newReader(t, fake, pool)
	chain := chainOf(pulls)

	if err := reader.Changes(ctx, repository(widgets, "main", chain[len(chain)-1].SHA), chain, pulls); err != nil {
		t.Fatalf("the changes' issues: %v", err)
	}
	want := map[int]string{11: stored, 12: titled, 13: inCommits, 14: "", 15: titled, 16: ""}
	if got := issuesOf(t, chain, pulls); !reflect.DeepEqual(got, want) {
		t.Fatalf("the changes' issues are %v, want %v", got, want)
	}
	if got := fake.asked("GET /repos/acme/widgets/pulls/"); len(got) != 2 {
		t.Fatalf("GitHub was asked %v, want #13's and #14's commit messages alone", got)
	}
	if counts := reader.Counts(); counts.Attribution != 2 || counts.Requests() != 2 {
		t.Fatalf("the attribution counted %+v, want its two commit-message pages", counts)
	}
}

// A change's issue counts for the project of that issue, which the attribution keeps wherever the
// issue's repository -- its own, its parent's, or its project's default -- is the pull request's.
// An issue of another project whose own repository is this one is kept, and so is its child,
// whose repository is its parent's; an issue of the same project resolving to its project's
// default elsewhere is dropped.
func TestAnIssueOfAnotherProjectWhoseRepositoryIsThisOneCountsForThatProject(t *testing.T) {
	database := storetest.Open(t)
	pool, ctx := database.Pool, testwait.Context(t)
	seedProject(t, ctx, pool, "ACME", widgets)
	seedProject(t, ctx, pool, "OPS", "acme/ops")
	here := seedIssue(t, ctx, pool, "OPS", 7, widgets, "")
	child := seedIssue(t, ctx, pool, "OPS", 9, "", here)
	away := seedIssue(t, ctx, pool, "OPS", 8, "", "")

	pulls := map[string]PullRequest{}
	for number, title := range map[int]string{21: here + ": the work", 22: child + ": the child's work", 23: away + ": work elsewhere"} {
		sha, facts := merged(number, title, "feature/"+strconv.Itoa(number))
		pulls[sha] = facts
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("number") != "23" {
			t.Errorf("the commit messages of #%s were read, want only #23's, whose title names no issue of this repository", r.PathValue("number"))
		}
		commitMessages(t, w, r, []string{"chore: work"})
	})
	reader, _ := newReader(t, fake, pool)
	chain := chainOf(pulls)

	if err := reader.Changes(ctx, repository(widgets, "main", chain[len(chain)-1].SHA), chain, pulls); err != nil {
		t.Fatalf("the changes' issues: %v", err)
	}
	if got, want := issuesOf(t, chain, pulls), map[int]string{21: here, 22: child, 23: ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the changes' issues are %v, want %v", got, want)
	}
	for _, key := range []string{here, child} {
		var project string
		if err := pool.QueryRow(ctx, `select project_key from issues where key = $1`, key).Scan(&project); err != nil || project != "OPS" {
			t.Fatalf("the project of %s is %q (%v), want OPS, the project the change counts for", key, project, err)
		}
	}
}

// A merged pull request no attribution has judged reads its commit messages a page of 100 at a
// time, and every page is counted in the requests the run made: one of 120 commits whose only
// issue of this repository is on its second page is attributed to it after two pages, and one of
// more than 500 commits stops at maxCommitPages, five.
func TestAnUncachedPullRequestOverOneHundredCommitsReadsItsSecondPage(t *testing.T) {
	database := storetest.Open(t)
	pool, ctx := database.Pool, testwait.Context(t)
	seedProject(t, ctx, pool, "ACME", widgets)
	issue := seedIssue(t, ctx, pool, "ACME", 9, "", "")

	long := make([]string, 120)
	for i := range long {
		long[i] = "chore: step " + strconv.Itoa(i)
	}
	long[110] = "fix(" + issue + "): the step that names the issue"
	longer := make([]string, 600)
	for i := range longer {
		longer[i] = "chore: step " + strconv.Itoa(i)
	}
	longer[550] = "fix(" + issue + "): past the fifth page"
	pulls := map[string]PullRequest{}
	for _, number := range []int{31, 32} {
		sha, facts := merged(number, "no key in the title", "feature/"+strconv.Itoa(number))
		pulls[sha] = facts
	}
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("number") {
		case "31":
			commitMessages(t, w, r, long)
		default:
			if page, _ := strconv.Atoi(r.URL.Query().Get("page")); page > 5 {
				t.Errorf("#32's commits were read to page %d, want them to stop at the fifth", page)
			}
			commitMessages(t, w, r, longer)
		}
	})
	reader, _ := newReader(t, fake, pool)
	chain := chainOf(pulls)

	if err := reader.Changes(ctx, repository(widgets, "main", chain[len(chain)-1].SHA), chain, pulls); err != nil {
		t.Fatalf("the changes' issues: %v", err)
	}
	if got, want := issuesOf(t, chain, pulls), map[int]string{31: issue, 32: ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the changes' issues are %v, want %v", got, want)
	}
	if got := len(fake.asked("GET /repos/acme/widgets/pulls/31/commits")); got != 2 {
		t.Fatalf("#31's commits were read in %d pages, want 2", got)
	}
	if got := len(fake.asked("GET /repos/acme/widgets/pulls/32/commits")); got != 5 {
		t.Fatalf("#32's commits were read in %d pages, want maxCommitPages, 5", got)
	}
	if counts := reader.Counts(); counts.Attribution != 7 || counts.Requests() != 7 {
		t.Fatalf("the attribution counted %+v, want #31's two pages and #32's five in the request total", counts)
	}
}
