package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// rateInstallation is one installation the rate reading's stand-in GitHub lists, and how GitHub
// answers the calls made with its token.
type rateInstallation struct {
	// id, owner and repository are the installation's id, its account and the first repository
	// it lists.
	id, owner, repository string
	// refused answers the installation's token request 403.
	refused bool
	// coreAhead is how long after the stand-in's start the installation's core counter resets,
	// zero for five minutes: more than the last minute, so a pass taken at the start schedules the
	// next one BeforeReset before the reset.
	coreAhead time.Duration
	// listing and workflows are the status GitHub answers GET /installation/repositories and the
	// workflows read with, zero for 200. A refusal names the core counter spent, used at its limit.
	listing, workflows int
	// listingBody, when set, is the body GitHub answers the repository listing with, 200 and the
	// installation's core counter in its headers: a listing that names no repository to read.
	listingBody string
	// bareWorkflows answers the workflows read with no rate-limit header at all.
	bareWorkflows bool
	// rateLimit is the status GET /rate_limit answers with, zero for 200; noGraphQL answers it
	// without resources.graphql.
	rateLimit int
	noGraphQL bool
	// permissions is what the list names the installation granted (listedPermissions), and
	// suspended lists it as suspended.
	permissions map[string]string
	suspended   bool
}

// listedPermissions is what the stand-in lists installation as granted: its own permissions, or,
// where it names none, contents and metadata at read.
func listedPermissions(installation rateInstallation) map[string]string {
	if installation.permissions != nil {
		return installation.permissions
	}
	return map[string]string{"contents": "read", "metadata": "read"}
}

// readingLimit is the limit of both counters the stand-in answers. The used count of an
// installation's core counter is ten times its id, and of its GraphQL counter the id itself
// (coreUsed, graphQLUsed), so the numbers each line carries can only be its own installation's.
const readingLimit = 5000

func coreUsed(id string) int {
	n, _ := strconv.Atoi(id)
	return 10 * n
}

func graphQLUsed(id string) int {
	n, _ := strconv.Atoi(id)
	return n
}

// rateSlugs is each stand-in App's slug by its id: the implement App's, 1, and the review App's, 2,
// two Apps as a daemon's boot requires.
var rateSlugs = map[string]string{"1": "example-implement", "2": "example-review"}

// rateGitHub is a stand-in GitHub for the rate reading. Each App lists its own installations under
// its JWT, each with the App's slug, each granted every permission a role's set names, and answers
// the installation that covers a repository of one of their owners; a token request mints a token
// of its own unless the installation refuses it. Each answer to an installation token carries that
// installation's core counter in its headers, as GitHub's REST answers do, except three: /graphql's,
// which carries the GraphQL counter, GET /rate_limit's, which names both counters in its body, and
// a bare workflows read's. Each installation's core counter resets coreAhead after start, and every
// GraphQL counter half an hour after it.
type rateGitHub struct {
	url, key            string
	start, graphQLReset time.Time

	mu sync.Mutex
	// listed is each App's installations by App id, and listStatus, when set, the status every
	// list is refused with.
	listed     map[string][]rateInstallation
	listStatus int
	// byID is every installation ever listed, as it was last listed, and tokens the installation
	// each token was minted on.
	byID   map[string]rateInstallation
	tokens map[string]string
	// calls counts the calls made with each installation's tokens, by "<installation> <path>".
	calls map[string]int
	// onRateLimit, when set, is called as GET /rate_limit is answered.
	onRateLimit func()
}

// rateStandIn starts the stand-in with the implement App alone, listing installations in their
// order.
func rateStandIn(t *testing.T, installations ...rateInstallation) *rateGitHub {
	t.Helper()
	return rateStandInFor(t, map[string][]rateInstallation{"1": installations})
}

