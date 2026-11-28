// results-ci-two-heads.test.ts holds the head line of a check CI proves, on an issue with two open
// pull requests, to the model's contract (model.CheckCI, packages/envoy/internal/dispatch/model/
// model.go): a reader picks the result derived at a head by `ci.result`, never another way. The
// check's `ci` is the read at one head and its results hold the result derived at the other, so
// the head line must not join one head's verdict to the other head's case. Main's renderer took
// the newest result of any head and printed "pass at <a> by ci (CI run …, absent)" for both reads.
import { expect, test } from "bun:test";
import type { IssueCheckResults } from "@legion/contracts";
import { formatCheckResults } from "../dispatch-commands/check-results";

const repo = "acme/widgets";
const a = "3dd926fb93c54e19ec3d6ebd7f3b91e50fa8aabb"; // head of #70, unit-tests passed
const b = "e8cf85a8e8cf85a8e8cf85a8e8cf85a8e8cf85a8"; // head of #71, no unit-tests run
const derivedAtA = {
  id: "ci-run:141030078695",
  issue_key: "ACME-1",
  check_issue_key: "ACME-1",
  check_artifact_id: "verification",
  check_block_id: "c5-unit",
  check_version: 9,
  check_sha256: "digest",
  check_words_moved: false,
  role: "ci",
  verdict: "pass",
  commit_sha: a,
  surface: "cli",
  run: "unit-tests / test",
  observed: `conclusion: success\nhttps://github.com/${repo}/actions/runs/46161533162/job/141030078695`,
  negative_control: "",
  screenshots: [],
  session: "",
  failed: 0,
  skipped: 0,
  excluded: 0,
  actor: { kind: "github_app", id: "github-actions" },
  created_at: "2026-11-28T15:58:54Z",
};
const ciAtB = {
  run: "unit-tests / test",
  state: "no_result",
  case: "absent",
  detail: `CI run unit-tests / test has no run at ${b}, the head of ${repo}#71.`,
  line: "- Check c5-unit (before_merge): CI run unit-tests / test has no result (absent)",
  commit: b,
  repo,
};

function headLine(check: Record<string, unknown>): string {
  const wire = {
    issue: "ACME-1",
    verification_artifact_id: "verification",
    checks: [
      {
        issue_key: "ACME-1",
        artifact_id: "verification",
        block_id: "c5-unit",
        ref: "dispatch://ACME-1/artifact/verification-md#b-c5-unit",
        sha256: "digest",
        version: 9,
        when: "before_merge",
        text: "",
        negative_control: "",
        run: "",
        surface: "cli",
        ci_check: "unit-tests / test",
        ci: ciAtB,
        ...check,
      },
    ],
  } as unknown as IssueCheckResults;
  const line = formatCheckResults(wire)
    .split("\n")
    .find((each) => each.startsWith("- c5-unit ("));
  if (line === undefined) throw new Error("no head line for c5-unit");
  return line;
}

test("--at a's head: the head line takes no verdict from a result the `ci` read did not derive", () => {
  const line = headLine({
    compared_with: [{ kind: "named", commit: a }],
    pass_stands: { answer: "yes", roles: ["ci"] },
    results: [
      { ...derivedAtA, commit_stands: { answer: "at_basis", basis: { kind: "named", commit: a } } },
    ],
  });
  // main prints: "- c5-unit (before_merge): pass at 3dd926fb… by ci (CI run unit-tests / test, absent), …"
  expect(line).not.toContain(`pass at ${a}`);
  expect(line).toContain("no result:");
});

test("plain read with two open heads: the head line names one head, not a's pass with b's case", () => {
  const atA = { kind: "pull_request", commit: a, pull_request: 70 };
  const atB = { kind: "pull_request", commit: b, pull_request: 71 };
  const line = headLine({
    compared_with: [atA, atB],
    pass_stands: {
      answer: "no",
      subjects: [
        { basis: atA, answer: "yes", roles: ["ci"], results: [derivedAtA.id] },
        { basis: atB, answer: "no", reason: "no_result", ci: ciAtB },
      ],
    },
    results: [{ ...derivedAtA, commit_stands: { answer: "at_basis", basis: atA } }],
  });
  expect(line).not.toContain(`pass at ${a}`);
  expect(line).toContain("no result:");
});
