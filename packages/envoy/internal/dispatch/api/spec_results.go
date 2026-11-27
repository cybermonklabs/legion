package api

// spec_results.go is the route a CI job writes the result of a check of the spec files with,
// POST /api/v1/projects/{key}/results, and the read beside it (R12 of the spec). The job holds a
// results token the secrets broker minted for its run (ciphertoken.Caller.ResultsOnly), and this
// route is the one route that token reaches (routeDecl.ResultsScope). Dispatch takes who wrote the
// result and what it is for from that token, from GitHub and from the spec files at the run's
// commit; the body says only what the job observed. The recorder of a result is the run, and no
// person: the broker's result entry names a person, whom nobody authenticated for this write and
// who ran no test, so that person stays inside the server. Each result is a new row of
// spec_check_results, which nothing updates or deletes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/bytelru"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// specDir is the directory of a repository that holds its spec files.
const specDir = ".dispatch/spec"

// The codes of this route's own refusals, and of the gate that holds a results token to it.
const (
	// codeResultsOnlyToken refuses a results token on every route the results list does not
	// name (refuseLimitedToken).
	codeResultsOnlyToken = "RESULTS_ONLY_TOKEN"
	// codeResultsScopeRequired refuses every other credential on the route: only a CI job's
	// results token writes a result of a spec check.
	codeResultsScopeRequired = "RESULTS_SCOPE_REQUIRED"
	// codeResultInput refuses a body this route does not take.
	codeResultInput = "RESULT_INPUT"
	// codeCommitNotTheRuns refuses a result for another commit than the run's.
	codeCommitNotTheRuns = "COMMIT_NOT_THE_RUNS"
	// codeRepositoryNotTheProjects refuses a run of a repository that is not the project's
	// default repository.
	codeRepositoryNotTheProjects = "REPOSITORY_NOT_THE_PROJECTS"
	// codeCheckRunNotTheRuns refuses a token whose check run GitHub names for another commit.
	codeCheckRunNotTheRuns = "CHECK_RUN_NOT_THE_RUNS"
	// codeCheckRunNotShown refuses a token whose check run GitHub refuses Dispatch for good, with
	// its 403 or 404 (githubapp.ErrRepositoryUnreadable): the App cannot see the run, or no such
	// run exists, so the run is not proven, and a write again is answered the same.
	codeCheckRunNotShown = "CHECK_RUN_NOT_SHOWN"
	// codeSpecFilesRefused refuses a result at a commit whose spec files the graph logic refuses
	// (specgraph.Judge): such files hold no check to write a result for.
	codeSpecFilesRefused = "SPEC_FILES_REFUSED"
	// codeCheckNotInFiles refuses a check the spec files at the run's commit do not hold.
	codeCheckNotInFiles = "CHECK_NOT_IN_FILES"
	// codeCheckNotThisJobs refuses a check whose test part names another job than the run's.
	codeCheckNotThisJobs = "CHECK_NOT_THIS_JOBS"
	// codeWorkflowNotShown refuses a check run whose job only the run's workflow file says, where
	// GitHub refuses Dispatch that file for good, with its 403 or 404: the commit does not hold the
	// file, or the App cannot see it, and a write again is answered the same.
	codeWorkflowNotShown = "WORKFLOW_NOT_SHOWN"
	// codeCheckRunUnreadable, codeFilesUnreadable and codeWorkflowUnreadable are GitHub not
	// answering, a 503: the result was not judged, and the job writes it again.
	codeCheckRunUnreadable = "CHECK_RUN_UNREADABLE"
	codeFilesUnreadable    = "FILES_UNREADABLE"
	codeWorkflowUnreadable = "WORKFLOW_UNREADABLE"
)

// The in-toto statement a result is stored as: its own type, and the type of its predicate.
const (
	inTotoStatementType          = "https://in-toto.io/Statement/v1"
	specCheckResultPredicateType = "https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1"
)

// specCheckResultInput is the body POST /api/v1/projects/{key}/results takes: what the job ran
// and what it observed, and nothing about who the job is. Every other member is refused by name.
type specCheckResultInput struct {
	// Commit is the commit the job tested, 40 lowercase hex characters; it must be the run's.
	Commit string `json:"commit" required:"true"`
	// Check is the id of a check of the spec files at that commit.
	Check string `json:"check" required:"true"`
	// Verdict is pass, fail or could_not_run.
	Verdict string `json:"verdict" required:"true"`
	// Observed is what the job observed; the result keeps contracts.SpecCheckObservedMax runes of
	// it (cutObserved).
	Observed string `json:"observed"`
}

// specCheckResultsQuery is GET /api/v1/projects/{key}/results's query.
type specCheckResultsQuery struct {
	// Commit is the commit whose results the read answers, 40 lowercase hex characters.
	Commit string `query:"commit"`
	// Check, when given, keeps the results of that check alone.
	Check string `query:"check"`
}

