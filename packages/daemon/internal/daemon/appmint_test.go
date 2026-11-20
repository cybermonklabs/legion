package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// gitHubStandIn serves the GitHub App endpoints a boot's token mint calls, for whichever owner
// accounts discover names: installation discovery answers through discover, every installation
// is granted every permission a role's set names, the exchange and identity lookups succeed,
// and each App's slug is legion-test-<its id>, so the two bot logins differ as two real Apps' do.
// It counts each App's installation discoveries by the App id its JWT names.
type gitHubStandIn struct {
	mu          sync.Mutex
	discoveries map[string]int
}

func (g *gitHubStandIn) Discoveries() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.discoveries)
}

// jwtIssuer is the App id a request's App JWT names.
func jwtIssuer(t *testing.T, r *http.Request) string {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Errorf("installation discovery without an App JWT: %q", r.Header.Get("Authorization"))
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err == nil {
		err = json.Unmarshal(payload, &claims)
	}
	if err != nil {
		t.Errorf("decode the App JWT: %v", err)
	}
	return claims.Issuer
}

// appTokens is a real App token manager whose GitHub is a stand-in, the implement App id 1 and the
// review App id 2; discover answers each installation discovery, with its App and its number, from 1,
// among that App's. The manager is returned as itself, not as the interface, so a test that counts
// what the boot asks of it can embed it (bootMints) and keep every other method it answers.
func appTokens(t *testing.T, discover func(w http.ResponseWriter, r *http.Request, app string, n int)) (*appauth.Manager, *gitHubStandIn) {
	t.Helper()
	return appTokensWith(t, discover, nil)
}

