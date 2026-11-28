package oauthwire_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
)

// TestAWorkflowPathIsWhatTheRunsRepositoryAndRefLeave: GitHub writes a run's workflow_ref as
// <repository>/<path>@<ref>, where ref is the run's own ref claim. A workflow file's name and a
// branch's name can each hold an "@", so no cut at an "@" reads the path: the path is what is left
// once the prefix "<repository>/" and the suffix "@<ref>" are removed. A workflow_ref that does
// not start with that prefix, does not end with that suffix, or holds no path between them names
// no path. The plain row is the control: a path and a branch that hold no "@".
func TestAWorkflowPathIsWhatTheRunsRepositoryAndRefLeave(t *testing.T) {
	const repository = "example-org/example-repo"
	for _, row := range []struct {
		name, workflowRef, ref, path string
		ok                           bool
	}{
		{name: "a plain path on a plain branch", workflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/legion/ACME-1",
			ref: "refs/heads/legion/ACME-1", path: ".github/workflows/ci-results.yaml", ok: true},
		{name: "a workflow file whose name holds an @", workflowRef: repository + "/.github/workflows/ci@probe.yaml@refs/heads/legion/ACME-1",
			ref: "refs/heads/legion/ACME-1", path: ".github/workflows/ci@probe.yaml", ok: true},
		{name: "a branch whose name holds an @", workflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/legion/a@b",
			ref: "refs/heads/legion/a@b", path: ".github/workflows/ci-results.yaml", ok: true},
		{name: "both at one time", workflowRef: repository + "/.github/workflows/ci@probe.yaml@refs/heads/legion/a@b",
			ref: "refs/heads/legion/a@b", path: ".github/workflows/ci@probe.yaml", ok: true},
		{name: "a workflow_ref that ends in another ref", workflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/main",
			ref: "refs/heads/legion/ACME-1"},
		{name: "a workflow_ref under another repository", workflowRef: "example-org/other-repo/.github/workflows/ci-results.yaml@refs/heads/main",
			ref: "refs/heads/main"},
		{name: "a workflow_ref under a repository whose name starts with the run's", workflowRef: repository + "-two/.github/workflows/ci-results.yaml@refs/heads/main",
			ref: "refs/heads/main"},
		{name: "a workflow_ref with no path", workflowRef: repository + "/@refs/heads/main", ref: "refs/heads/main"},
		{name: "a workflow_ref with no ref", workflowRef: repository + "/.github/workflows/ci-results.yaml", ref: "refs/heads/main"},
	} {
		t.Run(row.name, func(t *testing.T) {
			path, ok := oauthwire.WorkflowPath(row.workflowRef, repository, row.ref)
			if path != row.path || ok != row.ok {
				t.Fatalf("WorkflowPath(%q, %q, %q) = %q, %t; want %q, %t", row.workflowRef, repository, row.ref, path, ok, row.path, row.ok)
			}
		})
	}
}

