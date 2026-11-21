import { beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { DAEMON_MODULE, workerPane } from "./daemon-pane";
import {
  baseOf,
  RED_KEY,
  RED_VARIANT_FACTS,
  RED_VARIANT_WORLDS,
  RED_WORLDS,
  redWorlds,
  runSourced,
  sourced,
} from "./red-at-the-head.fixtures";

// red-at-the-head.sh, the red-at-the-head world rig.sh sources (its header): the world's facts and
// workflows, worker_fixture's pull request, red_fixture's red, the routes the gh stand-in answers
// from, the model of the red, the rig's report of world `l`'s check and the settlement of the
// re-run. Each row sources the library and calls its functions, with jj stood in for; what the
// rig's own commands do with them (run_red_at_the_head, cmd_run, cmd_batch) is rig.test.ts's, the
// scorer's half of the scenario score.test.ts's, and the stand-in's sequencing gh-standin.test.ts's.
// The worlds the rows build, and the library's prelude, are red-at-the-head.fixtures.ts's, which
// rig.test.ts builds the reviewer's variants with too.
// Every row runs the library and the stand-in as real processes. On a loaded machine one row takes
// longer than bun's 5 s default, so each row gets one minute.
setDefaultTimeout(60_000);

const root = path.resolve(import.meta.dir, "../../../..");
const work = temporaryDirectory("skill-scenarios-red-at-the-head");
const operatorHome = path.join(work, "operator-home");
// The home every child gets, made in a hook, below the one the reservation registered: a run that
// selects no row of this file makes none.
beforeAll(() => {
  mkdirSync(operatorHome);
});
/** The variables every child process of this file gets, as rig.test.ts gives its own: the caller's
 * PATH, a HOME of the test's own, the caller's mise data and config directories when the caller
 * names them (a tool on PATH can be a mise shim), and PUBLIC_LEAK_CHECK and
 * LEGION_LEAK_CHECK_TAG when the caller has them, since worker_fixture pushes into its bare remote
 * through whatever git is first on PATH, a Legion pod's leak guard among them. */
const miseData =
  process.env.MISE_DATA_DIR ??
  (process.env.XDG_DATA_HOME === undefined
    ? undefined
    : path.join(process.env.XDG_DATA_HOME, "mise"));
const childEnv = {
  PATH: process.env.PATH ?? "",
  HOME: operatorHome,
  ...(miseData === undefined ? {} : { MISE_DATA_DIR: miseData }),
  ...(process.env.XDG_CONFIG_HOME === undefined
    ? {}
    : { XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME }),
  ...(process.env.PUBLIC_LEAK_CHECK === undefined
    ? {}
    : { PUBLIC_LEAK_CHECK: process.env.PUBLIC_LEAK_CHECK }),
  ...(process.env.LEGION_LEAK_CHECK_TAG === undefined
    ? {}
    : { LEGION_LEAK_CHECK_TAG: process.env.LEGION_LEAK_CHECK_TAG }),
};

/** Runs SCRIPT under bash, after the library and rig.sh's note and fail are sourced
 * (red-at-the-head.fixtures.ts's `sourced`), with ARGS as $1... in an environment of PATH and the
 * test's own HOME, and ENV beside them. */
function bash(script: string, args: string[] = [], env: Record<string, string> = {}) {
  return runSourced(script, args, { ...childEnv, ...env });
}
/** bash as a child this test awaits, never a synchronous one, so a listener of the row answers
 * what the script publishes while it runs. */
async function bashAsync(script: string, args: string[], env: Record<string, string> = {}) {
  const child = Bun.spawn(["bash", "-c", `${sourced}\n${script}`, "-", ...args], {
    env: { ...childEnv, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { code, stdout, stderr };
}
let dirs = 0;
/** A new directory under the test's work directory. */
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}

/** The rig's one table of the worlds (jq/worlds.json, which red_world and red_workflows read): each
 * world's facts and the name of its red's lane, and each lane. */
const RED_TABLE: {
  worlds: Record<
    string,
    { after: "green" | "red"; aggregate: boolean; over_lane: boolean; lane: string }
  >;
  lanes: Record<string, { check: string; job: string }>;
} = JSON.parse(readFileSync(path.join(import.meta.dir, "jq", "worlds.json"), "utf8"));
/** Each world's red check, the job that carries it, and what the head settles at once the re-run
 * is pressed (red_fixture), from the table: its lane's check and job, and its `after`. In i and j
 * (`aggregate`) the head also carries `aggregate`, the aggregate check `ci` the base requires (red_model),
 * red over the failing lane and settled with it; world.json's `check` stays the lane. In g
 * (`over_lane`) the red check is that aggregate, and its failing lane is a red check run of its own
 * beside it (`aggregate` names the lane, the lane's job, there). */
const RED_WORLD = Object.fromEntries(
  Object.entries(RED_TABLE.worlds).map(([world, facts]) => {
    const { check, job } = RED_TABLE.lanes[facts.lane] as { check: string; job: string };
    const aggregate = facts.aggregate ? "ci" : facts.over_lane ? job : undefined;
    return [
      world,
      { check, job, after: facts.after, ...(aggregate === undefined ? {} : { aggregate }) },
    ];
  })
) as Record<string, { check: string; job: string; after: "green" | "red"; aggregate?: string }>;

const RED_REPO = "example/widgets";
const RED_PR = 1003;
const RED_TITLE = `feat: greet() (${RED_KEY})`;
/** red_fixture's ids for this pull request: the run that failed, its job, and the check runs —
 * lint, typecheck, the world's red, and the attempt the re-run makes. */
const RED_RUN_ID = 7_000_000 + RED_PR;
const RED_JOB_ID = 8_000_000 + RED_PR;
const RED_ID = { lint: 9_000_000 + RED_PR - 2, typecheck: 9_000_000 + RED_PR - 1 };
const RED_RED_ID = 9_000_000 + RED_PR;
/** The aggregate check of worlds i and j; the attempt the re-run makes of it is the next id. */
const RED_CI_ID = RED_RED_ID + 3;
/** The check runs and the jobs the re-run of the failed jobs carries to attempt 2 where the run of
 * the red holds lint and typecheck (every world but f): GitHub gives each carried job an id and a
 * check run of its own (measured, routes.jq's red_model). */
const RED_CARRIED = { lint: RED_RED_ID + 5, typecheck: RED_RED_ID + 6 };
const RED_CARRIED_JOB = { lint: RED_JOB_ID + 7, typecheck: RED_JOB_ID + 8 };
const RED_RUN_URL = `https://github.com/${RED_REPO}/actions/runs/${RED_RUN_ID}`;
const ghStandin = path.join(import.meta.dir, "gh-standin.ts");

/** This file's worlds (red-at-the-head.fixtures.ts), each in a directory of the file's own:
 * builtWorld, ownFixture for a row that writes into its world, and redFixture for the rows that
 * only read one, which share it. */
const { builtWorld, ownFixture, redFixture } = redWorlds(freshDir, childEnv);
/** The routes of a run's fixtures.json, each by its match and its marks. */
type Route = {
  match: string;
  head_read?: string;
  repo_refusal?: boolean;
  publish?: unknown;
  press?: boolean;
};
const routesOf = (run: string): Route[] =>
  JSON.parse(readFileSync(path.join(run, "fixtures.json"), "utf8")).routes;
/** The match of the one route of a run that `pick` takes. */
function routeOf(run: string, pick: (route: Route) => boolean): string {
  const found = routesOf(run).filter(pick);
  if (found.length !== 1) throw new Error(`${found.length} routes of ${run} answer the pick`);
  return (found[0] as Route).match;
}
/** The head and the base of world `a`'s pull request RED_PR, read from its world.json: its
 * commits are dated by the run's index and the batch's start, so every build makes these two. The
 * base holds the world's own workflows (red_workflows), so a world whose workflows differ from
 * `a`'s has a base and a head of its own, which a row over it reads from its world.json. Read at
 * load, before the suite makes its temporary directory, so from a directory of its own. */
const { head: RED_HEAD, base_sha: RED_BASE }: { head: string; base_sha: string } = (() => {
  const heads = mkdtempSync(path.join(tmpdir(), "skill-scenarios-heads-"));
  try {
    return JSON.parse(readFileSync(path.join(builtWorld("a", heads), "world.json"), "utf8"));
  } finally {
    rmSync(heads, { recursive: true, force: true });
  }
})();
/** The head and the base of a run's own world, from its world.json: a row over a world other
 * than `a` reads them here, since each world's base holds its own workflows. */
const shasOf = (run: string): { head: string; base_sha: string } =>
  JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"));

/** One `gh` call of the role's, through the stand-in rig.sh puts on its PATH. The environment is
 * the test's own, which names no Envoy, so a route that publishes records that it could not and
 * nothing leaves this machine; the row that reads a settlement names a listener of its own. Its
 * output is read byte for byte, a BOM at its start kept (a job log starts with one), where
 * Response's text() would drop it. */
async function gh(run: string, args: string[], env: Record<string, string> = {}) {
  const child = Bun.spawn(["bun", ghStandin, "gh", ...args], {
    env: { ...childEnv, SKILL_SCENARIO_RUN: run, ...env },
    stdout: "pipe",
    stderr: "pipe",
  });
  const read = async (stream: ReadableStream<Uint8Array>) =>
    new TextDecoder("utf-8", { ignoreBOM: true }).decode(await new Response(stream).arrayBuffer());
  const [stdout, stderr, exit] = await Promise.all([
    read(child.stdout),
    read(child.stderr),
    child.exited,
  ]);
  return { stdout, stderr, exit };
}
const outputLines = (stdout: string) => stdout.split("\n").filter((line) => line !== "");
/** What the stand-in recorded of each call of a run, in the order it answered them; a watch of a
 * run in progress is recorded before it waits, with no exit. */
type Recorded = { argv: string[]; route: string | null; exit: number | null };
function ghCalls(run: string): Recorded[] {
  return readFileSync(path.join(run, "calls.jsonl"), "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as Recorded);
}

/** The query the page gives for the counted set at a named head (red-at-the-head.md), read from
 * the page as a role copies it, with the world's owner and repository, so an edit of the page
 * reaches every row that sends it. */
const RED_ROLLUP_QUERY = (() => {
  const page = readFileSync(
    path.join(root, "skills/legion-worker/references/red-at-the-head.md"),
    "utf8"
  );
  const call = /^gh api graphql --paginate -F oid=<head> -f query='([^']*)'$/m.exec(page);
  if (call === null) throw new Error("red-at-the-head.md gives no query for the counted set");
  return (call[1] as string).replace('"<owner>"', '"example"').replace('"<repo>"', '"widgets"');
})();
/** One GraphQL read of the counted set at the head, as the page sends it, with `jq` over the
 * answer. */
const rollupRead = (run: string, filter: string) =>
  gh(run, [
    "api",
    "graphql",
    "--paginate",
    "-F",
    `oid=${shasOf(run).head}`,
    "-f",
    `query=${RED_ROLLUP_QUERY}`,
    "--jq",
    filter,
  ]);

/** The four reads of the head the page has a role make, each as a verdict per check: the three
 * reads of the counted set — the rollup of `gh pr view`, the rows of `gh pr checks` and the
 * GraphQL rollup of the head — and the check runs of the commit, which answer every run at it,
 * the latest id of a name being the one the counted set shows. */
async function readHead(run: string) {
  const rollup = await gh(run, [
    "pr",
    "view",
    String(RED_PR),
    "--json",
    "statusCheckRollup",
    "--jq",
    ".statusCheckRollup[] | [.name,.conclusion] | @tsv",
  ]);
  const graphql = await rollupRead(
    run,
    ".data.repository.object.statusCheckRollup.contexts.nodes[] | [.name,.conclusion] | @tsv"
  );
  const checks = await gh(run, ["pr", "checks", String(RED_PR)]);
  const runs = await gh(run, [
    "api",
    "--paginate",
    `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs?filter=all&per_page=100`,
    "--jq",
    ".check_runs[] | [.id,.name,.conclusion] | @tsv",
  ]);
  const latest: Record<string, { id: number; verdict: string }> = {};
  for (const line of outputLines(runs.stdout)) {
    const [id, name, conclusion] = line.split("\t");
    const seen = latest[name];
    const entry = { id: Number(id), verdict: conclusion === "success" ? "pass" : "fail" };
    if (seen === undefined || seen.id < entry.id) latest[name] = entry;
  }
  const verdict = (stdout: string, pass: string) =>
    Object.fromEntries(
      outputLines(stdout).map((line) => {
        const [name, answer] = line.split("\t");
        return [name, answer === pass ? "pass" : "fail"];
      })
    );
  return {
    rollup: verdict(rollup.stdout, "SUCCESS"),
    graphql: verdict(graphql.stdout, "SUCCESS"),
    checks: verdict(checks.stdout, "pass"),
    checksExit: checks.exit,
    checkRuns: Object.fromEntries(
      Object.entries(latest).map(([name, { verdict: answer }]) => [name, answer])
    ),
    checkRunRows: outputLines(runs.stdout),
  };
}
/** The verdict the head carries under each check, before the re-run and after it. The aggregate
 * of worlds i and j carries the verdict of the lane under it, since the re-run settles both. */
function headIs(world: string, when: "before" | "after"): Record<string, string> {
  const { check, after, aggregate } = RED_WORLD[world];
  const other: Record<string, string> = { lint: "pass", typecheck: "pass" };
  // The spent head's e2e check: an earlier run of it failed, and the one that replaced it passed.
  if (world === "e") other.e2e = "pass";
  const verdict = when === "after" && after === "green" ? "pass" : "fail";
  return {
    ...other,
    [check]: verdict,
    ...(aggregate === undefined ? {} : { [aggregate]: verdict }),
  };
}
/** What the commit's check runs answer, which is headIs but for world `k`: the by-hand run's
 * failure of the same check name is the latest run of it there, before the re-run and after it,
 * and the counted set holds neither. */
function runsAre(world: string, when: "before" | "after"): Record<string, string> {
  if (world !== "k") return headIs(world, when);
  return { ...headIs(world, when), [RED_WORLD[world].check]: "fail" };
}
/** The settlement the Envoy listener renders for a state of the head, as red_fixture writes it
 * for the re-run to publish and for the rig's own report of the check: `notify` is the id of the
 * run of `test (notify)` that state holds, the red's own or the attempt the re-run made, and `ci`
 * the id of the run of the aggregate that state holds, in worlds i and j alone. Once the re-run
 * made its attempt, lint and typecheck are the check runs it carried to attempt 2. `head` is the
 * world's own, world `a`'s unless a row names another. */
const settlement = (
  notify: number,
  passed: string[],
  failed: string[],
  ci?: number,
  head = RED_HEAD
) => ({
  kind: "checks",
  repo: RED_REPO,
  number: String(RED_PR),
  sha: head,
  check_runs: [
    ...(ci === undefined ? [] : [{ name: "ci", id: ci }]),
    { name: "lint", id: notify === RED_RED_ID ? RED_ID.lint : RED_CARRIED.lint },
    { name: "test (notify)", id: notify },
    { name: "typecheck", id: notify === RED_RED_ID ? RED_ID.typecheck : RED_CARRIED.typecheck },
  ],
  generation: 2,
  snapshot: head.slice(0, 8),
  failed: { count: failed.length, checks: failed },
  running: { count: 0, checks: [] },
  passed: { count: passed.length, checks: passed },
  queued: { count: 0, checks: [] },
  cancelled: { count: 0, checks: [] },
  skipped: { count: 0, checks: [] },
  failing_checks: failed.map((name) => ({ name, url: RED_RUN_URL })),
});
/** The bearer the run's own Envoy takes, which paneListener writes to the run's token file and a
 * listener of a row reads back: random, as a real one is, since the stand-in holds it and scrubs
 * it from every line it prints (`@legion/contracts/held-secrets`). */
const RIG_ENVOY_TOKEN = fixtureSecret("envoy");
/** That settlement as the envelope a listener receives it in. */
const envelope = (
  notify: number,
  passed: string[],
  failed: string[],
  ci?: number,
  head = RED_HEAD
) => ({
  authorization: `Bearer ${RIG_ENVOY_TOKEN}`,
  url: "/v1/messages/publish",
  body: {
    topic: `notifications.github.example.widgets.pr.${RED_PR}.checks`,
    source: "github",
    message:
      `checks settled on ${RED_REPO}#${RED_PR} @ ${head.slice(0, 7)}: ` +
      `${passed.length} passed, ${failed.length} failed, 0 cancelled, 0 skipped`,
    payload: JSON.stringify(settlement(notify, passed, failed, ci, head)),
  },
});

/** Names `listener` as the run's own Envoy, with a bearer file, in <run>/pane.env: the file
 * boot_worker writes the agent's environment to, from the batch's services file, and the one the
 * gh stand-in and red_report read the listener from. */
function paneListener(run: string, listener: string) {
  const token = path.join(run, "envoy-token");
  writeFileSync(token, `${RIG_ENVOY_TOKEN}\n`);
  writeFileSync(path.join(run, "pane.env"), `ENVOY_URL=${listener}\nENVOY_TOKEN_FILE=${token}\n`);
}

/** The rig reporting world `l`'s check between the two turns (red_report), with `listener` as
 * the Envoy the run's pane.env names, and what it printed. The shell runs as a child this test
 * awaits, never a synchronous one, so the listener of the row answers the publish while it runs. */
async function redReport(run: string, listener: string) {
  paneListener(run, listener);
  const { code, stdout, stderr } = await bashAsync("R=$1\nred_report", [run]);
  if (code !== 0) throw new Error(`red_report: ${stderr}`);
  return stdout;
}

/** The rig settling the re-run once the turn that pressed it has ended (rerun_settle): every read
 * of the head answers the settled attempt from then on, and the settlement goes to the listener the
 * run's pane.env names, where it names one. The shell runs as a child this test awaits, as
 * redReport's does, so the listener of the row answers the publish while it runs. */
async function settleRerun(run: string) {
  const { code, stdout, stderr } = await bashAsync("R=$1\nrerun_settle", [run]);
  if (code !== 0) throw new Error(`rerun_settle: ${stderr}`);
  return stdout;
}

describe("red_fixture", () => {
  // One row per world, because each world is a seed of its own and a disagreement must name the
  // world it is in. Catches: a world whose reads of the head disagree (a rollup that stays green
  // under a red `gh pr checks`, or a GraphQL rollup that answers the review threads of
  // worker_fixture), a read that answers the same state twice, a settlement that never lands, and
  // a `gh pr checks` whose status does not follow its rows. The counted set and the commit
  // endpoint answer the same checks in every world but `k`, whose commit endpoint holds the
  // by-hand run as well (runsAre). World `l` reports no red at all until the rig reports it, so
  // it has rows of its own below rather than a place in this one.
  const settling = RED_WORLDS.filter((world) => world !== "l");
  // Each world's head is read twice before the re-run, which is both that every read answers the
  // world's own red and the defect the first live run of world a measured: the head's second read
  // in a turn answered the settlement although the role had pressed nothing, so the role read the
  // head as already re-run and pressed no re-run of its own. The head turns on the act, never on
  // the read (gh-standin.ts, `follows`), and the re-run the role presses after its reads still
  // turns it.
  test.each([
    ...settling,
  ])("world %s: reading the head again, with no re-run between, answers red again", async (world) => {
    const run = ownFixture(world);

    const first = await readHead(run);
    const again = await readHead(run);
    for (const read of [first, again]) {
      expect({
        rollup: read.rollup,
        graphql: read.graphql,
        checks: read.checks,
        runs: read.checkRuns,
      }).toEqual({
        rollup: headIs(world, "before"),
        graphql: headIs(world, "before"),
        checks: headIs(world, "before"),
        runs: runsAre(world, "before"),
      });
      expect(read.checksExit).toBe(1);
    }

    // gh 2.98.0 prints nothing for a re-run it requested where no terminal is there (measured).
    const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    expect([rerun.stdout, rerun.exit]).toEqual(["", 0]);
    // The head reads settled once the turn that pressed the re-run has ended and the rig settled
    // it (rerun_settle), never in that turn (the tester's row below).
    await settleRerun(run);
    const after = await readHead(run);
    expect({
      rollup: after.rollup,
      graphql: after.graphql,
      checks: after.checks,
      runs: after.checkRuns,
    }).toEqual({
      rollup: headIs(world, "after"),
      graphql: headIs(world, "after"),
      checks: headIs(world, "after"),
      runs: runsAre(world, "after"),
    });
    expect(after.checksExit).toBe(RED_WORLD[world].after === "green" ? 0 : 1);
  });

  // Catches: a world `e` whose head's runs carry no second attempt, which is where the page
  // reads the spend, and a second attempt in every world, which would spend the re-run of the
  // eleven others. It reads the twelve worlds, so it has a bound of its own, about four times
  // its time alone: 51.74 s (the tester, 2026-11-19, at 6b97edd6) and 47.83 s on fold12, each
  // alone on the pod's own PATH with nothing else running on the pod.
  test("the head's runs answer attempt 1 in every world, and the spent head's attempt 2", async () => {
    const seen: Record<string, string[]> = {};
    for (const world of RED_WORLDS) {
      const run = redFixture(world);
      const runs = await gh(run, [
        "api",
        `repos/${RED_REPO}/actions/runs?head_sha=${shasOf(run).head}&event=pull_request&per_page=100`,
        "--jq",
        ".workflow_runs[] | [.id,.name,.run_attempt,.conclusion] | @tsv",
      ]);
      seen[world] = outputLines(runs.stdout);
    }
    const red = `${RED_RUN_ID}\tci\t1\tfailure`;
    expect(seen).toEqual({
      ...Object.fromEntries(RED_WORLDS.map((world) => [world, [red]])),
      e: [red, `${RED_RUN_ID - 1}\te2e\t2\tsuccess`],
      // The title check runs in a workflow of its own; lint and typecheck in the `ci` run beside.
      f: [`${RED_RUN_ID}\ttitle\t1\tfailure`, `${RED_RUN_ID + 2}\tci\t1\tsuccess`],
    });
  }, 210_000);

  // The page reads the spend on each earlier attempt of a run whose `run_attempt` is above 1.
  // Catches: a world `e` whose attempt read answers no jobs (the catch-all's runs list), which
  // leaves a role following the page unable to judge the spend, and an attempt answer in every
  // world. It reads the twelve worlds, so it has a bound of its own, about four times its time
  // alone: 50.14 s on fold12 (2026-11-19), alone on the pod's own PATH with nothing else running.
  test("the spent head's attempt 1 answers the superseded e2e failure, in world e alone", async () => {
    const seen: Record<string, string[]> = {};
    for (const world of RED_WORLDS) {
      const jobs = await gh(redFixture(world), [
        "api",
        "--paginate",
        `repos/${RED_REPO}/actions/runs/${RED_RUN_ID - 1}/attempts/1/jobs?per_page=100`,
        "--jq",
        ".total_count, (.jobs[] | [.name,.conclusion] | @tsv)",
      ]);
      seen[world] = outputLines(jobs.stdout);
    }
    const attempt = ["1", "e2e\tfailure"];
    const answered = RED_WORLDS.filter((world) => seen[world].join("\n") === attempt.join("\n"));
    expect(seen.e).toEqual(attempt);
    expect(answered).toEqual(["e"]);
  }, 210_000);

  // The commit endpoint of world `e` keeps the superseded e2e runs, which the page lets no role
  // judge the spend on. Catches: a seed that moved them out with the spend, which would leave the
  // world unable to tell a role judging there from one judging on the head's runs, and one that
  // carries them in every world.
  test("the spent head alone carries the superseded runs the commit endpoint answers", async () => {
    const spent = await readHead(redFixture("e"));
    const fresh = await readHead(redFixture("a"));
    expect(spent.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID - 4}\te2e\tfailure`,
      `${RED_RED_ID - 3}\te2e\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);
    expect(fresh.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);
  });

  // Catches: a read of another commit's check runs, the base's among them, answered with the
  // head's own red, so the base reads as failing the same test and a role takes the world's
  // flake for a defect on the base that no re-run clears (red-at-the-head.md, "When the cause is
  // another tree's"). The tester's round-9 batch at c0ace596 met it: trial-j-nos1-4 read main's
  // check runs at 77671688…, was answered `test (notify)` failure, and pressed no re-run, giving
  // that as its reason. Seven more runs of that batch and of round 6's read `commits/null/…` or
  // `commits/trunk/…` and were answered the same red.
  // On GitHub a ref no commit has is a 422, whose body gh prints on stdout (measured on gh 2.98.0, 2026-11-18).
  // Its bound: 19.74 s alone on the pod's own PATH at fold12 (the implementer, 2026-11-19), the
  // shared worlds a, i and j built in it, so 80 s, about four times that.
  test("a read of a commit other than the head answers none of the head's red", async () => {
    for (const world of ["a", "i", "j"]) {
      const run = redFixture(world);
      // The base, by its sha and by its branch: a commit nothing ran on, which GitHub answers
      // with no check run, exit 0.
      for (const commit of [shasOf(run).base_sha, "main"]) {
        const read = await gh(run, [
          "api",
          `repos/${RED_REPO}/commits/${commit}/check-runs`,
          "--jq",
          '.check_runs[] | select(.conclusion == "failure") | .name',
        ]);
        expect({ world, commit, exit: read.exit, failing: outputLines(read.stdout) }).toEqual({
          world,
          commit,
          exit: 0,
          failing: [],
        });
      }
      // A ref no commit has: GitHub's 422, whose body gh prints on stdout, skipping --jq, and
      // which names no check of the head's.
      const unknown = await gh(run, [
        "api",
        `repos/${RED_REPO}/commits/null/check-runs`,
        "--jq",
        '.check_runs[] | select(.conclusion == "failure") | .name',
      ]);
      expect({
        world,
        exit: unknown.exit,
        stderr: unknown.stderr,
        namesHeadRed: /test \(notify\)|"ci"/.test(unknown.stdout),
      }).toEqual({
        world,
        exit: 1,
        stderr: "gh: No commit found for SHA: null (HTTP 422)\n",
        namesHeadRed: false,
      });
    }
  }, 80_000);

  // Catches: a `--json` read that names a field the world's record lacks, or a field gh does not
  // know, answered exit 0 with the field left out, where GitHub answers every field the command
  // lists and gh 2.98.0 refuses an unknown one. The stand-in's picker (gh-standin.ts, `picked`)
  // kept only the fields the record held. The tester's round-10 batch at 4728c2f7 met it in 11
  // of its first 46 runs, every one a head run: `pr view --json mergeStateStatus` (a-head-2,
  // d-head-1, d-head-5, e-head-4) read the field as missing, `pr view --json files` (d-head-2)
  // exited 1 in the one world where the files decide attribution, and `run view --json
  // runAttempt` (a-head-1, a-head-4, b-head-3, b-head-4, c-head-1, c-head-2), a field gh refuses,
  // exited 0. Each such read must give the field a value, or be refused. The commits read names a
  // field the world does not hold, so it is the one that reaches the picker's missing-field case
  // on a rig that holds the rest: with only the other three, a picker set back to drop a missing
  // field passed this row (the tester's mutation at 76cc2379).
  test("a --json read answers every field it names, or is refused", async () => {
    const run = redFixture("a");
    const reads = [
      ["pr", "view", String(RED_PR), "--repo", RED_REPO, "--json", "mergeStateStatus"],
      ["pr", "view", String(RED_PR), "--repo", RED_REPO, "--json", "files"],
      ["pr", "view", String(RED_PR), "--repo", RED_REPO, "--json", "commits"],
      ["run", "view", String(RED_RUN_ID), "--repo", RED_REPO, "--json", "status,runAttempt"],
    ];
    for (const args of reads) {
      const read = await gh(run, args);
      const fields = args[args.indexOf("--json") + 1].split(",");
      const answered = read.exit === 0 ? (JSON.parse(read.stdout) as Record<string, unknown>) : {};
      const silent = fields.filter((field) => read.exit === 0 && answered[field] === undefined);
      expect({ call: args.join(" "), silent }).toEqual({ call: args.join(" "), silent: [] });
    }
  });

  // Catches: a route for other commits that takes a read of the head by another of its names, its
  // short sha or its branch, or a `status` read of it, each of which answered the head's red
  // before those routes and must answer it still; a base whose status or statuses read as
  // anything but GitHub's for a commit nothing ran on; and a commit the rig does not know that is
  // answered rather than refused, or refused under another name than the one the call gave it.
  test("the head keeps its red under every name, the base reads as no check, another commit is refused", async () => {
    const run = redFixture("a");
    const failing = async (ref: string, endpoint: string) =>
      outputLines(
        (
          await gh(run, [
            "api",
            `repos/${RED_REPO}/commits/${ref}/${endpoint}`,
            "--jq",
            '.check_runs[] | select(.conclusion == "failure") | .name',
          ])
        ).stdout
      );
    // A `status` read of the head answers its combined status, GitHub's for a commit no status was
    // posted to: no check run is in it, so it names no failure.
    expect({
      short: await failing(RED_HEAD.slice(0, 7), "check-runs"),
      branch: await failing(`legion/${RED_KEY}`, "check-runs"),
      ref: await failing(`refs/heads/legion/${RED_KEY}`, "check-runs?filter=all"),
      status: await failing(RED_HEAD, "status"),
    }).toEqual({
      short: ["test (notify)"],
      branch: ["test (notify)"],
      ref: ["test (notify)"],
      status: [],
    });
    const status = await gh(run, ["api", `repos/${RED_REPO}/commits/main/status`]);
    const statuses = await gh(run, [
      "api",
      `repos/${RED_REPO}/commits/${RED_BASE.slice(0, 7)}/statuses`,
    ]);
    // The combined status's other fields (its repository and URLs) are the measured rows' below.
    expect([JSON.parse(status.stdout), JSON.parse(statuses.stdout)]).toMatchObject([
      { state: "pending", statuses: [], total_count: 0, sha: RED_BASE },
      [],
    ]);
    const trunk = await gh(run, ["api", `repos/${RED_REPO}/commits/trunk/check-runs`]);
    expect([trunk.stdout, trunk.stderr, trunk.exit]).toEqual(
      githubAnswer("checkruns-422", "trunk")
    );
  });

  // Catches: a GraphQL rollup read of another commit answered with the head's counted set, the
  // defect of the check-runs row above through the read the page gives for the counted set at a
  // named head (RED_ROLLUP_QUERY), whether the call names the commit in its `oid` field or inline.
  // The base's rollup reads as GitHub's for a commit with no check, a commit the rig does not know
  // as no object at all, and the head's own read, by oid or by its branch, keeps the head's red.
  // It builds three worlds, so it has a bound of its own, about four times its time alone: 22.60 s
  // on fold12 (2026-11-19), alone on the pod's own PATH with nothing else running on the pod.
  test("a GraphQL rollup read of a commit other than the head answers none of the head's red", async () => {
    const unknown = "0123456789abcdef0123456789abcdef01234567";
    const inline =
      'query{repository(owner:"example",name:"widgets"){object(expression:"<commit>"){' +
      "... on Commit{statusCheckRollup{state}}}}}";
    // Each read as a role sends it: the page's query with the commit in its `oid` field, or a
    // query that names the commit inline; the base and the head are the world's own.
    const readsOf = (run: string): Record<string, string[]> => ({
      base: ["-F", `oid=${shasOf(run).base_sha}`, "-f", `query=${RED_ROLLUP_QUERY}`],
      main: ["-f", `query=${inline.replace("<commit>", "main")}`],
      unknown: ["-F", `oid=${unknown}`, "-f", `query=${RED_ROLLUP_QUERY}`],
      head: ["-F", `oid=${shasOf(run).head}`, "-f", `query=${RED_ROLLUP_QUERY}`],
      branch: ["-f", `query=${inline.replace("<commit>", `legion/${RED_KEY}`)}`],
    });
    for (const world of ["a", "i", "j"]) {
      const run = redFixture(world);
      const reads = readsOf(run);
      const objects: Record<string, { statusCheckRollup: { state: string } | null } | null> = {};
      for (const [read, args] of Object.entries(reads)) {
        const answer = await gh(run, [
          "api",
          "graphql",
          ...args,
          "--jq",
          ".data.repository.object",
        ]);
        objects[read] = JSON.parse(answer.stdout);
      }
      expect({
        world,
        base: objects.base,
        main: objects.main,
        unknown: objects.unknown,
        head: objects.head?.statusCheckRollup?.state,
        branch: objects.branch?.statusCheckRollup?.state,
      }).toEqual({
        world,
        base: { statusCheckRollup: null },
        main: { statusCheckRollup: null },
        unknown: null,
        head: "FAILURE",
        branch: "FAILURE",
      });
    }
  }, 100_000);

  // Catches: a read of the runs of another commit or branch, the base's among them, answered with
  // the runs of the head, through the REST read of the runs or `gh run list`, so the base reads as
  // failing the red of the head; and a route for those reads that takes the head's own read, by
  // its sha or by its branch.
  test("a read of the runs of another commit or branch answers no run, and the head's keeps its run", async () => {
    const run = redFixture("a");
    const runs = `repos/${RED_REPO}/actions/runs`;
    const conclusions = async (args: string[]) => outputLines((await gh(run, args)).stdout);
    expect({
      baseSha: await conclusions([
        "api",
        `${runs}?head_sha=${RED_BASE}&per_page=100`,
        "--jq",
        ".workflow_runs[] | .conclusion",
      ]),
      main: await conclusions([
        "api",
        `${runs}?branch=main`,
        "--jq",
        ".workflow_runs[] | .conclusion",
      ]),
      listMain: await conclusions([
        "run",
        "list",
        "--branch",
        "main",
        "--json",
        "conclusion",
        "--jq",
        ".[] | .conclusion",
      ]),
      listBase: await gh(run, ["run", "list", "--commit", RED_BASE]),
      head: await conclusions([
        "api",
        `${runs}?head_sha=${RED_HEAD}&event=pull_request&per_page=100`,
        "--jq",
        ".workflow_runs[] | .conclusion",
      ]),
      headBranch: await conclusions([
        "api",
        `${runs}?branch=legion/${RED_KEY}`,
        "--jq",
        ".workflow_runs[] | .conclusion",
      ]),
      listHead: await conclusions([
        "run",
        "list",
        "--commit",
        RED_HEAD,
        "--json",
        "conclusion",
        "--jq",
        ".[] | .conclusion",
      ]),
    }).toEqual({
      baseSha: [],
      main: [],
      listMain: [],
      listBase: { stdout: "", stderr: "", exit: 0 },
      head: ["failure"],
      headBranch: ["failure"],
      listHead: ["failure"],
    });
  });

  // Catches: a `gh run rerun` that names no run of the head pressing the head's re-run. A re-run
  // of the red's check run id, which trial-d-head-5, trial-e-head-2 and trial-g-head-2 made,
  // settled the head and published its settlement, where GitHub answers the 404 of a run it has
  // not got; `gh run rerun --help`, which trial-i-nos1-1 and trial-j-nos1-1 ran, did the same,
  // where gh prints its help. Neither may move the head, and the re-run of the head's own run
  // still settles it.
  test("a re-run of a check run id is refused and leaves the head red; the head's own run settles it", async () => {
    const run = ownFixture("a");
    const refused = await gh(run, ["run", "rerun", String(RED_RED_ID), "--failed"]);
    const help = await gh(run, ["run", "rerun", "--help"]);
    const before = await readHead(run);
    const pressed = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const after = await readHead(run);
    // Each of the four reads of the head as a verdict per check: the rollup of `gh pr view`, the
    // GraphQL rollup, `gh pr checks` and the check runs of the commit.
    expect({
      refused,
      help: [help.stdout.split("\n")[0], help.stderr, help.exit],
      before: [before.rollup, before.graphql, before.checks, before.checkRuns],
      pressed: [pressed.stdout, pressed.exit],
      after: [after.rollup, after.graphql, after.checks, after.checkRuns],
    }).toEqual({
      refused: {
        stdout: "",
        stderr:
          `failed to get run: HTTP 404: Not Found (https://api.github.com/repos/${RED_REPO}` +
          `/actions/runs/${RED_RED_ID}?exclude_pull_requests=true)\n`,
        exit: 1,
      },
      help: ["Rerun an entire run, only failed jobs, or a specific job from a run.", "", 0],
      before: Array(4).fill(headIs("a", "before")),
      pressed: ["", 0],
      after: Array(4).fill(headIs("a", "after")),
    });
  });

  // Catches: a title check whose log names another title than the pull request's own, which the
  // role reads as a red of some other change; and a check run whose output holds words GitHub
  // Actions never writes there (its output has no title, summary or text: measured by the
  // reviewer on cybermonklabs/legion, 2026-11-19), which a role could read the red from.
  test("the title check's output names the pull request's own title", async () => {
    const run = redFixture("f");
    const title = await gh(run, [
      "pr",
      "view",
      String(RED_PR),
      "--json",
      "title",
      "--jq",
      ".title",
    ]);
    const summary = await gh(run, [
      "api",
      `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs?filter=all`,
      "--jq",
      '.check_runs[] | select(.name == "title") | .output.summary',
    ]);
    const log = await gh(run, ["run", "view", String(RED_RUN_ID), "--log-failed"]);
    const said = `The title "${RED_TITLE}" has no scope: the pattern is <type>(<scope>): <subject>.`;
    expect(title.stdout).toBe(`${RED_TITLE}\n`);
    expect(summary.stdout).toBe("null\n");
    // gh prints a log line as `<job>\t<step>\t<time> <line>`, with GitHub's BOM before the time
    // on the first line of a step (measured, runview-job-log).
    expect(log.stdout).toBe(
      `title\tCheck the pull request title\t\uFEFF2026-03-04T10:00:00.0000000Z ${said}\n`
    );
  });

  // Catches: a world h whose log read answers a line like every other world's, which the page's
  // "a read that fails is recorded as failed" has nothing to record; an error in other words than
  // gh 2.98.0 prints for GitHub's 410 on a log past retention (measured by the lead on 2026-11-18,
  // run 30492596243 of cybermonklabs/legion): the reason `Server Error` and the URL of the log gh
  // asked for, the run's or, where the call names one, its attempt's; a REST read of the job's
  // log refused where GitHub answers its 410; and a world whose live job log answers nothing.
  test("world h's log reads fail as gh prints GitHub's 410, where world a's answer the failing line", async () => {
    const answer = async (run: string, args: string[]) => {
      const read = await gh(run, args);
      return [read.stdout, read.stderr, read.exit];
    };
    const red = ["run", "view", String(RED_RUN_ID)];
    const restLog = ["api", `repos/${RED_REPO}/actions/jobs/${RED_JOB_ID}/logs`];
    const runUrl = `https://api.github.com/repos/${RED_REPO}/actions/runs/${RED_RUN_ID}`;
    const gone = (logs: string) => [
      "",
      `failed to get run log: HTTP 410: Server Error (${runUrl}/${logs})\n`,
      1,
    ];
    const h = redFixture("h");
    expect({
      failed: await answer(h, [...red, "--log-failed"]),
      whole: await answer(h, [...red, "--log"]),
      attempt: await answer(h, [...red, "--attempt", "1", "--log-failed"]),
      job: await answer(h, ["run", "view", "--job", String(RED_JOB_ID), "--log"]),
      rest: await answer(h, restLog),
    }).toEqual({
      failed: gone("logs"),
      whole: gone("logs"),
      attempt: gone("attempts/1/logs"),
      job: gone("logs"),
      rest: [
        JSON.stringify({
          message: "Server Error",
          documentation_url:
            "https://docs.github.com/rest/actions/workflow-jobs#download-job-logs-for-a-workflow-run",
          status: "410",
        }),
        "gh: Server Error (HTTP 410)\n",
        1,
      ],
    });
    const a = redFixture("a");
    const at = "2026-03-04T10:00:00.0000000Z";
    const line = "(fail) notify > retries a refused send [3.00ms]";
    expect({
      failed: await answer(a, [...red, "--log-failed"]),
      rest: await answer(a, restLog),
    }).toEqual({
      failed: [`test (notify)\tRun bun test packages/notify\t\uFEFF${at} ${line}\n`, "", 0],
      rest: [
        `\uFEFF${at} ##[group]Run bun test packages/notify\n${at} ${line}\n` +
          `${at} ##[error]Process completed with exit code 1.\n${at} ##[endgroup]\n`,
        "",
        0,
      ],
    });
  });

  // Catches: a job log in other bytes than GitHub sends it and gh 2.98.0 prints it (measured on
  // job 135117329305 of cybermonklabs/legion, 2026-11-18: joblog-live and runview-job-log of the
  // measurements): the REST log without the one BOM at its start, a gh log without the BOM at the
  // first line of each step, a line without GitHub's timestamp, a log that does not end in a
  // newline, and an ESC byte, which gh refuses to print without --allow-escape-sequences. It
  // builds two worlds, so it has a bound of its own, about four times its time alone: 15.47 s on
  // fold12 (2026-11-19), alone on the pod's own PATH with nothing else running on the pod.
  test("a job log reads as GitHub sends it and gh prints it: its BOMs, timestamps and last newline", async () => {
    const BOM = "\uFEFF";
    const stamped = (line: string) => /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{7}Z /.test(line);
    const seen: { world: string; read: string; bom: boolean; stamps: boolean; end: boolean }[] = [];
    const shape = (world: string, read: string, text: string, bom: boolean, lines: string[]) =>
      seen.push({
        world,
        read,
        bom: bom && !text.includes("\u001b"),
        stamps: lines.length > 1 && lines.slice(0, -1).every(stamped),
        end: lines.at(-1) === "",
      });
    for (const world of ["a", "i"]) {
      const run = redFixture(world);
      for (const id of [RED_JOB_ID, RED_JOB_ID + 1]) {
        const read = `api .../actions/jobs/${id}/logs`;
        const text = (await gh(run, ["api", `repos/${RED_REPO}/actions/jobs/${id}/logs`])).stdout;
        const body = text.slice(BOM.length);
        shape(world, read, text, text.startsWith(BOM) && !body.includes(BOM), body.split("\n"));
      }
      for (const args of [
        ["run", "view", String(RED_RUN_ID), "--log-failed"],
        ["run", "view", String(RED_RUN_ID), "--log"],
        ["run", "view", "--job", String(RED_JOB_ID), "--log"],
      ]) {
        const text = (await gh(run, args)).stdout;
        const steps = new Set<string>();
        let boms = true;
        const logs = text.split("\n").map((line, index, all) => {
          if (index === all.length - 1) return line;
          const [job, step, log = ""] = line.split("\t");
          const first = !steps.has(`${job}\t${step}`);
          steps.add(`${job}\t${step}`);
          if (log.startsWith(BOM) !== first || log.slice(1).includes(BOM)) boms = false;
          return first ? log.slice(BOM.length) : log;
        });
        shape(world, args.join(" "), text, boms, logs);
      }
    }
    expect(seen).toEqual(seen.map((read) => ({ ...read, bom: true, stamps: true, end: true })));
  }, 70_000);

  // Catches: a counted set in a shape of the rig's own, which a role that reads it as GitHub
  // answers one — the state, the one page of contexts with the cursor of its last node, and a
  // CheckRun node per check — would read as nothing at all; a node that names no run, which
  // leaves the role nothing to re-run; and fields the page's query does not ask (each node's
  // detailsUrl) answered all the same.
  test("the GraphQL rollup answers the counted set in the shape GitHub answers", async () => {
    const run = redFixture("a");
    const answer = await rollupRead(run, ".data.repository.object.statusCheckRollup");
    // GitHub Actions is the app of every check run of the world (its databaseId on GitHub).
    const checkSuite = {
      app: { databaseId: 15368 },
      workflowRun: { event: "pull_request", databaseId: RED_RUN_ID },
    };
    expect(JSON.parse(answer.stdout)).toEqual({
      state: "FAILURE",
      contexts: {
        totalCount: 3,
        // GitHub's cursor of the third and last node (gh-graphql.ts), base64 of "3".
        pageInfo: { hasNextPage: false, endCursor: "Mw" },
        nodes: [
          {
            __typename: "CheckRun",
            name: "lint",
            databaseId: RED_ID.lint,
            status: "COMPLETED",
            conclusion: "SUCCESS",
            checkSuite,
          },
          {
            __typename: "CheckRun",
            name: "test (notify)",
            databaseId: RED_RED_ID,
            status: "COMPLETED",
            conclusion: "FAILURE",
            checkSuite,
          },
          {
            __typename: "CheckRun",
            name: "typecheck",
            databaseId: RED_ID.typecheck,
            status: "COMPLETED",
            conclusion: "SUCCESS",
            checkSuite,
          },
        ],
      },
    });
  });

  // Catches: a world k whose by-hand failure is of another check name, which no role would read
  // as the head's one re-run already spent, and one the counted set holds too, which would make
  // the two reads agree and leave the world measuring nothing.
  test("world k: the commit endpoint answers a by-hand failure the counted set does not hold", async () => {
    const run = ownFixture("k");
    const before = await readHead(run);
    // Two failing runs of the one check name at the commit, the second a by-hand run's, against
    // one counted run of it.
    expect(before.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
      `${RED_RED_ID + 2}\ttest (notify)\tfailure`,
    ]);
    expect(before.graphql).toEqual(headIs("k", "before"));
    expect(before.checks).toEqual(headIs("k", "before"));
    const counted = await rollupRead(
      run,
      ".data.repository.object.statusCheckRollup.contexts.nodes " +
        '| map(select(.name == "test (notify)")) | [length, .[0].databaseId]'
    );
    expect(JSON.parse(counted.stdout)).toEqual([1, RED_RED_ID]);
    // The by-hand run is a run of its own at that head, which is what puts it outside the set
    // GitHub counts for the pull request: its check run links to the job of that run and sits in
    // that run's check suite, though it names the pull request, as a workflow_dispatch run at the
    // current head of an open pull request does on GitHub (measured, github-answers/README.txt).
    const byHand = await gh(run, [
      "api",
      `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs?filter=all`,
      "--jq",
      `.check_runs[] | select(.id == ${RED_RED_ID + 2}) | [.details_url, .check_suite.id, (.pull_requests | map(.number) | join(","))] | @tsv`,
    ]);
    expect(byHand.stdout).toBe(
      `https://github.com/${RED_REPO}/actions/runs/${RED_RUN_ID + 1}/job/${RED_JOB_ID + 6}\t` +
        `${RED_RUN_ID + 1 - 1_000_000}\t${RED_PR}\n`
    );
    // The re-run clears the counted run and leaves the by-hand one, which is why a role that
    // reads the commit endpoint for the head's state reads it red after a green re-run.
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const after = await readHead(run);
    expect({ graphql: after.graphql, runs: after.checkRuns }).toEqual({
      graphql: headIs("k", "after"),
      runs: runsAre("k", "after"),
    });
  });

  // Catches: a world l whose counted set reports the red from the start, which the role could
  // decide on in its first turn, and one whose commit endpoint holds no failing run, which would
  // leave the role nothing to wait on. red_report is the rig reporting the check between the
  // turns: the counted set reports it, the run learns when, and the settlement wakes the role.
  test("world l: the counted set reports the red only once the rig reports it", async () => {
    const run = ownFixture("l");
    const unreported = await readHead(run);
    expect({
      rollup: unreported.rollup,
      graphql: unreported.graphql,
      checks: unreported.checks,
    }).toEqual({
      rollup: { lint: "pass", typecheck: "pass" },
      graphql: { lint: "pass", typecheck: "pass" },
      checks: { lint: "pass", typecheck: "pass" },
    });
    expect(unreported.checksExit).toBe(0);
    // The commit endpoint holds the failing run all along, which is the read the page forbids
    // deciding on.
    expect(unreported.checkRunRows).toEqual([
      `${RED_ID.lint}\tlint\tsuccess`,
      `${RED_ID.typecheck}\ttypecheck\tsuccess`,
      `${RED_RED_ID}\ttest (notify)\tfailure`,
    ]);

    const received: { authorization: string | null; url: string; body: unknown }[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        received.push({
          authorization: request.headers.get("authorization"),
          url: new URL(request.url).pathname,
          body: await request.json(),
        });
        return Response.json({ ok: true });
      },
    });
    const at = new Date();
    try {
      await redReport(run, `http://127.0.0.1:${server.port}`);
    } finally {
      server.stop(true);
    }
    expect(received).toEqual([envelope(RED_RED_ID, ["lint", "typecheck"], ["test (notify)"])]);
    const reportedAt = readFileSync(path.join(run, "reported-at"), "utf8");
    expect(reportedAt).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z\n$/);
    expect(new Date(reportedAt.trim()).getTime()).toBeGreaterThanOrEqual(
      Math.floor(at.getTime() / 1000) * 1000
    );

    const reported = await readHead(run);
    expect({ rollup: reported.rollup, graphql: reported.graphql, checks: reported.checks }).toEqual(
      {
        rollup: headIs("l", "before"),
        graphql: headIs("l", "before"),
        checks: headIs("l", "before"),
      }
    );
    expect(reported.checksExit).toBe(1);

    const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    expect(rerun.exit).toBe(0);
    await settleRerun(run);
    const after = await readHead(run);
    expect({
      rollup: after.rollup,
      graphql: after.graphql,
      checks: after.checks,
      runs: after.checkRuns,
    }).toEqual({
      rollup: headIs("l", "after"),
      graphql: headIs("l", "after"),
      checks: headIs("l", "after"),
      runs: runsAre("l", "after"),
    });
    expect(after.checksExit).toBe(0);
  });

  // Catches: a re-run that settles the moment it is pressed. On GitHub the re-run's attempt is
  // queued, then in progress, for minutes, and GitHub refuses a second press while the run still
  // runs (red-at-the-head.md, measured on run 43686474806). A stand-in whose reads follow the
  // press at once, and whose press publishes the settlement, hands a role that reads in the turn
  // that pressed the re-run the result the page has it wait for, and puts the settlement into
  // that same turn, where the scorer reads the role's one read on its wake as a poll (INFRA-1303:
  // j-head-2, j-nos1-3 and l-head-2 of the batch of record at 6a643564, each woken 0.08 to 15 s
  // after its press).
  test("in the turn that pressed it, the re-run's attempt is in progress, unsettled and not pressed again", async () => {
    const run = ownFixture("a");
    expect((await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"])).exit).toBe(0);
    const press = ghCalls(run).find((call) => call.argv[0] === "run" && call.argv[1] === "rerun");
    expect(press !== undefined && "published" in press).toBe(false);
    const runs = await gh(run, [
      "api",
      `repos/${RED_REPO}/actions/runs?head_sha=${RED_HEAD}&event=pull_request&per_page=100`,
      "--jq",
      `.workflow_runs[] | select(.id == ${RED_RUN_ID}) | [.run_attempt, .status, (.conclusion // "")] | @tsv`,
    ]);
    const [attempt, status, conclusion] = (outputLines(runs.stdout)[0] ?? "").split("\t");
    expect({
      exit: runs.exit,
      attempt,
      running: status === "queued" || status === "in_progress",
      conclusion,
    }).toEqual({ exit: 0, attempt: "2", running: true, conclusion: "" });
    expect((await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"])).exit).not.toBe(0);
  });

  // INFRA-1303 D10: catches a stand-in that refuses as unmodeled the log read of an attempt the
  // world holds, `gh run view <run> --attempt <n> --log-failed`, which GitHub answers: j-head-3 of
  // the batch of record at 9c3bd9df read world j's red-again attempt 2 that way and became a rig
  // error. Each attempt's log answers from the same model as the run's own log read: attempt 1
  // holds the red, attempt 2 does not exist before the press, runs in the turn that pressed it,
  // and, once settled, holds what the run's latest log holds, red again in `j` and green in `a`.
  test.each([
    "a",
    "j",
  ] as const)("world %s: each attempt's log is read as the run's log of that attempt", async (world) => {
    const run = ownFixture(world);
    const attemptLog = (attempt: number) =>
      gh(run, [
        "run",
        "view",
        String(RED_RUN_ID),
        "--repo",
        RED_REPO,
        "--attempt",
        String(attempt),
        "--log-failed",
      ]);
    const runLog = () =>
      gh(run, ["run", "view", String(RED_RUN_ID), "--repo", RED_REPO, "--log-failed"]);
    const before = await runLog();
    expect(before.exit).toBe(0);
    expect(before.stdout).toContain("retries a refused send");
    expect(await attemptLog(1)).toEqual(before);
    expect((await attemptLog(2)).stderr).toStartWith("rig: request not modeled: ");
    expect((await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"])).exit).toBe(0);
    expect(await attemptLog(2)).toEqual({
      stdout: "",
      stderr: `run ${RED_RUN_ID} is still in progress; logs will be available when it is complete\n`,
      exit: 1,
    });
    expect(await attemptLog(1)).toEqual(before);
    await settleRerun(run);
    const after = await runLog();
    expect(await attemptLog(2)).toEqual(after);
    expect(after.stdout.includes("retries a refused send")).toBe(RED_WORLD[world].after === "red");
    expect(await attemptLog(1)).toEqual(before);
    expect((await attemptLog(3)).stderr).toStartWith("rig: request not modeled: ");
  }, 120_000);

  // Catches: a pull request body whose check line contradicts the world's Verification document.
  // A reviewer world whose round goes on (world i) holds k1 as `after_merge` (worker_world, through
  // seed.ts tester-proof after-merge-check), and worker_fixture wrote "Check k1 (before_merge):
  // pass … recorded by implementer" into every world's body, a false claim a reviewer rightly
  // requests changes for (INFRA-1303: i-head-5 of the batch of record at 6a643564; 10 of world i's
  // 15 runs named the mismatch). The line an `after_merge` check carries before the merge is
  // pr-body.md's `- Check <block id> (after_merge): not yet run`.
  test("each world's pull request body names k1 as that world's Verification document writes it", async () => {
    const checkLines = async (world: string) =>
      outputLines(
        (
          await gh(redFixture(world), [
            "pr",
            "view",
            String(RED_PR),
            "--repo",
            RED_REPO,
            "--json",
            "body",
            "--jq",
            ".body",
          ])
        ).stdout
      ).filter((line) => line.startsWith("- Check k1 "));
    expect(await checkLines("i")).toEqual(["- Check k1 (after_merge): not yet run"]);
    expect(await checkLines("a")).toEqual([
      expect.stringMatching(/^- Check k1 \(before_merge\): pass at /),
    ]);
  });

  // The routes are the world's in their content alone, so one row runs the page's six calls in
  // every world. Catches: a call of the page that falls through to the stand-in's 404, a read of
  // the counted set answered by worker_fixture's generic graphql route, which holds a review
  // thread list, a check-runs read whose `filter=all` query misses the head's own route and takes
  // the generic one, a jobs read answered by the route for the head's runs, and an aggregate
  // world whose jobs name no failed lane.
  // The row builds the twelve worlds and makes six calls in each, so it has a bound of its own:
  // measured alone, 70.74 s with the pod's own git first on PATH and 47.08 s with plain git first
  // (the tester, 2026-11-18, at a201b17c), and 60.82 s with the pod's own git on the fold of the
  // review of 76cc2379 (2026-11-18), over the suite's 60 s default; 74.31 s on fold12
  // (2026-11-19), alone on the pod's own PATH with nothing else running on the pod. The bound is
  // about four times the slowest.
  test("every call the page names is answered by its own route, in every world", async () => {
    const seen: unknown[] = [];
    for (const world of RED_WORLDS) {
      const run = ownFixture(world);
      await gh(run, [
        "api",
        `repos/${RED_REPO}/actions/runs?head_sha=${RED_HEAD}&event=pull_request`,
        "--jq",
        ".workflow_runs[] | [.id,.event,.conclusion] | @tsv",
      ]);
      await rollupRead(run, ".data.repository.object.statusCheckRollup.state");
      const jobs = await gh(run, [
        "api",
        `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}/jobs`,
        "--jq",
        ".jobs[] | [.id,.name,.conclusion] | @tsv",
      ]);
      await gh(run, ["run", "view", String(RED_RUN_ID), "--log-failed"]);
      await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
      await gh(run, [
        "api",
        "--paginate",
        `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?filter=all&per_page=100`,
        "--jq",
        ".check_runs[] | [.id,.name,.conclusion] | @tsv",
      ]);
      // Each route by the read it answers: the REST reads by the endpoint their match is anchored
      // on, the rollup, the log and the press by the route of the run that answers each.
      const rollup = routeOf(run, (route) => route.head_read === "graphql");
      // The log of the run, read with no attempt named (world h has a route for attempt 1 too).
      const log = routeOf(
        run,
        (route) =>
          route.match.startsWith(`^gh run view ${RED_RUN_ID}(`) &&
          route.match.includes("(?=.*\\s--log") &&
          !route.match.includes("--attempt")
      );
      const press = routeOf(run, (route) => route.press === true);
      const label = (route: string | null) =>
        route === null
          ? "unmodeled"
          : route === rollup
            ? "rollup"
            : route === log
              ? "log"
              : route === press
                ? "press"
                : route.includes(`actions/runs/${RED_RUN_ID}/jobs`)
                  ? "jobs"
                  : route.includes("actions/runs")
                    ? "runs"
                    : route.includes("check-runs")
                      ? "check-runs"
                      : route;
      seen.push({
        world,
        routes: ghCalls(run).map((call) => label(call.route)),
        jobs: jobs.stdout,
      });
    }
    expect(seen).toEqual(
      RED_WORLDS.map((world) => ({
        world,
        routes: ["runs", "rollup", "jobs", "log", "press", "check-runs"],
        // The jobs of the run of the red: the failing lane, and beside it lint and typecheck (in
        // `f` the title check runs alone in a run of its own), and in `g`, `i` and `j` the
        // aggregate `ci` over the lane.
        jobs:
          `${RED_JOB_ID}\t${RED_WORLD[world].job}\tfailure\n` +
          (world === "f"
            ? ""
            : `${RED_JOB_ID + 1}\tlint\tsuccess\n${RED_JOB_ID + 2}\ttypecheck\tsuccess\n`) +
          (["g", "i", "j"].includes(world) ? `${RED_JOB_ID + 3}\tci\tfailure\n` : ""),
      }))
    );
  }, 300_000);

  // Catches: a `--required` read answered by the route for every check, which hands a role every
  // lane where GitHub hands it the required set alone (the review of red-at-the-head.md:86), a
  // required read that stays red after a green re-run, and a read without the flag that lost a
  // check to the new route. The base requires the one aggregate check `ci` (red_fixture), which
  // the heads of worlds `g`, `i` and `j` report; every other head answers gh's own `no required
  // checks reported` and exits 1. It reads the twelve worlds, so it has a bound of its own, about
  // four times its time alone: 68.78 s (the tester, 2026-11-19, at 6b97edd6) and 57.80 s on
  // fold12, each alone on the pod's own PATH with nothing else running on the pod.
  test("gh pr checks --required answers the required check alone, and a read without it every check", async () => {
    const none = {
      stdout: "",
      stderr: `no required checks reported on the 'legion/${RED_KEY}' branch\n`,
      exit: 1,
    };
    /** The job of the check run of `ci` a head first reports, in the worlds whose head reports
     * one: world g's red, and the aggregate over the red lane of i and j, the aggregate job of the
     * run of the red in each; its attempt 2 is the job the re-run makes. */
    const ciJob: Record<string, number> = {
      g: RED_JOB_ID + 3,
      i: RED_JOB_ID + 3,
      j: RED_JOB_ID + 3,
    };
    const seen: unknown[] = [];
    for (const world of RED_WORLDS) {
      const run = ownFixture(world);
      const required = ["pr", "checks", String(RED_PR), "--required"];

      const before = await gh(run, required);
      const every = await gh(run, ["pr", "checks", String(RED_PR)]);
      await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
      await settleRerun(run);
      const after = await gh(run, required);

      // Each route by the read it answers: the required set, the head's checks, the press. The
      // watch of the required set is a route of its own (`checks_watch`), which these reads miss.
      const names = new Map([
        [
          routeOf(
            run,
            (route) =>
              route.match.includes("--required") &&
              !route.match.includes("--json") &&
              route.head_read !== "checks_watch"
          ),
          "required",
        ],
        [
          routeOf(
            run,
            (route) => route.head_read === "checks" && !route.match.includes("--required")
          ),
          "checks",
        ],
        [routeOf(run, (route) => route.press === true), "press"],
      ]);
      seen.push({
        world,
        before,
        every: [
          Object.fromEntries(outputLines(every.stdout).map((line) => line.split("\t").slice(0, 2))),
          every.exit,
        ],
        after,
        routes: ghCalls(run).map((call) => names.get(call.route ?? "") ?? call.route),
      });
    }
    expect(seen).toEqual(
      RED_WORLDS.map((world) => {
        const ci = ciJob[world];
        const url = `${RED_RUN_URL}/job`;
        // The attempt the re-run made of `ci`, which settles as the world settles.
        const settled = headIs(world, "after").ci;
        return {
          world,
          before:
            ci === undefined
              ? none
              : { stdout: `ci\tfail\t0\t${url}/${ci}\t\n`, stderr: "", exit: 1 },
          // World `l`'s counted set has not reported its red at the first read.
          every:
            world === "l" ? [{ lint: "pass", typecheck: "pass" }, 0] : [headIs(world, "before"), 1],
          after:
            ci === undefined
              ? none
              : {
                  stdout: `ci\t${settled}\t0\t${url}/${ci + 2}\t\n`,
                  stderr: "",
                  exit: settled === "fail" ? 1 : 0,
                },
          routes: ["required", "checks", "press", "required"],
        };
      })
    );
  }, 280_000);

  /** The route of one read of the base's required set in `run`: the one route that takes `gh api
   * GET` of `endpoint` under the world's repository. */
  const requiredSetRoute = (run: string, endpoint: string) =>
    routeOf(run, (route) =>
      new RegExp(route.match).test(`gh api GET repos/${RED_REPO}/${endpoint}`)
    );

  // The five reads of the base's required set read nothing of the world (required_set_fixture),
  // so this row runs that function once, over worker_fixture's green fixture, and the row after
  // it checks that every world's fixture carries them. Catches: a read of the base's required
  // set that another route takes, or that falls to the 404: the two the reviewer's prompt gives
  // (packages/daemon/internal/prompts/go/reviewer.md, "CI is yours to read"), and the protection
  // endpoint, the rulesets list and the one ruleset. In the batch of 2026-11-17 the prompt's two
  // reads, with their `--jq`, took worker_fixture's generic check-runs route, whose match their
  // `required_status_checks` text holds, and exited 1, and the rules read without its `--jq`
  // (trial-i-head-3), the protection read (trial-i-head-1) and the rulesets list (trial-i-head-5)
  // answered the 404, so every reviewer read the base as requiring nothing while `gh pr checks
  // --required` named `ci`. Also a route that takes another's path (the branch route the
  // `/protection` below it, the rulesets list the one ruleset, the branch route the rules path or
  // the reverse), and a required check at another path than the one GitHub answers it at for its
  // endpoint, which the prompt's `--jq` reads as nothing.
  test("the base's rules, rulesets, branch and protection each answer the required ci", async () => {
    const run = builtWorld("green");
    expect(
      bash(`R=$1 worker_repo=$2\nrequired_set_fixture '["ci"]' main $3`, [run, RED_REPO, RED_BASE])
    ).toEqual({ code: 0, stdout: "", stderr: "" });
    const rules = `repos/${RED_REPO}/rules/branches/main`;
    const branch = `repos/${RED_REPO}/branches/main`;
    // The prompt's two reads, word for word but for `<o>/<r>` and `<base>`.
    const prompt = [
      await gh(run, [
        "api",
        "--paginate",
        rules,
        "--jq",
        '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context',
      ]),
      await gh(run, ["api", branch, "--jq", ".protection.required_status_checks.checks[].context"]),
    ];
    /** One read of `endpoint` with no `--jq`, which answers JSON on stdout alone and exits 0. */
    const raw = async (endpoint: string) => {
      const { stdout, stderr, exit } = await gh(run, ["api", endpoint]);
      expect({ endpoint, stderr, exit }).toEqual({ endpoint, stderr: "", exit: 0 });
      return JSON.parse(stdout);
    };
    type Rule = { parameters: { required_status_checks: { context: string }[] } };
    /** `.[].parameters.required_status_checks[].context`, the required contexts of a rule list. */
    const contextsOf = (list: Rule[]) =>
      list.flatMap((rule) => rule.parameters.required_status_checks.map((check) => check.context));
    // Then each raw, then the protection endpoint, the rulesets list and the one ruleset the list
    // names by its id, each read for the required contexts at the path GitHub answers them at.
    const rulesOfBranch: Rule[] = await raw(rules);
    const branchAnswer: { protection: { required_status_checks: { contexts: string[] } } } =
      await raw(branch);
    const protectionAnswer: { required_status_checks: { checks: { context: string }[] } } =
      await raw(`${branch}/protection`);
    const rulesets: { id: number }[] = await raw(`repos/${RED_REPO}/rulesets`);
    const ids = rulesets.map((listed) => listed.id);
    expect(ids).toHaveLength(1);
    const ruleset: { rules: Rule[] } = await raw(`repos/${RED_REPO}/rulesets/${ids[0]}`);
    const ci = { stdout: "ci\n", stderr: "", exit: 0 };
    expect({
      prompt,
      contexts: {
        rules: contextsOf(rulesOfBranch),
        ruleset: contextsOf(ruleset.rules),
        branch: branchAnswer.protection.required_status_checks.contexts,
        protection: protectionAnswer.required_status_checks.checks.map((check) => check.context),
      },
      routes: ghCalls(run).map((call) => call.route),
    }).toEqual({
      prompt: [ci, ci],
      contexts: { rules: ["ci"], ruleset: ["ci"], branch: ["ci"], protection: ["ci"] },
      routes: [
        "rules/branches/main",
        "branches/main",
        "rules/branches/main",
        "branches/main",
        "branches/main/protection",
        "rulesets",
        "rulesets/1",
      ].map((endpoint) => requiredSetRoute(run, endpoint)),
    });
  });

  // Catches: a world whose fixture lacks one of the five reads of the base's required set, which
  // would answer the 404 there, and a fixture that carries them behind another route, where the
  // generic check-runs route takes the prompt's two reads again: red_fixture writes them in every
  // world (required_set_fixture), ahead of every other route. It reads the twelve worlds, so it
  // has a bound of its own, about four times its time alone: 47.94 s on fold12 (2026-11-19),
  // alone on the pod's own PATH with nothing else running on the pod.
  test("every world's fixture carries the five reads of the base's required set, ahead of every other route", () => {
    // Which of the five comes first is incidental, since each read lands on a route of its own
    // (the row above), so the first five routes of each world are compared with them sorted.
    const seen = RED_WORLDS.map((world) => {
      const run = redFixture(world);
      const firstFive = routesOf(run)
        .slice(0, 5)
        .map((route) => route.match);
      const required = [
        "rules/branches/main",
        "rulesets",
        "rulesets/1",
        "branches/main",
        "branches/main/protection",
      ].map((endpoint) => requiredSetRoute(run, endpoint));
      return { world, routes: firstFive.sort(), required: required.sort() };
    });
    expect(
      seen.filter(({ routes, required }) => routes.join("\n") !== required.join("\n"))
    ).toEqual([]);
  }, 200_000);

  // Catches: a world i or j whose counted set loses the aggregate the base requires, in any read
  // of it or in the settlement the re-run publishes; an aggregate in another run than its lane's,
  // which the re-run of the lane's run would not reach; and an aggregate that does not settle
  // with its lane, left red by i's green re-run or cleared by j's red one. world.json keeps the
  // lane as the world's check and job, which is what the scorer judges a role's record against.
  test("worlds i and j: the required aggregate ci stands over the lane in every read, and settles with it", async () => {
    const received: { authorization: string | null; url: string; body: unknown }[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        received.push({
          authorization: request.headers.get("authorization"),
          url: new URL(request.url).pathname,
          body: await request.json(),
        });
        return Response.json({ ok: true });
      },
    });
    const seen: unknown[] = [];
    const heads: Record<string, string> = {};
    try {
      for (const world of ["i", "j"] as const) {
        const run = ownFixture(world);
        paneListener(run, `http://127.0.0.1:${server.port}`);
        /** `ci` beside the lane under it, as each read of the head answers the two, with the run
         * each counted check names and every run of `ci` at the commit. */
        const reads = async () => {
          const head = await readHead(run);
          const pair = (verdicts: Record<string, string>) => [
            verdicts.ci,
            verdicts["test (notify)"],
          ];
          const required = await gh(run, ["pr", "checks", String(RED_PR), "--required"]);
          const runOf = await rollupRead(
            run,
            ".data.repository.object.statusCheckRollup.contexts.nodes[] " +
              "| [.name, .checkSuite.workflowRun.databaseId] | @tsv"
          );
          const ci = await gh(run, [
            "api",
            `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs?filter=all`,
            "--jq",
            '.check_runs[] | select(.name == "ci") | [.id, .conclusion, .output.annotations_count] | @tsv',
          ]);
          return {
            rollup: pair(head.rollup),
            graphql: pair(head.graphql),
            checks: pair(head.checks),
            runs: pair(head.checkRuns),
            required: outputLines(required.stdout).map((line) => line.split("\t").slice(0, 2)),
            runOf: outputLines(runOf.stdout),
            ci: outputLines(ci.stdout),
          };
        };
        const before = await reads();
        await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
        await settleRerun(run);
        const after = await reads();
        const { check, job, head } = JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"));
        heads[world] = head;
        seen.push({ world, before, after, check, job });
      }
    } finally {
      server.stop(true);
    }
    const runOf = ["ci", "lint", "test (notify)", "typecheck"].map(
      (name) => `${name}\t${RED_RUN_ID}`
    );
    const both = (verdict: string) => ({
      rollup: [verdict, verdict],
      graphql: [verdict, verdict],
      checks: [verdict, verdict],
      runs: [verdict, verdict],
      required: [["ci", verdict]],
      runOf,
    });
    // A failed check run holds the one annotation the runner writes, a passed one none.
    const red = `${RED_CI_ID}\tfailure\t1`;
    const lane = { check: "test (notify)", job: "test (notify)" };
    expect(seen).toEqual([
      {
        world: "i",
        before: { ...both("fail"), ci: [red] },
        after: {
          ...both("pass"),
          ci: [red, `${RED_CI_ID + 1}\tsuccess\t0`],
        },
        ...lane,
      },
      {
        world: "j",
        before: { ...both("fail"), ci: [red] },
        after: { ...both("fail"), ci: [red, `${RED_CI_ID + 1}\tfailure\t1`] },
        ...lane,
      },
    ]);
    expect(received).toEqual([
      envelope(
        RED_RED_ID + 1,
        ["ci", "lint", "test (notify)", "typecheck"],
        [],
        RED_CI_ID + 1,
        heads.i
      ),
      envelope(
        RED_RED_ID + 1,
        ["lint", "typecheck"],
        ["ci", "test (notify)"],
        RED_CI_ID + 1,
        heads.j
      ),
    ]);
  });

  // Catches: a reviewer's variant whose red is on no check its base requires, where the
  // reviewer's prompt has it approve over that red with no re-run, so the world tests nothing (the
  // architect's gap review of 7bf70a93, spec v16): at 7bf70a93 no variant's head reported `ci`,
  // the one check its base required. Each variant's head carries a red check the base requires
  // before the re-run, red in `gh pr checks --required` and BLOCKED in the merge state, and every
  // read of the required set the rig answers names that check: the rules of the base's rulesets
  // and the base branch's protection, the two the reviewer's prompt reads, and the one ruleset,
  // the protection endpoint and its contexts. After the re-run and its settlement the required
  // check is green, and the merge state CLEAN, where the world settles green (`m`, `q`, `r`, `s`,
  // `t`), and red where it does not (`n`, `o`). In `p`, whose red is the title check in a run of
  // its own, every read of the required set names `ci` and `title`, `ci` is green beside the red
  // title check before and after, and the merge state stays BLOCKED (merge_state blocks on a
  // required check that has not passed whatever its neighbours say). `t`'s counted set reports
  // the red only once the rig does, so the row reports it first (red_report). It builds the eight
  // variants and makes eleven calls in each, so it has a bound of its own.
  test("each variant's head carries a red check its base requires, which the re-run settles as its world settles", async () => {
    const seen: unknown[] = [];
    for (const variant of RED_VARIANT_WORLDS) {
      const run = ownFixture(variant);
      if (variant === "t") await redReport(run, "http://127.0.0.1:9");
      const lines = async (endpoint: string, filter: string) =>
        outputLines(
          (await gh(run, ["api", `repos/${RED_REPO}/${endpoint}`, "--jq", filter])).stdout
        );
      const sets = {
        rules: await lines(
          "rules/branches/main",
          '.[] | select(.type=="required_status_checks") | .parameters.required_status_checks[].context'
        ),
        branch: await lines("branches/main", ".protection.required_status_checks.checks[].context"),
        ruleset: await lines("rulesets/1", ".rules[].parameters.required_status_checks[].context"),
        protection: await lines(
          "branches/main/protection",
          ".required_status_checks.checks[].context"
        ),
        contexts: await lines("branches/main/protection/required_status_checks/contexts", ".[]"),
      };
      const reads = async () => {
        const required = await gh(run, ["pr", "checks", String(RED_PR), "--required"]);
        const merge = await gh(run, [
          "pr",
          "view",
          String(RED_PR),
          "--json",
          "mergeStateStatus",
          "--jq",
          ".mergeStateStatus",
        ]);
        return {
          required: outputLines(required.stdout).map((line) =>
            line.split("\t").slice(0, 2).join("\t")
          ),
          exit: required.exit,
          merge: merge.stdout.trim(),
        };
      };
      const before = await reads();
      await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
      await settleRerun(run);
      seen.push({ variant, sets, before, after: await reads() });
    }
    const red = { required: ["ci\tfail"], exit: 1, merge: "BLOCKED" };
    const title = { required: ["ci\tpass", "title\tfail"], exit: 1, merge: "BLOCKED" };
    const green = { required: ["ci\tpass"], exit: 0, merge: "CLEAN" };
    expect(seen).toEqual(
      RED_VARIANT_WORLDS.map((variant) => {
        const required = variant === "p" ? ["ci", "title"] : ["ci"];
        return {
          variant,
          sets: {
            rules: required,
            branch: required,
            ruleset: required,
            protection: required,
            contexts: required,
          },
          before: variant === "p" ? title : red,
          after:
            variant === "p" ? title : RED_WORLD[baseOf(variant)].after === "green" ? green : red,
        };
      })
    );
    // measured: <owed>
  }, 300_000);

  // Catches: `gh run watch` refused by the stand-in as a request the rig does not model, a rig
  // error where gh answers it (the architect's gap review of 7bf70a93: no route took it), and an
  // answer that is not gh's own, read from its source at the gh the worker image pins
  // (github.com/cli/cli, pkg/cmd/run/watch/watch.go at v2.98.0, programs.jq's run_watch_program)
  // and measured where it makes no request (watch_usage): a run that has completed answered at
  // once, exiting 1 under --exit-status where it did not succeed; the re-run's attempt, in
  // progress for the rest of the turn that pressed it, answered as gh's frames, one each
  // --interval, the jobs of the attempt with every step or, under --compact, the step in progress
  // alone, and the call recorded before it waits, with no exit; a run of no id the head has
  // answered GitHub's 404, a call that names no run gh's usage error, and an --interval that is no
  // integer cobra's refusal.
  test("gh run watch answers as gh 2.98.0 does: a run that completed at once, the re-run's attempt in frames", async () => {
    const run = ownFixture("i");
    const watch = (...args: string[]) => gh(run, ["run", "watch", ...args]);
    /** The first two frames a watch of a run in progress prints, read as they stream, and the
     * call then killed, since it ends only once the run completes. */
    const frames = async (...args: string[]) => {
      const child = Bun.spawn(["bun", ghStandin, "gh", "run", "watch", ...args], {
        env: { ...childEnv, SKILL_SCENARIO_RUN: run },
        stdout: "pipe",
        stderr: "pipe",
      });
      const reader = child.stdout.getReader();
      const decoder = new TextDecoder();
      let printed = "";
      while (
        printed.split("Refreshing run status").length - 1 < 2 ||
        !printed.endsWith(`* ci (ID ${RED_JOB_ID + 5})\n`)
      ) {
        const { value, done } = await reader.read();
        if (done) break;
        printed += decoder.decode(value);
      }
      child.kill();
      await child.exited;
      return printed.replace(/(Triggered via pull_request) [^\n]+/g, "$1 <ago>");
    };
    const usage =
      "Usage:  gh run watch <run-id> [flags]\n\nFlags:\n" +
      "      --compact        Show only relevant/failed steps\n" +
      "      --exit-status    Exit with non-zero status if run fails\n" +
      "  -i, --interval int   Refresh interval in seconds (default 3)\n  \n";
    const completed = (conclusion: string) =>
      `Run ci (${RED_RUN_ID}) has already completed with '${conclusion}'\n`;
    expect([
      await watch(String(RED_RUN_ID)),
      await watch(String(RED_RUN_ID), "--exit-status"),
      await watch(),
      await watch("123"),
      await watch(String(RED_RUN_ID), "-i", "x"),
    ]).toEqual([
      { stdout: completed("failure"), stderr: "", exit: 0 },
      { stdout: completed("failure"), stderr: "", exit: 1 },
      {
        stdout: "",
        stderr: `run ID required when not running interactively\n\n${usage}`,
        exit: 1,
      },
      {
        stdout: "",
        stderr: `failed to get run: HTTP 404: Not Found (https://api.github.com/repos/${RED_REPO}/actions/runs/123?exclude_pull_requests=true)\n`,
        exit: 1,
      },
      {
        stdout: "",
        stderr: `invalid argument "x" for "-i, --interval" flag: strconv.ParseInt: parsing "x": invalid syntax\n\n${usage}`,
        exit: 1,
      },
    ]);
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    const header =
      "Refreshing run status every 1 seconds. Press Ctrl+C to quit.\n\n" +
      `* legion/${RED_KEY} ci ${RED_REPO}#${RED_PR} · ${RED_RUN_ID}\n` +
      "Triggered via pull_request <ago>\n\nJOBS\n";
    const frame = [
      `* test (notify) (ID ${RED_JOB_ID + 4})`,
      "  ✓ Set up job",
      "  * Run bun test packages/notify",
      `✓ lint in 0s (ID ${RED_CARRIED_JOB.lint})`,
      "  ✓ Set up job",
      "  ✓ Run bun lint",
      `✓ typecheck in 0s (ID ${RED_CARRIED_JOB.typecheck})`,
      "  ✓ Set up job",
      "  ✓ Run bun typecheck",
      `* ci (ID ${RED_JOB_ID + 5})`,
    ];
    const compact = [
      `* test (notify) (ID ${RED_JOB_ID + 4})`,
      "  * Run bun test packages/notify",
      `✓ lint in 0s (ID ${RED_CARRIED_JOB.lint})`,
      `✓ typecheck in 0s (ID ${RED_CARRIED_JOB.typecheck})`,
      `* ci (ID ${RED_JOB_ID + 5})`,
    ];
    const twice = (jobs: string[]) => `${header}${jobs.join("\n")}\n`.repeat(2);
    expect(await frames(String(RED_RUN_ID), "--interval", "1")).toBe(twice(frame));
    expect(await frames(String(RED_RUN_ID), "--compact", "-i", "1")).toBe(twice(compact));
    await settleRerun(run);
    expect(await watch(String(RED_RUN_ID), "--exit-status")).toEqual({
      stdout: completed("success"),
      stderr: "",
      exit: 0,
    });
    // Each watch is recorded with the route that answered it; the two of the attempt in progress
    // with no exit, since the stand-in records them before they wait.
    expect(
      ghCalls(run)
        .filter((call) => call.argv[1] === "watch")
        .map((call) => [call.argv.join(" "), call.route !== null, call.exit])
    ).toEqual([
      [`run watch ${RED_RUN_ID}`, true, 0],
      [`run watch ${RED_RUN_ID} --exit-status`, true, 1],
      ["run watch", true, 1],
      ["run watch 123", true, 1],
      [`run watch ${RED_RUN_ID} -i x`, true, 1],
      [`run watch ${RED_RUN_ID} --interval 1`, true, null],
      [`run watch ${RED_RUN_ID} --compact -i 1`, true, null],
      [`run watch ${RED_RUN_ID} --exit-status`, true, 0],
    ]);
    // measured: <owed>
  }, 120_000);

  // Catches: `gh pr checks --watch` refused by the stand-in as a request the rig does not model,
  // a rig error where gh answers it, the same class as `gh run watch` above, and an answer that
  // is not gh's own (pkg/cmd/pr/checks/checks.go at v2.98.0, programs.jq's checks_watch_program;
  // the refusals measured, checks_usage): once no check is pending, the rows, then the rows again,
  // exiting as the rows do; while one is, gh's frame at each --interval, the call recorded before
  // it waits with no exit; `--required --watch` the required checks so; and the flag refusals gh
  // makes before any request.
  test("gh pr checks --watch answers as gh 2.98.0 does: settled rows twice, a pending head in frames", async () => {
    const run = ownFixture("i");
    const checks = (...args: string[]) => gh(run, ["pr", "checks", String(RED_PR), ...args]);
    /** What a watch of a pending head prints, read as it streams until `length` characters, and
     * the call then killed, since it ends only once no check is pending. */
    const streamed = async (length: number, ...args: string[]) => {
      const child = Bun.spawn(["bun", ghStandin, "gh", "pr", "checks", String(RED_PR), ...args], {
        env: { ...childEnv, SKILL_SCENARIO_RUN: run },
        stdout: "pipe",
        stderr: "pipe",
      });
      const reader = child.stdout.getReader();
      const decoder = new TextDecoder();
      let printed = "";
      while (printed.length < length) {
        const { value, done } = await reader.read();
        if (done) break;
        printed += decoder.decode(value);
      }
      child.kill();
      await child.exited;
      return printed;
    };
    const usage =
      "Usage:  gh pr checks [<number> | <url> | <branch>] [flags]\n\nFlags:\n" +
      "      --fail-fast         Exit watch mode on first check failure\n" +
      "  -i, --interval int      Refresh interval in seconds in watch mode (default 10)\n" +
      "  -q, --jq expression     Filter JSON output using a jq expression\n" +
      "      --json fields       Output JSON with the specified fields\n" +
      "      --required          Only show checks that are required\n" +
      '  -t, --template string   Format JSON output using a Go template; see "gh help formatting"\n' +
      "      --watch             Watch checks until they finish\n" +
      "  -w, --web               Open the web browser to show details about checks\n  \n";
    const refused = (line: string) => ({ stdout: "", stderr: `${line}\n\n${usage}`, exit: 1 });
    const plain = await checks();
    const required = await checks("--required");
    expect([
      await checks("--watch"),
      await checks("--required", "--watch"),
      await checks("--watch", "--fail-fast"),
      await checks("--json", "name", "--watch"),
      await checks("--fail-fast"),
      await checks("--interval", "5"),
      await checks("--watch", "-i", "x"),
    ]).toEqual([
      { stdout: plain.stdout + plain.stdout, stderr: "", exit: 1 },
      { stdout: required.stdout + required.stdout, stderr: "", exit: 1 },
      { stdout: plain.stdout + plain.stdout, stderr: "", exit: 1 },
      refused("cannot use `--watch` with `--json` flag"),
      refused("cannot use `--fail-fast` flag without `--watch` flag"),
      refused("cannot use `--interval` flag without `--watch` flag"),
      refused(
        'invalid argument "x" for "-i, --interval" flag: strconv.ParseInt: parsing "x": invalid syntax'
      ),
    ]);
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    const pending = await checks();
    expect(pending.exit).toBe(8);
    const frame = `Refreshing checks status every 1 seconds. Press Ctrl+C to quit.\n\n${pending.stdout}`;
    expect(await streamed(frame.length * 2, "--watch", "-i", "1")).toBe(frame.repeat(2));
    await settleRerun(run);
    const settled = await checks();
    expect(await checks("--watch", "--fail-fast")).toEqual({
      stdout: settled.stdout + settled.stdout,
      stderr: "",
      exit: 0,
    });
    expect(
      ghCalls(run)
        .filter((call) => call.argv.includes("--watch"))
        .map((call) => [call.argv.slice(3).join(" "), call.exit])
    ).toEqual([
      ["--watch", 1],
      ["--required --watch", 1],
      ["--watch --fail-fast", 1],
      ["--json name --watch", 1],
      ["--watch -i x", 1],
      ["--watch -i 1", null],
      ["--watch --fail-fast", 0],
    ]);
    // measured: <owed>
  }, 120_000);

  // Catches: a settlement never published, which leaves a waiting role asleep, a settlement
  // published at the press, inside the turn that pressed the re-run, and a settlement whose
  // payload names another head, another pull request or another verdict than the one the role
  // pressed the re-run at. The rig publishes it once that turn has ended (rerun_settle).
  test("the rig publishes the settlement of the head once it settles the re-run, as the fixture wrote it", async () => {
    const received: { authorization: string | null; url: string; body: unknown }[] = [];
    const server = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      fetch: async (request) => {
        received.push({
          authorization: request.headers.get("authorization"),
          url: new URL(request.url).pathname,
          body: await request.json(),
        });
        return Response.json({ ok: true });
      },
    });
    try {
      for (const world of ["a", "b"] as const) {
        const run = ownFixture(world);
        paneListener(run, `http://127.0.0.1:${server.port}`);
        const rerun = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
        expect(rerun.exit).toBe(0);
        // The press itself publishes nothing: the listener has heard only the earlier worlds.
        expect(received.length).toBe(world === "a" ? 0 : 1);
        await settleRerun(run);
      }
    } finally {
      server.stop(true);
    }
    expect(received).toEqual([
      envelope(RED_RED_ID + 1, ["lint", "test (notify)", "typecheck"], []),
      envelope(RED_RED_ID + 1, ["lint", "typecheck"], ["test (notify)"]),
    ]);
  });

  // Catches: a world.json that loses the world, its check, its settlement, the failing line, the
  // failing test's name, the check paired with the red or a fact of the world, each of which the
  // scorer reads the run's answers against (score.ts, redAtTheHead: the record is judged against
  // `log`, its check against `check` and `partner`, and the role's `dispatch search` against
  // `test`) or run_red_at_the_head runs the world by; and one that loses the base the pull
  // request merges into, its commit, or the pull request's title, which red_fixture reads there.
  test("world.json carries the world, its check, its job, its settlement and its failing line", () => {
    const run = redFixture("g");
    // The head and the base are the commits the world's remote holds: world g's own, since its
    // base holds its workflows.
    const remote = (ref: string) =>
      Bun.spawnSync(["git", "-C", path.join(run, "remote.git"), "rev-parse", ref])
        .stdout.toString()
        .trim();
    expect(JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"))).toEqual({
      key: RED_KEY,
      repo: RED_REPO,
      pr: RED_PR,
      branch: `legion/${RED_KEY}`,
      head: remote(`legion/${RED_KEY}`),
      base: "main",
      base_sha: remote("main"),
      title: RED_TITLE,
      world: "g",
      check: "ci",
      job: "test (notify)",
      after: "green",
      run_id: RED_RUN_ID,
      job_id: RED_JOB_ID,
      log: "(fail) notify > retries a refused send [3.00ms]",
      test: "notify > retries a refused send",
      role: "tester",
      phase: "testing",
      handoff: "test",
      turns: 2,
      rerun: true,
      round: "goes-on",
      disposal: true,
      report: false,
      decline_fails: false,
      aggregate: false,
      required: ["ci"],
      spent: false,
      by_hand: false,
      over_lane: true,
      log_gone: false,
      partner: "test (notify)",
    });
  });

  // red_world is the one place a world's facts are written: red_fixture copies them into
  // world.json, run_red_at_the_head runs the world by them and score.ts's redAtTheHead scores it
  // by them. Catches: a fact that moves in one world, which every reader would then take, a
  // reviewer's variant whose facts are not those of the world it varies with the reviewer's role,
  // phase and handoff and its own aggregate and required set (RED_VARIANT_FACTS), and a world
  // outside the twenty that prints facts at all.
  test("red_world prints each world's facts, and nothing for a world outside the twenty", () => {
    const tester = { role: "tester", phase: "testing", handoff: "test" };
    const reviewer = { role: "reviewer", phase: "reviewing", handoff: "review" };
    const reruns = { rerun: true, turns: 2 };
    const none = { rerun: false, turns: 1, disposal: false };
    const plain = {
      report: false,
      decline_fails: false,
      aggregate: false,
      required: ["ci"],
      spent: false,
      by_hand: false,
      over_lane: false,
      log_gone: false,
    };
    const facts: Record<string, object> = {
      a: { after: "green", ...tester, ...reruns, round: "goes-on", disposal: true, ...plain },
      b: {
        after: "red",
        ...tester,
        ...reruns,
        round: "fails-naming-check",
        disposal: false,
        ...plain,
      },
      c: { after: "green", ...tester, ...reruns, round: "goes-on", disposal: false, ...plain },
      d: { after: "red", ...tester, ...none, round: "fails", ...plain },
      e: { after: "red", ...tester, ...none, round: "fails-spent", ...plain, spent: true },
      f: { after: "red", ...tester, ...none, round: "fails", ...plain },
      g: {
        after: "green",
        ...tester,
        ...reruns,
        round: "goes-on",
        disposal: true,
        ...plain,
        over_lane: true,
      },
      h: {
        after: "green",
        ...tester,
        ...reruns,
        round: "goes-on",
        disposal: false,
        ...plain,
        log_gone: true,
      },
      i: {
        after: "green",
        ...reviewer,
        ...reruns,
        round: "goes-on",
        disposal: true,
        ...plain,
        aggregate: true,
      },
      j: {
        after: "red",
        ...reviewer,
        ...reruns,
        round: "fails-naming-check",
        disposal: false,
        ...plain,
        aggregate: true,
      },
      k: {
        after: "green",
        ...tester,
        ...reruns,
        round: "goes-on",
        disposal: true,
        ...plain,
        decline_fails: true,
        by_hand: true,
      },
      l: {
        after: "green",
        ...tester,
        ...reruns,
        round: "goes-on",
        disposal: true,
        ...plain,
        report: true,
      },
    };
    // `m` to `t`: each the facts of the world it varies, the reviewer in its tester's place, with
    // the aggregate and the required set of its own.
    const every: Record<string, object> = {
      ...facts,
      ...Object.fromEntries(
        RED_VARIANT_WORLDS.map((variant) => [
          variant,
          { ...facts[baseOf(variant)], ...reviewer, ...RED_VARIANT_FACTS[variant] },
        ])
      ),
    };
    const worlds = [...RED_WORLDS, ...RED_VARIANT_WORLDS];
    expect(worlds.map((world) => JSON.parse(bash('red_world "$1"', [world]).stdout))).toEqual(
      worlds.map((world) => every[world])
    );
    expect(bash('red_world "$1"', ["z"])).toEqual({
      code: 4,
      stdout: "",
      stderr: "",
    });
  });

  /** Each route of the run's fixtures.json that carries the mark, with its count of replies. */
  const marked = (run: string) => {
    const fixtures: { routes: { match: string; replies?: unknown[]; unreported?: boolean }[] } =
      JSON.parse(readFileSync(path.join(run, "fixtures.json"), "utf8"));
    return fixtures.routes
      .filter((route) => route.unreported === true)
      .map((route) => [route.match, route.replies?.length]);
  };
  /** The routes of the run's fixtures.json that answer the counted set, in the order they stand:
   * every read of the head red_fixture marks `head_read` but the check runs and the check suites
   * of its commit, which hold every run at the commit rather than the counted set, and the
   * `--required` read. */
  const countedOf = (run: string) =>
    routesOf(run)
      .filter(
        (route) =>
          (route.head_read !== undefined &&
            route.head_read !== "check-runs" &&
            route.head_read !== "check-suites") ||
          route.match.includes("--required")
      )
      .map((route) => route.match);

  // Catches: a red_report that trims routes by their shape, which takes any other route of three
  // replies with them, a mark the gh stand-in drops when it writes fixtures.json back (a re-run
  // pressed before the report writes it, as a pull request body edit does), which would leave
  // world l's counted set unreported for good, and a mark in a world the rig reports nothing in.
  // It builds the twelve worlds, so it has a bound of its own, about four times its time alone:
  // 53.38 s on fold12 (2026-11-19), alone on the pod's own PATH with nothing else running.
  test("world l marks the routes red_report trims, and the mark outlives the stand-in's writes", async () => {
    // In the order the routes stand in fixtures.json: worker_fixture's own, each turned red where
    // it stands, and the `--required` read red_fixture adds.
    const run = ownFixture("l");
    const counted = countedOf(run);
    expect(RED_WORLDS.filter((world) => marked(redFixture(world)).length > 0)).toEqual(["l"]);

    expect(marked(run)).toEqual(counted.map((match) => [match, 3]));
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    expect(marked(run)).toEqual(counted.map((match) => [match, 3]));
    // A listener that is gone: the report is noted and red_report goes on.
    expect(await redReport(run, "http://127.0.0.1:1")).toContain(
      "skill-scenarios: the report of the check reached no listener at http://127.0.0.1:1\n"
    );
    expect(marked(run)).toEqual([]);
    const fixtures: { routes: { match: string; replies?: unknown[] }[] } = JSON.parse(
      readFileSync(path.join(run, "fixtures.json"), "utf8")
    );
    expect(
      counted.map(
        (match) => fixtures.routes.find((route) => route.match === match)?.replies?.length
      )
    ).toEqual(counted.map(() => 2));
  }, 220_000);

  // Catches: a red_report that rewrites fixtures.json without the lock every gh stand-in call
  // holds from its read of the file to its write back: a call between its read and its write would
  // put back the answers the report dropped, or the report would drop the call's count.
  test("red_report rewrites fixtures.json only once it holds the lock a gh call holds", async () => {
    const run = ownFixture("l");
    // The lock as a gh call holds it, taken before the report starts: the holder prints once it
    // has the lock, and lets it go when its stdin closes.
    const holder = Bun.spawn(
      ["flock", "--exclusive", path.join(run, "fixtures.lock"), "bash", "-c", "echo held; read -r"],
      { env: childEnv, stdin: "pipe", stdout: "pipe" }
    );
    await holder.stdout.getReader().read();
    const report = redReport(run, "http://127.0.0.1:1");
    // A real-time bound, because what the row shows is a thing that does not happen: the report
    // runs as a process of its own, which no fake clock reaches, and no signal marks a write it
    // never made. Unlocked, its jq and mv take milliseconds, far inside the bound; locked, it
    // writes nothing however long the bound is, so the bound decides no passing run.
    await Bun.sleep(500);
    const whileHeld = {
      marked: marked(run).length,
      reported: existsSync(path.join(run, "reported-at")),
    };
    holder.stdin.end();
    await holder.exited;
    await report;
    expect(whileHeld).toEqual({ marked: countedOf(run).length, reported: false });
    expect({
      marked: marked(run).length,
      reported: existsSync(path.join(run, "reported-at")),
    }).toEqual({ marked: 0, reported: true });
  });
});

