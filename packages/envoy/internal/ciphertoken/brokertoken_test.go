package ciphertoken_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/ratelimit"
	"github.com/sjawhar/envoy/internal/testport"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

const (
	// dispatchOrigin is DISPATCH_SERVER_URL for these tests: the origin a caller signs its
	// proof's 'htu' against, whatever host the request reaches Dispatch under.
	dispatchOrigin = "https://dispatch.test"
	// resourcePath is the route a caller presents its token to.
	resourcePath = "/api/v1/sessions"
	// accountToken is what the stub broker expects Dispatch to introspect as, standing in for
	// the pod's projected ServiceAccount token.
	accountToken = "dispatchs-own-projected-service-account-token"
	// callerAddress is the source address the rate-limiter buckets are keyed on.
	callerAddress = "203.0.113.7:51789"
)

// clock is a test's own reading of now for the cache and the address buckets, so a test proves
// what happens after the cache lapses without sleeping through it. A proof's own acceptance
// window is the library's, on the real clock, so this never moves far enough to matter there.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Now()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// signer produces the requests a caller holding one key sends, through the shared stub caller so
// that every proof in this suite is the one the real producer makes rather than a second signer
// written here.
type signer struct {
	t      *testing.T
	caller *ciphertokentest.Caller
	jkt    string
}

// newSigner is a caller with a key of its own.
func newSigner(t *testing.T) *signer {
	t.Helper()
	caller := ciphertokentest.NewCaller(t)
	return &signer{t: t, caller: caller, jkt: caller.JKT()}
}

// request is one signed request presenting token at url, as Dispatch's handler would see it.
func (s *signer) request(url, token string) *http.Request {
	s.t.Helper()
	signed := s.caller.Signed(s.t, http.MethodGet, url, token)
	signed.RemoteAddr = callerAddress
	return signed
}

// at is a signed GET of path under Dispatch's own origin.
func (s *signer) at(path, token string) *http.Request {
	return s.request(dispatchOrigin+path, token)
}

// rig is one verifier, the broker it asks and the clock it reads.
type rig struct {
	broker   *ciphertokentest.StubBroker
	clock    *clock
	verifier *ciphertoken.Verifier
}

// newRig builds a verifier on a database of its own, with options applied before it is built.
func newRig(t *testing.T, options ...func(*ciphertoken.Options)) *rig {
	t.Helper()
	database := storetest.Open(t)
	broker := ciphertokentest.NewStubBroker(t)
	tick := newClock()
	opts := ciphertoken.Options{
		BrokerURL: broker.Server.URL,
		Account:   ciphertoken.NewAccountToken(accountTokenFile(t, accountToken)),
		Origin:    dispatchOrigin,
		Replay:    store.NewDPoPProofs(database.Pool),
		Address:   sourceAddress,
		Now:       tick.now,
	}
	for _, option := range options {
		option(&opts)
	}
	verifier, err := ciphertoken.New(opts)
	if err != nil {
		t.Fatalf("ciphertoken.New: %v", err)
	}
	return &rig{broker: broker, clock: tick, verifier: verifier}
}

// sourceAddress is the deployment's rule for which address a bucket is keyed on, as Options
// takes it: every request of this suite comes from callerAddress, so one bucket serves a test.
// Dispatch passes auth.ForwardedAddress here, which this package no longer reads for itself.
func sourceAddress(r *http.Request) string { return r.RemoteAddr }

// accountTokenFile writes token to a file of the test's own directory, as a pod's projected
// ServiceAccount token is mounted.
func accountTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write the account token file: %v", err)
	}
	return path
}

// liveToken is the broker's answer for a token it issued to jkt for acting as a session,
// expiring in life. Its client is the exchange every such token comes from, which the check
// holds an answer to: a token issued to any other client is no session's credential.
func liveToken(jkt string, life time.Duration) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      "ses_a1b2c3",
		Person:       "sjawhar",
		ProjectScope: ciphertoken.ProjectScopeNamed,
		Projects:     []string{"INFRA"},
		Scope:        "dispatch.write",
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// machineLoginToken is the answer a machine login's own access token gets: the credential the
// agent-secrets helper holds to ask the broker for everything else. It is live and DPoP-bound
// like any other, and no exchange decided a session, a person or a project scope for it, so
// the broker names none.
func machineLoginToken(jkt string, life time.Duration) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:     true,
		Scope:      "machine",
		ExpiresAt:  time.Now().Add(life).Unix(),
		ClientID:   "agent-secrets-helper",
		Thumbprint: jkt,
	}
}

// serviceToken is the answer a token that names a service and no person gets: what the exchange
// mints for a machine whose entry names no person, which names the fleet its machine belongs to.
func serviceToken(jkt string, life time.Duration) ciphertoken.Introspection {
	answer := liveToken(jkt, life)
	answer.Person, answer.Service = "", "example-fleet"
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
	return answer
}

// readOnlyToken is the answer a token a read entry minted gets (BROKER_EXCHANGE_READ_ACCOUNTS):
// the one scope read, the project scope none, the entry's service and the session it fixes, and
// no person and no project. It is the answer the broker's own test holds an exchanged read token
// to (internal/cipher/oauth, TestAReadEntrysPodExchangesForATokenThatOnlyReads).
func readOnlyToken(jkt string, life time.Duration) ciphertoken.Introspection {
	return ciphertoken.Introspection{
		Active:       true,
		Session:      "ses_reader",
		Service:      "example-reader",
		ProjectScope: ciphertoken.ProjectScopeNone,
		Scope:        ciphertoken.ScopeRead,
		ExpiresAt:    time.Now().Add(life).Unix(),
		ClientID:     ciphertoken.ExchangeClientID,
		Thumbprint:   jkt,
	}
}

