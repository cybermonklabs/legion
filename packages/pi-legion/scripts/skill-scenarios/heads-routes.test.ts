// The GitHub a heads scenario's agent reads, rendered from a world given here as data: each call
// is answered by the first route whose match names it, as gh-standin.ts picks one, and each row
// asserts GitHub's answer to that call. rig.test.ts's heads_routes rows run the same renderer on
// the world rig.sh collects from a real fixture.
import { expect, setDefaultTimeout, test } from "bun:test";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { type HeadsWorld, type Route, type RouteTable, renderHeadsRoutes } from "./heads-routes";
import { fileRowLimit, SCRIPT_RUN } from "./test-limit";

// One row runs the renderer as a bun process of its own, a start a loaded machine stretches.
setDefaultTimeout(fileRowLimit({ ...SCRIPT_RUN, part: "heads-routes.ts" }));

const MAIN = "1".repeat(40);
const H702 = "2".repeat(40);
const H703 = "3".repeat(40);
const BASE_TREE = "4".repeat(40);
const TREE702 = "5".repeat(40);
const TREE703 = "6".repeat(40);
const DOCS_TREE = "7".repeat(40);
const README = "a".repeat(40);
const README703 = "b".repeat(40);
const USAGE = "c".repeat(40);
const USAGE_TEXT = "# Usage\n\nRun `widgets --help` to list every command of the widgets CLI.\n";

/** A file's bytes in base64 as heads_routes collects them: 60 characters a line, and a newline. */
const base64 = (text: string) =>
  `${(
    Buffer.from(text)
      .toString("base64")
      .match(/.{1,60}/g) ?? []
  ).join("\n")}\n`;
const blob = (file: string, sha: string, text: string) => ({
  path: file,
  mode: "100644",
  type: "blob",
  sha,
  size: Buffer.byteLength(text),
  content: base64(text),
});
const DIFF702 =
  "diff --git a/docs/usage.md b/docs/usage.md\nnew file mode 100644\nindex 0000000..cccccc\n--- /dev/null\n+++ b/docs/usage.md\n@@ -0,0 +1,3 @@\n+# Usage\n+\n+Run `widgets --help` to list every command of the widgets CLI.\n";
const DIFF703 =
  "diff --git a/README.md b/README.md\nindex aaaaaa..bbbbbb 100644\n--- a/README.md\n+++ b/README.md\n@@ -1,3 +1,5 @@\n # widgets\n \n Small CLI helpers.\n+\n+Usage: see docs/usage.md.\n";

/** The world of reviewer-own-head: #702 adds docs/usage.md, #703 points the README at it, the
 * base has no protection, and `named`, `named703` and `approved` set what the case changes. */
function world(
  options: {
    named?: HeadsWorld["pulls"][number]["named"];
    named703?: HeadsWorld["pulls"][number]["named"];
    approved?: boolean;
    only702?: boolean;
  } = {}
): HeadsWorld {
  const pulls: HeadsWorld["pulls"] = [
    {
      number: 702,
      head: H702,
      body: "Dispatch: ACME-7\n\n## For the reviewer\n",
      files: [{ path: "docs/usage.md", additions: 3, deletions: 0 }],
      jobs: ["lint", "test"],
      named: options.named ?? [],
      diff: DIFF702,
      url: "https://github.com/example/widgets/pull/702",
      subject: "docs: the usage page of the widgets CLI (ACME-7)",
    },
    {
      number: 703,
      head: H703,
      body: "Dispatch: ACME-7\n\n## Verification\n",
      files: [{ path: "README.md", additions: 2, deletions: 0 }],
      jobs: ["lint", "test"],
      named: options.named703 ?? [],
      diff: DIFF703,
      url: "https://github.com/example/widgets/pull/703",
      subject: "docs: point the README at the usage page (ACME-7)",
    },
  ];
  return {
    repo: "example/widgets",
    key: "ACME-7",
    title: "A usage page",
    base: "main",
    main: MAIN,
    approved: options.approved ?? false,
    at: "2026-11-22T12:00:00Z",
    pulls: options.only702 === true ? pulls.slice(0, 1) : pulls,
    branches: [
      { name: "legion/ACME-7-702", sha: H702 },
      { name: "legion/ACME-7-703", sha: H703 },
      { name: "main", sha: MAIN },
    ],
    trees: [
      {
        sha: MAIN,
        tree: BASE_TREE,
        message: "init\n",
        parent: "",
        entries: [blob("README.md", README, "# widgets\n\nSmall CLI helpers.\n")],
      },
      {
        sha: H702,
        tree: TREE702,
        message: "docs: the usage page of the widgets CLI (ACME-7)\n",
        parent: MAIN,
        entries: [
          blob("README.md", README, "# widgets\n\nSmall CLI helpers.\n"),
          { path: "docs", mode: "040000", type: "tree", sha: DOCS_TREE, size: 0, content: null },
          blob("docs/usage.md", USAGE, USAGE_TEXT),
        ],
      },
      {
        sha: H703,
        tree: TREE703,
        message: "docs: point the README at the usage page (ACME-7)\n",
        parent: MAIN,
        entries: [
          blob(
            "README.md",
            README703,
            "# widgets\n\nSmall CLI helpers.\n\nUsage: see docs/usage.md.\n"
          ),
        ],
      },
    ],
  };
}

