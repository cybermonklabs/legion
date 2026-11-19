import { beforeAll } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

// The `dispatch` a read script's test puts first on PATH: it logs its arguments to
// FAKE_DISPATCH_LOG, prints FAKE_DISPATCH_ERR on stderr, as the real command prints its
// `dispatch: <warning>` lines before its answer, then FAKE_DISPATCH_OUT as the real command's
// answer would print, and exits FAKE_DISPATCH_EXIT.
const fakeDispatch = `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$FAKE_DISPATCH_LOG"
printf '%s' "$FAKE_DISPATCH_ERR" >&2
printf '%s' "$FAKE_DISPATCH_OUT"
exit "\${FAKE_DISPATCH_EXIT:-0}"
`;

// bash is the shell a read script runs under, found once where a run's default PATH finds it, so
// a case that gives the script a PATH with no bash on it still runs the same shell.
const found = Bun.which("bash", { PATH: "/usr/bin:/bin" });
if (found === null) throw new Error("no bash in /usr/bin or /bin");
const bash: string = found;

/** One run of a read script: its exit code, both its streams, and each call of the fake
 *  `dispatch`, its arguments one call to a line. */
export interface ReadRun {
  code: number;
  stdout: string;
  stderr: string;
  calls: string;
}

/**
 * The runs of `script`, a read script of scripts/e2e, against the fake `dispatch` this writes into
 * `dir`. The fake is written in a `beforeAll` registered at this call, so the call comes after the
 * one that makes `dir` (`temporaryDirectory`), whose hook runs first. Each run gives the fake `out`
 * to print on stdout, `err` on stderr and `exit` to exit with, and runs the script with `args`,
 * `defaults` where it names none, on `path`, which by default finds the fake first.
 */
export function fakeDispatchRead(
  script: string,
  dir: string,
  defaults: string[]
): (out: string, args?: string[], exit?: number, path?: string, err?: string) => ReadRun {
  beforeAll(() => {
    writeFileSync(join(dir, "dispatch"), fakeDispatch, { mode: 0o755 });
  });
  let runs = 0;
  return (out, args = defaults, exit = 0, path = `${dir}:/usr/bin:/bin`, err = "") => {
    const log = join(dir, `calls-${++runs}`);
    writeFileSync(log, "");
    const result = Bun.spawnSync([bash, script, ...args], {
      env: {
        PATH: path,
        HOME: dir,
        FAKE_DISPATCH_LOG: log,
        FAKE_DISPATCH_OUT: out,
        FAKE_DISPATCH_ERR: err,
        FAKE_DISPATCH_EXIT: String(exit),
      },
    });
    return {
      code: result.exitCode,
      stdout: result.stdout.toString(),
      stderr: result.stderr.toString(),
      calls: readFileSync(log, "utf8"),
    };
  };
}

/** `lines` with each line that matches `pick` replaced by its `edit`, one to a line and
 *  newline-ended, as the fake `dispatch` prints an answer. */
export function changed(
  lines: readonly string[],
  pick: RegExp,
  edit: (line: string) => string
): string {
  return `${lines.map((line) => (pick.test(line) ? edit(line) : line)).join("\n")}\n`;
}
