import { expect, setDefaultTimeout, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit, SCRIPT_RUN } from "./test-limit";

// Each row runs the stand-in as a bun process of its own, a start a loaded machine stretches: each
// test and hook gets the limit test-limit.ts computes from the bounds of what they wait for, and
// the fixture's removal of the reservations below runs within it.
setDefaultTimeout(fileRowLimit({ ...SCRIPT_RUN, part: "gh-standin.ts" }));

// Each row's run directory, reserved here (the fixture's hook makes it before the rows run).
const runs = {
  broken: temporaryDirectory("gh-standin-broken", { real: true }),
  sound: temporaryDirectory("gh-standin-sound", { real: true }),
  unrouted: temporaryDirectory("gh-standin-unrouted", { real: true }),
  refused: temporaryDirectory("gh-standin-refused", { real: true }),
  listed: temporaryDirectory("gh-standin-listed", { real: true }),
  edited: temporaryDirectory("gh-standin-edited", { real: true }),
};
const review = "^gh api .*pulls/702/reviews";

/** Writes `routes` as the run's fixtures.json, which every later call of the run reads and keeps. */
function routed(run: string, routes: unknown[]) {
  mkdirSync(run, { recursive: true });
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
}

/** Runs the stand-in as a run's `gh` on the run's fixtures as they stand, and returns its answer
 * and its record. */
function ask(run: string, ...args: string[]) {
  const result = spawnWithin(
    { ...SCRIPT_RUN, part: "gh-standin.ts" },
    ["bun", path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    { env: { ...process.env, SKILL_SCENARIO_RUN: run } }
  );
  const calls = readFileSync(path.join(run, "calls.jsonl"), "utf8").trim().split("\n");
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    call: JSON.parse(calls.at(-1) ?? "{}"),
  };
}

/** Runs the stand-in as a run's `gh` with `routes`, and returns its answer and its record. */
function gh(run: string, routes: unknown[], ...args: string[]) {
  routed(run, routes);
  return ask(run, ...args);
}

/** Runs the stand-in as a run's `gh`, with one route that answers a review's post. */
function post(run: string, body: string) {
  mkdirSync(run, { recursive: true });
  writeFileSync(path.join(run, "review.json"), body);
  return gh(
    run,
    [{ match: review, stdout: { id: 7002 } }],
    "api",
    "--method",
    "POST",
    "repos/example/widgets/pulls/702/reviews",
    "--input",
    path.join(run, "review.json")
  );
}

// Catches: a stand-in that answers a review whose JSON GitHub cannot parse as posted, so an agent
// that wrote its body with an unescaped quote believes it approved.
test("a body that is not JSON gets GitHub's 400, exits 1 and is recorded so", () => {
  const { code, stdout, stderr, call } = post(
    runs.broken,
    '{"event":"APPROVE","body":"Run `"+"`"+`widgets --help`"}'
  );
  expect({ code, stdout, stderr }).toEqual({
    code: 1,
    stdout: "",
    stderr: "gh: Problems parsing JSON (HTTP 400)\n",
  });
  expect(call.exit).toBe(1);
});

// Catches: a check that refuses every body, so no review ever posts.
test("a body that is JSON gets the route's answer", () => {
  const { code, stdout, call } = post(runs.sound, '{"event":"APPROVE","body":"ok"}');
  expect({ code, stdout }).toEqual({ code: 0, stdout: '{"id":7002}\n' });
  expect(call.route).toBe(review);
});