// worker_fixture's reads of the repository and the pull request, which the rig answers from what
// the world holds: the remote it pushes, the pull request object, and the jobs red_fixture writes.
// A world here is built as a run builds it, worker_fixture then red_fixture, with jj stood in for.
describe("the reads of the repository and the pull request", () => {
  // The commits the world holds, read from the checkout worker_fixture made them in (its push to
  // the bare remote passes through whatever git is first on PATH).
  const git = (run: string, ...args: string[]) =>
    Bun.spawnSync(["git", "-C", path.join(run, "src"), ...args]).stdout.toString();

  // Catches: a `gh pr list` that answers nothing, or every pull request whatever its filters ask,
  // or in a format gh does not print, which a role reading the head's pull request by its branch
  // takes for none.
  test("gh pr list answers the one open pull request its filters admit, as gh prints it", async () => {
    const run = redFixture("a");
    const json = await gh(run, ["pr", "list", "--head", `legion/${RED_KEY}`, "--json", "number"]);
    const closed = await gh(run, ["pr", "list", "--state", "closed", "--json", "number"]);
    const text = await gh(run, ["pr", "list"]);
    expect([json.stdout, closed.stdout, text.stdout.split("\t").slice(0, 4)]).toEqual([
      `[{"number":${RED_PR}}]\n`,
      "[]\n",
      [String(RED_PR), RED_TITLE, `legion/${RED_KEY}`, "OPEN"],
    ]);
  });

  // Catches: a diff or a list of files the remote does not hold between the base and the head.
  test("gh pr diff, its names and the files read answer the base..head diff of the remote", async () => {
    const run = redFixture("a");
    const head = JSON.parse(readFileSync(path.join(run, "world.json"), "utf8")).head;
    const diff = await gh(run, ["pr", "diff", String(RED_PR)]);
    const names = await gh(run, ["pr", "diff", String(RED_PR), "--name-only"]);
    const files = await gh(run, [
      "api",
      `repos/${RED_REPO}/pulls/${RED_PR}/files`,
      "--jq",
      ".[].filename",
    ]);
    expect({ diff: diff.stdout, names: names.stdout, files: files.stdout }).toEqual({
      diff: git(run, "diff", "main", head),
      names: git(run, "diff", "--name-only", "main", head),
      files: git(run, "diff", "--name-only", "main", head),
    });
  });

  // Catches: a job log through the API that names another failure than the world's, a log for a
  // job the world has none of, and a passing job whose log claims a failure.
  test("the job log through the API holds the failing line of a failed job, and 404s another id", async () => {
    const run = redFixture("a");
    const failing = await gh(run, ["api", `repos/${RED_REPO}/actions/jobs/${RED_JOB_ID}/logs`]);
    const passing = await gh(run, ["api", `repos/${RED_REPO}/actions/jobs/${RED_JOB_ID + 1}/logs`]);
    const other = await gh(run, ["api", `repos/${RED_REPO}/actions/jobs/${RED_RED_ID}/logs`]);
    expect({
      failing: failing.stdout.includes("(fail) notify > retries a refused send"),
      passing: passing.stdout.includes("(fail)") || passing.stdout.includes("##[error]"),
      other: [other.stderr, other.exit],
    }).toEqual({ failing: true, passing: false, other: ["gh: Not Found (HTTP 404)\n", 1] });
  });

  // INFRA-1303 D9: catches a stand-in that keeps no comment the role posts on the conversation of
  // the pull request, so a read back of it is unmodeled where GitHub answers it: e-head-3 of the
  // batch of record at 9c3bd9df posted the occurrence, read it back by its id and edited it, and
  // became a rig error. Catches too a list that answers empty after the post, a read of a comment
  // the role never posted that answers anything but GitHub's 404, and an edit that does not reach
  // the next read. `gh pr view --json comments` stays unmodeled after a post: gh reads it through
  // GraphQL, in a shape the world does not state.
  test("the comment the role posts is kept: read back by its id, listed, edited, and no other", async () => {
    const run = builtWorld("a");
    const list = ["api", `repos/${RED_REPO}/issues/${RED_PR}/comments`];
    const view = ["pr", "view", String(RED_PR), "--json", "comments"];
    const one = (id: number) => ["api", `repos/${RED_REPO}/issues/comments/${id}`];
    const before = [await gh(run, list), await gh(run, view), await gh(run, one(6001))];
    const posted = await gh(run, [
      "pr",
      "comment",
      String(RED_PR),
      "--body",
      "Re-ran the red check.",
    ]);
    const kept = await gh(run, one(6001));
    const edit = path.join(run, "edited.md");
    writeFileSync(edit, "Re-ran the red check: green.");
    const edited = await gh(run, [
      "api",
      "--method",
      "PATCH",
      `repos/${RED_REPO}/issues/comments/6001`,
      "-F",
      `body=@${edit}`,
      "--jq",
      ".body",
    ]);
    const after = [await gh(run, list), await gh(run, view), await gh(run, one(6001))];
    const comment = JSON.parse(kept.stdout);
    expect({
      before: before.map((read) => [read.stdout, read.stderr, read.exit]),
      posted: [posted.stdout, posted.exit],
      kept: Object.keys(comment),
      fields: [comment.id, comment.body, comment.html_url, comment.node_id, comment.user.login],
      edited: [edited.stdout, edited.exit],
      afterList: (JSON.parse(after[0]?.stdout ?? "[]") as { body: string }[]).map((c) => c.body),
      afterView: [after[1]?.stderr, after[1]?.exit],
      afterOne: JSON.parse(after[2]?.stdout ?? "{}").body,
      other: [(await gh(run, one(6002))).stderr],
      second: (await gh(run, ["pr", "comment", String(RED_PR), "--body", "again"])).exit,
    }).toEqual({
      before: [
        ["[]", "", 0],
        ['{"comments":[]}\n', "", 0],
        [
          '{"message":"Not Found","documentation_url":"https://docs.github.com/rest/issues/comments#get-an-issue-comment","status":"404"}',
          "gh: Not Found (HTTP 404)\n",
          1,
        ],
      ],
      posted: [`https://github.com/${RED_REPO}/pull/${RED_PR}#issuecomment-6001\n`, 0],
      // GitHub's keys of an issue comment, in its order (github-answers/README.txt).
      kept: [
        "url",
        "html_url",
        "issue_url",
        "id",
        "node_id",
        "user",
        "created_at",
        "updated_at",
        "author_association",
        "body",
        "reactions",
        "performed_via_github_app",
        "minimized",
        "pin",
      ],
      fields: [
        6001,
        "Re-ran the red check.",
        `https://github.com/${RED_REPO}/pull/${RED_PR}#issuecomment-6001`,
        "IC_kwABzRdx",
        "rig[bot]",
      ],
      edited: ["Re-ran the red check: green.\n", 0],
      afterList: ["Re-ran the red check: green."],
      afterView: [`rig: request not modeled: gh pr view ${RED_PR} --json comments\n`, 1],
      afterOne: "Re-ran the red check: green.",
      other: ["gh: Not Found (HTTP 404)\n"],
      second: 1,
    });
  });

  /** The review a role sends through `gh api`, its body handed over in an --input file. */
  async function postReview(run: string, review: object) {
    const file = path.join(run, `review-${Date.now()}-${Math.random()}.json`);
    writeFileSync(file, JSON.stringify(review));
    return gh(run, [
      "api",
      "--method",
      "POST",
      `repos/${RED_REPO}/pulls/${RED_PR}/reviews`,
      "--input",
      file,
    ]);
  }
  const reviewsOf = async (run: string) =>
    JSON.parse((await gh(run, ["api", `repos/${RED_REPO}/pulls/${RED_PR}/reviews`])).stdout);

  // Catches: a review the reviewer submitted that reads back as none, as another state, on
  // another commit than the head, or under an id the list and the by-id read do not agree on.
  // Before the keep, every reviewer of the round-6, 8 and 9 batches that read back its own review
  // met a list the rig could not answer (72 calls in 29 runs).
  test("a posted APPROVE reads back APPROVED at the head, by the list, by its id and in gh pr view", async () => {
    const run = builtWorld("i");
    const head = JSON.parse(readFileSync(path.join(run, "world.json"), "utf8")).head;
    const posted = await postReview(run, {
      event: "APPROVE",
      body: "Looks right.",
      commit_id: head,
    });
    const sent = JSON.parse(posted.stdout);
    const byId = await gh(run, ["api", `repos/${RED_REPO}/pulls/${RED_PR}/reviews/${sent.id}`]);
    const view = await gh(run, ["pr", "view", String(RED_PR), "--json", "reviews,latestReviews"]);
    const shown = { state: "APPROVED", body: "Looks right.", commit_id: head };
    expect({
      posted: { state: sent.state, body: sent.body, commit_id: sent.commit_id, user: sent.user },
      list: (await reviewsOf(run)).map((review: { id: number }) => review.id),
      byId: JSON.parse(byId.stdout),
      view: JSON.parse(view.stdout),
    }).toEqual({
      posted: { ...shown, user: { login: "rig[bot]" } },
      list: [sent.id],
      byId: sent,
      // gh reads the reviews through GraphQL, which names a bot by its bare login; REST's
      // review above names it rig[bot].
      view: {
        reviews: [
          {
            id: sent.node_id,
            author: { login: "rig" },
            authorAssociation: "NONE",
            body: "Looks right.",
            submittedAt: sent.submitted_at,
            includesCreatedEdit: false,
            reactionGroups: [],
            state: "APPROVED",
            commit: { oid: head },
          },
        ],
        latestReviews: [
          {
            id: "",
            author: { login: "rig" },
            authorAssociation: "NONE",
            body: "Looks right.",
            submittedAt: sent.submitted_at,
            includesCreatedEdit: false,
            reactionGroups: [],
            state: "APPROVED",
            commit: { oid: "" },
          },
        ],
      },
    });
  });

  // Catches: a changes request that reads back as an approval, or as no review, through `gh api`
  // fields or `gh pr review`.
  test("a REQUEST_CHANGES reads back CHANGES_REQUESTED, through gh api fields and gh pr review", async () => {
    const run = builtWorld("j");
    await gh(run, [
      "api",
      "-X",
      "POST",
      `repos/${RED_REPO}/pulls/${RED_PR}/reviews`,
      "-f",
      "event=REQUEST_CHANGES",
      "-f",
      "body=test (notify) is red at the head.",
    ]);
    await gh(run, ["pr", "review", String(RED_PR), "--request-changes", "-b", "Still red."]);
    expect(
      (await reviewsOf(run)).map((review: { state: string; body: string }) => [
        review.state,
        review.body,
      ])
    ).toEqual([
      ["CHANGES_REQUESTED", "test (notify) is red at the head."],
      ["CHANGES_REQUESTED", "Still red."],
    ]);
  });

  // Catches: a second review that replaces the first, or reads back before it.
  test("two posted reviews read back as two, in the order sent, each under its own id", async () => {
    const run = builtWorld("i");
    const first = JSON.parse(
      (await postReview(run, { event: "REQUEST_CHANGES", body: "Red." })).stdout
    );
    const second = JSON.parse((await postReview(run, { event: "APPROVE", body: "Green." })).stdout);
    expect(
      (await reviewsOf(run)).map((review: { id: number; state: string }) => [
        review.id,
        review.state,
      ])
    ).toEqual([
      [first.id, "CHANGES_REQUESTED"],
      [second.id, "APPROVED"],
    ]);
    expect(second.id).not.toBe(first.id);
  });

  // Catches: a review by an id the role never sent answered with one it did send, or with any
  // review at all, before or after the role sends one.
  test("a review by an id no review the role sent has answers GitHub's 404", async () => {
    const run = builtWorld("i");
    const unknown = () => gh(run, ["api", `repos/${RED_REPO}/pulls/${RED_PR}/reviews/6999`]);
    const before = await unknown();
    await postReview(run, { event: "APPROVE", body: "Looks right." });
    const after = await unknown();
    expect([before, after].map((read) => [read.stdout, read.stderr, read.exit])).toEqual([
      githubAnswer("review-404"),
      githubAnswer("review-404"),
    ]);
  });

  /** A PUT of `endpoint` under the pull request's review `id`, its body an --input file. */
  async function putReview(run: string, id: number, endpoint: string, body: object) {
    const file = path.join(run, `put-${Date.now()}-${Math.random()}.json`);
    writeFileSync(file, JSON.stringify(body));
    return gh(run, [
      "api",
      "--method",
      "PUT",
      `repos/${RED_REPO}/pulls/${RED_PR}/reviews/${id}${endpoint}`,
      "--input",
      file,
    ]);
  }

  // INFRA-1303: trial-i-head-4 of the tester's batch at 585b19a3 updated the body of the review it
  // had sent (`gh api --method PUT …/reviews/6002 --input upd.json`); the rig refused it as
  // unmodeled, and the reviewer sent a second review. Catches: the update refused, answered with
  // the review as it was or with a field other than the body changed, and a list or a read by id
  // that answers the old body after it.
  test("an update of a sent review's body answers the review with that body, and every read reads it", async () => {
    const run = builtWorld("i");
    const sent = JSON.parse((await postReview(run, { event: "APPROVE", body: "Approve." })).stdout);
    const updated = await putReview(run, sent.id, "", { body: "Approve, with the footer." });
    const byId = await gh(run, ["api", `repos/${RED_REPO}/pulls/${RED_PR}/reviews/${sent.id}`]);
    expect({
      // gh's line and exit first: a refusal is named there, with nothing on stdout.
      updated: [updated.stderr, updated.exit, JSON.parse(updated.stdout || "null")],
      list: await reviewsOf(run),
      byId: JSON.parse(byId.stdout),
    }).toEqual({
      updated: ["", 0, { ...sent, body: "Approve, with the footer." }],
      list: [{ ...sent, body: "Approve, with the footer." }],
      byId: { ...sent, body: "Approve, with the footer." },
    });
  });

  // INFRA-1303: the same run then dismissed that review (`PUT …/reviews/6002/dismissals`, with a
  // message and the event DISMISS), which the rig refused as unmodeled too. Catches: the dismissal
  // refused, answered with the review in its old state or with another field changed, a list that
  // reads the review undismissed after it, and a COMMENTED review dismissed where GitHub answers
  // its 422.
  test("a dismissal of a sent approval answers it DISMISSED and the list reads it; a comment-only review answers GitHub's 422", async () => {
    const run = builtWorld("i");
    const approved = JSON.parse(
      (await postReview(run, { event: "APPROVE", body: "Approve." })).stdout
    );
    const commented = JSON.parse(
      (await postReview(run, { event: "COMMENT", body: "A note." })).stdout
    );
    const dismissal = {
      message: "Resubmitting with the required Legion footer.",
      event: "DISMISS",
    };
    const dismissed = await putReview(run, approved.id, "/dismissals", dismissal);
    const refused = await putReview(run, commented.id, "/dismissals", dismissal);
    expect({
      dismissed: [dismissed.stderr, dismissed.exit, JSON.parse(dismissed.stdout || "null")],
      list: (await reviewsOf(run)).map((review: { id: number; state: string }) => [
        review.id,
        review.state,
      ]),
      refused: [refused.stdout, refused.stderr, refused.exit],
    }).toEqual({
      dismissed: ["", 0, { ...approved, state: "DISMISSED" }],
      list: [
        [approved.id, "DISMISSED"],
        [commented.id, "COMMENTED"],
      ],
      refused: githubAnswer("review-dismiss-422"),
    });
  });

  /** The inline comments of the pull request, or of one review, as `path line body` rows. */
  const commentRows = async (run: string, endpoint: string) => {
    const read = await gh(run, [
      "api",
      `repos/${RED_REPO}/pulls/${RED_PR}/${endpoint}`,
      "--jq",
      ".[] | [.path, .line, .body, .commit_id, .user.login, .pull_request_review_id] | @tsv",
    ]);
    return [read.stdout, read.exit];
  };

  // Catches: a reviewer's inline comments that read back as none, as fewer, out of order, on
  // another path, line or commit, or under no review, from the pull request's review comments, from
  // the review's own, or as the review threads the reviewer's prompt reads. Three reviewers of the
  // 158 archived runs read their inline comments back (trial-i-ablate-1, trial-j-head-4,
  // trial-j-nos1-3) and one the threads after them (round 8's trial-i-head-5).
  test("a review posted with two inline comments reads back both, from the pull request and the review", async () => {
    const run = builtWorld("j");
    const head = JSON.parse(readFileSync(path.join(run, "world.json"), "utf8")).head;
    const review = JSON.parse(
      (
        await postReview(run, {
          event: "REQUEST_CHANGES",
          body: "test (notify) is red at the head.",
          commit_id: head,
          comments: [
            { path: "greet.ts", line: 2, side: "RIGHT", body: "Trim before the greeting." },
            { path: "greet.test.ts", line: 10, side: "RIGHT", body: "Cover an empty name too." },
          ],
        })
      ).stdout
    );
    const rows =
      `greet.ts\t2\tTrim before the greeting.\t${head}\trig[bot]\t${review.id}\n` +
      `greet.test.ts\t10\tCover an empty name too.\t${head}\trig[bot]\t${review.id}\n`;
    expect([
      await commentRows(run, "comments"),
      await commentRows(run, `reviews/${review.id}/comments`),
    ]).toEqual([
      [rows, 0],
      [rows, 0],
    ]);
    const threads = await gh(run, [
      "api",
      "graphql",
      "-f",
      'query=query($n:Int!){repository(owner:"example",name:"widgets"){pullRequest(number:$n){reviewThreads(first:100){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}',
      "-F",
      `n=${RED_PR}`,
      "--jq",
      ".data.repository.pullRequest.reviewThreads.nodes[] | [.isResolved, .comments.nodes[0].author.login, .comments.nodes[0].body] | @tsv",
    ]);
    // GraphQL names the comments' bot author by its bare login, rig (REST names it rig[bot]).
    expect([threads.stdout, threads.exit]).toEqual([
      "false\trig\tTrim before the greeting.\nfalse\trig\tCover an empty name too.\n",
      0,
    ]);
    // A query that asks less is answered no more than it asks, whatever its --jq names.
    const counted = await gh(run, [
      "api",
      "graphql",
      "-f",
      'query=query($n:Int!){repository(owner:"example",name:"widgets"){pullRequest(number:$n){reviewThreads(first:100){totalCount nodes{id isResolved}}}}}',
      "-F",
      `n=${RED_PR}`,
      "--jq",
      '.data.repository.pullRequest.reviewThreads | [.totalCount, (.nodes[0] | keys), has("pageInfo")]',
    ]);
    expect(JSON.parse(counted.stdout)).toEqual([2, ["id", "isResolved"], false]);
  });

  // Catches: a review with no inline comment whose comments read back another review's, or are
  // refused.
  test("a review posted with no inline comment reads back none", async () => {
    const run = builtWorld("i");
    const review = JSON.parse(
      (await postReview(run, { event: "APPROVE", body: "Looks right." })).stdout
    );
    expect([
      await commentRows(run, "comments"),
      await commentRows(run, `reviews/${review.id}/comments`),
    ]).toEqual([
      ["", 0],
      ["", 0],
    ]);
  });

  // Catches: the comments of a review the role never sent answered with another review's, or
  // with none, where GitHub answers its 404; trial-j-ablate-4 of round 9 read review 6001's.
  test("the comments of a review id no review the role sent has answer GitHub's 404", async () => {
    const run = builtWorld("j");
    await postReview(run, {
      event: "COMMENT",
      body: "One line.",
      comments: [{ path: "greet.ts", line: 1, body: "Here." }],
    });
    const unknown = await gh(run, [
      "api",
      `repos/${RED_REPO}/pulls/${RED_PR}/reviews/6001/comments`,
    ]);
    expect([unknown.stdout, unknown.stderr, unknown.exit]).toEqual(
      githubAnswer("review-comments-404")
    );
  });

  /** A GraphQL read of the review threads of the world's pull request whose threads ask `nodes`. */
  const threadsRead = (run: string, nodes: string) =>
    gh(run, [
      "api",
      "graphql",
      "-f",
      `query=query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){pageInfo{hasNextPage endCursor} nodes{${nodes}}}}}}`,
      "-F",
      "o=example",
      "-F",
      "r=widgets",
      "-F",
      `n=${RED_PR}`,
    ]);
  const PROMPT_NODES = "id isResolved comments(last:1){nodes{author{login} body}}";

  // Catches: a thread read back where the reviewer opened none, which a reviewer whose prompt
  // reads the threads before it approves would take for a conversation left open.
  test("a review with no inline comment reads back no review thread", async () => {
    const run = builtWorld("i");
    await postReview(run, { event: "APPROVE", body: "Looks right." });
    const read = await threadsRead(run, PROMPT_NODES);
    expect([JSON.parse(read.stdout), read.exit]).toEqual([
      {
        data: {
          repository: {
            pullRequest: {
              reviewThreads: { pageInfo: { hasNextPage: false, endCursor: null }, nodes: [] },
            },
          },
        },
      },
      0,
    ]);
  });

  // Catches: inline comments that open no thread, one thread for both, a thread read back as
  // resolved, or a thread whose last comment is another than the one the reviewer wrote. Round
  // 8's trial-i-head-5 read the threads after its inline review.
  test("a review with two inline comments reads back two unresolved threads, each with its comment", async () => {
    const run = builtWorld("j");
    await postReview(run, {
      event: "REQUEST_CHANGES",
      body: "test (notify) is red at the head.",
      comments: [
        { path: "greet.ts", line: 2, body: "Trim before the greeting." },
        { path: "greet.test.ts", line: 10, body: "Cover an empty name too." },
      ],
    });
    const read = await threadsRead(run, PROMPT_NODES);
    const threads = JSON.parse(read.stdout).data.repository.pullRequest.reviewThreads;
    expect([
      read.exit,
      threads.pageInfo.hasNextPage,
      threads.nodes.map((thread: { isResolved: boolean; comments: unknown }) => [
        thread.isResolved,
        thread.comments,
      ]),
      new Set(threads.nodes.map((thread: { id: string }) => thread.id)).size,
    ]).toEqual([
      0,
      false,
      [
        [false, { nodes: [{ author: { login: "rig" }, body: "Trim before the greeting." }] }],
        [false, { nodes: [{ author: { login: "rig" }, body: "Cover an empty name too." }] }],
      ],
      2,
    ]);
  });

  // Catches: a threads query asking fields the world does not hold of a thread (its path and
  // line) answered with the fields it does hold, a trimmed guess, where the rig has no answer;
  // GraphQL answers the selection the world holds, and refuses the rest (gh-graphql.ts).
  test("a threads query asking a field the world does not hold of a thread is refused as unmodeled", async () => {
    const run = builtWorld("j");
    await postReview(run, {
      event: "REQUEST_CHANGES",
      body: "test (notify) is red at the head.",
      comments: [{ path: "greet.ts", line: 2, body: "Trim before the greeting." }],
    });
    const read = await threadsRead(run, "id isResolved path line");
    expect([
      read.stdout,
      read.stderr.startsWith("rig: request not modeled: gh api graphql "),
      read.exit,
    ]).toEqual(["", true, 1]);
  });

  // Catches: a reviewer that reads its own login through GraphQL and is answered another than
  // the one `gh api user` answers, the login every kept review carries, or is refused.
  test("the GraphQL viewer's login is the login gh api user answers", async () => {
    const run = redFixture("i");
    const viewer = await gh(run, [
      "api",
      "graphql",
      "-f",
      "query={viewer{login}}",
      "--jq",
      ".data.viewer.login",
    ]);
    const user = await gh(run, ["api", "user", "--jq", ".login"]);
    expect([viewer.stdout, viewer.exit]).toEqual([user.stdout, 0]);
    expect(user.stdout).toBe("rig[bot]\n");
  });

  // Catches: `gh --version` refused, and a version other than the one the rig measured gh by.
  test("gh --version answers gh 2.98.0", async () => {
    expect((await gh(redFixture("a"), ["--version"])).stdout).toBe(
      "gh version 2.98.0 (2026-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.98.0\n"
    );
  });

  // Catches: a GraphQL read of the pull request answered from fields the object does not hold,
  // and one that asks a field the rig cannot state (its reviews) answered at all.
  test("a GraphQL read of the pull request answers the fields its object holds, and no other", async () => {
    const run = redFixture("a");
    const query = (fields: string) =>
      `query=query{repository(owner:"example",name:"widgets"){pullRequest(number:${RED_PR}){${fields}}}}`;
    const held = await gh(run, [
      "api",
      "graphql",
      "-f",
      query("headRefName reviewDecision"),
      "--jq",
      ".data.repository.pullRequest | [.headRefName, .reviewDecision]",
    ]);
    const reviews = await gh(run, [
      "api",
      "graphql",
      "-f",
      query("reviews(last:10){nodes{state}}"),
    ]);
    expect([
      JSON.parse(held.stdout),
      reviews.exit,
      reviews.stderr.startsWith("rig: request not modeled"),
    ]).toEqual([[`legion/${RED_KEY}`, null], 1, true]);
  });

  // Catches: a read of the base, the repository, a tree, a ref or the root contents answered from
  // anything but the remote the fixture pushed, and a branch the remote has not got answered. A
  // tree read answers the top level alone, and every subtree and blob under it where it asks
  // `recursive`, as GitHub's git/trees does (measured, gittree-root), now that the base holds the
  // world's workflows under .github/workflows.
  test("the base, the repository, its trees, refs and contents answer the remote", async () => {
    const run = redFixture("a");
    /** One read of `endpoint` under the repository, or of the repository itself where it is "". */
    const read = async (endpoint: string, filter: string) =>
      (
        await gh(run, [
          "api",
          endpoint === "" ? `repos/${RED_REPO}` : `repos/${RED_REPO}/${endpoint}`,
          "--jq",
          filter,
        ])
      ).stdout.trim();
    const trunk = await gh(run, ["api", `repos/${RED_REPO}/branches/trunk`]);
    expect({
      base: await read("commits/main", ".sha"),
      branch: await read("", ".default_branch"),
      tree: await read("git/trees/main?recursive=1", ".tree[].path"),
      top: await read("git/trees/main", ".tree[].path"),
      ref: await read("git/ref/heads/main", ".object.sha"),
      contents: await read("contents/?ref=main", ".[].name"),
      rules: await read("rules/branches/trunk", "length"),
      trunk: [trunk.stderr, trunk.exit],
    }).toEqual({
      base: git(run, "rev-parse", "main").trim(),
      branch: "main",
      tree: git(run, "ls-tree", "-r", "-t", "--name-only", "main").trim(),
      top: git(run, "ls-tree", "--name-only", "main").trim(),
      ref: git(run, "rev-parse", "main").trim(),
      contents: git(run, "ls-tree", "--name-only", "main").trim(),
      rules: "0",
      trunk: ["gh: Branch not found (HTTP 404)\n", 1],
    });
  });

  // Catches: a read of another repository answered with this world's, which a role that named the
  // wrong repository takes for its own, and that refusal counted as unmodeled.
  test("a repository other than the world's is refused as GitHub and gh refuse it", async () => {
    const run = builtWorld("a");
    const rest = await gh(run, ["api", "repos/skills-rig/project/branches/main"]);
    const list = await gh(run, ["pr", "list", "--repo", "skills-rig/project"]);
    const view = await gh(run, [
      "run",
      "view",
      String(RED_RUN_ID),
      "--repo",
      "skills-rig/project",
      "--log-failed",
    ]);
    expect([rest, list, view].map((read) => [read.stderr, read.exit])).toEqual([
      ["gh: Not Found (HTTP 404)\n", 1],
      [
        "GraphQL: Could not resolve to a Repository with the name 'skills-rig/project'. (repository)\n",
        1,
      ],
      [
        `failed to get run: HTTP 404: Not Found (https://api.github.com/repos/skills-rig/project/actions/runs/${RED_RUN_ID}?exclude_pull_requests=true)\n`,
        1,
      ],
    ]);
    expect(ghCalls(run).some((call) => "unmodeled" in call)).toBe(false);
  });
});

