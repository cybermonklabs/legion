// spec_results_test.go holds POST and GET /api/v1/projects/{key}/results to R12 of the spec:
// a CI job writes the result of a check of the spec files with its own token, Dispatch takes the
// commit, the job and the procedure from the token, from GitHub and from the files at that
// commit, never from the body, and appends each result as a new record.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/githubtest"
)

const (
	// specResultsProject is the project the CI job's results token writes the results of.
	specResultsProject = "ACME"
	// specResultsRepository is the repository of the job's run, the project's default repository.
	specResultsRepository = "example-org/example-repo"
	// specResultsCommit is the commit of the job's run, whose files hold the probe checks.
	specResultsCommit = "0123456789abcdef0123456789abcdef01234567"
	// specResultsOtherCommit is a commit the token does not name.
	specResultsOtherCommit = "89abcdef0123456789abcdef0123456789abcdef"
	// specResultsCheckRun is the check run of the job the default token was minted in.
	specResultsCheckRun = 97
)

// The checks of specResultsFiles: one of job go-checks, one of job envoy-go, one of the matrix job
// m, one of the job named `foo (bar)`, which has no matrix, and one of the matrix job build.
const (
	probeCheck         = "probe-passes"
	otherJobsCheck     = "second-probe-passes"
	matrixCheck        = "matrix-probe-passes"
	parenthesizedCheck = "parenthesized-probe-passes"
	buildCheck         = "build-probe-passes"
)

// specResultsFiles is .dispatch/spec at specResultsCommit: one statement and its five program
// checks, probeCheck, otherJobsCheck, matrixCheck, parenthesizedCheck and buildCheck. It is a tree
// the graph logic takes (specgraph.Judge finds no problem in it), as the files of a commit a job
// reports on must be.
var specResultsFiles = map[string]string{
	"probe.yaml": `---
type: requirement
id: the-probe-holds
kind: behavior
text:
  - The probe test passes on each commit.
---
type: check
id: probe-passes
serves: the-probe-holds
subject: code
procedure: program
test:
  runner: go
  selector: example.com/m/p TestP
  job: go-checks
claim: The probe test passes.
procedure_text:
  - The program runs the probe test one time.
failing_example:
  - A probe test that fails.
---
type: check
id: second-probe-passes
serves: the-probe-holds
subject: code
procedure: program
test:
  runner: go
  selector: example.com/m/p TestQ
  job: envoy-go
claim: The second probe test passes.
procedure_text:
  - The program runs the second probe test one time.
failing_example:
  - A second probe test that fails.
---
type: check
id: matrix-probe-passes
serves: the-probe-holds
subject: code
procedure: program
test:
  runner: go
  selector: example.com/m/p TestM
  job: m
claim: The matrix probe test passes on each leg.
procedure_text:
  - The program runs the matrix probe test on each leg.
failing_example:
  - A matrix probe test that fails on one leg.
---
type: check
id: parenthesized-probe-passes
serves: the-probe-holds
subject: code
procedure: program
test:
  runner: go
  selector: example.com/m/p TestR
  job: foo (bar)
claim: The parenthesized probe test passes.
procedure_text:
  - The program runs the parenthesized probe test one time.
failing_example:
  - A parenthesized probe test that fails.
---
type: check
id: build-probe-passes
serves: the-probe-holds
subject: code
procedure: program
test:
  runner: go
  selector: example.com/m/p TestB
  job: build
claim: The build probe test passes on each leg.
procedure_text:
  - The program runs the build probe test on each leg.
failing_example:
  - A build probe test that fails on one leg.
`,
}

// specPaddingFile is a spec file of size bytes that the graph logic takes (specgraph.Judge finds
// no problem in it): one term, padding-term<n>, as print writes it, whose definition is a run of
// letters as long as the size needs. A size below the shortest such file fails the test.
func specPaddingFile(t *testing.T, n, size int) string {
	t.Helper()
	term := specgraph.Term{ID: fmt.Sprintf("padding-term%04d", n), Name: "padding", Definition: "a"}
	shortest, err := term.Print()
	if err != nil || size < len(shortest) {
		t.Fatalf("a padding file of %d bytes: the shortest is %d bytes (%v)", size, len(shortest), err)
	}
	term.Definition = strings.Repeat("a", size-len(shortest)+1)
	printed, err := term.Print()
	if err != nil || len(printed) != size {
		t.Fatalf("the padding file is %d bytes, want %d (%v)", len(printed), size, err)
	}
	return printed
}

// specResultsWorkflowPath is the workflow file of the default run, the path its workflow_ref names.
const specResultsWorkflowPath = ".github/workflows/ci-results.yaml"

// specResultsWorkflow is the workflow file at specResultsCommit: the jobs go-checks and envoy-go
// with no matrix, the matrix job m, the job foo named `foo (bar)` with no matrix, and the matrix
// job build, one of whose matrix values holds parentheses of its own.
const specResultsWorkflow = `name: ci-results
on: push
jobs:
  go-checks:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  envoy-go:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  m:
    strategy:
      matrix:
        leg: [a, b]
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  foo:
    name: foo (bar)
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  build:
    strategy:
      matrix:
        os: [linux, "linux (arm)"]
    runs-on: ${{ matrix.os }}
    steps:
      - run: go test ./...
`

// specCheckDigest is the procedure digest the files give check id, read with the graph logic's
// one reading of spec files, as the route must, which finds no problem in them.
func specCheckDigest(t *testing.T, id string) string {
	t.Helper()
	files := map[string][]byte{}
	for name, content := range specResultsFiles {
		files[name] = []byte(content)
	}
	tree, problems := specgraph.Judge(files, specgraph.Options{})
	if len(problems) > 0 {
		t.Fatalf("the graph logic refuses the fixture files: %+v", problems)
	}
	check, ok := tree.CheckByID(id)
	if !ok {
		t.Fatalf("the fixture files hold no check %s", id)
	}
	return check.Digest()
}

// specCheckRunAnswer is one check run as the stand-in GitHub serves it.
type specCheckRunAnswer struct {
	name, headSHA string
}

// specGitHub is the GitHub the route reads: the installation of the repository, its check runs by
// id, the tree and blobs of .dispatch/spec at a commit, and workflow files by commit and path. A
// status set in checkRunStatus, filesStatus or workflowStatus answers every read of that kind with
// it. truncated marks each tree listing truncated, as GitHub marks one past its own bounds, and
// padding lists that many entries beside the files that are no spec files (nested directories). It
// counts each read, so a test can see a cached read made once, and a file read that never
// happened, and keeps each workflow file asked for as "<commit> <path>".
type specGitHub struct {
	mu             sync.Mutex
	checkRuns      map[int64]specCheckRunAnswer
	files          map[string]map[string]string
	workflows      map[string]map[string]string
	checkRunStatus int
	filesStatus    int
	workflowStatus int
	truncated      bool
	padding        int
	checkRunReads  int
	treeReads      int
	blobReads      int
	workflowsAsked []string
}