// TestATokenThatIsNoSessionsCredentialIsRefused: a live, DPoP-bound token the broker issued for
// something other than acting as a session names no session to record writes as, so it is
// refused with an outcome of its own rather than handed back as a Caller for whoever mounts
// this check to fill in. Each leg of the rule is its own token: a machine login's own, one the
// exchange issued that names neither a person nor a service, one that names a session and a
// person but comes from another client, and the ten whose project scope or list this package
// cannot read — an answer carrying no project scope at all, one carrying a word it does not know,
// a named scope naming no person and no project and a service that is no whole account subject (a
// fleet, the bare account prefix, an account with no name, an upper-case account), a named scope
// naming an account and a project, a named scope with two projects, and a list no exchange could
// have minted. Those are refused rather than read as a token that names no project, which would
// hand their caller every project the person may write; the one naming a project under no scope
// would otherwise be the louder of the two, since the project beside the scope is then read under
// nothing at all. A named scope naming no person is minted only by an account entry naming no
// person, whose service is the account (TestANamedAnswerNamingNoPersonIsItsAccount), so the same
// answer naming any other service is no token the broker mints. The proof-checked Verify and the
// answer-only Live refuse every one, so an open connection cannot keep one alive either.
func TestATokenThatIsNoSessionsCredentialIsRefused(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	noOwner := liveToken(caller.jkt, time.Hour)
	noOwner.Person = ""
	anotherClient := liveToken(caller.jkt, time.Hour)
	anotherClient.ClientID = "agent-secrets-helper"
	noScope := liveToken(caller.jkt, time.Hour)
	noScope.ProjectScope = ""
	unknownScope := liveToken(caller.jkt, time.Hour)
	unknownScope.ProjectScope = "organisation"
	namedFleet := liveToken(caller.jkt, time.Hour)
	namedFleet.Person, namedFleet.Service, namedFleet.Projects = "", "example-fleet", nil
	// Services that open like an account and are no whole account subject, which no account
	// entry can hold (exchange.NewSet holds each to oauthwire.AccountSubject).
	namedPrefixOnly := liveToken(caller.jkt, time.Hour)
	namedPrefixOnly.Person, namedPrefixOnly.Service, namedPrefixOnly.Projects = "", "system:serviceaccount:", nil
	namedNoName := liveToken(caller.jkt, time.Hour)
	namedNoName.Person, namedNoName.Service, namedNoName.Projects = "", "system:serviceaccount:ns", nil
	namedUpperCase := liveToken(caller.jkt, time.Hour)
	namedUpperCase.Person, namedUpperCase.Service, namedUpperCase.Projects = "", "system:serviceaccount:NS:Name", nil
	// An account with text after it, and an account with text before it: the grammar is anchored
	// at both ends, so neither is the account it holds.
	namedTrailing := liveToken(caller.jkt, time.Hour)
	namedTrailing.Person, namedTrailing.Service, namedTrailing.Projects = "", "system:serviceaccount:example-ns:example-public===x", nil
	namedLeading := liveToken(caller.jkt, time.Hour)
	namedLeading.Person, namedLeading.Service, namedLeading.Projects = "", "xsystem:serviceaccount:example-ns:example-public", nil
	namedServiceProject := liveToken(caller.jkt, time.Hour)
	namedServiceProject.Person, namedServiceProject.Service = "", "system:serviceaccount:example-ns:example-public"
	namedTwo := liveToken(caller.jkt, time.Hour)
	namedTwo.Projects = []string{"INFRA", "PRIV"}
	unsorted := liveToken(caller.jkt, time.Hour)
	unsorted.ProjectScope, unsorted.Projects = ciphertoken.ProjectScopePerson, []string{"PUB", "PRIV"}
	for name, answer := range map[string]ciphertoken.Introspection{
		"a machine login's own token":                   machineLoginToken(caller.jkt, time.Hour),
		"a token naming neither a person nor service":   noOwner,
		"a token of another client":                     anotherClient,
		"a token naming no project scope":               noScope,
		"a token of an unknown project scope":           unknownScope,
		"a named scope naming a fleet and no person":    namedFleet,
		"a named scope naming the bare account prefix":  namedPrefixOnly,
		"a named scope naming an account with no name":  namedNoName,
		"a named scope naming an upper-case account":    namedUpperCase,
		"a named scope naming an account and more":      namedTrailing,
		"a named scope naming text and an account":      namedLeading,
		"a named scope naming an account and a project": namedServiceProject,
		"a named scope naming two projects":             namedTwo,
		"a list no exchange mints":                      unsorted,
	} {
		t.Run(name, func(t *testing.T) {
			token := "a-token-that-is-" + strings.ReplaceAll(name, " ", "-")
			rig.broker.Says(token, answer)

			decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
			if !errors.Is(err, ciphertoken.ErrNotASession) {
				t.Fatalf("Verify answered %v, want ErrNotASession", err)
			}
			if !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
				t.Fatalf("Verify = %+v, want no caller at all", decided)
			}
			live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
			if !errors.Is(err, ciphertoken.ErrNotASession) {
				t.Fatalf("Live answered %v, want ErrNotASession", err)
			}
			if !reflect.DeepEqual(live.Caller(), ciphertoken.Caller{}) {
				t.Fatalf("Live = %+v, want no caller at all", live.Caller())
			}
		})
	}

	// The negative control: the same shape with every claim a session's token carries is the
	// caller it has always been.
	const sound = "a-token-the-exchange-issued"
	rig.broker.Says(sound, liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, sound)); err != nil {
		t.Fatalf("a session's own token was refused: %v", err)
	}
}

// TestLiveRefusesANamedAnswerItCannotRead: a named token is an operator's entry's, which names
// one project, or none for an account entry naming none. An answer of that scope naming two,
// sixteen or seventeen projects is no token the exchange minted and is refused, so a Dispatch
// reader never takes it as a caller no project limits.
func TestLiveRefusesANamedAnswerItCannotRead(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("P%02d", i)
	}
	for name, projects := range map[string][]string{"two": {"INFRA", "PRIV"}, "sixteen": tooMany[:16], "seventeen": tooMany} {
		answer := liveToken(caller.jkt, time.Hour)
		answer.Projects = projects
		token := "a-named-token-naming-" + name
		rig.broker.Says(token, answer)
		if _, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) {
			t.Errorf("Live of a named answer naming %s projects = %v, want ErrNotASession", name, err)
		}
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) {
			t.Errorf("Verify of a named answer naming %s projects = %v, want ErrNotASession", name, err)
		}
	}
}

// TestAnAnswerOfABrokerBeforeTheListIsRefused: a broker before the list answered a named token's
// one project under `project` and named no `projects`. This reader no longer reads that field, so
// such an answer would otherwise pass as a named token naming no project — the shared
// public-session account's shape, which Dispatch holds to no project. It is refused instead, as a
// token this reader cannot read, whatever else it says; the same answer without `project` is the
// public-session caller it says it is.
func TestAnAnswerOfABrokerBeforeTheListIsRefused(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	legacy := fmt.Sprintf(`{"active":true,"session":"ses_a1b2c3","person":"sjawhar","project_scope":"named","project":"INFRA","scope":"dispatch.write","exp":%d,"client_id":%q,"jkt":%q}`,
		time.Now().Add(time.Hour).Unix(), ciphertoken.ExchangeClientID, caller.jkt)
	const token = "a-token-an-older-broker-minted"
	rig.broker.SaysRaw(token, legacy)
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
		t.Fatalf("Verify of an older broker's named answer = %+v, %v; want no caller and ErrNotASession", decided, err)
	}
	if decided, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided.Caller(), ciphertoken.Caller{}) {
		t.Fatalf("Live of an older broker's named answer = %+v, %v; want no caller and ErrNotASession", decided.Caller(), err)
	}
	const control = "the-same-answer-without-project"
	rig.broker.SaysRaw(control, strings.Replace(legacy, `"project":"INFRA",`, "", 1))
	if decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, control)); err != nil || decided.ProjectScope != ciphertoken.ProjectScopeNamed || decided.Person != "sjawhar" {
		t.Fatalf("Verify of the same answer without project = %+v, %v; want the named public-session caller", decided, err)
	}
}

