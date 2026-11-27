// Package githubrate is the GitHub rate reading the Legion daemon and Dispatch both log: one JSON
// line, github_rate_reading, for each installation of a GitHub App, with the used count, the limit
// and the reset of the installation's REST core counter and its GraphQL counter, and the count of
// the program's own calls on each counter since that counter's last reset, by caller kind.
//
// GitHub budgets each installation, not each program, and gives no breakdown by caller. Its used
// counts are REST requests or GraphQL points; the own counts are answered calls (Counter), a 304
// Not Modified, which GitHub does not charge, aside. A GraphQL call can cost several points, and a
// refused REST call can cost nothing, so subtracting the own counts does not measure the use of
// every other holder.
//
// It is a package of its own, outside internal/, because the daemon (packages/daemon, module
// github.com/sjawhar/legion/daemon) is a Go module of its own, and Go's internal rule bars it from
// this module's internal packages. It holds standard library code and the redaction rule's
// reader of an answer's body (internal/redact), which the daemon reaches through oauth2call
// already.
package githubrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/redact"
)

// Interval is how often a program reads each installation. LastMinute is the window in which a
// reading is the counter's last reading before its reset, and BeforeReset is the aim within that
// window (Next), leaving room for the pass and for drift in GitHub's reset headers.
const (
	Interval    = 10 * time.Minute
	LastMinute  = time.Minute
	BeforeReset = 45 * time.Second
)

// resetDrift separates a new hourly window from clock drift in GitHub's reset headers. Live
// answers for one installation named resets a second apart within the same window, so any later
// reset is not enough to start counting again. Five minutes leaves room for that drift without
// joining the resets of two hours.
const resetDrift = 5 * time.Minute

// Msg is the message of the reading line, the name a log query filters on.
const Msg = "github_rate_reading"

// Other is the caller kind of a call whose context no caller tagged (CallerOf).
const Other = "other"

// The two counters a reading reads, by the name GitHub's X-RateLimit-Resource header gives each.
const (
	Core    = "core"
	GraphQL = "graphql"
)

type callerKey struct{}

// WithCaller tags ctx's calls to GitHub as the caller kind kind: one code path that calls GitHub,
// such as a sweep, a reconcile pass or the reading itself. A Counter's Transport counts each call
// under the kind its request's context carries.
func WithCaller(ctx context.Context, kind string) context.Context {
	return context.WithValue(ctx, callerKey{}, kind)
}

// CallerOf is the caller kind ctx carries (WithCaller), Other when no caller tagged it.
func CallerOf(ctx context.Context) string {
	if kind, _ := ctx.Value(callerKey{}).(string); kind != "" {
		return kind
	}
	return Other
}

// tokenLife is how long a registration names its token's installation: past the hour GitHub gives
// an installation token, so a token is counted for the whole of its life, and no registration of
// a token that can no longer call GitHub is kept beyond it.
const tokenLife = 2 * time.Hour

// rateLimitPaths are GitHub's GET /rate_limit at its API root and at a GitHub Enterprise Server's,
// whose REST root is /api/v3. The endpoint costs nothing (GitHub's rate-limit page: "Calling this
// endpoint does not count against your REST API rate limit"), so no call to it is counted. Only
// these two paths are the endpoint: a repository named rate_limit, GET /repos/OWNER/rate_limit, is
// a charged read like any other.
var rateLimitPaths = map[string]bool{"/rate_limit": true, "/api/v3/rate_limit": true}

// Counter counts the calls a program makes to GitHub with the installation tokens it registered,
// by installation, counter and caller kind, since each counter's last reset as GitHub's answers
// name it, and notes the secondary limits GitHub answers those tokens with (SecondaryUntil). It
// keeps the SHA-256 of each token it is given, never the token. It is the program's own: a
// secondary limit another program, or another process of the same program, met is not in it.
type Counter struct {
	mu        sync.Mutex
	now       func() time.Time
	tokens    map[[sha256.Size]byte]registration
	counts    map[window]*counted
	secondary map[string]time.Time
}

// registration is the installation a token was minted on, and when the Counter was told.
type registration struct {
	installation string
	at           time.Time
}

// window is one counter of one installation.
type window struct {
	installation, counter string
}

// counted is one window's counts by caller kind, and the reset GitHub named for them.
type counted struct {
	reset  time.Time
	byKind map[string]int
}