const GO_CHECKS = { name: "go-checks", checkRun: 9101, run: 41, suite: 41, status: "in_progress" };
const DOCS = { name: "docs", checkRun: 9001, run: 40, suite: 40, status: "completed" };
/** A named job that ended without a pass, and its log: the failed test of the changed page. */
const USAGE_LOG =
  "usage-test\tRun go test ./docs/...\t--- FAIL: TestTheUsagePageListsEveryCommand (0.01s)\n" +
  "usage-test\tRun go test ./docs/...\t    usage_test.go:21: docs/usage.md names 3 commands; `widgets --help` lists 4: add, export, list, remove\n" +
  "usage-test\tRun go test ./docs/...\tFAIL\n";
const USAGE_FAILED = {
  name: "usage-test",
  checkRun: 9201,
  run: 42,
  suite: 42,
  status: "completed",
  conclusion: "failure",
  log: USAGE_LOG,
};

/** The route gh-standin.ts answers `call` with: the first whose match names it. */
function routeOf(table: RouteTable, call: string): Route {
  const route = table.routes.find((candidate) => new RegExp(candidate.match, "s").test(call));
  if (route === undefined) throw new Error(`no route names: ${call}`);
  return route;
}
/** The answer gh-standin.ts gives `call` before any `--json` or `--jq`. */
const answer = (table: RouteTable, call: string) => routeOf(table, call).stdout;
const notFound = (doc: string) => ({
  stdout: { message: "Not Found", documentation_url: doc, status: "404" },
  stderr: "gh: Not Found (HTTP 404)\n",
  exit: 1,
  status: 404,
});
const plain = renderHeadsRoutes(world());

// Catches: a `gh pr` read answered for the wrong pull request, a number the world does not hold
// answered as if it did, or a base said to require checks.
test("gh pr: the pull request a call names, else the first; an unknown number refused", () => {
  expect(plain.current).toBe(702);
  expect(plain.login).toBe("rig[bot]");
  expect(answer(plain, "gh pr view --json number")).toMatchObject({
    number: 702,
    headRefOid: H702,
  });
  expect(answer(plain, "gh pr view 703 --json headRefOid")).toMatchObject({
    number: 703,
    headRefName: "legion/ACME-7-703",
    headRefOid: H703,
    baseRefName: "main",
    title: "A usage page (ACME-7)",
    mergeStateStatus: "CLEAN",
    reviewDecision: "",
    additions: 2,
    deletions: 0,
    changedFiles: 1,
    commits: [{ oid: H703, messageHeadline: "docs: point the README at the usage page (ACME-7)" }],
  });
  expect(answer(plain, "gh pr diff 703 --repo example/widgets")).toBe(DIFF703);
  expect(answer(plain, "gh pr diff")).toBe(DIFF702);
  expect(answer(plain, "gh pr list --state open")).toMatchObject([
    { number: 702 },
    { number: 703 },
  ]);
  expect(answer(plain, "gh pr edit 703 --body x")).toBe(
    "https://github.com/example/widgets/pull/703"
  );
  expect(routeOf(plain, "gh pr view 704 --json number")).toMatchObject({
    stderr:
      "GraphQL: Could not resolve to a PullRequest with that number. (repository.pullRequest)\n",
    exit: 1,
  });
  expect(routeOf(plain, "gh pr checks 703 --required")).toMatchObject({
    stderr: "no required checks reported on the 'legion/ACME-7-703' branch\n",
    exit: 1,
  });
  expect(routeOf(plain, "gh pr checks --required")).toMatchObject({
    stderr: "no required checks reported on the 'legion/ACME-7-702' branch\n",
    exit: 1,
  });
  expect(routeOf(plain, "gh pr ready").stderr).toBe(
    '! Pull request example/widgets#702 is already "ready for review"\n'
  );
  expect(routeOf(plain, "gh pr merge 702 --squash").stderr).toBe(
    "✓ Squashed and merged pull request example/widgets#702 (A usage page (ACME-7))\n"
  );
  expect(routeOf(plain, "gh pr merge -r").stderr).toBe(
    "✓ Rebased and merged pull request example/widgets#702 (A usage page (ACME-7))\n"
  );
  expect(routeOf(plain, "gh pr merge --merge").stderr).toBe(
    "✓ Merged pull request example/widgets#702 (A usage page (ACME-7))\n"
  );
});

