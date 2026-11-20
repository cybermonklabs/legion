/** Rows of the mark's shape (`e2e/harness-postgres-marks.ts`) for the check
 *  `v-no-leftover-postgres` (requirement R3 of pull request #4236): which paths a start trusts a
 *  mark and what it names by (`whyUntrusted`), and the one spelling a shell stack's mark holds its
 *  run directory by (`e2e/postgres-mark.ts label`). A mark another user wrote needs a second user,
 *  which a test run has none of, so these rows read owners from a stubbed `lstat`. The start's
 *  removal of a dead stack's Postgres, which acts only on what these checks trust, has its rows in
 *  `harness-postgres-removal.test.ts`, the files split from it by concern, and
 *  `harness-postgres-holders.test.ts`. */

import { expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, type Stats } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import * as marks from "../harness-postgres-marks";
import { partOf, rowMs, rowOf, type Waits } from "./harness-postgres-waits";

/** A stubbed `lstat` over `entries`, path to what it says; any other path is gone. */
function stubbedLstat(
  entries: Record<string, { kind: "directory" | "file" | "link"; uid: number; mode: number }>
) {
  return (path: string): Stats => {
    const entry = entries[path];
    if (entry === undefined) {
      throw Object.assign(new Error(`ENOENT: ${path}`), { code: "ENOENT" });
    }
    // The fields `whyUntrusted` reads, which is all a stub of the kernel's answer needs to hold.
    const stat = {
      uid: entry.uid,
      mode: entry.mode,
      isDirectory: () => entry.kind === "directory",
      isFile: () => entry.kind === "file",
      isSymbolicLink: () => entry.kind === "link",
    } as Stats;
    return stat;
  };
}

const me = 1000;
const other = 4242;
const root = { kind: "directory", uid: 0, mode: 0o755 } as const;
const sharedTmp = { kind: "directory", uid: 0, mode: 0o1777 } as const;

test("v-no-leftover-postgres: whyUntrusted: in /tmp, whose sticky bit lets another user make a directory of a guessed name first, a run directory of another user is untrusted, and one of this user's is trusted", () => {
  const theirs = stubbedLstat({
    "/": root,
    "/tmp": sharedTmp,
    "/tmp/dispatch-e2e-run-abcdef": { kind: "directory", uid: other, mode: 0o700 },
  });
  expect(marks.whyUntrusted("/tmp/dispatch-e2e-run-abcdef", me, "directory", theirs)).toBe(
    `/tmp/dispatch-e2e-run-abcdef belongs to user ${other}`
  );
  const mine = stubbedLstat({
    "/": root,
    "/tmp": sharedTmp,
    "/tmp/dispatch-e2e-run-abcdef": { kind: "directory", uid: me, mode: 0o700 },
  });
  expect(marks.whyUntrusted("/tmp/dispatch-e2e-run-abcdef", me, "directory", mine)).toBeUndefined();
});

test("v-no-leftover-postgres: whyUntrusted: a mark file of another user is untrusted, even in a run directory of this user's", () => {
  const file = "/tmp/dispatch-e2e-run-abcdef/postgres-marks/worker-0.json";
  const lstat = stubbedLstat({
    "/": root,
    "/tmp": sharedTmp,
    "/tmp/dispatch-e2e-run-abcdef": { kind: "directory", uid: me, mode: 0o700 },
    "/tmp/dispatch-e2e-run-abcdef/postgres-marks": { kind: "directory", uid: me, mode: 0o700 },
    [file]: { kind: "file", uid: other, mode: 0o600 },
  });
  expect(marks.whyUntrusted(file, me, "file", lstat)).toBe(`${file} belongs to user ${other}`);
});

test("v-no-leftover-postgres: whyUntrusted: a directory above the path that another user owns, or that others may write without a sticky bit, or that is a link, makes the path untrusted", () => {
  const leaf = { kind: "directory", uid: me, mode: 0o700 } as const;
  const above = (entry: { kind: "directory" | "file" | "link"; uid: number; mode: number }) =>
    marks.whyUntrusted(
      "/srv/run",
      me,
      "directory",
      stubbedLstat({ "/": root, "/srv": entry, "/srv/run": leaf })
    );
  expect(above({ kind: "directory", uid: other, mode: 0o755 })).toBe(
    `/srv belongs to user ${other}`
  );
  expect(above({ kind: "directory", uid: 0, mode: 0o777 })).toBe("/srv is writable by other users");
  expect(above({ kind: "link", uid: me, mode: 0o777 })).toBe("/srv is a symbolic link");
  expect(above({ kind: "directory", uid: me, mode: 0o755 })).toBeUndefined();
});

const markCommand = fileURLToPath(new URL("../postgres-mark.ts", import.meta.url));

/** One run of `postgres-mark.ts label`, a `bun` child that the row ends at the bound of what it
 *  waits for: its start under load, and no wait after it. */
const labelRun = partOf("a run of postgres-mark.ts label", { children: 1, reads: 0, bounds: [] });
/** What the next row waits for (`Waits`): three runs of the command, one after another. */
const threeLabels: Waits = {
  children: 0,
  reads: 0,
  bounds: [],
  parts: [labelRun, labelRun, labelRun],
};

// A shell driver spells its run directory `${TMPDIR:-/tmp}/…`, so a TMPDIR that ends in `/` gives
// it a doubled `/`: the command marks the directory that spelling names, under its one spelling,
// as it marks the same directory spelt plain.
test.skipIf(process.platform !== "linux")(
  "v-no-leftover-postgres: postgres-mark.ts label marks a run directory spelt with a doubled or a trailing / under its one spelling, which a start's sweep reads as a mark",
  () => {
    const row = rowOf(threeLabels);
    const tmp = mkdtempSync(join(tmpdir(), "mark-label-"));
    try {
      const work = join(tmp, "work");
      mkdirSync(work);
      const real = marks.identityOf(work);
      for (const spelt of [`${tmp}//work`, `${work}/`, work]) {
        row.part(labelRun);
        const label = spawnSync(
          process.execPath,
          [markCommand, "label", String(process.pid), spelt, "dispatch-e2e-pg-label-0"],
          { encoding: "utf8", timeout: labelRun.ms }
        );
        expect({ spelt, status: label.status, stderr: label.stderr }).toEqual({
          spelt,
          status: 0,
          stderr: "",
        });
        const mark = marks.markOf(label.stdout.trim().slice(`${marks.markLabel}=`.length));
        expect({ spelt, run: mark?.run, identity: mark?.identity }).toEqual({
          spelt,
          run: real,
          identity: real,
        });
      }
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  },
  // What the row waits for, summed by the fixtures' program (`rowMs`): three runs of the command,
  // one after another, each at its own bound.
  rowMs(threeLabels)
);
