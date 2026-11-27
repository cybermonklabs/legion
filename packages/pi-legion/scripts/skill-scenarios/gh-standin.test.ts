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
  const result = Bun.spawnSync(
    [process.execPath, path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    { env: { PATH: process.env.PATH ?? "", SKILL_SCENARIO_RUN: run } }
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

// A pull request body edit reaches every object that has a body, in a list answer as in an object
// answer, so the next `gh pr list --json body` reads the new body, as GitHub's would; a list whose
// objects have no body is answered as it was.
test("a body edit gives each object of a list answer that has a body the new body", () => {
  const run = runWith([
    { match: "^gh pr edit", stdout: "https://example.invalid/7" },
    { match: "^gh pr list", stdout: [{ number: 7, body: "old" }] },
    { match: "^gh pr view", stdout: { number: 7, body: "old" } },
    { match: "^gh api .*comments", stdout: [{ id: 1 }] },
  ]);
  expect(gh(run, "pr", "edit", "7", "--body", "new").code).toBe(0);
  expect(gh(run, "pr", "list", "--json", "number,body").stdout).toBe(
    '[{"number":7,"body":"new"}]\n'
  );
  expect(gh(run, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"new"}\n');
  expect(gh(run, "api", "repos/example/widgets/issues/7/comments").stdout).toBe('[{"id":1}]\n');
  const [edit] = readFileSync(path.join(run, "calls.jsonl"), "utf8").trimEnd().split("\n");
  expect(JSON.parse(edit ?? "{}")).toMatchObject({
    argv: ["pr", "edit", "7", "--body", "new"],
    body: "new",
    exit: 0,
  });
});

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
 * held to say when its call ended, no earlier than it began. */
function pullChanges(run: string): unknown[] {
  const Row = z.looseObject({
    at: z.string(),
    done: z.string(),
    exit: z.number(),
    pull: z.record(z.string(), z.unknown()).optional(),
  });
  const rows = readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => Row.parse(JSON.parse(line)));
  for (const row of rows) expect(Date.parse(row.done)).toBeGreaterThanOrEqual(Date.parse(row.at));
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
// the record. The row's real gh is a copy of /bin/echo, a binary that prints its arguments.
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
  pushed(run, "legion/ACME-12");
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
  expect(local("version")).toEqual({ code: 0, stdout: "version\n", stderr: "" });
  expect(existsSync(path.join(run, "calls.jsonl"))).toBe(false);
  const LocalCall = z.object({ argv: z.array(z.string()), gh: z.string(), exit: z.number() });
  expect(
    readFileSync(path.join(run, "gh-local.jsonl"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => LocalCall.parse(JSON.parse(line)))
      .map(({ argv, gh, exit }) => [argv.join(" "), gh, exit])
  ).toEqual([
    ["pr create --help", path.join(real, "gh"), 0],
    ["version", path.join(real, "gh"), 0],
  ]);
  expect(local("pr", "view", "1").code).toBe(1);
}, 60_000);
