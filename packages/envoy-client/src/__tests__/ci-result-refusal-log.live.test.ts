import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  type CIResultsStack,
  startCIResultsStack,
  useCIResultsStackDirectory,
  writeProbeSpec,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";

// Red test of the t-ci-2 acceptance run 2 at db6769f732 (F-A): a CI job's own `dispatch result
// --project` prints Dispatch's refusal of a spec tree with the name of a spec file as git holds
// it. A name with a line break then starts a line of its own in the job's log, and a line that
// starts with "::" is a workflow command to a GitHub runner. The file name is the commit
// author's text, as runnerlog.Printable says of a file name in a pull request.

const { PATH: SHELL_PATH = "" } = process.env;
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const stackDirectory = useCIResultsStackDirectory();
const work = temporaryDirectory("acceptance-tci2-run2-red");
/** The name of a spec file whose second line is a workflow command. */
const INJECTED = "x\n::error title=injected::boom.md";

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

describe.skipIf(!dispatchStackEnabled)("a CI job's refusal, as its log holds it", () => {
  let stack: CIResultsStack | undefined;
  const cliState = join(work, "cli-state");

  beforeAll(async () => {
    mkdirSync(cliState, { recursive: true });
    stack = await startCIResultsStack({ work: stackDirectory });
  }, 600_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("no line that a refused result write prints is a workflow command, whatever a spec file's name holds", async () => {
    if (stack === undefined) throw new Error("the CI results stack did not start");
    const dir = join(work, "checkout");
    const spec = join(dir, ".dispatch", "spec");
    mkdirSync(spec, { recursive: true });
    writeProbeSpec(spec);
    writeFileSync(
      join(spec, INJECTED),
      ':::requirement{#a-lonely-statement kind="behavior"}\nThe part gives its answer.\n:::\n'
    );
    git(dir, "init", "-q");
    git(dir, "add", "-A");
    // commit-tree, as a commit's own hooks may not read a path with a line break.
    const tree = git(dir, "write-tree").trim();
    const sha = git(
      dir,
      "commit-tree",
      tree,
      "-m",
      "a spec file whose name holds a line break"
    ).trim();
    stack.setFiles(sha, spec);
    const env = stack.jobEnv({
      sha,
      ref: "refs/heads/topic/inject",
      job: "go-checks",
      runAttempt: 1,
      checkRunId: 4301,
    });
    const child = Bun.spawn(
      [
        stack.agentSecrets,
        "result-grant",
        "--",
        process.execPath,
        ENTRY,
        "result",
        "--project",
        stack.project,
        "--check",
        "probe-passes",
        "--verdict",
        "pass",
        "--commit",
        sha,
      ],
      {
        env: { PATH: SHELL_PATH, HOME: cliState, DISPATCH_STATE_DIR: cliState, ...env },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    const [stdout, stderr, code] = await Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ]);
    const lines = `${stdout}${stderr}`.split(/\r|\n/);
    expect({
      code,
      refused: stdout.includes("SPEC_FILES_REFUSED"),
      workflowCommands: lines.filter(
        (line) => line.trimStart().startsWith("::") || line.includes("##[")
      ),
    }).toEqual({ code: 1, refused: true, workflowCommands: [] });
  }, 180_000);
});
