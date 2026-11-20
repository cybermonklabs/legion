import type { CheckBlock, CheckResult } from "./dispatch-api";

/**
 * A check of `GET /api/v1/issues/{key}/results` as a reader of this build reads it. The answer
 * serves each fact the branch heads and a CI job's own results added in a field of its own, so
 * that every field a dispatch command released before them reads keeps its meaning
 * (`keepFieldsOlderCommandsRead` in packages/envoy/internal/dispatch/api/results.go). This puts
 * them back into the lists that command reads, one list each: the heads of `branch_heads` follow
 * `compared_with`; the results of `recorded_results` join `results`, the two lists the server
 * orders newest first merged by `created_at`, a tie keeping the derived result first; and a result
 * whose `at_branch_head` names a head stands at it.
 */
export function checkWithNewerFields(check: CheckBlock): CheckBlock {
  const { branch_heads: heads, recorded_results: recorded, ...rest } = check;
  const served = (check.results ?? []).map(standsAtItsBranchHead);
  // Array.prototype.sort is stable, so each list keeps the server's own order, ties included.
  const results =
    recorded === undefined || recorded.length === 0
      ? served
      : [...served, ...recorded].sort(
          (a, b) => Date.parse(b.created_at) - Date.parse(a.created_at)
        );
  return { ...rest, compared_with: [...(check.compared_with ?? []), ...(heads ?? [])], results };
}

/** A result whose commit is a branch head the check compares with, standing at that head. */
function standsAtItsBranchHead(result: CheckResult): CheckResult {
  const { at_branch_head: head, ...rest } = result;
  return head === undefined
    ? rest
    : { ...rest, commit_stands: { answer: "at_basis", basis: head } };
}