// Catches: a named job still running that GitHub's reads call passed: `gh pr checks` must exit 8,
// the merge state be UNSTABLE, the rollup PENDING and the check run in progress.
test("a named job in progress: gh pr checks exits 8, UNSTABLE, PENDING, in_progress", () => {
  const running = renderHeadsRoutes(world({ named: [GO_CHECKS], only702: true }));
  expect(routeOf(running, "gh pr checks 702")).toMatchObject({
    stdout:
      "lint\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\ntest\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\ngo-checks\tpending\t0\thttps://github.com/example/widgets/actions/runs/41/job/9101",
    exit: 8,
  });
  expect(answer(running, "gh pr view")).toMatchObject({
    mergeStateStatus: "UNSTABLE",
    statusCheckRollup: [
      { name: "lint", status: "COMPLETED", conclusion: "SUCCESS" },
      { name: "test", status: "COMPLETED", conclusion: "SUCCESS" },
      { name: "go-checks", status: "IN_PROGRESS", conclusion: "" },
    ],
  });
  expect(answer(running, "gh api repos/example/widgets/pulls/702")).toMatchObject({
    mergeable_state: "unstable",
  });
  expect(answer(running, "gh api graphql -f query=q")).toMatchObject({
    data: {
      repository: {
        pullRequest: {
          mergeStateStatus: "UNSTABLE",
          commits: {
            nodes: [
              {
                commit: {
                  statusCheckRollup: {
                    state: "PENDING",
                    contexts: {
                      nodes: [
                        { name: "lint", status: "COMPLETED", conclusion: "SUCCESS" },
                        { name: "test", status: "COMPLETED", conclusion: "SUCCESS" },
                        { name: "go-checks", status: "IN_PROGRESS", conclusion: null },
                      ],
                    },
                  },
                },
              },
            ],
          },
        },
      },
    },
  });
  expect(answer(running, `gh api repos/example/widgets/commits/${H702}/check-runs`)).toMatchObject({
    total_count: 3,
    check_runs: [
      { id: 8000, name: "lint", run_id: 1, status: "completed", conclusion: "success" },
      { id: 8001, name: "test", run_id: 1, status: "completed", conclusion: "success" },
      {
        id: 9101,
        name: "go-checks",
        run_id: 41,
        check_suite: { id: 41 },
        status: "in_progress",
        conclusion: null,
        completed_at: null,
        html_url: "https://github.com/example/widgets/actions/runs/41/job/9101",
      },
    ],
  });
  // #703 is not in this world: its number is refused like any other.
  expect(routeOf(running, "gh pr view 703").exit).toBe(1);
  const passed = renderHeadsRoutes(world({ named: [DOCS] }));
  expect(routeOf(passed, "gh pr checks").exit).toBe(0);
  expect(routeOf(passed, "gh pr checks 703").exit).toBe(0);
});

// Catches: a named job that ended without a pass read as passed (the routes knew only success and
// in progress), and a log of the failed job that no read of gh answers, where the role texts send
// the implementer and the reviewer to read it. A job that failed at #703's head alone leaves #702's
// reads passed.
test("a named job that failed: gh pr checks exits 1, its run and job fail, and its log is read", () => {
  const failed = renderHeadsRoutes(world({ named: [USAGE_FAILED] }));
  expect(routeOf(failed, "gh pr checks 702")).toMatchObject({
    stdout:
      "lint\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\ntest\tpass\t10s\thttps://github.com/example/widgets/actions/runs/1\nusage-test\tfail\t10s\thttps://github.com/example/widgets/actions/runs/42/job/9201",
    exit: 1,
  });
  expect(answer(failed, "gh pr view")).toMatchObject({
    mergeStateStatus: "UNSTABLE",
    statusCheckRollup: [
      { name: "lint", status: "COMPLETED", conclusion: "SUCCESS" },
      { name: "test", status: "COMPLETED", conclusion: "SUCCESS" },
      { name: "usage-test", status: "COMPLETED", conclusion: "FAILURE" },
    ],
  });
  expect(answer(failed, "gh api graphql -f query=q")).toMatchObject({
    data: {
      repository: {
        pullRequest: {
          commits: { nodes: [{ commit: { statusCheckRollup: { state: "FAILURE" } } }] },
        },
      },
    },
  });
  expect(answer(failed, `gh api repos/example/widgets/commits/${H702}/check-runs`)).toMatchObject({
    check_runs: [
      { id: 8000 },
      { id: 8001 },
      { id: 9201, status: "completed", conclusion: "failure" },
    ],
  });
  expect(answer(failed, "gh api repos/example/widgets/actions/runs/42")).toMatchObject({
    id: 42,
    status: "completed",
    conclusion: "failure",
  });
  expect(answer(failed, "gh api repos/example/widgets/actions/jobs/9201")).toMatchObject({
    id: 9201,
    conclusion: "failure",
  });
  for (const call of [
    "gh api repos/example/widgets/actions/jobs/9201/logs",
    "gh run view 42 --log-failed",
    "gh run view 42 --repo example/widgets --log",
    "gh run view --job 9201 --log",
    "gh run view --log-failed --job=9201",
  ])
    expect(routeOf(failed, call)).toMatchObject({ stdout: USAGE_LOG });
  expect(answer(failed, "gh run view 42")).toContain("X usage-test");
  const elsewhere = renderHeadsRoutes(
    world({
      named: [{ ...DOCS, checkRun: 9202, run: 43, suite: 43, name: "usage-test" }],
      named703: [USAGE_FAILED],
    })
  );
  expect(routeOf(elsewhere, "gh pr checks 702").exit).toBe(0);
  expect(routeOf(elsewhere, "gh pr checks 703").exit).toBe(1);
  expect(routeOf(elsewhere, "gh run view --job 9201 --log")).toMatchObject({ stdout: USAGE_LOG });
});