// NewCounter makes an empty Counter.
func NewCounter() *Counter {
	return &Counter{
		now: time.Now, tokens: map[[sha256.Size]byte]registration{}, counts: map[window]*counted{},
		secondary: map[string]time.Time{},
	}
}

// Register says token is an installation token of installation, so the calls made with it are
// counted under that installation. Each registration lasts past the life GitHub gives the token,
// and a registration past it is dropped here.
func (c *Counter) Register(token, installation string) {
	if token == "" || installation == "" {
		return
	}
	hash := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for held, registered := range c.tokens {
		if now.Sub(registered.at) > tokenLife {
			delete(c.tokens, held)
		}
	}
	c.tokens[hash] = registration{installation: installation, at: now}
}

// Transport is base, or http.DefaultTransport when base is nil, counting each answer to a request
// whose Authorization header carries a registered token, under the counter the answer's
// X-RateLimit-Resource names, core or graphql and no other, and the caller kind of the request's
// context (CallerOf). A reset more than resetDrift later starts the counts again; an answer from
// an earlier window is ignored, so concurrent requests that finish out of order do not mix hours.
// A request with no registered token, the App's JWT among them, is never counted; neither is
// GitHub's GET /rate_limit endpoint (rateLimitPaths), which costs nothing, nor an answer of 304 Not
// Modified, which GitHub charges an authenticated request nothing for. It also notes each
// secondary limit GitHub answers a registered token with (SecondaryUntil).
func (c *Counter) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return countingTransport{counter: c, base: base}
}

type countingTransport struct {
	counter *Counter
	base    http.RoundTripper
}

func (t countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
		t.counter.noteSecondary(request, response)
	}
	if response.StatusCode != http.StatusNotModified && !rateLimitPaths[request.URL.Path] {
		t.counter.count(request, response.Header)
	}
	return response, nil
}

// longestSecondaryWait bounds the wait a secondary limit is noted for. GitHub counts its limits
// within the hour, so an answer asking for longer is read as an hour, never as no wait.
const longestSecondaryWait = time.Hour

// limitPeek is how much of a refusal's body is read for GitHub's wording of a rate limit: its
// refusal is a short JSON message, and the rest of the body is the caller's whether or not it was
// read.
const limitPeek = 64 << 10

// secondaryWording is what GitHub's secondary limit says in its refusal's message: "You have
// exceeded a secondary rate limit", and the older wording of the same limit, "You have triggered
// an abuse detection mechanism". GitHub writes those sentences for nothing else.
var secondaryWording = []string{"secondary rate limit", "abuse detection mechanism"}

// noteSecondary notes the secondary limit GitHub answered request with, a 403 or 429 refusal,
// under the installation of the registered token it carries, as GitHub's REST documentation
// describes one: a refusal whose message names the secondary limit, whatever
// X-RateLimit-Remaining says, as GitHub's own client (octokit's throttling plugin) reads it first;
// else, with requests remaining (X-RateLimit-Remaining is not 0), one that carries Retry-After, is
// a 429, or whose message names a rate limit. A refusal with no requests remaining and no such
// message is the primary limit, whose refusal still names the spent counter, and is not noted; a
// 403 that is none of these is GitHub refusing that request, not limiting the installation. The
// wait is Retry-After's, in seconds or as an HTTP date, else the minute GitHub asks for when it
// gives none, bounded by longestSecondaryWait, and the end noted is the latest any answer asked
// for. The head of the body is read for the message and put back whole for the caller.
func (c *Counter) noteSecondary(request *http.Request, response *http.Response) {
	token, ok := bearer(request.Header.Get("Authorization"))
	if !ok {
		return
	}
	hash := sha256.Sum256([]byte(token))
	c.mu.Lock()
	held, registered := c.tokens[hash]
	at := c.now()
	c.mu.Unlock()
	if !registered || at.Sub(held.at) > tokenLife {
		return
	}
	message := strings.ToLower(peekBody(response))
	named := false
	for _, wording := range secondaryWording {
		named = named || strings.Contains(message, wording)
	}
	retryAfter := response.Header.Get("Retry-After")
	switch {
	case named:
	case response.Header.Get("X-RateLimit-Remaining") == "0":
		return
	case retryAfter == "" && response.StatusCode == http.StatusForbidden && !strings.Contains(message, "rate limit"):
		return
	}
	until := at.Add(secondaryWait(retryAfter, at))
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.secondary[held.installation]) {
		c.secondary[held.installation] = until
	}
}