// rateStandInFor starts the stand-in with each App of listed, by id, listing its installations in
// their order. Installation ids are GitHub's, one per installation of every App.
func rateStandInFor(t *testing.T, listed map[string][]rateInstallation) *rateGitHub {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	github := &rateGitHub{
		key:          string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		start:        now,
		graphQLReset: now.Add(30 * time.Minute),
		listed:       map[string][]rateInstallation{},
		byID:         map[string]rateInstallation{},
		tokens:       map[string]string{},
		calls:        map[string]int{},
	}
	for app, installations := range listed {
		github.list(app, installations...)
	}
	github.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app":
			slug, known := rateSlugs[jwtIssuer(t, r)]
			if !known {
				githubtest.Unexpected(t, w, r)
				return
			}
			fmt.Fprintf(w, `{"slug":%q}`, slug)
		case r.URL.Path == "/app/installations":
			app := jwtIssuer(t, r)
			github.mu.Lock()
			installations, known := github.listed[app]
			status := github.listStatus
			github.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"Server Error"}`)
				return
			}
			if !known {
				githubtest.Unexpected(t, w, r)
				return
			}
			entries := make([]string, 0, len(installations))
			for _, installation := range installations {
				// A map of strings always marshals, and json.Marshal sorts its keys.
				permissions, _ := json.Marshal(listedPermissions(installation))
				since := "null"
				if installation.suspended {
					since = `"2026-11-14T08:00:00Z"`
				}
				entries = append(entries, fmt.Sprintf(`{"id":%s,"app_slug":%q,"account":{"login":%q},"permissions":%s,"suspended_at":%s}`,
					installation.id, rateSlugs[app], installation.owner, permissions, since))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(entries, ","))
		case strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/installation"):
			// The installation of the App the JWT names that covers the repository: its owner's.
			owner, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
			github.mu.Lock()
			installations := github.listed[jwtIssuer(t, r)]
			github.mu.Unlock()
			for _, installation := range installations {
				if installation.owner == owner {
					fmt.Fprintf(w, `{"id":%s}`, installation.id)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			github.mu.Lock()
			installation, known := github.byID[id]
			github.mu.Unlock()
			if !known {
				githubtest.Unexpected(t, w, r)
				return
			}
			if installation.refused {
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
				return
			}
			github.mu.Lock()
			token := fmt.Sprintf("rate-reading-%s-%d", id, len(github.tokens)+1)
			github.tokens[token] = id
			github.mu.Unlock()
			mintedToken(w, token, exchangeBody(t, r))
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			id := strings.TrimPrefix(r.URL.Path, "/app/installations/")
			fmt.Fprintf(w, `{"id":%s,"permissions":{"actions":"write","attestations":"read","checks":"write","contents":"write","issues":"write","metadata":"read","pull_requests":"write","security_events":"write","statuses":"write","workflows":"write"},"suspended_at":null}`, id)
		default:
			installation, ok := github.installationOf(r)
			if !ok {
				githubtest.Unexpected(t, w, r)
				return
			}
			github.answer(t, w, r, installation)
		}
	}})
	return github
}

// list makes installations app's whole list, from its next read on.
func (g *rateGitHub) list(app string, installations ...rateInstallation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listed[app] = installations
	for _, installation := range installations {
		g.byID[installation.id] = installation
	}
}

// refuseLists answers every list from its next read on with status.
func (g *rateGitHub) refuseLists(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listStatus = status
}

// whenRateLimitIsRead calls f each time GET /rate_limit is answered, from the next one on.
func (g *rateGitHub) whenRateLimitIsRead(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onRateLimit = f
}

// callsTo is how many calls to path were made with installation's tokens.
func (g *rateGitHub) callsTo(installation, path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[installation+" "+path]
}

// coreReset is when installation's core counter resets.
func (g *rateGitHub) coreReset(installation rateInstallation) time.Time {
	if installation.coreAhead == 0 {
		return g.start.Add(5 * time.Minute)
	}
	return g.start.Add(installation.coreAhead)
}

// installationOf is the installation whose token r carries, under either scheme GitHub reads one
// from, as it is listed now.
func (g *rateGitHub) installationOf(r *http.Request) (rateInstallation, bool) {
	_, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	g.mu.Lock()
	defer g.mu.Unlock()
	id, ok := g.tokens[token]
	return g.byID[id], ok
}

// answer answers a request made with installation's token.
func (g *rateGitHub) answer(t *testing.T, w http.ResponseWriter, r *http.Request, installation rateInstallation) {
	t.Helper()
	g.mu.Lock()
	g.calls[installation.id+" "+r.URL.Path]++
	onRateLimit := g.onRateLimit
	g.mu.Unlock()
	reset := g.coreReset(installation)
	switch r.URL.Path {
	case "/rate_limit":
		if onRateLimit != nil {
			onRateLimit()
		}
		if installation.rateLimit != 0 {
			w.WriteHeader(installation.rateLimit)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		graphQL := fmt.Sprintf(`,"graphql":{"limit":%d,"used":%d,"remaining":%d,"reset":%d}`,
			readingLimit, graphQLUsed(installation.id), readingLimit-graphQLUsed(installation.id), g.graphQLReset.Unix())
		if installation.noGraphQL {
			graphQL = ""
		}
		fmt.Fprintf(w, `{"resources":{"core":{"limit":%d,"used":0,"remaining":%d,"reset":%d}%s}}`, readingLimit, readingLimit, reset.Unix(), graphQL)
		return
	case "/graphql":
		counterHeaders(w, githubrate.GraphQL, graphQLUsed(installation.id), g.graphQLReset)
		fmt.Fprint(w, `{"data":{"viewer":{"login":"example-implement[bot]"}}}`)
		return
	}
	repository := "/repos/" + installation.repository
	switch r.URL.Path {
	case "/installation/repositories":
		if installation.listing != 0 {
			spentRefusal(w, installation.listing, reset)
			return
		}
		counterHeaders(w, githubrate.Core, coreUsed(installation.id), reset)
		if installation.listingBody != "" {
			fmt.Fprint(w, installation.listingBody)
			return
		}
		fmt.Fprintf(w, `{"total_count":1,"repositories":[{"full_name":%q}]}`, installation.repository)
	case repository + "/actions/workflows":
		switch {
		case installation.bareWorkflows:
			fmt.Fprint(w, `{"total_count":0,"workflows":[]}`)
		case installation.workflows != 0:
			spentRefusal(w, installation.workflows, reset)
		default:
			counterHeaders(w, githubrate.Core, coreUsed(installation.id), reset)
			fmt.Fprint(w, `{"total_count":0,"workflows":[]}`)
		}
	case "/users/example-implement[bot]", "/users/example-review[bot]":
		counterHeaders(w, githubrate.Core, coreUsed(installation.id), reset)
		fmt.Fprint(w, `{"id":42}`)
	case repository:
		counterHeaders(w, githubrate.Core, coreUsed(installation.id), reset)
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, installation.repository)
	case repository + "/pulls/1":
		counterHeaders(w, githubrate.Core, coreUsed(installation.id), reset)
		fmt.Fprint(w, `{"number":1}`)
	default:
		githubtest.Unexpected(t, w, r)
	}
}

// counterHeaders sets the rate-limit headers GitHub sends with an answer that spent counter.
func counterHeaders(w http.ResponseWriter, counter string, used int, reset time.Time) {
	w.Header().Set("X-RateLimit-Resource", counter)
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(readingLimit))
	w.Header().Set("X-RateLimit-Used", strconv.Itoa(used))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(readingLimit-used))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
}

// spentRefusal is GitHub's refusal, status, of a call on a spent core counter: the counter in its
// headers, used at its limit.
func spentRefusal(w http.ResponseWriter, status int, reset time.Time) {
	counterHeaders(w, githubrate.Core, readingLimit, reset)
	w.WriteHeader(status)
	fmt.Fprint(w, `{"message":"API rate limit exceeded for installation."}`)
}

// rateTokens is the daemon's own counting token manager (countingTokens) for apps whose GitHub is
// the stand-in, and its counter: the wiring the daemon's boot makes, which puts every REST client
// call of the process on the counter's transport until the test ends.
func rateTokens(t *testing.T, github *rateGitHub, apps config.GitHubApps) (*appauth.Manager, *githubrate.Counter) {
	t.Helper()
	t.Cleanup(func() { githubrest.SetTransport(nil) })
	tokens, counter := countingTokens(apps, github.url, quietLogger())
	t.Cleanup(tokens.Close)
	return tokens, counter
}

// rateClock is the reader's clock in a test, which the test moves: each wait the reader asks for
// passes at once, and the stand-in may move it while an installation is read. Only the reader's
// cadence, its order and its window checks read it; GitHub's counters and the Counter read the
// wall clock, which stays minutes from every reset.
type rateClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *rateClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *rateClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// rateRig is a rate reader of apps on the stand-in, through the daemon's own counting token
// manager, logging to logs, on clock, which starts at the stand-in's start.
type rateRig struct {
	github  *rateGitHub
	tokens  *appauth.Manager
	counter *githubrate.Counter
	reader  *rateReader
	logs    *testlog.Buffer
	clock   *rateClock
}

func newRateRig(t *testing.T, github *rateGitHub, apps config.GitHubApps) *rateRig {
	t.Helper()
	tokens, counter := rateTokens(t, github, apps)
	logs := &testlog.Buffer{}
	clock := &rateClock{at: github.start}
	reader := newRateReader(tokens, counter, apps, "ACME", github.url, slog.New(slog.NewJSONHandler(logs, nil)))
	reader.now = clock.now
	return &rateRig{github: github, tokens: tokens, counter: counter, reader: reader, logs: logs, clock: clock}
}

// passes runs the reader for n passes and stops it: each wait it asks for passes at once, moving
// the clock by that wait, and the wait after the nth pass is the end. between, when set, is called
// with the number of the next pass before it starts. It returns when each pass after the last one
// run was due (n of them, the last being the pass that was never run), and each pass's reading
// lines, each line of the log parsed as the one JSON object it must be.
func (r *rateRig) passes(t *testing.T, n int, between func(next int)) ([]time.Time, [][]rateLine) {
	t.Helper()
	var dues []time.Time
	var ends []int
	done := make(chan struct{})
	// Only run's goroutine calls after, once at the end of each pass, and the test reads dues and
	// ends once the reader has stopped.
	r.reader.after = func(wait time.Duration) <-chan time.Time {
		ends = append(ends, r.logs.Len())
		dues = append(dues, r.clock.now().Add(wait))
		fired := make(chan time.Time, 1)
		if len(ends) == n {
			close(done)
			return fired
		}
		r.clock.set(dues[len(dues)-1])
		if between != nil {
			between(len(ends) + 1)
		}
		fired <- r.clock.now()
		return fired
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.reader.run(ctx)
	}()
	closed := func(c chan struct{}) func() bool {
		return func() bool {
			select {
			case <-c:
				return true
			default:
				return false
			}
		}
	}
	testwait.Eventually(t, "the rate reading's last pass", closed(done))
	cancel()
	testwait.Eventually(t, "the rate reading to stop", closed(stopped))
	logs := r.logs.String()
	passes := make([][]rateLine, 0, n)
	for start, end := 0, 0; len(passes) < n; start = end {
		end = ends[len(passes)]
		passes = append(passes, rateLines(t, logs[start:end]))
	}
	return dues, passes
}

// rateLine is one line of the rate reading as the daemon's JSON log holds it. The counter fields
// are pointers and maps, so a line that carries none of them reads as nil.
type rateLine struct {
	Msg            string            `json:"msg"`
	Program        string            `json:"program"`
	Process        string            `json:"process"`
	Project        string            `json:"project"`
	AppID          string            `json:"app_id"`
	AppSlug        string            `json:"app_slug"`
	Owner          string            `json:"owner"`
	InstallationID string            `json:"installation_id"`
	Installations  int               `json:"installations"`
	Permissions    map[string]string `json:"permissions"`
	BeforeReset    bool              `json:"before_reset"`
	Error          string            `json:"error"`
	CoreUsed       *int              `json:"core_used"`
	CoreLimit      *int              `json:"core_limit"`
	CoreReset      string            `json:"core_reset"`
	CoreOwn        map[string]int    `json:"core_own"`
	GraphQLUsed    *int              `json:"graphql_used"`
	GraphQLLimit   *int              `json:"graphql_limit"`
	GraphQLReset   string            `json:"graphql_reset"`
	GraphQLOwn     map[string]int    `json:"graphql_own"`
}

// rateLines is the reading lines of logs, a JSON log, in their order, each line of the log parsed
// as the one JSON object it must be.
func rateLines(t *testing.T, logs string) []rateLine {
	t.Helper()
	var readings []rateLine
	for _, text := range strings.Split(strings.TrimSpace(logs), "\n") {
		if text == "" {
			continue
		}
		var line rateLine
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("the log line %q is not one JSON object: %v", text, err)
		}
		if line.Msg == githubrate.Msg {
			readings = append(readings, line)
		}
	}
	return readings
}

// lineIDs is the installation each line names, in their order.
func lineIDs(lines []rateLine) []string {
	ids := make([]string, 0, len(lines))
	for _, line := range lines {
		ids = append(ids, line.InstallationID)
	}
	return ids
}

// checkDueAt holds that a pass was due at want.
func checkDueAt(t *testing.T, what string, due, want time.Time) {
	t.Helper()
	if !due.Equal(want) {
		t.Errorf("%s was due at %s, want %s", what, due.Format(time.RFC3339Nano), want.Format(time.RFC3339))
	}
}

// checkReadLine holds a line that read installation's two counters on the stand-in.
func checkReadLine(t *testing.T, github *rateGitHub, line rateLine, installation rateInstallation) {
	t.Helper()
	if line.Error != "" {
		t.Errorf("installation %s's line carries error %q, want its counters", installation.id, line.Error)
		return
	}
	for _, field := range []struct {
		name      string
		got       *int
		want      int
		gotReset  string
		wantReset time.Time
	}{
		{"core_used", line.CoreUsed, coreUsed(installation.id), line.CoreReset, github.coreReset(installation)},
		{"core_limit", line.CoreLimit, readingLimit, line.CoreReset, github.coreReset(installation)},
		{"graphql_used", line.GraphQLUsed, graphQLUsed(installation.id), line.GraphQLReset, github.graphQLReset},
		{"graphql_limit", line.GraphQLLimit, readingLimit, line.GraphQLReset, github.graphQLReset},
	} {
		if field.got == nil || *field.got != field.want {
			t.Errorf("installation %s's %s = %v, want %d", installation.id, field.name, field.got, field.want)
		}
		if want := field.wantReset.UTC().Format(time.RFC3339); field.gotReset != want {
			t.Errorf("installation %s's reset beside %s = %q, want %q", installation.id, field.name, field.gotReset, want)
		}
	}
}

// checkErrorLine holds a line that carries an error naming want, no counter, and before_reset
// false: a failure out of every last minute the reading knows of the installation.
func checkErrorLine(t *testing.T, line rateLine, want string) {
	t.Helper()
	checkErrorLineAt(t, line, want, false)
}

// checkErrorLineAt holds a line that carries an error naming want, no counter, and before_reset
// beforeReset.
func checkErrorLineAt(t *testing.T, line rateLine, want string, beforeReset bool) {
	t.Helper()
	if !strings.Contains(line.Error, want) || line.BeforeReset != beforeReset {
		t.Errorf("installation %s's line = %+v, want an error naming %q and before_reset %t", line.InstallationID, line, want, beforeReset)
	}
	if line.CoreUsed != nil || line.GraphQLUsed != nil || line.CoreOwn != nil || line.GraphQLOwn != nil {
		t.Errorf("installation %s's failed line = %+v, want no counter", line.InstallationID, line)
	}
}

// A pass of the rate reading writes one JSON line for each installation GitHub lists for an App the
// daemon holds a key for, whichever owners the project serves: here the implement App lists three,
// and globex is an owner no role has minted a token for, which the reading mints one for itself.
// Each line carries the App, the installation, the count the list held, both counters as GitHub
// answered them, and the daemon's own calls on each counter by caller kind: the outbox's two REST
// calls and one GraphQL call on acme's installation before the reading (the bot user its mint read,
// a pull request read), and the reading's own two REST calls on each installation (its first
// repository and the workflows read), the second pass reading only the workflows again. GET
// /rate_limit counts for nothing. The review App is on a token_command, holds no key, and is not
// read, which the reading says once. The pass after the first is due BeforeReset before the core
// counter's reset, and its lines, read in that counter's last minute, say so. Each line carries the
// permissions the list names its installation granted, an error line's too, where a refused or a
// suspended installation is read no further. And where the installations of one pass reset at
// different times, before_reset is each line's own: only the line of the installation read in the
// last minute before one of its own resets carries it.
func TestRateReadingIsOneJSONLine(t *testing.T) {
	installations := []rateInstallation{
		{id: "77", owner: "acme", repository: "acme/widgets", permissions: map[string]string{"contents": "write", "metadata": "read"}},
		{id: "78", owner: "initech", repository: "initech/gadgets", permissions: map[string]string{"metadata": "read"}},
		{id: "79", owner: "globex", repository: "globex/gizmos", permissions: map[string]string{"issues": "read", "metadata": "read"}},
	}
	github := rateStandIn(t, installations...)
	apps := config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: github.key},
		Review:    config.GitHubApp{AppID: "2", TokenCommand: "exit 1"},
	}
	rig := newRateRig(t, github, apps)

	outbox := githubrate.WithCaller(context.Background(), "outbox")
	lease, err := rig.tokens.Token(outbox, appauth.Implement, "acme")
	if err != nil {
		t.Fatalf("mint acme's write token: %v", err)
	}
	if lease.Installation != "77" {
		t.Errorf("acme's lease names installation %q, want 77", lease.Installation)
	}
	if err := (githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(github.url, ghrepo.MustParse("acme/widgets"))}).Get(outbox, "/pulls/1", nil); err != nil {
		t.Fatalf("read acme/widgets#1: %v", err)
	}
	query, err := http.NewRequestWithContext(outbox, http.MethodPost, github.url+"/graphql", strings.NewReader(`{"query":"{viewer{login}}"}`))
	if err != nil {
		t.Fatal(err)
	}
	query.Header.Set("Authorization", "Bearer "+lease.Token)
	answered, err := githubrest.HTTPClient().Do(query)
	if err != nil {
		t.Fatalf("query GitHub's GraphQL API: %v", err)
	}
	answered.Body.Close()

	dues, passes := rig.passes(t, 2, nil)

	checkDueAt(t, "the second pass", dues[0], github.coreReset(installations[0]).Add(-githubrate.BeforeReset))
	if keyless := strings.Count(rig.logs.String(), rateReadingKeyless); keyless != 1 {
		t.Errorf("the log says %d times that the review App holds no key, want once:\n%s", keyless, rig.logs.String())
	}
	if started := strings.Count(rig.logs.String(), rateReadingStarts); started != 1 {
		t.Errorf("the log says %d times that the reading starts, want once:\n%s", started, rig.logs.String())
	}
	// The interval is written as Dispatch's start line writes it, a duration's text, not the
	// nanoseconds a JSON handler gives a time.Duration.
	if !strings.Contains(rig.logs.String(), `"interval":"10m0s"`) {
		t.Errorf("the start line does not give the interval as \"10m0s\":\n%s", rig.logs.String())
	}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	for pass, lines := range passes {
		if len(lines) != len(installations) {
			t.Fatalf("pass %d wrote %d lines, want one for each of the %d installations listed", pass+1, len(lines), len(installations))
		}
		read := map[string]bool{}
		for _, line := range lines {
			var installation rateInstallation
			for _, listed := range installations {
				if listed.id == line.InstallationID {
					installation = listed
				}
			}
			if installation.id == "" || read[installation.id] {
				t.Errorf("pass %d wrote a line for installation %q, want one line for each of 77, 78 and 79", pass+1, line.InstallationID)
				continue
			}
			read[installation.id] = true
			if line.Program != "daemon" || line.Process != hostname || line.Project != "ACME" || line.AppID != "1" || line.AppSlug != "example-implement" ||
				line.Owner != installation.owner || line.Installations != len(installations) || line.BeforeReset != (pass == 1) {
				t.Errorf("pass %d's line for installation %s = %+v, want program daemon, process %s, project ACME, App 1 example-implement, owner %s, installations %d and before_reset %t",
					pass+1, installation.id, line, hostname, installation.owner, len(installations), pass == 1)
			}
			checkReadLine(t, github, line, installation)
			if !maps.Equal(line.Permissions, installation.permissions) {
				t.Errorf("pass %d's line for installation %s says permissions %v, want the list's %v", pass+1, installation.id, line.Permissions, installation.permissions)
			}
			wantCore, wantGraphQL := map[string]int{"rate_reading": 2 + pass}, map[string]int{}
			if installation.id == "77" {
				wantCore["outbox"], wantGraphQL["outbox"] = 2, 1
			}
			if !maps.Equal(line.CoreOwn, wantCore) || line.CoreOwn == nil {
				t.Errorf("pass %d's core_own of installation %s = %v, want %v", pass+1, installation.id, line.CoreOwn, wantCore)
			}
			if !maps.Equal(line.GraphQLOwn, wantGraphQL) || line.GraphQLOwn == nil {
				t.Errorf("pass %d's graphql_own of installation %s = %v, want %v", pass+1, installation.id, line.GraphQLOwn, wantGraphQL)
			}
		}
	}

	t.Run("one pass over two installations whose resets differ", func(t *testing.T) {
		acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 50 * time.Second}
		initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", coreAhead: time.Hour}
		github := rateStandIn(t, acme, initech)
		app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
		rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

		_, passes := rig.passes(t, 1, nil)

		lines := passes[0]
		if !slices.Equal(lineIDs(lines), []string{"77", "78"}) {
			t.Fatalf("the pass wrote lines for %v, want 77 and 78", lineIDs(lines))
		}
		checkReadLine(t, github, lines[0], acme)
		checkReadLine(t, github, lines[1], initech)
		if !lines[0].BeforeReset || lines[1].BeforeReset {
			t.Errorf("the pass's lines say before_reset %t for 77, 50 s before its core reset, and %t for 78, an hour before its own; want true and false",
				lines[0].BeforeReset, lines[1].BeforeReset)
		}
	})

	t.Run("a refused and a suspended installation's error lines", func(t *testing.T) {
		acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", permissions: map[string]string{"contents": "write", "metadata": "read"}}
		initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", refused: true, permissions: map[string]string{"metadata": "read"}}
		globex := rateInstallation{id: "79", owner: "globex", repository: "globex/gizmos", suspended: true, permissions: map[string]string{"issues": "write", "metadata": "read"}}
		github := rateStandIn(t, acme, initech, globex)
		app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
		rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

		_, passes := rig.passes(t, 1, nil)

		lines := passes[0]
		if !slices.Equal(lineIDs(lines), []string{"77", "78", "79"}) {
			t.Fatalf("the pass wrote lines for %v, want 77, 78 and 79", lineIDs(lines))
		}
		checkReadLine(t, github, lines[0], acme)
		checkErrorLine(t, lines[1], "403")
		checkErrorLine(t, lines[2], "suspended")
		for n, installation := range []rateInstallation{acme, initech, globex} {
			if !maps.Equal(lines[n].Permissions, installation.permissions) {
				t.Errorf("installation %s's line says permissions %v, want the list's %v", installation.id, lines[n].Permissions, installation.permissions)
			}
		}
	})
}

// An installation whose token request GitHub refuses still has its line in every pass, carrying the
// refusal and no counter, so a pass writes as many lines as the list held; an error line is never
// the reading before a reset. Both roles name the one App here, which is read once: each pass
// writes two lines, not four. acme's read token is the first the App mints, so its mint also reads
// the App's bot user under it, a third REST call of the reading's on acme in the first pass.
func TestRateReadingWritesALineForARefusedInstallation(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
	initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", refused: true}
	github := rateStandIn(t, acme, initech)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	dues, passes := rig.passes(t, 2, nil)

	checkDueAt(t, "the second pass", dues[0], github.coreReset(acme).Add(-githubrate.BeforeReset))
	for pass, lines := range passes {
		if !slices.Equal(lineIDs(lines), []string{"77", "78"}) {
			t.Fatalf("pass %d wrote lines for installations %v, want 77 and 78 in the order GitHub lists them", pass+1, lineIDs(lines))
		}
		for _, line := range lines {
			if line.AppID != "1" || line.AppSlug != "example-implement" || line.Installations != 2 {
				t.Errorf("pass %d's line for installation %s = %+v, want App 1 example-implement and installations 2", pass+1, line.InstallationID, line)
			}
		}
		checkReadLine(t, github, lines[0], acme)
		if lines[0].BeforeReset != (pass == 1) {
			t.Errorf("pass %d's line for acme says before_reset %t, want %t", pass+1, lines[0].BeforeReset, pass == 1)
		}
		if want := map[string]int{"rate_reading": 3 + pass}; !maps.Equal(lines[0].CoreOwn, want) {
			t.Errorf("pass %d's core_own of acme = %v, want %v", pass+1, lines[0].CoreOwn, want)
		}
		if lines[1].Owner != "initech" {
			t.Errorf("pass %d's second line names owner %q, want initech", pass+1, lines[1].Owner)
		}
		checkErrorLine(t, lines[1], "403")
	}
}

// An App whose list GitHub answers with no installation still has one line in every pass, naming
// no installation and carrying the empty list as its error, so no pass is silent for an App the
// daemon holds a key for. The list names no slug, so the App's own, from GET /app, names it.
func TestRateReadingWritesALineForAnAppThatListsNoInstallation(t *testing.T) {
	github := rateStandIn(t)
	rig := newRateRig(t, github, config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: github.key},
		Review:    config.GitHubApp{AppID: "2", TokenCommand: "exit 1"},
	})

	_, passes := rig.passes(t, 2, nil)

	for pass, lines := range passes {
		if len(lines) != 1 {
			t.Fatalf("pass %d wrote %d lines, want the one line of the App", pass+1, len(lines))
		}
		line := lines[0]
		if line.AppID != "1" || line.AppSlug != "example-implement" || line.Owner != "" || line.InstallationID != "" || line.Installations != 0 {
			t.Errorf("pass %d's line = %+v, want App 1 example-implement, no installation and installations 0", pass+1, line)
		}
		checkErrorLine(t, line, "GET /app/installations listed no installation of the App")
	}
}

// Every pass is due when githubrate.Next says, after the pass before it, from the resets the
// reading holds that no pass has read in their last minute: the second BeforeReset before the core
// counter's reset, and the third an Interval after the second. The second pass reads acme in that
// reset's last minute, which marks the reset read, so only the GraphQL reset, half an hour on, is
// left to schedule by, and the core reset gets no second reading at once.
func TestRateReadingSchedulesEachPassByNextOfTheResetsItHolds(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
	github := rateStandIn(t, acme)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	dues, passes := rig.passes(t, 2, nil)

	beforeReset := github.coreReset(acme).Add(-githubrate.BeforeReset)
	checkDueAt(t, "the second pass", dues[0], beforeReset)
	if len(passes[1]) != 1 || !passes[1][0].BeforeReset {
		t.Fatalf("the second pass wrote %+v, want acme's line read in its core counter's last minute", passes[1])
	}
	if held := rig.reader.heldResets(beforeReset); !slices.EqualFunc(held, []time.Time{github.graphQLReset}, time.Time.Equal) {
		t.Errorf("after the second pass the reading schedules by resets %v, want the GraphQL reset alone", held)
	}
	checkDueAt(t, "the third pass", dues[1], beforeReset.Add(githubrate.Interval))
}

// The resets an installation's last good reading read keep scheduling the pass before them while
// a later reading fails, whether the installation's own read or the App's list fails, and stop once
// the installation is no longer listed. acme's core counter resets fifteen minutes after the first
// pass, so the second pass is an Interval after it and fails, and the third is due BeforeReset
// before that reset only while the reading still holds it.
func TestRateReadingKeepsAnInstallationsKnownResetsWhileItsReadingFails(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 15 * time.Minute}
	for _, tc := range []struct {
		name   string
		second func(github *rateGitHub)
		lines  []string
		held   bool
	}{
		{"its own read fails", func(github *rateGitHub) {
			failing := acme
			failing.bareWorkflows = true
			github.list("1", failing)
		}, []string{"77"}, true},
		{"the App's list fails", func(github *rateGitHub) { github.refuseLists(http.StatusInternalServerError) }, []string{""}, true},
		{"it is no longer listed", func(github *rateGitHub) {
			github.list("1", rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", refused: true})
		}, []string{"78"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := rateStandIn(t, acme)
			app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
			rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

			dues, passes := rig.passes(t, 2, func(int) { tc.second(github) })

			checkDueAt(t, "the second pass", dues[0], github.start.Add(githubrate.Interval))
			if ids := lineIDs(passes[1]); !slices.Equal(ids, tc.lines) || passes[1][0].Error == "" {
				t.Fatalf("the second pass wrote %+v, want one failed line for %q", passes[1], tc.lines)
			}
			want := github.start.Add(2 * githubrate.Interval)
			if tc.held {
				want = github.coreReset(acme).Add(-githubrate.BeforeReset)
			}
			checkDueAt(t, "the third pass", dues[1], want)
			if _, kept := rig.reader.installations["1"]["77"]; kept != tc.held {
				t.Errorf("the reading holds acme's installation %t, want %t", kept, tc.held)
			}
		})
	}
}

// A pass reads the installations whose counters are in their last minute first, so the reading the
// pass was scheduled for is not held behind every other installation's, and still reads every
// installation listed. Each line says whether it was read in its own counter's last minute.
func TestRateReadingReadsTheInstallationsInTheirLastMinuteFirst(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 30 * time.Minute}
	initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets"}
	github := rateStandIn(t, acme, initech)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	dues, passes := rig.passes(t, 2, nil)

	checkDueAt(t, "the second pass", dues[0], github.coreReset(initech).Add(-githubrate.BeforeReset))
	if ids := lineIDs(passes[0]); !slices.Equal(ids, []string{"77", "78"}) {
		t.Errorf("the first pass read %v, want the list's order, 77 then 78", ids)
	}
	if ids := lineIDs(passes[1]); !slices.Equal(ids, []string{"78", "77"}) {
		t.Fatalf("the second pass read %v, want 78, in its last minute, before 77", ids)
	}
	checkReadLine(t, github, passes[1][0], initech)
	checkReadLine(t, github, passes[1][1], acme)
	if !passes[1][0].BeforeReset || passes[1][1].BeforeReset {
		t.Errorf("the second pass's lines say before_reset %t for 78 and %t for 77, want true and false", passes[1][0].BeforeReset, passes[1][1].BeforeReset)
	}
}

// A pass follows the list GitHub answers now: an installation the list no longer holds has no line
// and is forgotten with its repository, one newly listed is read, and one still listed keeps the
// repository it listed, so it is not listed again.
func TestRateReadingFollowsAListThatChangesBetweenPasses(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
	initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets"}
	globex := rateInstallation{id: "79", owner: "globex", repository: "globex/gizmos"}
	github := rateStandIn(t, acme, initech)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	_, passes := rig.passes(t, 2, func(int) { github.list("1", initech, globex) })

	if ids := lineIDs(passes[0]); !slices.Equal(ids, []string{"77", "78"}) {
		t.Errorf("the first pass read %v, want 77 and 78", ids)
	}
	if ids := lineIDs(passes[1]); !slices.Equal(ids, []string{"78", "79"}) {
		t.Fatalf("the second pass read %v, want 78 and 79, the list GitHub answers now", ids)
	}
	for _, line := range passes[1] {
		if line.Installations != 2 {
			t.Errorf("the second pass's line for %s says installations %d, want 2", line.InstallationID, line.Installations)
		}
	}
	checkReadLine(t, github, passes[1][0], initech)
	checkReadLine(t, github, passes[1][1], globex)
	if listings := github.callsTo("78", "/installation/repositories"); listings != 1 {
		t.Errorf("initech's repositories were listed %d times, want once: the reading keeps the repository it listed", listings)
	}
	if held := slices.Sorted(maps.Keys(rig.reader.installations["1"])); !slices.Equal(held, []string{"78", "79"}) {
		t.Errorf("the reading holds installations %v, want 78 and 79 alone", held)
	}
}

// An installation whose repository listing GitHub answers with no repository to read, though it
// answered it, has its core counter read from the listing's own answer, which is a core call's
// too: an empty list, a name that is no repository's, and a body that is no JSON. The line carries
// both counters and no error, and no workflows read is made.
func TestRateReadingReadsTheCoreCounterFromAListingThatNamesNoRepository(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"an empty list", `{"total_count":0,"repositories":[]}`},
		{"a name that is no repository's", `{"total_count":1,"repositories":[{"full_name":"no-slash"}]}`},
		{"a body that is no JSON", `<html>no JSON</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", listingBody: tc.body}
			github := rateStandIn(t, acme)
			app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
			rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

			_, passes := rig.passes(t, 1, nil)

			if len(passes[0]) != 1 {
				t.Fatalf("the pass wrote %d lines, want one", len(passes[0]))
			}
			checkReadLine(t, github, passes[0][0], acme)
			if workflows := github.callsTo(acme.id, "/repos/acme/widgets/actions/workflows"); workflows != 0 {
				t.Errorf("the workflows were read %d times, want none: the listing named no repository", workflows)
			}
		})
	}
}