// Catches: workflow runs or jobs of a head that disagree with its check runs, a named job served
// under another run than the one rig-world.json names, and a read of an unknown run or job taken
// for an empty answer instead of GitHub's 404.
test("workflow runs and jobs of each head, by head, by run and by job, and GitHub's 404", () => {
  const named = renderHeadsRoutes(world({ named: [DOCS] }));
  expect(
    answer(named, `gh api repos/example/widgets/actions/runs?head_sha=${H702}&per_page=100`)
  ).toMatchObject({
    total_count: 2,
    workflow_runs: [
      { id: 1, name: "ci", status: "completed", conclusion: "success", head_sha: H702 },
      { id: 40, name: "docs", status: "completed", conclusion: "success", run_number: 40 },
    ],
  });
  expect(answer(named, `gh api repos/example/widgets/actions/runs?head_sha=${H703}`)).toMatchObject(
    { total_count: 1, workflow_runs: [{ id: 101, head_branch: "legion/ACME-7-703" }] }
  );
  expect(
    answer(named, `gh api repos/example/widgets/actions/runs?head_sha=${"9".repeat(40)}`)
  ).toEqual({ total_count: 0, workflow_runs: [] });
  expect(answer(named, "gh api repos/example/widgets/actions/runs/1/jobs")).toMatchObject({
    total_count: 2,
    jobs: [
      { id: 8000, run_id: 1, name: "lint", steps: [] },
      { id: 8001, run_id: 1, name: "test", steps: [] },
    ],
  });
  expect(answer(named, "gh api repos/example/widgets/actions/runs/40")).toMatchObject({
    id: 40,
    name: "docs",
    jobs_url: "https://api.github.com/repos/example/widgets/actions/runs/40/jobs",
    pull_requests: [{ number: 702 }],
  });
  expect(answer(named, "gh api repos/example/widgets/actions/jobs/9001")).toEqual({
    id: 9001,
    run_id: 40,
    name: "docs",
    head_sha: H702,
    status: "completed",
    conclusion: "success",
    started_at: "2026-11-22T12:00:00Z",
    completed_at: "2026-11-22T12:00:00Z",
    html_url: "https://github.com/example/widgets/actions/runs/40/job/9001",
    steps: [],
  });
  expect(answer(named, "gh api repos/example/widgets/check-runs/8100")).toMatchObject({
    id: 8100,
    name: "lint",
    head_sha: H703,
    run_id: 101,
  });
  expect(answer(named, "gh api repos/example/widgets/actions/runs")).toMatchObject({
    total_count: 3,
  });
  const { match: _job, ...job } = routeOf(named, "gh api repos/example/widgets/actions/jobs/1234");
  expect(job).toEqual(
    notFound("https://docs.github.com/rest/actions/workflow-jobs#get-a-job-for-a-workflow-run")
  );
  const { match: _run, ...run } = routeOf(named, "gh api repos/example/widgets/actions/runs/77");
  expect(run).toEqual(
    notFound("https://docs.github.com/rest/actions/workflow-runs#get-a-workflow-run")
  );
  const { match: _check, ...check } = routeOf(named, "gh api repos/example/widgets/check-runs/1");
  expect(check).toEqual(notFound("https://docs.github.com/rest/checks/runs#get-a-check-run"));
  expect(answer(named, `gh api repos/example/widgets/commits/${H702}/status`)).toEqual({
    state: "pending",
    sha: H702,
    total_count: 0,
    statuses: [],
    commit_url: `https://api.github.com/repos/example/widgets/commits/${H702}`,
    url: `https://api.github.com/repos/example/widgets/commits/${H702}/status`,
  });
});

/** The route gh-standin.ts answers `call` with in the world's `state`: the first whose match names
 * it and that answers in that state. */
function routeIn(table: RouteTable, state: string, call: string): Route {
  const route = table.routes.find(
    (candidate) =>
      (candidate.when === undefined || candidate.when === state) &&
      new RegExp(candidate.match, "s").test(call)
  );
  if (route === undefined) throw new Error(`no route of state '${state}' names: ${call}`);
  return route;
}

// Catches: a world whose failed job, once its re-run is pressed, still answers its failed first
// attempt (the role presses again, or reads a pass that never came), a second press GitHub would
// refuse taken as a new run, a settled attempt never read, and the first attempt's job and log lost
// once the second exists.
test("a named job run once more: the press, the second attempt running, then settled, and the first attempt kept", () => {
  const UNIT_LOG = "unit-tests\tRun bun test\t(fail) notify > retries a refused send [5001.12ms]\n";
  const unit = {
    name: "unit-tests",
    checkRun: 9301,
    run: 44,
    suite: 44,
    status: "completed",
    conclusion: "failure",
    log: UNIT_LOG,
    rerun: { checkRun: 9302, conclusion: "success" },
  };
  const rerun = renderHeadsRoutes(world({ named: [unit], only702: true }));
  // Before the press: the failed attempt, and each press moves the world on.
  expect(routeIn(rerun, "", "gh run view 44 --log-failed")).toMatchObject({ stdout: UNIT_LOG });
  expect(routeIn(rerun, "", "gh pr checks 702").exit).toBe(1);
  for (const press of [
    "gh run rerun 44 --failed",
    "gh run rerun --job 9301",
    "gh api -X POST repos/example/widgets/actions/runs/44/rerun-failed-jobs",
  ])
    expect(routeIn(rerun, "", press)).toMatchObject({ moves: "running" });
  // `--help` presses nothing; the world holds no answer of it, so the stand-in fails loudly.
  expect(() => routeIn(rerun, "", "gh run rerun 44 --help")).toThrow();
  // While the second attempt runs: every read answers it, and a press is refused.
  expect(routeIn(rerun, "running", "gh run view 44 --log-failed")).toMatchObject({
    stderr: "run 44 is still in progress; logs will be available when it is complete\n",
    exit: 1,
  });
  expect(routeIn(rerun, "running", "gh run view 44 --json status,attempt")).toMatchObject({
    stdout: { status: "in_progress", attempt: 2, conclusion: "" },
  });
  expect(routeIn(rerun, "running", "gh pr checks 702").exit).toBe(8);
  expect(routeIn(rerun, "running", "gh run rerun 44 --failed")).toMatchObject({
    stderr: "run 44 cannot be rerun; This workflow is already running\n",
    exit: 1,
  });
  expect(routeIn(rerun, "running", "gh run rerun 44 --failed").moves).toBeUndefined();
  expect(
    routeIn(rerun, "running", "gh api repos/example/widgets/actions/runs/44").stdout
  ).toMatchObject({ status: "in_progress", run_attempt: 2 });
  // The first attempt answers by its job's id and by its attempt in each state after the press.
  for (const state of ["running", "settled"]) {
    expect(
      routeIn(rerun, state, "gh api repos/example/widgets/actions/jobs/9301").stdout
    ).toMatchObject({ id: 9301, conclusion: "failure" });
    expect(routeIn(rerun, state, "gh run view 44 --attempt 1 --log-failed")).toMatchObject({
      stdout: UNIT_LOG,
    });
    expect(
      routeIn(rerun, state, "gh api repos/example/widgets/actions/runs/44/attempts/1/jobs").stdout
    ).toMatchObject({ jobs: [{ id: 9301, conclusion: "failure" }] });
  }
  // Settled: the second attempt as it concluded, and a press moves nothing.
  expect(routeIn(rerun, "settled", "gh run view 44 --json status,conclusion")).toMatchObject({
    stdout: { status: "completed", conclusion: "success", attempt: 2 },
  });
  expect(routeIn(rerun, "settled", "gh pr checks 702").exit).toBe(0);
  expect(
    routeIn(rerun, "settled", "gh api repos/example/widgets/actions/runs/44/attempts/2/jobs").stdout
  ).toMatchObject({ jobs: [{ id: 9302, conclusion: "success" }] });
  expect(routeIn(rerun, "settled", "gh run rerun 44 --failed")).toMatchObject({ stdout: "" });
  expect(routeIn(rerun, "settled", "gh run rerun 44 --failed").moves).toBeUndefined();
  // A world with no job run once more renders one table, with no state.
  expect(plain.routes.every((route) => route.when === undefined)).toBe(true);
});

