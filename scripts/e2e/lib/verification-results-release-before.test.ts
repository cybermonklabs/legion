import { beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { memoryRoot, temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { scriptFunctions } from "./script-functions";

// verification-results.sh's release_before_of_head: the release before the change, which
// older-rows' database is written by, is the newest commit of main without the change, read from
// git at each run: on a branch that has not merged, main at the commit it last merged, so a later
// merge of main moves it with no edit of the script; on main after the change merged, and on a
// branch of that main, the commit of main just before the merge. The fixture is an origin whose
// main is m1..m4 and whose branch `feature` forked at m1, added the change's first migration and
// merged m2; and a second origin, `merged`, whose main is m4, a squash of `feature` (this
// repository merges by squash alone), then m5.
const releaseBeforeOfHead = scriptFunctions(join(import.meta.dir, "..", "verification-results.sh"))(
  "release_before_of_head"
);
// The fixture's eight git repositories hold about 380 entries at once (250 files), made and removed
// inside a hook's 5 s: on a disk other programs share, the removal outran that bound (a run on a
// loaded box timed out in its afterAll), so the directory is on a tmpfs where one is (memoryRoot),
// with room for four runs at once.
const dir = temporaryDirectory("verification-results-release-before-test", {
  root: memoryRoot({ need: 4 * 2 ** 20 }),
});
// git with no configuration of the machine's, so a fixture reads the same on every runner.
const gitEnv = {
  PATH: process.env.PATH ?? "/usr/bin:/bin",
  HOME: dir,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_NOSYSTEM: "1",
  GIT_AUTHOR_NAME: "fixture",
  GIT_AUTHOR_EMAIL: "fixture@example.com",
  GIT_COMMITTER_NAME: "fixture",
  GIT_COMMITTER_EMAIL: "fixture@example.com",
};

function git(cwd: string, ...args: string[]): string {
  const run = Bun.spawnSync(["git", ...args], { cwd, env: gitEnv });
  if (run.exitCode !== 0) {
    throw new Error(`git ${args.join(" ")} in ${cwd}: ${run.stderr.toString()}`);
  }
  return run.stdout.toString().trim();
}

function commit(cwd: string, name: string, path = `${name}.txt`): string {
  Bun.spawnSync(
    ["bash", "-c", `mkdir -p "$(dirname "$2")" && printf '%s\\n' "$1" > "$2"`, "_", name, path],
    { cwd }
  );
  git(cwd, "add", path);
  git(cwd, "commit", "-q", "-m", name);
  return git(cwd, "rev-parse", "HEAD");
}

// The change's first migration, whose commit on main's first-parent line the release before is the
// parent of once the change has merged.
const changeMigration =
  "packages/envoy/internal/dispatch/store/migrations/20261117111040_pull_request_texts.up.sql";

// Each step runs git, which a loaded machine can take seconds over; bun's 5 s default is no bound
// of what these tests check.
const gitTimeoutMs = 60_000;
let origin = "";
let url = "";
let mergedUrl = "";
let m2 = "";
let m3 = "";
let m4 = "";
beforeAll(() => {
  origin = join(dir, "origin");
  mkdirSync(origin);
  git(origin, "init", "-q", "-b", "main");
  // A file:// origin answers a fetch by commit and a filtered clone as GitHub does.
  git(origin, "config", "uploadpack.allowFilter", "true");
  git(origin, "config", "uploadpack.allowAnySHA1InWant", "true");
  const m1 = commit(origin, "m1");
  m2 = commit(origin, "m2");
  git(origin, "checkout", "-q", "-b", "feature", m1);
  commit(origin, "f1");
  commit(origin, "the change's first migration", changeMigration);
  git(origin, "merge", "-q", "--no-edit", m2);
  git(origin, "checkout", "-q", "main");
  m3 = commit(origin, "m3");
  m4 = commit(origin, "m4");
  url = `file://${origin}`;
  // main after the change merged, as a squash commit, the one shape this repository merges by, and
  // a commit of main after that.
  const merged = join(dir, "merged-origin");
  git(dir, "clone", "-q", "--branch", "main", url, merged);
  git(merged, "config", "uploadpack.allowFilter", "true");
  git(merged, "config", "uploadpack.allowAnySHA1InWant", "true");
  git(merged, "merge", "-q", "--squash", "origin/feature");
  git(merged, "commit", "-q", "-m", "the change, squashed");
  commit(merged, "m5");
  mergedUrl = `file://${merged}`;
}, gitTimeoutMs);

/**
 * Runs release_before_of_head over checkout with a repository of its own: stdout and status. Lazy
 * fetching is off (GIT_NO_LAZY_FETCH), so a shallow checkout's HEAD reaches that repository only
 * by the function's own fetch, and no step stands on objects git would fetch one at a time.
 */
function releaseBefore(checkout: string, name: string): { out: string; code: number } {
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -euo pipefail\n${releaseBeforeOfHead}\nrelease_before_of_head "$1" "$2"`,
      "_",
      checkout,
      join(dir, `${name}.git`),
    ],
    { env: { ...gitEnv, GIT_NO_LAZY_FETCH: "1" } }
  );
  return { out: run.stdout.toString().trim(), code: run.exitCode };
}

describe("release_before_of_head", () => {
  test(
    "a whole checkout of the branch: the commit of main it last merged, not main's tip",
    () => {
      const checkout = join(dir, "whole");
      git(dir, "clone", "-q", "--branch", "feature", url, checkout);
      expect(releaseBefore(checkout, "whole-history")).toEqual({ out: m2, code: 0 });
      expect(m2).not.toBe(m4);
    },
    gitTimeoutMs
  );

  test(
    "a later merge of main, not yet pushed, moves it with no edit",
    () => {
      const checkout = join(dir, "merged");
      git(dir, "clone", "-q", "--branch", "feature", url, checkout);
      git(checkout, "merge", "-q", "--no-edit", m3);
      expect(releaseBefore(checkout, "merged-history")).toEqual({ out: m3, code: 0 });
    },
    gitTimeoutMs
  );

  test(
    "a shallow checkout, as CI's is: HEAD's history is fetched from origin",
    () => {
      const checkout = join(dir, "shallow");
      git(dir, "clone", "-q", "--depth=1", "--branch", "feature", url, checkout);
      expect(git(checkout, "rev-parse", "--is-shallow-repository")).toBe("true");
      expect(releaseBefore(checkout, "shallow-history")).toEqual({ out: m2, code: 0 });
    },
    gitTimeoutMs
  );

  test(
    "a checkout that names no origin prints nothing and fails",
    () => {
      const checkout = join(dir, "no-origin");
      git(dir, "clone", "-q", "--branch", "feature", url, checkout);
      git(checkout, "remote", "remove", "origin");
      const run = releaseBefore(checkout, "no-origin-history");
      expect(run.out).toBe("");
      expect(run.code).not.toBe(0);
    },
    gitTimeoutMs
  );

  test(
    "a checkout of main after the change merged, shallow as main's nightly run is: the commit of main before the merge",
    () => {
      const checkout = join(dir, "after-merge-main");
      git(dir, "clone", "-q", "--depth=1", "--branch", "main", mergedUrl, checkout);
      expect(git(checkout, "rev-parse", "--is-shallow-repository")).toBe("true");
      expect(releaseBefore(checkout, "after-merge-main-history")).toEqual({ out: m4, code: 0 });
    },
    gitTimeoutMs
  );

  test(
    "a branch of main after the change merged: the commit of main before the merge, not its merge base",
    () => {
      const checkout = join(dir, "after-merge-branch");
      git(dir, "clone", "-q", "--branch", "main", mergedUrl, checkout);
      git(checkout, "checkout", "-q", "-b", "later");
      commit(checkout, "l1");
      const run = releaseBefore(checkout, "after-merge-branch-history");
      expect(run).toEqual({ out: m4, code: 0 });
      expect(run.out).not.toBe(git(checkout, "merge-base", "HEAD", "origin/main"));
    },
    gitTimeoutMs
  );
});