// A first reading in a spent hour reads the core counter from GitHub's refusal, which names it as
// any answer does: where the listing of the installation's repositories is refused, from that
// refusal, with no workflows read, and where the workflows read is refused, 403 or 429, from that
// one. The line carries both counters and no error, and the refused calls are among the reading's
// own: the App's bot user, which acme's mint read, and each call GitHub answered.
func TestRateReadingReadsTheCoreCounterFromARefusalInASpentHour(t *testing.T) {
	for _, tc := range []struct {
		name      string
		spend     func(*rateInstallation)
		coreCalls int
		workflows int
	}{
		{"its repository listing refused 403", func(i *rateInstallation) { i.listing = http.StatusForbidden }, 2, 0},
		{"its workflows read refused 403", func(i *rateInstallation) { i.workflows = http.StatusForbidden }, 3, 1},
		{"its workflows read refused 429", func(i *rateInstallation) { i.workflows = http.StatusTooManyRequests }, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
			tc.spend(&acme)
			github := rateStandIn(t, acme)
			app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
			rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

			_, passes := rig.passes(t, 1, nil)

			if len(passes[0]) != 1 {
				t.Fatalf("the pass wrote %d lines, want one", len(passes[0]))
			}
			line := passes[0][0]
			if line.Error != "" || line.CoreUsed == nil || *line.CoreUsed != readingLimit || line.CoreLimit == nil || *line.CoreLimit != readingLimit ||
				line.CoreReset != github.coreReset(acme).UTC().Format(time.RFC3339) {
				t.Fatalf("the line = %+v, want no error and the core counter spent, %d of %d", line, readingLimit, readingLimit)
			}
			if line.GraphQLUsed == nil || *line.GraphQLUsed != graphQLUsed(acme.id) {
				t.Errorf("the line's graphql_used = %v, want %d", line.GraphQLUsed, graphQLUsed(acme.id))
			}
			if want := map[string]int{"rate_reading": tc.coreCalls}; !maps.Equal(line.CoreOwn, want) {
				t.Errorf("the line's core_own = %v, want %v", line.CoreOwn, want)
			}
			if workflows := github.callsTo(acme.id, "/repos/acme/widgets/actions/workflows"); workflows != tc.workflows {
				t.Errorf("the workflows were read %d times, want %d", workflows, tc.workflows)
			}
		})
	}
}

