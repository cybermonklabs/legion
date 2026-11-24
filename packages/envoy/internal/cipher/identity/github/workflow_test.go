package github_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
)

const (
	repository = "example-org/example-repo"
	workflow   = ".github/workflows/release.yaml"
	nightly    = ".github/workflows/nightly.yaml"
	ref        = "refs/heads/main"
	project    = "ACME"
	person     = "ada@example.com"
	// nightlySession is the session name the nightly workflow's entry fixes.
	nightlySession = "ci-nightly"
)

// The principals the listed entries' session names belong to, as the exchange spells them.
const (
	releasePrincipal = "workflow:" + repository + "/" + workflow + "@" + ref
	nightlyPrincipal = "workflow:" + repository + "/" + nightly + "@" + ref
)

// listed is the provider every decision test asks: one workflow whose sessions name themselves
// and one whose session the entry fixes, of one repository on one ref.
func listed(t *testing.T) *github.Provider {
	t.Helper()
	p, err := github.New(github.Config{Workflows: []github.WorkflowEntry{
		{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
		{Repository: repository, Workflow: nightly, Ref: ref, Project: project, Person: person, Session: nightlySession},
	}})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}
	return p
}

// build is the provider of workflows composed into a registry, which is where a session name two
// entries fix is refused.
func build(workflows []github.WorkflowEntry) (*identity.Registry, error) {
	p, err := github.New(github.Config{Workflows: workflows})
	if err != nil {
		return nil, err
	}
	return identity.NewRegistry([]identity.Provider{p}, nil)
}

