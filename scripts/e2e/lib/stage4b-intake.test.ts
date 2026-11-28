import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// Stage 4b's stream_carries, the preflight's read of whether production NATS's stream carries each
// half of the workflow's intake, against a nats_stream that answers as lib/nats-stream.ts does: a
// message (exit 0), no message by the read's budget (exit 3, which cannot tell a stream that holds
// none from a server that did not answer), or a connect that failed (exit 1). Only the first is a
// verdict; the other two block the run as UNKNOWN, never as a stream that carries no message.
const fn = scriptFunctions(join(import.meta.dir, "..", "stage4b-sandbox-tree.sh"));

function carries(status: number) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
stream=notifications nats_url=nats://nats.example:4222
note() { echo "   $*"; }
blocked() { echo "CHECK preflight: BLOCKED: $*"; exit 1; }
nats_stream() {
  case $STATUS in
  0) echo '{"subject":"notifications.github.sjawhar.legion-smoke.1","time":"2026-11-21T00:00:00.000Z","seq":7}' ;;
  3) echo "nats-stream: no message on $4 from stream $3 within $5 s: unknown whether it holds one" >&2; exit 3 ;;
  *) echo "nats-stream: could not connect to NATS at <nats-url>: refused" >&2; exit 1 ;;
  esac
}
${fn("stream_carries")}
stream_carries 'notifications.github.sjawhar.legion-smoke.>'
`,
    ],
    { env: { PATH: process.env.PATH ?? "", STATUS: String(status) } }
  );
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

describe("stream_carries", () => {
  test("notes the newest message the stream carries", () => {
    const result = carries(0);
    expect(result.stdout).toBe(
      `   production NATS notifications carries notifications.github.sjawhar.legion-smoke.>: newest {"subject":"notifications.github.sjawhar.legion-smoke.1","time":"2026-11-21T00:00:00.000Z","seq":7}\n`
    );
    expect(result.code).toBe(0);
  });

  test("blocks a read with no message by its budget as UNKNOWN, not as a stream that carries none", () => {
    const result = carries(3);
    expect(result.stdout).toBe(
      "CHECK preflight: BLOCKED: UNKNOWN whether production NATS notifications carries a message on notifications.github.sjawhar.legion-smoke.>: none came within 20 s, and a stream with none and a server that did not answer read the same (bun scripts/e2e/lib/nats-stream.ts last $LEGION_E2E_NATS_URL notifications 'notifications.github.sjawhar.legion-smoke.>'); the daemon's intake would miss that half of the workflow if it holds none\n"
    );
    expect(result.stderr).toContain("within 20 s: unknown whether it holds one");
    expect(result.code).toBe(1);
  });

  test("blocks a read that failed before it could say as UNKNOWN", () => {
    const result = carries(1);
    expect(result.stdout).toBe(
      "CHECK preflight: BLOCKED: UNKNOWN whether production NATS notifications carries a message on notifications.github.sjawhar.legion-smoke.>: the read exited 1 before it could say (bun scripts/e2e/lib/nats-stream.ts last $LEGION_E2E_NATS_URL notifications 'notifications.github.sjawhar.legion-smoke.>')\n"
    );
    expect(result.code).toBe(1);
  });
});
