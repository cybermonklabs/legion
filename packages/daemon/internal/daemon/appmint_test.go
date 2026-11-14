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
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// quickAppMints shortens a boot mint's attempt and its retry for the test, restoring them after.
func quickAppMints(t *testing.T) {
	attempt, retry := appMintAttempt, appMintRetry
	appMintAttempt, appMintRetry = 500*time.Millisecond, bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond, Attempts: 3}
	t.Cleanup(func() { appMintAttempt, appMintRetry = attempt, retry })
}

func installed(w http.ResponseWriter) {
	fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}}]`)
}

// bootsOrExits runs the daemon until it answers /healthz or run returns, and says which.
func bootsOrExits(t *testing.T, cfg config.Config, tokens appauth.Tokens, within time.Duration) error {
	t.Helper()
	return bootsOrExitsLogging(t, cfg, tokens, within, quietLogger())
}

// bootsOrExitsLogging is bootsOrExits with the daemon logging to log.
func bootsOrExitsLogging(t *testing.T, cfg config.Config, tokens appauth.Tokens, within time.Duration, log *slog.Logger) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, cfg, log, overrides{
			listen:         heldListen,
			runtime:        fakeRuntime(fake.NewRuntime(), &built{}).runtime,
			clock:          stillClock{},
			workflowTokens: tokens,
			githubAPI:      repositoriesGitHub(t),
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
func TestABootMintsEachAppOnceForEachOwnerOfTheMapping(t *testing.T) {
	quickAppMints(t)
	manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		fmt.Fprint(w, `[{"id":77,"account":{"login":"acme"}},{"id":88,"account":{"login":"globex"}}]`)
	})
	mints := &bootMints{Manager: manager}
	cfg := mappedWorkflowConfig(t, workflowNATS(t), "acme/widgets", "Acme/Gadgets", "globex/tools")
	if err := bootsOrExitsLogging(t, cfg, mints, 60*time.Second, mintGateLog(mints)); err != nil {
		t.Fatalf("boot = %v, want the daemon serving three repositories of two owner accounts", err)
	}
	want := []string{"implement acme", "review acme", "implement globex", "review globex"}
	if got := mints.Minted(); !slices.Equal(got, want) {
		t.Errorf("the mints the boot's App gate asked for = %v, want %v: one of each App on each owner account, and none for a second repository of an account", got, want)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
		t.Errorf("installation discoveries by App = %v, want one each: one discovery names every installation an App has", got)
	}
}
