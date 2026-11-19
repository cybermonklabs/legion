package githubapp

// commits.go holds this package's reads about one repository's commits: whether the repository
// holds a commit at all, and the reads a recorded check result is placed by — whether a
// commit sits on a branch, a repository's default branch, and the conditional reads of an open
// pull request's head a background pass fills Envoy's listener records in from (its check runs,
// its check suites, its commit statuses, the repository's open pull requests and the repository's
// own identity).
// Each answers "Dispatch cannot see this repository" through CheckRepositoryResponse, the one
// rule that says what unreadable means — delivery's own read of the pull requests GitHub names
// for a commit (github_prs.go, through readGitHubPage's bounded retry) classifies its answer by
// it too.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/excerpt"
	"github.com/sjawhar/envoy/internal/redact"
)

// readRepositoryJSON performs one GET of path (an API path under the API origin) with an
// installation token, through Client.Read, and answers the body of a read of repoFull whose
// answer is JSON, classified by CheckRepositoryResponse: a status the caller names in handled
// comes back as its own status for the caller to read, any other failure as the error that rule
// names.
func (c *Client) readRepositoryJSON(ctx context.Context, token, path, repoFull string, handled ...int) ([]byte, int, error) {
	body, status, header, err := c.Read(ctx, token, path)
	if err != nil {
		return nil, status, err
	}
	if err := CheckRepositoryResponse(status, header, body, token, repoFull, c.base+path, handled...); err != nil {
		return nil, status, err
	}
	return body, status, nil
}

// pollResponseLimit bounds the body of the conditional reads below, in place of the generic
// responseLimit (1 MiB), which a head with many check runs would fail at every pass, for ever.
// A page is 100 objects, and a check run carries both an output.summary and an output.text,
// each of which GitHub caps at 65,535 characters: a full page at both caps is about 13 MB of
// text before the JSON around it. Sixteen MiB is that bound with room over it, derived from
// GitHub's own caps and a full page — not measured against any repository. A head whose answer
// is past it is no failure of the pass that reads it: the pass skips that head, counts it and
// logs it (cireconcile's headFailed). The other reads are far smaller (a page of 100 pull
// requests is well under it) and share it, since one bound is one thing to reason about.
const pollResponseLimit = 16 << 20

// objectsPerPage is the page size the polled reads ask GitHub for, its own maximum: a head with
// few checks is then one request, and a page holding exactly this many says another page may
// follow.
const objectsPerPage = 100

// readConditionalRepositoryJSON is readRepositoryJSON made conditional: it sends the ETag the
// caller remembered from this path's last answer and returns the answer's own ETag beside the
// body, with 304 Not Modified handled, since the caller reads that status itself. It reads
// within pollResponseLimit rather than responseLimit.
func (c *Client) readConditionalRepositoryJSON(ctx context.Context, token, path, repoFull, etag string) ([]byte, int, string, error) {
	body, status, header, err := c.ReadConditionalWithin(ctx, token, path, etag, pollResponseLimit)
	if err != nil {
		return nil, status, "", err
	}
	if err := CheckRepositoryResponse(status, header, body, token, repoFull, c.base+path, http.StatusNotModified); err != nil {
		return nil, status, "", err
	}
	// GitHub repeats the ETag on a 304; when it does not, the empty value travels to the caller,
	// which decides whether to keep the one it had.
	return body, status, header.Get("ETag"), nil
}

// CheckRepositoryResponse classifies one repository read's answer. A 200, or a status the caller
// names in handled and reads itself, is no error. A 403 or 404 is ErrRepositoryUnreadable — the
// installation may not read this resource, or cannot see the repository at all — which the
// results read takes as unknown, never as a commit it placed elsewhere. A rate limit behind
// either status is the *RateLimitError instead. Any other status is the error CheckTokenResponse
// makes, named by the target. token is the installation token the read carried, which no error
// prints (BodyText); repoFull is the `owner/repo` the error names.
func CheckRepositoryResponse(status int, header http.Header, body []byte, token, repoFull, target string, handled ...int) error {
	if status == http.StatusOK || slices.Contains(handled, status) {
		return nil
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		if limited := rateLimit(status, header, body, token); limited != nil {
			return limited
		}
		return fmt.Errorf("%w: %s", ErrRepositoryUnreadable, repoFull)
	}
	if err := CheckTokenResponse(status, header, body, token); err != nil {
		return fmt.Errorf("GET %s: %w", target, err)
	}
	return nil
}

