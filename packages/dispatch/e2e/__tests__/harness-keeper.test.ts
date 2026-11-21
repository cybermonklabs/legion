import { afterAll, beforeAll, expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { pythonInterpreter } from "@legion/test-fixtures/host-tools";
import { socketPathRefusal } from "@legion/test-fixtures/socket-root";

import { keeperDirectory } from "../harness-start";
import { rowLimit } from "./bounded-subprocess";

// The directory the harness's port keepers serve their unix sockets in (`keeperDirectory`, e2e/
// harness-start.ts), under a TMPDIR long enough that a keeper's socket under it passes the 107
// bytes Linux holds: the keeper is `held-port.py keep`, Python, which refuses such a path. Every
// TMPDIR here is a directory of this file's own under a short root in /tmp, so the rows read the
// same whatever TMPDIR the run was given. The ids are two-process mode's held servers.
const ids = ["dispatch-a", "dispatch-b"];
const heldPort = fileURLToPath(
  new URL("../../../../scripts/e2e/lib/held-port.py", import.meta.url)
);

restoreEnvironment(["TMPDIR"]);

/** The short root every TMPDIR of this file is made in, and the directories a row made. */
let root = "";
let python = "";
const made: string[] = [];
beforeAll(() => {
  root = mkdtempSync("/tmp/hk-");
  python = pythonInterpreter();
});
afterAll(() => {
  for (const dir of made.splice(0)) rmSync(dir, { force: true, recursive: true });
  rmSync(root, { force: true, recursive: true });
});

/** A fresh directory under `root` whose path is exactly `bytes` bytes, made TMPDIR. */
function tmpdirOf(bytes: number): string {
  const dir = join(root, "t".repeat(bytes - root.length - 1));
  mkdirSync(dir);
  process.env.TMPDIR = dir;
  return dir;
}

/** `held-port.py keep` serving `socket`, as the harness starts it (`keepPort`): the port it held,
 *  or the refusal it exited with. Bounded at the short bound, and stopped either way. */
async function keep(socket: string): Promise<{ port?: number; refusal?: string }> {
  const portFile = `${socket}.port`;
  const keeper = Bun.spawn(
    [python, heldPort, "keep", "--port-file", portFile, "--socket", socket],
    { stdout: "ignore", stderr: "pipe" }
  );
  try {
    const deadline = Date.now() + 30_000;
    // The keeper writes the file through a rename once it serves its socket; a keeper that cannot
    // serve it exits first, naming why.
    while (!existsSync(portFile) && keeper.exitCode === null && Date.now() < deadline) {
      await Bun.sleep(50);
    }
    if (existsSync(portFile)) return { port: Number(readFileSync(portFile, "utf8").trim()) };
    keeper.kill("SIGKILL");
    await keeper.exited;
    return { refusal: (await new Response(keeper.stderr).text()).trim() };
  } finally {
    keeper.kill("SIGTERM");
    await keeper.exited;
  }
}

test(
  "under a TMPDIR the keepers' sockets do not fit under, their directory is under /tmp and they serve there",
  async () => {
    const tmp = tmpdirOf(120);
    // The control: a directory made directly under that TMPDIR, as the harness made it before,
    // which the keeper refuses, so this row fails if the directory stops falling back.
    const direct = mkdtempSync(join(tmp, "dispatch-e2e-held-"));
    made.push(direct);
    const refused = await keep(join(direct, "dispatch-b.sock"));
    expect(refused.refusal).toContain("AF_UNIX path too long");

    const dir = keeperDirectory(ids);
    made.push(dir);
    expect(dir.startsWith("/tmp/dispatch-e2e-held-")).toBe(true);
    for (const id of ids) {
      const socket = join(dir, `${id}.sock`);
      expect(socketPathRefusal(socket)).toBeUndefined();
      const held = await keep(socket);
      expect(held.refusal).toBeUndefined();
      expect(held.port).toBeGreaterThan(0);
    }
  },
  rowLimit("short", "short", "short")
);

test("under a TMPDIR the keepers' sockets fit under, their directory is under that TMPDIR", () => {
  const tmp = tmpdirOf(root.length + 2);
  const dir = keeperDirectory(ids);
  made.push(dir);
  expect(dir.startsWith(`${tmp}/dispatch-e2e-held-`)).toBe(true);
});
