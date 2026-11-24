package github

import "github.com/sjawhar/envoy/internal/cipher/exchange"

// WorkflowEntry lists one GitHub workflow of one repository on one ref, and what a run of it may
// write. Repository, Workflow and Ref are required, and so are Project and Person, or Job in
// their place: nothing is wildcarded by leaving a field out, so an entry that names no ref
// matches no run rather than every one of them. Session is not required.
type WorkflowEntry struct {
	// Repository is <owner>/<name>, the repository the run belongs to.
	Repository string
	// Workflow is the workflow file's path inside the repository, such as
	// .github/workflows/release.yaml, compared exactly.
	Workflow string
	// Ref is the git ref the run is on, such as refs/heads/main, compared exactly.
	Ref string
	// Project is the one project key the minted token names.
	Project string
	// Person is who the run acts for.
	Person string
	// Job, when set, is the job list of an entry that names no person and no project: its runs'
	// tokens name the workflow, <Repository>/<Workflow>@<Ref>, as their service and carry the
	// list (BROKER_EXCHANGE_JOB_WORKFLOWS).
	Job exchange.JobProjects
	// Session, when set, is the session name every token minted for this workflow carries, as
	// the kubernetes provider's AccountEntry.Session is for an account.
	Session string
}
