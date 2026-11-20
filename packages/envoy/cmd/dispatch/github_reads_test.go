package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// gitHubReadsSigningKey is DISPATCH_SIGNING_KEY for both processes of the check below, which
// serve one database at once: a process refuses to join a live peer whose key set it does not
// share (registry.go).
const gitHubReadsSigningKey = "github-reads-signing-key"

// The check c-health-github (INFRA-4324): the real envoy-dispatch binary, on a database and a NATS
// of its own and a GitHub App whose every call goes to a loopback GitHub, names the webhook
// redelivery sweep's newest attempt in /healthz's github member. While GitHub answers the sweep's
// listing 503 with a body of about 4 KiB, the entry reads failed, with its time and its cause's
// class, "status 503": /healthz answers whoever reaches the port, and a cause's text can name a
// repository (gitHubReadCauseClass). The record (github_read_attempts) holds the first failure
// itself: the client's own text of that answer, cut to 1,024 bytes on a rune boundary though a
// three-byte character straddles byte 1,024 of it, and ended with how long it was. The probe still
// answers 200 with ok true, and the three reads that sent nothing (no architecture source, no
// delivery settings, no mapped repository) read no_attempt. Once GitHub answers the listing, the
// entry reads succeeded, with no cause and a later time. A process of the same binary on the same
// database with no GitHub App names no read: github is null.
//
// The first sweep can meet NATS still connecting and record nothing, so each wait polls. Each
// wait's failure prints the last status and body /healthz answered, which is what the check's
// negative control reads: a build without the member answers 200, ok true and no github at all.
func TestHealthzNamesAFailedGitHubReadAndItsCause(t *testing.T) {
	database := storetest.Open(t)
	_, natsURL := testnats.Start(t)
	// failure is the body GitHub answers the webhook's deliveries listing with, with a 503; nil
	// answers it with no deliveries.
	var failure atomic.Pointer[string]
	github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries" {
			if body := failure.Load(); body != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, *body)
				return
			}
			_, _ = io.WriteString(w, "[]")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
	// The failure the client notes is "<METHOD> <target>: status <n>: <body>" (githubapp's
	// failedAnswer). The body is padded so that a three-byte character starts at byte 1,023 of
	// that text, and is three-byte characters from there on, so one straddles byte 1,024 and one
	// straddles the cut inside it, wherever the marker's length puts the cut.
	prefix := "GET " + github.URL + "/app/hook/deliveries?per_page=100: status 503: "
	pad := strings.Repeat("x", (1023-len(prefix))%3)
	body := pad + strings.Repeat("€", (4096-len(pad))/3)
	if uncut := prefix + body; utf8.RuneStart(uncut[1024]) {
		t.Fatalf("byte 1,024 of the failure's text starts a character; want one straddling it")
	}
	failure.Store(&body)
	marker := " … (cut from " + strconv.Itoa(len(prefix)+len(body)) + " bytes)"

	env := githubAppEnv(t, github.URL)
	env["DISPATCH_TEST_HOOKS"] = "1"
	// Two minutes is longer than any test may wait for a second sweep.
	env["DISPATCH_TEST_REDELIVERY_INTERVAL"] = "1s"
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL, SigningKey: gitHubReadsSigningKey, Env: env,
	})
	process.waitHealthy(t)

	failed := process.waitForGitHubReads(t, func(answer healthzAnswer) string {
		return failedOn503(answer, githubreads.WebhookRedelivery)
	})
	t.Logf("/healthz with the sweep failed: %d %s", failed.status, failed.body)
	if failed.status != http.StatusOK || !failed.OK {
		t.Fatalf("/healthz with a failed GitHub read = %d %s, want 200 with ok true", failed.status, failed.body)
	}
	failedAt := attemptTime(t, failed, githubreads.WebhookRedelivery)
	if class := *failed.reads[string(githubreads.WebhookRedelivery)].Cause; class != "status 503" {
		t.Fatalf("/healthz names the failed sweep's cause %q, want its class \"status 503\" alone: %s", class, failed.body)
	}
	cause := recordedCause(t, database, githubreads.WebhookRedelivery)
	if len(cause) > githubreads.CauseLimit || !utf8.ValidString(cause) || !strings.HasPrefix(cause, prefix) || !strings.HasSuffix(cause, marker) {
		t.Fatalf("the recorded cause %q is %d bytes, valid UTF-8 %t; want at most %d bytes of valid UTF-8 starting %q and ending %q",
			cause, len(cause), utf8.ValidString(cause), githubreads.CauseLimit, prefix, marker)
	}
	for _, read := range []githubreads.Read{githubreads.ArchitectureImport, githubreads.DeliveryReconcile, githubreads.CIReconcile} {
		if entry := failed.reads[string(read)]; entry.State != "no_attempt" || entry.At != nil || entry.Cause != nil {
			t.Errorf("%s of a read that sent nothing is not no_attempt with at and cause null: %s", read, failed.body)
		}
	}

	failure.Store(nil)
	succeeded := process.waitForGitHubReads(t, func(answer healthzAnswer) string {
		entry := answer.reads[string(githubreads.WebhookRedelivery)]
		if entry.State != "succeeded" || entry.At == nil || entry.Cause != nil {
			return fmt.Sprintf("%s is %q, not succeeded with cause null", githubreads.WebhookRedelivery, entry.State)
		}
		if at, err := time.Parse(time.RFC3339Nano, *entry.At); err != nil || !at.After(failedAt) {
			return fmt.Sprintf("%s succeeded at %s, not after the failed attempt at %s", githubreads.WebhookRedelivery, *entry.At, failedAt.Format(time.RFC3339Nano))
		}
		return ""
	})
	t.Logf("/healthz with the sweep succeeded: %d %s", succeeded.status, succeeded.body)
	if succeeded.status != http.StatusOK || !succeeded.OK {
		t.Fatalf("/healthz with every GitHub read answered = %d %s, want 200 with ok true", succeeded.status, succeeded.body)
	}

	withoutApp := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL, SigningKey: gitHubReadsSigningKey,
	})
	withoutApp.waitHealthy(t)
	answer := withoutApp.readHealthz(t)
	t.Logf("/healthz of the process with no GitHub App: %d %s", answer.status, answer.body)
	if answer.status != http.StatusOK || !answer.OK || string(answer.GitHub) != "null" {
		t.Fatalf("/healthz of a process with no GitHub App = %d %s, want 200 with ok true and github null", answer.status, answer.body)
	}
}

