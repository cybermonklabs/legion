package cireconcile

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of this file: GitHub, or whatever answers in its place, writes a field the pass prints
// - a check run's or a check suite's id, an App's id, a head commit's sha, a status's context, a
// repository's full name - as words that quote a credential's URL, and the line or the note that
// names the field holds none of the credential. Each read is made with the installation token.

// commitSHA is a head commit's sha as GitHub writes one.
const commitSHA = "0123456789abcdef0123456789abcdef01234567"

// remoteStandIn is a GitHub App stand-in for the rows of this file. It is installed on every
// repository and mints a token that holds no Commit statuses; every other request is answered by
// answer. It routes by the escaped path, as GitHub does, since a commit's sha in it may hold an
// escaped '/'.
func remoteStandIn(t *testing.T, answer http.HandlerFunc) *githubapp.Client {
	t.Helper()
	handle := func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
			writeJSON(t, w, http.StatusOK, map[string]any{
				"id": 1, "app_slug": "cireconcile-test",
				"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
			})
		case r.Method == http.MethodPost && path == "/app/installations/1/access_tokens":
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"token":       "ghs_installation_token_for_the_reconcile",
				"expires_at":  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
				"permissions": map[string]string{"checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read"},
			})
		default:
			answer(w, r)
		}
	}
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.cireconcile", PEM: pemText}, githubtest.Server(t, githubtest.Spec{Handle: handle}))
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// writeJSON answers payload with status.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode an answer: %v", err)
	}
}

// failCommitReads answers every read of a commit 500, and anything else as unexpected.
func failCommitReads(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.EscapedPath(), "/repos/acme/web/commits/") {
			githubtest.Unexpected(t, w, r)
			return
		}
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"message": "Server Error"})
	}
}

// logged is a Reconciler of client whose lines are written to the log it answers, as JSON.
func logged(client *githubapp.Client) (*Reconciler, *logtest.Buffer) {
	logs := &logtest.Buffer{}
	return &Reconciler{GitHub: client, Logger: slog.New(slog.NewJSONHandler(logs, nil))}, logs
}

// tokenHead is acme/web's pull request 7 at sha, read with client's installation token.
func tokenHead(t *testing.T, client *githubapp.Client, sha string) pullHead {
	t.Helper()
	token, err := client.RepositoryToken(t.Context(), "acme", "web")
	if err != nil {
		t.Fatalf("the installation token of acme/web: %v", err)
	}
	return pullHead{token: token, scope: client.TokenScope(token), owner: "acme", repo: "web", number: "7", sha: sha}
}

// lineOf is the line of logs whose message begins with msg, and fails t where there is none.
func lineOf(t *testing.T, logs *logtest.Buffer, msg string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var fields struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &fields) == nil && strings.HasPrefix(fields.Msg, msg) {
			return line
		}
	}
	t.Fatalf("no line %q was logged; the log is %q", msg, logs.String())
	return ""
}

// fieldWriter is a CI record writer that keeps the observations it is given and fails each kind
// of write a test names.
type fieldWriter struct {
	mu                                                           sync.Mutex
	runsFail, suiteFails, statusFails, hasRecordFails, doneFails bool
	suites                                                       []contracts.CIObservation
	requests                                                     []contracts.CIVerifyRequest
	done                                                         int
}

var errWriteRefused = errors.New("the CI store refused the write")

func (w *fieldWriter) RecordRuns(context.Context, []contracts.CIObservation) (bool, error) {
	if w.runsFail {
		return false, errWriteRefused
	}
	return true, nil
}

func (w *fieldWriter) RecordSuite(_ context.Context, observation contracts.CIObservation) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.suites = append(w.suites, observation)
	if w.suiteFails {
		return false, errWriteRefused
	}
	return true, nil
}

func (w *fieldWriter) RecordStatus(context.Context, contracts.StatusObservation) (bool, error) {
	if w.statusFails {
		return false, errWriteRefused
	}
	return true, nil
}

func (w *fieldWriter) HasRecord(string, string, string, string) (bool, error) {
	if w.hasRecordFails {
		return false, errWriteRefused
	}
	return true, nil
}

