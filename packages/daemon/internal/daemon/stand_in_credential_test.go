package daemon

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// fromTheDaemon reports whether r carries bearer, the credential the daemon under test presents to
// a stand-in. Any other request is another process's: the loopback port the kernel gives a
// stand-in may be one a process of the machine still sends to. It is answered 404 and logged, with
// its method and path quoted and cut short, and fails nothing, so a stand-in's power to fail the
// test stays with the daemon's own requests.
func fromTheDaemon(t *testing.T, w http.ResponseWriter, r *http.Request, bearer string) bool {
	t.Helper()
	if r.Header.Get("Authorization") == "Bearer "+bearer {
		return true
	}
	t.Logf("a stand-in answered 404 to %.16q %.200q, which carries no credential of the daemon's", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusNotFound)
	return false
}

// dispatchBoard starts a Dispatch board, closed when the test ends, and answers its URL. The board
// serves handler to each request that carries the daemon's Dispatch bearer (workflowDispatchToken)
// and answers any other request 404 (fromTheDaemon). Every Dispatch board of daemon_test.go starts
// here.
func dispatchBoard(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, workflowDispatchToken) {
			handler(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// workflowBranchGitHub is newBranchGitHub for a daemon whose GitHub App leases a
// workflowTokenRecorder mints: it serves each request that carries their token (workflowAppToken)
// and answers any other request 404 (fromTheDaemon), so it records no read or create of another
// process's.
func workflowBranchGitHub(t *testing.T, answer func() (int, string)) *branchGitHub {
	t.Helper()
	g := &branchGitHub{answer: answer, defaultBranch: "main"}
	handle := g.handle(t, nil)
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if fromTheDaemon(t, w, r, workflowAppToken) {
			handle(w, r)
		}
	}})
	return g
}

// kubeTestToken is the bearer every Kubernetes client of a test presents to the API stand-in
// (issueSandboxAPI): the user token of the kubeconfig writeKubeconfig writes, and the one
// sandboxRESTConfig names.
const kubeTestToken = "kube-test-token"

// aLease reports whether r carries, as its bearer, a GitHub App token one of this package's App
// token stand-ins leases a daemon: workflowTokenRecorder's (workflowAppToken), outboxTokens'
// (outbox-token, which forgettingTokens' leases carry too), roleTokens' (<role>-app-token-for-
// <owner>, which identityTokens' and servedOwnerTokens' leases carry too), loginTokens' and
// transientOnceTokens' (token), and the installation token appTokens' GitHub mints a real token
// manager (installation-token), each with "-read" after it for a read lease. It is the
// githubtest.Spec.Credential of the GitHub stand-ins of this package whose daemon leases its
// tokens from one of them, but for workflowBranchGitHub, which serves workflowAppToken's alone
// (fromTheDaemon): a test that reads which lease a request carried still tells one from another,
// and a request with none is another process's.
func aLease(r *http.Request) bool {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !bearer {
		return false
	}
	token = strings.TrimSuffix(token, "-read")
	switch token {
	case workflowAppToken, "outbox-token", "token", "installation-token":
		return true
	}
	role, owner, minted := strings.Cut(token, "-app-token-for-")
	return minted && owner != "" && (role == string(appauth.Implement) || role == string(appauth.Review))
}

// anAppCredential is the githubtest.Spec.Credential of a GitHub stand-in a real App token manager
// whose App key is key mints against: an App JWT key signed, as each App route's request carries
// it, or an installation token in GitHub's own `token` scheme, as the manager's read of the App's
// bot user carries the token the mint before it was answered.
func anAppCredential(key *rsa.PublicKey) func(*http.Request) bool {
	return func(r *http.Request) bool {
		authorization := r.Header.Get("Authorization")
		if token, installation := strings.CutPrefix(authorization, "token "); installation {
			return token != ""
		}
		jwt, bearer := strings.CutPrefix(authorization, "Bearer ")
		dot := strings.LastIndexByte(jwt, '.')
		if !bearer || dot < 0 {
			return false
		}
		signature, err := base64.RawURLEncoding.DecodeString(jwt[dot+1:])
		digest := sha256.Sum256([]byte(jwt[:dot]))
		return err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
	}
}

// anInstallationToken is the githubtest.Spec.Credential of a GitHub stand-in that a lease of a
// real App token manager reads: an installation token as GitHub mints one (ghs_…), as its bearer
// or in GitHub's own `token` scheme, which token mode's reads of the App's identity carry.
func anInstallationToken(r *http.Request) bool {
	authorization := r.Header.Get("Authorization")
	return strings.HasPrefix(authorization, "Bearer ghs_") || strings.HasPrefix(authorization, "token ghs_")
}