// A repository whose workflows read GitHub answers 404 is the installation's no longer: the line
// still carries the core counter that answer names, and the next pass lists the installation's
// repositories again rather than read the gone repository for ever.
func TestRateReadingListsTheRepositoriesAgainAfterTheRepositoryAnswers404(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", workflows: http.StatusNotFound}
	github := rateStandIn(t, acme)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	_, passes := rig.passes(t, 2, nil)

	for pass, lines := range passes {
		if len(lines) != 1 || lines[0].Error != "" || lines[0].CoreUsed == nil {
			t.Errorf("pass %d wrote %+v, want one line carrying the core counter the 404 names", pass+1, lines)
		}
	}
	if listings := github.callsTo(acme.id, "/installation/repositories"); listings != 2 {
		t.Errorf("acme's repositories were listed %d times over two passes, want twice: the 404 forgets the repository", listings)
	}
}

// A counter whose reset passes between the counter read and the snapshot of the daemon's own calls
// makes the line an error: the counter GitHub answered is the old hour's, and the own counts the new
// one's.
func TestRateReadingRefusesAReadingThatSpansTwoRateWindows(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
	github := rateStandIn(t, acme)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})
	github.whenRateLimitIsRead(func() { rig.clock.set(github.coreReset(acme).Add(time.Second)) })

	_, passes := rig.passes(t, 1, nil)

	if len(passes[0]) != 1 {
		t.Fatalf("the pass wrote %d lines, want one", len(passes[0]))
	}
	checkErrorLine(t, passes[0][0], "the reading spans two rate windows")
}

