package replay

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// The span every collect of these tests reads: from 2026-11-20 to 09:00 the next day, its inputs
// reaching four days before, so every captured line from 2026-11-16 on (the oldest of them from
// 2026-11-16T18:01Z) is in it; the samples of the two older gate registration shapes, from
// 2026-10-27 and 2026-11-10, are before it.
var (
	collectFrom   = time.Date(2026, time.November, 20, 0, 0, 0, 0, time.UTC)
	collectTo     = time.Date(2026, time.November, 21, 9, 0, 0, 0, time.UTC)
	collectBefore = 96 * time.Hour
)

// The tokens the stubs take; a request with another is refused, so the test reads that the
// collect sent its token, and only as the Authorization header.
const (
	stubDispatchToken = "dispatch-test-token"
	stubGitHubToken   = "github-test-token"
)

// stubRequest is one request a stub served: its method and its path with its query.
type stubRequest struct {
	Method, Target string
}

// stubRequests is every request a stub served, in the order served.
type stubRequests struct {
	mu   sync.Mutex
	seen []stubRequest
}

func (s *stubRequests) add(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, stubRequest{Method: r.Method, Target: r.URL.RequestURI()})
}

func (s *stubRequests) all() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// dispatchServed is what dispatchStub serves: each issue's event log, oldest first; the whole
// event log, by id; each project's issues, in Dispatch's order; and the reads it refuses once.
type dispatchServed struct {
	events map[string][]json.RawMessage
	log    []json.RawMessage
	issues map[string][]string
	// refused is the status the first read of each issue is answered with, a refusal a later
	// attempt clears; under stubLogRefusal, of the event log's first page.
	refused map[string]int
}

// stubLogRefusal is the key of dispatchServed's refused that refuses the event log's first page.
const stubLogRefusal = "events/log"

