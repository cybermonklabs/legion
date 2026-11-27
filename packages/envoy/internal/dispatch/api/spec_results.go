package api

// spec_results.go is the route a CI job writes the result of a check of the spec files with,
// POST /api/v1/projects/{key}/results, and the read beside it (R12 of the spec). The job holds a
// results token the secrets broker minted for its run (ciphertoken.Caller.ResultsOnly), and this
// route is one of the two routes that token reaches (resultsScopeRoutes), beside
// POST /api/v1/issues/{key}/results. Dispatch takes who wrote the result and what it is for from
// that token, from GitHub and from the spec files at the run's commit; the body says only what
// the job observed. The recorder of a result is the run, and no person: the broker's result entry
// names a person, whom nobody authenticated for this write and who ran no test, so that person
// stays inside the server. Each result is a new row of spec_check_results, which nothing updates
// or deletes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

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
	// name (refuseResultsToken).
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
	// codeSpecFilesRefused refuses a result at a commit whose spec files the graph logic refuses
	// (specgraph.Judge): such files hold no check to write a result for.
	codeSpecFilesRefused = "SPEC_FILES_REFUSED"
	// codeCheckNotInFiles refuses a check the spec files at the run's commit do not hold.
	codeCheckNotInFiles = "CHECK_NOT_IN_FILES"
	// codeCheckNotThisJobs refuses a check whose test part names another job than the run's.
	codeCheckNotThisJobs = "CHECK_NOT_THIS_JOBS"
	// codeCheckRunUnreadable and codeFilesUnreadable are GitHub not answering, a 503: the
	// result was not judged, and the job writes it again.
	codeCheckRunUnreadable = "CHECK_RUN_UNREADABLE"
	codeFilesUnreadable    = "FILES_UNREADABLE"
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

// specResultReads keeps what the route reads of GitHub for the life of the process, since
// neither changes: a check run's name and commit, by repository and id, and what the spec files
// hold, by repository and commit. Each is held as JSON in a byte-bounded cache, so many runs and
// commits cannot grow it without bound (bytelru).
type specResultReads struct {
	once      sync.Once
	checkRuns *bytelru.Cache[string, []byte]
	checks    *bytelru.Cache[string, []byte]
}

// The bounds of the two caches: a check run is a hundred bytes, and a commit's checks a few
// hundred bytes each.
const (
	specCheckRunCacheBytes = 1 << 20
	specChecksCacheBytes   = 16 << 20
)

func (c *specResultReads) init() {
	c.once.Do(func() {
		c.checkRuns = bytelru.New[string, []byte](specCheckRunCacheBytes)
		c.checks = bytelru.New[string, []byte](specChecksCacheBytes)
	})
}

