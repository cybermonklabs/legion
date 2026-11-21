// Package githubapi answers the web app's GitHub reads: a pull request's or issue's state, and a
// commit's checks and the workflow runs that made them, read as the GitHub App installation that
// covers the repository.
package githubapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubpath"
	"github.com/sjawhar/envoy/internal/dispatch/heldpath"
)

// workflowRunsQuery is each parameter a workflow-runs read (githubpath.Read.WorkflowRuns) may
// carry, as sent: the commit's full sha and the page. The read must name head_sha, and no
// parameter may repeat.
var workflowRunsQuery = regexp.MustCompile(`^(head_sha)=[0-9A-Fa-f]{40}$|^(per_page|page)=[0-9]+$`)

// workflowRunsResponseLimit bounds GitHub's workflow-runs answer, which the proxy trims to three
// fields per run before forwarding. GitHub writes each run's repository, head repository and head
// commit (about 12.8 KB a run on cybermonklabs/legion), so a page of 100 runs passes the client's 1 MiB
// default; this leaves room for 100 runs whose shared head commit message is long.
const workflowRunsResponseLimit = 16 << 20

// workflowRuns is the part of GitHub's workflow-runs answer the web app reads, and all of it the
// proxy passes on: the count and, per run in GitHub's order, its check suite, triggering event and
// workflow name. The rest of a run (its head commit's message and author, the actor, the
// repository) is never shown, so it never leaves the server.
type workflowRuns struct {
	TotalCount   int `json:"total_count"`
	WorkflowRuns []struct {
		CheckSuiteID int64  `json:"check_suite_id"`
		Event        string `json:"event"`
		Name         string `json:"name"`
	} `json:"workflow_runs"`
}

// workflowRunsOfOneCommit reports whether rawQuery reads the workflow runs of one commit and
// nothing else: head_sha as a full sha, and at most per_page and page beside it.
func workflowRunsOfOneCommit(rawQuery string) bool {
	seen := map[string]bool{}
	for _, parameter := range strings.Split(rawQuery, "&") {
		match := workflowRunsQuery.FindStringSubmatch(parameter)
		if match == nil {
			return false
		}
		name := match[1] + match[2]
		if seen[name] {
			return false
		}
		seen[name] = true
	}
	return seen["head_sha"]
}

// ProxyREST answers GET githubpath.Prefix<path> with GitHub's answer to GET /<path>, read with an
// installation token of the App installation covering the path's repository. A workflow-runs read
// GitHub answers 200 is trimmed to workflowRuns, and a body that does not decode as workflow runs
// is 502 GITHUB_UPSTREAM; every other answer, and a workflow-runs answer of any other status,
// passes through as GitHub sent it, but a rate limit. Any other method is 405; a path the web app
// does not read, or a workflow-runs read whose query is not one commit's head_sha and its page, is
// 404 GITHUB_PATH_REFUSED; a repository the App is not installed on, or a deployment with no App
// key, is 503 GITHUB_TOKEN_UNAVAILABLE. A rate limit is 503 GITHUB_RATE_LIMITED with Retry-After:
// GitHub's answer to the token mint or to the read, or a limit another request or process already
// met (githubapp.Limits), in which case GitHub is not asked. The caller has already established
// that a person is asking.
func ProxyREST(w http.ResponseWriter, r *http.Request, client *githubapp.Client) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		api.WriteJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "the GitHub proxy reads only: use GET", "code": "METHOD_NOT_ALLOWED"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, githubpath.Prefix)
	match, reads := githubpath.Match(path)
	if !reads || (match.WorkflowRuns && !workflowRunsOfOneCommit(r.URL.RawQuery)) {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{"error": "the GitHub proxy reads a pull request, an issue, a commit's check runs or a commit's workflow runs only", "code": "GITHUB_PATH_REFUSED"})
		return
	}
	owner, repo := match.Owner, match.Repo
	token, err := client.RepositoryToken(r.Context(), owner, repo)
	if limited, ok := githubapp.AsRateLimit(err); ok {
		rateLimited(w, limited)
		return
	}
	switch {
	case errors.Is(err, githubapp.ErrNoAppKey):
		unavailable(w, "the GitHub App is not configured")
		return
	case errors.Is(err, githubapp.ErrNoInstallation):
		unavailable(w, "the GitHub App is not installed on "+owner+"/"+repo)
		return
	case err != nil:
		slog.Warn("dispatch: GitHub proxy could not mint an installation token", "repository", owner+"/"+repo, "error", err)
		api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub installation token: " + err.Error(), "code": "GITHUB_UPSTREAM"})
		return
	}
	target := "/" + path
	// held is the read as a warning writes it while heldpath holds names: the query a caller
	// wrote, which reaches GitHub, is left out, and a workflow-runs read's is kept, since
	// workflowRunsOfOneCommit holds it to a commit and its page.
	held := target
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
		if match.WorkflowRuns {
			held = target
		}
	}
	read := client.Read
	if match.WorkflowRuns {
		read = func(ctx context.Context, token, path string) ([]byte, int, http.Header, error) {
			return client.ReadWithin(ctx, token, path, workflowRunsResponseLimit)
		}
	}
	body, status, header, err := read(r.Context(), token, target)
	if limited, ok := githubapp.AsRateLimit(err); ok {
		rateLimited(w, limited)
		return
	}
	if err != nil {
		warnReadFailed(target, held, err)
		var tooLarge *githubapp.ResponseTooLargeError
		if errors.As(err, &tooLarge) {
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": tooLarge.Error(), "code": "GITHUB_UPSTREAM"})
			return
		}
		api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub read failed", "code": "GITHUB_UPSTREAM"})
		return
	}
	if limited := githubapp.RateLimit(status, header, body); limited != nil {
		rateLimited(w, limited)
		return
	}
	if match.WorkflowRuns && status == http.StatusOK {
		var runs workflowRuns
		if err := json.Unmarshal(body, &runs); err != nil {
			warnUndecodable(target, held, err)
			api.WriteJSON(w, http.StatusBadGateway, map[string]string{"error": "GitHub answered the workflow runs with a body that does not decode", "code": "GITHUB_UPSTREAM"})
			return
		}
		api.WriteJSON(w, http.StatusOK, runs)
		return
	}
	if contentType := header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// warnReadFailed logs a read of target that failed with err. While heldpath holds names (a