// appTokensWith is appTokens whose stand-in asks answer first, when it is set: a request answer
// says it answered is answered so, and every other request as appTokens's stand-in answers it.
func appTokensWith(t *testing.T, discover func(w http.ResponseWriter, r *http.Request, app string, n int), answer func(w http.ResponseWriter, r *http.Request) bool) (*appauth.Manager, *gitHubStandIn) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	standIn := &gitHubStandIn{discoveries: map[string]int{}}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if answer != nil && answer(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/app/installations":
			app := jwtIssuer(t, r)
			standIn.mu.Lock()
			standIn.discoveries[app]++
			n := standIn.discoveries[app]
			standIn.mu.Unlock()
			discover(w, r, app, n)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":77}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			// A mint names the permissions its token holds, and refuses an answer wider than it
			// asked for (appauth), so the stand-in answers with them as the request named them,
			// as GitHub does.
			mintedToken(w, "installation-token", exchangeBody(t, r))
		case strings.HasPrefix(r.URL.Path, "/app/installations/"):
			// A mint reads what the installation is granted before it asks for a token and cuts
			// its request down to it (appauth's exchangeGranted). The stand-in's installation is
			// granted no issues, security_events or attestations, which these tests read nothing
			// of, so the cut leaves those out of each request.
			fmt.Fprint(w, `{"id":77,"permissions":{"actions":"write","checks":"write","contents":"write","metadata":"read","pull_requests":"write","statuses":"write","workflows":"write"}}`)
		case r.URL.Path == "/app":
			fmt.Fprintf(w, `{"slug":"legion-test-%s"}`, jwtIssuer(t, r))
		case strings.HasPrefix(r.URL.Path, "/users/"):
			fmt.Fprint(w, `{"id":42}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	return appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "1", PrivateKey: privatePEM},
		Review:    config.GitHubApp{AppID: "2", PrivateKey: privatePEM},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client()}), standIn
}

// quickAppMints shortens a boot mint's attempt and its retry for the test, restoring them after.
func quickAppMints(t *testing.T) {
	attempt, retry := appMintAttempt, appMintRetry
	appMintAttempt, appMintRetry = 500*time.Millisecond, bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond, Attempts: 3}
	t.Cleanup(func() { appMintAttempt, appMintRetry = attempt, retry })
}

// withStartRateLimits applies change to the start's rate-limit policy for the test, restoring the
// production policy after.
func withStartRateLimits(t *testing.T, change func(*rateLimitPolicy)) {
	t.Helper()
	production := startRateLimits
	change(&startRateLimits)
	t.Cleanup(func() { startRateLimits = production })
}

func installed(w http.ResponseWriter) {
	fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
}

// bootsOrExits runs the daemon until it answers /healthz or run returns, and says which.
func bootsOrExits(t *testing.T, cfg config.Config, tokens appauth.Source, within time.Duration) error {
	t.Helper()
	return bootsOrExitsLogging(t, cfg, tokens, within, quietLogger())
}

// bootsOrExitsLogging is bootsOrExits with the daemon logging to log.
func bootsOrExitsLogging(t *testing.T, cfg config.Config, tokens appauth.Source, within time.Duration, log *slog.Logger) error {
	t.Helper()
	return bootsOrExitsOn(t, cfg, tokens, repositoriesGitHub(t), within, log)
}

// bootsOrExitsOn is bootsOrExitsLogging with the daemon reading GitHub's REST API at githubAPI.
func bootsOrExitsOn(t *testing.T, cfg config.Config, tokens appauth.Source, githubAPI string, within time.Duration, log *slog.Logger) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, log, overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
			githubAPI:      githubAPI,
		})
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if stopped {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run after the test: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	var exited error
	answered := testwait.Holds(within, 20*time.Millisecond, func() bool {
		select {
		case err := <-done:
			stopped = true
			if err == nil {
				t.Fatal("run returned nil before the daemon answered /healthz")
			}
			exited = err
			return true
		default:
		}
		if response, err := pollClient.Get("http://127.0.0.1:" + strconv.Itoa(cfg.Port) + "/healthz"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return true
			}
		}
		return false
	})
	if exited != nil {
		return exited
	}
	if !answered {
		t.Fatalf("the daemon neither answered /healthz nor exited within %s", within)
	}
	return nil
}

// A GitHub request at boot that never answers is GitHub's trouble, not the App's: the boot mints
// again and comes up. Before, the one request ran until the boot budget ran out, and the daemon
// exited (a Stage 3 restart died exactly so: "mint implement GitHub App token at boot: GitHub App
// installation discovery: ... context deadline exceeded", #1347).
func TestABootWaitsOutAGitHubRequestThatNeverAnswers(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n == 1 {
			<-r.Context().Done()
			return
		}
		installed(w)
	})
	if err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second); err != nil {
		t.Fatalf("boot = %v, want it to mint again after one GitHub request that never answered", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 2, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's that never answered and one more, and the review App's one", got)
	}
}

// A definitive answer (the App's key refused) is refused at once, naming it and the owner the
// boot minted for (workflowConfig's mapping, acme): no retry changes it.
func TestABootRefusesADefinitiveGitHubAnswerAtOnce(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "mint implement GitHub App token at boot for acme: ") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want the refusal naming the implement mint, the owner acme and GitHub's 401", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's one: a 401 is not asked again", got)
	}
}

// Both Apps' tokens share one retry budget, so a boot waits at most appMintRetry before it refuses,
// however the failures fall between the two mints. Here the implement App's discovery fails twice
// and then answers, and the review App's never does: the budget's three attempts are spent before
// the review token is asked for more than once. With a budget per App the boot would wait both out,
// twice as long, which is what outlasted Stage 3's wait for /healthz.
func TestABootGivesBothAppTokensOneRetryBudget(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n > 2 {
			installed(w)
			return
		}
		http.Error(w, "{}", http.StatusBadGateway)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "retry budget (3 attempts)") || !strings.Contains(err.Error(), "mint review GitHub App token at boot") {
		t.Fatalf("boot = %v, want the refusal after the one budget's three attempts, naming the review mint", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 3, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's three and the review App's one", got)
	}
}

// bootMints counts the token mints the boot's App gate asks of a real token manager, by App role
// and owner account, in the order it asks for them. It counts until the gate closes: the daemon
// logs its "appauth" boot stage the moment the last owner is minted, and the GitHub reads of the
// mapping that follow mint the implement App once for every repository they name
// (defaultBranches.fetch), which is not what the gate proved. close is called from the log
// (mintGateLog), after the gate's last mint returned and before any read that follows it.
type bootMints struct {
	*appauth.Manager
	mu     sync.Mutex
	closed bool
	minted []string
}

func (b *bootMints) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	b.mu.Lock()
	if !b.closed {
		b.minted = append(b.minted, string(role)+" "+owner)
	}
	b.mu.Unlock()
	return b.Manager.Token(ctx, role, owner)
}

func (b *bootMints) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
}

// Minted is what the gate asked for, "<role> <owner>" each, in order.
func (b *bootMints) Minted() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.minted)
}

// mintGateLog is a daemon log that closes mints' count at the boot stage line the App gate ends
// with, and keeps nothing else.
func mintGateLog(mints *bootMints) *slog.Logger {
	return slog.New(slog.NewJSONHandler(lineWriter(func(line []byte) {
		if bytes.Contains(line, []byte(`"stage":"appauth"`)) {
			mints.close()
		}
	}), nil))
}

// lineWriter hands each log line a handler writes to a function, and keeps none of them. A slog
// handler writes one line per record, under a lock of its own, so the function is called once
// per record and never from two goroutines at once.
type lineWriter func(line []byte)

func (w lineWriter) Write(line []byte) (int, error) {
	w(line)
	return len(line), nil
}

// mappedWorkflowConfig is workflowConfig over a repository mapping of its own: every name mapped
// to the project, the first of them its default. The mapping read is the one boot call this
// Dispatch answers itself; every other it answers as workflowConfig's Dispatch does
// (servedBootCall), whose mapping is the one acme/widgets every other boot of this package reads.
func mappedWorkflowConfig(t *testing.T, natsURL string, mapped ...string) config.Config {
	t.Helper()
	cfg := workflowConfig(t, natsURL)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/settings/repo-projects":
			rows := make([]map[string]any, 0, len(mapped))
			for index, repo := range mapped {
				rows = append(rows, map[string]any{"repo": repo, "project": cfg.Project, "default": index == 0})
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(rows); err != nil {
				t.Errorf("write the repository mapping: %v", err)
			}
		case servedBootCall(t, w, r, cfg.Project):
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/issues":
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode([]any{}); err != nil {
				t.Errorf("write Dispatch issues: %v", err)
			}
		default:
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	cfg.DispatchURL = server.URL
	return cfg
}

// The boot proves each App's key once for each owner account of the repositories the mapping
// serves, not once for each repository: a token reaches every repository the account's
// installation holds, so a second repository of an account already minted for proves nothing
// further, while it costs the boot another mint and another retry budget to wait out. Here three
// repositories belong to two accounts, one of them spelled in the mapping as the account is not,
// and each App is minted twice, on the two accounts lowercased and in their sorted order, so one
// boot's log reads like the next's.
//
// Both Apps are installed on a third account, initech, that no mapped repository belongs to, and
// neither is minted on it: R1 asks only that an installed owner the mapping does not reach is not
// PROVED at boot, and a bundle reads its App's installation when it is built, so nothing of that
// owner is wanted before then. It is not that this project needs no work there: under R3 and R4
// an issue of this project may name an initech repository, and the daemon records and runs it.
// Without initech the row would pass as well on a boot that minted every owner the Apps'
// installations name, because those would be the mapped owners exactly.
func TestABootMintsEachAppOnceForEachOwnerOfTheMapping(t *testing.T) {
	quickAppMints(t)
	manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}},{"id":88,"account":{"login":"globex"}},{"id":99,"account":{"login":"initech"}}]`)
	})
	mints := &bootMints{Manager: manager}
	cfg := mappedWorkflowConfig(t, workflowNATS(t), "acme/widgets", "Acme/Gadgets", "globex/tools")
	if err := bootsOrExitsLogging(t, cfg, mints, 60*time.Second, mintGateLog(mints)); err != nil {
		t.Fatalf("boot = %v, want the daemon serving three repositories of two owner accounts", err)
	}
	minted := mints.Minted()
	for _, mint := range minted {
		if strings.HasSuffix(mint, " initech") {
			t.Errorf("the boot's App gate asked for %q, want no mint on initech: both Apps are installed there, and no repository the mapping serves belongs to it", mint)
		}
	}
	want := []string{"implement acme", "review acme", "implement globex", "review globex"}
	if !slices.Equal(minted, want) {
		t.Errorf("the mints the boot's App gate asked for = %v, want %v: one of each App on each owner account of the mapping, and none for a second repository of an account", minted, want)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want one each: one discovery names every installation an App has", got)
	}
}

