import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { basename, dirname, join, relative } from "node:path";
import { stopped } from "./live-daemon-proof-fixture";
import { scriptFunctions, sourcedLibraries } from "./script-functions";

// lib/run-verdict.sh is the one rule for a proof's last line: PASS only for a run that ends with
// status 0 and reached its end, the run_end the script calls after its last check. bash runs a
// shell's EXIT trap with status 0 when an untrapped TERM, INT or HUP ends it, so the status alone
// reads a killed run as a pass. The first two describes drive the real lib with a fixture proof
// that sources it; the last holds every script under scripts/e2e to the rule: no EXIT trap prints
// PASS but through run_verdict, and each run_end stands where nothing but the shell's end follows:
// the script's end, a subshell's `)` or an `exit 0`, or the closing brace of a function every call
// of which stands there in turn.

const lib = join(import.meta.dir, "run-verdict.sh");
const e2e = join(import.meta.dir, "..");

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
// refusal-fallback-omp.sh, clean-session-stop-tmux.sh, envoy-session-states.sh and
// lib/two-builds.sh: INT and TERM trapped to cleanup with the signal's status, HUP not trapped, so
// the EXIT trap reads 0 for a hangup. dispatch-session-settings.sh, the eighth script that traps
// INT and TERM, traps them to an exit with that status, which reaches its EXIT trap as the status.
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
// Every library each script sources (sourcedLibraries), read once.
const sources = new Map(scripts.map((path) => [path, sourcedLibraries(path)]));
// A line of code, not a comment.
const code = (line: string) => line.trim() !== "" && !line.trim().startsWith("#");

/**
 * The command of each trap SCRIPT sets on EXIT, in any command of a line (commandsOf), so a trap
 * an operator joins to another command is one: the trap's command text, its quotes taken off. A
 * trap that resets EXIT (`-`) or ignores it (`''`) sets none.
 */