// project is private, or the private projects are not known) it writes held, which carries no
// query a caller wrote, and in place of err, whose text names the whole target
// (githubapp.SendError), what kind of failure it was: the sender's stage, else other; whether
// the client's deadline or the caller's cancel ended it; and, past a limit, the limit. A failed
// read has no status, so it writes none.
func warnReadFailed(target, held string, err error) {
	const message = "dispatch: GitHub proxy read failed"
	if !heldpath.Holding() {
		slog.Warn(message, "path", target, "error", err)
		return
	}
	kind := "other"
	var sent *githubapp.SendError
	if errors.As(err, &sent) {
		kind = sent.Stage
	}
	attrs := []any{"path", held, "error_kind", kind,
		"timeout", errors.Is(err, context.DeadlineExceeded), "canceled", errors.Is(err, context.Canceled)}
	var tooLarge *githubapp.ResponseTooLargeError
	if errors.As(err, &tooLarge) {
		attrs = append(attrs, "limit", tooLarge.Limit)
	}
	slog.Warn(message, attrs...)
}

// warnUndecodable logs workflow runs GitHub answered 200 whose body did not decode. While
// heldpath holds names it writes held and, in place of err, which can quote the body, the kind of
// decode error: syntax, type, or other.
func warnUndecodable(target, held string, err error) {
	const message = "dispatch: GitHub proxy could not decode the workflow runs"
	if !heldpath.Holding() {
		slog.Warn(message, "path", target, "error", err)
		return
	}
	decodeError := "other"
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		decodeError = "syntax"
	case errors.As(err, &typed):
		decodeError = "type"
	}
	slog.Warn(message, "path", held, "status", http.StatusOK, "error_kind", "decode", "decode_error", decodeError)
}

func unavailable(w http.ResponseWriter, message string) {
	api.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": message, "code": "GITHUB_TOKEN_UNAVAILABLE"})
}

// rateLimited answers a GitHub rate limit: 503 GITHUB_RATE_LIMITED, Retry-After the whole seconds
// left of GitHub's wait (at least one), so a client that honours the header waits as GitHub asks,
// and the code tells any client the refusal is a rate limit rather than an outage.
func rateLimited(w http.ResponseWriter, limited *githubapp.RateLimitError) {
	seconds := max(int64(math.Ceil(limited.Wait.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	api.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "GitHub rate-limited the App; retry after " + (time.Duration(seconds) * time.Second).String(),
		"code":  "GITHUB_RATE_LIMITED",
	})
}