func (w *fieldWriter) VerifyRequests(context.Context) ([]contracts.CIVerifyRequest, error) {
	return w.requests, nil
}

func (w *fieldWriter) MarkVerified(context.Context, contracts.CIHead, uint64, int64) (bool, error) {
	return true, nil
}

func (w *fieldWriter) DoneVerifying(context.Context, contracts.CIHead, uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done++
	if w.doneFails {
		return errWriteRefused
	}
	return nil
}

// modified is one page GitHub answered 200 with, holding items.
func modified(items ...object) []pageOf[object] {
	return []pageOf[object]{{items: items, modified: true}}
}

// TestACheckRunWhoseIdIsNoIdPrintsItOnlyAsAnId: GitHub lists a head's check run under an id that
// is no number but quotes a credential's URL. The run is skipped, and the line that says so
// (contracts.GithubCIObservation) names its id only where it is an id. Captures
// cireconcile.run-id/not-an-id.
func TestACheckRunWhoseIdIsNoIdPrintsItOnlyAsAnId(t *testing.T) {
	std := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewJSONHandler(std, nil))
	client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.EscapedPath(); {
		case strings.HasSuffix(path, "/check-runs"):
			writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 1, "check_runs": []any{map[string]any{
				"id": redacttest.Words, "name": "build", "head_sha": commitSHA, "status": "completed",
				"conclusion": "success", "completed_at": "2026-11-24T00:00:00Z", "app": map[string]any{"id": 15368},
			}}})
		case strings.HasSuffix(path, "/check-suites"):
			writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 0, "check_suites": []any{}})
		default:
			githubtest.Unexpected(t, w, r)
		}
	})
	r, _ := logged(client)
	if at := r.readHead(t.Context(), &fieldWriter{}, tokenHead(t, client, commitSHA), &counts{}); at != nil {
		t.Fatalf("the read of the head stopped at %+v", at)
	}
	line := lineOf(t, std, "github ci check run skipped")
	redacttest.RequireNone(t, "cireconcile.run-id/not-an-id", line)
}

// TestACheckSuiteWhoseIdIsNoIdPrintsItOnlyAsAnId: GitHub lists a head's check suite under an id
// that is no number but quotes a credential's URL. The verify read holds its answer while that
// suite runs (verify-held), and a write of the suite once completed fails (record-failed); each
// line names the suite's id only where it is an id. Captures cireconcile.suite-id/<case>.
func TestACheckSuiteWhoseIdIsNoIdPrintsItOnlyAsAnId(t *testing.T) {
	t.Run("verify-held", func(t *testing.T) {
		client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
			switch path := r.URL.EscapedPath(); {
			case strings.HasSuffix(path, "/check-runs"):
				writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 0, "check_runs": []any{}})
			case strings.HasSuffix(path, "/check-suites"):
				writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 1, "check_suites": []any{map[string]any{
					"id": redacttest.Words, "head_sha": commitSHA, "status": "in_progress",
					"latest_check_runs_count": 1, "app": map[string]any{"id": 15368},
				}}})
			default:
				githubtest.Unexpected(t, w, r)
			}
		})
		r, logs := logged(client)
		writer := &fieldWriter{requests: []contracts.CIVerifyRequest{{
			Head: contracts.CIHead{Owner: "acme", Repo: "web", Number: "7", SHA: commitSHA}, Revision: 3, Episode: 1,
		}}}
		r.Open = func() (Writer, error) { return writer, nil }
		r.Verify(t.Context())
		line := lineOf(t, logs, "ci verify held its answer")
		redacttest.RequireNone(t, "cireconcile.suite-id/verify-held", line)
	})
	t.Run("record-failed", func(t *testing.T) {
		r, logs := logged(nil)
		suite := object{
			"id": redacttest.Words, "head_sha": commitSHA, "status": "completed", "conclusion": "success",
			"updated_at": "2026-11-24T00:00:00Z", "app": map[string]any{"id": float64(15368)},
		}
		r.recordSuites(t.Context(), &fieldWriter{suiteFails: true}, pullHead{owner: "acme", repo: "web", number: "7", sha: commitSHA}, modified(suite))
		line := lineOf(t, logs, "ci reconcile did not record a check suite")
		redacttest.RequireNone(t, "cireconcile.suite-id/record-failed", line)
	})
}