function exitTrapCommands(script: string): string[] {
  return commandsOf(script).flatMap((command) => {
    const trap = /^trap\s+('[^']*'|"[^"]*"|\S+)\s+([A-Z0-9 ]+?)\s*$/.exec(command);
    if (trap === null || !trap[2].split(/\s+/).includes("EXIT")) return [];
    const run = trap[1].replace(/^(['"])([\s\S]*)\1$/, "$2");
    return run === "-" || run === "" ? [] : [run];
  });
}

/** Each EXIT trap a script sets: the trap's own command text, and the function it runs, with the
 * file that defines it, when that command starts with a function the script reaches. */
function exitTraps(path: string) {
  return exitTrapCommands(text.get(path) ?? "").map((command) => {
    const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(command)?.[0];
    let fn: { name: string; body: string; file: string } | undefined;
    for (const file of name === undefined ? [] : (sources.get(path) ?? [])) {
      try {
        fn = { name: name as string, body: scriptFunctions(file)(name as string), file };
        break;
      } catch {
        // Not this file's function: the next one the script sources may define it.
      }
    }
    return { command, fn };
  });
}

/** Each command of BODY, a function's definition, that prints PASS itself: an echo or printf of
 *  it, in any command of a line (commandsOf). */
const printsPass = (body: string) =>
  commandsOf(body).filter((command) => /^(echo|printf)\b.*\bPASS\b/.test(command));

/** Whether the script at PATH calls run_end on a line of its own. */
const marks = (path: string) => /^\s*run_end\s*$/m.test(text.get(path) ?? "");

/** The line that opens a function a script defines over more than one line: `name() {`. */
const opens = /^([A-Za-z_][A-Za-z0-9_]*)\(\) \{\s*$/;

/** A line of code that opens an `if` it does not close: one whose own `fi` is not on it, as the
 *  `fi` of `if …; then …; fi` is. */
const opensIf = (word: string) => /^if\b/.test(word) && !/[\s;]fi(\s|;|$)/.test(word);

/** A line of code that closes an `if`: its `fi` alone, or before a comment. */
const closesIf = /^fi(\s+#.*)?$/;

/** A command's quoted text: single-quoted, or double-quoted with its backslash escapes. */
const quotedText = /'[^']*'|"(?:\\.|[^"\\])*"/g;

/** A command's comment: a `#` that starts a word, to the end of its line, after the space before it. */
const commentText = /(^|\s)#.*$/gm;

/**
 * The command that starts on line INDEX of LINES, with each line it continues onto with a
 * trailing backslash, as the shell reads its operators: each quoted text and each comment emptied.
 */
function commandAt(lines: string[], index: number): string {
  let command = lines[index] ?? "";
  for (let at = index; at < lines.length - 1 && lines[at].trimEnd().endsWith("\\"); at++) {
    command += `\n${lines[at + 1]}`;
  }
  return command.replace(quotedText, "''").replace(commentText, "$1");
}

/** A control operator (`;`, `|`, `||`, `&&`, `&`) in a command commandAt reads: after a call on
 *  its line it runs more, and `>&` and `&>` are redirections, not operators. */
const controlOperator = /;|\||&&|(?<![<>&])&(?!>)/;

/** What a command may follow at its start on its line: a reserved word that opens or goes on with
 *  a compound command, `!`, a subshell's `(`, and a function's `name()` and `{`. */
const leadingWords =
  /^(?:\s*(?:(?:if|then|else|elif|do|while|until|!|\{)(?=\s|$)|\(|[A-Za-z_][A-Za-z0-9_]*\(\)))*/;

/**
 * Each command of TEXT as the shell cuts it: at each line's end but where a trailing backslash
 * continues the line, and at each control operator (controlOperator) outside quoted text and
 * comments. Each comment is taken off, each quoted text kept as written, and the words a command
 * follows at its start (leadingWords) taken off it. So `cond && trap 'x' EXIT` holds the trap,
 * and `if c; then echo PASS; fi` the echo.
 */
function commandsOf(text: string): string[] {
  return text.split(/(?<!\\[ \t]*)\n/).flatMap((command) => {
    // The operators are read where each quoted text is blanked and each comment emptied, at the
    // offsets they stand at, and each command is cut from the text with its comments emptied.
    const masked = command.replace(quotedText, (quoted) => "_".repeat(quoted.length));
    const read = masked.replace(commentText, (comment, space: string) =>
      space.padEnd(comment.length)
    );
    let kept = "";
    for (let at = 0; at < command.length; at++) kept += read[at] === masked[at] ? command[at] : " ";
    const commands: string[] = [];
    let start = 0;
    for (const operator of read.matchAll(new RegExp(controlOperator.source, "g"))) {
      const at = operator.index ?? 0;
      commands.push(kept.slice(start, at));
      start = at + operator[0].length;
    }
    commands.push(kept.slice(start));
    return commands.map((simple) => simple.replace(leadingWords, "").trim()).filter(Boolean);
  });
}

/**
 * What runs after the command on line INDEX of the script at PATH, in the shell it runs in, read
 * from TEXTS, each script's text by its path: a line for each command that runs after it, and
 * none when only the shell's end follows. The lines of code after the command, past the lines it
 * continues onto with a trailing backslash, are read in order. The script's end, the `)` that ends
 * the subshell the command runs in, and an `exit 0` end the shell. A `fi` that closes an `if`
 * around the command ends nothing, and an `else` or `elif` branch does not run after the command,
 * up to that `if`'s own `fi`. A `}` alone on its line closes the function the command is the last
 * of (afterFunction), so what runs after the command is what runs after each call of that
 * function. Any other line is a command that runs after it. WHAT names the command in each line,
 * and SEEN is the functions already followed.
 */
function runsAfter(
  texts: Map<string, string>,
  path: string,
  index: number,
  what: string,
  seen: Set<string>
): string[] {
  const lines = (texts.get(path) ?? "").split("\n");
  let at = index;
  while (at < lines.length - 1 && lines[at].trimEnd().endsWith("\\")) at++;
  let skipped = 0;
  for (let next = at + 1; next < lines.length; next++) {
    if (!code(lines[next])) continue;
    const word = lines[next].trim();
    if (skipped > 0) {
      if (opensIf(word)) skipped++;
      else if (closesIf.test(word)) skipped--;
      continue;
    }
    if (word.startsWith(")") || word === "exit 0") return [];
    if (closesIf.test(word)) continue;
    if (/^(else|elif)\b/.test(word)) {
      skipped = 1;
      continue;
    }
    if (lines[next] === "}") return afterFunction(texts, path, lines, next, what, seen);
    return [`${what} runs more after it: ${shown(path)}:${next + 1} ${word}`];
  }
  return [];
}

/**
 * What runs after the function whose closing brace is line CLOSE of LINES, the script at PATH,
 * returns: what runs after each of its calls, in that script and in each script that sources it,
 * read from TEXTS (runsAfter). The function is the one the nearest `name() {` above the brace
 * opens. It must have one call at least, and each call must stand on a line of its own, as its
 * line's first word, with no control operator after it on its line (controlOperator); a call
 * inside another command, or followed on its line by one, is a command that can run more after it.
 */
function afterFunction(
  texts: Map<string, string>,
  path: string,
  lines: string[],
  close: number,
  what: string,
  seen: Set<string>
): string[] {
  let open = close - 1;
  while (open >= 0 && !opens.test(lines[open])) open--;
  const name = opens.exec(lines[open] ?? "")?.[1];
  if (name === undefined)
    return [`${what} is followed by ${shown(path)}:${close + 1}, a brace that closes no function`];
  if (seen.has(name)) return [];
  const named = new RegExp(`(^|[^A-Za-z0-9_])${name}([^A-Za-z0-9_]|$)`);
  const wrong: string[] = [];
  let calls = 0;
  for (const caller of scripts.filter((other) => sources.get(other)?.includes(path))) {
    const callerLines = (texts.get(caller) ?? "").split("\n");
    callerLines.forEach((line, index) => {
      if (!code(line) || opens.test(line) || !named.test(line)) return;
      calls++;
      const where = `${shown(caller)}:${index + 1}`;
      if (
        !new RegExp(`^\\s*${name}(\\s|$)`).test(line) ||
        controlOperator.test(commandAt(callerLines, index))
      ) {
        wrong.push(
          `${what}, last in ${name}(), which ${where} calls inside another command: ${line.trim()}`
        );
        return;
      }
      wrong.push(
        ...runsAfter(
          texts,
          caller,
          index,
          `${what}, last in ${name}() called at ${where},`,
          new Set([...seen, name])
        )
      );
    });
  }
  if (calls === 0) wrong.push(`${what} is last in ${name}(), which no script calls`);
  return wrong;
}

/** Each call of run_end in TEXTS, each script's text by its path, that is not alone on its line or
 * that has more run after it in its shell (runsAfter), and each line that sets the mark by hand. */
function runEndsWithMoreAfter(texts: Map<string, string>): string[] {
  const wrong: string[] = [];
  for (const path of scripts.filter((path) => path !== lib)) {
    (texts.get(path) ?? "").split("\n").forEach((line, index) => {
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
      wrong.push(
        ...runsAfter(texts, path, index, `${shown(path)}:${index + 1} run_end`, new Set())
      );
    });
  }
  return wrong;
}

describe("every script under scripts/e2e", () => {
  test("prints PASS from an EXIT trap only through run_verdict, from a file that sources the lib", () => {
    const wrong: string[] = [];
    for (const path of scripts) {
      for (const { command, fn } of exitTraps(path)) {
        if (/\bPASS\b/.test(command)) wrong.push(`${shown(path)}: trap '${command}' prints PASS`);
        if (fn === undefined) continue;
        for (const printed of printsPass(fn.body)) {
          wrong.push(`${shown(fn.file)}: ${fn.name} prints PASS itself: ${printed}`);
        }
        if (fn.body.includes("run_verdict") && !sources.get(fn.file)?.includes(lib)) {
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
          (other) => dirname(other) === e2e && sources.get(other)?.includes(fn.file)
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
    expect(runEndsWithMoreAfter(text)).toEqual([]);
  });

  // controller-sweep-kind.sh's left() runs after the sweep's driver and after the negative
  // control's: a run_end last in it is followed by its closing brace, and then by more of the
  // script each time, so a TERM during the negative control's driver would print PASS.
  test("refuses a run_end last in a function the script calls before more of it runs", () => {
    const sweep = join(e2e, "controller-sweep-kind.sh");
    const original = text.get(sweep) ?? "";
    const moved = original
      .replace(/\nrun_end\n$/, "\n")
      .replace(/(\nleft\(\) \{\n[\s\S]*?\n)\}\n/, "$1  run_end\n}\n");
    expect(moved.match(/^\s*run_end$/gm)).toEqual(["  run_end"]);
    expect(moved).toContain("\n  run_end\n}\n");
    const wrong = runEndsWithMoreAfter(new Map([...text, [sweep, moved]]));
    expect(wrong).toHaveLength(2);
    for (const line of wrong) {
      expect(line).toMatch(
        /^controller-sweep-kind\.sh:\d+ run_end, last in left\(\) called at controller-sweep-kind\.sh:\d+, runs more after it: controller-sweep-kind\.sh:\d+ pass$/
      );
    }
  });

  // busy-relaunch-delivery.sh's negative_control() ends in its run_end, and the script's last
  // line of code calls it. A call followed on its own line by a control operator runs more after
  // it there, so a run_end last in the function then has more run after it.
  test("refuses a run_end last in a function whose call runs more after it on its line", () => {
    const busy = join(e2e, "busy-relaunch-delivery.sh");
    const original = text.get(busy) ?? "";
    const chained = original.replace(
      /\n {2}negative_control\nfi\n$/,
      "\n  negative_control && note x\nfi\n"
    );
    expect(chained).not.toBe(original);
    expect(runEndsWithMoreAfter(new Map([...text, [busy, chained]]))).toEqual([
      expect.stringMatching(
        /^busy-relaunch-delivery\.sh:\d+ run_end, last in negative_control\(\), which busy-relaunch-delivery\.sh:\d+ calls inside another command: negative_control && note x$/
      ),
    ]);
  });

  // An `else` branch does not run after a run_end in the branch before it, up to its `if`'s own
  // `fi`, and a command after that `fi` does. The reader counts each `if` the branch opens and
  // each it closes: a one-line `if …; then …; fi` closes on its own line, whatever follows its
  // `fi` there, and a `fi` before a comment closes its `if`.
  test.each([
    ["a one-line if", "  if false; then note x; fi", "fi"],
    ["a one-line if with more after its fi", "  if false; then note x; fi; note y", "fi"],
    ["a fi before a comment", "  note x", "fi # the else branch"],
  ])("reads the command after an if whose else branch holds %s", (_, branch, close) => {
    const sweep = join(e2e, "controller-sweep-kind.sh");
    const original = text.get(sweep) ?? "";
    const branched = original.replace(
      /\nrun_end\n$/,
      `\nif true; then\n  run_end\nelse\n${branch}\n${close}\nnote after\n`
    );
    expect(branched).not.toBe(original);
    expect(runEndsWithMoreAfter(new Map([...text, [sweep, branched]]))).toEqual([
      expect.stringMatching(
        /^controller-sweep-kind\.sh:\d+ run_end runs more after it: controller-sweep-kind\.sh:\d+ note after$/
      ),
    ]);
  });
});

// The two readers of an EXIT trap read every command of a line (commandsOf), not its start alone:
// a trap or an echo that an operator joins to another command runs all the same, and one in a
// comment or in quoted text does not run.
describe("the readers of an EXIT trap read every command of a line", () => {
  test.each([
    ["alone on its line", "trap cleanup EXIT", ["cleanup"]],
    ["with a command after it", "trap 'cleanup 1' EXIT; note x", ["cleanup 1"]],
    ["after a condition", '[ -n "$dir" ] && trap cleanup EXIT', ["cleanup"]],
    ["before an ||", "trap cleanup EXIT || true", ["cleanup"]],
    ["after a pipe", "true | trap cleanup EXIT", ["cleanup"]],
    ["after a command sent to the background", "sleep 1 & trap cleanup EXIT", ["cleanup"]],
    ["in a one-line if", "if true; then trap cleanup EXIT; fi", ["cleanup"]],
    ["in quoted text", "note 'trap cleanup EXIT; x'", []],
    ["in a comment", "note x # trap cleanup EXIT", []],
  ])("reads a trap %s", (_, script, commands) => {
    expect(exitTrapCommands(script)).toEqual(commands);
  });

  // Each row is a function's whole definition, as scriptFunctions gives it.
  const within = (line: string) => `cleanup() {\n  ${line}\n}`;
  test.each([
    ["after a condition", within('[ "$rc" -eq 0 ] && echo PASS'), "echo PASS"],
    ["after another command", within("note done; printf '%s\\n' PASS"), "printf '%s\\n' PASS"],
    [
      "in a one-line if",
      within('if [ "$rc" -eq 0 ]; then echo "e2e: PASS"; fi'),
      'echo "e2e: PASS"',
    ],
    ["in a one-line function", "cleanup() { echo PASS; }", "echo PASS"],
  ])("finds an echo of PASS %s", (_, definition, printed) => {
    expect(printsPass(definition)).toEqual([printed]);
  });

  test.each([
    ["in a comment", within("note x # echo PASS")],
    ["in quoted text", within("note 'x; echo PASS'")],
  ])("finds no echo of PASS %s", (_, definition) => {
    expect(printsPass(definition)).toEqual([]);
  });
});