// CommitOnBranch answers whether sha is an ancestor of branch's head, or is that head
// (`GET /repos/{owner}/{repo}/compare/{branch}...{sha}`, whose `status` is `behind` when sha is
// behind the branch — that is, the branch holds it — and `identical` when it is the branch's
// head; `ahead` and `diverged` both mean the branch does not hold it). Only `status` is decoded:
// the comparison's commit list, which an unpaginated answer carries up to 250 of, is asked for
// one page of one so the body stays small, and `status` is a field of every page. A 403 or 404
// is ErrRepositoryUnreadable, which the results read takes as unknown about the repository's
// default branch and as "not on it" about an after-merge check's base branch, which GitHub no
// longer holding is the usual reason for; a 404 is also GitHub's answer for a commit the
// repository does not have, which is likewise no proof that the branch lacks it.
func (c *Client) CommitOnBranch(ctx context.Context, token, owner, repo, branch, sha string) (bool, error) {
	if c == nil {
		return false, ErrNoAppKey
	}
	// The three dots are the comparison's own syntax and travel unescaped; each side is escaped
	// on its own so a `/` in a branch name travels as %2F.
	path := repoPath(owner, repo) +
		"/compare/" + url.PathEscape(branch) + "..." + url.PathEscape(sha) + "?per_page=1"
	body, _, err := c.readRepositoryJSON(ctx, token, path, owner+"/"+repo)
	if err != nil {
		return false, err
	}
	var comparison struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &comparison); err != nil {
		return false, fmt.Errorf("decode the comparison of %s/%s %s...%s: %w", owner, repo, branch, sha, err)
	}
	return comparison.Status == "behind" || comparison.Status == "identical", nil
}

// HasCommit answers whether the repository holds sha
// (`GET /repos/{owner}/{repo}/commits/{sha}`). Measured against GitHub on 2026-11-05: that
// path answers 200 for a commit a repository the caller can read holds, 422 with the message
// `No commit found for SHA: <sha>` for one it does not, and 404 Not Found for a repository
// the caller cannot see. That split is the whole mechanism of this read: a (false, nil)
// answer comes of that one 422 and of nothing else, and it is the only proof of absence
// Dispatch has.
//
// The status alone does not make the proof. GitHub documents 422 on this path as "Validation
// failed, or the endpoint has been spammed", so a throttled read carries the same status as a
// commit that is not there; the message is what tells them apart (noCommitForSHA).
//
// Every other answer is the error, which says that Dispatch cannot tell rather than that the
// commit is missing: any other 422, the 403 and the 404 CheckRepositoryResponse makes
// ErrRepositoryUnreadable of, a rate limit, a 5xx, a transport failure, and a nil client as
// ErrNoAppKey, the way CommitOnBranch answers it. The 404 is unusable on its own either way —
// it is GitHub's answer for an unreadable repository and, as CommitOnBranch's comment records,
// for a commit a readable repository does not have.
//
// A held commit's body is read for nothing, so the read asks GitHub for the smallest answer
// that path sends: `Accept: application/vnd.github.sha`, whose answer is the commit's SHA on
// one line, in place of the commit object with its message, its parents and every file's
// patch. That media type asks for the shape of a commit, and GitHub sends a failure as JSON
// under it as under any other, so the 422's message still arrives. Ref reads the same path
// that way, for the same reason; this read cannot call Ref, which folds 404 and 422 into one
// ErrNoBranch and so loses the split the classification above turns on.
func (c *Client) HasCommit(ctx context.Context, token, owner, repo, sha string) (bool, error) {
	if c == nil {
		return false, ErrNoAppKey
	}
	target := c.base + repoPath(owner, repo) + "/commits/" + url.PathEscape(sha)
	body, status, header, err := c.request(ctx, c.TokenScope(token), http.MethodGet, target, "Bearer "+token, mediaTypeSHA, "", nil, responseLimit)
	if err != nil {
		return false, err
	}
	if status == http.StatusUnprocessableEntity && noCommitForSHA(body) {
		return false, nil
	}
	if err := CheckRepositoryResponse(status, header, body, token, owner+"/"+repo, target); err != nil {
		return false, err
	}
	return true, nil
}