func (g *specGitHub) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","checks":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"token":"ghs_fake_77","expires_at":"2099-01-01T00:00:00Z"}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.checkRunReads++
		if g.checkRunStatus != 0 {
			w.WriteHeader(g.checkRunStatus)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		run, ok := g.checkRuns[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d,"name":%q,"head_sha":%q,"status":"in_progress"}`, id, run.name, run.headSHA)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/trees/{ref}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.treeReads++
		if g.filesStatus != 0 {
			w.WriteHeader(g.filesStatus)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		commit, dir, _ := strings.Cut(r.PathValue("ref"), ":")
		files, ok := g.files[commit]
		if dir != ".dispatch/spec" || !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		// GitHub lists a tree's entries in name order, as a checkout's directory reads.
		var entries []map[string]any
		for _, name := range slices.Sorted(maps.Keys(files)) {
			entries = append(entries, map[string]any{"path": name, "mode": "100644", "type": "blob", "sha": "blob-" + commit + "-" + name, "size": len(files[name])})
		}
		for i := range g.padding {
			entries = append(entries, map[string]any{"path": fmt.Sprintf("nested-%04d-%s", i, strings.Repeat("d", 200)), "mode": "040000", "type": "tree", "sha": fmt.Sprintf("%040d", i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "tree-" + commit, "truncated": g.truncated, "tree": entries})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/blobs/{sha}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.blobReads++
		commit, name, _ := strings.Cut(strings.TrimPrefix(r.PathValue("sha"), "blob-"), "-")
		content, ok := g.files[commit][name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"})
	})
	// The contents read as GitHub answers it under the object media type: a file of over 1 MB is
	// served with its size, no content and the encoding "none".
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		commit, path := r.URL.Query().Get("ref"), r.PathValue("path")
		g.workflowsAsked = append(g.workflowsAsked, commit+" "+path)
		if g.workflowStatus != 0 {
			w.WriteHeader(g.workflowStatus)
			fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		content, ok := g.workflows[commit][path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		answer := map[string]any{"type": "file", "path": path, "size": len(content), "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content))}
		if len(content) > 1<<20 {
			answer["encoding"], answer["content"] = "none", ""
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	return githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
}

// specResultsRig is a Dispatch whose verifier serves results tokens as cmd/dispatch's does, a
// GitHub that holds the run's check run and files, and project ACME whose default repository is
// the run's repository.
type specResultsRig struct {
	*brokerTokenRig
	github *specGitHub
	caller *brokerCaller
	tokens int
}

func newSpecResultsRig(t *testing.T) *specResultsRig {
	t.Helper()
	return newSpecResultsRigOn(t, testServerOptions{})
}

// newSpecResultsRigOn is newSpecResultsRig on a server built from serving, for a test that gives
// the server a store of its own and the rename pairs read from it (renamedRepositoryStore). The
// project's default repository is set as a person sets it, through PUT
// /api/v1/projects/{key}/default-repository.
func newSpecResultsRigOn(t *testing.T, serving testServerOptions) *specResultsRig {
	t.Helper()
	github := &specGitHub{
		checkRuns: map[int64]specCheckRunAnswer{specResultsCheckRun: {name: "go-checks", headSHA: specResultsCommit}},
		files:     map[string]map[string]string{specResultsCommit: specResultsFiles},
		workflows: map[string]map[string]string{specResultsCommit: {specResultsWorkflowPath: specResultsWorkflow}},
	}
	_, pemText := githubapptest.Key(t)
	serving.app = &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText}
	serving.githubAPIBase = github.serve(t)
	rig := newBrokerTokenRigOn(t, serving)
	for _, project := range []string{specResultsProject, "OTHER"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	if set := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+specResultsProject+"/default-repository", map[string]any{"repo": specResultsRepository}); set.Code != http.StatusOK {
		t.Fatalf("set %s's default repository to %s: status=%d body=%s", specResultsProject, specResultsRepository, set.Code, set.Body.String())
	}
	return &specResultsRig{brokerTokenRig: rig, github: github, caller: newBrokerCaller(t)}
}

// token has the broker answer a results token of project ACME for the default run, with change
// applied to the answer, and answers the token.
func (r *specResultsRig) token(change func(*ciphertoken.Introspection)) string {
	r.tokens++
	token := fmt.Sprintf("a-results-token-%d", r.tokens)
	answer := resultsOnlyToken(r.caller.jkt, specResultsProject)
	if change != nil {
		change(&answer)
	}
	r.broker.Says(token, answer)
	return token
}

// post writes body to project's results route under token.
func (r *specResultsRig) post(t *testing.T, project, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return r.serve(r.caller.presenting(t, http.MethodPost, "/api/v1/projects/"+project+"/results", token, body))
}

// passOf is the body a job writes for check: a pass at the default commit.
func passOf(check string) map[string]any {
	return map[string]any{"commit": specResultsCommit, "check": check, "verdict": "pass", "observed": "ok  \texample.com/m/p\t0.01s"}
}

// read is every stored result of project ACME at commit, of check when it is not empty, as the
// route answers a person.
func (r *specResultsRig) read(t *testing.T, commit, check string) []model.SpecCheckResult {
	t.Helper()
	path := "/api/v1/projects/" + specResultsProject + "/results?commit=" + commit
	if check != "" {
		path += "&check=" + check
	}
	response := r.asPerson(t, http.MethodGet, path, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Results []model.SpecCheckResult `json:"results"`
	}](t, response).Results
}

// stored is how many rows spec_check_results holds.
func (r *specResultsRig) stored(t *testing.T) int {
	t.Helper()
	var count int
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) from spec_check_results`).Scan(&count); err != nil {
		t.Fatalf("count spec_check_results: %v", err)
	}
	return count
}

// refusedWith fails the test unless response is status with code and nothing was stored.
func (r *specResultsRig) refusedWith(t *testing.T, response *httptest.ResponseRecorder, status int, code string) refusalAnswer {
	t.Helper()
	refusal := decodeBody[refusalAnswer](t, response)
	if response.Code != status || refusal.Code != code {
		t.Fatalf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), status, code)
	}
	if held := r.stored(t); held != 0 {
		t.Fatalf("the refused write left %d results, want none", held)
	}
	return refusal
}

// stored201 decodes a 201 answer, failing the test on any other.
func stored201(t *testing.T, response *httptest.ResponseRecorder) model.SpecCheckResult {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	return decodeBody[model.SpecCheckResult](t, response)
}

// TestAJobsResultIsStoredAsAnAttestationOfItsRun: the job's result is stored as an in-toto
// statement whose subject is the token's commit, whose job comes from the check run GitHub names
// for the token, whose procedure digest comes from the files at that commit, and whose recorder
// is the run the token proved: its session and principal, and no person. The body names none of
// them. The read answers the same row.
func TestAJobsResultIsStoredAsAnAttestationOfItsRun(t *testing.T) {
	rig := newSpecResultsRig(t)
	result := stored201(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)))

	statement := result.Attestation
	if statement.Type != "https://in-toto.io/Statement/v1" ||
		statement.PredicateType != "https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1" {
		t.Fatalf("the statement is %q of %q, want an in-toto Statement v1 of a spec check result", statement.Type, statement.PredicateType)
	}
	if len(statement.Subject) != 1 || statement.Subject[0].Name != "git+https://github.com/"+specResultsRepository ||
		statement.Subject[0].Digest["gitCommit"] != specResultsCommit {
		t.Fatalf("the subject is %+v, want the repository at the token's commit", statement.Subject)
	}
	predicate := statement.Predicate
	want := model.SpecCheckPredicate{
		Project: specResultsProject, Check: probeCheck, ProcedureDigest: specCheckDigest(t, probeCheck),
		Test:    model.SpecCheckTest{Runner: "go", Selector: "example.com/m/p TestP", Job: "go-checks"},
		Verdict: "pass", Observed: "ok  \texample.com/m/p\t0.01s", Job: "go-checks", Leg: "",
		Run: model.SpecCheckRun{
			Repository: specResultsRepository, WorkflowRef: specResultsRepository + "/.github/workflows/ci-results.yaml@refs/heads/topic",
			Ref: "refs/heads/topic", EventName: "push", RunID: "7", Attempt: 1, CheckRunID: "97", RunnerEnvironment: "github-hosted",
		},
		Recorder: model.SpecCheckRecorder{
			Session:   "ci-results:" + specResultsRepository + "/.github/workflows/ci-results.yaml",
			Principal: "workflow-results:" + specResultsRepository + "/.github/workflows/ci-results.yaml",
		},
		RecordedAt: predicate.RecordedAt,
	}
	if predicate != want || predicate.RecordedAt.IsZero() {
		t.Fatalf("the predicate is\n%+v\nwant\n%+v", predicate, want)
	}
	if result.Kind != "run" || result.Seq <= 0 || result.ID == "" {
		t.Fatalf("the row is kind %q, seq %d, id %q, want a run with Dispatch's record number and an id", result.Kind, result.Seq, result.ID)
	}
	read := rig.read(t, specResultsCommit, probeCheck)
	if len(read) != 1 || read[0].ID != result.ID || read[0].Seq != result.Seq || read[0].Attestation.Predicate != predicate {
		t.Fatalf("the read answered %+v, want the one row the write answered", read)
	}
}

// TestAResultNamesItsRunAndNoPerson: the airlock authenticated a run of a workflow. The person
// of the result entry is the broker's configuration: nobody authenticated that person for this
// write, and that person ran no test (R12, "Nobody writes a result for work that it did not
// do"). So the stored row, the write's answer, which `dispatch result --project` prints into a
// CI log, the read's answer and the write's access-log line each name the run and no person,
// and the line names the run's session as the caller.
func TestAResultNamesItsRunAndNoPerson(t *testing.T) {
	rig := newSpecResultsRig(t)
	person := resultsOnlyToken(rig.caller.jkt, specResultsProject).Person
	logged := captureLog(t)
	written := rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck))
	result := stored201(t, written)
	lines := accessLines(t, logged)
	read := rig.asPerson(t, http.MethodGet, "/api/v1/projects/"+specResultsProject+"/results?commit="+specResultsCommit, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("the read: status=%d body=%s", read.Code, read.Body.String())
	}
	if len(lines) != 1 {
		t.Fatalf("the write logged %d access-log lines, want 1: %q", len(lines), lines)
	}
	for _, text := range []struct{ what, text string }{
		{"the write's answer", written.Body.String()},
		{"the stored row", storedRow(t, rig, result.ID)},
		{"the read's answer", read.Body.String()},
		{"the write's access-log line", lines[0]},
	} {
		if strings.Contains(text.text, person) {
			t.Errorf("%s names the result entry's person %s: %s", text.what, person, text.text)
		}
	}
	session := "caller=ci-results:" + specResultsRepository + "/.github/workflows/ci-results.yaml"
	if !strings.Contains(lines[0], session) {
		t.Errorf("the write's access-log line %q does not carry %q", lines[0], session)
	}
}