// peekBody is the head of response's body, at most limitPeek bytes, decided on as it came and
// printed nowhere. It is read through the answer's stream (redact.StreamAnswer), the one reader of
// a body besides redact.ReadAnswer, and that stream, the head put back in front of it, is the body
// the caller then reads whole through its own reader, which bounds it: the stream sets no bound
// of its own. A body that cannot be read is "".
func peekBody(response *http.Response) string {
	stream := redact.StreamAnswer(response, math.MaxInt64)
	head, err := io.ReadAll(io.LimitReader(stream, limitPeek))
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), stream), stream}
	if err != nil {
		return ""
	}
	return string(head)
}

// secondaryWait is the wait a secondary limit asked for at at: Retry-After's seconds, or its HTTP
// date, else a minute, bounded by longestSecondaryWait.
func secondaryWait(retryAfter string, at time.Time) time.Duration {
	wait := time.Minute
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		wait = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(retryAfter); err == nil {
		wait = date.Sub(at)
	}
	return min(max(wait, 0), longestSecondaryWait)
}

// SecondaryUntil is the end of the latest secondary limit GitHub answered one of installation's
// registered tokens with, and the zero time when none was. A time already past is a limit that
// ended; the caller compares it with its own clock. It is this Counter's alone, so it holds the
// limits the calls counted through it met: every client of a process that counts through the one
// Counter, and no other process.
func (c *Counter) SecondaryUntil(installation string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.secondary[installation]
}

// count counts one answer, headers, to request (Transport).
func (c *Counter) count(request *http.Request, headers http.Header) {
	token, ok := bearer(request.Header.Get("Authorization"))
	if !ok {
		return
	}
	resource := headers.Get("X-RateLimit-Resource")
	if resource != Core && resource != GraphQL {
		return
	}
	// Zero when the answer names no reset, which starts no window and keeps the one held.
	reset, _ := epoch(headers.Get("X-RateLimit-Reset"))
	kind := CallerOf(request.Context())
	// Hashed before the lock, so a count holds it for the map reads alone.
	hash := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	held, ok := c.tokens[hash]
	if !ok || c.now().Sub(held.at) > tokenLife {
		return
	}
	key := window{installation: held.installation, counter: resource}
	counts := c.counts[key]
	if counts == nil || (!reset.IsZero() && reset.Sub(counts.reset) > resetDrift) {
		// A new window: GitHub named a reset more than resetDrift past the one held.
		counts = &counted{reset: reset, byKind: map[string]int{}}
		c.counts[key] = counts
	} else if !reset.IsZero() {
		if counts.reset.Sub(reset) > resetDrift {
			return
		}
		if reset.After(counts.reset) {
			counts.reset = reset
		}
	}
	counts.byKind[kind]++
}

// bearer is the token an Authorization header carries under the two schemes GitHub reads an
// installation token from, "Bearer" and "token".
func bearer(authorization string) (string, bool) {
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || token == "" || (!strings.EqualFold(scheme, "bearer") && !strings.EqualFold(scheme, "token")) {
		return "", false
	}
	return token, true
}

