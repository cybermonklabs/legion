package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
)

// `legion issue status <issue> <status>` is the operator's: without --operator-token-file it is a
// usage error naming the flag, and the daemon is never reached — no grant is read from the
// environment.
func TestStatusIssueRequiresTheOperatorTokenFile(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	d := newControllerDaemon(t)
	t.Setenv("LEGION_DAEMON_URL", d.url)
	code, out, errb := issueStatus(t, "LEGION-208", "todo")
	if want := "legion issue status: --operator-token-file is required"; code != 2 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("legion issue status = %d, stdout %q, stderr %q; want a usage error saying %q", code, out, errb, want)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// issueStatus runs `legion issue status <args>` and holds it to the rule every run keeps: the
// operator token is in neither stream.
func issueStatus(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "issue", "status"}, args...), &out, &errb)
	for name, stream := range map[string]string{"stdout": out.String(), "stderr": errb.String()} {
		if strings.Contains(stream, controllerOperatorToken) {
			t.Fatalf("legion issue status %v printed the operator token on %s: %q", args, name, stream)
		}
	}
	return code, out.String(), errb.String()
}

// statusDaemon is the controller daemon whose issue status route answers queued, the answer of a
// status call that queued its writes, once the route has taken the request as far as that daemon
// reaches (controllerDaemonOptions.statusAnswer).
func statusDaemon(t *testing.T, queued string) *controllerDaemon {
	t.Helper()
	return newControllerDaemonWithOptions(t, controllerDaemonOptions{steps: config.DesignGateRootIssues.Steps(), statusAnswer: queued})
}

// statusRequest is the one status request d saw, decoded strictly as the route decodes it.
func statusRequest(t *testing.T, d *controllerDaemon) (seenRequest, api.IssueStatusRequest) {
	t.Helper()
	var status []seenRequest
	for _, request := range d.requests() {
		if request.path == "/legion/v1/issues/status" {
			status = append(status, request)
		}
	}
	if len(status) != 1 {
		t.Fatalf("%d status requests reached the daemon, want one", len(status))
	}
	var sent api.IssueStatusRequest
	decodeStrict(t, status[0].body, &sent)
	return status[0], sent
}

// From an operator shell, the operator's bearer buys a controller grant through the grants
// route's operator form, and the grant sets the status: two requests, the bearer on the first
// only.
func TestStatusWithTheOperatorTokenFileMintsAGrantAndSetsTheStatus(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	const queued = `{"queued":[{"issue":"LEGSMOKE-3","status":"backlog","row":41}]}`
	d := statusDaemon(t, queued)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	t.Setenv("LEGION_DAEMON_URL", "http://127.0.0.1:1")

	code, out, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if code != 0 || errb != "" {
		t.Fatalf("legion issue status = %d, stderr %q; want 0", code, errb)
	}
	if out != queued+"\n" {
		t.Errorf("stdout = %q, want the daemon's answer, the write it queued", out)
	}
	requests := d.requests()
	if len(requests) != 2 {
		t.Fatalf("%d requests reached the daemon, want the grant and the status", len(requests))
	}
	grant := requests[0]
	if grant.method != http.MethodPost || grant.path != "/legion/v1/grants" || grant.authorization != "Bearer "+controllerOperatorToken ||
		string(grant.body) != "{}" || grant.status != http.StatusOK {
		t.Fatalf("grant request = %s %s auth %q body %q → %d", grant.method, grant.path, grant.authorization, grant.body, grant.status)
	}
	var minted api.GrantResponse
	if err := json.Unmarshal(grant.answer, &minted); err != nil || minted.GrantID == "" {
		t.Fatalf("grant answer %s: %v", grant.answer, err)
	}
	status, sent := statusRequest(t, d)
	if status.authorization != "" || status.status != http.StatusOK ||
		sent != (api.IssueStatusRequest{GrantID: minted.GrantID, Issue: "LEGSMOKE-3", Status: "backlog"}) {
		t.Fatalf("status request auth %q body %+v → %d; want the minted grant, no bearer, and no argument", status.authorization, sent, status.status)
	}
}

