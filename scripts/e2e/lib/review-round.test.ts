import { expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// The review round is a count of returns, which the daemon keeps where it keeps each of them
// (record.ReturnCounts, packages/daemon/internal/record/plan.go): on the plan's task for an issue
// with plan rows at its generation, and on the implementer's phases row for an issue with none.
// assert_handoff_record and assert_round_handoff read it with lib/workflow.sh's review_round, and a
// read of the phases row alone answers 0 for an issue with plan rows, so a live run fails from round
// 1. The drivers run only live, so this drives review_round against a stubbed db_value that answers
// as such a database: whether the issue has plan rows at its generation, the round on its task, and
// the round on its implementer's phases row.
const fn = scriptFunctions(join(import.meta.dir, "workflow.sh"));

function reviewRound(db: { plan: boolean; task: string; phases: string }) {
  const result = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
db_value() {
  case "$1" in
    *issue_plans*) if [ "$PLAN" = 1 ]; then printf t; else printf f; fi ;;
    *plan_tasks*) printf '%s' "$TASK" ;;
    *phases*) printf '%s' "$PHASES" ;;
    *) printf 'unexpected query: %s\\n' "$1" >&2; return 1 ;;
  esac
}
${fn("review_round")}
review_round LEGION-208`,
    ],
    { env: { ...process.env, PLAN: db.plan ? "1" : "0", TASK: db.task, PHASES: db.phases } }
  );
  return {
    exit: result.exitCode,
    round: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

test("review_round reads the round on the plan's task for an issue with plan rows", () => {
  expect(reviewRound({ plan: true, task: "2", phases: "0" })).toEqual({
    exit: 0,
    round: "2",
    stderr: "",
  });
});

test("review_round reads the round on the implementer's phases row for an issue with no plan rows", () => {
  expect(reviewRound({ plan: false, task: "2", phases: "1" })).toEqual({
    exit: 0,
    round: "1",
    stderr: "",
  });
});
