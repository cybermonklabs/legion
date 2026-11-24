package github_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
	"github.com/sjawhar/envoy/internal/cipher/identity/github"
)

// getenv reads e, as the broker's own environment reads a variable it was not given: "".
func getenv(e map[string]string) func(string) string {
	return func(name string) string { return e[name] }
}

// withGitHub is the environment with GitHub's issuer and audience set, the pair
// BROKER_EXCHANGE_WORKFLOWS needs.
func withGitHub() map[string]string {
	return map[string]string{
		"BROKER_EXCHANGE_GITHUB_ISSUER":   "https://token.actions.githubusercontent.com",
		"BROKER_EXCHANGE_GITHUB_AUDIENCE": "dispatch-exchange",
	}
}

// TestLoadReadsTheExchangeWorkflows pins BROKER_EXCHANGE_WORKFLOWS:
// <repository>=<workflow>=<ref>=<project>=<person>[=<session>] entries in order, none when unset,
// and refusals naming the entry for one that is not of that form or lists a workflow twice. It
// needs GitHub's issuer and audience, whose tokens its runs present.
func TestLoadReadsTheExchangeWorkflows(t *testing.T) {
	c, err := github.LoadConfig(getenv(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workflows != nil {
		t.Fatalf("Workflows = %v, want nil when BROKER_EXCHANGE_WORKFLOWS is unset", c.Workflows)
	}
	e := withGitHub()
	e["BROKER_EXCHANGE_WORKFLOWS"] = "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE=ada@example.com=release=1"
	c, err = github.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []github.WorkflowEntry{{
		Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main",
		Project: "EXAMPLE", Person: "ada@example.com", Session: "release=1",
	}}
	if !slices.Equal(c.Workflows, want) {
		t.Fatalf("Workflows = %v, want %v", c.Workflows, want)
	}
	for _, bad := range []string{
		"example-org/example-repo", "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main=EXAMPLE",
		"example-org/example-repo=.github/workflows/release.yaml==EXAMPLE=ada@example.com",
	} {
		e := withGitHub()
		e["BROKER_EXCHANGE_WORKFLOWS"] = bad
		if _, err := github.LoadConfig(getenv(e)); err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_WORKFLOWS") || !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("BROKER_EXCHANGE_WORKFLOWS = %q: err = %v, want a refusal naming the setting and the entry", bad, err)
		}
	}
	e = withGitHub()
	entry := "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main"
	e["BROKER_EXCHANGE_WORKFLOWS"] = entry + "=A=ada@example.com " + entry + "=B=ada@example.com"
	if _, err := github.LoadConfig(getenv(e)); err == nil || err.Error() != "BROKER_EXCHANGE_WORKFLOWS names the workflow "+entry+" twice; one workflow run writes as one identity, so name it once" {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS naming one workflow twice: err = %v, want config.Load's refusal naming the workflow", err)
	}
	if _, err := github.LoadConfig(getenv(map[string]string{"BROKER_EXCHANGE_WORKFLOWS": entry + "=EXAMPLE=ada@example.com"})); err == nil ||
		!strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") {
		t.Errorf("BROKER_EXCHANGE_WORKFLOWS with no GitHub issuer: err = %v, want a refusal naming BROKER_EXCHANGE_GITHUB_ISSUER", err)
	}
}

// jobWorkflow is the identity, <repository>=<workflow path>=<ref>, of the workflow
// BROKER_EXCHANGE_JOB_WORKFLOWS lists; this repository is public, so it is a placeholder.
const jobWorkflow = "example-org/example-repo=.github/workflows/sweep.yaml=refs/heads/main"

// jobList is the job list exchange.ParseJobProjects reads from text.
func jobList(t *testing.T, text string) exchange.JobProjects {
	t.Helper()
	job, err := exchange.ParseJobProjects(text)
	if err != nil {
		t.Fatalf("exchange.ParseJobProjects(%q): %v", text, err)
	}
	return job
}

