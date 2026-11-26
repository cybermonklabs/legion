import { expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { fileRowLimit, SCRIPT_RUN, VCS_COMMAND } from "./test-limit";

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
  moved: temporaryDirectory("gh-standin-moved", { real: true }),
  cloned: temporaryDirectory("gh-standin-cloned", { real: true }),
};
const review = "^gh api .*pulls/702/reviews";

/** Writes `routes` as the run's fixtures.json, which every later call of the run reads and keeps. */
function routed(run: string, routes: unknown[]) {
  mkdirSync(run, { recursive: true });
  writeFileSync(path.join(run, "fixtures.json"), JSON.stringify({ routes }));
}

/** Runs the stand-in as a run's `gh` in `cwd` on the run's fixtures as they stand, and returns its
 * answer and its record. */
function askIn(cwd: string | undefined, run: string, ...args: string[]) {
  const result = spawnWithin(
    { ...SCRIPT_RUN, part: "gh-standin.ts" },
    ["bun", path.join(import.meta.dir, "gh-standin.ts"), "gh", ...args],
    { cwd, env: { ...process.env, SKILL_SCENARIO_RUN: run } }
  );
  const calls = readFileSync(path.join(run, "calls.jsonl"), "utf8").trim().split("\n");
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
    call: JSON.parse(calls.at(-1) ?? "{}"),
  };
}

/** Runs the stand-in as a run's `gh` on the run's fixtures as they stand, and returns its answer
 * and its record. */
const ask = (run: string, ...args: string[]) => askIn(undefined, run, ...args);

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

// Catches: a `-R <repo>` typed before the command (`gh -R o/r pr diff 702`), which gh reads as the
// flag of the command after it, failing every route since each starts with its command: a rig error
// in place of GitHub's answer. The call is matched and recorded as gh reads it, so the scorer sees
// its command first. A command that takes no `-R` (`gh api`) keeps the call as typed.
test("a -R or --repo before a pr or run command is read after the command, as gh reads it", () => {
  routed(runs.moved, [
    { match: "^gh pr diff -R example/widgets 702$", stdout: "the diff" },
    { match: "^gh pr view --repo=example/widgets 702 --json number$", stdout: { number: 702 } },
    { match: "^gh run list --repo example/widgets --limit 10$", stdout: "the runs" },
  ]);
  const diff = ask(runs.moved, "-R", "example/widgets", "pr", "diff", "702");
  expect({ code: diff.code, stdout: diff.stdout }).toEqual({ code: 0, stdout: "the diff\n" });
  expect(diff.call).toMatchObject({
    argv: ["pr", "diff", "-R", "example/widgets", "702"],
    route: "^gh pr diff -R example/widgets 702$",
    exit: 0,
  });
  expect(
    ask(runs.moved, "--repo=example/widgets", "pr", "view", "702", "--json", "number").stdout
  ).toBe('{"number":702}\n');
  expect(
    ask(runs.moved, "--repo", "example/widgets", "run", "list", "--limit", "10").stdout
  ).toBe("the runs\n");
  const api = ask(runs.moved, "-R", "example/widgets", "api", "repos/example/widgets/pulls/702");
  expect(api.code).toBe(97);
  expect(api.call).toMatchObject({
    argv: ["-R", "example/widgets", "api", "repos/example/widgets/pulls/702"],
    no_route: true,
  });
});

// Catches: a `gh repo clone` answered with no clone (the agent then works in a directory that does
// not exist) or with a clone of something other than the run's remote, and git's flags after `--`
// dropped or taken for gh's.
test("gh repo clone clones the run's remote into the directory it names, with git's flags after --", () => {
  const git = (cwd: string, ...args: string[]) => {
    const result = spawnWithin(
      { ...VCS_COMMAND, part: `git ${args.join(" ")}` },
      [
        "git",
        "-c",
        "user.name=Fixture",
        "-c",
        "user.email=fixture@example.invalid",
        "-c",
        "commit.gpgsign=false",
        ...args,
      ],
      { cwd }
    );
    if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr}`);
    return result.stdout.toString().trim();
  };
  const source = path.join(runs.cloned, "source");
  mkdirSync(source, { recursive: true });
  git(runs.cloned, "init", "-q", "--bare", "-b", "main", "remote.git");
  git(source, "init", "-q", "-b", "main");
  writeFileSync(path.join(source, "README.md"), "# widgets\n");
  git(source, "add", "README.md");
  git(source, "commit", "-qm", "init");
  git(source, "commit", "-q", "--allow-empty", "-m", "second");
  git(source, "push", "-q", path.join(runs.cloned, "remote.git"), "main");
  const head = git(source, "rev-parse", "HEAD");
  const work = path.join(runs.cloned, "work");
  mkdirSync(work);
  routed(runs.cloned, [{ match: "^gh repo clone example/widgets( |$)", clone: true }]);

  const named = askIn(work, runs.cloned, "repo", "clone", "example/widgets", "w702", "--", "--depth", "1");
  expect(named.code).toBe(0);
  expect(named.call).toMatchObject({ route: "^gh repo clone example/widgets( |$)", exit: 0 });
  expect(readFileSync(path.join(work, "w702", "README.md"), "utf8")).toBe("# widgets\n");
  expect(git(path.join(work, "w702"), "rev-parse", "HEAD")).toBe(head);
  expect(git(path.join(work, "w702"), "rev-parse", "--is-shallow-repository")).toBe("true");

  const unnamed = askIn(work, runs.cloned, "repo", "clone", "example/widgets");
  expect(unnamed.code).toBe(0);
  expect(git(path.join(work, "widgets"), "rev-list", "--count", "HEAD")).toBe("2");

  const again = askIn(work, runs.cloned, "repo", "clone", "example/widgets", "w702");
  expect(again.code).not.toBe(0);
  expect(again.stderr).toContain("already exists and is not an empty directory");
  expect(existsSync(path.join(work, "w702", "README.md"))).toBe(true);
});