// A reading that spans two rate windows in the last minute before its other counter's reset is a
// before_reset error line, as Dispatch's is: that minute was tried and read nothing.
func TestRateReadingMarksASpanningReadingInALastMinuteAsBeforeReset(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 29*time.Minute + 50*time.Second}
	github := rateStandIn(t, acme)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})
	// The core counter resets while GET /rate_limit is answered, 9 s before the GraphQL reset.
	github.whenRateLimitIsRead(func() { rig.clock.set(github.coreReset(acme).Add(time.Second)) })

	_, passes := rig.passes(t, 1, nil)

	if len(passes[0]) != 1 {
		t.Fatalf("the pass wrote %d lines, want one", len(passes[0]))
	}
	checkErrorLineAt(t, passes[0][0], "the reading spans two rate windows", true)
}

// Each way an installation's reading fails gives that installation its error line, and the
// installations listed before and after it are read as ever.
func TestRateReadingWritesAnErrorLineForEachFailureBetweenTwoGoodReadings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  func(*rateInstallation)
		error string
	}{
		{"its token request refused", func(i *rateInstallation) { i.refused = true }, "mint"},
		{"its workflows read naming no core counter", func(i *rateInstallation) { i.bareWorkflows = true }, "core counter"},
		{"its GET /rate_limit refused", func(i *rateInstallation) { i.rateLimit = http.StatusForbidden }, "GET /rate_limit"},
		{"its GET /rate_limit naming no GraphQL counter", func(i *rateInstallation) { i.noGraphQL = true }, "resources.graphql"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets"}
			initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets"}
			globex := rateInstallation{id: "79", owner: "globex", repository: "globex/gizmos"}
			tc.fail(&initech)
			github := rateStandIn(t, acme, initech, globex)
			app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
			rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

			_, passes := rig.passes(t, 1, nil)

			lines := passes[0]
			if !slices.Equal(lineIDs(lines), []string{"77", "78", "79"}) {
				t.Fatalf("the pass wrote lines for %v, want 77, 78 and 79", lineIDs(lines))
			}
			checkReadLine(t, github, lines[0], acme)
			checkErrorLine(t, lines[1], tc.error)
			checkReadLine(t, github, lines[2], globex)
		})
	}
}

