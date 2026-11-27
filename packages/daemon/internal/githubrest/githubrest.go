// Package githubrest is the daemon's client for one repository's REST API with an installation
// token. appauth mints those tokens with its own client.
package githubrest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/oauth2call"
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
// (RepositoryAPI), which Get, Post and GetPages take paths under; an empty endpoint is
// api.github.com. A client whose API is GitHub's GraphQL endpoint (GraphQLEndpoint) sends
// queries (GraphQL) instead.
//
// Each of them also takes shown, the name a failure of the call prints for it (call): a path
// that holds another party's words - an account GitHub named, a branch GitHub answered - holds
// them as they came, and GitHub's answer to it, or net/http's error for a request that got no
// answer, would print them so; its caller passes the path as a message may print it instead.
// "" names the call by its path, as a path the daemon wrote is named.
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

// Get reads path under the client's REST base into into, naming the read shown when it fails
// (Client). An answer other than 2xx is an *Answer.
func (c Client) Get(ctx context.Context, path, shown string, into any) error {
	_, _, err := c.call(ctx, http.MethodGet, c.API+path, shown, nil, into)
	return err
}

// Post sends body, as JSON, to path under the client's API, and reads GitHub's answer into into
// when into is not nil, naming the call shown when it fails (Client). An answer other than 2xx is
// an *Answer.
func (c Client) Post(ctx context.Context, path, shown string, body, into any) error {
	return c.send(ctx, http.MethodPost, path, shown, body, into)
}

// send sends body, as JSON, to path under the client's API with method, and reads GitHub's answer
// into into when into is not nil, naming the call shown when it fails (Client).
func (c Client) send(ctx context.Context, method, path, shown string, body, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", method, cmp.Or(shown, path), err)
	}
	_, _, err = c.call(ctx, method, c.API+path, shown, encoded, into)
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

// spent is the budget a rate limit spent, read from its answer's headers and whether its body names
// GitHub's secondary limit (named, namesSecondaryLimit): GitHub's secondary limit when the body
// names it, whatever x-ratelimit-remaining says: octokit's throttling plugin, GitHub's own client,
// reads a message saying "secondary rate" before that header, and this package reads the limit's
// older name, "abuse detection mechanism", the same way. Else the secondary limit when the answer
// asks for a Retry-After or leaves requests (no x-ratelimit-remaining: 0); and otherwise the
// primary budget x-ratelimit-resource names, where it names a resource GitHub documents
// (documentedResources), or the core budget, the REST API's own, when it names none or any other.
// The header is the answering party's own words, which whatever answers in GitHub's place can
// make an echo of the request's token, and a Budget is decided on and printed as it is (the
// daemon's limitAttributes logs it), so only a name GitHub documents is kept as a Budget; the
// header itself is kept for a log as the rule reads it (LimitHeaders.Resource).
func spent(headers http.Header, named bool) Budget {
	if named || headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") != "0" {
		return BudgetSecondary
	}
	if resource := headers.Get("X-RateLimit-Resource"); slices.Contains(documentedResources, resource) {
		return Budget(resource)
	}
	return BudgetCore
}

// documentedResources are the rate-limit resources x-ratelimit-resource names one of, the one a
// request counted against: the objects under `resources` that GitHub's REST reference documents
// for GET /rate_limit ("Rate limits for the REST API", "REST API endpoints for rate limits").
var documentedResources = []string{
	"core", "search", "code_search", "graphql", "integration_manifest", "dependency_snapshots", "dependency_sbom",
	"code_scanning_upload", "code_scanning_autofix", "actions_runner_registration", "source_import", "scim", "copilot_usage_records",
}

