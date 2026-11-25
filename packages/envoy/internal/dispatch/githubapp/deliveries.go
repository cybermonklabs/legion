package githubapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/serverwait"
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
	target, err := c.target("/app/hook/deliveries?per_page=100")
	if err != nil {
		return nil, err
	}
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
			return nil, fmt.Errorf("GET %s: %s", ShownBody(target), StatusText(status, body.Words().String()))
		}
		next, err := nextLink(header.Get("Link"), c.base.String())
		if err != nil {
			return nil, err
		}
		var page []Delivery
		if err := body.Decode(&page); err != nil {
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
	target, err := c.target("/app/hook/deliveries/" + strconv.FormatInt(deliveryID, 10) + "/attempts")
	if err != nil {
		return err
	}
	body, status, header, err := c.request(ctx, AppScope, http.MethodPost, target, "Bearer "+jwt, mediaTypeJSON, "", nil, responseLimit)
	if err != nil {
		return err
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return limited
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("POST %s: status %d: %w", ShownBody(target), status, ErrNoDelivery)
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("POST %s: %s", ShownBody(target), StatusText(status, body.Words().String()))
	}
	return nil
}

// RateLimitError is GitHub refusing a request because the App is over a rate limit. Wait is how
// long GitHub asks for before the next request. GitHub warns that requests made while limited
// may get the integration banned.
//
// Body is GitHub's refusal, the body it answered the request with as the rule prints it
// (redact.Answer.Words: with every credential the request carried redacted), and the error's text
// carries it only at a status other than 2xx, as StatusText does: a 2xx body is the data the
// request asked for, which can be a private repository's, and the text reaches GET /healthz as a
// cause however a loop wraps it. At a 2xx the text carries wording in Body's place, whoever builds
// the error: GitHub's wording of the limit, which answeredLimit alone sets, leaving Body empty
// there.
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
	// wording is GitHub's wording of a limit answered at a 2xx status (graphQLLimitWording),
	// which answeredLimit alone sets.
	wording string
	// message is the `message` of the answer's JSON, decoded from the answer as it came with the
	// record of its request (answerMessage), which answeredLimit alone sets: a body past the rule's
	// reach keeps its message, which Body, the rule's reading of the whole body, may not parse to.
	message redact.RemoteText
}

func (e *RateLimitError) Error() string {
	if e.Held {
		return fmt.Sprintf("GitHub rate limit on %s recorded at %s is in force; the request was not sent",
			e.Scope, e.Recorded.UTC().Format(time.RFC3339Nano))
	}
	if e.Status/100 == 2 {
		return withBody(limitAnswered(e.Status, e.Wait), e.wording)
	}
	return withBody(limitAnswered(e.Status, e.Wait), e.Body)
}

// limitAnswered is the text of a limit GitHub answered with status and asked wait for, up to its
// body: the head of a RateLimitError's text, and of the cause the client notes for such a limit
// (failedAnswer).
func limitAnswered(status int, wait time.Duration) string {
	return fmt.Sprintf("rate-limited by GitHub (status %d), next request in %s", status, wait)
}

// Message is the `message` of the JSON GitHub answers a refused request with, as the rule prints
// it, and "" for a body that is not JSON or names none, as a held error's empty Body does, and so
// does the empty Body of a 2xx limit that answeredLimit built. A limit answeredLimit built from an
// answer names the message it decoded from that answer (answerMessage); one built with a Body
// alone names the message of that text (responseMessage).
func (e *RateLimitError) Message() string {
	if !e.message.IsEmpty() {
		return e.message.String()
	}
	return responseMessage([]byte(e.Body))
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
// (any other 403 is a refusal of that request). The wait is Retry-After's, in seconds or as an
// HTTP date, else the time until x-ratelimit-reset when no requests remain, else the one minute
// GitHub asks for when it gives neither (rateLimitWait). Exported so other GitHub-calling packages
// (delivery's reconcile) can reuse the same detection this client already has, instead of treating
// every non-2xx the same.
func RateLimit(status int, header http.Header, body redact.Answer) *RateLimitError {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return nil
	}
	exhausted := header.Get("X-Ratelimit-Remaining") == "0"
	if status == http.StatusForbidden && header.Get("Retry-After") == "" && !exhausted && !redact.Holds(body, func(b []byte) bool { return redact.ContainsFold(b, rateLimitPhrase) }) {
		return nil
	}
	return answeredLimit(status, header, body)
}

