import { describe, expect, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { expectNoMissingHelpers, nameMissingHelpers, scriptFunctions } from "./script-functions";

// assert_review_round_handoff, taken from lib/workflow.sh by name. A changes-requested review ends
// its round whether or not the reviewer has completed, so the issue can stand in implementing
// before the daemon records the reviewer's completion and the handoff record it names. The check
// of that record must wait for the completion, not read the state once. The fake daemon state
// below holds no reviewer handoff record for its first reads, as at a round whose review landed
// before the reviewer completed, and holds one after.
const path = join(import.meta.dir, "workflow.sh");
const fn = scriptFunctions(path);
const dir = temporaryDirectory("review-round-handoff-test");

function run(readsBeforeCompletion: number) {
  const reads = join(dir, `reads-${Math.random().toString(36).slice(2)}`);
  const checked = join(dir, `checked-${Math.random().toString(36).slice(2)}`);
  writeFileSync(reads, "0");
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
${nameMissingHelpers(path)}
fail() { printf 'FAIL %s\\n' "$*" >&2; exit 1; }
daemon_state() {
  local n
  n=$(cat "$READS")
  printf '%s' $((n + 1)) >"$READS"
  if [ "$n" -lt "$BEFORE" ]; then
    printf '%s\\n' '{"issues":{"LEG-1":{"phase":"implementing","workers":{"reviewer":{}}}}}'
  else
    printf '%s\\n' '{"issues":{"LEG-1":{"phase":"implementing","workers":{"reviewer":{"handoffRecord":{"id":"h-1","version":2}}}}}}'
  fi
}
until_true() {
  local limit=$1 what=$2 i
  shift 2
  for ((i = 0; i < limit; i++)); do
    if "$@"; then return 0; fi
  done
  fail "timed out waiting for $what"
}
assert_handoff_record() {
  local id
  id=$(daemon_state | jq -r --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].handoffRecord.id // empty')
  [ -n "$id" ] || fail "$1 has no $2 $3 round $4 handoff record in the daemon's state"
  printf '%s %s %s %s %s\\n' "$1" "$2" "$3" "$4" "$id" >"$CHECKED"
}
${fn("reviewer_completed")}
${fn("assert_review_round_handoff")}
assert_review_round_handoff LEG-1 2
`,
    ],
    {
      env: {
        PATH: process.env.PATH,
        READS: reads,
        CHECKED: checked,
        BEFORE: String(readsBeforeCompletion),
      },
    }
  );
  const stdout = result.stdout.toString();
  const stderr = result.stderr.toString();
  expectNoMissingHelpers(stdout, stderr);
  let seen = "";
  try {
    seen = readFileSync(checked, "utf8").trim();
  } catch {}
  return { code: result.exitCode, stderr: stderr.trim(), seen };
}

describe("assert_review_round_handoff", () => {
  test("waits for the reviewer's completion that comes after the round ended", () => {
    const { code, stderr, seen } = run(3);
    expect(stderr).toBe("");
    expect(code).toBe(0);
    expect(seen).toBe("LEG-1 reviewer reviewing 2 h-1");
  });

  test("checks the record at once when the completion came first", () => {
    const { code, seen } = run(0);
    expect(code).toBe(0);
    expect(seen).toBe("LEG-1 reviewer reviewing 2 h-1");
  });
});
