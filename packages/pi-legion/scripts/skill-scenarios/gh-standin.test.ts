import { expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import { githubFixtures } from "./case-world";
import { GH_FLAGS } from "./gh-standin";
import type { World } from "./heldout-case";

// Every row runs gh-standin.ts as a scenario's agent does, a process of its own, against a run
// directory holding the row's own fixtures.json.
const work = temporaryDirectory("gh-standin");
let runs = 0;

/** A new run directory whose fixtures.json holds `routes`. */
function runWith(routes: object[]): string {
  const run = path.join(work, `run-${++runs}`);
  mkdirSync(run);
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
  return run;
}

/** `gh <args>`, answered by the stand-in from the fixtures of `run`. */
function gh(run: string, ...args: string[]) {
  return ghWith(run, args);
}

/** `gh <args>` with `stdin` on its standard input. */
function ghWith(run: string, args: string[], stdin?: string) {
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    {
      env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: run },
      stdin: stdin === undefined ? "ignore" : new TextEncoder().encode(stdin),
    }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

// `--json` picks the fields it names of each object of a list answer, as `gh pr list --json` does,
// and of an object answer, and `--jq` reads the picked list.
test("--json picks the named fields of each object of a list answer and of an object answer", () => {
  const run = runWith([
    {
      match: "^gh pr list",
      stdout: [
        { number: 7, url: "https://example.invalid/7", body: "seven" },
        { number: 8, url: "https://example.invalid/8", body: "eight" },
      ],
    },
    {
      match: "^gh pr view",
      stdout: { number: 7, url: "https://example.invalid/7", body: "seven" },
    },
  ]);
  expect(gh(run, "pr", "list", "--json", "number,url")).toEqual({
    code: 0,
    stdout:
      '[{"number":7,"url":"https://example.invalid/7"},{"number":8,"url":"https://example.invalid/8"}]\n',
    stderr: "",
  });
  expect(gh(run, "pr", "list", "--json", "number", "--jq", ".[].number").stdout).toBe("7\n8\n");
  expect(gh(run, "pr", "view", "7", "--json", "url").stdout).toBe(
    '{"url":"https://example.invalid/7"}\n'
  );
});

/** A pull request of the case world (heldout-case.ts `World`). */
type WorldPull = NonNullable<World["github"]>["pullRequests"][number];

/** A pull request of the case world, open on `ref`. */
function worldPull(number: number, ref: string, body = "old"): WorldPull {
  return {
    number,
    state: "open" as const,
    draft: false,
    head: { ref, sha: "b".repeat(40) },
    base: "main",
    title: `T${number}`,
    body,
    mergeCommit: null,
    reviews: [],
    reviewThreads: [],
    checks: [],
  };
}

/** A run of the case world whose GitHub holds `pulls` on acme/widgets, as rig.sh builds it, and
 * whose issue is ACME-12 (its branch `legion/ACME-12`); `world: false` gives it no world.json. */
function worldRun(pulls: WorldPull[], more: object[] = [], world = true) {
  const run = runWith([
    ...githubFixtures(
      {
        repository: "acme/widgets",
        pullRequests: pulls,
        mergeQueue: [
          { pullRequest: 7, request: "enqueuePullRequest", answer: "queued", detail: "" },
          { pullRequest: 7, request: "dequeuePullRequest", answer: "removed", detail: "" },
        ],
      },
      "main",
      "ACME-12"
    ).routes,
    ...more,
  ]);
  if (world)
    writeFileSync(
      path.join(run, "world.json"),
      JSON.stringify({ dispatch: { issue: { key: "ACME-12" } } })
    );
  return run;
}

/** The body of each pull request a read answers with, by number. */
function bodies(stdout: string): Record<number, string> {
  const each = (value: unknown): [number, string][] =>
    Array.isArray(value)
      ? value.flatMap(each)
      : typeof value === "object" && value !== null && "number" in value && "body" in value
        ? [[Number(value.number), String(value.body)]]
        : [];
  return Object.fromEntries(each(JSON.parse(stdout)));
}

// A body edit is kept on the pull request it names alone, in every route that answers with it (its
// view, its REST read, each list, the issue it is), so GitHub's next read of it holds the new body
// and the next read of any other pull request its own. With no selector it is the issue branch's
// open pull request. One that names a pull request GitHub holds none of, or none where the branch
// has no open one, fails with gh's words and changes nothing.
test("a body edit changes the pull request it names alone, in each route that answers with it", () => {
  const issue = [
    { match: "^gh api .*repos/acme/widgets/issues/8(?: |$)", stdout: { number: 8, body: "old" } },
  ];
  const run = worldRun([worldPull(7, "legion/ACME-12"), worldPull(8, "legion/ACME-13")], issue);
  const reads = () => ({
    list: bodies(gh(run, "pr", "list", "--json", "number,body").stdout),
    seven: bodies(gh(run, "pr", "view", "7", "--json", "number,body").stdout),
    eight: bodies(gh(run, "api", "repos/acme/widgets/pulls/8").stdout),
    issue: bodies(gh(run, "api", "repos/acme/widgets/issues/8").stdout),
  });
  expect(gh(run, "pr", "edit", "8", "-b", "new eight").code).toBe(0);
  expect(reads()).toEqual({
    list: { 7: "old", 8: "new eight" },
    seven: { 7: "old" },
    eight: { 8: "new eight" },
    issue: { 8: "new eight" },
  });
  // No selector: the issue branch's open pull request, #7.
  expect(gh(run, "pr", "edit", "--body", "new seven").code).toBe(0);
  expect(reads().list).toEqual({ 7: "new seven", 8: "new eight" });
  expect(gh(run, "pr", "edit", "99", "-b", "x")).toEqual({
    code: 1,
    stdout: "",
    stderr:
      "GraphQL: Could not resolve to a PullRequest with the number of 99. (repository.pullRequest)\n",
  });
  expect(pullChanges(run)).toEqual([
    { action: "edit", numbers: [8], body: "new eight" },
    { action: "edit", numbers: [7], body: "new seven" },
  ]);
  const none = worldRun([worldPull(8, "legion/ACME-13")]);
  expect(gh(none, "pr", "edit", "-b", "x")).toEqual({
    code: 1,
    stdout: "",
    stderr: 'no pull requests found for branch "legion/ACME-12"\n',
  });
  expect(bodies(gh(none, "pr", "list", "--json", "number,body").stdout)).toEqual({ 8: "old" });
  expect(pullChanges(none)).toEqual([]);
});

// A review is kept on the one pull request the call names, or, where it names none, on the issue
// branch's one open pull request, however many routes answer with it. With no such pull request,
// or without exactly one of --approve, --request-changes and --comment (the last two with a body),
// the call fails with gh's words and keeps no review.
test("gh pr review saves its review on the one pull request it names, or on the branch's, or fails as gh does", () => {
  const run = worldRun([worldPull(7, "legion/ACME-12"), worldPull(8, "legion/ACME-13")]);
  const reviews = (number: number) =>
    JSON.parse(gh(run, "api", `repos/acme/widgets/pulls/${number}/reviews`).stdout).map(
      (each: { state: string; body: string }) => [each.state, each.body]
    );
  expect(gh(run, "pr", "review", "-a")).toMatchObject({ code: 0 });
  expect(gh(run, "pr", "review", "8", "-cb", "A note.")).toMatchObject({ code: 0 });
  expect(reviews(7)).toEqual([["APPROVED", ""]]);
  expect(reviews(8)).toEqual([["COMMENTED", "A note."]]);
  expect(JSON.parse(gh(run, "pr", "view", "7", "--json", "reviewDecision").stdout)).toEqual({
    reviewDecision: "APPROVED",
  });
  const usage = readFileSync(testdata("f9-gh-requests.json"), "utf8");
  const refusal = (argv: string[]): string => {
    const found = z
      .array(z.looseObject({ argv: z.array(z.string()), stderr: z.string() }))
      .parse(JSON.parse(usage))
      .find((each) => each.argv.join(" ") === argv.join(" "));
    if (found === undefined) throw new Error(`no captured refusal for gh ${argv.join(" ")}`);
    return found.stderr;
  };
  for (const argv of [
    ["pr", "review", "7"],
    ["pr", "review", "7", "-a", "-r"],
    ["pr", "review", "7", "-a", "-r", "-b", "x"],
    ["pr", "review", "7", "-c"],
  ]) {
    expect(gh(run, ...argv)).toEqual({ code: 1, stdout: "", stderr: refusal(argv) });
  }
  expect(pullChanges(run).map((change) => [change.action, change.number, change.state])).toEqual([
    ["review", 7, "APPROVED"],
    ["review", 8, "COMMENTED"],
  ]);
  const none = worldRun([worldPull(8, "legion/ACME-13")]);
  expect(gh(none, "pr", "review", "-a")).toEqual({
    code: 1,
    stdout: "",
    stderr: 'no pull requests found for branch "legion/ACME-12"\n',
  });
  expect(JSON.parse(gh(none, "api", "repos/acme/widgets/pulls/8/reviews").stdout)).toEqual([]);
  expect(pullChanges(none)).toEqual([]);
}, 60_000);

// A review submission answers as GitHub does: the POST with the review it created, and every later
// read of the pull request and of its review list holds that review, its decision following. A
// pull request body edit leaves the reviews' own bodies alone.
test("a review POST answers with the created review, and the pull request's reads then hold it", () => {
  const review = { id: 5, body: "Looks fine at A.", state: "APPROVED", commit_id: "a".repeat(40) };
  const view = {
    number: 73,
    url: "https://github.com/acme/widgets/pull/73",
    headRefName: "legion/ACME-37",
    headRefOid: "b".repeat(40),
    body: "old",
    reviews: [review],
    reviewDecision: "APPROVED",
  };
  const run = runWith([
    { match: "^gh pr view(?=.* 73(?: |$))", stdout: view },
    { match: "^gh api .*repos/acme/widgets/pulls/73/reviews(?: |$)", stdout: [review] },
    { match: "^gh api .*repos/acme/widgets/pulls/73(?: |$)", stdout: { ...view, state: "open" } },
    { match: "^gh pr (edit|review)(?=.* 73(?: |$))", stdout: view.url },
  ]);
  writeFileSync(path.join(run, "clock.txt"), "2030-01-07T12:00:00Z\n");
  writeFileSync(
    path.join(run, "clock-real.txt"),
    `${new Date().toISOString().replace("Z", "123456Z")}\n`
  );
  writeFileSync(
    path.join(run, "review.json"),
    JSON.stringify({ commit_id: "b".repeat(40), event: "REQUEST_CHANGES", body: "Changes at B." })
  );
  const posted = gh(
    run,
    "api",
    "-X",
    "POST",
    "repos/acme/widgets/pulls/73/reviews",
    "--input",
    path.join(run, "review.json"),
    "--jq",
    "{id,state,commit_id}"
  );
  expect(posted).toEqual({
    code: 0,
    stdout: `{"commit_id":"${"b".repeat(40)}","id":6,"state":"CHANGES_REQUESTED"}\n`,
    stderr: "",
  });
  const listed = JSON.parse(gh(run, "api", "repos/acme/widgets/pulls/73/reviews").stdout);
  expect(listed.map((each: { state: string }) => each.state)).toEqual([
    "APPROVED",
    "CHANGES_REQUESTED",
  ]);
  expect(listed[1].submitted_at.startsWith("2030-01-07T12:00:0")).toBe(true);
  expect(JSON.parse(gh(run, "pr", "view", "73", "--json", "reviewDecision").stdout)).toEqual({
    reviewDecision: "CHANGES_REQUESTED",
  });
  expect(gh(run, "pr", "review", "73", "--approve", "--body", "Fine at B.").code).toBe(0);
  const viewed = JSON.parse(gh(run, "api", "repos/acme/widgets/pulls/73").stdout);
  expect(
    viewed.reviews.map((each: { id: number; commit_id: string }) => [each.id, each.commit_id])
  ).toEqual([
    [5, "a".repeat(40)],
    [6, "b".repeat(40)],
    [7, "b".repeat(40)],
  ]);
  expect(viewed.reviewDecision).toBe("APPROVED");
  expect(gh(run, "pr", "edit", "73", "--body", "new").code).toBe(0);
  const after = JSON.parse(gh(run, "api", "repos/acme/widgets/pulls/73/reviews").stdout);
  expect(after.map((each: { body: string }) => each.body)).toEqual([
    "Looks fine at A.",
    "Changes at B.",
    "Fine at B.",
  ]);
  expect(JSON.parse(gh(run, "pr", "view", "73", "--json", "body").stdout)).toEqual({ body: "new" });
  // Each row names the change the call kept to a pull request, which lane D's replay makes again.
  expect(pullChanges(run)).toEqual([
    {
      action: "review",
      number: 73,
      state: "CHANGES_REQUESTED",
      commit: "b".repeat(40),
      body: "Changes at B.",
      submittedAt: listed[1].submitted_at,
    },
    {
      action: "review",
      number: 73,
      state: "APPROVED",
      commit: "b".repeat(40),
      body: "Fine at B.",
      submittedAt: viewed.reviews[2].submitted_at,
    },
    { action: "edit", numbers: [73], body: "new" },
  ]);
});

/** The pull request change each row of the run's calls.jsonl kept, in order, once every row is
 * held to say when its call ended, no earlier than it began, and a row that exited other than 0 to
 * keep none. */
function pullChanges(run: string): Record<string, unknown>[] {
  const Row = z.looseObject({
    at: z.string(),
    done: z.string(),
    exit: z.number(),
    pull: z.record(z.string(), z.unknown()).optional(),
  });
  const file = path.join(run, "calls.jsonl");
  const rows = existsSync(file)
    ? readFileSync(file, "utf8")
        .trimEnd()
        .split("\n")
        .map((line) => Row.parse(JSON.parse(line)))
    : [];
  for (const row of rows) {
    expect(Date.parse(row.done)).toBeGreaterThanOrEqual(Date.parse(row.at));
    if (row.exit !== 0) expect(row.pull).toBeUndefined();
  }
  return rows.flatMap((row) => (row.pull === undefined ? [] : [row.pull]));
}

// Paged reads. Each testdata/f7-* fixture is the output of one real `gh` 2.98.0 call (its
// `.command`, whose first line is the command, and its `.stdout`, `.stderr` and `.exit`) against
// a public pull request with two review threads, paged a thread at a time. Each row runs that
// command, as captured, against a route holding the pages GitHub sent, and holds the stand-in's
// answer to what gh printed.
const testdata = (name: string) => path.join(import.meta.dir, "testdata", name);
const captured = (name: string) => readFileSync(testdata(name), "utf8");

/** One page of a pull request's review threads, as GitHub's GraphQL answers it. */
interface Page {
  data: {
    repository: {
      pullRequest: {
        reviewThreads: { pageInfo: { hasNextPage: boolean; endCursor: string }; nodes: unknown[] };
      };
    };
  };
}

/** The pages GitHub sent: the slurped capture holds each one as it came. */
const threadPages = JSON.parse(captured("f7-paginate-slurp.stdout")) as Page[];
const pagedRoute = { match: "^gh api", pages: threadPages };

/** The captured fixture `name`'s command, run against the stand-in: a `gh` on its PATH answers as
 * gh-standin.ts does, so the command runs exactly as gh was given it. */
function runCaptured(run: string, name: string) {
  const bin = path.join(work, "bin");
  mkdirSync(bin, { recursive: true });
  writeFileSync(
    path.join(bin, "gh"),
    `#!/bin/sh\nexec ${JSON.stringify(process.execPath)} ${JSON.stringify(path.join(import.meta.dir, "gh-standin.ts"))} gh "$@"\n`,
    { mode: 0o755 }
  );
  const command = captured(`${name}.command`).split("\n")[0] ?? "";
  const result = Bun.spawnSync(["sh", "-c", command], {
    env: { PATH: `${bin}:${process.env.PATH ?? ""}`, SKILL_SCENARIO_RUN: run },
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

// `--paginate` prints each page as one JSON object with nothing between them, `--jq` runs its
// filter on each page (compact, keys sorted), and `--slurp` prints one array of the pages.
test("--paginate answers every page, plain, through --jq a page at a time, or slurped, as gh does", () => {
  const run = runWith([pagedRoute]);
  for (const name of ["f7-paginate", "f7-paginate-jq", "f7-paginate-slurp"]) {
    expect(runCaptured(run, name)).toEqual({
      code: Number(captured(`${name}.exit`)),
      stdout: captured(`${name}.stdout`),
      stderr: "",
    });
  }
  const slurped = JSON.parse(runCaptured(run, "f7-paginate-slurp").stdout) as typeof threadPages;
  expect(slurped).toHaveLength(2);
  expect(slurped.map((page) => Object.keys(page))).toEqual([["data"], ["data"]]);
  expect(
    slurped.map((page) => Object.keys(page.data.repository.pullRequest.reviewThreads))
  ).toEqual([
    ["pageInfo", "nodes"],
    ["pageInfo", "nodes"],
  ]);
  expect(
    slurped.map((page) => page.data.repository.pullRequest.reviewThreads.pageInfo.hasNextPage)
  ).toEqual([true, false]);
  const filtered = runCaptured(run, "f7-paginate-jq").stdout.trimEnd().split("\n");
  expect(filtered).toHaveLength(2);
  expect(filtered.map((line) => Object.keys(JSON.parse(line)))).toEqual([
    ["repository"],
    ["repository"],
  ]);
});

// gh refuses `--slurp` with a filter or a template, and without `--paginate`, with its sentence,
// its usage and exit status 1, and prints nothing else.
test("--slurp with --jq, or without --paginate, is refused with gh's own words and exit status", () => {
  const run = runWith([pagedRoute]);
  for (const name of ["f7-paginate-slurp-jq", "f7-slurp-only"]) {
    expect(runCaptured(run, name)).toEqual({
      code: Number(captured(`${name}.exit`)),
      stdout: "",
      stderr: captured(`${name}.stderr`),
    });
  }
});

// A REST list read in pages is one list, as `gh api --paginate` prints it.
test("--paginate joins pages that are lists into one list", () => {
  const labels = JSON.parse(captured("f7-rest-paginate.stdout")) as unknown[];
  const run = runWith([{ match: "^gh api", pages: labels.map((label) => [label]) }]);
  expect(runCaptured(run, "f7-rest-paginate")).toEqual({
    code: Number(captured("f7-rest-paginate.exit")),
    stdout: captured("f7-rest-paginate.stdout"),
    stderr: "",
  });
});

// Without --paginate the read after a cursor is the page after the one that ended at it: the first
// page where the call names none, and, past the last, a page with no nodes and no next page.
test("a read without --paginate answers the page after its cursor", () => {
  const run = runWith([pagedRoute]);
  const [first, last] = threadPages as [Page, Page];
  const cursor = (page: Page) => page.data.repository.pullRequest.reviewThreads.pageInfo.endCursor;
  const read = (...variables: string[]): Page =>
    JSON.parse(gh(run, "api", "graphql", "-f", "query=query{x}", ...variables).stdout);
  expect(read()).toEqual(first);
  expect(read("-F", `endCursor=${cursor(first)}`)).toEqual(last);
  const past = read("-F", `endCursor=${cursor(last)}`).data.repository.pullRequest.reviewThreads;
  expect(past.nodes).toEqual([]);
  expect(past.pageInfo).toEqual({ hasNextPage: false, endCursor: cursor(last) });
});

// The merger's READY world: a pull request open on a merge-queue base, clean, not in the queue.
// Its routes are testdata/f8-merge-queue-routes.json, which the rig's fixtures.json is given
// beside the pull request's own (a `gh pr view --json baseRefName` route is the test's).
test("the merge-queue routes answer the merger's reads and its enqueue, and record each call", () => {
  const { routes } = z
    .object({ routes: z.array(z.looseObject({})) })
    .parse(JSON.parse(captured("f8-merge-queue-routes.json")));
  const run = runWith([
    { match: "^gh pr view .*--json baseRefName", stdout: { baseRefName: "main" } },
    ...routes,
  ]);
  expect(
    gh(
      run,
      "pr",
      "view",
      "5676",
      "-R",
      "cybermonklabs/legion",
      "--json",
      "baseRefName",
      "-q",
      ".baseRefName"
    ).stdout
  ).toBe("main\n");
  expect(
    gh(
      run,
      "api",
      "repos/cybermonklabs/legion",
      "--jq",
      "[.default_branch, .allow_merge_commit, .squash_merge_commit_title, .squash_merge_commit_message] | @tsv"
    ).stdout
  ).toBe("main\tfalse\tPR_TITLE\tBLANK\n");
  expect(
    gh(
      run,
      "api",
      "--paginate",
      "repos/cybermonklabs/legion/rules/branches/main",
      "--jq",
      ".[].type"
    ).stdout
  ).toBe(
    "deletion\nnon_fast_forward\nrequired_linear_history\npull_request\nrequired_status_checks\nmerge_queue\ncode_scanning\n"
  );

  const id = gh(
    run,
    "pr",
    "view",
    "5676",
    "-R",
    "cybermonklabs/legion",
    "--json",
    "id",
    "-q",
    ".id"
  ).stdout.trim();
  expect(id).toBe("PR_kwDORFy7ds8AAAABSFTGIw");
  const enqueue = gh(
    run,
    "api",
    "graphql",
    "-f",
    "query=mutation($pr:ID!,$head:GitObjectID!){enqueuePullRequest(input:{pullRequestId:$pr,expectedHeadOid:$head}){mergeQueueEntry{position}}}",
    "-f",
    `pr=${id}`,
    "-f",
    "head=c9fd01e13d79d5c8ba66b3f3280ee5893a01ec6f"
  );
  expect(JSON.parse(enqueue.stdout)).toEqual({
    data: { enqueuePullRequest: { mergeQueueEntry: { position: 1 } } },
  });

  const watch = gh(
    run,
    "api",
    "graphql",
    "-F",
    "number=5676",
    "-f",
    "owner=cybermonklabs",
    "-f",
    "name=legion",
    "-f",
    "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){state mergedAt isInMergeQueue mergeStateStatus headRefOid mergeQueueEntry{state position}}}}"
  );
  expect(JSON.parse(watch.stdout).data.repository.pullRequest).toEqual({
    state: "OPEN",
    mergedAt: null,
    isInMergeQueue: false,
    mergeStateStatus: "CLEAN",
    headRefOid: "c9fd01e13d79d5c8ba66b3f3280ee5893a01ec6f",
    mergeQueueEntry: null,
  });
  const timeline = gh(
    run,
    "api",
    "graphql",
    "-F",
    "number=5676",
    "-f",
    "owner=cybermonklabs",
    "-f",
    "name=legion",
    "-f",
    "query=query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){timelineItems(itemTypes:[ADDED_TO_MERGE_QUEUE_EVENT,REMOVED_FROM_MERGE_QUEUE_EVENT],last:20){nodes{__typename}}}}}"
  );
  expect(JSON.parse(timeline.stdout).data.repository.pullRequest.timelineItems.nodes).toEqual([]);

  const calls = readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line) as { route: string | null; exit: number });
  expect(calls.map((call) => call.exit)).toEqual([0, 0, 0, 0, 0, 0, 0]);
  expect(calls[4]?.route).toBe("^gh api graphql .*enqueuePullRequest");
});

/** A commit pushed to branch `branch` of the run's remote (git-world.ts's `remote.git`), made in a
 * clone of the row's own; its sha. */
function pushed(run: string, branch: string): string {
  const remote = path.join(run, "remote.git");
  const clone = path.join(run, "clone");
  const env = {
    PATH: process.env.PATH ?? "",
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_AUTHOR_NAME: "Scenario Worker",
    GIT_AUTHOR_EMAIL: "worker@example.invalid",
    GIT_COMMITTER_NAME: "Scenario Worker",
    GIT_COMMITTER_EMAIL: "worker@example.invalid",
  };
  const git = (cwd: string, ...args: string[]) => {
    const result = Bun.spawnSync(["git", ...args], { cwd, env });
    if (result.exitCode !== 0) throw new Error(result.stderr.toString());
    return result.stdout.toString().trim();
  };
  if (!existsSync(remote)) {
    git(run, "init", "-q", "--bare", remote);
    git(run, "init", "-q", clone);
  }
  git(clone, "commit", "-q", "--allow-empty", "-m", `on ${branch}`);
  git(clone, "push", "-q", remote, `HEAD:refs/heads/${branch}`);
  return git(clone, "rev-parse", "HEAD");
}

// A pull request the role opens is kept as GitHub keeps it: every read of it answers with it, by
// number, by branch, with no selector on the issue branch, in the open lists and over REST; its
// head follows each push to its branch; a body edit reaches it. GitHub's refusals hold: a branch
// the remote does not hold, and a second pull request of a branch into the same base. A REST
// creation of another branch takes the next number and is answered whole.
test("a pull request the role opens is kept, follows its branch, and GitHub's refusals hold", () => {
  const run = runWith(
    githubFixtures(
      { repository: "acme/widgets", pullRequests: [], mergeQueue: [] },
      "main",
      "ACME-12"
    ).routes
  );
  writeFileSync(
    path.join(run, "world.json"),
    JSON.stringify({ dispatch: { issue: { key: "ACME-12" } } })
  );
  pushed(run, "main");
  expect(gh(run, "pr", "create", "--base", "main", "--title", "T", "--body", "B")).toEqual({
    code: 1,
    stdout: "",
    stderr:
      "pull request create failed: GraphQL: Head sha can't be blank, Base sha can't be blank, No commits between main and legion/ACME-12, Head ref must be a branch (createPullRequest)\n",
  });
  const first = pushed(run, "legion/ACME-12");
  expect(gh(run, "pr", "create", "--base", "main", "--title", "T", "--body", "B")).toEqual({
    code: 0,
    stdout: "https://github.com/acme/widgets/pull/1\n",
    stderr: "",
  });
  expect(
    JSON.parse(
      gh(run, "pr", "view", "1", "--json", "number,state,headRefName,headRefOid,title,body").stdout
    )
  ).toEqual({
    number: 1,
    state: "OPEN",
    headRefName: "legion/ACME-12",
    headRefOid: first,
    title: "T",
    body: "B",
  });
  expect(gh(run, "pr", "view", "--json", "number").stdout).toBe('{"number":1}\n');
  expect(gh(run, "pr", "list", "--head", "legion/ACME-12", "--json", "number").stdout).toBe(
    '[{"number":1}]\n'
  );
  expect(gh(run, "pr", "list", "--json", "number").stdout).toBe('[{"number":1}]\n');
  expect(gh(run, "pr", "list", "--state", "all", "--json", "number").stdout).toBe(
    '[{"number":1}]\n'
  );
  expect(gh(run, "pr", "list", "--state", "closed", "--json", "number").stdout).toBe("[]\n");
  expect(gh(run, "api", "repos/acme/widgets/pulls", "--jq", ".[].number").stdout).toBe("1\n");
  expect(gh(run, "api", "repos/acme/widgets/pulls/1", "--jq", ".state").stdout).toBe("open\n");
  expect(gh(run, "api", "repos/acme/widgets/pulls/1/reviews").stdout).toBe("[]\n");
  expect(gh(run, "pr", "create", "--head", "legion/ACME-12", "--title", "again")).toEqual({
    code: 1,
    stdout: "",
    stderr:
      'a pull request for branch "legion/ACME-12" into branch "main" already exists:\nhttps://github.com/acme/widgets/pull/1\n',
  });
  const second = pushed(run, "legion/ACME-12");
  expect(gh(run, "pr", "view", "legion/ACME-12", "--json", "headRefOid").stdout).toBe(
    `{"headRefOid":"${second}"}\n`
  );
  expect(gh(run, "api", "repos/acme/widgets/pulls/1", "--jq", ".head.sha").stdout).toBe(
    `${second}\n`
  );
  expect(gh(run, "pr", "list", "--json", "headRefOid", "--jq", ".[].headRefOid").stdout).toBe(
    `${second}\n`
  );
  expect(gh(run, "pr", "edit", "1", "--body", "new").code).toBe(0);
  expect(gh(run, "pr", "view", "1", "--json", "body").stdout).toBe('{"body":"new"}\n');

  const third = pushed(run, "legion/ACME-13");
  const posted = gh(
    run,
    "api",
    "-X",
    "POST",
    "repos/acme/widgets/pulls",
    "-f",
    "head=acme:legion/ACME-13",
    "-f",
    "base=main",
    "-f",
    "title=U",
    "--jq",
    "{number, state, head: .head.ref}"
  );
  expect(posted).toEqual({
    code: 0,
    stdout: '{"head":"legion/ACME-13","number":2,"state":"open"}\n',
    stderr: "",
  });
  expect(gh(run, "pr", "view", "2", "--json", "title").stdout).toBe('{"title":"U"}\n');
  expect(gh(run, "pr", "list", "--json", "number", "--jq", ".[].number").stdout).toBe("1\n2\n");
  const refused = gh(
    run,
    "api",
    "-X",
    "POST",
    "repos/acme/widgets/pulls",
    "-f",
    "head=legion/ACME-13",
    "-f",
    "base=main"
  );
  expect([refused.code, refused.stderr, JSON.parse(refused.stdout).errors]).toEqual([
    1,
    "gh: Validation Failed (HTTP 422)\n",
    [
      {
        resource: "PullRequest",
        code: "custom",
        message: "A pull request already exists for acme:legion/ACME-13.",
      },
    ],
  ]);
  const exits = readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => z.object({ exit: z.number() }).parse(JSON.parse(line)).exit);
  // The three refusals: the branch the remote did not hold, and the two second pull requests.
  expect(exits.filter((exit) => exit !== 0)).toHaveLength(3);
  // The opened pull requests, each at the head its branch had then, and the body edit; no refusal
  // kept a change.
  expect(pullChanges(run)).toEqual([
    {
      action: "open",
      number: 1,
      head: "legion/ACME-12",
      base: "main",
      sha: first,
      title: "T",
      body: "B",
      draft: false,
    },
    { action: "edit", numbers: [1], body: "new" },
    {
      action: "open",
      number: 2,
      head: "legion/ACME-13",
      base: "main",
      sha: third,
      title: "U",
      body: "",
      draft: false,
    },
  ]);
}, 60_000);

// Help and the version reach no GitHub: the real gh, the first executable gh on PATH outside the
// run's bin directory that is no script (a wrapper such as `legion gh`'s is skipped), answers them,
// and they go to gh-local.jsonl, so `gh pr create --help` opens no pull request and is no call of
// the record. The row's real gh is a copy of /bin/echo, a binary that prints its arguments. Help
// is a flag as gh reads the arguments, never a flag's value: `--body -h` opens a pull request whose
// body is `-h`, and that call is the record's.
test("help and the version are the real gh's answers and no call of the record", () => {
  const run = runWith(
    githubFixtures(
      { repository: "acme/widgets", pullRequests: [], mergeQueue: [] },
      "main",
      "ACME-12"
    ).routes
  );
  writeFileSync(
    path.join(run, "world.json"),
    JSON.stringify({ dispatch: { issue: { key: "ACME-12" } } })
  );
  const head = pushed(run, "legion/ACME-12");
  const standin = path.join(import.meta.dir, "gh-standin.ts");
  const own = path.join(run, "bin");
  const wrapper = path.join(run, "wrapper");
  const real = path.join(run, "real");
  for (const dir of [own, wrapper, real]) mkdirSync(dir);
  writeFileSync(path.join(own, "gh"), `#!/bin/sh\nexec ${process.execPath} ${standin} gh "$@"\n`, {
    mode: 0o755,
  });
  writeFileSync(path.join(wrapper, "gh"), '#!/bin/sh\necho "wrapper: $*"\nexit 2\n', {
    mode: 0o755,
  });
  copyFileSync("/bin/echo", path.join(real, "gh"));
  chmodSync(path.join(real, "gh"), 0o755);
  const local = (...args: string[]) => {
    const result = Bun.spawnSync([process.execPath, standin, "gh", ...args], {
      env: {
        PATH: `${own}:${wrapper}:${real}:${process.env.PATH ?? ""}`,
        SKILL_SCENARIO_RUN: run,
      },
    });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
    };
  };
  expect(local("pr", "create", "--help")).toEqual({
    code: 0,
    stdout: "pr create --help\n",
    stderr: "",
  });
  expect(local("pr", "create", "-h")).toEqual({ code: 0, stdout: "pr create -h\n", stderr: "" });
  expect(local("version")).toEqual({ code: 0, stdout: "version\n", stderr: "" });
  expect(existsSync(path.join(run, "calls.jsonl"))).toBe(false);
  expect(local("pr", "view", "1").code).toBe(1);
  expect(local("pr", "create", "--title", "T", "--body", "-h")).toEqual({
    code: 0,
    stdout: "https://github.com/acme/widgets/pull/1\n",
    stderr: "",
  });
  expect(pullChanges(run)).toEqual([
    {
      action: "open",
      number: 1,
      head: "legion/ACME-12",
      base: "main",
      sha: head,
      title: "T",
      body: "-h",
      draft: false,
    },
  ]);
  const LocalCall = z.object({ argv: z.array(z.string()), gh: z.string(), exit: z.number() });
  expect(
    readFileSync(path.join(run, "gh-local.jsonl"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => LocalCall.parse(JSON.parse(line)))
      .map(({ argv, gh, exit }) => [argv.join(" "), gh, exit])
  ).toEqual([
    ["pr create --help", path.join(real, "gh"), 0],
    ["pr create -h", path.join(real, "gh"), 0],
    ["version", path.join(real, "gh"), 0],
  ]);
}, 60_000);

// The stand-in reads each command's flags as gh 2.98.0's help lists them (testdata/f9-gh-help.json,
// captured as f9-gh.command says): each flag, its one-letter form, and whether it takes a value.
test("the flags the stand-in reads are the ones gh 2.98.0's help lists for each command", () => {
  const help = z.record(z.string(), z.string()).parse(JSON.parse(captured("f9-gh-help.json")));
  const listed = Object.fromEntries(
    Object.entries(help).map(([command, text]) => {
      let section = "";
      const flags: string[] = [];
      for (const line of text.split("\n")) {
        if (/^[A-Z][A-Z ]+$/.test(line)) section = line;
        const flag = /^\s+(?:-(\w), )?--([\w-]+)(?: (\S+))?\s{2,}/.exec(line);
        if (flag && (section === "FLAGS" || section === "INHERITED FLAGS"))
          flags.push(`${flag[1] ? `${flag[1]} ` : ""}${flag[2]}${flag[3] ? "=" : ""}`);
      }
      return [command, flags];
    })
  );
  expect(GH_FLAGS).toEqual(listed);
});

/** One call of the real gh in testdata/f9-gh-requests.json, and the requests it sent. */
const Capture = z.object({
  argv: z.array(z.string()),
  stdin: z.string().optional(),
  branch: z.enum(["one", "none", "detached"]),
  exit: z.number(),
  stderr: z.string(),
  requests: z.array(
    z.object({
      method: z.string(),
      path: z.string(),
      body: z.unknown().optional(),
      operation: z.string().optional(),
      variables: z.record(z.string(), z.unknown()).nullish(),
    })
  ),
});
type Capture = z.infer<typeof Capture>;
const Fields = z.record(z.string(), z.unknown());

/** The change gh asked GitHub to make for `capture`, in the words of gh-standin's `pull`, or
 * undefined where it asked none. */
function changeAsked(capture: Capture): Record<string, unknown> | undefined {
  const variables = (operation: string) =>
    capture.requests.find((request) => request.operation === `mutation ${operation}`)?.variables;
  const input = (operation: string) => Fields.safeParse(variables(operation)?.input).data;
  const number = (id: unknown) => Number(/^PR_node(\d+)$/.exec(String(id))?.[1]);
  const review = input("PullRequestReviewAdd");
  if (review !== undefined) {
    const states: Record<string, string> = {
      APPROVE: "APPROVED",
      REQUEST_CHANGES: "CHANGES_REQUESTED",
      COMMENT: "COMMENTED",
    };
    const event = String(review.event);
    return {
      action: "review",
      number: number(review.pullRequestId),
      state: states[event],
      body: review.body,
    };
  }
  const create = input("PullRequestCreate");
  if (create !== undefined)
    return {
      action: "open",
      head: create.headRefName,
      base: create.baseRefName,
      title: create.title,
      body: create.body,
      draft: create.draft,
    };
  const update = input("PullRequestUpdate");
  if (update !== undefined)
    return {
      action: "edit",
      numbers: [number(update.pullRequestId)],
      body: update.body,
      ...(update.title === undefined ? {} : { title: update.title }),
    };
  const merge = input("PullRequestMerge");
  if (merge !== undefined)
    return {
      action: "merge",
      number: number(merge.pullRequestId),
      method: String(merge.mergeMethod).toLowerCase(),
    };
  const disable = variables("PullRequestAutoMergeDisable");
  if (disable) return { action: "disable-auto", number: number(disable.prID) };
  const comment = input("CommentCreate");
  const close = input("PullRequestClose");
  if (close !== undefined)
    return {
      action: "close",
      number: number(close.pullRequestId),
      ...(comment === undefined ? {} : { comment: comment.body }),
    };
  const draft = input("ConvertPullRequestToDraft");
  if (draft !== undefined) return { action: "draft", number: number(draft.pullRequestId) };
  if (comment !== undefined)
    return { action: "comment", number: number(comment.subjectId), body: comment.body };
  const [sent] = capture.requests;
  const body = Fields.safeParse(sent?.body).data ?? {};
  if (sent?.path === "/graphql" && /^mutation/.test(String(body.query))) {
    const name = /\{(\w+)\(/.exec(String(body.query))?.[1];
    const named: Record<string, string> = {
      enqueuePullRequest: "enqueue",
      dequeuePullRequest: "dequeue",
    };
    return { action: named[name ?? ""], id: Fields.parse(body.variables).pr };
  }
  if (sent === undefined || sent.method === "GET" || sent.path === "/graphql") return undefined;
  const rest = `${sent.method} ${sent.path}`;
  const asked: [RegExp, Record<string, unknown>][] = [
    [
      /^POST \/repos\/acme\/widgets\/pulls$/,
      { action: "open", head: "legion/ACME-13", base: body.base },
    ],
    [
      /^POST .*\/pulls\/7\/reviews$/,
      { action: "review", number: 7, state: "APPROVED", body: body.body },
    ],
    [
      /^PATCH .*\/pulls\/7$/,
      body.state === "closed"
        ? { action: "close", number: 7 }
        : { action: "edit", numbers: [7], body: body.body },
    ],
    [/^PUT .*\/pulls\/7\/merge$/, { action: "merge", number: 7, method: body.merge_method }],
    [/^POST .*\/pulls\/7\/comments$/, { action: "review-comment", number: 7, body: body.body }],
    [/^POST .*\/issues\/7\/comments$/, { action: "comment", number: 7, body: body.body }],
    [/^POST .*\/pulls\/7$/, { action: "request", number: 7, method: "POST" }],
  ];
  return asked.find(([pattern]) => pattern.test(rest))?.[1];
}

// gh's arguments are read as gh 2.98.0 reads them, on calls of the real gh (testdata/f9-gh-requests.json,
// f9-gh.command): each row runs the call against a world that holds the pull requests gh's GitHub
// held (#7 open on the issue's branch, legion/ACME-12, and #77; for a row whose branch is "none", #77
// alone; for "detached", no issue branch). The stand-in records the request gh sent for a `gh api`
// call (its method, path and fields, `-H` a header and no head) and the pull request a `gh pr` call
// read; it keeps the change gh asked GitHub to make, as gh asked it; and it refuses each call gh
// refused before it asked anything, with gh's own words.
test("each captured call of the real gh is read, kept and refused as gh did", () => {
  const Row = z.looseObject({
    request: z.object({
      method: z.string().optional(),
      path: z.string().optional(),
      fields: Fields,
    }),
    pull: Fields.optional(),
  });
  const captures = z.array(Capture).parse(JSON.parse(captured("f9-gh-requests.json")));
  expect(captures.length).toBeGreaterThan(40);
  for (const capture of captures) {
    const name = `gh ${capture.argv.join(" ")} (${capture.branch})`;
    const run = worldRun(
      capture.branch === "none"
        ? [worldPull(77, "legion/ACME-21")]
        : [worldPull(7, "legion/ACME-12"), worldPull(77, "legion/ACME-21")],
      [{ match: "^gh ", stdout: "" }],
      capture.branch !== "detached"
    );
    // A pull request opens on a branch the run's remote holds.
    if (capture.argv[1] === "create" || capture.argv[1] === "repos/acme/widgets/pulls")
      pushed(run, "legion/ACME-13");
    const answered = ghWith(run, capture.argv, capture.stdin);
    const [line = "{}"] = readFileSync(path.join(run, "calls.jsonl"), "utf8").trimEnd().split("\n");
    const row = Row.parse(JSON.parse(line));
    const asked = changeAsked(capture);
    const [first] = capture.requests;
    if (capture.argv[0] === "api" && first !== undefined) {
      const body = Fields.safeParse(first.body).data ?? {};
      expect({ name, method: row.request.method, path: row.request.path }).toEqual({
        name,
        method: first.method,
        path: first.path.replace(/^\//, "").replace(/\?.*$/, ""),
      });
      if (first.method !== "GET")
        expect({ name, fields: row.request.fields }).toMatchObject({
          name,
          fields:
            first.path === "/graphql"
              ? { query: body.query, ...Fields.parse(body.variables ?? {}) }
              : body,
        });
      expect(row.request.fields).not.toHaveProperty("head", "Accept: application/vnd.github+json");
    }
    const byNumber = capture.requests.find(
      (request) => request.operation === "query PullRequestByNumber"
    );
    if (byNumber !== undefined)
      expect({ name, number: row.request.fields.number }).toEqual({
        name,
        number: byNumber.variables?.pr_number,
      });
    const listed = capture.requests.find(
      (request) => request.operation === "fragment pr on PullRequest"
    );
    if (listed?.variables?.headBranch !== undefined)
      expect({ name, head: row.request.fields.head }).toEqual({
        name,
        head: listed.variables.headBranch,
      });
    if (asked !== undefined) {
      expect({ name, pull: row.pull }).toMatchObject({ name, pull: asked });
    } else if (capture.exit === 1) {
      expect({ name, ...answered, pull: row.pull }).toEqual({
        name,
        code: 1,
        stdout: "",
        stderr: capture.stderr,
        pull: undefined,
      });
    } else if (capture.argv.slice(0, 2).join(" ") === "pr reopen") {
      // gh read #7 open and asked nothing; the stand-in holds no pull request's state across a
      // close, so it takes the reopen.
      expect(row.pull).toEqual({ action: "reopen", number: 7 });
    } else expect({ name, pull: row.pull }).toEqual({ name, pull: undefined });
  }
}, 300_000);

// A call the stand-in refused keeps no change and leaves every answer as it was: an enqueue the
// merge queue refuses, and a body edit whose --jq filter fails.
test("a refused call keeps no change and changes no answer", () => {
  const run = runWith(
    githubFixtures(
      {
        repository: "acme/widgets",
        pullRequests: [worldPull(7, "legion/ACME-12")],
        mergeQueue: [
          { pullRequest: 7, request: "enqueuePullRequest", answer: "refused", detail: "refused" },
        ],
      },
      "main",
      "ACME-12"
    ).routes
  );
  const enqueue =
    "query=mutation($pr:ID!){enqueuePullRequest(input:{pullRequestId:$pr}){mergeQueueEntry{position}}}";
  expect(gh(run, "api", "graphql", "-f", enqueue, "-f", "pr=PR_7").code).toBe(1);
  const edit = ["api", "-X", "PATCH", "repos/acme/widgets/pulls/7", "-f", "body=new"];
  expect(gh(run, ...edit, "--jq", ".[").code).toBe(1);
  expect(gh(run, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"old"}\n');
  expect(pullChanges(run)).toEqual([]);
  expect(gh(run, ...edit).code).toBe(0);
  expect(gh(run, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"new"}\n');
  expect(pullChanges(run)).toEqual([{ action: "edit", numbers: [7], body: "new" }]);
});