// TestANamedAnswerNamingNoProjectIsAPublicSession: an account entry naming a person and no project
// (the shared public-session account's) mints a named token naming no project; its answer is the
// caller it says, with the named scope and no project.
func TestANamedAnswerNamingNoProjectIsAPublicSession(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	answer := liveToken(caller.jkt, time.Hour)
	answer.Projects = nil
	const token = "a-public-session-token"
	rig.broker.Says(token, answer)
	want := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", ProjectScope: ciphertoken.ProjectScopeNamed}
	for name, decide := range map[string]func() (ciphertoken.Caller, error){
		"Verify": func() (ciphertoken.Caller, error) {
			return rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		},
		"Live": func() (ciphertoken.Caller, error) {
			live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
			return live.Caller(), err
		},
	} {
		decided, err := decide()
		if err != nil || !reflect.DeepEqual(decided, want) {
			t.Errorf("%s = %+v, %v; want %+v", name, decided, err, want)
		}
	}
}

// TestANamedAnswerNamingNoPersonIsItsAccount: an account entry naming no person and no project
// mints a named token naming no person and no project, whose service is the account it acts as;
// its answer is that caller, under Verify, Live and the NATS auth service's SessionCaller alike,
// and so is the answer of such an entry that fixes its session (<service account>===<session>),
// whose service is the same account. The same answer naming a project, or naming no service, is
// no token the exchange mints.
func TestANamedAnswerNamingNoPersonIsItsAccount(t *testing.T) {
	const account = "system:serviceaccount:example-ns:example-public"
	rig := newRig(t)
	caller := newSigner(t)
	answer := liveToken(caller.jkt, time.Hour)
	answer.Person, answer.Service, answer.Projects = "", account, nil
	const token = "a-personless-account-token"
	rig.broker.Says(token, answer)
	want := ciphertoken.Caller{Session: "ses_a1b2c3", Service: account, ProjectScope: ciphertoken.ProjectScopeNamed}
	for name, decide := range map[string]func() (ciphertoken.Caller, error){
		"Verify": func() (ciphertoken.Caller, error) {
			return rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		},
		"Live": func() (ciphertoken.Caller, error) {
			live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
			return live.Caller(), err
		},
		"SessionCaller": func() (ciphertoken.Caller, error) { return ciphertoken.SessionCaller(answer) },
	} {
		decided, err := decide()
		if err != nil || !reflect.DeepEqual(decided, want) {
			t.Errorf("%s = %+v, %v; want %+v", name, decided, err, want)
		}
	}
	fixed := answer
	fixed.Session = "example-public-session"
	if decided, err := ciphertoken.SessionCaller(fixed); err != nil || !reflect.DeepEqual(decided, ciphertoken.Caller{Session: fixed.Session, Service: account, ProjectScope: ciphertoken.ProjectScopeNamed}) {
		t.Errorf("SessionCaller of the answer of an entry naming no person that fixes its session = %+v, %v; want that session acting for %s", decided, err, account)
	}
	withProject := answer
	withProject.Projects = []string{"INFRA"}
	noService := answer
	noService.Service = ""
	for name, refused := range map[string]ciphertoken.Introspection{"naming a project": withProject, "naming no service": noService} {
		if _, err := ciphertoken.SessionCaller(refused); !errors.Is(err, ciphertoken.ErrNotASession) {
			t.Errorf("SessionCaller of a named answer naming no person and %s = %v, want ErrNotASession", name, err)
		}
	}
}

// TestLiveRefusesAServiceAnswerWithProjects: a token that names a service and no person names no
// project (the exchange refuses a machine's trade naming one), so an answer that does is no token
// the exchange minted.
func TestLiveRefusesAServiceAnswerWithProjects(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	answer := serviceToken(caller.jkt, time.Hour)
	answer.Projects = []string{"INFRA"}
	const token = "a-service-token-naming-a-project"
	rig.broker.Says(token, answer)
	if _, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) {
		t.Fatalf("Live of a service answer naming a project = %v, want ErrNotASession", err)
	}
}

