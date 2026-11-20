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

	"github.com/sjawhar/envoy/internal/brokertoken"
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
	// specResultsRepository is the repository of the job's run, which the project maps.
	specResultsRepository = "example-org/example-repo"
	// specResultsCommit is the commit of the job's run, whose files hold the probe checks.
	specResultsCommit = "0123456789abcdef0123456789abcdef01234567"
	// specResultsOtherCommit is a commit the token does not name.
	specResultsOtherCommit = "89abcdef0123456789abcdef0123456789abcdef"
	// specResultsCheckRun is the check run of the job the default token was minted in.
	specResultsCheckRun = 97
)

// The checks of specResultsFiles: one of job go-checks, one of job envoy-go, and one of the
// matrix job m.
const (
	probeCheck     = "probe-passes"
	otherJobsCheck = "second-probe-passes"
	matrixCheck    = "matrix-probe-passes"
)

// specResultsFiles is .dispatch/spec at specResultsCommit: one statement and its three program
// checks, probeCheck, otherJobsCheck and matrixCheck. It is a tree the graph logic takes
// (specgraph.Judge finds no problem in it), as the files of a commit a job reports on must be.
var specResultsFiles = map[string]string{
	"probe.md": `:::requirement{#the-probe-holds kind="behavior"}
The probe test passes on each commit.
:::

:::check{#probe-passes serves="the-probe-holds" subject="code" procedure="program" runner="go" selector="example.com/m/p TestP" job="go-checks"}
Claim: The probe test passes.
Procedure: The program runs the probe test one time.
Failing example: A probe test that fails.
:::

:::check{#second-probe-passes serves="the-probe-holds" subject="code" procedure="program" runner="go" selector="example.com/m/p TestQ" job="envoy-go"}
Claim: The second probe test passes.
Procedure: The program runs the second probe test one time.
Failing example: A second probe test that fails.
:::

:::check{#matrix-probe-passes serves="the-probe-holds" subject="code" procedure="program" runner="go" selector="example.com/m/p TestM" job="m"}
Claim: The matrix probe test passes on each leg.
Procedure: The program runs the matrix probe test on each leg.
Failing example: A matrix probe test that fails on one leg.
:::
`,
}

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
// id, and the tree and blobs of .dispatch/spec at a commit. A status set in checkRunStatus or
// filesStatus answers every read of that kind with it. truncated marks each tree listing
// truncated, as GitHub marks one past its own bounds, and padding lists that many entries beside
// the files that are no spec files (nested directories). It counts each read, so a test can see a
// cached read made once, and a file read that never happened.
type specGitHub struct {
	mu             sync.Mutex
	checkRuns      map[int64]specCheckRunAnswer
	files          map[string]map[string]string
	checkRunStatus int
	filesStatus    int
	truncated      bool
	padding        int
	checkRunReads  int
	treeReads      int
	blobReads      int
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
	return githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
}

// specResultsRig is a Dispatch whose verifier serves results tokens as cmd/dispatch's does, a
// GitHub that holds the run's check run and files, and project ACME mapped to the repository.
type specResultsRig struct {
	*brokerTokenRig
	github *specGitHub
	caller *brokerCaller
	tokens int
}

func newSpecResultsRig(t *testing.T) *specResultsRig {
	t.Helper()
	github := &specGitHub{
		checkRuns: map[int64]specCheckRunAnswer{specResultsCheckRun: {name: "go-checks", headSHA: specResultsCommit}},
		files:     map[string]map[string]string{specResultsCommit: specResultsFiles},
	}
	_, pemText := githubapptest.Key(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: github.serve(t),
	})
	for _, project := range []string{specResultsProject, "OTHER"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	if mapped := rig.asPerson(t, http.MethodPut, "/api/v1/settings/repo-projects/"+specResultsRepository, map[string]any{"project": specResultsProject}); mapped.Code != http.StatusOK {
		t.Fatalf("map %s to %s: status=%d body=%s", specResultsRepository, specResultsProject, mapped.Code, mapped.Body.String())
	}
	return &specResultsRig{brokerTokenRig: rig, github: github, caller: newBrokerCaller(t)}
}

