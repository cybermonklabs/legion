package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Delivery is one attempt GitHub recorded for the App's webhook
// (`GET /app/hook/deliveries`). A redelivery is a new attempt under the original GUID.
type Delivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Redelivery  bool      `json:"redelivery"`
	Status      string    `json:"status"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
	// Action is empty for events that carry none (GitHub serves null).
	Action       string `json:"action"`
	RepositoryID int64  `json:"repository_id"`
}

// Deliveries lists every attempt the App webhook made at or after since, newest first, whatever
// its status: GitHub's own redelivery script needs them all, since a GUID with an OK attempt is
// delivered whatever its failures, and GitHub's status=failure filter takes only status codes from
// 400 to 599, which leaves out an attempt that got no HTTP answer. It follows the Link header's
// cursor from page to page and stops at the first attempt older than since. GitHub keeps three
// days of attempts; nothing older is listed whatever since says. A rate-limited answer is a
// *RateLimitError. before, when set, runs ahead of each page's request, and its error ends the
// listing: the sweep uses it to check a shared rate limit before every request.
func (c *Client) Deliveries(ctx context.Context, since time.Time, before func() error) ([]Delivery, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	var listed []Delivery
	target := c.base + "/app/hook/deliveries?per_page=100"
	for target != "" {
		if before != nil {
			if err := before(); err != nil {
				return nil, err
			}
		}
		jwt, err := c.appJWT()
		if err != nil {
			return nil, err
		}
		body, status, header, err := c.request(ctx, AppScope, http.MethodGet, target, "Bearer "+jwt, mediaTypeJSON, "", nil, responseLimit)
		if err != nil {
			return nil, err
		}
		if limited := RateLimit(status, header, body); limited != nil {
			return nil, limited
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d: %s", target, status, body)
		}
		next, err := nextLink(header.Get("Link"), c.base)
		if err != nil {
			return nil, err
		}
		var page []Delivery
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode webhook deliveries: %w", err)
		}
		for _, delivery := range page {
			if delivery.DeliveredAt.Before(since) {
				return listed, nil
			}
			listed = append(listed, delivery)
		}
		target = next
	}
	return listed, nil
}

// ErrNoDelivery is GitHub's 404 for a redelivery request: it no longer has the delivery (GitHub
// keeps a delivery three days), so no later request can succeed.
var ErrNoDelivery = errors.New("GitHub no longer has the delivery")

// Redeliver asks GitHub to attempt a recorded delivery again
// (`POST /app/hook/deliveries/{id}/attempts`). GitHub answers 202 and delivers asynchronously;
// the outcome appears in the listing as a new attempt under the delivery's GUID. A rate-limited
// answer is a *RateLimitError, and a 404 is ErrNoDelivery.
func (c *Client) Redeliver(ctx context.Context, deliveryID int64) error {
	if c == nil {
		return ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return err
	}
	target := c.base + "/app/hook/deliveries/" + strconv.FormatInt(deliveryID, 10) + "/attempts"
	body, status, header, err := c.request(ctx, AppScope, http.MethodPost, target, "Bearer "+jwt, mediaTypeJSON, "", nil, responseLimit)
	if err != nil {
		return err
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return limited
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("POST %s: status %d: %w", target, status, ErrNoDelivery)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("POST %s: status %d: %s", target, status, body)
	}
	return nil
}

// RateLimitError is GitHub refusing a request because the App is over a rate limit. Wait is how
// long GitHub asks for before the next request. GitHub warns that requests made while limited
// may get the integration banned.
//
// A held error is a limit already recorded for Scope in the limits every process shares
// (Limits): the request was never sent, Status and Body are empty, Wait is what the recorded
// limit has left, and Recorded is when that limit was last recorded.
//
// A held error's text names the limit by Scope and Recorded, never by Wait: a caller that keeps
// the text and compares it with the last one (the architecture importer's one sync_failed event
// per failure) reads one limit as one failure however much of it has passed, and a new or
// extended limit as a new failure. Anything that needs the remaining wait reads Wait.
type RateLimitError struct {
	Status   int
	Wait     time.Duration
	Body     string
	Scope    string
	Held     bool
	Recorded time.Time
}

func (e *RateLimitError) Error() string {
	if e.Held {
		return fmt.Sprintf("GitHub rate limit on %s recorded at %s is in force; the request was not sent",
			e.Scope, e.Recorded.UTC().Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("rate-limited by GitHub (status %d), next request in %s: %s", e.Status, e.Wait, e.Body)
}

// AsRateLimit is errors.As(err, &limited) collapsed to one line: delivery's reconcile.go,
// github_prs.go and intake.go each repeated the same three-line var/errors.As idiom at every
// point a *RateLimitError must be told apart from every other per-item failure.
func AsRateLimit(err error) (*RateLimitError, bool) {
	var limited *RateLimitError
	ok := errors.As(err, &limited)
	return limited, ok
}

// RateLimit reads a rate-limited answer, as GitHub's REST rate-limit documentation describes one:
// a 429, or a 403 with no requests remaining, a Retry-After, or a message naming a rate limit
// (any other 403 is a refusal of that request). The wait is Retry-After's seconds, else the time
// until x-ratelimit-reset when no requests remain, else the one minute GitHub asks for when it
// gives neither. Exported so other GitHub-calling packages (delivery's reconcile) can reuse the
// same detection this client already has, instead of treating every non-2xx the same.
func RateLimit(status int, header http.Header, body []byte) *RateLimitError {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return nil
	}
	exhausted := header.Get("X-Ratelimit-Remaining") == "0"
	if status == http.StatusForbidden && header.Get("Retry-After") == "" && !exhausted && !containsFold(body, rateLimitPhrase) {
		return nil
	}
	return &RateLimitError{Status: status, Wait: rateLimitWait(header, exhausted), Body: string(body)}
}

// GraphQLRateLimit reads a rate-limited answer to a GraphQL query, which GitHub spells four
// different ways and none of them the way it spells a rate-limited REST call, and says which of
// GitHub's two limits it is. A query past the App's primary GraphQL budget is answered HTTP 200
// with an `errors` entry of type RATE_LIMITED and a null `data`, so a caller reading the status
// alone sees a good answer and a caller reading `data` alone sees an empty one. The request that
// spends the last point is answered with its data and `x-ratelimit-remaining: 0`. A secondary
// limit -- too many points a minute, too many queries at once -- is answered either 200 or 403,
// with a `retry-after` header, or with a message about exceeding a secondary rate limit and no
// header at all.
//
// All four are read here, at either status: an `errors` entry of type RATE_LIMITED, no requests
// remaining, a `retry-after`, or a secondary-limit message in an errors entry. The wait is
// Retry-After's seconds when GitHub gives them, else the time until x-ratelimit-reset when no
// requests remain, else the minute GitHub asks for when it gives neither. Every other status is
// read as RateLimit reads it on a REST path, a 429 among them, and the envelope and the headers
// then say which limit that answer is.
//
// Nothing here is read from a repository's own data. The answer to a query is the data GitHub
// holds, and that data is the names people give their check runs and their pull requests: a body
// searched whole for GitHub's secondary-limit wording calls a perfectly good page of a
// repository that holds a check run of that name a rate limit, every pass, so the cursor never
// moves past that page and the heads behind it are never reconciled. The entries of `errors` and
// the response headers are GitHub's own envelope and are the whole of what is read.
//
// secondary tells the two limits apart, which the caller records under different scopes
// (Client.GraphQL): GitHub budgets the App's GraphQL points apart from its REST calls, while the
// secondary limits are shared between the two. GitHub's own wording decides it where the answer
// carries it, since GitHub writes that sentence for nothing else; then the two statements that
// the primary budget is spent, the RATE_LIMITED entry and no requests remaining; and a
// `retry-after`, or the 429 GitHub answers too many requests with, where neither of those says
// the budget is spent.
func GraphQLRateLimit(status int, header http.Header, body []byte) (limited *RateLimitError, secondary bool) {
	spent, worded := graphQLLimitErrors(body)
	exhausted := header.Get("X-Ratelimit-Remaining") == "0"
	retryAfter := header.Get("Retry-After") != ""
	switch {
	case status == http.StatusOK:
		if !spent && !worded && !exhausted && !retryAfter {
			return nil, false
		}
		limited = &RateLimitError{Status: status, Wait: rateLimitWait(header, exhausted), Body: string(body)}
	default:
		limited = RateLimit(status, header, body)
		if limited == nil && (spent || worded) {
			// A limit GitHub answered as GraphQL answers one -- a 403 carrying the error entry
			// or the wording and no header at all -- which the REST reader does not see.
			limited = &RateLimitError{Status: status, Wait: rateLimitWait(header, exhausted), Body: string(body)}
		}
		if limited == nil {
			return nil, false
		}
	}
	switch {
	case worded:
		return limited, true
	case spent || exhausted:
		return limited, false
	default:
		return limited, retryAfter || status == http.StatusTooManyRequests
	}
}

// graphQLLimitErrors reads a GraphQL answer's `errors` for the two things GitHub says a rate
// limit with there: an entry of type RATE_LIMITED, which is the App's GraphQL budget spent, and
// an entry whose message carries GitHub's secondary-limit wording, "You have exceeded a
// secondary rate limit", which is all a secondary limit gives when it comes with no header and
// no error type. The message is matched on the phrase alone, since GitHub has worded the rest of
// the sentence more than one way.
//
// A body that is not the envelope at all -- which cannot be a 200 from GitHub's GraphQL endpoint
// -- holds no such entry and is no limit: the caller's own decode then reports it as the
// malformed answer it is.
//
// The bytes are searched for the two before the body is parsed at all. Every good answer comes
// through here, and a good answer is a page of up to the megabyte this client reads: parsing
// each of them whole to look for entries almost none of them carries was the cost of the one
// that does. A body neither of them is anywhere in holds no such entry, and a body one of them
// is in is parsed, since only the entries of `errors` say whether GitHub wrote it or a
// repository did.
func graphQLLimitErrors(body []byte) (spent, secondary bool) {
	if !bytes.Contains(body, graphQLRateLimitedType) && !containsFold(body, secondaryRateLimitPhrase) {
		return false, false
	}
	var answer struct {
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, false
	}
	for _, entry := range answer.Errors {
		spent = spent || entry.Type == "RATE_LIMITED"
		// The entries of one answer are a handful of short messages, which this copies to
		// search; the megabyte of data beside them is what the check above spares.
		secondary = secondary || containsFold([]byte(entry.Message), secondaryRateLimitPhrase)
	}
	return spent, secondary
}

// What the readers above look for: the two phrases GitHub writes in a sentence of its own
// wording, each lower case and each matched in whatever case it is spelled, and the enum an
// errors entry carries verbatim.
var (
	rateLimitPhrase          = []byte("rate limit")
	secondaryRateLimitPhrase = []byte("secondary rate limit")
	graphQLRateLimitedType   = []byte("RATE_LIMITED")
)

// containsFold reports whether body holds phrase, which must be lower-case ASCII, in whatever
// case it is written, and copies nothing. Every answer a limit is read from runs through it,
// most of them good ones of up to this client's megabyte, and bytes.ToLower over such a body
// allocated and copied the whole of it to find a phrase that is almost never there. The body is
// scanned for the phrase's first byte in either case instead and only those windows compared,
// which on a megabyte of answer measured about half the time the copy alone took.
func containsFold(body, phrase []byte) bool {
	first := string([]byte{phrase[0], phrase[0] - 'a' + 'A'})
	for from := 0; from+len(phrase) <= len(body); from++ {
		at := bytes.IndexAny(body[from:len(body)-len(phrase)+1], first)
		if at < 0 {
			return false
		}
		from += at
		if bytes.EqualFold(body[from:from+len(phrase)], phrase) {
			return true
		}
	}
	return false
}

// rateLimitWait is how long GitHub asks for before the next request under a limit it answered
// with: Retry-After's seconds, else the time until x-ratelimit-reset when the answer says no
// requests remain, else the one minute GitHub asks for when it gives neither. A reset already
// past is no wait at all rather than a negative one.
func rateLimitWait(header http.Header, exhausted bool) time.Duration {
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil {
		return time.Duration(seconds) * time.Second
	}
	if reset, err := strconv.ParseInt(header.Get("X-Ratelimit-Reset"), 10, 64); err == nil && exhausted {
		return max(time.Until(time.Unix(reset, 0)), 0)
	}
	return time.Minute
}

// CheckResponse turns a response's status/header/body into an error only when status is not 200:
// RateLimit's own *RateLimitError when the response is a GitHub rate limit, else a generic
// "status %d: %s" error carrying the raw body. Factors the identical "check rate limit, then
// check status" three lines delivery's GitHub fetchers (github_prs.go, github_runs.go,
// client.go's own ListInstallationRepositoriesByID) all repeated at every call site.
func CheckResponse(status int, header http.Header, body []byte) error {
	if status == http.StatusOK {
		return nil
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return limited
	}
	return fmt.Errorf("status %d: %s", status, body)
}

// nextLink returns the rel="next" target of an RFC 8288 Link header, refusing one outside the
// client's API origin: the next page is fetched with the App's JWT.
func nextLink(header, base string) (string, error) {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		isNext := false
		for _, param := range segments[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		target := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(segments[0]), "<"), ">")
		parsed, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("parse next page link %q: %w", target, err)
		}
		origin, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("parse API base %q: %w", base, err)
		}
		if parsed.Scheme != origin.Scheme || parsed.Host != origin.Host {
			return "", fmt.Errorf("next page link %q leaves the API origin %s", target, base)
		}
		return target, nil
	}
	return "", nil
}