// TestTheRecorderIsThePrincipalTheBrokerAuthenticated: the identity on a result is the one the
// airlock authenticated (R12). The broker authenticates a result entry's run as
// workflow-results:<repository>/<path>, the path being the run's workflow_ref without its
// repository and without "@" and the run's own ref (oauthwire.WorkflowPath), and Dispatch stores
// the recorder by that same rule. A workflow file's name and a branch's name can each hold an
// "@", so a cut at the first or the last "@" stores a principal the broker did not authenticate,
// or one that two workflows share. The plain row is the control. The stand-in GitHub serves the
// run's workflow file at the path each row's workflow_ref names, the file in which go-checks, the
// job of the check, is a plain job.
func TestTheRecorderIsThePrincipalTheBrokerAuthenticated(t *testing.T) {
	for _, row := range []struct{ name, path, ref string }{
		{"a plain path on a plain branch", ".github/workflows/ci-results.yaml", "refs/heads/topic"},
		{"a workflow file whose name holds an @", ".github/workflows/ci@probe.yaml", "refs/heads/topic"},
		{"a branch whose name holds an @", ".github/workflows/ci-results.yaml", "refs/heads/legion/a@b"},
		{"both at one time", ".github/workflows/ci@probe.yaml", "refs/heads/legion/a@b"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			rig.github.workflows[specResultsCommit][row.path] = specResultsWorkflow
			workflow := specResultsRepository + "/" + row.path
			token := rig.token(func(answer *ciphertoken.Introspection) {
				answer.Session = "ci-results:" + workflow
				answer.CI.WorkflowRef, answer.CI.Ref = workflow+"@"+row.ref, row.ref
			})
			result := stored201(t, rig.post(t, specResultsProject, token, passOf(probeCheck)))
			want := model.SpecCheckRecorder{Session: "ci-results:" + workflow, Principal: "workflow-results:" + workflow}
			if got := result.Attestation.Predicate.Recorder; got != want {
				t.Fatalf("the recorder is %+v, want %+v", got, want)
			}
			if read := rig.read(t, specResultsCommit, probeCheck); len(read) != 1 || read[0].Attestation.Predicate.Recorder != want {
				t.Fatalf("the read answered %+v, want the one row with the recorder %+v", read, want)
			}
		})
	}
}

// TestAnotherCommitIsRefusedNamingBoth: a job writes a result only for the commit of its own
// run, and the refusal names both commits.
func TestAnotherCommitIsRefusedNamingBoth(t *testing.T) {
	rig := newSpecResultsRig(t)
	body := passOf(probeCheck)
	body["commit"] = specResultsOtherCommit
	refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), body), http.StatusForbidden, "COMMIT_NOT_THE_RUNS")
	if !strings.Contains(refusal.Error, specResultsOtherCommit) || !strings.Contains(refusal.Error, specResultsCommit) {
		t.Fatalf("the refusal %q names not both commits", refusal.Error)
	}
}

// TestAnIdentityFieldInTheBodyIsRefused: the identity on a result is the one the token proved,
// never a field the caller set, so a body naming any field but the four is refused by name.
func TestAnIdentityFieldInTheBodyIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	for _, field := range []string{"actor", "job", "leg", "attempt", "role", "session", "repository", "run_id", "check_run_id", "procedure_digest", "subject"} {
		t.Run(field, func(t *testing.T) {
			body := passOf(probeCheck)
			body[field] = "chosen-by-the-caller"
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), body), http.StatusBadRequest, "RESULT_INPUT")
			if !strings.Contains(refusal.Error, field) {
				t.Fatalf("the refusal %q does not name %s", refusal.Error, field)
			}
		})
	}
}

// TestACheckTheFilesDoNotHoldIsRefused: a result is for a check of the files at the run's commit.
func TestACheckTheFilesDoNotHoldIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf("no-such-check")), http.StatusConflict, "CHECK_NOT_IN_FILES")
}

// TestSpecFilesTheGraphLogicRefusesAreRefused: Dispatch reads the spec files at the run's commit
// through specgraph.Judge, the one reading that spec-check and the CI reporter use, so a tree that
// reading refuses is refused here too, 409 SPEC_FILES_REFUSED naming its first problem, and nothing
// is stored: one tree has one answer, and a check "has five parts" (R6). A file over the size
// bound that every reader of spec files holds them to (notes.MaxFileSize) is the same refusal,
// since GitHub serving it again changes nothing.
func TestSpecFilesTheGraphLogicRefusesAreRefused(t *testing.T) {
	for _, row := range []struct {
		name, file, content, first string
	}{
		{
			name: "a statement no check serves, in another file", file: "broken.yaml",
			content: "---\ntype: requirement\nid: broken\nkind: behavior\ntext:\n  - No check serves this statement.\n",
			first:   ".dispatch/spec/broken.yaml:1: broken: leaf-without-check: no statement is under it and no check serves it",
		},
		{
			name: "a check without its failing example", file: "probe.yaml",
			content: strings.Replace(specResultsFiles["probe.yaml"], "failing_example:\n  - A probe test that fails.\n", "", 1),
			first:   ".dispatch/spec/probe.yaml:7: probe-passes: schema: node: missing property 'failing_example'",
		},
		{
			name: "a spec file that holds a NUL byte", file: "nul.yaml", content: "# A heading\n\x00\n",
			first: ".dispatch/spec/nul.yaml:2: -: bad-yaml: control characters are not allowed",
		},
		{
			name: "a spec file over the size bound", file: "big.yaml", content: strings.Repeat("\n", notes.MaxFileSize+1),
			first: "file too large: .dispatch/spec/big.yaml is 1048577 bytes (limit 1048576)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			files := maps.Clone(specResultsFiles)
			files[row.file] = row.content
			rig.github.files[specResultsCommit] = files
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusConflict, "SPEC_FILES_REFUSED")
			if !strings.Contains(refusal.Error, row.first) {
				t.Fatalf("the refusal %q does not name the first problem %q", refusal.Error, row.first)
			}
		})
	}
}

// TestEachFileOverTheBoundIsAProblem: spec-check refuses a tree with every file over the size
// bound as a problem of its own, one line each, before it reads any file (notes.ReadSet). The
// route counts the problems the same way, so two files over the bound are two problems, and the
// first is the first file in name order. The one-file row of
// TestSpecFilesTheGraphLogicRefusesAreRefused is the control.
func TestEachFileOverTheBoundIsAProblem(t *testing.T) {
	rig := newSpecResultsRig(t)
	files := maps.Clone(specResultsFiles)
	files["big-b.yaml"] = strings.Repeat("\n", notes.MaxFileSize+2)
	files["big-a.yaml"] = strings.Repeat("\n", notes.MaxFileSize+1)
	rig.github.files[specResultsCommit] = files
	refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusConflict, "SPEC_FILES_REFUSED")
	const want = "have 2 problems, so they hold no check to write a result for. The first: file too large: .dispatch/spec/big-a.yaml is 1048577 bytes (limit 1048576)"
	if !strings.Contains(refusal.Error, want) {
		t.Fatalf("the refusal %q does not say %q", refusal.Error, want)
	}
}

