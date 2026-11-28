package oauthwire_test

import (
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