// TestACheckSuiteIsStoredUnderTheIdGitHubGaveIt: a check suite GitHub lists under an id that is no
// number is written to the CI store under that id as it came. A shape is checked only where an id
// prints, so two distinct ids are never stored under one phrase.
func TestACheckSuiteIsStoredUnderTheIdGitHubGaveIt(t *testing.T) {
	r, _ := logged(nil)
	writer := &fieldWriter{}
	suite := object{
		"id": redacttest.Words, "head_sha": commitSHA, "status": "completed", "conclusion": "success",
		"updated_at": "2026-11-24T00:00:00Z", "app": map[string]any{"id": float64(15368)},
	}
	r.recordSuites(t.Context(), writer, pullHead{owner: "acme", repo: "web", number: "7", sha: commitSHA}, modified(suite))
	if len(writer.suites) != 1 || writer.suites[0].SuiteID != redacttest.Words {
		t.Fatalf("the CI store was written %+v; want the one suite under the id GitHub gave it, %q", writer.suites, redacttest.Words)
	}
}

// TestAHeadWhoseShaIsNoShaPrintsNoCredential: GitHub names a head commit by words that are no sha
// but quote a credential's URL. The reconcile's read of that head fails (read), and the verify
// read's answer for it fails and its request is not deleted (verify); each line names the head's
// sha only where it is a sha, and its error holds no credential either. Captures
// cireconcile.head-sha/<case>.
func TestAHeadWhoseShaIsNoShaPrintsNoCredential(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		client := remoteStandIn(t, failCommitReads(t))
		r, logs := logged(client)
		if at := r.readHead(t.Context(), &fieldWriter{}, tokenHead(t, client, redacttest.Words), &counts{}); at != nil {
			t.Fatalf("the read of the head stopped at %+v", at)
		}
		line := lineOf(t, logs, "ci reconcile did not read a head")
		redacttest.RequireNone(t, "cireconcile.head-sha/read", line)
	})
	t.Run("verify", func(t *testing.T) {
		client := remoteStandIn(t, failCommitReads(t))
		r, logs := logged(client)
		writer := &fieldWriter{doneFails: true, requests: []contracts.CIVerifyRequest{{
			Head: contracts.CIHead{Owner: "acme", Repo: "web", Number: "7", SHA: redacttest.Words}, Revision: 3, Episode: 1,
		}}}
		r.Open = func() (Writer, error) { return writer, nil }
		r.Verify(t.Context())
		lineOf(t, logs, "ci verify")
		redacttest.RequireNone(t, "cireconcile.head-sha/verify", logs.String())
	})
}

// TestAVerifyReadOfAHeadWhoseShaIsNoShaGivesItsEpisodeUp: a verify request names a head commit by
// a value that is no full sha (abc1234, a short one). The App client refuses each read of that
// commit before any request (githubapp.ErrNotASHA), and no later read mends that, so the verify
// loop gives the episode up at once: it logs one ERROR line, deletes the request, and deletes a
// later request of the same episode unread, with no read of GitHub for either.
func TestAVerifyReadOfAHeadWhoseShaIsNoShaGivesItsEpisodeUp(t *testing.T) {
	var reads sync.Map
	client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
		reads.Store(r.URL.EscapedPath(), true)
		failCommitReads(t)(w, r)
	})
	r, logs := logged(client)
	writer := &fieldWriter{requests: []contracts.CIVerifyRequest{{
		Head: contracts.CIHead{Owner: "acme", Repo: "web", Number: "7", SHA: "abc1234"}, Revision: 3, Episode: 1,
	}}}
	r.Open = func() (Writer, error) { return writer, nil }
	r.Verify(t.Context())
	lineOf(t, logs, "ci verify gave up an episode")
	writer.requests[0].Revision = 4
	r.Verify(t.Context())
	if writer.done != 2 {
		t.Errorf("the verify loop deleted %d requests of the episode; want both of them", writer.done)
	}
	reads.Range(func(path, _ any) bool {
		t.Errorf("the verify loop read %s of a head whose sha is no full sha", path)
		return true
	})
}

