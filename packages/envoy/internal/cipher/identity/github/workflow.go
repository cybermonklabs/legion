package github

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/exchange"
)

// repositoryPattern is a GitHub repository as its token's `repository` claim names it,
// <owner>/<name>: the owner a letter or digit, then up to 38 letters, digits, hyphens and
// underscores, since a managed user's login ends in `_<shortcode>` (`mona-cat_octo`); the name up
// to 100 of letters, digits, hyphens, underscores and dots.
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// workflowKey is the triple a workflow run is looked up by: every field of an entry is required,
// so the key is the whole identity and no partial match exists.
type workflowKey struct{ repository, workflow, ref string }

func (k workflowKey) principal() string { return Principal(k.repository, k.workflow, k.ref) }

// address is the workflow as an operator reads one, <repository>/<path>@<ref>: its principal
// without the kind, and the service a job workflow's token names.
func (k workflowKey) address() string { return k.repository + "/" + k.workflow + "@" + k.ref }

// Principal is a workflow's spelling of a principal, <repository>/<path>@<ref> led by its kind, as
// an operator reads one elsewhere. A path or ref carrying "@" could make two entries spell one
// string, and New refuses that pair rather than let one's runs hold the other's names. Every run
// of one workflow is one holder of a session name. Postgres holds this spelling in
// oauth_session_bindings.principal, so it never changes.
func Principal(repository, workflow, ref string) string {
	return "workflow:" + workflowKey{repository, workflow, ref}.address()
}

// workflow answers the claims for a continuous-integration workflow run's GitHub token, from the
// claims GitHub puts in it: `repository`, `workflow_ref`, `job_workflow_ref` and `ref`.
//
// The identity is the repository, the workflow file's path and the ref, each compared exactly. A
// run whose job comes from a reusable workflow other than the one the run started in is refused:
// `job_workflow_ref` names the workflow file the job is written in and `workflow_ref` the one the
// run entered through, and where they differ the code that is running is not the code the entry
// names.
//
// The principal is that listed workflow, not the run: every run of it is one holder, so the next
// run of a release workflow keeps writing under the name the run before it held, however soon
// after it starts.
//
// A job entry's runs are answered with a token that names the workflow as its service,
// <repository>/<path>@<ref>, which is its principal without the kind, and no person, with the
// entry's scope and whole list whatever the request names (exchange.Claims.Grant).
func (p *Provider) workflow(token map[string]any, session string, projects []string) (exchange.Claims, error) {
	repository, err := exchange.ClaimString(token, "repository")
	if err != nil {
		return exchange.Claims{}, err
	}
	workflowRef, err := exchange.ClaimString(token, "workflow_ref")
	if err != nil {
		return exchange.Claims{}, err
	}
	jobRef, err := exchange.ClaimString(token, "job_workflow_ref")
	if err != nil {
		return exchange.Claims{}, err
	}
	ref, err := exchange.ClaimString(token, "ref")
	if err != nil {
		return exchange.Claims{}, err
	}
	if jobRef != workflowRef {
		return exchange.Claims{}, fmt.Errorf("%w: %s ran %s, whose job is written in %s; an entry lists the workflow a job runs in", exchange.ErrNoEntry, repository, workflowRef, jobRef)
	}
	workflow, ok := workflowPath(workflowRef, repository)
	if !ok {
		return exchange.Claims{}, fmt.Errorf("%w: the subject token's workflow_ref is not <owner>/<name>/<path>@<ref> under its repository %s", exchange.ErrClaimMissing, repository)
	}
	key := workflowKey{repository, workflow, ref}
	entry, listed := p.workflows[key]
	if !listed {
		return exchange.Claims{}, fmt.Errorf("%w: no entry lists the workflow %s of %s on %s", exchange.ErrNoEntry, workflow, repository, ref)
	}
	name, err := exchange.ResolveSession(entry.Session, session, repository+"/"+workflow)
	if err != nil {
		return exchange.Claims{}, err
	}
	claims := exchange.Claims{Session: name, Principal: key.principal()}
	if err := claims.Grant(repository+"/"+workflow, key.address(), entry.Person, entry.Project, entry.Job, projects); err != nil {
		return exchange.Claims{}, err
	}
	return claims, nil
}

// workflowPath is the workflow file's path inside repository that workflowRef names, a GitHub
// workflow_ref being "<owner>/<name>/<path>@<ref>". It answers false for a ref that is not of that
// form or not under repository.
func workflowPath(workflowRef, repository string) (string, bool) {
	rest, under := strings.CutPrefix(workflowRef, repository+"/")
	if !under {
		return "", false
	}
	at := strings.LastIndex(rest, "@")
	if at <= 0 {
		return "", false
	}
	return rest[:at], true
}
