// heads-scenarios.txt, the catalog of the heads scenarios' worlds, and the readers that take each
// world from it: rig.sh's batch and run, heads-world.ts's validation, seed.ts's usage and
// score.ts's run names.
//
//   bun test heads-scenarios.test.ts

import { expect, setDefaultTimeout, test } from "bun:test";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { copyOfThisDirectory, rigFunctions } from "./catalog-copy";
import { HEADS_SCENARIOS, headsCases, isHeadsScenario } from "./heads-scenarios";
import { fileRowLimit, RIG_STEP, SCRIPT_RUN } from "./test-limit";

// The most a row runs as processes of their own: six rig.sh steps, one for each heads scenario.
setDefaultTimeout(fileRowLimit(RIG_STEP, RIG_STEP, RIG_STEP, RIG_STEP, RIG_STEP, RIG_STEP));

const makeDirectory = temporaryDirectories("heads-catalog");

test("the heads scenarios have one canonical membership", () => {
  expect(HEADS_SCENARIOS).toEqual([
    "reviewer-own-head",
    "reviewer-pass-and-fail",
    "architect-pass-and-fail",
    "ci-pass-and-fail",
    "fix-judge",
    "tester-reads-no-ci",
  ]);
  expect(headsCases("tester-reads-no-ci")).toEqual(["ci-late", "ci-red"]);
  expect(isHeadsScenario("reviewer-own-head")).toBe(true);
  expect(isHeadsScenario("measure-before-ask")).toBe(false);
});