// Catches: a pull request served on legion/<key>-<number> where the world names its branch, as a
// fix's task names the issue's own branch.
test("a pull request on the branch its world names", () => {
  const named = world({ only702: true });
  const onBranch = renderHeadsRoutes({
    ...named,
    pulls: named.pulls.map((pull) => ({ ...pull, branch: "legion/ACME-7" })),
  });
  expect(answer(onBranch, "gh pr view")).toMatchObject({
    headRefName: "legion/ACME-7",
    head: { ref: "legion/ACME-7" },
  });
});

// Catches: a review or comment the run posts that is never kept (the stand-in then never lists
// it), a read of the reviews taken for a write, and an approval that names another head.
test("reviews and comments: the writes kept, the lists, a review by its id, and the approval", () => {
  expect(
    routeOf(plain, "gh api --method POST repos/example/widgets/pulls/702/reviews --input r.json")
      .keep
  ).toBe("review");
  expect(routeOf(plain, "gh api repos/example/widgets/issues/703/comments -f body=x").keep).toBe(
    "comment"
  );
  expect(routeOf(plain, "gh pr review 702 --approve").keep).toBe("review");
  expect(routeOf(plain, "gh pr comment 703 --body x").keep).toBe("comment");
  expect(
    routeOf(plain, "gh api repos/example/widgets/pulls/702/reviews -X GET -f per_page=100")
  ).toEqual({
    match: "^gh api .*pulls/702/reviews",
    stdout: [],
    list: "reviews",
    pull: 702,
  });
  expect(routeOf(plain, "gh api repos/example/widgets/pulls/703/reviews/7002")).toEqual({
    match: "^gh api .*pulls/703/reviews/[0-9]+([ ?]|$)",
    lookup: "review",
    pull: 703,
  });
  expect(routeOf(plain, "gh api repos/example/widgets/issues/702/comments")).toMatchObject({
    stdout: [],
    list: "comments",
    pull: 702,
  });
  expect(answer(plain, "gh api repos/example/widgets/pulls/702/comments")).toEqual([]);
  const approved = renderHeadsRoutes(world({ approved: true }));
  const review = {
    id: 7001,
    node_id: "PRR_rig7001",
    user: { login: "rig-review[bot]", type: "Bot" },
    state: "APPROVED",
    commit_id: H702,
    submitted_at: "2026-11-22T12:00:00Z",
    html_url: "https://github.com/example/widgets/pull/702#pullrequestreview-7001",
    body: `No findings at this head.\n\nSecurity: none.\nFast-follow: none.\n\n<!-- legion: {"session":"rig-reviewer","phase":"review","head":"${H702}"} -->`,
  };
  expect(answer(approved, "gh api repos/example/widgets/pulls/702/reviews")).toEqual([review]);
  expect(answer(approved, "gh api repos/example/widgets/pulls/703/reviews")).toEqual([]);
  expect(answer(approved, "gh pr view 702")).toMatchObject({
    reviewDecision: "APPROVED",
    latestReviews: [
      { author: { login: "rig-review[bot]" }, state: "APPROVED", commit: { oid: H702 } },
    ],
  });
  expect(answer(approved, "gh api graphql -F n=702")).toMatchObject({
    data: { repository: { pullRequest: { reviewDecision: "APPROVED" } } },
  });
  expect(answer(plain, "gh api graphql -f query=q")).toMatchObject({
    data: { repository: { pullRequest: { number: 702, reviewDecision: null } } },
  });
});

