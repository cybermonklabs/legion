import { afterEach, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Rig } from "./session-settings-common";
import { Omp } from "./session-settings-omp";

const dirs = temporaryDirectories("session-settings-omp-test");
const pids: number[] = [];
const mark = process.env.LEGION_E2E_RUN;
afterEach(() => {
  for (const pid of pids.splice(0)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {}
  }
  if (mark === undefined) delete process.env.LEGION_E2E_RUN;
  else process.env.LEGION_E2E_RUN = mark;
});

// A rig whose Oh My Pi is a stand-in: it writes the LEGION_E2E_RUN it was handed, prints Oh My Pi's
// `ready` event, and waits.
function rigOf(dir: string): Rig {
  const omp = join(dir, "omp");
  writeFileSync(
    omp,
    `#!/bin/sh\nprintf '%s' "\${LEGION_E2E_RUN-unset}" >'${join(dir, "mark")}'\necho '{"type":"ready"}'\nexec sleep 60\n`,
    { mode: 0o755 }
  );
  for (const sub of ["evidence/logs", "home", "agent", "cwd", "tmp"]) {
    mkdirSync(join(dir, sub), { recursive: true });
  }
  return {
    work: dir,
    evidence: join(dir, "evidence"),
    omp,
    home: join(dir, "home"),
    agentDir: join(dir, "agent"),
    sessionCwd: join(dir, "cwd"),
    tmp: join(dir, "tmp"),
    pidFile: join(dir, "omp.pids"),
    dispatchUrl: "http://127.0.0.1:1",
    envoyUrl: "http://127.0.0.1:1",
    natsUrl: "nats://127.0.0.1:1",
    envoyTokenFile: join(dir, "envoy-token"),
    dispatchTokenFile: join(dir, "dispatch-token"),
  } as unknown as Rig;
}

test("Omp.start hands the Oh My Pi it starts the run's mark, and records its pid with its start", async () => {
  const dir = dirs();
  const rig = rigOf(dir);
  process.env.LEGION_E2E_RUN = join(dir, "run");
  const omp = await Omp.start(rig, "marked", null);
  pids.push(omp.pid);
  expect(readFileSync(join(dir, "mark"), "utf8")).toBe(join(dir, "run"));
  const start = readFileSync(`/proc/${omp.pid}/stat`, "utf8")
    .replace(/^[\s\S]*\) /, "")
    .split(" ")[19];
  expect(readFileSync(rig.pidFile, "utf8")).toBe(`${omp.pid} ${start}\n`);
});

test("Omp.start refuses to start an Oh My Pi without the run's mark, or with an empty one", async () => {
  for (const value of [undefined, ""]) {
    const dir = dirs();
    const rig = rigOf(dir);
    if (value === undefined) delete process.env.LEGION_E2E_RUN;
    else process.env.LEGION_E2E_RUN = value;
    await expect(Omp.start(rig, "unmarked", null)).rejects.toThrow(/LEGION_E2E_RUN is not set/);
    expect(() => readFileSync(rig.pidFile, "utf8")).toThrow();
  }
});