// --children and --parent are the two arguments of Dispatch's parent rule, and the command sends
// them as the controller's set_status does (R24 of dispatch://INFRA-3320): --children include or
// unlink with a backlog or icebox, --parent include with a todo, each in the request's own field.
// The daemon's route takes each request through its own checks of the words and the grant.
func TestStatusSendsTheChildrenAndParentArguments(t *testing.T) {
	t.Parallel()
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	for _, tc := range []struct {
		args []string
		want api.IssueStatusRequest
	}{
		{[]string{"backlog", "--children", "include"}, api.IssueStatusRequest{Status: "backlog", Children: "include"}},
		{[]string{"icebox", "--children", "unlink"}, api.IssueStatusRequest{Status: "icebox", Children: "unlink"}},
		{[]string{"todo", "--parent", "include"}, api.IssueStatusRequest{Status: "todo", Parent: "include"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			queued := `{"queued":[{"issue":"LEGSMOKE-3","status":"` + tc.want.Status + `","row":7}]}`
			d := statusDaemon(t, queued)
			args := append([]string{"LEGSMOKE-3"}, tc.args...)
			code, out, errb := issueStatus(t, append(args, "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))...)
			if code != 0 || errb != "" || out != queued+"\n" {
				t.Fatalf("legion issue status %v = %d, stdout %q, stderr %q; want 0 and the daemon's answer", args, code, out, errb)
			}
			status, sent := statusRequest(t, d)
			tc.want.Issue, tc.want.GrantID = "LEGSMOKE-3", sent.GrantID
			if sent.GrantID == "" || status.status != http.StatusOK || sent != tc.want {
				t.Fatalf("status request %+v → %d, want %+v with the minted grant", sent, status.status, tc.want)
			}
		})
	}
}

// --config finds the daemon the way `legion state --config` does.
func TestStatusWithTheOperatorTokenFileFindsTheDaemonFromTheConfiguration(t *testing.T) {
	t.Parallel()
	d := statusDaemon(t, `{"queued":[{"issue":"LEGSMOKE-3","status":"icebox","row":41}]}`)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "icebox", "--operator-token-file", tokenFile, "--config", legionConfig(t, "demo", d.port))
	if code != 0 {
		t.Fatalf("legion issue status = %d, stderr %q", code, errb)
	}
	if _, sent := statusRequest(t, d); sent.Issue != "LEGSMOKE-3" || sent.Status != "icebox" {
		t.Fatalf("status request %+v, want LEGSMOKE-3 icebox", sent)
	}
}

// The wrong bearer is the daemon's refusal, printed as every operator command prints one; no
// status request follows.
func TestStatusWithAWrongOperatorTokenIsRefusedAndSetsNothing(t *testing.T) {
	t.Parallel()
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", "not-the-operator-token\n")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if code != 1 || errb != "legion issue status: the daemon answered 403 Forbidden: Invalid operator token\n" {
		t.Fatalf("legion issue status = %d, stderr %q; want the 403 refusal", code, errb)
	}
	if requests := d.requests(); len(requests) != 1 || requests[0].path != "/legion/v1/grants" {
		t.Fatalf("requests = %d, want only the refused grant request", len(requests))
	}
}

// A token file it cannot read is refused naming the flag and the system's reason, never the path,
// before the daemon is reached.
func TestStatusRefusesAnOperatorTokenFileItCannotRead(t *testing.T) {
	t.Parallel()
	d := newControllerDaemon(t)
	absent := filepath.Join(t.TempDir(), "absent")
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", absent, "--port", strconv.Itoa(d.port))
	if code != 1 || !strings.Contains(errb, "--operator-token-file names a file that could not be read: it does not exist") || strings.Contains(errb, absent) {
		t.Fatalf("legion issue status = %d, stderr %q", code, errb)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// The operator token file is held to `legion controller start`'s rule: one its group or others
// can read is refused naming the path and the mode, and the daemon is never reached.
func TestStatusRefusesAGroupReadableOperatorTokenFile(t *testing.T) {
	t.Parallel()
	d := newControllerDaemon(t)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	if err := os.Chmod(tokenFile, 0o640); err != nil {
		t.Fatal(err)
	}
	code, _, errb := issueStatus(t, "LEGSMOKE-3", "backlog", "--operator-token-file", tokenFile, "--port", strconv.Itoa(d.port))
	if want := "--operator-token-file " + tokenFile + " is readable by its group or others (mode 0640); chmod 0600 it"; code != 1 || !strings.Contains(errb, want) {
		t.Fatalf("legion issue status = %d, stderr %q; want 1 and %q", code, errb, want)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}

// Each usage error is its own, not the missing flag's: a status outside the three, a stray
// argument, a flag without its value, and a children or parent argument the daemon's route would
// refuse (its INVALID_CHILDREN and INVALID_PARENT) are refused with exit 2 even with the token
// file given, and the daemon is never reached.
func TestStatusIssueUsage(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	d := newControllerDaemon(t)
	t.Setenv("LEGION_DAEMON_URL", d.url)
	tokenFile := writeFile(t, "operator-token", controllerOperatorToken+"\n")
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"LEGSMOKE-3", "done", "--operator-token-file", tokenFile}, `the status is todo, backlog or icebox, not "done"`},
		{[]string{"LEGSMOKE-3", "backlog", "extra", "--operator-token-file", tokenFile}, "name the issue and its status"},
		{[]string{"LEGSMOKE-3", "backlog", "--operator-token-file"}, "flag needs an argument: --operator-token-file"},
		{[]string{"LEGSMOKE-3", "backlog", "--children", "keep", "--operator-token-file", tokenFile}, `--children is include or unlink, not "keep"`},
		{[]string{"LEGSMOKE-3", "todo", "--children", "include", "--operator-token-file", tokenFile}, "--children goes with backlog or icebox, which move an issue out of work"},
		{[]string{"LEGSMOKE-3", "todo", "--parent", "unlink", "--operator-token-file", tokenFile}, `--parent is include, not "unlink"`},
		{[]string{"LEGSMOKE-3", "icebox", "--parent", "include", "--operator-token-file", tokenFile}, "--parent goes with todo, which moves an issue into work"},
	} {
		code, _, errb := issueStatus(t, tc.args...)
		if code != 2 || !strings.Contains(errb, "legion issue status: "+tc.says) {
			t.Errorf("legion issue status %v = %d, stderr %q; want a usage error saying %q", tc.args, code, errb, tc.says)
		}
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon, want none", n)
	}
}
