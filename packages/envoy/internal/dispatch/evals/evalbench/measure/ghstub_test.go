package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
)

// stubAt is the stand-in's handler with a clock the test sets and a log it reads.
func stubAt(now time.Time) (*githubStub, *bytes.Buffer) {
	log := &bytes.Buffer{}
	return &githubStub{log: log, now: func() time.Time { return now }}, log
}

// call sends one request to the stand-in's handler and answers its status and body.
func call(t *testing.T, stub http.Handler, method, target, accept string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response := httptest.NewRecorder()
	stub.ServeHTTP(response, request)
	return response.Code, response.Body.String()
}

// TestTheStubAnswersTheAppsInstallationAndTokenCallsAndNoOther: the stand-in answers the lookup of
// the installation that covers a repository (200), the mint of its token (201) and the list of the
// App's installations (200, empty), and every other request 404, a repository's own routes among
// them, whatever the method and whatever the request asks to accept; so a read of a repository's
// history that reached it would find the App with no access to the repository.
func TestTheStubAnswersTheAppsInstallationAndTokenCallsAndNoOther(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	stub, _ := stubAt(now)
	for _, row := range []struct {
		name           string
		method, target string
		accept         string
		status         int
		body           []string
	}{
		{"the installation of a repository", http.MethodGet, "/repos/acme/infra-platform/installation", "application/json", http.StatusOK, []string{`"id":1`, `"contents":"read"`, `"pull_requests":"read"`}},
		{"the token of an installation", http.MethodPost, "/app/installations/1/access_tokens", "", http.StatusCreated, []string{`"token":"ghs_evalbench"`, `"expires_at":"2026-11-26T13:00:00Z"`}},
		{"the App's installations", http.MethodGet, "/app/installations?per_page=100&page=1", "application/json", http.StatusOK, []string{"[]"}},
		{"a repository", http.MethodGet, "/repos/acme/infra-platform", "", http.StatusNotFound, []string{"Not Found"}},
		{"the commits of a repository", http.MethodGet, "/repos/acme/infra-platform/commits?sha=main&per_page=100", "", http.StatusNotFound, nil},
		{"a patch", http.MethodGet, "/repos/acme/infra-platform/commits/abc", "application/vnd.github.diff", http.StatusNotFound, nil},
		{"a comparison", http.MethodGet, "/repos/acme/infra-platform/compare/a...b", "application/vnd.github.diff", http.StatusNotFound, nil},
		{"the pull requests", http.MethodGet, "/repos/acme/infra-platform/pulls?state=closed", "", http.StatusNotFound, nil},
		{"the installation, by POST", http.MethodPost, "/repos/acme/infra-platform/installation", "", http.StatusNotFound, nil},
		{"a token, by GET", http.MethodGet, "/app/installations/1/access_tokens", "", http.StatusNotFound, nil},
		{"the root", http.MethodGet, "/", "", http.StatusNotFound, nil},
	} {
		status, body := call(t, stub, row.method, row.target, row.accept)
		if status != row.status {
			t.Errorf("%s: %s %s answered %d %s, want %d", row.name, row.method, row.target, status, body, row.status)
		}
		for _, want := range row.body {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the answer %s lacks %s", row.name, body, want)
			}
		}
	}
}

// TestTheStubLogsEveryRequestItIsSentWhateverItAnswers: each request, the 404s too, is one JSON line
// with its time, method, path with its query and Accept, in the order the stand-in received them.
func TestTheStubLogsEveryRequestItIsSentWhateverItAnswers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 123_000_000, time.UTC)
	stub, log := stubAt(now)
	sent := []githubRequest{
		{Time: now, Method: http.MethodGet, Path: "/repos/acme/infra-platform/installation", Accept: "application/json"},
		{Time: now, Method: http.MethodPost, Path: "/app/installations/1/access_tokens"},
		{Time: now, Method: http.MethodGet, Path: "/repos/acme/infra-platform/commits/abc", Accept: "application/vnd.github.diff"},
		{Time: now, Method: http.MethodGet, Path: "/repos/acme/infra-platform/commits?sha=main&per_page=100"},
	}
	for _, request := range sent {
		call(t, stub, request.Method, request.Path, request.Accept)
	}
	lines := strings.Split(strings.TrimSuffix(log.String(), "\n"), "\n")
	if len(lines) != len(sent) {
		t.Fatalf("the log holds %d lines for %d requests:\n%s", len(lines), len(sent), log.String())
	}
	for index, line := range lines {
		var logged githubRequest
		if err := json.Unmarshal([]byte(line), &logged); err != nil {
			t.Fatalf("line %d is no JSON request: %v: %s", index+1, err, line)
		}
		if want := sent[index]; !logged.Time.Equal(want.Time) || logged.Method != want.Method || logged.Path != want.Path || logged.Accept != want.Accept {
			t.Errorf("line %d is %+v, want %+v", index+1, logged, want)
		}
	}
}

