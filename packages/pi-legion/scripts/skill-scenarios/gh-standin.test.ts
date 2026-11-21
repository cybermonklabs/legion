import { afterEach, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { answer, Fixtures, parseCall, withoutServices } from "./gh-standin";

const standin = path.join(import.meta.dir, "gh-standin.ts");
const runDirectory = temporaryDirectories("skill-scenarios-gh-standin");
const stops: Array<() => void> = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** The fields this file reads of a call the stand-in recorded to calls.jsonl. */
interface Call {
  argv: string[];
  /** The call as the routes read it, where the stand-in resolved one. */
  request?: string;
  route: string | null;
  /** The status the call exited with; null for a `watch` reply, recorded before it waits. */
  exit: number | null;
  unmodeled?: true;
  published?: { status: number; body: string } | { error: string };
}

/** A run directory of a test's own, holding the routes the stand-in answers from, for a row that
 * tests the process itself: the lock, what it prints and exits with, its record, its write of
 * fixtures.json and its publish. The environment it reads is this process's without its
 * Dispatch, Envoy and NATS variables, which a Legion pane exports for the live services (the
 * stand-in's own filter), and then the world's own (`env`, which a row may add to). A row that
 * publishes names its listener and its token file in the run's pane.env (`paneEnv`), as rig.sh
 * does, and a row that names none publishes nowhere. Each call is spawned rather than run in this
 * process: the stand-in is a process per call, and a test serving its publish needs its own loop
 * free while the call runs. */
function world(routes: unknown[]) {
  const run = runDirectory();
  const env: Record<string, string> = { SKILL_SCENARIO_RUN: run };
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
  /** One run of the stand-in with `argv`: a `gh` call, or the rig's `--check` of the file. */
  async function standinRun(...argv: string[]) {
    const child = Bun.spawn(["bun", standin, ...argv], {
      env: { ...withoutServices(process.env), ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [stdout, stderr, exit] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    return { stdout, stderr, exit };
  }
  return {
    run,
    env,
    /** One `gh` call through the stand-in, as the scenario's PATH makes it. */
    gh: (...args: string[]) => standinRun("gh", ...args),
    /** The parse rig.sh's worker_world runs over fixtures.json before any agent boots. */
    check: () => standinRun("--check"),
    /** Whether the stand-in recorded any call at all. */
    recorded: () => existsSync(path.join(run, "calls.jsonl")),
    calls(): Call[] {
      return readFileSync(path.join(run, "calls.jsonl"), "utf8")
        .split("\n")
        .filter((line) => line !== "")
        .map((line) => JSON.parse(line) as Call);
    },
    fixtures(): { routes: { match: string; answered?: number }[] } {
      return JSON.parse(readFileSync(path.join(run, "fixtures.json"), "utf8"));
    },
  };
}

/** What a call through `answering` gives: what it prints and exits with, whether the rig refused it
 * as unmodeled, the request the routes read and the route that answered or refused it. */
interface Answering {
  stdout: string;
  stderr: string;
  exit: number;
  refused: boolean;
  request?: string;
  route: string | null;
}

/** The routes a row answers from, through the stand-in's own parseCall and answer in this process,
 * for a row that tests how a call is read and answered: each call reads the files and the stdin
 * (`-`) it names from `files`, by the name the call gives, and the next call answers from the
 * fixtures this one left, as the process writes them back. A call gh refuses before any request
 * prints its words and exits 1; one the rig does not model is `refused`, as the process records
 * it (unmodeled). */
function answering(
  routes: unknown[],
  env: Record<string, string> = {},
  files: Record<string, string> = {}
) {
  let fixtures = Fixtures.parse({ routes });
  async function gh(...args: string[]): Promise<Answering> {
    const parsed = await parseCall(args, env, async (source) => {
      const text = files[source];
      if (text === undefined) throw new Error(`the row names no file ${source}`);
      return text;
    });
    if (parsed.kind === "gh")
      return { stdout: "", stderr: parsed.stderr, exit: 1, refused: false, route: null };
    if (parsed.kind === "refused")
      return {
        stdout: "",
        stderr: "",
        exit: 1,
        refused: true,
        request: parsed.request,
        route: null,
      };
    const { request } = parsed.call;
    const answered = answer(parsed.call, fixtures, new Date());
    if (answered.refused)
      return { stdout: "", stderr: "", exit: 1, refused: true, request, route: answered.route };
    fixtures = answered.fixtures;
    const { stdout, stderr, status, route } = answered;
    return { stdout, stderr, exit: status, refused: false, request, route };
  }
  return { gh, fixtures: () => fixtures };
}

/** A listener of the test's own: it records every request that reached it and answers `status`
 * with `body`. */
function listener(status = 200, body: unknown = { ok: true }) {
  const received: {
    method: string;
    pathname: string;
    authorization: string | null;
    body: unknown;
  }[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      received.push({
        method: request.method,
        pathname: new URL(request.url).pathname,
        authorization: request.headers.get("authorization"),
        body: await request.json(),
      });
      return Response.json(body, { status });
    },
  });
  const stop = () => server.stop(true);
  stops.push(stop);
  return { url: `http://127.0.0.1:${server.port}`, received, stop };
}

/** The token file the stand-in reads the bearer from, as rig.sh's services write it. */
function tokenFile(run: string, token: string): string {
  const file = path.join(run, "envoy-token");
  writeFileSync(file, `${token}\n`);
  return file;
}

/** Names `url` as the run's own listener, with the bearer `token`, in <run>/pane.env: the file
 * rig.sh writes the agent's environment to, which the stand-in reads its listener from. */
function paneEnv(run: string, url: string, token: string) {
  writeFileSync(
    path.join(run, "pane.env"),
    `ENVOY_URL=${url}\nENVOY_TOKEN_FILE=${tokenFile(run, token)}\n`
  );
}

/** The head's checks before a re-run and after it: the one row a re-run world turns on. */
const RED = { stdout: "test\tfail\t20s\thttps://example.invalid/2", exit: 1 };
const GREEN = { stdout: "test\tpass\t20s\thttps://example.invalid/2" };
const RERUN = "✓ Requested rerun of failed jobs";

const HEAD = "9f1a2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3";
/** The settlement a re-run's route publishes: a cistore.Summary of the green head. */
const SUMMARY = {
  kind: "checks",
  repo: "example/widgets",
  number: "1000",
  sha: HEAD,
  check_runs: [
    { name: "lint", id: 920_001 },
    { name: "test", id: 920_002 },
  ],
  generation: 2,
  snapshot: "f3a1c0d2",
  failed: { count: 0, checks: [] },
  running: { count: 0, checks: [] },
  passed: { count: 2, checks: ["lint", "test"] },
  queued: { count: 0, checks: [] },
  cancelled: { count: 0, checks: [] },
  skipped: { count: 0, checks: [] },
  failing_checks: [],
};
/** The envelope a fixture writes for that settlement: every field of it is the fixture's own. */
const ENVELOPE = {
  topic: "notifications.github.example.widgets.pr.1000.checks",
  source: "github",
  message: `checks settled on example/widgets#1000 @ ${HEAD.slice(0, 7)}: 2 passed, 0 failed, 0 cancelled, 0 skipped`,
  payload: JSON.stringify(SUMMARY),
};

test("a route's replies answer the calls matching it in order", async () => {
  const { gh } = answering([{ match: "^gh pr checks", replies: [RED, GREEN] }]);

  const red = await gh("pr", "checks");
  const green = await gh("pr", "checks");

  expect([red.stdout, red.exit]).toEqual([`${RED.stdout}\n`, 1]);
  expect([green.stdout, green.exit]).toEqual([`${GREEN.stdout}\n`, 0]);
});

test("a call past the end of a route's replies takes the last reply again", async () => {
  const { gh, calls, fixtures } = world([{ match: "^gh pr checks", replies: [RED, GREEN] }]);

  await gh("pr", "checks");
  await gh("pr", "checks");
  const third = await gh("pr", "checks");

  expect([third.stdout, third.exit]).toEqual([`${GREEN.stdout}\n`, 0]);
  expect(calls().map((call) => call.exit)).toEqual([1, 0, 0]);
  // The stand-in is a fresh process per call, so the count of calls the route has answered is the
  // fixture file's.
  expect(fixtures().routes[0]?.answered).toBe(3);
});

test("a route that follows another advances on that route's calls, never on its own", async () => {
  const { gh } = answering([
    { match: "^gh pr checks", replies: [RED, GREEN], follows: "^gh run rerun" },
    { match: "^gh pr view", replies: [RED, GREEN] },
    { match: "^gh run rerun", stdout: RERUN },
  ]);

  const read = [await gh("pr", "checks"), await gh("pr", "checks"), await gh("pr", "checks")];
  // The route beside it follows nothing, so it advances on its own calls, as every route did.
  const own = [await gh("pr", "view"), await gh("pr", "view")];
  await gh("run", "rerun", "17420", "--failed");
  const pressed = await gh("pr", "checks");

  expect(read.map((call) => [call.stdout, call.exit])).toEqual([
    [`${RED.stdout}\n`, 1],
    [`${RED.stdout}\n`, 1],
    [`${RED.stdout}\n`, 1],
  ]);
  expect(own.map((call) => call.stdout)).toEqual([`${RED.stdout}\n`, `${GREEN.stdout}\n`]);
  expect([pressed.stdout, pressed.exit]).toEqual([`${GREEN.stdout}\n`, 0]);
  // Slowest measured 5.09 s (pod, alone, timed out at bun's 5 s, 2026-11-21); also 1.22-3.99 s in the pod, 0.34 s in CI run 44797028466.
}, 16_000);

// Catches (INFRA-1303 D4): a body edit made after the press that never reaches the reply the rig
// puts in place once it settles the re-run (`settled`), so the pull request read after the
// settlement would answer the body from before the edit.
test("a body edit reaches a route's settled reply as it reaches its replies", async () => {
  const view = (body: string) => ({ stdout: { body, number: 1000 } });
  const { gh, fixtures } = answering([
    {
      match: "^gh pr view",
      replies: [view("before"), view("running")],
      settled: view("settled"),
      follows: "^gh run rerun",
    },
    { match: "^gh run rerun", stdout: "" },
    { match: "^gh pr edit", stdout: "https://github.com/example/widgets/pull/1000" },
  ]);

  await gh("run", "rerun", "17420", "--failed");
  await gh("pr", "edit", "1000", "--body", "edited");
  const route = fixtures().routes[0] as unknown as {
    replies: { stdout: { body: string } }[];
    settled: { stdout: { body: string } };
  };

  expect({
    replies: route.replies.map((reply) => reply.stdout.body),
    settled: route.settled.stdout.body,
  }).toEqual({ replies: ["edited", "edited"], settled: "edited" });
});

/** How many times each row below runs its calls at once: the lost update they pin was measured
 * at 6 of 20 trials of one re-run beside three reads of the head. Each row's bound is its own,
 * three times its slowest measured time, beside it. */
const TRIALS = 20;

test("reads of the head made beside the re-run never lose the re-run's count", async () => {
  const seen: unknown[] = [];
  for (let trial = 0; trial < TRIALS; trial += 1) {
    const { gh, fixtures } = world([
      { match: "^gh pr checks", replies: [RED, GREEN], follows: "^gh run rerun" },
      { match: "^gh run rerun", stdout: RERUN },
    ]);

    await Promise.all([
      gh("run", "rerun", "17420", "--failed"),
      gh("pr", "checks"),
      gh("pr", "checks"),
      gh("pr", "checks"),
    ]);
    const after = await gh("pr", "checks");

    seen.push({ rerun: fixtures().routes[1]?.answered, after: [after.stdout, after.exit] });
  }
  expect(seen).toEqual(
    Array.from({ length: TRIALS }, () => ({ rerun: 1, after: [`${GREEN.stdout}\n`, 0] }))
  );
  // Slowest measured 33.54 s (pod, whole file, 2026-11-21); also 26.49-28.07 s there, 16.6-17.7 s alone (pod, 2026-11-19), 3.24 s in CI run 44797028466.
}, 101_000);

test("calls of one route made at once each take a reply of their own and each count", async () => {
  const calls = 6;
  const seen: unknown[] = [];
  for (let trial = 0; trial < TRIALS; trial += 1) {
    const { gh, fixtures } = world([{ match: "^gh pr view", replies: [RED, GREEN] }]);

    const answers = await Promise.all(Array.from({ length: calls }, () => gh("pr", "view")));

    seen.push({
      red: answers.filter((answer) => answer.exit === 1).length,
      answered: fixtures().routes[0]?.answered,
    });
  }
  // One call takes the first reply and every other one the last, as one call after another would.
  expect(seen).toEqual(Array.from({ length: TRIALS }, () => ({ red: 1, answered: calls })));
  // Slowest measured 24.4 s (pod, alone, 2026-11-19); also 19.81-22.57 s (pod, whole file, 2026-11-21), 3.15 s in CI run 44797028466.
}, 74_000);

test("a follows that names no route's match is refused, naming it, and answers no call", async () => {
  const typo = world([
    { match: "^gh pr checks", replies: [RED, GREEN], follows: "^gh run rerunn" },
    { match: "^gh run rerun", stdout: RERUN },
  ]);
  const refusal =
    `gh-standin: ${typo.run}/fixtures.json is refused:\n` +
    `✖ route "^gh pr checks" follows "^gh run rerunn", which is no route's match\n` +
    "  → at routes[0].follows\n";

  const read = await typo.gh("pr", "checks");
  const rerun = await typo.gh("run", "rerun", "17420", "--failed");
  const checked = await typo.check();

  expect(read).toEqual({ stdout: "", stderr: refusal, exit: 2 });
  expect(rerun).toEqual({ stdout: "", stderr: refusal, exit: 2 });
  expect(checked).toEqual({ stdout: "", stderr: refusal, exit: 2 });
  expect(typo.recorded()).toBe(false);

  // The same file with the follows spelled as the route's match: the check passes and records
  // nothing, and the read answers.
  const spelled = world([
    { match: "^gh pr checks", replies: [RED, GREEN], follows: "^gh run rerun" },
    { match: "^gh run rerun", stdout: RERUN },
  ]);
  expect(await spelled.check()).toEqual({ stdout: "", stderr: "", exit: 0 });
  expect(spelled.recorded()).toBe(false);
  expect((await spelled.gh("pr", "checks")).exit).toBe(1);
});

test("a spent route falls through to the next route that matches, and to the refusal when none does", async () => {
  const { gh, calls } = world([
    { match: "^gh run rerun \\d+ --failed", stdout: RERUN, spends: 1 },
    { match: "^gh run rerun", stderr: "gh: the head's re-run is spent\n", exit: 1 },
  ]);

  const first = await gh("run", "rerun", "17420", "--failed");
  const second = await gh("run", "rerun", "17420", "--failed");

  expect(first.stdout).toBe(`${RERUN}\n`);
  expect([second.stderr, second.exit]).toEqual(["gh: the head's re-run is spent\n", 1]);
  expect(calls().map((call) => call.route)).toEqual([
    "^gh run rerun \\d+ --failed",
    "^gh run rerun",
  ]);

  const alone = world([{ match: "^gh pr checks", stdout: GREEN.stdout, spends: 1 }]);
  expect((await alone.gh("pr", "checks")).stdout).toBe(`${GREEN.stdout}\n`);
  const spent = await alone.gh("pr", "checks");

  expect([spent.stderr, spent.exit]).toEqual(["rig: request not modeled: gh pr checks\n", 1]);
  expect(alone.calls().map((call) => call.route)).toEqual(["^gh pr checks", null]);
  expect(
    readFileSync(path.join(alone.run, "calls.jsonl"), "utf8")
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line).unmodeled)
  ).toEqual([undefined, true]);
});

