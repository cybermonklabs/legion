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
	"os"
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
			// as GitHub does, and with a token in GitHub's shape (githubtest.InstallationToken),
			// which shares no run with GitHub's messages.
			mintedToken(w, githubtest.InstallationToken(t.Name()), exchangeBody(t, r))
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

// quickAppMints are the start's App-call bounds shortened for a test: a 500 ms attempt and three
// attempts 10 ms apart, with the production rate-limit policy, which a test changes on the value it
// gets. The daemons a test boots on them (bootsOn, bootStoppedWhen) are given them alone, so the
// test runs beside every other test.
func quickAppMints() *appCallBounds {
	calls := productionAppCalls()
	calls.attempt, calls.retry = 500*time.Millisecond, bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 10 * time.Millisecond, Attempts: 3}
	return &calls
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
	return bootsOrExitsOn(t, cfg, tokens, repositoriesGitHub(t), within, log, nil)
}

// bootsOn is bootsOrExitsLogging with the daemon's start making its App calls on calls.
func bootsOn(t *testing.T, cfg config.Config, tokens appauth.Source, calls *appCallBounds, within time.Duration, log *slog.Logger) error {
	t.Helper()
	return bootsOrExitsOn(t, cfg, tokens, repositoriesGitHub(t), within, log, calls)
}

// bootsOrExitsOn is bootsOrExitsLogging with the daemon reading GitHub's REST API at githubAPI, and
// its start making its App calls on calls (nil for the production bounds).
func bootsOrExitsOn(t *testing.T, cfg config.Config, tokens appauth.Source, githubAPI string, within time.Duration, log *slog.Logger, calls *appCallBounds) error {
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
			appCalls:       calls,
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
	t.Parallel()
	calls := quickAppMints()
	held := calls.attempt + calls.attempt/2
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
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
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
	t.Parallel()
	calls := quickAppMints()
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
		http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
	})
	err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "prove the implement App's key by reading its installations: ") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want the refusal naming the implement App's key proof and GitHub's 401", err)
	}
	if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1}) {
		t.Errorf("installation discoveries by App = %v, want the implement App's one: a 401 is not asked again", got)
	}
}

// Both Apps' key proofs share one retry budget, so a boot waits at most the start's retry before it
// refuses, however the failures fall between the two reads. Here the implement App's discovery
// fails twice and then answers, and the review App's never does: the budget's three attempts are
// spent before the review App's installations are asked for more than once. With a budget per App
// the boot would wait both out, twice as long, which is what outlasted Stage 3's wait for /healthz.
func TestABootGivesBothAppsKeyProofsOneRetryBudget(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	tokens, standIn := appTokens(t, func(w http.ResponseWriter, r *http.Request, app string, n int) {
		if app == "1" && n > 2 {
			installed(w)
			return
		}
		http.Error(w, "{}", http.StatusBadGateway)
	})
	err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, quietLogger())
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
	t.Parallel()
	t.Run("each App's key is proved and nothing is minted", func(t *testing.T) {
		calls := quickAppMints()
		manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		var logged syncBuffer
		tokens := &refusingMints{Manager: manager, log: &logged}
		cfg := workflowConfigOn(t, workflowNATS(t), liveTreeBoard)
		seedLiveTreeOn(t, cfg, "acme/widgets")
		if err := bootsOn(t, cfg, tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
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
		calls := quickAppMints()
		manager, standIn := appTokens(t, func(w http.ResponseWriter, _ *http.Request, app string, _ int) {
			if app == "2" {
				http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
				return
			}
			installed(w)
		})
		err := bootsOn(t, workflowConfig(t, workflowNATS(t)), &refusingMints{Manager: manager}, calls, 60*time.Second, quietLogger())
		if err == nil || !strings.Contains(err.Error(), "prove the review App's key by reading its installations: ") || !strings.Contains(err.Error(), "(401)") {
			t.Fatalf("boot = %v, want the refusal naming the review App's key proof and GitHub's 401", err)
		}
		if got := standIn.Discoveries(); !maps.Equal(got, map[string]int{"1": 1, "2": 1}) {
			t.Errorf("installation discoveries by App = %v, want one each: a 401 is not asked again", got)
		}
	})

	t.Run("an App with no installation boots, with one WARN line naming it", func(t *testing.T) {
		calls := quickAppMints()
		manager, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, app string, _ int) {
			if app == "1" {
				fmt.Fprint(w, `[]`)
				return
			}
			installed(w)
		})
		var logged syncBuffer
		if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), &refusingMints{Manager: manager}, calls, 60*time.Second, slog.New(slog.NewJSONHandler(&logged, nil))); err != nil {
			t.Fatalf("boot = %v, want the daemon up: an App installed on no account boots", err)
		}
		if warned := warnLinesNaming(logged.String(), "a GitHub App is installed on no account"); len(warned) != 1 || !strings.Contains(warned[0], `"app":"implement"`) {
			t.Fatalf("the boot's WARN lines of an App installed on no account = %q, want one naming the implement App", warned)
		}
	})
}

