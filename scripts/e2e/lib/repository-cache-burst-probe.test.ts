import { expect } from "bun:test";
import { join } from "node:path";
import { hostGate } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// scripts/e2e/repository-cache-burst-probe.sh is the probe the repository cache's burst Job runs
// (repository-cache-burst-job.sh), and the Job runs only in a cluster. Its `self-test` holds, with
// no Job, every check that needs no cluster: the cgroup parser against its fixtures, the counter
// reads, the manifest check, the verdict against synthetic series and the leak scan. This file runs
// that self-test as a reader runs it, a bash process of its own, with a TMPDIR of this file's, so
// a change to the probe that breaks one of its rows fails here.
const probe = join(import.meta.dir, "..", "repository-cache-burst-probe.sh");
const scratch = temporaryDirectory("repository-cache-burst-probe");
// The self-test needs bash 4 or later, GNU coreutils, jq and awk: outside CI a host without one
// skips the row, naming it, and under GITHUB_ACTIONS the row fails by its own name (hostGate).
const host = hostGate({ programs: ["bash", "jq", "awk"] });

host.test("the probe's self-test holds every row and ends SELF-TEST pass", () => {
  const run = Bun.spawnSync(["bash", probe, "self-test"], {
    env: { PATH: process.env.PATH ?? "", TMPDIR: scratch },
    stdout: "pipe",
    stderr: "pipe",
  });
  const lines = run.stdout.toString().trimEnd().split("\n");
  expect({
    exitCode: run.exitCode,
    failed: lines.filter((line) => line.startsWith("FAIL ")),
    last: lines.at(-1),
  }).toEqual({ exitCode: 0, failed: [], last: "SELF-TEST pass" });
});
