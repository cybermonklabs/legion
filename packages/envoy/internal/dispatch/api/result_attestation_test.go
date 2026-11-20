// result_attestation_test.go holds POST and GET /api/v1/issues/{key}/results to R12 of the spec
// for a CI job's result: it carries the in-toto statement of its run, built by the builder of the
// project's results route, and no other result carries one.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The statement as the wire carries it, read apart from the server's own types, so each test
// reads the names a client reads.
type wireStatement struct {
	Type          string        `json:"_type"`
	Subject       []wireSubject `json:"subject"`
	PredicateType string        `json:"predicateType"`
	Predicate     wirePredicate `json:"predicate"`
}

type wireSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type wirePredicate struct {
	Project         string       `json:"project"`
	Check           string       `json:"check"`
	ProcedureDigest string       `json:"procedure_digest"`
	Test            wireTest     `json:"test"`
	Verdict         string       `json:"verdict"`
	Observed        string       `json:"observed"`
	Job             string       `json:"job"`
	Leg             string       `json:"leg"`
	Run             wireRun      `json:"run"`
	Recorder        wireRecorder `json:"recorder"`
	RecordedAt      time.Time    `json:"recorded_at"`
}

type wireTest struct {
	Runner   string `json:"runner"`
	Selector string `json:"selector"`
	Job      string `json:"job"`
}

type wireRun struct {
	Repository        string `json:"repository"`
	WorkflowRef       string `json:"workflow_ref"`
	Ref               string `json:"ref"`
	EventName         string `json:"event_name"`
	RunID             string `json:"run_id"`
	Attempt           int    `json:"attempt"`
	CheckRunID        string `json:"check_run_id"`
	RunnerEnvironment string `json:"runner_environment"`
}

type wireRecorder struct {
	Session   string `json:"session"`
	Principal string `json:"principal"`
}

// A result as the wire carries it, with its statement.
type attestedWireResult struct {
	ID          string         `json:"id"`
	Branch      string         `json:"branch"`
	CheckSHA256 string         `json:"check_sha256"`
	Session     string         `json:"session"`
	CreatedAt   time.Time      `json:"created_at"`
	Attestation *wireStatement `json:"attestation"`
}

// checkWithRawResults is one check of the results read, each result as the raw object the wire
// carries, so a test can tell a member left out from one that is null. A check CI proves holds
// what was recorded against it in recorded_results, beside its derived result in results.
type checkWithRawResults struct {
	BlockID         string            `json:"block_id"`
	Ref             string            `json:"ref"`
	Results         []json.RawMessage `json:"results"`
	RecordedResults []json.RawMessage `json:"recorded_results"`
}

// recordedWithStatement decodes a 201 answer of the record route, failing the test on any other
// status, and answers its result and the raw result object.
func recordedWithStatement(t *testing.T, what string, response *httptest.ResponseRecorder) (attestedWireResult, json.RawMessage) {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s, want 201", what, response.Code, response.Body.String())
	}
	raw := decodeBody[struct {
		Result json.RawMessage `json:"result"`
	}](t, response).Result
	var result attestedWireResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("%s: decode the result %s: %v", what, raw, err)
	}
	return result, raw
}

// readRawChecks is the results read of issue, as alice reads it (dispatchRequest, as readJobCheck
// does).
func (r *ciJobRig) readRawChecks(t *testing.T, issue string) []checkWithRawResults {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Checks []checkWithRawResults `json:"checks"`
	}](t, response).Checks
}

// rawCheck is check blockID of checks, failing the test where the read holds none.
func rawCheck(t *testing.T, checks []checkWithRawResults, blockID string) checkWithRawResults {
	t.Helper()
	for _, check := range checks {
		if check.BlockID == blockID {
			return check
		}
	}
	t.Fatalf("the read holds no check %s", blockID)
	return checkWithRawResults{}
}

// rawResultOf is the result id of check, decoded, and its raw object, failing the test where the
// check holds none.
func rawResultOf(t *testing.T, check checkWithRawResults, id string) (attestedWireResult, json.RawMessage) {
	t.Helper()
	for _, raw := range append(append([]json.RawMessage{}, check.Results...), check.RecordedResults...) {
		var result attestedWireResult
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode a result of %s %s: %v", check.BlockID, raw, err)
		}
		if result.ID == id {
			return result, raw
		}
	}
	t.Fatalf("check %s holds no result %s", check.BlockID, id)
	return attestedWireResult{}, nil
}

// hasMember reports whether the JSON object raw holds the member name.
func hasMember(t *testing.T, raw json.RawMessage, name string) bool {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		t.Fatalf("decode %s as an object: %v", raw, err)
	}
	_, held := members[name]
	return held
}

