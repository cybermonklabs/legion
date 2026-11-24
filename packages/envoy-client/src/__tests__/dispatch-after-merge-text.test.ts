import { expect, test } from "bun:test";
import type { CheckBlock, CheckPassStands, CheckResult, IssueCheckResults } from "@legion/contracts";
import { formatCheckResults } from "../dispatch-commands/render-results";

const repo = "acme/widgets";
const merge = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const commit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const other = "cccccccccccccccccccccccccccccccccccccccc";
const result: CheckResult = {
  id: "result-1", issue_key: "DSP-1", check_issue_key: "DSP-1", check_artifact_id: "verification",
  check_block_id: "live", check_version: 1, check_sha256: "digest", check_words_moved: false,
  role: "tester", verdict: "pass", commit_sha: commit, commit_stands: { answer: "at_basis" },
  surface: "the program", run: "read", observed: "The program answers.", negative_control: "",
  failed: 0, skipped: 0, excluded: 0, actor: { kind: "user", id: "alice" }, created_at: "2026-11-20T00:00:00Z",
};
const check: CheckBlock = {
  issue_key: "DSP-1", artifact_id: "verification", block_id: "live", ref: "dispatch://DSP-1/artifact/verification#b-live",
  sha256: "digest", version: 1, when: "after_merge", text: "The program answers.",
  compared_with: [{ kind: "branch", branch: "main", repo, commit, merge_commit: merge }],
  pass_stands: { answer: "yes", roles: ["tester"], commit }, results: [result],
};
function render(changes: Partial<CheckBlock>, brief = false): string {
  const wire: IssueCheckResults = { issue: "DSP-1", verification_artifact_id: "verification", checks: [{ ...check, ...changes }] };
  return formatCheckResults(wire, brief);
}

test("an after-merge code pass names the merge and the deciding result commit", () => {
  expect(render({})).toContain(`A pass stands: yes, holds the merge ${merge} in ${repo} at ${commit}.`);
});

test("an after-merge conflict names the live fail and the competing pass, not a vote", () => {
  const fail = { ...result, id: "fail-1", role: "reviewer", verdict: "fail" as const };
  const text = render({ pass_stands: { answer: "unknown", reason: "pass_and_fail", results: ["fail-1", result.id], commit }, results: [fail, result] });
  expect(text).toContain(`A pass stands: no; the live fail is fail-1 by reviewer at ${commit}. The pass it stands against is result-1 by tester at ${commit}. Retract the wrong result, then run dispatch results again.`);
  expect(text).not.toContain("A pass stands: not known.");
});

test("a live check names its program and the deploy interval, then each result that cannot count", () => {
  const deploy = { program: "worker", commit, deployed_at: "2026-11-20T01:00:00Z" };
  const text = render({ subject: "production", program: "worker", pass_stands: { answer: "yes", commit, roles: ["tester"], deploy }, results: [
    { ...result, id: "old-build", commit_sha: other, not_counted: "it names a commit that the current deploy of the program this check reads does not run" },
    { ...result, id: "before-deploy", not_counted: "it was recorded before the current deploy of the program this check reads" },
  ] });
  expect(text).toContain(`Program worker runs ${commit} since ${deploy.deployed_at}; a pass of this check must come from this deploy.`);
  expect(text).toContain("It counts for nothing: it names a commit that the current deploy of the program this check reads does not run.");
  expect(text).toContain("It counts for nothing: it was recorded before the current deploy of the program this check reads.");
});

test("an unknown live result says what is absent without reading as an arbitrary negative", () => {
  for (const [reason, sentence] of [
    ["program_not_named", `A pass stands: not known; this check does not name which program of ${repo} it reads. Write program=\"<program>\" on the check.`],
    ["no_deploy", `A pass stands: not known; Dispatch holds no deploy for worker of ${repo}. Record one with dispatch deploy.`],
    ["no_result_from_deploy", "A pass stands: not known; no result of this check comes from the current deploy of worker. Run the check against it."],
  ] as const) {
    const pass = { answer: "unknown", reason } as CheckPassStands;
    expect(render({ subject: "graph", program: "worker", pass_stands: pass, results: [] })).toContain(sentence);
  }
});

test("a result at a commit before the merge is an explicit exclusion", () => {
  const text = render({ results: [{ ...result, not_counted: "its commit predates the merge of the work" }] });
  expect(text).toContain("It counts for nothing: its commit predates the merge of the work.");
});

test("brief cuts only a reading from a commit that counts at no basis", () => {
  const long = "The complete observation. ".repeat(30);
  const current = render({ results: [{ ...result, observed: long }] }, true);
  expect(current).toContain(long);
  const outside = render({ results: [{ ...result, observed: long, commit_stands: { answer: "other_commit" } }] }, true);
  expect(outside).not.toContain(long);
  expect(outside).toContain(`${long.slice(0, 200)}… (result result-1)`);
});

test("a before-merge check keeps its existing result head wording", () => {
  expect(render({ when: "before_merge", compared_with: [{ kind: "pull_request", commit, pull_request: 7 }], pass_stands: { answer: "yes", roles: ["tester"] } }))
    .toContain(`- live (before_merge). Compared with the head of pull request #7 at ${commit}. A pass stands: yes, from tester.`);
});
