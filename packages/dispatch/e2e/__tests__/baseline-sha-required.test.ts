/** Every run that boots the base release names that release: BASELINE_SHA has no default.
 *
 *  Version 39 of this change's Verification document holds that BASELINE_SHA, the release this
 *  branch lands after, has no default, and that a run which leaves it unset is refused, naming the
 *  variable. A default lets an unset run boot or read some other base, such as main's merge base
 *  or whatever binary $TMPDIR/base-dispatch holds, and pass on that base without being able to
 *  fail. This file runs `lib-baseline.sh --selftest` and each of the four drivers that boot the
 *  base with BASELINE_SHA unset, and holds that each exits 2, says that BASELINE_SHA is unset,
 *  names no default (no `merge-base`), and starts nothing.
 *
 *  Nothing real starts in any row. The base binary and the head binary each row hands the driver
 *  are scripts of the row's own that leave a marker file and exit 1, and a `go` of the row's own
 *  stands first on PATH and does the same, so a run that builds the base, or boots either server,
 *  leaves a marker. DATABASE_URL and LEGION_TEST_PG_DSN name a database on a port nothing listens
 *  on, so a run that goes past its refusal reaches no database either.
 *
 *  A row skips where this host lacks a program its script requires before anything else, because
 *  that script then refuses for the program and never reaches BASELINE_SHA: CI's `dispatch-ui`
 *  job has no psql. */

import { expect, test } from "bun:test";
import { existsSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostTool } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

import { masked, rowLimit, runBounded } from "./bounded-subprocess";

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));

/** A database on a port nothing listens on: psql gets no answer there, in this pod or in CI. */
const unreachableDatabase =
  "postgres://legion_test@127.0.0.1:1/infra_1150_baseline_sha_required?sslmode=disable";

interface Script {
  /** The script, relative to packages/dispatch, and its arguments. */
  readonly argv: readonly string[];
  /** What the script requires on PATH before it reads anything else. */
  readonly requires: readonly string[];
  /** Whether the row hands the script a base binary, as every driver's own default does. */
  readonly givesBinary: boolean;
}

const scripts: Record<string, Script> = {
  "lib-baseline.sh --selftest": {
    argv: ["e2e/acceptance/lib-baseline.sh", "--selftest"],
    requires: ["env", "git", "openssl", "psql", "python3", "sleep", "tar"],
    givesBinary: false,
  },
  "restore-redirects.sh": {
    argv: ["e2e/acceptance/restore-redirects.sh"],
    requires: ["cmp", "curl", "git", "jq", "psql", "sed", "sort", "tr"],
    givesBinary: true,
  },
  "decision-nodes.sh": {
    argv: ["e2e/acceptance/decision-nodes.sh"],
    requires: ["cmp", "curl", "date", "diff", "jq", "psql", "sed", "sort", "tr"],
    givesBinary: true,
  },
  "answered-decisions.sh": {
    argv: ["e2e/acceptance/answered-decisions.sh"],
    requires: ["cmp", "curl", "jq", "psql", "sed", "sort"],
    givesBinary: true,
  },
  "answer-attribute-removal.sh": {
    argv: ["e2e/acceptance/answer-attribute-removal.sh"],
    requires: ["date", "grep", "jq", "psql", "sed", "tr"],
    givesBinary: true,
  },
};

const fakes = temporaryDirectories("baseline-sha-required");

/** An executable at `path` that leaves `marker` and exits 1. */
function leavesMarker(path: string, marker: string): void {
  writeFileSync(path, `#!/usr/bin/env bash\n: >'${marker}'\nexit 1\n`, { mode: 0o755 });
}

for (const [name, script] of Object.entries(scripts)) {
  const missing = script.requires.filter((program) => hostTool(program) === null);
  if (missing.length > 0) {
    console.warn(
      `Skipped the row of ${name} in ${import.meta.path}: this host does not have ` +
        `${missing.join(", ")}, which the script requires before it reads BASELINE_SHA.`
    );
  }
  test.skipIf(missing.length > 0)(
    `${name} refuses a run that leaves BASELINE_SHA unset, naming it, and starts nothing`,
    async () => {
      const directory = fakes();
      const bin = join(directory, "bin");
      const scratch = join(directory, "tmp");
      await Bun.$`mkdir -p ${bin} ${scratch}`.quiet();
      const marker = join(directory, "started");
      leavesMarker(join(bin, "go"), marker);
      const baseBinary = join(directory, "base-dispatch");
      const headBinary = join(directory, "envoy-dispatch");
      leavesMarker(baseBinary, marker);
      leavesMarker(headBinary, marker);
      // lib-baseline.sh reads the commit out of LEGION_WORKSPACE when no binary is given. A
      // checkout of one commit, whose origin/main is that commit, makes any default it computes
      // resolve at once and archive almost nothing, so a run that falls back on a default reaches
      // the `go` above within the bound rather than archiving this repository.
      const checkout = join(directory, "checkout");
      if (!script.givesBinary) {
        await Bun.$`git init -q ${checkout} && git -C ${checkout} -c user.name=row -c user.email=row@example.invalid commit -q --allow-empty -m one && git -C ${checkout} update-ref refs/remotes/origin/main HEAD`.quiet();
      }
      const { code, stderr } = await runBounded({
        argv: ["bash", ...script.argv],
        bound: "short",
        cwd: dispatchPackage,
        env: {
          DATABASE_URL: unreachableDatabase,
          HEAD_BINARY: headBinary,
          HOME: process.env.HOME ?? "",
          LEGION_TEST_PG_DSN: unreachableDatabase,
          PATH: `${bin}:${process.env.PATH ?? ""}`,
          PR_BODY_FILE: join(directory, "no-body.md"),
          TMPDIR: scratch,
          ...(script.givesBinary
            ? { BASELINE_BINARY: baseBinary }
            : { LEGION_WORKSPACE: checkout }),
        },
        from: import.meta.url,
      });
      const said = `${name} exited ${code}, and its stderr read:\n${masked(stderr) || "(nothing)"}`;
      expect(existsSync(marker), `${name} built or booted a server. ${said}`).toBe(false);
      expect(
        code,
        `${name} went on past an unset BASELINE_SHA instead of refusing it with exit 2. ${said}`
      ).toBe(2);
      expect(stderr, `${name} refused, but not for BASELINE_SHA. ${said}`).toMatch(/BASELINE_SHA/);
      expect(stderr, `${name} names a default for BASELINE_SHA. ${said}`).not.toMatch(/merge-base/);
    },
    rowLimit("short")
  );
}
