// packages/envoy/cmd/dev-proxy-stands/projects_test.go
//
// The two stands the Slack limit's check reads, as that check reaches them: the Dispatch stand's
// project-access routes, served from the file --project-access-file names to the one bearer it
// holds, and the Slack stand, which answers a post as Slack does and records it.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// accessFile is the project-access file the local stack writes for its Slack limit: one private
// project, its owner, the person its access list names and the one channel its owner allows.
const accessFile = `{
  "rows": {"PRIV": {"visibility": "private", "owner": "owner@airlock.test", "listed": ["person@airlock.test"], "slack_channels": ["C0ALLOWED1"]}},
  "people": {"person@airlock.test": "full"},
  "token": "the-stack-dispatch-bearer"
}`

// getWithBearer makes one GET with bearer and answers the status and the JSON object it answered, or
// nil for a body that is no JSON object.
func getWithBearer(t *testing.T, url, bearer string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	var object map[string]any
	_ = json.Unmarshal(body, &object)
	return answer.StatusCode, object
}

// TestTheDispatchStandServesTheProjectAccessFile: with --project-access-file the Dispatch stand
// answers the broker's project-access client from the file, to the file's bearer alone, and still
// answers the judge's reads of an issue; without it, it answers no access route.
func TestTheDispatchStandServesTheProjectAccessFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "project-access.json")
	if err := os.WriteFile(file, []byte(accessFile), 0o600); err != nil {
		t.Fatal(err)
	}
	out := startStands(t, "--dir", dir, "--project-access-file", file)
	dispatch := out.Stands[standDispatch].URL
	const bearer = "the-stack-dispatch-bearer"

	status, project := getWithBearer(t, dispatch+"/api/v1/access/projects/PRIV?person=person@airlock.test", bearer)
	channels, _ := project["slack_channels"].([]any)
	if status != http.StatusOK || project["visibility"] != "private" || project["visible"] != true || project["owner"] != "owner@airlock.test" ||
		!slices.Equal(channels, []any{"C0ALLOWED1"}) {
		t.Errorf("the project route answered %d %v; want PRIV private, visible to its listed person, with the owner's one channel", status, project)
	}
	if status, person := getWithBearer(t, dispatch+"/api/v1/access/person?person=person@airlock.test", bearer); status != http.StatusOK || person["member_kind"] != "full" {
		t.Errorf("the person route answered %d %v; want 200 and member_kind full", status, person)
	}
	if status, refused := getWithBearer(t, dispatch+"/api/v1/access/person?person=person@airlock.test", "another-bearer"); status != http.StatusForbidden {
		t.Errorf("another bearer was answered %d %v; want 403", status, refused)
	}
	if status, issue := getWithBearer(t, dispatch+"/api/v1/issues/"+out.DispatchOpenIssue, "the-judge-bearer"); status != http.StatusOK || issue == nil {
		t.Errorf("the judge's read of %s answered %d %v; want the issue, beside the access routes", out.DispatchOpenIssue, status, issue)
	}
	if records := recordsOf(t, out.Stands[standDispatch].Records); len(records) != 4 {
		t.Errorf("the Dispatch stand recorded %d requests; want the four this row made", len(records))
	}

	plain := startStands(t, "--dir", t.TempDir())
	if _, person := getWithBearer(t, plain.Stands[standDispatch].URL+"/api/v1/access/person?person=person@airlock.test", bearer); person["member_kind"] != nil {
		t.Errorf("with no --project-access-file the person route answered %v; want no access answer at all", person)
	}
}

// TestTheStandsRefuseAProjectAccessFileTheyCannotRead: a file that is not there, that is no JSON
// object of the three keys or that names no bearer is a usage error naming the flag, and nothing
// is served.
func TestTheStandsRefuseAProjectAccessFileTheyCannotRead(t *testing.T) {
	dir := t.TempDir()
	binary := buildStands(t)
	for name, content := range map[string]string{
		"no such file":   "",
		"no JSON":        "rows: PRIV",
		"an unknown key": `{"rows": {}, "people": {}, "token": "t", "extra": 1}`,
		"no bearer":      `{"rows": {"PRIV": {"visibility": "private"}}, "people": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if content != "" {
				if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), binary, "--dir", t.TempDir(), "--project-access-file", file)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			err := cmd.Run()
			exit, _ := err.(*exec.ExitError)
			if exit == nil || exit.ExitCode() != exitUsageError || !strings.Contains(stderr.String(), "--project-access-file") {
				t.Fatalf("a file with %s ended %v, stderr %q; want exit %d naming --project-access-file", name, err, stderr.String(), exitUsageError)
			}
		})
	}
}

// TestTheSlackStandAnswersAPostAndRecordsIt: the Slack stand answers POST /api/chat.postMessage
// {"ok":true}, as Slack answers a post it made, under the name --host gave it, and records the
// post with its Authorization and its body; any other request is answered the stand's own body.
func TestTheSlackStandAnswersAPostAndRecordsIt(t *testing.T) {
	out := startStands(t, "--dir", t.TempDir(), "--host", "slack=slack.airlock.test")
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(out.CACertFile)
	if err != nil {
		t.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("%s holds no PEM certificate", out.CACertFile)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "slack.airlock.test", MinVersion: tls.VersionTLS12},
	}}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, out.Stands[standSlack].URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer the-bot-token")
		request.Header.Set("Content-Type", "application/json")
		answer, err := client.Do(request)
		if err != nil {
			t.Fatalf("%s %s at the Slack stand: %v", method, path, err)
		}
		defer answer.Body.Close()
		read, err := io.ReadAll(answer.Body)
		if err != nil {
			t.Fatal(err)
		}
		return answer.StatusCode, strings.TrimSpace(string(read))
	}

	if status, body := call(http.MethodPost, "/api/chat.postMessage", `{"channel":"C0ALLOWED1","text":"t"}`); status != http.StatusOK || body != `{"ok":true}` {
		t.Errorf("the post was answered %d %q; want 200 {\"ok\":true}", status, body)
	}
	if _, body := call(http.MethodGet, "/api/auth.test", ""); body == `{"ok":true}` {
		t.Errorf("a request that is no post was answered %q; want the stand's own body", body)
	}
	records := recordsOf(t, out.Stands[standSlack].Records)
	if len(records) != 2 || records[0].Method != http.MethodPost || records[0].Path != "/api/chat.postMessage" ||
		records[0].Header.Get("Authorization") != "Bearer the-bot-token" || records[0].Body != `{"channel":"C0ALLOWED1","text":"t"}` {
		t.Fatalf("the Slack stand recorded %+v; want the post first, with its Authorization and its body", records)
	}
}
