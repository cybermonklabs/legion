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
// (RepositoryAPI), which Get, Post, GetPages and GetListPages take paths under; an empty endpoint
// is api.github.com.
type Client struct {
	Token string
	API   string
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

// spent is the budget a rate limit spent, read from its answer's headers and whether its body names
// GitHub's secondary limit (named, namesSecondaryLimit): GitHub's secondary limit when the body
// names it, whatever x-ratelimit-remaining says: octokit's throttling plugin, GitHub's own client,
// reads a message saying "secondary rate" before that header, and this package reads the limit's
// older name, "abuse detection mechanism", the same way. Else the secondary limit when the answer
// asks for a Retry-After or leaves requests (no x-ratelimit-remaining: 0); and otherwise the
// primary budget x-ratelimit-resource names, or the core budget, the REST API's own, when it names
// none.
func spent(headers http.Header, named bool) Budget {
	if named || headers.Get("Retry-After") != "" || headers.Get("X-RateLimit-Remaining") != "0" {
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
//
// The next page's URL is what GitHub's Link header names (nextPage): GitHub's words, or those of
// whatever answers in its place, which can echo the request's token back. So nothing this returns
// names it: the refusal of a next page elsewhere names the origin the token is sent to alone, and
// the failure of a later page's read names the page by its number (laterPage).
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
	n := 1
	for target := c.API + path + separator + "per_page=100"; target != ""; n++ {
		var page []T
		var object map[string]json.RawMessage
		var into any = &page
		if field != "" {
			into = &object
		}
		next, _, err := c.call(ctx, http.MethodGet, target, nil, into)
		if err != nil {
			return nil, laterPage(err, path, n)
		}
		if field != "" {
			list, ok := object[field]
			if !ok {
				return nil, fmt.Errorf("GitHub answered GET %s with no %q list", path, field)
			}
			if json.Unmarshal(list, &page) != nil {
				return nil, fmt.Errorf("decode the %q list GitHub answered GET %s with: its items are not the shape this call reads", field, path)
			}
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

// laterPage is err, the failure of the read of page n of GET path, as getPages returns it: the
// first page's as it came, since its request is the caller's path under the client's API, and a
// later page's naming that page as path and its number, in place of the URL GitHub's Link header
// named for it: GitHub's answer names it (Answer.Path), and so does net/http's error for a
// request that got no answer (url.Error.URL).
func laterPage(err error, path string, n int) error {
	if n == 1 {
		return err
	}
	shown := fmt.Sprintf("%s (page %d)", path, n)
	var answer *Answer
	if errors.As(err, &answer) {
		answer.Path = shown
		return err
	}
	var unanswered *url.Error
	if errors.As(err, &unanswered) {
		unanswered.URL = shown
	}
	return err
}

// call sends method to target, with body as JSON when it is not nil, reads a 2xx answer into into
// when into is not nil, and returns the next page's URL its Link header names ("" for none) and
// GitHub's response headers. The answer is read through oauth2call.ReadAnswer: an answer other
// than 2xx is an *Answer, decided on as it came and printed as the rule reads it, with the token
// the call carried redacted, and a decode's error quotes no value. When it is a rate limit, it
// names the budget spent, the REST API's core budget when GitHub names no other.
func (c Client) call(ctx context.Context, method, target string, body []byte, into any) (string, http.Header, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return "", nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	read, err := oauth2call.ReadAnswer(response, 64<<20)
	if err != nil {
		return "", response.Header, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", response.Header, c.refusal(method, request.URL, response, read)
	}
	if into == nil {
		return nextPage(response.Header.Get("Link")), response.Header, nil
	}
	return nextPage(response.Header.Get("Link")), response.Header, read.Decode(into)
}

// refusal is response, GitHub's answer other than 2xx to method at target, as the *Answer its
// caller returns (AnswerOf), naming the path under the client's API (answerPath).
func (c Client) refusal(method string, target *url.URL, response *http.Response, read oauth2call.Answer) *Answer {
	return AnswerOf(method, c.answerPath(target), response, read)
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
// /repositories/<id>/, which getPages then names by its number instead (laterPage). Client names
// its answers' paths so (answerPath), and appauth names those its errors carry so under its own
// base URL (appauth.AnswerError).
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
	return signals(response, named)
}

// RateLimitSignals says whether response's headers or its body, read as read and decided on as it
// came, carry GitHub's rate limit, whatever the status: x-ratelimit-remaining: 0, a retry-after,
// or a body naming the secondary limit. A status GitHub does not document a limit on still means
// the limit and not the answer when it carries one of these, so a caller reading such a status as
// a verdict of its own (appauth's 422 for a permission set) asks this first.
func RateLimitSignals(response *http.Response, read oauth2call.Answer) bool {
	return signals(response, oauth2call.Holds(read, namesSecondaryLimit))
}

// signals is RateLimitSignals of an answer whose body names the secondary limit or does not
// (named).
func signals(response *http.Response, named bool) bool {
	return response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "" || named
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

// retryAfter is how long GitHub asks a caller its rate limit answered to wait before calling again,
// as its REST documentation's "Exceeding the rate limit" says: the retry-after header's seconds,
// else, when x-ratelimit-remaining is 0, until the x-ratelimit-reset epoch second, capped at an
// hour, GitHub's primary rate limit's window, so a host clock behind GitHub's never stretches the
// wait past it; else at least a minute, which is what a secondary limit named by its message alone
// is waited. A reset this host's clock already reads as past is waited the minute too. Zero for an
// answer that is not a rate limit (limited, rateLimited).
func retryAfter(response *http.Response, limited bool, now time.Time) time.Duration {
	if !limited {
		return 0
	}
	headers := response.Header
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
// out; a later page GetPages read is named by its number under the caller's path, laterPage),
// GitHub's HTTP status, the body GitHub sent with it as the rule prints it, the rate-limit
// headers GitHub sent with it (Limit), and whether it is a rate limit, which every further call on
// the same token meets until the limit resets, with how long GitHub asks the caller to wait before
// its next call (retryAfter; zero when it is not one) and the budget the limit spent (empty when
// it is not one). Body is for what the answer prints - a message, a log line, an error - and
// nothing decides on it: a caller decides on GitHub's words as they came, through Says and Decode.
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
// these decide nothing, and are kept so that a log of a refusal names the limit that refused it as
// GitHub sent it, including where it disagrees with the body. A header is the answering party's
// own words, so each is kept as the number it is written back from where it parses as one (an
// HTTP date for Retry-After), and otherwise as the rule reads it (oauth2call.Header): a
// resource's name, and anything a stand-in wrote in GitHub's place.
type LimitHeaders struct {
	RetryAfter string
	Limit      string
	Remaining  string
	Reset      string
	Resource   string
}

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
