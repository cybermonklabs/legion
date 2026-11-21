package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
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

// gitHubStandIn serves the GitHub App endpoints a boot's proof of each App's key and a token mint
// call, for whichever owner accounts discover names: installation discovery answers through
// discover, every installation is granted every permission a role's set names, the exchange and
// identity lookups succeed, and each App's slug is legion-test-<its id>, so the two bot logins
// differ as two real Apps' do. It counts each App's installation discoveries by the App id its JWT
// names, and every token exchange, which is a mint.
type gitHubStandIn struct {
	mu          sync.Mutex
	discoveries map[string]int
	exchanges   int
}

func (g *gitHubStandIn) Discoveries() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.discoveries)
}

// Exchanges is how many installation tokens GitHub was asked to mint.
func (g *gitHubStandIn) Exchanges() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exchanges
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
// what the boot asks of it can embed it (refusingMints) and keep every other method it answers.
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
			standIn.mu.Lock()
			standIn.exchanges++
			standIn.mu.Unlock()
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

// quickAppMints shortens the attempt and the retry of a boot's read of GitHub's App endpoints for
// the test, restoring them after.
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

// A GitHub request at boot that does not answer within an attempt's bound is GitHub's trouble, not
// the App's: the attempt the bound ended is transient, and the boot's proof of the App's key runs
// again and comes up. Here the implement App's first read of its installations is held for half an
// attempt's bound past the bound and then answered, so the first attempt ends on its own deadline
// and the second, which joins the read still in flight (appauth.Manager.InstalledOwners), takes
// its answer: one read of each App, two attempts, the first said once as transient. Before, the
// one request ran until the boot budget ran out, and the daemon exited (a Stage 3 restart died
// exactly so: "mint implement GitHub App token at boot: GitHub App installation discovery: ...
// context deadline exceeded", #1347). Control: with appAttempt classing its own deadline as a
// refusal, the boot exits on the first attempt's "context deadline exceeded".
func TestABootRunsItsKeyProofAgainAfterAGitHubRequestOutlastsAnAttempt(t *testing.T) {
	quickAppMints(t)
	held := appMintAttempt + appMintAttempt/2
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n == 1 {
			select {
			case <-time.After(held):
			case <-r.Context().Done():
				return
			}
		}
		installed(w)
	})
	var logged syncBuffer
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
		t.Fatalf("boot = %v, want the key proof run again after the attempt GitHub's request outlasted", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want one of each: the second attempt joins the implement App's read in flight", got)
	}
	retried := warnLinesNaming(logged.String(), "boot probe failed transiently", `"probe":"GitHub App key proof"`)
	if len(retried) != 1 {
		t.Fatalf("the key proof was said transient %d times, want once: the attempt its bound ended, then the one that passed", len(retried))
	}
	if !strings.Contains(retried[0], context.DeadlineExceeded.Error()) {
		t.Errorf("the transient attempt's line = %s, want it naming the attempt's deadline", retried[0])
	}
}

// A definitive answer (the App's key refused) is refused at once, naming the App whose key GitHub
// refused and GitHub's answer: no retry changes it.
func TestABootRefusesADefinitiveGitHubAnswerAtOnce(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "prove the implement App's key by reading its installations: ") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want the refusal naming the implement App's key proof and GitHub's 401", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's one: a 401 is not asked again", got)
	}
}

// Both Apps' key proofs share one retry budget, so a boot waits at most appMintRetry before it
// refuses, however the failures fall between the two reads. Here the implement App's discovery
// fails twice and then answers, and the review App's never does: the budget's three attempts are
// spent before the review App's installations are asked for more than once. With a budget per App
// the boot would wait both out, twice as long, which is what outlasted Stage 3's wait for /healthz.
func TestABootGivesBothAppsKeyProofsOneRetryBudget(t *testing.T) {
	quickAppMints(t)
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n > 2 {
			installed(w)
			return
		}
		http.Error(w, "{}", http.StatusBadGateway)
	})
	err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second)
	if err == nil || !strings.Contains(err.Error(), "retry budget (3 attempts)") || !strings.Contains(err.Error(), "review App") {
		t.Fatalf("boot = %v, want the refusal after the one budget's three attempts, naming the review App's key proof", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 3, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's three and the review App's one", got)
	}
}

