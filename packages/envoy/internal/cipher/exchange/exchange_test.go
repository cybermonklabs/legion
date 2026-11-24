package exchange_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
	"github.com/sjawhar/envoy/internal/cipher/identity/google"
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
	repository    = "example-org/example-repo"
	workflow      = ".github/workflows/release.yaml"
	nightly       = ".github/workflows/nightly.yaml"
	ref           = "refs/heads/main"
	project       = "ACME"
	person        = "ada@example.com"
	// fixedSession is the session name the daemon account's entry fixes, nightlySession the one
	// the nightly workflow's entry fixes, and machineSession the one the fixed fleet's entry
	// fixes: one name belongs to one principal, so two entries never fix the same one.
	fixedSession   = "legion-daemon:ACME"
	nightlySession = "ci-nightly"
	machineSession = "example-fleet-1"
	podUID         = "11111111-2222-3333-4444-555555555555"
	credentialID   = "66666666-7777-8888-9999-aaaaaaaaaaaa"
	// The three Google accounts the set lists, each a placeholder: pinnedAccount is held to one
	// Compute Engine project and one instance-name pattern and writes for a person, poolAccount
	// is pinned to nothing and names no person, so its machines are its service and nothing
	// else, and fixedAccount is pinned to nothing and fixes the session its machines write
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
	// one a person signing in on a machine must belong to, and stranger a person of another one.
	signInDomain = "example.com"
	stranger     = "mallory@other.example"
	// The job entries the set lists, each naming its identity and a job list and no person.
	// jobAccount's pods name their sessions and write ACME, publicJobAccount's name theirs and
	// reach every public project and the private projects publicJobs names, and
	// fixedJobAccount's write ACME under the session the entry fixes. jobWorkflow is public and
	// names its sessions, and fixedJobWorkflow writes ACME under the session its entry fixes;
	// the same goes for the two Google accounts of the service jobService, the fixed one pinned
	// as pinnedAccount is.
	jobAccount             = "system:serviceaccount:example-ns:example-job"
	publicJobAccount       = "system:serviceaccount:example-ns:example-public-job"
	fixedJobAccount        = "system:serviceaccount:example-ns:example-fixed-job"
	jobWorkflow            = ".github/workflows/dispatch-job.yaml"
	fixedJobWorkflow       = ".github/workflows/dispatch-nightly.yaml"
	jobMachineAccount      = "example-job@example-project.iam.gserviceaccount.com"
	fixedJobMachineAccount = "example-fixed-job@example-project.iam.gserviceaccount.com"
	jobService             = "example-jobs"
	jobPodSession          = "example-job-pod"
	jobRunSession          = "example-job-run"
	jobMachineSession      = "example-job-machine"
	// publicJobs is the public entries' job list as a setting may spell it, its keys unsorted,
	// and privA and privB are the private projects it names.
	publicJobs = "public,PRIVB,PRIVA"
	privA      = "PRIVA"
	privB      = "PRIVB"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	workerPrincipal  = "account:" + workerAccount
	daemonPrincipal  = "account:" + daemonAccount
	readerPrincipal  = "account:" + readerAccount
	releasePrincipal = "workflow:" + repository + "/" + workflow + "@" + ref
	nightlyPrincipal = "workflow:" + repository + "/" + nightly + "@" + ref
	personPrincipal  = "person:" + person
	pinnedPrincipal  = "google:" + pinnedAccount
	poolPrincipal    = "google:" + poolAccount
	fixedPrincipal   = "google:" + fixedAccount

	jobPrincipal              = "account:" + jobAccount
	publicJobPrincipal        = "account:" + publicJobAccount
	fixedJobPrincipal         = "account:" + fixedJobAccount
	jobWorkflowPrincipal      = "workflow:" + repository + "/" + jobWorkflow + "@" + ref
	fixedJobWorkflowPrincipal = "workflow:" + repository + "/" + fixedJobWorkflow + "@" + ref
	jobMachinePrincipal       = "google:" + jobMachineAccount
	fixedJobMachinePrincipal  = "google:" + fixedJobMachineAccount
)

// listedSet is the set every provider's entries compose, with each provider's decision beside
// it, so a test reads a decision and the set it was listed in side by side, as the exchange does.
type listedSet struct {
	*exchange.Set
	registry *identity.Registry
	pods     *kubernetes.Provider
	runs     *github.Provider
	machines *google.Provider
}

