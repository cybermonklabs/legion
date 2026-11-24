import { describe, expect, test } from "bun:test";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { jsonl } from "./omp-session-fixtures";
import { scriptFunctions } from "./script-functions";

// worker_notices (lib/workflow.sh), the read Stage 3's notices-reach-architects-alone check fails
// on: a line for each workflow notice delivered into a phase-worker session. It knows a notice by
// its kind, which its jq capture names by hand, so a kind the daemon publishes and the capture
// lacks reaching a worker's session passes the check. The kinds are read from the daemon's own
// list, validNoticeKinds in packages/daemon/internal/record/outbox.go (string literals and the
// NoticeKind constants it names), so a kind added there and not to the capture fails here.
const root = join(import.meta.dir, "..", "..", "..");
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));
const dir = temporaryDirectory("worker-notices-test");

function daemonKinds(): string[] {
  const source = readFileSync(
    join(root, "packages", "daemon", "internal", "record", "outbox.go"),
    "utf8"
  );
  const list = /^var validNoticeKinds = \[\]NoticeKind\{([\s\S]*?)^\}/m.exec(source);
  if (list === null) throw new Error("outbox.go declares no validNoticeKinds list");
  return list[1]
    .replace(/\/\/.*$/gm, "")
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item !== "")
    .map((item) => {
      const literal = /^"([^"]+)"$/.exec(item);
      if (literal !== null) return literal[1];
      const constant = new RegExp(`\\b${item}\\s+NoticeKind = "([^"]+)"`).exec(source);
      if (constant === null) throw new Error(`outbox.go declares no NoticeKind constant ${item}`);
      return constant[1];
    });
}

function captureKinds(): string[] {
  const capture = /summary: \(\?<summary>\(([^)]*)\) on /.exec(fn("worker_notices"));
  if (capture === null) throw new Error("worker_notices holds no summary capture of notice kinds");
  return capture[1].split("|");
}

// worker_notices over a session directory holding one phase worker's session, whose Envoy
// deliveries carry each of summaries.
function workerNotices(name: string, summaries: string[]): string[] {
  const sessions = join(dir, name);
  mkdirSync(sessions, { recursive: true });
  const lines = [
    {
      type: "custom_message",
      customType: "envoy-role-claim",
      data: { role: "legsmoke-7-implementer" },
    },
    ...summaries.map((summary) => ({
      type: "custom_message",
      customType: "envoy-message",
      content: `envoy:\n  summary: ${summary}\n`,
    })),
  ];
  writeFileSync(join(sessions, "worker.jsonl"), jsonl(lines));
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -uo pipefail
${fn("worker_sessions")}
${fn("worker_notices")}
worker_notices "$SESSIONS"
`,
    ],
    { env: { PATH: process.env.PATH ?? "", SESSIONS: sessions } }
  );
  expect(result.stderr.toString()).toBe("");
  expect(result.exitCode).toBe(0);
  return result.stdout
    .toString()
    .split("\n")
    .filter((line) => line !== "");
}

describe("worker_notices", () => {
  test("names every notice kind the daemon publishes, and no other", () => {
    const daemon = daemonKinds();
    expect(daemon).toContain("phase-finished");
    expect(daemon).toContain("owner-step");
    expect(captureKinds().sort()).toEqual(daemon.sort());
  });

  test("prints one line for each notice kind delivered into a phase worker's session", () => {
    const kinds = daemonKinds();
    const got = workerNotices(
      "every-kind",
      kinds.map((kind) => `${kind} on LEGSMOKE-7`)
    );
    expect(got).toEqual(
      kinds.map((kind) => `worker.jsonl legsmoke-7-implementer ${kind} on LEGSMOKE-7`)
    );
  });

  test("prints the owner's step notice reaching a worker, and nothing for a message that is no notice", () => {
    expect(workerNotices("owner-step", ["owner-step on LEGSMOKE-7"])).toEqual([
      "worker.jsonl legsmoke-7-implementer owner-step on LEGSMOKE-7",
    ]);
    expect(workerNotices("no-notice", ["a question on LEGSMOKE-7"])).toEqual([]);
  });
});
