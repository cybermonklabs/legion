package oauth_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/cipher/dispatchread"
	"github.com/sjawhar/envoy/internal/cipher/enroll"
	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauth"
	"github.com/sjawhar/envoy/internal/cipher/oauthstore"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/projectaccess"
	"github.com/sjawhar/envoy/internal/oidc"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The listed identities every exchange test is written against: pods of three accounts of one
// cluster and runs of two workflows of one repository, each with the project and person an entry
// gives it, pods of a fourth account a read entry lists, whose tokens only read, and runs of a
// third workflow a result entry lists, whose tokens write only their project's results.
const (
	exchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	account       = "system:serviceaccount:example-ns:example-worker"
	person        = "ada@example.com"
	project       = "ACME"
	repository    = "example-org/example-repo"
	workflow      = ".github/workflows/release.yaml"
	ref           = "refs/heads/main"
	// daemonAccount is an account whose entry fixes the session daemonSession, as the Legion
	// daemon's does; deployWorkflow is a workflow whose entry fixes deploySession.
	daemonAccount  = "system:serviceaccount:example-ns:example-daemon"
	daemonSession  = "legion-daemon:ACME"
	deployWorkflow = ".github/workflows/deploy.yaml"
	deploySession  = "ci-deploy"
	// otherAccount is listed for another project and another person: a caller who may exchange,
	// and who must not write under any of the session names above.
	otherAccount = "system:serviceaccount:other-ns:example-worker"
	otherProject = "OTHER"
	otherPerson  = "mallory@example.com"
	// readAccount is the account the read entry lists: its pods are minted a token that holds
	// the scope read and the project scope none, names readService and readSession, and names no
	// person and no project.
	readAccount = "system:serviceaccount:example-ns:example-reader"
	readService = "example-reader"
	readSession = "example-reader-session"
	// resultsWorkflow is the workflow the result entry lists on every branch one segment below
	// refs/heads/: its runs are minted a token that holds the scope results and the project scope
	// results, names resultsSession, the person and the project, and carries the run.
	resultsWorkflow = ".github/workflows/ci-results.yaml"
	resultsSession  = "ci-results:" + repository + "/" + resultsWorkflow
	resultsBranch   = "refs/heads/topic"
	commitSHA       = "0123456789abcdef0123456789abcdef01234567"
	// podAudience and workflowAudience are what each issuer's tokens must be minted for; a token
	// carrying anything else is not spendable at this broker.
	podAudience      = "https://broker.example"
	workflowAudience = "https://broker.example/ci"
	// signInDomain is the Google Workspace domain a person signing in on a machine must belong
	// to, BROKER_SIGNIN_DOMAIN's value in these tests, and deviceAudience the audience the
	// device-flow client's own ID tokens carry.
	signInDomain   = "example.com"
	deviceAudience = "https://broker.example/signin"
	// registrar is the login account of the deployment's daemon that writes every pod-session
	// registration these tests make. One daemon registers them all, so a name two of its pods are
	// registered for, of one account or two, is held by that one registrar.
	registrar = "system:serviceaccount:legion-system:legion-daemon"
)

// newPolicy builds the exchange's listed identities with no read entry and no result entry,
// failing the test on an entry it refuses. machines are the Google accounts a test that drives
// the machine subject token lists; every other test passes none.
func newPolicy(t *testing.T, accounts []exchange.AccountEntry, workflows []exchange.WorkflowEntry, machines ...exchange.GoogleEntry) *exchange.Set {
	t.Helper()
	return newPolicyListing(t, accounts, nil, workflows, nil, machines...)
}

// newPolicyListing is newPolicy with read entries listed beside the accounts and result entries
// beside the workflows, as the exchange fixture's own policy lists one of each.
func newPolicyListing(t *testing.T, accounts []exchange.AccountEntry, reads []exchange.ReadEntry, workflows []exchange.WorkflowEntry, results []exchange.ResultWorkflowEntry, machines ...exchange.GoogleEntry) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(accounts, reads, workflows, results, machines, signInDomain)
	if err != nil {
		t.Fatalf("exchange.NewSet: %v", err)
	}
	return set
}

// exchangeFixture is a fixture whose token exchange lists the accounts, the read entry, the
// workflows and the result entry above, with a local issuer standing in for the cluster's and
// another for GitHub's.
type exchangeFixture struct {
	*fixture
	cluster    *oidctest.Issuer
	clusterKey *oidctest.Key
	github     *oidctest.Issuer
	githubKey  *oidctest.Key
	// config is the token exchange's configuration as built, before the test's adjust.
	config oauth.ExchangeConfig
}

// newExchangeFixture starts both issuers and builds the provider against them, with adjust
// applied to the configuration last so a test can shorten a lifetime or drop an issuer.
func newExchangeFixture(t *testing.T, adjust func(*oauth.Config)) *exchangeFixture {
	t.Helper()
	cluster, github := oidctest.New(t), oidctest.New(t)
	clusterKey, githubKey := cluster.PublishKey(t, "cluster-key"), github.PublishKey(t, "github-key")
	clusterKeys, err := oidc.NewKeys(context.Background(), cluster.URL())
	if err != nil {
		t.Fatalf("cluster keys: %v", err)
	}
	githubKeys, err := oidc.NewKeys(context.Background(), github.URL())
	if err != nil {
		t.Fatalf("GitHub keys: %v", err)
	}
	config := oauth.ExchangeConfig{
		Policy: newPolicyListing(t,
			[]exchange.AccountEntry{
				{Account: account, Project: project, Person: person},
				{Account: daemonAccount, Project: project, Person: person, Session: daemonSession},
				{Account: otherAccount, Project: otherProject, Person: otherPerson},
			},
			[]exchange.ReadEntry{{Account: readAccount, Service: readService, Session: readSession}},
			[]exchange.WorkflowEntry{
				{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
				{Repository: repository, Workflow: deployWorkflow, Ref: ref, Project: project, Person: person, Session: deploySession},
			},
			[]exchange.ResultWorkflowEntry{{Repository: repository, Workflow: resultsWorkflow, RefPattern: "refs/heads/*", Project: project, Person: person}}),
		Kubernetes: oauth.IssuerConfig{Issuer: cluster.URL(), Audience: podAudience, Keys: clusterKeys},
		GitHub:     oauth.IssuerConfig{Issuer: github.URL(), Audience: workflowAudience, Keys: githubKeys},
	}
	inner := newFixtureWith(t, func(c *oauth.Config) {
		c.Exchange = config
		adjust(c)
	})
	return &exchangeFixture{fixture: inner, cluster: cluster, clusterKey: clusterKey, github: github, githubKey: githubKey, config: config}
}

// replica is a second broker process on f's database, trusting f's issuers, started with a
// configuration that lists policy's identities instead of f's: a replica of a rollout that
// changed the listed identities, beside one that has not yet been replaced.
func (f *exchangeFixture) replica(t *testing.T, policy *exchange.Set) *exchangeFixture {
	t.Helper()
	config := f.config
	config.Policy = policy
	inner := newFixtureOn(t, f.dsn, func(c *oauth.Config) { c.Exchange = config })
	return &exchangeFixture{fixture: inner, cluster: f.cluster, clusterKey: f.clusterKey, github: f.github, githubKey: f.githubKey, config: config}
}

// podToken is a projected service-account token of the account above bound to the pod uid, with
// change applied to its claims so a test can spoil exactly one of them.
func (f *exchangeFixture) podToken(t *testing.T, uid string, change func(map[string]any)) string {
	t.Helper()
	claims := f.cluster.Claims(account, podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	if change != nil {
		change(claims)
	}
	return f.cluster.Mint(t, f.clusterKey, claims)
}

// podTokenOf is a projected service-account token of another listed account, bound to the pod uid.
func (f *exchangeFixture) podTokenOf(t *testing.T, of, uid string) string {
	t.Helper()
	return f.podToken(t, uid, func(claims map[string]any) { claims["sub"] = of })
}

// registers records that the pod uid of the account of may exchange under session, as the
// deployment's daemon (registrar) does through POST /v1/oauth/exchange/pod-sessions before it
// starts that pod. A pod of an entry that fixes no session writes under nothing else.
func (f *exchangeFixture) registers(t *testing.T, of, uid, session string) {
	t.Helper()
	if _, err := f.provider.RegisterPodSession(context.Background(), registrar, of, uid, session, "", 0); err != nil {
		t.Fatalf("register %s's pod %s for %s: %v", of, uid, session, err)
	}
}

// workflowToken is a continuous-integration workflow run's GitHub token, with change applied to
// its claims. Each call mints a fresh jti, as a run does: the exchange accepts one once.
func (f *exchangeFixture) workflowToken(t *testing.T, run string, change func(map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":              f.github.URL(),
		"sub":              "repo:" + repository + ":ref:" + ref,
		"aud":              workflowAudience,
		"iat":              now.Add(-time.Minute).Unix(),
		"exp":              now.Add(15 * time.Minute).Unix(),
		"jti":              "run-" + run,
		"repository":       repository,
		"workflow_ref":     repository + "/" + workflow + "@" + ref,
		"job_workflow_ref": repository + "/" + workflow + "@" + ref,
		"ref":              ref,
		"run_id":           run,
	}
	if change != nil {
		change(claims)
	}
	return f.github.Mint(t, f.githubKey, claims)
}

// resultRunToken is the GitHub token of a run of resultsWorkflow on resultsBranch: a workflow
// token, with the run's commit, its event, its attempt, its runner and the check run of its job,
// which a result entry's token carries on as its run. Each call mints a fresh jti, as a run does.
func (f *exchangeFixture) resultRunToken(t *testing.T, run string) string {
	t.Helper()
	return f.workflowToken(t, run, func(claims map[string]any) {
		claims["sub"] = "repo:" + repository + ":ref:" + resultsBranch
		claims["workflow_ref"] = repository + "/" + resultsWorkflow + "@" + resultsBranch
		claims["job_workflow_ref"] = claims["workflow_ref"]
		claims["ref"] = resultsBranch
		claims["sha"] = commitSHA
		claims["event_name"] = "push"
		claims["run_attempt"] = "1"
		claims["runner_environment"] = "github-hosted"
		claims["check_run_id"] = "9" + run
	})
}

// resultRunForm is the exchange request of a run of resultsWorkflow, naming no session, since
// the result entry fixes it.
func (f *exchangeFixture) resultRunForm(t *testing.T, run string) url.Values {
	t.Helper()
	return exchangeForm(f.resultRunToken(t, run), oauthwire.SubjectTokenTypeGitHub, "")
}

// exchangeForm is a complete token-exchange request a test spoils one field at a time: the
// exchange client with no credential of its own, the subject token it presents, and the session
// name it asks to write under.
func exchangeForm(subjectToken, subjectTokenType, session string) url.Values {
	return url.Values{
		"grant_type":         {exchangeGrant},
		"client_id":          {oauthwire.ExchangeClientID},
		"subject_token":      {subjectToken},
		"subject_token_type": {subjectTokenType},
		"session":            {session},
	}
}

// exchange posts form to the token endpoint with a DPoP proof of key, the key the issued token
// is bound to.
func (f *exchangeFixture) exchange(t *testing.T, form url.Values, key *ecdsa.PrivateKey) (int, map[string]any) {
	t.Helper()
	status, _, body := f.post(t, "/token", form, key)
	return status, body
}

// exchanged is the access token an exchange was answered with, failing the test when it was
// refused: a test that means to be refused reads the answer itself.
func (f *exchangeFixture) exchanged(t *testing.T, form url.Values, key *ecdsa.PrivateKey) string {
	t.Helper()
	status, body := f.exchange(t, form, key)
	return accessTokenOf(t, status, body)
}

// accessTokenOf is the access token an exchange answered with, failing the test when it was
// refused.
func accessTokenOf(t *testing.T, status int, body map[string]any) string {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("exchange = %d %v, want 200", status, body)
	}
	token, _ := body["access_token"].(string)
	if token == "" {
		t.Fatalf("exchange answered %v, which carries no access token", body)
	}
	return token
}

// TestAPodExchangesItsTokenForOneThatIntrospects: a pod presents its projected service-account
// token and is answered an access token bound to the key its proof named, which introspects as
// the session it asked for, the person and project its account is listed with, and that key.
func TestAPodExchangesItsTokenForOneThatIntrospects(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	token := accessTokenOf(t, status, body)
	if body["issued_token_type"] != "urn:ietf:params:oauth:token-type:access_token" {
		t.Errorf("exchange answered issued_token_type %v, want the access token type", body["issued_token_type"])
	}
	if body["token_type"] != "DPoP" {
		t.Errorf("exchange answered token_type %v, want DPoP", body["token_type"])
	}
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	want := oauthwire.Introspection{
		Active: true, Session: "implementer-g1", Person: person, ProjectScope: oauthwire.ProjectScopeNamed,
		Projects: []string{project}, ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key),
		ExpiresAt: answer.ExpiresAt,
	}
	if !reflect.DeepEqual(answer, want) {
		t.Errorf("Introspect = %+v, want %+v", answer, want)
	}
	if answer.ExpiresAt <= time.Now().Unix() {
		t.Errorf("the token expires at %d, which is not in the future", answer.ExpiresAt)
	}
}

