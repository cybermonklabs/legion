package api

// result_attestation.go builds the in-toto statement that a CI job's result carries, for both
// routes a job writes a result through (R12 of the spec: "A result carries the subject that it
// read and the procedure that ran, in the shape of an in-toto attestation"): POST
// /api/v1/projects/{key}/results, for a check of the spec files, and POST
// /api/v1/issues/{key}/results, for a check block that its ci_check proves. Both routes build the
// statement here and stamp its recorded_at here, so the statement, its subject, its run and its
// recorder are one shape on both. The predicate type says which kind of check the predicate's
// check, procedure_digest and test name.

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The type of the in-toto statement, and the two predicate types of a CI job's result.
const (
	inTotoStatementType = "https://in-toto.io/Statement/v1"
	// specCheckResultPredicateType is the predicate of a result of a check of the spec files:
	// check is the check's id as the files at the subject commit hold it, procedure_digest its
	// specgraph.Check.Digest, and test its test part.
	specCheckResultPredicateType = "https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1"
	// checkBlockResultPredicateType is the predicate of a result of a check block of an issue's
	// Verification document that its ci_check proves: check is the block's canonical reference
	// (text.FormatBlockRef), procedure_digest the block's text_sha256 when the result was recorded
	// (the result's check_sha256), and test.job the block's ci_check, with no runner and no
	// selector, since the block names a job and no test.
	checkBlockResultPredicateType = "https://github.com/cybermonklabs/legion/attestations/check-block-result/v1"
)

// attestedResult is what the statement of one CI job's result is built from: the check and its
// procedure as the route read them, what the job observed, the job and its leg as the route
// decided them, and the run its results token proved.
type attestedResult struct {
	predicateType   string
	project         string
	check           string
	procedureDigest string
	test            model.ResultTest
	verdict         string
	observed        string
	// job is the job that made the run's check run, and leg its matrix leg, or "" for none, as
	// the route decided them: the project's results route from the run's workflow file (legOf,
	// workflowJobs), since a check run's name alone shows no leg.
	job, leg string
	// run is the run the results token proved, read whole by the verifier (ciphertoken.Caller.CI,
	// oauthwire.ParseCIRun): its numbers and its workflow's path are read off it.
	run oauthwire.Run
	// session is the session the results token proved.
	session string
}

// statement is the in-toto Statement v1 of a: its subject the run's repository at the run's
// commit, and its predicate the check, its procedure, the verdict and what was observed, the job
// and its leg, the run and its recorder, which names the run and no person: the broker's own
// spelling of the run's workflow (oauthwire.ResultPrincipal), so the identity on a result is the
// one the broker authenticated. RecordedAt stays zero: the insert writes it (stampedAttestation).
func (a attestedResult) statement() model.ResultAttestation {
	return model.ResultAttestation{
		Type: inTotoStatementType,
		Subject: []model.AttestationSubject{{
			Name: "git+https://github.com/" + a.run.Repository, Digest: map[string]string{"gitCommit": a.run.SHA},
		}},
		PredicateType: a.predicateType,
		Predicate: model.ResultPredicate{
			Project: a.project, Check: a.check, ProcedureDigest: a.procedureDigest, Test: a.test,
			Verdict: a.verdict, Observed: a.observed, Job: a.job, Leg: a.leg,
			Run: model.ResultRun{
				Repository: a.run.Repository, WorkflowRef: a.run.WorkflowRef, Ref: a.run.Ref, EventName: a.run.EventName,
				RunID: a.run.RunID, Attempt: int(a.run.Attempt), CheckRunID: a.run.CheckRunID, RunnerEnvironment: a.run.RunnerEnvironment,
			},
			Recorder: model.ResultRecorder{Session: a.session, Principal: oauthwire.ResultPrincipal(a.run.Repository, a.run.Workflow)},
		},
	}
}

// stampedAttestation is the SQL value of the statement bound to the parameter param (such as
// "$12"), with its predicate.recorded_at set to the inserting transaction's now(), written in UTC
// with a Z: the time the row takes as its own, which every reader decodes as one instant. A null
// statement stays null, since jsonb_set is strict.
func stampedAttestation(param string) string {
	return fmt.Sprintf(`jsonb_set(%s::jsonb, '{predicate,recorded_at}', to_jsonb(to_char(now() at time zone 'utc', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')))`, param)
}
