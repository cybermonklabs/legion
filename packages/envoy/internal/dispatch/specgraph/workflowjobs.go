package specgraph

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// WorkflowJobs is what a GitHub Actions workflow says of its jobs: Jobs, the id of each, and
// Awaited, the jobs that its citations job needs. The citations job can read the report of an
// awaited job only, since only those have ended when it starts.
type WorkflowJobs struct {
	Jobs    map[string]bool
	Awaited map[string]bool
}

// ReadWorkflowJobs reads the jobs of workflowYAML and the needs of its job citationsJob, which a
// workflow writes as one job id or a list of them. A workflow that is no YAML, that has no jobs or
// no job citationsJob, or whose needs are in another form, is an error: what the citations job
// waits for is then unknown, and never a set of no job.
func ReadWorkflowJobs(workflowYAML []byte, citationsJob string) (WorkflowJobs, error) {
	var workflow struct {
		Jobs map[string]struct {
			Needs any `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflowYAML, &workflow); err != nil {
		return WorkflowJobs{}, fmt.Errorf("specgraph: the workflow: %w", err)
	}
	if len(workflow.Jobs) == 0 {
		return WorkflowJobs{}, errors.New("specgraph: the workflow has no jobs")
	}
	citations, ok := workflow.Jobs[citationsJob]
	if !ok {
		return WorkflowJobs{}, fmt.Errorf("specgraph: the workflow has no job %q", citationsJob)
	}
	jobs := WorkflowJobs{Jobs: map[string]bool{}, Awaited: map[string]bool{}}
	for id := range workflow.Jobs {
		jobs.Jobs[id] = true
	}
	var needs []any
	switch value := citations.Needs.(type) {
	case nil:
	case string:
		needs = []any{value}
	case []any:
		needs = value
	default:
		return WorkflowJobs{}, fmt.Errorf("specgraph: the needs of job %q are %T, not a job id or a list of them", citationsJob, value)
	}
	for _, need := range needs {
		id, ok := need.(string)
		if !ok {
			return WorkflowJobs{}, fmt.Errorf("specgraph: the needs of job %q hold %v, which is no job id", citationsJob, need)
		}
		jobs.Awaited[id] = true
	}
	return jobs, nil
}
