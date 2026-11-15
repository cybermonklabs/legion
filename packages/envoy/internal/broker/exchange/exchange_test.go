package exchange_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
)

const (
	workerAccount = "system:serviceaccount:example-ns:example-worker"
	daemonAccount = "system:serviceaccount:example-ns:example-daemon"
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
	// one a person signing in on a machine must belong to; otherDomain is any other Workspace,
	// and stranger a person of it.
	signInDomain = "example.com"
	otherDomain  = "other.example"
	stranger     = "mallory@other.example"
)

// The workflow whose runs write CI results, listed with three ref patterns: the probe branch,
// every pull request's merge ref, and the merge queue's branches for main. commit is the sha a
// run of it carries, and the session and principal are the ones every run of it, on any of those
// refs, is answered under.
const (
	resultFlow      = ".github/workflows/ci-result.yaml"
	probeRef        = "refs/heads/probe/infra-3577-ci-result"
	pullPattern     = "refs/pull/*/merge"
	queuePattern    = "refs/heads/gh-readonly-queue/main/*"
	commit          = "0123456789abcdef0123456789abcdef01234567"
	resultSession   = "ci-results:" + repository + "/" + resultFlow
	resultPrincipal = "workflow-results:" + repository + "/" + resultFlow
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	workerPrincipal  = "account:" + workerAccount
	daemonPrincipal  = "account:" + daemonAccount
	releasePrincipal = "workflow:" + repository + "/" + workflow + "@" + ref
	nightlyPrincipal = "workflow:" + repository + "/" + nightly + "@" + ref
	personPrincipal  = "person:" + person
	pinnedPrincipal  = "google:" + pinnedAccount
	poolPrincipal    = "google:" + poolAccount
	fixedPrincipal   = "google:" + fixedAccount
)

// listed is the set every decision test asks: one account whose sessions name themselves, one
// account whose session the entry fixes, the same pair for one workflow of one repository on one
// ref, a workflow whose runs write CI results on the refs of three patterns, and three Google
// accounts — one pinned to a project and an instance-name pattern that writes for a person, one
// pinned to nothing that names no person, and one that fixes the session its machines write
// under.
func listed(t *testing.T) *exchange.Set {
	t.Helper()
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{
			{Account: workerAccount, Project: project, Person: person},
			{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
		},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: nightly, Ref: ref, Project: project, Person: person, Session: nightlySession},
		},
		resultEntries(probeRef, pullPattern, queuePattern),
		[]exchange.GoogleEntry{
			{Account: pinnedAccount, Service: pinnedService, CloudProject: cloudProject, Instance: instanceGlob, Project: project, Person: person},
			{Account: poolAccount, Service: poolService},
			{Account: fixedAccount, Service: poolService, Session: machineSession},
		},
		signInDomain,
	)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

// resultEntries lists resultFlow of repository once per pattern, each for the one project and
// person.
func resultEntries(patterns ...string) []exchange.ResultWorkflowEntry {
	entries := make([]exchange.ResultWorkflowEntry, 0, len(patterns))
	for _, pattern := range patterns {
		entries = append(entries, exchange.ResultWorkflowEntry{Repository: repository, Workflow: resultFlow, Ref: pattern, Project: project, Person: person})
	}
	return entries
}

// resultToken is the token claims of a run of resultFlow on gitRef, carrying every claim of the
// run a result entry's token records, the check run among them.
func resultToken(gitRef string) map[string]any {
	token := workflowToken(repository, repository+"/"+resultFlow+"@"+gitRef, repository+"/"+resultFlow+"@"+gitRef, gitRef)
	token["sha"] = commit
	token["event_name"] = "pull_request"
	token["run_attempt"] = "2"
	token["runner_environment"] = "self-hosted"
	token["actor"] = "ada-gh"
	token["check_run_id"] = "987654321"
	return token
}