// noCommitForSHA reports whether a body is the one GitHub sends for a commit a readable
// repository does not hold: a JSON object whose `message` begins `No commit found for SHA`,
// which GitHub follows with the SHA that was asked about. A body that is not JSON, and a
// message that begins with anything else — the validation failure and the spammed endpoint
// GitHub names for the same status — is no proof of absence.
func noCommitForSHA(body []byte) bool {
	return strings.HasPrefix(responseMessage(body), "No commit found for SHA")
}

// responseMessage is the `message` of body, the JSON object GitHub answers a refused request
// with, and "" for a body that is not JSON or names none.
func responseMessage(body []byte) string {
	var answer struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return ""
	}
	return answer.Message
}

// ErrorJSON reports whether body is GitHub's error JSON: an object whose message is a string, the
// shape GitHub refuses a request with, read as responseMessage reads it, so every body Message
// finds a message in is one.
func ErrorJSON(body []byte) bool {
	var answer struct {
		Message *string `json:"message"`
	}
	return json.Unmarshal(body, &answer) == nil && answer.Message != nil
}

// BodyText is body, an answer's to a request that carried tokens (the App JWT or an installation
// token), as an error or a log line may print it: as GitHub sent it where it is GitHub's error JSON
// (ErrorJSON), GitHub's word on the request. Any other body was written by something in GitHub's
// place, such as a gateway or a proxy, which can echo the request back, its Authorization header
// included, so it is named in one line with each of tokens and any Authorization, Bearer or DPoP
// value redacted (excerpt.BodyForText), and never whole.
func BodyText(body []byte, tokens ...string) string {
	if ErrorJSON(body) {
		return string(body)
	}
	return excerpt.BodyForText(string(body), "GitHub's error JSON", tokens...)
}

// TokenStatusText is StatusText for an answer to a request that carried token: `status <n>:
// <body>` with the body as BodyText names it, so a body that is not GitHub's error JSON prints in
// one line with token redacted. An answer with no body, and every answer at a 2xx status, ends at
// its status, as StatusText's does.
func TokenStatusText(status int, body []byte, token string) string {
	return StatusText(status, errorBody(body, token))
}

// errorBody is body as an error of this package carries it (StatusError, RateLimitError,
// TokenStatusText): empty for an answer with no body, so the error's text ends at its head as
// withBody has every such text end, and otherwise as BodyText names it.
func errorBody(body []byte, token string) string {
	if len(body) == 0 {
		return ""
	}
	return BodyText(body, token)
}

// CheckTokenResponse is CheckResponse for an answer to a request that carried token, the App JWT
// or an installation token: nil at 200, else the *RateLimitError or the *StatusError, each with
// the status and, as its body, the body as its text prints it (BodyText). GitHub's own error JSON
// is then the body as GitHub sent it, and any other body is named in one line with token
// redacted, since whatever wrote it in GitHub's place can echo the request back; the rule reads
// either form as it reads every remote field (redact.RemoteText). Message is read from the body
// as GitHub sent it, so it finds GitHub's own message, and none in a body BodyText names, as
// CheckResponse does. The reads of client.go and this file and every GitHub read of delivery
// check their answers with it.
func CheckTokenResponse(status int, header http.Header, body []byte, token string) error {
	if status == http.StatusOK {
		return nil
	}
	if limited := rateLimit(status, header, body, token); limited != nil {
		return limited
	}
	return &StatusError{Status: status, body: redact.NewRemoteText(errorBody(body, token)), message: redact.NewRemoteText(responseMessage(body))}
}

// rateLimit is RateLimit for an answer to a request that carried token, its Body the body as its
// text prints it, as CheckTokenResponse's is.
func rateLimit(status int, header http.Header, body []byte, token string) *RateLimitError {
	limited := RateLimit(status, header, body)
	if limited != nil {
		limited.Body = errorBody(body, token)
	}
	return limited
}

// DefaultBranch is the name of the repository's default branch (`GET /repos/{owner}/{repo}`,
// `default_branch` alone), the branch a default-branch result's commit must be on, and one of
// the two an after-merge result's commit may be on (the other is the base branch its issue's pull
// request merged into). A 403 or 404 is ErrRepositoryUnreadable.
func (c *Client) DefaultBranch(ctx context.Context, token, owner, repo string) (string, error) {
	if c == nil {
		return "", ErrNoAppKey
	}
	path := repoPath(owner, repo)
	body, _, err := c.readRepositoryJSON(ctx, token, path, owner+"/"+repo)
	if err != nil {
		return "", err
	}
	var repository struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &repository); err != nil {
		return "", fmt.Errorf("decode %s/%s: %w", owner, repo, err)
	}
	if repository.DefaultBranch == "" {
		return "", fmt.Errorf("%s/%s: GitHub named no default branch", owner, repo)
	}
	return repository.DefaultBranch, nil
}