// specCheckResults is the body GET /api/v1/projects/{key}/results answers.
type specCheckResults struct {
	Results []model.SpecCheckResult `json:"results"`
}

// specCheckFacts is what the route reads of one check of the spec files at a commit: its
// procedure digest, and its test part, empty for a check with none.
type specCheckFacts struct {
	Digest   string `json:"digest"`
	Runner   string `json:"runner"`
	Selector string `json:"selector"`
	Job      string `json:"job"`
}

// specFiles is what the route reads of the spec files at a commit: how many problems the graph
// logic finds in them and the first one, as specgraph.Judge orders them, and, when it finds none,
// their checks by id. A set over notes.MaxFiles or notes.MaxSetSize is one problem, and each file
// over notes.MaxFileSize is one, as they are to every reader of spec files, and then the graph
// logic reads none of them.
type specFiles struct {
	Problems int                       `json:"problems,omitempty"`
	First    string                    `json:"first,omitempty"`
	Checks   map[string]specCheckFacts `json:"checks,omitempty"`
}

// workflowJobs is what the route reads of a workflow file at a commit: the name GitHub gives the
// check runs of each of its jobs, the job's `name:` where it sets one and else its id, among the
// names of the jobs with a matrix or among those of the jobs with none. A job whose name holds an
// expression (`${{`) is in neither, since the names of its check runs are known only once a run
// evaluates it. Problem, when set, is why the file says nothing of its jobs: it is over
// notes.MaxFileSize, or it does not read as a workflow file.
type workflowJobs struct {
	Problem string   `json:"problem,omitempty"`
	Matrix  []string `json:"matrix,omitempty"`
	Plain   []string `json:"plain,omitempty"`
	// Expressions is whether a job of the file is named by something a run evaluates: its name
	// holds an expression, or is no scalar. The name of a check run such a job makes is known only
	// to that run, so no name of the file's own says which job made a given check run.
	Expressions bool `json:"expressions,omitempty"`
}

// specResultReads keeps what the route reads of GitHub for the life of the process, since none
// of it changes: a check run's name and commit, by repository and id, what the spec files hold,
// by repository and commit, and the jobs of a workflow file, by repository, commit and path. Each
// is held as JSON in a byte-bounded cache, so many runs and commits cannot grow it without bound
// (bytelru).
type specResultReads struct {
	once      sync.Once
	checkRuns *bytelru.Cache[string, []byte]
	checks    *bytelru.Cache[string, []byte]
	workflows *bytelru.Cache[string, []byte]
}

// The bounds of the three caches: a check run is a hundred bytes, a commit's checks a few hundred
// bytes each, and a workflow file's jobs a name each. The names are a part of a file of at most
// notes.MaxFileSize, so the jobs of every file within that bound fit the workflow cache.
const (
	specCheckRunCacheBytes = 1 << 20
	specChecksCacheBytes   = 16 << 20
	specWorkflowCacheBytes = 4 << 20
)

func (c *specResultReads) init() {
	c.once.Do(func() {
		c.checkRuns = bytelru.New[string, []byte](specCheckRunCacheBytes)
		c.checks = bytelru.New[string, []byte](specChecksCacheBytes)
		c.workflows = bytelru.New[string, []byte](specWorkflowCacheBytes)
	})
}