// discoveredOnAcme answers every installation discovery with the App's one installation, on acme.
func discoveredOnAcme(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) }

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

// keyProofHolds is a token source whose InstalledOwners, the boot's key proof (proveAppKeys, its
// one caller before the daemon serves a launch), first writes the file begun, so a token_command
// can hold each release asked for once the key proof has begun and none the App logins read asked
// for, whatever the number of runs the logins read made: an attempt of the logins read that ends
// after it took a role's release, as one can on a loaded box past its attempt bound, runs the
// command again on its next attempt for every role, the roles it already read among them
// (appLoginsAtBoot).
type keyProofHolds struct {
	*appauth.Manager
	begun string
}

func (k *keyProofHolds) InstalledOwners(ctx context.Context, role appauth.AppRole) ([]string, error) {
	if err := os.WriteFile(k.begun, nil, 0o644); err != nil {
		return nil, err
	}
	return k.Manager.InstalledOwners(ctx, role)
}

// A token_command still running when an attempt of the boot's key proof ends holds the boot
// without ending it, however many attempt bounds it outlasts: a grant may wait on a person's
// approval. Each such attempt is pending and spends no attempt of the start's retry
// (appauth.ErrCommandPending, appAttempt), the next attempt joins the command's run still going
// (appauth.Manager.InstalledOwners, whose token-mode read is runTokenCommand's), and once the
// command prints its release the key proof passes and the boot serves. Here each App's
// token_command holds each release of every installation the key proof asks for (keyProofHolds)
// for twice the bounds of all the retry's attempts and writes the hold down, so each App's
// command holds one run: the key proof's attempts join it. It holds no release the logins read
// asked for; an attempt of the logins read may still outlast its bound on a loaded box, pending
// or transient, which is the boot doing its job, so no line of the logins read is counted.
// Control: with InstalledOwners answering its caller's bare ctx.Err() in token mode too, as it did
// before INFRA-4401's review 5996655281, each attempt the bound ends is transient, and the boot
// refuses once the three are spent, naming the key proof's context deadline exceeded.
func TestABootWaitsForATokenCommandThatOutlastsEveryAttemptOfTheKeyProof(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	hold := 2 * time.Duration(calls.retry.Attempts) * calls.attempt
	dir := t.TempDir()
	begun := filepath.Join(dir, "key-proof-begun")
	held := func(role appauth.AppRole) string { return filepath.Join(dir, string(role)+".held") }
	manager := commandTokensWith(t, func(http.ResponseWriter, *http.Request) bool { return false }, func(role appauth.AppRole) string {
		return fmt.Sprintf(`if [ -z "${%s}" ] && [ -e %q ]; then echo held >>%q; sleep %.3f; fi; `,
			appauth.TokenCommandOwner, begun, held(role), hold.Seconds())
	})
	logs := &syncBuffer{}
	started := time.Now()
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), &keyProofHolds{Manager: manager, begun: begun}, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil))); err != nil {
		t.Fatalf("boot = %v, want it to wait for the token_command past every attempt of the key proof and serve", err)
	}
	if took := time.Since(started); took < hold {
		t.Errorf("the daemon served %s after it started, want after the key proof's command held %s", took, hold)
	}
	for _, role := range appauth.Roles {
		runs, err := os.ReadFile(held(role))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the %s App's held runs: %v", role, err)
		}
		if n := strings.Count(string(runs), "held\n"); n != 1 {
			t.Errorf("the %s App's token_command held %d runs for the key proof, want one: each attempt after the first joins the run still going", role, n)
		}
	}
	byProbe := func(lines []map[string]any, probe string) []map[string]any {
		return slices.DeleteFunc(lines, func(line map[string]any) bool { return line["probe"] != probe })
	}
	proof := byProbe(loggedMessages(t, logs.String(), "boot probe's work is still running"), "GitHub App key proof")
	if len(proof) <= calls.retry.Attempts {
		t.Fatalf("the key proof was pending %d times, want more than the retry's %d attempts", len(proof), calls.retry.Attempts)
	}
	for _, line := range proof {
		if detail, _ := line["detail"].(string); !strings.Contains(detail, `.token_command for every installation is still running after context deadline exceeded (a grant may wait on a person's approval)`) {
			t.Errorf("a pending line's detail = %q, want runTokenCommand's answer that the token_command's release of every installation is still running", detail)
		}
	}
	if transient := byProbe(loggedMessages(t, logs.String(), "boot probe failed transiently"), "GitHub App key proof"); len(transient) != 0 {
		t.Errorf("the key proof failed transiently %d times, want never: each attempt the command outlasted is pending", len(transient))
	}
}

