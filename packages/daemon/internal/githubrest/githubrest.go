// Package githubrest is the daemon's client for one repository's REST API with an installation
// token. appauth mints those tokens with its own client.
package githubrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// RepositoryAPI is repository's REST base under endpoint, GitHub's API root; empty is
// https://api.github.com.
func RepositoryAPI(endpoint string, repository ghrepo.Repository) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/repos/" + repository.String()
}

// Client calls GitHub with an installation token. Its API is a repository's REST base
// (RepositoryAPI), which Get, Post, GetPages and GetListPages take paths under; an empty endpoint
// is api.github.com. A client whose API is GitHub's GraphQL endpoint (GraphQLEndpoint) sends
// queries (GraphQL) instead.
//
// ETags, when set, makes every GET the client sends conditional: it carries the ETag GitHub last
// answered for the same URL under the same Scope (If-None-Match), and GitHub's 304, which says the
// answer has not changed and costs none of the token's rate limit, is read as that earlier answer
// (ETags.keep). Nil sends every GET whole. WithETags sets it for one read. Scope names, with no
// secret, what the token may read - its App, its installation's owner and its permissions - so
// that clients of two scopes sharing one ETags never answer each other's reads: a token minted
// again in the same scope keeps the answers kept before it. A client with no Scope sends every GET
// whole, ETags or not, since nothing would keep its answers apart from another client's.
type Client struct {
	Token string
	API   string
	Scope string
	ETags *ETags
}

// WithETags is the client with every GET conditional on what etags kept for its Scope
// (Client.ETags), which a client with no Scope ignores.
func (c Client) WithETags(etags *ETags) Client {
	c.ETags = etags
	return c
}

// Get reads path under the client's REST base into into. An answer other than 2xx is an *Answer.
func (c Client) Get(ctx context.Context, path string, into any) error {
	_, _, err := c.call(ctx, http.MethodGet, c.API+path, nil, into)
	return err
}

// Post sends body, as JSON, to path under the client's API, and reads GitHub's answer into into
// when into is not nil. An answer other than 2xx is an *Answer.
func (c Client) Post(ctx context.Context, path string, body, into any) error {
	return c.send(ctx, http.MethodPost, path, body, into)
}

// send sends body, as JSON, to path under the client's API with method, and reads GitHub's answer
// into into when into is not nil.
func (c Client) send(ctx context.Context, method, path string, body, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", method, path, err)
	}
	_, _, err = c.call(ctx, method, c.API+path, encoded, into)
	return err
}

// Budget is the GitHub rate-limit budget a call spends: the REST API's core budget, or GitHub's
// secondary limit, which every call of the token meets.
type Budget string

const (
	BudgetCore      Budget = "core"
	BudgetSecondary Budget = "secondary"
)

// RetryAfter is how long GitHub asks the caller to wait, and the budget its limit spent, when err
// carries GitHub's rate limit: an *Answer that is one. Any other error is no limit: zero, and no
// budget.
func RetryAfter(err error) (time.Duration, Budget) {
	var answer *Answer
	if errors.As(err, &answer) && answer.RateLimited {
		return answer.RetryAfter, answer.Budget
	}
	return 0, ""
}

// spent is the budget a rate limit spent, read from its answer's headers and its body, body.
// GitHub's secondary limit when the body names it (NamesSecondaryLimit), whatever
// x-ratelimit-remaining says: octokit's throttling plugin, GitHub's own client, reads a message
// saying "secondary rate" before that header, and this package reads the limit's older name,
// "abuse detection mechanism", the same way. Else the secondary limit when the answer asks for a
// Retry-After or leaves requests (no x-ratelimit-remaining: 0); and otherwise the primary budget
// x-ratelimit-resource names, or the core budget, the REST API's own, when it names none.
func spent(headers http.Header, body []byte) Budget {
	if NamesSecondaryLimit(string(body)) || headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") != "0" {
		return BudgetSecondary
	}
	if resource := headers.Get("X-RateLimit-Resource"); resource != "" {
		return Budget(resource)
	}
	return BudgetCore
}

// GetPages reads every page of the JSON array GitHub answers at path, a hundred items to a page,
// following the Link header's rel="next" until GitHub names none.
func GetPages[T any](ctx context.Context, c Client, path string) ([]T, error) {
	return getPages[T](ctx, c, path, "")
}