// rateLimitWaitLine is the line a start logs for each wait for GitHub's rate limit on an App call
// (bootprobe.Run's wait outcome).
const rateLimitWaitLine = "boot probe waits out GitHub's rate limit"

// discoveredOnAcme answers every installation discovery with the App's one installation, on acme.
func discoveredOnAcme(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) }

// spentUntil is an answer for appTokensWith or commandTokens: each request refused names is
// answered with GitHub's 403 for a spent rate limit (spentLimit) until the limit's reset, ahead of
// the first such request, which it stores in reset; every request after the reset, and every
// other request, is left to the stand-in.
func spentUntil(ahead time.Duration, reset *atomic.Int64, refused func(r *http.Request) bool) func(w http.ResponseWriter, r *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if !refused(r) {
			return false
		}
		reset.CompareAndSwap(0, time.Now().Add(ahead).Unix())
		if !time.Now().Before(time.Unix(reset.Load(), 0)) {
			return false
		}
		spentLimit(w, reset.Load())
		return true
	}
}

// botIdentityRead is whether r is a mint's read of its App's bot user (appauth's botIdentity).
func botIdentityRead(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/users/") }

// oneRateLimitWait is the one rate-limit wait line of the daemon's JSON log logs, and fails the
// test unless there is exactly one, for probe.
func oneRateLimitWait(t *testing.T, logs, probe string) map[string]any {
	t.Helper()
	waits := loggedLines(t, logs, rateLimitWaitLine)
	if len(waits) != 1 || waits[0]["probe"] != probe {
		t.Fatalf("rate-limit wait lines = %v, want one, for the %s", waits, probe)
	}
	return waits[0]
}

// loggedDuration is the duration a JSON log line holds under key.
func loggedDuration(t *testing.T, line map[string]any, key string) time.Duration {
	t.Helper()
	text, _ := line[key].(string)
	duration, err := time.ParseDuration(text)
	if err != nil {
		t.Fatalf("the line's %s = %v, want a duration: %v", key, line[key], err)
	}
	return duration
}

// commandTokens is a real App token manager in token mode whose GitHub is a stand-in, the implement
// App id 1 and the review App id 2. Each App's token_command releases its installation on acme with
// a token that names the App, the owner the command was asked for ("any" for none) and whether the
// token is read-only ("1", else "write"): ghs_<role>_<owner>_<read>. The stand-in answers what
// token mode asks of GitHub under such a token — the GraphQL viewer (legion-<role>[bot]), GET
// /apps/legion-<role>, the bot user, and the installation's repositories, acme/widgets alone — and
// asks answer first, as appTokensWith's does.
func commandTokens(t *testing.T, answer func(w http.ResponseWriter, r *http.Request) bool) *appauth.Manager {
	t.Helper()
	ids := map[string]string{string(appauth.Implement): "1", string(appauth.Review): "2"}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if answer(w, r) {
			return
		}
		role, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "token ghs_"), "_")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			fmt.Fprintf(w, `{"data":{"viewer":{"login":"legion-%s[bot]"}}}`, role)
		case strings.HasPrefix(r.URL.Path, "/apps/legion-"):
			slug := strings.TrimPrefix(r.URL.Path, "/apps/")
			fmt.Fprintf(w, `{"id":%s,"slug":%q}`, ids[strings.TrimPrefix(slug, "legion-")], slug)
		case strings.HasPrefix(r.URL.Path, "/users/"):
			fmt.Fprint(w, `{"id":42}`)
		case r.URL.Path == "/installation/repositories":
			fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	command := func(role appauth.AppRole) string {
		return fmt.Sprintf(`printf '{"installations":{"acme":{"installation_id":77,"token":"ghs_%s_%%s_%%s","expires_at":"%s"}}}' "${%s:-any}" "${%s:-write}"`,
			role, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), appauth.TokenCommandOwner, appauth.TokenCommandReadOnly)
	}
	return appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: ids[string(appauth.Implement)], TokenCommand: command(appauth.Implement)},
		Review:    config.GitHubApp{AppID: ids[string(appauth.Review)], TokenCommand: command(appauth.Review)},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
}

