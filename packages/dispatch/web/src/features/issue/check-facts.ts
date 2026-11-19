import type { CheckBlock, CheckResult } from "../../api/types";
import type { NodeFact } from "./issue-tree";
import {
  CHECK_WHEN_WORDS,
  NODE_STATE_WORDS,
  RESULT_STANDS_WORDS,
  RESULT_VERDICT_WORDS,
} from "./page-words";

/**
 * What a check and a recorded result hold that their rows have no line for, as the facts their
 * bodies draw (dispatch://INFRA-1196 R15, `dispatch://INFRA-1196/artifact/plan-md#b-t16`).
 *
 * Pure, over the results read alone, so the body of a check is the same wherever the tree draws
 * it. Two rules decide what is here:
 *
 * - **A check draws all four of its attributes, blank or not.** That a check names no negative
 *   control is itself what a reader came to see, so the fact keeps its name with no value. A
 *   check CI proves also names the CI run that proves it, and, while that run has left it no
 *   result, the server's sentence saying why (`ci`, which the server gives no other check).
 * - **A result draws only what it recorded.** `Failed 0`, `Skipped 0` and `Excluded 0` took
 *   three rows under every pass on the old tab
 *   (`dispatch://INFRA-1196/artifact/design-notes-md` section 5, fault 9), so a count is drawn
 *   when it counts something, and a commit Dispatch could not place is drawn not at all.
 *
 * A `when` reads as words, a `run` as a command with its own copy control, and a screenshot as
 * a picture through the bytes route the page already serves.
 */

/** The four attributes of a check, in the order its body draws them, with the CI run that proves
 *  it after its own `run` where the server reads one: the result derived from that run is drawn
 *  as a recorded one is, under the check, and where the run proved nothing the check has no
 *  result at all, so its body says why in the server's own words (`ci.detail`). A block that
 *  carries `ci_check` and holds a recorded result is no check CI proves, and the server gives
 *  it no `ci`: it reads as the ordinary check it is. */
export function checkFacts(check: CheckBlock): NodeFact[] {
  const when: Readonly<Record<string, string>> = CHECK_WHEN_WORDS;
  const facts: NodeFact[] = [
    { draw: "word", name: "when", value: when[check.when] ?? "" },
    { draw: "text", name: "surface", value: check.surface },
    { draw: "command", name: "run", value: check.run },
  ];
  if (check.ci !== undefined) {
    facts.push({ draw: "text", name: "ci_check", value: check.ci.run });
    if ((check.results ?? []).length === 0) {
      facts.push({ draw: "text", name: "ci_unproved", value: check.ci.detail });
    }
  }
  facts.push({ draw: "text", name: "negative_control", value: check.negative_control });
  return facts;
}

/** What one recorded result says of itself: its verdict, whether it still speaks for the work,
 *  who ran it where and how, the commit it ran at, what it counted, and its screenshots. The
 *  result's own `observed` is the node's text and is no fact of its own. */
export function resultFacts(result: CheckResult): NodeFact[] {
  // "Up to date" is both halves of what the removed `currency` meant: at the commit the work
  // stands at, and against the words the result proved. A check reworded since the result ran
  // keeps the first and loses the second, so the result is out of date whatever its commit is
  // (`check_words_moved`, which `tree-drafts.ts`'s `checkState` reads for the same reason), and
  // that is read before where its commit stands. The server reads both per result
  // (`results_comparison.go`).
  const stands =
    result.check_words_moved === true
      ? NODE_STATE_WORDS.check.older
      : result.commit_stands === undefined
        ? undefined
        : RESULT_STANDS_WORDS[result.commit_stands.answer];
  const verdicts: Readonly<Record<string, string>> = RESULT_VERDICT_WORDS;
  const facts: NodeFact[] = [
    { draw: "word", name: "verdict", value: verdicts[result.verdict] ?? result.verdict },
  ];
  if (stands !== undefined) facts.push({ draw: "word", name: "stands", value: stands });
  for (const [name, value] of [
    ["role", result.role],
    ["surface", result.surface],
  ] as const) {
    if (value !== "") facts.push({ draw: "text", name, value });
  }
  if (result.run !== "") facts.push({ draw: "command", name: "run", value: result.run });
  for (const [name, value] of [
    ["commit", result.commit_sha],
    ["negative_control", result.negative_control],
  ] as const) {
    if (value !== "") facts.push({ draw: "text", name, value });
  }
  for (const [name, count] of [
    ["failed", result.failed],
    ["skipped", result.skipped],
    ["excluded", result.excluded],
  ] as const) {
    if (count > 0) facts.push({ draw: "text", name, value: String(count) });
  }
  for (const screenshot of result.screenshots ?? []) {
    facts.push({ draw: "picture", name: "screenshot", value: screenshot });
  }
  return facts;
}