// ProjectsForAccount is the registry's, which spells each account's principal through its own
// provider (identity.Registry.ProjectsForAccount).
func (l listedSet) ProjectsForAccount(account string) ([]string, bool) {
	return l.registry.ProjectsForAccount(account)
}

func (l listedSet) Pod(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return l.pods.Decide(identity.Trade{TokenType: kubernetes.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

func (l listedSet) Workflow(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return l.runs.Decide(identity.Trade{TokenType: github.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

func (l listedSet) Google(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return l.machines.Decide(identity.Trade{TokenType: google.SubjectTokenType, Subject: token, Session: session, Projects: projects})
}

func (l listedSet) Person(token, actor map[string]any, session string, projects []string) (exchange.Claims, error) {
	return l.machines.Decide(identity.Trade{TokenType: google.PersonTokenType, Subject: token, Actor: actor, Session: session, Projects: projects})
}

// build composes the providers of the entries given into one set, as the identity registry does.
func build(accounts []exchange.AccountEntry, reads []exchange.ReadEntry, workflows []exchange.WorkflowEntry, machines []exchange.GoogleEntry) (listedSet, error) {
	pods, err := kubernetes.New(kubernetes.Config{Accounts: accounts, Reads: reads})
	if err != nil {
		return listedSet{}, err
	}
	runs, err := github.New(github.Config{Workflows: workflows})
	if err != nil {
		return listedSet{}, err
	}
	fleets, err := google.New(google.Config{Machines: machines, SignInDomain: signInDomain})
	if err != nil {
		return listedSet{}, err
	}
	registry, err := identity.NewRegistry([]identity.Provider{pods, fleets, runs}, nil)
	if err != nil {
		return listedSet{}, err
	}
	return listedSet{Set: registry.Policy(), registry: registry, pods: pods, runs: runs, machines: fleets}, nil
}

// listed is the set every decision test asks: one account whose sessions name themselves, one
// account whose session the entry fixes, one read entry, the same pair for one workflow of one
// repository on one ref, three Google accounts — one pinned to a project and an
// instance-name pattern that writes for a person, one pinned to nothing that names no person,
// and one that fixes the session its machines write under — and the job entries: three
// accounts, two workflows and two Google accounts that name a job list and no person.
func listed(t *testing.T) listedSet {
	t.Helper()
	set, err := build(
		[]exchange.AccountEntry{
			{Account: workerAccount, Project: project, Person: person},
			{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
			{Account: jobAccount, Job: jobList(t, project)},
			{Account: publicJobAccount, Job: jobList(t, publicJobs)},
			{Account: fixedJobAccount, Job: jobList(t, project), Session: jobPodSession},
		},
		[]exchange.ReadEntry{{Account: readerAccount, Service: readerService, Session: readerSession}},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: nightly, Ref: ref, Project: project, Person: person, Session: nightlySession},
			{Repository: repository, Workflow: jobWorkflow, Ref: ref, Job: jobList(t, publicJobs)},
			{Repository: repository, Workflow: fixedJobWorkflow, Ref: ref, Job: jobList(t, project), Session: jobRunSession},
		},
		[]exchange.GoogleEntry{
			{Account: pinnedAccount, Service: pinnedService, CloudProject: cloudProject, Instance: instanceGlob, Project: project, Person: person},
			{Account: poolAccount, Service: poolService},
			{Account: fixedAccount, Service: poolService, Session: machineSession},
			{Account: jobMachineAccount, Service: jobService, Job: jobList(t, publicJobs)},
			{Account: fixedJobMachineAccount, Service: jobService, CloudProject: cloudProject, Instance: instanceGlob, Job: jobList(t, project), Session: jobMachineSession},
		},
	)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
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

// workflowToken is a GitHub workflow run's token claims. jobRef is the workflow the job itself is
// written in, which differs from workflowRef only for a run that called a reusable workflow.
func workflowToken(repo, workflowRef, jobRef, gitRef string) map[string]any {
	return map[string]any{
		"repository":       repo,
		"workflow_ref":     workflowRef,
		"job_workflow_ref": jobRef,
		"ref":              gitRef,
		"run_id":           "4242",
	}
}

// runRef is the workflow_ref a run of path in repository on ref carries.
func runRef(path string) string { return repository + "/" + path + "@" + ref }

// TestNewSetRefusesAnEntryItCannotServe holds what the set refuses across providers, each
// provider having refused its own entries already (internal/cipher/identity/<kind>): a session
// name entries of two providers fix, and an identity two listings name. It holds too, through
// the providers, an account subject with text around it and every job entry a provider refuses.
func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	account := exchange.AccountEntry{Account: workerAccount, Project: project, Person: person, Session: fixedSession}
	flow := exchange.WorkflowEntry{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person, Session: fixedSession}
	machine := exchange.GoogleEntry{Account: pinnedAccount, Service: pinnedService, Session: fixedSession}
	with := func(change func(*exchange.AccountEntry)) exchange.AccountEntry {
		entry := account
		change(&entry)
		return entry
	}
	withFlow := func(change func(*exchange.WorkflowEntry)) exchange.WorkflowEntry {
		entry := flow
		change(&entry)
		return entry
	}
	withMachine := func(change func(*exchange.GoogleEntry)) exchange.GoogleEntry {
		entry := machine
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
		name      string
		accounts  []exchange.AccountEntry
		reads     []exchange.ReadEntry
		workflows []exchange.WorkflowEntry
		machines  []exchange.GoogleEntry
		names     string
	}{
		{name: "an account with text after it",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "system:serviceaccount:example-ns:example-public===x" })}, names: "example-public===x"},
		{name: "an account with text before it",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Account = "xsystem:serviceaccount:example-ns:example-public" })}, names: "xsystem:serviceaccount:example-ns:example-public"},
		{name: "a session name an account and a workflow fix",
			accounts: []exchange.AccountEntry{account}, workflows: []exchange.WorkflowEntry{flow}, names: workerAccount},
		{name: "a session name a machine and an account fix",
			accounts: []exchange.AccountEntry{account}, machines: []exchange.GoogleEntry{machine}, names: pinnedAccount},
		// An entry names a person and its project, or a job list, and never both; a workflow entry
		// that names neither mints a token answerable to nobody. An account entry that names
		// neither acts as the account itself (TestAnAccountEntryMayNameNoPerson).
		{name: "a job account that names a person too",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Project, e.Job = "", jobList(t, project) })}, names: workerAccount},
		{name: "a job account that names a project too",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Person, e.Job = "", jobList(t, project) })}, names: workerAccount},
		{name: "a job account whose fixed session carries whitespace",
			accounts: []exchange.AccountEntry{{Account: workerAccount, Job: jobList(t, publicJobs), Session: "example job"}}, names: workerAccount},
		{name: "an account a person entry and a job entry both list",
			accounts: []exchange.AccountEntry{account, {Account: workerAccount, Job: jobList(t, project)}}, names: "account entry 2"},
		{name: "an account a job entry and a read entry both list",
			accounts: []exchange.AccountEntry{{Account: workerAccount, Job: jobList(t, project)}},
			reads:    []exchange.ReadEntry{withReader(func(e *exchange.ReadEntry) { e.Account = workerAccount })}, names: `read entry 1, "` + workerAccount + `"`},
		{name: "a job workflow that names a person too",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Project, e.Job = "", jobList(t, publicJobs) })}, names: workflow},
		{name: "a job workflow that names a project too",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Person, e.Job = "", jobList(t, publicJobs) })}, names: workflow},
		{name: "a workflow naming neither a person nor a job list",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Project, e.Person = "", "" })}, names: workflow},
		{name: "a job workflow whose fixed session is unprintable",
			workflows: []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Job: jobList(t, project), Session: "example\x00job"}}, names: workflow},
		{name: "a session name a job account and a job workflow fix",
			accounts:  []exchange.AccountEntry{{Account: workerAccount, Job: jobList(t, project), Session: jobPodSession}},
			workflows: []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Job: jobList(t, project), Session: jobPodSession}}, names: workerAccount},
		{name: "a Google job entry that names a person too",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Person, e.Job = person, jobList(t, project) })}, names: pinnedAccount},
		{name: "a Google job entry that names a project too",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Project, e.Job = project, jobList(t, project) })}, names: pinnedAccount},
		{name: "a Google job entry that names a person and a project too",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Project, e.Person, e.Job = project, person, jobList(t, publicJobs) })}, names: pinnedAccount},
		{name: "a Google job entry whose fixed session carries whitespace",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Session, e.Job = "example job", jobList(t, project) })}, names: pinnedAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := build(c.accounts, c.reads, c.workflows, c.machines)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
	t.Run("an identity two listings name", func(t *testing.T) {
		set, err := exchange.NewSet([]exchange.Listing{
			{Where: "first entry", Principal: "account:" + workerAccount},
			{Where: "second entry", Principal: "account:" + workerAccount},
		})
		if err == nil || !strings.Contains(err.Error(), "first entry") || !strings.Contains(err.Error(), "second entry") {
			t.Fatalf("NewSet(one principal twice) = %v, %v; want a refusal naming both entries", set, err)
		}
	})
}