// answeredLimit is the RateLimitError of a limit GitHub answered with status, header and body, and
// the one constructor of such an error (RateLimit, GraphQLRateLimit). Its Body is body, GitHub's
// refusal as the rule prints it, at every status but a 2xx, and at a 2xx it is empty and wording
// is GitHub's wording of the limit read from the envelope around the data or from header
// (graphQLLimitWording), since such a body is the data the request asked for and no text of the
// error may carry it.
func answeredLimit(status int, header http.Header, body redact.Answer) *RateLimitError {
	limited := &RateLimitError{Status: status, Wait: rateLimitWait(header)}
	if status/100 == 2 {
		limited.wording = graphQLLimitWording(header, body)
	} else {
		limited.Body = body.Words().String()
		limited.message = answerMessage(body)
	}
	return limited
}

// GraphQLRateLimit reads a rate-limited answer to a GraphQL query, which GitHub spells several
// ways and none of them the way it spells a rate-limited REST call, and says which of GitHub's
// two limits it is. A query past the App's primary GraphQL budget is answered HTTP 200 with an
// `errors` entry of type RATE_LIMITED and a null `data`, so a caller reading the status alone
// sees a good answer and a caller reading `data` alone sees an empty one. The request that
// spends the last point is answered with its data and `x-ratelimit-remaining: 0`. A secondary
// limit -- too many points a minute, too many queries at once -- is answered either 200 or 403,
// with a `retry-after` header, or with GitHub's wording about a secondary rate limit and no
// header at all. That wording comes in an `errors` entry, and at 403 in the REST-shaped body
// whose top-level `message` carries it, which GitHub answers a query with as readily as it
// answers a REST call with one (graphQLLimitErrors).
//
// Every one of them is read here, at either status, and the wait is Retry-After's when GitHub
// gives one, else the time until x-ratelimit-reset when no requests remain, else the minute GitHub
// asks for when it gives neither (rateLimitWait).
//
// RateLimit reads the answer first: it covers the two statuses GitHub answers a REST-style
// limit with, 403 and 429, and answers nil at every other status, 200 among them. What it
// cannot see is a limit GitHub spells in the envelope alone -- the RATE_LIMITED entry, or the
// secondary wording with no header beside it -- so the error is built here, once, for an answer
// it read as no limit that the query's own signals say is one. A header alone counts only at
// 200, the status GitHub answers the query itself with: at any other status GitHub is refusing
// the request rather than answering it, and the headers on that refusal say nothing about the
// budget. Either way answeredLimit builds the error, so a limit read in a 200 has an empty Body
// and a text that carries GitHub's wording of the limit and nothing of the query's data.
//
// Nothing here is read from a repository's own data. The answer to a query is the data GitHub
// holds, and that data is the names people give their check runs and their pull requests: a body
// searched whole for GitHub's secondary-limit wording calls a perfectly good page of a
// repository that holds a check run of that name a rate limit, every pass, so the cursor never
// moves past that page and the heads behind it are never reconciled. GitHub's own envelope is
// the entries of `errors`, the top-level `message` of the REST-shaped body it refuses a request
// with, and the response headers, and those are the whole of what is read.
//
// secondary tells the two limits apart, which the caller records under different scopes
// (Client.GraphQL): GitHub budgets the App's GraphQL points apart from its REST calls, while the
// secondary limits are shared between the two. GitHub's own wording decides it wherever the
// answer carries it, since GitHub writes that sentence for nothing else; then the two statements
// that the primary budget is spent, the RATE_LIMITED entry and no requests remaining; and a
// `retry-after`, or the 429 GitHub answers too many requests with, where neither of those says
// the budget is spent.
func GraphQLRateLimit(status int, header http.Header, body redact.Answer) (limited *RateLimitError, secondary bool) {
	spent, worded := graphQLLimitErrors(body)
	exhausted := header.Get("X-Ratelimit-Remaining") == "0"
	retryAfter := header.Get("Retry-After") != ""
	limited = RateLimit(status, header, body)
	if limited == nil {
		if !spent && !worded && (status != http.StatusOK || (!exhausted && !retryAfter)) {
			return nil, false
		}
		limited = answeredLimit(status, header, body)
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

// graphQLLimitErrors reads a GraphQL answer for the two things GitHub says a rate limit with in
// its own envelope: an entry of `errors` whose type is RATE_LIMITED, which is the App's GraphQL
// budget spent, and GitHub's secondary-limit wording, "You have exceeded a secondary rate
// limit", which is all a secondary limit gives when it comes with no header and no error type.
// The wording is read from an entry's message and from the body's own top-level `message`,
// because GitHub answers a secondary limit at 403 with the REST-shaped body that carries it
// there and no `errors` at all, and a reader of the entries alone takes such an answer for the
// App's spent GraphQL budget and records it under the wrong scope. It is matched on the phrase
// alone, since GitHub has worded the rest of the sentence more than one way.
//
// `data` is a repository's own, and is read for neither: a body that is not the envelope at all
// -- which cannot be a 200 from GitHub's GraphQL endpoint -- holds no such entry and no such
// message, and is no limit, so the caller's own decode reports it as the malformed answer it is.
//
// The bytes are searched for the two before the body is parsed at all (holds), because every good
// answer comes through here and a good answer is a page of up to the megabyte this client
// reads. A body neither of them is anywhere in holds no such entry and no such message, and a
// body one of them is in is parsed, since only GitHub's own envelope says whether GitHub wrote
// the words or a repository did.
func graphQLLimitErrors(body redact.Answer) (spent, secondary bool) {
	answer, ok := readLimitEnvelope(body)
	if !ok {
		return false, false
	}
	// The message and the entries of one answer are a handful of short strings, which this
	// copies to search; the megabyte of data beside them is what the check before the parse
	// spares.
	secondary = secondaryWording(answer.Message)
	for _, entry := range answer.Errors {
		spent = spent || entry.spent()
		secondary = secondary || secondaryWording(entry.Message)
	}
	return spent, secondary
}

// limitEnvelope is GitHub's own envelope of an answer, the whole of what is read from a body a
// GraphQL limit may be spelled in: the top-level message of the body GitHub refuses a request
// with, which no answer to a query carries, and the entries of `errors`. A repository's own data
// is under `data` and is read for nothing. It is read to decide alone, and prints nowhere: what of
// it a text carries is its limitWords.
type limitEnvelope struct {
	Message string       `json:"message"`
	Errors  []limitEntry `json:"errors"`
}

// limitEntry is one entry of an answer's `errors`.
type limitEntry struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// limitWords is limitEnvelope as GitHub's words, each held with the request's record
// (redact.Answer.Decode), which is what graphQLLimitWording prints of the envelope.
type limitWords struct {
	Message redact.RemoteText `json:"message"`
	Errors  []limitEntryWords `json:"errors"`
}

// limitEntryWords is limitEntry as GitHub's words, held with the request's record.
type limitEntryWords struct {
	Type    redact.RemoteText `json:"type"`
	Message redact.RemoteText `json:"message"`
}

// spent is whether the entry says the App's GraphQL budget is spent: its type is RATE_LIMITED.
func (e limitEntry) spent() bool { return e.Type == graphQLRateLimited }

// secondaryWording is whether message, an entry's or the envelope's own, holds GitHub's wording of
// a secondary limit.
func secondaryWording(message string) bool {
	return redact.ContainsFold([]byte(message), secondaryRateLimitPhrase)
}

// readLimitEnvelope parses body's envelope when GitHub's RATE_LIMITED type or its secondary-limit
// phrase is anywhere in it (graphQLLimitErrors), and answers false for a body that holds neither
// or is no JSON.
func readLimitEnvelope(body redact.Answer) (limitEnvelope, bool) {
	if !redact.Holds(body, func(b []byte) bool {
		return bytes.Contains(b, graphQLRateLimitedType) || redact.ContainsFold(b, secondaryRateLimitPhrase)
	}) {
		return limitEnvelope{}, false
	}
	var answer limitEnvelope
	if err := body.Decode(&answer); err != nil {
		return limitEnvelope{}, false
	}
	return answer, true
}

// graphQLLimitWording is GitHub's own wording of a limit it answered a query with at a 2xx status:
// the wording answeredLimit gives that limit's RateLimitError in place of the answer's body, which
// is the query's data, and so the detail of every text the error or the client's note of it makes.
// It is what of the envelope names the limit, read where GitHub's type or phrase for a limit is
// anywhere in the body (readLimitEnvelope): the top-level message where it holds GitHub's
// secondary-limit wording, and each `errors` entry of type RATE_LIMITED or whose message holds that
// wording, as "<type>: <message>", joined by "; ". Any other entry -- an error GitHub answers
// beside its data, about a repository the query names -- is not the limit, and a cause that named
// it would name another failure than the one the request met. Where nothing names the limit --
// the answer that spends the last point carries its data and no error, and the type or phrase can
// be in that data -- it names the limit by the headers GitHub read it from instead,
// X-RateLimit-Remaining, X-RateLimit-Reset and Retry-After, each "<name>: <value>" as GitHub sent
// it, joined the same way. Nothing of `data` is read.
func graphQLLimitWording(header http.Header, body redact.Answer) string {
	var parts []string
	var words limitWords
	if answer, ok := readLimitEnvelope(body); ok && body.Decode(&words) == nil && len(words.Errors) == len(answer.Errors) {
		if secondaryWording(answer.Message) {
			parts = append(parts, words.Message.String())
		}
		for i, entry := range answer.Errors {
			if !entry.spent() && !secondaryWording(entry.Message) {
				continue
			}
			said := words.Errors[i]
			switch {
			case entry.Type != "" && entry.Message != "":
				parts = append(parts, said.Type.String()+": "+said.Message.String())
			case entry.Type != "":
				parts = append(parts, said.Type.String())
			default:
				parts = append(parts, said.Message.String())
			}
		}
	}
	if len(parts) == 0 {
		for _, name := range limitHeaders {
			if value := header.Get(name); value != "" {
				parts = append(parts, name+": "+value)
			}
		}
	}
	return strings.Join(parts, "; ")
}

// limitHeaders are the headers GitHub names a GraphQL limit with, as its documentation spells
// them.
var limitHeaders = []string{"X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"}

// What the readers above look for, in the bytes as they came (redact.Holds): the two phrases
// GitHub writes in a sentence of its own wording, each lower case and each matched in whatever
// case it is spelled (redact.ContainsFold), and the enum an errors entry carries verbatim.
const (
	rateLimitPhrase          = "rate limit"
	secondaryRateLimitPhrase = "secondary rate limit"
	graphQLRateLimited       = "RATE_LIMITED"
)

var graphQLRateLimitedType = []byte(graphQLRateLimited)

// rateLimitWait is how long GitHub asks for before the next request under a limit it answered
// with, read now through serverwait.Read, else the one minute GitHub asks for when the answer
// names no wait. Client.request records it in github_rate_limits under the call's scope, so a wait
// of an hour stops every Dispatch process's calls under that scope for that hour: every App-JWT
// call, for a limit an App-JWT call met.
func rateLimitWait(header http.Header) time.Duration {
	if wait, named := serverwait.Read(header, time.Now()); named {
		return wait
	}
	return time.Minute
}

// StatusError is GitHub answering a request with a status other than 200 that is no rate limit
// (CheckResponse): the status, and the whole body GitHub answered. Its text is StatusText's, which
// leaves out the body of an answer at a 2xx status and shows any other body as ShownBody does,
// with the body as it prints itself (redact.RemoteText), since it is GitHub's words: a URL in
// them prints with no user information and no query or fragment value, and a body the rule
// cannot read prints as redact.WordsNotReadable, under every verb, in JSON and in a log field.
type StatusError struct {
	Status  int
	body    redact.RemoteText
	message redact.RemoteText
}

func (e *StatusError) Error() string {
	return StatusText(e.Status, e.body.String())
}

// Format prints Error under the verb, flags, width and precision it is given, as fmt prints that
// string, so no verb prints this error's fields.
func (e *StatusError) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, fmt.FormatString(f, verb), e.Error())
}

