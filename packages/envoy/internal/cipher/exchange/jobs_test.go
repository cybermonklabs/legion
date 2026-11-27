package exchange_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// jobList is the job list text spells, read as a job setting's projects field is.
func jobList(t testing.TB, text string) exchange.JobProjects {
	t.Helper()
	job, err := exchange.ParseJobProjects(text)
	if err != nil {
		t.Fatalf("ParseJobProjects(%q): %v", text, err)
	}
	return job
}

// projectKeys is n project keys, P00 onwards, each named once.
func projectKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("P%02d", i)
	}
	return keys
}

// actionsToken is a GitHub Actions run's token claims, in the claim names and value forms GitHub's
// token request answered with in a run captured for this broker's GitHub Actions identity, each
// value that named the account replaced with an example of the same form: a push of gitRef,
// running the workflow at path of repo. The exchange reads four of them; the rest are there
// because a real token carries them. Its numbers are float64, as a verified token's claim map
// holds JSON numbers.
func actionsToken(repo, path, gitRef string) map[string]any {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	owner, name, _ := strings.Cut(repo, "/")
	run := repo + "/" + path + "@" + gitRef
	return map[string]any{
		"actor":                 "example-bot[bot]",
		"actor_id":              "1",
		"aud":                   "https://cipher.example",
		"base_ref":              "",
		"check_run_id":          "2",
		"enterprise":            owner,
		"enterprise_id":         "3",
		"event_name":            "push",
		"exp":                   float64(1795461665),
		"head_ref":              "",
		"iat":                   float64(1795461365),
		"iss":                   "https://token.actions.githubusercontent.com",
		"job_workflow_ref":      run,
		"job_workflow_sha":      sha,
		"jti":                   "00000000-0000-4000-8000-000000000000",
		"nbf":                   float64(1795461065),
		"ref":                   gitRef,
		"ref_protected":         "false",
		"ref_type":              "branch",
		"repository":            repo,
		"repository_id":         "4",
		"repository_owner":      owner,
		"repository_owner_id":   "5",
		"repository_visibility": "public",
		"run_attempt":           "1",
		"run_id":                "6",
		"run_number":            "1",
		"runner_environment":    "github-hosted",
		"sha":                   sha,
		"sub":                   "repo:" + owner + "@5/" + name + "@4:ref:" + gitRef,
		"workflow":              "dispatch-job",
		"workflow_ref":          run,
		"workflow_sha":          sha,
	}
}

// TestParseJobProjectsReadsOneKeyOrPublicAndItsList pins the reading of a job entry's projects:
// one project key, whose token Dispatch's project check judges as a person's named token for that
// project, or public followed by ,<key> for each private project, at most oauthwire.MaxProjects
// of them, answered sorted; public is no project key and does not count toward them. Every other
// spelling is refused naming the rule it breaks, so a list that is not one is never read as some
// part of it, and the list answered is the caller's own.
func TestParseJobProjectsReadsOneKeyOrPublicAndItsList(t *testing.T) {
	sixteen := projectKeys(oauthwire.MaxProjects)
	for _, c := range []struct {
		name     string
		text     string
		scope    exchange.ProjectScope
		projects []string
	}{
		{name: "one key", text: "ACME", scope: exchange.ProjectScopeNamed, projects: []string{"ACME"}},
		{name: "public alone", text: "public", scope: exchange.ProjectScopePublic, projects: nil},
		{name: "public and two keys, answered sorted", text: "public,PRIVB,PRIVA", scope: exchange.ProjectScopePublic, projects: []string{"PRIVA", "PRIVB"}},
		{name: "public and sixteen keys", text: "public," + strings.Join(sixteen, ","), scope: exchange.ProjectScopePublic, projects: sixteen},
	} {
		t.Run(c.name, func(t *testing.T) {
			job, err := exchange.ParseJobProjects(c.text)
			if err != nil || job.Scope() != c.scope || !reflect.DeepEqual(job.Projects(), c.projects) {
				t.Fatalf("ParseJobProjects(%q) = %v %v, %v; want %v %v", c.text, job.Scope(), job.Projects(), err, c.scope, c.projects)
			}
			if projects := job.Projects(); len(projects) > 0 {
				projects[0] = "CHANGED"
				if again := job.Projects(); !reflect.DeepEqual(again, c.projects) {
					t.Fatalf("Projects() after the caller changed its copy = %v, want %v: the list stays as it was read", again, c.projects)
				}
			}
		})
	}
	for _, c := range []struct {
		name  string
		text  string
		names string
	}{
		{name: "empty", text: "", names: "no project is named"},
		{name: "a key that is no project key", text: "acme", names: `"acme" is not a project key`},
		{name: "two keys without public", text: "ACME,OTHER", names: "without public"},
		{name: "public after a key", text: "ACME,public", names: "public comes first"},
		{name: "a key named twice", text: "public,PRIVA,PRIVA", names: "PRIVA is named twice"},
		{name: "an empty key", text: "public,", names: "an empty key"},
		{name: "public and seventeen keys", text: "public," + strings.Join(projectKeys(oauthwire.MaxProjects+1), ","), names: fmt.Sprintf("at most %d", oauthwire.MaxProjects)},
		{name: "public twice", text: "public,public", names: "public is named twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			job, err := exchange.ParseJobProjects(c.text)
			if err == nil {
				t.Fatalf("ParseJobProjects(%q) = %v %v, want a refusal", c.text, job.Scope(), job.Projects())
			}
			if !strings.Contains(err.Error(), c.names) {
				t.Errorf("ParseJobProjects(%q) refused with %q, which does not name the rule %q", c.text, err, c.names)
			}
		})
	}
}

