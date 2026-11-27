package specgraph

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// The jobs of a workflow and the jobs that its citations job waits for: only their reports can the
// citations job read.
func TestReadWorkflowJobs(t *testing.T) {
	t.Parallel()
	workflow, err := os.ReadFile("testdata/citations/workflow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorkflowJobs(workflow, "spec-citations")
	if err != nil {
		t.Fatal(err)
	}
	if jobs := slices.Sorted(maps.Keys(got.Jobs)); !slices.Equal(jobs, []string{"changes", "envoy-go", "go-checks", "spec-citations"}) {
		t.Errorf("Jobs = %q; want the four jobs of the workflow", jobs)
	}
	if awaited := slices.Sorted(maps.Keys(got.Awaited)); !slices.Equal(awaited, []string{"changes", "go-checks"}) {
		t.Errorf("Awaited = %q; want the needs of spec-citations", awaited)
	}
	if !got.Jobs["envoy-go"] || got.Awaited["envoy-go"] {
		t.Error("envoy-go is not a job, or spec-citations waits for it")
	}

	// needs as one job, and no needs.
	one, err := ReadWorkflowJobs(workflow, "go-checks")
	if err != nil || !slices.Equal(slices.Sorted(maps.Keys(one.Awaited)), []string{"changes"}) {
		t.Errorf("ReadWorkflowJobs(go-checks) = %+v, %v; want Awaited holding changes alone", one, err)
	}
	none, err := ReadWorkflowJobs(workflow, "envoy-go")
	if err != nil || len(none.Awaited) != 0 {
		t.Errorf("ReadWorkflowJobs(envoy-go) = %+v, %v; want no job awaited", none, err)
	}
}

// A workflow that cannot say what the citations job waits for is an error, never a set of no job.
// Each row reads the words of this package, and never the YAML library's, which are its own to
// change.
func TestReadWorkflowJobsRefusesAnUnreadableWorkflow(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, yaml, err string }{
		{"no such job", "jobs:\n  go-checks:\n    runs-on: ubuntu-24.04\n", `specgraph: the workflow has no job "spec-citations"`},
		{"no jobs", "name: x\n", "specgraph: the workflow has no jobs"},
		{"not YAML", "jobs: [\n", "specgraph: the workflow: "},
		{"needs of another type", "jobs:\n  spec-citations:\n    needs: {a: b}\n", `specgraph: the needs of job "spec-citations" are `},
		{"a need that is no name", "jobs:\n  spec-citations:\n    needs: [go-checks, 3]\n", `specgraph: the needs of job "spec-citations" hold `},
	} {
		got, err := ReadWorkflowJobs([]byte(row.yaml), "spec-citations")
		if err == nil || !strings.Contains(err.Error(), row.err) {
			t.Errorf("%s: ReadWorkflowJobs = %+v, %v; want an error holding %q", row.name, got, err, row.err)
		}
	}
}
