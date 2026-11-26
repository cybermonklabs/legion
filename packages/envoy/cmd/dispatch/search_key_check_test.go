package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// searchKeyRootDrops is what the root's pull request drops, which this build must boot without
// and never reach (this issue's spec R17): document_search with the
// artifact_versions_document_search trigger and its function, document_search_term_queue() with
// its trigger on document_search, and document_search_backfill, which Store.FillDocumentSearch
// read at each boot.
var searchKeyRootDrops = []string{
	`drop trigger artifact_versions_document_search on artifact_versions`,
	`drop function artifact_versions_document_search()`,
	`drop trigger document_search_term_queue on document_search`,
	`drop function document_search_term_queue()`,
	`drop table document_search`,
	`drop table document_search_backfill`,
}

// searchKeyDashboardHost is the host of the dashboard origin the dev sign-in process serves
// (DISPATCH_SERVER_URL), which every request to it names as its Host: a dev sign-in server
// answers no other host (routes' requireHost).
const searchKeyDashboardHost = "127.0.0.1"

// searchKeySignIn signs login in through GET /auth/_dev/signin on p, as a browser on this machine
// does, and answers the session cookies the sign-in sets.
func searchKeySignIn(t *testing.T, p *dispatchProcess, login string) []*http.Cookie {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, p.url("/auth/_dev/signin?login="+url.QueryEscape(login)), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = searchKeyDashboardHost
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /auth/_dev/signin: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusFound || len(response.Cookies()) == 0 {
		t.Fatalf("GET /auth/_dev/signin?login=%s: status %d, %d cookies, want 302 setting the session cookie: %s",
			login, response.StatusCode, len(response.Cookies()), body)
	}
	return response.Cookies()
}

// searchKeySend sends body to path on p as JSON, as the person whose session cookies are cookies,
// from the dashboard's own origin (Sec-Fetch-Site: same-origin), which a write under a cookie
// needs.
func searchKeySend(t *testing.T, p *dispatchProcess, cookies []*http.Cookie, method, path, body string) httpAnswer {
	t.Helper()
	request, err := http.NewRequest(method, p.url(path), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = searchKeyDashboardHost
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: read the answer: %v", method, path, err)
	}
	return httpAnswer{status: response.StatusCode, header: response.Header, body: answer}
}

