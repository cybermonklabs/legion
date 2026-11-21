import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { claimOf, evidenceOf, MINUTE, SECOND, START } from "../test/stall-fixtures";
// The import below is this file's merge-base check: on the merge base there is no
// `./stall-evidence`, so every test here fails to resolve rather than passing vacuously.
import {
  boundedText,
  readStallProcesses,
  STALL_DIGEST_LIMIT,
  STALL_READ_ENVIRONMENT,
  stallArgumentsDigest,
  stallEvidenceRecord,
  stallProcessTree,
} from "./stall-evidence";

// ---------------------------------------------------------------------------------------------
// Real processes, read from this pod's own /proc.
// ---------------------------------------------------------------------------------------------

const spawned: Bun.Subprocess[] = [];

afterEach(() => {
  for (const child of spawned.splice(0)) child.kill("SIGKILL");
});

/** One real child of this process, with the environment the case needs, left running until the
 * case ends. `sh -c` is what makes a process of its own: a command Oh My Pi runs in its own shell
 * has none (measured). */
async function child(environment: Record<string, string>): Promise<number> {
  // `exec`, so the process the case reads is the one `kill` ends: a shell that forks `sleep`
  // leaves it running when the shell is killed, reparented to pid 1 (measured).
  const running = Bun.spawn(["sh", "-c", "exec sleep 300"], {
    env: { ...process.env, ...environment },
    stdio: ["ignore", "ignore", "ignore"],
  });
  spawned.push(running);
  // The pid is live the moment `spawn` returns, but the kernel carries the process and its
  // environment a moment later. What is awaited is that reading, not a guessed delay: this case
  // is about a real process tree, so there is no clock to fake. The command awaited is the one
  // `exec` leaves, `sleep 300` exactly: the shell's own line, `sh -c exec sleep 300`, names it
  // too and is read first (252 of 300 spawns, measured), and a reading taken while `exec` runs
  // has no argv at all, so the case would read the process as `[sh]`.
  await until(async () => {
    const scan = await readStallProcesses();
    if (!scan.read) return false;
    const found = scan.processes.find((candidate) => candidate.pid === running.pid);
    return (
      found !== undefined &&
      found.environment.PI_TOOL_CALL_ID === environment.PI_TOOL_CALL_ID &&
      found.command === "sleep 300"
    );
  });
  return running.pid;
}

/** Waits for one reading of the machine to come true, up to ten seconds. */
async function until(ready: () => Promise<boolean>): Promise<void> {
  for (let attempt = 0; attempt < 1_000; attempt += 1) {
    if (await ready()) return;
    await Bun.sleep(10);
  }
  throw new Error("the machine never reached the state the case needs");
}

test("the tree names every process of the agent, read from this pod's own /proc", async () => {
  const claimSecret = fixtureSecret("claim");
  const pid = await child({
    PI_TOOL_CALL_ID: "toolu_stuck",
    // A credential of the kind an environment routinely holds: the reader keeps two names and
    // this is not one of them, so nothing downstream can write it anywhere.
    LEGION_CLAIM_SECRET: claimSecret,
  });

  const scan = await readStallProcesses();
  if (!scan.read) throw new Error(`this pod's /proc could not be read: ${scan.reason}`);
  const agent = scan.processes[0];
  expect(agent?.pid).toBe(process.pid);
  // Measurement 1: the agent's own environment is the one `/proc` will not hand it here, so the
  // reader reads other pids only.
  expect(agent?.environment).toEqual({});

  const found = scan.processes.find((candidate) => candidate.pid === pid);
  expect(found).toBeDefined();
  expect(found?.ppid).toBe(process.pid);
  expect(found?.command).toContain("sleep 300");
  // The identifier, the start time, the state and the processor time, as the kernel reports them.
  expect(found?.startedAt).toBeGreaterThan(Date.now() - 60 * SECOND);
  expect(found?.startedAt).toBeLessThanOrEqual(Date.now());
  expect(found?.state).toMatch(/^[A-Za-z]$/);
  expect(found?.cpuMs).toBeGreaterThanOrEqual(0);
  // The call its environment names, which is exactly the frame's `toolCallId` (measured).
  expect(found?.toolCallId).toBe("toolu_stuck");
  // And nothing else of that environment is kept, whatever else the process inherited.
  expect(
    Object.keys(found?.environment ?? {}).filter(
      (name) => !STALL_READ_ENVIRONMENT.some((wanted) => wanted === name)
    )
  ).toEqual([]);

  // What the evidence carries: the same processes, with no environment and no parent.
  const tree = stallProcessTree(scan);
  if (!tree.read) throw new Error("the tree was read and then reported unread");
  expect(JSON.stringify(tree)).not.toContain(claimSecret);
  expect(tree.processes.find((candidate) => candidate.pid === pid)).toEqual({
    pid,
    command: found?.command as string,
    startedAt: found?.startedAt as number,
    state: found?.state as string,
    cpuMs: found?.cpuMs as number,
    toolCallId: "toolu_stuck",
  });
});