// TestCallerCarriesTheProjects: the token's list reaches the Caller whole, under either scope, as
// the caller's own copy: a named token's one project and a person token's list alike.
func TestCallerCarriesTheProjects(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	person := liveToken(caller.jkt, time.Hour)
	person.ProjectScope, person.Projects = ciphertoken.ProjectScopePerson, []string{"PRIV", "PUB"}
	for name, c := range map[string]struct {
		answer ciphertoken.Introspection
		want   ciphertoken.Caller
	}{
		"named":  {liveToken(caller.jkt, time.Hour), ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", Projects: []string{"INFRA"}, ProjectScope: ciphertoken.ProjectScopeNamed}},
		"person": {person, ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", Projects: []string{"PRIV", "PUB"}, ProjectScope: ciphertoken.ProjectScopePerson}},
	} {
		token := "a-" + name + "-token-with-projects"
		rig.broker.Says(token, c.answer)
		decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		if err != nil || !reflect.DeepEqual(decided, c.want) {
			t.Errorf("Verify(%s) = %+v, %v; want %+v", name, decided, err, c.want)
		}
	}
}

// TestATokenNamingAServiceAndNoPersonIsASessionsCredential: a machine whose entry names no
// person is exchanged a token that names its service instead, and such a token is a session's
// credential like any other here — it names the session its writes are recorded as, and the
// service is what whoever mounts this check holds it to. Both checks answer the service, so an
// open connection resolves to the same caller the request did.
func TestATokenNamingAServiceAndNoPersonIsASessionsCredential(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-the-exchange-issued-a-machine"
	rig.broker.Says(token, serviceToken(caller.jkt, time.Hour))

	// The scope is carried whole, as callerOf carries it for every answer: a service-only token
	// gains nothing by it, since the one reader of the field (api.broker_token's person-scope
	// branch) asks for a person first, and this caller has none.
	want := ciphertoken.Caller{Session: "ses_a1b2c3", Service: "example-fleet", ProjectScope: ciphertoken.ProjectScopePerson}
	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil {
		t.Fatalf("Verify refused a token naming a service and no person: %v", err)
	}
	if !reflect.DeepEqual(decided, want) {
		t.Fatalf("Verify = %+v, want %+v: the session and the service, with no person and no project", decided, want)
	}
	live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
	if err != nil || !reflect.DeepEqual(live.Caller(), want) {
		t.Fatalf("Live = %+v, %v; want %+v", live.Caller(), err, want)
	}
}

// TestAReadOnlyTokenIsAcceptedOnlyByAVerifierThatServesOne: a read-only token — the project scope
// none beside the one scope read, a service, and no person and no projects — is a session's
// credential only to a verifier whose service asked for it (Options.ServesReadOnly), and is
// ErrNotASession to every other, as a build before the switch refused it. The verifier that
// serves one answers its Caller marked read-only, so that service's own gate reads the mark and
// nothing else.
//
// The rule is exact, and the switch widens it by nothing: none in any other shape (no scope, a
// second word beside read, a person, a project in its list, no service) and the word read beside
// the project scope named or person are refused by both verifiers. The negative control is a
// named token carrying a scope that is not read, which both accept unmarked, so each refusal
// above is the word read's or the shape's and never the scope member's being set at all. Each row
// goes through Verify and through Live, so an open connection's heartbeat is held to the same
// rule as the request that opened it. SessionCaller, the NATS auth service's reading of an answer,
// serves no read-only token: it answers each row as the verifier without the switch does.
func TestAReadOnlyTokenIsAcceptedOnlyByAVerifierThatServesOne(t *testing.T) {
	caller := newSigner(t)
	readOnly := readOnlyToken(caller.jkt, time.Hour)
	readOnlyCaller := ciphertoken.Caller{Session: "ses_reader", Service: "example-reader", ProjectScope: ciphertoken.ProjectScopeNone, ReadOnly: true}

	noScope := readOnlyToken(caller.jkt, time.Hour)
	noScope.Scope = ""
	readAndMachine := readOnlyToken(caller.jkt, time.Hour)
	readAndMachine.Scope = "read machine"
	withPerson := readOnlyToken(caller.jkt, time.Hour)
	withPerson.Person = "sjawhar"
	withProject := readOnlyToken(caller.jkt, time.Hour)
	withProject.Projects = []string{"INFRA"}
	noService := readOnlyToken(caller.jkt, time.Hour)
	noService.Service = ""
	namedRead := liveToken(caller.jkt, time.Hour)
	namedRead.Scope = ciphertoken.ScopeRead
	personRead := liveToken(caller.jkt, time.Hour)
	personRead.ProjectScope, personRead.Projects = ciphertoken.ProjectScopePerson, nil
	personRead.Scope = "dispatch.write read"
	named := liveToken(caller.jkt, time.Hour)
	namedCaller := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", Projects: []string{"INFRA"}, ProjectScope: ciphertoken.ProjectScopeNamed}

	rows := []struct {
		name   string
		answer ciphertoken.Introspection
		// without and with are the Caller each verifier answers: the zero Caller is a refusal,
		// which is ErrNotASession for every row here.
		without ciphertoken.Caller
		with    ciphertoken.Caller
	}{
		{name: "the read-only token", answer: readOnly, with: readOnlyCaller},
		{name: "none with no scope", answer: noScope},
		{name: "none with a second scope beside read", answer: readAndMachine},
		{name: "none naming a person", answer: withPerson},
		{name: "none naming a project", answer: withProject},
		{name: "none naming no service", answer: noService},
		{name: "read beside the named scope", answer: namedRead},
		{name: "read among other scopes beside the person scope", answer: personRead},
		{name: "the named scope carrying another scope", answer: named, without: namedCaller, with: namedCaller},
	}
	for _, serves := range []bool{false, true} {
		rig := newRig(t, func(options *ciphertoken.Options) { options.ServesReadOnly = serves })
		verifier := "without the switch"
		if serves {
			verifier = "with the switch"
		}
		for _, row := range rows {
			t.Run(verifier+"/"+row.name, func(t *testing.T) {
				token := "a-token-of-" + strings.ReplaceAll(row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				want := row.without
				if serves {
					want = row.with
				}
				var wantErr error
				if reflect.DeepEqual(want, ciphertoken.Caller{}) {
					wantErr = ciphertoken.ErrNotASession
				}

				decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(decided, want) {
					t.Fatalf("Verify = %+v, %v; want %+v, %v", decided, err, want, wantErr)
				}
				live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(live.Caller(), want) {
					t.Fatalf("Live = %+v, %v; want %+v, %v", live.Caller(), err, want, wantErr)
				}
			})
		}
	}
	for _, row := range rows {
		t.Run("SessionCaller/"+row.name, func(t *testing.T) {
			var wantErr error
			if reflect.DeepEqual(row.without, ciphertoken.Caller{}) {
				wantErr = ciphertoken.ErrNotASession
			}
			got, err := ciphertoken.SessionCaller(row.answer)
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, row.without) {
				t.Fatalf("SessionCaller = %+v, %v; want %+v, %v", got, err, row.without, wantErr)
			}
		})
	}
}

// TestANarrowedAnswerIsACallerOnlyToAVerifierThatServesReadOnly: a token narrowed to reading — a
// narrowed project scope beside the scope read, or read person for a read-person token that names
// its person — is a session's credential only to a verifier whose service serves a read-only
// token, as the read entry's token is. With the switch, each answer of the golden file the
// broker's own test holds its narrowed answers to is a Caller marked read-only, naming the
// session, the person, the service and the projects the answer names, under the base word its
// narrowed word narrows, with the answer's cut. Without the switch, and to SessionCaller, each is
// ErrNotASession, as a build that predates the words refuses it.
//
// The switch widens the rule by nothing else. With it, these are ErrNotASession too: read person,
// in either order, on a narrowed word that may not hold person, approve or machine beside read,
// read named twice, no scope at all, a read-none token naming a person or a project, a read-named
// token whose projects break the named rules, one naming no person and a service that is no
// Kubernetes account, and one naming no person, an account and a project, a cut on a token that
// writes, a cut on the read entry's token (the word none, which no trade that asks for read and a
// cut carries), and a cut that is not an RFC 3339 time. The controls are the golden machine
// login's answer with a cut, which the switch accepts with its cut, and the named token with no
// cut, which both verifiers accept unmarked, so each refusal of a cut is the cut's. Each row goes
// through Verify and through Live.
func TestANarrowedAnswerIsACallerOnlyToAVerifierThatServesReadOnly(t *testing.T) {
	caller := newSigner(t)
	const session = "ses_narrowed"
	answers := ciphertokentest.NarrowedAnswers(t, caller.jkt, session, time.Now().Add(time.Hour).Unix())
	// bases is the word each narrowed word reads as, written out here rather than read from
	// oauthwire.BaseProjectScope, so the test holds the helper to it.
	bases := map[string]string{
		oauthwire.ProjectScopeReadNamed:  ciphertoken.ProjectScopeNamed,
		oauthwire.ProjectScopeReadPerson: ciphertoken.ProjectScopePerson,
		oauthwire.ProjectScopeReadNone:   ciphertoken.ProjectScopeNone,
	}
	const cutText = "2026-11-18T09:30:00.123456Z"
	cut := time.Date(2026, 11, 18, 9, 30, 0, 123456000, time.UTC)
	// cuts is the cut of each golden answer that names one; every other names none.
	cuts := map[string]time.Time{"machine-login-with-a-cut": cut}

	type row struct {
		name   string
		answer ciphertoken.Introspection
		// without and with are the Caller each verifier answers: the zero Caller is a refusal,
		// which is ErrNotASession for every row here.
		without ciphertoken.Caller
		with    ciphertoken.Caller
	}
	var rows []row
	narrowedWords := map[string]bool{}
	for _, kind := range slices.Sorted(maps.Keys(answers)) {
		answer := answers[kind]
		base, narrowed := bases[answer.ProjectScope]
		if !narrowed {
			t.Fatalf("the golden answer %s carries the project scope %q, which is no narrowed word", kind, answer.ProjectScope)
		}
		narrowedWords[answer.ProjectScope] = true
		rows = append(rows, row{name: "the golden answer " + kind, answer: answer, with: ciphertoken.Caller{
			Session: session, Person: answer.Person, Service: answer.Service, Projects: answer.Projects,
			ProjectScope: base, ReadOnly: true, Cut: cuts[kind],
		}})
	}
	if len(narrowedWords) != len(bases) {
		t.Fatalf("the golden answers carry the narrowed words %v, want each of %v", slices.Sorted(maps.Keys(narrowedWords)), slices.Sorted(maps.Keys(bases)))
	}

	// changed is the golden answer of kind with one change made, on a copy of its projects.
	changed := func(kind string, change func(*ciphertoken.Introspection)) ciphertoken.Introspection {
		answer, ok := answers[kind]
		if !ok {
			t.Fatalf("the golden file holds no answer for %s", kind)
		}
		answer.Projects = slices.Clone(answer.Projects)
		change(&answer)
		return answer
	}
	scoped := func(kind, scope string) ciphertoken.Introspection {
		return changed(kind, func(answer *ciphertoken.Introspection) { answer.Scope = scope })
	}
	named := liveToken(caller.jkt, time.Hour)
	namedCaller := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", Projects: []string{"INFRA"}, ProjectScope: ciphertoken.ProjectScopeNamed}
	namedCut := liveToken(caller.jkt, time.Hour)
	namedCut.AsOf = cutText
	personCut := liveToken(caller.jkt, time.Hour)
	personCut.ProjectScope, personCut.Projects, personCut.AsOf = ciphertoken.ProjectScopePerson, nil, cutText
	readEntryCut := readOnlyToken(caller.jkt, time.Hour)
	readEntryCut.AsOf = cutText
	rows = append(rows,
		row{name: "read person on read-named", answer: scoped("pod-entry", "read person")},
		row{name: "read person on read-none", answer: scoped("read-entry", "read person")},
		row{name: "read person on read-person naming a service", answer: scoped("google-identity-no-person", "read person")},
		row{name: "approve beside read", answer: scoped("pod-entry", "read approve")},
		row{name: "approve beside read person", answer: scoped("machine-login", "read person approve")},
		row{name: "machine beside read", answer: scoped("machine-login", "read machine")},
		row{name: "person before read on read-named", answer: scoped("pod-entry", "person read")},
		row{name: "read named twice", answer: scoped("read-entry", "read read")},
		row{name: "a narrowed word with no scope", answer: scoped("pod-entry", "")},
		row{name: "read-none naming a person", answer: changed("read-entry", func(answer *ciphertoken.Introspection) {
			answer.Person = "ada@example.com"
		})},
		row{name: "read-none naming a project", answer: changed("read-entry", func(answer *ciphertoken.Introspection) {
			answer.Projects = []string{"ACME"}
		})},
		row{name: "read-named naming two projects", answer: changed("pod-entry", func(answer *ciphertoken.Introspection) {
			answer.Projects = []string{"ACME", "OTHER"}
		})},
		row{name: "read-named naming no person and a service that is no account", answer: changed("pod-entry", func(answer *ciphertoken.Introspection) {
			answer.Person, answer.Service, answer.Projects = "", "example-fleet", nil
		})},
		row{name: "read-named naming no person, an account and a project", answer: changed("pod-entry-no-person", func(answer *ciphertoken.Introspection) {
			answer.Projects = []string{"ACME"}
		})},
		row{name: "a cut on a named token", answer: namedCut},
		row{name: "a cut on a person token", answer: personCut},
		row{name: "a cut that does not parse", answer: changed("machine-login-with-a-cut", func(answer *ciphertoken.Introspection) {
			answer.AsOf = "yesterday"
		})},
		row{name: "a cut with a space for its T", answer: changed("machine-login-with-a-cut", func(answer *ciphertoken.Introspection) {
			answer.AsOf = "2026-11-18 09:30:00.123456Z"
		})},
		row{name: "a cut at the zero time", answer: changed("machine-login-with-a-cut", func(answer *ciphertoken.Introspection) {
			answer.AsOf = "0001-01-01T00:00:00Z"
		})},
		// A trade that asks for no scope keeps the read entry's word none, and the broker refuses a
		// cut without scope=read, so no broker mints none beside a cut: only a narrowed word
		// carries one.
		row{name: "a cut on the read entry's token", answer: readEntryCut},
		row{name: "the named token with no cut", answer: named, without: namedCaller, with: namedCaller},
	)

	for _, serves := range []bool{false, true} {
		// The bucket is wide enough for every row's token: what is under test is the answer.
		rig := newRig(t, func(options *ciphertoken.Options) {
			options.ServesReadOnly = serves
			options.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: 2 * len(rows)}
		})
		verifier := "without the switch"
		if serves {
			verifier = "with the switch"
		}
		for _, row := range rows {
			t.Run(verifier+"/"+row.name, func(t *testing.T) {
				token := "a-token-of-" + strings.ReplaceAll(row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				want := row.without
				if serves {
					want = row.with
				}
				var wantErr error
				if reflect.DeepEqual(want, ciphertoken.Caller{}) {
					wantErr = ciphertoken.ErrNotASession
				}

				decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(decided, want) {
					t.Fatalf("Verify = %+v, %v; want %+v, %v", decided, err, want, wantErr)
				}
				live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(live, want) {
					t.Fatalf("Live = %+v, %v; want %+v, %v", live, err, want, wantErr)
				}
			})
		}
	}
	for _, row := range rows {
		t.Run("SessionCaller/"+row.name, func(t *testing.T) {
			var wantErr error
			if reflect.DeepEqual(row.without, ciphertoken.Caller{}) {
				wantErr = ciphertoken.ErrNotASession
			}
			got, err := ciphertoken.SessionCaller(row.answer)
			if !errors.Is(err, wantErr) || !reflect.DeepEqual(got, row.without) {
				t.Fatalf("SessionCaller = %+v, %v; want %+v, %v", got, err, row.without, wantErr)
			}
		})
	}
}

// TestShapeOfReadsTheScopeAsASet: a scope is a set of words, whose order means nothing (RFC
// 6749, section 3.3). So each golden answer of a narrowed person's token, whose scope the broker
// grants as read person, reads the same with its scope written person read: the same Caller,
// marked read-only and carrying its cut, to a verifier that serves a read-only token, and the
// same refusal to one that does not and to SessionCaller, as a read-only token's shape reads from
// outside. Each row goes through Verify and through Live.
func TestShapeOfReadsTheScopeAsASet(t *testing.T) {
	caller := newSigner(t)
	const session = "ses_scope_set"
	answers := ciphertokentest.NarrowedAnswers(t, caller.jkt, session, time.Now().Add(time.Hour).Unix())
	var kinds []string
	for _, kind := range slices.Sorted(maps.Keys(answers)) {
		if answers[kind].Scope == "read person" {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no golden answer carries the scope read person")
	}
	reordered := func(kind string) ciphertoken.Introspection {
		answer := answers[kind]
		answer.Projects = slices.Clone(answer.Projects)
		answer.Scope = "person read"
		return answer
	}

	for _, serves := range []bool{false, true} {
		// The bucket is wide enough for both tokens of every kind: what is under test is the answer.
		rig := newRig(t, func(options *ciphertoken.Options) {
			options.ServesReadOnly = serves
			options.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: 4 * len(kinds)}
		})
		verifier := "without the switch"
		if serves {
			verifier = "with the switch"
		}
		for _, kind := range kinds {
			t.Run(verifier+"/"+kind, func(t *testing.T) {
				granted, written := "a-token-of-"+kind+"-granted", "a-token-of-"+kind+"-reordered"
				rig.broker.Says(granted, answers[kind])
				rig.broker.Says(written, reordered(kind))
				for _, read := range []struct {
					name string
					call func(context.Context, *http.Request) (ciphertoken.Caller, error)
				}{{"Verify", rig.verifier.Verify}, {"Live", func(ctx context.Context, r *http.Request) (ciphertoken.Caller, error) {
					live, err := rig.verifier.Live(ctx, r)
					return live.Caller(), err
				}}} {
					want, wantErr := read.call(t.Context(), caller.at(resourcePath, granted))
					if serves && (wantErr != nil || !want.ReadOnly) {
						t.Fatalf("%s of the scope read person = %+v, %v; want a read-only Caller", read.name, want, wantErr)
					}
					got, err := read.call(t.Context(), caller.at(resourcePath, written))
					if !reflect.DeepEqual(got, want) || (err == nil) != (wantErr == nil) || (wantErr != nil && !errors.Is(err, wantErr)) {
						t.Fatalf("%s of the scope person read = %+v, %v; want %+v, %v, as the scope read person reads", read.name, got, err, want, wantErr)
					}
				}
			})
		}
	}
	for _, kind := range kinds {
		t.Run("SessionCaller/"+kind, func(t *testing.T) {
			want, wantErr := ciphertoken.SessionCaller(answers[kind])
			got, err := ciphertoken.SessionCaller(reordered(kind))
			if !reflect.DeepEqual(got, want) || !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) {
				t.Fatalf("SessionCaller of the scope person read = %+v, %v; want %+v, %v, as the scope read person reads", got, err, want, wantErr)
			}
		})
	}
}

// TestALiveTokenWithASoundProofIsAccepted: the two checks together admit the caller, and what
// the broker said the token is reaches the caller's decision.
func TestALiveTokenWithASoundProofIsAccepted(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-the-broker-issued"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil {
		t.Fatalf("Verify refused a live token with a sound proof: %v", err)
	}
	wantCaller := ciphertoken.Caller{
		Session:      "ses_a1b2c3",
		Person:       "sjawhar",
		Projects:     []string{"INFRA"},
		ProjectScope: ciphertoken.ProjectScopeNamed,
	}
	if !reflect.DeepEqual(decided, wantCaller) {
		t.Fatalf("Verify = %+v, want the session, the person, the project and its scope and nothing else of the answer", decided)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the broker was asked %d times, want once", rig.broker.Count())
	}
	if want := "Bearer " + accountToken; rig.broker.Bearers()[0] != want {
		t.Fatalf("the broker was presented %q, want Dispatch's own %q", rig.broker.Bearers()[0], want)
	}
}