// Catches: a press of the re-run through another command (a REST POST) that moves no read that
// follows the re-run, so the head stays red after it; and a counts_as naming no route, which
// would press nothing for good.
test("a route that counts as another advances every route that follows that one", async () => {
  const { gh } = answering([
    { match: "^gh pr checks", replies: [RED, GREEN], follows: "^gh run rerun" },
    { match: "^gh run rerun", stdout: RERUN },
    { match: "^gh api POST ", stdout: {}, counts_as: "^gh run rerun" },
  ]);
  const before = await gh("pr", "checks");
  await gh("api", "-X", "POST", "repos/example/widgets/actions/runs/17420/rerun-failed-jobs");
  const after = await gh("pr", "checks");
  expect([before.exit, after.exit]).toEqual([1, 0]);
  const typo = world([{ match: "^gh api", stdout: {}, counts_as: "^gh run rerunn" }]);
  expect((await typo.check()).exit).toBe(2);
});

// Catches: a list answered whole whatever the call's flags asked of it; a program handed the raw
// argv, so an attached short flag (`-ssuccess`) or a GET's `-f` fields filter nothing, where gh
// sends them (the review of 76cc2379, C2 and C3); and --json fields picked from an object but not
// from each object of a list, as gh picks them.
test("a route's program reads the call's flags and query as gh sends them, and --json picks from a list", async () => {
  const base = "b".repeat(40);
  const { gh } = answering([
    {
      match: "^gh run list",
      stdout: [
        { databaseId: 1, status: "completed", conclusion: "failure" },
        { databaseId: 2, status: "completed", conclusion: "success" },
      ],
      program: '$flags["--status"] as $s | map(select($s == null or .conclusion == $s))',
    },
    {
      match: "^gh api GET repos/example/widgets/actions/runs\\?",
      stdout: {
        workflow_runs: [
          { id: 1, head_sha: "a".repeat(40) },
          { id: 2, head_sha: base },
        ],
      },
      program:
        ".workflow_runs |= map(select(.head_sha == $query.head_sha and $query.event != null))",
    },
  ]);
  const long = await gh("run", "list", "--status", "success", "--json", "databaseId");
  const attached = await gh("run", "list", "-ssuccess", "--json", "databaseId");
  const fields = await gh(
    ...["api", "-X", "GET", "repos/example/widgets/actions/runs"],
    ...["-f", `head_sha=${base}`, "-f", "event=pull_request", "--jq", ".workflow_runs[].id"]
  );
  expect([long.stdout, attached.stdout, fields.stdout]).toEqual([
    '[{"databaseId":2}]\n',
    '[{"databaseId":2}]\n',
    "2\n",
  ]);
  // gh sends a GET's fields as its query, by name: `event` before `head_sha` (measured).
  expect(fields.request?.split(" ")[3]).toBe(
    `repos/example/widgets/actions/runs?event=pull_request&head_sha=${base}`
  );
});

