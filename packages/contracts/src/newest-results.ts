import type { CheckBlock } from "./dispatch-api";

/**
 * The newest result of each role at what a recorded check compared with, pass and fail alike,
 * oldest first, in the words `dispatch results` prints on a check's head line and the dashboard's
 * Verification tab shows beside its answer: `Newest at that commit: implementer: pass (2120a467);
 * tester: fail (9f5fd8c4, newer)`. The roles of the passes that stand, alone, read as a verdict and
 * hid a newer fail beside a pass; this names both, and leaves whether a pass stands to Dispatch's
 * own answer.
 *
 * A result counts where its commit stands at one of the compared commits (`at_basis`). A retracted
 * result proves nothing and is left out. The API serves a check's results newest first, so the
 * first result of each role is its newest. Each is named by the first eight characters of its id,
 * which its own line names whole. One recorded later than the one before it in the list says
 * `newer`, and one the check's words moved after says so. Where the check compared with heads or
 * named commits the place is `at that commit` or `at those commits`, where with branches `on that
 * branch` or `on those branches`, and for a mix `at what the check compared with`.
 *
 * Undefined where the check compared with nothing, or no result stands at a compared commit.
 */
export function newestResultsText(
  check: Pick<CheckBlock, "compared_with" | "results">
): string | undefined {
  const bases = check.compared_with ?? [];
  const newest = new Map<string, NonNullable<CheckBlock["results"]>[number]>();
  for (const result of check.results ?? []) {
    if (result.commit_stands?.answer !== "at_basis" || result.retracted !== undefined) continue;
    if (!newest.has(result.role)) newest.set(result.role, result);
  }
  if (bases.length === 0 || newest.size === 0) return undefined;
  const ordered = [...newest.values()].reverse();
  const named = ordered.map((result, at) => {
    const notes = [result.id.slice(0, 8)];
    if (at > 0 && result.created_at !== ordered[at - 1]?.created_at) notes.push("newer");
    if (result.check_words_moved === true) notes.push("the check's words moved after it");
    return `${result.role}: ${result.verdict} (${notes.join(", ")})`;
  });
  const branches = bases.filter((basis) => basis.kind === "branch").length;
  const place =
    branches === 0
      ? bases.length > 1
        ? "at those commits"
        : "at that commit"
      : branches === bases.length
        ? bases.length > 1
          ? "on those branches"
          : "on that branch"
        : "at what the check compared with";
  return `Newest ${place}: ${named.join("; ")}`;
}