// TestAJobWorkflowPathEndsAtTheLastBranchMark: a result entry's run carries the token's
// job_workflow_ref, <repository>/<path>@refs/heads/<branch>, the job's workflow file at the branch
// the entry takes it at, which is not the run's ref. A workflow file's name may hold "@" and
// "@refs/heads/", and a branch's name may hold "@", so the path ends at the last "@refs/heads/".
// A job_workflow_ref not under the repository, with no branch, with no path or naming no branch's
// name names no path. The plain row is the control.
func TestAJobWorkflowPathEndsAtTheLastBranchMark(t *testing.T) {
	const repository = "example-org/example-repo"
	for _, row := range []struct {
		name, jobWorkflowRef, path string
		ok                         bool
	}{
		{name: "a plain path at the default branch", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/main",
			path: ".github/workflows/ci-results.yaml", ok: true},
		{name: "a workflow file whose name holds an @", jobWorkflowRef: repository + "/.github/workflows/ci@probe.yaml@refs/heads/main",
			path: ".github/workflows/ci@probe.yaml", ok: true},
		{name: "a workflow file whose name holds @refs/heads/", jobWorkflowRef: repository + "/.github/workflows/a@refs/heads/b.yaml@refs/heads/main",
			path: ".github/workflows/a@refs/heads/b.yaml", ok: true},
		{name: "a branch whose name holds an @", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/a@b",
			path: ".github/workflows/ci-results.yaml", ok: true},
		{name: "a workflow at a commit", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml@0123456789abcdef0123456789abcdef01234567"},
		{name: "a workflow at a tag", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml@refs/tags/v1"},
		{name: "a workflow under another repository", jobWorkflowRef: "example-org/other-repo/.github/workflows/ci-results.yaml@refs/heads/main"},
		{name: "a workflow under a repository whose name starts with the run's", jobWorkflowRef: repository + "-two/.github/workflows/ci-results.yaml@refs/heads/main"},
		{name: "no path", jobWorkflowRef: repository + "/@refs/heads/main"},
		{name: "no branch's name", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/"},
		{name: "no ref", jobWorkflowRef: repository + "/.github/workflows/ci-results.yaml"},
	} {
		t.Run(row.name, func(t *testing.T) {
			path, ok := oauthwire.JobWorkflowPath(row.jobWorkflowRef, repository)
			if path != row.path || ok != row.ok {
				t.Fatalf("JobWorkflowPath(%q, %q) = %q, %t; want %q, %t", row.jobWorkflowRef, repository, path, ok, row.path, row.ok)
			}
		})
	}
}

// TestARunCarriesTheJobsWorkflowAtTheCommitThatRan: a run of a branch whose job is written in the
// result entry's workflow at the default branch reads with its workflow named as the job's,
// <repository>/<path>@refs/heads/main, and the commit of that file the job ran, which is not the
// commit the run tested. The defect it catches: a run whose workflow is read as the branch's own
// copy of the file, the copy no job ran. A run whose job's workflow commit is missing or not 40
// lowercase hex digits is refused naming job_workflow_sha, and one whose job's workflow is at a
// commit rather than a branch is refused naming job_workflow_ref.
func TestARunCarriesTheJobsWorkflowAtTheCommitThatRan(t *testing.T) {
	const (
		repository = "example-org/example-repo"
		tested     = "0123456789abcdef0123456789abcdef01234567"
		ran        = "89abcdef0123456789abcdef0123456789abcdef"
	)
	run := oauthwire.CIRun{
		Repository: repository, WorkflowRef: repository + "/.github/workflows/ci-results.yaml@refs/heads/main", WorkflowSHA: ran,
		Ref: "refs/heads/legion/ACME-1", SHA: tested, EventName: "push", RunID: "7", RunAttempt: "1",
		RunnerEnvironment: "github-hosted", CheckRunID: "97",
	}
	read, err := oauthwire.ParseCIRun(run)
	want := oauthwire.Run{
		Repository: repository, WorkflowRef: run.WorkflowRef, WorkflowSHA: ran, Ref: run.Ref, SHA: tested,
		EventName: "push", RunnerEnvironment: "github-hosted", Workflow: ".github/workflows/ci-results.yaml",
		ID: 7, Attempt: 1, CheckRun: 97,
	}
	if err != nil || read != want {
		t.Fatalf("ParseCIRun(a branch's run of the default branch's workflow) = %+v, %v; want %+v", read, err, want)
	}
	for name, sha := range map[string]string{"no job_workflow_sha": "", "a short job_workflow_sha": ran[:7], "an upper-case job_workflow_sha": "89ABCDEF0123456789ABCDEF0123456789ABCDEF"} {
		spoiled := run
		spoiled.WorkflowSHA = sha
		if _, err := oauthwire.ParseCIRun(spoiled); err == nil || !strings.Contains(err.Error(), "job_workflow_sha claim is not 40 lowercase hex digits") {
			t.Errorf("ParseCIRun(a run with %s) = %v, want a refusal naming job_workflow_sha", name, err)
		}
	}
	atCommit := run
	atCommit.WorkflowRef = repository + "/.github/workflows/ci-results.yaml@" + ran
	if _, err := oauthwire.ParseCIRun(atCommit); err == nil || !strings.Contains(err.Error(), "job_workflow_ref claim") {
		t.Errorf("ParseCIRun(a run whose job's workflow is at a commit) = %v, want a refusal naming job_workflow_ref", err)
	}
}