// failingWriter is a log that refuses every write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestAStubThatCannotLogAnswers500: a request the stand-in could not record is answered 500, so a
// paint never passes over a request the log lacks while the server reads an answer for it.
func TestAStubThatCannotLogAnswers500(t *testing.T) {
	t.Parallel()
	stub := &githubStub{log: failingWriter{}, now: time.Now}
	if status, body := call(t, stub, http.MethodGet, "/repos/acme/infra-platform/installation", ""); status != http.StatusInternalServerError {
		t.Errorf("a request the stand-in could not log answered %d %s, want 500", status, body)
	}
}

// TestTheStubListensOnALoopbackPortAndStopsWithItsLogWhole: the stand-in serves on a port the
// kernel chooses at a loopback address, publishes the port to a file that holds it whole, answers
// a request over the socket, and stops at its context's end with the request in the log.
func TestTheStubListensOnALoopbackPortAndStopsWithItsLogWhole(t *testing.T) {
	t.Parallel()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	directory := t.TempDir()
	portFile := filepath.Join(directory, "github.port")
	if err := writePortFile(portFile, listener); err != nil {
		t.Fatalf("publish the port: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	if written, err := os.ReadFile(portFile); err != nil || string(written) != port+"\n" {
		t.Errorf("the port file holds %q (%v), want %q", written, err, port+"\n")
	}
	if _, err := os.Stat(portFile + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("the port file's temporary copy was left behind (%v)", err)
	}

	logPath := filepath.Join(directory, "github.jsonl")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	defer log.Close()
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveGitHubStub(ctx, listener, log) }()
	response, err := http.Get("http://" + listener.Addr().String() + "/repos/acme/infra-platform/installation")
	if err != nil {
		t.Fatalf("GET the installation: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("the installation answered %d over the socket", response.StatusCode)
	}
	stop()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("the stand-in stopped with %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stand-in did not stop at its context's end")
	}
	requests, err := readGitHubLog(logPath)
	if err != nil || len(requests) != 1 || requests[0].Path != "/repos/acme/infra-platform/installation" {
		t.Errorf("the log holds %+v (%v), want the one installation request", requests, err)
	}
}

// TestTheStubListensOnLoopbackAlone: an address that is no loopback host is refused, since the
// stand-in answers the App's calls and the server it stands in for hands it a signed App token.
func TestTheStubListensOnLoopbackAlone(t *testing.T) {
	t.Parallel()
	for address, ok := range map[string]bool{
		"127.0.0.1:0": true, "127.0.0.9:0": true, "[::1]:0": true, "localhost:0": true,
		"0.0.0.0:0": false, ":0": false, "[::]:0": false, "10.0.0.5:0": false, "example.com:80": false, "nonsense": false,
	} {
		if err := loopbackListen(address); (err == nil) != ok {
			t.Errorf("-listen %s: %v, want accepted: %t", address, err, ok)
		}
	}
}

// TestTheStubCommandNeedsItsLogAndItsPortFileAndALoopbackAddress: a command line without -log or
// -port-file, or one that names a public address, exits 2 and starts no server.
func TestTheStubCommandNeedsItsLogAndItsPortFileAndALoopbackAddress(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"-log", filepath.Join(directory, "log")},
		{"-port-file", filepath.Join(directory, "port")},
		{"-log", filepath.Join(directory, "log"), "-port-file", filepath.Join(directory, "port"), "-listen", "0.0.0.0:0"},
	} {
		var stderr bytes.Buffer
		if status := ghstub(args, &stderr); status != 2 {
			t.Errorf("ghstub %q exited %d (%s), want 2", args, status, stderr.String())
		}
	}
}

// TestTheLogIsReadBackAsItWasWrittenAndAMalformedLineNamesItself: readGitHubLog reads what the
// handler wrote, an empty log as no request, and refuses a line that is no request by its number.
func TestTheLogIsReadBackAsItWasWrittenAndAMalformedLineNamesItself(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "github.jsonl")
	if requests, err := readGitHubLog(path); err == nil {
		t.Errorf("a log that does not exist read as %+v", requests)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if requests, err := readGitHubLog(path); err != nil || len(requests) != 0 {
		t.Errorf("an empty log read as %+v (%v), want no request", requests, err)
	}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	stub, log := stubAt(now)
	call(t, stub, http.MethodGet, "/repos/acme/infra-platform/installation", "")
	call(t, stub, http.MethodGet, "/app/installations", "")
	if err := os.WriteFile(path, append(log.Bytes(), "not json\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readGitHubLog(path)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("a log whose third line is malformed read as %v, want a refusal naming line 3", err)
	}
	if err := os.WriteFile(path, log.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	requests, err := readGitHubLog(path)
	if err != nil || len(requests) != 2 || requests[1].Path != "/app/installations" {
		t.Errorf("the log read as %+v (%v), want its two requests in order", requests, err)
	}
}