// recordSpecCheckResult is POST /api/v1/projects/{key}/results. The checks run in this order,
// each before anything is read of GitHub that it does not need: the credential is a results
// token; the body names the four fields alone, well formed; its commit is the run's; the run's
// repository is the project's default repository; GitHub names the run's check run at that
// commit; the graph logic takes the files at that commit; the files hold the check; and the run's
// workflow file at the commit shows the check run to be a run of the check's job (jobOfCheckRun).
// GitHub's 403 or 404 for the check run or the workflow file is a final refusal, and any other
// failure of GitHub's a 503 that asks for the write again (githubReadRefusal). The project check
// of the path is withinProjectScope's, which register puts in front of this handler.
func (s *server) recordSpecCheckResult(w http.ResponseWriter, r *http.Request) {
	actor, _, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if actor.Credential == nil || !actor.Credential.ResultsOnly || actor.Credential.CI == nil {
		writeError(w, codeResultsScopeRequired, http.StatusForbidden,
			"only a CI job's results token writes the result of a spec check, and this request's credential is not one")
		return
	}
	run := *actor.Credential.CI
	var input specCheckResultInput
	body, err := readJSONBody(r, &input, maxJSONRequestBytes, errJSONTooLarge)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := validSpecCheckResultInput(input, body); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Commit != run.SHA {
		writeError(w, codeCommitNotTheRuns, http.StatusForbidden, fmt.Sprintf(
			"the result is for commit %s, and this token's run tested commit %s: a job writes results only for the commit that it tested",
			input.Commit, run.SHA))
		return
	}
	// The run was read whole once, by the verifier (ciphertoken.Caller.CI, oauthwire.ParseCIRun):
	// its numbers and its workflow's path are read off it, never parsed again here.
	project := r.PathValue("key")
	ctx := r.Context()
	isDefault, err := s.isProjectsDefaultRepository(ctx, project, run.Repository)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !isDefault {
		writeError(w, codeRepositoryNotTheProjects, http.StatusForbidden, fmt.Sprintf(
			"%s, the repository of this token's run, is not the default repository of project %s", run.Repository, project))
		return
	}

	owner, name, _ := strings.Cut(run.Repository, "/")
	reader := &repositoryReader{s: s, owner: owner, name: name}
	checkRun, err := s.specCheckRun(ctx, reader, run.Repository, run.CheckRun)
	if err != nil {
		s.writeHandlerError(w, githubReadRefusal(err, fmt.Sprintf("check run %d of %s", run.CheckRun, run.Repository),
			"as it refuses a check run the App cannot see or that does not exist, so this token's run is not proven",
			codeCheckRunNotShown, http.StatusForbidden, codeCheckRunUnreadable))
		return
	}
	if checkRun.HeadSHA != run.SHA {
		writeError(w, codeCheckRunNotTheRuns, http.StatusForbidden, fmt.Sprintf(
			"GitHub names commit %s for check run %d, and this token's run tested commit %s: the check run is no job of this run",
			checkRun.HeadSHA, run.CheckRun, run.SHA))
		return
	}
	files, err := s.specFilesAt(ctx, reader, run.Repository, run.SHA)
	if err != nil {
		writeError(w, codeFilesUnreadable, http.StatusServiceUnavailable, fmt.Sprintf(
			"GitHub did not answer the read of %s of %s at %s, so the result was not judged; write it again: %v", specDir, run.Repository, run.SHA, err))
		return
	}
	if files.Problems > 0 {
		problems := "1 problem"
		if files.Problems != 1 {
			problems = fmt.Sprintf("%d problems", files.Problems)
		}
		writeError(w, codeSpecFilesRefused, http.StatusConflict, fmt.Sprintf(
			"the spec files of %s at %s have %s, so they hold no check to write a result for. The first: %s",
			run.Repository, run.SHA, problems, files.First))
		return
	}
	check, held := files.Checks[input.Check]
	if !held {
		writeError(w, codeCheckNotInFiles, http.StatusConflict, fmt.Sprintf(
			"the spec files of %s at %s hold no check with the id %q, and a result is for a check that the files hold", run.Repository, run.SHA, input.Check))
		return
	}
	job, leg, err := s.jobOfCheckRun(ctx, reader, run, checkRun, input.Check, check)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	row := specCheckRow{
		project: project, check: input.Check, procedureDigest: check.Digest, job: job, leg: leg,
		run: run, verdict: input.Verdict, observed: cutObserved(input.Observed),
		session: actor.Credential.Session,
		test:    model.SpecCheckTest{Runner: check.Runner, Selector: check.Selector, Job: check.Job},
	}
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	result, err := insertSpecCheckResult(ctx, tx, row)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, fmt.Errorf("commit a spec check result: %w", err))
		return
	}
	WriteJSON(w, http.StatusCreated, result)
}

// validSpecCheckResultInput refuses a body that names a member other than the four, or whose
// four are not well formed, each with RESULT_INPUT naming what is wrong. A member the body names
// beyond the four is refused rather than ignored, as decodeJSON ignores one: the identity on a
// result is never a field the caller set, and a body that names one is a caller that thinks it
// sets it.
func validSpecCheckResultInput(input specCheckResultInput, body []byte) error {
	if undeclared := undeclaredMembers(&input, body); len(undeclared) > 0 {
		return errorf(http.StatusBadRequest, codeResultInput,
			"the body takes commit, check, verdict and observed alone, and names %s: Dispatch reads who wrote a result from its token, never from its body",
			strings.Join(undeclared, ", "))
	}
	if !resultCommitSHA.MatchString(input.Commit) {
		return errorf(http.StatusBadRequest, codeResultInput, "commit is the 40-character lowercase hex commit the job tested")
	}
	if strings.TrimSpace(input.Check) == "" {
		return errorf(http.StatusBadRequest, codeResultInput, "check names a check of the spec files at the commit")
	}
	switch input.Verdict {
	case "pass", "fail", "could_not_run":
	default:
		return errorf(http.StatusBadRequest, codeResultInput, "verdict is pass, fail or could_not_run")
	}
	return nil
}