// Catches: a `--paginate` REST read that prints only its first page, where gh follows GitHub's
// `Link: rel="next"` and prints every page, the pages of an array as one array and the pages of an
// object one after another with nothing between (measured, 2026-11-19, two pages of
// cybermonklabs/legion each); a `--jq` run over the whole rather than each page; a `--slurp` that
// lists one page; and a paged read that names no per_page sent without the 100 gh asks for
// (measured, GH_DEBUG=api).
test("--paginate follows the pages a route's program gives and prints them as gh joins them", async () => {
  const page =
    '(($query.per_page // "30") | tonumber) as $per | (($query.page // "1") | tonumber) as $page' +
    " | .[($page - 1) * $per:$page * $per]";
  const { gh } = answering([
    {
      match: "^gh api GET repos/example/widgets/pulls\\?",
      stdout: [{ n: 1 }, { n: 2 }, { n: 3 }],
      program: page,
    },
    {
      match: "^gh api GET repos/example/widgets/check-runs\\?",
      stdout: [{ id: 1 }, { id: 2 }, { id: 3 }],
      program: `(${page}) as $runs | {total_count: length, check_runs: $runs}`,
    },
  ]);
  const array = await gh("api", "--paginate", "repos/example/widgets/pulls?per_page=2");
  const object = await gh("api", "--paginate", "repos/example/widgets/check-runs?per_page=2");
  const filtered = await gh(
    "api",
    "--paginate",
    "repos/example/widgets/check-runs?per_page=2",
    "--jq",
    ".check_runs[].id"
  );
  const slurped = await gh(
    "api",
    "--paginate",
    "--slurp",
    "repos/example/widgets/pulls?per_page=2"
  );
  const whole = await gh("api", "--paginate", "repos/example/widgets/pulls");
  expect([array.stdout, object.stdout, filtered.stdout, slurped.stdout, whole.stdout]).toEqual([
    '[{"n":1},{"n":2},{"n":3}]',
    '{"total_count":3,"check_runs":[{"id":1},{"id":2}]}{"total_count":3,"check_runs":[{"id":3}]}',
    "1\n2\n3\n",
    '[[{"n":1},{"n":2}],[{"n":3}]]',
    '[{"n":1},{"n":2},{"n":3}]',
  ]);
  expect(whole.request).toBe("gh api GET repos/example/widgets/pulls?per_page=100 --paginate");
});