// TestASpecDirectoryOverASetBoundIsRefusedBeforeAnyFileIsRead: the route holds the spec files of a
// commit to the two bounds of the set that spec-check holds them to (notes.CheckSet), from GitHub's
// listing: a listing of one file more than notes.MaxFiles, or whose sizes sum to one byte more
// than notes.MaxSetSize while each file is within notes.MaxFileSize, is 409 SPEC_FILES_REFUSED
// with that one finding, and GitHub is asked for no file. The files of a commit at each bound
// write their result.
func TestASpecDirectoryOverASetBoundIsRefusedBeforeAnyFileIsRead(t *testing.T) {
	// The probe spec and padding files up to count files in all, each a file the graph logic takes.
	padded := func(t *testing.T, count int) map[string]string {
		files := maps.Clone(specResultsFiles)
		for i := 0; len(files) < count; i++ {
			files[fmt.Sprintf("n%04d.yaml", i)] = specPaddingFile(t, i, 100)
		}
		return files
	}
	// The probe spec and padding files, each within notes.MaxFileSize, summing to total.
	summing := func(t *testing.T, total int) map[string]string {
		files := maps.Clone(specResultsFiles)
		left := total - len(files["probe.yaml"])
		for i := 0; left > 0; i++ {
			size := min(left, notes.MaxFileSize)
			files[fmt.Sprintf("padding-%02d.yaml", i)] = specPaddingFile(t, i, size)
			left -= size
		}
		return files
	}
	const refusedAs = "have 1 problem, so they hold no check to write a result for. The first: set too large: .dispatch/spec holds "
	for _, row := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "one file over MaxFiles", files: padded(t, notes.MaxFiles+1), want: refusedAs + "1001 files (limit 1000)"},
		{name: "one byte over MaxSetSize", files: summing(t, notes.MaxSetSize+1), want: refusedAs + "16777217 bytes in its files (limit 16777216)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			rig.github.files[specResultsCommit] = row.files
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusConflict, "SPEC_FILES_REFUSED")
			if !strings.Contains(refusal.Error, row.want) {
				t.Errorf("the refusal %q does not say %q", refusal.Error, row.want)
			}
			rig.github.mu.Lock()
			defer rig.github.mu.Unlock()
			if rig.github.blobReads != 0 {
				t.Errorf("GitHub was asked for %d files, want none: the listing refuses the set", rig.github.blobReads)
			}
		})
	}
	for _, row := range []struct {
		name  string
		files map[string]string
	}{
		{name: "MaxFiles files", files: padded(t, notes.MaxFiles)},
		{name: "files summing MaxSetSize", files: summing(t, notes.MaxSetSize)},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			rig.github.files[specResultsCommit] = row.files
			stored201(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)))
		})
	}
}

// TestASpecDirectoryGitHubCannotListWholeIsRefused: a listing GitHub truncates, and a listing
// over the 1 MiB Dispatch reads of one listing, here from entries that are no spec files, answer
// the same at every read of that commit, so a write again can never be judged: each is 409
// SPEC_FILES_REFUSED saying why, and no file is read, not a 503 that asks for the write again.
func TestASpecDirectoryGitHubCannotListWholeIsRefused(t *testing.T) {
	const refusedAs = "have 1 problem, so they hold no check to write a result for. The first: set too large: "
	for _, row := range []struct {
		name   string
		setUp  func(*specGitHub)
		reason string
	}{
		{name: "a truncated listing", setUp: func(g *specGitHub) { g.truncated = true }, reason: "GitHub truncated its listing of .dispatch/spec, as it does past 100,000 entries or 7 MB; a set holds at most 1000 files"},
		{name: "a listing over 1 MiB", setUp: func(g *specGitHub) { g.padding = 4000 }, reason: "GitHub's listing of .dispatch/spec is over 1048576 bytes, the most Dispatch reads of one listing; a set holds at most 1000 files, whose listing fits"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			row.setUp(rig.github)
			for range 2 {
				refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusConflict, "SPEC_FILES_REFUSED")
				if !strings.Contains(refusal.Error, refusedAs+row.reason) {
					t.Errorf("the refusal %q does not say %q", refusal.Error, refusedAs+row.reason)
				}
			}
			rig.github.mu.Lock()
			defer rig.github.mu.Unlock()
			if rig.github.blobReads != 0 {
				t.Errorf("GitHub was asked for %d files, want none", rig.github.blobReads)
			}
		})
	}
}

// TestTheSetBoundFitsTheChecksCache: notes.MaxSetSize is the bytes of the checks cache, which
// keeps no entry larger than itself, so the judged files of a set within the bound are kept and
// read once for each commit (notes.MaxSetSize's doc).
func TestTheSetBoundFitsTheChecksCache(t *testing.T) {
	if notes.MaxSetSize != specChecksCacheBytes {
		t.Fatalf("notes.MaxSetSize is %d and the checks cache holds %d bytes: change the two together", notes.MaxSetSize, specChecksCacheBytes)
	}
}

// TestACheckOfAnotherJobIsRefused: the check names the job envoy-go, and the run's check run is
// the job go-checks, which did not run that check.
func TestACheckOfAnotherJobIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(otherJobsCheck)), http.StatusConflict, "CHECK_NOT_THIS_JOBS")
}

// checkRunNamed has the stand-in GitHub hold check run id at the default commit, named name, and
// answers a results token of that check run.
func (r *specResultsRig) checkRunNamed(id int64, name string) string {
	r.github.checkRuns[id] = specCheckRunAnswer{name: name, headSHA: specResultsCommit}
	return r.token(func(answer *ciphertoken.Introspection) { answer.CI.CheckRunID = fmt.Sprint(id) })
}

// workflowsAsked is each workflow file the stand-in GitHub was asked for, as "<commit> <path>".
func (r *specResultsRig) workflowsAsked() []string {
	r.github.mu.Lock()
	defer r.github.mu.Unlock()
	return slices.Clone(r.github.workflowsAsked)
}

// TestAJobWhoseNameEndsInParenthesesWritesItsOwnCheck: GitHub names a job's check run by the job's
// name, and a name may end in parentheses with no matrix behind them: the job foo is named
// `foo (bar)` and has no matrix. Its check run writes the check that names the job `foo (bar)`,
// with no leg. GitHub is asked for the run's workflow file once, which says that one job is named
// `foo (bar)` and that no matrix job makes that name for a leg.
func TestAJobWhoseNameEndsInParenthesesWritesItsOwnCheck(t *testing.T) {
	rig := newSpecResultsRig(t)
	result := stored201(t, rig.post(t, specResultsProject, rig.checkRunNamed(31, "foo (bar)"), passOf(parenthesizedCheck)))
	if p := result.Attestation.Predicate; p.Job != "foo (bar)" || p.Leg != "" {
		t.Fatalf("the result is of job %q, leg %q; want the job %q and no leg", p.Job, p.Leg, "foo (bar)")
	}
	if asked, want := rig.workflowsAsked(), []string{specResultsCommit + " " + specResultsWorkflowPath}; !slices.Equal(asked, want) {
		t.Fatalf("GitHub was asked for the workflow files %q, want %q, once", asked, want)
	}
}

// TestAnExactNameThatALegOfAMatrixJobMakesSaysNoJob: a matrix job test whose matrix holds the
// value unit makes a check run named `test (unit)` for that leg, and a plain job named
// `test (unit)` makes a check run of that same name. So the name does not say which of the two
// jobs ran, and a check run of that name is refused the check that names the job `test (unit)`,
// 409 CHECK_NOT_THIS_JOBS, saying which matrix job makes the name. The control is the same file
// with the matrix job renamed tests, whose legs make no check run of that name: the plain job's
// check run writes the check, with no leg.
func TestAnExactNameThatALegOfAMatrixJobMakesSaysNoJob(t *testing.T) {
	const check = "unit-probe-passes"
	files := maps.Clone(specResultsFiles)
	files["probe.yaml"] += "---\ntype: check\nid: " + check + "\nserves: the-probe-holds\nsubject: code\nprocedure: program\n" +
		"test:\n  runner: go\n  selector: example.com/m/p TestU\n  job: test (unit)\n" +
		"claim: The unit probe test passes.\nprocedure_text:\n  - The program runs the unit probe test one time.\n" +
		"failing_example:\n  - A unit probe test that fails.\n"
	workflow := func(matrixJob string) string {
		return "jobs:\n" +
			"  " + matrixJob + ":\n    strategy:\n      matrix:\n        suite: [unit]\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n" +
			"  other:\n    name: test (unit)\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n"
	}
	rigWith := func(t *testing.T, matrixJob string) *specResultsRig {
		rig := newSpecResultsRig(t)
		rig.github.files[specResultsCommit] = files
		rig.github.workflows[specResultsCommit][specResultsWorkflowPath] = workflow(matrixJob)
		return rig
	}
	t.Run("a matrix job test beside the plain job test (unit)", func(t *testing.T) {
		rig := rigWith(t, "test")
		refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.checkRunNamed(71, "test (unit)"), passOf(check)), http.StatusConflict, "CHECK_NOT_THIS_JOBS")
		if says := `the matrix job "test" in ` + specResultsWorkflowPath + " at " + specResultsCommit + ` makes a check run of that name for its leg "unit"`; !strings.Contains(refusal.Error, says) {
			t.Fatalf("the refusal %q does not say %q", refusal.Error, says)
		}
	})
	t.Run("the control: the matrix job named tests", func(t *testing.T) {
		rig := rigWith(t, "tests")
		result := stored201(t, rig.post(t, specResultsProject, rig.checkRunNamed(71, "test (unit)"), passOf(check)))
		if p := result.Attestation.Predicate; p.Job != "test (unit)" || p.Leg != "" {
			t.Fatalf("the result is of job %q, leg %q; want the job %q and no leg", p.Job, p.Leg, "test (unit)")
		}
	})
}

