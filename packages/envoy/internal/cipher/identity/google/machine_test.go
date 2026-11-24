package google_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
)

const (
	project = "ACME"
	person  = "ada@example.com"
	// machineSession is the session name the fixed fleet's entry fixes.
	machineSession = "example-fleet-1"
	// The three Google accounts the provider lists, each a placeholder: pinnedAccount is held to
	// one Compute Engine project and one instance-name pattern and writes for a person,
	// poolAccount is pinned to nothing and names no person, so its machines are its service and
	// nothing else, and fixedAccount is pinned to nothing and fixes the session its machines write
	// under. strangerAccount is listed nowhere.
	pinnedAccount   = "example-fleet@example-project.iam.gserviceaccount.com"
	poolAccount     = "example-pool@example-project.iam.gserviceaccount.com"
	fixedAccount    = "example-fixed@example-project.iam.gserviceaccount.com"
	strangerAccount = "example-stranger@example-project.iam.gserviceaccount.com"
	cloudProject    = "example-project"
	instanceGlob    = "example-vm-*"
	instanceName    = "example-vm-7"
	pinnedService   = "example-fleet"
	poolService     = "example-pool"
	// signInDomain is the Google Workspace domain BROKER_SIGNIN_DOMAIN names in these tests, the
	// one a person signing in on a machine must belong to; otherDomain is any other Workspace,
	// and stranger a person of it.
	signInDomain = "example.com"
	otherDomain  = "other.example"
	stranger     = "mallory@other.example"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	personPrincipal = "person:" + person
	pinnedPrincipal = "google:" + pinnedAccount
	poolPrincipal   = "google:" + poolAccount
	fixedPrincipal  = "google:" + fixedAccount
)

// machines are the three Google entries every decision test asks: one pinned to a project and an
// instance-name pattern that writes for a person, one pinned to nothing that names no person, and
// one that fixes the session its machines write under.
var machines = []google.GoogleEntry{
	{Account: pinnedAccount, Service: pinnedService, CloudProject: cloudProject, Instance: instanceGlob, Project: project, Person: person},
	{Account: poolAccount, Service: poolService},
	{Account: fixedAccount, Service: poolService, Session: machineSession},
}

func listed(t *testing.T) *google.Provider {
	t.Helper()
	p, err := google.New(google.Config{Machines: machines, SignInDomain: signInDomain})
	if err != nil {
		t.Fatalf("google.New: %v", err)
	}
	return p
}

// build is the provider of machines composed into a registry, which is where a session name two
// entries fix is refused.
func build(entries []google.GoogleEntry) (*identity.Registry, error) {
	p, err := google.New(google.Config{Machines: entries, SignInDomain: signInDomain})
	if err != nil {
		return nil, err
	}
	return identity.NewRegistry([]identity.Provider{p}, nil)
}