// decide is a run's trade of token naming session and projects.
func decide(p *github.Provider, token map[string]any, session string, projects []string) (exchange.Claims, error) {
	return p.Decide(identity.Trade{TokenType: github.SubjectTokenType, Subject: token, Session: session, Projects: projects})
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

// withRun is token as another run of the same workflow carries it.
func withRun(token map[string]any, run string) map[string]any {
	token["run_id"] = run
	return token
}

func TestNewSetRefusesAnEntryItCannotServe(t *testing.T) {
	flow := github.WorkflowEntry{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person}
	withFlow := func(change func(*github.WorkflowEntry)) github.WorkflowEntry {
		entry := flow
		change(&entry)
		return entry
	}
	for _, c := range []struct {
		name      string
		workflows []github.WorkflowEntry
		names     string
	}{
		{name: "a repository that is not <owner>/<name>",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "example-repo" })}, names: "example-repo"},
		// A repository's owner is a GitHub login, so each owner below is refused for what no
		// login holds.
		{name: "a repository whose owner holds a dot, which only a repository name may",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "a.b/example-repo" })}, names: "a.b/example-repo"},
		{name: "a repository whose owner holds a slash, which makes it no <owner>/<name>",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "a/b/example-repo" })}, names: "a/b/example-repo"},
		{name: "a repository whose owner starts with a hyphen, which no login does",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "-ada/example-repo" })}, names: "-ada/example-repo"},
		{name: "a repository whose owner starts with an underscore, which no login does",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "_ada/example-repo" })}, names: "_ada/example-repo"},
		{name: "a repository whose owner is 40 characters, past GitHub's 39",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = strings.Repeat("a", 40) + "/example-repo" })},
			names:     strings.Repeat("a", 40) + "/example-repo"},
		{name: "a repository with no owner",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Repository = "/example-repo" })}, names: "/example-repo"},
		{name: "a workflow with no path",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Workflow = "" })}, names: repository},
		{name: "a workflow with no ref",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Ref = "" })}, names: workflow},
		{name: "a workflow with no project",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Project = "" })}, names: workflow},
		{name: "a workflow with no person",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Person = " " })}, names: workflow},
		{name: "a workflow whose fixed session is unprintable",
			workflows: []github.WorkflowEntry{withFlow(func(e *github.WorkflowEntry) { e.Session = "legion\x00daemon" })}, names: workflow},
		{name: "the same workflow twice",
			workflows: []github.WorkflowEntry{flow, withFlow(func(e *github.WorkflowEntry) { e.Person = "bob@example.com" })}, names: "workflow entry 2"},
		{name: "two workflows whose principals spell alike",
			workflows: []github.WorkflowEntry{
				withFlow(func(e *github.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml@refs/heads/b", "c" }),
				withFlow(func(e *github.WorkflowEntry) { e.Workflow, e.Ref = ".github/workflows/a.yaml", "refs/heads/b@c" }),
			}, names: "workflow entry 1"},
		{name: "a session name two workflows fix",
			workflows: []github.WorkflowEntry{
				withFlow(func(e *github.WorkflowEntry) { e.Session = nightlySession }),
				{Repository: repository, Workflow: nightly, Ref: ref, Project: project, Person: person, Session: nightlySession},
			}, names: "workflow entry 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			set, err := build(c.workflows)
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
	set, err := github.New(github.Config{Workflows: []github.WorkflowEntry{
		{Repository: repository, Workflow: workflow, Ref: ref, Project: project, Person: person},
		{Repository: repository, Workflow: workflow, Ref: "refs/heads/release", Project: "OTHER", Person: person},
	}})
	if err != nil {
		t.Fatalf("NewSet(one workflow on two refs): %v", err)
	}
	for _, c := range []struct {
		gitRef string
		want   string
	}{{ref, project}, {"refs/heads/release", "OTHER"}} {
		claims, err := decide(set, workflowToken(repository, repository+"/"+workflow+"@"+c.gitRef, repository+"/"+workflow+"@"+c.gitRef, c.gitRef), "release-bot", nil)
		if err != nil || !slices.Equal(claims.Projects, []string{c.want}) {
			t.Errorf("Workflow(on %s) = %+v, %v; want the projects [%s]", c.gitRef, claims, err, c.want)
		}
	}
}

// TestNewSetTakesAManagedUsersRepository: a managed user's login ends in `_<shortcode>`, so a
// workflow entry of a repository such an account owns is read, and a run of that workflow is the
// entry's.
func TestNewSetTakesAManagedUsersRepository(t *testing.T) {
	const managed = "mona-cat_octo/example-repo"
	set, err := github.New(github.Config{Workflows: []github.WorkflowEntry{{Repository: managed, Workflow: workflow, Ref: ref, Project: project, Person: person}}})
	if err != nil {
		t.Fatalf("NewSet(a workflow of %s): %v", managed, err)
	}
	run := managed + "/" + workflow + "@" + ref
	claims, err := decide(set, workflowToken(managed, run, run, ref), "release-bot", nil)
	if err != nil || !slices.Equal(claims.Projects, []string{project}) || claims.Person != person {
		t.Errorf("Workflow(a run of %s) = %+v, %v; want the projects [%s] for %s", managed, claims, err, project, person)
	}
}

// TestTheReservationsAreTheNamesTheEntriesFix: the session name the nightly entry fixes is
// reserved for its principal, and the release workflow, which fixes none, reserves nothing.
func TestTheReservationsAreTheNamesTheEntriesFix(t *testing.T) {
	registry, err := identity.NewRegistry([]identity.Provider{listed(t)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	want := []exchange.Reservation{{Session: nightlySession, Principal: nightlyPrincipal}}
	if got := registry.Policy().Reservations(); !slices.Equal(got, want) {
		t.Fatalf("Reservations = %+v, want %+v", got, want)
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
			session: "release-bot", want: exchange.Claims{Session: "release-bot", Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: releasePrincipal}},
		{name: "the session the entry fixes", token: workflowToken(repository, runRef(nightly), runRef(nightly), ref),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: nightlyPrincipal}},
		{name: "another run of the workflow is the same principal", token: withRun(workflowToken(repository, runRef(nightly), runRef(nightly), ref), "4243"),
			session: "", want: exchange.Claims{Session: nightlySession, Person: person, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Principal: nightlyPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := decide(set, c.token, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
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
			claims, err := decide(set, c.token, c.session, nil)
			if !errors.Is(err, c.want) {
				t.Fatalf("Workflow(%s) = %+v, %v; want %v", c.name, claims, err, c.want)
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("Workflow(%s) refused with %q, which does not name %q", c.name, err, c.names)
			}
		})
	}
}

// TestEveryDecisionCarriesItsProjects: a run carries its entry's one project under the named
// scope.
func TestEveryDecisionCarriesItsProjects(t *testing.T) {
	run, err := decide(listed(t), workflowToken(repository, runRef(workflow), runRef(workflow), ref), "release-bot", nil)
	if err != nil {
		t.Fatalf("Workflow: %v", err)
	}
	if run.Scope != exchange.ProjectScopeNamed || !slices.Equal(run.Projects, []string{project}) {
		t.Errorf("Workflow = scope %v, projects %v; want scope %v, projects %v", run.Scope, run.Projects, exchange.ProjectScopeNamed, []string{project})
	}
}