// repoPath is the API path of owner/repo, each part escaped as CommitOnBranch escapes its own.
func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// pageNumber is the page a caller asked for, counting from one: a caller that counted from zero,
// or below, asks GitHub for the first page rather than for a page GitHub does not name.
func pageNumber(page int) int {
	if page < 1 {
		return 1
	}
	return page
}

// ObjectPage is one page of a conditional read of GitHub check objects or commit statuses: the
// objects as contracts.GithubCIObservation or contracts.GithubStatusObservation takes them, the
// answer's ETag, whether GitHub answered 304 Not Modified (Objects is then empty), and whether the
// page was full, so a caller knows another page may follow.
type ObjectPage struct {
	Objects     []map[string]any
	ETag        string
	NotModified bool
	Full        bool
}

// CommitCheckRuns reads page page (1-based) of GET
// /repos/{owner}/{repo}/commits/{sha}/check-runs?filter=all&per_page=100, conditional on etag.
// filter=all, not GitHub's default latest, because a run Envoy's listener never recorded may be
// an earlier attempt of a check that has since run again.
func (c *Client) CommitCheckRuns(ctx context.Context, token, owner, repo, sha string, page int, etag string) (ObjectPage, error) {
	if c == nil {
		return ObjectPage{}, ErrNoAppKey
	}
	repoFull := owner + "/" + repo
	path := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha) +
		"/check-runs?filter=all&per_page=" + strconv.Itoa(objectsPerPage) + "&page=" + strconv.Itoa(pageNumber(page))
	return c.readObjectPage(ctx, token, path, repoFull, etag, func(body []byte) ([]map[string]any, error) {
		var answer struct {
			CheckRuns []map[string]any `json:"check_runs"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return nil, fmt.Errorf("decode the check_runs of %s: %w", repoFull, err)
		}
		return answer.CheckRuns, nil
	})
}

// CommitCheckSuites reads page page (1-based) of GET
// /repos/{owner}/{repo}/commits/{sha}/check-suites?per_page=100, conditional on etag.
func (c *Client) CommitCheckSuites(ctx context.Context, token, owner, repo, sha string, page int, etag string) (ObjectPage, error) {
	if c == nil {
		return ObjectPage{}, ErrNoAppKey
	}
	repoFull := owner + "/" + repo
	path := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha) +
		"/check-suites?per_page=" + strconv.Itoa(objectsPerPage) + "&page=" + strconv.Itoa(pageNumber(page))
	return c.readObjectPage(ctx, token, path, repoFull, etag, func(body []byte) ([]map[string]any, error) {
		var answer struct {
			CheckSuites []map[string]any `json:"check_suites"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return nil, fmt.Errorf("decode the check_suites of %s: %w", repoFull, err)
		}
		return answer.CheckSuites, nil
	})
}

// CommitStatuses reads page page (1-based) of GET
// /repos/{owner}/{repo}/commits/{sha}/status?per_page=100, conditional on etag: GitHub's combined
// status of the commit, whose statuses list holds the newest status of each context. Only that
// list is decoded, each status as GitHub lists it (context, state, created_at, target_url,
// description); the combined state beside it is GitHub's verdict over those statuses, which no
// caller reads. A token without Commit statuses is answered 403, which is ErrRepositoryUnreadable
// as every 403 here is.
func (c *Client) CommitStatuses(ctx context.Context, token, owner, repo, sha string, page int, etag string) (ObjectPage, error) {
	if c == nil {
		return ObjectPage{}, ErrNoAppKey
	}
	repoFull := owner + "/" + repo
	path := repoPath(owner, repo) + "/commits/" + url.PathEscape(sha) +
		"/status?per_page=" + strconv.Itoa(objectsPerPage) + "&page=" + strconv.Itoa(pageNumber(page))
	return c.readObjectPage(ctx, token, path, repoFull, etag, func(body []byte) ([]map[string]any, error) {
		var answer struct {
			Statuses []map[string]any `json:"statuses"`
		}
		if err := json.Unmarshal(body, &answer); err != nil {
			return nil, fmt.Errorf("decode the statuses of %s: %w", repoFull, err)
		}
		return answer.Statuses, nil
	})
}

