import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { processEnded, processRunning } from "./process-end";

/** The state field of /proc/<pid>/stat: `S` sleeping, `T` stopped, `Z` a zombie. */
function stateOf(pid: number): string {
  const stat = readFileSync(`/proc/${pid}/stat`, "utf-8");
  return stat[stat.lastIndexOf(")") + 2] ?? "";
}

/** The first line `stream` carries, which a process that holds the stream open may write alone. */
async function firstLine(stream: ReadableStream<Uint8Array>): Promise<string> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let text = "";
  while (!text.includes("\n")) {
    const { value, done } = await reader.read();
    if (done) break;
    text += decoder.decode(value, { stream: true });
  }
  reader.releaseLock();
  return text.split("\n")[0] ?? "";
}

test("a running process reads as running, and the wait ends once it is killed", async () => {
  const child = Bun.spawn(["sleep", "600"]);
  try {
    expect(processRunning(child.pid)).toBe(true);
    child.kill("SIGKILL");
    await processEnded(child.pid);
    expect(processRunning(child.pid)).toBe(false);
  } finally {
    child.kill("SIGKILL");
  }
});

// A killed orphan stays a zombie until whatever adopted it reaps it, and `kill(pid, 0)` answers for
// a zombie. Here the parent is stopped, so it cannot reap its killed child: the child has ended,
// and the wait must say so.
test("a zombie has ended, though kill(pid, 0) still answers for it", async () => {
  const parent = Bun.spawn(["/bin/sh", "-c", "sleep 600 & echo $!; wait"], {
    stdout: "pipe",
    stderr: "ignore",
  });
  try {
    const pid = Number(await firstLine(parent.stdout));
    expect(pid).toBeGreaterThan(0);
    process.kill(parent.pid, "SIGSTOP");
    // The pace of the poll, not a bound: a stop is delivered when the kernel schedules it, and a
    // parent not yet stopped would reap the child it is waiting for.
    while (stateOf(parent.pid) !== "T") await Bun.sleep(10);
    process.kill(pid, "SIGKILL");
    await processEnded(pid);
    expect(stateOf(pid)).toBe("Z");
    expect(() => process.kill(pid, 0)).not.toThrow();
  } finally {
    parent.kill("SIGKILL");
  }
});