// A daemon holding no App's key reads nothing, so it logs no start line for the reading, only that
// each App is not read.
func TestRateReadingLogsNoStartLineWithoutAKey(t *testing.T) {
	apps := config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", TokenCommand: "exit 1"},
		Review:    config.GitHubApp{AppID: "2", TokenCommand: "exit 1"},
	}
	var logs testlog.Buffer
	reader := newRateReader(nil, githubrate.NewCounter(), apps, "ACME", "", slog.New(slog.NewJSONHandler(&logs, nil)))
	reader.run(context.Background())
	if started := strings.Count(logs.String(), rateReadingStarts); started != 0 {
		t.Errorf("a daemon holding no key says %d times that the reading starts, want never:\n%s", started, logs.String())
	}
	if keyless := strings.Count(logs.String(), rateReadingKeyless); keyless != 2 {
		t.Errorf("the log says %d times that an App holds no key, want once for each of the two:\n%s", keyless, logs.String())
	}
}

// The key held by either role is enough to read an App: a token-command role encountered first
// must not hide another role's key for that same App.
func TestRateReadingUsesAKeyHeldByEitherRoleOfASharedApp(t *testing.T) {
	apps := config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", TokenCommand: "exit 1"},
		Review:    config.GitHubApp{AppID: "1", PrivateKey: "a key held by the review role"},
	}
	reader := newRateReader(nil, githubrate.NewCounter(), apps, "ACME", "", quietLogger())
	if len(reader.keyed) != 1 || reader.keyed[0].role != appauth.Review || len(reader.keyless) != 0 {
		t.Fatalf("the App's reading roles = keyed %v, keyless %v, want the review role's key once", reader.keyed, reader.keyless)
	}
}