// GetPages reads every page of the JSON array GitHub answers at path, a hundred items to a page,
// following the Link header's rel="next" until GitHub names none, naming the read shown when it
// fails (Client). A next page on another scheme or host than the REST base's is refused, so the
// token goes nowhere but the API it was configured for.
//
// The next page's URL is what GitHub's Link header names (nextPage): GitHub's words, or those of
// whatever answers in its place, which can echo the request's token back. So nothing this returns
// names it: the refusal of a next page elsewhere names the origin the token is sent to alone, and
// a later page's read is named by its number after the read's own name, shown or else path, in
// place of the URL GitHub's header named for it (call). The first page's read is named shown, as
// its caller asked.
func GetPages[T any](ctx context.Context, c Client, path, shown string) ([]T, error) {
	base, err := url.Parse(c.API)
	if err != nil {
		return nil, fmt.Errorf("parse the REST base %q: %w", c.API, err)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	name := cmp.Or(shown, path)
	var all []T
	n := 1
	for target := c.API + path + separator + "per_page=100"; target != ""; n++ {
		var page []T
		pageName := shown
		if n > 1 {
			pageName = fmt.Sprintf("%s (page %d)", name, n)
		}
		next, _, err := c.call(ctx, http.MethodGet, target, pageName, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next != "" {
			nextURL, err := url.Parse(next)
			if err != nil || nextURL.Scheme != base.Scheme || !strings.EqualFold(nextURL.Host, base.Host) {
				return nil, fmt.Errorf("GitHub named a next page that is not on %s://%s, the only origin sent the token", base.Scheme, base.Host)
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
// GitHub's response headers. The answer is read through oauth2call.ReadAnswer: an answer other
// than 2xx is an *Answer, decided on as it came and printed as the rule reads it, with the token
// the call carried redacted, and a decode's error quotes no value. When it is a rate limit, it
// names the budget spent, the REST API's core budget when GitHub names no other. A GET of a client
// with ETags is conditional (Client.ETags): GitHub's 304 is read as the answer kept for target, its
// body and its next page.
//
// A failure names the call shown, its caller's name for it (Client), wherever it would name
// target: GitHub's answer's path (refusal) and net/http's error for a request that got no answer
// (Unanswered). With shown "" each names target as it is.
func (c Client) call(ctx context.Context, method, target, shown string, body []byte, into any) (string, http.Header, error) {
	next, response, err := c.exchange(ctx, method, target, shown, body, into, restAccept)
	if response == nil {
		return next, nil, err
	}
	return next, response.Header, err
}

// exchange is call with accept as the request's Accept header, answering GitHub's response, its
// body read and closed, in place of its headers alone (nil for a request that got no answer):
// GraphQL reads the rate-limit headers of a 200 from it as the rule reads them (limitHeaders).
func (c Client) exchange(ctx context.Context, method, target, shown string, body []byte, into any, accept string) (string, *http.Response, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return "", nil, Unanswered(err, shown)
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
		return "", nil, Unanswered(err, shown)
	}
	defer response.Body.Close()
	read, err := oauth2call.ReadAnswer(response, 64<<20)
	if err != nil {
		return "", response, err
	}
	if response.StatusCode == http.StatusNotModified && kept.etag != "" {
		// Unchanged since the answer kept for target: that answer is GitHub's answer now.
		if into == nil {
			return kept.next, response, nil
		}
		return kept.next, response, kept.answer.Decode(into)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", response, c.refusal(method, request.URL, shown, response, read)
	}
	next := nextPage(response.Header.Get("Link"))
	if conditional {
		c.ETags.keep(cacheKey, response.Header.Get("ETag"), read, next)
	}
	if into == nil {
		return next, response, nil
	}
	return next, response, read.Decode(into)
}

// Unanswered is err, the failure of a call that got no answer, naming the call shown in place of
// the URL net/http's error names (url.Error.URL), which is the target as it came, and err as it
// came when shown is "". call returns its failures so, and appauth, which calls GitHub with a
// client of its own, names its reads so where their path holds GitHub's words.
func Unanswered(err error, shown string) error {
	var failed *url.Error
	if shown != "" && errors.As(err, &failed) {
		failed.URL = shown
	}
	return err
}

// refusal is response, GitHub's answer other than 2xx to method at target, as the *Answer its
// caller returns (AnswerOf), naming the call shown, or, when shown is "", by the path under the
// client's API (answerPath).
func (c Client) refusal(method string, target *url.URL, shown string, response *http.Response, read oauth2call.Answer) *Answer {
	path := shown
	if path == "" {
		path = c.answerPath(target)
	}
	return AnswerOf(method, path, response, read)
}

// AnswerOf is response, GitHub's answer other than 2xx to method at path, read as read
// (oauth2call.ReadAnswer: Client's call and appauth's refusalBody read it so), as an *Answer: the
// status, the body as the rule prints it - GitHub's words, with the token the call carried
// redacted - the rate-limit headers as GitHub sent them, whether the answer is GitHub's rate limit
// (RateLimited) and, when it is, how long to wait (retryAfter) and which budget it spent (spent) —
// the REST API's core budget when GitHub names no other. What the answer is, is decided on the body
// as it came, never on the rule's reading of it, which is for what the answer prints: the rule
// prints a whole body whose one URL sits beside a JSON quote as not readable, as GitHub's own
// documentation URL does, and cuts a word that holds a run of the token the call carried. Client
// builds every answer it returns with it, and appauth builds the one its own errors carry
// (appauth.AnswerError), so both read GitHub's answers one way.
func AnswerOf(method, path string, response *http.Response, read oauth2call.Answer) *Answer {
	named := oauth2call.Holds(read, namesSecondaryLimit)
	limited := rateLimited(response, named)
	refused := &Answer{Method: method, Path: path, Status: response.StatusCode, Body: strings.TrimSpace(read.Words().String()),
		RateLimited: limited, RetryAfter: retryAfter(response, limited, time.Now()), Limit: limitHeaders(response), read: read}
	if limited {
		refused.Budget = spent(response.Header, named)
	}
	return refused
}

// answerPath is the path an Answer names for target under the client's API (PathUnder).
func (c Client) answerPath(target *url.URL) string { return PathUnder(c.API, target) }

// PathUnder is the path an Answer names for target, its query left out: the path under api, or
// GitHub's whole path for a URL outside it, such as a later page GitHub names under
// /repositories/<id>/, which GetPages names by its number instead. Client names its answers'
// paths so where its caller gives the call no name of its own (refusal), and appauth names those
// its errors carry so under its own base URL (appauth.AnswerError).
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

// rateLimited says whether response is GitHub's rate limit, where named says whether its body, as
// it came, says it is the secondary limit: a 429, or a 403 carrying x-ratelimit-remaining: 0 or a
// retry-after, or naming the secondary limit, which GitHub's REST documentation allows with
// neither header set. Those are the statuses GitHub documents a limit on; a caller that reads
// another status as an answer of its own asks RateLimitSignals before it does.
func rateLimited(response *http.Response, named bool) bool {
	if response.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if response.StatusCode != http.StatusForbidden {
		return false
	}
	return signals(response.Header, named)
}

// RateLimitSignals says whether response's headers or its body, read as read and decided on as it
// came, carry GitHub's rate limit, whatever the status: x-ratelimit-remaining: 0, a retry-after,
// or a body naming the secondary limit. A status GitHub does not document a limit on still means
// the limit and not the answer when it carries one of these, so a caller reading such a status as
// a verdict of its own (appauth's 422 for a permission set) asks this first.
func RateLimitSignals(response *http.Response, read oauth2call.Answer) bool {
	return signals(response.Header, oauth2call.Holds(read, namesSecondaryLimit))
}

// limitSignals says whether headers or text, an answer's errors' messages as they came, carry
// GitHub's rate limit (RateLimitSignals); GraphQL's 200 answers are read through it
// (GraphQLErrors.rateLimited).
func limitSignals(headers http.Header, text string) bool {
	return signals(headers, NamesSecondaryLimit(text))
}

// signals is RateLimitSignals of an answer whose headers are headers and whose body names the
// secondary limit or does not (named).
func signals(headers http.Header, named bool) bool {
	return headers.Get("X-RateLimit-Remaining") == "0" || headers.Get("Retry-After") != "" || named
}

// Limit is whether response, whose body is body as a caller read it from another client, is
// GitHub's rate limit (rateLimited), and the budget the limit spent (spent): the reading this
// package gives its own answers, for a caller that reads GitHub's answers through another client,
// as the leak guard reads them through gh.
func Limit(response *http.Response, body []byte) (bool, Budget) {
	named := namesSecondaryLimit(body)
	if !rateLimited(response, named) {
		return false, ""
	}
	return true, spent(response.Header, named)
}

// NamesSecondaryLimit says whether text, a GitHub answer's body or error message as it came, says
// it is GitHub's secondary rate limit (secondaryLimitMessages).
func NamesSecondaryLimit(text string) bool { return namesSecondaryLimit([]byte(text)) }

// namesSecondaryLimit is NamesSecondaryLimit of body's bytes, each of secondaryLimitMessages
// matched in whatever case it is written (oauth2call.ContainsFold): the test oauth2call.Holds
// gives an answer's body as it came.
func namesSecondaryLimit(body []byte) bool {
	for _, named := range secondaryLimitMessages {
		if oauth2call.ContainsFold(body, named) {
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
// gives one: an *Answer, however the caller wrapped it, whose body as it came says so
// (timeoutMessage, Answer.Says), never its Body, the rule's reading of it.
func NamesTimeout(err error) bool {
	var answer *Answer
	return errors.As(err, &answer) && answer.Says(timeoutMessage)
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

// retryAfter is how long GitHub asks a caller its rate limit answered to wait before calling
// again, read from the answer's headers at now (LimitHeaders.WaitAt); zero for an answer that is
// not a rate limit (limited, rateLimited).
func retryAfter(response *http.Response, limited bool, now time.Time) time.Duration {
	if !limited {
		return 0
	}
	wait, _ := limitHeaders(response).WaitAt(now)
	return wait
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
// out), or the name its caller gave the call (Client; a later page GetPages read is named by its
// number after that name, GetPages), GitHub's HTTP status, the body GitHub sent with it as the
// rule prints it, the rate-limit headers GitHub sent with it (Limit), and whether it is a rate
// limit, which every further call on the same token meets until the limit resets, with how long
// GitHub asks the caller to wait before its next call (retryAfter; zero when it is not one) and
// the budget the limit spent (empty when it is not one). Body is for what the answer prints - a
// message, a log line, an error - and nothing decides on it: a caller decides on GitHub's words as
// they came, through Says and Decode.
type Answer struct {
	Method      string
	Path        string
	Status      int
	Body        string
	RateLimited bool
	RetryAfter  time.Duration
	Budget      Budget
	Limit       LimitHeaders
	// read is the answer as it came (AnswerOf), which Says and Decode read.
	read oauth2call.Answer
}

// Says reports whether GitHub's body, as it came, holds phrase, which must be lower-case ASCII, in
// whatever case it is written (oauth2call.ContainsFold): a decision on GitHub's wording, made on
// its words and never on Body, the rule's reading of them, which can print the whole body as not
// readable or cut a word that holds a run of the token the call carried. An Answer built without a
// read says nothing.
func (a *Answer) Says(phrase string) bool {
	return oauth2call.Holds(a.read, func(body []byte) bool { return oauth2call.ContainsFold(body, phrase) })
}

// Decode fills v from GitHub's body as it came (oauth2call.Answer.Decode): the fields a caller
// decides on, each RemoteText among them printing only as the rule reads it. Its error quotes no
// value of the body.
func (a *Answer) Decode(v any) error { return a.read.Decode(v) }

// LimitHeaders is the rate-limit headers of a GitHub answer, "" for each it did not send:
// Retry-After, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset (an epoch second) and
// X-RateLimit-Resource. RetryAfter and Budget are what the client reads from them and decides by;
// the headers are kept so that a log of a refusal names the limit that refused it as GitHub sent
// it, including where it disagrees with the body, and so that a caller keeping time on a clock of
// its own reads GitHub's wait at its own time (WaitAt). A header is the answering party's own
// words, so each is kept as the number it is written back from where it parses as one (an HTTP
// date for Retry-After), and otherwise as the rule reads it (oauth2call.Header): a resource's
// name, and anything a stand-in wrote in GitHub's place.
type LimitHeaders struct {
	RetryAfter string
	Limit      string
	Remaining  string
	Reset      string
	Resource   string
}

// WaitAt is the wait these headers ask for at now, as GitHub's REST documentation's "Exceeding the
// rate limit" reads them, and whether they name when the limit ends (named): Retry-After's seconds,
// at most maxRetryAfter; else, with no call left (X-RateLimit-Remaining 0), until the
// X-RateLimit-Reset epoch second, at most an hour, GitHub's primary rate limit's window, so a
// clock behind GitHub's never stretches the wait past it. Else a minute, the least a secondary
// limit named by its message alone is waited, and named false; a reset already past at now names
// no time either. An answer's RetryAfter is this at the host's time when the answer arrived, a
// REST answer's (retryAfter) and a GraphQL rate limit's, which GitHub answers 200 with
// (Client.GraphQL); internal/daemon's startWaits reads it at the time of its own clock, on which
// it waits, and waits its own time where named is false.
func (l LimitHeaders) WaitAt(now time.Time) (wait time.Duration, named bool) {
	if seconds, err := strconv.ParseInt(l.RetryAfter, 10, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, int64(maxRetryAfter/time.Second))) * time.Second, true
	}
	if l.Remaining == "0" {
		if reset, err := strconv.ParseInt(l.Reset, 10, 64); err == nil {
			if wait := time.Unix(reset, 0).Sub(now); wait > 0 {
				return min(wait, time.Hour), true
			}
		}
	}
	return time.Minute, false
}

// maxRetryAfter is the longest Retry-After WaitAt reads whole. Seconds are clamped to it before
// they are multiplied, so a header past what a time.Duration holds (more than 9,223,372,036
// seconds) never wraps to a negative wait, which a caller would sleep as none and try again at
// once. A day is far past any wait a caller takes whole: the daemon's start waits at most an hour
// (rateLimitPolicy.Max).
const maxRetryAfter = 24 * time.Hour

// limitHeaders is response's rate-limit headers as LimitHeaders keeps them.
func limitHeaders(response *http.Response) LimitHeaders {
	read := func(name string) string {
		value := strings.TrimSpace(response.Header.Get(name))
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			return strconv.FormatInt(n, 10)
		}
		if at, err := http.ParseTime(value); err == nil && name == "Retry-After" {
			return at.UTC().Format(http.TimeFormat)
		}
		return oauth2call.Header(response, name).String()
	}
	return LimitHeaders{
		RetryAfter: read("Retry-After"), Limit: read("X-RateLimit-Limit"), Remaining: read("X-RateLimit-Remaining"),
		Reset: read("X-RateLimit-Reset"), Resource: read("X-RateLimit-Resource"),
	}
}

func (a *Answer) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", a.Method, a.Path, a.Status, a.Body)
}