// main.go gives each of the four loops that read GitHub under a lease its recorder, and each its
// own read's name: the real binary, with NATS, the App's settings and a loopback GitHub that
// answers every request 503, and a database holding what makes each loop send a request (a mapped
// repository and the CI state bucket, the delivery settings, and one enabled architecture
// source), names all four failed with the class "status 503", and records each with a cause that
// names a request of its own loop. A loop left without its recorder leaves its entry no_attempt,
// and two loops given each other's names put a cause under the wrong key; either fails naming the
// entry.
//
// The test settings bring every loop's pass inside the test's window: the sweep's and the CI
// reconcile's intervals, and a first pass at start for the delivery reconcile and the
// architecture import, whose first ticks are otherwise jittered over five minutes.
func TestHealthzNamesEachGitHubReadThroughTheServersWiring(t *testing.T) {
	database := storetest.Open(t)
	mapCIReconcileRepository(t, database)
	seedDeliverySettings(t, database)
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee) values ('ARCH', 'ARCH', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('ARCH', 'acme/arch', 'main', 1, '{"kind":"session","id":"test"}');
	`); err != nil {
		t.Fatalf("seed the architecture source: %v", err)
	}
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
	github := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"Service Unavailable"}`)
	}})

	env := githubAppEnv(t, github.URL)
	env["DISPATCH_TEST_HOOKS"] = "1"
	env["DISPATCH_TEST_REDELIVERY_INTERVAL"] = "1s"
	env["DISPATCH_TEST_CI_RECONCILE_INTERVAL"] = "1s"
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	env["DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START"] = "1"
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{NATSURL: natsURL, Env: env})
	process.waitHealthy(t)

	answer := process.waitForGitHubReads(t, func(answer healthzAnswer) string {
		var missing []string
		for _, read := range githubreads.Reads() {
			if lack := failedOn503(answer, read); lack != "" {
				missing = append(missing, lack)
			}
		}
		return strings.Join(missing, "; ")
	})
	t.Logf("/healthz with every GitHub read failed: %d %s", answer.status, answer.body)
	if answer.status != http.StatusOK || !answer.OK {
		t.Fatalf("/healthz with every GitHub read failed = %d %s, want 200 with ok true", answer.status, answer.body)
	}
	// What a request of each loop names: the sweep's listing, the CI reconcile's mapped repository
	// (GitHub-cased where GitHub named it), the delivery settings' deploy repository, and the
	// architecture source's repository or its installation's token mint, whichever the import's
	// resolve sends first.
	names := map[githubreads.Read][]string{
		githubreads.WebhookRedelivery:  {"/app/hook/deliveries"},
		githubreads.ArchitectureImport: {"acme/arch", "/app/installations/1/access_tokens"},
		githubreads.DeliveryReconcile:  {"acme/deploy"},
		githubreads.CIReconcile:        {ciReconcileRow},
	}
	for _, read := range githubreads.Reads() {
		if class := *answer.reads[string(read)].Cause; class != "status 503" {
			t.Errorf("/healthz names %s's cause %q, want its class \"status 503\" alone: %s", read, class, answer.body)
		}
		cause := recordedCause(t, database, read)
		if !slices.ContainsFunc(names[read], func(name string) bool { return strings.Contains(strings.ToLower(cause), strings.ToLower(name)) }) {
			t.Errorf("%s's recorded cause names no request of its own loop, want one naming any of %q: %s", read, names[read], cause)
		}
	}
}