// introspected is what the provider answers for token, failing the test on an error.
func (f *exchangeFixture) introspected(t *testing.T, token string) oauthwire.Introspection {
	t.Helper()
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	return answer
}

// readPodForm is an exchange request of a pod of the read entry's account, naming no session and
// no scope, as the read entry's one client sends it.
func (f *exchangeFixture) readPodForm(t *testing.T) url.Values {
	t.Helper()
	return exchangeForm(f.podTokenOf(t, readAccount, "pod-uid-reader"), oauthwire.SubjectTokenTypeKubernetes, "")
}

// TestAReadEntrysPodExchangesForATokenThatOnlyReads: a pod of the account the read entry lists
// presents its projected token naming no scope and no session, and is answered a token that
// introspects as exactly the read entry's: the scope read, the project scope none, the entry's
// service and the session it fixes, no person and no project, bound to the key its proof named.
// The entry fixes its session, so nothing was registered for the pod and none is needed. The
// literal below is the answer every resource server reads a read-only token by, and a test of a
// build that predates the read entry copies it with its words spelled out.
func TestAReadEntrysPodExchangesForATokenThatOnlyReads(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	answer := f.introspected(t, f.exchanged(t, f.readPodForm(t), key))
	want := oauthwire.Introspection{Active: true, Scope: "read", ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key), Session: readSession, Service: readService, ProjectScope: "none", ExpiresAt: answer.ExpiresAt}
	if !reflect.DeepEqual(answer, want) {
		t.Errorf("Introspect = %+v, want %+v", answer, want)
	}
	if answer.ExpiresAt <= time.Now().Unix() {
		t.Errorf("the token expires at %d, which is not in the future", answer.ExpiresAt)
	}
	if rows := f.count(t, "oauth_pod_sessions"); rows != 0 {
		t.Errorf("oauth_pod_sessions holds %d rows, want none: a read entry fixes its session and registers nothing", rows)
	}
}

// TestTheEntryDecidesTheScopeAndARequestNamingMachineWidensNothing: the exchange client is
// registered for the scope machine, so a request may name it, and the library validates it
// without granting it. Under the read entry the token then holds read and nothing beside it;
// under a named entry the same request is granted no scope at all, as it was before read entries
// existed. The scope is the entry's to decide, never the request's.
func TestTheEntryDecidesTheScopeAndARequestNamingMachineWidensNothing(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	read := f.readPodForm(t)
	read.Set("scope", oauthwire.Scope)
	if answer := f.introspected(t, f.exchanged(t, read, key)); answer.Scope != oauthwire.ScopeRead || answer.ProjectScope != oauthwire.ProjectScopeNone {
		t.Errorf("a read entry's exchange naming the scope %s introspects as %+v, want the scope %q alone and the project scope %q",
			oauthwire.Scope, answer, oauthwire.ScopeRead, oauthwire.ProjectScopeNone)
	}
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	named := exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	named.Set("scope", oauthwire.Scope)
	if answer := f.introspected(t, f.exchanged(t, named, key)); answer.Scope != "" || answer.ProjectScope != oauthwire.ProjectScopeNamed || !slices.Equal(answer.Projects, []string{project}) {
		t.Errorf("a named entry's exchange naming the scope %s introspects as %+v, want no scope and the named project %s", oauthwire.Scope, answer, project)
	}
}

// TestARequestNamingTheReadScopeIsRefusedUnderEveryEntry: the exchange client is not registered
// for the scope read, so the library refuses a request that names it invalid_scope before the
// exchange decides anything, under a named entry and under the read entry alike, and stores no
// token. The same request naming no scope is then answered, so what refused it was the scope.
func TestARequestNamingTheReadScopeIsRefusedUnderEveryEntry(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	for _, c := range []struct {
		name string
		form func(*testing.T) url.Values
	}{
		{name: "a named entry", form: func(t *testing.T) url.Values {
			return exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
		}},
		{name: "the read entry", form: f.readPodForm},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.count(t, "oauth_access_tokens")
			asked := c.form(t)
			asked.Set("scope", oauthwire.ScopeRead)
			status, body := f.exchange(t, asked, key)
			if status != http.StatusBadRequest || body["error"] != "invalid_scope" || body["access_token"] != nil {
				t.Fatalf("an exchange under %s naming the scope %s = %d %v, want 400 invalid_scope and no access token", c.name, oauthwire.ScopeRead, status, body)
			}
			if after := f.count(t, "oauth_access_tokens"); after != before {
				t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
			}
			f.exchanged(t, c.form(t), key)
		})
	}
}