// TestAnAccountEntryMayNameNoPerson: an account entry may name neither a person nor a project
// (<service account>==), an account whose tokens act as the account itself. The set boots with
// it, and its pods' tokens are named, name no person and no project, and carry the
// account as their service, which is what Dispatch records their writes under, beside the account
// as the principal their session names belong to. An entry that fixes a session answers that
// session and no pod uid, and its session is reserved for the account as any fixed one is. A
// request naming a project is refused as under any entry naming none, and MappedProjects and
// ProjectsForAccount answer neither a person nor a project.
func TestAnAccountEntryMayNameNoPerson(t *testing.T) {
	set, err := build([]exchange.AccountEntry{
		{Account: workerAccount},
		{Account: daemonAccount, Session: fixedSession},
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("NewSet with account entries naming no person and no project = %v, want them taken", err)
	}
	claims, err := set.Pod(podToken(workerAccount, podUID), "public-session-1", nil)
	want := exchange.Claims{Session: "public-session-1", Service: workerAccount, Scope: exchange.ProjectScopeNamed, Principal: workerPrincipal, Instance: podUID}
	if err != nil || !reflect.DeepEqual(claims, want) {
		t.Fatalf("Pod under an entry naming no person = %+v, %v; want %+v: named, no person, no project, the account as its service", claims, err, want)
	}
	fixed, err := set.Pod(podToken(daemonAccount, podUID), "", nil)
	wantFixed := exchange.Claims{Session: fixedSession, Service: daemonAccount, Scope: exchange.ProjectScopeNamed, Principal: daemonPrincipal}
	if err != nil || !reflect.DeepEqual(fixed, wantFixed) {
		t.Fatalf("Pod under an entry naming no person that fixes its session = %+v, %v; want %+v", fixed, err, wantFixed)
	}
	if got := set.Reservations(); !reflect.DeepEqual(got, []exchange.Reservation{{Session: fixedSession, Principal: daemonPrincipal}}) {
		t.Fatalf("Reservations = %v, want the fixed session held for %s", got, daemonPrincipal)
	}
	if _, err := set.Pod(podToken(workerAccount, podUID), "public-session-1", []string{project}); !errors.Is(err, exchange.ErrProjectsFixed) {
		t.Fatalf("a request naming %s under an entry naming no person = %v, want ErrProjectsFixed", project, err)
	}
	if gotPerson, projects, ok := set.MappedProjects(workerPrincipal); gotPerson != "" || projects != nil || !ok {
		t.Fatalf("MappedProjects = %q, %v, %v; want no person, no project, true", gotPerson, projects, ok)
	}
	if projects, ok := set.ProjectsForAccount(workerAccount); projects != nil || !ok {
		t.Fatalf("ProjectsForAccount = %v, %v; want no project, true", projects, ok)
	}
}

// TestAPersonlessEntryNamesNoProject: of the entries that name no job list, only an account entry
// that names no project may name no person: an account entry that names a project names its
// person, so no account entry's token that names a project is personless. An account entry naming a
// project and no person is refused, naming the entry and that rule; a person that is blank without
// being empty is refused whatever the project; and a workflow entry still needs a person, whether
// or not it names a project.
func TestAPersonlessEntryNamesNoProject(t *testing.T) {
	for _, c := range []struct {
		name      string
		accounts  []exchange.AccountEntry
		workflows []exchange.WorkflowEntry
		names     []string
	}{
		{name: "an account entry naming a project and no person",
			accounts: []exchange.AccountEntry{{Account: workerAccount, Project: project}},
			names:    []string{workerAccount, "names no person only when it names no project"}},
		{name: "an account entry naming no project and a blank person",
			accounts: []exchange.AccountEntry{{Account: workerAccount, Person: " "}},
			names:    []string{workerAccount, "the person is required"}},
		{name: "a workflow entry naming a project and no person",
			workflows: []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref, Project: project}},
			names:     []string{workflow, "the person is required"}},
		{name: "a workflow entry naming neither",
			workflows: []exchange.WorkflowEntry{{Repository: repository, Workflow: workflow, Ref: ref}},
			names:     []string{workflow}},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := build(c.accounts, nil, c.workflows, nil)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			for _, name := range c.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, name)
				}
			}
		})
	}
}

