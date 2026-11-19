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
	"log/slog"
	"maps"
	"net/http"
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
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	standIn := &gitHubStandIn{discoveries: map[string]int{}}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
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
			// its request down to it (appauth's exchangeGranted), so the stand-in's installation
			// is granted every permission a role's set names.
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
// lease or a read lease, is refused before it reaches GitHub, each counted: everything else it
// answers as the manager does, the App logins and the App-level read of each App's installations
// among them.
type refusingMints struct {
	*appauth.Manager
	asked atomic.Int32
}

func (m *refusingMints) Token(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	m.asked.Add(1)
	return appauth.Lease{}, errors.New("this token source mints no lease")
}

func (m *refusingMints) ReadToken(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	m.asked.Add(1)
	return appauth.Lease{}, errors.New("this token source mints no lease")
}

// The boot proves each role App's key with GitHub's App-level read of its installations, GET
// /app/installations under the App's JWT, once for each App, and mints no token for any owner: a
// project works in each repository both role Apps reach, so no owner's installation is the boot's
// to prove. A refusal of that read stops the boot, naming the App; an App with no installation
// boots, and one WARN line names it, since an operator may install it once the daemon is up. The
// token source here refuses every mint, of a write lease or a read lease, and the boot comes up
// all the same, having asked it for none and GitHub for no installation token.
//
// The control is any Token call during the boot (INFRA-4401 P4; dispatch://INFRA-4401 check a-go,
// item 8): with the boot's mint per owner back (mintAtBoot), that mint is refused and the boot
// stops, and GitHub is asked for a token.
func TestTheBootProvesEachAppKeyWithItsInstallations(t *testing.T) {
	t.Run("each App's key is proved and nothing is minted", func(t *testing.T) {
		quickAppMints(t)
		manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		tokens := &refusingMints{Manager: manager}
		var logged syncBuffer
		if err := bootsOrExitsLogging(t, workflowConfig(t, workflowNATS(t)), tokens, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
			t.Fatalf("boot = %v, want the daemon up: the boot mints no token, so a source that refuses every mint stops nothing", err)
		}
		if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
			t.Errorf("installation discoveries by App = %v, want one each: the key proof of the implement App (1) and the review App (2)", got)
		}
		if asked, exchanged := tokens.asked.Load(), standIn.Exchanges(); asked != 0 || exchanged != 0 {
			t.Errorf("the boot asked the token source for %d leases and GitHub for %d installation tokens, want none of either", asked, exchanged)
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