// isProjectsDefaultRepository answers whether repository, the repository GitHub names a run by,
// is the default repository of project (projects.default_repo), the one repository Dispatch gives
// a project: a project has one or none (project_default_repository.go), so a project with none is
// the project of no run. The stored default is read under the name its repository is called today
// (refs.CanonicalRepoSQL over the column and Deps.Renames), as PUT
// /api/v1/projects/{key}/default-repository stores the default, so a default that a build before
// this one stored under a name its repository was renamed away from is still the run's
// repository. The run's repository is compared as the broker authenticated it and never renamed:
// GitHub names a renamed repository's runs by its new name, so a run named by an old name is a run
// of a repository made under that name since the rename, which is not the project's. GitHub spells
// a repository as its owner does and Dispatch stores the default lowercased (parseIssueRepo), so
// both sides are compared lowercased, as every comparison of a repository is.
func (s *server) isProjectsDefaultRepository(ctx context.Context, project, repository string) (bool, error) {
	var isDefault bool
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select exists (select 1 from projects where key = $1 and lower(`+refs.CanonicalRepoSQL(s.deps.Renames, "default_repo")+`) = lower($2))`,
		project, repository).Scan(&isDefault); err != nil {
		return false, fmt.Errorf("read whether %s is the default repository of %s: %w", repository, project, err)
	}
	return isDefault, nil
}

// repositoryReader mints the installation token of one repository once a read needs it, so a
// request whose two reads are both cached mints none.
type repositoryReader struct {
	s           *server
	owner, name string
	token       string
}

func (r *repositoryReader) installationToken(ctx context.Context) (string, error) {
	if r.token != "" {
		return r.token, nil
	}
	token, err := r.s.deps.GitHub.RepositoryToken(ctx, r.owner, r.name)
	if err != nil {
		return "", err
	}
	r.token = token
	return token, nil
}

// readCached is what cache holds under key, decoded from the JSON it is held as, or, where it holds
// nothing that decodes, what read answers, which is then held as JSON. An error of read is answered
// with the zero value, and nothing is held.
func readCached[T any](cache *bytelru.Cache[string, []byte], key string, read func() (T, error)) (T, error) {
	if cached, ok := cache.Get(key); ok {
		var value T
		if err := json.Unmarshal(cached, &value); err == nil {
			return value, nil
		}
	}
	value, err := read()
	if err != nil {
		var zero T
		return zero, err
	}
	if encoded, err := json.Marshal(value); err == nil {
		cache.Put(key, encoded)
	}
	return value, nil
}

// specCheckRun is check run id of repository: the process's cached answer, or GitHub's.
func (s *server) specCheckRun(ctx context.Context, reader *repositoryReader, repository string, id int64) (githubapp.CheckRun, error) {
	s.specReads.init()
	return readCached(s.specReads.checkRuns, repository+"#"+strconv.FormatInt(id, 10), func() (githubapp.CheckRun, error) {
		token, err := reader.installationToken(ctx)
		if err != nil {
			return githubapp.CheckRun{}, err
		}
		return s.deps.GitHub.CheckRun(ctx, token, reader.owner, reader.name, id)
	})
}

// specFilesAt is what the spec files of repository at commit hold: the process's cached answer, or
// the files GitHub serves read with the graph logic's one reading of them (specgraph.Judge), with
// no retired list, since the files hold no history. The spec files are the entries of the spec
// directory that specgraph.IsFile keeps, the rule spec-check reads a directory by, and a commit
// whose tree holds no spec directory holds no check. A set over the bounds of the notes package is
// a problem of the files, not a GitHub that did not answer, found from GitHub's listing before any
// file is read, as spec-check finds it (notes.ReadSet): a listing over notes.MaxFiles files or
// notes.MaxSetSize bytes in sum is one problem (notes.CheckSet), and else each listed file over
// notes.MaxFileSize is one; where none is, a file GitHub serves over it is one. A listing GitHub
// truncates, or one over the response limit Dispatch reads it within, is one problem too: a
// commit's tree never changes, so every read of it answers the same, and a write again could
// never be judged.
func (s *server) specFilesAt(ctx context.Context, reader *repositoryReader, repository, commit string) (specFiles, error) {
	s.specReads.init()
	return readCached(s.specReads.checks, repository+"@"+commit, func() (specFiles, error) {
		token, err := reader.installationToken(ctx)
		if err != nil {
			return specFiles{}, err
		}
		listing, err := s.deps.GitHub.Dir(ctx, token, reader.owner, reader.name, commit, specDir, specgraph.IsFile)
		var tooLarge *githubapp.ResponseTooLargeError
		switch {
		case errors.Is(err, githubapp.ErrTreeTruncated):
			return specFiles{Problems: 1, First: fmt.Sprintf("%v: GitHub truncated its listing of %s, as it does past 100,000 entries or 7 MB; a set holds at most %d files", notes.ErrSetTooLarge, specDir, notes.MaxFiles)}, nil
		case errors.As(err, &tooLarge):
			return specFiles{Problems: 1, First: fmt.Sprintf("%v: GitHub's listing of %s is over %d bytes, the most Dispatch reads of one listing; a set holds at most %d files, whose listing fits", notes.ErrSetTooLarge, specDir, tooLarge.Limit, notes.MaxFiles)}, nil
		case err != nil:
			return specFiles{}, err
		}
		var listed int64
		for _, file := range listing.Files {
			listed += file.Size
		}
		if err := notes.CheckSet(specDir, len(listing.Files), listed); err != nil {
			return specFiles{Problems: 1, First: err.Error()}, nil
		}
		var files specFiles
		for _, file := range listing.Files {
			if err := notes.CheckSize(file.Path, file.Size); err != nil {
				if files.Problems == 0 {
					files.First = err.Error()
				}
				files.Problems++
			}
		}
		if files.Problems == 0 {
			read, err := s.deps.GitHub.DirFiles(ctx, token, reader.owner, reader.name, listing)
			switch {
			case errors.Is(err, notes.ErrFileTooLarge), errors.Is(err, notes.ErrSetTooLarge):
				files = specFiles{Problems: 1, First: err.Error()}
			case err != nil:
				return specFiles{}, err
			default:
				files = judgeSpecFiles(read)
			}
		}
		return files, nil
	})
}

// judgeSpecFiles is what the route keeps of files as specgraph.Judge reads them: how many
// problems they have and the first, written as spec-check writes one, "<dir>/<file>:<line>:
// <node>: <detail>" with each value of the input in it cut (specgraph.Problem.Text), or, with
// none, every check by id.
func judgeSpecFiles(read map[string][]byte) specFiles {
	tree, problems := specgraph.Judge(read, specgraph.Options{})
	if len(problems) > 0 {
		first := problems[0]
		return specFiles{Problems: len(problems), First: fmt.Sprintf("%s/%s:%d: %s", specDir, first.Pos.File, first.Pos.Line, first.Text())}
	}
	files := specFiles{Checks: make(map[string]specCheckFacts, len(tree.Checks))}
	for _, check := range tree.Checks {
		facts := specCheckFacts{Digest: check.Digest()}
		if check.Test != nil {
			facts.Runner, facts.Selector, facts.Job = check.Test.Runner, check.Test.Selector, check.Test.Job
		}
		files.Checks[check.ID] = facts
	}
	return files
}

// githubReadRefusal is the refusal of a result whose read of what, GitHub's, failed with err.
// GitHub's 403 or 404 (githubapp.ErrRepositoryUnreadable) refuses Dispatch the read for good, so
// a write again is answered the same: the final refusal notShown, at status, saying why the result
// needs what. Any other failure is GitHub not answering: unreadable, a 503 that asks for the write
// again, since the result was not judged.
func githubReadRefusal(err error, what, why, notShown string, status int, unreadable string) *apiError {
	if errors.Is(err, githubapp.ErrRepositoryUnreadable) {
		return errorf(status, notShown, "GitHub refuses Dispatch the read of %s, %s: the result is refused, and a write again is refused the same: %v", what, why, err)
	}
	return errorf(http.StatusServiceUnavailable, unreadable, "GitHub did not answer the read of %s, so the result was not judged; write it again: %v", what, err)
}

// jobOfCheckRun is the job and the leg that checkRun, this token's check run, ran, where the run's
// workflow file at the run's commit shows that job to be the one check names (check being the
// facts the files give the check of that id), or the refusal: 409 CHECK_NOT_THIS_JOBS saying why,
// or githubReadRefusal's for the file. A check run is the job's when its name is the job's name and
// the file shows no other job makes that name (workflowJobs.whyNotTheJob); or when its name is the
// job's name and a leg in parentheses (legOf) and the file makes the job a matrix job
// (workflowJobs.whyNotALeg). A name of neither shape is refused before the file is read.
func (s *server) jobOfCheckRun(ctx context.Context, reader *repositoryReader, run oauthwire.Run, checkRun githubapp.CheckRun, id string, check specCheckFacts) (job, leg string, err error) {
	isLeg := false
	if checkRun.Name != check.Job {
		if leg, isLeg = legOf(checkRun.Name, check.Job); !isLeg {
			named := "names no job"
			if check.Job != "" {
				named = fmt.Sprintf("names the job %q", check.Job)
			}
			return "", "", errorf(http.StatusConflict, codeCheckNotThisJobs,
				"check %s %s, and this token's job is %q (check run %d): a job writes results only for the checks that name it",
				id, named, checkRun.Name, run.CheckRun)
		}
	}
	jobs, err := s.specWorkflowAt(ctx, reader, run.Repository, run.SHA, run.Workflow)
	if err != nil {
		return "", "", githubReadRefusal(err, fmt.Sprintf("%s of %s at %s, the run's workflow file", run.Workflow, run.Repository, run.SHA),
			fmt.Sprintf("which says which job makes the check run %d, %q", run.CheckRun, checkRun.Name),
			codeWorkflowNotShown, http.StatusConflict, codeWorkflowUnreadable)
	}
	where := run.Workflow + " at " + run.SHA
	if isLeg {
		if why := jobs.whyNotALeg(checkRun.Name, check.Job, where); why != "" {
			return "", "", errorf(http.StatusConflict, codeCheckNotThisJobs,
				"check %s names the job %q, and this token's check run %d is named %q, a leg of %q only where the run's workflow file makes %q a matrix job, and %s: a job writes results only for the checks that name it",
				id, check.Job, run.CheckRun, checkRun.Name, check.Job, check.Job, why)
		}
		return check.Job, leg, nil
	}
	if why := jobs.whyNotTheJob(checkRun.Name, where); why != "" {
		return "", "", errorf(http.StatusConflict, codeCheckNotThisJobs,
			"check %s names the job %q, and this token's check run %d is named %q, that job's check run only where the run's workflow file says so, and %s: a job writes results only for the checks that name it",
			id, check.Job, run.CheckRun, checkRun.Name, why)
	}
	return checkRun.Name, "", nil
}

// legOf reads name, a check run's name, as GitHub names the check run of one leg of a matrix job
// called job: the job's name, " (", the leg's matrix values, ")". The leg is all that the
// parentheses hold, a matrix value that holds parentheses of its own included; ok is false for any
// other name, and for a job of no name. GitHub writes that shape for a job that is no leg too, one
// whose own name ends in parentheses, so the shape alone makes no check run a leg: the route
// takes the leg only where the run's workflow file says so (workflowJobs.whyNotALeg).
func legOf(name, job string) (leg string, ok bool) {
	if job == "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(name, job+" (")
	if !ok {
		return "", false
	}
	leg, ok = strings.CutSuffix(rest, ")")
	return leg, ok && leg != ""
}

// whyNotTheJob is why jobs, the jobs of the run's workflow file (where names that file at its
// commit), do not show the check run called name to be the job of that very name, or "" where they
// show it. They show it where exactly one job with no matrix is named name, no matrix job makes
// that name for a leg, and no job's name holds an expression: a matrix job named name makes no
// check run of that name, since every leg's name carries its values, while a second job of the
// name, a matrix job J whose leg's name is the name (legOf(name, J)), or a name a run evaluates,
// makes a check run of that name whose job the name does not say. A matrix job's legs are known
// only once a run expands its matrix, so the leg's shape alone refuses the name.
func (jobs workflowJobs) whyNotTheJob(name, where string) string {
	switch {
	case jobs.Problem != "":
		return fmt.Sprintf("%s does not say which jobs make a check run of that name: %s", where, jobs.Problem)
	case jobs.Expressions:
		return fmt.Sprintf("a job in %s is named by an expression, so the name of a check run its run makes is known only to that run, and this name does not say which job ran", where)
	case jobs.named(name) > 1:
		return fmt.Sprintf("%d jobs in %s are named %q, so the name does not say which job ran", jobs.named(name), where, name)
	case !slices.Contains(jobs.Plain, name):
		return fmt.Sprintf("no job with no matrix in %s is named %q, and a matrix job of that name makes no check run of it", where, name)
	}
	for _, matrixJob := range jobs.Matrix {
		if leg, ok := legOf(name, matrixJob); ok {
			return fmt.Sprintf("the matrix job %q in %s makes a check run of that name for its leg %q, so the name does not say which job ran", matrixJob, where, leg)
		}
	}
	return ""
}

// whyNotALeg is why jobs, the jobs of the run's workflow file (where names that file at its
// commit), do not show the check run called name, which legOf reads as a leg of job, to be a leg
// of job, or "" where they show it. They show it where some job named job has a matrix, no job
// with no matrix is named name, and no job's name holds an expression: such a job, or a name a run
// evaluates, makes a check run of that very name, so the name would not say which job ran. A file
// that says nothing of its jobs shows nothing.
func (jobs workflowJobs) whyNotALeg(name, job, where string) string {
	switch {
	case jobs.Problem != "":
		return fmt.Sprintf("%s does not say which jobs have a matrix: %s", where, jobs.Problem)
	case jobs.Expressions:
		return fmt.Sprintf("a job in %s is named by an expression, so the name of a check run its run makes is known only to that run, and this name does not say which job ran", where)
	case !slices.Contains(jobs.Matrix, job):
		return fmt.Sprintf("no job named %q in %s has a matrix", job, where)
	case slices.Contains(jobs.Plain, name):
		return fmt.Sprintf("the job named %q in %s has no matrix, so its check run has that name too, and the name does not say which job ran", name, where)
	}
	return ""
}

// named is how many of the file's jobs, with a matrix or without, are named name.
func (jobs workflowJobs) named(name string) int {
	count := 0
	for _, held := range [][]string{jobs.Matrix, jobs.Plain} {
		for _, job := range held {
			if job == name {
				count++
			}
		}
	}
	return count
}

// specWorkflowAt is what the workflow file at path in repository at commit says of its jobs: the
// process's cached answer, or the file GitHub serves read with readWorkflowJobs. A file over
// notes.MaxFileSize, or one whose answer is over the response limit Dispatch reads a file within,
// is a problem of the file and not a GitHub that did not answer: a commit's file never changes, so
// every read of it answers the same.
func (s *server) specWorkflowAt(ctx context.Context, reader *repositoryReader, repository, commit, path string) (workflowJobs, error) {
	s.specReads.init()
	return readCached(s.specReads.workflows, repository+"@"+commit+":"+path, func() (workflowJobs, error) {
		token, err := reader.installationToken(ctx)
		if err != nil {
			return workflowJobs{}, err
		}
		content, err := s.deps.GitHub.File(ctx, token, reader.owner, reader.name, commit, path)
		var tooLarge *githubapp.ResponseTooLargeError
		switch {
		case errors.Is(err, notes.ErrFileTooLarge):
			return workflowJobs{Problem: err.Error()}, nil
		case errors.As(err, &tooLarge):
			return workflowJobs{Problem: fmt.Sprintf("GitHub's answer for it is over %d bytes, the most Dispatch reads of one file", tooLarge.Limit)}, nil
		case err != nil:
			return workflowJobs{}, err
		}
		return readWorkflowJobs(content), nil
	})
}

// readWorkflowJobs reads the jobs of a workflow file (workflowJobs). A job's name is its `name:`
// where that is set, else its id, as GitHub names the job's check runs. A job has a matrix where
// its `strategy.matrix` is a mapping with a key, or an expression, which GitHub expands into legs
// when the run starts. A file that YAML cannot read into jobs is a problem of the file.
func readWorkflowJobs(content []byte) workflowJobs {
	var workflow struct {
		Jobs map[string]struct {
			Name     yaml.Node `yaml:"name"`
			Strategy struct {
				Matrix yaml.Node `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(content, &workflow); err != nil {
		return workflowJobs{Problem: fmt.Sprintf("it does not read as a workflow file: %v", err)}
	}
	var jobs workflowJobs
	for id, job := range workflow.Jobs {
		name := id
		if node := yamlValue(&job.Name); node.Kind != 0 && node.Tag != "!!null" {
			if node.Kind != yaml.ScalarNode {
				jobs.Expressions = true
				continue
			}
			name = node.Value
		}
		if strings.Contains(name, "${{") {
			jobs.Expressions = true
			continue
		}
		matrix := yamlValue(&job.Strategy.Matrix)
		if matrix.Kind == yaml.MappingNode && len(matrix.Content) > 0 ||
			matrix.Kind == yaml.ScalarNode && matrix.Tag != "!!null" && matrix.Value != "" {
			jobs.Matrix = append(jobs.Matrix, name)
		} else {
			jobs.Plain = append(jobs.Plain, name)
		}
	}
	// Sorted, so one file is held as one encoding whatever order the map gives its jobs in.
	slices.Sort(jobs.Matrix)
	slices.Sort(jobs.Plain)
	return jobs
}