// The boot's logins read runs each App's token_command once across its attempts, since each run
// asks the secrets broker for a release, which may wait on a person's approval of a grant. Here
// GitHub answers the review App's first viewer read 502, after the logins read took both Apps'
// releases. The next attempt reads the review App alone (appLoginsAtBoot), under the release it
// kept (appauth's loginRelease), so each App's command runs twice in all for its release of every
// installation: once for the logins read and once for the key proof, which never takes a release
// the logins read was answered with. Before, the next attempt read both Apps again, each running
// its command a second time for the logins read.
func TestABootRunsEachAppsTokenCommandOnceForTheLoginsReadAcrossItsAttempts(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	dir := t.TempDir()
	runs := func(role appauth.AppRole) string { return filepath.Join(dir, string(role)+".runs") }
	var refused atomic.Bool
	tokens := commandTokensWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		review := strings.HasPrefix(r.Header.Get("Authorization"), "token ghs_"+string(appauth.Review)+"_")
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" && review && refused.CompareAndSwap(false, true) {
			http.Error(w, "{}", http.StatusBadGateway)
			return true
		}
		return false
	}, func(role appauth.AppRole) string {
		return fmt.Sprintf(`[ -n "${%s}" ] || echo run >>%q; `, appauth.TokenCommandOwner, runs(role))
	})
	if err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, quietLogger()); err != nil {
		t.Fatalf("boot = %v, want it to read the review App's login again after GitHub's 502 and serve", err)
	}
	if !refused.Load() {
		t.Fatal("GitHub answered no viewer read of the review App 502: the attempt this test is about never failed")
	}
	for _, role := range appauth.Roles {
		ran, err := os.ReadFile(runs(role))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the %s App's runs: %v", role, err)
		}
		if n := strings.Count(string(ran), "run\n"); n != 2 {
			t.Errorf("the %s App's token_command ran %d times for its release of every installation, want 2: one for the logins read across its attempts and one for the key proof", role, n)
		}
	}
}