// TestARunningSuitesIdPrintsInDecimalDigits: the line of a verify read held by a running check
// suite names the suite by the id GitHub gave it, a JSON number, in decimal digits, as the CI
// store records an id, whatever its size: a check suite's id is eleven digits today.
func TestARunningSuitesIdPrintsInDecimalDigits(t *testing.T) {
	client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.EscapedPath(); {
		case strings.HasSuffix(path, "/check-runs"):
			writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 0, "check_runs": []any{}})
		case strings.HasSuffix(path, "/check-suites"):
			writeJSON(t, w, http.StatusOK, map[string]any{"total_count": 1, "check_suites": []any{map[string]any{
				"id": int64(31234567890), "head_sha": commitSHA, "status": "in_progress",
				"latest_check_runs_count": 1, "app": map[string]any{"id": 15368},
			}}})
		default:
			githubtest.Unexpected(t, w, r)
		}
	})
	r, logs := logged(client)
	writer := &fieldWriter{requests: []contracts.CIVerifyRequest{{
		Head: contracts.CIHead{Owner: "acme", Repo: "web", Number: "7", SHA: commitSHA}, Revision: 3, Episode: 1,
	}}}
	r.Open = func() (Writer, error) { return writer, nil }
	r.Verify(t.Context())
	if line := lineOf(t, logs, "ci verify held its answer"); !strings.Contains(line, `"suite":"31234567890"`) {
		t.Errorf("the line is %s; want the suite named 31234567890", line)
	}
}

// TestAHeadsLinesNameItsShaOnlyWhereItIsOne: each other line the pass writes about a head names
// its sha only where it is a sha, whatever fails: the read of whether the head still has a CI
// record (has-record), the write of its check runs (runs), of a check suite (suite), and of a
// commit status, whose context GitHub wrote as words that quote a credential's URL too (status),
// its commit statuses unread, in the line and in the note the pass's attempt keeps for GET
// /healthz (statuses-unread), and an answer too large to read (too-large). Captures
// cireconcile.head-lines/<case>.
func TestAHeadsLinesNameItsShaOnlyWhereItIsOne(t *testing.T) {
	head := pullHead{owner: "acme", repo: "web", number: "7", sha: redacttest.Words}
	t.Run("has-record", func(t *testing.T) {
		r, logs := logged(nil)
		r.heads = map[string]headState{head.key(): {observed: true}}
		r.kept(&fieldWriter{hasRecordFails: true}, head)
		redacttest.RequireNone(t, "cireconcile.head-lines/has-record", lineOf(t, logs, "ci reconcile did not read whether a head still has its CI record"))
	})
	t.Run("runs", func(t *testing.T) {
		r, logs := logged(nil)
		run := object{
			"id": float64(41), "name": "build", "head_sha": commitSHA, "status": "completed", "conclusion": "success",
			"completed_at": "2026-11-24T00:00:00Z", "app": map[string]any{"id": float64(15368)},
		}
		r.recordRuns(t.Context(), &fieldWriter{runsFail: true}, head, modified(run))
		redacttest.RequireNone(t, "cireconcile.head-lines/runs", lineOf(t, logs, "ci reconcile did not record the check runs of a head"))
	})
	t.Run("suite", func(t *testing.T) {
		r, logs := logged(nil)
		suite := object{
			"id": float64(42), "head_sha": commitSHA, "status": "completed", "conclusion": "success",
			"updated_at": "2026-11-24T00:00:00Z", "app": map[string]any{"id": float64(15368)},
		}
		r.recordSuites(t.Context(), &fieldWriter{suiteFails: true}, head, modified(suite))
		redacttest.RequireNone(t, "cireconcile.head-lines/suite", lineOf(t, logs, "ci reconcile did not record a check suite"))
	})
	t.Run("status", func(t *testing.T) {
		r, logs := logged(nil)
		status := object{"context": redacttest.Words, "state": "success", "created_at": "2026-11-24T00:00:00Z"}
		r.recordStatuses(t.Context(), &fieldWriter{statusFails: true}, head, modified(status))
		redacttest.RequireNone(t, "cireconcile.head-lines/status", lineOf(t, logs, "ci reconcile did not record a commit status"))
	})
	t.Run("statuses-unread", func(t *testing.T) {
		client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
			// GitHub's answer to a read of statuses the token may not make.
			writeJSON(t, w, http.StatusForbidden, map[string]any{"message": "Resource not accessible by integration"})
		})
		r, logs := logged(client)
		ctx, attempt := githubreads.Begin(t.Context())
		r.readStatuses(ctx, &fieldWriter{}, tokenHead(t, client, redacttest.Words), nil, false, &counts{})
		_, _, cause := attempt.Outcome()
		redacttest.RequireNone(t, "cireconcile.head-lines/statuses-unread", lineOf(t, logs, "ci reconcile read no commit status of a head")+"\n"+cause)
	})
	t.Run("too-large", func(t *testing.T) {
		r, logs := logged(nil)
		r.headFailed(t.Context(), &githubapp.ResponseTooLargeError{Limit: 1 << 20}, head, &counts{})
		redacttest.RequireNone(t, "cireconcile.head-lines/too-large", lineOf(t, logs, "ci reconcile skipped a head whose answer from GitHub was too large"))
	})
}