// LogValue is Error, so a structured log field names the error only as a message may.
func (e *StatusError) LogValue() slog.Value { return slog.StringValue(e.Error()) }

// PrintedBody is the body as Error prints it (withBody: as the rule reads it, then as ShownBody
// shows it), for a caller that rebuilds a message from Error's text (results_ci.go's
// withGitHubMessage).
func (e *StatusError) PrintedBody() string { return ShownBody(e.body.String()) }

// Message is the `message` of the body, the JSON GitHub answers a refused request with, and empty
// for a body that is not JSON or names none (answerMessage). It prints itself as the body does.
func (e *StatusError) Message() redact.RemoteText { return e.message }

// shownBodyRunes is how many runes of GitHub's text a line or an error shows (ShownBody): the
// start of the text, where GitHub writes the `message` of a refusal, in a line of bounded length.
const shownBodyRunes = 512

// ShownBody is body, text GitHub gave, as a line or an error shows it: whole where it is at most
// shownBodyRunes runes long, and otherwise its first shownBodyRunes runes and `… (<n> bytes)`, n
// the length of the whole body. A line that logs an error writes the error's text whole, and a
// body may be as long as a response may be (responseLimit, 1 MiB), which put 1,048,697 bytes into
// one field of a WARN line (review of cybermonklabs/legion#4209 at def907052c, H3). So the errors
// of this package cut each text of GitHub's once, where it enters an error, and n is the size of
// the text that was cut:
//
//   - A refusal's body is cut alone, with the body's size. Every error of this package whose text
//     holds a body GitHub answered holds it as this shows it (withBody). A reader that finds the
//     body at the end of such a text (api's gitHubErrorText) finds it as this shows it.
//   - A transport error of send names its target as ShownBody shows it, with the target's size,
//     and then the URL it asked and its failure by redact.Unanswered: a transport failure's own
//     words or a fixed class, so it quotes no line of an answer at all.
//   - Any other error that quotes GitHub's text, such as a bucket error that quotes a key
//     (redeliver's bucketErr), is cut whole, with the size of its text (ShownErr).
//   - A request's target, which a status error names, is cut alone, with the target's size. So is
//     a next page link in nextLink's refusal. A target holds names Dispatch keeps and text GitHub
//     gave (a next page link).
//   - A URL's parse error whose text is still longer than shownErrMost (4,414 bytes) after the
//     cuts above is cut whole, with the size of that text (ShownErr).
//
// It reads at most shownBodyRunes × 4 bytes of body, the most that many runes take, and never
// splits a rune. A byte that is no UTF-8 counts as one rune.
func ShownBody[Body ~string | ~[]byte](body Body) string {
	head := string(body[:min(len(body), shownBodyRunes*utf8.UTFMax)])
	runes := 0
	for at := range head {
		if runes == shownBodyRunes {
			return fmt.Sprintf("%s… (%d bytes)", head[:at], len(body))
		}
		runes++
	}
	if len(head) < len(body) {
		return fmt.Sprintf("%s… (%d bytes)", head, len(body))
	}
	return head
}

