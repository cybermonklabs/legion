import { afterAll, expect, setDefaultTimeout, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS);

const scratch = mkdtempSync(path.join(tmpdir(), "process-group-"));
afterAll(() => rmSync(scratch, { recursive: true, force: true }));

// The watch kills the group its command leads. A caller that forgot `detached: true` runs the
// command in its own group, and a watch that killed "its own group" would kill that caller once it
// closed the pipe after a normal run. Run in a process of its own, in a group of its own, so a
// wrong watch kills that process and not this test's runner.
//
// The caller prints only once the watch has acted: the watch is a process of the caller's group,
// and it ends once it has seen the pipe close and run its kill, so the caller waits until no live
// process but itself is left in its group (`/proc/<pid>/stat`: a zombie has ended). A watch that
// killed the group kills the caller before that, however late it acts. Bun's per-test timeout is
// the only bound.
test("a watched command spawned without a group of its own kills nothing of its caller's", async () => {
  const caller = `
import { readdirSync, readFileSync } from "node:fs";
import { groupWatchedBy } from ${JSON.stringify(path.resolve(import.meta.dir, "../process-group.ts"))};
/** How many live processes other than this one are in its group, which it leads. */
const others = () => {
  let live = 0;
  for (const entry of readdirSync("/proc")) {
    if (!/^[0-9]+$/.test(entry) || Number(entry) === process.pid) continue;
    let stat;
    try {
      stat = readFileSync("/proc/" + entry + "/stat", "utf-8");
    } catch {
      continue; // Gone since the directory was read.
    }
    // "pid (comm) state ppid pgrp ...": comm can hold spaces and parentheses, so the fields are
    // read after its last ")".
    const [state, , group] = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
    if (Number(group) === process.pid && state !== "Z") live += 1;
  }
  return live;
};
const child = Bun.spawn(groupWatchedBy(["/bin/sh", "-c", "exit 0"]), {
  stdin: "pipe",
  stdout: "ignore",
  stderr: "ignore",
});
await child.exited;
void child.stdin.end();
// The pace of the poll, not a bound: nothing announces another process's end to this one.
while (others() > 0) await Bun.sleep(10);
console.log("caller survived");
`;
  const run = Bun.spawn([process.execPath, "-e", caller], {
    cwd: scratch,
    env: { PATH: process.env.PATH ?? "" },
    stdout: "pipe",
    stderr: "pipe",
    detached: true,
  });
  const [out] = await Promise.all([new Response(run.stdout).text(), run.exited]);
  expect(run.signalCode).toBeNull();
  expect(out).toBe("caller survived\n");
}, 20_000);