test("a machine whose /proc cannot be read records that, never an empty tree", async () => {
  const absent = path.join(mkdtempSync(path.join(tmpdir(), "stall-proc-")), "proc");
  try {
    const scan = await readStallProcesses({ procRoot: absent });
    expect(scan.read).toBe(false);
    if (scan.read) throw new Error("a /proc that is not there was read");
    expect(scan.reason).toContain(absent);
    // And the evidence says the same thing, rather than carrying no process at all.
    expect(stallProcessTree(scan)).toEqual({ read: false, reason: scan.reason });
    const record = stallEvidenceRecord(evidenceOf({ tree: stallProcessTree(scan) }), START, {
      session: { kind: "phase-worker", role: "tester", tree: "INFRA-1000", issue: "INFRA-1274" },
      claim: claimOf(),
    });
    expect(record.tree).toEqual({ read: false, reason: scan.reason });
  } finally {
    rmSync(path.dirname(absent), { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------------------------
// The record: the tool, the digest, the start, the age, the tree.
// ---------------------------------------------------------------------------------------------

test("the record names the tool, the digest, the start, the age and the real tree", async () => {
  const pid = await child({ PI_TOOL_CALL_ID: "toolu_stuck" });
  const tree = stallProcessTree(await readStallProcesses());
  const at = START + 65 * MINUTE;
  const record = stallEvidenceRecord(evidenceOf({ tree }), at, {
    session: { kind: "phase-worker", role: "implementer", tree: "INFRA-1000", issue: "INFRA-1274" },
    claim: claimOf(),
  });

  expect(record.kind).toBe("evidence");
  expect(record.at).toBe(new Date(at).toISOString());
  expect(record.issue).toBe("INFRA-1274");
  expect(record.role).toBe("implementer");
  expect(record.toolCallId).toBe("toolu_stuck");
  expect(record.tool).toBe("bash");
  expect(record.arguments).toBe('command="sleep 100000" pty=true');
  expect(record.startedAt).toBe(new Date(START).toISOString());
  expect(record.openMs).toBe(65 * MINUTE);
  expect(record.allowedMs).toBe(60 * MINUTE);
  expect(record.rule).toEqual({ kind: "bound", boundMs: 60 * MINUTE });

  if (!record.tree.read) throw new Error(`the tree was not read: ${record.tree.reason}`);
  const process = record.tree.processes.find((candidate) => candidate.pid === pid);
  expect(process?.command).toContain("sleep 300");
  expect(process?.startedAt).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  expect(process?.state).toMatch(/^[A-Za-z]$/);
  expect(typeof process?.cpuMs).toBe("number");
  expect(process?.toolCallId).toBe("toolu_stuck");
});

test("the record's allowance is the call's own deadline plus the grace where it promised one", () => {
  const record = stallEvidenceRecord(
    evidenceOf({
      overdue: {
        rule: { kind: "deadline", deadlineMs: 5 * MINUTE, graceMs: 5 * MINUTE },
        openMs: 11 * MINUTE,
        pastMs: MINUTE,
      },
    }),
    START,
    { session: { kind: "controller" }, claim: undefined }
  );
  expect(record.allowedMs).toBe(10 * MINUTE);
  expect(record.session).toBe("controller");
  expect(record.issue).toBeUndefined();
  expect(record.role).toBeUndefined();
});

test("an argument far longer than the bound is cut to it rather than written whole", () => {
  const tail = "end-of-the-very-long-argument";
  const digest = stallArgumentsDigest({ command: `${"x".repeat(5_000)} ${tail}`, timeout: 0 });
  expect(digest.length).toBeLessThanOrEqual(STALL_DIGEST_LIMIT);
  expect(digest).not.toContain(tail);
  expect(digest).toContain("characters in all");
  expect(digest.startsWith('command="xxxx')).toBe(true);

  // The bound is a bound, whatever it is set to, and the marker is counted against it.
  expect(boundedText("x".repeat(100), 40).length).toBe(40);
  expect(boundedText("short", 40)).toBe("short");
  // A command's many lines read as one.
  expect(stallArgumentsDigest({ command: "one\n  two\n" })).toBe('command="one\\n two\\n"');
});
