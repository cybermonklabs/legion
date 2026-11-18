import { beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// step-records-read.sh (the read of a deployed issue's step records after the merge) against a
// fake `dispatch` on PATH: it logs its arguments, prints FAKE_DISPATCH_ERR on stderr, as the real
// command prints its `dispatch: <warning>` lines before its answer, then FAKE_DISPATCH_OUT as the
// real command's answer would print, and exits FAKE_DISPATCH_EXIT. The answers below are the lines
// `dispatch steps` prints (packages/envoy-client/src/dispatch-commands/steps.ts): a header, then one
// run per line.
const script = join(import.meta.dir, "..", "step-records-read.sh");
const dir = temporaryDirectory("step-records-read-test");
beforeAll(() => {
  writeFileSync(
    join(dir, "dispatch"),
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$FAKE_DISPATCH_LOG"
printf '%s' "$FAKE_DISPATCH_ERR" >&2
printf '%s' "$FAKE_DISPATCH_OUT"
exit "\${FAKE_DISPATCH_EXIT:-0}"
`,
    { mode: 0o755 }
  );
});

const key = "INFRA-42";
const run = (step: string) =>
  `default ${step} legion-infra-infra-42-${step.split(" ")[2]} anthropic/claude-opus-5 2026-11-20T09:00:00Z 2026-11-20T09:40:00Z passed`;
// A whole answer: steps 1 to 5, a review that failed and the runs it sent back, the retro and the
// merge, each ended, each with an agent, each step with a model.
const whole = [
  `${key}: 10 steps in 1 tasks`,
  run("1 step planner planning attempt 1"),
  run("2 step implementer implementation attempt 1"),
  run("3 step tester testing attempt 1"),
  "default 4 step reviewer review attempt 1 legion-infra-infra-42-reviewer openai/gpt-6.1-sol 2026-11-20T10:00:00Z 2026-11-20T10:30:00Z failed",
  run("2 step implementer implementation attempt 2"),
  run("3 step tester testing attempt 2"),
  run("4 step reviewer review attempt 2"),
  run("5 step acceptance review attempt 1"),
  "default 0 retro implementer - attempt 1 legion-infra-infra-42-implementer anthropic/claude-opus-5 2026-11-20T11:00:00Z 2026-11-20T11:10:00Z passed",
  "default 0 merge merger - attempt 1 legion-infra-infra-42-merger - 2026-11-20T11:11:00Z 2026-11-20T11:20:00Z passed",
];
// answer is whole with the lines that match drop taken out, the header's count following.
function without(drop: RegExp): string {
  const lines = whole.slice(1).filter((line) => !drop.test(line));
  return `${[`${key}: ${lines.length} steps in 1 tasks`, ...lines].join("\n")}\n`;
}
// changed is whole with the line that matches pick replaced by its edit.
function changed(pick: RegExp, edit: (line: string) => string): string {
  return `${whole.map((line) => (pick.test(line) ? edit(line) : line)).join("\n")}\n`;
}

// bash is the shell the script runs under, found once where read's own PATH finds it, so a case
// that gives the script a PATH with no bash on it still runs the same shell.
const bash = Bun.which("bash", { PATH: "/usr/bin:/bin" });
if (bash === null) throw new Error("no bash in /usr/bin or /bin");
let runs = 0;
// read runs the script with args against the fake dispatch, which prints err on stderr and out on
// stdout and exits exit, on path, which by default finds the fake first.
function read(
  out: string,
  args: string[] = [key],
  exit = 0,
  path = `${dir}:/usr/bin:/bin`,
  err = ""
) {
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
}

describe("step-records-read.sh", () => {
  test("a whole answer passes, exit 0, read once with dispatch steps --issue", () => {
    const { code, stdout, calls } = read(`${whole.join("\n")}\n`);
    expect(calls).toBe(`steps --issue ${key}\n`);
    expect(stdout).toContain(whole[0]);
    expect(stdout).toContain(
      `PASS: ${key}: 10 records; steps 1 to 5, the retro and the merge each have one, each record ended, each step and retro with an agent, each step with a model`
    );
    expect(code).toBe(0);
  });

  test("a warning the command prints on stderr is no part of its answer: the read passes", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout, stderr } = read(`${whole.join("\n")}\n`, [key], 0, undefined, warning);
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(stderr).toContain(warning.trim());
    expect(code).toBe(0);
  });

  test("an answer with the merge record missing fails, exit 1, naming it", () => {
    const { code, stdout } = read(without(/ merge merger /));
    expect(stdout).toContain(`FAIL: ${key}: no record of the merge`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("an answer with no record at all is UNKNOWN, exit 2, never a pass", () => {
    const { code, stdout } = read(`${key}: 0 steps in 0 tasks\n`);
    expect(stdout).toContain(`UNKNOWN: dispatch steps lists no record of ${key}`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("each step of kind step must be there: a missing step 3 fails, exit 1", () => {
    const { code, stdout } = read(without(/ 3 step tester /));
    expect(stdout).toContain(`FAIL: ${key}: no record of step 3`);
    expect(code).toBe(1);
  });

  test("a record still running fails, exit 1, naming its line", () => {
    const { code, stdout } = read(
      changed(/ 5 step acceptance /, (line) => line.replace(/ \S+ passed$/, " running running"))
    );
    expect(stdout).toContain(
      "FAIL: the record never ended: default 5 step acceptance review attempt 1"
    );
    expect(code).toBe(1);
  });

  test("an end that is no moment, `-` or any word, fails, exit 1: only a moment ends a record", () => {
    // steps.ts prints `running` there for a run that has not ended; a printer that wrote the end
    // through orDash would print `-`, and another a word of its own: none of them is an end.
    for (const end of ["-", "unknown"]) {
      const { code, stdout } = read(
        changed(/ 2 step implementer implementation attempt 2 /, (line) =>
          line.replace(/ \S+ passed$/, ` ${end} passed`)
        )
      );
      expect(stdout).toContain(
        `FAIL: the record never ended: default 2 step implementer implementation attempt 2 legion-infra-infra-42-implementer anthropic/claude-opus-5 2026-11-20T09:00:00Z ${end} passed`
      );
      expect(stdout).not.toContain("PASS");
      expect(code).toBe(1);
    }
  });

  test("a retro with no agent fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/ retro implementer /, (line) =>
        line.replace("legion-infra-infra-42-implementer", "-")
      )
    );
    expect(stdout).toContain("FAIL: the record names no agent: default 0 retro implementer");
    expect(code).toBe(1);
  });

  test("a step with no agent fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/ 3 step tester testing attempt 2 /, (line) =>
        line.replace("legion-infra-infra-42-tester", "-")
      )
    );
    expect(stdout).toContain(
      "FAIL: the record names no agent: default 3 step tester testing attempt 2 - anthropic/claude-opus-5"
    );
    expect(code).toBe(1);
  });

  test("a merge with no agent passes, with a line that says no merger ran it", () => {
    const { code, stdout } = read(
      changed(/ merge merger /, (line) => line.replace("legion-infra-infra-42-merger", "-"))
    );
    expect(stdout).toContain(
      "the merge has no agent: no merger ran it: default 0 merge merger - attempt 1 - -"
    );
    expect(stdout).not.toContain("FAIL");
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(code).toBe(0);
  });

  test("a merge with an agent is checked as every record is: one never ended fails, exit 1", () => {
    const { code, stdout } = read(
      changed(/ merge merger /, (line) => line.replace(/ \S+ passed$/, " running running"))
    );
    expect(stdout).toContain("FAIL: the record never ended: default 0 merge merger");
    expect(stdout).not.toContain("the merge has no agent");
    expect(code).toBe(1);
  });

  test("a merge with no agent that never ended fails, exit 1: no agent waives no end", () => {
    const { code, stdout } = read(
      changed(/ merge merger /, (line) =>
        line
          .replace("legion-infra-infra-42-merger", "-")
          .replace(/ \S+ passed$/, " running running")
      )
    );
    expect(stdout).toContain(
      "FAIL: the record never ended: default 0 merge merger - attempt 1 - - 2026-11-20T11:11:00Z running running"
    );
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a step of kind step with no model fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/ 1 step planner /, (line) => line.replace("anthropic/claude-opus-5", "-"))
    );
    expect(stdout).toContain("FAIL: the step names no model: default 1 step planner planning");
    expect(code).toBe(1);
  });

  test("a retro with no model passes, exit 0: only a record of kind step must name one", () => {
    const { code, stdout } = read(
      changed(/ retro implementer /, (line) => line.replace("anthropic/claude-opus-5", "-"))
    );
    expect(stdout).toContain(
      "default 0 retro implementer - attempt 1 legion-infra-infra-42-implementer - "
    );
    expect(stdout).not.toContain("FAIL");
    expect(stdout).toContain(`PASS: ${key}: 10 records`);
    expect(code).toBe(0);
  });

  test("a read that fails is UNKNOWN, exit 2, with what the command said", () => {
    const { code, stdout } = read(
      "Dispatch at https://dispatch.example is unreachable: ECONNREFUSED\n",
      [key],
      1
    );
    expect(stdout).toContain(
      `UNKNOWN: dispatch steps --issue ${key} exited 1, so nothing was read`
    );
    expect(stdout).toContain("is unreachable: ECONNREFUSED");
    expect(code).toBe(2);
  });

  test("a failed read's message keeps what it said on stderr and on stdout on lines of their own", () => {
    const warning = "dispatch: the bearer token expires in 2 days\n";
    const { code, stdout } = read(`${key}: 7 steps in 1 tasks\n`, [key], 1, undefined, warning);
    expect(stdout).toContain(`exited 1, so nothing was read: ${warning}${key}: 7 steps in 1 tasks`);
    expect(stdout).not.toContain(`days${key}`);
    expect(code).toBe(2);
  });

  test("an answer that is no read of the issue is UNKNOWN, exit 2", () => {
    const header = read("OTHER-1: 10 steps in 1 tasks\n");
    expect(header.stdout).toContain(
      "UNKNOWN: the first line of the answer is no header of INFRA-42"
    );
    expect(header.code).toBe(2);
    const short = read(`${key}: 11 steps in 1 tasks\n${whole.slice(1).join("\n")}\n`);
    expect(short.stdout).toContain(
      "UNKNOWN: the header says 11 records and 10 lines of runs follow it"
    );
    expect(short.code).toBe(2);
  });

  test("an answer with a line that is no run is UNKNOWN, exit 2, even when the count holds", () => {
    const { code, stdout } = read(
      `${key}: 11 steps in 1 tasks\n${whole.slice(1).join("\n")}\nwarning: the server is slow\n`
    );
    expect(stdout).toContain("UNKNOWN: the line is no run: warning: the server is slow");
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("no dispatch command on PATH is UNKNOWN, exit 2, naming what is missing", () => {
    // The PATH the script gets holds no directory: bash is named by the path read found it at.
    const { code, stdout } = read("", [key], 0, join(dir, "no-such-directory"));
    expect(stdout).toContain("UNKNOWN: no dispatch command is on PATH, so nothing was read");
    expect(code).toBe(2);
  });

  test("no issue, two issues or a malformed key is refused, exit 2, reading nothing", () => {
    for (const args of [[], [key, "INFRA-43"], ["infra-42"]]) {
      const { code, stderr, calls } = read(`${whole.join("\n")}\n`, args);
      expect(stderr).toContain("usage: scripts/e2e/step-records-read.sh <ISSUE>");
      expect(calls).toBe("");
      expect(code).toBe(2);
    }
  });
});
