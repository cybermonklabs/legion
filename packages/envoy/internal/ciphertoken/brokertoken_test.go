package ciphertoken_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

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
	// proof's 'htu' against. A verifier of this origin alone checks every request against it,
	// whatever host the request reaches Dispatch under; a verifier of two origins (twoOrigins)
	// checks against it every request whose host is not outsideOrigin's.
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
		Origins:   []string{dispatchOrigin},
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
// person but comes from
// another client, and the six whose project scope or list this package cannot read — an answer
// carrying no project scope at all, one carrying a word it does not know, a named scope with no
// project and no person, a named scope with two projects, and a list no exchange could have
// minted. Those are refused rather than read as a token that names no project, which would hand
// their caller every project the person may write; the one naming a project under no scope would
// otherwise be the louder of the two, since the project beside the scope is then read under
// nothing at all. The proof-checked Verify and the answer-only Live refuse every one, so an open
// connection cannot keep one alive either.
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
	namedNoOne := liveToken(caller.jkt, time.Hour)
	namedNoOne.Person, namedNoOne.Service, namedNoOne.Projects = "", "example-fleet", nil
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
		"a named scope naming no project and no person": namedNoOne,
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
			if !reflect.DeepEqual(live, ciphertoken.Caller{}) {
				t.Fatalf("Live = %+v, want no caller at all", live)
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
// one project, or none for an account entry naming none, and always a person. An answer of that
// scope naming two or more projects, or none and no person, is no token the exchange minted and is
// refused, so a Dispatch reader never takes it as a caller no project limits.
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
	if decided, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token)); !errors.Is(err, ciphertoken.ErrNotASession) || !reflect.DeepEqual(decided, ciphertoken.Caller{}) {
		t.Fatalf("Live of an older broker's named answer = %+v, %v; want no caller and ErrNotASession", decided, err)
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
			return rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
		},
	} {
		decided, err := decide()
		if err != nil || !reflect.DeepEqual(decided, want) {
			t.Errorf("%s = %+v, %v; want %+v", name, decided, err, want)
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
	if err != nil || !reflect.DeepEqual(live, want) {
		t.Fatalf("Live = %+v, %v; want %+v", live, err, want)
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

// TestTheAccountATokenRecordsReachesBothChecksFromTheCacheToo: the Google account the broker
// answers a token's credential records is part of the caller both checks answer, and an answer
// remembered in the cache keeps it, so the requests inside AffirmativeCacheTTL and an open
// connection's heartbeat read the same account the first request did. A token whose credential
// records none answers none, and the time its credential was made (credential_created_at, Unix
// seconds), which a resource server holds such a token to.
func TestTheAccountATokenRecordsReachesBothChecksFromTheCacheToo(t *testing.T) {
	rig := newRig(t)
	caller := newSigner(t)
	const token, unrecorded = "a-token-whose-sign-in-records-its-account", "a-token-from-before-the-broker-recorded-accounts"
	answer := liveToken(caller.jkt, time.Hour)
	answer.AccountIssuer, answer.AccountSubject = "https://accounts.google.com", "109sjawhar"
	rig.broker.Says(token, answer)
	older := liveToken(caller.jkt, time.Hour)
	older.CredentialCreatedAt = 1794900000
	rig.broker.Says(unrecorded, older)

	want := ciphertoken.Caller{
		Session: "ses_a1b2c3", Person: "sjawhar", Projects: []string{"INFRA"}, ProjectScope: ciphertoken.ProjectScopeNamed,
		AccountIssuer: "https://accounts.google.com", AccountSubject: "109sjawhar",
	}
	for request := range 2 {
		decided, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, token))
		if err != nil || !reflect.DeepEqual(decided, want) {
			t.Fatalf("Verify %d = %+v, %v; want %+v", request, decided, err, want)
		}
	}
	live, err := rig.verifier.Live(t.Context(), caller.at(resourcePath, token))
	if err != nil || !reflect.DeepEqual(live, want) {
		t.Fatalf("Live = %+v, %v; want %+v", live, err, want)
	}
	if rig.broker.Count() != 1 {
		t.Fatalf("two requests and a heartbeat cost %d introspections, want 1: the later two read the cache", rig.broker.Count())
	}

	none, err := rig.verifier.Verify(t.Context(), caller.at(resourcePath, unrecorded))
	if err != nil || none.AccountIssuer != "" || none.AccountSubject != "" || !none.CredentialCreatedAt.Equal(time.Unix(1794900000, 0)) {
		t.Fatalf("Verify of a token recording no account = %+v, %v; want a caller naming no account, its credential made at %s", none, err, time.Unix(1794900000, 0).UTC())
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

// TestNewRefusesAnOriginItCannotCheckAProofAgainst: no origin, an origin that is not an absolute
// http(s) origin, and two origins with one Host (host and port), which no request's Host could
// tell apart, are deployment errors, and each fails the build of the verifier rather than every
// request. The control: two origins on two hosts build.
func TestNewRefusesAnOriginItCannotCheckAProofAgainst(t *testing.T) {
	database := storetest.Open(t)
	build := func(t *testing.T, origins []string) error {
		_, err := ciphertoken.New(ciphertoken.Options{
			BrokerURL: "https://broker.test",
			Account:   ciphertoken.NewAccountToken(accountTokenFile(t, accountToken)),
			Origins:   origins,
			Replay:    store.NewDPoPProofs(database.Pool),
			Address:   sourceAddress,
		})
		return err
	}
	for name, origins := range map[string][]string{
		"none":                    nil,
		"empty":                   {""},
		"no scheme":               {"dispatch.test"},
		"not http":                {"ftp://dispatch.test"},
		"with a path":             {"https://dispatch.test/dispatch"},
		"a second with a path":    {dispatchOrigin, "https://outside.test/dispatch"},
		"a second with no scheme": {dispatchOrigin, "outside.test"},
		"two on one host":         {dispatchOrigin, "http://DISPATCH.test"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := build(t, origins); err == nil {
				t.Fatalf("New accepted the origins %q", origins)
			}
		})
	}
	if err := build(t, []string{dispatchOrigin, outsideOrigin}); err != nil {
		t.Fatalf("New refused two origins on two hosts: %v", err)
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
			Origins:   []string{dispatchOrigin},
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