/** The template of each review the routes below keep. */
const REVIEW = {
  id: 7001,
  comment_id: 7101,
  commit_id: HEAD,
  user: { login: "rig[bot]" },
  author_association: "NONE",
  html_url: "https://github.com/example/widgets/pull/1000",
  pull_request_url: "https://api.github.com/repos/example/widgets/pulls/1000",
  added_lines: { "a.ts": 3 },
};
/** Routes that keep the reviews a role sends and read them back with their inline comments, and
 * take the comments it posts on a line or on the pull request, which they do not keep. */
const LINE_COMMENT = "^gh api POST \\S+/pulls/\\d+/comments";
const UNTIL_LINE_COMMENT = {
  follows: LINE_COMMENT,
  replies: [{ stdout: [] }, { unmodeled: true }],
};
const REVIEW_ROUTES = [
  { match: "^gh api POST \\S+/reviews(?:\\s|$)", review: REVIEW },
  { match: LINE_COMMENT, stdout: { id: 1 } },
  { match: "^gh api POST \\S+/issues/\\d+/comments", stdout: { id: 2 } },
  { match: "^gh api GET \\S+/pulls/\\d+/comments", reviews: "comments", ...UNTIL_LINE_COMMENT },
  {
    match: "^gh api GET \\S+/reviews/(?<id>\\d+)/comments",
    reviews: "review_comments",
    ...UNTIL_LINE_COMMENT,
  },
  { match: "^gh api GET \\S+/reviews/(?<id>\\d+)", reviews: "one", ...UNTIL_LINE_COMMENT },
  { match: "^gh api GET \\S+/reviews", reviews: "list", ...UNTIL_LINE_COMMENT },
  { match: "^gh pr review", stdout: "", review: REVIEW },
  {
    match: "^gh pr view",
    stdout: { number: 1000 },
    reviews: "pr",
    comments: "^gh api POST \\S+/issues/\\d+/comments",
  },
];
const REVIEWS = "repos/example/widgets/pulls/1000/reviews";

/** The `--input` file of the review below, by the name a call gives it. */
const COMMENT_REVIEW = {
  "review.json": JSON.stringify({
    event: "COMMENT",
    body: "See the line.",
    comments: [{ path: "a.ts", line: 3, side: "RIGHT", body: "here" }],
  }),
};

// The rules of the review model are review-keep.test.ts's, called directly. Catches: a review
// route that keeps nothing across calls (each call answers from the fixtures the one before it
// left, as fixtures.json carries them from one stand-in process to the next), a read mode wired to
// another read, a review or a read the model refuses
// answered rather than refused as unmodeled, a review read by an id no review has answered other
// than GitHub's 404, and the reads still answering once the role posts a line comment of its own.
test("the reviews a role sends are kept across calls, read back by every read, and refused where the model refuses", async () => {
  const answered = answering(REVIEW_ROUTES, {}, COMMENT_REVIEW);
  const seen: Answering[] = [];
  const gh = async (...args: string[]) => {
    const call = await answered.gh(...args);
    seen.push(call);
    return call;
  };
  const pending = JSON.parse((await gh("api", "-X", "POST", REVIEWS, "-f", "body=draft")).stdout);
  const refused = [
    await gh("api", "-X", "POST", REVIEWS, "-f", "body=again"),
    await gh("pr", "view", "1000", "--json", "latestReviews"),
  ];
  const commented = JSON.parse(
    (await gh("api", "-X", "POST", REVIEWS, "--input", "review.json")).stdout
  );
  const comments = JSON.parse(
    (await gh("api", "repos/example/widgets/pulls/1000/comments")).stdout
  );
  const ofReview = JSON.parse((await gh("api", `${REVIEWS}/7002/comments`)).stdout);
  const list = JSON.parse((await gh("api", REVIEWS)).stdout);
  const one = JSON.parse((await gh("api", `${REVIEWS}/7002`)).stdout);
  const none = await gh("api", `${REVIEWS}/7003`);
  const view = JSON.parse((await gh("pr", "view", "1000", "--json", "reviews,comments")).stdout);

  expect([pending.id, pending.state]).toEqual([7001, "PENDING"]);
  expect(refused.map((call) => [call.refused, call.exit])).toEqual([
    [true, 1],
    [true, 1],
  ]);
  expect([commented.id, commented.state, commented.commit_id]).toEqual([7002, "COMMENTED", HEAD]);
  expect([comments.map((c: { id: number }) => c.id), ofReview]).toEqual([[7101], comments]);
  expect(list.map((review: { id: number }) => review.id)).toEqual([7001, 7002]);
  expect(one).toEqual(commented);
  expect([none.stderr, none.exit]).toEqual(["gh: Not Found (HTTP 404)\n", 1]);
  expect([view.reviews.map((review: { state: string }) => review.state), view.comments]).toEqual([
    ["PENDING", "COMMENTED"],
    [],
  ]);
  // A line comment posted on its own opens a review GitHub keeps and the stand-in does not.
  await gh("api", "-X", "POST", "repos/example/widgets/pulls/1000/comments", "-f", "body=z");
  expect((await gh("api", REVIEWS)).exit).toBe(1);
  expect(seen.filter((call) => call.refused).length).toBe(3);
  // What the calls left, as the next process would read it from fixtures.json, still parses.
  expect(Fixtures.safeParse(answered.fixtures()).success).toBe(true);
  const typo = world([{ match: "^gh pr view", reviews: "pr", comments: "^gh api nothing" }]);
  expect((await typo.check()).exit).toBe(2);
});

