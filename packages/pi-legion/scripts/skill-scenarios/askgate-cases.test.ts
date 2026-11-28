// askgate-cases.txt, the catalog of the askgate scenario's cases, and the readers that take each
// case from it: rig.sh's batch and run, and score.ts's run names.
//
//   bun test askgate-cases.test.ts

import { expect, setDefaultTimeout, test } from "bun:test";
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { spawnWithin } from "@legion/test-fixtures/row-clock";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { copyOfThisDirectory, rigCommand } from "./catalog-copy";
import { fileRowLimit, RIG_STEP, SCRIPT_RUN } from "./test-limit";

// The most a row runs as processes of their own: two rig.sh steps and one bun script, or three
// bun scripts.
setDefaultTimeout(fileRowLimit(RIG_STEP, RIG_STEP, SCRIPT_RUN));

const makeDirectory = temporaryDirectories("askgate-catalog");

// Catches: a reader that keeps a list of the askgate cases of its own, so a case added to the
// catalog alone is refused by rig.sh's batch or run, or read by score.ts as another case's run.
// The added case's name begins with another case's (upload), so a pattern that tries the shorter
// name first reads its run as upload's, of the label twice-head.
test("a case added to the askgate catalog alone reaches rig.sh and score.ts", () => {
  const copy = copyOfThisDirectory(makeDirectory("tree"));
  const tree = path.resolve(copy, "../../../..");
  appendFileSync(path.join(copy, "askgate-cases.txt"), "upload-twice\n");
  const env = {
    PATH: process.env.PATH ?? "",
    HOME: makeDirectory("home"),
    SKILL_SCENARIOS_ASKGATE_CASE: "upload-twice",
    SKILL_SCENARIOS_PARALLEL: "1",
  };
  const bash = (part: string, lines: string[], args: string[]) => {
    const ran = spawnWithin({ ...RIG_STEP, part }, rigCommand(copy, lines, args), { env });
    return `exit=${ran.exitCode} ${ran.stderr.toString()}`;
  };

  // A batch of one run, whose run line a stand-in rig.sh records.
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
      `here=$1 work=$2 root=${tree}`,
      "cmd_batch askgate 1 head",
    ],
    [here, makeDirectory("batch")]
  );
  let runs = "no run line";
  try {
    runs = readFileSync(path.join(here, "runs"), "utf8");
  } catch {}
  // A run of the added case, its run function stubbed.
  const work = makeDirectory("run");
  mkdirSync(path.join(work, "services"));
  writeFileSync(path.join(work, "services", "env"), "");
  const run = bash(
    "rig.sh cmd_run",
    [
      'load_label() { P=$work/profiles/0000000000000001; mkdir -p "$P"; echo "checkout /c/$1" >"$P/built-from"; }',
      "on_exit() { :; }",
      `run_askgate() { echo "$SKILL_SCENARIOS_ASKGATE_CASE" >"$R/ran"; echo exit=0 >"$R/out.txt"; }`,
      `work=$1 maps=$1/maps services_env=$1/services/env root=${tree} hide_state_file=''`,
      "cmd_run askgate head 1 1 1700000000",
    ],
    [work]
  );
  let ranWith = "no run of the name askgate-upload-twice-head-1";
  try {
    ranWith = readFileSync(path.join(work, "maps/runs/askgate-upload-twice-head-1/ran"), "utf8");
  } catch {}

  // run-names.ts's reading of two runs' names, which score.ts reads them through, from the copy.
  const probe = path.join(copy, "catalog-probe.ts");
  writeFileSync(
    probe,
    [
      'import { runFields } from "./run-names";',
      "console.log(JSON.stringify(",
      '  ["askgate-upload-twice-head-1", "askgate-upload-head-1"].map(',
      "    (name) => [runFields(name)?.case, runFields(name)?.label]",
      "  )",
      "));",
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

  expect({ batch, runs, run, ranWith, read: JSON.parse(probed.stdout.toString()) }).toEqual({
    batch: "exit=0 ",
    runs: "run askgate head 1 1 1700000000\n",
    run: "exit=0 ",
    ranWith: "upload-twice\n",
    read: [
      ["upload-twice", "head"],
      ["upload", "head"],
    ],
  });
});

// Catches: an askgate case the catalog lists and score.ts has no rule for, which it read by
// another case's rule: a run of the case constructor by the rules check's, since
// RULE_REFUSED_CASES["constructor"] is what every object inherits, and a run of any other such case
// by upload's, the scorer's last. The case upload is the control: its run is scored by its rule.
test("score.ts reads an askgate run of a case it has no rule for as a rig error, constructor among them", () => {
  const read: [string, string][] = [];
  for (const which of ["upload", "constructor", "upload-twice"]) {
    const copy = copyOfThisDirectory(makeDirectory("tree"));
    if (which !== "upload") appendFileSync(path.join(copy, "askgate-cases.txt"), `${which}\n`);
    // A run as rig.sh's map holds it: its own Dispatch, its exit, the issue's asks and a transcript
    // of a session whose agent did nothing.
    const runs = path.join(makeDirectory("score"), "runs");
    const run = path.join(runs, `askgate-${which}-on-1`);
    mkdirSync(path.join(run, "sessions"), { recursive: true });
    writeFileSync(
      path.join(run, "dispatch.json"),
      JSON.stringify({
        postgres: "pg-1",
        database: "run_1",
        port: 20000,
        url: "http://127.0.0.1:20000",
      })
    );
    writeFileSync(path.join(run, "out.txt"), "exit=0\n");
    writeFileSync(path.join(run, "asks.json"), "[]\n");
    writeFileSync(
      path.join(run, "sessions", "2026-09-30T14-51-17-651Z_01a0f2cc.jsonl"),
      [
        { type: "session", id: "01a0f2cc", cwd: "/rig/cwd" },
        { type: "model_change", model: "anthropic/claude-opus-4-8" },
        { type: "thinking_level_change", thinkingLevel: "high" },
        { type: "message", message: { role: "user", content: [{ type: "text", text: "Ask." }] } },
        {
          type: "message",
          message: { role: "assistant", content: [{ type: "text", text: "Done." }] },
        },
      ]
        .map((entry) => `${JSON.stringify(entry)}\n`)
        .join("")
    );
    const scored = spawnWithin(
      { ...SCRIPT_RUN, part: `score.ts runs, a run of the askgate case ${which}` },
      [process.execPath, "score.ts", "runs", runs, "askgate"],
      { cwd: copy, env: { PATH: process.env.PATH ?? "", HOME: makeDirectory("home") } }
    );
    read.push([
      which,
      scored.stdout
        .toString()
        .split("\n")
        .find((line) => line.startsWith(`askgate-${which}-on-1\t`)) ??
        `exit=${scored.exitCode} ${scored.stderr.toString()}`,
    ]);
  }
  expect(read).toEqual([
    [
      "upload",
      "askgate-upload-on-1\tpass=false\tref=false\tcase=upload reviewedUpload=none exit=0",
    ],
    [
      "constructor",
      "askgate-constructor-on-1\trig error: askgate-cases.txt lists the case constructor, which score.ts has no rule for",
    ],
    [
      "upload-twice",
      "askgate-upload-twice-on-1\trig error: askgate-cases.txt lists the case upload-twice, which score.ts has no rule for",
    ],
  ]);
});