// TestPodAnswersAJobAccountAsItsOwnService: a job account's pod is answered with a token that
// names the account as its service and no person, and carries its entry's scope and whole list:
// one project under named, the private projects under public. An entry that fixes no session
// answers the pod uid of the token as Instance, so a job pod registers as a person's pod does,
// and refuses a request that names no session; one that fixes a session answers it and no
// Instance.
func TestPodAnswersAJobAccountAsItsOwnService(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
		refused error
	}{
		{name: "a named job account", token: podToken(jobAccount, podUID), session: "example-job-1",
			want: exchange.Claims{Session: "example-job-1", Service: jobAccount, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Job: true, Principal: jobPrincipal, Instance: podUID}},
		{name: "a public job account with a list", token: podToken(publicJobAccount, podUID), session: "example-job-2",
			want: exchange.Claims{Session: "example-job-2", Service: publicJobAccount, Scope: exchange.ProjectScopePublic, Projects: []string{privA, privB}, Job: true, Principal: publicJobPrincipal, Instance: podUID}},
		{name: "a session the job entry fixes, the request naming none", token: podToken(fixedJobAccount, podUID), session: "",
			want: exchange.Claims{Session: jobPodSession, Service: fixedJobAccount, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Job: true, Principal: fixedJobPrincipal}},
		{name: "a free session carries the pod uid as Instance", token: podToken(publicJobAccount, "another-pod-uid"), session: "example-job-3",
			want: exchange.Claims{Session: "example-job-3", Service: publicJobAccount, Scope: exchange.ProjectScopePublic, Projects: []string{privA, privB}, Job: true, Principal: publicJobPrincipal, Instance: "another-pod-uid"}},
		{name: "no session named", token: podToken(jobAccount, podUID), session: "", refused: exchange.ErrSessionRequired},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Pod(c.token, c.session, nil)
			if c.refused != nil {
				if !errors.Is(err, c.refused) {
					t.Fatalf("Pod(%s) = %+v, %v; want %v", c.name, claims, err, c.refused)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Pod(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

// TestWorkflowAnswersAJobWorkflowAsItsOwnService: a run of a job workflow, presenting a token of
// the claims GitHub Actions writes, is answered with a token that names the workflow as its
// service, <repository>/<path>@<ref>, the workflow's principal without its kind, and no person,
// with its entry's scope and whole list.
func TestWorkflowAnswersAJobWorkflowAsItsOwnService(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		path    string
		session string
		want    exchange.Claims
	}{
		{name: "a public job workflow, the session the request names", path: jobWorkflow, session: "example-run-1",
			want: exchange.Claims{Session: "example-run-1", Service: repository + "/" + jobWorkflow + "@" + ref, Scope: exchange.ProjectScopePublic, Projects: []string{privA, privB}, Job: true, Principal: jobWorkflowPrincipal}},
		{name: "a named job workflow, the session its entry fixes", path: fixedJobWorkflow, session: "",
			want: exchange.Claims{Session: jobRunSession, Service: repository + "/" + fixedJobWorkflow + "@" + ref, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Job: true, Principal: fixedJobWorkflowPrincipal}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Workflow(actionsToken(repository, c.path, ref), c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Workflow(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}

// TestGoogleAnswersAJobAccountAsItsOwnService: a machine of a job Google account is answered with
// a token that names the account's address as its service and no person, with its entry's scope
// and whole list, and Fleet the service BROKER_VM_SERVICES lists the account for, so the launcher
// credential its exchange mints records the service it does today.
func TestGoogleAnswersAJobAccountAsItsOwnService(t *testing.T) {
	set := listed(t)
	for _, c := range []struct {
		name    string
		token   map[string]any
		session string
		want    exchange.Claims
	}{
		{name: "a public job machine, the session the request names", token: googleToken(jobMachineAccount, true, "", ""), session: "example-job-machine-1",
			want: exchange.Claims{Session: "example-job-machine-1", Service: jobMachineAccount, Scope: exchange.ProjectScopePublic, Projects: []string{privA, privB}, Job: true, Principal: jobMachinePrincipal, Fleet: jobService}},
		{name: "a named job machine where its entry pins it, the session its entry fixes", token: googleToken(fixedJobMachineAccount, true, cloudProject, instanceName), session: "",
			want: exchange.Claims{Session: jobMachineSession, Service: fixedJobMachineAccount, Scope: exchange.ProjectScopeNamed, Projects: []string{project}, Job: true, Principal: fixedJobMachinePrincipal, Fleet: jobService}},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims, err := set.Google(c.token, c.session, nil)
			if err != nil || !reflect.DeepEqual(claims, c.want) {
				t.Fatalf("Google(%s) = %+v, %v; want %+v", c.name, claims, err, c.want)
			}
		})
	}
}