// TestAReadEntrysPodNamingAnotherSessionIsRefused: the read entry fixes the session its token
// names, so a pod of it that asks for any other is refused, and the refusal names the session the
// entry fixes; no token is stored.
func TestAReadEntrysPodNamingAnotherSessionIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	before := f.count(t, "oauth_access_tokens")
	form := f.readPodForm(t)
	form.Set("session", "implementer-g1")
	status, body := f.exchange(t, form, key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" || body["access_token"] != nil {
		t.Fatalf("a read entry's exchange naming another session = %d %v, want 400 invalid_request and no access token", status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, readSession) {
		t.Errorf("the refusal reads %q, which does not name the session %s the entry fixes", hint, readSession)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
}

// TestAReadTokenIsNoSubjectTokenForAnotherExchange: a read token presented back to the exchange
// as an access token, the subject type a dev box's machine login trades under, is refused, and
// nothing is minted from it: the token the exchange client was issued is not one it may trade
// again, and a read token names no machine credential either.
func TestAReadTokenIsNoSubjectTokenForAnotherExchange(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	read := f.exchanged(t, f.readPodForm(t), key)
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, exchangeForm(read, oauthwire.SubjectTokenTypeAccessToken, readSession), key)
	if status != http.StatusBadRequest || body["access_token"] != nil {
		t.Fatalf("an exchange of a read token = %d %v, want 400 and no access token", status, body)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
}

// TestAResultGrantHoldsItsRun: a run of the result entry's workflow that asks for the airlock
// placeholder is granted a row that holds the scope results and the project scope results, the
// session and the person the entry gives and its one project, and the run its GitHub token names:
// its repository, workflow, ref and commit, its event, its run and attempt, its runner and the
// check run of its job. A results token the broker mints for the grant carries these claims.
func TestAResultGrantHoldsItsRun(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	grant := f.grantOf(t, f.exchanged(t, askingForThePlaceholder(f.resultRunForm(t, "7")), newKey(t)))
	want := oauthwire.CIRun{
		Repository: repository, WorkflowRef: repository + "/" + resultsWorkflow + "@" + resultsBranch, Ref: resultsBranch,
		SHA: commitSHA, EventName: "push", RunID: "7", RunAttempt: "1", RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
	raw, err := json.Marshal(grant.Extra[oauthwire.ClaimCI])
	if err != nil {
		t.Fatal(err)
	}
	var run oauthwire.CIRun
	if err := json.Unmarshal(raw, &run); err != nil || run != want {
		t.Fatalf("the grant holds the run %s (%v), want %+v", raw, err, want)
	}
	if grant.Session != resultsSession || !reflect.DeepEqual(grant.Scopes, []string{oauthwire.ScopeResults}) ||
		grant.Extra[oauth.ClaimSession] != resultsSession || grant.Extra[oauth.ClaimPerson] != person ||
		grant.Extra[oauth.ClaimProjectScope] != oauthwire.ProjectScopeResults || grant.Extra[oauth.ClaimProjects] != project {
		t.Errorf("the grant = %+v, want session %s, the scope results, person %s, the project scope results and the project %s", grant, resultsSession, person, project)
	}
}

// TestAResultPlaceholderIsNoSubjectToken: the placeholder a result entry's run is granted,
// presented back to the exchange as an access token, is refused, and nothing is minted or granted
// from it: the placeholder is no token the broker knows.
func TestAResultPlaceholderIsNoSubjectToken(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	held := f.exchanged(t, askingForThePlaceholder(f.resultRunForm(t, "7")), key)
	tokens, grants := f.count(t, "oauth_access_tokens"), f.count(t, "result_grants")
	for _, form := range []url.Values{
		exchangeForm(held, oauthwire.SubjectTokenTypeAccessToken, "a-session-of-its-own"),
		askingForThePlaceholder(exchangeForm(held, oauthwire.SubjectTokenTypeAccessToken, "a-session-of-its-own")),
	} {
		status, body := f.exchange(t, form, key)
		if status != http.StatusBadRequest || body["access_token"] != nil {
			t.Fatalf("an exchange of the placeholder asking for %q = %d %v, want 400 and nothing answered", form.Get("requested_token_type"), status, body)
		}
	}
	if after := f.count(t, "oauth_access_tokens"); after != tokens {
		t.Errorf("access tokens after the refused exchanges = %d, want the %d there were", after, tokens)
	}
	if after := f.count(t, "result_grants"); after != grants {
		t.Errorf("result grants after the refused exchanges = %d, want the %d there were", after, grants)
	}
}

// TestARequestNamingTheResultsScopeIsRefusedUnderEveryEntry: the exchange client is not
// registered for the scope results either, so the library refuses a request that names it
// invalid_scope before the exchange decides anything, under a named entry, the read entry and the
// result entry asking for its placeholder alike, and stores no token. The same request naming no
// scope is then answered, so what refused it was the scope: the result entry grants results
// without being asked.
func TestARequestNamingTheResultsScopeIsRefusedUnderEveryEntry(t *testing.T) {
	f := newResultsFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	runs := 0
	for _, c := range []struct {
		name string
		form func(*testing.T) url.Values
	}{
		{name: "a named entry", form: func(t *testing.T) url.Values {
			return exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
		}},
		{name: "the read entry", form: f.readPodForm},
		{name: "the result entry", form: func(t *testing.T) url.Values {
			runs++
			return askingForThePlaceholder(f.resultRunForm(t, strconv.Itoa(runs)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.count(t, "oauth_access_tokens")
			asked := c.form(t)
			asked.Set("scope", oauthwire.ScopeResults)
			status, body := f.exchange(t, asked, key)
			if status != http.StatusBadRequest || body["error"] != "invalid_scope" || body["access_token"] != nil {
				t.Fatalf("an exchange under %s naming the scope %s = %d %v, want 400 invalid_scope and no access token", c.name, oauthwire.ScopeResults, status, body)
			}
			if after := f.count(t, "oauth_access_tokens"); after != before {
				t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
			}
			f.exchanged(t, c.form(t), key)
		})
	}
}

// TestEveryOtherEntryMintsTheTokenItDidAndNoScope: beside the read entry, a pod of an account
// whose entry fixes its session, a run of a workflow whose entry fixes its session, and a dev box
// session trading its machine login are each answered exactly the token they were before read
// entries existed, holding no OAuth scope. TestAPodExchangesItsTokenForOneThatIntrospects holds
// the named pod's answer the same way.
func TestEveryOtherEntryMintsTheTokenItDidAndNoScope(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	deploy := func(claims map[string]any) {
		claims["workflow_ref"] = repository + "/" + deployWorkflow + "@" + ref
		claims["job_workflow_ref"] = claims["workflow_ref"]
	}
	for _, c := range []struct {
		name  string
		token string
		want  oauthwire.Introspection
	}{
		{name: "a pod of a fixed-session account",
			token: f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, "pod-uid-daemon"), oauthwire.SubjectTokenTypeKubernetes, ""), key),
			want:  oauthwire.Introspection{Active: true, Session: daemonSession, Person: person, ProjectScope: oauthwire.ProjectScopeNamed, Projects: []string{project}, ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key)}},
		{name: "a run of a fixed-session workflow",
			token: f.exchanged(t, exchangeForm(f.workflowToken(t, "4242", deploy), oauthwire.SubjectTokenTypeGitHub, deploySession), key),
			want:  oauthwire.Introspection{Active: true, Session: deploySession, Person: person, ProjectScope: oauthwire.ProjectScopeNamed, Projects: []string{project}, ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key)}},
		{name: "a dev box session",
			token: f.exchanged(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key),
			want:  oauthwire.Introspection{Active: true, Session: "ada-devbox", Person: person, ProjectScope: oauthwire.ProjectScopePerson, ClientID: oauthwire.ExchangeClientID, Thumbprint: thumbprint(t, key)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := f.introspected(t, c.token)
			c.want.ExpiresAt = answer.ExpiresAt
			if !reflect.DeepEqual(answer, c.want) {
				t.Errorf("Introspect = %+v, want %+v", answer, c.want)
			}
		})
	}
}

// TestAPodExchangesTheSameTokenTwice: a pod's projected token is a standing credential its
// kubelet refreshes, so a role that renews exchanges the same one again. Neither the replay store
// nor the session name's claim may refuse the second exchange.
func TestAPodExchangesTheSameTokenTwice(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	subject := f.podToken(t, "pod-uid-1", nil)
	first := f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	second := f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if first == second {
		t.Fatal("the renewal answered the token the first exchange did")
	}
	for _, token := range []string{first, second} {
		answer, err := f.provider.Introspect(context.Background(), token)
		if err != nil || !answer.Active {
			t.Fatalf("Introspect of a renewed pod's token = %+v, %v, want it live", answer, err)
		}
	}
}

// TestAWorkflowTokenIsRefusedTheSecondTime: a workflow run mints a fresh token per request, so
// one presentation is all a token needs and the second is a replay.
func TestAWorkflowTokenIsRefusedTheSecondTime(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.workflowToken(t, "4242", nil)
	f.exchanged(t, exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("the second exchange of one workflow token = %d %v, want 400 invalid_request", status, body)
	}
	// A fresh run presents a fresh token, and is the same workflow, so it writes under the same
	// session name.
	fresh := f.workflowToken(t, "4243", nil)
	f.exchanged(t, exchangeForm(fresh, oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
}

// TestASubjectTokenMintedForAnotherAudienceIsRefused: the library checks a subject token's issuer
// and its expiry and never its audience, so this is the broker's own check and a token a pod was
// given for another service is not spendable here.
func TestASubjectTokenMintedForAnotherAudienceIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		claims["aud"] = []string{"https://another.example"}
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token for another audience = %d %v, want 400 invalid_request", status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, podAudience) {
		t.Errorf("the refusal reads %q, which does not name the audience this broker accepts", hint)
	}
}

// TestAnUnlistedAccountIsRefused: the pod's token verifies and the cluster is the configured one;
// what refuses it is that no entry lists the service account it was minted for.
func TestAnUnlistedAccountIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		claims["sub"] = "system:serviceaccount:example-ns:example-other"
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange by an unlisted account = %d %v, want 400 invalid_request", status, body)
	}
}

// TestAPodTokenBoundToNoPodIsRefused: a token carrying no pod uid proves only that the account
// exists, not that Kubernetes minted it for a pod of this cluster.
func TestAPodTokenBoundToNoPodIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	subject := f.podToken(t, "pod-uid-1", func(claims map[string]any) {
		delete(claims, "kubernetes.io")
	})
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token bound to no pod = %d %v, want 400 invalid_request", status, body)
	}
}

// TestASessionNameAnotherPrincipalHoldsIsRefused: a pod of another listed account is another
// principal, so it may not mint a token naming the session a pod of the first is writing under,
// and the refusal names it; under a name of its own it is answered.
func TestASessionNameAnotherPrincipalHoldsIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	// Both pods are registered for what they ask: the refusal below is the claim on the name, not
	// a missing registration, and a registration names no holder.
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	f.registers(t, otherAccount, "pod-uid-2", "implementer-g1")
	f.registers(t, otherAccount, "pod-uid-2", "implementer-g2")
	f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-2"), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	refusedTheName(t, status, body, "implementer-g1")
	f.exchanged(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-2"), oauthwire.SubjectTokenTypeKubernetes, "implementer-g2"), key)
}

// refusedTheName fails the test unless an exchange was refused invalid_grant for naming session,
// a name another principal holds, with a refusal that names it.
func refusedTheName(t *testing.T, status int, body map[string]any, session string) {
	t.Helper()
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("an exchange naming the session %s another principal holds = %d %v, want 400 invalid_grant", session, status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, "'"+session+"' belongs to another identity") {
		t.Errorf("the refusal reads %q, which does not name the session %s as another identity's", hint, session)
	}
	if body["access_token"] != nil {
		t.Errorf("the refusal carries an access token: %v", body)
	}
}

// TestAnExchangeAskingForAJSONWebTokenIsRefused: this provider signs no claim set of its own, so
// it registers no JSON Web Token type and the library refuses a caller asking for one.
func TestAnExchangeAskingForAJSONWebTokenIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	form := exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
	form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:jwt")
	status, body := f.exchange(t, form, key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange asking for a JSON Web Token = %d %v, want 400 invalid_request", status, body)
	}
}

// TestASubjectTokenSignedWithASharedSecretIsRefused: the library parses a subject token allowing
// every algorithm, 'none' among them, and leaves the choice to the type's key function. An
// issuer publishes public keys, so a token signed with a secret is refused before any key is
// resolved.
func TestASubjectTokenSignedWithASharedSecretIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	claims := f.cluster.Claims(account, podAudience)
	claims["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": "pod-uid-1"}}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: make([]byte, 32)},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	status, body := f.exchange(t, exchangeForm(subject, oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange of a token signed with a shared secret = %d %v, want 400 invalid_request", status, body)
	}
}

// devBoxToken is a dev box's machine access token: a machine login of key approved for person,
// polled for its token pair.
func (f *exchangeFixture) devBoxToken(t *testing.T, key *ecdsa.PrivateKey, person string) string {
	t.Helper()
	return f.devBoxTokenOf(t, key, person, f.newCredential(t))
}

// devBoxTokenOf is devBoxToken for a login whose approval minted the launcher credential named.
func (f *exchangeFixture) devBoxTokenOf(t *testing.T, key *ecdsa.PrivateKey, person, credential string) string {
	t.Helper()
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(context.Background(), userCode, true,
		map[string]any{"operator": person, "credential_id": credential}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	return accessTokenOf(t, status, body)
}

// TestADevBoxSessionExchangesItsMachineToken: the machine access token's subject type never
// reaches the library's subject hook, because the library reads the subject off the stored
// session, so the exchange's own step runs after the request validates. The person is the
// approver the login recorded and the token names no project.
func TestADevBoxSessionExchangesItsMachineToken(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	machineToken := f.devBoxToken(t, key, person)
	exchanged := f.exchanged(t, exchangeForm(machineToken, "urn:ietf:params:oauth:token-type:access_token", "ada-devbox"), key)
	answer, err := f.provider.Introspect(context.Background(), exchanged)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.Session != "ada-devbox" || answer.Person != person {
		t.Fatalf("Introspect = %+v, want a live token of ada-devbox acting for %s", answer, person)
	}
	if answer.ProjectScope != oauthwire.ProjectScopePerson || answer.Projects != nil {
		t.Errorf("Introspect = %+v, want a token acting as its person that names no project, since the trade asked for none", answer)
	}
	if answer.Thumbprint != thumbprint(t, key) {
		t.Errorf("Introspect answered thumbprint %q, want the machine's own key %q", answer.Thumbprint, thumbprint(t, key))
	}
}

// TestADevBoxTradeNamingAWildcardSessionIsRefused: a machine login's trade that names the session
// *, > or one holding either is refused 400 invalid_request naming the unusable session, and no
// token is minted: a token of such a session would make a relay socket subscribe a wildcard over
// every session's relay subject. Control: the same login's trade naming ada-devbox is minted.
func TestADevBoxTradeNamingAWildcardSessionIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	machineToken := f.devBoxToken(t, key, person)
	for _, session := range []string{"*", ">", "ada-*"} {
		status, body := f.exchange(t, exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, session), key)
		description, _ := body["error_description"].(string)
		if status != http.StatusBadRequest || body["error"] != "invalid_request" || !strings.Contains(description, exchange.ErrSessionInvalid.Error()) {
			t.Errorf("a trade naming the session %q = %d %v, want 400 invalid_request naming %q", session, status, body, exchange.ErrSessionInvalid)
		}
		if _, minted := body["access_token"]; minted {
			t.Errorf("a trade naming the session %q minted a token", session)
		}
	}
	f.exchanged(t, exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
}