// A token GitHub refuses on the boot's logins read (the review App's viewer read answered 401)
// stops the boot at once, naming the App and GitHub's answer: a refusal is the App's answer, not
// GitHub's trouble (appAttempt), so no attempt waits out the 401 to read the login again, and each
// App's token_command, whose run may wait on a person's approval of a grant, runs once. The key
// proof never starts. An attempt the test's 500 ms bound ends on a loaded box is transient and runs
// again under the release it kept (appauth's loginRelease), which is the boot doing its job, so
// the row reads the 401 in the waited-out attempts' lines, not the number of reads GitHub saw.
func TestABootStopsAtOnceWhenGitHubRefusesAnAppsTokenOnTheLoginsRead(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	dir := t.TempDir()
	runs := func(role appauth.AppRole) string { return filepath.Join(dir, string(role)+".runs") }
	tokens := commandTokensWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		review := strings.HasPrefix(r.Header.Get("Authorization"), "token ghs_"+string(appauth.Review)+"_")
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" && review {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return true
		}
		return false
	}, func(role appauth.AppRole) string {
		return fmt.Sprintf(`[ -n "${%s}" ] || echo run >>%q; `, appauth.TokenCommandOwner, runs(role))
	})
	logs := &syncBuffer{}
	err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
	if err == nil || !strings.Contains(err.Error(), "read the review App's login") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want it refused, naming the review App's login read and GitHub's 401", err)
	}
	for _, line := range loggedMessages(t, logs.String(), "boot probe failed transiently") {
		if detail, _ := line["detail"].(string); line["probe"] == "GitHub App logins read" && strings.Contains(detail, "(401)") {
			t.Errorf("the boot waited out the review App's 401 to read its login again: %q", detail)
		}
	}
	for _, role := range appauth.Roles {
		ran, err := os.ReadFile(runs(role))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the %s App's runs: %v", role, err)
		}
		if n := strings.Count(string(ran), "run\n"); n != 1 {
			t.Errorf("the %s App's token_command ran %d times, want once: the boot stops at the logins read", role, n)
		}
	}
}

// A token GitHub refuses under the release the logins read kept stops the boot at once too:
// GitHub answers the review App's first viewer read 502, so the logins read keeps that App's
// release (appauth's loginRelease) and its next attempt reads under it, and GitHub answers that
// read, and every later one, 401. The boot stops naming the 401, waits out no attempt that carries
// it, and runs each App's token_command once: the kept release served the second read. Found by
// Tester4958-2: the row above refuses a fresh release, and a refusal made transient only for a
// kept release, which production would wait out across its retry's attempts on the same
// refused token, passed it.
func TestABootStopsAtOnceWhenGitHubRefusesTheTokenTheLoginsReadKept(t *testing.T) {
	t.Parallel()
	calls := quickAppMints()
	dir := t.TempDir()
	runs := func(role appauth.AppRole) string { return filepath.Join(dir, string(role)+".runs") }
	var reads atomic.Int32
	tokens := commandTokensWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		review := strings.HasPrefix(r.Header.Get("Authorization"), "token ghs_"+string(appauth.Review)+"_")
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" && review {
			if reads.Add(1) == 1 {
				http.Error(w, "{}", http.StatusBadGateway)
				return true
			}
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return true
		}
		return false
	}, func(role appauth.AppRole) string {
		return fmt.Sprintf(`[ -n "${%s}" ] || echo run >>%q; `, appauth.TokenCommandOwner, runs(role))
	})
	logs := &syncBuffer{}
	err := bootsOn(t, workflowConfig(t, workflowNATS(t)), tokens, calls, 60*time.Second, slog.New(slog.NewJSONHandler(logs, nil)))
	if n := reads.Load(); n < 2 {
		t.Fatalf("GitHub was asked the review App's login %d times: the read under the kept release never came (boot = %v)", n, err)
	}
	if err == nil || !strings.Contains(err.Error(), "read the review App's login") || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("boot = %v, want it refused, naming the review App's login read and GitHub's 401", err)
	}
	for _, line := range loggedMessages(t, logs.String(), "boot probe failed transiently") {
		if detail, _ := line["detail"].(string); line["probe"] == "GitHub App logins read" && strings.Contains(detail, "(401)") {
			t.Errorf("the boot waited out the review App's 401 under the release it kept to read its login again: %q", detail)
		}
	}
	for _, role := range appauth.Roles {
		ran, err := os.ReadFile(runs(role))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read the %s App's runs: %v", role, err)
		}
		if n := strings.Count(string(ran), "run\n"); n != 1 {
			t.Errorf("the %s App's token_command ran %d times, want once: the read after the 502 uses the release it kept", role, n)
		}
	}
}
