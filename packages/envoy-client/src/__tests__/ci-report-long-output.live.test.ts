import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { cpSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  type CIResultsStack,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";
import { buildGoBinaries } from "../../test/envoy-stack";

// The stored text of a test whose output is longer than 64 KiB starts at the output's own start,
// or says before the test's body that its start was cut. The report is the events of a real
// `go test -json` run of a module example.com/acc: TestSeventyK prints "START-MARKER of
// TestSeventyK", 70 lines of 1023 "s", "END-MARKER of TestSeventyK" and fails; TestThreeK prints
// its start marker, 3000 "k" and its end marker and fails, and its stored text is the control,
// which starts at its start whatever the reader keeps of a long output.

const { PATH: SHELL_PATH = "" } = process.env;
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const FIXTURES = join(import.meta.dir, "..", "..", "test", "fixtures", "ci-report-long-output");
const stackDirectory = useCIResultsStackDirectory();
const work = temporaryDirectory("ci-report-long-output");

function git(cwd: string, ...args: string[]): string {
  const ran = Bun.spawnSync(
    [
      "git",
      "-c",
      "user.name=ACME CI",
      "-c",
      "user.email=ci@example.com",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ],
    {
      cwd,
      env: {
        PATH: SHELL_PATH,
        HOME: work,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
      },
    }
  );
  if (ran.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString();
}

/** Whether the stored text says where it starts: it starts with the test's own first line, or the
 *  text before the first character of the test's body holds a note that says it was cut. */
function saysWhereItStarts(stored: string, firstLine: string, bodyChar: string): boolean {
  if (stored.startsWith(firstLine)) return true;
  const body = stored.indexOf(bodyChar.repeat(20));
  return body > 0 && /cut/i.test(stored.slice(0, body));
}

describe.skipIf(!dispatchStackEnabled)("a long output of a test, as Dispatch keeps it", () => {
  let stack: CIResultsStack | undefined;
  const cliState = join(work, "cli-state");

  beforeAll(async () => {
    mkdirSync(cliState, { recursive: true });
    await buildGoBinaries({
      bin: join(work, "bin"),
      packages: ["./cmd/ci-report"],
      stack: "long output of a test",
      binaries: "./cmd/ci-report",
    });
    stack = await startCIResultsStack({ work: stackDirectory });
  }, 600_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("the stored text of an output over 64 KiB starts at the output's start, or says that its start was cut", async () => {
    if (stack === undefined) throw new Error("the CI results stack did not start");
    const dir = join(work, "checkout");
    cpSync(join(FIXTURES, "spec"), join(dir, ".dispatch", "spec"), { recursive: true });
    git(dir, "init", "-q");
    git(dir, "add", "-A");
    git(dir, "commit", "-q", "-m", "long output");
    const sha = git(dir, "rev-parse", "HEAD").trim();
    stack.setFiles(sha, join(dir, ".dispatch", "spec"));
    const env = stack.jobEnv({
      sha,
      ref: "refs/heads/topic/long-output",
      job: "go-checks",
      runAttempt: 1,
      checkRunId: 4201,
    });
    const child = Bun.spawn(
      [
        join(work, "bin", "ci-report"),
        "--dir",
        ".dispatch/spec",
        "--job",
        "go-checks",
        "--report",
        join(FIXTURES, "go-test-long-output.jsonl"),
        "--project",
        stack.project,
        "--dispatch-cli",
        `${process.execPath} ${ENTRY}`,
      ],
      {
        cwd: dir,
        env: {
          PATH: SHELL_PATH,
          HOME: cliState,
          DISPATCH_STATE_DIR: cliState,
          GITHUB_SHA: sha,
          ...env,
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    expect({ code, stderr }).toMatchObject({ code: 0 });
    expect(stdout.split("\n").filter((line) => line !== "").length).toBe(2);

    const rows = (await stack.results(sha)) as unknown as {
      attestation: { predicate: { check: string; verdict: string; observed: string } };
    }[];
    const kept = Object.fromEntries(
      rows.map((row) => [row.attestation.predicate.check, row.attestation.predicate.observed])
    );
    expect({
      "three-k-output": saysWhereItStarts(
        kept["three-k-output"] ?? "",
        "=== RUN   TestThreeK\nSTART-MARKER",
        "k"
      ),
      "seventy-k-output": saysWhereItStarts(
        kept["seventy-k-output"] ?? "",
        "=== RUN   TestSeventyK\nSTART-MARKER",
        "s"
      ),
      seventyKStartsWith: (kept["seventy-k-output"] ?? "").slice(0, 40),
    }).toEqual({
      "three-k-output": true,
      "seventy-k-output": true,
      seventyKStartsWith: expect.any(String),
    });
  }, 180_000);
});