// recordedCause is the cause github_read_attempts holds for read's newest attempt, which the
// operator reads there: /healthz names only its class.
func recordedCause(t *testing.T, database *store.Store, read githubreads.Read) string {
	t.Helper()
	var cause *string
	if err := database.Pool.QueryRow(t.Context(), `select cause from github_read_attempts where read = $1`, string(read)).Scan(&cause); err != nil {
		t.Fatalf("read %s's recorded cause: %v", read, err)
	}
	if cause == nil {
		t.Fatalf("github_read_attempts holds no cause for %s: its newest attempt succeeded", read)
	}
	return *cause
}

// healthzAnswer is one /healthz answer as the GitHub read tests read it: its status and its body
// as written, which every failure prints; ok; github as written, nil when absent and `null` when
// null; and github's entries by read, where it is an object.
type healthzAnswer struct {
	status int
	body   string
	OK     bool            `json:"ok"`
	GitHub json.RawMessage `json:"github"`
	reads  map[string]gitHubReadAnswer
}

// gitHubReadAnswer is one entry of github as written.
type gitHubReadAnswer struct {
	State string  `json:"state"`
	At    *string `json:"at"`
	Cause *string `json:"cause"`
}

// readHealthz reads the process's /healthz once, bounded so a probe that hangs fails the test.
func (p *dispatchProcess) readHealthz(t *testing.T) healthzAnswer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url("/healthz"), nil)
	if err != nil {
		t.Fatalf("build the /healthz request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	answer := healthzAnswer{status: response.StatusCode, body: string(body)}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decode /healthz %d %s: %v", answer.status, body, err)
	}
	if len(answer.GitHub) > 0 && string(answer.GitHub) != "null" {
		if err := json.Unmarshal(answer.GitHub, &answer.reads); err != nil {
			t.Fatalf("decode /healthz's github %s: %v", answer.GitHub, err)
		}
	}
	return answer
}

// waitForGitHubReads polls /healthz until missing finds nothing lacking in an answer ("") and
// returns that answer. Once its bound passes it fails the test with what missing last found
// lacking and the last status and body /healthz answered, then the process's output.
func (p *dispatchProcess) waitForGitHubReads(t *testing.T, missing func(healthzAnswer) string) healthzAnswer {
	t.Helper()
	bound := testwait.Bounded(t, 2*time.Minute, 5*time.Second)
	var (
		last healthzAnswer
		lack string
	)
	if !testwait.Holds(bound, 200*time.Millisecond, func() bool {
		last = p.readHealthz(t)
		lack = missing(last)
		return lack == ""
	}) {
		t.Fatalf("/healthz did not report it within %v: %s; last %d %s\n%s", bound, lack, last.status, last.body, p.Output.String())
	}
	return last
}

// failedOn503 is what keeps read's entry in answer from being a failed attempt whose cause holds
// GitHub's 503, "" when nothing does.
func failedOn503(answer healthzAnswer, read githubreads.Read) string {
	entry := answer.reads[string(read)]
	if entry.State == "failed" && entry.Cause != nil && strings.Contains(*entry.Cause, "status 503") {
		return ""
	}
	return fmt.Sprintf("%s is %q, not failed with a cause holding status 503", read, entry.State)
}

// attemptTime is the time read's entry in answer names, which must be RFC 3339 in UTC.
func attemptTime(t *testing.T, answer healthzAnswer, read githubreads.Read) time.Time {
	t.Helper()
	entry := answer.reads[string(read)]
	if entry.At == nil {
		t.Fatalf("%s names no time: %s", read, answer.body)
	}
	at, err := time.Parse(time.RFC3339Nano, *entry.At)
	if err != nil || !strings.HasSuffix(*entry.At, "Z") {
		t.Fatalf("%s's at %q is not RFC 3339 in UTC (%v): %s", read, *entry.At, err, answer.body)
	}
	return at
}
