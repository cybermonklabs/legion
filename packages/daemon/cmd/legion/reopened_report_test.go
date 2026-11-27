package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim" // main_test.go's `claim` helper holds the bare name
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reportRequested is the moment of the controller's accepted todo in each issue the report test
// reads.
var reportRequested = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// reportIssue is one issue as the stand-in Dispatch serves it: its event log, each version of its
// close-wait.md, what each version of its Verification document changed, and whether the second
// page of its log answers 500, a page of events the report cannot read.
type reportIssue struct {
	events    []json.RawMessage
	failPage  bool
	closeWait map[int]string
	changes   map[int]dispatch.ArtifactChanges
}

// reportDispatch is Dispatch as `legion reopened-report` reads it, which takes only the
// credential scheme names with token, and counts each request it was sent.
type reportDispatch struct {
	url    string
	scheme string
	token  string
	issues map[string]*reportIssue
	mu     sync.Mutex
	asked  []string
}

// newReportDispatch serves issues, taking each request whose Authorization is scheme and token.
func newReportDispatch(t *testing.T, scheme, token string, issues map[string]*reportIssue) *reportDispatch {
	t.Helper()
	d := &reportDispatch{scheme: scheme, token: token, issues: issues}
	mux := http.NewServeMux()
	issueOf := func(w http.ResponseWriter, key string) *reportIssue {
		issue, ok := d.issues[key]
		if !ok {
			http.Error(w, `{"code":"ISSUE_NOT_FOUND","error":"no such issue"}`, http.StatusNotFound)
		}
		return issue
	}
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if issueOf(w, key) == nil {
			return
		}
		writeReportJSON(w, []map[string]any{
			{"id": "cw-" + key, "slug": record.CloseWaitDocumentSlug, "kind": "doc", "role": "", "versions": []map[string]int{{"number": 1}}},
			{"id": "vr-" + key, "slug": dispatch.VerificationSlug, "kind": "doc", "role": dispatch.ArtifactRoleVerification, "versions": []map[string]int{{"number": 1}}},
		})
	})
	mux.HandleFunc("GET /api/v1/issues/{key}/events", func(w http.ResponseWriter, r *http.Request) {
		issue := issueOf(w, r.PathValue("key"))
		if issue == nil {
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if issue.failPage && after > 0 {
			http.Error(w, `{"code":"INTERNAL","error":"the page could not be read"}`, http.StatusInternalServerError)
			return
		}
		page := []json.RawMessage{}
		for _, raw := range issue.events {
			var event struct {
				Seq int64 `json:"seq"`
			}
			_ = json.Unmarshal(raw, &event)
			if event.Seq > after && len(page) < limit {
				page = append(page, raw)
			}
		}
		writeReportJSON(w, page)
	})
	mux.HandleFunc("GET /api/v1/artifacts/{id}/versions/{number}", func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.PathValue("id"), "cw-")
		number, _ := strconv.Atoi(r.PathValue("number"))
		var markdown string
		if issue := d.issues[key]; ok && issue != nil {
			markdown, ok = issue.closeWait[number]
		} else {
			ok = false
		}
		if !ok {
			http.Error(w, `{"code":"NOT_FOUND","error":"no such version"}`, http.StatusNotFound)
			return
		}
		writeReportJSON(w, map[string]any{"number": number, "markdown": markdown})
	})
	mux.HandleFunc("GET /api/v1/artifacts/{id}/changes", func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.PathValue("id"), "vr-")
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		to, _ := strconv.Atoi(r.URL.Query().Get("to"))
		var changes dispatch.ArtifactChanges
		if issue := d.issues[key]; ok && issue != nil && from == to-1 {
			changes, ok = issue.changes[to]
		} else {
			ok = false
		}
		if !ok {
			http.Error(w, `{"code":"NOT_FOUND","error":"no such pair"}`, http.StatusNotFound)
			return
		}
		changes.ArtifactID, changes.From, changes.To = r.PathValue("id"), from, to
		writeReportJSON(w, changes)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.asked = append(d.asked, r.URL.Path)
		d.mu.Unlock()
		if r.Header.Get("Authorization") != d.scheme+" "+d.token || (d.scheme == "DPoP") != (r.Header.Get("DPoP") != "") {
			http.Error(w, `{"code":"UNAUTHORIZED","error":"no such credential"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	d.url = server.URL
	return d
}

// requests is each path the stand-in was asked, in order.
func (d *reportDispatch) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.asked...)
}

// writeReportJSON answers value as JSON.
func writeReportJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

// reportLog writes one issue's Dispatch log for the stand-in, each event at the next sequence.
type reportLog struct {
	t     *testing.T
	key   string
	issue *reportIssue
	waits int
}

// newReportLog is the log of key, empty.
func newReportLog(t *testing.T, key string) *reportLog {
	return &reportLog{t: t, key: key, issue: &reportIssue{closeWait: map[int]string{}, changes: map[int]dispatch.ArtifactChanges{}}}
}

// event appends one event of typ by actor at at, carrying payload, as Dispatch's log serves it.
func (l *reportLog) event(typ string, actor dispatch.Actor, at time.Time, payload map[string]any) {
	l.t.Helper()
	seq := int64(len(l.issue.events) + 1)
	encoded, err := json.Marshal(map[string]any{
		"id": 5000 + seq, "issue_key": l.key, "artifact_id": nil, "project": "ACME", "seq": seq, "type": typ,
		"actor": map[string]string{"kind": actor.Kind, "id": actor.ID}, "notify": false,
		"created_at": at.Format(time.RFC3339Nano), "payload": payload,
	})
	if err != nil {
		l.t.Fatal(err)
	}
	l.issue.events = append(l.issue.events, encoded)
}

// status appends an issue event of the daemon's that leaves the issue at status.
func (l *reportLog) status(at time.Time, status string) {
	l.event("issue.updated", dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(l.key)}, at,
		map[string]any{"key": l.key, "status": status, "changed": []string{"status"}})
}

// wait appends the daemon's next version of close-wait.md, rendered from stored as the daemon
// renders it (record.CloseWaitDocument).
func (l *reportLog) wait(at time.Time, stored record.StoredWait) {
	l.t.Helper()
	encoded, err := json.Marshal(stored)
	if err != nil {
		l.t.Fatal(err)
	}
	markdown, err := record.CloseWaitDocument(record.Issue{Key: l.key, CloseWait: encoded})
	if err != nil {
		l.t.Fatal(err)
	}
	l.waits++
	l.issue.closeWait[l.waits] = markdown
	daemon := dispatch.Actor{Kind: "session", ID: dispatch.DaemonSession(l.key)}
	if l.waits == 1 {
		l.event("artifact.created", daemon, at, map[string]any{"artifact": map[string]any{"id": "cw-" + l.key, "versions": []map[string]int{{"number": 1}}}})
		return
	}
	l.event("artifact.version", daemon, at, map[string]any{"artifact_id": "cw-" + l.key, "version": map[string]int{"number": l.waits}})
}

// reopenedLog is an issue whose first work merged and closed, then the controller's todo at
// reportRequested, the change it started open, and that change's first phase started at once.
func reopenedLog(t *testing.T, key string) *reportLog {
	l := newReportLog(t, key)
	l.event("issue.created", dispatch.Actor{Kind: "user", ID: "operator@example.com"}, reportRequested.Add(-96*time.Hour),
		map[string]any{"key": key, "status": "todo"})
	closed := reportRequested.Add(-72 * time.Hour)
	l.wait(closed, record.StoredWait{Entered: closed, Closed: &closed})
	l.status(closed.Add(time.Second), "done")
	l.status(reportRequested, "todo")
	l.wait(reportRequested.Add(2*time.Second), record.StoredWait{NextChange: &record.WaitChange{Generation: 2, Since: reportRequested,
		Reasons: []record.WaitReason{{ID: 17, Kind: record.DefectKindRequest, Text: "Reopened after an early close: the export.", Role: legionclaim.RoleController, Since: reportRequested}}}})
	l.status(reportRequested.Add(3*time.Second), "in_progress")
	return l
}

// ownerEdit appends version 4 of the Verification document by the owner, adding a check block.
func (l *reportLog) ownerEdit(at time.Time) {
	l.issue.changes[4] = dispatch.ArtifactChanges{Blocks: []dispatch.BlockChange{{ID: "c-export", Type: "check", Change: dispatch.ChangeAdded}}}
	l.event("artifact.version", dispatch.Actor{Kind: "session", ID: "ses-owner"}, at, map[string]any{"artifact_id": "vr-" + l.key, "version": map[string]int{"number": 4}})
}

// reportIssues is the three issues the report test reads: ACME-1, whose owner wrote a check during
// the change's first phase and whose change merged as pull request #51; ACME-2, whose log's second
// page cannot be read; and ACME-3, whose change is still open.
func reportIssues(t *testing.T) map[string]*reportIssue {
	written := reopenedLog(t, "ACME-1")
	written.ownerEdit(reportRequested.Add(time.Hour))
	merged := reportRequested.Add(3 * time.Hour)
	written.wait(merged.Add(5*time.Second), record.StoredWait{Entered: merged, Ended: &record.WaitChangeEnd{Generation: 2, PullRequest: 51, At: merged}})
	unreadable := newReportLog(t, "ACME-2")
	for i := range 200 {
		unreadable.event("message.created", dispatch.Actor{Kind: "session", ID: "ses-owner"}, reportRequested.Add(time.Duration(i)*time.Second), map[string]any{"body": "a note"})
	}
	unreadable.issue.failPage = true
	open := reopenedLog(t, "ACME-3")
	open.ownerEdit(reportRequested.Add(time.Hour))
	return map[string]*reportIssue{"ACME-1": written.issue, "ACME-2": unreadable.issue, "ACME-3": open.issue}
}

// reportConfig writes a controller.yaml naming the project ACME, dispatchURL and the lines extra
// (its Dispatch credential), and answers its path.
func reportConfig(t *testing.T, dispatchURL string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	lines := []string{"project: ACME", "daemon_url: http://127.0.0.1:1", "operator_token_file: ./operator-token",
		"envoy_url: http://envoy.test:9020", "nats_urls: [nats://a:4222]"}
	if dispatchURL != "" {
		lines = append(lines, "dispatch_url: "+dispatchURL)
	}
	lines = append(lines, extra...)
	path := filepath.Join(dir, "controller.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// reportLines is each line the report printed, its fields, by the line's first word.
func reportLines(stdout string) map[string][][]string {
	lines := map[string][][]string{}
	for line := range strings.Lines(stdout) {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			lines[fields[0]] = append(lines[fields[0]], fields[1:])
		}
	}
	return lines
}

// fieldsOf is the lines of kind whose first field is key.
func fieldsOf(lines map[string][][]string, kind, key string) [][]string {
	var of [][]string
	for _, fields := range lines[kind] {
		if len(fields) > 0 && fields[0] == key {
			of = append(of, fields[1:])
		}
	}
	return of
}

// `legion reopened-report` reads each issue through the controller's Dispatch credential and
// prints its row by field: the category, the interval's start and end, the change, each version
// in the interval with the check blocks it changed, what it could not read, and each category's
// count with its keys. It exits 1 when a row could not be read, 0 otherwise. A call it cannot run
// as written is a usage error, and a file that names no credential reads nothing and says so.
func TestReopenedReportPrintsEachRowByField(t *testing.T) {
	t.Parallel()
	const token = "dispatch-token-7c1e"
	tokenFile := writeFile(t, "dispatch-token", token+"\n")
	t.Run("each row by field", func(t *testing.T) {
		t.Parallel()
		d := newReportDispatch(t, "Bearer", token, reportIssues(t))
		config := reportConfig(t, d.url, "dispatch_token_file: "+tokenFile)
		code, stdout, stderr := legionRun(t, "reopened-report", "--config", config, "--issue", "ACME-1", "--issue", "ACME-2", "--issue", "ACME-3")
		lines := reportLines(stdout)
		if code != 1 || !reflect.DeepEqual(fieldsOf(lines, "total", "not_assessable"), [][]string{{"1", "ACME-2"}}) {
			t.Fatalf("legion reopened-report = %d with totals %q, want 1 for the one row it could not read, ACME-2; stdout %q, stderr %q", code, lines["total"], stdout, stderr)
		}
		want := map[string][][]string{
			"row":    {{"ACME-1", "checks_written"}, {"ACME-2", "not_assessable"}, {"ACME-3", "pending"}},
			"start":  {{"ACME-1", "seq=4", "at=2026-11-20T10:00:00Z"}, {"ACME-2", "UNKNOWN"}, {"ACME-3", "seq=4", "at=2026-11-20T10:00:00Z"}},
			"change": {{"ACME-1", "2"}, {"ACME-2", "UNKNOWN"}, {"ACME-3", "2"}},
			"end":    {{"ACME-1", "merged", "pull_request=51", "at=2026-11-20T13:00:00Z"}, {"ACME-2", "UNKNOWN"}, {"ACME-3", "open"}},
			"version": {{"ACME-1", "number=4", "actor=session:ses-owner", "at=2026-11-20T11:00:00Z", "checks=c-export:added"},
				{"ACME-3", "number=4", "actor=session:ses-owner", "at=2026-11-20T11:00:00Z", "checks=c-export:added"}},
			"total": {{"checks_written", "1", "ACME-1"}, {"request_withdrawn", "0"}, {"instruction_not_followed", "0"}, {"pending", "1", "ACME-3"},
				{"not_assessable", "1", "ACME-2"}},
			"assessed": {{"1"}},
		}
		for kind, rows := range want {
			if !reflect.DeepEqual(lines[kind], rows) {
				t.Errorf("the %s lines are %q, want %q", kind, lines[kind], rows)
			}
		}
		if unread := fieldsOf(lines, "unread", "ACME-2"); len(unread) == 0 || len(lines["unread"]) != len(unread) {
			t.Errorf("the unread lines are %q, want at least one, of ACME-2 alone", lines["unread"])
		}
		if len(lines["withdrawal"]) != 0 || len(lines["unknown"]) != 0 {
			t.Errorf("the report printed withdrawal lines %q and unknown lines %q, want none", lines["withdrawal"], lines["unknown"])
		}
	})
	t.Run("every row read", func(t *testing.T) {
		t.Parallel()
		d := newReportDispatch(t, "Bearer", token, reportIssues(t))
		config := reportConfig(t, d.url, "dispatch_token_file: "+tokenFile)
		code, stdout, stderr := legionRun(t, "reopened-report", "--config", config, "--issue", "ACME-1", "--issue", "ACME-3")
		lines := reportLines(stdout)
		if code != 0 || !reflect.DeepEqual(fieldsOf(lines, "total", "not_assessable"), [][]string{{"0"}}) || len(lines["unread"]) != 0 {
			t.Fatalf("legion reopened-report = %d with totals %q and unread %q, want 0 with no row it could not read; stderr %q", code, lines["total"], lines["unread"], stderr)
		}
	})
	t.Run("a call it cannot run as written", func(t *testing.T) {
		t.Parallel()
		d := newReportDispatch(t, "Bearer", token, reportIssues(t))
		config := reportConfig(t, d.url, "dispatch_token_file: "+tokenFile)
		for _, tc := range []struct {
			name  string
			args  []string
			named string
		}{
			{"no issue", []string{"--config", config}, "--issue"},
			{"a word that is no issue key", []string{"--config", config, "--issue", "acme1"}, "acme1"},
			{"no configuration", []string{"--issue", "ACME-1"}, "--config"},
			{"a positional word", []string{"--config", config, "--issue", "ACME-1", "ACME-3"}, "ACME-3"},
		} {
			code, stdout, stderr := legionRun(t, append([]string{"reopened-report"}, tc.args...)...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, tc.named) {
				t.Errorf("%s: legion reopened-report %q = %d, stdout %q, stderr %q; want 2, a usage error naming %s", tc.name, tc.args, code, stdout, stderr, tc.named)
			}
		}
		if asked := d.requests(); len(asked) != 0 {
			t.Fatalf("the usage errors asked Dispatch %q, want nothing", asked)
		}
	})
	t.Run("a file that names no Dispatch credential", func(t *testing.T) {
		t.Parallel()
		d := newReportDispatch(t, "Bearer", token, reportIssues(t))
		for _, tc := range []struct {
			name   string
			config string
		}{
			{"no dispatch_url", reportConfig(t, "")},
			{"a dispatch_url with no credential", reportConfig(t, d.url)},
		} {
			code, stdout, _ := legionRun(t, "reopened-report", "--config", tc.config, "--issue", "ACME-1")
			lines := reportLines(stdout)
			if code != 1 || !reflect.DeepEqual(lines["unknown"], [][]string{{"config"}}) || !reflect.DeepEqual(lines["assessed"], [][]string{{"UNKNOWN"}}) ||
				len(lines["total"]) != 5 || !reflect.DeepEqual(lines["total"][0], []string{"checks_written", "UNKNOWN"}) || len(lines["row"]) != 0 {
				t.Errorf("%s: legion reopened-report = %d, stdout %q; want 1 with unknown config, every total and assessed UNKNOWN, and no row", tc.name, code, stdout)
			}
		}
		if asked := d.requests(); len(asked) != 0 {
			t.Fatalf("a file with no credential asked Dispatch %q, want nothing", asked)
		}
	})
	t.Run("the broker route", func(t *testing.T) {
		t.Parallel()
		broker := newStubBroker(t, "")
		d := newReportDispatch(t, "DPoP", controllerBrokerToken, reportIssues(t))
		config := reportConfig(t, d.url, "dispatch_token_broker: true")
		projected := writeFile(t, "projected-token", "projected.service.account.token\n")
		env := map[string]string{"AGENT_SECRETS_URL": broker.url, "AGENT_SECRETS_MACHINE_IDENTITY": "kubernetes", "AGENT_SECRETS_MACHINE_TOKEN_FILE": projected}
		var stdout, stderr bytes.Buffer
		code := runReopenedReport(context.Background(), config, []string{"ACME-1"}, func(name string) string { return env[name] }, &stdout, &stderr)
		lines := reportLines(stdout.String())
		if code != 0 || !reflect.DeepEqual(lines["row"], [][]string{{"ACME-1", "checks_written"}}) || broker.trades.Load() == 0 {
			t.Fatalf("legion reopened-report on the broker route = %d with rows %q after %d trades, want 0, ACME-1 checks_written and a trade; stdout %q, stderr %q",
				code, lines["row"], broker.trades.Load(), stdout.String(), stderr.String())
		}
		if asked := d.requests(); len(asked) == 0 {
			t.Fatal("the broker route asked Dispatch nothing")
		}
		if sessions := broker.tradedSessions(); len(sessions) == 0 || !strings.HasPrefix(sessions[0], "legion-reopened-report-") {
			t.Fatalf("the report traded under the sessions %q, want one of its own", sessions)
		}
	})
}