// tokenAnswer is a token endpoint answer read off a goroutine, where a test may not fail.
type tokenAnswer struct {
	status int
	body   map[string]any
	err    error
}

// postToken posts form to the token endpoint at tokenURL with the DPoP proof already signed, for a
// goroutine to run: it fails nothing itself and answers what came back.
func postToken(tokenURL string, form url.Values, proof string) tokenAnswer {
	req, err := http.NewRequest(http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return tokenAnswer{err: err}
	}
	defer resp.Body.Close()
	answer := tokenAnswer{status: resp.StatusCode}
	answer.err = json.NewDecoder(resp.Body).Decode(&answer.body)
	return answer
}

// waitForBlocked returns once a backend running a statement matching like waits on a lock that
// blocker holds, failing after ten seconds. Matching on the blocker, not only the statement,
// keeps a test of another package sharing the database from answering for this one.
func waitForBlocked(t *testing.T, pool *pgxpool.Pool, blocker uint32, like string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where wait_event_type = 'Lock' and $1 = any(pg_blocking_pids(pid)) and query like $2`, int32(blocker), like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no statement matching %s waited on a lock backend %d holds", like, blocker)
}

// waitForWaiting returns once backend pid waits on a lock in a statement matching like, failing
// at once if the operation running there returns first, and after ten seconds.
func waitForWaiting(t *testing.T, pool *pgxpool.Pool, pid uint32, like string, returned <-chan error) {
	t.Helper()
	if !testwait.Holds(10*time.Second, 5*time.Millisecond, func() bool {
		select {
		case err := <-returned:
			t.Fatalf("the operation returned (%v) before it waited on a lock in a statement matching %s", err, like)
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where pid = $1 and wait_event_type = 'Lock' and query like $2`, int32(pid), like).Scan(&waiting); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		return waiting > 0
	}) {
		t.Fatalf("backend %d never waited on a lock in a statement matching %s", pid, like)
	}
}

// TestAMachineExchangeHoldsItsCredentialAgainstARevoke: an exchange of a machine access token
// holds its launcher credential's row from before its token is issued until it commits, so a
// revoke arriving in between waits for it and then ends the exchanged token with the rest. Without
// the lock the revoke would commit first, ending only the rows it could see, and the exchanged
// token stored after it would introspect live for its whole lifetime.
//
// The exchange is held between its lock and its commit by a share lock on oauth_access_tokens,
// which lets its plain read of the subject token through and stops its insert of the new one. The
// revoke is enroll.RevokeReusedCredential, the revoke that runs in a caller's transaction: it
// takes the credential's row first and ends its tokens as enroll.Service.RevokeCredential does.
func TestAMachineExchangeHoldsItsCredentialAgainstARevoke(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	key := newKey(t)
	credential := f.newCredential(t)
	machineToken := f.devBoxTokenOf(t, key, person, credential)

	holder, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `lock table oauth_access_tokens in share mode`); err != nil {
		t.Fatalf("hold oauth_access_tokens: %v", err)
	}
	form := exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox")
	proof := dpopProof(t, key, f.url+"/token")
	exchanged := make(chan tokenAnswer, 1)
	go func() { exchanged <- postToken(f.url+"/token", form, proof) }()
	waitForBlocked(t, f.pool, holder.Conn().PgConn().PID(), "insert into oauth_access_tokens%")

	pids, revoked := make(chan uint32, 1), make(chan error, 1)
	go func() {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			close(pids)
			revoked <- err
			return
		}
		defer tx.Rollback(ctx)
		pids <- tx.Conn().PgConn().PID()
		if err := enroll.RevokeReusedCredential(ctx, tx, credential); err != nil {
			revoked <- err
			return
		}
		revoked <- tx.Commit(ctx)
	}()
	pid, ok := <-pids
	if !ok {
		t.Fatalf("begin the revoke: %v", <-revoked)
	}
	waitForWaiting(t, f.pool, pid, "%from launcher_credentials%", revoked)

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release oauth_access_tokens: %v", err)
	}
	answer := <-exchanged
	if answer.err != nil {
		t.Fatalf("the exchange: %v", answer.err)
	}
	token := accessTokenOf(t, answer.status, answer.body)
	if err := <-revoked; err != nil {
		t.Fatalf("the revoke: %v", err)
	}

	if got, err := f.provider.Introspect(ctx, token); err != nil || got.Active {
		t.Fatalf("Introspect of the token exchanged as the machine was revoked = %+v, %v, want it not live", got, err)
	}
	var named, active int
	if err := f.pool.QueryRow(ctx, `select count(*), count(*) filter (where active) from oauth_access_tokens where credential_id = $1`, credential).Scan(&named, &active); err != nil {
		t.Fatal(err)
	}
	if named != 2 || active != 0 {
		t.Fatalf("access tokens naming the revoked credential = %d, %d of them active; want the machine's and the exchanged one, neither active", named, active)
	}
}

// TestAMachineExchangeOfAnEndedCredentialIsRefused: a machine access token whose row is still
// active, but whose launcher credential has lapsed, is refused invalid_grant by the exchange's
// credential check, and no token is stored for it.
func TestAMachineExchangeOfAnEndedCredentialIsRefused(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	credential := f.newCredential(t)
	machineToken := f.devBoxTokenOf(t, key, person, credential)
	if _, err := f.pool.Exec(context.Background(), `update launcher_credentials set expires_at = now() - interval '1 minute' where id = $1`, credential); err != nil {
		t.Fatal(err)
	}
	before := f.count(t, "oauth_access_tokens")
	status, body := f.exchange(t, exchangeForm(machineToken, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("an exchange of a lapsed machine login's token = %d %v, want 400 invalid_grant", status, body)
	}
	if after := f.count(t, "oauth_access_tokens"); after != before {
		t.Fatalf("access tokens after the refused exchange = %d, want the %d there were", after, before)
	}
}

// introspectsAs fails the test unless token is live and introspects under session, acting for
// person and naming project.
func (f *exchangeFixture) introspectsAs(t *testing.T, token, session, person, project string) {
	t.Helper()
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var projects []string
	if project != "" {
		projects = []string{project}
	}
	if !answer.Active || answer.Session != session || answer.Person != person || !slices.Equal(answer.Projects, projects) {
		t.Fatalf("Introspect = %+v, want a live token of %s acting for %s in %q", answer, session, person, project)
	}
}

// TestEveryPodOfAFixedSessionAccountWritesUnderItsName: the session name an entry fixes belongs
// to the account, not to a pod of it, so the replacement pod of a rolling update is answered while
// the pod it replaces still holds a live token, and the old pod's renewals go on being answered
// beside it, neither of them registered for anything.
func TestEveryPodOfAFixedSessionAccountWritesUnderItsName(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	for _, uid := range []string{"pod-uid-old", "pod-uid-new", "pod-uid-old", "pod-uid-new"} {
		token := f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, uid), oauthwire.SubjectTokenTypeKubernetes, ""), key)
		f.introspectsAs(t, token, daemonSession, person, project)
	}
}

// TestEveryRunOfAFixedSessionWorkflowWritesUnderItsName: the session name an entry fixes belongs
// to the workflow, not to a run of it, so runs that start within one access lifetime of each
// other, as two merges in a row do, are each answered under it.
func TestEveryRunOfAFixedSessionWorkflowWritesUnderItsName(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	deploy := func(claims map[string]any) {
		claims["workflow_ref"] = repository + "/" + deployWorkflow + "@" + ref
		claims["job_workflow_ref"] = claims["workflow_ref"]
	}
	for _, run := range []string{"4242", "4243", "4244"} {
		token := f.exchanged(t, exchangeForm(f.workflowToken(t, run, deploy), oauthwire.SubjectTokenTypeGitHub, deploySession), key)
		f.introspectsAs(t, token, deploySession, person, project)
	}
}

// TestAFixedSessionEntryNeedsNoRegistration: an entry that fixes a session writes under the name
// its configuration names, so nothing about such a caller is registered and nothing of it is
// gated. The replacement pod of a fixed-session account and the next run of a fixed-session
// workflow are each answered with the registration table empty, which is also what a deployment
// whose daemon registers nothing for them looks like.
func TestAFixedSessionEntryNeedsNoRegistration(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	for _, uid := range []string{"pod-uid-old", "pod-uid-replacement"} {
		token := f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, uid), oauthwire.SubjectTokenTypeKubernetes, ""), key)
		f.introspectsAs(t, token, daemonSession, person, project)
	}
	deploy := func(claims map[string]any) {
		claims["workflow_ref"] = repository + "/" + deployWorkflow + "@" + ref
		claims["job_workflow_ref"] = claims["workflow_ref"]
	}
	for _, run := range []string{"4242", "4243"} {
		token := f.exchanged(t, exchangeForm(f.workflowToken(t, run, deploy), oauthwire.SubjectTokenTypeGitHub, deploySession), key)
		f.introspectsAs(t, token, deploySession, person, project)
	}
	if rows := f.count(t, "oauth_pod_sessions"); rows != 0 {
		t.Fatalf("oauth_pod_sessions holds %d rows, want none: a caller whose entry fixes its session name is registered for nothing", rows)
	}
}

// TestAnUnregisteredPodOfTheSameAccountIsRefusedTheName: the residual the registration closes.
// Two pods of one account are one principal, so the claim on a session name cannot tell them
// apart and refuses neither; what does is the registration their deployment makes per pod uid. A
// pod registered for the name writes under it, the pod that replaces it writes under it once it
// is registered too, and a third pod of that very account, registered for a name of its own and
// for nothing else, is refused the first name — invalid_grant, since the subject token is sound
// and it is broker state that refuses — with a refusal that names the session.
func TestAnUnregisteredPodOfTheSameAccountIsRefusedTheName(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	const role = "implementer-g1"
	for _, uid := range []string{"pod-uid-1", "pod-uid-replacement"} {
		f.registers(t, account, uid, role)
		f.introspectsAs(t, f.exchanged(t, exchangeForm(f.podToken(t, uid, nil), oauthwire.SubjectTokenTypeKubernetes, role), key), role, person, project)
	}
	f.registers(t, account, "pod-uid-third", "implementer-g2")
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-third", nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("the exchange of a third pod of %s naming %s = %d %v, want 400 invalid_grant and no access token: a pod writes under a session name its deployment registered for it", account, role, status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, "'"+role+"'") || !strings.Contains(hint, "registration") {
		t.Errorf("the refusal reads %q, which does not name the session %s and say that a pod writes under a name its deployment registered for it", hint, role)
	}
	f.introspectsAs(t, f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-third", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g2"), key), "implementer-g2", person, project)
}

