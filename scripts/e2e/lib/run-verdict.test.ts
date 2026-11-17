import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { basename, dirname, join, relative } from "node:path";
import { scriptFunctions, sourcedLibraries } from "./script-functions";

// lib/run-verdict.sh is the one rule for a proof's last line: PASS only for a run that ends with
// status 0 and reached its end, the run_end the script calls after its last check. bash runs a
// shell's EXIT trap with status 0 when an untrapped TERM, INT or HUP ends it, so the status alone
// reads a killed run as a pass. The first two describes drive the real lib with a fixture proof
// that sources it; the last holds every script under scripts/e2e to the rule: no EXIT trap prints
// PASS but through run_verdict, and each run_end stands where nothing but the shell's end follows.

const lib = join(import.meta.dir, "run-verdict.sh");
const e2e = join(import.meta.dir, "..");
const stopped =
  "the run stopped before its end, by a signal or an exit of its own: its status 0 is no pass";

/** Where the fixture proof stops: before its first check, between its two checks, or nowhere. */
type Stop = { at: "setup" | "checks"; with: string } | undefined;

/**
 * A fixture proof: it sources the real lib, sets its EXIT trap (TRAPS), runs a setup and two
 * checks, and calls run_end after the last. STOP runs a command of the caller's at that point:
 * a signal the shell sends itself, an exit of its own, or a failure.
 */
function fixtureProof(stop: Stop, traps = "trap cleanup EXIT") {
  const at = (point: string) => (stop?.at === point ? `${stop.with}\nsleep 5` : "");
  const result = Bun.spawnSync([
    "bash",
    "-c",
    `set -euo pipefail
. ${JSON.stringify(lib)}
check=setup
cleanup() {
  local status=\${1:-$?}
  trap - EXIT INT TERM
  run_verdict "$status" "fixture e2e: PASS" "fixture e2e: FAIL at $check"
}
${traps}
${at("setup")}
check=first
echo "ok first"
${at("checks")}
check=last
echo "ok last"
run_end`,
  ]);
  return {
    code: result.exitCode,
    signal: result.signalCode ?? null,
    out: result.stdout.toString(),
    err: result.stderr.toString(),
  };
}

const signals = [
  { name: "TERM", status: 143 },
  { name: "INT", status: 130 },
  { name: "HUP", status: 129 },
] as const;

describe("run_verdict over a fixture proof", () => {
  for (const { name, status } of signals) {
    test(`reads FAIL, keeping the shell's status, for a run a SIG${name} ended between its checks`, () => {
      const run = fixtureProof({ at: "checks", with: `kill -s ${name} $$` });
      expect(run.out).toBe("ok first\n");
      expect(run.err).toBe(`${stopped}\nfixture e2e: FAIL at first\n`);
      expect(run.code === status || run.signal === `SIG${name}`).toBe(true);
    });
  }

  test("reads FAIL for a run a SIGTERM ended in its setup, before its first check", () => {
    const run = fixtureProof({ at: "setup", with: "kill -s TERM $$" });
    expect(run.out).toBe("");
    expect(run.err).toBe(`${stopped}\nfixture e2e: FAIL at setup\n`);
    expect(run.code === 143 || run.signal === "SIGTERM").toBe(true);
  });

  test("reads FAIL, and exits 1, for an exit 0 the run makes before its end", () => {
    const run = fixtureProof({ at: "setup", with: "exit 0" });
    expect(run.out).toBe("");
    expect(run.err).toBe(`${stopped}\nfixture e2e: FAIL at setup\n`);
    expect(run.code).toBe(1);
  });

  test("reads FAIL, keeping its status, for a run that failed a check", () => {
    const run = fixtureProof({ at: "checks", with: "exit 3" });
    expect(run.err).toBe("fixture e2e: FAIL at first\n");
    expect(run.code).toBe(3);
  });

  test("reads PASS, on stdout, for a run that reached its end", () => {
    const run = fixtureProof(undefined);
    expect(run.out).toBe("ok first\nok last\nfixture e2e: PASS\n");
    expect(run.err).toBe("");
    expect(run.code).toBe(0);
  });
});

// The shape of dispatch-claimant-delivery.sh, verification-results.sh, plan-pointer-drive.sh,
// refusal-fallback-omp.sh and lib/two-builds.sh: INT and TERM trapped to cleanup with the
// signal's status, HUP not trapped, so the EXIT trap reads 0 for a hangup.
describe("run_verdict under a script that traps INT and TERM itself", () => {
  const traps = `trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM`;

  test("reads FAIL for a run a SIGHUP ended, which its EXIT trap reads as status 0", () => {
    const run = fixtureProof({ at: "checks", with: "kill -s HUP $$" }, traps);
    expect(run.err).toBe(`${stopped}\nfixture e2e: FAIL at first\n`);
    expect(run.code === 129 || run.signal === "SIGHUP").toBe(true);
  });

  test("reads FAIL with exit 143 for a run its own TERM trap ended", () => {
    const run = fixtureProof({ at: "checks", with: "kill -s TERM $$" }, traps);
    expect(run.err).toBe("fixture e2e: FAIL at first\n");
    expect(run.code).toBe(143);
  });

  test("reads PASS for a run that reached its end", () => {
    const run = fixtureProof(undefined, traps);
    expect(run.out).toEndWith("fixture e2e: PASS\n");
    expect(run.code).toBe(0);
  });
});

