import { afterEach, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { ownGroup, untilRunning } from "./own-group";
import { procRootOf } from "./proc-root";
import { libRowBoundMs, rowWaits } from "./row-waits";

// refuse_leftovers (lib/leftovers.sh) is a stage driver's check that no earlier run of its stage left
// a process running whose driver is gone: a process names its run by /tmp/<PREFIX>.<driver pid>. in
// its command line, its environment or its working directory. It reads the processes of
// `${proc_root:-/proc}`; each row points proc_root at a proc root of its own processes
// (lib/proc-root.ts), so its time is its own and not the host's process count.
const leftovers = join(import.meta.dir, "leftovers.sh");
const dir = temporaryDirectory("leftovers-test");
const skip = skipWithout("perl", "sleep", "find");
const prefix = "legion-e2e-leftovers-test";
// sleeper waits for what it started to run its program, a wait that ends 20 s before the row's bound.
const waits = rowWaits(libRowBoundMs);

const started: number[] = [];
afterEach(() => {
  for (const pid of started.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
});
async function sleeper(
  options: { cwd?: string; env?: Record<string, string>; argv?: string[] } = {}
) {
  const child = Bun.spawn(ownGroup(options.argv ?? ["sleep", "60"]), {
    cwd: options.cwd ?? dir,
    env: { PATH: process.env.PATH ?? "", ...(options.env ?? {}) },
    stdout: "ignore",
    stderr: "ignore",
  });
  started.push(child.pid);
  await untilRunning(child.pid, options.argv?.[0] ?? "sleep", waits.end());
  return child.pid;
}
// A pid no process holds now: a process this row started and reaped, the driver of a run that is
// gone.
function goneDriver(): number {
  const gone = Bun.spawnSync(["true"]);
  return gone.pid;
}
// The rows read processes, not containers: refuse_leftovers asks `docker ps` for an earlier run's
// containers first, so each row puts a docker first on PATH that lists none. It keeps the rows off
// the host's containers, and lets them run where there is no docker at all (a Legion pod), where
// the host's missing docker said `docker: command not found` on the stderr the rows hold empty.
let noContainers = "";
function dockerListingNone(): string {
  if (noContainers === "") {
    noContainers = mkdtempSync(join(dir, "docker-"));
    writeFileSync(join(noContainers, "docker"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  }
  return noContainers;
}
// check runs refuse_leftovers PREFIX over ROOT, with PATH's front directory BIN when given, and
// answers its status and what its `fail` said (a driver's fail, here one that prints and returns).
function check(root: string, bin = "") {
  const ran = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -u\nfail() { printf '%s\\n' "$*"; return 1; }\n. ${JSON.stringify(leftovers)}\nrefuse_leftovers ${prefix}; echo "status $?"`,
    ],
    {
      env: {
        PATH: `${bin}${bin ? ":" : ""}${dockerListingNone()}:${process.env.PATH ?? ""}`,
        proc_root: root,
      },
      stdout: "pipe",
      stderr: "pipe",
    }
  );
  return { out: ran.stdout.toString(), err: ran.stderr.toString() };
}

test.skipIf(skip)(
  "names each process an earlier run left, by its command line, its environment or its working directory, and no process of a live run",
  async () => {
    const gone = goneDriver();
    const runDir = mkdtempSync(`/tmp/${prefix}.${gone}.`);
    const liveDir = mkdtempSync(`/tmp/${prefix}.${process.pid}.`);
    try {
      // A bash whose last argument is the run's directory, which it keeps in its command line.
      const byArgs = await sleeper({ argv: ["bash", "-c", "sleep 60; :", "bash", runDir] });
      const byEnv = await sleeper({ env: { RUN: `${runDir}/state` } });
      const byCwd = await sleeper({ cwd: runDir });
      const live = await sleeper({ cwd: liveDir });
      const none = await sleeper();
      const r = check(procRootOf(dir, [byArgs, byEnv, byCwd, live, none]));
      expect(r.err).toBe("");
      const said = r.out;
      for (const pid of [byArgs, byEnv, byCwd]) expect(said).toContain(`process ${pid}, `);
      for (const pid of [live, none]) expect(said).not.toContain(`process ${pid}, `);
      expect(said).toEndWith("status 1\n");
    } finally {
      for (const pid of started.splice(0)) {
        try {
          process.kill(pid, "SIGKILL");
        } catch {}
      }
      Bun.spawnSync(["rm", "-rf", runDir, liveDir]);
    }
  }
);

test.skipIf(skip)(
  "starts no program for each process it reads: the working directories it needs are read by one find",
  async () => {
    // A tr, a readlink and a cat that record each call and then run the real one: a walk that ran
    // one for each process says so, once a process, in the log.
    const bin = mkdtempSync(join(dir, "bin-"));
    const calls = join(bin, "calls");
    writeFileSync(calls, "");
    for (const tool of ["tr", "readlink", "cat"]) {
      writeFileSync(
        join(bin, tool),
        `#!/bin/sh\nprintf '%s\\n' ${tool} >>'${calls}'\nexec /usr/bin/${tool} "$@"\n`,
        { mode: 0o755 }
      );
    }
    const pids: number[] = [];
    for (let i = 0; i < 20; i++) pids.push(await sleeper());
    const r = check(procRootOf(dir, pids), bin);
    expect(r.err).toBe("");
    expect(r.out).toBe("status 0\n");
    expect(readFileSync(calls, "utf8")).toBe("");
  }
);

test.skipIf(skip)("a proc root that lists no process is reported, and found no leftover", () => {
  const empty = mkdtempSync(join(dir, "empty-"));
  mkdirSync(join(empty, "not-a-pid"));
  const r = check(empty);
  expect(r.err).toBe("");
  expect(r.out).toBe(
    `refuse_leftovers: ${empty} lists no process, so whether an earlier ${prefix} run left processes running was not read\nstatus 2\n`
  );
});

// The working directories are read by one find, and a find that read nothing is no answer: one too
// old for -files0-from (findutils before 4.9, which says so and exits 1) and one a signal ends are
// each reported, naming the find's status and its complaint, with 2. The control is the find every
// walk of a busy machine runs: it exits 1 for an entry of another user's process, which is no
// complaint, so the check still answers.
function findStub(body: string) {
  const bin = mkdtempSync(join(dir, "find-"));
  writeFileSync(join(bin, "find"), `#!/bin/sh\n${body}\n`, { mode: 0o755 });
  return bin;
}
test.skipIf(skip)(
  "a find that fails, that a signal ends or that is too old for -files0-from is reported with 2, and one that meets only unreadable entries is not",
  async () => {
    const root = procRootOf(dir, [await sleeper()]);
    const said = (bin: string) => check(root, bin);
    const tooOld = said(findStub(`echo "find: unknown predicate \\\`-files0-from'" >&2\nexit 1`));
    expect(tooOld).toEqual({
      out: `refuse_leftovers: find exited 1 reading the working directories under ${root}: find: unknown predicate \`-files0-from'\nstatus 2\n`,
      err: "",
    });
    const killed = said(findStub("kill -KILL $$"));
    expect(killed).toEqual({
      out: `refuse_leftovers: find exited 137 reading the working directories under ${root}: find ended with status 137 and printed nothing\nstatus 2\n`,
      err: "",
    });
    const unreadable = said(
      findStub(`/usr/bin/find "$@"\necho "find: '$proc_root/1/cwd': Permission denied" >&2\nexit 1`)
    );
    expect(unreadable).toEqual({ out: "status 0\n", err: "" });
  }
);