// dispatchStub serves Dispatch's issue event log (GET /api/v1/issues/<KEY>/events?after=&limit=)
// from served.events, and 404 for an issue it holds none of; the whole event log (GET
// /api/v1/events/log?after=&limit=, the events with an id above after, at most limit of them);
// and a project's issues of every status a page at a time (GET
// /api/v1/issues?project=&limit=&offset=, as {issues, total, limit, offset}), refusing a listing
// with no limit, since only a page names its total. Any other route is 404.
func dispatchStub(t *testing.T, served dispatchServed) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	var mu sync.Mutex
	refuse := func(w http.ResponseWriter, key string) bool {
		mu.Lock()
		status, refused := served.refused[key]
		delete(served.refused, key)
		mu.Unlock()
		if refused {
			http.Error(w, `{"error":"try again"}`, status)
		}
		return refused
	}
	serve := func(w http.ResponseWriter, what string, body any) {
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(body); err != nil {
			t.Errorf("serve %s: %v", what, err)
		}
	}
	number := func(event json.RawMessage, member string) int64 {
		var head map[string]json.RawMessage
		var n int64
		if err := json.Unmarshal(event, &head); err != nil || json.Unmarshal(head[member], &n) != nil {
			t.Errorf("a stub event %s has no %s", event, member)
		}
		return n
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		if r.Header.Get("Authorization") != "Bearer "+stubDispatchToken {
			http.Error(w, `{"code":"UNAUTHORIZED","error":"no token"}`, http.StatusUnauthorized)
			return
		}
		query := r.URL.Query()
		after, _ := strconv.ParseInt(query.Get("after"), 10, 64)
		limit, limited := strconv.Atoi(query.Get("limit"))
		key, isEvents := strings.CutPrefix(r.URL.Path, "/api/v1/issues/")
		key, isEvents = strings.CutSuffix(key, "/events")
		switch {
		case r.Method != http.MethodGet:
		case r.URL.Path == "/api/v1/events/log":
			if refuse(w, stubLogRefusal) {
				return
			}
			page := []json.RawMessage{}
			for _, event := range served.log {
				if number(event, "id") > after && len(page) < limit {
					page = append(page, event)
				}
			}
			serve(w, "the event log", page)
			return
		case r.URL.Path == "/api/v1/issues":
			offset, _ := strconv.Atoi(query.Get("offset"))
			if limited != nil || limit < 1 || limit > 250 || offset < 0 || query.Has("status") {
				http.Error(w, `{"code":"INVALID_QUERY","error":"name a limit of 1 to 250, an offset and no status"}`, http.StatusBadRequest)
				return
			}
			all := served.issues[query.Get("project")]
			page := []map[string]string{}
			for _, key := range all[min(offset, len(all)):min(offset+limit, len(all))] {
				page = append(page, map[string]string{"key": key})
			}
			serve(w, "the issues", map[string]any{"issues": page, "total": len(all), "limit": limit, "offset": offset})
			return
		case isEvents:
			if refuse(w, key) {
				return
			}
			log, known := served.events[key]
			if !known {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"code":"ISSUE_NOT_FOUND","error":"issue not found"}`)
				return
			}
			page := []json.RawMessage{}
			for _, event := range log {
				if number(event, "seq") > after && len(page) < limit {
					page = append(page, event)
				}
			}
			serve(w, "the events of "+key, page)
			return
		}
		http.Error(w, `{"code":"NOT_FOUND","error":"no such route"}`, http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// githubStub serves GitHub's commit listing of acme/widgets's main from pages, each page after the
// first reached through the Link header of the one before, as GitHub pages a listing. It refuses
// a request for another listing or without the query collect-merges sends.
func githubStub(t *testing.T, pages [][]byte) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		query := r.URL.Query()
		switch {
		case r.Header.Get("Authorization") != "Bearer "+stubGitHubToken:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		case r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widgets/commits", query.Get("sha") != capturedBranch,
			query.Get("since") != "2026-11-19T04:56:30Z", query.Has("until"), query.Get("per_page") != "100":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		page := 1
		if text := query.Get("page"); text != "" {
			page, _ = strconv.Atoi(text)
		}
		if page < len(pages) {
			next := *r.URL
			query.Set("page", strconv.Itoa(page+1))
			next.RawQuery = query.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next", <%s%s>; rel="last"`, server.URL, next.RequestURI(), server.URL, next.RequestURI()))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pages[page-1])
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// timelineStub serves GitHub's timeline of each pull request of cybermonklabs/legion that
// timelines holds (GET /repos/cybermonklabs/legion/issues/<n>/timeline?per_page=100), its pages
// after the first reached through the Link header of the one before. It refuses a request for
// another route, repository or pull request, or without the query collect-queue sends.
func timelineStub(t *testing.T, timelines map[int][][]byte) (*httptest.Server, *stubRequests) {
	t.Helper()
	requests := &stubRequests{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
		query := r.URL.Query()
		number, isTimeline := strings.CutPrefix(r.URL.Path, "/repos/cybermonklabs/legion/issues/")
		number, isTimeline = strings.CutSuffix(number, "/timeline")
		n, err := strconv.Atoi(number)
		pages, known := timelines[n]
		switch {
		case r.Header.Get("Authorization") != "Bearer "+stubGitHubToken:
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		case r.Method != http.MethodGet || !isTimeline || err != nil || !known || query.Get("per_page") != "100":
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		page := 1
		if text := query.Get("page"); text != "" {
			page, _ = strconv.Atoi(text)
		}
		if page < len(pages) {
			next := *r.URL
			query.Set("page", strconv.Itoa(page+1))
			next.RawQuery = query.Encode()
			w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, server.URL, next.RequestURI()))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(pages[page-1])
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// capturedEvents is every captured Dispatch event by its issue, each issue's oldest first.
func capturedEvents(t *testing.T) map[string][]json.RawMessage {
	t.Helper()
	events := make(map[string][]json.RawMessage)
	for _, name := range capturedFiles(t) {
		if !strings.HasPrefix(name, "events/") {
			continue
		}
		event := capturedOne[json.RawMessage](t, name)
		var head struct {
			Key string `json:"issue_key"`
		}
		if err := json.Unmarshal(event, &head); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		events[head.Key] = append(events[head.Key], event)
	}
	for _, log := range events {
		slices.SortFunc(log, func(a, b json.RawMessage) int { return cmp.Compare(seqOf(t, a), seqOf(t, b)) })
	}
	return events
}

func seqOf(t *testing.T, event json.RawMessage) int64 {
	t.Helper()
	var head struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(event, &head); err != nil {
		t.Fatal(err)
	}
	return head.Seq
}

// capturedEntries is every captured log entry, by its file.
func capturedEntries(t *testing.T) map[string]capturedEntry {
	t.Helper()
	entries := make(map[string]capturedEntry)
	for _, name := range capturedFiles(t) {
		if strings.HasPrefix(name, "log/") {
			entries[name] = capturedOne[capturedEntry](t, name)
		}
	}
	return entries
}

// daemonOrder is line, a daemon line as Cloud Logging keeps it (its attributes sorted), as the
// daemon's JSON handler wrote it on stderr: time, level and msg first, then the rest.
func daemonOrder(t *testing.T, line []byte) []byte {
	t.Helper()
	value, err := decodeOrdered(line)
	if err != nil {
		t.Fatal(err)
	}
	object := value.(jsonObject)
	head := map[string]int{"time": 0, "level": 1, "msg": 2}
	slices.SortStableFunc(object, func(a, b jsonMember) int {
		rank := func(m jsonMember) int {
			if r, ok := head[m.key]; ok {
				return r
			}
			return len(head)
		}
		return rank(a) - rank(b)
	})
	var out bytes.Buffer
	if err := encodeOrdered(&out, object); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// compactJSON is data compact, as a window's file holds each line.
func compactJSON(t *testing.T, data []byte) json.RawMessage {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// The lines a collect finds only in the kubectl capture, beside the captured ones: a slot taken
// two days before the captured cap, under another cap, an issue that waits on another, and a pull
// request of ACME-29 that left the merge queue; the first two name an issue no captured line
// names. The first is written with --prefix as well as --timestamps.
const (
	collectSlotTaken = `{"time":"2026-11-19T00:00:00.5Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-58","slots":150,"cap":150}`
	collectWaitsOn   = `{"time":"2026-11-19T01:00:00Z","level":"INFO","msg":"admission waits for what this issue waits on","issue":"ACME-27","blockers":["ACME-42"]}`
	collectQueueLeft = `{"time":"2026-11-19T04:00:00Z","level":"INFO","msg":"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",` +
		`"issue":"ACME-29","pullRequest":4910,"head":"0123456789abcdef0123456789abcdef01234567"}`
)

// collectAttributions are the issue.pull_request_attribution_changed events of ACME-29 that
// Dispatch answers a collect with, beside the captured events: one attributes the pull request
// cybermonklabs/legion#5150 to it, and one acme/other#3, a pull request of another repository.
var collectAttributions = []string{
	`{"id":900003,"issue_key":"ACME-29","artifact_id":null,"project":"ACME","seq":71,"type":"issue.pull_request_attribution_changed",` +
		`"actor":{"kind":"system","id":"delivery-attribution"},"notify":false,"created_at":"2026-11-20T11:00:00Z",` +
		`"payload":{"to":"ACME-29","url":"https://github.com/cybermonklabs/legion/pull/5150","from":null,"repo":"cybermonklabs/legion","number":5150,"source":"branch"}}`,
	`{"id":900004,"issue_key":"ACME-29","artifact_id":null,"project":"ACME","seq":72,"type":"issue.pull_request_attribution_changed",` +
		`"actor":{"kind":"system","id":"delivery-attribution"},"notify":false,"created_at":"2026-11-20T12:00:00Z",` +
		`"payload":{"to":"ACME-29","url":"https://github.com/acme/other/pull/3","from":null,"repo":"acme/other","number":3,"source":"external_link"}}`,
}

// collectLog is Dispatch's whole event log as a collect's stub serves it, by id: ACME's default
// assignee set before the reach, another project's settings event, ACME-77's creation (an issue of
// ACME's that no line names and the listing of ACME's issues lacks), ACME's architecture source
// set inside the reach, and two settings events of ACME at the reach's end and after it.
var collectLog = []string{
	`{"id":1,"issue_key":null,"artifact_id":null,"project":"ACME","seq":1,"type":"settings.project_default_assignee.updated",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-10T00:00:00Z","payload":{"project":"ACME","assignee":"person-1@example.invalid"}}`,
	`{"id":2,"issue_key":null,"artifact_id":null,"project":"OTHERA","seq":1,"type":"settings.repo_project.updated",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-17T00:00:00Z","payload":{"deleted":false,"mapping":{"repo":"acme/other","project":"OTHERA"}}}`,
	`{"id":3,"issue_key":"ACME-77","artifact_id":null,"project":"ACME","seq":1,"type":"issue.created",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-17T00:00:00Z","payload":{"key":"ACME-77","status":"todo","changed":["status"]}}`,
	`{"id":4,"issue_key":null,"artifact_id":null,"project":"ACME","seq":2,"type":"settings.architecture_source.updated",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-18T00:00:00Z","payload":{"source":{"repo":"cybermonklabs/legion","branch":"main"},"deleted":false}}`,
	`{"id":5,"issue_key":null,"artifact_id":null,"project":"ACME","seq":3,"type":"settings.project_default_assignee.updated",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-21T09:00:00Z","payload":{"project":"ACME","assignee":"person-2@example.invalid"}}`,
	`{"id":6,"issue_key":null,"artifact_id":null,"project":"ACME","seq":4,"type":"settings.project_default_assignee.updated",` +
		`"actor":{"kind":"user","id":"person-1@example.invalid"},"notify":false,"created_at":"2026-11-22T00:00:00Z","payload":{"project":"ACME","assignee":"person-1@example.invalid"}}`,
}

// collectIssues is the listing of ACME's issues the stub serves: issues the lines name, and ACME-55,
// which no line and no event of an issue a line names names.
var collectIssues = []string{"ACME-27", "ACME-29", "ACME-32", "ACME-55", "ACME-58"}

// The deployment's records of the signals a collect is given beside the daemon's lines. The
// provider's record is a minute with no limit in force, then the captured minutes, after a minute
// before the reach; its captured minutes of 2026-11-23 are after the reach. The launcher's lines
// are t25's export rows of an hour, gzipped under a directory of their own, with a line before the
// reach and one at its end, and a Cloud Logging read of two entries, one the export holds too. The
// pods' memory is the export's rows of an hour, two series of each pod, before the reach and in
// it, under a directory of their own. Each export directory holds, beside its hour objects, the
// export's manifest of their hashes (collectManifest), which is no record, so collect reads the
// hour objects alone.
const (
	collectManifest     = "0000000000000000000000000000000000000000000000000000000000000000  2026-11-20/00.jsonl\n"
	collectProviderHead = `{"minute": "2026-11-15T23:59:00Z", "uncached": 1, "limit": 6000000, "limit_source": "gateway-log", "limit_scope": "Claude", "own": 0, "provider": 0}` + "\n" +
		`{"minute": "2026-11-16T14:29:00Z", "uncached": 10, "limit": null, "limit_source": null, "limit_scope": null, "own": 0, "provider": 0}` + "\n"
	collectGuardEnded   = `launcher: memory guard: ended pid=41 role=tester size=1048576 reading=7000000000 counter=7100000000 shmem=0 limit=8589934592 executable="go"`
	collectGuardStarted = "launcher: memory guard: started act_percent=85 floor_percent=10 wait=5s interval=100ms limit=8589934592 reading=23052288"
	collectGuardStopped = "launcher: memory guard: stopped for this rise reason=nothing to end reading=7303979008 endable=0 protected=7429586944 unread=0 unheld=-125607936 limit=8589934592 left_out=0"
	collectMemory       = `{"minute":"2026-11-15T23:59:00Z","pod":"legion-acme-acme-9","memory_type":"non-evictable","bytes":5}
{"minute":"2026-11-20T00:00:00Z","pod":"legion-acme-acme-9","memory_type":"evictable","bytes":100}
{"minute":"2026-11-20T00:00:00Z","pod":"legion-acme-acme-9","memory_type":"non-evictable","bytes":900}
{"minute":"2026-11-20T00:00:00Z","pod":"legion-acme-acme-8","memory_type":"non-evictable","bytes":3}
{"minute":"2026-11-20T00:00:00Z","pod":"legion-acme-acme-8","memory_type":"evictable","bytes":7}
{"minute":"2026-11-20T00:01:00Z","pod":"legion-acme-acme-9","memory_type":"non-evictable","bytes":50}
`
)

// collectChildAdded is the child.added event of ACME-32 naming the child ACME-60 that Dispatch
// answers a collect of the captured samples with, beside the captured events.
const collectChildAdded = `{"id":900001,"issue_key":"ACME-32","artifact_id":null,"project":"ACME","seq":5,"type":"child.added",` +
	`"actor":{"kind":"session","id":"legion-daemon:ACME"},"notify":false,"created_at":"2026-11-20T10:00:00Z","payload":{"child_key":"ACME-60"}}`

// collectBelowPage is the commit GitHub lists below the captured commit page: the first parent of
// the page's oldest commit, committed 20 seconds before it, cut to what collect-merges reads, its
// message neutralized as the page's are. The page holds five commits alone, so a collect-merges
// that holds them as the span's whole history reads from the oldest one's time, and its walk ends
// at this commit, before the span.
const collectBelowPage = `{"sha":"e4584fae753a80a71074cd7d33aaaacb4c8660bf","commit":{"message":"fix(dispatch): captured commit message (#4559)",` +
	`"committer":{"date":"2026-11-20T04:56:10Z"}},"parents":[{"sha":"c980d91b540ece46364256983f24a3f5a63b3841"}]}`

// collectRun is one collect, collect-merges and collect-queue of the captured samples against the
// stubs, and what each stub saw.
type collectRun struct {
	out       string
	collected Collected
	merges    []Merge
	queued    QueueCollected
	dispatch  []stubRequest
	github    []stubRequest
	timelines []stubRequest
	waits     []time.Duration
	captured  map[string]capturedEntry
	events    map[string][]json.RawMessage
	settings  Settings
	parts     []Part
}

// collectCaptured writes the captured daemon lines as the three forms a person collects them in —
// a gzipped Cloud Logging export of JSON lines, a Cloud Logging export as one JSON array, and
// `kubectl logs --timestamps` output under a directory of its own — with the signals' records
// (collectProviderHead and the captured provider minutes, the launcher's lines, the pods' memory)
// and the default assignee, and collects the window from them against a Dispatch stub answering
// the captured events, the event log collectLog and the listing collectIssues, and a GitHub stub
// answering the captured commit page and the commit below it (collectBelowPage) in two pages, then
// collect-merges main of acme/widgets into it over [04:56:30, 06:00) of 2026-11-20, from the
// page's oldest commit's time, then collect-queue of cybermonklabs/legion against a GitHub stub
// answering the captured timelines of #4910, in two pages, and of #5150. The export holds every
// captured line, a row with no jsonPayload and a line of a kind the trace does not read; the
// array a line before the span and one at its end; the kubectl output every captured line again
// (its attributes in the daemon's own order), the three lines of its own, a line of other output
// and a line of a kind the trace does not read. Dispatch also answers a child.added event of
// ACME-32 naming the child ACME-60, the attributions of ACME-29 (collectAttributions), an event of
// ACME-29 after the span, and ACME-77's events; it refuses the first read of the event log and of
// ACME-29 with 503 and of ACME-58 with 429.
func collectCaptured(t *testing.T) collectRun {
	t.Helper()
	run := collectRun{captured: capturedEntries(t), events: capturedEvents(t)}
	logs := t.TempDir()

	var export bytes.Buffer
	zipped := gzip.NewWriter(&export)
	var kubectl bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(run.captured)) {
		entry := run.captured[name]
		fmt.Fprintf(zipped, "{\"timestamp\":%q,\"jsonPayload\":%s}\n", entry.Timestamp, compactJSON(t, entry.JSONPayload))
		fmt.Fprintf(&kubectl, "%s %s\n", entry.Timestamp, daemonOrder(t, entry.JSONPayload))
	}
	fmt.Fprintln(zipped, `{"timestamp":"2026-11-20T01:00:00Z","textPayload":"a line the daemon wrote that is no JSON"}`)
	fmt.Fprintln(zipped, `{"timestamp":"2026-11-20T01:00:01Z","jsonPayload":{"time":"2026-11-20T01:00:01Z","level":"INFO","msg":"daemon: the image probe's attempt outlives slow_command_timeout_seconds"}}`)
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&kubectl, "[pod/legion-acme-daemon-0/legion] 2026-11-19T00:00:00.6Z %s\n", collectSlotTaken)
	fmt.Fprintf(&kubectl, "2026-11-19T01:00:00.1Z %s\n", collectWaitsOn)
	fmt.Fprintf(&kubectl, "2026-11-19T04:00:00.1Z %s\n", collectQueueLeft)
	fmt.Fprintln(&kubectl, "2026-11-19T02:00:00Z goroutine dump follows: this is no JSON line")
	fmt.Fprintln(&kubectl, `2026-11-19T03:00:00Z {"time":"2026-11-19T03:00:00Z","level":"INFO","msg":"legion workflow boot stage done elsewhere"}`)
	array := `[
  {"timestamp":"2026-11-10T00:00:01Z","jsonPayload":{"time":"2026-11-10T00:00:00Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-70","slots":1,"cap":9}},
  {"timestamp":"2026-11-21T09:00:01Z","jsonPayload":{"time":"2026-11-21T09:00:00Z","level":"INFO","msg":"admission: slot taken","issue":"ACME-71","slots":1,"cap":9}}
]`
	var launcherExport bytes.Buffer
	zipped = gzip.NewWriter(&launcherExport)
	for _, row := range []struct{ at, pod, text string }{
		{"2026-11-15T23:00:00Z", "legion-acme-acme-9", collectGuardStarted},
		{"2026-11-20T01:00:00.5Z", "legion-acme-acme-9", collectGuardEnded},
		{"2026-11-20T02:00:00Z", "legion-acme-acme-8", collectGuardStarted},
		{"2026-11-21T09:00:00Z", "legion-acme-acme-8", collectGuardEnded},
	} {
		line, err := json.Marshal(map[string]string{"timestamp": row.at, "pod": row.pod, "text": row.text})
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(zipped, "%s\n", line)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := json.Marshal([]map[string]any{
		{"timestamp": "2026-11-20T03:00:00Z", "resource": map[string]any{"labels": map[string]string{"pod_name": "legion-acme-acme-7"}}, "textPayload": collectGuardStopped},
		{"timestamp": "2026-11-20T01:00:00.5Z", "resource": map[string]any{"labels": map[string]string{"pod_name": "legion-acme-acme-9"}}, "textPayload": collectGuardEnded},
	})
	if err != nil {
		t.Fatal(err)
	}
	inputs := t.TempDir()
	for name, data := range map[string][]byte{
		filepath.Join(logs, "a-export.jsonl.gz"):                         export.Bytes(),
		filepath.Join(logs, "b-export-array.json"):                       []byte(array),
		filepath.Join(logs, "c", "kubectl.log"):                          kubectl.Bytes(),
		filepath.Join(inputs, "provider.jsonl"):                          append([]byte(collectProviderHead), signalsFixture(t, "provider/minutes.jsonl")...),
		filepath.Join(inputs, "launcher", "2026-11-20", "01.jsonl.gz"):   launcherExport.Bytes(),
		filepath.Join(inputs, "launcher", "manifest-2026-11-20.sha256"):  []byte(collectManifest),
		filepath.Join(inputs, "launcher-read.out"):                       entries,
		filepath.Join(inputs, "memory", "2026-11-20", "00.jsonl"):        []byte(collectMemory),
		filepath.Join(inputs, "memory", "2026-11-20", "manifest.sha256"): []byte(collectManifest),
		filepath.Join(inputs, "memory", "manifest-2026-11-20.sha256"):    []byte(collectManifest),
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	served := maps.Clone(run.events)
	served["ACME-32"] = []json.RawMessage{json.RawMessage(collectChildAdded)}
	served["ACME-29"] = slices.Clone(served["ACME-29"])
	for _, event := range collectAttributions {
		served["ACME-29"] = append(served["ACME-29"], json.RawMessage(event))
	}
	served["ACME-29"] = append(served["ACME-29"], json.RawMessage(`{"id":900002,"issue_key":"ACME-29","artifact_id":null,"project":"ACME","seq":73,`+
		`"type":"message.created","actor":{"kind":"session","id":"legion-daemon:ACME"},"notify":false,"created_at":"2026-11-21T10:00:00Z","payload":{"body":"after the span"}}`))
	served["ACME-77"] = []json.RawMessage{json.RawMessage(collectLog[2])}
	var eventLog []json.RawMessage
	for _, event := range collectLog {
		eventLog = append(eventLog, json.RawMessage(event))
	}
	dispatchServer, dispatchSeen := dispatchStub(t, dispatchServed{events: served, log: eventLog, issues: map[string][]string{"ACME": collectIssues},
		refused: map[string]int{stubLogRefusal: http.StatusServiceUnavailable, "ACME-29": http.StatusServiceUnavailable, "ACME-58": http.StatusTooManyRequests}})

	var page []json.RawMessage
	if err := json.Unmarshal(capturedFile(t, "github/commits.json"), &page); err != nil {
		t.Fatal(err)
	}
	page = append(page, json.RawMessage(collectBelowPage))
	var pages [][]byte
	for _, part := range [][]json.RawMessage{page[:3], page[3:]} {
		data, err := json.Marshal(part)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, data)
	}
	githubServer, githubSeen := githubStub(t, pages)
	var queued []json.RawMessage
	if err := json.Unmarshal(signalsFixture(t, "github/timeline-4910.json"), &queued); err != nil {
		t.Fatal(err)
	}
	timelinePages := map[int][][]byte{5150: {signalsFixture(t, "github/timeline-5150.json")}}
	for _, part := range [][]json.RawMessage{queued[:60], queued[60:]} {
		data, err := json.Marshal(part)
		if err != nil {
			t.Fatal(err)
		}
		timelinePages[4910] = append(timelinePages[4910], data)
	}
	timelineServer, timelineSeen := timelineStub(t, timelinePages)

	settings, err := DaemonSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.TickInterval = 30 * time.Minute
	settings.GateSteps = []record.DesignStep{record.DesignStepSpec}
	run.settings = settings
	run.parts = []Part{{From: collectFrom, To: collectTo, Release: "legion-v19.41.3", Commit: record.MainRule.Commit, Rule: record.MainRule.Name}}
	var waitsMu sync.Mutex
	wait := func(_ context.Context, d time.Duration) error {
		waitsMu.Lock()
		defer waitsMu.Unlock()
		run.waits = append(run.waits, d)
		return nil
	}
	run.out = filepath.Join(t.TempDir(), "window")
	run.collected, err = Collect(context.Background(), CollectConfig{
		Project: "ACME", From: collectFrom, To: collectTo, Before: collectBefore, Logs: []string{logs},
		Provider: filepath.Join(inputs, "provider.jsonl"), LauncherLogs: []string{filepath.Join(inputs, "launcher"), filepath.Join(inputs, "launcher-read.out")},
		Memory: []string{filepath.Join(inputs, "memory")}, DefaultAssignee: "person-1@example.invalid",
		Dispatch: NewDispatchReader(dispatchServer.URL, stubDispatchToken), Concurrency: 3, Settings: settings, Parts: run.parts,
		Collector: "0123456789abcdef0123456789abcdef01234567", Out: run.out, Wait: wait,
	})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	run.merges, err = CollectMerges(context.Background(), MergesConfig{Repo: "acme/widgets", Branch: capturedBranch,
		From: time.Date(2026, time.November, 20, 4, 56, 30, 0, time.UTC), To: time.Date(2026, time.November, 20, 6, 0, 0, 0, time.UTC),
		Token: stubGitHubToken, API: githubServer.URL, Window: run.out, Wait: wait})
	if err != nil {
		t.Fatalf("CollectMerges: %v", err)
	}
	run.queued, err = CollectQueue(context.Background(), QueueConfig{Repo: "cybermonklabs/legion", Token: stubGitHubToken, API: timelineServer.URL,
		Window: run.out, Wait: wait})
	if err != nil {
		t.Fatalf("CollectQueue: %v", err)
	}
	run.dispatch, run.github, run.timelines = dispatchSeen.all(), githubSeen.all(), timelineSeen.all()
	return run
}

// A collect of the captured samples writes a window whose every file reads back through
// ReadWindow as written: the log holds each captured line written in the collect's reach once, as
// Cloud Logging kept it, with the kubectl capture's own three lines, in time order, and no line
// before the reach (two captured lines among them), at the span's end, of a kind the trace does
// not read, or that is no daemon line; window.json holds the project, the
// span, the reach, the cap of each change (the earlier line's 150, then the daemon's start line's
// 157), the kinds the log holds, the settings, the part, the collector and the default assignee;
// the events are each captured issue's that a line names, up to the span's end, the blocker's and
// parent's those events name, the child a child.added names, and each issue of the project that
// the listing of its issues or the event log names and no line does (ACME-55, ACME-77), every
// other key Dispatch did not have listed as missing; and the merges are the captured page's five
// first-parent commits, read over two pages with the commit below it, which the walk passes and
// does not keep.
// The signals' records are each one's lines in the reach: the provider's minutes, the launcher's
// lines of both its forms as the window's rows, each line once, in time order, the Cloud Logging
// read named on its own though its name is no JSON file's, and the pods' memory as each pod's two
// series summed a minute, each export's manifest beside its hour objects read as no record; the
// settings events are the project's own, read from the event log from its start to the reach's
// end, the one before the reach included; and the signals map holds a record of the idle share,
// the kills and the approvals over the reach (the default assignee is known from the reach's
// start), of the provider where a limit is in force (each run of minutes with one), and, once
// collect-queue ran, of the queue over the reach. collect-queue reads the timeline of each pull
// request of cybermonklabs/legion that an attribution event of the window's issues or a line of
// its log names, #4910 over two pages and #5150, and writes their merge queue events as GitHub
// serves them; it reads no pull request of another repository, and none a line names of an issue
// whose repository the window does not give, which it names.
// Every request the collect, the collect-merges and the collect-queue send is a GET: the stubs'
// record of what each served holds no other method, and holds requests (a collect that read
// nothing would pass a check of its methods alone).
func TestCollectWritesAWindowThatReadsBackAndSendsGetAlone(t *testing.T) {
	t.Parallel()
	run := collectCaptured(t)
	read, err := ReadWindow(run.out)
	if err != nil {
		t.Fatalf("ReadWindow of the collected window: %v", err)
	}

	type timed struct {
		at   time.Time
		line json.RawMessage
	}
	reach := collectFrom.Add(-collectBefore)
	var want []timed
	// add holds line among the lines the log keeps where it was written in the collect's reach,
	// [reach, collectTo), and says whether it was.
	add := func(line []byte) bool {
		var head struct {
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			t.Fatal(err)
		}
		if head.Time.Before(reach) || !head.Time.Before(collectTo) {
			return false
		}
		want = append(want, timed{at: head.Time, line: compactJSON(t, line)})
		return true
	}
	inReach := 0
	for _, entry := range run.captured {
		if add(entry.JSONPayload) {
			inReach++
		}
	}
	add([]byte(collectSlotTaken))
	add([]byte(collectWaitsOn))
	add([]byte(collectQueueLeft))
	slices.SortFunc(want, func(a, b timed) int { return a.at.Compare(b.at) })
	if len(read.Log) != len(want) {
		t.Errorf("the log holds %d lines, want %d", len(read.Log), len(want))
	}
	kinds := make(map[string]bool)
	for i, line := range want {
		if i < len(read.Log) && !bytes.Equal(read.Log[i], line.line) {
			t.Errorf("log line %d is\n  %s\nwant\n  %s", i+1, read.Log[i], line.line)
		}
		var head struct {
			Msg string `json:"msg"`
		}
		if err := json.Unmarshal(line.line, &head); err != nil {
			t.Fatal(err)
		}
		kinds[head.Msg] = true
	}
	if got, wantKinds := read.Lines, slices.Sorted(maps.Keys(kinds)); !slices.Equal(got, wantKinds) {
		t.Errorf("window.json's lines are %q, want %q", got, wantKinds)
	}
	if c := run.collected; c.Kept != len(want) || c.Duplicates != inReach || c.NotLines != 2 || c.Read != 2*len(run.captured)+7 {
		t.Errorf("the collect read %d lines, kept %d, left %d out as read twice and %d as no daemon line; want %d, %d, %d and 2",
			c.Read, c.Kept, c.Duplicates, c.NotLines, 2*len(run.captured)+7, len(want), inReach)
	}

	wantCaps := []CapAt{{At: time.Date(2026, time.November, 19, 0, 0, 0, 5e8, time.UTC), Cap: 150},
		{At: capturedTime(t, "2026-11-21T02:01:27.894781099Z"), Cap: 157}}
	if !sameReading(read.Caps, wantCaps) {
		t.Errorf("window.json's caps are %+v, want %+v", read.Caps, wantCaps)
	}
	s := run.settings
	if read.Project != "ACME" || !read.From.Equal(collectFrom) || !read.To.Equal(collectTo) || time.Duration(read.Before) != collectBefore ||
		time.Duration(read.TickInterval) != s.TickInterval || !slices.Equal(read.GateSteps, s.GateSteps) || time.Duration(read.Linger) != s.Linger ||
		read.ReviewRoundCap != s.ReviewRoundCap || time.Duration(read.StalledTreeNotice) != s.StalledTreeNotice ||
		time.Duration(read.WorkerSilence) != s.WorkerSilence || time.Duration(read.WorkerBackground) != s.WorkerBackground ||
		read.Collector != "0123456789abcdef0123456789abcdef01234567" || !sameReading(read.Parts, run.parts) || len(read.Experiments) != 0 ||
		read.DefaultAssignee != "person-1@example.invalid" || read.P0Bounds != nil {
		t.Errorf("window.json reads back as %+v, want the project, span, reach, settings, part, collector and default assignee the collect was given", read)
	}

	wantEvents := map[string][]json.RawMessage{
		"ACME-32": {compactJSON(t, []byte(collectChildAdded))},
		"ACME-77": {compactJSON(t, []byte(collectLog[2]))},
	}
	for _, key := range []string{"ACME-29", "ACME-27", "ACME-58", "ACME-55"} {
		for _, event := range run.events[key] {
			wantEvents[key] = append(wantEvents[key], compactJSON(t, event))
		}
	}
	for _, event := range collectAttributions {
		wantEvents["ACME-29"] = append(wantEvents["ACME-29"], compactJSON(t, []byte(event)))
	}
	if !reflect.DeepEqual(read.Events, wantEvents) {
		t.Errorf("the window's events are of %v, want of %v (each issue's captured events, ACME-29's with its attributions and without the one after the span)",
			slices.Sorted(maps.Keys(read.Events)), slices.Sorted(maps.Keys(wantEvents)))
		for key, events := range wantEvents {
			if !reflect.DeepEqual(read.Events[key], events) {
				t.Errorf("the events of %s are\n  %s\nwant\n  %s", key, read.Events[key], events)
			}
		}
	}
	missing := map[string]bool{"ACME-24": true, "ACME-15": true, "ACME-52": true, "ACME-53": true, "ACME-60": true, "ACME-42": true, "ACME-41": true}
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 16, 18, 22, 26, 28, 30, 31, 33, 34, 35, 36, 37, 38, 39, 40, 43, 44, 45, 46, 47, 48, 49, 50, 51, 56, 57} {
		missing[fmt.Sprintf("ACME-%d", n)] = true
	}
	wantMissing := slices.Sorted(maps.Keys(missing))
	if !slices.Equal(run.collected.Missing, wantMissing) {
		t.Errorf("the collect lists as missing %v, want %v", run.collected.Missing, wantMissing)
	}
	for _, request := range run.dispatch {
		for _, unread := range []string{"ACME-70", "ACME-71"} {
			if strings.Contains(request.Target, "/"+unread+"/") {
				t.Errorf("the collect read %s, which no kept line, no event of a named issue and no listing names", request.Target)
			}
		}
	}
	if !slices.Equal(run.waits, []time.Duration{time.Second, time.Second, time.Second}) {
		t.Errorf("the reads Dispatch refused for now waited %v, want one second each before their second attempt", run.waits)
	}

	reachEnd := collectTo
	stamped := func(text string) time.Time { return capturedTime(t, text) }
	var wantProvider []ProviderMinute
	for _, line := range bytes.Split(bytes.TrimSpace(append([]byte(collectProviderHead), signalsFixture(t, "provider/minutes.jsonl")...)), []byte("\n")) {
		minute, err := ParseProviderMinute(line)
		if err != nil {
			t.Fatal(err)
		}
		if !minute.Minute.Before(reach) && minute.Minute.Before(reachEnd) {
			wantProvider = append(wantProvider, minute)
		}
	}
	if len(wantProvider) != 8 || !sameReading(read.Provider, wantProvider) {
		t.Errorf("the provider's minutes are %s, want the 8 of the reach, %s", describeReading(read.Provider), describeReading(wantProvider))
	}
	wantLauncher := []LauncherLine{
		{Time: stamped("2026-11-20T01:00:00.5Z"), Pod: "legion-acme-acme-9", Text: collectGuardEnded},
		{Time: stamped("2026-11-20T02:00:00Z"), Pod: "legion-acme-acme-8", Text: collectGuardStarted},
		{Time: stamped("2026-11-20T03:00:00Z"), Pod: "legion-acme-acme-7", Text: collectGuardStopped},
	}
	if !sameReading(read.Launcher, wantLauncher) {
		t.Errorf("the launcher's lines are %s, want %s", describeReading(read.Launcher), describeReading(wantLauncher))
	}
	wantMemory := []MemorySample{
		{Minute: stamped("2026-11-20T00:00:00Z"), Pod: "legion-acme-acme-8", Bytes: 10},
		{Minute: stamped("2026-11-20T00:00:00Z"), Pod: "legion-acme-acme-9", Bytes: 1000},
		{Minute: stamped("2026-11-20T00:01:00Z"), Pod: "legion-acme-acme-9", Bytes: 50},
	}
	if !sameReading(read.Memory, wantMemory) {
		t.Errorf("the pods' memory is %s, want %s", describeReading(read.Memory), describeReading(wantMemory))
	}
	if wantSettings := []json.RawMessage{compactJSON(t, []byte(collectLog[0])), compactJSON(t, []byte(collectLog[3]))}; !reflect.DeepEqual(read.Settings, wantSettings) {
		t.Errorf("the settings events are\n  %s\nwant the project's own up to the reach's end\n  %s", read.Settings, wantSettings)
	}
	if c := run.collected; c.SettingsEvents != 2 || c.ProjectIssues != len(collectIssues) {
		t.Errorf("the collect counts %d settings events and %d issues of the project, want 2 and %d", c.SettingsEvents, c.ProjectIssues, len(collectIssues))
	}
	wholeReach := []Span{{From: reach, To: reachEnd}}
	wantSignals := map[string][]Span{
		SignalIdle: wholeReach, SignalKills: wholeReach, SignalApprovals: wholeReach, SignalQueue: wholeReach,
		SignalProvider: {{From: stamped("2026-11-16T14:30:00Z"), To: stamped("2026-11-16T14:33:00Z")},
			{From: stamped("2026-11-17T17:46:00Z"), To: stamped("2026-11-17T17:50:00Z")}},
	}
	if len(read.Signals) != len(wantSignals) {
		t.Errorf("the signals map names %v, want %v", slices.Sorted(maps.Keys(read.Signals)), slices.Sorted(maps.Keys(wantSignals)))
	}
	for signal, spans := range wantSignals {
		if !sameReading(read.Signals[signal], spans) {
			t.Errorf("the record of %s spans %v, want %v", signal, read.Signals[signal], spans)
		}
	}

	var wantQueue []json.RawMessage
	for _, pr := range []int{4910, 5150} {
		var items []json.RawMessage
		if err := json.Unmarshal(signalsFixture(t, fmt.Sprintf("github/timeline-%d.json", pr)), &items); err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			var head struct {
				Event string `json:"event"`
			}
			if err := json.Unmarshal(item, &head); err != nil {
				t.Fatal(err)
			}
			if head.Event == "added_to_merge_queue" || head.Event == "removed_from_merge_queue" || head.Event == "merged" {
				wantQueue = append(wantQueue, compactJSON(t, fmt.Appendf(nil, `{"pullRequest":%d,"item":%s}`, pr, item)))
			}
		}
	}
	if len(read.Queue) != 1 || !reflect.DeepEqual(read.Queue["cybermonklabs/legion"], wantQueue) {
		t.Errorf("the window's queue is\n  %s\nwant cybermonklabs/legion's\n  %s", read.Queue, wantQueue)
	}
	if q := run.queued; !slices.Equal(q.PullRequests, []int{4910, 5150}) || q.Lines != len(wantQueue) || !slices.Equal(q.NoRepo, []int{4547, 5367}) {
		t.Errorf("collect-queue read the pull requests %v, wrote %d lines and names %v as of an issue with no repository; want [4910 5150], %d and [4547 5367]",
			q.PullRequests, q.Lines, q.NoRepo, len(wantQueue))
	}
	if len(run.timelines) != 3 {
		t.Errorf("collect-queue read %d pages, want 3: %v", len(run.timelines), run.timelines)
	}

	if want := capturedMergeReadings(t)["commits.json"]; len(read.Merges) != 1 || !sameReading(read.Merges["acme/widgets"], want) || !sameReading(run.merges, want) {
		t.Errorf("the window's merges are %+v (collect-merges answered %+v), want acme/widgets's %+v", read.Merges, run.merges, want)
	}
	if len(run.github) != 2 {
		t.Errorf("collect-merges read %d pages, want the listing's 2: %v", len(run.github), run.github)
	}

	if len(run.dispatch) == 0 || len(run.github) == 0 || len(run.timelines) == 0 {
		t.Fatalf("the stubs served %d Dispatch, %d GitHub commit and %d GitHub timeline requests; the collect read nothing", len(run.dispatch),
			len(run.github), len(run.timelines))
	}
	for stub, requests := range map[string][]stubRequest{"Dispatch": run.dispatch, "GitHub": run.github, "GitHub's timeline": run.timelines} {
		for _, request := range requests {
			if request.Method != http.MethodGet {
				t.Errorf("the collect sent %s %s %s; it sends GET alone", stub, request.Method, request.Target)
			}
		}
	}
}

// The settings a collect records where the deployment set none are the daemon's own defaults, read
// from the configuration's key table: an hour's tick, the gate on spec, a 72-hour linger, three
// review rounds, a six-hour stalled-tree notice, 30 minutes of worker silence and four hours of
// background work.
func TestCollectDefaultsToTheDaemonsSettings(t *testing.T) {
	t.Parallel()
	got, err := DaemonSettings()
	if err != nil {
		t.Fatalf("DaemonSettings: %v", err)
	}
	want := Settings{TickInterval: time.Hour, GateSteps: []record.DesignStep{record.DesignStepSpec}, Linger: 72 * time.Hour,
		ReviewRoundCap: 3, StalledTreeNotice: 6 * time.Hour, WorkerSilence: 30 * time.Minute, WorkerBackground: 4 * time.Hour}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DaemonSettings = %+v, want %+v", got, want)
	}
}