/** Routes that change a review the role sent: its body, and its dismissal. */
const REVIEW_EDITS = [
  {
    match: "^gh api PUT \\S+/reviews/(?<id>\\d+)(?:\\s|$)",
    review_edit: "update",
    ...UNTIL_LINE_COMMENT,
  },
  {
    match: "^gh api PUT \\S+/reviews/(?<id>\\d+)/dismissals(?:\\s|$)",
    review_edit: "dismiss",
    ...UNTIL_LINE_COMMENT,
  },
];

/** The `--input` file of the update below, by the name the call gives it. */
const UPDATE_BODY = { "body.json": JSON.stringify({ body: "Looks right; the re-run is green." }) };

// INFRA-1303: trial-i-head-4 of the tester's batch at 585b19a3 updated the body of the review it
// had sent and dismissed it, and the stand-in refused both as unmodeled. The rules of the change
// are review-keep.test.ts's, called directly. Catches: a change answered but kept nowhere (each
// call answers from the fixtures the one before it left, as fixtures.json carries them from one
// stand-in process to the next), a read that answers the review as it was before the change,
// GitHub's 422 for a COMMENTED review answered in anything but its files, and a change the model
// refuses answered rather than refused as unmodeled.
test("a review's body update and its dismissal answer as GitHub does, and the next reads read them", async () => {
  const answered = answering([...REVIEW_ROUTES, ...REVIEW_EDITS], {}, UPDATE_BODY);
  const seen: Answering[] = [];
  const gh = async (...args: string[]) => {
    const call = await answered.gh(...args);
    seen.push(call);
    return call;
  };
  const approved = JSON.parse(
    (await gh("api", "-X", "POST", REVIEWS, "-f", "event=APPROVE", "-f", "body=Looks right."))
      .stdout
  );
  const updated = JSON.parse(
    (await gh("api", "--method", "PUT", `${REVIEWS}/7001`, "--input", "body.json")).stdout
  );
  const listed = JSON.parse((await gh("api", REVIEWS)).stdout);
  const dismissed = JSON.parse(
    (
      await gh(
        "api",
        "-X",
        "PUT",
        `${REVIEWS}/7001/dismissals`,
        "-f",
        "message=Superseded.",
        "-f",
        "event=DISMISS"
      )
    ).stdout
  );
  const one = JSON.parse((await gh("api", `${REVIEWS}/7001`)).stdout);
  await gh("api", "-X", "POST", REVIEWS, "-f", "event=COMMENT", "-f", "body=See the line.");
  const commented = await gh("api", "-X", "PUT", `${REVIEWS}/7002/dismissals`, "-f", "message=x");
  const unknown = await gh("api", "-X", "PUT", `${REVIEWS}/7009`, "-f", "body=x");
  const measured = (part: string) =>
    readFileSync(
      path.join(import.meta.dir, "github-answers", `review-dismiss-422.${part}`),
      "utf8"
    );

  expect(updated).toEqual({ ...approved, body: "Looks right; the re-run is green." });
  expect(listed).toEqual([updated]);
  expect(dismissed).toEqual({ ...updated, state: "DISMISSED" });
  expect(one).toEqual(dismissed);
  expect([commented.stdout, commented.stderr, commented.exit]).toEqual([
    measured("stdout"),
    measured("stderr"),
    1,
  ]);
  expect([unknown.refused, unknown.exit]).toEqual([true, 1]);
  expect(seen.flatMap((call) => (call.refused ? [call.request] : []))).toEqual([
    `gh api PUT ${REVIEWS}/7009 --raw-field body=x`,
  ]);
});

test("the re-run and the failed-log read are each recorded with the route that answered them", async () => {
  const log = "--- FAIL: TestSettlesTheHead (0.12s)";
  const { gh, calls } = world([
    { match: "^gh run rerun \\d+ --failed", stdout: RERUN },
    { match: "^gh run view \\d+ --log-failed", stdout: log },
  ]);

  await gh("run", "rerun", "17420", "--failed");
  await gh("run", "view", "17420", "--log-failed");

  expect(
    calls().map((call) => ({ argv: call.argv.join(" "), route: call.route, exit: call.exit }))
  ).toEqual([
    { argv: "run rerun 17420 --failed", route: "^gh run rerun \\d+ --failed", exit: 0 },
    { argv: "run view 17420 --log-failed", route: "^gh run view \\d+ --log-failed", exit: 0 },
  ]);
});