// TestAnUnregisteredPodSessionIsRetryable: a pod's trade can reach the broker before its
// registration does — its daemon registers at the claim, a session's watch at the pod's creation,
// and either registrar can be down for minutes. That refusal is still invalid_grant, and its
// error_description begins pod_session_not_registered:, the words both clients read as the one
// refusal of the broker's state that a later trade can cure, so they trade again rather than stop
// for good. It names the session, and the same pod's next trade under that name, once its
// deployment has registered it, is answered.
func TestAnUnregisteredPodSessionIsRetryable(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	const role, uid = "implementer-g1", "pod-uid-early"
	status, body := f.exchange(t, exchangeForm(f.podToken(t, uid, nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	description, _ := body["error_description"].(string)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" || body["access_token"] != nil {
		t.Fatalf("the trade of pod %s under %s before its registration = %d %v, want 400 invalid_grant and no access token", uid, role, status, body)
	}
	if !strings.HasPrefix(description, "pod_session_not_registered: ") || !strings.Contains(description, "'"+role+"'") {
		t.Fatalf("the refusal reads %q, want it to begin %q, the words a client trades again on, and to name the session %s", description, "pod_session_not_registered: ", role)
	}
	f.registers(t, account, uid, role)
	f.introspectsAs(t, f.exchanged(t, exchangeForm(f.podToken(t, uid, nil), oauthwire.SubjectTokenTypeKubernetes, role), key), role, person, project)
}

// TestAnotherPrincipalIsRefusedANameAfterItsHoldersTokenLapses: the review's reproduction. With
// an access lifetime of one second, a pod writes under a Legion role's session name and its token
// lapses; a pod of another account, listed for another project and another person, then names the
// same session and is refused, because the lapse of a token hands no name over. The first pod's
// next renewal is answered under its own person and project.
func TestAnotherPrincipalIsRefusedANameAfterItsHoldersTokenLapses(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	key := newKey(t)
	const role = "019a-role-session"
	f.registers(t, account, "pod-uid-1", role)
	f.registers(t, otherAccount, "pod-uid-other", role)
	first := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	time.Sleep(time.Second + 500*time.Millisecond)
	if answer, err := f.provider.Introspect(context.Background(), first); err != nil || answer.Active {
		t.Fatalf("Introspect of the holder's token past its lifetime = %+v, %v, want it lapsed", answer, err)
	}
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, role), key)
	refusedTheName(t, status, body, role)
	renewed := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
	f.introspectsAs(t, renewed, role, person, project)
}

// TestANameAnEntryFixesIsRefusedToEveryOtherCaller: the names entries fix are held for their
// entries from the broker's first exchange, so a dev box — even one of the very person the
// daemon acts for — and a pod of another account are refused them before the entry's own caller
// has ever exchanged, and still once it has.
func TestANameAnEntryFixesIsRefusedToEveryOtherCaller(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	for _, session := range []string{daemonSession, deploySession} {
		status, body := f.exchange(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, session), key)
		refusedTheName(t, status, body, session)
	}
	// Registered for the name, so what refuses the pod of the other account is the entry that
	// fixes the name for the daemon's, which no registration of another pod touches.
	f.registers(t, otherAccount, "pod-uid-other", daemonSession)
	status, body := f.exchange(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)
	token := f.exchanged(t, exchangeForm(f.podTokenOf(t, daemonAccount, "pod-uid-daemon"), oauthwire.SubjectTokenTypeKubernetes, ""), key)
	f.introspectsAs(t, token, daemonSession, person, project)
	status, body = f.exchange(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)
}

// TestARePointedNameIsItsNewEntrysWhicheverReplicaReservesLast: the tester's reproduction, two
// broker processes on one database. A rollout re-points the daemon's fixed name from its account
// to another account. A replica on the new configuration answers that account's first exchange
// under the name, then a replica still on the old configuration, which had not exchanged yet,
// reserves its names at an unrelated exchange. The account the new configuration fixes the name
// for is still answered under it, and the daemon's account, which it no longer fixes the name for,
// is refused it there. Until the rollout ends the old replica's configuration decides its own
// claims as well: the daemon's account asking it is given the name, and the new entry's next
// exchange on the new replica takes it back.
func TestARePointedNameIsItsNewEntrysWhicheverReplicaReservesLast(t *testing.T) {
	old := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	daemonForm := func(f *exchangeFixture, session string) url.Values {
		return exchangeForm(f.podTokenOf(t, daemonAccount, "pod-uid-daemon"), oauthwire.SubjectTokenTypeKubernetes, session)
	}
	otherToken := func(f *exchangeFixture) string {
		return f.exchanged(t, exchangeForm(f.podTokenOf(t, otherAccount, "pod-uid-other"), oauthwire.SubjectTokenTypeKubernetes, ""), key)
	}
	old.introspectsAs(t, old.exchanged(t, daemonForm(old, ""), key), daemonSession, person, project)

	next := old.replica(t, newPolicy(t,
		[]exchange.AccountEntry{
			{Account: account, Project: project, Person: person},
			{Account: daemonAccount, Project: project, Person: person},
			{Account: otherAccount, Project: otherProject, Person: otherPerson, Session: daemonSession},
		},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: deployWorkflow, Ref: ref, Project: project, Person: person, Session: deploySession},
		}))
	// On the new configuration the daemon's own entry fixes no name, so its pod is registered for
	// the one it asks for below: what refuses it there is the entry that now fixes that name for
	// another account, and not a missing registration.
	next.registers(t, daemonAccount, "pod-uid-daemon", daemonSession)
	next.introspectsAs(t, otherToken(next), daemonSession, otherPerson, otherProject)

	late := old.replica(t, old.config.Policy)
	late.registers(t, account, "pod-uid-1", "implementer-g1")
	late.exchanged(t, exchangeForm(late.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	var holder string
	if err := old.pool.QueryRow(context.Background(), `select principal from oauth_session_bindings where session = $1`, daemonSession).Scan(&holder); err != nil {
		t.Fatalf("read the claim on %s: %v", daemonSession, err)
	}
	if want := "account:" + otherAccount; holder != want {
		t.Fatalf("after the old configuration reserved last, %s is held by %q, want %q: reserving takes no name", daemonSession, holder, want)
	}

	next.introspectsAs(t, otherToken(next), daemonSession, otherPerson, otherProject)
	status, body := next.exchange(t, daemonForm(next, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)

	late.introspectsAs(t, late.exchanged(t, daemonForm(late, ""), key), daemonSession, person, project)
	next.introspectsAs(t, otherToken(next), daemonSession, otherPerson, otherProject)
	status, body = next.exchange(t, daemonForm(next, daemonSession), key)
	refusedTheName(t, status, body, daemonSession)
}

// TestAPrincipalReclaimsItsOwnNameAsOftenAsItLikes: one account's pods, a pod relaunched under a
// new uid among them, renew one Legion session's name as often as they like, and so do one
// person's two machines; each claim runs SessionRetention past the expiry of the token it was
// made for, which is the issued token's own expiry rather than a moment computed before it.
func TestAPrincipalReclaimsItsOwnNameAsOftenAsItLikes(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	const role = "019a-role-session"
	var token string
	for _, uid := range []string{"pod-uid-1", "pod-uid-1", "pod-uid-relaunched", "pod-uid-1", "pod-uid-relaunched"} {
		// Each pod of the account is registered for the name as its deployment starts it; a
		// relaunch is a second uid registered for the same name, which is why the name survives it.
		f.registers(t, account, uid, role)
		token = f.exchanged(t, exchangeForm(f.podToken(t, uid, nil), oauthwire.SubjectTokenTypeKubernetes, role), key)
		f.introspectsAs(t, token, role, person, project)
	}
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	var claimed time.Time
	if err := f.pool.QueryRow(context.Background(), `select expires_at from oauth_session_bindings where session = $1`, role).Scan(&claimed); err != nil {
		t.Fatalf("read the claim: %v", err)
	}
	// The answer's exp is whole seconds, so the claim lies within a second of it either way.
	if issued := time.Unix(answer.ExpiresAt, 0).Add(oauthstore.SessionRetention); claimed.Before(issued.Add(-time.Second)) || claimed.After(issued.Add(time.Second)) {
		t.Errorf("the claim runs out at %s, want SessionRetention past the issued token's expiry, %s", claimed, issued)
	}
	for _, machineKey := range []*ecdsa.PrivateKey{newKey(t), newKey(t)} {
		machine := f.devBoxToken(t, machineKey, person)
		for range 2 {
			devBox := f.exchanged(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), machineKey)
			f.introspectsAs(t, devBox, "ada-devbox", person, "")
		}
	}
}

// TestIntrospectingATokenTheBrokerCannotStandBehind: a token it never minted, one it has revoked
// and one whose life has passed are each answered as simply not live, with no error and nothing
// else disclosed. The expired case is why the access lifetime is a setting: a second proves it.
func TestIntrospectingATokenTheBrokerCannotStandBehind(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	ctx := context.Background()
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	revoked := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set active = false`); err != nil {
		t.Fatalf("revoke the token: %v", err)
	}
	expired := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	live, err := f.provider.Introspect(ctx, expired)
	if err != nil || !live.Active {
		t.Fatalf("Introspect of a fresh token = %+v, %v, want it live", live, err)
	}
	time.Sleep(time.Second + 100*time.Millisecond)
	for name, token := range map[string]string{
		"one the broker never minted": "a-token-nobody-issued",
		"a revoked one":               revoked,
		"an expired one":              expired,
	} {
		t.Run(name, func(t *testing.T) {
			answer, err := f.provider.Introspect(ctx, token)
			if err != nil {
				t.Fatalf("Introspect = %v, want no error", err)
			}
			if !reflect.DeepEqual(answer, oauthwire.Introspection{}) {
				t.Fatalf("Introspect = %+v, want not live and nothing else", answer)
			}
		})
	}
}

// TestIntrospectingALiveTokenWithACharacterAddedIsNotLive: a string made of a live token and one
// character in front of it is not a token the broker minted, so it is answered as simply not
// live, with no error, as a token nobody issued is. The signature half of such a string is the
// live token's own, so the store finds the token's row, and what fails is the library's check of
// the key half, which is no longer base64url. That is the library finding the token wanting and
// never a storage outage: answered as a broker failure, the introspection route says 503
// DATABASE_UNAVAILABLE and logs an error for a request that is only malformed, and Dispatch, which
// hands the broker everything after the first space of `Authorization: DPoP  <token>`, tells the
// caller of such a header that the broker cannot be reached and to try again.
func TestIntrospectingALiveTokenWithACharacterAddedIsNotLive(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	token := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
	// The control: the token as it was minted is live, so each refusal below is the added
	// character's.
	if live := f.introspected(t, token); !live.Active {
		t.Fatalf("Introspect of the token as minted = %+v, want it live", live)
	}
	for name, added := range map[string]string{
		"a space":                       " ",
		"a character outside base64url": "!",
	} {
		t.Run(name, func(t *testing.T) {
			answer, err := f.provider.Introspect(ctx, added+token)
			if err != nil {
				t.Fatalf("Introspect = %v, want no error: a malformed token is not live, and no part of the broker failed", err)
			}
			if !reflect.DeepEqual(answer, oauthwire.Introspection{}) {
				t.Fatalf("Introspect = %+v, want not live and nothing else", answer)
			}
		})
	}
}

// TestIntrospectionAnswersOnlyAnAccessToken: the introspection route is how Dispatch reads one
// of the broker's access tokens back, and its answer carries no token type, so a resource server
// cannot tell a refresh token from an access token by reading it. A machine login's refresh token
// outlives the access token issued beside it; answered live, it would pass for a credential after
// that access token has expired. It is answered as simply not live — both while the access token
// issued beside it is live and once that access token has expired.
func TestIntrospectionAnswersOnlyAnAccessToken(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.AccessLifetime = time.Second })
	ctx := context.Background()
	key := newKey(t)
	deviceCode, userCode := f.authorize(t, key)
	if err := f.provider.ApproveDevice(ctx, userCode, true,
		map[string]any{"operator": person, "credential_id": f.newCredential(t)}); err != nil {
		t.Fatalf("ApproveDevice: %v", err)
	}
	status, body := f.poll(t, deviceCode, key)
	accessTokenOf(t, status, body)
	refreshToken, _ := body["refresh_token"].(string)
	if refreshToken == "" {
		t.Fatalf("the machine login's poll answered %v, which carries no refresh token", body)
	}
	check := func(t *testing.T) {
		t.Helper()
		answer, err := f.provider.Introspect(ctx, refreshToken)
		if err != nil {
			t.Fatalf("Introspect = %v, want no error", err)
		}
		if !reflect.DeepEqual(answer, oauthwire.Introspection{}) {
			t.Fatalf("Introspect of a refresh token = %+v, want not live and nothing else", answer)
		}
	}
	t.Run("while the access token issued beside it is live", check)
	time.Sleep(time.Second + 100*time.Millisecond)
	t.Run("once that access token has expired", check)
}

// TestAnIntrospectionFailureNamesItsCause: when Introspect cannot stand behind an answer it
// returns an error, which the route logs as the one line an operator reads beside its 503. That
// error must say what failed. The library wraps a failed read of a token's row in its own
// unauthorized error, whose text is only "request_unauthorized", so returned as it is, an outage
// and a row the broker cannot decode both reach the log as an authorization failure naming
// neither: the cause is kept beneath it and never printed.
func TestAnIntrospectionFailureNamesItsCause(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	token := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)

	t.Run("a stored row naming a client the provider does not register", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set request = jsonb_set(request, '{client_id}', '"unregistered-client"')`); err != nil {
			t.Fatalf("rewrite the token's row: %v", err)
		}
		_, err := f.provider.Introspect(ctx, token)
		if err == nil {
			t.Fatal("Introspect of a row naming an unregistered client answered no error")
		}
		if !strings.Contains(err.Error(), "unregistered-client") {
			t.Fatalf("Introspect's error reads %q: it must name the row's unregistered client, which is what failed", err.Error())
		}
	})

	t.Run("a stored projects claim that is not its string", func(t *testing.T) {
		if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set request = jsonb_set(jsonb_set(request, '{client_id}', '"dispatch-token-exchange"'), '{session,extra,projects}', '["ACME"]')`); err != nil {
			t.Fatalf("rewrite the token's row: %v", err)
		}
		_, err := f.provider.Introspect(ctx, token)
		if !errors.Is(err, oauth.ErrStoredClaimMalformed) || !strings.Contains(err.Error(), "implementer-g1") {
			t.Fatalf("Introspect of a row whose projects claim is a list = %v, want ErrStoredClaimMalformed naming the session implementer-g1", err)
		}
	})

	t.Run("a store that has gone away", func(t *testing.T) {
		f.pool.Close()
		_, err := f.provider.Introspect(ctx, token)
		if err == nil {
			t.Fatal("Introspect on a closed store answered no error")
		}
		if !strings.Contains(err.Error(), "closed pool") {
			t.Fatalf("Introspect's error reads %q: it must carry the store's own failure (pgx's \"closed pool\")", err.Error())
		}
	})
}

// TestIntrospectingATokenWhoseRowDoesNotDecodeIsABrokerFailure: a live token whose stored row
// holds a public_key_jwk that is not base64 is a row the broker cannot decode, so Introspect
// returns the decode error, for the route to log beside its 503, and never answers the token not
// live. The row fails with the same base64 decode error a malformed token does
// (TestIntrospectingALiveTokenWithACharacterAddedIsNotLive): told apart by that error alone, a
// row the broker cannot read would be answered as a token nobody issued, denying a sound token
// and hiding the fault.
func TestIntrospectingATokenWhoseRowDoesNotDecodeIsABrokerFailure(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	token := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
	// The control: the token's row as it was written is live, so the failure below is the
	// rewrite's.
	if live := f.introspected(t, token); !live.Active {
		t.Fatalf("Introspect of the token as minted = %+v, want it live", live)
	}
	if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set request = jsonb_set(request, '{session,public_key_jwk}', '"!"')`); err != nil {
		t.Fatalf("rewrite the token's row: %v", err)
	}
	answer, err := f.provider.Introspect(ctx, token)
	if err == nil {
		t.Fatalf("Introspect of a row whose public_key_jwk is not base64 = %+v and no error, want the broker's failure", answer)
	}
	if !strings.Contains(err.Error(), "public_key_jwk") {
		t.Fatalf("Introspect's error reads %q: it must name the row's public_key_jwk, which is what failed to decode", err.Error())
	}
}