// shownErrMost is the longest text ShownErr answers, 4,414 bytes: two texts as ShownBody shows
// them, and 256 bytes for the words of the error's type around them. A text ShownBody shows is at
// most 2,079 bytes: shownBodyRunes runes of at most utf8.UTFMax bytes each, and `… (<n> bytes)`
// for the largest n, 9223372036854775807 (math.MaxInt64).
const shownErrMost = 2*(shownBodyRunes*utf8.UTFMax+len("… (9223372036854775807 bytes)")) + 256

// ShownErr is err as the errors of this package show it, cut once where it enters such an error,
// and nil for nil. The text of its answer is never longer than shownErrMost. ShownErr is err
// itself where ShownBody shows each text in err whole and err's text is at most shownErrMost, so
// err keeps its own type and every method a caller type-asserts, such as net.Error's Timeout.
// Otherwise it cuts the text of GitHub's that err holds as ShownBody shows it. It reads the type
// of err itself (a type switch), not of the errors that err wraps:
//
//   - A *url.Error, the error of a URL that does not parse (send's build of a request of its
//     target, nextLink's parse of a next page link), has the text `<Op> "<URL>": <cause>`, with
//     the URL in a field of its own. ShownErr answers a new *url.Error with the same Op, the URL as
//     ShownBody shows it, with the URL's size, and the cause (Err) as ShownErr shows it, so
//     errors.As reaches the answer as it reaches err, and a long URL never pushes the cause out of
//     the text. Where that text is still longer than shownErrMost, ShownErr cuts it whole, as
//     below, with the size of that text.
//   - Any other error is cut whole: ShownErr answers an error whose text is err's text as
//     ShownBody shows it, with the size of err's text, and whose Unwrap is err (shownError). So
//     errors.Is and errors.As still reach err, and Timeout and Temporary still answer through err's
//     chain. Go's own words take part of the shownBodyRunes runes there.
func ShownErr(err error) error {
	shown, _ := shownErr(err)
	return shown
}

