package kubernetes_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/kubernetes"
)

const (
	workerAccount = "system:serviceaccount:example-ns:example-worker"
	daemonAccount = "system:serviceaccount:example-ns:example-daemon"
	// readerAccount is the account the read entry lists, readerService the service its token
	// names and readerSession the session it fixes.
	readerAccount = "system:serviceaccount:example-ns:example-reader"
	readerService = "example-reader"
	readerSession = "example-reader-session"
	project       = "ACME"
	person        = "ada@example.com"
	// fixedSession is the session name the daemon account's entry fixes.
	fixedSession = "legion-daemon:ACME"
	podUID       = "11111111-2222-3333-4444-555555555555"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	workerPrincipal = "account:" + workerAccount
	daemonPrincipal = "account:" + daemonAccount
	readerPrincipal = "account:" + readerAccount
)

// listed is the provider every decision test asks: one account whose sessions name themselves,
// one account whose session the entry fixes, and one read entry.
func listed(t *testing.T) *kubernetes.Provider {
	t.Helper()
	p, err := kubernetes.New(kubernetes.Config{
		Accounts: []exchange.AccountEntry{
			{Account: workerAccount, Project: project, Person: person},
			{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
		},
		Reads: []exchange.ReadEntry{{Account: readerAccount, Service: readerService, Session: readerSession}},
	})
	if err != nil {
		t.Fatalf("kubernetes.New: %v", err)
	}
	return p
}

// build is the provider of accounts and reads composed into a registry, which is where a session
// name two entries fix is refused.
func build(accounts []exchange.AccountEntry, reads []exchange.ReadEntry) (*identity.Registry, error) {
	p, err := kubernetes.New(kubernetes.Config{Accounts: accounts, Reads: reads})
	if err != nil {
		return nil, err
	}
	return identity.NewRegistry([]identity.Provider{p}, nil)
}

// decide is a pod's trade of token naming session and projects.
func decide(p *kubernetes.Provider, token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return p.Decide(identity.Trade{TokenType: kubernetes.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

// podToken is a projected service-account token's claims as the exchange is handed them, bound to
// podUID unless uid is empty.
func podToken(account, uid string) map[string]any {
	token := map[string]any{"sub": account}
	if uid != "" {
		token["kubernetes.io"] = map[string]any{"pod": map[string]any{"uid": uid}}
	}
	return token
}

func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	account := exchange.AccountEntry{Account: workerAccount, Project: project, Person: person}
	with := func(change func(*exchange.AccountEntry)) exchange.AccountEntry {
		entry := account
		change(&entry)
		return entry
	}
	reader := exchange.ReadEntry{Account: readerAccount, Service: readerService, Session: readerSession}
	withReader := func(change func(*exchange.ReadEntry)) exchange.ReadEntry {
		entry := reader
		change(&entry)
		return entry
	}
	for _, c := range []struct {
		name     string
		accounts []exchange.AccountEntry
		reads    []exchange.ReadEntry
		names    string
	}{
		{name: "an account that is not a service account subject",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "example-worker" })}, names: "example-worker"},
		{name: "an account with no namespace",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "system:serviceaccount:example-worker" })}, names: "system:serviceaccount:example-worker"},
		{name: "an empty account",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "" })}, names: "account entry 1"},
		{name: "an account with no project",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Project = "  " })}, names: workerAccount},
		{name: "an account with no person",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Person = "" })}, names: workerAccount},
		{name: "an account whose fixed session carries whitespace",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = "legion daemon" })}, names: workerAccount},
		{name: "an account whose fixed session is too long",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = strings.Repeat("s", exchange.MaxSessionBytes+1) })}, names: workerAccount},
		{name: "an account whose fixed session is a wildcard",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = "*" })}, names: workerAccount},
		{name: "the same account twice",
			accounts: []exchange.AccountEntry{account, with(func(e *exchange.AccountEntry) { e.Project = "OTHER" })}, names: "account entry 2"},
		{name: "a session name two accounts fix",
			accounts: []exchange.AccountEntry{
				with(func(e *exchange.AccountEntry) { e.Session = fixedSession }),
				{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
			}, names: "account entry 1"},
		{name: "a read entry whose account is not a service account subject",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Account = "example-reader" })}, names: `read entry 1, "example-reader"`},
		{name: "a read entry with no account",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Account = "" })}, names: "read entry 1"},
		{name: "a read entry with a blank service",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Service = " " })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "a read entry with no service",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Service = "" })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "a read entry with no session, which it must fix",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Session = "" })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "a read entry whose session carries whitespace",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Session = "example reader" })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "a read entry whose session is too long",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Session = strings.Repeat("s", exchange.MaxSessionBytes+1) })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "a read entry whose session holds a wildcard",
			reads: []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Session = "example-reader.>" })}, names: `read entry 1, "` + readerAccount + `"`},
		{name: "the same read account twice",
			reads: []exchange.ReadEntry{reader, withReader(func(e *exchange.ReadEntry) { e.Session = "example-reader-other" })}, names: `read entry 2, "` + readerAccount + `"`},
		{name: "an account an account entry and a read entry both list",
			accounts: []exchange.AccountEntry{account},
			reads:    []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Account = workerAccount })}, names: `read entry 1, "` + workerAccount + `"`},
		{name: "a session name a read entry and an account fix",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = readerSession })},
			reads:    []exchange.ReadEntry{reader}, names: `read entry 1, "` + readerAccount + `"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := build(c.accounts, c.reads)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestAnAccountEntryWithNoProjectNamesNone: an account entry may name a person and no project,
// which is the shared public-session account's entry. Its pods' tokens are named, act for the
// entry's person, and name no project; a request naming one is refused as any entry's is; and
// MappedProjects answers the same empty list. A blank project that is not empty is still refused.
func TestAnAccountEntryWithNoProjectNamesNone(t *testing.T) {
	p, err := kubernetes.New(kubernetes.Config{Accounts: []exchange.AccountEntry{{Account: workerAccount, Person: person}}})
	if err != nil {
		t.Fatalf("NewSet with an account entry naming no project = %v, want it taken", err)
	}
	claims, err := decide(p, podToken(workerAccount, podUID), "public-session-1", nil)
	want := exchange.Claims{Session: "public-session-1", Person: person, Scope: exchange.ProjectScopeNamed, Principal: workerPrincipal, Instance: podUID}
	if err != nil || !reflect.DeepEqual(claims, want) {
		t.Fatalf("Pod under an entry naming no project = %+v, %v; want %+v: named, its person, no project", claims, err, want)
	}
	if _, err := decide(p, podToken(workerAccount, podUID), "public-session-1", []string{project}); !errors.Is(err, exchange.ErrProjectsFixed) {
		t.Fatalf("a request naming %s under an entry naming none = %v, want ErrProjectsFixed", project, err)
	}
	registry, err := identity.NewRegistry([]identity.Provider{p}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if gotPerson, projects, ok := registry.Policy().MappedProjects(workerPrincipal); gotPerson != person || len(projects) != 0 || !ok {
		t.Fatalf("MappedProjects = %q, %v, %v; want %s, no project, true", gotPerson, projects, ok, person)
	}
	if _, err := kubernetes.New(kubernetes.Config{Accounts: []exchange.AccountEntry{{Account: workerAccount, Project: "  ", Person: person}}}); err == nil {
		t.Fatal("NewSet with an account entry whose project is blank but not empty = nil error, want a refusal")
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: every session name an entry fixes is reserved for
// that entry's principal, in the order the entries were listed, and a name no entry fixes is not
// reserved at all.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	registry, err := identity.NewRegistry([]identity.Provider{listed(t)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []exchange.Reservation{
		{Session: fixedSession, Principal: daemonPrincipal},
		{Session: readerSession, Principal: readerPrincipal},
	}
	if got := registry.Policy().Reservations(); !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
	}
}

// TestPodAnswersTheListedAccount pins what each kind of entry answers, Instance included: an
// entry that fixes no session answers the pod uid, which a registration has to name for the
// exchange to go through, and an entry that fixes one answers none, since every pod of that
// account writes under the configuration's name and is registered for nothing. A read entry
// always fixes one, and answers its service and session, the project scope that writes nothing,
// and no person and no project.
func TestPodAnswersTheListedAccount(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: podToken(workerAccount, podUID), session: "implementer-g1",
			want: exchange.Claims{Session: "implementer-g1", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: workerPrincipal, Instance: podUID}},
		{name: "the session the entry fixes, the request naming none", token: podToken(daemonAccount, podUID), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: daemonPrincipal}},
		{name: "the session the entry fixes, the request naming the same", token: podToken(daemonAccount, podUID), session: fixedSession,
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: daemonPrincipal}},
		{name: "another pod of the account is the same principal", token: podToken(daemonAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: daemonPrincipal}},
		{name: "a read entry's session, the request naming none", token: podToken(readerAccount, podUID), session: "",
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
		{name: "a read entry's session, the request naming the same", token: podToken(readerAccount, podUID), session: readerSession,
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
		{name: "another pod of the read account is the same principal", token: podToken(readerAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := decide(set, c.token, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Pod(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestPodRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no subject", token: map[string]any{}, session: "implementer-g1", want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a subject that is not a string", token: map[string]any{"sub": 7}, session: "implementer-g1", want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a token bound to no pod", token: podToken(workerAccount, ""), session: "implementer-g1", want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "a pod binding with no uid", token: map[string]any{"sub": workerAccount, "kubernetes.io": map[string]any{"pod": map[string]any{}}},
			session: "implementer-g1", want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "an account no entry lists", token: podToken("system:serviceaccount:example-ns:example-other", podUID),
			session: "implementer-g1", want: exchange.ErrNoEntry, names: "system:serviceaccount:example-ns:example-other"},
		{name: "a session the entry does not fix", token: podToken(daemonAccount, podUID), session: "implementer-g1",
			want: exchange.ErrSessionMismatch, names: fixedSession},
		{name: "a session the read entry does not fix", token: podToken(readerAccount, podUID), session: "implementer-g1",
			want: exchange.ErrSessionMismatch, names: readerSession},
		{name: "a read account's token bound to no pod", token: podToken(readerAccount, ""), session: "",
			want: exchange.ErrNoPodBinding, names: readerAccount},
		{name: "no session where the entry fixes none", token: podToken(workerAccount, podUID), session: "",
			want: exchange.ErrSessionRequired, names: workerAccount},
		{name: "a session carrying whitespace", token: podToken(workerAccount, podUID), session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "a session longer than the cap", token: podToken(workerAccount, podUID), session: strings.Repeat("s", exchange.MaxSessionBytes+1),
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := decide(set, c.token, c.session, nil)
			if !errors.Is(err, c.want) {
				t.Fatalf("Pod(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Pod(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestRegistrationRefusesWhatNeedsNoRegistration pins what a pod-session registration may be
// written for: the pod of an entry that fixes no session, and nothing else. A token bound to no
// pod proves only that the account exists, an account no entry lists exchanges nothing to begin
// with, and an entry that fixes a session writes under the configuration's name, so registering
// one of its pods would say something no exchange reads. A read entry always fixes its session,
// so its account is refused as such an entry is, 409 SESSION_FIXED at the route, never as an
// account no entry lists.
func TestRegistrationRefusesWhatNeedsNoRegistration(t *testing.T) {
	set := listed(t)
	if err := set.Registration(workerAccount, podUID, "implementer-g1"); err != nil {
		t.Fatalf("Registration of a pod of %s for a name of its own = %v, want it accepted", workerAccount, err)
	}
	for _, c := range []struct {
		name         string
		account, uid string
		session      string
		want         error
		names        string
	}{
		{name: "a token bound to no pod", account: workerAccount, uid: "", session: "implementer-g1",
			want: exchange.ErrNoPodBinding, names: workerAccount},
		{name: "an account no entry lists", account: "system:serviceaccount:example-ns:example-other", uid: podUID,
			session: "implementer-g1", want: exchange.ErrNoEntry, names: "system:serviceaccount:example-ns:example-other"},
		{name: "an entry that fixes a session", account: daemonAccount, uid: podUID, session: fixedSession,
			want: exchange.ErrSessionFixed, names: fixedSession},
		{name: "a read entry, which fixes its session", account: readerAccount, uid: podUID, session: readerSession,
			want: exchange.ErrSessionFixed, names: readerSession},
		{name: "a read entry asked for another name", account: readerAccount, uid: podUID, session: "implementer-g1",
			want: exchange.ErrSessionFixed, names: readerAccount},
		{name: "a session carrying whitespace", account: workerAccount, uid: podUID, session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "no session at all", account: workerAccount, uid: podUID, session: "",
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := set.Registration(c.account, c.uid, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Registration(%s) = %v; want %v", c.name, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Registration(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestPodNeverRepeatsAnUnusableSessionName(t *testing.T) {
	set := listed(t)
	const unusable = "implementer\x00g1"
	_, err := decide(set, podToken(workerAccount, podUID), unusable, nil)
	if !errors.Is(err, exchange.ErrSessionInvalid) {
		t.Fatalf("Pod(an unprintable session) = %v, want ErrSessionInvalid", err)
	}
	if strings.Contains(err.Error(), unusable) {
		t.Errorf("Pod refused with %q, which repeats what the request sent", err)
	}
}

// TestEveryDecisionCarriesItsProjects: a pod of an account entry carries its entry's one project
// under the named scope, and a read entry's pod says that it writes nothing and carries no
// project.
func TestEveryDecisionCarriesItsProjects(t *testing.T) {
	set := listed(t)
	pod, err := decide(set, podToken(workerAccount, podUID), "implementer-g1", nil)
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	read, err := decide(set, podToken(readerAccount, podUID), "", nil)
	if err != nil {
		t.Fatalf("Pod of the read account: %v", err)
	}
	for _, c := range []struct {
		name     string
		claims   exchange.Claims
		scope    exchange.ProjectScope
		projects []string
	}{
		{name: "Pod", claims: pod, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Pod of the read account", claims: read, scope: exchange.ProjectScopeNone, projects: nil},
	} {
		if c.claims.Scope != c.scope || !slices.Equal(c.claims.Projects, c.projects) {
			t.Errorf("%s = scope %v, projects %v; want scope %v, projects %v", c.name, c.claims.Scope, c.claims.Projects, c.scope, c.projects)
		}
	}
}

func TestSessionAtTheCapIsUsable(t *testing.T) {
	set := listed(t)
	name := strings.Repeat("s", exchange.MaxSessionBytes)
	claims, err := decide(set, podToken(workerAccount, podUID), name, nil)
	if err != nil || claims.Session != name {
		t.Fatalf("Pod(a session of exactly %d bytes) = %+v, %v; want it answered", exchange.MaxSessionBytes, claims, err)
	}
}