// refusingMints is a real App token manager over a stand-in GitHub whose every mint, of a write
// lease or a read lease, is refused before it reaches GitHub and, where log is set, written to it
// as a line of its own (leaseAskedLine), so a test whose daemon logs to the same writer reads which
// leases were asked before the daemon served (linesBeforeServing): everything else it answers as
// the manager does, the App logins and the App-level read of each App's installations among them.
type refusingMints struct {
	*appauth.Manager
	log io.Writer
}

// leaseAskedLine is the message of the line refusingMints writes for each lease it is asked for.
const leaseAskedLine = "test: the token source was asked for a lease"

func (m *refusingMints) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return m.refuse(role, owner)
}

func (m *refusingMints) ReadToken(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return m.refuse(role, owner)
}

// refuse writes the line of a lease asked of role's App on owner's installation where log is set,
// and refuses it.
func (m *refusingMints) refuse(role appauth.AppRole, owner string) (appauth.Lease, error) {
	if m.log != nil {
		fmt.Fprintf(m.log, "{\"msg\":%q,\"app\":%q,\"owner\":%q}\n", leaseAskedLine, role, owner)
	}
	return appauth.Lease{}, errors.New("this token source mints no lease")
}

// linesBeforeServing is each line of logged, a daemon's JSON log, whose message is msg and that
// comes before the boot's last stage line, the API's (`legion workflow boot stage`, stage `api`),
// which run writes just before it serves: what the boot did before the API could answer, apart
// from what the refresher and the daemon's other loops do once it serves.
func linesBeforeServing(logged, msg string) []string {
	var lines []string
	for line := range strings.SplitSeq(logged, "\n") {
		if strings.Contains(line, `"msg":"legion workflow boot stage"`) && strings.Contains(line, `"stage":"api"`) {
			return lines
		}
		if strings.Contains(line, `"msg":"`+msg+`"`) {
			lines = append(lines, line)
		}
	}
	return lines
}

// The boot proves each role App's key with GitHub's App-level read of its installations, GET
// /app/installations under the App's JWT, once for each App, and mints no token for any owner: a
// project works in each repository both role Apps reach, so no owner's installation is the boot's
// to prove. A refusal of that read stops the boot, naming the App; an App with no installation
// boots, and one WARN line names it, since an operator may install it once the daemon is up. The
// token source here refuses every mint, of a write lease or a read lease, and the boot comes up
// all the same.
//
// The first case boots over a live record on acme/widgets, an owner both Apps are installed on,
// so a mint per owner put back into the boot has an owner to mint for. The record carries its
// repository columns and no claim, and no `repository_renames` pair is configured, so neither the
// backfill nor the rename pass reads GitHub and no worker is relaunched. The boot asks the token
// source for no lease before it serves, counted up to its last stage line, the API's: once the API
// serves, the refresher's preparation of acme/widgets asks for one to read its default branch
// (prepareAtStart), which this source refuses as it refuses every other. GitHub is asked for no
// installation token at all, since every lease the daemon asks goes to this source.
//
// The control is a mint per owner put back into the key proof (INFRA-4401 P4; dispatch://INFRA-4401
// check a-go, item 8): proveAppKeys asking the token source for each role's token on acme. That
// mint is refused and the boot stops.
func TestTheBootProvesEachAppKeyWithItsInstallations(t *testing.T) {
	t.Run("each App's key is proved and nothing is minted", func(t *testing.T) {
		quickAppMints(t)
		manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		var logged syncBuffer
		tokens := &refusingMints{Manager: manager, log: &logged}
		cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
		seedLiveTreeOn(t, cfg, "acme/widgets")
		if err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
			t.Fatalf("boot = %v, want the daemon up: the boot mints no token, so a source that refuses every mint stops nothing", err)
		}
		if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
			t.Errorf("installation discoveries by App = %v, want one each: the key proof of the implement App (1) and the review App (2)", got)
		}
		if asked := linesBeforeServing(logged.String(), leaseAskedLine); len(asked) != 0 {
			t.Errorf("the boot asked the token source for %d leases before it served, want none: %q", len(asked), asked)
		}
		if exchanged := standIn.Exchanges(); exchanged != 0 {
			t.Errorf("GitHub was asked for %d installation tokens, want none", exchanged)
		}
		if warned := warnLinesNaming(logged.String(), "a GitHub App is installed on no account"); len(warned) != 0 {
			t.Errorf("the boot warned %q, want nothing: both Apps are installed on acme", warned)
		}
	})

	t.Run("a refused key stops the boot, naming the App", func(t *testing.T) {
		quickAppMints(t)
		manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, app string, _ int) {
			if app == "2" {
				http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
				return
			}
			installed(w)
		})
		err := bootsOrExits(t, workflowConfig(t, workflowNATS(t)), &refusingMints{Manager: manager}, 60*time.Second)
		if err == nil || !strings.Contains(err.Error(), "prove the review App's key by reading its installations: ") || !strings.Contains(err.Error(), "(401)") {
			t.Fatalf("boot = %v, want the refusal naming the review App's key proof and GitHub's 401", err)
		}
		if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
			t.Errorf("installation discoveries by App = %v, want one each: a 401 is not asked again", got)
		}
	})

	t.Run("an App with no installation boots, with one WARN line naming it", func(t *testing.T) {
		quickAppMints(t)
		manager, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, app string, _ int) {
			if app == "1" {
				fmt.Fprint(w, `[]`)
				return
			}
			installed(w)
		})
		var logged syncBuffer
		if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), &refusingMints{Manager: manager}, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
			t.Fatalf("boot = %v, want the daemon up: an App installed on no account boots", err)
		}
		if warned := warnLinesNaming(logged.String(), "a GitHub App is installed on no account"); len(warned) != 1 || !strings.Contains(warned[0], `"app":"implement"`) {
			t.Fatalf("the boot's WARN lines of an App installed on no account = %q, want one naming the implement App", warned)
		}
	})
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