// shownErr is ShownErr(err), and cut is whether shown is an error other than err. shown starts as
// err, and the nil case answers nil and false. The *url.Error case replaces shown only where it
// cuts a field, and ends at the check of shownErrMost after the switch. It reads cut for its cause
// and never compares two errors, since == panics on two errors of one type that Go cannot compare.
func shownErr(err error) (shown error, cut bool) {
	shown = err
	switch e := err.(type) {
	case nil:
		return nil, false
	case *url.Error:
		cause, causeCut := shownErr(e.Err)
		if target := ShownBody(e.URL); causeCut || target != e.URL {
			shown, cut = &url.Error{Op: e.Op, URL: target, Err: cause}, true
		}
	default:
		text := err.Error()
		if cutText := ShownBody(text); cutText != text {
			return &shownError{err: err, text: cutText}, true
		}
		return err, false
	}
	if text := shown.Error(); len(text) > shownErrMost {
		return &shownError{err: err, text: ShownBody(text)}, true
	}
	return shown, cut
}

// shownError is an error ShownErr cut whole: text is, as ShownBody shows it, err's text, or the
// text of err with its fields cut where that text is longer than shownErrMost. ShownErr makes the
// text once, when it makes the error. Unwrap is err.
type shownError struct {
	err  error
	text string
}