// Catches: a flag that comes first after the command unseen, as when a route's lookahead wanted a
// space before the flag and the command's own space had been taken already: `-X GET` first kept a
// read of the reviews or comments as one written, a body (`--input`, `-f`) or `-X POST` first and
// alone left a review or comment unkept and answered it as the list, and `--required` first
// answered for the first pull request. Each row asserts the route that answers the call, and what
// it keeps or lists.
for (const [call, expected] of [
  [
    "gh api -X GET repos/example/widgets/pulls/702/reviews -f per_page=100",
    { match: "^gh api .*pulls/702/reviews", stdout: [], list: "reviews", pull: 702 },
  ],
  [
    "gh api --method GET repos/example/widgets/issues/703/comments -f per_page=100",
    { match: "^gh api .*issues/703/comments([ ?]|$)", stdout: [], list: "comments", pull: 703 },
  ],
  [
    "gh api --method=GET repos/example/widgets/pulls/703/reviews -F per_page=100",
    { match: "^gh api .*pulls/703/reviews", stdout: [], list: "reviews", pull: 703 },
  ],
  [
    "gh api -XGET repos/example/widgets/issues/702/comments --raw-field per_page=100",
    { match: "^gh api .*issues/702/comments([ ?]|$)", stdout: [], list: "comments", pull: 702 },
  ],
  ["gh api -X POST repos/example/widgets/pulls/702/reviews -f event=APPROVE", { keep: "review" }],
  [
    "gh api --method=POST repos/example/widgets/pulls/702/reviews --input r.json",
    { keep: "review" },
  ],
  ["gh api -XPOST repos/example/widgets/pulls/703/reviews", { keep: "review" }],
  ["gh api --input r.json repos/example/widgets/pulls/702/reviews", { keep: "review" }],
  ["gh api -f body=x repos/example/widgets/issues/703/comments", { keep: "comment" }],
  [
    "gh pr checks --required 703",
    { stderr: "no required checks reported on the 'legion/ACME-7-703' branch\n", exit: 1 },
  ],
] as const satisfies readonly (readonly [string, Partial<Route>])[]) {
  test(`a flag first after the command is seen as it is later: ${call}`, () => {
    const route = routeOf(plain, call);
    expect(route).toMatchObject(expected);
    expect({ keep: route.keep, list: route.list }).toEqual({
      keep: "keep" in expected ? expected.keep : undefined,
      list: "list" in expected ? expected.list : undefined,
    });
  });
}

// Catches: the REST reads of a pull request answered for another one, its files without the
// status or patch GitHub gives, its commit without the base as parent, and a PATCH of its body
// answered by a read.
test("REST pull requests: each by number, its commits and files, a PATCH, the list and a 404", () => {
  expect(answer(plain, "gh api repos/example/widgets/pulls/703 --jq .head.sha")).toMatchObject({
    url: "https://api.github.com/repos/example/widgets/pulls/703",
    id: 9000703,
    number: 703,
    state: "open",
    mergeable_state: "clean",
    head: {
      label: "example:legion/ACME-7-703",
      ref: "legion/ACME-7-703",
      sha: H703,
      repo: { full_name: "example/widgets" },
    },
    base: { label: "example:main", ref: "main", sha: MAIN },
  });
  expect(routeOf(plain, "gh api -X PATCH repos/example/widgets/pulls/703 -f body=new").match).toBe(
    "^gh api (?=.*(-X|--method)[ =]?PATCH( |$))(?=.*pulls/703([ ?]|$))"
  );
  expect(answer(plain, "gh api repos/example/widgets/pulls/702/commits")).toMatchObject([
    {
      sha: H702,
      commit: {
        message: "docs: the usage page of the widgets CLI (ACME-7)\n",
        tree: { sha: TREE702 },
      },
      parents: [{ sha: MAIN, html_url: `https://github.com/example/widgets/commit/${MAIN}` }],
    },
  ]);
  expect(answer(plain, "gh api repos/example/widgets/pulls/702/files")).toEqual([
    {
      sha: USAGE,
      filename: "docs/usage.md",
      status: "added",
      additions: 3,
      deletions: 0,
      changes: 3,
      blob_url: `https://github.com/example/widgets/blob/${H702}/docs/usage.md`,
      raw_url: `https://github.com/example/widgets/raw/${H702}/docs/usage.md`,
      contents_url: `https://api.github.com/repos/example/widgets/contents/docs/usage.md?ref=${H702}`,
      patch:
        "@@ -0,0 +1,3 @@\n+# Usage\n+\n+Run `widgets --help` to list every command of the widgets CLI.",
    },
  ]);
  expect(answer(plain, "gh api repos/example/widgets/pulls/703/files")).toMatchObject([
    { sha: README703, filename: "README.md", status: "modified" },
  ]);
  expect(answer(plain, "gh api repos/example/widgets/pulls?state=open")).toMatchObject([
    { number: 702 },
    { number: 703 },
  ]);
  const { match: _pull, ...pull } = routeOf(plain, "gh api repos/example/widgets/pulls/9");
  expect(pull).toEqual(notFound("https://docs.github.com/rest/pulls/pulls#get-a-pull-request"));
  expect(answer(plain, "gh api graphql -f query=pullRequest(number: 703)")).toMatchObject({
    data: { repository: { pullRequest: { number: 703, headRefOid: H703 } } },
  });
});

