package exchange_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/exchange"
)

// TestATradeNamesOnlyProjectsItsPersonSeesUnderItsEntry is the part of the check that this pure
// package can prove: an operator's entry decides its token's one project, so an account, a
// workflow and a Google entry each refuse a request naming any other; a token that names no
// person names no project at all; and a trade that acts as its person carries exactly the list it
// asked for, which the exchange then holds to what Dispatch says that person may see.
func TestATradeNamesOnlyProjectsItsPersonSeesUnderItsEntry(t *testing.T) {
	set := listed(t)
	other := []string{"OTHER"}
	both := []string{project, "OTHER"}
	for _, c := range []struct {
		name   string
		decide func([]string) (exchange.Claims, error)
	}{
		{"an account entry", func(p []string) (exchange.Claims, error) {
			return set.Pod(podToken(workerAccount, podUID), "implementer-g1", p)
		}},
		{"a workflow entry", func(p []string) (exchange.Claims, error) {
			return set.Workflow(workflowToken(repository, runRef(workflow), runRef(workflow), ref), "release-bot", p)
		}},
		{"a Google entry naming a person", func(p []string) (exchange.Claims, error) {
			return set.Google(googleToken(pinnedAccount, true, cloudProject, instanceName), "example-machine", p)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, asked := range [][]string{other, both} {
				claims, err := c.decide(asked)
				if !errors.Is(err, exchange.ErrProjectsFixed) || !strings.Contains(err.Error(), project) {
					t.Fatalf("a request naming %v under %s = %+v, %v; want ErrProjectsFixed naming the entry's %s", asked, c.name, claims, err, project)
				}
			}
			for _, asked := range [][]string{nil, {project}} {
				claims, err := c.decide(asked)
				if err != nil || !slices.Equal(claims.Projects, []string{project}) || claims.Scope != exchange.ProjectScopeNamed {
					t.Fatalf("a request naming %v under %s = %+v, %v; want the entry's one project, named", asked, c.name, claims, err)
				}
			}
		})
	}
	t.Run("a fleet's service", func(t *testing.T) {
		claims, err := set.Google(googleToken(poolAccount, true, "", ""), "example-pool-1", []string{project})
		if !errors.Is(err, exchange.ErrProjectsNoPerson) || !strings.Contains(err.Error(), poolAccount) {
			t.Fatalf("a fleet's service naming %s = %+v, %v; want ErrProjectsNoPerson naming its account", project, claims, err)
		}
	})
	t.Run("a read entry's service", func(t *testing.T) {
		claims, err := set.Pod(podToken(readerAccount, podUID), "", []string{project})
		if !errors.Is(err, exchange.ErrProjectsNoPerson) || !strings.Contains(err.Error(), readerAccount) {
			t.Fatalf("a read entry's pod naming %s = %+v, %v; want ErrProjectsNoPerson naming its account", project, claims, err)
		}
	})
	t.Run("a machine credential and a sign-in carry the asked list", func(t *testing.T) {
		asked := []string{"OTHER", "PRIV"}
		machine, err := set.Machine(credentialID, person, "ada-devbox", asked)
		if err != nil || !slices.Equal(machine.Projects, asked) {
			t.Fatalf("Machine(%v) = %+v, %v; want exactly that list", asked, machine, err)
		}
		signIn, err := set.Person(personToken(person, true, signInDomain), googleToken(poolAccount, true, "", ""), "example-box", asked)
		if err != nil || !slices.Equal(signIn.Projects, asked) {
			t.Fatalf("Person(%v) = %+v, %v; want exactly that list", asked, signIn, err)
		}
		asked[0] = "CHANGED"
		if machine.Projects[0] != "OTHER" || signIn.Projects[0] != "OTHER" {
			t.Fatalf("the claims share the caller's slice: %v, %v", machine.Projects, signIn.Projects)
		}
	})
}

// TestMappedProjects: the person and projects an operator's entry gives a principal, which a
// release reads for a pod or a machine with no token in hand; a principal no entry lists, and a
// person's own, are answered false.
func TestMappedProjects(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		principal string
		person    string
		projects  []string
		ok        bool
	}{
		{workerPrincipal, person, []string{project}, true},
		{daemonPrincipal, person, []string{project}, true},
		{releasePrincipal, person, []string{project}, true},
		{pinnedPrincipal, person, []string{project}, true},
		{poolPrincipal, "", nil, true},
		{readerPrincipal, "", nil, true},
		{personPrincipal, "", nil, false},
		{"account:system:serviceaccount:example-ns:example-other", "", nil, false},
		{"google:" + strangerAccount, "", nil, false},
		{workerAccount, "", nil, false},
	} {
		gotPerson, gotProjects, ok := set.MappedProjects(c.principal)
		if gotPerson != c.person || !slices.Equal(gotProjects, c.projects) || ok != c.ok {
			t.Errorf("MappedProjects(%q) = %q, %v, %v; want %q, %v, %v", c.principal, gotPerson, gotProjects, ok, c.person, c.projects, c.ok)
		}
	}
	_, projects, _ := set.MappedProjects(workerPrincipal)
	projects[0] = "CHANGED"
	if _, again, _ := set.MappedProjects(workerPrincipal); !slices.Equal(again, []string{project}) {
		t.Fatalf("MappedProjects after the caller changed its copy = %v, want [%s]: the set stays as it was built", again, project)
	}
}

// TestProjectsForAccountAnswersWhatTheTokenWouldCarry: the projects a token minted for one
// account carries, read from the very entries Pod and Google mint from, so a grant made from this
// answer and the token of that account cannot disagree.
func TestProjectsForAccountAnswersWhatTheTokenWouldCarry(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name     string
		account  string
		projects []string
		ok       bool
	}{
		{"a Kubernetes subject", workerAccount, []string{project}, true},
		{"a Google account naming a person", pinnedAccount, []string{project}, true},
		{"a Google account naming none", poolAccount, nil, true},
		{"an unlisted Kubernetes subject", "system:serviceaccount:example-ns:example-other", nil, false},
		{"an unlisted Google account", strangerAccount, nil, false},
		{"a workflow's repository, which is no account", repository, nil, false},
	} {
		got, ok := set.ProjectsForAccount(c.account)
		if !slices.Equal(got, c.projects) || ok != c.ok {
			t.Errorf("ProjectsForAccount(%s, %q) = %v, %v; want %v, %v", c.name, c.account, got, ok, c.projects, c.ok)
		}
		if c.ok {
			var minted exchange.Claims
			var err error
			if strings.HasPrefix(c.account, "system:serviceaccount:") {
				minted, err = set.Pod(podToken(c.account, podUID), "example-session", nil)
			} else {
				minted, err = set.Google(googleToken(c.account, true, cloudProject, instanceName), "example-session", nil)
			}
			if err != nil || !slices.Equal(minted.Projects, got) {
				t.Errorf("the token minted for %s carries %v (%v), and ProjectsForAccount answers %v: the two must agree", c.account, minted.Projects, err, got)
			}
		}
	}
}