// searchKeyLogLines is every line of p's output that names document_search: where a statement
// that reads or writes the dropped table failed, the server's log names it.
func searchKeyLogLines(p *dispatchProcess) string {
	var lines []string
	for _, line := range strings.Split(p.Output.String(), "\n") {
		if strings.Contains(line, "document_search") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// This build boots, removes a document, writes a version and searches over a database where the
// root's pull request has dropped document_search, its trigger and function,
// document_search_term_queue() with its trigger, and document_search_backfill (this issue's spec
// R17): it reads none of them. It calls no boot fill of document_search, its search reads
// node_search, and its removal's one statement naming document_search
// (artifactRemovalClears in api/artifact_removal.go) runs only where
// to_regclass('public.document_search') finds the table.
func TestThisBuildBootsAndRemovesWithTheRootsSearchTablesDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	for _, statement := range searchKeyRootDrops {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("drop what the root's pull request drops: %s: %v", statement, err)
		}
	}
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		Binary: cmdtest.Build(t, "envoy-dispatch"),
		Env: map[string]string{
			"DISPATCH_DEV_SIGNIN":              "1",
			"DISPATCH_IDENTITY":                "cookie",
			"DISPATCH_IDENTITY_HEADER_TRUSTED": "",
			"DISPATCH_INSECURE_COOKIE":         "1",
			"DISPATCH_SERVER_URL":              "http://" + searchKeyDashboardHost,
		},
	})
	process.waitHealthy(t)
	if health := process.send(t, http.MethodGet, "/healthz", "", nil); health.status != http.StatusOK {
		t.Fatalf("GET /healthz over the database without the root's search tables answers %s, want 200", health)
	}
	cookies := searchKeySignIn(t, process, "alice")
	call := func(method, path, body string, status int) []byte {
		t.Helper()
		answer := searchKeySend(t, process, cookies, method, path, body)
		if answer.status != status {
			t.Fatalf("%s %s answers %s, want %d; the server's lines naming document_search:\n%s", method, path, answer, status, searchKeyLogLines(process))
		}
		return answer.body
	}
	call(http.MethodPost, "/api/v1/projects", `{"key":"DROP","name":"Dropped search tables"}`, http.StatusCreated)
	var issue struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(call(http.MethodPost, "/api/v1/issues", `{"project":"DROP","title":"Search tables dropped","spec":"The spec."}`, http.StatusCreated), &issue); err != nil || issue.Key == "" {
		t.Fatalf("create the issue: %v", err)
	}
	owner := "/api/v1/issues/" + issue.Key + "/artifacts"
	var untouched, notes struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(call(http.MethodPost, owner, `{"name":"untouched.md"}`, http.StatusCreated), &untouched); err != nil || untouched.Artifact.ID == "" {
		t.Fatalf("make a document by typing a title: %v", err)
	}
	if err := json.Unmarshal(call(http.MethodPost, owner, `{"name":"notes.md","content":"The first version.\n"}`, http.StatusCreated), &notes); err != nil || notes.Artifact.ID == "" {
		t.Fatalf("upload notes.md: %v", err)
	}
	var refKey string
	if err := database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, untouched.Artifact.ID).Scan(&refKey); err != nil {
		t.Fatalf("read the document's ref_key: %v", err)
	}
	hasSearchRow := func() bool {
		t.Helper()
		var held bool
		if err := database.Pool.QueryRow(ctx, `select exists (
			select 1 from node_search where node_kind = 'artifact' and node_address = $1)`, refKey).Scan(&held); err != nil {
			t.Fatalf("read the document row of %s: %v", refKey, err)
		}
		return held
	}
	if !hasSearchRow() {
		t.Fatalf("the empty document %s holds no search row before removal", refKey)
	}

	if removed := searchKeySend(t, process, cookies, http.MethodDelete, "/api/v1/artifacts/"+untouched.Artifact.ID, ""); removed.status != http.StatusNoContent {
		t.Errorf("DELETE /api/v1/artifacts/%s answers %s, want 204: the removal's statement over document_search runs only where to_regclass('public.document_search') finds it; the server's lines naming document_search:\n%s",
			untouched.Artifact.ID, removed, searchKeyLogLines(process))
	}
	if hasSearchRow() {
		t.Errorf("node_search still holds the removed document %s", refKey)
	}

	var text struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(call(http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", "", http.StatusOK), &text); err != nil || text.Token == "" {
		t.Fatalf("read the token of notes.md: %v", err)
	}
	const second = "The second version holds the word quillmarrow.\n"
	upload, err := json.Marshal(map[string]any{"name": "notes.md", "content": second, "precondition": map[string]string{"document": text.Token}})
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Version struct {
			Number int `json:"number"`
		} `json:"version"`
	}
	if err := json.Unmarshal(call(http.MethodPost, owner, string(upload), http.StatusCreated), &written); err != nil || written.Version.Number != 2 {
		t.Fatalf("upload version 2 of notes.md: version %d, %v", written.Version.Number, err)
	}
	var stored struct {
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(call(http.MethodGet, "/api/v1/artifacts/"+notes.Artifact.ID+"/text", "", http.StatusOK), &stored); err != nil || stored.Markdown != second {
		t.Errorf("notes.md reads %q after its version 2, want %q stored: %v", stored.Markdown, second, err)
	}

	if search := searchKeySend(t, process, cookies, http.MethodGet, "/api/v1/search?q=quillmarrow", ""); search.status != http.StatusOK {
		t.Errorf("GET /api/v1/search answers %s, want 200: this build's search reads node_search and never document_search; the server's lines naming document_search:\n%s",
			search, searchKeyLogLines(process))
	}
}