// TestALegOfAMatrixJobWritesItsJobsCheck: the job build has a matrix in the workflow file of the
// run at the run's commit, so its check run `build (<values>)` is the leg <values> of build and
// writes the check that names the job build. The leg is all that the parentheses after
// `build (` hold, a matrix value that holds parentheses of its own included. GitHub is asked for
// the workflow file once for the commit, at the path the run's workflow_ref names, and a matrix
// written as an expression is a matrix too.
func TestALegOfAMatrixJobWritesItsJobsCheck(t *testing.T) {
	t.Run("a matrix of values", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		for i, leg := range []string{"linux", "linux (arm)"} {
			result := stored201(t, rig.post(t, specResultsProject, rig.checkRunNamed(int64(41+i), "build ("+leg+")"), passOf(buildCheck)))
			if p := result.Attestation.Predicate; p.Job != "build" || p.Leg != leg {
				t.Fatalf("the result is of job %q, leg %q; want the job build and the leg %q", p.Job, p.Leg, leg)
			}
		}
		if asked, want := rig.workflowsAsked(), []string{specResultsCommit + " " + specResultsWorkflowPath}; !slices.Equal(asked, want) {
			t.Fatalf("GitHub was asked for the workflow files %q, want %q, once", asked, want)
		}
	})
	t.Run("a matrix written as an expression", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		rig.github.workflows[specResultsCommit][specResultsWorkflowPath] = "jobs:\n  build:\n    strategy:\n      matrix: ${{ fromJSON(needs.setup.outputs.legs) }}\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n"
		result := stored201(t, rig.post(t, specResultsProject, rig.checkRunNamed(41, "build (linux)"), passOf(buildCheck)))
		if p := result.Attestation.Predicate; p.Job != "build" || p.Leg != "linux" {
			t.Fatalf("the result is of job %q, leg %q; want the job build and the leg linux", p.Job, p.Leg)
		}
	})
}

// TestANameThatOnlyStartsWithAJobsNameIsNoLegOfIt: GitHub does not keep a name that ends in
// parentheses for the legs of a matrix. A check run named `build (linux)` is a leg of the job
// build only where the workflow file of the run at its commit makes build a matrix job, so the
// check of the job build is refused 409 CHECK_NOT_THIS_JOBS, saying why, where build has no
// matrix or an empty one, where no job is named build, where the job's name is an expression,
// where a job named `build (linux)` with no matrix makes a check run of that very name, and where
// the file is over notes.MaxFileSize or does not read as a workflow file.
func TestANameThatOnlyStartsWithAJobsNameIsNoLegOfIt(t *testing.T) {
	const at = " in " + specResultsWorkflowPath + " at " + specResultsCommit + " "
	job := func(id, body string) string {
		return "  " + id + ":\n" + body + "    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n"
	}
	matrix := "    strategy:\n      matrix:\n        os: [linux]\n"
	for _, row := range []struct {
		name, workflow, says string
	}{
		{name: "the job build has no matrix", workflow: "jobs:\n" + job("build", ""), says: `no job named "build"` + at + "has a matrix"},
		{name: "the job build has an empty matrix", workflow: "jobs:\n" + job("build", "    strategy:\n      matrix: {}\n"), says: `no job named "build"` + at + "has a matrix"},
		{name: "no job is named build", workflow: "jobs:\n" + job("test", matrix), says: `no job named "build"` + at + "has a matrix"},
		{name: "the job's name is an expression", workflow: "jobs:\n" + job("build", "    name: ${{ 'build' }}\n"+matrix), says: "a job" + at + "is named by an expression"},
		{
			name:     "a job named build (linux) with no matrix beside the matrix job build",
			workflow: "jobs:\n" + job("build", matrix) + job("other", "    name: build (linux)\n"),
			says:     `the job named "build (linux)"` + at + "has no matrix",
		},
		{
			name:     "a workflow file over the size bound",
			workflow: "jobs:\n" + job("build", matrix) + strings.Repeat("#", notes.MaxFileSize+1-len("jobs:\n"+job("build", matrix))),
			says:     "file too large: " + specResultsWorkflowPath + " is 1048577 bytes (limit 1048576)",
		},
		{name: "a file that does not read as a workflow file", workflow: "jobs: [\n", says: "does not read as a workflow file"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			rig.github.workflows[specResultsCommit][specResultsWorkflowPath] = row.workflow
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.checkRunNamed(51, "build (linux)"), passOf(buildCheck)), http.StatusConflict, "CHECK_NOT_THIS_JOBS")
			if !strings.Contains(refusal.Error, row.says) {
				t.Fatalf("the refusal %q does not say %q", refusal.Error, row.says)
			}
		})
	}
}