// yamlValue is the node an alias names, followed to the end, or node itself where it is no alias.
func yamlValue(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	return node
}

// cutObserved is what a result keeps of what a job observed: the text whole when it is
// contracts.SpecCheckObservedMax runes or fewer, else its first contracts.SpecCheckObservedStart
// runes, contracts.SpecCheckObservedCutNote and its last contracts.SpecCheckObservedEnd runes,
// that many runes in all, so the stored text keeps the test's name at its start and why it failed
// at its end, and says that the middle was cut. `dispatch result --project` cuts by the same rule
// before it sends the text.
func cutObserved(text string) string {
	runes := utf8.RuneCountInString(text)
	if runes <= contracts.SpecCheckObservedMax {
		return text
	}
	return text[:runeOffset(text, contracts.SpecCheckObservedStart)] + contracts.SpecCheckObservedCutNote +
		text[runeOffset(text, runes-contracts.SpecCheckObservedEnd):]
}

// runeOffset is the byte offset in text of its rune numbered n, counted from 0, and len(text)
// for an n past its last rune.
func runeOffset(text string, n int) int {
	runes := 0
	for i := range text {
		if runes == n {
			return i
		}
		runes++
	}
	return len(text)
}

// specCheckRow is one row of spec_check_results before it is written: what the request and the
// spec files gave, and its token's run read whole (oauthwire.Run), from which the row's
// repository, commit, check run, run and attempt columns and the attestation's subject, run and
// recorder are all derived. The attestation the row stores is built from it alone
// (insertSpecCheckResult).
type specCheckRow struct {
	project, check, procedureDigest, job, leg string
	run                                       oauthwire.Run
	verdict, observed, session                string
	test                                      model.SpecCheckTest
}