// epoch is an X-RateLimit-Reset value, an epoch second, as a time.
func epoch(value string) (time.Time, bool) {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

// Own is installation's own counts on each counter by caller kind, since the counter's last reset
// as GitHub's last counted answer named it: empty, never nil, for a counter with no call counted,
// and for one whose reset has passed with no call counted since.
func (c *Counter) Own(installation string) (core, graphql map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.own(installation, Core), c.own(installation, GraphQL)
}

func (c *Counter) own(installation, counter string) map[string]int {
	counts := c.counts[window{installation: installation, counter: counter}]
	if counts == nil || (!counts.reset.IsZero() && !c.now().Before(counts.reset)) {
		return map[string]int{}
	}
	return maps.Clone(counts.byKind)
}

// Bucket is one counter of one installation as a reading reads it: GitHub's used count, limit and
// reset, and the program's own calls on it by caller kind (Counter.Own).
type Bucket struct {
	Used, Limit int
	Reset       time.Time
	Own         map[string]int
}

// CoreBucket is the REST core counter as the headers of a REST answer name it, the answer a
// reading takes it from: GET /repos/OWNER/REPO/actions/workflows?per_page=1. An answer whose
// X-RateLimit-Resource is not core, or that lacks the used count, the limit or the reset, is
// refused, so no reading carries a counter GitHub did not answer. A refusal names each header as
// shownResource and shownHeader do: the headers are the answering party's words, which whatever
// answers in GitHub's place can make an echo of the call's token, and the refusal reaches the
// reading's line (Reading.Error).
func CoreBucket(headers http.Header) (Bucket, error) {
	if resource := headers.Get("X-RateLimit-Resource"); resource != Core {
		return Bucket{}, fmt.Errorf("GitHub's answer names the %s counter, not core", shownResource(resource))
	}
	used, err := strconv.Atoi(headers.Get("X-RateLimit-Used"))
	if err != nil || used < 0 {
		return Bucket{}, fmt.Errorf("GitHub's answer has no non-negative X-RateLimit-Used count: %s", shownHeader(headers.Get("X-RateLimit-Used")))
	}
	limit, err := strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	if err != nil || limit <= 0 {
		return Bucket{}, fmt.Errorf("GitHub's answer has no X-RateLimit-Limit: %s", shownHeader(headers.Get("X-RateLimit-Limit")))
	}
	reset, ok := epoch(headers.Get("X-RateLimit-Reset"))
	if !ok {
		return Bucket{}, fmt.Errorf("GitHub's answer has no X-RateLimit-Reset epoch second: %s", shownHeader(headers.Get("X-RateLimit-Reset")))
	}
	return Bucket{Used: used, Limit: limit, Reset: reset}, nil
}

// notANumber is what a refusal names in place of a rate-limit header that is no number, and
// notACounter in place of a counter this package does not read.
const (
	notANumber  = "(a value that is not a number)"
	notACounter = "(a counter this reading does not read)"
)

// shownResource is resource, an X-RateLimit-Resource header, as a refusal names it: quoted where
// it is empty or one of the two counters a reading reads (Core, GraphQL), and notACounter
// otherwise.
func shownResource(resource string) string {
	if resource == "" || resource == Core || resource == GraphQL {
		return strconv.Quote(resource)
	}
	return notACounter
}

// shownHeader is value, a rate-limit header holding a number, as a refusal names it: quoted where
// it is empty, the number it parses as, written back from that number and quoted, where it parses
// as one, and notANumber otherwise.
func shownHeader(value string) string {
	if value == "" {
		return `""`
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return strconv.Quote(strconv.FormatInt(n, 10))
	}
	return notANumber
}

// RateLimitAnswer is GET /rate_limit's answer, as much of it as the GraphQL counter is read from
// (GraphQLBucketOf). A caller holding the answer's bytes reads the counter through GraphQLBucket;
// one that must decode an answer through a rule of its own, as Dispatch's reading decodes every
// answer through redact.Answer.Decode, decodes into this and reads the counter from it.
type RateLimitAnswer struct {
	Resources struct {
		GraphQL *struct {
			Limit int   `json:"limit"`
			Used  *int  `json:"used"`
			Reset int64 `json:"reset"`
		} `json:"graphql"`
	} `json:"resources"`
}

// GraphQLBucketOf is the GraphQL counter answer names as resources.graphql. An answer that names
// none, or names one with no limit, no used count or no reset, is no reading.
func GraphQLBucketOf(answer RateLimitAnswer) (Bucket, error) {
	counter := answer.Resources.GraphQL
	if counter == nil || counter.Limit <= 0 || counter.Used == nil || *counter.Used < 0 || counter.Reset <= 0 {
		return Bucket{}, fmt.Errorf("GitHub's rate_limit answer holds no resources.graphql counter")
	}
	return Bucket{Used: *counter.Used, Limit: counter.Limit, Reset: time.Unix(counter.Reset, 0).UTC()}, nil
}

// GraphQLBucket is the GraphQL counter GET /rate_limit's answer, body, names as resources.graphql.
func GraphQLBucket(body []byte) (Bucket, error) {
	var answer RateLimitAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return Bucket{}, fmt.Errorf("GitHub's rate_limit answer is not JSON: %w", err)
	}
	return GraphQLBucketOf(answer)
}

// Reading is one installation's reading, the line Write logs.
type Reading struct {
	Program, Process, Project string // "daemon" or "dispatch"; os.Hostname(); the daemon's project key, "" for Dispatch
	AppID, AppSlug            string
	Owner, InstallationID     string
	Installations             int               // how many installations GET /app/installations listed for the App
	Permissions               map[string]string // the installation's permissions as GET /app/installations answered them to the App's JWT
	BeforeReset               bool              // the reading in the last minute before a reset
	Core, GraphQL             Bucket
	Error                     string // set on a failed reading, which then carries no Bucket
}