// TestALegNameThatAnotherMatrixJobCanMakeSaysNoLeg: legOf reads the check run `build (linux) (x64)`
// as the leg `linux) (x64` of the matrix job build, and the matrix job named `build (linux)` can
// make that same name for its leg x64; a second matrix job named build can make `build (linux)`
// for its own leg linux. Dispatch reads no matrix values, so in each case the name does not say
// which job ran, and the check of the job build is refused 409 CHECK_NOT_THIS_JOBS, saying which
// other matrix job can make the name. The control is the same name with build the one matrix job
// that can make it: the leg writes build's check.
func TestALegNameThatAnotherMatrixJobCanMakeSaysNoLeg(t *testing.T) {
	const at = " in " + specResultsWorkflowPath + " at " + specResultsCommit
	job := func(id, body string) string {
		return "  " + id + ":\n" + body + "    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n"
	}
	build := job("build", "    strategy:\n      matrix:\n        os: [linux]\n")
	for _, row := range []struct {
		name, workflow, checkRun, says string
	}{
		{
			name:     "the matrix job build (linux) beside the matrix job build",
			workflow: "jobs:\n" + build + job("other", "    name: build (linux)\n    strategy:\n      matrix:\n        arch: [x64]\n"),
			checkRun: "build (linux) (x64)",
			says:     `the matrix job "build (linux)"` + at + ` can make a check run of that name for its leg "x64"`,
		},
		{
			name:     "a second matrix job named build",
			workflow: "jobs:\n" + build + job("other", "    name: build\n    strategy:\n      matrix:\n        arch: [x64]\n"),
			checkRun: "build (linux)",
			says:     `the matrix job "build"` + at + ` can make a check run of that name for its leg "linux"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			rig.github.workflows[specResultsCommit][specResultsWorkflowPath] = row.workflow
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.checkRunNamed(61, row.checkRun), passOf(buildCheck)), http.StatusConflict, "CHECK_NOT_THIS_JOBS")
			if !strings.Contains(refusal.Error, row.says) {
				t.Fatalf("the refusal %q does not say %q", refusal.Error, row.says)
			}
		})
	}
	t.Run("the control: build is the one matrix job that can make the name", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		rig.github.workflows[specResultsCommit][specResultsWorkflowPath] = "jobs:\n" + build + job("other", "    name: test\n    strategy:\n      matrix:\n        arch: [x64]\n")
		result := stored201(t, rig.post(t, specResultsProject, rig.checkRunNamed(61, "build (linux) (x64)"), passOf(buildCheck)))
		if p := result.Attestation.Predicate; p.Job != "build" || p.Leg != "linux) (x64" {
			t.Fatalf("the result is of job %q, leg %q; want the job build and the leg %q", p.Job, p.Leg, "linux) (x64")
		}
	})
}

// TestACheckRunOfAnotherCommitIsRefused: GitHub names another commit for the token's check run,
// so the check run is no job of this run.
func TestACheckRunOfAnotherCommitIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.github.checkRuns[specResultsCheckRun] = specCheckRunAnswer{name: "go-checks", headSHA: specResultsOtherCommit}
	rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusForbidden, "CHECK_RUN_NOT_THE_RUNS")
}

// TestAForgedAccessTokenIsRefusedAtTheRoute: a token the broker answers inactive, presented with
// a sound DPoP proof of the caller's own key, writes nothing.
func TestAForgedAccessTokenIsRefusedAtTheRoute(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.refusedWith(t, rig.post(t, specResultsProject, "a-token-the-broker-never-issued", passOf(probeCheck)), http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE")
}

// TestOnlyAResultsTokenWritesTheRoute: a session's own broker token of the project, and a
// person's cookie, are refused the route: only a CI job's results token writes a result there.
func TestOnlyAResultsTokenWritesTheRoute(t *testing.T) {
	rig := newSpecResultsRig(t)
	named := liveNamedToken(rig.caller.jkt)
	named.Projects = []string{specResultsProject}
	const session = "a-session-token-of-the-project"
	rig.broker.Says(session, named)
	rig.refusedWith(t, rig.post(t, specResultsProject, session, passOf(probeCheck)), http.StatusForbidden, "RESULTS_SCOPE_REQUIRED")
	rig.refusedWith(t, rig.asPerson(t, http.MethodPost, "/api/v1/projects/"+specResultsProject+"/results", passOf(probeCheck)), http.StatusForbidden, "RESULTS_SCOPE_REQUIRED")
}

// TestAProjectNotTheTokensIsRefused: a results token writes the results of its own project alone,
// a public project's included. The token names ACME, and a write to the public project OTHER is
// refused 403 PROJECT_SCOPE before the handler runs. So it is where OTHER's default repository is
// the run's repository, where the handler would take the run as OTHER's and store its result under
// OTHER.
func TestAProjectNotTheTokensIsRefused(t *testing.T) {
	t.Run("a public project", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		rig.refusedWith(t, rig.post(t, "OTHER", rig.token(nil), passOf(probeCheck)), http.StatusForbidden, codeProjectScope)
	})
	t.Run("a public project whose default repository is the run's", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		for _, set := range []struct{ project, repository string }{{specResultsProject, "example-org/another-repo"}, {"OTHER", specResultsRepository}} {
			if written := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+set.project+"/default-repository", map[string]any{"repo": set.repository}); written.Code != http.StatusOK {
				t.Fatalf("set %s's default repository to %s: status=%d body=%s", set.project, set.repository, written.Code, written.Body.String())
			}
		}
		rig.refusedWith(t, rig.post(t, "OTHER", rig.token(nil), passOf(probeCheck)), http.StatusForbidden, codeProjectScope)
	})
}

// TestARepositoryTheProjectDoesNotMapIsRefused: the token's run is of a repository that is not
// the project's default repository, so its files are no files of the project. The run is whole,
// its workflow_ref under that repository, as the broker mints one. A project with no default
// repository takes the result of no run, and the refusal says the run's repository is not the
// project's default. The run's repository being another project's default makes it no repository
// of the token's project: OTHER's default is the run's repository, ACME's is another, and the
// token names ACME. A run named by the name the project's default was renamed away from is a run
// of a repository made under that name since, as GitHub names a renamed repository's runs by the
// new name: with the pair old-repo → new-repo and the default new-repo, a run of old-repo, in any
// case, is refused (the rename row of TestARepositorySpelledWithCapitalsIsTheProjectsRepository,
// a default stored under the old name and a run of the new, is its control).
func TestARepositoryTheProjectDoesNotMapIsRefused(t *testing.T) {
	t.Run("another repository", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		token := rig.token(func(answer *ciphertoken.Introspection) {
			answer.CI.Repository = "example-org/another-repo"
			answer.CI.WorkflowRef = "example-org/another-repo/.github/workflows/ci-results.yaml@refs/heads/topic"
		})
		refusal := rig.refusedWith(t, rig.post(t, specResultsProject, token, passOf(probeCheck)), http.StatusForbidden, "REPOSITORY_NOT_THE_PROJECTS")
		if want := "example-org/another-repo, the repository of this token's run, is not the default repository of project " + specResultsProject; !strings.Contains(refusal.Error, want) {
			t.Fatalf("the refusal reads %q, want it to say %q", refusal.Error, want)
		}
	})
	t.Run("a project with no default repository", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		if cleared := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+specResultsProject+"/default-repository", map[string]any{"repo": nil}); cleared.Code != http.StatusOK {
			t.Fatalf("clear %s's default repository: status=%d body=%s", specResultsProject, cleared.Code, cleared.Body.String())
		}
		rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusForbidden, "REPOSITORY_NOT_THE_PROJECTS")
	})
	t.Run("the default repository of another project", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		for _, set := range []struct{ project, repository string }{{specResultsProject, "example-org/another-repo"}, {"OTHER", specResultsRepository}} {
			if written := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+set.project+"/default-repository", map[string]any{"repo": set.repository}); written.Code != http.StatusOK {
				t.Fatalf("set %s's default repository to %s: status=%d body=%s", set.project, set.repository, written.Code, written.Body.String())
			}
		}
		refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusForbidden, "REPOSITORY_NOT_THE_PROJECTS")
		if want := "is not the default repository of project " + specResultsProject; !strings.Contains(refusal.Error, want) {
			t.Fatalf("the refusal reads %q, want it to say %q", refusal.Error, want)
		}
	})
	t.Run("the name the project's default repository was renamed away from", func(t *testing.T) {
		for _, repository := range []string{"example-org/old-repo", "Example-Org/OLD-REPO"} {
			t.Run(repository, func(t *testing.T) {
				database, renames := renamedRepositoryStore(t, "RENM", "example-org/old-repo", "example-org/new-repo")
				rig := newSpecResultsRigOn(t, testServerOptions{store: database, renames: renames})
				if written := rig.asPerson(t, http.MethodPut, "/api/v1/projects/"+specResultsProject+"/default-repository", map[string]any{"repo": "example-org/new-repo"}); written.Code != http.StatusOK {
					t.Fatalf("set %s's default repository to example-org/new-repo: status=%d body=%s", specResultsProject, written.Code, written.Body.String())
				}
				token := rig.token(func(answer *ciphertoken.Introspection) {
					answer.Session = "ci-results:" + repository + "/.github/workflows/ci-results.yaml"
					answer.CI.Repository = repository
					answer.CI.WorkflowRef = repository + "/.github/workflows/ci-results.yaml@refs/heads/topic"
				})
				refusal := rig.refusedWith(t, rig.post(t, specResultsProject, token, passOf(probeCheck)), http.StatusForbidden, "REPOSITORY_NOT_THE_PROJECTS")
				if want := repository + ", the repository of this token's run, is not the default repository of project " + specResultsProject; !strings.Contains(refusal.Error, want) {
					t.Fatalf("the refusal reads %q, want it to say %q", refusal.Error, want)
				}
			})
		}
	})
}

// TestARepositorySpelledWithCapitalsIsTheProjectsRepository: GitHub spells a run's repository as
// its owner does (`Example-Org/Example-Repo`), and Dispatch stores a project's default repository
// lowercased (parseIssueRepo). The check reads both sides lowercased, as every comparison of a
// repository does (refs.CanonicalRepoSQL), so the run of the project's default repository writes
// its result whatever the case of its name. A default stored under the name its repository was
// renamed away from is read under the new name, the name GitHub names the run's repository by
// (refs.Renames, over the pairs the server was given). The pair is seeded by SQL and handed to the
// server, as no route writes one (renamedRepositoryStore). The default is written under the old
// name by SQL, as a build before this one writes it during a roll: PUT
// /api/v1/projects/{key}/default-repository stores the new name, and the next boot's rename pass
// rewrites the old one (store.RewriteRenamedRepositories). The row is stored under the repository
// as the run names it. The lowercase run of every other test is the control.
func TestARepositorySpelledWithCapitalsIsTheProjectsRepository(t *testing.T) {
	for _, row := range []struct {
		name, repository string
		// storedDefault, where set, is the project's default as the row holds it, written by SQL.
		storedDefault string
	}{
		{name: "the project's default repository", repository: "Example-Org/Example-Repo"},
		{name: "the name the project's default repository was renamed to", repository: "Example-Org/New-Repo", storedDefault: "example-org/old-repo"},
	} {
		t.Run(row.name, func(t *testing.T) {
			database, renames := renamedRepositoryStore(t, "RENM", "example-org/old-repo", "example-org/new-repo")
			rig := newSpecResultsRigOn(t, testServerOptions{store: database, renames: renames})
			if row.storedDefault != "" {
				if tag, err := rig.database.Pool.Exec(context.Background(),
					`update projects set default_repo = $2 where key = $1`, specResultsProject, row.storedDefault); err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("store %s's default repository as %s, as a build before this one would: %v (%d rows)", specResultsProject, row.storedDefault, err, tag.RowsAffected())
				}
			}
			token := rig.token(func(answer *ciphertoken.Introspection) {
				answer.Session = "ci-results:" + row.repository + "/.github/workflows/ci-results.yaml"
				answer.CI.Repository = row.repository
				answer.CI.WorkflowRef = row.repository + "/.github/workflows/ci-results.yaml@refs/heads/topic"
			})
			result := stored201(t, rig.post(t, specResultsProject, token, passOf(probeCheck)))
			if got := result.Attestation.Predicate.Run.Repository; got != row.repository {
				t.Fatalf("the result is stored under the repository %q, want %q as the run names it", got, row.repository)
			}
		})
	}
}

// TestGitHubUnreadableIsAServiceError: a check run, files or a workflow file GitHub does not
// answer leave the result unjudged, so the route answers 503 and stores nothing, and the job
// writes it again. GitHub's 403 or 404 for the check run or the workflow file is GitHub refusing
// Dispatch the read for good (githubapp.ErrRepositoryUnreadable): the App cannot see it, or the
// commit does not hold it, and a write again is answered the same. So it is a final refusal that
// does not ask for the write again, 403 CHECK_RUN_NOT_SHOWN for the check run, the token's own
// run, and 409 WORKFLOW_NOT_SHOWN for the workflow file, which says which job made the check run.
// A workflow file the stand-in does not hold is GitHub's 404 too.
func TestGitHubUnreadableIsAServiceError(t *testing.T) {
	for _, row := range []struct {
		name   string
		setUp  func(*specGitHub)
		status int
		code   string
	}{
		{name: "the check run, GitHub answering 502", setUp: func(g *specGitHub) { g.checkRunStatus = http.StatusBadGateway }, status: http.StatusServiceUnavailable, code: "CHECK_RUN_UNREADABLE"},
		{name: "the check run, GitHub answering 500", setUp: func(g *specGitHub) { g.checkRunStatus = http.StatusInternalServerError }, status: http.StatusServiceUnavailable, code: "CHECK_RUN_UNREADABLE"},
		{name: "the check run, GitHub answering 404", setUp: func(g *specGitHub) { g.checkRunStatus = http.StatusNotFound }, status: http.StatusForbidden, code: "CHECK_RUN_NOT_SHOWN"},
		{name: "the check run, GitHub answering 403", setUp: func(g *specGitHub) { g.checkRunStatus = http.StatusForbidden }, status: http.StatusForbidden, code: "CHECK_RUN_NOT_SHOWN"},
		{name: "the files, GitHub answering 502", setUp: func(g *specGitHub) { g.filesStatus = http.StatusBadGateway }, status: http.StatusServiceUnavailable, code: "FILES_UNREADABLE"},
		{name: "the workflow file, GitHub answering 500", setUp: func(g *specGitHub) { g.workflowStatus = http.StatusInternalServerError }, status: http.StatusServiceUnavailable, code: "WORKFLOW_UNREADABLE"},
		{name: "the workflow file, GitHub answering 404", setUp: func(g *specGitHub) { g.workflowStatus = http.StatusNotFound }, status: http.StatusConflict, code: "WORKFLOW_NOT_SHOWN"},
		{name: "the workflow file, GitHub answering 403", setUp: func(g *specGitHub) { g.workflowStatus = http.StatusForbidden }, status: http.StatusConflict, code: "WORKFLOW_NOT_SHOWN"},
		{name: "a workflow file the commit does not hold", setUp: func(g *specGitHub) { g.workflows = nil }, status: http.StatusConflict, code: "WORKFLOW_NOT_SHOWN"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			row.setUp(rig.github)
			refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.checkRunNamed(61, "build (linux)"), passOf(buildCheck)), row.status, row.code)
			if again := strings.Contains(refusal.Error, "write it again"); again != (row.status == http.StatusServiceUnavailable) {
				t.Fatalf("the refusal %q asks for the write again: %v, want %v", refusal.Error, again, row.status == http.StatusServiceUnavailable)
			}
		})
	}
}

// storedRow is one row of spec_check_results as Postgres holds it, as text.
func storedRow(t *testing.T, rig *specResultsRig, id string) string {
	t.Helper()
	var row string
	if err := rig.database.Pool.QueryRow(context.Background(), `select row_to_json(r)::text from spec_check_results r where id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return row
}

// TestAResultIsAppendedAndNeverEdited: a second result of one check at one commit is a second
// row with a higher record number, and the first row's bytes stay as they were. GitHub is read
// once for the check run, once for the files and once for the run's workflow file: each read is
// cached for the process.
func TestAResultIsAppendedAndNeverEdited(t *testing.T) {
	rig := newSpecResultsRig(t)
	first := stored201(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)))
	before := storedRow(t, rig, first.ID)
	failed := passOf(probeCheck)
	failed["verdict"] = "fail"
	second := stored201(t, rig.post(t, specResultsProject, rig.token(nil), failed))
	if second.ID == first.ID || second.Seq <= first.Seq {
		t.Fatalf("the second result is id %s seq %d beside the first's %s %d, want a new row after it", second.ID, second.Seq, first.ID, first.Seq)
	}
	if after := storedRow(t, rig, first.ID); after != before {
		t.Fatalf("the first row changed:\n%s\nto\n%s", before, after)
	}
	if held := rig.stored(t); held != 2 {
		t.Fatalf("spec_check_results holds %d rows, want 2", held)
	}
	rig.github.mu.Lock()
	defer rig.github.mu.Unlock()
	workflow := []string{specResultsCommit + " " + specResultsWorkflowPath}
	if rig.github.checkRunReads != 1 || rig.github.treeReads != 1 || !slices.Equal(rig.github.workflowsAsked, workflow) {
		t.Fatalf("GitHub was read %d times for the check run, %d for the files and for the workflow files %q, want once, once and %q once",
			rig.github.checkRunReads, rig.github.treeReads, rig.github.workflowsAsked, workflow)
	}
}