// TestAPersonScopedTokenNamesNoProject: a token scoped to its person whose trade asked for no
// project names none, and says so with its scope.
func TestAPersonScopedTokenNamesNoProject(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-dev-box-sessions-token"
	answer := liveToken(caller.jkt, time.Hour)
	answer.ProjectScope, answer.Projects = ciphertoken.ProjectScopePerson, nil
	rig.broker.Says(token, answer)

	decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
	if err != nil {
		t.Fatalf("Verify refused a person-scoped token: %v", err)
	}
	want := ciphertoken.Caller{Session: "ses_a1b2c3", Person: "sjawhar", ProjectScope: ciphertoken.ProjectScopePerson}
	if !reflect.DeepEqual(decided, want) {
		t.Fatalf("Verify = %+v, want the session and the person with the person scope and no project", decided)
	}
}

// TestOneTokenCostsOneIntrospectionInsideTheCachesLife: the cache is what keeps this check off
// the broker's account-class bucket, so many requests bearing one live token cost one
// introspection, and the answer is dropped at the token's own expiry rather than held for the
// whole window.
func TestOneTokenCostsOneIntrospectionInsideTheCachesLife(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-presented-again-and-again"
	// Two seconds, inside AffirmativeCacheTTL, so the answer's own expiry is what ends it.
	rig.broker.Says(token, liveToken(caller.jkt, 2*time.Second))

	for request := range 8 {
		if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
			t.Fatalf("request %d: %v", request, err)
		}
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("8 requests cost %d introspections, want 1", rig.broker.Count())
	}

	rig.clock.advance(3 * time.Second)
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
		t.Fatalf("the request past the answer's expiry: %v", err)
	}
	if rig.broker.Count() != 2 {
		t.Fatalf("the request past the answer's expiry cost %d introspections in all, want 2", rig.broker.Count())
	}
}

// TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection: the single flight is what makes a
// burst on a cold cache cost one introspection rather than one a request, and the address bucket
// is spent inside that flight, so the burst costs one request of it rather than one each. The
// bucket here holds exactly one, so charging per request would refuse every request but the
// first — which is what an honest caller making a burst of calls on a token it has just been
// given would meet.
func TestRequestsArrivingAtOnceWithOneTokenCostOneIntrospection(t *testing.T) {
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Hour, Burst: 1}
	})
	caller := newSigner(t)
	const token = "a-token-many-requests-arrive-with-at-once"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	// Each request needs its own proof: a replayed one is refused, which is a different check.
	const requests = 8
	signed := make([]*http.Request, requests)
	for i := range signed {
		signed[i] = caller.at(fmt.Sprintf("%s/%d", resourcePath, i), token)
	}
	var wait sync.WaitGroup
	failures := make([]error, requests)
	for i := range signed {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, failures[i] = rig.verifier.Verify(t.Context(), signed[i])
		}()
	}
	wait.Wait()
	for i, err := range failures {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("%d requests arriving at once cost %d introspections, want 1", requests, rig.broker.Count())
	}
}

// TestATokenTheBrokerCallsNotLiveIsRefusedAndNeverRemembered: a refusal is never served from the
// cache, so a token the broker reinstates is accepted on its next presentation rather than kept
// refused.
func TestATokenTheBrokerCallsNotLiveIsRefusedAndNeverRemembered(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-the-broker-does-not-know"

	for request := range 2 {
		_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		if !errors.Is(err, ciphertoken.ErrNotActive) {
			t.Fatalf("request %d answered %v, want ErrNotActive", request, err)
		}
	}
	if rig.broker.Count() != 2 {
		t.Fatalf("two presentations of a refused token cost %d introspections, want 2", rig.broker.Count())
	}

	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err != nil {
		t.Fatalf("the token the broker now calls live: %v", err)
	}
}

// TestAProofSignedByAnotherKeyIsRefused: holding the token is not enough, which is what the
// binding is for.
func TestAProofSignedByAnotherKeyIsRefused(t *testing.T) {
	rig := newRig(t)
	owner, thief := newSigner(t), newSigner(t)
	const token = "a-token-bound-to-the-owners-key"
	rig.broker.Says(token, liveToken(owner.jkt, time.Hour))

	_, err := rig.verifier.Verify(t.Context(), thief.at(resourcePath, token))
	if !errors.Is(err, ciphertoken.ErrKeyMismatch) {
		t.Fatalf("a proof signed by another key answered %v, want ErrKeyMismatch", err)
	}
}