// Every shell script of scripts/e2e and its libraries, read once.
const scripts = [
  ...new Bun.Glob("*.sh").scanSync({ cwd: e2e, absolute: true }),
  ...new Bun.Glob("lib/*.sh").scanSync({ cwd: e2e, absolute: true }),
].sort();
const shown = (path: string) => relative(e2e, path);
const text = new Map(scripts.map((path) => [path, readFileSync(path, "utf8")]));
// A line of code, not a comment.
const code = (line: string) => line.trim() !== "" && !line.trim().startsWith("#");

/** Each EXIT trap a script sets: the trap's own command text, and the function it runs, with the
 * file that defines it, when that command starts with a function the script reaches. */
function exitTraps(path: string) {
  const found: { command: string; fn?: { name: string; body: string; file: string } }[] = [];
  for (const trap of (text.get(path) ?? "").matchAll(
    /^\s*trap\s+('[^']*'|"[^"]*"|\S+)\s+([A-Z0-9 ]+?)\s*$/gm
  )) {
    if (!trap[2].split(/\s+/).includes("EXIT")) continue;
    const command = trap[1].replace(/^(['"])([\s\S]*)\1$/, "$2");
    if (command === "-" || command === "") continue;
    const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(command)?.[0];
    let fn: { name: string; body: string; file: string } | undefined;
    for (const file of name === undefined ? [] : sourcedLibraries(path)) {
      try {
        fn = { name: name as string, body: scriptFunctions(file)(name as string), file };
        break;
      } catch {
        // Not this file's function: the next one the script sources may define it.
      }
    }
    found.push({ command, fn });
  }
  return found;
}

/** Whether the script at PATH calls run_end on a line of its own. */
const marks = (path: string) => /^\s*run_end\s*$/m.test(text.get(path) ?? "");

describe("every script under scripts/e2e", () => {
  test("prints PASS from an EXIT trap only through run_verdict, from a file that sources the lib", () => {
    const wrong: string[] = [];
    for (const path of scripts) {
      for (const { command, fn } of exitTraps(path)) {
        if (/\bPASS\b/.test(command)) wrong.push(`${shown(path)}: trap '${command}' prints PASS`);
        if (fn === undefined) continue;
        for (const line of fn.body.split("\n")) {
          if (/^\s*(echo|printf)\b.*\bPASS\b/.test(line)) {
            wrong.push(`${shown(fn.file)}: ${fn.name} prints PASS itself: ${line.trim()}`);
          }
        }
        if (fn.body.includes("run_verdict") && !sourcedLibraries(fn.file).includes(lib)) {
          wrong.push(
            `${shown(fn.file)}: ${fn.name} calls run_verdict and sources no lib/run-verdict.sh`
          );
        }
      }
    }
    expect(wrong).toEqual([]);
  });

  test("whose EXIT trap prints its verdict through run_verdict reaches a run_end", () => {
    const wrong: string[] = [];
    for (const path of scripts) {
      for (const { fn } of exitTraps(path)) {
        if (fn === undefined || !fn.body.includes("run_verdict") || marks(fn.file)) continue;
        // A library's trap: each entry script that sources the library marks its own end.
        const callers = scripts.filter(
          (other) => dirname(other) === e2e && sourcedLibraries(other).includes(fn.file)
        );
        if (callers.length === 0)
          wrong.push(`${shown(fn.file)}: no script sources it or marks an end`);
        for (const caller of callers.filter((other) => !marks(other))) {
          wrong.push(
            `${shown(caller)}: ${fn.name} in ${basename(fn.file)} prints its verdict, and it calls no run_end`
          );
        }
      }
    }
    expect(wrong).toEqual([]);
  });

  test("calls run_end alone on its line, with nothing but the shell's end after it", () => {
    const wrong: string[] = [];
    for (const path of scripts.filter((path) => path !== lib)) {
      const lines = (text.get(path) ?? "").split("\n");
      lines.forEach((line, index) => {
        if (!code(line)) return;
        if (/\brun_ended=/.test(line))
          wrong.push(`${shown(path)}:${index + 1} sets the mark by hand`);
        if (!/\brun_end\b/.test(line)) return;
        if (!/^\s*run_end\s*$/.test(line)) {
          wrong.push(
            `${shown(path)}:${index + 1} calls run_end inside another command: ${line.trim()}`
          );
          return;
        }
        // The next line of code: none (the script's end), the closing brace of the function that
        // runs the proof, or the `exit 0` of an end the script takes early.
        const next = lines.slice(index + 1).find(code);
        if (next !== undefined && next !== "}" && next.trim() !== "exit 0") {
          wrong.push(`${shown(path)}:${index + 1} runs more after run_end: ${next.trim()}`);
        }
      });
    }
    expect(wrong).toEqual([]);
  });
});