// A collect writes a window into a directory of its own: an output that holds a file already is
// refused before anything is read, and the file is left as it was.
func TestCollectRefusesAnOutputThatHoldsFiles(t *testing.T) {
	t.Parallel()
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := DaemonSettings()
	if err != nil {
		t.Fatal(err)
	}
	server, seen := dispatchStub(t, dispatchServed{})
	_, err = Collect(context.Background(), CollectConfig{Project: "ACME", From: collectFrom, To: collectTo, Logs: []string{out},
		Dispatch: NewDispatchReader(server.URL, stubDispatchToken), Settings: settings, Collector: "abc", Out: out})
	if err == nil || !strings.Contains(err.Error(), "notes.txt") {
		t.Errorf("Collect into a directory holding notes.txt = %v, want a refusal naming it", err)
	}
	if len(seen.all()) != 0 {
		t.Errorf("the refused collect read Dispatch: %v", seen.all())
	}
	if data, err := os.ReadFile(filepath.Join(out, "notes.txt")); err != nil || string(data) != "mine" {
		t.Errorf("notes.txt reads %q, %v after the refused collect; want it as it was", data, err)
	}
}

// A collect with a run-out reads past the window's end (the architect's D12): the daemon's lines and
// each issue's Dispatch events written in [To, To + after) join the window, which records the margin
// in window.json, and a line or an event written at the run-out's end or later stays out, as one
// written at To does without a run-out. The test collects three slot lines of ACME-9, an hour before
// the span's end, an hour after it and two hours after it, with a run-out of two hours, against a
// Dispatch stub answering three events of ACME-9 at the same moments. It fails where a collect stops
// at the span's end or records no margin.
func TestCollectReadsTheRunOutAfterTheWindow(t *testing.T) {
	t.Parallel()
	moments := []time.Time{collectTo.Add(-time.Hour), collectTo.Add(time.Hour), collectTo.Add(2 * time.Hour)}
	var log bytes.Buffer
	var lines, events []json.RawMessage
	for i, at := range moments {
		line := fmt.Sprintf(`{"time":%q,"level":"INFO","msg":"admission: slot taken","issue":"ACME-9","slots":%d,"cap":9}`,
			at.Format(time.RFC3339Nano), i+1)
		fmt.Fprintln(&log, line)
		lines = append(lines, compactJSON(t, []byte(line)))
		events = append(events, compactJSON(t, fmt.Appendf(nil, `{"id":%d,"issue_key":"ACME-9","artifact_id":null,"project":"ACME","seq":%d,`+
			`"type":"message.created","actor":{"kind":"session","id":"legion-daemon:ACME"},"notify":false,"created_at":%q,"payload":{"body":"note %d"}}`,
			900101+i, i+1, at.Format(time.RFC3339Nano), i+1)))
	}
	logs := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(logs, log.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	server, _ := dispatchStub(t, dispatchServed{events: map[string][]json.RawMessage{"ACME-9": events}})
	settings, err := DaemonSettings()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "window")
	if _, err := Collect(context.Background(), CollectConfig{Project: "ACME", From: collectFrom, To: collectTo, Before: time.Hour,
		After: 2 * time.Hour, Logs: []string{logs}, Dispatch: NewDispatchReader(server.URL, stubDispatchToken), Settings: settings,
		Collector: "0123456789abcdef0123456789abcdef01234567", Out: out}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	read, err := ReadWindow(out)
	if err != nil {
		t.Fatalf("ReadWindow of the collected window: %v", err)
	}
	if time.Duration(read.After) != 2*time.Hour || !read.To.Equal(collectTo) {
		t.Errorf("window.json reads back a span to %s with a run-out of %s, want to %s with one of 2h0m0s", stamp(read.To),
			time.Duration(read.After), stamp(collectTo))
	}
	if !reflect.DeepEqual(read.Log, lines[:2]) {
		t.Errorf("the log holds\n  %s\nwant the lines before the run-out's end\n  %s", read.Log, lines[:2])
	}
	if !reflect.DeepEqual(read.Events["ACME-9"], events[:2]) {
		t.Errorf("ACME-9's events are\n  %s\nwant those before the run-out's end\n  %s", read.Events["ACME-9"], events[:2])
	}
}