// TestTheReadIsInRecordOrderNewestFirst: the read answers every row of the commit by Dispatch's
// record number, newest first. Two more rows are written in one transaction, so they carry one
// recorded_at, which a read ordered by time alone would answer in either order.
func TestTheReadIsInRecordOrderNewestFirst(t *testing.T) {
	rig := newSpecResultsRig(t)
	var wrote []int64
	for range 3 {
		wrote = append(wrote, stored201(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck))).Seq)
	}
	ctx := context.Background()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	workflow := ".github/workflows/ci-results.yaml"
	run := oauthwire.Run{
		Repository: specResultsRepository, WorkflowRef: specResultsRepository + "/" + workflow + "@refs/heads/topic",
		Ref: "refs/heads/topic", SHA: specResultsCommit, EventName: "push", RunnerEnvironment: "github-hosted",
		Workflow: workflow, ID: 7, Attempt: 1, CheckRun: specResultsCheckRun,
	}
	for range 2 {
		row := specCheckRow{
			project: specResultsProject, check: probeCheck, procedureDigest: specCheckDigest(t, probeCheck),
			job: "go-checks", run: run, verdict: "pass", session: "ci-results:" + specResultsRepository + "/" + workflow,
		}
		stored, err := insertSpecCheckResult(ctx, tx, row)
		if err != nil {
			t.Fatalf("insert in one transaction: %v", err)
		}
		wrote = append(wrote, stored.Seq)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	read := rig.read(t, specResultsCommit, "")
	if len(read) != len(wrote) {
		t.Fatalf("the read answered %d rows, want %d", len(read), len(wrote))
	}
	for i, result := range read {
		if want := wrote[len(wrote)-1-i]; result.Seq != want {
			t.Fatalf("row %d of the read is seq %d, want %d: the rows newest first are %v", i, result.Seq, want, wrote)
		}
	}
}