// Catches: a reader that keeps a list of the worlds of its own, so a world added to the catalog
// alone is missing from rig.sh's batch, refused by its run, by heads-world.ts or by seed.ts's
// usage, or read by score.ts as another world's run. The added case's name begins another case's
// of its scenario (no-verdict, no-verdict-fail), so a pattern that tries the shorter name first
// reads no-verdict-fail's runs as no-verdict's.
test("a world added to the catalog alone reaches rig.sh, heads-world.ts, seed.ts and score.ts", () => {
  const copy = copyOfThisDirectory(makeDirectory("tree"));
  const tree = path.resolve(copy, "../../../..");
  appendFileSync(path.join(copy, "heads-scenarios.txt"), "architect-pass-and-fail no-verdict\n");
  const env = { PATH: process.env.PATH ?? "", HOME: makeDirectory("home") };
  const functions = rigFunctions(copy);
  const bash = (part: string, lines: string[], args: string[]) => {
    const ran = spawnWithin(
      { ...RIG_STEP, part },
      ["bash", "-c", [functions, ...lines].join("\n"), "-", ...args],
      { env: { ...env, SKILL_SCENARIOS_PARALLEL: "1" } }
    );
    return `exit=${ran.exitCode} ${ran.stderr.toString()}`;
  };

  // A batch of one run, one at a time (SKILL_SCENARIOS_PARALLEL=1), so its run lines keep the
  // order it hands them to `rig.sh run` in, which a stand-in rig.sh records.
  const here = makeDirectory("here");
  writeFileSync(path.join(here, "rig.sh"), `printf '%s\\n' "$*" >>"${here}/runs"\n`);
  const batch = bash(
    "rig.sh cmd_batch",
    [
      "load_label() { :; }",
      'lock_services() { S=$work/services; mkdir -p "$S"; }',
      "on_exit() { :; }",
      "services_up() { :; }",
      "date() { echo 1700000000; }",
      "go() { :; }",
      `here=$1 work=$2 root=${tree}`,
      "cmd_batch architect-pass-and-fail 1 head",
    ],
    [here, makeDirectory("batch")]
  );
  // A run of the added world, its run function stubbed.
  const work = makeDirectory("run");
  mkdirSync(path.join(work, "services"));
  writeFileSync(path.join(work, "services", "env"), "");
  const run = bash(
    "rig.sh cmd_run",
    [
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_architect_pass_and_fail() { printf '%s\\n' "$*" >"$R/ran"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${tree} hide_state_file=''`,
      "cmd_run architect-pass-and-fail head 1 4 1700000000 no-verdict",
    ],
    [work]
  );
  const ranFile = path.join(work, "maps/runs/architect-pass-and-fail-no-verdict-head-1/ran");
  let ranWith = "no run of the name architect-pass-and-fail-no-verdict-head-1";
  try {
    ranWith = readFileSync(ranFile, "utf8");
  } catch {}

  // heads-world.ts's world and score.ts's reading of three runs' names, from the copy.
  const probe = path.join(copy, "catalog-probe.ts");
  writeFileSync(
    probe,
    [
      'import { headsWorld } from "./heads-world";',
      'import { CASED_RUNS } from "./score";',
      'const read = (name: string) => CASED_RUNS["architect-pass-and-fail"]?.exec(name)?.slice(1);',
      "console.log(JSON.stringify({",
      '  world: headsWorld("architect-pass-and-fail", "no-verdict") ?? null,',
      "  runs: [",
      '    "architect-pass-and-fail-no-verdict-head-1",',
      '    "architect-pass-and-fail-no-verdict-fail-head-1",',
      '    "architect-pass-and-fail-head-1",',
      "  ].map(read),",
      "}));",
    ].join("\n")
  );
  const probed = spawnWithin(
    { ...SCRIPT_RUN, part: "catalog-probe.ts" },
    [process.execPath, probe],
    { cwd: copy, env }
  );
  expect({ code: probed.exitCode, stderr: probed.stderr.toString() }).toEqual({
    code: 0,
    stderr: "",
  });
  const usage = spawnWithin({ ...SCRIPT_RUN, part: "seed.ts" }, [process.execPath, "seed.ts"], {
    cwd: copy,
    env,
  }).stderr.toString();

  expect({
    batch,
    runs: readFileSync(path.join(here, "runs"), "utf8"),
    run,
    ranWith,
    probed: JSON.parse(probed.stdout.toString()),
    usage: usage.includes(
      "architect-pass-and-fail [no-verdict-fail|no-verdict-pass|ci-check-fail|ci-check-fail-other-head|no-verdict]|ci-pass-and-fail"
    ),
  }).toEqual({
    batch: "exit=0 ",
    runs: [
      "",
      " no-verdict-fail",
      " no-verdict-pass",
      " ci-check-fail",
      " ci-check-fail-other-head",
      " no-verdict",
    ]
      .map((which, at) => `run architect-pass-and-fail head 1 ${at + 1} 1700000000${which}\n`)
      .join(""),
    run: "exit=0 ",
    ranWith: "4 1700000000 no-verdict\n",
    probed: {
      world: "architect-pass-and-fail-no-verdict",
      runs: [
        ["no-verdict", "head"],
        ["no-verdict-fail", "head"],
        [null, "head"],
      ],
    },
    usage: true,
  });
});

/** Runs `lines` under bash after every function of `copy`'s rig.sh, with `args` as $1...: the
 * exit status and stderr, as one string. */
function rigStep(copy: string, part: string, lines: string[], args: string[]): string {
  const ran = spawnWithin(
    { ...RIG_STEP, part },
    ["bash", "-c", [rigFunctions(copy), ...lines].join("\n"), "-", ...args],
    {
      env: {
        PATH: process.env.PATH ?? "",
        HOME: makeDirectory("home"),
        SKILL_SCENARIOS_PARALLEL: "1",
      },
    }
  );
  return `exit=${ran.exitCode} ${ran.stderr.toString()}`;
}

/** The lines of a cmd_run of `copy`'s rig.sh in a run directory of its own, `work`, whose label
 * loads as a stub and whose exit handler does nothing. */
function runLines(work: string, tree: string): string[] {
  mkdirSync(path.join(work, "services"));
  writeFileSync(path.join(work, "services", "env"), "");
  return [
    'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
    "on_exit() { :; }",
    `work=$1 maps=$1/maps services_env=$1/services/env root=${tree} hide_state_file=''`,
  ];
}

// Catches: a heads scenario's runner that drops the run's case, so the run of a case added to the
// catalog alone carries the case's name and builds the scenario's own world: the runner of
// reviewer-pass-and-fail called heads_world with no case. Each scenario's actual runner runs; only
// heads_world, which builds the world, records what it was handed and stops the run there.
test("each heads scenario's runner hands a case added to the catalog alone to the world it builds", () => {
  const copy = copyOfThisDirectory(makeDirectory("tree"));
  const tree = path.resolve(copy, "../../../..");
  appendFileSync(
    path.join(copy, "heads-scenarios.txt"),
    HEADS_SCENARIOS.map((scenario) => `${scenario} new-case\n`).join("")
  );
  const handed: Record<string, string> = {};
  for (const scenario of HEADS_SCENARIOS) {
    const work = makeDirectory("run");
    const ran = rigStep(
      copy,
      `rig.sh cmd_run ${scenario}`,
      [
        ...runLines(work, tree),
        'heads_world() { printf "%s\\n" "$*" >"$R/world"; : >"$R/fixture-only"; }',
        `cmd_run ${scenario} head 1 3 1700000000 new-case`,
      ],
      [work]
    );
    try {
      handed[scenario] = readFileSync(
        path.join(work, "maps", "runs", `${scenario}-new-case-head-1`, "world"),
        "utf8"
      );
    } catch {
      handed[scenario] = ran;
    }
  }
  expect(handed).toEqual(
    Object.fromEntries(HEADS_SCENARIOS.map((s) => [s, `${s} 3 1700000000 new-case\n`]))
  );
});

// Catches: a catalog row whose scenario rig.sh does not run, such as a typo of one it does, which
// the readers passed over: the batch of the scenario meant ran without that row's case, exited 0
// and said nothing, and score.ts read the case's runs as runs of a label.
test("a catalog row whose scenario rig.sh does not run refuses every batch and run before anything starts, and the score", () => {
  const copy = copyOfThisDirectory(makeDirectory("tree"));
  const tree = path.resolve(copy, "../../../..");
  const catalog = path.join(copy, "heads-scenarios.txt");
  const good = readFileSync(catalog, "utf8");
  const typo = good.replace(
    "architect-pass-and-fail no-verdict-fail\n",
    "architect-pass-and-failx no-verdict-fail\n"
  );
  expect(typo).not.toBe(good);
  writeFileSync(catalog, typo);
  const refusal = "lists the scenario 'architect-pass-and-failx', which rig.sh does not run";

  // A batch of the scenario the row meant, and of a scenario with no heads world: neither starts
  // its services, nor hands a run to `rig.sh run`.
  const batches: Record<string, unknown> = {};
  for (const scenario of ["architect-pass-and-fail", "brainstorm"]) {
    const here = makeDirectory("here");
    writeFileSync(path.join(here, "rig.sh"), `printf '%s\\n' "$*" >>"${here}/runs"\n`);
    const batch = rigStep(
      copy,
      `rig.sh cmd_batch ${scenario}`,
      [
        "load_label() { :; }",
        `lock_services() { echo started >>"${here}/started"; S=$work/services; mkdir -p "$S"; }`,
        "on_exit() { :; }",
        "services_up() { :; }",
        "date() { echo 1700000000; }",
        "go() { :; }",
        `here=$1 work=$2 root=${tree}`,
        `cmd_batch ${scenario} 1 head`,
      ],
      [here, makeDirectory("batch")]
    );
    batches[scenario] = {
      refused: batch.startsWith("exit=1 ") && batch.includes(refusal),
      started: readdirSync(here).filter((file) => file !== "rig.sh"),
    };
  }
  // A run, refused before its world is built.
  const work = makeDirectory("run");
  const run = rigStep(
    copy,
    "rig.sh cmd_run",
    [
      ...runLines(work, tree),
      'heads_world() { : >"$R/world"; : >"$R/fixture-only"; }',
      "cmd_run architect-pass-and-fail head 1 3 1700000000 no-verdict-pass",
    ],
    [work]
  );
  // The score of an empty map of runs.
  const runs = path.join(makeDirectory("score"), "runs");
  mkdirSync(runs);
  const scored = spawnWithin(
    { ...SCRIPT_RUN, part: "score.ts runs" },
    [process.execPath, "score.ts", "runs", runs],
    { cwd: copy, env: { PATH: process.env.PATH ?? "", HOME: makeDirectory("home") } }
  );

  expect({
    batches,
    run: run.startsWith("exit=1 ") && run.includes(refusal),
    built: existsSync(path.join(work, "maps")),
    score: {
      failed: scored.exitCode !== 0,
      says: scored.stderr
        .toString()
        .includes(
          "heads-scenarios.txt lists the scenario architect-pass-and-failx, which score.ts has no scorer for"
        ),
    },
  }).toEqual({
    batches: {
      "architect-pass-and-fail": { refused: true, started: [] },
      brainstorm: { refused: true, started: [] },
    },
    run: true,
    built: false,
    score: { failed: true, says: true },
  });
});

// Catches: a lookup of a catalog scenario that reads what every object inherits. scorers is an
// object, so scorers["constructor"] was Object, a catalog row naming constructor passed the
// scorer check, and its runs were neither scored nor refused. __proto__ and toString are names the
// catalog's reader refuses (a name is lower-case words and hyphens); constructor is the inherited
// name that shape lets through. The known scenario is the control: its run is listed, so a score
// that reads no run cannot pass for a refusal.
test("score.ts refuses a catalog scenario named after what every object inherits, and lists a known one's run", () => {
  const read: [string, { failed: boolean; listed: boolean; refused: boolean }][] = [];
  for (const scenario of ["architect-pass-and-fail", "constructor", "__proto__", "toString"]) {
    const copy = copyOfThisDirectory(makeDirectory("tree"));
    const catalog = path.join(copy, "heads-scenarios.txt");
    writeFileSync(
      catalog,
      readFileSync(catalog, "utf8").replace(
        "architect-pass-and-fail no-verdict-fail\n",
        `${scenario} no-verdict-fail\n`
      )
    );
    const runs = path.join(makeDirectory("score"), "runs");
    const run = `${scenario}-no-verdict-fail-head-1`;
    mkdirSync(path.join(runs, run), { recursive: true });
    writeFileSync(path.join(runs, run, "fixture-only"), "");
    const scored = spawnWithin(
      { ...SCRIPT_RUN, part: `score.ts runs, the catalog naming ${scenario}` },
      [process.execPath, "score.ts", "runs", runs],
      { cwd: copy, env: { PATH: process.env.PATH ?? "", HOME: makeDirectory("home") } }
    );
    const stderr = scored.stderr.toString();
    read.push([
      scenario,
      {
        failed: scored.exitCode !== 0,
        listed: scored.stdout.toString().includes(`${run}\tfixture only`),
        refused:
          stderr.includes(`lists the scenario ${scenario}, which score.ts has no scorer for`) ||
          stderr.includes(`'${scenario} no-verdict-fail' is not <scenario> [<case>]`),
      },
    ]);
  }
  const refused = { failed: true, listed: false, refused: true };
  expect(read).toEqual([
    ["architect-pass-and-fail", { failed: false, listed: true, refused: false }],
    ["constructor", refused],
    ["__proto__", refused],
    ["toString", refused],
  ]);
});