func (e *shownError) Error() string { return e.text }

func (e *shownError) Unwrap() error { return e.err }

// Timeout is what the first error in err's chain that has a Timeout method answers, and false
// where no error in the chain has one. So a cut error answers net.Error's Timeout as err does. A
// url.Error whose Err is a cut error also answers it: url.Error's Timeout asks only its Err, by a
// type assertion, and does not read the chain.
func (e *shownError) Timeout() bool {
	var timeout interface{ Timeout() bool }
	return errors.As(e.err, &timeout) && timeout.Timeout()
}

// Temporary is what the first error in err's chain that has a Temporary method answers, and false
// where no error in the chain has one, for the same reason as Timeout.
func (e *shownError) Temporary() bool {
	var temporary interface{ Temporary() bool }
	return errors.As(e.err, &temporary) && temporary.Temporary()
}

// StatusText is the text of an answer GitHub gave with status and body that its reader takes as
// a failure: `status <n>: <body>`, or `status <n>` alone for an answer with no body (withBody)
// and for every answer at a 2xx status, whatever its body. A 2xx body is the data the request
// asked for, not GitHub's refusal of it -- the token mint expects 201, so a 200 there would carry
// an installation token -- and a loop notes such a text as its cause, which GET /healthz serves
// with no credential. The status alone says what went wrong, an answer other than the one its
// reader expected, so no marker stands in for the body, and every text that carries no body has
// the one shape. Every error of this package that words such an answer words it so, and so does
// any other package's (cireconcile's query).
func StatusText[Body string | []byte](status int, body Body) string {
	head := "status " + strconv.Itoa(status)
	if status/100 == 2 {
		return head
	}
	return withBody(head, body)
}