// TestListedAnswersEachEntrysListing: a reader holding no token reads what an operator's entry
// says of an identity, a machine's service among it, and nothing of an identity no entry lists.
func TestListedAnswersEachEntrysListing(t *testing.T) {
	set := listed(t)
	if got, ok := set.Listed(pinnedPrincipal); !ok || got.Service != pinnedService || got.Person != person || got.Project != project {
		t.Errorf("Listed(%s) = %+v, %t; want its service, person and project", pinnedPrincipal, got, ok)
	}
	if got, ok := set.Listed(readerPrincipal); !ok || got.Service != readerService || got.Session != readerSession {
		t.Errorf("Listed(%s) = %+v, %t; want its service and session", readerPrincipal, got, ok)
	}
	if got, ok := set.Listed(personPrincipal); ok {
		t.Errorf("Listed(%s) = %+v, true; want no listing of a person's own principal", personPrincipal, got)
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: every session name an entry fixes is reserved for
// that entry's principal, in the order the entries were listed, and a name no entry fixes is not
// reserved at all. A job entry of each kind that fixes a session reserves it as every other entry
// does. The answer is the caller's own copy, so the set stays as it was built.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	set := listed(t)
	// The providers in rank order, kubernetes, google, github, and each provider's entries in
	// its own order.
	want := []exchange.Reservation{
		{Session: fixedSession, Principal: daemonPrincipal},
		{Session: jobPodSession, Principal: fixedJobPrincipal},
		{Session: readerSession, Principal: readerPrincipal},
		{Session: machineSession, Principal: fixedPrincipal},
		{Session: jobMachineSession, Principal: fixedJobMachinePrincipal},
		{Session: nightlySession, Principal: nightlyPrincipal},
		{Session: jobRunSession, Principal: fixedJobWorkflowPrincipal},
	}
	got := set.Reservations()
	if !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
	}
	got[0].Principal = "account:changed"
	if again := set.Reservations(); !slices.Equal(again, want) {
		t.Errorf("Reservations after the caller changed its copy = %+v, want %+v", again, want)
	}
}