// insertSpecCheckResult is the one writer of spec_check_results: it appends row with the in-toto
// statement of it, whose recorded_at is the row's own (the transaction's now(), written in UTC
// with a Z so every reader decodes one instant the same way), and answers the row as the read
// answers it. Nothing updates or deletes a row.
func insertSpecCheckResult(ctx context.Context, q queryer, row specCheckRow) (model.SpecCheckResult, error) {
	run := row.run
	statement := model.SpecCheckAttestation{
		Type: inTotoStatementType,
		Subject: []model.AttestationSubject{{
			Name: "git+https://github.com/" + run.Repository, Digest: map[string]string{"gitCommit": run.SHA},
		}},
		PredicateType: specCheckResultPredicateType,
		Predicate: model.SpecCheckPredicate{
			Project: row.project, Check: row.check, ProcedureDigest: row.procedureDigest, Test: row.test,
			Verdict: row.verdict, Observed: row.observed, Job: row.job, Leg: row.leg,
			Run: model.SpecCheckRun{
				Repository: run.Repository, WorkflowRef: run.WorkflowRef, Ref: run.Ref, EventName: run.EventName,
				RunID: strconv.FormatInt(run.ID, 10), Attempt: int(run.Attempt), CheckRunID: strconv.FormatInt(run.CheckRun, 10),
				RunnerEnvironment: run.RunnerEnvironment,
			},
			// The principal is the broker's own spelling of the run's workflow (oauthwire.ResultPrincipal),
			// so the identity on a result is the one the broker authenticated.
			Recorder: model.SpecCheckRecorder{
				Session: row.session, Principal: oauthwire.ResultPrincipal(run.Repository, run.Workflow),
			},
		},
	}
	encoded, err := json.Marshal(statement)
	if err != nil {
		return model.SpecCheckResult{}, fmt.Errorf("encode a spec check result: %w", err)
	}
	var (
		result model.SpecCheckResult
		stored []byte
	)
	if err := q.QueryRow(ctx, `
		insert into spec_check_results (project_key, repository, check_id, commit_sha, procedure_digest, job, leg,
			check_run_id, run_id, attempt, verdict, attestation, session)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			jsonb_set($12::jsonb, '{predicate,recorded_at}',
				to_jsonb(to_char(now() at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))), $13)
		returning id::text, seq, attestation`,
		row.project, run.Repository, row.check, run.SHA, row.procedureDigest, row.job, row.leg,
		run.CheckRun, run.ID, run.Attempt, row.verdict, encoded, row.session,
	).Scan(&result.ID, &result.Seq, &stored); err != nil {
		return model.SpecCheckResult{}, fmt.Errorf("insert a spec check result: %w", err)
	}
	if err := json.Unmarshal(stored, &result.Attestation); err != nil {
		return model.SpecCheckResult{}, fmt.Errorf("decode a stored spec check result: %w", err)
	}
	result.Kind = model.SpecCheckResultKindRun
	return result, nil
}