// TestTheExchangeClientAuthenticatesWithNoCredentialAndOnlyThere: the broker answers the exchange
// client as authenticated when it presents nothing at the token-exchange grant, because the
// subject token is the credential and shipping a secret to every pod is what this removes. Every
// neighbouring case stays the library's own refusal.
func TestTheExchangeClientAuthenticatesWithNoCredentialAndOnlyThere(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	for _, c := range []struct {
		name   string
		form   func(*testing.T) url.Values
		status int
		code   string
	}{
		{
			name: "the exchange client presenting no credential is the one case",
			form: func(t *testing.T) url.Values {
				return exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1")
			},
			status: http.StatusOK,
		},
		{
			name: "another client id presenting no credential is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-2", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g2")
				form.Set("client_id", "example-client")
				return form
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
		{
			name: "the machine login's client presenting no credential is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-3", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g3")
				form.Set("client_id", oauthwire.ClientID)
				return form
			},
			status: http.StatusBadRequest,
			code:   "invalid_grant",
		},
		{
			name: "the exchange client presenting no credential on another grant is refused",
			form: func(*testing.T) url.Values {
				return url.Values{
					"grant_type":    {"refresh_token"},
					"client_id":     {oauthwire.ExchangeClientID},
					"refresh_token": {"a-token-nobody-issued"},
				}
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
		{
			name: "the exchange client presenting a wrong secret is refused",
			form: func(t *testing.T) url.Values {
				form := exchangeForm(f.podToken(t, "pod-uid-4", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g4")
				form.Set("client_secret", "not-the-secret")
				return form
			},
			status: http.StatusUnauthorized,
			code:   "invalid_client",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.exchange(t, c.form(t), key)
			if status != c.status {
				t.Fatalf("= %d %v, want %d", status, body, c.status)
			}
			if c.code != "" && body["error"] != c.code {
				t.Fatalf("= %v, want the error %s", body, c.code)
			}
		})
	}
}

// TestAnIssuerTheBrokerWasNotToldAboutRegistersNoType: a type whose issuer is not configured is
// not registered at all, so a caller presenting it is refused by the library before any handler
// of the broker's runs.
func TestAnIssuerTheBrokerWasNotToldAboutRegistersNoType(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.GitHub = oauth.IssuerConfig{} })
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.workflowToken(t, "4242", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange against an unconfigured issuer = %d %v, want 400 invalid_request", status, body)
	}
}

// TestAnExchangeWithNoListedIdentityIsRefusedByName: a broker whose deployment has listed no
// identity still composes the exchange, because the token-exchange grant is the exchange
// client's only one, and refuses every caller by name rather than faulting on a policy nobody
// built.
func TestAnExchangeWithNoListedIdentityIsRefusedByName(t *testing.T) {
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Policy = nil })
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	if status != http.StatusBadRequest || body["error"] != "invalid_request" {
		t.Fatalf("an exchange against an empty policy = %d %v, want 400 invalid_request", status, body)
	}
	if hint, _ := body["error_description"].(string); !strings.Contains(hint, account) {
		t.Errorf("the refusal reads %q, which does not name the account no entry lists", hint)
	}
}

// fakeAccess is Dispatch's project-access answers for the exchange's tests: which person sees
// which project, which projects Dispatch holds, and each person's member kind. It records every
// question, can be made unreachable, and caches nothing, so a test changes an answer and the next
// trade reads it.
type fakeAccess struct {
	mu    sync.Mutex
	sees  map[string][]string // person → the projects they see
	held  []string            // the projects Dispatch holds
	kinds map[string]string   // person → member kind; absent is none
	down  bool
	// refused is Dispatch answering 403 to the broker's own account token.
	refused bool
	asked   []string
}

func newFakeAccess() *fakeAccess {
	return &fakeAccess{
		sees:  map[string][]string{person: {"PRIV", project}, otherPerson: {otherProject}},
		held:  []string{"PRIV", "SECRET", project, otherProject},
		kinds: map[string]string{},
	}
}

func (a *fakeAccess) Access(_ context.Context, key, who string) (projectaccess.Answer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, "access "+key+" "+who)
	if a.down {
		return projectaccess.Answer{}, errors.New("dispatch could not be reached")
	}
	if a.refused {
		return projectaccess.Answer{}, fmt.Errorf("%w: dispatch answered 403", dispatchread.ErrBrokerRefused)
	}
	if !slices.Contains(a.held, key) {
		return projectaccess.Answer{}, projectaccess.ErrProjectNotFound
	}
	return projectaccess.Answer{Project: key, Visibility: "private", Person: who, Visible: slices.Contains(a.sees[who], key)}, nil
}

