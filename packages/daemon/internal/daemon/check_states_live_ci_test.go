//go:build e2e

package daemon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// statesCIFailureTold is the daemon's line where it queues the implementer's message of a failed
// CI job (workflow's tellCIFailure), written from that message's row: its pull request, check,
// head and job's case.
const statesCIFailureTold = "workflow: the implementer is told of a failed CI job"

// dispatchGitHub starts the stand-in of GitHub that the run's Dispatch reads through a throwaway
// GitHub App, as packages/dispatch/e2e/run-server.sh gives Dispatch one, and answers the settings
// that point Dispatch at it: the App's client id, a key made for this run alone, and the stand-in's
// loopback base. The stand-in serves the installation lookup and the token mint every installation
// token needs, and the check runs of a commit: at r.ciFailedHead, statesCIJob completed with the
// conclusion failure, and at every other head none. Every other path answers GitHub's 404, so a read
// the case did not foresee reaches no GitHub and decides nothing.
func (r *statesProof) dispatchGitHub() []string {
	t := r.t
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("make the throwaway App's key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	write := func(w http.ResponseWriter, status int, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode the stand-in's answer: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
			write(w, http.StatusOK, map[string]any{"id": 1, "app_slug": "checkstates",
				"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"}})
			return
		case req.Method == http.MethodPost && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
			write(w, http.StatusCreated, map[string]any{"token": "ghs_checkstates", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
			return
		case req.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/check-runs"):
			parts := strings.Split(path, "/")
			sha := parts[len(parts)-2]
			runs := []map[string]any{}
			if failed := r.ciFailedHead.Load(); failed != nil && *failed == sha {
				runs = append(runs, map[string]any{"id": 7001, "name": statesCIJob, "head_sha": sha, "status": "completed",
					"conclusion": "failure", "completed_at": time.Now().UTC().Format(time.RFC3339), "app": map[string]any{"slug": "github-actions"}})
			}
			write(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	return []string{"DISPATCH_APP_CLIENT_ID=Iv1.checkstates", "DISPATCH_APP_PEM_B64=" + pemText, "DISPATCH_GITHUB_API_BASE=" + base}
}

// ci-failure-tells: k3 of the issue is a check whose ci_check names statesCIJob. With its pull
// request staged in Dispatch at its head and the issue in testing, the case publishes the CI
// settlement of that head (notifications.github.<repo>.pr.<n>.checks) on the run's own broker, the
// job failed. Dispatch's handler of a settlement marks the issue owed, its settle reads the job's
// run at the head from the stand-in of GitHub (completed, failure), and it publishes k3's state,
// no for fail with the CI case failure. Within 10 seconds of the publish the daemon queues the
// implementer's one message of that failure, whose row names the pull request, k3, the head and the
// case failure, as its line written from that row says; it starts no implementer and gives it no
// task, and GET /legion/v1/state shows the issue still in testing. The outbox finishes the row once
// it has run it, so the case reads the line the daemon writes from the row as the row commits; a
// read that finds no line says UNKNOWN and what it could not read.
func (r *statesProof) ciFailureTells() {
	issue := r.issues["ci-failure-tells"][0]
	r.readyTester(issue)
	r.stageHead(issue)
	implementer := r.token(issue.key, claim.RoleImplementer)
	published := time.Now()
	r.publishSettlementOf(issue.number, issue.head, 1, []string{"build"}, []string{statesCIJob})
	r.note("published the CI settlement of #%d at %s: build passed, %s failed", issue.number, issue.head, statesCIJob)
	var told *loggedRecord
	for deadline := published.Add(statesLate); told == nil && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		told = r.lastLog(func(l loggedRecord) bool { return l.Message == statesCIFailureTold && l.Attrs["issue"] == issue.key })
	}
	if told == nil {
		held := r.lastLog(func(l loggedRecord) bool {
			return l.Message == statesHeld && l.Attrs["issue"] == issue.key && l.Attrs["check"] == "k3"
		})
		r.fail("told", "UNKNOWN: the daemon's log holds no %q line for %s %s after the settlement (the newest state of k3 it holds: %v), so no message to the implementer can be read",
			statesCIFailureTold, issue.key, statesLate, held)
	}
	if took := told.at.Sub(published); took < 0 || took > statesWithin {
		r.fail("told-within", "the daemon queued the implementer's message of %s at %s, %s after the settlement, outside the bound of %s",
			issue.key, told.at.UTC().Format(time.RFC3339Nano), took.Round(time.Millisecond), statesWithin)
	}
	if told.Attrs["check"] != "k3" || told.Attrs["head"] != issue.head || told.Attrs["ci_case"] != "failure" || told.Attrs["pull_request"] != float64(issue.number) {
		r.fail("message-fields", "the implementer's message of %s names %v, want k3, the head %s, the case failure and #%d", issue.key, told.Attrs, issue.head, issue.number)
	}
	if lines := r.logs(func(l loggedRecord) bool { return l.Message == statesCIFailureTold && l.Attrs["issue"] == issue.key }); len(lines) != 1 {
		r.fail("message-once", "the daemon queued %d messages of a failed CI job for %s, want one", len(lines), issue.key)
	}
	r.note("read back: the daemon queued %s's implementer (%s) one message, %s after the settlement: check %v, head %v, case %v, #%v",
		issue.key, implementer, told.at.Sub(published).Round(time.Millisecond), told.Attrs["check"], told.Attrs["head"], told.Attrs["ci_case"], told.Attrs["pull_request"])
	if got := r.phaseOf(issue, "stays-testing"); got != phase.Testing {
		r.fail("stays-testing", "GET /legion/v1/state shows %s in %s after the failed CI job, want %s: the message moves nothing", issue.key, got, phase.Testing)
	}
	r.noImplementerTask(issue)
	r.note("read back: %s stays in %s, and its implementer was given no task and not started", issue.key, phase.Testing)
}