// TestAReplayedProofIsRefused: the same proof presented twice is refused, which is Dispatch's
// replay store doing its work — the second presentation asks the broker nothing, so only the
// store can have refused it.
func TestAReplayedProofIsRefused(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-whose-proof-is-captured"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))
	signed := caller.at(resourcePath, token)

	if _, err := rig.verifier.Verify(t.Context(), signed); err != nil {
		t.Fatalf("the first presentation: %v", err)
	}
	_, err := rig.verifier.Verify(t.Context(), signed)
	if !errors.Is(err, ciphertoken.ErrProofInvalid) {
		t.Fatalf("the replayed proof answered %v, want ErrProofInvalid", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the replay cost %d introspections in all, want 1: it was refused by the store", rig.broker.Count())
	}
}

// TestTheProofIsCheckedAgainstDispatchsOwnOriginNotAForwardedHeader: the library rebuilds the
// expected 'htu' from X-Forwarded-Proto and the Host, both of which a caller behind a proxy can
// choose, so Dispatch checks against DISPATCH_SERVER_URL plus the request's path instead. A
// proof naming that origin is accepted however the request is addressed, and one naming an
// origin the caller picked is refused.
func TestTheProofIsCheckedAgainstDispatchsOwnOriginNotAForwardedHeader(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token = "a-token-presented-through-a-proxy"
	rig.broker.Says(token, liveToken(caller.jkt, time.Hour))

	sound := caller.at(resourcePath, token)
	sound.Host = "dispatch.internal:8080"
	sound.Header.Set("X-Forwarded-Proto", "http")
	sound.Header.Set("X-Forwarded-Host", "whatever.the.caller.says")
	if _, err := rig.verifier.Verify(t.Context(), sound); err != nil {
		t.Fatalf("a proof naming Dispatch's own origin was refused behind a proxy: %v", err)
	}

	chosen := caller.request("http://whatever.the.caller.says"+resourcePath, token)
	chosen.Host = "whatever.the.caller.says"
	chosen.Header.Set("X-Forwarded-Proto", "http")
	_, err := rig.verifier.Verify(t.Context(), chosen)
	if !errors.Is(err, ciphertoken.ErrProofInvalid) {
		t.Fatalf("a proof naming the caller's own origin answered %v, want ErrProofInvalid", err)
	}
}

// TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker: the address bucket is
// what stops a stranger spending the budget a person's approval needs (INFRA-228), so K requests
// bearing K tokens the broker has never issued cost fewer than K introspections. With the bound
// lifted and nothing else changed they cost K, which is what says the bucket did it.
func TestAFloodOfDistinctUnknownTokensFromOneAddressIsBoundedBeforeTheBroker(t *testing.T) {
	const flood = 40
	spend := func(t *testing.T, rig *rig) int64 {
		caller := newSigner(t)
		for request := range flood {
			token := fmt.Sprintf("a-token-the-broker-never-issued-%d", request)
			if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token)); err == nil {
				t.Fatalf("request %d was accepted; want a refusal", request)
			}
		}
		return rig.broker.Count()
	}

	bounded := spend(t, newRig(t))
	if bounded >= flood || bounded < int64(ciphertoken.DefaultAddressLimit.Burst) {
		t.Fatalf("%d requests from one address cost %d introspections, want the bucket's burst of %d and fewer than %d",
			flood, bounded, ciphertoken.DefaultAddressLimit.Burst, flood)
	}
	// The negative control: the same flood with the bucket wide enough to admit all of it.
	unbounded := spend(t, newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Nanosecond, Burst: flood}
	}))
	if unbounded != flood {
		t.Fatalf("with the bound lifted, %d requests cost %d introspections, want %d", flood, unbounded, flood)
	}
}

// TestARefusedAddressIsNeverTheBrokersVerdict: the refusal a spent bucket gives is its own
// outcome, so a caller is told to come back rather than told its token is bad.
func TestARefusedAddressIsNeverTheBrokersVerdict(t *testing.T) {
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newSigner(t)
	rig.broker.Says("the-first-token", liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "the-first-token")); err != nil {
		t.Fatalf("the request that spends the bucket: %v", err)
	}

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-second-token"))
	if !errors.Is(err, ciphertoken.ErrRateLimited) {
		t.Fatalf("the request with no token left in the bucket answered %v, want ErrRateLimited", err)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("the refused request reached the broker: %d introspections, want 1", rig.broker.Count())
	}
	if rig.verifier.RetryAfter() != time.Minute {
		t.Fatalf("RetryAfter = %v, want the bucket's minute", rig.verifier.RetryAfter())
	}
}

// TestABrokerThatCannotBeReachedIsTheUnreachableOutcome: a caller whose broker is away must be
// asked to come back, not told its token is refused, or it would discard a token that is live.
func TestABrokerThatCannotBeReachedIsTheUnreachableOutcome(t *testing.T) {
	// A broker at a port the test holds and nothing listens on.
	url := "http://" + testport.Refused(t)
	rig := newRig(t, func(o *ciphertoken.Options) { o.BrokerURL = url })
	caller := newSigner(t)

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-token-nobody-can-be-asked-about"))
	if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) {
		t.Fatalf("a broker that cannot be reached answered %v, want ErrBrokerUnreachable", err)
	}
	if errors.Is(err, ciphertoken.ErrNotActive) {
		t.Fatalf("a broker that cannot be reached refused the token: %v", err)
	}
}

// TestAnAccountTokenDispatchCannotReadAsksTheBrokerNothing: Dispatch's own missing credential is
// its deployment's failure, so it is the unreachable outcome and the error names the file.
func TestAnAccountTokenDispatchCannotReadAsksTheBrokerNothing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nothing-here")
	rig := newRig(t, func(o *ciphertoken.Options) { o.Account = ciphertoken.NewAccountToken(missing) })
	caller := newSigner(t)

	_, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "a-token-never-asked-about"))
	if !errors.Is(err, ciphertoken.ErrBrokerUnreachable) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("an unreadable account token answered %v, want ErrBrokerUnreachable naming %s", err, missing)
	}
	var unreadable *ciphertoken.AccountTokenError
	if !errors.As(err, &unreadable) {
		t.Fatalf("err = %v, want an *AccountTokenError in the chain", err)
	}
	if rig.broker.Count() != 0 {
		t.Fatalf("the broker was asked %d times, want not at all", rig.broker.Count())
	}
}