// TestAJobsResultOnTheIssueRouteCarriesTheStatementOfItsRun: the job's result for c1 carries an
// in-toto Statement v1 of its run. Its subject is the run's repository at the commit it tested,
// its predicate type the one of a check block, and its predicate names the check by the read's
// reference of it, the digest of the check's words as the row's check_sha256, the job the check's
// ci_check names, what the job observed, the run the token proved and the recorder, which is the
// run's session and principal and no person. Its recorded_at is the result's created_at.
func TestAJobsResultOnTheIssueRouteCarriesTheStatementOfItsRun(t *testing.T) {
	rig := newCIJobRig(t)
	response := rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass"))
	result, raw := recordedWithStatement(t, "the job's result for c1", response)
	if result.Attestation == nil {
		t.Fatalf("the job's result for c1 carries no attestation: %s", response.Body.String())
	}
	if result.CheckSHA256 == "" {
		t.Fatalf("the job's result for c1 holds no check_sha256: %s", raw)
	}
	ref := rawCheck(t, rig.readRawChecks(t, rig.issue), "c1").Ref
	if ref == "" {
		t.Fatalf("the read names no ref of c1")
	}

	want := wireStatement{
		Type: "https://in-toto.io/Statement/v1",
		Subject: []wireSubject{{
			Name: "git+https://github.com/" + ciJobRepository, Digest: map[string]string{"gitCommit": ciJobCommit},
		}},
		PredicateType: "https://github.com/cybermonklabs/legion/attestations/check-block-result/v1",
		Predicate: wirePredicate{
			Project: ciJobProject, Check: ref, ProcedureDigest: result.CheckSHA256,
			Test:    wireTest{Runner: "", Selector: "", Job: ciJobName},
			Verdict: "pass", Observed: "ok", Job: ciJobName, Leg: "",
			Run: wireRun{
				Repository: ciJobRepository, WorkflowRef: ciJobRepository + "/.github/workflows/ci-results.yaml@" + ciJobRef,
				Ref: ciJobRef, EventName: "push", RunID: "7", Attempt: 1, CheckRunID: "97", RunnerEnvironment: "github-hosted",
			},
			Recorder: wireRecorder{
				Session: ciJobSession, Principal: "workflow-results:" + ciJobRepository + "/.github/workflows/ci-results.yaml",
			},
		},
	}
	got := *result.Attestation
	recordedAt := got.Predicate.RecordedAt
	got.Predicate.RecordedAt = time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the job's result for c1 carries the statement\n%+v\nwant\n%+v", got, want)
	}
	if recordedAt.IsZero() || !recordedAt.Equal(result.CreatedAt) {
		t.Fatalf("the statement's recorded_at is %v, want the result's created_at %v", recordedAt, result.CreatedAt)
	}
	statement := decodeBody[struct {
		Result struct {
			Attestation json.RawMessage `json:"attestation"`
		} `json:"result"`
	}](t, response).Result.Attestation
	if bytes.Contains(statement, []byte("ada@example.com")) {
		t.Fatalf("the statement names the result entry's person ada@example.com: %s", statement)
	}
}