// rateReadCalls supplies the two token-source calls whose cancellation and deadlines the reader
// owns, without a second HTTP server standing between those contexts and the test.
type rateReadCalls struct {
	list func(context.Context, appauth.AppRole) (appauth.AppInstallations, error)
	mint func(context.Context, appauth.AppRole, string) (appauth.Lease, error)
}

func (a rateReadCalls) ListInstallations(ctx context.Context, role appauth.AppRole) (appauth.AppInstallations, error) {
	return a.list(ctx, role)
}

func (a rateReadCalls) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return a.mint(ctx, role, owner)
}

// A cancelled reader starts no list at all, so stopping a daemon makes no new GitHub work.
func TestRateReadingDoesNotStartAListAfterItStops(t *testing.T) {
	calls := 0
	source := rateReadCalls{list: func(context.Context, appauth.AppRole) (appauth.AppInstallations, error) {
		calls++
		return appauth.AppInstallations{}, nil
	}}
	app := config.GitHubApp{AppID: "1", PrivateKey: "a key"}
	reader := newRateReader(source, githubrate.NewCounter(), config.GitHubApps{Implement: app, Review: app}, "ACME", "", quietLogger())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader.run(ctx)
	if calls != 0 {
		t.Fatalf("the stopped reader listed %d Apps, want none", calls)
	}
}

// Each list, and each installation's mint and reads, has a bound of its own: here acme's mint
// waits until its bound has really ended, and globex's is still asked, under a bound that began
// after acme's ended, and gets its own line.
func TestRateReadingBoundsEachListAndEachInstallationOnItsOwn(t *testing.T) {
	var acmeEnded, globexDeadline time.Time
	source := rateReadCalls{
		list: func(ctx context.Context, _ appauth.AppRole) (appauth.AppInstallations, error) {
			if _, bounded := ctx.Deadline(); !bounded {
				t.Error("the installation list has no deadline")
			}
			return appauth.AppInstallations{Slug: "example-app", Installations: []appauth.ListedInstallation{{ID: "77", Owner: "acme"}, {ID: "79", Owner: "globex"}}}, nil
		},
		mint: func(ctx context.Context, _ appauth.AppRole, owner string) (appauth.Lease, error) {
			if owner == "acme" {
				select {
				case <-ctx.Done():
				case <-time.After(time.Minute):
					t.Error("acme's read had no bound that ended")
				}
				acmeEnded = time.Now()
				return appauth.Lease{}, ctx.Err()
			}
			globexDeadline, _ = ctx.Deadline()
			return appauth.Lease{}, errors.New("GitHub refused globex's token")
		},
	}
	app := config.GitHubApp{AppID: "1", PrivateKey: "a key"}
	var logs testlog.Buffer
	reader := newRateReader(source, githubrate.NewCounter(), config.GitHubApps{Implement: app, Review: app}, "ACME", "", slog.New(slog.NewJSONHandler(&logs, nil)))
	reader.readBound = 100 * time.Millisecond

	reader.pass(context.Background())

	lines := rateLines(t, logs.String())
	if !slices.Equal(lineIDs(lines), []string{"77", "79"}) {
		t.Fatalf("the pass wrote lines for %v, want acme's and globex's", lineIDs(lines))
	}
	checkErrorLine(t, lines[0], context.DeadlineExceeded.Error())
	checkErrorLine(t, lines[1], "GitHub refused globex's token")
	if !globexDeadline.After(acmeEnded) {
		t.Errorf("globex's read was bounded until %s, want a bound of its own, ending after acme's ended at %s", globexDeadline, acmeEnded)
	}
}