// Catches: a stand-in that answers a call its world has no route for as GitHub's 404, which an
// agent reads as GitHub saying the thing does not exist (the fifth round's reviewers read the base
// branch so and waited), and one that fails such a call quietly, with nothing in the run's log.
test("a call no route names fails loudly: exit 97, its line on stderr and in no-route.log, and a no_route record", () => {
  const { code, stdout, stderr, call } = gh(
    runs.unrouted,
    [{ match: review, stdout: [] }],
    "api",
    "repos/example/widgets/branches/main",
    "--jq",
    ".protected"
  );
  const line = "STAND-IN HAS NO ROUTE: gh api repos/example/widgets/branches/main --jq .protected";
  expect({ code, stdout, stderr }).toEqual({ code: 97, stdout: "", stderr: `${line}\n` });
  expect(readFileSync(path.join(runs.unrouted, "no-route.log"), "utf8")).toBe(`${line}\n`);
  expect(call).toMatchObject({ route: null, no_route: true, exit: 97 });
});

// Catches: an error route answered as a success, filtered by --jq, or with no status line under -i.
test("a route that refuses prints GitHub's body unfiltered, then gh's message, and exits 1", () => {
  const refusal = {
    match: "^gh api .*branches/main/protection",
    stdout: { message: "Resource not accessible by integration", status: "403" },
    stderr: "gh: Resource not accessible by integration (HTTP 403)\n",
    exit: 1,
    status: 403,
  };
  const { code, stdout, stderr, call } = gh(
    runs.refused,
    [refusal],
    "api",
    "-i",
    "repos/example/widgets/branches/main/protection",
    "--jq",
    ".message"
  );
  expect({ code, stderr }).toEqual({ code: 1, stderr: refusal.stderr });
  expect(stdout).toBe(
    'HTTP/2.0 403 Forbidden\r\nContent-Type: application/json; charset=utf-8\r\nX-Github-Api-Version-Selected: 2022-11-28\r\n\r\n{"message":"Resource not accessible by integration","status":"403"}'
  );
  expect(call).toMatchObject({ route: refusal.match, exit: 1 });
});

// Catches: a `--json` that picks fields of an object answer alone, so `gh pr list --json` prints each
// pull request whole, and a `--jq` that reads the list before the pick.
test("--json picks the named fields of each object of a list answer and of an object answer", () => {
  routed(runs.listed, [
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
  const { code, stdout, stderr } = ask(runs.listed, "pr", "list", "--json", "number,url");
  expect({ code, stdout, stderr }).toEqual({
    code: 0,
    stdout:
      '[{"number":7,"url":"https://example.invalid/7"},{"number":8,"url":"https://example.invalid/8"}]\n',
    stderr: "",
  });
  expect(ask(runs.listed, "pr", "list", "--json", "number", "--jq", ".[].number").stdout).toBe(
    "7\n8\n"
  );
  expect(ask(runs.listed, "pr", "view", "7", "--json", "url").stdout).toBe(
    '{"url":"https://example.invalid/7"}\n'
  );
});

// Catches: a body edit kept for an object answer alone, so the next `gh pr list --json body` reads
// the old body where GitHub's would read the new one; and one that gives a body to a list whose
// objects have none.
test("a body edit gives each object of a list answer that has a body the new body", () => {
  routed(runs.edited, [
    { match: "^gh pr edit", stdout: "https://example.invalid/7" },
    { match: "^gh pr list", stdout: [{ number: 7, body: "old" }] },
    { match: "^gh pr view", stdout: { number: 7, body: "old" } },
    { match: "^gh api .*comments", stdout: [{ id: 1 }] },
  ]);
  const edit = ask(runs.edited, "pr", "edit", "7", "--body", "new");
  expect(edit.code).toBe(0);
  expect(edit.call).toMatchObject({
    argv: ["pr", "edit", "7", "--body", "new"],
    body: "new",
    exit: 0,
  });
  expect(ask(runs.edited, "pr", "list", "--json", "number,body").stdout).toBe(
    '[{"number":7,"body":"new"}]\n'
  );
  expect(ask(runs.edited, "pr", "view", "7", "--json", "body").stdout).toBe('{"body":"new"}\n');
  expect(ask(runs.edited, "api", "repos/example/widgets/issues/7/comments").stdout).toBe(
    '[{"id":1}]\n'
  );
});
