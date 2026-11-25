import { expect, test } from "bun:test";
import type {
  CheckBlock,
  CheckPassStands,
  CheckResult,
  IssueCheckResults,
} from "@legion/contracts";
import { formatCheckResults } from "../dispatch-commands/render-results";

const repo = "acme/widgets";
const merge = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const commit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const pass: CheckResult = {
  id: "pass-1",
  issue_key: "DSP-1",
  check_issue_key: "DSP-1",
  check_artifact_id: "verification",
  check_block_id: "live",
  check_version: 1,
  check_sha256: "digest",
  check_words_moved: false,
  role: "tester",
  verdict: "pass",
  commit_sha: commit,
  commit_stands: { answer: "at_basis" },
  surface: "the program",
  run: "read",
  observed: "The program answers.",
  negative_control: "",
  screenshots: [],
  session: "",
  failed: 0,
  skipped: 0,
  excluded: 0,
  actor: { kind: "user", id: "alice" },
  created_at: "2026-11-20T00:00:00Z",
};
const fail: CheckResult = { ...pass, id: "fail-1", role: "reviewer", verdict: "fail" };
const deploy = { program: "worker", commit, deployed_at: "2026-11-20T01:00:00Z" };
const check: CheckBlock = {
  issue_key: "DSP-1",
  artifact_id: "verification",
  block_id: "live",
  ref: "dispatch://DSP-1/artifact/verification#b-live",
  sha256: "digest",
  version: 1,
  when: "after_merge",
  text: "The program answers.",
  negative_control: "",
  run: "read",
  surface: "the program",
  compared_with: [{ kind: "branch", branch: "main", repo, commit, merge_commit: merge }],
  pass_stands: { answer: "yes", roles: ["tester"], commit },
  results: [pass],
};
function render(changes: Partial<CheckBlock>, brief = false): string {
  const wire: IssueCheckResults = {
    issue: "DSP-1",
    verification_artifact_id: "verification",
    checks: [{ ...check, ...changes }],
  };
  return formatCheckResults(wire, brief, { "DSP-1": repo });
}
const cases: readonly [string, CheckPassStands, CheckResult[], string][] = [
  [
    "yes",
    { answer: "yes", roles: ["tester"], results: [pass.id], commit },
    [pass],
    `A pass stands: yes, from tester, as the newest result is pass pass-1 at ${commit}.`,
  ],
  [
    "no, fail",
    { answer: "no", reason: "fail", results: [fail.id], commit },
    [fail],
    `A pass stands: no, as the newest result is fail fail-1 at ${commit}.`,
  ],
  [
    "no, no_result",
    { answer: "no", reason: "no_result", results: [] },
    [],
    "A pass stands: no, as no result counts at a commit that holds the merge.",
  ],
  [
    "unknown, pass_and_fail",
    { answer: "unknown", reason: "pass_and_fail", results: [pass.id, fail.id], commit },
    [pass, fail],
    `A pass stands: not known, as pass pass-1 and fail fail-1 both count at ${commit}. A retraction of the wrong one ends it.`,
  ],
  [
    "unknown, not_placed",
    { answer: "unknown", reason: "not_placed", results: [pass.id], commit },
    [pass],
    `A pass stands: not known, as Dispatch could not place pass pass-1 at ${commit}.`,
  ],
  [
    "unknown, read_failed",
    { answer: "unknown", reason: "read_failed", results: [] },
    [],
    "A pass stands: not known, as a read Dispatch made did not answer.",
  ],
  [
    "unknown, program_not_named",
    { answer: "unknown", reason: "program_not_named", results: [] },
    [],
    `A pass stands: not known, as ${repo} runs more than one program and this check names none.`,
  ],
  [
    "unknown, no_deploy",
    { answer: "unknown", reason: "no_deploy", results: [] },
    [],
    `A pass stands: not known, as Dispatch holds no deploy of worker in ${repo}.`,
  ],
  [
    "unknown, deploy_not_merged",
    { answer: "unknown", reason: "deploy_not_merged", results: [], deploy },
    [],
    `A pass stands: not known, as worker runs ${commit} since ${deploy.deployed_at}, which does not hold the merge.`,
  ],
  [
    "unknown, no_result_from_deploy",
    { answer: "unknown", reason: "no_result_from_deploy", results: [], deploy },
    [],
    `A pass stands: not known, as no result counts for worker at ${commit}, deployed ${deploy.deployed_at}.`,
  ],
];
for (const [name, pass_stands, results, sentence] of cases) {
  test(`after-merge text: ${name}`, () => {
    expect(
      render({
        subject: name.includes("deploy") || name.includes("program") ? "production" : "code",
        program: "worker",
        pass_stands,
        results,
      })
    ).toContain(sentence);
  });
}

test("a branch basis names the actual merge and an unknown merge says so", () => {
  expect(render({})).toContain(`in ${repo} branch main, merged at ${merge}`);
  expect(render({ compared_with: [], compared_nothing: "not_merged", results: [] })).toContain(
    "Compared with nothing: the work has no merge that Dispatch knows."
  );
});

test("an answer with no basis still names its check owner's repository", () => {
  expect(
    render({
      compared_with: [],
      program: "worker",
      pass_stands: { answer: "unknown", reason: "no_deploy" },
    })
  ).toContain(`Dispatch holds no deploy of worker in ${repo}`);
});

test("an excluded result says why it counts for nothing", () => {
  expect(
    render({
      results: [
        {
          ...pass,
          not_counted: "it was recorded before the current deploy of the program this check reads",
        },
      ],
    })
  ).toContain(
    "It counts for nothing: it was recorded before the current deploy of the program this check reads."
  );
});

test("brief cuts only a reading from a commit that counts at no basis", () => {
  const long = "The complete observation. ".repeat(30);
  expect(render({ results: [{ ...pass, observed: long }] }, true)).toContain(long);
  const outside = render(
    { results: [{ ...pass, observed: long, commit_stands: { answer: "other_commit" } }] },
    true
  );
  expect(outside).not.toContain(long);
  expect(outside).toContain(`${long.slice(0, 200)}… (result pass-1)`);
});

test("a before-merge check keeps its existing result head wording", () => {
  expect(
    render({
      when: "before_merge",
      compared_with: [{ kind: "pull_request", commit, pull_request: 7 }],
      pass_stands: { answer: "yes", roles: ["tester"] },
    })
  ).toContain(
    `- live (before_merge). Compared with the head of pull request #7 at ${commit}. A pass stands: yes, from tester.`
  );
});