// Write logs r as one record at info, Msg its message: the program, the process, the project, the
// App's id and slug, the installation's owner and id, how many installations the App's list held,
// the installation's permissions as the list named them, whether it is the reading before a reset,
// and each counter's used count, limit, reset (RFC 3339) and own counts by caller kind. A failed
// reading carries its error instead of the counters, held to ErrorLimit bytes (errorText), so a
// pass still writes one line for each installation listed; it names the installation's
// permissions all the same, since the list named them whatever the reading did after it. A line
// that names no installation, a failed or empty list's, names no permissions; one that does names
// them as an object, {} where the list named none.
//
// The slug, the owner and the permissions are GitHub's words as GET /app/installations answered
// them to the App's JWT, or those of whatever answers in GitHub's place, which can echo the JWT
// there. The line names each only in the shape GitHub gives it: the slug and the owner as logins
// (shownLogin), and the permissions from two closed sets (shownPermissions), each permission
// GitHub does not document left out of the object and counted in permissions_undocumented, which
// a line names only where it counted one. A caller decides on the words as they came.
func Write(log *slog.Logger, r Reading) {
	attrs := []slog.Attr{
		slog.String("program", r.Program),
		slog.String("process", r.Process),
		slog.String("project", r.Project),
		slog.String("app_id", r.AppID),
		slog.String("app_slug", shownLogin(r.AppSlug)),
		slog.String("owner", shownLogin(r.Owner)),
		slog.String("installation_id", r.InstallationID),
		slog.Int("installations", r.Installations),
	}
	if r.InstallationID != "" {
		permissions, undocumented := shownPermissions(r.Permissions)
		attrs = append(attrs, slog.Any("permissions", permissions))
		if undocumented > 0 {
			attrs = append(attrs, slog.Int("permissions_undocumented", undocumented))
		}
	}
	attrs = append(attrs, slog.Bool("before_reset", r.BeforeReset))
	if r.Error != "" {
		attrs = append(attrs, slog.String("error", errorText(r.Error)))
	} else {
		attrs = append(attrs, bucketAttrs(Core, r.Core)...)
		attrs = append(attrs, bucketAttrs(GraphQL, r.GraphQL)...)
	}
	log.LogAttrs(context.Background(), slog.LevelInfo, Msg, attrs...)
}

// notALogin is what a line names in place of a slug or an owner that is no login GitHub allows,
// in the daemon's words for the same (githubrest.NotALogin).
const notALogin = "(a login GitHub does not allow)"

// notDocumented is what a line names in place of an access GitHub does not document, in the
// daemon's words for the same (githubrest.NotDocumented).
const notDocumented = "(a word GitHub does not document there)"

// loginShape is a login as GitHub serves them, the daemon's one grammar of it (ghrepo.IsLogin): a
// letter or digit, then up to 38 letters, digits, hyphens and underscores.
var loginShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)

// tokenForm is the GitHub token forms, as the daemon matches them (githubrest.HoldsToken):
// installation, OAuth, user-to-server, refresh and personal (gh[opsur]_), and fine-grained
// personal (github_pat_). A token GitHub mints is 40 characters or more and fails loginShape's 39,
// but a shorter text spelled as one passes the underscore the shape admits.
var tokenForm = regexp.MustCompile(`(gh[opsur]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,})`)

// shownLogin is login, a slug or an account GitHub's list names, as a line names it, as the
// daemon's githubrest.ShownLogin names one: login where it is "" or has a login's shape
// (loginShape), an App's bot account's "[bot]" after it allowed, and holds no GitHub token form
// (tokenForm); notALogin otherwise.
func shownLogin(login string) string {
	if login == "" || loginShape.MatchString(strings.TrimSuffix(login, "[bot]")) && !tokenForm.MatchString(login) {
		return login
	}
	return notALogin
}

// documentedAccesses are the accesses GitHub gives a permission, as documentedPermissions'
// source lists them.
var documentedAccesses = []string{"read", "write", "admin"}