// An installation read with more than a minute to its reset, whose last minute begins while the
// rest of the pass is read, is read again at once: its reset is one no line has read in its last
// minute, and githubrate.Next makes such a reset due now. Here acme is read 70 s before its core
// reset, initech's read takes the clock 30 s on, and the pass after is due as that one ends; acme
// is read first in it and its line says before_reset. Without the reading at once, the pass would
// be due after acme's reset and acme's hour would have no line from its last minute.
func TestRateReadingReadsAtOnceAnInstallationWhoseLastMinuteBeganDuringThePass(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 70 * time.Second}
	initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", coreAhead: time.Hour}
	github := rateStandIn(t, acme, initech)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})
	reads := 0
	github.whenRateLimitIsRead(func() {
		if reads++; reads == 2 {
			rig.clock.set(github.start.Add(30 * time.Second))
		}
	})

	dues, passes := rig.passes(t, 2, nil)

	if ids := lineIDs(passes[0]); !slices.Equal(ids, []string{"77", "78"}) || passes[0][0].BeforeReset {
		t.Fatalf("the first pass wrote %+v, want 77 read 70 s before its reset, not in its last minute, then 78", passes[0])
	}
	checkDueAt(t, "the second pass", dues[0], github.start.Add(30*time.Second))
	if len(passes[1]) == 0 || passes[1][0].InstallationID != "77" || !passes[1][0].BeforeReset {
		t.Fatalf("the second pass wrote %+v, want 77 read first, in its core counter's last minute", passes[1])
	}
	checkReadLine(t, github, passes[1][0], acme)
}

// A reset whose last minute a line has read schedules no second early pass, whatever GitHub's
// header says of it at the next read: GitHub names the same reset a second apart within one hour.
// acme's core counter resets 50 s after the first pass, which reads it in that last minute;
// initech's, 80 s after, schedules the second pass BeforeReset before it, where acme is read again
// in its last minute with the reset GitHub now names a second later. Each line read in its last
// minute marks that reset read, so no reset is left for the next pass but the GraphQL counters',
// half an hour on, and the third pass is due an Interval after the second.
func TestRateReadingSchedulesNoSecondPassForASampledResetThatDrifts(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 50 * time.Second}
	initech := rateInstallation{id: "78", owner: "initech", repository: "initech/gadgets", coreAhead: 80 * time.Second}
	github := rateStandIn(t, acme, initech)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})
	drifted := acme
	drifted.coreAhead = 51 * time.Second

	dues, passes := rig.passes(t, 2, func(int) { github.list("1", drifted, initech) })

	second := github.coreReset(initech).Add(-githubrate.BeforeReset)
	checkDueAt(t, "the second pass", dues[0], second)
	if ids := lineIDs(passes[1]); !slices.Equal(ids, []string{"77", "78"}) {
		t.Fatalf("the second pass read %v, want 77 and 78", ids)
	}
	checkReadLine(t, github, passes[1][0], drifted)
	if !passes[1][0].BeforeReset || !passes[1][1].BeforeReset {
		t.Errorf("the second pass's lines say before_reset %t and %t, want both read in their last minute", passes[1][0].BeforeReset, passes[1][1].BeforeReset)
	}
	if held := rig.reader.heldResets(second); !slices.EqualFunc(held, []time.Time{github.graphQLReset, github.graphQLReset}, time.Time.Equal) {
		t.Errorf("the reading schedules by resets %v, want the two GraphQL resets alone: every core reset was read in its last minute", held)
	}
	checkDueAt(t, "the third pass", dues[1], second.Add(githubrate.Interval))
}

// A try that fails in a counter's last minute counts as that minute's reading, so it schedules no
// pass at once: whether the installation's reading fails or the App's list does, the pass marks
// each reset it holds that is in its last minute, so a try that keeps failing costs no pass after
// pass until the reset. acme's first reading holds its core reset 70 s ahead, the second pass is
// due BeforeReset before it and fails, and the third is due an Interval after the second. The
// installation's failed line says before_reset, since it was taken in the last minute of a reset
// the reading knows of it, so the hour shows a last-minute try that read nothing; the App's line,
// which names no installation, does not.
func TestRateReadingSchedulesNoPassAtOnceAfterAFailedTryInALastMinute(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 70 * time.Second}
	for _, tc := range []struct {
		name        string
		second      func(github *rateGitHub)
		error       string
		beforeReset bool
	}{
		{"the installation's reading fails", func(github *rateGitHub) {
			failing := acme
			failing.bareWorkflows = true
			github.list("1", failing)
		}, "core counter", true},
		{"the App's list fails", func(github *rateGitHub) { github.refuseLists(http.StatusInternalServerError) }, "list the implement App's installations", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			github := rateStandIn(t, acme)
			app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
			rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

			dues, passes := rig.passes(t, 2, func(int) { tc.second(github) })

			second := github.coreReset(acme).Add(-githubrate.BeforeReset)
			checkDueAt(t, "the second pass", dues[0], second)
			if len(passes[1]) != 1 {
				t.Fatalf("the second pass wrote %d lines, want one", len(passes[1]))
			}
			checkErrorLineAt(t, passes[1][0], tc.error, tc.beforeReset)
			if held := rig.reader.heldResets(second); !slices.EqualFunc(held, []time.Time{github.graphQLReset}, time.Time.Equal) {
				t.Errorf("the reading schedules by resets %v, want the GraphQL reset alone: the core reset's last minute was tried", held)
			}
			checkDueAt(t, "the third pass", dues[1], second.Add(githubrate.Interval))
		})
	}
}

// A reading that reads the core counter and then fails keeps what it read: the core reset 70 s
// ahead schedules the next pass BeforeReset before it, though GET /rate_limit answered 503 and the
// line carries the error.
func TestRateReadingKeepsTheCoreResetOfAReadingWhoseGraphQLReadFails(t *testing.T) {
	acme := rateInstallation{id: "77", owner: "acme", repository: "acme/widgets", coreAhead: 70 * time.Second, rateLimit: http.StatusServiceUnavailable}
	github := rateStandIn(t, acme)
	app := config.GitHubApp{AppID: "1", PrivateKey: github.key}
	rig := newRateRig(t, github, config.GitHubApps{Implement: app, Review: app})

	dues, passes := rig.passes(t, 1, nil)

	if len(passes[0]) != 1 {
		t.Fatalf("the pass wrote %d lines, want one", len(passes[0]))
	}
	checkErrorLine(t, passes[0][0], "GET /rate_limit")
	checkDueAt(t, "the second pass", dues[0], github.coreReset(acme).Add(-githubrate.BeforeReset))
}