// readObjectPage reads one page of a commit's check objects or commit statuses: path is the page's
// whole API path, and objects decodes the answer GitHub sends, {"total_count":n,"<key>":[…]}, into
// the objects under the one key that listing carries. The caller passes that decode rather than
// the key's name, so a listing nobody has written a decode for cannot be asked for at all, where a
// name this function chose a field by could answer no objects and no error. Each decode takes the
// objects alone into maps, as the webhook's own bodies reach contracts.GithubCIObservation:
// total_count is GitHub's count of the whole list, which the page size and Full already say what
// a caller needs of.
func (c *Client) readObjectPage(ctx context.Context, token, path, repoFull, etag string, objects func([]byte) ([]map[string]any, error)) (ObjectPage, error) {
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, path, repoFull, etag)
	if err != nil {
		return ObjectPage{}, err
	}
	if status == http.StatusNotModified {
		return ObjectPage{ETag: answerETag, NotModified: true}, nil
	}
	decoded, err := objects(body)
	if err != nil {
		return ObjectPage{}, err
	}
	return ObjectPage{Objects: decoded, ETag: answerETag, Full: len(decoded) == objectsPerPage}, nil
}

// PullRequestHead is one open pull request of a repository: its number and its head commit.
type PullRequestHead struct {
	Number  int64
	HeadSHA string
}

// PullRequestPage is one page of GET /repos/{owner}/{repo}/pulls?state=open&per_page=100, read
// conditionally as ObjectPage is.
type PullRequestPage struct {
	PullRequests []PullRequestHead
	ETag         string
	NotModified  bool
	Full         bool
}

// OpenPullRequests reads page page (1-based) of the repository's open pull requests, conditional
// on etag: the heads a pass reads the check runs and suites of. GitHub's answer is a bare array,
// each item carrying number and head.sha.
func (c *Client) OpenPullRequests(ctx context.Context, token, owner, repo string, page int, etag string) (PullRequestPage, error) {
	if c == nil {
		return PullRequestPage{}, ErrNoAppKey
	}
	path := repoPath(owner, repo) + "/pulls?state=open&per_page=" + strconv.Itoa(objectsPerPage) +
		"&page=" + strconv.Itoa(pageNumber(page))
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, path, owner+"/"+repo, etag)
	if err != nil {
		return PullRequestPage{}, err
	}
	if status == http.StatusNotModified {
		return PullRequestPage{ETag: answerETag, NotModified: true}, nil
	}
	var answer []struct {
		Number int64 `json:"number"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return PullRequestPage{}, fmt.Errorf("decode the open pull requests of %s/%s: %w", owner, repo, err)
	}
	pulls := make([]PullRequestHead, len(answer))
	for i, pull := range answer {
		pulls[i] = PullRequestHead{Number: pull.Number, HeadSHA: pull.Head.SHA}
	}
	return PullRequestPage{PullRequests: pulls, ETag: answerETag, Full: len(answer) == objectsPerPage}, nil
}

// RepositoryAnswer is GET /repos/{owner}/{repo} read conditionally for the two fields a poller
// keys its records by: the name GitHub itself gives the repository, and its id.
type RepositoryAnswer struct {
	FullName    string
	ID          int64
	ETag        string
	NotModified bool
}

// RepositoryIdentity reads owner/repo itself, conditional on etag, for the name and id a poller
// keys its records by. A repository the App cannot see is ErrRepositoryUnreadable, as every read
// here is.
func (c *Client) RepositoryIdentity(ctx context.Context, token, owner, repo, etag string) (RepositoryAnswer, error) {
	if c == nil {
		return RepositoryAnswer{}, ErrNoAppKey
	}
	body, status, answerETag, err := c.readConditionalRepositoryJSON(ctx, token, repoPath(owner, repo), owner+"/"+repo, etag)
	if err != nil {
		return RepositoryAnswer{}, err
	}
	if status == http.StatusNotModified {
		return RepositoryAnswer{ETag: answerETag, NotModified: true}, nil
	}
	var repository struct {
		FullName string `json:"full_name"`
		ID       int64  `json:"id"`
	}
	if err := json.Unmarshal(body, &repository); err != nil {
		return RepositoryAnswer{}, fmt.Errorf("decode %s/%s: %w", owner, repo, err)
	}
	return RepositoryAnswer{FullName: repository.FullName, ID: repository.ID, ETag: answerETag}, nil
}