// A start whose read of the App's bot user GitHub refuses for a spent rate limit waits until the
// reset GitHub names and serves after it (c-boot-mint-rate-limit case 1), saying so in one line
// that names the call, the time it waits until and the total waited. The refusals outlast the
// three quick attempts, which the wait does not spend: before, the start exited once they were
// spent, before the reset.
func TestABootWaitsOutARateLimitOnTheAppBotIdentityReadAndServesAfterTheReset(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(3*time.Second, &reset, botIdentityRead))
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the App's bot user and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := time.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	wait := oneRateLimitWait(t, logs.String(), "GitHub App tokens mint")
	if want := resetAt.UTC().Format(time.RFC3339); wait["until"] != want {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], want)
	}
	if waited := loggedDuration(t, wait, "waited"); waited <= 0 || waited > 3*time.Second+100*time.Millisecond {
		t.Errorf("the wait line's waited = %s, want the time to the reset, at most 3s", waited)
	}
}

// GitHub's 403 naming its rate limit in its message alone, with calls left in its headers, is a
// rate limit to the start's mint too (c-boot-mint-rate-limit case 2). It names no time, so the
// start waits a minute, says so, and serves after it. Before, the start read it as GitHub's answer
// about the App and exited at the first attempt.
func TestABootWaitsOutARateLimitNamedOnlyByItsMessageOnTheAppMint(t *testing.T) {
	quickAppMints(t)
	var refusals atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !botIdentityRead(r) || refusals.Add(1) > 1 {
			return false
		}
		w.Header().Set("X-RateLimit-Limit", "15000")
		w.Header().Set("X-RateLimit-Remaining", "14671")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 7.","status":"403"}`)
		return true
	})
	logs := &syncBuffer{}
	started := time.Now()
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 90*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out the rate limit GitHub's message names and serve", err)
	}
	if took := time.Since(started); took < time.Minute {
		t.Errorf("the daemon served %s after it started, want after the minute's wait", took)
	}
	// The line gives the wait left when it is written, to a tenth of a second, so a slow runner
	// can show a little under the minute decided; never more.
	if wait := loggedDuration(t, oneRateLimitWait(t, logs.String(), "GitHub App tokens mint"), "wait"); wait <= time.Minute-time.Second || wait > time.Minute {
		t.Errorf("the wait line's wait = %s, want 1m0s, the wait for a refusal that names no time", wait)
	}
}

