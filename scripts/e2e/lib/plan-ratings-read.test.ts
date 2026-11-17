import { beforeAll, describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// plan-ratings-read.sh (the read of a deployed issue's plan and ratings after the merge) against a
// fake `dispatch` on PATH: it logs its arguments, prints FAKE_DISPATCH_OUT as the real command's
// answer would print, and exits FAKE_DISPATCH_EXIT. The answers below are the lines `dispatch
// plans` prints (packages/envoy-client/src/dispatch-commands/plans.ts): a header, then one
// finished issue per line, newest close first.
const script = join(import.meta.dir, "..", "plan-ratings-read.sh");
const dir = temporaryDirectory("plan-ratings-read-test");
beforeAll(() => {
  writeFileSync(
    join(dir, "dispatch"),
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"$FAKE_DISPATCH_LOG"
printf '%s' "$FAKE_DISPATCH_OUT"
exit "\${FAKE_DISPATCH_EXIT:-0}"
`,
    { mode: 0o755 }
  );
});

const project = "INFRA";
const key = "INFRA-42";
const plan = "plan default (1 tasks: planner implementer tester reviewer acceptance:owner)";
const line42 = `${key} closed 2026-11-20T11:20:00.123456Z ${plan} ratings 4/5 "The plan fit the work; the review sent it back once." time 2h20m failed steps 1`;
// A whole answer: the issue rated once, an issue rated twice whose second reason holds a quotation
// mark and a line break, and an issue finished before the release that rates each plan, which
// lists no plan, no rating and no time.
const whole = [
  `${project}: 3 finished issues`,
  `INFRA-43 closed 2026-11-20T12:00:00Z ${plan} ratings 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped." time 0h55m failed steps 0`,
  line42,
  "INFRA-7 closed 2026-11-01T08:00:00Z plan none ratings none time - failed steps 0",
];
// changed is whole with the line that matches pick replaced by its edit.
function changed(pick: RegExp, edit: (line: string) => string): string {
  return whole.map((line) => (pick.test(line) ? edit(line) : line)).join("\n") + "\n";
}

let runs = 0;
function read(out: string, args: string[] = [project, key], exit = 0) {
  const log = join(dir, `calls-${++runs}`);
  writeFileSync(log, "");
  const result = Bun.spawnSync(["bash", script, ...args], {
    env: {
      PATH: `${dir}:/usr/bin:/bin`,
      HOME: dir,
      FAKE_DISPATCH_LOG: log,
      FAKE_DISPATCH_OUT: out,
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

describe("plan-ratings-read.sh", () => {
  test("a whole answer passes, exit 0, read once with dispatch plans --project, naming the rating and its reason", () => {
    const { code, stdout, calls } = read(whole.join("\n") + "\n");
    expect(calls).toBe(`plans --project ${project}\n`);
    expect(stdout).toContain(whole[0]);
    expect(stdout).toContain(
      `PASS: ${key}: plan default (1 tasks: planner implementer tester reviewer acceptance:owner), ratings 4/5 "The plan fit the work; the review sent it back once.", time 2h20m, failed steps 1`
    );
    expect(code).toBe(0);
  });

  test("an issue rated twice, a reason with a quotation mark and a line break among them, passes", () => {
    const { code, stdout } = read(whole.join("\n") + "\n", [project, "INFRA-43"]);
    expect(stdout).toContain(
      `PASS: INFRA-43: ${plan}, ratings 2/5 "The plan had no test step.", 5/5 "It said \\"ship it\\"\\nand it shipped.", time 0h55m, failed steps 0`
    );
    expect(code).toBe(0);
  });

  test("an issue with no plan, no rating and no time fails, exit 1, one line per fault", () => {
    const { code, stdout } = read(whole.join("\n") + "\n", [project, "INFRA-7"]);
    expect(stdout).toContain("FAIL: INFRA-7 lists no plan: INFRA-7 closed");
    expect(stdout).toContain("FAIL: INFRA-7 lists no rating: INFRA-7 closed");
    expect(stdout).toContain("FAIL: INFRA-7 lists no time: INFRA-7 closed");
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a plan with no rating fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/^INFRA-42 /, (line) => line.replace(/ratings .* time/, "ratings none time"))
    );
    expect(stdout).toContain(`FAIL: ${key} lists no rating: ${key} closed`);
    expect(stdout).not.toContain("lists no plan");
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a rating with an empty reason fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/^INFRA-42 /, (line) => line.replace(/4\/5 "[^"]*"/, '4/5 ""'))
    );
    expect(stdout).toContain(`FAIL: ${key} lists a rating with no reason: ${key} closed`);
    expect(code).toBe(1);
  });

  test("a rating with no time fails, exit 1, naming it", () => {
    const { code, stdout } = read(
      changed(/^INFRA-42 /, (line) => line.replace("time 2h20m", "time -"))
    );
    expect(stdout).toContain(`FAIL: ${key} lists no time: ${key} closed`);
    expect(code).toBe(1);
  });

  test("a read that lists finished issues and not the issue fails, exit 1, naming it", () => {
    const lines = whole.slice(1).filter((line) => !line.startsWith(`${key} `));
    const { code, stdout } = read(
      [`${project}: ${lines.length} finished issues`, ...lines].join("\n") + "\n"
    );
    expect(stdout).toContain(
      `FAIL: the read lists 2 finished issues of ${project} and no line of ${key}`
    );
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(1);
  });

  test("a read that lists no finished issue at all is UNKNOWN, exit 2, never a pass", () => {
    const { code, stdout } = read(`${project}: 0 finished issues\n`);
    expect(stdout).toContain(`UNKNOWN: dispatch plans lists no finished issue of ${project}`);
    expect(stdout).not.toContain("PASS");
    expect(code).toBe(2);
  });

  test("a read that fails is UNKNOWN, exit 2, with what the command said", () => {
    const { code, stdout } = read(
      "Dispatch at https://dispatch.example is unreachable: ECONNREFUSED\n",
      [project, key],
      1
    );
    expect(stdout).toContain(
      `UNKNOWN: dispatch plans --project ${project} exited 1, so nothing was read`
    );
    expect(stdout).toContain("is unreachable: ECONNREFUSED");
    expect(code).toBe(2);
  });

  test("an answer that is no read of the project is UNKNOWN, exit 2", () => {
    const header = read("OTHER: 3 finished issues\n" + whole.slice(1).join("\n") + "\n");
    expect(header.stdout).toContain(
      `UNKNOWN: the first line of the answer is no header of ${project}`
    );
    expect(header.code).toBe(2);
    const short = read(`${project}: 4 finished issues\n` + whole.slice(1).join("\n") + "\n");
    expect(short.stdout).toContain(
      "UNKNOWN: the header says 4 finished issues and 3 lines of issues follow it"
    );
    expect(short.code).toBe(2);
    const torn = read(
      changed(/^INFRA-43 /, (line) => line.replace(/, 5\/5 .*" time/, ', 5/5 "torn time'))
    );
    expect(torn.stdout).toContain("UNKNOWN: the line is no finished issue: INFRA-43 closed");
    expect(torn.stdout).not.toContain("PASS");
    expect(torn.code).toBe(2);
  });

  test("a wrong count of arguments, a malformed key or an issue of another project is refused, exit 2, reading nothing", () => {
    for (const args of [
      [],
      [project],
      [project, key, "INFRA-43"],
      ["infra", key],
      [project, "infra-42"],
      [project, "OTHER-42"],
    ]) {
      const { code, stderr, calls } = read(whole.join("\n") + "\n", args);
      expect(stderr).toContain("usage: scripts/e2e/plan-ratings-read.sh <PROJECT> <ISSUE>");
      expect(calls).toBe("");
      expect(code).toBe(2);
    }
  });
});