// GetListPages reads every page of the list GitHub answers at path as the field of a JSON object,
// as its check-run and workflow-run lists answer ({"total_count": n, "check_runs": [...]}), paged as
// GetPages pages an array. An answer without the field is an error, never an empty list.
func GetListPages[T any](ctx context.Context, c Client, path, field string) ([]T, error) {
	return getPages[T](ctx, c, path, field)
}

// getPages is GetPages, reading each page's items from the field of the object it answers when
// field is not empty. A next page on another scheme or host than the REST base's is refused, so the
// token goes nowhere but the API it was configured for.
func getPages[T any](ctx context.Context, c Client, path, field string) ([]T, error) {
	base, err := url.Parse(c.API)
	if err != nil {
		return nil, fmt.Errorf("parse the REST base %q: %w", c.API, err)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	var all []T
	for target := c.API + path + separator + "per_page=100"; target != ""; {
		var page []T
		var object map[string]json.RawMessage
		var into any = &page
		if field != "" {
			into = &object
		}
		next, _, err := c.call(ctx, http.MethodGet, target, nil, into)
		if err != nil {
			return nil, err
		}
		if field != "" {
			list, ok := object[field]
			if !ok {
				return nil, fmt.Errorf("GitHub answered GET %s with no %q list", path, field)
			}
			if err := json.Unmarshal(list, &page); err != nil {
				return nil, fmt.Errorf("decode the %q list GitHub answered GET %s with: %w", field, path, err)
			}
		}
		all = append(all, page...)
		if next != "" {
			nextURL, err := url.Parse(next)
			if err != nil || nextURL.Scheme != base.Scheme || !strings.EqualFold(nextURL.Host, base.Host) {
				return nil, fmt.Errorf("GET %s: GitHub's next page %q is not on %s://%s, the only origin sent the token", path, next, base.Scheme, base.Host)
			}
		}
		target = next
	}
	return all, nil
}

// restAccept is the media type every REST call asks for.
const restAccept = "application/vnd.github+json"

// call sends method to target, with body as JSON when it is not nil, reads a 2xx answer into into
// when into is not nil, and returns the next page's URL its Link header names ("" for none) and
// GitHub's response headers. An answer other than 2xx is an *Answer; when it is a rate limit, it
// names the budget spent, the REST API's core budget when GitHub names no other. A GET of a client
// with ETags is conditional (Client.ETags): GitHub's 304 is read as the answer kept for target, its
// body and its next page.
func (c Client) call(ctx context.Context, method, target string, body []byte, into any) (string, http.Header, error) {
	return c.exchange(ctx, method, target, body, into, restAccept)
}

// exchange is call with accept as the request's Accept header.
func (c Client) exchange(ctx context.Context, method, target string, body []byte, into any, accept string) (string, http.Header, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return "", nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", accept)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	var kept etagEntry
	conditional := method == http.MethodGet && c.ETags != nil && c.Scope != ""
	cacheKey := c.Scope + " " + target
	if conditional {
		var found bool
		if kept, found = c.ETags.lookup(cacheKey); found {
			request.Header.Set("If-None-Match", kept.etag)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return "", response.Header, err
	}
	if response.StatusCode == http.StatusNotModified && kept.etag != "" {
		// Unchanged since the answer kept for target: that answer is GitHub's answer now.
		if into == nil {
			return kept.next, response.Header, nil
		}
		return kept.next, response.Header, json.Unmarshal(kept.body, into)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", response.Header, c.refusal(method, request.URL, response, answer)
	}
	next := nextPage(response.Header.Get("Link"))
	if conditional {
		c.ETags.keep(cacheKey, response.Header.Get("ETag"), answer, next)
	}
	if into == nil {
		return next, response.Header, nil
	}
	return next, response.Header, json.Unmarshal(answer, into)
}

// refusal is response, GitHub's answer other than 2xx to method at target, as the *Answer its
// caller returns (AnswerOf), naming the path under the client's API (answerPath).
func (c Client) refusal(method string, target *url.URL, response *http.Response, body []byte) *Answer {
	return AnswerOf(method, c.answerPath(target), response, body)
}

// AnswerOf is response, GitHub's answer other than 2xx to method at path, whose body is body, as an
// *Answer: the status, the body as GitHub sent it, the rate-limit headers as GitHub sent them,
// whether the answer is GitHub's rate limit (RateLimited) and, when it is, how long to wait
// (retryAfter) and which budget it spent (spent) — the REST API's core budget when GitHub names no
// other. Client builds every answer it returns with it, and appauth builds the one its own errors
// carry (appauth.AnswerError), so both read GitHub's answers one way.
func AnswerOf(method, path string, response *http.Response, body []byte) *Answer {
	refused := &Answer{Method: method, Path: path, Status: response.StatusCode, Body: strings.TrimSpace(string(body)),
		RateLimited: RateLimited(response, body), RetryAfter: retryAfter(response, body, time.Now()), Limit: limitHeaders(response.Header)}
	if refused.RateLimited {
		refused.Budget = spent(response.Header, body)
	}
	return refused
}

// answerPath is the path an Answer names for target under the client's API (PathUnder).
func (c Client) answerPath(target *url.URL) string { return PathUnder(c.API, target) }

// PathUnder is the path an Answer names for target, its query left out: the path under api, or
// GitHub's whole path for a URL outside it, such as a later page GitHub names under
// /repositories/<id>/. Client names its answers' paths so (answerPath), and appauth names those its
// errors carry so under its own base URL (appauth.AnswerError).
func PathUnder(api string, target *url.URL) string {
	if base, err := url.Parse(api); err == nil {
		if rest, found := strings.CutPrefix(target.Path, base.Path); found && strings.HasPrefix(rest, "/") {
			return rest
		}
	}
	return target.Path
}

// secondaryLimitMessages are what GitHub's secondary rate limit says in the body of the 403 it can
// answer with instead of a 429: "You have exceeded a secondary rate limit", with the older wording
// of the same limit, "You have triggered an abuse detection mechanism", beside it. The hyphenated
// "secondary rate-limit" stands for no GitHub answer this repository holds: the leak guard matched
// it from #4011 until #4052 moved the guard's reading here without it, and it is kept so that the
// guard reads no answer as a limit less often than before.
var secondaryLimitMessages = []string{"secondary rate limit", "secondary rate-limit", "abuse detection mechanism"}

// RateLimited says whether response, whose body is body, is GitHub's rate limit: a 429, or a 403
// carrying x-ratelimit-remaining: 0 or a retry-after, or one whose body says it is the secondary
// limit, which GitHub's REST documentation allows with neither header set. Those are the statuses
// GitHub documents a limit on; a caller that reads another status as an answer of its own asks
// RateLimitSignals before it does.
func RateLimited(response *http.Response, body []byte) bool {
	if response.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	return RateLimitSignals(response, body)
}

// RateLimitSignals says whether response's headers or its body, body, carry GitHub's rate limit,
// whatever the status: x-ratelimit-remaining: 0, a retry-after, or a body naming the secondary
// limit. A status GitHub does not document a limit on still means the limit and not the answer
// when it carries one of these, so a caller reading such a status as a verdict of its own
// (appauth's 422 for a permission set) asks this first.
func RateLimitSignals(response *http.Response, body []byte) bool {
	return limitSignals(response.Header, string(body))
}

// limitSignals says whether headers or text, an answer's body or its errors' messages, carry
// GitHub's rate limit (RateLimitSignals); GraphQL's 200 answers are read through it too
// (GraphQLErrors.rateLimited).
func limitSignals(headers http.Header, text string) bool {
	return headers.Get("X-RateLimit-Remaining") == "0" || headers.Get("Retry-After") != "" || NamesSecondaryLimit(text)
}

// Limit is whether response, whose body is body, is GitHub's rate limit (RateLimited), and the
// budget the limit spent (spent): the reading this package gives its own answers, for a caller
// that reads GitHub's answers through another client, as the leak guard reads them through gh.
func Limit(response *http.Response, body []byte) (bool, Budget) {
	if !RateLimited(response, body) {
		return false, ""
	}
	return true, spent(response.Header, body)
}

// NamesSecondaryLimit says whether text, a GitHub answer's body or error message, says it is
// GitHub's secondary rate limit (secondaryLimitMessages).
func NamesSecondaryLimit(text string) bool {
	message := strings.ToLower(text)
	for _, named := range secondaryLimitMessages {
		if strings.Contains(message, named) {
			return true
		}
	}
	return false
}

// timeoutMessage is what GitHub says in the body of its answer, a 502, to a GraphQL query that ran
// out of the time it gives one query: "We couldn't respond to your request in time. Sorry about
// that. Please try resubmitting your request and contact us if the problem persists."
const timeoutMessage = "couldn't respond to your request in time"

// NamesTimeout says whether err carries GitHub's answer that a request ran out of the time it
// gives one: an *Answer, however the caller wrapped it, whose body says so (timeoutMessage).
func NamesTimeout(err error) bool {
	var answer *Answer
	return errors.As(err, &answer) && strings.Contains(strings.ToLower(answer.Body), timeoutMessage)
}

// Answered says whether err, the outcome of a call, is an answer GitHub gave: nil, the data it
// answered; an answer other than 2xx (*Answer); or a 200 whose errors name what GitHub could not
// answer (GraphQLErrors). Anything else - a connection that failed, a deadline that ended the
// call, a token that could not be minted for it - reached no answer of GitHub's.
func Answered(err error) bool {
	var answer *Answer
	var graphQLErrors GraphQLErrors
	return err == nil || errors.As(err, &answer) || errors.As(err, &graphQLErrors)
}

// retryAfter is how long GitHub asks a caller its rate limit answered to wait before calling again,
// as its REST documentation's "Exceeding the rate limit" says: the retry-after header's seconds,
// else, when x-ratelimit-remaining is 0, until the x-ratelimit-reset epoch second, capped at an
// hour, GitHub's primary rate limit's window, so a host clock behind GitHub's never stretches the
// wait past it; else at least a minute, which is what a secondary limit named by its message alone
// is waited. A reset this host's clock already reads as past is waited the minute too. Zero for an
// answer that is not a rate limit.
func retryAfter(response *http.Response, body []byte, now time.Time) time.Duration {
	if !RateLimited(response, body) {
		return 0
	}
	return limitWait(response.Header, now)
}

// limitWait is how long a rate limit's headers ask a caller to wait, read as retryAfter reads
// them: the retry-after header's seconds, else the x-ratelimit-reset epoch second when
// x-ratelimit-remaining is 0, capped at an hour, else a minute. GraphQL's own rate limit, which
// GitHub answers 200 with, is read through it too (Client.GraphQL).
func limitWait(headers http.Header, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(headers.Get("Retry-After")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Unix(reset, 0).Sub(now); wait > 0 {
				if wait > time.Hour {
					return time.Hour
				}
				return wait
			}
		}
	}
	return time.Minute
}

// nextPage is the URL a Link header names rel="next", "" when it names none.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, found := strings.Cut(strings.TrimSpace(part), ";")
		if found && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// Answer is a GitHub REST answer other than 2xx: the request's method and path (its query left
// out), GitHub's HTTP status, the body GitHub sent with it, the rate-limit headers GitHub sent with
// it (Limit), and whether it is a rate limit, which every further call on the same token meets
// until the limit resets, with how long GitHub asks the caller to wait before its next call
// (retryAfter; zero when it is not one) and the budget the limit spent (empty when it is not one).
type Answer struct {
	Method      string
	Path        string
	Status      int
	Body        string
	RateLimited bool
	RetryAfter  time.Duration
	Budget      Budget
	Limit       LimitHeaders
}

// LimitHeaders is the rate-limit headers of a GitHub answer as GitHub sent them, "" for each it did
// not send: Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset (an epoch
// second) and X-RateLimit-Resource. RetryAfter and Budget are what the client reads from them and
// decides by; these decide nothing, and are kept as received so that a log of a refusal names the
// limit that refused it in GitHub's own words, including where they disagree with the body.
type LimitHeaders struct {
	RetryAfter string
	Limit      string
	Remaining  string
	Reset      string
	Resource   string
}

// limitHeaders is headers' rate-limit headers as GitHub sent them.
func limitHeaders(headers http.Header) LimitHeaders {
	return LimitHeaders{
		RetryAfter: headers.Get("Retry-After"), Limit: headers.Get("X-RateLimit-Limit"), Remaining: headers.Get("X-RateLimit-Remaining"),
		Reset: headers.Get("X-RateLimit-Reset"), Resource: headers.Get("X-RateLimit-Resource"),
	}
}

func (a *Answer) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", a.Method, a.Path, a.Status, a.Body)
}