// TestTheResultsReadAnswersTheStatementTheWriteStored: the statement the write answers is the one
// the row stores and the one the read answers, value for value.
func TestTheResultsReadAnswersTheStatementTheWriteStored(t *testing.T) {
	rig := newCIJobRig(t)
	written, _ := recordedWithStatement(t, "the job's result for c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	if written.Attestation == nil {
		t.Fatalf("the write's answer for c1 carries no attestation")
	}
	read, _ := rawResultOf(t, rawCheck(t, rig.readRawChecks(t, rig.issue), "c1"), written.ID)
	if read.Attestation == nil {
		t.Fatalf("the read's result for c1 carries no attestation")
	}
	var column string
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select attestation::text from ci_check_results where id = $1::uuid`, written.ID).Scan(&column); err != nil {
		t.Fatalf("read the stored statement: %v", err)
	}
	var stored wireStatement
	if err := json.Unmarshal([]byte(column), &stored); err != nil {
		t.Fatalf("decode the stored statement %s: %v", column, err)
	}
	if !reflect.DeepEqual(*written.Attestation, *read.Attestation) || !reflect.DeepEqual(*written.Attestation, stored) {
		t.Fatalf("the write answered\n%+v\nthe read answered\n%+v\nthe row stores\n%+v\nwant one statement", *written.Attestation, *read.Attestation, stored)
	}
}

// TestOnlyAJobsResultCarriesAStatementOfItsRun: a result a role recorded and a result Dispatch
// derives from a check run carry no attestation member, in the write's answer and in the read,
// while the job's result beside them carries one; the job's row is the one row of
// ci_check_results, and check_results, which holds the tester's row, holds no statement at all.
func TestOnlyAJobsResultCarriesAStatementOfItsRun(t *testing.T) {
	rig := newCIJobRig(t)
	job, _ := recordedWithStatement(t, "the job's result for c1", rig.post(t, rig.issue, rig.jobToken(nil), jobResultOf("c1", "pass")))
	tester, testerRaw := recordedWithStatement(t, "the tester's result for k1",
		rig.post(t, rig.issue, rig.sessionToken(), resultBody("k1", "pass", ciJobCommit, map[string]any{"role": "tester"})))
	if hasMember(t, testerRaw, "attestation") {
		t.Fatalf("the tester's write answers an attestation: %s", testerRaw)
	}
	writeHead(t, rig.database, ciJobRepository, 70, ciJobHead, "open", rig.issue)
	rig.github.listings.set(ciJobHead, completedRun(11, ciJobName, "success"))

	checks := rig.readRawChecks(t, rig.issue)
	if _, raw := rawResultOf(t, rawCheck(t, checks, "c1"), job.ID); !hasMember(t, raw, "attestation") {
		t.Fatalf("the read's result of the job on c1 carries no attestation: %s", raw)
	}
	if _, raw := rawResultOf(t, rawCheck(t, checks, "c1"), ciResultIDPrefix+"11"); hasMember(t, raw, "attestation") {
		t.Fatalf("the derived result on c1 carries an attestation: %s", raw)
	}
	if _, raw := rawResultOf(t, rawCheck(t, checks, "k1"), tester.ID); hasMember(t, raw, "attestation") {
		t.Fatalf("the tester's result on k1 carries an attestation: %s", raw)
	}

	var jobRows, testerRows int
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select (select count(*) from ci_check_results where id = $1::uuid and attestation is not null),
		       (select count(*) from check_results where id = $2::uuid)
	`, job.ID, tester.ID).Scan(&jobRows, &testerRows); err != nil {
		t.Fatalf("read the stored rows: %v", err)
	}
	if jobRows != 1 || testerRows != 1 {
		t.Fatalf("ci_check_results holds %d rows of the job's result with a statement, and check_results %d of the tester's; want 1 and 1", jobRows, testerRows)
	}
}

// TestAJobsMatrixLegIsItsLegOnTheIssueRoute: a job whose check run is one leg of a matrix,
// "go-checks (linux)", records the check whose ci_check names that check run. Its statement reads
// the job and the leg from the check run's name as the project's route reads them, and names the
// check's ci_check whole as its test's job.
func TestAJobsMatrixLegIsItsLegOnTheIssueRoute(t *testing.T) {
	rig := newCIJobRig(t)
	filed := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": ciJobProject, "title": "A matrix job proves it", "spec": "A spec\n"})
	if filed.Code != http.StatusCreated {
		t.Fatalf("create an issue of %s: status=%d body=%s", ciJobProject, filed.Code, filed.Body.String())
	}
	key := decodeBody[model.Issue](t, filed).Key
	uploadDocument(t, rig.handler, "/api/v1/issues/"+key, "verification.md",
		ciCheckBlock("m1", `negative_control="`+ciJobNegativeControl+`" ci_check="go-checks (linux)"`))
	rig.github.setCheckRun(98, "go-checks (linux)", ciJobCommit)
	// The run pushed the new issue's own branch: a job's result counts only on a branch of its issue.
	token := rig.jobToken(func(answer *brokertoken.Introspection) {
		answer.CI.CheckRunID = "98"
		answer.CI.Ref = "refs/heads/legion/" + key
		answer.CI.WorkflowRef = ciJobRepository + "/.github/workflows/ci-results.yaml@refs/heads/legion/" + key
	})
	response := rig.post(t, key, token, jobResultOf("m1", "pass"))
	result, _ := recordedWithStatement(t, "the job's result for m1", response)
	if result.Attestation == nil {
		t.Fatalf("the job's result for m1 carries no attestation: %s", response.Body.String())
	}
	predicate := result.Attestation.Predicate
	if predicate.Job != "go-checks" || predicate.Leg != "linux" || predicate.Test.Job != "go-checks (linux)" || predicate.Run.CheckRunID != "98" {
		t.Fatalf("the statement names job %q, leg %q, test job %q and check run %q; want go-checks, linux, %q and 98",
			predicate.Job, predicate.Leg, predicate.Test.Job, predicate.Run.CheckRunID, "go-checks (linux)")
	}
}
