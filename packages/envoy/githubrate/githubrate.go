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
// this module's internal packages. It holds standard library code only.
package githubrate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
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
// name it. It keeps the SHA-256 of each token it is given, never the token.
type Counter struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens map[[sha256.Size]byte]registration
	counts map[window]*counted
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
	return &Counter{now: time.Now, tokens: map[[sha256.Size]byte]registration{}, counts: map[window]*counted{}}
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
// Modified, which GitHub charges an authenticated request nothing for.
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
	if err == nil && response.StatusCode != http.StatusNotModified && !rateLimitPaths[request.URL.Path] {
		t.counter.count(request, response.Header)
	}
	return response, err
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
// refused, so no reading carries a counter GitHub did not answer.
func CoreBucket(headers http.Header) (Bucket, error) {
	if resource := headers.Get("X-RateLimit-Resource"); resource != Core {
		return Bucket{}, fmt.Errorf("GitHub's answer names the %q counter, not core", resource)
	}
	used, err := strconv.Atoi(headers.Get("X-RateLimit-Used"))
	if err != nil || used < 0 {
		return Bucket{}, fmt.Errorf("GitHub's answer has no non-negative X-RateLimit-Used count: %q", headers.Get("X-RateLimit-Used"))
	}
	limit, err := strconv.Atoi(headers.Get("X-RateLimit-Limit"))
	if err != nil || limit <= 0 {
		return Bucket{}, fmt.Errorf("GitHub's answer has no X-RateLimit-Limit: %q", headers.Get("X-RateLimit-Limit"))
	}
	reset, ok := epoch(headers.Get("X-RateLimit-Reset"))
	if !ok {
		return Bucket{}, fmt.Errorf("GitHub's answer has no X-RateLimit-Reset epoch second: %q", headers.Get("X-RateLimit-Reset"))
	}
	return Bucket{Used: used, Limit: limit, Reset: reset}, nil
}

// GraphQLBucket is the GraphQL counter GET /rate_limit's answer, body, names as resources.graphql.
func GraphQLBucket(body []byte) (Bucket, error) {
	var answer struct {
		Resources struct {
			GraphQL *struct {
				Limit int   `json:"limit"`
				Used  *int  `json:"used"`
				Reset int64 `json:"reset"`
			} `json:"graphql"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Bucket{}, fmt.Errorf("GitHub's rate_limit answer is not JSON: %w", err)
	}
	counter := answer.Resources.GraphQL
	if counter == nil || counter.Limit <= 0 || counter.Used == nil || *counter.Used < 0 || counter.Reset <= 0 {
		return Bucket{}, fmt.Errorf("GitHub's rate_limit answer holds no resources.graphql counter")
	}
	return Bucket{Used: *counter.Used, Limit: counter.Limit, Reset: time.Unix(counter.Reset, 0).UTC()}, nil
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
// reading carries its error instead of the counters, so a pass still writes one line for each
// installation listed; it names the installation's permissions all the same, since the list named
// them whatever the reading did after it. A line that names no installation, a failed or empty
// list's, names no permissions; one that does names them as an object, {} where the list named
// none.
func Write(log *slog.Logger, r Reading) {
	attrs := []slog.Attr{
		slog.String("program", r.Program),
		slog.String("process", r.Process),
		slog.String("project", r.Project),
		slog.String("app_id", r.AppID),
		slog.String("app_slug", r.AppSlug),
		slog.String("owner", r.Owner),
		slog.String("installation_id", r.InstallationID),
		slog.Int("installations", r.Installations),
	}
	if r.InstallationID != "" {
		permissions := r.Permissions
		if permissions == nil {
			permissions = map[string]string{}
		}
		attrs = append(attrs, slog.Any("permissions", permissions))
	}
	attrs = append(attrs, slog.Bool("before_reset", r.BeforeReset))
	if r.Error != "" {
		attrs = append(attrs, slog.String("error", r.Error))
	} else {
		attrs = append(attrs, bucketAttrs(Core, r.Core)...)
		attrs = append(attrs, bucketAttrs(GraphQL, r.GraphQL)...)
	}
	log.LogAttrs(context.Background(), slog.LevelInfo, Msg, attrs...)
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
