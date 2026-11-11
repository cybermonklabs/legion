import { afterEach, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";

// GitHub answers the pull request's head as the commit its branch points at, and the worker skill
// has every pushing role read that head after its own push and stop when it is not its commit. A
// stand-in that answers the fixture's first commit for ever stalls every such role: measured on
// 2026-11-06, two runs of the loop that follows an approval pushed their text fixes, read the
// head, found the fixture's
// commit and ended with a WAITING line instead of completing. So the stand-in reads the tip of the
// run's own bare remote at the call, and serves the pull request's diff from it too.
const STANDIN = path.join(import.meta.dir, "gh-standin.ts");
const made: string[] = [];

afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function git(dir: string, ...args: string[]): string {
  const result = Bun.spawnSync(
    ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-C", dir, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null" } }
  );
  if (result.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}

/** A run directory with the pull-request route of rig.sh's worker_fixture. With `fixture` it is
 * the run of a scenario that builds a worker world: a world.json and a bare remote of its own
 * whose issue branch holds two commits. Without it, it is the run of a scenario that builds
 * none, which has neither — rig.sh writes the world and opens the remote together
 * (worker_fixture), so no run of this rig has one and not the other. */
function runDir(options: { fixture: boolean }): { dir: string; first: string; second: string } {
  const dir = mkdtempSync(path.join(tmpdir(), "gh-standin-"));
  made.push(dir);
  const work = path.join(dir, "work");
  mkdirSync(work);
  git(work, "init", "-q", "-b", "legion/LWEVAL-1");
  writeFileSync(path.join(work, "greet.ts"), "export const greet = () => 1;\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "the approved head");
  const first = git(work, "rev-parse", "HEAD");
  writeFileSync(path.join(work, "docs.md"), "the text fix\n");
  git(work, "add", "-A");
  git(work, "commit", "-qm", "docs: the text fix");
  const second = git(work, "rev-parse", "HEAD");
  git(work, "update-ref", "refs/heads/legion/LWEVAL-1", first);
  if (options.fixture) {
    git(dir, "clone", "-q", "--bare", work, path.join(dir, "remote.git"));
    git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", first);
    writeFileSync(
      path.join(dir, "world.json"),
      JSON.stringify({ key: "LWEVAL-1", branch: "legion/LWEVAL-1", base: "main", head: first })
    );
  }
  // The routes rig.sh's fixture_write writes, in the shapes it writes them: the pull request as
  // one object for `gh pr view`, the same object in a list for `gh pr list`, which is GitHub's
  // answer to a listing of the open pull requests of a branch, the frozen diff, and the edit
  // route a body edit needs a match of before the stand-in keeps the new body.
  const pull = { number: 1001, headRefOid: first, head: { sha: first }, body: "a body" };
  writeFileSync(
    path.join(dir, "fixtures.json"),
    JSON.stringify({
      routes: [
        { match: "^gh pr view", stdout: pull },
        { match: "^gh pr list", stdout: [pull] },
        { match: "^gh pr diff", stdout: "the frozen diff\n" },
        { match: "^gh pr edit", stdout: "https://example.invalid/pull/1001#issuecomment-1" },
      ],
    })
  );
  return { dir, first, second };
}

const headOf = (dir: string) =>
  Bun.spawnSync(
    ["bun", STANDIN, "gh", "pr", "view", "1001", "--json", "headRefOid", "--jq", ".headRefOid"],
    {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
    }
  )
    .stdout.toString()
    .trim();

// Catches: a stand-in that answers the fixture's commit after the role pushed, so every pushing
// role reads a head that is not its own and stops.
test("the pull request's head is the tip of the run's own remote", () => {
  const { dir, first, second } = runDir({ fixture: true });
  expect(headOf(dir)).toBe(first);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(headOf(dir)).toBe(second);
});

// Catches: a stand-in that refuses or empties the answer for a run with no worker fixture of its
// own, which every scenario that builds no world is.
test("a run with no worker fixture keeps the route's own head", () => {
  const { dir, first } = runDir({ fixture: false });
  expect(headOf(dir)).toBe(first);
});

/** What the stand-in answers for `gh <args...>` in run directory `dir`, as `by` if the run names
 * an agent. */
const answer = (dir: string, args: string[], by?: string) =>
  Bun.spawnSync(["bun", STANDIN, "gh", ...args], {
    env: {
      ...process.env,
      SKILL_SCENARIO_RUN: dir,
      ...(by === undefined ? {} : { SKILL_SCENARIO_AGENT: by }),
    },
  })
    .stdout.toString()
    .trim();

// Catches: a stand-in that answers the diff the fixture froze after another role pushed to the
// pull request, so a reviewer reading the fix the implementer pushed sees the change without it
// and resolves nothing. The run's own remote is the fact, as it is for the head.
test("the pull request's diff is the diff of the run's own remote", () => {
  const { dir, second } = runDir({ fixture: true });
  const remote = path.join(dir, "remote.git");
  git(
    remote,
    "update-ref",
    "refs/heads/main",
    git(remote, "rev-parse", "refs/heads/legion/LWEVAL-1")
  );
  expect(answer(dir, ["pr", "diff", "1001"])).toBe("");
  git(remote, "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(answer(dir, ["pr", "diff", "1001"])).toContain("+the text fix");
  expect(answer(dir, ["pr", "diff", "1001", "--name-only"])).toBe("docs.md");
});

// Catches: a stand-in that drops the run's world for a scenario that builds none, which would
// crash every `gh pr diff` of a run with no world.json instead of answering its route.
test("a run with no worker fixture keeps the route's own diff", () => {
  expect(answer(runDir({ fixture: false }).dir, ["pr", "diff", "1001"])).toBe("the frozen diff");
});

// Catches: a stand-in that reads a world.json it cannot parse as a run with no world of its own.
// The two readings serve different answers: a run that builds no world is served the fixture's
// frozen head and diff, which is right for it and is silence for a run whose world the rig wrote
// wrongly — a pushing role would read a head that is not its own and stop, and a reviewer the
// diff without the fix it was sent to read. So only a missing file is "no world", and a file that
// does not parse exits with an error naming it. The second world below is the one that exercises
// `base`, which rig.sh's fixture_write writes into every world and this schema defaults for none.
test("a world.json that does not parse exits with an error, and one with no base too", () => {
  for (const text of ["{not json", JSON.stringify({ branch: "legion/LWEVAL-1" })]) {
    const { dir } = runDir({ fixture: true });
    writeFileSync(path.join(dir, "world.json"), text);
    const call = Bun.spawnSync(["bun", STANDIN, "gh", "pr", "view", "1001"], {
      env: { ...process.env, SKILL_SCENARIO_RUN: dir },
    });
    expect(call.exitCode).toBe(2);
    expect(call.stderr.toString()).toContain("world.json is no world");
  }
});

// Catches: a stand-in that records no agent, so a run that boots one agent per role cannot tell
// which role made a call and the score reads one role's work as the other's.
test("each call records the agent the run named, and none when it names one", () => {
  const { dir } = runDir({ fixture: true });
  answer(dir, ["pr", "view", "1001"], "implementer");
  answer(dir, ["pr", "view", "1001"]);
  const recorded = readFileSync(path.join(dir, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line).by);
  expect(recorded).toEqual(["implementer", undefined]);
});

// Catches: a stand-in with no answer for `gh api repos/<o>/<r>/compare/<a>...<b>`, which the rule
// has the implementer run to show its push changes no code that runs (review-threads.md step 2).
// The real GitHub API answers that form — measured on 2026-11-10, `gh api
// repos/cybermonklabs/legion/compare/756789f8...main --jq '{status,ahead_by,behind_by}'` exited 0
// — while this stand-in answered `gh: Not Found (HTTP 404)`, and the implementer's comparison
// failed in all 10 text-fixes head runs of that batch. The two heads a comparison names are the
// run's own, so it is read from the run's remote and no route serves it.
test("a comparison of two commits is read from the run's own remote", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const compared = answer(dir, [
    "api",
    `repos/example/widgets/compare/${first}...${second}`,
    "--jq",
    "[.status, .ahead_by, .behind_by, (.files | length), .files[0].filename] | @tsv",
  ]);
  expect(compared).toBe(["ahead", 1, 0, 1, "docs.md"].join("\t"));
  // The other way round is the same comparison read from the other side, as GitHub reads it.
  expect(
    answer(dir, [
      "api",
      `repos/example/widgets/compare/${second}...${first}`,
      "--jq",
      "[.status, .ahead_by, .behind_by, (.files | length)] | @tsv",
    ])
  ).toBe(["behind", 0, 1, 0].join("\t"));
  // A comparison of one commit with itself changes nothing, which is what the fix's own
  // comparison looks like when the implementer names the same head twice.
  expect(
    answer(dir, [
      "api",
      `repos/example/widgets/compare/${first}...${first}`,
      "--jq",
      "[.status, .ahead_by, .behind_by, (.files | length)] | @tsv",
    ])
  ).toBe(["identical", 0, 0, 0].join("\t"));
});

// Catches: a comparison answered out of the run's world for refs the remote has not got, which
// would hand an agent counts of a comparison nobody made. GitHub answers 404 for a comparison of
// commits a repository does not hold, and so does the stand-in, through the routes.
test("a comparison the run's remote cannot resolve is 404, as GitHub answers it", () => {
  const { dir } = runDir({ fixture: true });
  const call = Bun.spawnSync(
    ["bun", STANDIN, "gh", "api", "repos/example/widgets/compare/deadbeef...cafed00d"],
    { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
  );
  expect(call.exitCode).toBe(1);
  expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
});

// Catches: a comparison whose files carry no `patch`, which is the field a reviewer reads the
// change out of. It was absent through the batch of 2026-11-10 — a reviewer that asked for the
// patch got `null` and one fell back to `jj file show`, working around the rig on the read step
// 3 of review-threads.md rests on. GitHub's `patch` is the file's hunks alone, with no
// `diff --git` or `index` header above them.
test("each file of a comparison carries the hunks of its own diff", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const patch = answer(dir, [
    "api",
    `repos/example/widgets/compare/${first}...${second}`,
    "--jq",
    ".files[0].patch",
  ]);
  expect(patch).toStartWith("@@ -0,0 +1 @@");
  expect(patch).toContain("+the text fix");
});

// Catches: a stand-in with no answer for the diff media type, which the API documents for this
// same comparison and which a reviewer asks for when it wants the change rather than the
// counts. The answer is the diff itself, never the JSON.
test("a comparison asked for as a diff is the unified diff", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const diff = answer(dir, [
    "api",
    "-H",
    "Accept: application/vnd.github.v3.diff",
    `repos/example/widgets/compare/${first}...${second}`,
  ]);
  expect(diff).toStartWith("diff --git a/docs.md b/docs.md");
  expect(diff).toContain("+the text fix");
});