// resultRun is the run resultToken(gitRef) describes, as a result entry's Claims carry it.
func resultRun(gitRef string) *oauthwire.CIRun {
	return &oauthwire.CIRun{
		Repository: repository, WorkflowRef: repository + "/" + resultFlow + "@" + gitRef, Ref: gitRef, SHA: commit,
		EventName: "pull_request", RunID: "4242", RunAttempt: "2", RunnerEnvironment: "self-hosted", Actor: "ada-gh",
		CheckRunID: "987654321",
	}
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

func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	account := exchange.AccountEntry{Account: workerAccount, Project: project, Person: person}
	flow := exchange.WorkflowEntry{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person}
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
	machine := exchange.GoogleEntry{Account: pinnedAccount, Service: pinnedService}
	withMachine := func(change func(*exchange.GoogleEntry)) exchange.GoogleEntry {
		entry := machine
		change(&entry)
		return entry
	}
	result := exchange.ResultWorkflowEntry{Repository: repository, Workflow: resultFlow, Ref: pullPattern, Project: project, Person: person}
	withResult := func(change func(*exchange.ResultWorkflowEntry)) exchange.ResultWorkflowEntry {
		entry := result
		change(&entry)
		return entry
	}
	withPattern := func(pattern string) []exchange.ResultWorkflowEntry {
		return []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) { e.Ref = pattern })}
	}
	for _, c := range []struct {
		name      string
		machines  []exchange.GoogleEntry
		accounts  []exchange.AccountEntry
		workflows []exchange.WorkflowEntry
		results   []exchange.ResultWorkflowEntry
		names     string
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
		{name: "the same account twice",
			accounts: []exchange.AccountEntry{account, with(func(e *exchange.AccountEntry) { e.Project = "OTHER" })}, names: "account entry 2"},
		{name: "a repository that is not <owner>/<name>",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Repository = "example-repo" })}, names: "example-repo"},
		{name: "a workflow with no path",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Workflow = "" })}, names: repository},
		{name: "a workflow with no ref",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Ref = "" })}, names: workflow},
		{name: "a workflow with no project",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Project = "" })}, names: workflow},
		{name: "a workflow with no person",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Person = " " })}, names: workflow},
		{name: "a workflow whose fixed session is unprintable",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Session = "legion\x00daemon" })}, names: workflow},
		{name: "the same workflow twice",
			workflows: []exchange.WorkflowEntry{flow, withFlow(func(e *exchange.WorkflowEntry) { e.Person = "bob@example.com" })}, names: "workflow entry 2"},
		{name: "two workflows whose principals spell alike",
			workflows: []exchange.WorkflowEntry{
				withFlow(func(e *exchange.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml@refs/heads/b", "c" }),
				withFlow(func(e *exchange.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml", "refs/heads/b@c" }),
			}, names: "workflow entry 1"},
		{name: "a session name two accounts fix",
			accounts: []exchange.AccountEntry{
				with(func(e *exchange.AccountEntry) { e.Session = fixedSession }),
				{Account: daemonAccount, Project: project, Person: person, Session: fixedSession},
			}, names: "account entry 1"},
		{name: "a session name an account and a workflow fix",
			accounts:  []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = fixedSession })},
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Session = fixedSession })}, names: workerAccount},
		{name: "a Google account that is not an email address",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Account = "example-fleet" })}, names: "example-fleet"},
		{name: "a Google account written with capitals, which no token carries",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Account = "Example-Fleet@example-project.iam.gserviceaccount.com" })},
			names:    "google entry 1"},
		{name: "a Google account with no service",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Service = " " })}, names: pinnedAccount},
		{name: "a machine pinned to a project and no instance pattern",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.CloudProject = cloudProject })}, names: pinnedAccount},
		{name: "a machine pinned to an instance pattern and no project",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Instance = instanceGlob })}, names: pinnedAccount},
		{name: "a machine whose instance pattern is not a pattern",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.CloudProject, e.Instance = cloudProject, "example-[vm" })},
			names:    "example-[vm"},
		{name: "a machine named a person and no project",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Person = person })}, names: pinnedAccount},
		{name: "a machine named a project and no person",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Project = project })}, names: pinnedAccount},
		{name: "a machine whose fixed session carries whitespace",
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Session = "example fleet" })}, names: pinnedAccount},
		{name: "the same Google account twice",
			machines: []exchange.GoogleEntry{machine, withMachine(func(e *exchange.GoogleEntry) { e.Service = poolService })}, names: "google entry 2"},
		{name: "a session name a machine and an account fix",
			accounts: []exchange.AccountEntry{with(func(e *exchange.AccountEntry) { e.Session = fixedSession })},
			machines: []exchange.GoogleEntry{withMachine(func(e *exchange.GoogleEntry) { e.Session = fixedSession })}, names: pinnedAccount},
		{name: "a result workflow whose repository is not <owner>/<name>",
			results: []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) { e.Repository = "example-repo" })}, names: "example-repo"},
		{name: "a result workflow with no path",
			results: []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) { e.Workflow = " " })}, names: "result workflow entry 1"},
		{name: "a result workflow with no ref pattern", results: withPattern(""), names: "result workflow entry 1"},
		{name: "a result workflow with no project",
			results: []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) { e.Project = "" })}, names: resultFlow},
		{name: "a result workflow with no person",
			results: []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) { e.Person = " " })}, names: resultFlow},
		{name: "a ref pattern of another prefix", results: withPattern("refs/remotes/origin/*"), names: "refs/remotes/origin/*"},
		{name: "a ref pattern that is no ref at all", results: withPattern("main"), names: "main"},
		{name: "a ref pattern that is only its prefix's first segment", results: withPattern("refs/heads"), names: "refs/heads"},
		{name: "a wildcard in a ref pattern's second segment", results: withPattern("refs/*/main"), names: "refs/*/main"},
		{name: "a single-character wildcard in a ref pattern's second segment", results: withPattern("refs/he?ds/main"), names: "refs/he?ds/main"},
		{name: "a class in a ref pattern's first segment", results: withPattern("[r]efs/heads/main"), names: "[r]efs/heads/main"},
		// The refusal quotes the entry, so its backslash reads doubled; the entry's number names it.
		{name: "an escape in a ref pattern's second segment", results: withPattern(`refs/he\ads/main`), names: "result workflow entry 1"},
		{name: "a ref pattern that is not a pattern", results: withPattern("refs/heads/probe/[x"), names: "refs/heads/probe/[x"},
		{name: "the same result workflow and pattern twice",
			results: []exchange.ResultWorkflowEntry{result, withResult(func(e *exchange.ResultWorkflowEntry) { e.Person = "bob@example.com" })}, names: "result workflow entry 2"},
		{name: "a workflow listed for its exact runs and for its results",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Workflow = resultFlow })},
			results:   []exchange.ResultWorkflowEntry{result}, names: "result workflow entry 1"},
		{name: "a result workflow whose session an exact workflow fixes",
			workflows: []exchange.WorkflowEntry{withFlow(func(e *exchange.WorkflowEntry) { e.Session = resultSession })},
			results:   []exchange.ResultWorkflowEntry{result}, names: "result workflow entry 1"},
		{name: "a result workflow whose session name would be too long",
			results: []exchange.ResultWorkflowEntry{withResult(func(e *exchange.ResultWorkflowEntry) {
				e.Workflow = ".github/workflows/" + strings.Repeat("w", exchange.MaxSessionBytes) + ".yaml"
			})}, names: "result workflow entry 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := exchange.NewSet(c.accounts, c.workflows, c.results, c.machines, signInDomain)
			if err == nil {
				t.Fatalf("NewSet(%s) = %v, want a refusal", c.name, set)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("NewSet(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestNewSetTakesTheSameAccountAndWorkflowKeyTwiceApart(t *testing.T) {
	set, err := exchange.NewSet(
		[]exchange.AccountEntry{{Account: workerAccount, Project: project, Person: person}},
		[]exchange.WorkflowEntry{
			{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
			{Repository: repository, Workflow: workflow, Ref: "refs/heads/release", Project: "OTHER", Person: person},
		},
		nil, nil, signInDomain,
	)
	if err != nil {
		t.Fatalf("NewSet(one workflow on two refs): %v", err)
	}
	for _, c := range []struct {
		gitRef string
		want   string
	}{{ref, project}, {"refs/heads/release", "OTHER"}} {
		claims, err := set.Workflow(workflowToken(repository, repository+"/"+workflow+"@"+c.gitRef, repository+"/"+workflow+"@"+c.gitRef, c.gitRef), "release-bot")
		if err != nil || claims.Project != c.want {
			t.Errorf("Workflow(on %s) = %+v, %v; want project %s", c.gitRef, claims, err, c.want)
		}
	}
}

// TestCheckRefPatternTakesALiteralKindOfRef pins the one grammar of a result entry's ref pattern,
// which the configuration and NewSet both refuse by: path.Match's syntax, under refs/heads/,
// refs/pull/ or refs/tags/, with nothing but literal text before the second `/`. A wildcard after
// it is the pattern's whole point, the pull request's number and the merge queue's branch
// among them, so a pattern of one segment past the prefix is taken too.
func TestCheckRefPatternTakesALiteralKindOfRef(t *testing.T) {
	for _, good := range []string{probeRef, pullPattern, queuePattern, "refs/tags/v*", "refs/heads/*", "refs/heads/release-[0-9]*"} {
		if err := exchange.CheckRefPattern(good); err != nil {
			t.Errorf("CheckRefPattern(%q) = %v, want it taken", good, err)
		}
	}
	for _, bad := range []string{"", "main", "refs", "refs/heads", "heads/main", "refs/remotes/origin/main", "refs/notes/*",
		"refs/*/main", "refs/*", "*/heads/main", "refs/he?ds/main", "refs/[h]eads/main", `refs/he\ads/main`, `refs\/heads/main`, "refs/heads/[x"} {
		if err := exchange.CheckRefPattern(bad); err == nil {
			t.Errorf("CheckRefPattern(%q) took it, want a refusal", bad)
		}
	}
}

// TestAResultWorkflowIsNamedOnceAsOneHolder: every pattern of one result workflow writes under
// one session that belongs to one principal, so listing the workflow with three patterns
// reserves its one name once, for the workflow, and no pattern is a holder of its own.
func TestAResultWorkflowIsNamedOnceAsOneHolder(t *testing.T) {
	if got := exchange.ResultSession(repository, resultFlow); got != resultSession {
		t.Fatalf("ResultSession = %q, want %q", got, resultSession)
	}
	set, err := exchange.NewSet(nil, nil, resultEntries(probeRef, pullPattern, queuePattern), nil, signInDomain)
	if err != nil {
		t.Fatalf("NewSet(one result workflow, three patterns): %v", err)
	}
	want := []exchange.Reservation{{Session: resultSession, Principal: resultPrincipal}}
	if got := set.Reservations(); !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: every session name an entry fixes is reserved for
// that entry's principal, in the order the entries were listed, and a name no entry fixes is not
// reserved at all. The answer is the caller's own copy, so the set stays as it was built.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	set := listed(t)
	want := []exchange.Reservation{
		{Session: fixedSession, Principal: daemonPrincipal},
		{Session: nightlySession, Principal: nightlyPrincipal},
		{Session: resultSession, Principal: resultPrincipal},
		{Session: machineSession, Principal: fixedPrincipal},
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
// account writes under the configuration's name and is registered for nothing.
func TestPodAnswersTheListedAccount(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: podToken(workerAccount, podUID), session: "implementer-g1",
			want: exchange.Claims{Session: "implementer-g1", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: workerPrincipal, Instance: podUID}},
		{name: "the session the entry fixes, the request naming none", token: podToken(daemonAccount, podUID), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
		{name: "the session the entry fixes, the request naming the same", token: podToken(daemonAccount, podUID), session: fixedSession,
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
		{name: "another pod of the account is the same principal", token: podToken(daemonAccount, "another-pod-uid"), session: "",
			want: exchange.Claims{Session: fixedSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: daemonPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session)
			if err != nil || claims != c.want {
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
		{name: "no session where the entry fixes none", token: podToken(workerAccount, podUID), session: "",
			want: exchange.ErrSessionRequired, names: workerAccount},
		{name: "a session carrying whitespace", token: podToken(workerAccount, podUID), session: "implementer g1",
			want: exchange.ErrSessionInvalid, names: workerAccount},
		{name: "a session longer than the cap", token: podToken(workerAccount, podUID), session: strings.Repeat("s", exchange.MaxSessionBytes+1),
			want: exchange.ErrSessionInvalid, names: workerAccount},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session)
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
// one of its pods would say something no exchange reads.
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
	_, err := set.Pod(podToken(workerAccount, podUID), unusable)
	if !errors.Is(err, exchange.ErrSessionInvalid) {
		t.Fatalf("Pod(an unprintable session) = %v, want ErrSessionInvalid", err)
	}
	if strings.Contains(err.Error(), unusable) {
		t.Errorf("Pod refused with %q, which repeats what the request sent", err)
	}
}

func TestWorkflowAnswersTheListedRun(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the session the request names", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "release-bot", want: exchange.Claims{Session: "release-bot", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: releasePrincipal}},
		{name: "the session the entry fixes", token: workflowToken(repository, runRef(nightly), runRef(nightly), ref),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: nightlyPrincipal}},
		{name: "another run of the workflow is the same principal", token: withRun(workflowToken(repository, runRef(nightly), runRef(nightly), ref), "4243"),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: nightlyPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if err != nil || claims != c.want {
				t.Fatalf("Workflow(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

func TestWorkflowRefusesWhatItCannotDecide(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a token with no repository", token: workflowToken("", runRef(workflow), runRef(workflow), ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "repository"},
		{name: "a token with no workflow_ref", token: workflowToken(repository, "", runRef(workflow), ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "workflow_ref"},
		{name: "a token with no job_workflow_ref", token: workflowToken(repository, runRef(workflow), "", ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "job_workflow_ref"},
		{name: "a token with no ref", token: workflowToken(repository, runRef(workflow), runRef(workflow), ""),
			session: "release-bot", want: exchange.ErrClaimMissing, names: "ref"},
		{name: "a workflow_ref that names no path", token: workflowToken(repository, repository+"/"+workflow, repository+"/"+workflow, ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: repository},
		{name: "a workflow_ref under another repository", token: workflowToken(repository, "example-org/other-repo/"+workflow+"@"+ref, "example-org/other-repo/"+workflow+"@"+ref, ref),
			session: "release-bot", want: exchange.ErrClaimMissing, names: repository},
		{name: "a job written in a reusable workflow", token: workflowToken(repository, runRef(workflow), runRef(".github/workflows/reusable.yaml"), ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: ".github/workflows/reusable.yaml"},
		{name: "a workflow no entry lists", token: workflowToken(repository, runRef(".github/workflows/other.yaml"), runRef(".github/workflows/other.yaml"), ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: ".github/workflows/other.yaml"},
		{name: "a ref no entry lists", token: workflowToken(repository, repository+"/"+workflow+"@refs/heads/topic", repository+"/"+workflow+"@refs/heads/topic", "refs/heads/topic"),
			session: "release-bot", want: exchange.ErrNoEntry, names: "refs/heads/topic"},
		{name: "a repository no entry lists", token: workflowToken("example-org/other-repo", "example-org/other-repo/"+workflow+"@"+ref, "example-org/other-repo/"+workflow+"@"+ref, ref),
			session: "release-bot", want: exchange.ErrNoEntry, names: "example-org/other-repo"},
		{name: "a session the entry does not fix", token: workflowToken(repository, runRef(nightly), runRef(nightly), ref),
			session: "release-bot", want: exchange.ErrSessionMismatch, names: nightlySession},
		{name: "no session where the entry fixes none", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "", want: exchange.ErrSessionRequired, names: workflow},
		{name: "a session carrying whitespace", token: workflowToken(repository, runRef(workflow), runRef(workflow), ref),
			session: "release bot", want: exchange.ErrSessionInvalid, names: workflow},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Workflow(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Workflow(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// withRun is token as another run of the same workflow carries it.
func withRun(token map[string]any, run string) map[string]any {
	token["run_id"] = run
	return token
}

// TestWorkflowAnswersAResultRunWithItsCapabilityAndItsRun pins what a result entry answers: its
// project and person, the session and principal its workflow fixes whatever the ref, the one
// capability, and the run read off the token. Each of the three patterns is reached by a ref of
// its own, the pull request's and the merge queue's by refs the patterns' `*` stands for, and a
// request may name the fixed session or none. A check run given as a JSON number, which a
// decoded claim map holds as float64 or as json.Number, is read as the same decimal string, and
// a token carrying none answers none, since GitHub's older tokens do not carry it.
func TestWorkflowAnswersAResultRunWithItsCapabilityAndItsRun(t *testing.T) {
	set := listed(t)
	claimsFor := func(run *oauthwire.CIRun) exchange.Claims {
		return exchange.Claims{Session: resultSession, Person: person, Scope: exchange.ProjectScopeNamed, Project: project,
			Principal: resultPrincipal, Capability: oauthwire.CapabilityCIResults, CI: run}
	}
	const pullRef, queueRef = "refs/pull/7/merge", "refs/heads/gh-readonly-queue/main/pr-7-" + commit
	withoutCheckRun := resultToken(pullRef)
	delete(withoutCheckRun, "check_run_id")
	runWithoutCheckRun := resultRun(pullRef)
	runWithoutCheckRun.CheckRunID = ""
	numbered := func(value any) map[string]any {
		token := resultToken(pullRef)
		token["check_run_id"] = value
		return token
	}
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "the probe branch, naming no session", token: resultToken(probeRef), want: claimsFor(resultRun(probeRef))},
		{name: "a pull request's merge ref", token: resultToken(pullRef), want: claimsFor(resultRun(pullRef))},
		{name: "the merge queue's branch", token: resultToken(queueRef), want: claimsFor(resultRun(queueRef))},
		{name: "the request naming the fixed session", token: resultToken(pullRef), session: resultSession, want: claimsFor(resultRun(pullRef))},
		{name: "a token carrying no check run", token: withoutCheckRun, want: claimsFor(runWithoutCheckRun)},
		{name: "a check run decoded as a float", token: numbered(float64(987654321)), want: claimsFor(resultRun(pullRef))},
		{name: "a check run decoded as a json.Number", token: numbered(json.Number("987654321")), want: claimsFor(resultRun(pullRef))},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Workflow(%s) = %+v (run %+v), %v; want %+v (run %+v)", c.name, claims, claims.CI, err, c.want, c.want.CI)
			}
		})
	}
}

// TestWorkflowRefusesAResultRunItCannotDecide: a ref no pattern matches, `*` included, which
// never crosses a `/`, is no entry's; a run whose token lacks a claim of the run, or carries one
// out of its shape, is refused naming the claim, since the run it records must be the run that
// asked; a request naming another session is refused as an exact entry's is; and a job written
// in another workflow is refused as it is for an exact entry.
func TestWorkflowRefusesAResultRunItCannotDecide(t *testing.T) {
	set := listed(t)
	const pullRef = "refs/pull/7/merge"
	spoiled := func(claim string, value any) map[string]any {
		token := resultToken(pullRef)
		if value == nil {
			delete(token, claim)
		} else {
			token[claim] = value
		}
		return token
	}
	reusable := resultToken(pullRef)
	reusable["job_workflow_ref"] = repository + "/.github/workflows/reusable.yaml@" + pullRef
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    error
		names   string
	}{
		{name: "a branch no pattern matches", token: resultToken("refs/heads/other"), want: exchange.ErrNoEntry, names: "refs/heads/other"},
		{name: "a ref under the pull request's whose * would cross a /", token: resultToken("refs/pull/7/8/merge"), want: exchange.ErrNoEntry, names: "refs/pull/7/8/merge"},
		{name: "a merge queue ref one segment deeper", token: resultToken("refs/heads/gh-readonly-queue/main/a/b"), want: exchange.ErrNoEntry, names: "refs/heads/gh-readonly-queue/main/a/b"},
		{name: "the probe branch's child", token: resultToken(probeRef + "/x"), want: exchange.ErrNoEntry, names: probeRef + "/x"},
		{name: "a token carrying no sha", token: spoiled("sha", nil), want: exchange.ErrClaimMissing, names: "sha"},
		{name: "a sha in capitals", token: spoiled("sha", strings.ToUpper(commit)), want: exchange.ErrClaimMissing, names: "sha"},
		{name: "a sha one digit short", token: spoiled("sha", commit[1:]), want: exchange.ErrClaimMissing, names: "sha"},
		{name: "a sha that is not hexadecimal", token: spoiled("sha", "g"+commit[1:]), want: exchange.ErrClaimMissing, names: "sha"},
		{name: "a token carrying no run_id", token: spoiled("run_id", nil), want: exchange.ErrClaimMissing, names: "run_id"},
		{name: "a run_id that is not decimal", token: spoiled("run_id", "42a"), want: exchange.ErrClaimMissing, names: "run_id"},
		{name: "a run_id of zero", token: spoiled("run_id", "0"), want: exchange.ErrClaimMissing, names: "run_id"},
		{name: "a run_id with a leading zero", token: spoiled("run_id", "04242"), want: exchange.ErrClaimMissing, names: "run_id"},
		{name: "a run_id with a sign", token: spoiled("run_id", "+4242"), want: exchange.ErrClaimMissing, names: "run_id"},
		{name: "a token carrying no run_attempt", token: spoiled("run_attempt", nil), want: exchange.ErrClaimMissing, names: "run_attempt"},
		{name: "a run_attempt that is not decimal", token: spoiled("run_attempt", "two"), want: exchange.ErrClaimMissing, names: "run_attempt"},
		{name: "a token carrying no event_name", token: spoiled("event_name", nil), want: exchange.ErrClaimMissing, names: "event_name"},
		{name: "a token carrying no runner_environment", token: spoiled("runner_environment", nil), want: exchange.ErrClaimMissing, names: "runner_environment"},
		{name: "a token carrying no actor", token: spoiled("actor", nil), want: exchange.ErrClaimMissing, names: "actor"},
		{name: "a check_run_id that is not decimal", token: spoiled("check_run_id", "abc"), want: exchange.ErrClaimMissing, names: "check_run_id"},
		{name: "a check_run_id that is a fraction", token: spoiled("check_run_id", 1.5), want: exchange.ErrClaimMissing, names: "check_run_id"},
		{name: "a check_run_id of another type", token: spoiled("check_run_id", true), want: exchange.ErrClaimMissing, names: "check_run_id"},
		{name: "a session the entry does not fix", token: resultToken(pullRef), session: "ci-release", want: exchange.ErrSessionMismatch, names: resultSession},
		{name: "a job written in a reusable workflow", token: reusable, want: exchange.ErrNoEntry, names: ".github/workflows/reusable.yaml"},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(c.token, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Workflow(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Workflow(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestARunTwoResultEntriesMatchIsRefusedNamingBoth: two patterns of one workflow can both match
// one ref, which no load-time check can rule out for patterns in general, and the two may name
// different projects or people. The run is refused, naming both patterns, rather than answered
// by whichever entry was listed first; a ref only one of them matches is answered by it.
func TestARunTwoResultEntriesMatchIsRefusedNamingBoth(t *testing.T) {
	const wide, narrow = "refs/heads/probe/*", "refs/heads/probe/infra-*"
	set, err := exchange.NewSet(nil, nil, resultEntries(wide, narrow), nil, signInDomain)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	_, err = set.Workflow(resultToken(probeRef), "")
	if !errors.Is(err, exchange.ErrNoEntry) {
		t.Fatalf("Workflow(a ref both patterns match) = %v, want %v", err, exchange.ErrNoEntry)
	}
	for _, want := range []string{"two entries match", wide, narrow} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal reads %q, which does not carry %q", err, want)
		}
	}
	if claims, err := set.Workflow(resultToken("refs/heads/probe/other"), ""); err != nil || claims.CI == nil || claims.CI.Ref != "refs/heads/probe/other" {
		t.Fatalf("Workflow(a ref only %s matches) = %+v, %v; want its run answered", wide, claims, err)
	}
}

func TestMachineActsForTheApproverAndWritesEveryProjectThePersonMay(t *testing.T) {
	set := listed(t)
	claims, err := set.Machine(credentialID, person, "ada-devbox")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	want := exchange.Claims{Session: "ada-devbox", Person: person, Scope: exchange.ProjectScopePerson, Principal: personPrincipal}
	if claims != want {
		t.Fatalf("Machine = %+v, want %+v: a machine credential names no project and says which absence that is", claims, want)
	}
	// Another machine of the same person is the same principal: the name belongs to the person.
	other, err := set.Machine("77777777-8888-9999-aaaa-bbbbbbbbbbbb", person, "ada-devbox")
	if err != nil || other != want {
		t.Fatalf("Machine(another credential of %s) = %+v, %v; want %+v", person, other, err, want)
	}
}

// TestEveryDecisionScopesItsProject: the zero ProjectScope is what a Claims nobody built carries,
// so no constructor may answer it; a later project check refuses a token that names neither a
// project nor the person's whole reach.
func TestEveryDecisionScopesItsProject(t *testing.T) {
	set := listed(t)
	pod, err := set.Pod(podToken(workerAccount, podUID), "implementer-g1")
	if err != nil {
		t.Fatalf("Pod: %v", err)
	}
	run, err := set.Workflow(workflowToken(repository, runRef(workflow), runRef(workflow), ref), "release-bot")
	if err != nil {
		t.Fatalf("Workflow: %v", err)
	}
	machine, err := set.Machine(credentialID, person, "ada-devbox")
	if err != nil {
		t.Fatalf("Machine: %v", err)
	}
	for _, c := range []struct {
		name   string
		claims exchange.Claims
		want   exchange.ProjectScope
	}{
		{name: "Pod", claims: pod, want: exchange.ProjectScopeNamed},
		{name: "Workflow", claims: run, want: exchange.ProjectScopeNamed},
		{name: "Machine", claims: machine, want: exchange.ProjectScopePerson},
	} {
		if c.claims.Scope != c.want {
			t.Errorf("%s scope = %v, want %v", c.name, c.claims.Scope, c.want)
		}
		if (c.claims.Project != "") != (c.want == exchange.ProjectScopeNamed) {
			t.Errorf("%s = %+v: a named scope names a project and no other scope does", c.name, c.claims)
		}
	}
	if got := exchange.ProjectScopeUnset.String(); !strings.Contains(got, "no project scope") {
		t.Errorf("ProjectScopeUnset.String() = %q, want it to say the scope was never set", got)
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
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Machine(c.credential, c.approver, c.session)
			if !errors.Is(err, c.want) {
				t.Fatalf("Machine(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Machine(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

func TestSessionAtTheCapIsUsable(t *testing.T) {
	set := listed(t)
	name := strings.Repeat("s", exchange.MaxSessionBytes)
	claims, err := set.Pod(podToken(workerAccount, podUID), name)
	if err != nil || claims.Session != name {
		t.Fatalf("Pod(a session of exactly %d bytes) = %+v, %v; want it answered", exchange.MaxSessionBytes, claims, err)
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
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "another machine of that account is the same principal",
			token: googleToken(pinnedAccount, true, cloudProject, "example-vm-8"), session: "example-other",
			want: exchange.Claims{Session: "example-other", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
		{name: "email_verified as the string Google writes on some of its tokens",
			token: googleToken(pinnedAccount, "true", cloudProject, instanceName), session: "example-machine",
			want: exchange.Claims{Session: "example-machine", Person: person, Scope: exchange.ProjectScopeNamed, Project: project, Principal: pinnedPrincipal, Fleet: pinnedService}},
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
			claims, err := set.Google(c.token, c.session)
			if err != nil || claims != c.want {
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
			claims, err := set.Google(c.token, c.session)
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
		_, err := set.Google(token, "example-machine")
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
			claims, err := set.Person(c.token, c.actor, c.session)
			if err != nil || claims != c.want {
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
			claims, err := set.Person(c.token, c.actor, c.session)
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
		_, err := set.Person(c.token, c.actor, "example-box")
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

// TestAnEmptySignInDomainAdmitsNobody: a set built with no domain refuses every person, whatever
// their token says, rather than taking every account outside any Workspace. It is the rule
// oidc.WorkspaceLogin states, and the one a broker told about no domain has to follow.
func TestAnEmptySignInDomainAdmitsNobody(t *testing.T) {
	set, err := exchange.NewSet(nil, nil, nil, []exchange.GoogleEntry{{Account: poolAccount, Service: poolService}}, "")
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	for _, token := range []map[string]any{
		personToken(person, true, signInDomain),
		personToken(person, true, ""),
	} {
		if _, err := set.Person(token, googleToken(poolAccount, true, "", ""), "example-box"); !errors.Is(err, exchange.ErrNotInDomain) {
			t.Errorf("Person(hd %v) = %v, want %v: an empty configured domain admits nobody", token["hd"], err, exchange.ErrNotInDomain)
		}
	}
}

// TestReadGoogleIdentityReadsGooglesClaimShapes: the one reading of a Google identity token's own
// claims, which this package decides on and the broker's VM token verifier reads the same token
// through. A claim of another shape reads as absent, and a token minted off a Compute Engine
// instance reads as carrying no instance rather than as a malformed one.
func TestReadGoogleIdentityReadsGooglesClaimShapes(t *testing.T) {
	whole := exchange.ReadGoogleIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	if whole.Account != pinnedAccount || !whole.EmailVerified || !whole.Compute.Whole() || whole.Compute.Empty() {
		t.Fatalf("ReadGoogleIdentity(a VM's token) = %+v, want the account, a verified email and the whole instance", whole)
	}
	if whole.Compute.Project != cloudProject || whole.Compute.Instance != instanceName ||
		whole.Compute.Zone != "example-zone-a" || whole.Compute.InstanceID != "1234567890" {
		t.Errorf("ReadGoogleIdentity read the instance as %+v", whole.Compute)
	}
	none := exchange.ReadGoogleIdentity(googleToken(poolAccount, "true", "", ""))
	if none.Account != poolAccount || !none.EmailVerified || !none.Compute.Empty() || none.Compute.Whole() {
		t.Fatalf("ReadGoogleIdentity(a pool's token) = %+v, want the account, a verified email and no instance", none)
	}
	odd := exchange.ReadGoogleIdentity(map[string]any{"email": 7, "email_verified": "yes", "google": "not an object"})
	if odd != (exchange.GoogleIdentity{}) {
		t.Errorf("ReadGoogleIdentity(claims of other shapes) = %+v, want every field empty", odd)
	}
}

// TestMachineSubjectNamesOneMachineAndTheKeyItHolds: the subject the broker keys a machine's
// launcher credential by. A machine is named by where it runs and by the key its proof
// presented, so a copy of its Google identity token presented with another key asks for a
// subject of its own and moves nothing of the machine's; two machines of one account are two
// credentials; and two instances of one account presenting one key are two subjects, which the
// live-key index refuses rather than letting them share a credential. Every one of them reads
// its account and its key back, and a Kubernetes ServiceAccount reads as no machine at all.
func TestMachineSubjectNamesOneMachineAndTheKeyItHolds(t *testing.T) {
	onInstance := exchange.ReadGoogleIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	onNone := exchange.ReadGoogleIdentity(googleToken(poolAccount, true, "", ""))

	instanceSubject := exchange.MachineSubject(onInstance, "thumbprint-one")
	if want := pinnedAccount + "@" + cloudProject + "/example-zone-a/1234567890#thumbprint-one"; instanceSubject != want {
		t.Fatalf("MachineSubject(a Compute Engine machine) = %q, want %q", instanceSubject, want)
	}
	if other := exchange.MachineSubject(onInstance, "thumbprint-two"); other == instanceSubject {
		t.Errorf("another key on the same instance asks for the subject %q the machine's own credential holds; a copied token must move no credential", instanceSubject)
	}
	first := exchange.MachineSubject(onNone, "thumbprint-one")
	if want := poolAccount + "#thumbprint-one"; first != want {
		t.Fatalf("MachineSubject(a machine minted on no instance) = %q, want %q", first, want)
	}
	if second := exchange.MachineSubject(onNone, "thumbprint-two"); second == first {
		t.Errorf("two machines of %s with keys of their own share the subject %q; each must hold a credential of its own", poolAccount, first)
	}
	for _, c := range []struct {
		subject string
		want    exchange.Subject
	}{
		{subject: instanceSubject, want: exchange.Subject{Kind: exchange.SubjectMachine, Account: pinnedAccount}},
		{subject: first, want: exchange.Subject{Kind: exchange.SubjectMachine, Account: poolAccount}},
		{subject: "system:serviceaccount:legion-system:legion-daemon"},
		{subject: pinnedAccount},
		{subject: "#thumbprint-one"},
		{subject: ""},
	} {
		if got := exchange.ReadSubject(c.subject); got != c.want {
			t.Errorf("ReadSubject(%q) = %+v, want %+v", c.subject, got, c.want)
		}
	}
}

// TestPersonSubjectNamesOneSignInAndReadsItsMachineBack: a sign-in's subject tells one person's
// sign-in on one box from every other — two people on one box, and one person on two boxes, are
// four different strings — and the machine's account reads back out of it, which is what holds
// the credential to a listed fleet on every call. No subject of any other kind reads as a
// sign-in's, and a sign-in's never reads as a machine's, whose credential acts for nobody.
func TestPersonSubjectNamesOneSignInAndReadsItsMachineBack(t *testing.T) {
	const (
		boxOne = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
		boxTwo = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	)
	subjects := map[string]struct {
		subject, account string
	}{
		"one person on one box":                  {exchange.PersonSubject(person, pinnedAccount, boxOne), pinnedAccount},
		"the same person on another box":         {exchange.PersonSubject(person, pinnedAccount, boxTwo), pinnedAccount},
		"the same person on another fleet's box": {exchange.PersonSubject(person, poolAccount, boxOne), poolAccount},
		"another person on the first box":        {exchange.PersonSubject(stranger, pinnedAccount, boxOne), pinnedAccount},
	}
	seen := map[string]string{}
	for name, c := range subjects {
		if other, taken := seen[c.subject]; taken {
			t.Errorf("%s and %s spell one subject, %q: each sign-in holds a credential of its own", name, other, c.subject)
		}
		seen[c.subject] = name
		want := exchange.Subject{Kind: exchange.SubjectSignIn, Account: c.account}
		if got := exchange.ReadSubject(c.subject); got != want {
			t.Errorf("ReadSubject(%q) = %+v, want %+v: it is what holds the sign-in to a listed fleet", c.subject, got, want)
		}
	}
	onInstance := exchange.ReadGoogleIdentity(googleToken(pinnedAccount, true, cloudProject, instanceName))
	onNone := exchange.ReadGoogleIdentity(googleToken(poolAccount, true, "", ""))
	for _, subject := range []string{
		exchange.MachineSubject(onInstance, boxOne),
		exchange.MachineSubject(onNone, boxOne),
		workerAccount,
		"signin:" + person,
		"signin:|" + pinnedAccount + "#" + boxOne,
		"",
	} {
		if got := exchange.ReadSubject(subject); got.Kind == exchange.SubjectSignIn {
			t.Errorf("ReadSubject(%q) = %+v; want no sign-in: it is a subject of another kind", subject, got)
		}
	}
}
