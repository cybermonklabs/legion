import { afterAll, beforeAll, expect, test } from "bun:test";
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { hostTool } from "@legion/test-fixtures/host-tools";

import { buildStallLibrary, endStall, stallDisk, stallEnvironment } from "../disk-stall";
import { eventually, rowLimit, runBounded, watch } from "./bounded-subprocess";

/** e2e/fsync-stall.c, the library the browser suite's disk stall preloads into Postgres
 *  (e2e/disk-stall.ts), driven here through `/usr/bin/dd`, whose `conv=fsync` and
 *  `conv=fdatasync` end its run with the one call each names. In CI the row that stalls the disk
 *  (e2e/read-marker-disk-stall.multiprocess.e2e.ts) runs against a server with `fsync` off, which
 *  never calls either, so these rows are what hold the library to what it says it does. `dd` is
 *  named by its path, so a `dd` some other directory of `PATH` puts first is not what is read.
 *
 *  The library is built once, by `buildStallLibrary`, the harness's own build, whose `cc` runs
 *  with no bound of `bounded-subprocess.ts`, as `e2e/psql.ts`'s psql does: a bound here would
 *  reach past `e2e/__tests__/`. Every `dd` these rows wait for is a bounded child. */

const dd = "/usr/bin/dd";
const missing = [
  ...(process.platform === "linux" ? [] : ["Linux, whose loader reads LD_PRELOAD"]),
  ...(existsSync(dd) ? [] : [dd]),
  ...(existsSync("/proc/self/wchan") ? [] : ["/proc/<pid>/wchan, which names a sleeping call"]),
  ...(hostTool("cc") === null ? ["cc on this host's PATH"] : []),
];
const live = missing.length === 0;
if (!live) {
  console.warn(
    `Skipped the tests in ${import.meta.path}. This host does not have: ${missing.join("; ")}.`
  );
}

let dir = "";
beforeAll(async () => {
  if (!live) return;
  dir = mkdtempSync(join(tmpdir(), "fsync-stall-"));
  await buildStallLibrary(dir);
});
afterAll(() => {
  if (dir !== "") rmSync(dir, { force: true, recursive: true });
});

/** The bytes each `dd` writes before the call it ends on. */
const written = 4096;
let outputs = 0;

/** One `dd` of `written` bytes into a file of its own, ending on `conv`, with the variables
 *  `env` sets and no other: `dd` is named by its path and needs none, so what each row sets is
 *  all its runs differ by. */
function runDd(conv: "fsync" | "fdatasync", env: Record<string, string | undefined>) {
  outputs += 1;
  const output = join(dir, `out-${outputs}`);
  const environment: Record<string, string> = {};
  for (const [name, value] of Object.entries(env)) {
    if (value !== undefined) environment[name] = value;
  }
  return {
    output,
    run: runBounded({
      argv: [
        dd,
        "if=/dev/zero",
        `of=${output}`,
        `bs=${written}`,
        "count=1",
        `conv=${conv}`,
        "status=none",
      ],
      bound: "short",
      env: environment,
      from: import.meta.url,
    }),
  };
}

/** Whether the `dd` writing `output` sleeps in the kernel's `hrtimer_nanosleep` now, as
 *  `/proc/<pid>/wchan` names the call a sleeping process is in. The process is found by its
 *  command line, which holds `of=<output>`, a path no other process names. */
function sleepsInNanosleep(output: string): boolean {
  for (const entry of readdirSync("/proc")) {
    if (!/^\d+$/.test(entry)) continue;
    try {
      const argv = readFileSync(`/proc/${entry}/cmdline`, "utf8").split("\0");
      if (argv[0] !== dd || !argv.includes(`of=${output}`)) continue;
      return readFileSync(`/proc/${entry}/wchan`, "utf8").includes("nanosleep");
    } catch {
      // The process ended between the listing and the read.
    }
  }
  return false;
}

for (const conv of ["fsync", "fdatasync"] as const) {
  test.skipIf(!live)(
    `dd conv=${conv} waits while the stall file exists, and goes through once it is removed`,
    async () => {
      stallDisk(dir);
      try {
        const { output, run } = runDd(conv, stallEnvironment(dir));
        const ended = watch(run);
        // dd has written its bytes, reached the call it ends on, and sleeps in the library's
        // wait, which polls the file between short sleeps: dd itself never sleeps, so a dd seen
        // in a kernel sleep with its bytes written and its run not ended is one the library
        // holds. A dd the library let through ends with no such sleep (the control below).
        await eventually(`dd conv=${conv} to sleep in the library's wait`, () => {
          return (
            existsSync(output) && statSync(output).size === written && sleepsInNanosleep(output)
          );
        });
        expect(ended(), `dd conv=${conv} ended while the stall file existed`).toBe(false);
        endStall(dir);
        const done = await run;
        expect({ code: done.code, stderr: done.stderr }).toEqual({ code: 0, stderr: "" });
      } finally {
        endStall(dir);
      }
    },
    rowLimit("short", "short")
  );

  // The control: the stall file present and no library preloaded. dd ends of itself, so the
  // wait above is the library's and not one dd or the file makes.
  test.skipIf(!live)(
    `dd conv=${conv} with the stall file present and no library preloaded ends at once`,
    async () => {
      stallDisk(dir);
      try {
        const done = await runDd(conv, { LD_PRELOAD: undefined, PG_STALL_FILE: undefined }).run;
        expect({ code: done.code, stderr: done.stderr }).toEqual({ code: 0, stderr: "" });
      } finally {
        endStall(dir);
      }
    },
    rowLimit("short")
  );

  // The stall file exists throughout, at the path the library would be told: a dd the library
  // held would not end inside its bound, and an empty stderr is a preload the loader made, since
  // one it refused it reports there and goes on without.
  test.skipIf(!live)(
    `dd conv=${conv} with the library preloaded ends at once where no stall file is named, or the named one is absent`,
    async () => {
      stallDisk(dir);
      try {
        const unnamed = await runDd(conv, { ...stallEnvironment(dir), PG_STALL_FILE: undefined })
          .run;
        const empty = await runDd(conv, { ...stallEnvironment(dir), PG_STALL_FILE: "" }).run;
        const absent = await runDd(conv, {
          ...stallEnvironment(dir),
          PG_STALL_FILE: join(dir, "no-such-file"),
        }).run;
        for (const done of [unnamed, empty, absent]) {
          expect({ code: done.code, stderr: done.stderr }).toEqual({ code: 0, stderr: "" });
        }
      } finally {
        endStall(dir);
      }
    },
    rowLimit("short", "short", "short")
  );
}

// The build makes its directory readable by every user and a container mounts all of it, so a
// directory holding a developer's own files is refused before anything changes in it: its mode
// and its contents are what they were. The refusal comes before `cc`, so the row needs none.
test(
  "a stall directory that holds files of its own is refused and keeps its mode",
  async () => {
    const own = mkdtempSync(join(tmpdir(), "fsync-stall-own-"));
    try {
      writeFileSync(join(own, "notes.txt"), "");
      const mode = statSync(own).mode & 0o777;
      await expect(buildStallLibrary(own)).rejects.toThrow(`=${own} holds notes.txt.`);
      expect({ entries: readdirSync(own), mode: statSync(own).mode & 0o777 }).toEqual({
        entries: ["notes.txt"],
        mode,
      });
    } finally {
      rmSync(own, { force: true, recursive: true });
    }
  },
  rowLimit()
);
