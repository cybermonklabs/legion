/** Which base `e2e/acceptance/project-access-blob.sh` (c-blob-unchanged) compares against when it
 *  is not given one: HEAD's merge base with the base branch (origin/legion/INFRA-1149). A history
 *  that merged both ways has two merge bases, and `git merge-base` alone can answer main's, which
 *  holds none of the base branch's routes. The driver takes the newest merge base on the base
 *  branch's first-parent line, and when none is on it, it stops with UNKNOWN naming every
 *  candidate.
 *
 *  Each row copies the driver and the shell it sources into a fresh repository of its own, in the
 *  same relative places, so the driver's `git -C <root>` reads that repository. Every commit is
 *  made with `commit-tree` and refs moved with `update-ref`, so no hook a machine's git
 *  configuration installs runs, and each commit carries its own date, which is what decides the
 *  merge base `git merge-base` answers first. A row the default admits goes on to the driver's
 *  prerequisites and stops at the first, DATABASE_URL, which no row sets: nothing is built and no
 *  server starts. */

import { expect, test } from "bun:test";
import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const repositoryRoot = fileURLToPath(new URL("../../../..", import.meta.url));
const driver = "packages/dispatch/e2e/acceptance/project-access-blob.sh";
/** The driver and every file it sources, from the repository root. */
const sources = [
  driver,
  "packages/dispatch/e2e/acceptance/access-lib.sh",
  "scripts/e2e/lib/two-builds.sh",
  "scripts/e2e/lib/rig.sh",
  "scripts/e2e/lib/loopback-dsn.sh",
  "scripts/e2e/lib/loopback-dsn.py",
  "scripts/e2e/lib/port-file.sh",
];

const scratch = temporaryDirectories("project-access-blob-base");

/** A repository the test writes commits into, each with a date of its own. */
class Repository {
  readonly directory = scratch();
  private tree = "";
  private clock = 1_700_000_000;

  constructor() {
    for (const source of sources) {
      mkdirSync(dirname(join(this.directory, source)), { recursive: true });
      copyFileSync(join(repositoryRoot, source), join(this.directory, source));
    }
    this.git("init", "-q", "-b", "main");
    this.tree = this.git("write-tree");
  }

  git(...args: string[]): string {
    const date = `${this.clock} +0000`;
    const run = Bun.spawnSync(["git", "-C", this.directory, ...args], {
      env: {
        GIT_AUTHOR_NAME: "test",
        GIT_AUTHOR_EMAIL: "test@example.com",
        GIT_AUTHOR_DATE: date,
        GIT_COMMITTER_NAME: "test",
        GIT_COMMITTER_EMAIL: "test@example.com",
        GIT_COMMITTER_DATE: date,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
        PATH: process.env.PATH ?? "",
      },
    });
    if (run.exitCode !== 0) {
      throw new Error(`git ${args.join(" ")}: ${run.stderr.toString()}`);
    }
    return run.stdout.toString().trim();
  }

  /** A commit with these parents, the first parent first, dated after every commit before it. */
  commit(message: string, ...parents: string[]): string {
    this.clock += 60;
    return this.git(
      "commit-tree",
      this.tree,
      ...parents.flatMap((parent) => ["-p", parent]),
      "-m",
      message
    );
  }

  ref(name: string, commit: string): void {
    this.git("update-ref", "--no-deref", name, commit);
  }

  /** One run of the driver here, with no base named and no DATABASE_URL. */
  run(): { exitCode: number; stdout: string; stderr: string } {
    const run = Bun.spawnSync(["bash", driver], {
      cwd: this.directory,
      env: { PATH: process.env.PATH ?? "", HOME: this.directory, TMPDIR: this.directory },
    });
    return {
      exitCode: run.exitCode ?? -1,
      stdout: run.stdout.toString(),
      stderr: run.stderr.toString(),
    };
  }
}

test("of two merge bases, the one on the base branch's first-parent line is the base", () => {
  const repository = new Repository();
  const root = repository.commit("main's root");
  // The base branch starts from main, and this stack from the base branch.
  const branch = repository.commit("the base branch's first change", root);
  const stack = repository.commit("this stack's first change", branch);
  // Main moves on; the base branch merges it, and so, on its own, does this stack.
  const main = repository.commit("main moves on", root);
  const branchMerge = repository.commit("the base branch merges main", branch, main);
  const head = repository.commit("this stack merges main", stack, main);
  repository.ref("refs/remotes/origin/main", main);
  repository.ref("refs/remotes/origin/legion/INFRA-1149", branchMerge);
  repository.ref("HEAD", head);

  const bases = repository
    .git("merge-base", "--all", "HEAD", "origin/legion/INFRA-1149")
    .split("\n");
  expect(bases.toSorted()).toEqual([branch, main].toSorted());
  // The fixture is the case the default has to get right: `git merge-base` alone answers main's
  // commit, which holds none of the base branch's changes.
  expect(repository.git("merge-base", "HEAD", "origin/legion/INFRA-1149")).toBe(main);

  const run = repository.run();
  expect(run.stdout).toContain(`the merge base is HEAD's with origin/legion/INFRA-1149: ${branch}`);
  expect(run.stderr).toContain("DATABASE_URL is required");
});

test("merge bases none of which is on the base branch's first-parent line are UNKNOWN, each named", () => {
  const repository = new Repository();
  const root = repository.commit("main's root");
  const branch = repository.commit("the base branch's first change", root);
  // Two side branches the base branch merges as second parents, and this stack merges both.
  const left = repository.commit("a side branch", root);
  const right = repository.commit("another side branch", root);
  const branchHead = repository.commit(
    "the base branch merges the other side branch",
    repository.commit("the base branch merges a side branch", branch, left),
    right
  );
  const head = repository.commit("this stack merges both side branches", left, right);
  repository.ref("refs/remotes/origin/main", root);
  repository.ref("refs/remotes/origin/legion/INFRA-1149", branchHead);
  repository.ref("HEAD", head);

  const run = repository.run();
  expect(run.exitCode).toBe(1);
  expect(run.stderr).toContain("UNKNOWN: HEAD and origin/legion/INFRA-1149 have 2 merge bases");
  expect(run.stderr).toContain(left);
  expect(run.stderr).toContain(right);
  expect(run.stderr).toContain("name the base with --base-commit");
  expect(run.stderr).not.toContain("DATABASE_URL is required");
});

test("one merge base is the base", () => {
  const repository = new Repository();
  const root = repository.commit("main's root");
  const branch = repository.commit("the base branch's first change", root);
  const head = repository.commit("this stack's first change", branch);
  repository.ref("refs/remotes/origin/main", root);
  repository.ref("refs/remotes/origin/legion/INFRA-1149", branch);
  repository.ref("HEAD", head);

  const run = repository.run();
  expect(run.stdout).toContain(`the merge base is HEAD's with origin/legion/INFRA-1149: ${branch}`);
  expect(run.stderr).toContain("DATABASE_URL is required");
});