// failures is a testing.TB that keeps each failure a stand-in reports with Errorf instead of
// failing the test it runs in, so a test reads whether a request failed it.
type failures struct {
	testing.TB
	mu     sync.Mutex
	failed []string
}

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, fmt.Sprintf(format, args...))
}

// reported is every failure kept so far.
func (f *failures) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failed)
}

// standInRequest sends method to url, with no body and with authorization as its Authorization
// ("" for none), and answers the status the stand-in gave.
func standInRequest(t *testing.T, method, url, authorization string) int {
	t.Helper()
	request, err := http.NewRequestWithContext(testwait.Context(t), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send %s %s: %v", method, url, err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// metadataIdentity is the request a helper of the machine sent to a reused loopback port, which
// failed a test of this package (dispatch://INFRA-3904): a read of its metadata server's identity
// token, carrying no credential of the daemon's.
const metadataIdentity = "/computeMetadata/v1/instance/service-accounts/default/identity"

// A stand-in of this package that fails its test on a request it does not expect answers one that
// carries no credential of the daemon's 404 and fails nothing, while one that carries the daemon's
// credential still fails the test. Each stand-in of the first half is sent, without a credential,
// the request it failed its test on before: any request it does not model, which here is the
// metadata read of dispatch://INFRA-3904, or a write to its one modelled route whose body does not
// decode. Each runs under this test itself, so a failure it reported would fail this test. The
// second half runs the GitHub stand-in of the default-branch reads under failures, which reads
// what it reported: nothing for the read without a lease, and the unexpected read with one.
func TestAStandInFailsItsTestOnlyOnTheDaemonsOwnRequests(t *testing.T) {
	sandboxAPI := newIssueSandboxAPI()
	broker := httptest.NewServer(&expiringBroker{t: t, clock: newWaveClock()})
	t.Cleanup(broker.Close)
	for _, row := range []struct{ name, method, url string }{
		{"the live proofs' Dispatch board", http.MethodGet, newLiveDispatch(t, "TEST").url + metadataIdentity},
		{"the boot's Dispatch of the repository limits", http.MethodGet, liveBootDispatch(t, "TEST", []string{"acme/widgets"}, nil) + metadataIdentity},
		{"the webhook App check's Dispatch", http.MethodGet, newWebhookDispatch(t, true, nil).url + metadataIdentity},
		{"the registrations' broker", http.MethodPost, broker.URL + "/v1/oauth/exchange/pod-sessions"},
		{"the rigs' Kubernetes API", http.MethodGet, sandboxAPI.serve(t) + metadataIdentity},
		{"the default-branch reads' GitHub", http.MethodGet, newRepositoryGitHub(t, "main").url + metadataIdentity},
		{"the issue branch's GitHub", http.MethodGet, newBranchGitHub(t, nil, branchCreated).url + metadataIdentity},
		{"the two owners' GitHub", http.MethodGet, newTwoOwnersGitHub(t).url + metadataIdentity},
		{"a transferred repository's GitHub", http.MethodPost, newRenameGitHub(t, func(*http.Request, string) string { return "{}" }).url + "/graphql"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if status := standInRequest(t, row.method, row.url, ""); status != http.StatusNotFound {
				t.Errorf("%s %s without the daemon's credential: status %d, want 404", row.method, row.url, status)
			}
		})
	}
	sandboxAPI.requireServed(t)

	recorded := &failures{TB: t}
	github := newRepositoryGitHub(recorded, "main")
	if status := standInRequest(t, http.MethodGet, github.url+metadataIdentity, ""); status != http.StatusNotFound {
		t.Errorf("GET %s without a lease: status %d, want 404", metadataIdentity, status)
	}
	if got := recorded.reported(); len(got) != 0 {
		t.Fatalf("a request without a lease made the stand-in report %q, want nothing", got)
	}
	// The implement App's lease as roleTokens mints it for acme/elsewhere's owner.
	lease, err := roleTokens{}.Token(t.Context(), appauth.Implement, "acme")
	if err != nil {
		t.Fatalf("mint the implement App's lease: %v", err)
	}
	standInRequest(t, http.MethodGet, github.url+"/repos/acme/elsewhere", "Bearer "+lease.Token)
	if got, want := recorded.reported(), []string{"unexpected GitHub request GET /repos/acme/elsewhere"}; !slices.Equal(got, want) {
		t.Fatalf("a request with the implement App's lease that the stand-in does not expect made it report %q, want %q", got, want)
	}
	if reads := github.reads.Load(); reads != 0 {
		t.Fatalf("the stand-in counted %d reads of acme/widgets, want none", reads)
	}
}

// standInChildEnv marks the child run of TestADaemonStandInStillFailsItsTestOnTheDaemonsOwnUnexpectedRequest,
// which TestMain sends past the database and the build (main_test.go).
const standInChildEnv = "LEGION_DAEMON_STAND_IN_CHILD"

// A stand-in gated by fromTheDaemon, which takes the test's own *testing.T, still fails its test
// on a request that carries the daemon's credential and that it does not expect. A child run of
// this test binary sends one to each — the live proofs' board, the repository limits' board and
// the webhook check's board under the Dispatch bearer, the registrations' broker under the
// daemon's own token, and the rigs' Kubernetes API under the kubeconfig's bearer — and the parent
// reads that the child failed, with each stand-in's own failure line.
func TestADaemonStandInStillFailsItsTestOnTheDaemonsOwnUnexpectedRequest(t *testing.T) {
	if os.Getenv(standInChildEnv) == "1" {
		dispatchBearer := "Bearer " + workflowDispatchToken
		standInRequest(t, http.MethodGet, newLiveDispatch(t, "TEST").url+"/computeMetadata/live", dispatchBearer)
		standInRequest(t, http.MethodGet, liveBootDispatch(t, "TEST", []string{"acme/widgets"}, nil)+"/computeMetadata/limits", dispatchBearer)
		standInRequest(t, http.MethodGet, newWebhookDispatch(t, true, nil).url+"/computeMetadata/webhook", dispatchBearer)
		broker := httptest.NewServer(&expiringBroker{t: t, clock: newWaveClock()})
		t.Cleanup(broker.Close)
		standInRequest(t, http.MethodPost, broker.URL+"/v1/oauth/exchange/pod-sessions", "Bearer "+registrationsToken)
		api := newIssueSandboxAPI()
		standInRequest(t, http.MethodGet, api.serve(t)+"/computeMetadata/kube", "Bearer "+kubeTestToken)
		api.requireServed(t)
		return
	}
	// The child's deadline is what is left of this test's (testwait.Context), so its own waits give
	// up the margin before this test's do; with no deadline here (-timeout 0) it has none either. A
	// child still running where this test's waits give up is killed with the context.
	waits := testwait.Context(t)
	timeout := "0"
	if deadline, bounded := waits.Deadline(); bounded {
		timeout = time.Until(deadline).String()
	}
	child := exec.CommandContext(waits, os.Args[0], "-test.run=^TestADaemonStandInStillFailsItsTestOnTheDaemonsOwnUnexpectedRequest$", "-test.count=1", "-test.timeout="+timeout)
	child.Env = append(os.Environ(), standInChildEnv+"=1")
	out, err := child.CombinedOutput()
	if exit, failed := err.(*exec.ExitError); !failed || exit.ExitCode() != 1 {
		t.Fatalf("the child run = %v, want exit status 1; output:\n%s", err, out)
	}
	for _, want := range []string{
		"Dispatch request = GET /computeMetadata/live",
		"Dispatch request = GET /computeMetadata/limits",
		"Dispatch request = GET /computeMetadata/webhook",
		"the registration's body does not decode",
		"the rig's Kubernetes API did not serve [GET /computeMetadata/kube]",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the child run's output does not name %q:\n%s", want, out)
		}
	}
}