func (a *fakeAccess) MemberKind(_ context.Context, who string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, "kind "+who)
	if a.down {
		return "", errors.New("dispatch could not be reached")
	}
	if a.refused {
		return "", fmt.Errorf("%w: dispatch answered 403", dispatchread.ErrBrokerRefused)
	}
	if kind, held := a.kinds[who]; held {
		return kind, nil
	}
	return projectaccess.MemberNone, nil
}

func (a *fakeAccess) States(context.Context, []string) (map[string]projectaccess.Answer, error) {
	return nil, errors.New("the exchange asks no project's state")
}

// questions is every question asked so far, and forgets them.
func (a *fakeAccess) questions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	asked := a.asked
	a.asked = nil
	return asked
}

func (a *fakeAccess) set(change func(*fakeAccess)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(a)
}

// withProjects is form asking for the projects raw spells.
func withProjects(form url.Values, raw string) url.Values {
	form.Set(oauthwire.FormProjects, raw)
	return form
}

// refusedSaying fails the test unless the exchange was refused with status and code, carried no
// access token, and its description carries says.
func refusedSaying(t *testing.T, status int, body map[string]any, wantStatus int, code, says string) {
	t.Helper()
	description, _ := body["error_description"].(string)
	if status != wantStatus || body["error"] != code || body["access_token"] != nil || !strings.Contains(description, says) {
		t.Fatalf("exchange = %d %v %q, want %d %s with no access token, saying %q", status, body["error"], description, wantStatus, code, says)
	}
}

// introspectsProjects fails the test unless token is live and names exactly projects.
func (f *exchangeFixture) introspectsProjects(t *testing.T, token string, scope string, projects []string) {
	t.Helper()
	answer, err := f.provider.Introspect(context.Background(), token)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if !answer.Active || answer.ProjectScope != scope || !slices.Equal(answer.Projects, projects) {
		t.Fatalf("Introspect = %+v, want a live %s token naming %v", answer, scope, projects)
	}
}

// TestAPersonTradeNamesOnlyProjectsItsPersonSees: a dev box session's trade acts as its person
// and names the projects it asks for, each of which Dispatch must say that person sees. One it
// does not see is refused naming it, and one Dispatch does not hold is refused in the very same
// words; a list that is no list is refused before Dispatch is asked anything.
func TestAPersonTradeNamesOnlyProjectsItsPersonSees(t *testing.T) {
	access := newFakeAccess()
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	trade := func(raw string) (int, map[string]any) {
		return f.exchange(t, withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), raw), key)
	}

	status, body := trade("PRIV " + project)
	f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{project, "PRIV"})

	status, body = trade("PRIV SECRET")
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "The token names a project its person cannot see: SECRET.")
	unseen, _ := body["error_description"].(string)
	status, body = trade("PRIV NOWHERE")
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "The token names a project its person cannot see: NOWHERE.")
	if missing, _ := body["error_description"].(string); missing != strings.ReplaceAll(unseen, "SECRET", "NOWHERE") {
		t.Fatalf("the refusal of a project Dispatch does not hold reads %q, and of one it holds %q: the two must read alike so a refusal never says whether a project exists", missing, unseen)
	}

	access.questions()
	status, body = trade("priv")
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "priv' is not a project key")
	if asked := access.questions(); len(asked) != 0 {
		t.Fatalf("a list that is no list asked Dispatch %v, want nothing", asked)
	}
}

// TestARestrictedMembersEmptyListIsRefused: a restricted member sees only the projects that name
// them, so their trade names which; one naming none is refused, saying how to name them.
func TestARestrictedMembersEmptyListIsRefused(t *testing.T) {
	access := newFakeAccess()
	access.kinds[person] = projectaccess.MemberRestricted
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	status, body := f.exchange(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request",
		person+" is a restricted member, whose token names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on.")
}

// TestAFullMembersEmptyListIsTaken: a full member's trade that names no project is answered as it
// is today, a token acting as its person that names none.
func TestAFullMembersEmptyListIsTaken(t *testing.T) {
	access := newFakeAccess()
	access.kinds[person] = projectaccess.MemberFull
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	token := f.exchanged(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	f.introspectsProjects(t, token, oauthwire.ProjectScopePerson, nil)
}

// TestAPersonWithNoDispatchRowIsTaken: a person Dispatch holds no row for — one who has never
// signed in to Dispatch — is no restricted member, so their trade naming no project is answered as
// it is today.
func TestAPersonWithNoDispatchRowIsTaken(t *testing.T) {
	access := newFakeAccess()
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	token := f.exchanged(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	f.introspectsProjects(t, token, oauthwire.ProjectScopePerson, nil)
	if asked := access.questions(); !slices.Equal(asked, []string{"kind " + person}) {
		t.Fatalf("the trade asked Dispatch %v, want only %s's member kind", asked, person)
	}
}

// TestAK3OutageMintsAnEmptyListAsToday: while Dispatch cannot be reached, a trade naming no
// project is answered as it is today, and one naming a project is refused as a broker that cannot
// check it — never minted unchecked — with a status a client asks again on.
func TestAK3OutageMintsAnEmptyListAsToday(t *testing.T) {
	access := newFakeAccess()
	access.down = true
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	token := f.exchanged(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	f.introspectsProjects(t, token, oauthwire.ProjectScopePerson, nil)

	status, body := f.exchange(t, withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), "PRIV"), key)
	refusedSaying(t, status, body, http.StatusServiceUnavailable, "temporarily_unavailable", "Dispatch")
}

// TestASignInTradeAsksNoMemberKind: a person's sign-in was verified against the deployment's own
// Workspace domain, so it is a full member's and asks Dispatch no member kind; a list it names is
// still held to what that person sees.
func TestASignInTradeAsksNoMemberKind(t *testing.T) {
	access := newFakeAccess()
	access.kinds[person] = projectaccess.MemberRestricted
	f := newSignInFixtureWith(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	trade := func(raw string) (int, map[string]any) {
		form := signInForm(f.personToken(t, person, deviceAudience, true, signInDomain), f.machineToken(t, signInMachine, machineAudience, true), "ada-box-1")
		return f.exchange(t, withProjects(form, raw), key)
	}
	status, body := trade("")
	f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, nil)
	status, body = trade("PRIV")
	f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{"PRIV"})
	status, body = trade("SECRET")
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "The token names a project its person cannot see: SECRET.")
	for _, question := range access.questions() {
		if strings.HasPrefix(question, "kind ") {
			t.Fatalf("a sign-in trade asked Dispatch %q, want no member kind asked of a person the broker verified itself", question)
		}
	}
}

// TestANamedTradeIsRefusedWhenItsEntryPersonCannotSeeTheProject: an operator's entry maps a token
// to one project and one person, and the exchange holds that pairing to Dispatch at every trade,
// for each of the three settings that write a named entry and for a result entry, whose grant
// names its one project the same way.
func TestANamedTradeIsRefusedWhenItsEntryPersonCannotSeeTheProject(t *testing.T) {
	access := newFakeAccess()
	access.sees[person] = []string{"PRIV"}
	f := newResultsFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names the project "+project+", which its person "+person+" cannot see")
	status, body = f.exchange(t, exchangeForm(f.workflowToken(t, "4242", nil), oauthwire.SubjectTokenTypeGitHub, "ci-release"), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names the project "+project+", which its person "+person+" cannot see")
	status, body = f.exchange(t, askingForThePlaceholder(f.resultRunForm(t, "8")), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names the project "+project+", which its person "+person+" cannot see")

	machines := newSignInFixtureWith(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, nil, nil, exchange.GoogleEntry{Account: signInMachine, Service: signInFleet, Project: project, Person: person})
		c.Exchange.Projects = access
	})
	form := exchangeForm(machines.machineToken(t, signInMachine, machineAudience, true), oauthwire.SubjectTokenTypeGoogle, "example-machine")
	status, body = machines.exchange(t, form, key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names the project "+project+", which its person "+person+" cannot see")

	access.set(func(a *fakeAccess) { a.sees[person] = []string{project} })
	f.introspectsProjects(t, f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key),
		oauthwire.ProjectScopeNamed, []string{project})
	if grant := f.grantOf(t, f.exchanged(t, askingForThePlaceholder(f.resultRunForm(t, "9")), key)); grant.Extra[oauth.ClaimProjectScope] != oauthwire.ProjectScopeResults || grant.Extra[oauth.ClaimProjects] != project {
		t.Errorf("the result grant's project scope and projects = %v and %v, want %s and %s", grant.Extra[oauth.ClaimProjectScope], grant.Extra[oauth.ClaimProjects], oauthwire.ProjectScopeResults, project)
	}
	machines.introspectsProjects(t, machines.exchanged(t, form, key), oauthwire.ProjectScopeNamed, []string{project})
}

// TestATokenStoredBeforeTheListIsNotLive: a token minted before this release stored its one
// project under `project` and no `projects`. Read with no `projects`, it would pass as a named
// token naming none, the public-session account's shape, which Dispatch holds to no project; it
// is answered not live instead, so its holder trades again for a token of this release. The same
// row with `projects` restored, and a machine login's own token, which no exchange decided and so
// carries neither claim, are unchanged.
func TestATokenStoredBeforeTheListIsNotLive(t *testing.T) {
	f := newExchangeFixture(t, func(*oauth.Config) {})
	ctx := context.Background()
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	token := f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	tag, err := f.pool.Exec(ctx, `update oauth_access_tokens
		set request = jsonb_set(request #- '{session,extra,projects}', '{session,extra,project}', to_jsonb($1::text))
		where request #> '{session,extra,projects}' is not null and request #> '{session,extra,project_scope}' is not null`, project)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rewrite the token's row to the older shape: %v, %d rows; want exactly the one row rewritten", err, tag.RowsAffected())
	}
	answer, err := f.provider.Introspect(ctx, token)
	if err != nil || !reflect.DeepEqual(answer, oauthwire.Introspection{}) {
		t.Fatalf("Introspect of a row stored before the list = %+v, %v; want not live", answer, err)
	}
	if _, err := f.pool.Exec(ctx, `update oauth_access_tokens set request = jsonb_set(request #- '{session,extra,project}', '{session,extra,projects}', to_jsonb($1::text))`, project); err != nil {
		t.Fatal(err)
	}
	f.introspectsProjects(t, token, oauthwire.ProjectScopeNamed, []string{project})

	machine := f.devBoxToken(t, newKey(t), person)
	if answer, err := f.provider.Introspect(ctx, machine); err != nil || !answer.Active || answer.ProjectScope != "" || answer.Projects != nil {
		t.Fatalf("Introspect of a machine login's own token = %+v, %v; want live with neither project claim", answer, err)
	}

	// A person token stored before the list wrote its scope and no project claim at all, which
	// meant then what an empty list means now: it is read as the person token naming no project
	// it always was, so a deployment with no private project keeps every such token live.
	boxKey := newKey(t)
	traded := f.exchanged(t, exchangeForm(f.devBoxToken(t, boxKey, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), boxKey)
	tag, err = f.pool.Exec(ctx, `update oauth_access_tokens set request = request #- '{session,extra,projects}'
		where request #>> '{session,extra,project_scope}' = 'person'`)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rewrite the person token's row to the older shape: %v, %d rows; want exactly the one row rewritten", err, tag.RowsAffected())
	}
	f.introspectsProjects(t, traded, oauthwire.ProjectScopePerson, nil)
}