// keyProofRead is whether r is the boot's proof of an App's key: its read of the App's
// installations under the App's JWT (proveAppKeys).
func keyProofRead(r *http.Request) bool { return r.URL.Path == "/app/installations" }

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
	return commandTokensWith(t, answer, func(appauth.AppRole) string { return "" })
}

// commandTokensWith is commandTokens whose token_command of each App role runs the shell that
// before gives for the role first, then prints its release.
func commandTokensWith(t *testing.T, answer func(w http.ResponseWriter, r *http.Request) bool, before func(role appauth.AppRole) string) *appauth.Manager {
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
		return before(role) + fmt.Sprintf(`printf '{"installations":{"acme":{"installation_id":77,"token":"ghs_%s_%%s_%%s","expires_at":"%s"}}}' "${%s:-any}" "${%s:-write}"`,
			role, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), appauth.TokenCommandOwner, appauth.TokenCommandReadOnly)
	}
	return appauth.New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: ids[string(appauth.Implement)], TokenCommand: command(appauth.Implement)},
		Review:    config.GitHubApp{AppID: ids[string(appauth.Review)], TokenCommand: command(appauth.Review)},
	}, appauth.Options{BaseURL: server.URL, HTTPClient: server.Client(), Stderr: io.Discard})
}

// A token_command still running when an attempt of the boot's key proof ends holds the boot
// without ending it, however many attempt bounds it outlasts: a grant may wait on a person's
// approval. Each such attempt is pending and spends no attempt of appMintRetry
// (appauth.ErrCommandPending, appAttempt), the next attempt joins the command's run still going
// (appauth.Manager.InstalledOwners, whose token-mode read is runTokenCommand's), and once the
// command prints its release the key proof passes and the boot serves. Here each App's
// token_command answers the logins read at once, and holds the key proof's release of every
// installation for twice the bounds of all appMintRetry's attempts. Control: with InstalledOwners
// answering its caller's bare ctx.Err() in token mode too, as it did before INFRA-4401's review
// 5996655281, each attempt the bound ends is transient, and the boot refuses once the three are
// spent, naming the key proof's context deadline exceeded.
func TestABootWaitsForATokenCommandThatOutlastsEveryAttemptOfTheKeyProof(t *testing.T) {
	quickAppMints(t)
	hold := 2 * time.Duration(appMintRetry.Attempts) * appMintAttempt
	dir := t.TempDir()
	tokens := commandTokensWith(t, func(http.ResponseWriter, *http.Request) bool { return false }, func(role appauth.AppRole) string {
		// The runs that name no owner are the logins read's, then the key proof's, which holds.
		runs := filepath.Join(dir, string(role)+".runs")
		return fmt.Sprintf(`if [ -z "${%s}" ]; then echo run >>%q; [ "$(( $(wc -l <%q) ))" -ne 2 ] || sleep %.3f; fi; `,
			appauth.TokenCommandOwner, runs, runs, hold.Seconds())
	})
	logs := &syncBuffer{}
	started := time.Now()
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait for the token_command past every attempt of the key proof and serve", err)
	}
	if took := time.Since(started); took < hold {
		t.Errorf("the daemon served %s after it started, want after the key proof's command held %s", took, hold)
	}
	byProbe := func(lines []map[string]any, probe string) []map[string]any {
		return slices.DeleteFunc(lines, func(line map[string]any) bool { return line["probe"] != probe })
	}
	pending := loggedMessages(t, logs.String(), "boot probe's work is still running")
	if logins := byProbe(slices.Clone(pending), "GitHub App logins read"); len(logins) != 0 {
		t.Errorf("the logins read was pending %d times, want never: its command answers at once", len(logins))
	}
	proof := byProbe(pending, "GitHub App key proof")
	if len(proof) <= appMintRetry.Attempts {
		t.Fatalf("the key proof was pending %d times, want more than appMintRetry's %d attempts", len(proof), appMintRetry.Attempts)
	}
	for _, line := range proof {
		if detail, _ := line["detail"].(string); !strings.Contains(detail, `.token_command for "" is still running after context deadline exceeded (a grant may wait on a person's approval)`) {
			t.Errorf("a pending line's detail = %q, want runTokenCommand's answer that the token_command's release of every installation is still running", detail)
		}
	}
	if transient := byProbe(loggedMessages(t, logs.String(), "boot probe failed transiently"), "GitHub App key proof"); len(transient) != 0 {
		t.Errorf("the key proof failed transiently %d times, want never: each attempt the command outlasted is pending", len(transient))
	}
}