// TestLoadAppendsTheJobWorkflowsAfterTheWorkflows pins where BROKER_EXCHANGE_JOB_WORKFLOWS lands:
// each entry, in order, joins Workflows after every BROKER_EXCHANGE_WORKFLOWS entry, holding its
// workflow, its projects as Job and the session it fixes, and no person and no project, so New
// numbers a job entry after the others. A workflow both settings list is refused in config.Load's
// words, and the setting needs GitHub's issuer and audience, whose tokens its runs present.
func TestLoadAppendsTheJobWorkflowsAfterTheWorkflows(t *testing.T) {
	const release = "example-org/example-repo=.github/workflows/release.yaml=refs/heads/main"
	e := withGitHub()
	e["BROKER_EXCHANGE_JOB_WORKFLOWS"] = jobWorkflow + "=public=sweep=1\texample-org/example-repo=.github/workflows/triage.yaml=refs/heads/main=ACME"
	e["BROKER_EXCHANGE_WORKFLOWS"] = release + "=EXAMPLE=ada@example.com"
	c, err := github.LoadConfig(getenv(e))
	if err != nil {
		t.Fatal(err)
	}
	want := []github.WorkflowEntry{
		{Repository: "example-org/example-repo", Workflow: ".github/workflows/release.yaml", Ref: "refs/heads/main", Project: "EXAMPLE", Person: "ada@example.com"},
		{Repository: "example-org/example-repo", Workflow: ".github/workflows/sweep.yaml", Ref: "refs/heads/main", Job: jobList(t, "public"), Session: "sweep=1"},
		{Repository: "example-org/example-repo", Workflow: ".github/workflows/triage.yaml", Ref: "refs/heads/main", Job: jobList(t, "ACME")},
	}
	if !slices.Equal(c.Workflows, want) {
		t.Fatalf("Workflows = %+v, want %+v: the job entries after the workflow entries", c.Workflows, want)
	}
	e = withGitHub()
	e["BROKER_EXCHANGE_WORKFLOWS"] = jobWorkflow + "=EXAMPLE=ada@example.com"
	e["BROKER_EXCHANGE_JOB_WORKFLOWS"] = jobWorkflow + "=EXAMPLE"
	want2 := "BROKER_EXCHANGE_JOB_WORKFLOWS and BROKER_EXCHANGE_WORKFLOWS both name the workflow " + jobWorkflow + "; one workflow run has one answer, a token that acts for its person or a job's that names none, so name it in one of them"
	if _, err := github.LoadConfig(getenv(e)); err == nil || err.Error() != want2 {
		t.Errorf("a workflow both settings list: err = %v, want %q", err, want2)
	}
	if _, err := github.LoadConfig(getenv(map[string]string{"BROKER_EXCHANGE_JOB_WORKFLOWS": jobWorkflow + "=EXAMPLE"})); err == nil ||
		!strings.HasPrefix(err.Error(), "BROKER_EXCHANGE_JOB_WORKFLOWS needs BROKER_EXCHANGE_GITHUB_ISSUER and BROKER_EXCHANGE_GITHUB_AUDIENCE") {
		t.Errorf("BROKER_EXCHANGE_JOB_WORKFLOWS with no GitHub issuer: err = %v, want a refusal naming the setting and the issuer", err)
	}
	e = withGitHub()
	e["BROKER_EXCHANGE_JOB_WORKFLOWS"] = jobWorkflow + "=acme"
	if _, err := github.LoadConfig(getenv(e)); err == nil || !strings.HasPrefix(err.Error(), "BROKER_EXCHANGE_JOB_WORKFLOWS names projects no job entry may name, got "+strconv.Quote(jobWorkflow+"=acme")) {
		t.Errorf("BROKER_EXCHANGE_JOB_WORKFLOWS naming no project key: err = %v, want a refusal naming the setting and the entry", err)
	}
}

// TestLoadRefusesAHalfConfiguredGitHubIssuer pins that GitHub's issuer and audience are
// both-or-neither, as the cluster's pair is: half a pair would register a token type whose tokens
// could not be checked.
func TestLoadRefusesAHalfConfiguredGitHubIssuer(t *testing.T) {
	for _, half := range []string{"BROKER_EXCHANGE_GITHUB_ISSUER", "BROKER_EXCHANGE_GITHUB_AUDIENCE"} {
		_, err := github.LoadConfig(getenv(map[string]string{half: "https://token.actions.githubusercontent.com"}))
		if err == nil || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_ISSUER") || !strings.Contains(err.Error(), "BROKER_EXCHANGE_GITHUB_AUDIENCE") {
			t.Errorf("LoadConfig(%s alone) = %v, want a refusal naming both halves", half, err)
		}
	}
	c, err := github.LoadConfig(getenv(nil))
	if err != nil || c.Issuer != "" || c.Audience != "" {
		t.Fatalf("LoadConfig with neither half = (%q, %q, %v), want no GitHub issuer and no error", c.Issuer, c.Audience, err)
	}
}