// listSpecCheckResults is GET /api/v1/projects/{key}/results: every stored result of the project
// at ?commit=, of ?check= alone when it is given, newest first by record number. A results token
// never reaches it (refuseLimitedToken): it writes results and reads nothing.
func (s *server) listSpecCheckResults(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := r.URL.Query()
	commit, check := query.Get("commit"), query.Get("check")
	if !resultCommitSHA.MatchString(commit) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest, "commit is the 40-character lowercase hex commit whose results to read")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select id::text, seq, attestation from spec_check_results
		where project_key = $1 and commit_sha = $2 and ($3 = '' or check_id = $3)
		order by seq desc`, r.PathValue("key"), commit, check)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read spec check results: %w", err))
		return
	}
	defer rows.Close()
	answer := specCheckResults{Results: []model.SpecCheckResult{}}
	for rows.Next() {
		result := model.SpecCheckResult{Kind: model.SpecCheckResultKindRun}
		var stored []byte
		if err := rows.Scan(&result.ID, &result.Seq, &stored); err != nil {
			s.writeHandlerError(w, fmt.Errorf("read a spec check result: %w", err))
			return
		}
		if err := json.Unmarshal(stored, &result.Attestation); err != nil {
			s.writeHandlerError(w, fmt.Errorf("decode a stored spec check result: %w", err))
			return
		}
		answer.Results = append(answer.Results, result)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read spec check results: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, answer)
}