// A start whose proof of an App's key GitHub refuses for a spent rate limit waits until the reset
// GitHub names and serves after it (c-boot-mint-rate-limit case 1, on the key proof), saying so in
// one line that names the call, the time it waits until and the total waited. The refusals outlast
// the three quick attempts, which the wait does not spend: before, the start exited once they were
// spent, before the reset.
func TestABootWaitsOutARateLimitOnTheAppKeyProofAndServesAfterTheReset(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(3*time.Second, &reset, keyProofRead))
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the key proof and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := time.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	wait := oneRateLimitWait(t, logs.String(), "GitHub App key proof")
	if want := resetAt.UTC().Format(time.RFC3339); wait["until"] != want {
		t.Errorf("the wait line's until = %v, want GitHub's reset, %s", wait["until"], want)
	}
	if waited := loggedDuration(t, wait, "waited"); waited <= 0 || waited > 3*time.Second+100*time.Millisecond {
		t.Errorf("the wait line's waited = %s, want the time to the reset, at most 3s", waited)
	}
}

// GitHub's 403 naming its rate limit in its message alone, with calls left in its headers, is a
// rate limit to the start's key proof too (c-boot-mint-rate-limit case 2, on the key proof). It
// names no time, so the start waits a minute, says so, and serves after it. Before, the start read
// it as GitHub's answer about the App and exited at the first attempt.
func TestABootWaitsOutARateLimitNamedOnlyByItsMessageOnTheAppKeyProof(t *testing.T) {
	quickAppMints(t)
	var refusals atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) || refusals.Add(1) > 1 {
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
	if wait := loggedDuration(t, oneRateLimitWait(t, logs.String(), "GitHub App key proof"), "wait"); wait <= time.Minute-time.Second || wait > time.Minute {
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

// A start whose tokens come from a token_command waits out GitHub's rate limit on the App read that
// checks a released token (appauth's tokenIdentity: the GraphQL viewer), which the logins read makes
// under the token the command releases for every installation, and serves after the reset
// (c-boot-mint-rate-limit case 4, on the logins read). The limit refuses the implement App's token
// so released. Before, the start exited once the three quick attempts were spent, before the reset.
func TestABootWaitsOutARateLimitOnATokenCommandsAppRead(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens := commandTokens(t, spentUntil(3*time.Second, &reset, func(r *http.Request) bool {
		return r.URL.Path == "/graphql" && r.Header.Get("Authorization") == "token ghs_implement_any_write"
	}))
	logs := &syncBuffer{}
	if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait out GitHub's rate limit on the released token's App read and serve after the reset", err)
	}
	resetAt := time.Unix(reset.Load(), 0)
	if served := time.Now(); served.Before(resetAt) {
		t.Fatalf("the daemon served at %s, before GitHub's reset at %s", served, resetAt)
	}
	if wait := oneRateLimitWait(t, logs.String(), "GitHub App logins read"); wait["until"] != resetAt.UTC().Format(time.RFC3339) {
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

// A Retry-After GitHub sends on the start's key proof is waited as GitHub asks, at most the maximum
// (c-boot-mint-rate-limit case 5, first half, on the key proof; the maximum shortened to 2 s): a 429
// asking for a second waits a second, one asking for 30 s waits the 2 s, and the start serves after
// each.
//
// The wait line's checks follow from the start's own decisions, whatever the machine's load: the
// start's count of its waits (waited) is the wait it decided; the wait left can be any amount up to
// that wait, nothing or less once the line comes late; and the until (whole seconds) is at most the
// wait past the line and less than a second short of the wait past the refused read, which the
// start answered after. One bound still follows the clock: the second read at most 3 s past the
// wait, the bound on a wait that runs past its until. An earlier version held the wait left to
// equal the wait decided; with a 1 s delay injected before the line, it failed.
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
				if !keyProofRead(r) {
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
			wait := oneRateLimitWait(t, logs.String(), "GitHub App key proof")
			if waited := loggedDuration(t, wait, "waited"); waited != tc.want {
				t.Errorf("the start's waits so far = %s, want %s: the one wait it decided", waited, tc.want)
			}
			if left := loggedDuration(t, wait, "wait"); left > tc.want {
				t.Errorf("the wait left = %s, want at most the %s wait decided", left, tc.want)
			}
			until, err := time.Parse(time.RFC3339, fmt.Sprint(wait["until"]))
			if err != nil {
				t.Fatalf("the wait line's until = %v: %v", wait["until"], err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reads) < 2 || reads[1].Sub(reads[0]) < tc.want || reads[1].Sub(reads[0]) > tc.want+3*time.Second {
				t.Fatalf("GitHub was read for the App's installations at %v, want the refused read and one more %s after it", reads, tc.want)
			}
			if line := loggedAt(t, wait); until.After(line.Add(tc.want)) || !until.After(reads[0].Add(tc.want-time.Second)) {
				t.Errorf("the wait line at %s says until %s, want at most %s past the line and less than a second short of %s past the refused read at %s",
					line, until, tc.want, tc.want, reads[0])
			}
		})
	}
}

// Refusals of one call of the start that name no time wait a minute, then two (c-boot-mint-rate-limit
// case 5, second half, at the production minute, on the key proof): GitHub answers every read of the
// App's installations with a 429 and no rate-limit header. The start's count of its waits (waited)
// is a minute at the first wait line and three at the second, so the second wait is two minutes; the
// second wait begins only once the first has ended, and ends two minutes after it begins, so its
// until is two minutes or more past the first's and at most two minutes past its own line. A stop
// during that wait ends the start at once.
//
// The waited, wait-left and until checks follow from the start's own decisions, whatever the
// machine's load: a line is written some time after its wait was decided, so its wait left can be
// any amount under the wait decided, and an until (whole seconds) any amount under two minutes past
// its line. Two bounds still follow the clock: the 90 s the test gives the two lines, and the 1 s
// the start may take to return after its stop. The 90 s is the only bound on how long the first wait
// runs past its until. An earlier version also held the second line to between 60 and 70 s after
// the first, and each wait left to within 1 s of the wait decided; with a 1 s delay injected before
// each line, both failed.
func TestABootWaitsOneMinuteThenTwoForRefusalsOfOneCallThatNameNoTime(t *testing.T) {
	quickAppMints(t)
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) {
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
	if first, second := loggedDuration(t, waits[0], "waited"), loggedDuration(t, waits[1], "waited"); first != time.Minute || second != 3*time.Minute {
		t.Errorf("the start's waits so far = %s and then %s, want 1m0s and then 3m0s: a wait of a minute, then one of two", first, second)
	}
	if first, second := loggedDuration(t, waits[0], "wait"), loggedDuration(t, waits[1], "wait"); first <= 0 || first > time.Minute || second <= 0 || second > 2*time.Minute {
		t.Errorf("the waits left = %s and %s, want each above nothing and at most its wait, 1m0s and then 2m0s", first, second)
	}
	untils := make([]time.Time, len(waits))
	for i, wait := range waits {
		until, err := time.Parse(time.RFC3339, fmt.Sprint(wait["until"]))
		if err != nil {
			t.Fatalf("wait line %d's until = %v: %v", i+1, wait["until"], err)
		}
		untils[i] = until
	}
	if first := loggedAt(t, waits[0]); untils[0].After(first.Add(time.Minute)) {
		t.Errorf("the first wait's until = %s, want at most a minute after its line, %s", untils[0], first)
	}
	if second := loggedAt(t, waits[1]); second.Before(untils[0]) || untils[1].Before(untils[0].Add(2*time.Minute)) || untils[1].After(second.Add(2*time.Minute)) {
		t.Errorf("the second wait's line at %s and until %s, want both past the first wait's until %s, and the until two minutes or more past it and at most two minutes past the line",
			second, untils[1], untils[0])
	}
}

// All rate-limit waits of one start end at the bound after the first of them (c-boot-mint-rate-limit
// case 6, on the key proof, the bound shortened to 3 s): GitHub refuses every read of the App's
// installations with a reset ten seconds ahead, the one wait ends at the bound, and the refusal
// after it ends the start, naming the call, GitHub's last answer with its headers and message, the
// 3 s waited and the App whose key the read proves.
func TestABootEndsWhenItsRateLimitWaitsReachTheirBound(t *testing.T) {
	quickAppMints(t)
	withStartRateLimits(t, func(policy *rateLimitPolicy) { policy.Bound = 3 * time.Second })
	tokens, _ := appTokensWith(t, discoveredOnAcme, func(w http.ResponseWriter, r *http.Request) bool {
		if !keyProofRead(r) {
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
		"the GitHub App key proof waited 3s for GitHub's rate limit, the most a start waits, and GitHub still refuses it: ",
		"GitHub answered GET /app/installations with 403",
		`X-RateLimit-Remaining "0"`,
		"API rate limit exceeded for installation ID 7.",
		"prove the implement App's key by reading its installations: ",
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
// cleanly (c-boot-mint-rate-limit case 9, on the key proof): the App's installations are refused
// until a reset a minute ahead, and run returns nil within a second of the stop.
func TestABootStoppedWhileItWaitsOutARateLimitEndsAtOnce(t *testing.T) {
	quickAppMints(t)
	var reset atomic.Int64
	tokens, _ := appTokensWith(t, discoveredOnAcme, spentUntil(time.Minute, &reset, keyProofRead))
	logs := &syncBuffer{}
	took := bootStoppedWhen(t, workflowConfig(t, workflowNATS(t)), tokens, logs, 30*time.Second, func(logs string) bool {
		return strings.Contains(logs, rateLimitWaitLine)
	})
	if took > time.Second {
		t.Errorf("run returned %s after its stop, want within 1s", took)
	}
	if stopped := loggedMessages(t, logs.String(), "legion daemon stopped before it proved its GitHub Apps' keys"); len(stopped) != 1 {
		t.Errorf("the stop's lines = %v, want one", stopped)
	}
}