// machine is a machine's trade of token naming session and projects; signIn is a person's trade
// of token with actor as the machine acting for them.
func machine(p *google.Provider, token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return p.Decide(identity.Trade{TokenType: google.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

func signIn(p *google.Provider, token, actor map[string]any, session string, projects []string) (exchange.Claims, error) {
	return p.Decide(identity.Trade{TokenType: google.PersonTokenType, Subject: token, Actor: actor, Session: session, Projects: projects})
}

// googleToken is a Google identity token's claims as the exchange is handed them: the account it
// was minted for, whether Google holds the address verified, and the Compute Engine instance it
// was minted on — none when instance is empty, which is what a token minted through a workload
// identity pool and a pod's own metadata token carry.
func googleToken(account string, verified any, cloud, instance string) map[string]any {
	token := map[string]any{"email": account, "email_verified": verified}
	if instance != "" {
		token["google"] = map[string]any{"compute_engine": map[string]any{
			"project_id":    cloud,
			"zone":          "example-zone-a",
			"instance_name": instance,
			"instance_id":   "1234567890",
		}}
	}
	return token
}

// personToken is a person's own Google identity token's claims as the exchange is handed them:
// the subject Google gives the account, the address it was minted for, whether Google holds that
// address verified, and the Workspace domain in `hd`.
func personToken(login string, verified any, domain string) map[string]any {
	token := map[string]any{"sub": "109" + login, "email": login, "email_verified": verified}
	if domain != "" {
		token["hd"] = domain
	}
	return token
}

func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	entry := google.GoogleEntry{Account: pinnedAccount, Service: pinnedService}
	withMachine := func(change func(*google.GoogleEntry)) google.GoogleEntry {
		e := entry
		change(&e)
		return e
	}
	for _, c := range []struct {
		name     string
		machines []google.GoogleEntry
		names    string
	}{
		{name: "a Google account that is not an email address",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Account = "example-fleet" })}, names: "example-fleet"},
		{name: "a Google account written with capitals, which no token carries",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Account = "Example-Fleet@example-project.iam.gserviceaccount.com" })},
			names:    "google entry 1"},
		{name: "a Google account with no service",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Service = " " })}, names: pinnedAccount},
		{name: "a machine pinned to a project and no instance pattern",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.CloudProject = cloudProject })}, names: pinnedAccount},
		{name: "a machine pinned to an instance pattern and no project",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Instance = instanceGlob })}, names: pinnedAccount},
		{name: "a machine whose instance pattern is not a pattern",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.CloudProject, e.Instance = cloudProject, "example-[vm" })},
			names:    "example-[vm"},
		{name: "a machine named a person and no project",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Person = person })}, names: pinnedAccount},
		{name: "a machine named a project and no person",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Project = project })}, names: pinnedAccount},
		{name: "a machine whose fixed session carries whitespace",
			machines: []google.GoogleEntry{withMachine(func(e *google.GoogleEntry) { e.Session = "example fleet" })}, names: pinnedAccount},
		{name: "the same Google account twice",
			machines: []google.GoogleEntry{entry, withMachine(func(e *google.GoogleEntry) { e.Service = poolService })}, names: "google entry 2"},
		{name: "a session name two machines fix",
			machines: []google.GoogleEntry{
				withMachine(func(e *google.GoogleEntry) { e.Session = machineSession }),
				{Account: fixedAccount, Service: poolService, Session: machineSession},
			}, names: "google entry 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := build(c.machines)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: the session name the fixed fleet's entry fixes is
// reserved for its principal, and the entries that fix none reserve nothing.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	registry, err := identity.NewRegistry([]identity.Provider{listed(t)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []exchange.Reservation{{Session: machineSession, Principal: fixedPrincipal}}
	if got := registry.Policy().Reservations(); !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
	}
}