// TestAVerdictChecksAppIdPrintsOnlyAsAnId: GitHub names the App of a verdict check by an id that
// is no number but quotes a credential's URL, and the review App's rule drops the check, with no
// review App configured (no-reviewer-app) and with another one (untrusted). Each line names the
// App's id only where it is an id. Captures cireconcile.verdict-app/<case>.
func TestAVerdictChecksAppIdPrintsOnlyAsAnId(t *testing.T) {
	for _, c := range []struct{ name, reviewer, msg string }{
		{"no-reviewer-app", "", "ci reconcile dropped a verdict check"},
		{"untrusted", "15368", "ci reconcile ignored a verdict check another App created"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, logs := logged(nil)
			r.ReviewerAppID = c.reviewer
			observation := contracts.CIObservation{Owner: "acme", Repo: "web", Number: "7", SHA: commitSHA, CheckName: "tester", AppID: redacttest.Words}
			if r.recordVerdict(observation, pullHead{owner: "acme", repo: "web", number: "7", sha: commitSHA}) {
				t.Fatal("the rule recorded a verdict check the review App did not create")
			}
			redacttest.RequireNone(t, "cireconcile.verdict-app/"+c.name, lineOf(t, logs, c.msg))
		})
	}
}

// TestARepositorysFullNamePrintsThroughTheRule: GitHub answers a covered repository's identity
// with a full name that quotes a credential's URL, with no id (no-id), and with an id, a name that
// is no owner/name (not-owner-name). The line that skips the repository and the note the pass's
// attempt keeps for GET /healthz name the full name only as the rule reads it. Captures
// cireconcile.full-name/<case>.
func TestARepositorysFullNamePrintsThroughTheRule(t *testing.T) {
	for _, c := range []struct {
		name, msg string
		id        int64
	}{
		{"no-id", "ci reconcile skipped a repository GitHub did not answer for", 0},
		{"not-owner-name", "ci reconcile skipped a repository GitHub named oddly", 42},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := remoteStandIn(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != "/repos/acme/web" {
					githubtest.Unexpected(t, w, r)
					return
				}
				writeJSON(t, w, http.StatusOK, map[string]any{"full_name": redacttest.Words, "id": c.id})
			})
			r, logs := logged(client)
			ctx, attempt := githubreads.Begin(t.Context())
			if _, at := r.resolve(ctx, "acme/web", true, &counts{}, map[int64]struct{}{}); at != nil {
				t.Fatalf("the resolve stopped at %+v", at)
			}
			_, _, cause := attempt.Outcome()
			redacttest.RequireNone(t, "cireconcile.full-name/"+c.name, lineOf(t, logs, c.msg)+"\n"+cause)
		})
	}
}