// Catches: a repository, branch, ref or rule read answered other than as GitHub answers an App's
// token on a base with no protection, and a read of another repository answered at all.
test("the repository, its branches, refs, protection and rules, /user, and another repository", () => {
  expect(answer(plain, "gh api repos/example/widgets --jq .default_branch")).toMatchObject({
    full_name: "example/widgets",
    name: "widgets",
    owner: { login: "example", type: "Organization" },
    default_branch: "main",
    permissions: { push: true, admin: false },
  });
  expect(answer(plain, "gh repo view example/widgets --json defaultBranchRef")).toMatchObject({
    nameWithOwner: "example/widgets",
    defaultBranchRef: { name: "main" },
    viewerPermission: "WRITE",
  });
  expect(answer(plain, "gh api repos/example/widgets/branches")).toEqual([
    {
      name: "legion/ACME-7-702",
      commit: { sha: H702, url: `https://api.github.com/repos/example/widgets/commits/${H702}` },
      protected: false,
    },
    {
      name: "legion/ACME-7-703",
      commit: { sha: H703, url: `https://api.github.com/repos/example/widgets/commits/${H703}` },
      protected: false,
    },
    {
      name: "main",
      commit: { sha: MAIN, url: `https://api.github.com/repos/example/widgets/commits/${MAIN}` },
      protected: false,
    },
  ]);
  expect(answer(plain, "gh api repos/example/widgets/branches/legion%2FACME-7-703")).toMatchObject({
    name: "legion/ACME-7-703",
    commit: { sha: H703 },
    protected: false,
    protection: { enabled: false, required_status_checks: { enforcement_level: "off" } },
  });
  const { match: _protection, ...protection } = routeOf(
    plain,
    "gh api repos/example/widgets/branches/main/protection"
  );
  expect(protection).toEqual({
    stdout: {
      message: "Resource not accessible by integration",
      documentation_url:
        "https://docs.github.com/rest/branches/branch-protection#get-branch-protection",
      status: "403",
    },
    stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
    exit: 1,
    status: 403,
  });
  expect(routeOf(plain, "gh api repos/example/widgets/branches/nope")).toMatchObject({
    stderr: "gh: Branch not found (HTTP 404)\n",
    status: 404,
  });
  expect(answer(plain, "gh api repos/example/widgets/git/ref/heads/legion/ACME-7-702")).toEqual({
    ref: "refs/heads/legion/ACME-7-702",
    node_id: "REF_rig",
    url: "https://api.github.com/repos/example/widgets/git/refs/heads/legion/ACME-7-702",
    object: {
      sha: H702,
      type: "commit",
      url: `https://api.github.com/repos/example/widgets/git/commits/${H702}`,
    },
  });
  expect(routeOf(plain, "gh api repos/example/widgets/git/refs/heads/nope").status).toBe(404);
  expect(answer(plain, "gh api repos/example/widgets/rules/branches/main")).toEqual([]);
  expect(answer(plain, "gh api repos/example/widgets/rulesets")).toEqual([]);
  expect(routeOf(plain, "gh api user --jq .login")).toMatchObject({ status: 403, exit: 1 });
  const { match: _other, ...other } = routeOf(
    plain,
    "gh api repos/skills-rig/project/branches/main"
  );
  expect(other).toEqual(notFound("https://docs.github.com/rest/repos/repos#get-a-repository"));
  expect(routeOf(plain, "gh --version").stdout).toBe(
    "gh version 2.98.0 (2026-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.98.0\n"
  );
  expect(routeOf(plain, "gh auth status").stdout).toContain(
    "Logged in to github.com account rig[bot] (GH_TOKEN)"
  );
});

// Catches: a compare of the base with a head that is not GitHub's, and a compare with a commit
// the world does not hold answered as if it did (an architect compared the base with one).
test("compare: the base with each head by name or sha, and GitHub's 404 for an unknown commit", () => {
  const ahead = {
    url: "https://api.github.com/repos/example/widgets/compare/main...legion/ACME-7-703",
    status: "ahead",
    ahead_by: 1,
    behind_by: 0,
    total_commits: 1,
    base_commit: { sha: MAIN },
    merge_base_commit: { sha: MAIN },
    commits: [
      {
        sha: H703,
        commit: {
          message: "docs: point the README at the usage page (ACME-7)",
          author: { name: "Fixture", email: "fixture@example.invalid" },
        },
      },
    ],
    files: [{ filename: "README.md", additions: 2, deletions: 0, changes: 2 }],
  };
  expect(answer(plain, "gh api repos/example/widgets/compare/main...legion/ACME-7-703")).toEqual(
    ahead
  );
  expect(answer(plain, `gh api repos/example/widgets/compare/${MAIN}%2E%2E%2E${H703}`)).toEqual(
    ahead
  );
  const doc = "https://docs.github.com/rest/commits/commits#compare-two-commits";
  for (const call of [
    `gh api repos/example/widgets/compare/main...${"e".repeat(40)}`,
    `gh api repos/example/widgets/compare/${"e".repeat(40)}...${H702}`,
  ]) {
    const { match: _compare, ...refusal } = routeOf(plain, call);
    expect({ call, refusal }).toEqual({ call, refusal: notFound(doc) });
  }
});