// TestGoogleAnswersTheListedMachine pins what each kind of Google entry answers: a pinned entry
// that names a person answers that person and its one project, an entry that pins nothing answers
// its service and no person at all, and a token carrying no Compute Engine claims — a workload
// identity pool's, and the one a pod's metadata server answers — is taken by an entry that pins
// nothing. No Google entry ever answers an Instance: nothing registers a machine, so the claim on
// the session name is what keeps two machines of one account apart. Every one of them answers
// its Fleet, the service its account is listed for, whether or not the token names that service:
// it is what the launcher credential the exchange mints records.
func TestGoogleAnswersTheListedMachine(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "a pinned machine of the project and a name the pattern matches",
			token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example-machine",
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "another machine of that account is the same principal",
			token: googleToken(pinnedAccount, true, cloudProject, "example-vm-8"), session: "example-other",
			want: exchange.Claims{Session: "example-other", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "email_verified as the string Google writes on some of its tokens",
			token: googleToken(pinnedAccount, "true", cloudProject, instanceName), session: "example-machine",
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "an entry naming no person answers its service and no project",
			token: googleToken(poolAccount, true, "", ""), session: "example-pool-1",
			want: exchange.Claims{Session: "example-pool-1", Service: poolService, Scope: exchange.ProjectScopePerson, Principal: poolPrincipal, Fleet: poolService}},
		{name: "an entry pinning nothing takes a machine that does run on an instance",
			token: googleToken(poolAccount, true, "other-project", "anything"), session: "example-pool-2",
			want: exchange.Claims{Session: "example-pool-2", Service: poolService, Scope: exchange.ProjectScopePerson, Principal: poolPrincipal, Fleet: poolService}},
		{name: "the session the entry fixes, the request naming none",
			token: googleToken(fixedAccount, true, "", ""), session: "",
			want: exchange.Claims{Session: machineSession, Service: poolService, Scope: exchange.ProjectScopePerson, Principal: fixedPrincipal, Fleet: poolService}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := machine(set, c.token, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Google(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestGoogleRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no email", token: map[string]any{"email_verified": true}, session: "example-machine",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an email that is not a string", token: map[string]any{"email": 7, "email_verified": true}, session: "example-machine",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an account no entry lists", token: googleToken(strangerAccount, true, cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrNoEntry, names: strangerAccount},
		{name: "an email Google has not verified", token: googleToken(pinnedAccount, false, cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "an email_verified of the string false", token: googleToken(pinnedAccount, "false", cloudProject, instanceName), session: "example-machine",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "a token with no Compute Engine claims against a pinned entry", token: googleToken(pinnedAccount, true, "", ""), session: "example-machine",
			want: exchange.ErrNoComputeEngine, names: pinnedAccount},
		{name: "a token with half a Compute Engine identity against a pinned entry",
			token:   map[string]any{"email": pinnedAccount, "email_verified": true, "google": map[string]any{"compute_engine": map[string]any{"project_id": cloudProject}}},
			session: "example-machine", want: exchange.ErrNoComputeEngine, names: pinnedAccount},
		{name: "a machine of another project", token: googleToken(pinnedAccount, true, "other-project", instanceName), session: "example-machine",
			want: exchange.ErrMachineNotAllowed, names: "other-project"},
		{name: "an instance name the pattern does not match", token: googleToken(pinnedAccount, true, cloudProject, "example-other-1"), session: "example-machine",
			want: exchange.ErrMachineNotAllowed, names: "example-other-1"},
		{name: "no session where the entry fixes none", token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "",
			want: exchange.ErrSessionRequired, names: pinnedAccount},
		{name: "a session the entry does not fix", token: googleToken(fixedAccount, true, "", ""), session: "example-other",
			want: exchange.ErrSessionMismatch, names: machineSession},
		{name: "a session carrying whitespace", token: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example machine",
			want: exchange.ErrSessionInvalid, names: pinnedAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := machine(set, c.token, c.session, nil)
			if !errors.Is(err, c.want) {
				t.Fatalf("Google(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Google(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestGoogleSaysWhichRefusalItIs: the five refusals a machine can meet are five sentences, each
// naming what an operator would change. They are compared against one another here, so that
// collapsing two of them into one message fails.
func TestGoogleSaysWhichRefusalItIs(t *testing.T) {
	set := listed(t)
	said := map[string]string{}
	for name, token := range map[string]map[string]any{
		"an account no entry lists":   googleToken(strangerAccount, true, cloudProject, instanceName),
		"an unverified email":         googleToken(pinnedAccount, false, cloudProject, instanceName),
		"no Compute Engine claims":    googleToken(pinnedAccount, true, "", ""),
		"another Compute project":     googleToken(pinnedAccount, true, "other-project", instanceName),
		"an instance name not listed": googleToken(pinnedAccount, true, cloudProject, "example-other-1"),
	} {
		_, err := machine(set, token, "example-machine", nil)
		if err == nil {
			t.Fatalf("Google(%s) was answered, want a refusal", name)
		}
		said[name] = err.Error()
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
}

// TestPersonAnswersTheSignedInPersonAndTheMachineActingForThem pins what a sign-in's trade
// decides: the person is the subject of the token and the machine only acts for them, so the
// claims name the person, the machine's listed account as the actor, and no project at all —
// a person's sign-in on their own box reaches whichever projects that person reaches, as their
// dev box's machine credential does. The principal is the person, so one person's boxes are one
// holder and their session names follow them from box to box. The machine's fleet decides
// nothing else: a fleet whose entry names a person and one whose entry names none answer alike,
// because what the token names is the person who signed in, never the fleet.
func TestPersonAnswersTheSignedInPersonAndTheMachineActingForThem(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name         string
		token, actor map[string]any
		session      string
		want         exchange.Claims
	}{
		{name: "a person of the domain on a pinned machine",
			token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, true, cloudProject, instanceName), session: "example-box-1",
			want: exchange.Claims{Session: "example-box-1", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: pinnedAccount, Fleet: pinnedService}},
		{name: "the same person on a second box of a fleet that names nobody",
			token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box-2",
			want: exchange.Claims{Session: "example-box-2", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount, Fleet: poolService}},
		{name: "an address Google spells with capitals is the lowercase login",
			token: personToken("Ada@example.com", true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box-3",
			want: exchange.Claims{Session: "example-box-3", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount, Fleet: poolService}},
		{name: "email_verified as the string Google writes on some of its tokens",
			token: personToken(person, "true", signInDomain), actor: googleToken(poolAccount, "true", "", ""), session: "example-box-4",
			want: exchange.Claims{Session: "example-box-4", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal, Actor: poolAccount, Fleet: poolService}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := signIn(set, c.token, c.actor, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Person(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

// TestPersonRefusesWhatItCannotDecide walks every way a sign-in's trade is refused, each by the
// sentinel an operator acts on and naming what to fix. The machine is read first and the person
// second, so a box no entry lists is refused whoever is signed in on it.
func TestPersonRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name         string
		token, actor map[string]any
		session      string
		want         error
		names        string
	}{
		{name: "no actor token at all", token: personToken(person, true, signInDomain), actor: nil, session: "example-box",
			want: exchange.ErrNoActorToken, names: "machine-google-identity"},
		{name: "an actor token naming no account", token: personToken(person, true, signInDomain), actor: map[string]any{"email_verified": true}, session: "example-box",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "an actor token of an account no entry lists", token: personToken(person, true, signInDomain), actor: googleToken(strangerAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNoEntry, names: strangerAccount},
		{name: "a machine whose own address Google has not verified", token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, false, cloudProject, instanceName), session: "example-box",
			want: exchange.ErrEmailNotVerified, names: pinnedAccount},
		{name: "a machine running where its entry does not allow", token: personToken(person, true, signInDomain), actor: googleToken(pinnedAccount, true, "other-project", instanceName), session: "example-box",
			want: exchange.ErrMachineNotAllowed, names: "other-project"},
		{name: "a person's token naming no subject", token: map[string]any{"email": person, "email_verified": true, "hd": signInDomain}, actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrClaimMissing, names: "sub"},
		{name: "a person's token naming no address", token: map[string]any{"sub": "109", "email_verified": true, "hd": signInDomain}, actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrClaimMissing, names: "email"},
		{name: "a person of another Workspace domain", token: personToken(stranger, true, otherDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNotInDomain, names: stranger},
		{name: "a person's token carrying no hd at all", token: personToken(person, true, ""), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrNotInDomain, names: person},
		{name: "a person whose address Google has not verified", token: personToken(person, false, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example-box",
			want: exchange.ErrEmailNotVerified, names: person},
		{name: "no session, which no entry fixes for a person", token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "",
			want: exchange.ErrSessionRequired, names: person},
		{name: "a session carrying whitespace", token: personToken(person, true, signInDomain), actor: googleToken(poolAccount, true, "", ""), session: "example box",
			want: exchange.ErrSessionInvalid, names: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := signIn(set, c.token, c.actor, c.session, nil)
			if !errors.Is(err, c.want) {
				t.Fatalf("Person(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Person(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestPersonSaysWhichRefusalItIs: the refusals the spec names each read differently, since what
// the person does about each one differs — sign in as an account of the domain, verify the
// address, present the machine's token, have the fleet listed, or run where the entry allows.
func TestPersonSaysWhichRefusalItIs(t *testing.T) {
	set := listed(t)
	said := map[string]string{}
	for name, c := range map[string]struct{ token, actor map[string]any }{
		"another domain":             {personToken(stranger, true, otherDomain), googleToken(poolAccount, true, "", "")},
		"an unverified person":       {personToken(person, false, signInDomain), googleToken(poolAccount, true, "", "")},
		"an unverified machine":      {personToken(person, true, signInDomain), googleToken(pinnedAccount, false, cloudProject, instanceName)},
		"a machine no entry lists":   {personToken(person, true, signInDomain), googleToken(strangerAccount, true, "", "")},
		"no machine acting for them": {personToken(person, true, signInDomain), nil},
		"a machine where it may not": {personToken(person, true, signInDomain), googleToken(pinnedAccount, true, "other-project", instanceName)},
	} {
		_, err := signIn(set, c.token, c.actor, "example-box", nil)
		if err == nil {
			t.Fatalf("Person(%s) was answered, want a refusal", name)
		}
		said[name] = err.Error()
	}
	for name, text := range said {
		for other, otherText := range said {
			if name != other && text == otherText {
				t.Errorf("the refusals of %s and of %s read alike: %q", name, other, text)
			}
		}
	}
}

// TestAnEmptySignInDomainAdmitsNobody: a provider built with no domain refuses every person,
// whatever their token says, rather than taking every account outside any Workspace. It is the
// rule oidc.WorkspaceLogin states, and the one a broker told about no domain has to follow.
func TestAnEmptySignInDomainAdmitsNobody(t *testing.T) {
	set, err := google.New(google.Config{Machines: []google.GoogleEntry{{Account: poolAccount, Service: poolService}}})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	for _, token := range []map[string]any{
		personToken(person, true, signInDomain),
		personToken(person, true, ""),
	} {
		if _, err := signIn(set, token, googleToken(poolAccount, true, "", ""), "example-box", nil); !errors.Is(err, exchange.ErrNotInDomain) {
			t.Errorf("Person(hd %v) = %v, want %v: an empty configured domain admits nobody", token["hd"], err, exchange.ErrNotInDomain)
		}
	}
}

// TestEveryDecisionCarriesItsProjects: a machine whose entry names a person carries its one
// project, a machine whose entry names none carries no project under the person scope, and a
// person's sign-in carries the list its trade asked for.
func TestEveryDecisionCarriesItsProjects(t *testing.T) {
	set := listed(t)
	fleet, err := machine(set, googleToken(pinnedAccount, true, cloudProject, instanceName), "example-machine", nil)
	if err != nil {
		t.Fatalf("Google: %v", err)
	}
	service, err := machine(set, googleToken(poolAccount, true, "", ""), "example-pool-1", nil)
	if err != nil {
		t.Fatalf("Google: %v", err)
	}
	signedIn, err := signIn(set, personToken(person, true, signInDomain), googleToken(poolAccount, true, "", ""), "example-box", []string{"PRIV"})
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	for _, c := range []struct {
		name     string
		claims   exchange.Claims
		scope    exchange.ProjectScope
		projects []string
	}{
		{name: "Google with a person", claims: fleet, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Google naming no person", claims: service, scope: exchange.ProjectScopePerson, projects: nil},
		{name: "Person", claims: signedIn, scope: exchange.ProjectScopePerson, projects: []string{"PRIV"}},
	} {
		if c.claims.Scope != c.scope || !slices.Equal(c.claims.Projects, c.projects) {
			t.Errorf("%s = scope %v, projects %v; want scope %v, projects %v", c.name, c.claims.Scope, c.claims.Projects, c.scope, c.projects)
		}
	}
}

// TestEachEntryListsItsService: a Google entry's listing carries the service BROKER_VM_SERVICES
// lists its account for, which enroll and requests read in place of that setting, and the person
// and project it names.
func TestEachEntryListsItsService(t *testing.T) {
	registry, err := identity.NewRegistry([]identity.Provider{listed(t)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, c := range []struct {
		principal string
		want      exchange.Listing
	}{
		{pinnedPrincipal, exchange.Listing{Where: `google entry 1, "` + pinnedAccount + `"`, Principal: pinnedPrincipal, Person: person, Project: project, Service: pinnedService}},
		{poolPrincipal, exchange.Listing{Where: `google entry 2, "` + poolAccount + `"`, Principal: poolPrincipal, Service: poolService}},
		{fixedPrincipal, exchange.Listing{Where: `google entry 3, "` + fixedAccount + `"`, Principal: fixedPrincipal, Session: machineSession, Service: poolService}},
	} {
		if got, ok := registry.Policy().Listed(c.principal); !ok || got != c.want {
			t.Errorf("Listed(%s) = %+v, %t; want %+v", c.principal, got, ok, c.want)
		}
	}
}