// token has the broker answer a results token of project ACME for the default run, with change
// applied to the answer, and answers the token.
func (r *specResultsRig) token(change func(*brokertoken.Introspection)) string {
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
// or one that two workflows share. The plain row is the control.
func TestTheRecorderIsThePrincipalTheBrokerAuthenticated(t *testing.T) {
	for _, row := range []struct{ name, path, ref string }{
		{"a plain path on a plain branch", ".github/workflows/ci-results.yaml", "refs/heads/topic"},
		{"a workflow file whose name holds an @", ".github/workflows/ci@probe.yaml", "refs/heads/topic"},
		{"a branch whose name holds an @", ".github/workflows/ci-results.yaml", "refs/heads/legion/a@b"},
		{"both at one time", ".github/workflows/ci@probe.yaml", "refs/heads/legion/a@b"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			workflow := specResultsRepository + "/" + row.path
			token := rig.token(func(answer *brokertoken.Introspection) {
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
			name: "an unclosed block in another file", file: "broken.md",
			content: ":::requirement{#broken kind=\"behavior\"}\nThis block never closes.\n",
			first:   ".dispatch/spec/broken.md:1: broken: leaf-without-check: no statement is under it and no check serves it",
		},
		{
			name: "a check without its failing example", file: "probe.md",
			content: strings.Replace(specResultsFiles["probe.md"], "Failing example: A probe test that fails.\n", "", 1),
			first:   ".dispatch/spec/probe.md:5: probe-passes: no-failing-example: the check has no failing example",
		},
		{
			name: "a spec file that holds a NUL byte", file: "nul.md", content: "# A heading\n\x00\n",
			first: ".dispatch/spec/nul.md:2: -: bad-encoding: a spec file is UTF-8 text with no NUL byte",
		},
		{
			name: "a spec file over the size bound", file: "big.md", content: strings.Repeat("\n", notes.MaxFileSize+1),
			first: "file too large: .dispatch/spec/big.md is 1048577 bytes (limit 1048576)",
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
	files["big-b.md"] = strings.Repeat("\n", notes.MaxFileSize+2)
	files["big-a.md"] = strings.Repeat("\n", notes.MaxFileSize+1)
	rig.github.files[specResultsCommit] = files
	refusal := rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusConflict, "SPEC_FILES_REFUSED")
	const want = "have 2 problems, so they hold no check to write a result for. The first: file too large: .dispatch/spec/big-a.md is 1048577 bytes (limit 1048576)"
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
	// The probe spec and blank files up to count files in all.
	padded := func(count int) map[string]string {
		files := maps.Clone(specResultsFiles)
		for i := 0; len(files) < count; i++ {
			files[fmt.Sprintf("n%04d.md", i)] = "\n"
		}
		return files
	}
	// The probe spec and files of blank lines, each within notes.MaxFileSize, summing to total.
	summing := func(total int) map[string]string {
		files := maps.Clone(specResultsFiles)
		left := total - len(files["probe.md"])
		for i := 0; left > 0; i++ {
			size := min(left, notes.MaxFileSize)
			files[fmt.Sprintf("blank-%02d.md", i)] = strings.Repeat("\n", size)
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
		{name: "one file over MaxFiles", files: padded(notes.MaxFiles + 1), want: refusedAs + "1001 files (limit 1000)"},
		{name: "one byte over MaxSetSize", files: summing(notes.MaxSetSize + 1), want: refusedAs + "16777217 bytes in its files (limit 16777216)"},
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
		{name: "MaxFiles files", files: padded(notes.MaxFiles)},
		{name: "files summing MaxSetSize", files: summing(notes.MaxSetSize)},
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

// TestAProjectNotTheTokensIsRefused: a results token writes the results of its own project alone.
func TestAProjectNotTheTokensIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	rig.refusedWith(t, rig.post(t, "OTHER", rig.token(nil), passOf(probeCheck)), http.StatusForbidden, codeProjectScope)
}

// TestARepositoryTheProjectDoesNotMapIsRefused: the token's run is of a repository the project
// does not map, so its files are no files of the project. The run is whole, its workflow_ref
// under that repository, as the broker mints one.
func TestARepositoryTheProjectDoesNotMapIsRefused(t *testing.T) {
	rig := newSpecResultsRig(t)
	token := rig.token(func(answer *brokertoken.Introspection) {
		answer.CI.Repository = "example-org/another-repo"
		answer.CI.WorkflowRef = "example-org/another-repo/.github/workflows/ci-results.yaml@refs/heads/topic"
	})
	rig.refusedWith(t, rig.post(t, specResultsProject, token, passOf(probeCheck)), http.StatusForbidden, "REPOSITORY_NOT_THE_PROJECTS")
}

// TestARepositorySpelledWithCapitalsIsTheProjectsRepository: GitHub spells a run's repository as
// its owner does (`Example-Org/Example-Repo`), and Dispatch stores a mapping's repository, and the
// name it was renamed to, lowercased (canonicalRepo). Every comparison of a repository reads both
// sides lowercased (refs.CanonicalRepoSQL), so the run of a mapped repository, and of a repository
// a mapping was renamed to, writes its result whatever the case of its name. The row is stored
// under the repository as the run names it. The lowercase run of every other test is the control.
func TestARepositorySpelledWithCapitalsIsTheProjectsRepository(t *testing.T) {
	for _, row := range []struct{ name, repository string }{
		{"a mapped repository", "Example-Org/Example-Repo"},
		{"a repository a mapping was renamed to", "Example-Org/New-Repo"},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newSpecResultsRig(t)
			renamed := map[string]any{"project": specResultsProject, "renamed_to": "Example-Org/New-Repo"}
			if mapped := rig.asPerson(t, http.MethodPut, "/api/v1/settings/repo-projects/example-org/old-repo", renamed); mapped.Code != http.StatusOK {
				t.Fatalf("map example-org/old-repo, renamed to Example-Org/New-Repo: status=%d body=%s", mapped.Code, mapped.Body.String())
			}
			token := rig.token(func(answer *brokertoken.Introspection) {
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

// TestGitHubUnreadableIsAServiceError: a check run or files GitHub does not answer leave the
// result unjudged, so the route answers 503 and stores nothing, and the job writes it again.
func TestGitHubUnreadableIsAServiceError(t *testing.T) {
	t.Run("the check run", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		rig.github.checkRunStatus = http.StatusBadGateway
		rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusServiceUnavailable, "CHECK_RUN_UNREADABLE")
	})
	t.Run("the files", func(t *testing.T) {
		rig := newSpecResultsRig(t)
		rig.github.filesStatus = http.StatusBadGateway
		rig.refusedWith(t, rig.post(t, specResultsProject, rig.token(nil), passOf(probeCheck)), http.StatusServiceUnavailable, "FILES_UNREADABLE")
	})
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
// once for the check run and once for the files: both reads are cached for the process.
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
	if rig.github.checkRunReads != 1 || rig.github.treeReads != 1 {
		t.Fatalf("GitHub was read %d times for the check run and %d for the files, want once each", rig.github.checkRunReads, rig.github.treeReads)
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
	for range 2 {
		row := specCheckRow{
			project: specResultsProject, repository: specResultsRepository, check: probeCheck, commit: specResultsCommit,
			procedureDigest: specCheckDigest(t, probeCheck), job: "go-checks", checkRunID: specResultsCheckRun, runID: 7, attempt: 1,
			verdict: "pass", session: "ci-results:" + specResultsRepository + "/.github/workflows/ci-results.yaml",
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
		token := rig.token(func(answer *brokertoken.Introspection) {
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
}

// TestTwoLegsOfOneNameInOneAttemptAreTwoRows: two check runs of one name in one attempt are two
// legs, and each is a row.
func TestTwoLegsOfOneNameInOneAttemptAreTwoRows(t *testing.T) {
	rig := newSpecResultsRig(t)
	for _, id := range []int64{11, 12} {
		rig.github.checkRuns[id] = specCheckRunAnswer{name: "go-checks", headSHA: specResultsCommit}
		stored201(t, rig.post(t, specResultsProject, rig.token(func(answer *brokertoken.Introspection) { answer.CI.CheckRunID = fmt.Sprint(id) }), passOf(probeCheck)))
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
	other := rig.token(func(answer *brokertoken.Introspection) { answer.CI.CheckRunID = "11" })
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
		stored201(t, rig.post(t, specResultsProject, rig.token(func(answer *brokertoken.Introspection) { answer.CI.RunID = run }), passOf(probeCheck)))
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
	if why := notTheLimitedRefusal(response, codeResultsOnlyToken); why != "" {
		t.Fatalf("the read %s, want 403 %s", why, codeResultsOnlyToken)
	}
}