// A stand-in that holds the daemon to its requests answers a stranger's request 404, fails
// nothing, and counts nothing of it, in each form the request takes: with no credential, with
// another process's bearer, with the daemon's token under another scheme, with more after the
// daemon's token, and with a prefix of it. A Dispatch board (dispatchBoard) does not reach its
// handler. The dequeue rig's GitHub takes no hold of a held read and counts no read of the pull
// request or of its reviews, the counts TestADequeuedPullRequestReachesItsMerger reads. Its Envoy
// listener counts no refusal on a merger's topic no session holds and delivers nothing on one a
// session holds. The moved issue's GitHub (workflowBranchGitHub) records no create of a branch,
// the record TestAnIssueMovedWhileBootListsIsStillAdmitted reads. The first row's request is the
// one a helper of the machine sent to a reused port: a read of its metadata server's identity
// token. The daemon's own credential, sent last, is served and counted, so each count can move.
func TestAStandInAnswersARequestWithoutTheDaemonsCredential404(t *testing.T) {
	var reached atomic.Int64
	board := dispatchBoard(t, func(http.ResponseWriter, *http.Request) { reached.Add(1) })
	github := newDequeueGitHub(t)
	issue := dequeueIssue{key: "ACME-1", number: 301, head: dequeueHead(301)}
	github.serve(issue)
	release := github.holdRead(issue.number)
	t.Cleanup(release)
	var cfg config.Config
	listener := newDequeueListener(t, &cfg)
	const unheld, held = "notifications.legion.ACME.role.merger-1", "notifications.legion.ACME.role.merger-2"
	listener.unheld(unheld)
	listener.holds(held)
	branches := workflowBranchGitHub(t, branchCreated)
	pull := github.url + "/repos/" + startedAgainRepo + "/pulls/301"

	type stand struct{ name, token, method, url, body string }
	dispatch := stand{"a Dispatch board", workflowDispatchToken, http.MethodGet, board + metadataIdentity, ""}
	heldRead := stand{"the dequeue rig's GitHub at a held read", workflowAppToken, http.MethodGet, pull, ""}
	reviews := stand{"the dequeue rig's GitHub at the reviews", workflowAppToken, http.MethodGet, pull + "/reviews", ""}
	refusal := stand{"the dequeue rig's Envoy listener on an unheld merger's topic", dequeueEnvoyToken, http.MethodPost,
		cfg.EnvoyURL + "/v1/messages/publish", `{"topic":"` + unheld + `","message":"m"}`}
	delivery := stand{"the dequeue rig's Envoy listener on a held merger's topic", dequeueEnvoyToken, http.MethodPost,
		cfg.EnvoyURL + "/v1/messages/publish", `{"topic":"` + held + `","message":"m"}`}
	branch := stand{"the moved issue's GitHub at a create of its branch", workflowAppToken, http.MethodPost,
		branches.url + "/repos/acme/widgets/git/refs", `{"ref":"refs/heads/legion/ACME-1","sha":"` + branchBaseCommit + `"}`}
	// Each request is bounded by the one bound every wait of the test has (testwait.Context).
	waits := testwait.Context(t)
	do := func(s stand, authorization string) (int, error) {
		request, err := http.NewRequestWithContext(waits, s.method, s.url, strings.NewReader(s.body))
		if err != nil {
			return 0, err
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, err
		}
		_ = response.Body.Close()
		return response.StatusCode, nil
	}
	send := func(t *testing.T, s stand, authorization string) int {
		t.Helper()
		status, err := do(s, authorization)
		if err != nil {
			t.Fatalf("%s %s with Authorization %q: %v", s.method, s.url, authorization, err)
		}
		return status
	}

	for _, s := range []stand{dispatch, heldRead, reviews, refusal, delivery, branch} {
		for _, stranger := range []struct{ name, authorization string }{
			{"with no credential", ""},
			{"with another process's bearer", "Bearer another-process-token-not-a-secret"},
			{"with the daemon's token under another scheme", "token " + s.token},
			{"with more after the daemon's token", "Bearer " + s.token + "-x"},
			{"with a prefix of the daemon's token", "Bearer " + s.token[:len(s.token)-1]},
		} {
			t.Run(s.name+" "+stranger.name, func(t *testing.T) {
				if status := send(t, s, stranger.authorization); status != http.StatusNotFound {
					t.Fatalf("%s %s with Authorization %q: status %d, want 404", s.method, s.url, stranger.authorization, status)
				}
			})
		}
	}
	type counted struct {
		handler, reads, reviewReads, heldReads int
		holdSet                                bool
		refusals, deliveries, creates          int
	}
	counts := func() counted {
		github.mu.Lock()
		p := github.pulls[issue.number]
		c := counted{handler: int(reached.Load()), reads: p.reads, reviewReads: p.reviewReads, heldReads: p.heldReads, holdSet: p.hold != nil}
		github.mu.Unlock()
		c.refusals, c.deliveries, c.creates = listener.refused(unheld), len(listener.accepted(held)), len(branches.created())
		return c
	}
	if got, want := counts(), (counted{holdSet: true}); got != want {
		t.Errorf("after the strangers' requests the stand-ins counted %+v, want %+v", got, want)
	}
	for _, s := range []stand{dispatch, reviews, refusal, delivery, branch} {
		send(t, s, "Bearer "+s.token)
	}
	// The daemon's own read of the held pull request waits for the hold, which the test releases
	// once GitHub holds the read.
	answered := make(chan error, 1)
	go func() {
		_, err := do(heldRead, "Bearer "+heldRead.token)
		answered <- err
	}()
	testwait.Eventually(t, "GitHub holding the daemon's own read of the held pull request", func() bool {
		return github.heldReads(issue.number) == 1
	})
	release()
	if err := testwait.Receive(t, "the answer to the daemon's own read of the held pull request", answered); err != nil {
		t.Fatalf("%s %s with the daemon's token: %v", heldRead.method, heldRead.url, err)
	}
	if got, want := counts(), (counted{handler: 1, reads: 1, reviewReads: 1, heldReads: 1, refusals: 1, deliveries: 1, creates: 1}); got != want {
		t.Errorf("after the daemon's own requests the stand-ins counted %+v, want %+v", got, want)
	}
}