// recordSpecCheckResult is POST /api/v1/projects/{key}/results. The checks run in this order,
// each before anything is read of GitHub that it does not need: the credential is a results
// token; the body names the four fields alone, well formed; its commit is the run's; the run's
// repository is the project's default repository; GitHub names the run's check run at that
// commit; the graph logic takes the files at that commit; the files hold the check; and the
// check's job is the check run's. The project check of the path is withinProjectScope's, which
// register puts in front of this handler.
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
		s.writeHandlerError(w, commitNotTheRuns(input.Commit, run.SHA))
		return
	}
	numbers, err := runNumbersOf(run)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	principal, err := resultPrincipal(run)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
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
	checkRun, err := s.specCheckRun(ctx, reader, run.Repository, numbers.checkRun)
	if err != nil {
		writeError(w, codeCheckRunUnreadable, http.StatusServiceUnavailable, fmt.Sprintf(
			"GitHub did not answer the read of check run %d of %s, so the result was not judged; write it again: %v", numbers.checkRun, run.Repository, err))
		return
	}
	if checkRun.HeadSHA != run.SHA {
		s.writeHandlerError(w, checkRunNotTheRuns(checkRun.HeadSHA, numbers.checkRun, run.SHA))
		return
	}
	job, _ := jobAndLeg(checkRun.Name)
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
	if check.Job != job {
		named := "names no job"
		if check.Job != "" {
			named = fmt.Sprintf("names the job %q", check.Job)
		}
		writeError(w, codeCheckNotThisJobs, http.StatusConflict, fmt.Sprintf(
			"check %s %s, and this token's job is %q (check run %d, %q): a job writes results only for the checks that name it",
			input.Check, named, job, numbers.checkRun, checkRun.Name))
		return
	}

	row := specCheckRow{
		result: attestedResult{
			predicateType: specCheckResultPredicateType, project: project, check: input.Check,
			procedureDigest: check.Digest,
			test:            model.ResultTest{Runner: check.Runner, Selector: check.Selector, Job: check.Job},
			verdict:         input.Verdict, observed: cutObserved(input.Observed), checkRunName: checkRun.Name,
			run: run, attempt: numbers.attempt, session: actor.Credential.Session, principal: principal,
		},
		checkRunID: numbers.checkRun, runID: numbers.run,
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

// commitNotTheRuns refuses a result for commit, another commit than tested, the commit the run of
// the results token tested. Both results routes answer it.
func commitNotTheRuns(commit, tested string) *apiError {
	return errorf(http.StatusForbidden, codeCommitNotTheRuns,
		"the result is for commit %s, and this token's run tested commit %s: a job writes results only for the commit that it tested",
		commit, tested)
}

// checkRunNotTheRuns refuses a results token whose check run, checkRun, GitHub names at named,
// another commit than tested, the commit its run tested. Both results routes answer it.
func checkRunNotTheRuns(named string, checkRun int64, tested string) *apiError {
	return errorf(http.StatusForbidden, codeCheckRunNotTheRuns,
		"GitHub names commit %s for check run %d, and this token's run tested commit %s: the check run is no job of this run",
		named, checkRun, tested)
}

// runNumbers is the three numbers of a run, read from its token: its check run, its workflow run
// and its attempt.
type runNumbers struct {
	checkRun, run int64
	attempt       int
}

// runNumbersOf reads the run's numbers. The broker mints a results token only for a run whose
// numbers are positive decimals and the verifier admits no other (ciphertoken.shapeOf), so a
// number that does not read is Dispatch's own fault, a 500.
func runNumbersOf(run oauthwire.CIRun) (runNumbers, error) {
	checkRun, err := strconv.ParseInt(run.CheckRunID, 10, 64)
	if err != nil {
		return runNumbers{}, fmt.Errorf("read the check run of a results token: %w", err)
	}
	workflowRun, err := strconv.ParseInt(run.RunID, 10, 64)
	if err != nil {
		return runNumbers{}, fmt.Errorf("read the run of a results token: %w", err)
	}
	attempt, err := strconv.Atoi(run.RunAttempt)
	if err != nil {
		return runNumbers{}, fmt.Errorf("read the attempt of a results token: %w", err)
	}
	return runNumbers{checkRun: checkRun, run: workflowRun, attempt: attempt}, nil
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
// no retired list, since the files hold no history. A commit whose tree holds no spec directory
// holds no check. A set over the bounds of the notes package is a problem of the files, not a
// GitHub that did not answer, found from GitHub's listing before any file is read, as spec-check
// finds it (notes.ReadSet): a listing over notes.MaxFiles files or notes.MaxSetSize bytes in sum
// is one problem (notes.CheckSet), and else each listed file over notes.MaxFileSize is one; where
// none is, a file GitHub serves over it is one. A listing GitHub truncates, or one over the
// response limit Dispatch reads it within, is one problem too: a commit's tree never changes, so
// every read of it answers the same, and a write again could never be judged.
func (s *server) specFilesAt(ctx context.Context, reader *repositoryReader, repository, commit string) (specFiles, error) {
	s.specReads.init()
	return readCached(s.specReads.checks, repository+"@"+commit, func() (specFiles, error) {
		token, err := reader.installationToken(ctx)
		if err != nil {
			return specFiles{}, err
		}
		listing, err := s.deps.GitHub.Dir(ctx, token, reader.owner, reader.name, commit, specDir)
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
// <node>: <detail>", or, with none, every check by id.
func judgeSpecFiles(read map[string][]byte) specFiles {
	tree, problems := specgraph.Judge(read, specgraph.Options{})
	if len(problems) > 0 {
		first := problems[0]
		return specFiles{Problems: len(problems), First: fmt.Sprintf("%s/%s:%d: %s: %s", specDir, first.Pos.File, first.Pos.Line, first.Node, first.Detail)}
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

// jobAndLeg reads a check run's name as GitHub names a job's check run: the job's name, then its
// matrix values in parentheses. A name ending in ")" splits at its last " (" into the job and
// its leg; any other name is the job with no leg.
func jobAndLeg(name string) (job, leg string) {
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		return name[:i], name[i+2 : len(name)-1]
	}
	return name, ""
}

// resultPrincipal is the broker's principal of a result entry's run, read by the broker's own
// rule from what the broker authenticated (oauthwire.WorkflowPath, oauthwire.ResultPrincipal):
// workflow-results:, the repository and the path of the workflow file, which its workflow_ref
// names before "@" and the run's ref. The verifier admits a results token only where the path
// reads (ciphertoken.wholeRun), so a run whose path does not read is Dispatch's own fault, a 500.
func resultPrincipal(run oauthwire.CIRun) (string, error) {
	workflow, ok := oauthwire.WorkflowPath(run.WorkflowRef, run.Repository, run.Ref)
	if !ok {
		return "", fmt.Errorf("read the workflow of a results token: %s is not %s/<path>@%s", run.WorkflowRef, run.Repository, run.Ref)
	}
	return oauthwire.ResultPrincipal(run.Repository, workflow), nil
}

// cutObserved is what a result keeps of what a job observed, on both results routes: the text
// whole when it is contracts.SpecCheckObservedMax runes or fewer, else its first
// contracts.SpecCheckObservedStart runes, contracts.SpecCheckObservedCutNote and its last
// contracts.SpecCheckObservedEnd runes, that many runes in all, so the stored text keeps the
// test's name at its start and why it failed at its end, and says that the middle was cut.
// `dispatch result --project` cuts by the same rule before it sends the text.
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

// specCheckRow is one row of spec_check_results before it is written: the result whose statement
// it stores (attestedResult.statement), which every column but the run's two numbers is read
// from, so no column can say another thing than the statement, and the check run and the
// workflow run as runNumbersOf reads them.
type specCheckRow struct {
	result            attestedResult
	checkRunID, runID int64
}

// insertSpecCheckResult is the one writer of spec_check_results: it appends row with its
// statement, whose recorded_at is the row's own (stampedAttestation), and answers the row as the
// read answers it. Nothing updates or deletes a row.
func insertSpecCheckResult(ctx context.Context, q queryer, row specCheckRow) (model.SpecCheckResult, error) {
	statement := row.result.statement()
	encoded, err := json.Marshal(statement)
	if err != nil {
		return model.SpecCheckResult{}, fmt.Errorf("encode a spec check result: %w", err)
	}
	predicate := statement.Predicate
	var (
		result model.SpecCheckResult
		stored []byte
	)
	if err := q.QueryRow(ctx, `
		insert into spec_check_results (project_key, repository, check_id, commit_sha, procedure_digest, job, leg,
			check_run_id, run_id, attempt, verdict, attestation, session)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, `+stampedAttestation("$12")+`, $13)
		returning id::text, seq, attestation`,
		predicate.Project, predicate.Run.Repository, predicate.Check, statement.Subject[0].Digest["gitCommit"],
		predicate.ProcedureDigest, predicate.Job, predicate.Leg, row.checkRunID, row.runID, predicate.Run.Attempt,
		predicate.Verdict, encoded, predicate.Recorder.Session,
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
// never reaches it (refuseResultsToken): it writes results and reads nothing.
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
