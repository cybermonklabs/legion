#!/usr/bin/env bun
/**
 * `rig.sh health <run dir>...`: whether the world of each run could take what its agents did. It
 * does not score the run, it judges no agent, and it writes nothing. A run is healthy when all
 * three of these hold:
 *   1. finished: its out.txt ends with an `exit=` line (completion-answers.ts's `finished`);
 *   2. no refused completion: no `handoff/complete` was refused 4xx with an answer its world's
 *      entry does not allow for the run's label and the completing role. The scorer's
 *      refused-completion rule reads this through the same function (`refusedCompletion`, which
 *      score.ts's `unscored` calls);
 *   3. taken: a scenario whose world must take a role's completion (`ANSWERS`' `takes`) took it at
 *      least once, with an answer the entry allows. reviewer-text-findings requires one of the
 *      reviewer's; threads-closed requires none.
 * Prints one line per run, `<run>  healthy` or `<run>  unhealthy: <reason>`, naming the first
 * rule the run fails (for rule 2, the role, status and code of the first such refusal), then
 * `health: <n> of <m> runs unhealthy`. Exits 0 when every run is healthy, 1 otherwise, and 2,
 * printing no run, for no argument or for a path that is not a run directory of a scenario
 * `ANSWERS` names and of a label `LABEL_DAEMONS` names.
 */
import { existsSync, statSync } from "node:fs";
import path from "node:path";
import {
  answerOf,
  daemonLines,
  finished,
  labelOf,
  refusedCompletion,
  scenarioOf,
  takenCompletions,
} from "./completion-answers";

const USAGE = "usage: rig.sh health <run dir>...";

/** The run directory `dir` names, with its scenario and label, or why it is none. */
function runOf(dir: string): { scenario: string; label: string } | string {
  if (!existsSync(dir) || !statSync(dir).isDirectory()) return `${dir} is not a directory`;
  const scenario = scenarioOf(dir);
  if (scenario === undefined)
    return `${dir} is no run of a scenario health reads (completion-answers.ts's ANSWERS)`;
  try {
    return { scenario, label: labelOf(dir) };
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
}

/** Why the run is unhealthy, or undefined when it is healthy. */
function unhealthy(runDir: string, scenario: string, label: string): string | undefined {
  if (!finished(runDir)) return "unfinished: out.txt has no exit= line";
  const lines = daemonLines(runDir);
  const refused = refusedCompletion(lines, scenario, label);
  if (refused !== undefined)
    return `refused completion: POST ${refused.line.path} of the ${refused.role ?? "unnamed role"} answered ${answerOf(refused.line)}, which the ${label} label's daemon does not answer it with in this world`;
  const taken = takenCompletions(lines, scenario, label);
  if (taken !== undefined && taken.count === 0)
    return `no completion taken: no completion of the ${taken.role} was answered as the ${label} label's daemon answers one it takes`;
  return undefined;
}

if (import.meta.main) {
  const dirs = Bun.argv.slice(2);
  if (dirs.length === 0) {
    console.error(USAGE);
    process.exit(2);
  }
  const runs = dirs.map((dir) => ({ dir, run: runOf(dir) }));
  const bad = runs.find((one) => typeof one.run === "string");
  if (bad !== undefined) {
    console.error(`health: ${String(bad.run)}`);
    console.error(USAGE);
    process.exit(2);
  }
  let failed = 0;
  for (const { dir, run } of runs) {
    if (typeof run === "string") continue;
    const reason = unhealthy(dir, run.scenario, run.label);
    if (reason !== undefined) failed += 1;
    console.log(
      `${path.basename(dir)}  ${reason === undefined ? "healthy" : `unhealthy: ${reason}`}`
    );
  }
  console.log(`health: ${failed} of ${runs.length} runs unhealthy`);
  process.exit(failed === 0 ? 0 : 1);
}
