import { expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// A proof script writes the run's legion.yaml in write_legion_config through an unquoted heredoc,
// so that the script's variables expand in it; a command substitution in one of its comments then
// runs too. Backticks in two comments did. parked-decision-devbox.sh's `legion` ran the first legion
// on PATH and wrote what it printed into the file: its first line into the comment, every later line
// as YAML. stage3-devbox-workflow.sh's `production_check: tester` ran a command of that name, so the
// comment lost those words and bash wrote "command not found". Each row runs the function under its
// script's own `set` line, with a stand-in first on PATH for the command the comment named that
// records each call and prints two lines, and holds the file to the comment as written, to no call
// and to an empty stderr.
const e2e = join(import.meta.dir, "..");
const dir = temporaryDirectory("legion-yaml-comments-test");
const cases = [
  {
    script: "parked-decision-devbox.sh",
    command: "legion",
    comment:
      '# Four: an issue holds one of the cap\'s slots as "Admission and the `legion` label" in',
  },
  {
    script: "stage3-devbox-workflow.sh",
    command: "production_check:",
    comment:
      "# default repository in Dispatch. `production_check: tester` is the word that names the role a merge's production check",
  },
];

for (const { script, command, comment } of cases) {
  test(`${script}'s write_legion_config writes its comments as written and runs no command one names`, () => {
    const scriptPath = join(e2e, script);
    const fn = scriptFunctions(scriptPath);
    const set = readFileSync(scriptPath, "utf8").match(/^set -.*$/m)?.[0];
    if (set === undefined) throw new Error(`${scriptPath} has no top-level set line`);
    const work = join(dir, script, "work");
    const bin = join(dir, script, "bin");
    mkdirSync(work, { recursive: true });
    mkdirSync(bin, { recursive: true });
    writeFileSync(join(work, "postgres-password"), "password");
    writeFileSync(join(work, "github-apps.yaml"), "github_apps: {}\n");
    const calls = join(dir, script, "calls");
    writeFileSync(
      join(bin, command),
      `#!/bin/sh\necho "$*" >>'${calls}'\nprintf 'ran\\nstray: line\\n'\n`,
      { mode: 0o755 }
    );
    const ran = Bun.spawnSync(
      ["bash", "-c", `${set}\n${fn("write_legion_config")}\nwrite_legion_config`],
      {
        env: {
          PATH: `${bin}:${process.env.PATH ?? ""}`,
          work,
          project: "proj",
          repo: "owner/repo",
          port_daemon: "1",
          port_pg: "2",
          port_listener: "3",
          port_nats: "4",
          port_dispatch: "5",
          state: join(work, "state"),
          pin: "omp@1",
        },
        stdout: "pipe",
        stderr: "pipe",
      }
    );
    expect({ code: ran.exitCode, stderr: ran.stderr.toString() }).toEqual({ code: 0, stderr: "" });
    expect(existsSync(calls)).toBe(false);
    const written = readFileSync(join(work, "legion.yaml"), "utf8");
    expect(written.split("\n")).toContain(comment);
    expect(written).not.toContain("stray: line");
  });
}