// withBody is head, then `: ` and body as ShownBody shows it when body is not empty: the one rule
// of every text of an answer this package words (StatusText, RateLimitError, failedAnswer). An
// answer with no body ends at its head, since a loop notes such a text as it is and GET /healthz
// serves it as a read's cause, where a `: ` with nothing after it says nothing. A body is bounded
// because a response may hold 1 MiB, and a line that logs the text writes it whole.
func withBody[Body string | []byte](head string, body Body) string {
	if len(body) == 0 {
		return head
	}
	return head + ": " + ShownBody(body)
}

// CheckResponse turns a response's status/header/body into an error only when status is not 200:
// RateLimit's own *RateLimitError when the response is a GitHub rate limit, else a *StatusError
// carrying the status and the body (NewStatusError). The body is a redact.Answer, which only
// redact.ReadAnswer makes, so no caller can hand it bytes it did not read through the reader.
// Factors the identical "check rate limit, then check status" three lines delivery's GitHub
// fetchers (github_prs.go, github_runs.go, client.go's own ListInstallationRepositoriesByID) all
// repeated at every call site.
func CheckResponse(status int, header http.Header, body redact.Answer) error {
	if status == http.StatusOK {
		return nil
	}
	if limited := RateLimit(status, header, body); limited != nil {
		return limited
	}
	return NewStatusError(status, body)
}

// NewStatusError is GitHub's answer with status and body as a *StatusError: the body and its
// `message` held as GitHub's words with the request's record (redact.Answer.Words, Decode), and
// the one way to make the error.
func NewStatusError(status int, body redact.Answer) *StatusError {
	return &StatusError{Status: status, body: body.Words(), message: answerMessage(body)}
}

// nextLinkMaxBytes is the longest next page link nextLink follows. GitHub's links of the deliveries
// listing are about a hundred bytes. A link is GitHub's text, and once followed it is the target of
// the next request and of each error that names that request, so a longer one is refused and shown
// as ShownBody shows GitHub's text.
const nextLinkMaxBytes = 2048

// nextLink returns the rel="next" target of an RFC 8288 Link header, refusing one outside the
// client's API origin, since the next page is fetched with the App's JWT, and one longer than
// nextLinkMaxBytes. Each refusal names the link as ShownBody shows it, and the error of a link
// that url.Parse cannot parse as ShownErr shows it, since that error quotes the link again.
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
		if len(target) > nextLinkMaxBytes {
			return "", fmt.Errorf("next page link %q is longer than %d bytes", ShownBody(target), nextLinkMaxBytes)
		}
		parsed, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("parse next page link %q: %w", ShownBody(target), ShownErr(err))
		}
		origin, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("parse API base %q: %w", base, err)
		}
		if parsed.Scheme != origin.Scheme || parsed.Host != origin.Host {
			return "", fmt.Errorf("next page link %q leaves the API origin %s", ShownBody(target), base)
		}
		return target, nil
	}
	return "", nil
}