// TestARequestPresentingNoDPoPTokenIsNotThisCredential: a request carrying another credential —
// a bearer token, or a browser's session cookie and no Authorization header at all — belongs to
// another check, and this one says so rather than refusing it.
//
// It says so without reading the request's body, which whoever mounts this check still has to
// hand to a handler. RFC 6750 puts an access token in a form parameter as well as a header, so a
// reading that looks there parses the form, and the body of a multipart upload — which is how
// Dispatch's document writes arrive, under a cookie — is gone before its handler sees it.
func TestARequestPresentingNoDPoPTokenIsNotThisCredential(t *testing.T) {
	rig := newRig(t)
	var body bytes.Buffer
	upload := multipart.NewWriter(&body)
	part, err := upload.CreateFormFile("file", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# a document the handler still needs\n")); err != nil {
		t.Fatal(err)
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	sent := body.String()

	for name, authorization := range map[string]string{
		"a bearer token":          "Bea" + "rer a-token-of-another-kind",
		"a browser's own cookie":  "",
		"a scheme nothing mounts": "Negotiate something-else-entirely",
	} {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, dispatchOrigin+resourcePath, strings.NewReader(sent))
			if err != nil {
				t.Fatal(err)
			}
			if authorization != "" {
				request.Header.Set("Authorization", authorization)
			}
			request.Header.Set("Content-Type", upload.FormDataContentType())
			if _, ok := ciphertoken.Presented(request); ok {
				t.Fatal("Presented read another credential as this one")
			}
			if _, err := rig.verifier.Verify(context.Background(), request); !errors.Is(err, ciphertoken.ErrNotPresented) {
				t.Fatalf("the request answered %v, want ErrNotPresented", err)
			}
			read, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatalf("read the body back: %v", err)
			}
			if string(read) != sent {
				t.Fatalf("the body reads back as %d of its %d bytes, want all of them: deciding the credential consumed it", len(read), len(sent))
			}
		})
	}
}

// TestNewRefusesAnOriginItCannotCheckAProofAgainst: an origin that is not an absolute http(s)
// origin is a deployment error, and it fails the build of the verifier rather than every
// request.
func TestNewRefusesAnOriginItCannotCheckAProofAgainst(t *testing.T) {
	database := storetest.Open(t)
	for name, origin := range map[string]string{
		"empty":       "",
		"no scheme":   "dispatch.test",
		"not http":    "ftp://dispatch.test",
		"with a path": "https://dispatch.test/dispatch",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ciphertoken.New(ciphertoken.Options{
				BrokerURL: "https://broker.test",
				Account:   ciphertoken.NewAccountToken(accountTokenFile(t, accountToken)),
				Origin:    origin,
				Replay:    store.NewDPoPProofs(database.Pool),
				Address:   sourceAddress,
			})
			if err == nil {
				t.Fatalf("New accepted the origin %q", origin)
			}
		})
	}
}

// TestNewRefusesAVerifierWithNoSourceAddress: which address an introspection bucket is keyed on
// is the deployment's rule and this package has none of its own, so a Verifier built without
// one is refused at the build rather than left bounding the broker by some default nobody
// chose. The negative control is the same options with the function supplied.
func TestNewRefusesAVerifierWithNoSourceAddress(t *testing.T) {
	database := storetest.Open(t)
	options := func() ciphertoken.Options {
		return ciphertoken.Options{
			BrokerURL: "https://broker.test",
			Account:   ciphertoken.NewAccountToken(accountTokenFile(t, accountToken)),
			Origin:    dispatchOrigin,
			Replay:    store.NewDPoPProofs(database.Pool),
		}
	}
	if _, err := ciphertoken.New(options()); err == nil {
		t.Fatal("New built a verifier with no source address function")
	}
	supplied := options()
	supplied.Address = sourceAddress
	if _, err := ciphertoken.New(supplied); err != nil {
		t.Fatalf("New refused the same options with the address function supplied: %v", err)
	}
}

// TestLiveUntilAnswersWhenTheTokenStopsBeingLive: a connection that outlives its request tells a
// token whose life ended from one the broker ended early by the instant the broker said it would
// end, since both answer ErrNotActive once they are over. So LiveUntil hands that instant back beside
// the caller: the answer's 'exp', to the second the broker writes it, and the zero time for an answer
// that names none. A token the broker does not hold live answers ErrNotActive and no instant, as
// Live does.
func TestLiveUntilAnswersWhenTheTokenStopsBeingLive(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const (
		dated   = "a-token-the-broker-dated"
		undated = "a-token-the-broker-named-no-expiry-for"
		unknown = "a-token-the-broker-never-issued"
	)
	answer := liveToken(caller.jkt, time.Hour)
	rig.broker.Says(dated, answer)
	decided, until, err := rig.verifier.LiveUntil(t.Context(), caller.at(resourcePath, dated))
	if err != nil {
		t.Fatalf("LiveUntil of a live token: %v", err)
	}
	if want := time.Unix(answer.ExpiresAt, 0); !until.Equal(want) {
		t.Fatalf("LiveUntil answered the expiry %v, want the answer's exp %v", until, want)
	}
	if decided.Session != answer.Session || decided.Person != answer.Person {
		t.Fatalf("LiveUntil = %+v, want the caller Live answers for the same token", decided)
	}

	noExpiry := liveToken(caller.jkt, time.Hour)
	noExpiry.ExpiresAt = 0
	rig.broker.Says(undated, noExpiry)
	if _, until, err := rig.verifier.LiveUntil(t.Context(), caller.at(resourcePath, undated)); err != nil || !until.IsZero() {
		t.Fatalf("LiveUntil of a token with no exp = %v, %v; want the zero time and no error", until, err)
	}

	if decided, until, err := rig.verifier.LiveUntil(t.Context(), caller.at(resourcePath, unknown)); !errors.Is(err, ciphertoken.ErrNotActive) || !until.IsZero() || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
		t.Fatalf("LiveUntil of a token the broker does not hold = %+v, %v, %v; want no caller, the zero time and ErrNotActive", decided, until, err)
	}
}

// TestLiveUntilSpendsNoAddressBucket: LiveUntil is the recheck of a connection whose opening
// request Verify already admitted with its proof (the listener's relay socket), so it introspects
// with the source address's bucket spent, where Live and Verify are refused ErrRateLimited: a
// stranger who empties the bucket of an address, which every client behind one load balancer
// shares, cannot close the sockets opened from it. Control: Live, with the same bucket spent, is
// refused ErrRateLimited and reaches no broker.
func TestLiveUntilSpendsNoAddressBucket(t *testing.T) {
	rig := newRig(t, func(o *ciphertoken.Options) {
		o.AddressLimit = ratelimit.Limit{Every: time.Minute, Burst: 1}
	})
	caller := newSigner(t)
	rig.broker.Says("the-token-that-spends-the-bucket", liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, "the-token-that-spends-the-bucket")); err != nil {
		t.Fatalf("the request that spends the bucket: %v", err)
	}
	rig.broker.Says("a-socket-token", liveToken(caller.jkt, time.Hour))
	if _, _, err := rig.verifier.LiveUntil(t.Context(), caller.at(resourcePath, "a-socket-token")); err != nil {
		t.Fatalf("LiveUntil with the address's bucket spent = %v, want the broker's answer", err)
	}
	if rig.broker.Count() != 2 {
		t.Fatalf("LiveUntil cost %d introspections in all, want 2", rig.broker.Count())
	}
	rig.broker.Says("a-websocket-token", liveToken(caller.jkt, time.Hour))
	if _, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, "a-websocket-token")); !errors.Is(err, ciphertoken.ErrRateLimited) {
		t.Fatalf("Live with the address's bucket spent = %v, want ErrRateLimited", err)
	}
}