// The world as one repository, its workflows, and the reads the batch of record at 6a643564 made
// that the rig did not answer (INFRA-1303, the architect's list for the lap after that batch).
describe("the world's repository, workflows and reads", () => {
  const withoutGo = skipWithout("go");
  const ALL = ["green", ...RED_WORLDS] as const;
  const world = (run: string): { repo: string; base: string; head: string; base_sha: string } =>
    JSON.parse(readFileSync(path.join(run, "world.json"), "utf8"));
  const read = async (run: string, args: string[]) => {
    const answer = await gh(run, args);
    return [answer.stdout, answer.stderr, answer.exit];
  };

  // D1. Catches: a world whose pull request, repository or runs name a repository or a default
  // branch other than the one world.json holds, its one source (world_repository). It builds every
  // world, so it has a bound of its own, about four times its time alone: 68.21 s alone on the
  // pod's own PATH with nothing else running (the implementer, 2026-11-19, on fold13), 16.23 s in
  // the whole suite quiet and 28.01 s beside one CPU-bound loop, where earlier rows built the worlds.
  test("every world's pull request, repository and runs name the repository and branch world.json holds", async () => {
    const seen: unknown[] = [];
    for (const name of ALL) {
      const run = name === "green" ? builtWorld("green") : redFixture(name);
      const { repo, base } = world(run);
      const pr = await gh(run, ["pr", "view", String(RED_PR), "--json", "baseRefName,url"]);
      const pull = await gh(run, [
        "api",
        `repos/${RED_REPO}/pulls/${RED_PR}`,
        "--jq",
        "[.base.ref, .base.repo.full_name, .head.repo.full_name] | @tsv",
      ]);
      const repository = await gh(run, [
        "repo",
        "view",
        "--json",
        "nameWithOwner,defaultBranchRef",
      ]);
      const runs =
        name === "green"
          ? ""
          : (
              await gh(run, [
                "api",
                `repos/${RED_REPO}/actions/runs`,
                "--jq",
                "[.workflow_runs[].repository.full_name] | unique | .[]",
              ])
            ).stdout;
      seen.push({
        name,
        world: [repo, base],
        pr: JSON.parse(pr.stdout),
        pull: pull.stdout,
        repository: JSON.parse(repository.stdout),
        runs,
      });
    }
    expect(seen).toEqual(
      ALL.map((name) => ({
        name,
        world: [RED_REPO, "main"],
        pr: { baseRefName: "main", url: `https://github.com/${RED_REPO}/pull/${RED_PR}` },
        pull: `main\t${RED_REPO}\t${RED_REPO}\n`,
        repository: { defaultBranchRef: { name: "main" }, nameWithOwner: RED_REPO },
        runs: name === "green" ? "" : `${RED_REPO}\n`,
      }))
    );
  }, 280_000);

  // D1. Catches: a pane whose addressing names a repository, a default branch or an owner the
  // world does not hold (`skills-rig/project`, branch `trunk`, at 6a643564: seven runs found no
  // such repository and concluded there was no CI to read), and a gh bundle under another owner.
  // The addressing comes from the daemon's own AddressingFragment, through daemon-pane.go, whose
  // `go run` builds once a world: 182.28 s alone on the pod's own PATH with nothing else running
  // (the implementer, 2026-11-19, on fold13), 111.99 s in the whole suite quiet and 219.46 s beside
  // one CPU-bound loop, so its bound is about four times its time alone.
  test.skipIf(withoutGo)(
    "every world's pane addresses the repository, branch and owner world.json holds",
    () => {
      const home = freshDir("addressing-home");
      const ghPath = path.join(import.meta.dir, "gh-standin.ts");
      const seen = ALL.map((name) => {
        const run = name === "green" ? builtWorld("green") : redFixture(name);
        const { repo, base } = world(run);
        const pane = workerPane(
          { rig: run, port: 13399, profile: "rig-profile" },
          { HOME: home, PATH: process.env.PATH ?? "" },
          { project: "skillsrig", issue: RED_KEY, role: "tester" },
          { repository: repo, defaultBranch: base },
          { daemonModule: DAEMON_MODULE, systemPrompt: true, ghPath }
        );
        const promptFile = (pane.systemPromptArgument ?? "").replace("--append-system-prompt ", "");
        const prompt = readFileSync(promptFile.replace(/^'|'$/g, ""), "utf8");
        return {
          name,
          addressing: prompt.includes(
            `Your issue's repository is ${repo} (default branch ${base}); your role's GitHub App is installed on the owner ${repo.split("/")[0]}, and its token reaches every repository of that owner.`
          ),
          owner: path.basename(pane.env.GH_CONFIG_DIR ?? ""),
        };
      });
      expect(seen).toEqual(ALL.map((name) => ({ name, addressing: true, owner: "example" })));
    },
    730_000
  );

  // D2. Catches: `gh pr checks --json` unmodeled (trial-l-head-1 of the batch of record, a rig
  // error), a field gh lists answered otherwise than measured, and a re-run's attempt read as
  // settled in the turn that pressed it.
  test("gh pr checks --json answers gh's fields, before, while and after the re-run", async () => {
    const run = ownFixture("a");
    const json = ["pr", "checks", String(RED_PR), "--json", "name,state,bucket,link"];
    const job = (id: number) => `${RED_RUN_URL}/job/${id}`;
    const before = await read(run, json);
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    const running = await read(run, [...json.slice(0, 4), "bucket,completedAt,state"]);
    await settleRerun(run);
    const after = await read(run, json);
    const every = await read(run, [
      ...json.slice(0, 4),
      "bucket,completedAt,description,event,link,name,startedAt,state,workflow",
    ]);
    const bogus = await read(run, [...json.slice(0, 4), "bogus"]);
    const check = (name: string, id: number, bucket: string, state: string) => ({
      bucket,
      link: job(id),
      name,
      state,
    });
    expect({ before, running, after, every: JSON.parse(every[0] as string)[1], bogus }).toEqual({
      before: [
        `${JSON.stringify([
          check("lint", RED_JOB_ID + 1, "pass", "SUCCESS"),
          check("test (notify)", RED_JOB_ID, "fail", "FAILURE"),
          check("typecheck", RED_JOB_ID + 2, "pass", "SUCCESS"),
        ])}\n`,
        "",
        0,
      ],
      running: [
        `${JSON.stringify([
          { bucket: "pass", completedAt: "2026-03-04T10:00:00Z", state: "SUCCESS" },
          { bucket: "pending", completedAt: "0001-01-01T00:00:00Z", state: "IN_PROGRESS" },
          { bucket: "pass", completedAt: "2026-03-04T10:00:00Z", state: "SUCCESS" },
        ])}\n`,
        "",
        0,
      ],
      after: [
        `${JSON.stringify([
          check("lint", RED_CARRIED_JOB.lint, "pass", "SUCCESS"),
          check("test (notify)", RED_JOB_ID + 4, "pass", "SUCCESS"),
          check("typecheck", RED_CARRIED_JOB.typecheck, "pass", "SUCCESS"),
        ])}\n`,
        "",
        0,
      ],
      every: {
        bucket: "pass",
        completedAt: "2026-03-04T10:31:08Z",
        description: "",
        event: "pull_request",
        link: job(RED_JOB_ID + 4),
        name: "test (notify)",
        startedAt: "2026-03-04T10:31:08Z",
        state: "SUCCESS",
        workflow: "ci",
      },
      bogus: [
        "",
        `Unknown JSON field: "bogus"\nAvailable fields:\n${[
          "bucket",
          "completedAt",
          "description",
          "event",
          "link",
          "name",
          "startedAt",
          "state",
          "workflow",
        ]
          .map((field) => `  ${field}\n`)
          .join("")}`,
        1,
      ],
    });
  });

  // The three reads. Catches: each unmodeled in the batch of record: `gh run list --branch main
  // --workflow ci` (trial-e-head-4, refused by a program error), the workflows directory
  // (trial-l-ablate-1) and a comparison of the base with the head (trial-l-ablate-4).
  test("a run list by branch and workflow, the workflows directory and the comparison answer the world", async () => {
    const e = redFixture("e");
    const l = redFixture("l");
    const { head, base_sha } = world(l);
    expect({
      main: await read(e, ["run", "list", "--branch", "main", "--workflow", "ci", "--limit", "3"]),
      named: await read(e, [
        "run",
        "list",
        "--workflow",
        "E2E",
        "--json",
        "name",
        "--jq",
        ".[].name",
      ]),
      unknown: await read(e, ["run", "list", "--workflow", "nope"]),
      directory: await read(l, [
        "api",
        `repos/${RED_REPO}/contents/.github/workflows`,
        "--jq",
        ".[] | [.name, .type, .path] | @tsv",
      ]),
      missing: await read(l, ["api", `repos/${RED_REPO}/contents/.github/workflows/ci.yml`]),
      compare: await read(l, [
        "api",
        `repos/${RED_REPO}/compare/${base_sha}...${head}`,
        "--jq",
        '[.status, .ahead_by, .behind_by, ([.files[].filename] | join(","))] | @tsv',
      ]),
      unknownRef: await read(l, ["api", `repos/${RED_REPO}/compare/nope...${head}`]),
    }).toEqual({
      main: ["", "", 0],
      named: ["e2e\n", "", 0],
      unknown: ["", "could not find any workflows named nope\n", 1],
      directory: ["ci.yaml\tfile\t.github/workflows/ci.yaml\n", "", 0],
      missing: [
        readFileSync(path.join(import.meta.dir, "github-answers/contents-404.stdout"), "utf8"),
        readFileSync(path.join(import.meta.dir, "github-answers/contents-404.stderr"), "utf8"),
        1,
      ],
      compare: ["ahead\t1\t0\tgreet.test.ts,greet.ts\n", "", 0],
      unknownRef: [
        readFileSync(path.join(import.meta.dir, "github-answers/compare-404.stdout"), "utf8"),
        readFileSync(path.join(import.meta.dir, "github-answers/compare-404.stderr"), "utf8"),
        1,
      ],
    });
  });

  // (1). Catches: rows that print a time the world's checks never took (`20s` for a check that
  // started and completed in one second) and that end without the empty description gh prints
  // after a tab (output.go:47-63 at v2.98.0; measured on cybermonklabs/legion#3514), and a
  // pending check exiting 0 or 1 where gh exits 8.
  test("gh pr checks prints each row as gh does: its elapsed time, its link and its description", async () => {
    const green = builtWorld("green");
    const a = ownFixture("a");
    const before = await read(a, ["pr", "checks", String(RED_PR)]);
    await gh(a, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    const running = await read(a, ["pr", "checks", String(RED_PR)]);
    const job = (id: number) => `${RED_RUN_URL}/job/${id}`;
    expect({ green: await read(green, ["pr", "checks", String(RED_PR)]), before, running }).toEqual(
      {
        green: [
          "test\tpass\t0\thttps://example.invalid/2\t\nlint\tpass\t0\thttps://example.invalid/1\t\n",
          "",
          0,
        ],
        before: [
          `lint\tpass\t0\t${job(RED_JOB_ID + 1)}\t\n` +
            `test (notify)\tfail\t0\t${job(RED_JOB_ID)}\t\n` +
            `typecheck\tpass\t0\t${job(RED_JOB_ID + 2)}\t\n`,
          "",
          1,
        ],
        running: [
          `lint\tpass\t0\t${job(RED_CARRIED_JOB.lint)}\t\n` +
            `test (notify)\tpending\t0\t${job(RED_JOB_ID + 4)}\t\n` +
            `typecheck\tpass\t0\t${job(RED_CARRIED_JOB.typecheck)}\t\n`,
          "",
          8,
        ],
      }
    );
  });

  // (2). Catches: a run view whose first line leaves out the pull request of the run's branch,
  // which gh 2.98.0 names after the workflow (view.go:349-352; measured `* legion/INFRA-1303 CI
  // cybermonklabs/legion#3514 · 44586483868`).
  test("gh run view names the pull request of the run's branch after its workflow", async () => {
    const view = await gh(redFixture("a"), ["run", "view", String(RED_RUN_ID)]);
    expect(view.stdout.split("\n")[1]).toBe(
      `X legion/${RED_KEY} ci ${RED_REPO}#${RED_PR} · ${RED_RUN_ID}`
    );
  });

  // (3). Catches: runs that name a workflow file the repository does not hold, and a pull request
  // that changes a workflow, which would make the step reds the change's by reading 4 of the
  // page. Every world's base holds the file each run names, and the pull request changes nothing
  // under .github, through the files read or on the world's remote. Its bound is about four times
  // its time alone: 57.91 s alone on the pod's own PATH with nothing else running (the implementer,
  // 2026-11-19, on fold13), 10.20 s in the whole suite quiet and 20.43 s beside one CPU-bound loop.
  test("every world's base holds the workflows its runs name, and its pull request changes none", async () => {
    const seen: unknown[] = [];
    for (const name of ALL) {
      const run = name === "green" ? builtWorld("green") : redFixture(name);
      const { head, base_sha } = world(run);
      const remote = path.join(run, "remote.git");
      const tree = Bun.spawnSync(["git", "-C", remote, "ls-tree", "-r", "--name-only", base_sha])
        .stdout.toString()
        .split("\n")
        .filter((line) => line !== "");
      const diff = Bun.spawnSync(["git", "-C", remote, "diff", "--name-only", base_sha, head])
        .stdout.toString()
        .split("\n")
        .filter((line) => line !== "");
      const files = await gh(run, [
        "api",
        `repos/${RED_REPO}/pulls/${RED_PR}/files`,
        "--jq",
        ".[].filename",
      ]);
      const paths =
        name === "green"
          ? []
          : outputLines(
              (
                await gh(run, [
                  "api",
                  `repos/${RED_REPO}/actions/runs`,
                  "--jq",
                  ".workflow_runs[].path",
                ])
              ).stdout
            );
      seen.push({
        name,
        named: paths.every((file) => tree.includes(file)),
        workflows: tree.some((file) => file.startsWith(".github/workflows/")),
        changed: [...diff, ...outputLines(files.stdout)].filter((file) =>
          file.startsWith(".github/")
        ),
      });
    }
    expect(seen).toEqual(ALL.map((name) => ({ name, named: true, workflows: true, changed: [] })));
  }, 240_000);

  // (3), the architect's first condition. Catches: a workflow whose jobs or steps differ from the
  // jobs the world's runs hold, which would let a role read a step red as the change's or not by
  // luck: each job of each run of the head names, after GitHub's own `Set up job`, the steps of
  // the job of that name in its workflow, in order, by the name GitHub gives a step (its own
  // `name`, else `Run <command>`); and where a step reads a file it names the directory, so the
  // lane of every foreign red names packages/notify and world `d`'s the root, while world `c`'s
  // set-up step reads none and downloads the Bun its log names. Its bound is about four times its
  // time alone: 51.86 s alone on the pod's own PATH with nothing else running (the implementer,
  // 2026-11-19, on fold13), 6.58 s in the whole suite quiet and 14.70 s beside one CPU-bound loop.
  test("each workflow's jobs and steps are the jobs and steps of the world's runs", async () => {
    type Step = { name?: string; run?: string; uses?: string; with?: Record<string, string> };
    const seen: unknown[] = [];
    for (const name of RED_WORLDS) {
      const run = redFixture(name);
      const { base_sha } = world(run);
      const remote = path.join(run, "remote.git");
      const runs: { id: number; path: string }[] = JSON.parse(
        (
          await gh(run, [
            "api",
            `repos/${RED_REPO}/actions/runs`,
            "--jq",
            "[.workflow_runs[] | {id, path}]",
          ])
        ).stdout
      );
      const lanes: string[] = [];
      let agree = true;
      for (const { id, path: file } of runs) {
        const yaml = Bun.YAML.parse(
          Bun.spawnSync(["git", "-C", remote, "show", `${base_sha}:${file}`]).stdout.toString()
        ) as { jobs: Record<string, { name: string; steps: Step[] }> };
        const listed: { name: string; steps: { name: string }[] }[] = JSON.parse(
          (
            await gh(run, [
              "api",
              `repos/${RED_REPO}/actions/runs/${id}/jobs?filter=all`,
              "--jq",
              "[.jobs[] | {name, steps}]",
            ])
          ).stdout
        );
        for (const job of listed) {
          const model = Object.values(yaml.jobs).find((entry) => entry.name === job.name);
          const named = (model?.steps ?? []).map((step) => step.name ?? `Run ${step.run}`);
          if (
            JSON.stringify(job.steps.map((step) => step.name)) !==
            JSON.stringify(["Set up job", ...named])
          )
            agree = false;
          for (const step of model?.steps ?? [])
            lanes.push(`${job.name}: ${step.run ?? `${step.uses} ${JSON.stringify(step.with)}`}`);
        }
      }
      seen.push({
        name,
        agree,
        lane: [...new Set(lanes)].filter((line) => !/^(lint|typecheck):/.test(line)),
      });
    }
    const notify = "test (notify): bun test packages/notify";
    const aggregate = `ci: test "\${{ !contains(needs.*.result, 'failure') }}" = true`;
    expect(seen).toEqual(
      RED_WORLDS.map((name) => ({
        name,
        agree: true,
        lane:
          name === "c"
            ? ['build (linux-x64): oven-sh/setup-bun@v2 {"bun-version":"1.4.4"}']
            : name === "d"
              ? ["test (widgets): bun test ."]
              : name === "e"
                ? [notify, "e2e: bun run e2e"]
                : name === "f"
                  ? [expect.stringMatching(/^title: \[\[ "\$TITLE" =~ /)]
                  : ["g", "i", "j"].includes(name)
                    ? [notify, aggregate]
                    : [notify],
      }))
    );
  }, 210_000);
});

// Catches: a wake the rig gives for a re-run nobody pressed. `gh run rerun --help`, with or
// without the run's id, and a re-run gh refuses with its usage press nothing (red_fixture answers
// each with gh's own text), so a rerun_pressed that took one for the press would settle a re-run
// nobody pressed and start the role's next turn on its settlement. rerun_pressed reads the count of
// the press route (`press`), which the stand-in moves on a press alone: the run's, or, counting as
// it, the press of the red's job and the REST press. It reads it once the turn that pressed it has
// ended, so it waits for nothing.
test("rerun_pressed takes the press of the head's re-run, its job's too, and no other call", async () => {
  const pressed = (run: string) => bash("R=$1\nrerun_pressed", [run]).code;
  const run = ownFixture("a");
  await gh(run, ["run", "rerun", "--help"]);
  await gh(run, ["run", "rerun", String(RED_RUN_ID), "--help"]);
  await gh(run, ["run", "rerun", "--failed"]);
  const before = pressed(run);
  await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
  const job = ownFixture("a");
  await gh(job, ["run", "rerun", "--job", String(RED_JOB_ID)]);
  expect([before, pressed(run), pressed(job)]).toEqual([1, 0, 0]);
});

/** The caller's own bearer, which no service the rig starts may be handed: random, as a real one
 * is, since a process that reads it holds it and scrubs it from every line it prints. */
const CALLER_TOKEN = fixtureSecret("caller");

// The variables a Legion pane exports for the live services, each set to a value of the caller's
// own, which nothing the rig starts may be handed: the rig's own Envoy is the one it writes into a
// run's pane.env, and its Dispatch the run's own server (rig.sh's start, base_env, boot_worker).
describe("the caller's Envoy, Dispatch and NATS", () => {
  // Catches: a gh stand-in that hands the Envoy, Dispatch or NATS of its own environment to a
  // route's program or a call's --jq filter, or that publishes a fixture's settlement to the
  // listener its own environment names rather than the run's. A stand-in run by hand from a Legion
  // pane on 2026-11-18 published a settlement to the live relay that way (topic
  // notifications.github.example.widgets.pr.1003.checks). The probe is the program and the filter
  // themselves: each prints every DISPATCH_, ENVOY_ and NATS_ variable of its own environment.
  test("reach no program, filter or publish of the gh stand-in, which publishes to the run's own listener", async () => {
    const reached = { caller: [] as string[], run: [] as string[] };
    const listen = (into: string[]) =>
      Bun.serve({
        port: 0,
        hostname: "127.0.0.1",
        fetch: (request) => {
          into.push(new URL(request.url).pathname);
          return Response.json({ ok: true });
        },
      });
    const callerListener = listen(reached.caller);
    const runListener = listen(reached.run);
    try {
      const run = freshDir("service-variables");
      paneListener(run, `http://127.0.0.1:${runListener.port}`);
      const probe = '$ENV | with_entries(select(.key | test("^(DISPATCH|ENVOY|NATS)_")))';
      writeFileSync(
        path.join(run, "fixtures.json"),
        JSON.stringify({
          routes: [
            {
              match: `^gh api GET repos/${RED_REPO}/environment(?:\\s|$)`,
              stdout: {},
              program: probe,
              publish: { topic: "notifications.github.example.widgets.pr.1003.checks" },
            },
          ],
        })
      );
      const callerToken = path.join(run, "caller-token");
      writeFileSync(callerToken, `${CALLER_TOKEN}\n`);
      const caller = {
        ENVOY_URL: `http://127.0.0.1:${callerListener.port}`,
        ENVOY_TOKEN_FILE: callerToken,
        ENVOY_NATS_URL: "nats://caller-nats.invalid:4222",
        ENVOY_CONFIG_FILE: path.join(run, "caller-envoy.json"),
        DISPATCH_URL: `http://127.0.0.1:${callerListener.port}`,
        DISPATCH_HOST: "caller-dispatch.invalid",
        DISPATCH_TOKEN_FILE: callerToken,
        DISPATCH_SESSION_ID: "caller-session",
      };
      const program = await gh(run, ["api", `repos/${RED_REPO}/environment`], caller);
      const filter = await gh(run, ["api", `repos/${RED_REPO}/environment`, "--jq", probe], caller);
      expect({
        program: [program.stdout, program.exit],
        filter: [filter.stdout, filter.exit],
        reached,
      }).toEqual({
        program: ["{}", 0],
        filter: ["{}\n", 0],
        reached: { caller: [], run: ["/v1/messages/publish", "/v1/messages/publish"] },
      });
    } finally {
      callerListener.stop(true);
      runListener.stop(true);
    }
  });
});

// The round-5 review's probes of worker_fixture's and red_fixture's routes (M2 to M5 and its
// third Minor), each a request a route answered with the head's state, or as a request GitHub
// would answer otherwise, before the routes were made to answer only what they name.
describe("a request the routes do not name is answered as GitHub answers it, or refused", () => {
  /** The rollup a GraphQL read answers: its state, null where the commit has none, or the
   * request's exit and stderr where it has no answer. */
  const rollup = async (
    run: string,
    args: string[]
  ): Promise<string | null | { exit: number; stderr: string }> => {
    const read = await gh(run, ["api", "graphql", ...args]);
    if (read.exit !== 0) return { exit: read.exit, stderr: read.stderr };
    const found = /"statusCheckRollup":(null|\{"state":"(\w+)"\})/.exec(read.stdout);
    return found === null ? read.stdout : (found[2] ?? null);
  };
  const query = (selection: string) =>
    `query={repository(owner:"example",name:"widgets"){${selection}}}`;
  const commitRollup = "... on Commit{statusCheckRollup{state}}";

  // Catches (M2): a GraphQL rollup read of the base, by the default branch, its ref, the pull
  // request's base ref or an expression, answered with the head's red, the round-9 class through
  // GraphQL; the pull request's own last commit read as null rather than the head's red; and
  // another pull request answered with this one's.
  test("a GraphQL rollup read answers the head's red only where it names the head", async () => {
    const run = redFixture("a");
    expect({
      defaultBranch: await rollup(run, ["-f", query(`defaultBranchRef{target{${commitRollup}}}`)]),
      mainRef: await rollup(run, [
        "-f",
        query(`ref(qualifiedName:"refs/heads/main"){target{${commitRollup}}}`),
      ]),
      baseRef: await rollup(run, [
        "-f",
        query(`pullRequest(number:${RED_PR}){baseRef{target{${commitRollup}}}}`),
      ]),
      expression: await rollup(run, [
        "-F",
        "ref=main",
        "-f",
        `query=query($ref:String!){repository(owner:"example",name:"widgets"){object(expression:$ref){${commitRollup}}}}`,
      ]),
      lastCommit: await rollup(run, [
        "-f",
        query(
          `pullRequest(number:${RED_PR}){commits(last:1){nodes{commit{statusCheckRollup{state}}}}}`
        ),
      ]),
      headRef: await rollup(run, [
        "-f",
        query(`pullRequest(number:${RED_PR}){headRef{target{${commitRollup}}}}`),
      ]),
      otherPull: await rollup(run, [
        "-f",
        query("pullRequest(number:999){commits(last:1){nodes{commit{statusCheckRollup{state}}}}}"),
      ]),
    }).toEqual({
      defaultBranch: null,
      mainRef: null,
      baseRef: null,
      expression: null,
      lastCommit: "FAILURE",
      headRef: "FAILURE",
      otherPull: {
        exit: 1,
        stderr: "gh: Could not resolve to a PullRequest with the number of 999.\n",
      },
    });
  });

  // Catches (M3): `repos/{owner}/{repo}/...` refused as another repository, where gh fills both
  // from GH_REPO, which the pane sets, before it sends the request.
  test("{owner} and {repo} read the repository GH_REPO names, as gh fills them", async () => {
    const run = redFixture("a");
    const env = { GH_REPO: RED_REPO };
    const pull = await gh(
      run,
      ["api", `repos/{owner}/{repo}/pulls/${RED_PR}`, "--jq", ".head.sha"],
      env
    );
    const runs = await gh(
      run,
      [
        "api",
        `repos/{owner}/{repo}/commits/${RED_HEAD}/check-runs`,
        "--jq",
        ".check_runs | length",
      ],
      env
    );
    expect([pull.stdout, pull.exit, runs.stdout, runs.exit]).toEqual([
      `${RED_HEAD}\n`,
      0,
      "3\n",
      0,
    ]);
  });

  // Catches (M4): a review or a comment sent through `gh api` with fields and no -X, which gh
  // sends as a POST, answered as a read and kept nowhere, so the role's review reads back as none
  // and its comment as never posted.
  test("a write with fields and no -X is the POST gh sends, and is kept as GitHub keeps it", async () => {
    const run = ownFixture("i");
    const reviews = `repos/${RED_REPO}/pulls/${RED_PR}/reviews`;
    const sent = await gh(run, [
      "api",
      reviews,
      "-f",
      "event=APPROVE",
      "-f",
      "body=LGTM",
      "--jq",
      ".state",
    ]);
    const listed = await gh(run, ["api", reviews, "--jq", 'map(.state) | join(",")']);
    const viewed = await gh(run, [
      "pr",
      "view",
      String(RED_PR),
      "--json",
      "reviews",
      "--jq",
      '.reviews[] | .state + " " + .author.login',
    ]);
    const comment = await gh(run, [
      "api",
      `repos/${RED_REPO}/issues/${RED_PR}/comments`,
      "-f",
      "body=hello",
    ]);
    const comments = await gh(run, ["pr", "view", String(RED_PR), "--json", "comments"]);
    expect({
      sent: sent.stdout,
      listed: listed.stdout,
      viewed: viewed.stdout,
      comment: comment.exit,
      comments: [comments.stderr, comments.exit],
    }).toEqual({
      sent: "APPROVED\n",
      listed: "APPROVED\n",
      // gh reads a review's author through GraphQL, which names a bot by its bare login.
      viewed: "APPROVED rig\n",
      comment: 0,
      comments: [`rig: request not modeled: gh pr view ${RED_PR} --json comments\n`, 1],
    });
  });

  // Catches (M5): another pull request, a branch or a URL answered with this pull request; a
  // `pulls` query whose filters are ignored; another attempt answered with the newest; and a
  // re-run of another job (lint, which passed) taken as the press of the red's own.
  test("another pull request, filter, attempt or job is refused as gh refuses it, or as unmodeled", async () => {
    const run = ownFixture("a");
    const answer = async (args: string[]) => {
      const read = await gh(run, args);
      return [read.stdout, read.stderr, read.exit];
    };
    const unmodeled = (args: string[]) => [
      "",
      `rig: request not modeled: gh ${args.join(" ")}\n`,
      1,
    ];
    const notFound = (n: number) => [
      "",
      `GraphQL: Could not resolve to a PullRequest with the number of ${n}. (repository.pullRequest)\n`,
      1,
    ];
    const other = [
      ["pr", "view", "main", "--json", "title"],
      ["pr", "view", "https://github.com/other/repo/pull/1003", "--json", "title"],
      ["run", "view", String(RED_RUN_ID), "--attempt", "1", "--json", "attempt,conclusion"],
      ["run", "rerun", "--job", String(RED_JOB_ID + 1)],
    ];
    expect({
      pull: await answer(["pr", "view", "999", "--json", "title"]),
      checks: await answer(["pr", "checks", "998"]),
      closed: await answer([
        "api",
        `repos/${RED_REPO}/pulls?head=example:main&state=closed`,
        "--jq",
        "length",
      ]),
      open: await answer([
        "api",
        `repos/${RED_REPO}/pulls?head=example:legion/${RED_KEY}&state=open`,
        "--jq",
        'map(.number) | join(",")',
      ]),
      other: await Promise.all(other.map(answer)),
    }).toEqual({
      pull: notFound(999),
      checks: notFound(998),
      closed: ["0\n", "", 0],
      open: [`${RED_PR}\n`, "", 0],
      other: other.map(unmodeled),
    });
  });

  // Catches (round 5's third Minor): the head's branch named with its slash encoded (`%2F`), as a
  // URL path carries it, answered as a branch GitHub does not have; a comment whose body holds
  // `-R src/dir`-like text refused as another repository; and the job flag written as gh takes it,
  // `-j<id>`, answered with the usage error rather than as the press.
  test("an encoded branch, a body that holds flag text and a joined -j each read as gh sends them", async () => {
    const run = ownFixture("a");
    const branch = await gh(run, [
      "api",
      `repos/${RED_REPO}/branches/legion%2F${RED_KEY}`,
      "--jq",
      ".commit.sha",
    ]);
    const comment = await gh(run, ["pr", "comment", String(RED_PR), "--body", "cp -R src/dir out"]);
    const pressed = await gh(run, ["run", "rerun", `-j${RED_JOB_ID}`]);
    await settleRerun(run);
    const checks = await gh(run, ["pr", "checks", String(RED_PR)]);
    expect({
      branch: branch.stdout,
      comment: [comment.stdout, comment.exit],
      pressed: [pressed.stdout, pressed.stderr, pressed.exit],
      checks: checks.exit,
    }).toEqual({
      branch: `${RED_HEAD}\n`,
      comment: [`https://github.com/${RED_REPO}/pull/${RED_PR}#issuecomment-6001\n`, 0],
      pressed: ["", "", 0],
      checks: 0,
    });
  });

  // Catches (M6b): `gh run rerun <id> --help` taken as the press: gh prints its help and re-runs
  // nothing, so the head must read red after it as before.
  test("gh run rerun <id> --help prints gh's help and presses nothing", async () => {
    const run = ownFixture("a");
    const help = await gh(run, ["run", "rerun", String(RED_RUN_ID), "--help"]);
    const checks = await gh(run, ["pr", "checks", String(RED_PR)]);
    expect([help.exit, help.stdout.split("\n")[0], checks.exit]).toEqual([
      0,
      "Rerun an entire run, only failed jobs, or a specific job from a run.",
      1,
    ]);
  });

  // Catches (replay A's residual, round 6 trial-b-ablate-2): a job view that also names a run
  // refused, where gh 2.98.0 reads the job and the run it belongs to, whatever run the call names
  // (measured: `gh run view 9001002 --job 8001003` asks for job 8001003, then run 7001003).
  test("a job view that also names a run reads the job, as gh does", async () => {
    const run = redFixture("a");
    const alone = await gh(run, ["run", "view", "--job", String(RED_JOB_ID), "--log"]);
    const named = await gh(run, [
      "run",
      "view",
      String(RED_RED_ID),
      "--job",
      String(RED_JOB_ID),
      "--log",
    ]);
    expect([named.stdout, named.stderr, named.exit]).toEqual([alone.stdout, "", 0]);
    expect(alone.stdout).toContain("(fail) notify > retries a refused send");
  });

  // Catches (round 6 trial-c-head-4): a `gh api` call with a flag `gh api` does not take
  // (`--json`) refused as a request the rig does not model, where gh 2.98.0 refuses it itself
  // before any request (measured), so the run is the role's to score.
  test("gh api with a flag it does not take answers gh's own refusal", async () => {
    const run = ownFixture("a");
    const read = await gh(run, [
      "api",
      `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}`,
      "--json",
      "status",
    ]);
    expect([read.stdout, read.stderr.split("\n").slice(0, 3), read.exit]).toEqual([
      "",
      ["unknown flag: --json", "", "Usage:  gh api <endpoint> [flags]"],
      1,
    ]);
    expect(ghCalls(run).some((call) => "unmodeled" in call)).toBe(false);
  });

  // Catches (round 6 trial-i-head-3 and trial-j-ablate-4, round 9 trial-j-nos1-2 and
  // trial-j-nos1-3): `gh pr view <n>` with no --json refused, where gh 2.98.0 prints the pull
  // request a field a line to no terminal (measured); a view whose body does not follow an edit
  // of it; and a view after a review, whose reviewers line is not measured, answered at all.
  test("gh pr view with no --json prints the pull request as gh does, and follows its body", async () => {
    const run = ownFixture("i");
    const view = () => gh(run, ["pr", "view", String(RED_PR), "--repo", RED_REPO]);
    // The fields as the same pull request answers them under --json.
    const pr = JSON.parse(
      (await gh(run, ["pr", "view", String(RED_PR), "--json", "body,additions,deletions"])).stdout
    );
    const before = await view();
    await gh(run, ["pr", "edit", String(RED_PR), "--body", "A new body."]);
    const edited = await view();
    await gh(run, [
      "api",
      `repos/${RED_REPO}/pulls/${RED_PR}/reviews`,
      "-f",
      "event=APPROVE",
      "-f",
      "body=Looks right.",
    ]);
    const reviewed = await view();
    const lines = (body: string) =>
      [
        `title:\t${RED_TITLE}`,
        "state:\tOPEN",
        "author:\trig",
        "labels:\t",
        "assignees:\t",
        "reviewers:\t",
        "projects:\t",
        "milestone:\t",
        `number:\t${RED_PR}`,
        `url:\thttps://github.com/${RED_REPO}/pull/${RED_PR}`,
        `additions:\t${pr.additions}`,
        `deletions:\t${pr.deletions}`,
        "auto-merge:\tdisabled",
        "--",
        body,
        "",
      ].join("\n");
    expect([before.stdout, before.exit]).toEqual([lines(pr.body), 0]);
    expect([edited.stdout, edited.exit]).toEqual([lines("A new body."), 0]);
    expect([
      reviewed.stdout,
      reviewed.exit,
      reviewed.stderr.startsWith("rig: request not modeled"),
    ]).toEqual(["", 1, true]);
  });
});

// The rule the review of 76cc2379 holds the routes to: a route reads every parameter, field, flag
// and variable a call hands it, or refuses the call. Each row below is a finding of that review,
// its probes as the reviewer made them.
describe("a route reads every parameter, field, flag and variable it is handed, or refuses", () => {
  /** A call's output lines where it exits 0, else its stderr and exit. */
  const lines = async (run: string, args: string[]) => {
    const read = await gh(run, args);
    return read.exit === 0 ? outputLines(read.stdout) : [read.stderr, read.exit];
  };
  const unmodeled = (args: string[]) => [`rig: request not modeled: gh ${args.join(" ")}\n`, 1];
  const checkRunsJq = '.total_count, (.check_runs[] | "\\(.id) \\(.name) \\(.conclusion)")';
  const checkRuns = (run: string, query = "", flags: string[] = []) =>
    lines(run, [
      "api",
      ...flags,
      `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs${query}`,
      "--jq",
      checkRunsJq,
    ]);
  const lint = `${RED_ID.lint} lint success`;
  const typecheck = `${RED_ID.typecheck} typecheck success`;
  const red = `${RED_RED_ID} test (notify) failure`;
  const carriedLint = `${RED_CARRIED.lint} lint success`;
  const carriedTypecheck = `${RED_CARRIED.typecheck} typecheck success`;

  // Catches (C1): the commit's check runs answered GitHub's `filter=all` list to every read: after
  // a green re-run the default read still held the failed attempt of `test (notify)`, world e's
  // held both e2e attempts before any press, and `check_name`, `status` and a GET's fields
  // filtered nothing, where GitHub's default is the latest check run of each name (measured on
  // cybermonklabs/legion 05e64382, github-answers/README.txt); a parameter the route does not read
  // was answered as if absent.
  test("the head's check runs answer the latest of each name, or every one with filter=all", async () => {
    const a = ownFixture("a");
    const before = await checkRuns(a);
    await gh(a, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(a);
    const appId = [
      "api",
      `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs?app_id=1`,
      "--jq",
      checkRunsJq,
    ];
    const e = redFixture("e");
    expect({
      before,
      after: await checkRuns(a),
      afterAll: await checkRuns(a, "?filter=all"),
      lintOnly: await checkRuns(a, "?check_name=lint"),
      inProgress: await checkRuns(a, "?status=in_progress"),
      lintField: await checkRuns(a, "", ["-X", "GET", "-f", "check_name=lint"]),
      appId: await lines(a, appId),
      e: await checkRuns(e),
      eAll: await checkRuns(e, "?filter=all&per_page=100"),
    }).toEqual({
      before: ["3", lint, typecheck, red],
      // The re-run carried lint and typecheck to attempt 2 as check runs of their own (red_model).
      after: ["3", `${RED_RED_ID + 1} test (notify) success`, carriedLint, carriedTypecheck],
      afterAll: [
        "6",
        lint,
        typecheck,
        red,
        `${RED_RED_ID + 1} test (notify) success`,
        carriedLint,
        carriedTypecheck,
      ],
      lintOnly: ["1", carriedLint],
      inProgress: ["0"],
      lintField: ["1", carriedLint],
      appId: unmodeled(appId),
      e: ["4", lint, typecheck, `${RED_RED_ID - 3} e2e success`, red],
      eAll: [
        "5",
        lint,
        typecheck,
        `${RED_RED_ID - 4} e2e failure`,
        `${RED_RED_ID - 3} e2e success`,
        red,
      ],
    });
  });

  // Catches (C2): a GET's `-f`/`-F` fields dropped from the query the programs read, so the base's
  // runs and main's answered the head's failing run, and closed pulls the open one, where gh sends
  // the fields as the query (measured: `GET .../actions/runs?event=pull_request&head_sha=...`).
  test("a GET's fields are its query, as gh sends them", async () => {
    const run = redFixture("a");
    const runs = (fields: string[], jq: string) =>
      lines(run, ["api", "-X", "GET", `repos/${RED_REPO}/actions/runs`, ...fields, "--jq", jq]);
    const byCommit = ".workflow_runs[] | [.id, .conclusion] | @tsv";
    expect({
      base: await runs(["-f", `head_sha=${RED_BASE}`, "-f", "event=pull_request"], byCommit),
      head: await runs(["-f", `head_sha=${RED_HEAD}`, "-f", "event=pull_request"], byCommit),
      main: await runs(["-f", "branch=main"], ".total_count"),
      closed: await lines(run, [
        ...["api", "-X", "GET", `repos/${RED_REPO}/pulls`, "-f", "state=closed"],
        ...["--jq", "length"],
      ]),
      open: await lines(run, [
        ...["api", "-X", "GET", `repos/${RED_REPO}/pulls`, "-f", "state=open"],
        ...["--jq", ".[].number"],
      ]),
    }).toEqual({
      base: [],
      head: [`${RED_RUN_ID}\tfailure`],
      main: ["0"],
      closed: ["0"],
      open: [String(RED_PR)],
    });
  });

  // Catches (C3): an attached short flag (`-bmain`, `-c<sha>`, `-sclosed`), which the stand-in
  // reads as gh does, filtering nothing in the program that answers it, so the base's runs and
  // main's answered the head's failing run and closed pulls the open one; and `-v`, gh run view's
  // short `--verbose`, together with the plain view itself, which a program that reads the flags
  // must answer (replay 1 of the fold of 76cc2379 met a plain view refused).
  test("an attached short flag filters as its long form does", async () => {
    const run = redFixture("a");
    const out = async (args: string[]) => (await gh(run, args)).stdout;
    const view = ["run", "view", String(RED_RUN_ID)];
    const plain = await gh(run, view);
    const verbose = await out([...view, "--verbose"]);
    expect({
      main: await out(["run", "list", "-bmain", "--json", "conclusion,headBranch"]),
      branch: await out(["run", "list", `-blegion/${RED_KEY}`, "--json", "conclusion,headBranch"]),
      base: await out(["run", "list", `-c${RED_BASE}`, "--json", "databaseId"]),
      head: await out(["run", "list", `-c${RED_HEAD}`, "--json", "databaseId"]),
      closed: await out(["pr", "list", "-sclosed", "--json", "number"]),
      open: await out(["pr", "list", "-sopen", "--json", "number"]),
      plain: [plain.exit, plain.stdout.includes("✓ Set up job"), plain.stdout === verbose],
      short: await out([...view, "-v"]),
    }).toEqual({
      main: "[]\n",
      branch: `[{"conclusion":"failure","headBranch":"legion/${RED_KEY}"}]\n`,
      base: "[]\n",
      head: `[{"databaseId":${RED_RUN_ID}}]\n`,
      closed: "[]\n",
      open: `[{"number":${RED_PR}}]\n`,
      // The plain view lists the steps of the failed job alone; --verbose those of every job.
      plain: [0, true, false],
      short: verbose,
    });
  });

  // Catches (C4): a variable of type `Int!` given no value read as null, and a String given for an
  // Int, in a variable or written in the query, answered as no such pull request, where GitHub
  // refuses each request before it reads anything; a declared variable the query does not use
  // answered. Each is the request GitHub answered, byte for byte (github-answers/graphql-*).
  test("a GraphQL variable or argument GitHub cannot take answers GitHub's measured refusal", async () => {
    const run = redFixture("a");
    const answer = async (args: string[]) => {
      const read = await gh(run, ["api", "graphql", ...args]);
      return [read.stdout, read.stderr, read.exit];
    };
    const pull = (number: string) =>
      `{repository(owner:"cybermonklabs",name:"legion"){pullRequest(number:${number}){number}}}`;
    expect({
      none: await answer(["-f", `query=query($n:Int!)${pull("$n")}`]),
      string: await answer(["-f", "n=1003", "-f", `query=query($n:Int!)${pull("$n")}`]),
      literal: await answer(["-f", `query=query${pull('"1003"')}`]),
      unused: await answer(["-F", "n=1003", "-f", `query=query($n:Int!,$u:String)${pull("$n")}`]),
      // A variable the query does not declare is ignored, as GitHub ignores it (measured).
      extra: await answer([
        ...["-F", `n=${RED_PR}`, "-f", "extra=1"],
        "-f",
        `query=query($n:Int!){repository(owner:"example",name:"widgets"){pullRequest(number:$n){number}}}`,
      ]),
    }).toEqual({
      none: githubAnswer("graphql-variable-null"),
      string: githubAnswer("graphql-variable-coerce"),
      literal: githubAnswer("graphql-literal-type"),
      unused: githubAnswer("graphql-variable-unused"),
      extra: [`{"data":{"repository":{"pullRequest":{"number":${RED_PR}}}}}`, "", 0],
    });
  });

  // Catches (the page's counted-set query): red-at-the-head.md's own query, with `$endCursor` and
  // `endCursor`, refused as unmodeled because the rollup held no cursor. GitHub's page holds the
  // cursor of its last node, base64 of its index, and the page after it no node and no cursor
  // (measured on cybermonklabs/legion 05e64382: "NjQ" for the 64th of 64, null after it).
  test("the page's counted-set query answers GitHub's cursor, and the page after it none", async () => {
    const run = redFixture("a");
    const page = (args: string[]) =>
      lines(run, [
        ...["api", "graphql", ...args, "-F", `oid=${RED_HEAD}`, "-f", `query=${RED_ROLLUP_QUERY}`],
        "--jq",
        ".data.repository.object.statusCheckRollup.contexts | [.totalCount, .pageInfo.hasNextPage, .pageInfo.endCursor, (.nodes | length)] | @json",
      ]);
    const other = [
      ...["api", "graphql", "-f", "endCursor=MQ", "-F", `oid=${RED_HEAD}`],
      ...["-f", `query=${RED_ROLLUP_QUERY}`, "--jq", ".data"],
    ];
    expect({
      first: await page(["--paginate"]),
      after: await page(["-f", "endCursor=Mw"]),
      other: await lines(run, other),
    }).toEqual({
      first: ['[3,false,"Mw",3]'],
      after: ["[3,false,null,0]"],
      other: unmodeled(other),
    });
  });

  // Catches (the minors): a page past the first answering page 1 again; `jobs?filter=all` after a
  // press listing attempt 2 alone; `actor`, `created` and `exclude_pull_requests` ignored; a
  // revision expression (`<branch>~1`) answered null where GitHub answers the base; a mutation
  // answered as a query; and the repository's owner and name matched with regard to case, where
  // GitHub matches them without (measured).
  test("a page, a filter, a parameter, an expression, a mutation and a name in another case", async () => {
    const run = ownFixture("a");
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const actor = ["api", `repos/${RED_REPO}/actions/runs?actor=rig`, "--jq", ".total_count"];
    const created = ["api", `repos/${RED_REPO}/actions/runs?created=>2026-01-01`];
    const expression = [
      ...["api", "graphql", "-f"],
      `query={repository(owner:"example",name:"widgets"){object(expression:"legion/${RED_KEY}~1"){oid}}}`,
    ];
    const mutation = [
      ...["api", "graphql", "-f"],
      'query=mutation{repository(owner:"example",name:"widgets"){name}}',
    ];
    expect({
      pulls: await lines(run, ["api", `repos/${RED_REPO}/pulls?page=2`, "--jq", "length"]),
      runs: await lines(run, [
        ...["api", `repos/${RED_REPO}/actions/runs?page=2`],
        ...["--jq", "[.total_count, (.workflow_runs | length)] | @json"],
      ]),
      jobs: await lines(run, [
        ...["api", `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}/jobs?filter=all`],
        ...["--jq", '.total_count, (.jobs[] | "\\(.id) \\(.run_attempt) \\(.conclusion)")'],
      ]),
      actor: await lines(run, actor),
      created: await lines(run, created),
      excluded: await lines(run, [
        ...["api", `repos/${RED_REPO}/actions/runs?exclude_pull_requests=true`],
        ...["--jq", "[.workflow_runs[].pull_requests | length] | @json"],
      ]),
      expression: await lines(run, expression),
      mutation: await lines(run, mutation),
      graphqlCase: await lines(run, [
        ...["api", "graphql", "-f"],
        `query={repository(owner:"Example",name:"Widgets"){pullRequest(number:${RED_PR}){number}}}`,
      ]),
      restCase: await lines(run, [
        "api",
        `repos/Example/Widgets/pulls/${RED_PR}`,
        "--jq",
        ".number",
      ]),
      repoCase: await lines(run, [
        "pr",
        "view",
        String(RED_PR),
        "--repo",
        "EXAMPLE/widgets",
        "--json",
        "number",
      ]),
    }).toEqual({
      pulls: ["0"],
      runs: ["[1,0]"],
      jobs: [
        "6",
        `${RED_JOB_ID} 1 failure`,
        `${RED_JOB_ID + 1} 1 success`,
        `${RED_JOB_ID + 2} 1 success`,
        `${RED_JOB_ID + 4} 2 success`,
        `${RED_CARRIED_JOB.lint} 2 success`,
        `${RED_CARRIED_JOB.typecheck} 2 success`,
      ],
      actor: unmodeled(actor),
      created: unmodeled(created),
      excluded: ["[0]"],
      expression: unmodeled(expression),
      mutation: unmodeled(mutation),
      graphqlCase: [`{"data":{"repository":{"pullRequest":{"number":${RED_PR}}}}}`],
      restCase: [String(RED_PR)],
      repoCase: [`{"number":${RED_PR}}`],
    });
  });

  // Catches (the REST objects): a `--jq` read of a field GitHub's object holds answered null: a
  // check run's `head_sha`, `check_suite` and `pull_requests`, a run's `workflow_id` and
  // `previous_attempt_url`, the pull's `head.repo`. Each is the world's own: the head, the run's
  // check suite and pull request, the run's workflow, the attempt before it, the repository.
  test("a check run, a run and the pull carry the fields GitHub's objects hold", async () => {
    const run = ownFixture("a");
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const runs = `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}`;
    expect({
      checkRuns: await lines(run, [
        ...["api", `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs`, "--jq"],
        ".check_runs[] | [.id, .head_sha, .check_suite.id, (.pull_requests | map(.number))] | @json",
      ]),
      run: await lines(run, [
        ...["api", runs, "--jq"],
        "[.run_attempt, .workflow_id, .previous_attempt_url] | @json",
      ]),
      attempt1: await lines(run, [
        ...["api", `${runs}/attempts/1`, "--jq"],
        "[.run_attempt, .previous_attempt_url] | @json",
      ]),
      pull: await lines(run, [
        ...["api", `repos/${RED_REPO}/pulls/${RED_PR}`, "--jq"],
        "[.head.repo.full_name, .base.repo.full_name] | @json",
      ]),
    }).toEqual({
      checkRuns: [RED_RED_ID + 1, RED_CARRIED.lint, RED_CARRIED.typecheck].map(
        (id) => `[${id},"${RED_HEAD}",${RED_RUN_ID - 1_000_000},[${RED_PR}]]`
      ),
      run: [`[2,1,"https://api.github.com/${runs}/attempts/1"]`],
      attempt1: ["[1,null]"],
      pull: [`["${RED_REPO}","${RED_REPO}"]`],
    });
  });
});

// The review of 6b97edd6, under the architect's ruling of 2026-11-19 02:55Z: the reads of the
// head that still answered what they do not model (K1, P1 to P5), the jobs of an attempt a re-run
// made (b), and the objects GitHub answers as the reviewer measured them on cybermonklabs/legion
// (a, infra-1303-github-fields-measured). Each row is the reviewer's probe.
describe("the reads of the head answer as GitHub does, objects and pages included", () => {
  /** A call's output lines where it exits 0, else its stderr and exit. */
  const lines = async (run: string, args: string[]) => {
    const read = await gh(run, args);
    return read.exit === 0 ? outputLines(read.stdout) : [read.stderr, read.exit];
  };
  const unmodeled = (args: string[]) => [`rig: request not modeled: gh ${args.join(" ")}\n`, 1];
  /** The check suite of a run of the head (red_model: the run id less a million). */
  const suite = (run: number) => run - 1_000_000;

  // Catches (K1): the default check-runs read keeping one check run of a name across the commit,
  // where GitHub keeps the latest of each name in each check suite (measured by the reviewer on
  // b789479b99: the default read answers five pr-title runs of five suites, as filter=all does):
  // world k's by-hand run, a suite of its own, hid the counted failure before the press, and the
  // re-run's success hid the by-hand failure after it.
  test("the default check-runs read keeps the latest of each name in each check suite", async () => {
    const run = ownFixture("k");
    const read = () =>
      lines(run, [
        ...["api", `repos/${RED_REPO}/commits/${shasOf(run).head}/check-runs`, "--jq"],
        '.check_runs[] | "\\(.id) \\(.name) \\(.conclusion) \\(.check_suite.id)"',
      ]);
    const before = await read();
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const counted = suite(RED_RUN_ID);
    const byHand = `${RED_RED_ID + 2} test (notify) failure ${suite(RED_RUN_ID + 1)}`;
    expect({ before, after: await read() }).toEqual({
      before: [
        `${RED_ID.lint} lint success ${counted}`,
        `${RED_ID.typecheck} typecheck success ${counted}`,
        `${RED_RED_ID} test (notify) failure ${counted}`,
        byHand,
      ],
      after: [
        `${RED_RED_ID + 1} test (notify) success ${counted}`,
        `${RED_CARRIED.lint} lint success ${counted}`,
        `${RED_CARRIED.typecheck} typecheck success ${counted}`,
        byHand,
      ],
    });
  });

  // Catches (P1): an attempt's jobs answered as none for `filter=all`, the spend read's endpoint
  // (red-at-the-head.md), where GitHub's attempt-jobs endpoint takes per_page and page alone and
  // answers the attempt's jobs whatever `filter` says; and its page ignored.
  test("an attempt's jobs are every job of that attempt, whatever filter says", async () => {
    const run = redFixture("a");
    const jobs = (query: string) =>
      lines(run, [
        ...["api", `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}/attempts/1/jobs${query}`],
        ...["--jq", "[.total_count, [.jobs[].id]] | @json"],
      ]);
    const attempt1 = `[3,[${RED_JOB_ID},${RED_JOB_ID + 1},${RED_JOB_ID + 2}]]`;
    expect({
      all: await jobs("?filter=all"),
      latest: await jobs("?filter=latest"),
      none: await jobs(""),
      page: await jobs("?per_page=1&page=2"),
    }).toEqual({
      all: [attempt1],
      latest: [attempt1],
      none: [attempt1],
      page: [`[3,[${RED_JOB_ID + 1}]]`],
    });
  });

  // Catches (P2): the head's check suites answered whole whatever `check_name`, `app_id`,
  // `per_page` and `page` asked, the four parameters GitHub documents and filters on; and another
  // parameter answered as if absent.
  test("the head's check suites read check_name, app_id and the page", async () => {
    const run = redFixture("k");
    const suites = (query: string) =>
      lines(run, [
        ...["api", `repos/${RED_REPO}/commits/${shasOf(run).head}/check-suites${query}`],
        ...["--jq", "[.total_count, [.check_suites[].id]] | @json"],
      ]);
    const bogus = [
      ...["api", `repos/${RED_REPO}/commits/${shasOf(run).head}/check-suites?bogus=1`],
      ...["--jq", ".total_count"],
    ];
    expect({
      lint: await suites("?check_name=lint"),
      otherApp: await suites("?app_id=1"),
      actions: await suites("?app_id=15368"),
      page: await suites("?per_page=1&page=2"),
      bogus: await lines(run, bogus),
    }).toEqual({
      lint: [`[1,[${suite(RED_RUN_ID)}]]`],
      otherApp: ["[0,[]]"],
      actions: [`[2,[${suite(RED_RUN_ID)},${suite(RED_RUN_ID + 1)}]]`],
      page: [`[2,[${suite(RED_RUN_ID + 1)}]]`],
      bogus: unmodeled(bogus),
    });
  });

  // Catches (P3): a run and an attempt of it listing their pull request under
  // `exclude_pull_requests=true`, a parameter both endpoints document and gh run view sends.
  test("a run and an attempt of it read exclude_pull_requests", async () => {
    const run = redFixture("a");
    const count = (path: string) =>
      lines(run, [
        "api",
        `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}${path}`,
        "--jq",
        ".pull_requests | length",
      ]);
    expect({
      run: await count("?exclude_pull_requests=true"),
      attempt: await count("/attempts/1?exclude_pull_requests=true"),
      plain: await count(""),
    }).toEqual({ run: ["0"], attempt: ["0"], plain: ["1"] });
  });

  // Catches (P4): a check run's annotations answered whole on every page, where GitHub answers
  // an empty page past the end.
  test("a check run's annotations take their page", async () => {
    const run = redFixture("a");
    const count = (query: string) =>
      lines(run, [
        "api",
        `repos/${RED_REPO}/check-runs/${RED_RED_ID}/annotations${query}`,
        "--jq",
        "length",
      ]);
    expect([await count(""), await count("?per_page=1&page=5")]).toEqual([["1"], ["0"]]);
  });

  // Catches (P5): `--paginate` stopping at the first page of a REST read, where gh follows
  // GitHub's `Link: rel="next"` and prints every page (objects one after another, nothing between:
  // measured on cybermonklabs/legion, 2026-11-19); and the request gh sends for a paged read that
  // names no per_page, which asks for 100 (measured, GH_DEBUG=api).
  test("--paginate prints every page a read's paging gives, as gh joins them", async () => {
    const a = ownFixture("a");
    const f = redFixture("f");
    const checkRuns = `repos/${RED_REPO}/commits/${RED_HEAD}/check-runs`;
    const page = async (n: number) =>
      (await gh(a, ["api", `${checkRuns}?per_page=2&page=${n}`])).stdout;
    const pages = (await page(1)) + (await page(2));
    const joined = await gh(a, ["api", "--paginate", `${checkRuns}?per_page=2`]);
    await gh(a, ["api", "--paginate", checkRuns, "--jq", ".total_count"]);
    const requests = readFileSync(path.join(a, "calls.jsonl"), "utf8")
      .trimEnd()
      .split("\n")
      .map((line) => (JSON.parse(line) as { request?: string }).request);
    expect({
      checkRuns: await lines(a, [
        "api",
        "--paginate",
        `${checkRuns}?per_page=1`,
        "--jq",
        ".check_runs[].id",
      ]),
      runs: await lines(f, [
        ...[
          "api",
          "--paginate",
          `repos/${RED_REPO}/actions/runs?head_sha=${shasOf(f).head}&per_page=1`,
        ],
        ...["--jq", ".workflow_runs[].id"],
      ]),
      joined: [joined.stdout, joined.exit],
      request: requests.at(-1),
    }).toEqual({
      checkRuns: [String(RED_ID.lint), String(RED_ID.typecheck), String(RED_RED_ID)],
      runs: [String(RED_RUN_ID), String(RED_RUN_ID + 2)],
      joined: [pages, 0],
      request: `gh api GET ${checkRuns}?per_page=100 --paginate --jq .total_count`,
    });
  });

  // Catches (b): attempt 2 of a re-run of the failed jobs listing the re-run jobs alone, where
  // GitHub lists every job of the run at attempt 2 (measured on cybermonklabs/legion run
  // 44406867953: 58 jobs at attempt 2, 2 re-run, 56 carried, each carried job with an id and a
  // check run of its own, its attempt-1 conclusion, steps and times, and a new creation time).
  test("attempt 2 lists every job of the run, the ones it did not re-run carried", async () => {
    const run = ownFixture("a");
    await gh(run, ["run", "rerun", String(RED_RUN_ID), "--failed"]);
    await settleRerun(run);
    const attempt = (n: number) =>
      lines(run, [
        ...["api", `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}/attempts/${n}/jobs`, "--jq"],
        '.jobs[] | [.id, .name, .run_attempt, .conclusion, .created_at, .started_at, (.check_run_url | split("/") | last)] | @tsv',
      ]);
    const t0 = "2026-03-04T10:00:00Z";
    const t1 = "2026-03-04T10:31:08Z";
    expect({ one: await attempt(1), two: await attempt(2) }).toEqual({
      one: [
        `${RED_JOB_ID}\ttest (notify)\t1\tfailure\t${t0}\t${t0}\t${RED_RED_ID}`,
        `${RED_JOB_ID + 1}\tlint\t1\tsuccess\t${t0}\t${t0}\t${RED_ID.lint}`,
        `${RED_JOB_ID + 2}\ttypecheck\t1\tsuccess\t${t0}\t${t0}\t${RED_ID.typecheck}`,
      ],
      two: [
        `${RED_JOB_ID + 4}\ttest (notify)\t2\tsuccess\t${t1}\t${t1}\t${RED_RED_ID + 1}`,
        `${RED_CARRIED_JOB.lint}\tlint\t2\tsuccess\t${t1}\t${t0}\t${RED_CARRIED.lint}`,
        `${RED_CARRIED_JOB.typecheck}\ttypecheck\t2\tsuccess\t${t1}\t${t0}\t${RED_CARRIED.typecheck}`,
      ],
    });
  });

  // Catches (a): a check run, a job or a run missing a key GitHub answers, holding one GitHub does
  // not (an `actor` on a check run or a job), or holding the world's words where GitHub Actions
  // holds none (the output of a check run): measured by the reviewer on cybermonklabs/legion,
  // 2026-11-19 (check-runs/135286572101, actions/jobs/134911513960, actions/runs/44335620601),
  // each key in the order GitHub writes it.
  test("a check run, a job and a run carry the keys GitHub answers, in its order", async () => {
    const run = redFixture("a");
    const json = async (endpoint: string) =>
      JSON.parse((await gh(run, ["api", `repos/${RED_REPO}/${endpoint}`])).stdout);
    const checkRun = await json(`check-runs/${RED_RED_ID}`);
    const job = await json(`actions/jobs/${RED_JOB_ID}`);
    const workflowRun = await json(`actions/runs/${RED_RUN_ID}`);
    const api = `https://api.github.com/repos/${RED_REPO}`;
    const repo = { id: 1, url: api, name: "widgets" };
    expect({
      checkRun: Object.keys(checkRun),
      job: Object.keys(job),
      step: Object.keys(job.steps[0]),
      run: Object.keys(workflowRun),
    }).toEqual({
      checkRun: [
        "id",
        "name",
        "node_id",
        "head_sha",
        "external_id",
        "url",
        "html_url",
        "details_url",
        "status",
        "conclusion",
        "started_at",
        "completed_at",
        "output",
        "check_suite",
        "app",
        "pull_requests",
      ],
      job: [
        "id",
        "run_id",
        "workflow_name",
        "head_branch",
        "run_url",
        "run_attempt",
        "node_id",
        "head_sha",
        "url",
        "html_url",
        "status",
        "conclusion",
        "created_at",
        "started_at",
        "completed_at",
        "name",
        "steps",
        "check_run_url",
        "labels",
        "runner_id",
        "runner_name",
        "runner_group_id",
        "runner_group_name",
      ],
      step: ["name", "status", "conclusion", "number", "started_at", "completed_at"],
      run: [
        "id",
        "name",
        "node_id",
        "head_branch",
        "head_sha",
        "path",
        "display_title",
        "run_number",
        "event",
        "status",
        "conclusion",
        "workflow_id",
        "check_suite_id",
        "check_suite_node_id",
        "url",
        "html_url",
        "pull_requests",
        "created_at",
        "updated_at",
        "actor",
        "run_attempt",
        "referenced_workflows",
        "run_started_at",
        "triggering_actor",
        "jobs_url",
        "logs_url",
        "check_suite_url",
        "artifacts_url",
        "cancel_url",
        "rerun_url",
        "previous_attempt_url",
        "workflow_url",
        "head_commit",
        "repository",
        "head_repository",
      ],
    });
    expect({
      output: checkRun.output,
      pages: [checkRun.url, checkRun.html_url, checkRun.details_url],
      nodes: [
        checkRun.node_id === job.node_id,
        checkRun.node_id.slice(0, 4),
        workflowRun.node_id.slice(0, 4),
      ],
      app: checkRun.app.slug,
      suite: [checkRun.check_suite.id, workflowRun.check_suite_id],
      pullRequests: checkRun.pull_requests,
      actors: [
        "actor" in checkRun,
        "actor" in job,
        workflowRun.actor.login,
        workflowRun.actor.type,
        workflowRun.triggering_actor.login,
      ],
      head: [
        workflowRun.head_commit.id,
        workflowRun.repository.full_name,
        workflowRun.head_repository.full_name,
      ],
    }).toEqual({
      output: {
        title: null,
        summary: null,
        text: null,
        annotations_count: 1,
        annotations_url: `${api}/check-runs/${RED_RED_ID}/annotations`,
      },
      pages: [
        `${api}/check-runs/${RED_RED_ID}`,
        `${RED_RUN_URL}/job/${RED_JOB_ID}`,
        `${RED_RUN_URL}/job/${RED_JOB_ID}`,
      ],
      nodes: [true, "CR_k", "WFR_"],
      app: "github-actions",
      suite: [suite(RED_RUN_ID), suite(RED_RUN_ID)],
      pullRequests: [
        {
          url: `${api}/pulls/${RED_PR}`,
          id: RED_PR,
          number: RED_PR,
          head: { ref: `legion/${RED_KEY}`, sha: RED_HEAD, repo },
          base: { ref: "main", sha: RED_BASE, repo },
        },
      ],
      actors: [false, false, "rig[bot]", "Bot", "rig[bot]"],
      head: [RED_HEAD, RED_REPO, RED_REPO],
    });
  });

  // Catches: a node id in another shape than GitHub's, which routes.jq's node_id reads from the
  // ids the reviewer measured: each of them comes back byte for byte.
  test("node_id gives back the node ids GitHub gave", () => {
    const nodeId = (args: string) =>
      bash(`red_jq -nr --arg repo ${RED_REPO} 'include "routes"; node_id(${args})'`).stdout.trim();
    expect([
      nodeId('"CR"; 0; [1146927990, 134911513960]'),
      nodeId('"WFR"; 2; [1146927990, 44335620601]'),
      nodeId('"CS"; 0; [1146927990, 120014807360]'),
      nodeId('"R"; 0; [1146927990]'),
    ]).toEqual([
      "CR_kwDORFy7ds8AAAAfaVpVaA",
      "WFR_kwLORFy7ds8AAAAKUpvh-Q",
      "CS_kwDORFy7ds8AAAAb8XChQA",
      "R_kgDORFy7dg",
    ]);
  });
});

/** GitHub's answer `name` as gh 2.98.0 printed it, read from the files the stand-in reads it
 * from (github-answers/README.txt names each request): [stdout, stderr, exit], with the probe's
 * commit put back as `commit` where the answer names one. */
function githubAnswer(name: string, commit?: string) {
  const read = (part: string) => {
    const text = readFileSync(
      path.join(import.meta.dir, "github-answers", `${name}.${part}`),
      "utf8"
    );
    return commit === undefined ? text : text.replaceAll("1".repeat(40), commit);
  };
  return [read("stdout"), read("stderr"), 1];
}

// The answers GitHub gives, as measured with gh 2.98.0 against api.github.com on 2026-11-18
// (github-answers/README.txt), for each request the rig answers with an error or for a commit the
// repository does not hold, and the check suites of the head.
describe("GitHub's answers, as measured", () => {
  const answer = async (run: string, args: string[]) => {
    const read = await gh(run, args);
    return [read.stdout, read.stderr, read.exit];
  };

  // Catches: a `gh api` body printed with a newline gh does not print after it, on success or on
  // error, where a `--jq` output keeps the newline jq ends it with (measured).
  test("gh api prints a body as GitHub sent it, with no newline after it, and --jq output as jq ends it", async () => {
    const run = redFixture("a");
    const repository = await answer(run, ["api", `repos/${RED_REPO}`]);
    expect(String(repository[0]).endsWith("}")).toBe(true);
    expect(await answer(run, ["api", `repos/${RED_REPO}`, "--jq", ".full_name"])).toEqual([
      `${RED_REPO}\n`,
      "",
      0,
    ]);
    expect(
      await answer(run, ["api", `repos/${RED_REPO}/actions/runs/1`, "--jq", ".message"])
    ).toEqual(githubAnswer("run-404"));
  });

  // Catches: an error the rig answers in other bytes than GitHub's, or with no body where gh prints
  // the one GitHub sent (the round-5 rig printed `gh: Not Found (HTTP 404)` alone for every one),
  // and a commit the call named left out of the answer that names it. Measured alone on
  // 2026-11-19, 8.4 s and 17.2 s with a Legion pod's own git first on PATH, 14.9 s with plain git
  // first, and 15.70 s on fold12 on the pod's own PATH with nothing else running on the pod; the
  // bound is about four times the slowest.
  test("every REST error the rig answers is GitHub's measured answer", async () => {
    const run = ownFixture("a");
    const repo = `repos/${RED_REPO}`;
    const other = "2".repeat(40);
    const cases: [string[], string, string?][] = [
      [["api", `${repo}/branches/no-such-branch`], "branch-404"],
      [["api", `${repo}/commits/${other}`], "commit-422", other],
      [["api", `${repo}/commits/trunk`], "commit-422", "trunk"],
      [["api", `${repo}/commits/${other}/check-runs`], "checkruns-422", other],
      [["api", `${repo}/commits/${other}/check-suites`], "suites-422", other],
      [["api", `${repo}/commits/trunk/check-suites`], "suites-ref-404"],
      [["api", `${repo}/commits/trunk/status`], "status-ref-404"],
      [["api", `${repo}/commits/trunk/statuses`], "statuses-ref-404"],
      [["api", `${repo}/runs/${RED_RED_ID}`], "noroute-404"],
      [["api", `${repo}/check-runs`], "checkruns-list-404"],
      [["api", `${repo}/git/ref/heads/no-such-branch`], "gitref-404"],
      [["api", `${repo}/git/refs/heads/no-such-branch`], "gitrefs-404"],
      [["api", `${repo}/pulls/999999`], "pull-404"],
      [["api", `${repo}/pulls/999999/reviews`], "reviews-404"],
      [["api", `${repo}/issues/999999/comments`], "comments-404"],
      [["api", `${repo}/pulls/${RED_PR}/reviews/1`], "review-404"],
      [["api", `${repo}/pulls/${RED_PR}/reviews/1/comments`], "review-comments-404"],
      [["api", `${repo}/actions/runs/1`], "run-404"],
      [["api", `${repo}/actions/runs/1/jobs`], "run-jobs-404"],
      [["api", `${repo}/actions/runs/1/attempts/1`], "run-attempt-404"],
      [["api", `${repo}/actions/runs/1/attempts/1/jobs`], "run-attempt-jobs-404"],
      [["api", `${repo}/actions/runs/1/logs`], "run-logs-404"],
      [["api", "-X", "POST", `${repo}/actions/runs/1/rerun-failed-jobs`], "rerun-404"],
      [["api", "-X", "POST", `${repo}/actions/runs/1/rerun`], "run-rerun-404"],
      [["api", `${repo}/actions/runs/${RED_RUN_ID}/attempts/9`], "attempt-404"],
      [["api", `${repo}/actions/runs/${RED_RUN_ID}/attempts/9/jobs`], "attempt-jobs-404"],
      [["api", `${repo}/actions/jobs/1`], "job-404"],
      [["api", `${repo}/actions/jobs/1/logs`], "joblog-404"],
      [["api", "-X", "POST", `${repo}/actions/jobs/1/rerun`], "jobrerun-404"],
      [["api", `${repo}/check-runs/1`], "checkrun-404"],
      [["api", `${repo}/check-runs/1/annotations`], "annotations-404"],
      [["api", "repos/skills-rig/project/pulls/1"], "repo-404-pulls"],
      [["api", "repos/skills-rig/project/commits/abc/check-runs"], "repo-404-checkruns"],
      [["api", "repos/skills-rig/project/actions/runs/1"], "repo-404-runs"],
      [["api", "repos/skills-rig/project/branches/main"], "repo-404-branch"],
    ];
    const seen: unknown[] = [];
    for (const [args] of cases)
      seen.push({ call: args.join(" "), answer: await answer(run, args) });
    expect(seen).toEqual(
      cases.map(([args, name, commit]) => ({
        call: args.join(" "),
        answer: githubAnswer(name, commit),
      }))
    );
    const h = redFixture("h");
    expect(await answer(h, ["api", `${repo}/actions/jobs/${RED_JOB_ID}/logs`])).toEqual(
      githubAnswer("joblog-410")
    );
    expect(ghCalls(run).some((call) => "unmodeled" in call)).toBe(false);
  }, 70_000);

  // Catches: a request of another repository than the world's (runs of rounds 6 and 9 read and
  // wrote skills-rig/project) refused as unmodeled where GitHub answers its 404, answered with
  // another endpoint's 404 (the protection of a branch with the branch's), or one the rig has not
  // measured answered rather than refused. It runs under the file's minute: measured alone on
  // 2026-11-19, 9.2 s with a Legion pod's own git first on PATH and 6.9 s with plain git first.
  test("each request of another repository the archived runs make is GitHub's measured 404", async () => {
    const run = ownFixture("a");
    const repo = "repos/skills-rig/project";
    const cases: [string[], string][] = [
      [["api", repo], "repo-404-repo"],
      [["api", `${repo}/pulls`], "repo-404-pulls-list"],
      [["api", `${repo}/pulls/1/reviews`], "repo-404-reviews"],
      [["api", `${repo}/actions/runs?head_sha=${RED_HEAD}&per_page=100`], "repo-404-runs-list"],
      [["api", `${repo}/actions/runs/1/jobs`], "repo-404-run-jobs"],
      [["api", `${repo}/actions/jobs/1`], "repo-404-job"],
      [["api", "--paginate", `${repo}/rules/branches/main`], "repo-404-rules"],
      [["api", `${repo}/branches/main/protection`], "repo-404-protection"],
      [
        ["api", `${repo}/branches/main/protection/required_status_checks`],
        "repo-404-required-checks",
      ],
      [["api", "-X", "PATCH", `${repo}/pulls/1`, "-f", "title=x"], "repo-404-pull-patch"],
      [
        ["api", "-X", "POST", `${repo}/pulls/1/reviews`, "-f", "event=COMMENT", "-f", "body=x"],
        "repo-404-review-post",
      ],
    ];
    const seen: unknown[] = [];
    for (const [args] of cases)
      seen.push({ call: args.join(" "), answer: await answer(run, args) });
    expect(seen).toEqual(
      cases.map(([args, name]) => ({ call: args.join(" "), answer: githubAnswer(name) }))
    );
    const unmeasured = await gh(run, ["api", `${repo}/issues`]);
    expect([
      unmeasured.stderr.startsWith("rig: request not modeled: gh "),
      unmeasured.exit,
    ]).toEqual([true, 1]);
  });

  // Catches: the combined status of a full commit sha the repository does not hold refused as an
  // unknown commit, where GitHub answers it pending with no status (measured, status-422 of the
  // lead's measurements), and its statuses refused where GitHub answers none.
  test("a full sha the repository does not hold reads pending with no status, and no statuses", async () => {
    const run = redFixture("a");
    const other = "2".repeat(40);
    const status = await gh(run, ["api", `repos/${RED_REPO}/commits/${other}/status`]);
    const statuses = await answer(run, ["api", `repos/${RED_REPO}/commits/${other}/statuses`]);
    const url = `https://api.github.com/repos/${RED_REPO}/commits/${other}`;
    expect(status.exit).toBe(0);
    const parsed = JSON.parse(status.stdout);
    expect(Object.keys(parsed)).toEqual([
      "state",
      "statuses",
      "sha",
      "total_count",
      "repository",
      "commit_url",
      "url",
    ]);
    expect(parsed).toMatchObject({
      state: "pending",
      statuses: [],
      sha: other,
      total_count: 0,
      repository: { full_name: RED_REPO },
      commit_url: url,
      url: `${url}/status`,
    });
    expect(statuses).toEqual(["[]", "", 0]);
  });

  // Catches (round 6 trial-l-head-1): the head's check suites refused, where GitHub answers one
  // GitHub Actions suite per workflow run of the head; a suite whose id is not its run's
  // check_suite_id; and a run or a GraphQL check suite that names no suite.
  test("the head's check suites are one GitHub Actions suite per run, each its run's check_suite_id", async () => {
    const seen: unknown[] = [];
    for (const world of ["a", "k"] as const) {
      const run = redFixture(world);
      const runs = JSON.parse(
        (await gh(run, ["api", `repos/${RED_REPO}/actions/runs?head_sha=${shasOf(run).head}`]))
          .stdout
      ).workflow_runs as {
        id: number;
        check_suite_id: number;
        check_suite_node_id: string;
        conclusion: string;
      }[];
      const suites = JSON.parse(
        (await gh(run, ["api", `repos/${RED_REPO}/commits/${shasOf(run).head}/check-suites`]))
          .stdout
      );
      seen.push({
        world,
        total: suites.total_count,
        suites: suites.check_suites.map(
          (suite: { id: number; node_id: string; app: { slug: string }; conclusion: string }) => [
            suite.id,
            suite.node_id,
            suite.app.slug,
            suite.conclusion,
          ]
        ),
      });
      expect(seen.at(-1)).toEqual({
        world,
        total: runs.length,
        suites: runs.map((r) => [
          r.check_suite_id,
          r.check_suite_node_id,
          "github-actions",
          r.conclusion,
        ]),
      });
      expect(
        runs.every((r) => r.check_suite_id > 0 && r.check_suite_node_id.startsWith("CS_"))
      ).toBe(true);
    }
  });

  // Catches: a GraphQL check suite that names no suite, or another than its run's.
  test("a GraphQL check suite's databaseId is its run's check_suite_id", async () => {
    const run = redFixture("a");
    const runs = JSON.parse(
      (await gh(run, ["api", `repos/${RED_REPO}/actions/runs/${RED_RUN_ID}`])).stdout
    );
    const read = await gh(run, [
      "api",
      "graphql",
      "-F",
      `oid=${RED_HEAD}`,
      "-f",
      'query=query($oid:GitObjectID!){repository(owner:"example",name:"widgets"){object(oid:$oid){... on Commit{statusCheckRollup{contexts(first:100){nodes{... on CheckRun{name checkSuite{databaseId}}}}}}}}}',
      "--jq",
      '.data.repository.object.statusCheckRollup.contexts.nodes | map(.checkSuite.databaseId) | unique | map(tostring) | join(",")',
    ]);
    expect(read.stdout).toBe(`${runs.check_suite_id}\n`);
  });
});