// TestPodAnswersTheListedAccount pins what each kind of entry answers, Instance included: an
// entry that fixes no session answers the pod uid, which a registration has to name for the
// exchange to go through, and an entry that fixes one answers none, since every pod of that
// account writes under the configuration's name and is registered for nothing. A read entry
// always fixes one, and answers its service and session, the project scope that writes nothing,
// and no person and no project. The daemon account's entry fixes legion-daemon:ACME, the daemon
// session of its own project, so it answers LegionStatus too
// (TestPodGivesLegionStatusToTheDaemonsEntryAlone).
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
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, LegionStatus: true, Principal: daemonPrincipal}},
		{name: "the session the entry fixes, the request naming the same", token: podToken(daemonAccount, podUID), session: fixedSession,
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, LegionStatus: true, Principal: daemonPrincipal}},
		{name: "another pod of the account is the same principal", token: podToken(daemonAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, LegionStatus: true, Principal: daemonPrincipal}},
		{name: "a read entry's session, the request naming none", token: podToken(readerAccount, podUID), session: "",
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
		{name: "a read entry's session, the request naming the same", token: podToken(readerAccount, podUID), session: readerSession,
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
		{name: "another pod of the read account is the same principal", token: podToken(readerAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: readerSession, Service: readerService, Scope: exchange.ProjectScopeNone, Principal: readerPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Pod(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

// TestPodGivesLegionStatusToTheDaemonsEntryAlone: Pod answers LegionStatus for one kind of entry
// alone, an account entry that names a project and fixes the session legion-daemon:<that
// project>. Each other row is a pod whose token carries a session that looks like a daemon's or a
// Legion role's, and Pod answers it LegionStatus false:
//
//   - an entry of ACME that fixes no session, whose request names legion-daemon:ACME;
//   - an entry of P2 that fixes legion-daemon:P3;
//   - an entry that names no project and fixes legion-daemon: alone;
//   - an entry of ACME that fixes a Legion role's session;
//   - an entry of CASE that fixes Legion-Daemon:CASE, since the comparison holds the case;
//   - a job entry whose list is JOB and that fixes legion-daemon:JOB, since a job entry names no
//     Project (its projects are its job list);
//   - a read entry that fixes legion-daemon:READ.
//
// The decision reads the session the entry fixes, and never the session the request names.
func TestPodGivesLegionStatusToTheDaemonsEntryAlone(t *testing.T) {
	const (
		crossAccount     = "system:serviceaccount:example-ns:example-cross"
		noProjectAccount = "system:serviceaccount:example-ns:example-noproject"
		roleAccount      = "system:serviceaccount:example-ns:example-role"
		caseAccount      = "system:serviceaccount:example-ns:example-case"
		roleSession      = "legion-acme-acme-1-implementer"
	)
	set, err := build(
		[]exchange.AccountEntry{
			{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
			{Account: workerAccount, Project: project, Person: person},
			{Account: crossAccount, Project: "P2", Person: person, Session: "legion-daemon:P3"},
			{Account: noProjectAccount, Person: person, Session: "legion-daemon:"},
			{Account: roleAccount, Project: project, Person: person, Session: roleSession},
			{Account: caseAccount, Project: "CASE", Person: person, Session: "Legion-Daemon:CASE"},
			{Account: jobAccount, Job: jobList(t, "JOB"), Session: "legion-daemon:JOB"},
		},
		[]exchange.ReadEntry{{Account: readerAccount, Service: readerService, Session: "legion-daemon:READ"}},
		nil, nil)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	for _, c := range []struct {
		name    string
		account string
		// session is the session the request names, and claimed the one Pod answers.
		session, claimed string
		want             bool
	}{
		{name: "the daemon's entry, an account of ACME fixing legion-daemon:ACME", account: daemonAccount, claimed: fixedSession, want: true},
		{name: "the daemon's entry, the request naming the session it fixes", account: daemonAccount, session: fixedSession, claimed: fixedSession, want: true},
		{name: "an account of ACME fixing no session, whose request names legion-daemon:ACME", account: workerAccount, session: fixedSession, claimed: fixedSession},
		{name: "an account of P2 fixing legion-daemon:P3", account: crossAccount, claimed: "legion-daemon:P3"},
		{name: "an account naming no project, fixing legion-daemon:", account: noProjectAccount, claimed: "legion-daemon:"},
		{name: "an account of ACME fixing the role's session", account: roleAccount, claimed: roleSession},
		{name: "an account of CASE fixing Legion-Daemon:CASE", account: caseAccount, claimed: "Legion-Daemon:CASE"},
		{name: "a job entry of JOB fixing legion-daemon:JOB", account: jobAccount, claimed: "legion-daemon:JOB"},
		{name: "a read entry fixing legion-daemon:READ", account: readerAccount, claimed: "legion-daemon:READ"},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(podToken(c.account, podUID), c.session, nil)
			if err != nil {
				t.Fatalf("Pod(%s) = %v, want claims", c.name, err)
			}
			if claims.Session != c.claimed {
				t.Fatalf("Pod(%s) answered the session %q, want %q", c.name, claims.Session, c.claimed)
			}
			if claims.LegionStatus != c.want {
				t.Errorf("Pod(%s) answered LegionStatus %t, want %t", c.name, claims.LegionStatus, c.want)
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
// account no entry lists. A job account is held to the same rule as a person's: its pod is
// registered when its entry fixes no session, and refused when it fixes one.
func TestRegistrationRefusesWhatNeedsNoRegistration(t *testing.T) {
	set := listed(t)
	for _, account := range []string{workerAccount, jobAccount} {
		if err := set.pods.Registration(account, podUID, "implementer-g1"); err != nil {
			t.Fatalf("Registration of a pod of %s for a name of its own = %v, want it accepted", account, err)
		}
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
		{name: "a job entry that fixes a session", account: fixedJobAccount, uid: podUID, session: jobPodSession,
			want: exchange.ErrSessionFixed, names: jobPodSession},
		{name: "a read entry asked for another name", account: readerAccount, uid: podUID, session: "implementer-g1",
			want: exchange.ErrSessionFixed, names: readerAccount},
		{name: "a session carrying whitespace", account: workerAccount, uid: podUID, session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "no session at all", account: workerAccount, uid: podUID, session: "",
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := set.pods.Registration(c.account, c.uid, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Registration(%s) = %v; want %v", c.name, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Registration(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestMachineActsForTheApproverAndWritesEveryProjectThePersonMay(t *testing.T) {
	set := listed(t)
	claims, err := set.Machine(credentialID, person, "ada-devbox", nil, exchange.ApproverUnrestricted)
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	want := exchange.Claims{Session: "ada-devbox", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal}
	if !reflect.DeepEqual(claims, want) {
		t.Fatalf("Machine = %+v, want %+v: a machine credential that asked for no project names none and says which absence that is", claims, want)
	}
	// Another machine of the same person is the same principal: the name belongs to the person.
	other, err := set.Machine("77777777-8888-9999-aaaa-bbbbbbbbbbbb", person, "ada-devbox", nil, exchange.ApproverUnrestricted)
	if err != nil || !reflect.DeepEqual(other, want) {
		t.Fatalf("Machine(another credential of %s) = %+v, %v; want %+v", person, other, err, want)
	}
}

// TestEveryDecisionCarriesItsProjects: the zero ProjectScope is what a Claims nobody built
// carries, so no constructor may answer it; a named scope carries its entry's one project and a
// person scope the list its trade asked for, so a later reader never has to guess which a list
// is. A read entry's decision says that it writes nothing, carries no project, and reads apart
// from the zero value in a refusal or a log line too. A job entry's decision carries its entry's
// scope and whole list, the public scope included, which reads apart from every other in a
// refusal or a log line. A person's own credential (Machine of a member not restricted, and
// Person here) is never named: a restricted member's machine login is the one person's
// credential decided named (TestARestrictedMembersMachineLoginIsNamedToItsOneProject), and
// Dispatch holds such a token to its account as it holds the person scope's, telling it from an
// operator entry's by the credential its trade recorded.
func TestEveryDecisionCarriesItsProjects(t *testing.T) {
	set := listed(t)
	pod, err := set.Pod(podToken(workerAccount, podUID), "implementer-g1", nil)
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	run, err := set.Workflow(workflowToken(repository, runRef(workflow), runRef(workflow), ref), "release-bot", nil)
	if err != nil {
		t.Fatalf("Workflow: %v", err)
	}
	read, err := set.Pod(podToken(readerAccount, podUID), "", nil)
	if err != nil {
		t.Fatalf("Pod of the read account: %v", err)
	}
	fleet, err := set.Google(googleToken(pinnedAccount, true, cloudProject, instanceName), "example-machine", nil)
	if err != nil {
		t.Fatalf("Google: %v", err)
	}
	service, err := set.Google(googleToken(poolAccount, true, "", ""), "example-pool-1", nil)
	if err != nil {
		t.Fatalf("Google: %v", err)
	}
	machine, err := set.Machine(credentialID, person, "ada-devbox", []string{"PRIV", "PUB"}, exchange.ApproverUnrestricted)
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	signIn, err := set.Person(personToken(person, true, signInDomain), googleToken(poolAccount, true, "", ""), "example-box", []string{"PRIV"})
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	jobPod, err := set.Pod(podToken(jobAccount, podUID), "example-job-1", nil)
	if err != nil {
		t.Fatalf("Pod of a named job account: %v", err)
	}
	publicPod, err := set.Pod(podToken(publicJobAccount, podUID), "example-job-2", nil)
	if err != nil {
		t.Fatalf("Pod of a public job account: %v", err)
	}
	jobRun, err := set.Workflow(actionsToken(repository, jobWorkflow, ref), "example-run-1", nil)
	if err != nil {
		t.Fatalf("Workflow of a public job: %v", err)
	}
	jobMachine, err := set.Google(googleToken(fixedJobMachineAccount, true, cloudProject, instanceName), "", nil)
	if err != nil {
		t.Fatalf("Google of a named job: %v", err)
	}
	for _, c := range []struct {
		name     string
		claims   exchange.Claims
		scope    exchange.ProjectScope
		projects []string
	}{
		{name: "Pod", claims: pod, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Workflow", claims: run, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Google with a person", claims: fleet, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Google naming no person", claims: service, scope: exchange.ProjectScopePerson, projects: nil},
		{name: "Machine", claims: machine, scope: exchange.ProjectScopePerson, projects: []string{"PRIV", "PUB"}},
		{name: "Person", claims: signIn, scope: exchange.ProjectScopePerson, projects: []string{"PRIV"}},
		{name: "Pod of the read account", claims: read, scope: exchange.ProjectScopeNone, projects: nil},
		{name: "Pod of a named job account", claims: jobPod, scope: exchange.ProjectScopeNamed, projects: []string{project}},
		{name: "Pod of a public job account", claims: publicPod, scope: exchange.ProjectScopePublic, projects: []string{privA, privB}},
		{name: "Workflow of a public job", claims: jobRun, scope: exchange.ProjectScopePublic, projects: []string{privA, privB}},
		{name: "Google of a named job", claims: jobMachine, scope: exchange.ProjectScopeNamed, projects: []string{project}},
	} {
		if c.claims.Scope != c.scope || !slices.Equal(c.claims.Projects, c.projects) {
			t.Errorf("%s = scope %v, projects %v; want scope %v, projects %v", c.name, c.claims.Scope, c.claims.Projects, c.scope, c.projects)
		}
	}
	if got := exchange.ProjectScopeUnset.String(); !strings.Contains(got, "no project scope") {
		t.Errorf("ProjectScopeUnset.String() = %q, want it to say the scope was never set", got)
	}
	if none, unset := exchange.ProjectScopeNone.String(), exchange.ProjectScopeUnset.String(); none == unset || strings.Contains(none, "no project scope") {
		t.Errorf("ProjectScopeNone.String() = %q beside ProjectScopeUnset's %q, want a decision that writes nothing to read apart from one never made", none, unset)
	}
	public := exchange.ProjectScopePublic.String()
	if !strings.Contains(public, "public") {
		t.Errorf("ProjectScopePublic.String() = %q, want it to say the token reaches the public projects", public)
	}
	for _, other := range []exchange.ProjectScope{exchange.ProjectScopeUnset, exchange.ProjectScopeNamed, exchange.ProjectScopePerson, exchange.ProjectScopeNone} {
		if other.String() == public {
			t.Errorf("ProjectScopePublic.String() = %q, which %d reads as too", public, int(other))
		}
	}
}

func TestMachineRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name       string
		credential string
		approver   string
		session    string
		want       error
		names      string
	}{
		{name: "a credential with no id", credential: " ", approver: person, session: "ada-devbox", want: exchange.ErrClaimMissing, names: "id"},
		{name: "a credential with no approver", credential: credentialID, approver: " ", session: "ada-devbox", want: exchange.ErrClaimMissing, names: "approver"},
		{name: "a request naming no session", credential: credentialID, approver: person, session: "", want: exchange.ErrSessionRequired, names: person},
		{name: "a request naming a session with whitespace", credential: credentialID, approver: person, session: "ada devbox", want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming a session past the cap", credential: credentialID, approver: person, session: strings.Repeat("s", exchange.MaxSessionBytes+1), want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming the wildcard session *", credential: credentialID, approver: person, session: "*", want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming the wildcard session >", credential: credentialID, approver: person, session: ">", want: exchange.ErrSessionInvalid, names: person},
		{name: "a request naming a session holding a wildcard", credential: credentialID, approver: person, session: "ada-*", want: exchange.ErrSessionInvalid, names: person},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Machine(c.credential, c.approver, c.session, nil, exchange.ApproverUnrestricted)
			if !errors.Is(err, c.want) {
				t.Fatalf("Machine(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Machine(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestASessionNameHoldsNoWildcard: ValidSession takes every shape of session name a deployment
// trades under, now that it refuses a name holding a NATS wildcard ('*' or '>', whose rows are
// TestMachineRefusesWhatItCannotDecide's): an Envoy session id, the Legion daemon's fixed session,
// a run pod's fixed session holding a slash, the read entry's, and a person's sign-in
// (cipherclient.SessionFor), which holds the person's address and so a '.'.
func TestASessionNameHoldsNoWildcard(t *testing.T) {
	for _, name := range []string{"01930000-0000-7000-8000-000000000001", "ses_agent", fixedSession, "run-holds/example-run", readerSession, "signin-ada@example.com-abcdefghijkl"} {
		if !exchange.ValidSession(name) {
			t.Errorf("ValidSession(%q) = false, want true", name)
		}
	}
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