// TestEveryAttemptAndLegIsKept: each leg of the matrix job m and each attempt of a leg is a row
// of its own, with the job and the leg from its check run's name, and none is marked or dropped.
// The workflow file that makes m a matrix job is read of GitHub once for the commit.
func TestEveryAttemptAndLegIsKept(t *testing.T) {
	rig := newSpecResultsRig(t)
	runs := []struct {
		id      int64
		name    string
		attempt string
		verdict string
	}{{21, "m (a)", "1", "fail"}, {22, "m (b)", "1", "pass"}, {23, "m (a)", "2", "pass"}}
	for _, run := range runs {
		rig.github.checkRuns[run.id] = specCheckRunAnswer{name: run.name, headSHA: specResultsCommit}
		token := rig.token(func(answer *ciphertoken.Introspection) {
			answer.CI.CheckRunID, answer.CI.RunAttempt = fmt.Sprint(run.id), run.attempt
		})
		body := passOf(matrixCheck)
		body["verdict"] = run.verdict
		stored201(t, rig.post(t, specResultsProject, token, body))
	}
	read := rig.read(t, specResultsCommit, matrixCheck)
	type kept struct {
		job, leg, verdict string
		attempt           int
	}
	var got []kept
	for _, result := range read {
		p := result.Attestation.Predicate
		got = append(got, kept{p.Job, p.Leg, p.Verdict, p.Run.Attempt})
	}
	want := []kept{{"m", "a", "pass", 2}, {"m", "b", "pass", 1}, {"m", "a", "fail", 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the read answered %v, want %v newest first", got, want)
	}
	if asked := rig.workflowsAsked(); len(asked) != 1 {
		t.Fatalf("GitHub was asked for the workflow files %q, want one read for the commit", asked)
	}
}

// TestTwoLegsOfOneNameInOneAttemptAreTwoRows: two check runs of one name in one attempt are two
// legs, and each is a row.
func TestTwoLegsOfOneNameInOneAttemptAreTwoRows(t *testing.T) {
	rig := newSpecResultsRig(t)
	for _, id := range []int64{11, 12} {
		rig.github.checkRuns[id] = specCheckRunAnswer{name: "go-checks", headSHA: specResultsCommit}
		stored201(t, rig.post(t, specResultsProject, rig.token(func(answer *ciphertoken.Introspection) { answer.CI.CheckRunID = fmt.Sprint(id) }), passOf(probeCheck)))
	}
	read := rig.read(t, specResultsCommit, probeCheck)
	if len(read) != 2 || read[0].Attestation.Predicate.Run.CheckRunID != "12" || read[1].Attestation.Predicate.Run.CheckRunID != "11" {
		t.Fatalf("the read answered %+v, want the rows of check runs 12 and 11", read)
	}
}

// TestAKeyOfOneRunNeverAnswersAnotherRun: an Idempotency-Key belongs to whoever sent it, and a
// results token was sent by its run, not by the person its result entry names. So two jobs of one
// entry that write the same body under one key are two results, and only a repeat by the same
// run is answered the first answer. The repeat is the control: the key layer is on.
func TestAKeyOfOneRunNeverAnswersAnotherRun(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.github.checkRuns[11] = specCheckRunAnswer{name: "go-checks", headSHA: specResultsCommit}
	first := rig.token(nil)
	other := rig.token(func(answer *ciphertoken.Introspection) { answer.CI.CheckRunID = "11" })
	keyed := func(token string) *httptest.ResponseRecorder {
		request := rig.caller.presenting(t, http.MethodPost, "/api/v1/projects/"+specResultsProject+"/results", token, passOf(probeCheck))
		request.Header.Set(contracts.IdempotencyKeyHeader, "one-key")
		return rig.serve(request)
	}
	written := keyed(first)
	if written.Code != http.StatusCreated || replayed(written) {
		t.Fatalf("the first run's write: status=%d replayed=%v body=%s, want 201 written", written.Code, replayed(written), written.Body.String())
	}
	if another := keyed(other); another.Code != http.StatusCreated || replayed(another) {
		t.Fatalf("the other run's write under the same key: status=%d replayed=%v body=%s, want 201 of its own", another.Code, replayed(another), another.Body.String())
	}
	if again := keyed(first); !replayed(again) || again.Body.String() != written.Body.String() {
		t.Fatalf("the first run's repeat: replayed=%v body=%s, want the first answer %s", replayed(again), again.Body.String(), written.Body.String())
	}
	if held := rig.stored(t); held != 2 {
		t.Fatalf("spec_check_results holds %d rows, want one for each run", held)
	}
}

// TestTwoRunsAtOneCommitAreTwoRows: two workflow runs of one commit each write a row.
func TestTwoRunsAtOneCommitAreTwoRows(t *testing.T) {
	rig := newSpecResultsRig(t)
	for _, run := range []string{"1", "2"} {
		stored201(t, rig.post(t, specResultsProject, rig.token(func(answer *ciphertoken.Introspection) { answer.CI.RunID = run }), passOf(probeCheck)))
	}
	read := rig.read(t, specResultsCommit, probeCheck)
	if len(read) != 2 || read[0].Attestation.Predicate.Run.RunID != "2" || read[1].Attestation.Predicate.Run.RunID != "1" {
		t.Fatalf("the read answered %+v, want the rows of runs 2 and 1", read)
	}
}

// TestAnObservedTextPastTheBoundIsCutAndSaysSo: a result keeps at most
// contracts.SpecCheckObservedMax runes of what the job observed. A longer text is kept as its
// first contracts.SpecCheckObservedStart runes, contracts.SpecCheckObservedCutNote and its last
// contracts.SpecCheckObservedEnd runes, that many runes in all, so the stored text says where it
// was cut; a text of exactly the bound is kept whole. A rune outside the basic plane, two UTF-16
// units and four bytes, is one rune of the count.
func TestAnObservedTextPastTheBoundIsCutAndSaysSo(t *testing.T) {
	const start, end, note = contracts.SpecCheckObservedStart, contracts.SpecCheckObservedEnd, contracts.SpecCheckObservedCutNote
	for _, row := range []struct {
		name, sent, kept string
	}{
		{"a text of the bound", strings.Repeat("é", contracts.SpecCheckObservedMax), strings.Repeat("é", contracts.SpecCheckObservedMax)},
		{"a text one past the bound", "s" + strings.Repeat("é", contracts.SpecCheckObservedMax-1) + "e",
			"s" + strings.Repeat("é", start-1) + note + strings.Repeat("é", end-1) + "e"},
		{"a text outside the basic plane", strings.Repeat("🙂", 2500), strings.Repeat("🙂", start) + note + strings.Repeat("🙂", end)},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			body := passOf(probeCheck)
			body["observed"] = row.sent
			observed := stored201(t, rig.post(t, specResultsProject, rig.token(nil), body)).Attestation.Predicate.Observed
			if observed != row.kept || utf8.RuneCountInString(observed) != contracts.SpecCheckObservedMax {
				t.Fatalf("observed holds %d runes, starting %q and ending %q; want %d, starting %q and ending %q",
					utf8.RuneCountInString(observed), observed[:min(len(observed), 60)], observed[max(0, len(observed)-160):],
					contracts.SpecCheckObservedMax, row.kept[:min(len(row.kept), 60)], row.kept[max(0, len(row.kept)-160):])
			}
		})
	}
}

// TestAFailingTestsLogKeepsItsStartAndItsEnd: a failing test's output says which test ran in its
// first line and why it failed at its end (the message and the --- FAIL line), so a text over the
// bound keeps both, with the note that says the middle was cut between them, and the stored text
// is still contracts.SpecCheckObservedMax runes.
func TestAFailingTestsLogKeepsItsStartAndItsEnd(t *testing.T) {
	const first, last = "=== RUN   TestX\n", "--- FAIL: TestX (0.01s)\n"
	sent := first + strings.Repeat("x\n", (10000-len(first)-len(last))/2) + last
	if utf8.RuneCountInString(sent) != 10000 {
		t.Fatalf("the sent text holds %d runes, want 10000", utf8.RuneCountInString(sent))
	}
	rig := newSpecResultsRig(t)
	body := passOf(probeCheck)
	body["verdict"], body["observed"] = "fail", sent
	observed := stored201(t, rig.post(t, specResultsProject, rig.token(nil), body)).Attestation.Predicate.Observed
	note := strings.Index(observed, contracts.SpecCheckObservedCutNote)
	if !strings.HasPrefix(observed, first) || !strings.HasSuffix(observed, last) || note < len(first) ||
		note+len(contracts.SpecCheckObservedCutNote) > len(observed)-len(last) || utf8.RuneCountInString(observed) != contracts.SpecCheckObservedMax {
		t.Fatalf("observed holds %d runes, starting %q, ending %q, the note at byte %d; want %d runes starting %q and ending %q with the note between them",
			utf8.RuneCountInString(observed), observed[:min(len(observed), 40)], observed[max(0, len(observed)-40):], note,
			contracts.SpecCheckObservedMax, first, last)
	}
}

// TestACouldNotRunResultIsStored: a job whose test could not run writes that, and it is stored as
// it was written.
func TestACouldNotRunResultIsStored(t *testing.T) {
	rig := newSpecResultsRig(t)
	body := passOf(probeCheck)
	body["verdict"] = "could_not_run"
	if verdict := stored201(t, rig.post(t, specResultsProject, rig.token(nil), body)).Attestation.Predicate.Verdict; verdict != "could_not_run" {
		t.Fatalf("the stored verdict is %q, want could_not_run", verdict)
	}
}

// TestTheReadIsRefusedToAResultsToken: a results token writes results and reads nothing, the
// results of its own project included.
func TestTheReadIsRefusedToAResultsToken(t *testing.T) {
	rig := newSpecResultsRig(t)
	response := rig.serve(rig.caller.presenting(t, http.MethodGet, "/api/v1/projects/"+specResultsProject+"/results?commit="+specResultsCommit, rig.token(nil), nil))
	if why := notTheGatesRefusal(response, codeResultsOnlyToken, ""); why != "" {
		t.Fatalf("the read %s, want 403 %s", why, codeResultsOnlyToken)
	}
}