// Catches: a read of a file at a head (contents, raw or JSON, a directory, a tree, a blob, the
// commit) with no route, bytes that are not the commit's own, and a path or object no commit
// holds answered instead of GitHub's 404.
test("the files of the base and each head: contents, raw bytes, listings, trees, blobs, commits", () => {
  const usage = answer(plain, `gh api repos/example/widgets/contents/docs/usage.md?ref=${H702}`);
  expect(usage).toEqual({
    type: "file",
    encoding: "base64",
    size: Buffer.byteLength(USAGE_TEXT),
    name: "usage.md",
    path: "docs/usage.md",
    content: base64(USAGE_TEXT),
    sha: USAGE,
    url: `https://api.github.com/repos/example/widgets/contents/docs/usage.md?ref=${H702}`,
    git_url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
    html_url: `https://github.com/example/widgets/blob/${H702}/docs/usage.md`,
    download_url: `https://raw.githubusercontent.com/example/widgets/${H702}/docs/usage.md`,
  });
  expect(
    answer(
      plain,
      "gh api -H Accept: application/vnd.github.raw repos/example/widgets/contents/docs/usage.md?ref=legion%2FACME-7-702"
    )
  ).toBe(USAGE_TEXT);
  // The base answers with no ref at all, and its file there is the base's own.
  expect(answer(plain, "gh api repos/example/widgets/contents/README.md")).toMatchObject({
    sha: README,
  });
  expect(
    answer(plain, `gh api repos/example/widgets/contents/README.md?ref=${H703}`)
  ).toMatchObject({
    sha: README703,
  });
  expect(answer(plain, "gh api repos/example/widgets/contents/docs?ref=legion/ACME-7-702")).toEqual(
    [
      {
        type: "file",
        size: Buffer.byteLength(USAGE_TEXT),
        name: "usage.md",
        path: "docs/usage.md",
        sha: USAGE,
        url: "https://api.github.com/repos/example/widgets/contents/docs/usage.md?ref=legion/ACME-7-702".replace(
          "legion/ACME-7-702",
          H702
        ),
      },
    ]
  );
  expect(answer(plain, `gh api repos/example/widgets/contents/?ref=${H702}`)).toMatchObject([
    { type: "file", name: "README.md" },
    { type: "dir", name: "docs", size: 0 },
  ]);
  const { match: _missing, ...missing } = routeOf(
    plain,
    `gh api repos/example/widgets/contents/no/such/file.md?ref=${H702}`
  );
  expect(missing).toEqual(
    notFound("https://docs.github.com/rest/repos/contents#get-repository-content")
  );
  expect(answer(plain, `gh api repos/example/widgets/git/trees/${H702}?recursive=1`)).toMatchObject(
    {
      sha: TREE702,
      truncated: false,
      tree: [
        { path: "README.md", type: "blob", sha: README, size: 30 },
        {
          path: "docs",
          mode: "040000",
          type: "tree",
          sha: DOCS_TREE,
          url: `https://api.github.com/repos/example/widgets/git/trees/${DOCS_TREE}`,
        },
        { path: "docs/usage.md", type: "blob", sha: USAGE },
      ],
    }
  );
  expect(answer(plain, "gh api repos/example/widgets/git/trees/legion/ACME-7-702")).toMatchObject({
    sha: TREE702,
    tree: [{ path: "README.md" }, { path: "docs" }],
  });
  expect(answer(plain, `gh api repos/example/widgets/git/trees/${DOCS_TREE}`)).toEqual({
    sha: DOCS_TREE,
    url: `https://api.github.com/repos/example/widgets/git/trees/${DOCS_TREE}`,
    truncated: false,
    tree: [
      {
        path: "usage.md",
        mode: "100644",
        type: "blob",
        sha: USAGE,
        url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
        size: Buffer.byteLength(USAGE_TEXT),
      },
    ],
  });
  expect(routeOf(plain, `gh api repos/example/widgets/git/trees/${"e".repeat(40)}`).status).toBe(
    404
  );
  expect(answer(plain, `gh api repos/example/widgets/git/blobs/${USAGE}`)).toEqual({
    sha: USAGE,
    node_id: "B_rig",
    size: Buffer.byteLength(USAGE_TEXT),
    url: `https://api.github.com/repos/example/widgets/git/blobs/${USAGE}`,
    content: base64(USAGE_TEXT),
    encoding: "base64",
  });
  expect(routeOf(plain, `gh api repos/example/widgets/git/blobs/${"e".repeat(40)}`).status).toBe(
    404
  );
  expect(answer(plain, "gh api repos/example/widgets/commits/legion/ACME-7-702")).toMatchObject({
    sha: H702,
    commit: {
      message: "docs: the usage page of the widgets CLI (ACME-7)\n",
      tree: { sha: TREE702 },
    },
    parents: [{ sha: MAIN }],
    files: [{ filename: "docs/usage.md", status: "added", changes: 3 }],
  });
  expect(answer(plain, "gh api repos/example/widgets/commits/main")).toMatchObject({
    sha: MAIN,
    parents: [],
    files: [],
  });
  expect(answer(plain, `gh api repos/example/widgets/git/commits/${H703}`)).toEqual({
    sha: H703,
    message: "docs: point the README at the usage page (ACME-7)\n",
    tree: { sha: TREE703 },
    parents: [{ sha: MAIN }],
  });
});

// Catches: a renderer whose command line reads no world, or takes a world that lacks a field
// for one the routes then answer with nothing.
test("heads-routes.ts reads the world on stdin, prints the table, and refuses a broken world", () => {
  const script = path.join(import.meta.dir, "heads-routes.ts");
  const run = (input: string) =>
    spawnWithin({ ...SCRIPT_RUN, part: "heads-routes.ts" }, [
      "bash",
      "-c",
      'printf %s "$1" | bun "$2"',
      "-",
      input,
      script,
    ]);
  const printed = run(JSON.stringify(world()));
  expect(printed.exitCode).toBe(0);
  expect(printed.stdout.toString()).toBe(`${JSON.stringify(plain, null, 2)}\n`);
  const { at: _at, ...undated } = world();
  const refused = run(JSON.stringify(undated));
  expect(refused.exitCode).not.toBe(0);
  expect(refused.stdout.toString()).toBe("");
  expect(refused.stderr.toString()).toMatch(/"path": \[\s*"at"\s*\]/);
});