// documentedPermissions are the permissions an App can be granted: the properties of
// components.schemas.app-permissions in GitHub's REST description (github/rest-api-description,
// read on 2026-11-27). A permission GitHub adds later is counted, not named, until it is listed
// here.
var documentedPermissions = []string{
	"actions", "administration", "artifact_metadata", "attestations", "checks", "code_quality",
	"codespaces", "contents", "custom_properties_for_organizations", "dependabot_secrets",
	"deployments", "discussions", "email_addresses",
	"enterprise_custom_properties_for_organizations", "environments", "followers", "git_ssh_keys",
	"gpg_keys", "interaction_limits", "issues", "members", "merge_queues", "metadata",
	"organization_administration", "organization_announcement_banners",
	"organization_copilot_agent_settings", "organization_copilot_requests",
	"organization_copilot_seat_management", "organization_custom_org_roles",
	"organization_custom_properties", "organization_custom_roles", "organization_events",
	"organization_external_properties_for_repos", "organization_hooks", "organization_packages",
	"organization_personal_access_token_requests", "organization_personal_access_tokens",
	"organization_plan", "organization_projects", "organization_secrets",
	"organization_self_hosted_runners", "organization_user_blocking", "packages", "pages",
	"profile", "pull_requests", "repository_custom_properties", "repository_hooks",
	"repository_projects", "secret_scanning_alerts", "secrets", "security_events", "single_file",
	"starring", "statuses", "vulnerability_alerts", "workflows",
}

// shownPermissions is permissions as a line names them, never nil: each permission GitHub
// documents (documentedPermissions) with its access where GitHub documents that access
// (documentedAccesses) and notDocumented in its place otherwise; and how many permissions it left
// out, those GitHub does not document, whose names no line holds.
func shownPermissions(permissions map[string]string) (map[string]string, int) {
	shown := make(map[string]string, len(permissions))
	undocumented := 0
	for permission, access := range permissions {
		if !slices.Contains(documentedPermissions, permission) {
			undocumented++
			continue
		}
		if !slices.Contains(documentedAccesses, access) {
			access = notDocumented
		}
		shown[permission] = access
	}
	return shown, undocumented
}

// ErrorLimit is the most bytes a line's error holds, as a recorded GitHub read's cause does
// (Dispatch's github_read_attempts). A refusal's error carries GitHub's whole body, up to a
// megabyte, and a log store cuts a line that long short or drops it (Cloud Logging at 256 KiB).
const ErrorLimit = 1024

// errorText is value as valid UTF-8, each invalid byte U+FFFD, and where that is longer than
// ErrorLimit bytes, its head cut at a whole character and ended with " … (cut from <n> bytes)", n
// being the valid text's length, the whole inside the limit, so a reader sees that it was cut and
// by how much.
func errorText(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= ErrorLimit {
		return value
	}
	marker := " … (cut from " + strconv.Itoa(len(value)) + " bytes)"
	cut := ErrorLimit - len(marker)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + marker
}

func bucketAttrs(counter string, b Bucket) []slog.Attr {
	own := b.Own
	if own == nil {
		own = map[string]int{}
	}
	return []slog.Attr{
		slog.Int(counter+"_used", b.Used),
		slog.Int(counter+"_limit", b.Limit),
		slog.String(counter+"_reset", b.Reset.UTC().Format(time.RFC3339)),
		slog.Any(counter+"_own", own),
	}
}

// Next is when the next reading is due after one at now. resets are the resets its caller has read
// and not yet read in their last minute (InLastMinute at the read that named each); the caller
// passes no other. A reset in the future and within LastMinute of now is due at once: at is now and
// beforeReset true, so a pass that ended inside a counter's last minute it began outside of still
// reads that counter there. Otherwise the earliest reset less BeforeReset, when that comes before
// now+Interval, is due with beforeReset true, and otherwise the next reading is now+Interval. A
// caller that passed a reset it had already read in its last minute would be told to read it again
// at once, so it passes only those it has not; that also keeps a reset whose header drifts a second
// later from asking for a second reading. A zero reset, a failed reading's, is none, and so is one
// that is not in the future.
func Next(now time.Time, resets []time.Time) (at time.Time, beforeReset bool) {
	at = now.Add(Interval)
	for _, reset := range resets {
		until := reset.Sub(now)
		if reset.IsZero() || until <= 0 {
			continue
		}
		if until <= LastMinute {
			return now, true
		}
		if last := reset.Add(-BeforeReset); !last.After(at) {
			at, beforeReset = last, true
		}
	}
	return at, beforeReset
}

// InLastMinute says whether a reading at at is in the last minute before one of its counters
// resets. It uses that reading's own resets, since another installation in the same pass can
// have its reset much later, and a regular pass can already land in a counter's last minute.
func InLastMinute(at time.Time, resets ...time.Time) bool {
	for _, reset := range resets {
		if until := reset.Sub(at); until > 0 && until <= LastMinute {
			return true
		}
	}
	return false
}