// A start whose read of the Apps' logins GitHub refuses for a spent rate limit waits until the reset
// and serves after it (c-boot-mint-rate-limit case 3). Before, the start exited once the three quick
// attempts were spent, before the reset.
func TestABootWaitsOutARateLimitOnTheAppLoginsRead(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(3*time.Second, &reset, func(r *http.Request) bool { return r.URL.Path == "/app" }))
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the logins read and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := time.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	if wait := oneRateLimitWait(t, logs.String(), "GitHub App logins read"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], resetAt.UTC().Format(time.RFC3339))
	}
}

// A start whose token comes from a token_command waits out GitHub's rate limit on the App read that
// checks the released token (appauth's tokenIdentity: the GraphQL viewer) and serves after the reset
// (c-boot-mint-rate-limit case 4). The limit refuses the mint's write token on acme, so the mapping's
// reads and the logins read before it pass. Before, the start exited once the three quick attempts
// were spent, before the reset.
func TestABootWaitsOutARateLimitOnATokenCommandsAppRead(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens := commandTokens(t, spentUntil(3*time.Second, &reset, func(r *http.Request) bool {
		return r.URL.Path == "/graphql" && r.Header.Get("Authorization") == "token ghs_implement_acme_write"
	}))
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the released token's App read and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := time.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	if wait := oneRateLimitWait(t, logs.String(), "GitHub App tokens mint"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], resetAt.UTC().Format(time.RFC3339))
	}
}

// bootStoppedWhen runs the daemon on cfg with tokens, logging to logs as JSON, until ready reads
// what the test waits for in the log, then stops it, and says how long run took to return after the
// stop. The test fails if ready does not hold within within, if run returns before the stop, or if
// it returns an error after it.
func bootStoppedWhen(t *testing.T, cfg config.Config, tokens appauth.Source, logs *syncBuffer, within time.Duration, ready func(logs string) bool) time.Duration {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, slog.New(slog.NewJSONHandler(logs, nil)), overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
			githubAPI:      repositoriesGitHub(t),
		})
	}()
	var ended error
	returned := false
	if !testwait.Holds(within, testwait.Poll, func() bool {
		select {
		case ended = <-done:
			returned = true
			return true
		default:
		}
		return ready(logs.String())
	}) {
		t.Fatalf("the daemon's log did not hold what the test waits for within %s; it holds:\n%s", within, logs.String())
	}
	if returned {
		t.Fatalf("run returned %v before the test stopped it; the log holds:\n%s", ended, logs.String())
	}
	cancel()
	stopped := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run stopped by the test = %v, want a clean stop", err)
		}
		return time.Since(stopped)
	case <-time.After(30 * time.Second):
		t.Fatal("run did not return within 30s of its stop")
	}
	return 0
}