// TestAnAccountEntryWithNoProjectMintsANamedTokenWithNoProject: the shared public-session
// account's entry names a person and no project. Its pod's token introspects named, with the
// entry's person and no project; when Dispatch calls that person a restricted member, who sees
// only the projects that name them, the trade is refused; a broker with no check, and one whose
// Dispatch cannot be asked, mint it as they would any list naming no project. A workflow entry
// may not leave its project empty.
func TestAnAccountEntryWithNoProjectMintsANamedTokenWithNoProject(t *testing.T) {
	trade := func(t *testing.T, access projectaccess.Access) (*exchangeFixture, int, map[string]any) {
		t.Helper()
		f := newExchangeFixture(t, func(c *oauth.Config) {
			c.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person}}, nil)
			c.Exchange.Projects = access
		})
		f.registers(t, account, "pod-uid-1", "public-session-1")
		status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), newKey(t))
		return f, status, body
	}
	mints := func(t *testing.T, access projectaccess.Access) {
		t.Helper()
		f, status, body := trade(t, access)
		token := accessTokenOf(t, status, body)
		f.introspectsProjects(t, token, oauthwire.ProjectScopeNamed, nil)
		answer, err := f.provider.Introspect(context.Background(), token)
		if err != nil || answer.Person != person {
			t.Fatalf("Introspect = %+v, %v; want the entry's person %s", answer, err, person)
		}
	}

	t.Run("a full member's entry mints a named token naming no project", func(t *testing.T) {
		access := newFakeAccess()
		access.kinds[person] = projectaccess.MemberFull
		mints(t, access)
	})
	t.Run("a restricted member's entry is refused", func(t *testing.T) {
		access := newFakeAccess()
		access.kinds[person] = projectaccess.MemberRestricted
		_, status, body := trade(t, access)
		refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "its entry names no project, and its person "+person+" is a restricted member")
	})
	t.Run("with no check, and with Dispatch down, it mints as a list naming no project does", func(t *testing.T) {
		mints(t, nil)
		down := newFakeAccess()
		down.kinds[person] = projectaccess.MemberRestricted
		down.down = true
		mints(t, down)
	})
	t.Run("a workflow entry may not leave its project empty", func(t *testing.T) {
		if _, err := exchange.NewSet(nil, nil, []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Person: person}}, nil, nil, signInDomain); err == nil {
			t.Fatal("NewSet with a workflow entry naming no project = nil error, want a refusal")
		}
	})
}

// TestANamedTradeMintsAsTodayWithNoProjectAccess: a broker with no project-access check mints an
// entry's token as it always has: the operator's own setting is what chose its project. A broker
// whose Dispatch cannot be asked is TestANamedTradeMintsUncheckedOnlyForAnEmptyList's.
func TestANamedTradeMintsAsTodayWithNoProjectAccess(t *testing.T) {
	unchecked := newExchangeFixture(t, func(*oauth.Config) {})
	key := newKey(t)
	unchecked.registers(t, account, "pod-uid-1", "implementer-g1")
	unchecked.introspectsProjects(t, unchecked.exchanged(t, exchangeForm(unchecked.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key),
		oauthwire.ProjectScopeNamed, []string{project})

	// A person's list, though, is never minted unchecked: with no check configured, a trade
	// naming a project is refused naming the setting that would check it.
	status, body := unchecked.exchange(t, withProjects(exchangeForm(unchecked.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), "PRIV"), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "BROKER_PROJECT_ACCESS_URL")
}

// TestEveryTradeChecksAgainBecauseThereIsNoRefreshToken: the grant issues no refresh token, so a
// renewal is a fresh trade, and a person who lost a project since the last one is refused it at the
// next.
func TestEveryTradeChecksAgainBecauseThereIsNoRefreshToken(t *testing.T) {
	access := newFakeAccess()
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	machine := f.devBoxToken(t, key, person)
	form := func() url.Values {
		return withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), "PRIV")
	}
	status, body := f.exchange(t, form(), key)
	if _, issued := body["refresh_token"]; issued {
		t.Fatalf("the trade answered a refresh token: %v; a renewal must be a fresh trade", body)
	}
	f.introspectsProjects(t, accessTokenOf(t, status, body), oauthwire.ProjectScopePerson, []string{"PRIV"})
	access.set(func(a *fakeAccess) { a.sees[person] = []string{project} })
	status, body = f.exchange(t, form(), key)
	refusedSaying(t, status, body, http.StatusBadRequest, "invalid_request", "The token names a project its person cannot see: PRIV.")
}

// capturedLog swaps the default logger for one that writes JSON lines into a buffer until the
// test ends, for the tests that read what the exchange logs.
func capturedLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buffer
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines is every logged line carrying each of fragments.
func (b *lockedBuffer) lines(fragments ...string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []string
	for _, line := range strings.Split(b.buf.String(), "\n") {
		matches := line != ""
		for _, fragment := range fragments {
			matches = matches && strings.Contains(line, fragment)
		}
		if matches {
			found = append(found, line)
		}
	}
	return found
}

// TestANamedTokenStoresItsProjectUnderTheOlderClaimToo: while an older broker replica still
// answers introspection beside this one (a rolling update, a canary, a rollback), it reads a named
// token's one project from the older `project` claim. A named token naming one project stores it
// there too, so the older replica answers `"project":"ACME"`, which this release's readers refuse,
// rather than a named token naming none, which they would read as the public-session shape and
// hold to no project. A named token naming none, and a person token, store no such claim.
func TestANamedTokenStoresItsProjectUnderTheOlderClaimToo(t *testing.T) {
	olderClaim := func(t *testing.T, f *exchangeFixture, session string) any {
		t.Helper()
		var claim any
		if err := f.pool.QueryRow(context.Background(), `select request #> '{session,extra,project}' from oauth_access_tokens
			where request #>> '{session,extra,session}' = $1`, session).Scan(&claim); err != nil {
			t.Fatalf("read the stored row of %s: %v", session, err)
		}
		return claim
	}
	f := newExchangeFixture(t, func(*oauth.Config) {})
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	f.exchanged(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), newKey(t))
	if got := olderClaim(t, f, "implementer-g1"); got != project {
		t.Fatalf("the named token's stored `project` claim = %#v, want %q, so an older replica answers it and this release's readers refuse it", got, project)
	}
	boxKey := newKey(t)
	f.exchanged(t, exchangeForm(f.devBoxToken(t, boxKey, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), boxKey)
	if got := olderClaim(t, f, "ada-devbox"); got != nil {
		t.Fatalf("the person token's stored `project` claim = %#v, want none", got)
	}

	public := newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: account, Person: person}}, nil)
	})
	public.registers(t, account, "pod-uid-1", "public-session-1")
	public.exchanged(t, exchangeForm(public.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), newKey(t))
	if got := olderClaim(t, public, "public-session-1"); got != nil {
		t.Fatalf("the public-session token's stored `project` claim = %#v, want none", got)
	}
}

// TestDispatchRefusingTheBrokersTokenRefusesTheTrade: Dispatch answering 403 to the broker's own
// account token is a deployment that has not listed the broker as one that may ask, a
// misconfiguration and not an outage, so the trade is refused naming that refusal and the setting
// that holds the token, never minted unchecked: an operator entry's token, a person's trade naming
// a project, and a person's trade naming none alike.
func TestDispatchRefusingTheBrokersTokenRefusesTheTrade(t *testing.T) {
	access := newFakeAccess()
	access.refused = true
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	const says = "Dispatch refused the broker's own account token (BROKER_PROJECT_ACCESS_TOKEN_FILE)"
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
	machine := f.devBoxToken(t, key, person)
	status, body = f.exchange(t, withProjects(exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), "PRIV"), key)
	refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
	status, body = f.exchange(t, exchangeForm(machine, oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	refusedSaying(t, status, body, http.StatusInternalServerError, "server_error", says)
}

// TestAnUncheckedMintLogsItsEntry: while Dispatch cannot be reached, an operator entry's token
// naming no project and a person's trade naming no project mint as they always have, and each such
// mint logs one line naming what was minted unchecked: the entry's principal, or the person. An
// entry's token naming a project is refused temporarily_unavailable instead, and logs no such line.
func TestAnUncheckedMintLogsItsEntry(t *testing.T) {
	logs := capturedLog(t)
	access := newFakeAccess()
	access.down = true
	f := newExchangeFixture(t, func(c *oauth.Config) { c.Exchange.Projects = access })
	key := newKey(t)
	f.registers(t, account, "pod-uid-1", "implementer-g1")
	status, body := f.exchange(t, exchangeForm(f.podToken(t, "pod-uid-1", nil), oauthwire.SubjectTokenTypeKubernetes, "implementer-g1"), key)
	refusedSaying(t, status, body, http.StatusServiceUnavailable, "temporarily_unavailable", "Dispatch could not be asked")
	if got := logs.lines("unchecked", account, "implementer-g1"); len(got) != 0 {
		t.Fatalf("the refused entry logged %d lines of an unchecked mint naming %s, want none:\n%s", len(got), account, strings.Join(got, "\n"))
	}

	public := newExchangeFixture(t, func(c *oauth.Config) {
		c.Exchange.Policy = newPolicy(t, []exchange.AccountEntry{{Account: otherAccount, Person: person}}, nil)
		c.Exchange.Projects = access
	})
	public.registers(t, otherAccount, "pod-uid-2", "public-session-1")
	public.exchanged(t, exchangeForm(public.podTokenOf(t, otherAccount, "pod-uid-2"), oauthwire.SubjectTokenTypeKubernetes, "public-session-1"), key)
	if got := logs.lines("unchecked", otherAccount, "public-session-1"); len(got) != 1 {
		t.Fatalf("the no-project entry's unchecked mint logged %d lines naming %s, want 1:\n%s", len(got), otherAccount, strings.Join(logs.lines(), "\n"))
	}

	f.exchanged(t, exchangeForm(f.devBoxToken(t, key, person), oauthwire.SubjectTokenTypeAccessToken, "ada-devbox"), key)
	if got := logs.lines("unchecked", person, "ada-devbox"); len(got) != 1 {
		t.Fatalf("the person's unchecked mint logged %d lines naming %s, want 1:\n%s", len(got), person, strings.Join(logs.lines(), "\n"))
	}
}