// Catches: a compare ref handed to `git` as it was written. A ref is argv, and `git` reads a
// leading `-` as an option wherever it falls: measured on 2026-11-11, `git -C <bare> diff
// --no-renames "--output=<path>...<sha>"` created the file `<path>...<sha>` while it parsed
// its options and only then failed for want of a work tree, so the ref wrote a file of the
// caller's choosing. The diff media type is the call that reaches `git diff` first. The
// stand-in refuses such a ref instead, and the call falls to the routes, whose 404 is GitHub's
// answer for a ref it has not got.
test("a compare ref that reads as an option is refused, not run", () => {
  const { dir, first } = runDir({ fixture: true });
  const written = path.join(dir, "ref-wrote-this");
  const call = Bun.spawnSync(
    [
      "bun",
      STANDIN,
      "gh",
      "api",
      "-H",
      "Accept: application/vnd.github.v3.diff",
      `repos/example/widgets/compare/--output=${written}...${first}`,
    ],
    { env: { ...process.env, SKILL_SCENARIO_RUN: dir } }
  );
  expect(call.exitCode).toBe(1);
  expect(call.stderr.toString()).toContain("gh: Not Found (HTTP 404)");
  expect(existsSync(`${written}...${first}`)).toBe(false);
});

// Catches: a stand-in that answers a listing with the fixture's own object untouched. `gh pr
// list` answers a list of the objects `gh pr view` answers one of (rig.sh's fixture_write), and
// the worker skill has a role with no pull request number find it that way, so a role that
// listed the pull request after a fix push read the head the fixture froze — the very head the
// rule has it compare its push against — and a listing after a body edit read the body before
// it. Every rule over an answer's object is a rule over a listed one.
test("a listing's elements each carry the run's own head, the kept body and the chosen fields", () => {
  const { dir, first, second } = runDir({ fixture: true });
  const listed = (fields: string) => answer(dir, ["pr", "list", "--json", fields, "--jq", "@json"]);
  expect(JSON.parse(listed("number,headRefOid"))).toEqual([{ number: 1001, headRefOid: first }]);
  git(path.join(dir, "remote.git"), "update-ref", "refs/heads/legion/LWEVAL-1", second);
  expect(JSON.parse(listed("headRefOid"))).toEqual([{ headRefOid: second }]);
  answer(dir, ["pr", "edit", "1001", "--body", "the edited body"]);
  expect(JSON.parse(listed("body"))).toEqual([{ body: "the edited body" }]);
});
