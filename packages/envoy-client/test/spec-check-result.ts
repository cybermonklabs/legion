// packages/envoy-client/test/spec-check-result.ts
//
// A result a CI job stored for a check of ACME's spec files, as Dispatch serves it, for the tests
// of the session's `dispatch results --project` (dispatch-cli.test.ts) and of the job's own
// `dispatch result --project` (dispatch-job-result.test.ts).

/** The commit a CI job tested, which every result of a check of ACME's spec files is for. */
export const SPEC_COMMIT = "1111111111111111111111111111111111111111";

/** One result a CI job stored for check p-1 of ACME's spec files at SPEC_COMMIT, as Dispatch
 *  serves it: Dispatch's record number and an in-toto statement of the job's run. */
export function storedSpecResult(seq: number, verdict: string, leg: string, attempt: number) {
  const workflow = "example-org/example-repo/.github/workflows/ci-results.yaml";
  return {
    id: `00000000-0000-4000-8000-0000000000${seq}`,
    seq,
    kind: "run",
    attestation: {
      _type: "https://in-toto.io/Statement/v1",
      subject: [
        {
          name: "git+https://github.com/example-org/example-repo",
          digest: { gitCommit: SPEC_COMMIT },
        },
      ],
      predicateType: "https://github.com/cybermonklabs/legion/attestations/spec-check-result/v1",
      predicate: {
        project: "ACME",
        check: "p-1",
        procedure_digest: "sha256:5f1d9c0e",
        test: { runner: "go", selector: "example.com/m/p TestP", job: "go-checks" },
        verdict,
        observed: verdict === "pass" ? "--- PASS: TestP (0.01s)" : "--- FAIL: TestP (0.02s)",
        job: "go-checks",
        leg,
        run: {
          repository: "example-org/example-repo",
          workflow_ref: `${workflow}@refs/heads/feat/probe`,
          ref: "refs/heads/feat/probe",
          event_name: "push",
          run_id: "4242",
          attempt,
          check_run_id: "7001",
          runner_environment: "github-hosted",
        },
        recorder: {
          session: `ci-results:${workflow}`,
          principal: `workflow-results:${workflow}`,
        },
        recorded_at: "2026-11-16T10:00:00Z",
      },
    },
  };
}