// Catches: a `gh run watch` of a run in progress answered once and ended, where gh 2.98.0 prints
// a frame each time it reads the run, every --interval seconds, and ends only once the run has
// completed (pkg/cmd/run/watch/watch.go), which no call in the rig outlives; a watch recorded only
// when it ends, which a call the role's tool kills never does, so the scorer would never see it;
// and an interval gh would print its frames with no pause at, which the rig does not model.
test("a watch reply prints its frame again at each interval until the call is killed, recorded with no exit", async () => {
  const match = "^gh run watch 7(?=\\s|$)";
  const { env, gh, calls } = world([{ match, stdout: "* run 7\n", watch: true }]);
  const inherited = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !/^(?:DISPATCH|ENVOY|NATS)_/.test(name))
  );
  const child = Bun.spawn(["bun", standin, "gh", "run", "watch", "7", "--interval", "1"], {
    env: { ...inherited, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let printed = "";
  const first = { at: 0, calls: [] as Call[] };
  while (printed.split("* run 7\n").length - 1 < 2) {
    const { value, done } = await reader.read();
    if (done) break;
    printed += decoder.decode(value);
    if (first.at === 0) first.at = performance.now();
    if (first.calls.length === 0) first.calls = calls();
  }
  const between = performance.now() - first.at;
  child.kill();
  await child.exited;
  expect(printed).toBe("* run 7\n* run 7\n");
  // The frames stand an interval apart, and the call was recorded, with no exit, by the time the
  // first frame was printed.
  expect(between).toBeGreaterThanOrEqual(900);
  expect(
    first.calls.map((call) => ({ argv: call.argv.join(" "), route: call.route, exit: call.exit }))
  ).toEqual([{ argv: "run watch 7 --interval 1", route: match, exit: null }]);
  const busy = await gh("run", "watch", "7", "--interval", "0");
  expect([busy.stderr.startsWith("rig: request not modeled: gh "), busy.exit]).toEqual([true, 1]);
});

// Catches: `gh pr checks --watch` with gh's `gh run watch` interval, where gh 2.98.0 reads the
// checks every 10 seconds unless --interval names another (pkg/cmd/pr/checks/checks.go:21), and
// its --interval, --fail-fast and --watch refused as flags the rig does not model.
test("a watch of the checks prints its frame every 10 seconds unless --interval names another", async () => {
  const match = "^gh pr checks 1(?=\\s|$)(?=.*\\s--watch(?:\\s|$))";
  const { env, calls } = world([{ match, stdout: "* checks\n", watch: true }]);
  const inherited = Object.fromEntries(
    Object.entries(process.env).filter(([name]) => !/^(?:DISPATCH|ENVOY|NATS)_/.test(name))
  );
  /** What the watch printed in `ms` milliseconds after its first frame. */
  const printedIn = async (ms: number, ...args: string[]) => {
    const child = Bun.spawn(["bun", standin, "gh", "pr", "checks", "1", ...args], {
      env: { ...inherited, ...env },
      stdout: "pipe",
      stderr: "pipe",
    });
    const reader = child.stdout.getReader();
    const decoder = new TextDecoder();
    let printed = decoder.decode((await reader.read()).value);
    const until = performance.now() + ms;
    const more = (async () => {
      for (;;) {
        const { value, done } = await reader.read();
        if (done) return;
        printed += decoder.decode(value);
      }
    })();
    // The watch is a child process on the real clock, so its interval is waited out, not faked.
    await Bun.sleep(Math.max(0, until - performance.now()));
    child.kill();
    await Promise.all([child.exited, more]);
    return printed;
  };
  expect(await printedIn(2500, "--watch")).toBe("* checks\n");
  expect(await printedIn(1500, "--watch", "--fail-fast", "-i", "1")).toBe("* checks\n* checks\n");
  expect(calls().map((call) => [call.argv.join(" "), call.exit])).toEqual([
    ["pr checks 1 --watch", null],
    ["pr checks 1 --watch --fail-fast -i 1", null],
  ]);
  // Slowest measured 4.36 s (pod, whole file, 2026-11-21); also 4.32-4.34 s there; no CI run yet.
}, 14_000);

// Catches: a read that writes fixtures.json back, which changes what a later reader of the world
// reads, so the order of two readers decides what each reads (at 7bf70a93 every call wrote the
// file back, in the stand-in's own form, with the route's count moved: the eight variant rows of
// rig.test.ts failed in a whole-file run for it, CI run 44797028466). Two readers of one world in
// both orders: a `gh pr view` of a head that follows the re-run, and a row's read of the file
// itself; each reads the same in either order.
test("two readers of one world read the same in either order: a read writes nothing back", async () => {
  const { gh, run } = world([
    { match: "^gh run rerun 7 --failed", replies: [{ stdout: RERUN }] },
    { match: "^gh pr view 1", follows: "^gh run rerun 7 --failed", replies: [RED, GREEN] },
  ]);
  const file = () => readFileSync(path.join(run, "fixtures.json"), "utf8");
  const view = async () => {
    const read = await gh("pr", "view", "1");
    return [read.stdout, read.exit];
  };
  const fileFirst = { file: file(), view: await view() };
  const viewFirst = { view: await view(), file: file() };
  expect(viewFirst).toEqual(fileFirst);
});

// Catches: a call that would write a world rows share, written in silence, so a later row reads a
// world the order of rows made: the stand-in refuses it, naming the world and the call, and writes
// nothing, while a read of the same world is answered as ever.
test("a call that would write a shared world is refused by name, and a read of it is answered", async () => {
  const { gh, run, recorded } = world([
    { match: "^gh run rerun 7 --failed", replies: [{ stdout: RERUN }] },
    { match: "^gh pr view 1", follows: "^gh run rerun 7 --failed", replies: [RED, GREEN] },
  ]);
  writeFileSync(path.join(run, "shared"), "");
  const before = readFileSync(path.join(run, "fixtures.json"), "utf8");
  const read = await gh("pr", "view", "1");
  const press = await gh("run", "rerun", "7", "--failed");
  expect([read.stdout, read.exit]).toEqual([`${RED.stdout}\n`, 1]);
  expect(press).toEqual({
    stdout: "",
    stderr: `gh-standin: ${run} is a world rows share, and gh run rerun 7 --failed would write it: run this call in a world of its own (red-at-the-head.fixtures.ts's ownFixture)\n`,
    exit: 2,
  });
  expect(readFileSync(path.join(run, "fixtures.json"), "utf8")).toBe(before);
  expect(recorded()).toBe(true);
});

// Catches: a refusal that names a fixed ref, or none, where GitHub names the one the call asked
// for, so a role that read a commit by a wrong name (`null`, a branch nobody made) cannot tell
// which of its reads was refused; and a `$<name>` the route's match has no group for that is
// dropped rather than printed as the fixture wrote it.
test("a route's stderr names the text each named group of its match took in the call", async () => {
  const { gh } = answering([
    {
      match: "^gh api .*commits/(?<commit>[^\\s?]*)/check-runs",
      stderr: "gh: No commit found for SHA: $<commit> (HTTP 422)\n",
      exit: 1,
    },
    { match: "^gh run view", stderr: "gh: no run $<run>\n", exit: 1 },
  ]);
  /** What a call prints and exits with. */
  const printed = async (...args: string[]) => {
    const { stdout, stderr, exit } = await gh(...args);
    return { stdout, stderr, exit };
  };

  expect(await printed("api", "repos/example/widgets/commits/null/check-runs")).toEqual({
    stdout: "",
    stderr: "gh: No commit found for SHA: null (HTTP 422)\n",
    exit: 1,
  });
  expect(await printed("api", "repos/example/widgets/commits/trunk/check-runs?filter=all")).toEqual(
    {
      stdout: "",
      stderr: "gh: No commit found for SHA: trunk (HTTP 422)\n",
      exit: 1,
    }
  );
  expect(await printed("run", "view", "17420")).toEqual({
    stdout: "",
    stderr: "gh: no run $<run>\n",
    exit: 1,
  });
});

test("a route's publish posts the envelope the fixture wrote, with the bearer, to the listener", async () => {
  const envoy = listener();
  const { gh, calls, run } = world([
    { match: "^gh run rerun \\d+ --failed", stdout: RERUN, publish: ENVELOPE },
  ]);
  paneEnv(run, envoy.url, "rig-listener-bearer");

  const result = await gh("run", "rerun", "17420", "--failed");

  expect([result.stdout, result.exit]).toEqual([`${RERUN}\n`, 0]);
  expect(envoy.received).toEqual([
    {
      method: "POST",
      pathname: "/v1/messages/publish",
      authorization: "Bearer rig-listener-bearer",
      body: ENVELOPE,
    },
  ]);
  expect(calls()[0]?.published).toEqual({ status: 200, body: JSON.stringify({ ok: true }) });
});

test("a publish the listener refuses, or that reaches nothing, leaves the call's exit status alone", async () => {
  const refusing = listener(400, { error: "message is required", expected: ["message"] });
  const refused = world([{ match: "^gh run rerun", stdout: RERUN, publish: ENVELOPE }]);
  paneEnv(refused.run, refusing.url, "rig-listener-bearer");

  const answered = await refused.gh("run", "rerun", "17420", "--failed");

  expect([answered.stdout, answered.exit]).toEqual([`${RERUN}\n`, 0]);
  expect(refused.calls()[0]?.published).toEqual({
    status: 400,
    body: JSON.stringify({ error: "message is required", expected: ["message"] }),
  });

  // A listener that is gone: the publish fails outright, and the call still exits as its route says.
  const unreachable = world([{ match: "^gh run rerun", stdout: "", exit: 2, publish: ENVELOPE }]);
  // The dead address rig.sh's own probe uses: a listener bound and stopped here would be stopped
  // twice by the afterEach, and its freed port could be taken by another process mid-test.
  paneEnv(unreachable.run, "http://127.0.0.1:1", "rig-listener-bearer");

  expect((await unreachable.gh("run", "rerun", "17420", "--failed")).exit).toBe(2);
  expect(unreachable.calls()[0]?.published).toHaveProperty("error");
});

// The stand-in's own environment stands in for a Legion pane's, whose ENVOY_URL and
// ENVOY_TOKEN_FILE name the live relay: a stand-in run by hand from a pane is given them.
// Catches: a stand-in that publishes to the listener its own environment names rather than the
// one the run's pane.env names, which on 2026-11-18 posted a fixture's settlement to the live
// relay (topic notifications.github.example.widgets.pr.1003.checks).
test("a run whose pane.env names no listener publishes nowhere, whatever Envoy the stand-in is given", async () => {
  const pane = listener();
  const { env, gh, calls, run } = world([
    { match: "^gh run rerun \\d+ --failed", stdout: RERUN, publish: ENVELOPE },
  ]);
  env.ENVOY_URL = pane.url;
  env.ENVOY_TOKEN_FILE = tokenFile(run, "pane-relay-bearer");

  const result = await gh("run", "rerun", "17420", "--failed");

  expect([result.stdout, result.exit]).toEqual([`${RERUN}\n`, 0]);
  expect(pane.received).toEqual([]);
  expect(calls()[0]?.published).toEqual({
    error: `${run}/pane.env names no ENVOY_URL or ENVOY_TOKEN_FILE`,
  });
});

// Catches: a route that reads the call as the agent typed it rather than as gh resolves it, so
// `-R x` and `--repo=x` are two requests, text inside a body reads as a flag of its own (`--body
// "a -R other/repo b"` taken for another repository), `{owner}/{repo}` reads as another
// repository, a write sent with fields and no -X reads as a GET, a GET's fields read as anything
// but the query gh sends them as, and a flag the rig does not model is answered as if it were
// absent.
test("a call is read as gh resolves it: long flags, one argument after another, gh api's method and endpoint", async () => {
  const { gh } = answering(
    [
      { match: "^gh pr view 1000(?:\\s|$)", stdout: { title: "t", number: 1000 } },
      { match: "^gh pr comment 1000 ", stdout: "https://github.com/example/widgets/pull/1000" },
      {
        match: "^gh api POST repos/example/widgets/pulls/1000/reviews(?:\\s|$)",
        stdout: { id: 1 },
      },
      {
        match: "^gh api GET repos/example/widgets/pulls(?:\\?\\S*)?(?:\\s|$)",
        stdout: [{ number: 1000 }],
      },
    ],
    { GH_REPO: "example/widgets" }
  );
  const calls = [
    await gh("pr", "view", "1000", "-R", "example/widgets", "--json", "title"),
    await gh("pr", "view", "1000", "--repo=example/widgets", "--json=title"),
    await gh("pr", "comment", "1000", "--body", "a -R other/repo b"),
    await gh("api", "/repos/{owner}/{repo}/pulls/1000/reviews", "-f", "event=APPROVE"),
    await gh(
      "api",
      "-X",
      "GET",
      "https://api.github.com/repos/example/widgets/pulls",
      "-f",
      "state=open"
    ),
  ];
  const web = await gh("pr", "view", "1000", "--web");

  expect([...calls, web].map((call) => call.request ?? null)).toEqual([
    "gh pr view 1000 --repo example/widgets --json title",
    "gh pr view 1000 --repo example/widgets --json title",
    "gh pr comment 1000 --body a\u001f-R\u001fother/repo\u001fb",
    "gh api POST repos/example/widgets/pulls/1000/reviews --raw-field event=APPROVE",
    "gh api GET repos/example/widgets/pulls?state=open",
    null,
  ]);
  expect([...calls, web].map((call) => call.route)).toEqual([
    "^gh pr view 1000(?:\\s|$)",
    "^gh pr view 1000(?:\\s|$)",
    "^gh pr comment 1000 ",
    "^gh api POST repos/example/widgets/pulls/1000/reviews(?:\\s|$)",
    "^gh api GET repos/example/widgets/pulls(?:\\?\\S*)?(?:\\s|$)",
    null,
  ]);
  expect([web.exit, web.refused]).toEqual([1, true]);
});

// Catches: a --json read that drops a field gh refuses or the world does not hold and answers
// exit 0 without it, where gh 2.98.0 refuses an unknown field with its list (measured) and the
// rig cannot say what GitHub holds for the other; fields printed in the order the call names them
// rather than gh's; and `--paginate --slurp` printed as one page rather than a list of pages, the
// read sent as gh sends a paged read that names no per_page, asking for 100 (measured).
test("--json picks gh's fields by name, refuses one gh does not know, and leaves one the world lacks unmodeled", async () => {
  const { gh } = answering([
    { match: "^gh pr view 1000(?:\\s|$)", stdout: { title: "t", number: 1000, headRefOid: HEAD } },
    { match: "^gh pr list(?:\\s|$)", stdout: [{ number: 1, title: "a", state: "OPEN" }] },
    {
      match: "^gh api GET repos/example/widgets/pulls\\?per_page=100(?:\\s|$)",
      stdout: [{ number: 1000 }],
    },
  ]);
  const picked = await gh("pr", "view", "1000", "--json", "title,number");
  const listed = await gh("pr", "list", "--json", "title,number");
  const unknown = await gh("pr", "view", "1000", "--json", "bogus");
  const lacking = await gh("pr", "view", "1000", "--json", "number,mergeStateStatus");
  const slurped = await gh("api", "repos/example/widgets/pulls", "--paginate", "--slurp");

  expect([picked.stdout, listed.stdout, slurped.stdout]).toEqual([
    `${JSON.stringify({ number: 1000, title: "t" })}\n`,
    `${JSON.stringify([{ number: 1, title: "a" }])}\n`,
    JSON.stringify([[{ number: 1000 }]]),
  ]);
  expect([unknown.stdout, unknown.exit]).toEqual(["", 1]);
  expect(unknown.stderr).toStartWith(
    'Unknown JSON field: "bogus"\nAvailable fields:\n  additions\n  assignees\n  author\n'
  );
  expect([lacking.stdout, lacking.exit, lacking.refused]).toEqual(["", 1, true]);
  expect([picked, listed, unknown, lacking, slurped].map((call) => call.refused)).toEqual([
    false,
    false,
    false,
    true,
    false,
  ]);
});

// Catches: a GraphQL read answered with the whole object a route holds rather than the query's own
// selection, a field the world does not hold answered as missing rather than refused, a pull
// request the world does not have answered with the world's own (round 5's `pullRequest(number:
// 999)`), and two spellings of one query read as two requests.
test("a GraphQL query is answered with its own selection of the world, or GitHub's error, or refused", async () => {
  const { gh } = answering([
    {
      match: "^gh api POST graphql ",
      graphql: true,
      stdout: {
        graphql: {
          'repository(name:"widgets",owner:"example")': {
            name: "widgets",
            "pullRequest(number:1000)": { $ref: "pr" },
            "pullRequest(*)": {
              $error: {
                type: "NOT_FOUND",
                message: "Could not resolve to a PullRequest with the number of {number}.",
              },
            },
          },
        },
        $refs: { pr: { title: "t", number: 1000, headRefOid: HEAD } },
      },
    },
  ]);
  const query = (pr: string) =>
    `{repository(owner:"example",name:"widgets"){pullRequest(number:${pr}){title}}}`;
  const selected = await gh("api", "graphql", "-f", `query=${query("1000")}`);
  const variables = await gh(
    "api",
    "graphql",
    "-F",
    "n=1000",
    "-f",
    'query=query($n:Int!){ repository(name: "widgets", owner: "example") { pr: pullRequest(number: $n) { number headRefOid } } }'
  );
  const missing = await gh("api", "graphql", "-f", `query=${query("999")}`);
  const unheld = await gh(
    "api",
    "graphql",
    "-f",
    'query={repository(owner:"example",name:"widgets"){pullRequest(number:1000){title bogus}}}'
  );
  const respelled = await gh(
    "api",
    "graphql",
    "-f",
    'query={ repository( name: "widgets", owner: "example" ) { pullRequest( number: 1000 ) { title } } }'
  );

  expect([selected.stdout, selected.exit]).toEqual([
    JSON.stringify({ data: { repository: { pullRequest: { title: "t" } } } }),
    0,
  ]);
  expect(JSON.parse(variables.stdout)).toEqual({
    data: { repository: { pr: { number: 1000, headRefOid: HEAD } } },
  });
  expect([JSON.parse(missing.stdout), missing.stderr, missing.exit]).toEqual([
    {
      data: { repository: { pullRequest: null } },
      errors: [
        {
          type: "NOT_FOUND",
          path: ["repository", "pullRequest"],
          locations: [{ line: 1, column: query("999").indexOf("pullRequest") + 1 }],
          message: "Could not resolve to a PullRequest with the number of 999.",
        },
      ],
    },
    "gh: Could not resolve to a PullRequest with the number of 999.\n",
    1,
  ]);
  expect([unheld.stdout, unheld.exit, unheld.refused]).toEqual(["", 1, true]);
  expect(respelled.stdout).toBe(selected.stdout);
  expect(respelled.request).toBe(selected.request);
});

// Catches: a measured answer printed in other bytes than the files hold, or with the probe's
// commit (1111…1111) where the call named another, and a `$<name>` in a route's stdout left
// unfilled; and a `gh api` body printed with a newline after it, which gh does not print.
test("a route's github answer prints the measured files with the call's commit, and gh api adds no newline", async () => {
  const { gh } = answering([
    {
      match: "^gh api GET repos/example/widgets/commits/(?<commit>\\w+)/check-runs(?:\\s|$)",
      github: "checkruns-422",
      sha: "commit",
    },
    {
      match: "^gh api GET repos/example/widgets/commits/(?<commit>\\w+)/status(?:\\s|$)",
      stdout: { sha: "$<commit>" },
    },
  ]);
  const files = (part: string) =>
    readFileSync(path.join(import.meta.dir, "github-answers", `checkruns-422.${part}`), "utf8");
  const refused = await gh("api", "repos/example/widgets/commits/abc123/check-runs");
  const status = await gh("api", "repos/example/widgets/commits/abc123/status");
  expect([refused.stdout, refused.stderr, refused.exit]).toEqual([
    files("stdout").replaceAll("1".repeat(40), "abc123"),
    files("stderr").replaceAll("1".repeat(40), "abc123"),
    1,
  ]);
  expect([status.stdout, status.exit]).toEqual(['{"sha":"abc123"}', 0]);
});