// The token the start's repository reads mint (defaultBranches.fetch, inside nameMapping) waits out
// GitHub's rate limit as the start's own mint does: here the first implement token the start mints
// expires inside appauth's refresh window, so the repository read mints again, and GitHub refuses
// that exchange once for a spent limit with a reset two seconds ahead. The start waits until the
// reset, saying so in one line for the repository read, and serves. GitHub never refused the read
// of the default branch itself, so #4043's wait line is not logged.
func TestABootWaitsOutARateLimitOnTheMintOfItsRepositoryReads(t *testing.T) {
	quickAppMints(t)
	var exchanges, reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") || jwtIssuer(t, r) != "1" {
			return false
		}
		switch exchanges.Add(1) {
		case 1:
			held, _ := json.Marshal(exchangeBody(t, r))
			fmt.Fprintf(w, `{"token":"installation-token","expires_at":%q,"repository_selection":"all","permissions":%s}`,
				time.Now().Add(10*time.Minute).Format(time.RFC3339), held)
			return true
		case 2:
			reset.Store(time.Now().Add(2 * time.Second).Unix())
			spentLimit(w, reset.Load())
			return true
		}
		return false
	})
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on its repository read's mint and serve", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if wait := oneRateLimitWait(t, logs.String(), "repository read"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], resetAt.UTC().Format(time.RFC3339))
	}
	if got := exchanges.Load(); got < 3 {
		t.Errorf("implement token exchanges = %d, want the boot's, the refused one and the one after the reset", got)
	}
	if waits := loggedLines(t, logs.String(), waitLine); len(waits) != 0 {
		t.Errorf("default-branch wait lines = %v, want none: GitHub never refused the read itself", waits)
	}
}

// A Retry-After GitHub sends on the start's mint is waited as GitHub asks, at most the maximum
// (c-boot-mint-rate-limit case 5, first half; the maximum shortened to 2 s): a 429 asking for a
// second waits a second, one asking for 30 s waits the 2 s, and the start serves after each.
func TestABootWaitsAsLongAsGitHubAsksAtMostTheMaximum(t *testing.T) {
	quickAppMints(t)
	withStartRateLimits(t, func(policy *rateLimitPolicy) { policy.Max = 2 * time.Second })
	for _, tc := range []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{name: "a Retry-After of a second", retryAfter: "1", want: time.Second},
		{name: "a Retry-After above the maximum", retryAfter: "30", want: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var reads []time.Time
			tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
				if !botIdentityRead(r) {
					return false
				}
				mu.Lock()
				reads = append(reads, time.Now())
				first := len(reads) == 1
				mu.Unlock()
				if !first {
					return false
				}
				w.Header().Set("Retry-After", tc.retryAfter)
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit."}`)
				return true
			})
			logs := &syncBuffer{}
			if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
				t.Fatalf("boot = %v, want it to wait out GitHub's Retry-After and serve", err)
			}
			if wait := loggedDuration(t, oneRateLimitWait(t, logs.String(), "GitHub App tokens mint"), "wait"); wait != tc.want {
				t.Errorf("the wait line's wait = %s, want %s", wait, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reads) < 2 || reads[1].Sub(reads[0]) < tc.want || reads[1].Sub(reads[0]) > tc.want+3*time.Second {
				t.Fatalf("GitHub was read for the bot user at %v, want the refused read and one more %s after it", reads, tc.want)
			}
		})
	}
}

// Refusals of one call of the start that name no time wait a minute, then two (c-boot-mint-rate-limit
// case 5, second half, at the production minute): GitHub answers every read of the App's bot user
// with a 429 and no rate-limit header. The first wait line says a minute; the second, a minute
// later, says two minutes, until a time two minutes ahead. A stop during that wait ends the start
// at once.
func TestABootWaitsOneMinuteThenTwoForRefusalsOfOneCallThatNameNoTime(t *testing.T) {
	quickAppMints(t)
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !botIdentityRead(r) {
			return false
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"Too many requests."}`)
		return true
	})
	logs := &syncBuffer{}
	took := bootStoppedWhen(t, workflowConfig(t, workflowNATS(t)), tokens, logs, 90*time.Second, func(logs string) bool {
		return strings.Count(logs, rateLimitWaitLine) >= 2
	})
	if took > time.Second {
		t.Errorf("run returned %s after its stop, want within 1s", took)
	}
	waits := loggedLines(t, logs.String(), rateLimitWaitLine)
	if len(waits) != 2 {
		t.Fatalf("rate-limit wait lines = %v, want two", waits)
	}
	// Each line gives the wait left when it is written, to a tenth of a second, so a slow runner can
	// show a little under the wait decided; never more.
	if first, second := loggedDuration(t, waits[0], "wait"), loggedDuration(t, waits[1], "wait"); first <= time.Minute-time.Second || first > time.Minute ||
		second <= 2*time.Minute-time.Second || second > 2*time.Minute {
		t.Errorf("the waits = %s and %s, want 1m0s and then 2m0s", first, second)
	}
	if gap := loggedAt(t, waits[1]).Sub(loggedAt(t, waits[0])); gap < time.Minute || gap > time.Minute+10*time.Second {
		t.Errorf("the second wait came %s after the first, want about a minute", gap)
	}
	until, err := time.Parse(time.RFC3339, fmt.Sprint(waits[1]["until"]))
	if ahead := until.Sub(loggedAt(t, waits[1])); err != nil || ahead < 2*time.Minute-time.Second || ahead > 2*time.Minute+time.Second {
		t.Errorf("the second wait's until = %v (%v), want two minutes after its line", waits[1]["until"], err)
	}
}

// All rate-limit waits of one start end at the bound after the first of them (c-boot-mint-rate-limit
// case 6, the bound shortened to 3 s): GitHub refuses every read of the App's bot user with a reset
// ten seconds ahead, the one wait ends at the bound, and the refusal after it ends the start, naming
// the call, GitHub's last answer with its headers and message, and the 3 s waited.
func TestABootEndsWhenItsRateLimitWaitsReachTheirBound(t *testing.T) {
	quickAppMints(t)
	withStartRateLimits(t, func(policy *rateLimitPolicy) { policy.Bound = 3 * time.Second })
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !botIdentityRead(r) {
			return false
		}
		spentLimit(w, time.Now().Add(10*time.Second).Unix())
		return true
	})
	logs := &syncBuffer{}
	started := time.Now()
	err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
	took := time.Since(started)
	for _, want := range []string{
		"the GitHub App tokens mint waited 3s for GitHub's rate limit, the most a start waits, and GitHub still refuses it: ",
		"GitHub answered GET /users/legion-test-1[bot] with 403",
		`X-RateLimit-Remaining "0"`,
		"API rate limit exceeded for installation ID 7.",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("boot = %v, want the start ended naming %q", err, want)
		}
	}
	if took < 3*time.Second || took > 15*time.Second {
		t.Errorf("the start ended %s after it began, want soon after the 3s bound", took)
	}
	if waits := loggedLines(t, logs.String(), rateLimitWaitLine); len(waits) != 1 {
		t.Errorf("rate-limit wait lines = %v, want the one that ended at the bound", waits)
	}
}

// A stop that arrives while the start waits out GitHub's rate limit ends the start at once and
// cleanly (c-boot-mint-rate-limit case 9): the App's bot user is refused until a reset a minute
// ahead, and run returns nil within a second of the stop.
func TestABootStoppedWhileItWaitsOutARateLimitEndsAtOnce(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(time.Minute, &reset, botIdentityRead))
	logs := &syncBuffer{}
	took := bootStoppedWhen(t, workflowConfig(t, workflowNATS(t)), tokens, logs, 30*time.Second, func(logs string) bool {
		return strings.Contains(logs, rateLimitWaitLine)
	})
	if took > time.Second {
		t.Errorf("run returned %s after its stop, want within 1s", took)
	}
	if stopped := loggedMessages(t, logs.String(), "legion daemon stopped before it read its repository mapping"); len(stopped) != 1 {
		t.Errorf("the stop's lines = %v, want one", stopped)
	}
}
