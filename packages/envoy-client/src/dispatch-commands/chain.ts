import type {
  CheckResult,
  IssueChain,
  IssueChainCheck,
  IssueChainResult,
  IssueChainUnproven,
  IssueTreeParent,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import type { CommandContext, DispatchToolResult } from "./context";
import { optionalString, stringArg } from "./context";
import { commitStandsText, passStandsText, reasonText } from "./render-results";

/** `text` on one line: a block's words can span lines, and each node of the chain prints on one. */
function oneLine(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** A node by the block id its store id ends with (`<owner>#<block id>`), or by its id whole where
 *  it names no block: an issue's key, an ask's id. */
function shortId(id: string): string {
  const at = id.lastIndexOf("#");
  return at < 0 ? id : id.slice(at + 1);
}

/** Where a node stands in the tree: the block or the issue it hangs from. */
function parentText(parent: IssueTreeParent | null): string {
  if (parent === null) return "nothing";
  return parent.kind === "issue" ? parent.id : shortId(parent.id);
}

/** What a check or a plan task serves, by the requirements' block ids, or `nothing`. */
function servingText(serves: readonly string[] | null): string {
  const served = serves ?? [];
  return served.length === 0 ? "nothing" : served.map(shortId).join(" ");
}

/** One reason a check stands unproven, in words, naming the result it is about. A reason this
 *  build has no words for prints by its code, so a later server's reason still reaches the reader. */
function unprovenText(check: IssueChainCheck, unproven: IssueChainUnproven): string {
  const result = unproven.result ?? "";
  switch (unproven.reason) {
    case "no_result":
      return "it holds no result you may see";
    case "compared_nothing":
      return check.compared_nothing === undefined
        ? "it is compared with nothing"
        : `it is compared with nothing: ${reasonText(check.compared_nothing)}`;
    case "failed":
      return `result ${result} failed`;
    case "retracted":
      return `result ${result} is retracted`;
    case "check_words_moved":
      return `result ${result} ran against words the check no longer holds`;
    case "other_commit":
      return `result ${result} is at none of the compared commits`;
    case "commit_unknown":
      return `where the commit of result ${result} stands is not known`;
    default:
      return result === "" ? unproven.reason : `${unproven.reason} (result ${result})`;
  }
}

/** Whether the check's words moved after one result was recorded. */
function wordsText(result: CheckResult): string {
  if (result.check_words_moved === undefined) return "Whether the check's words moved is not known";
  return result.check_words_moved
    ? "The check's words moved after it"
    : "The check's words did not move after it";
}

/** One result of a check: its verdict, role and commit, where that commit stands, whether the
 *  check's words moved, its retraction, and whether it proves the check; then its reading. */
function resultLines(read: IssueChainResult): string[] {
  const result = read.result;
  const retracted =
    result.retracted === undefined
      ? ""
      : ` ${actorText(result.retracted.actor)} retracted it at ${result.retracted.created_at}: ${oneLine(result.retracted.reason).replace(/\.$/, "")}.`;
  const proves = read.proves ? "It proves the check." : "It proves nothing.";
  return [
    `  Result ${result.id}: ${result.verdict} by ${result.role} at ${result.commit_sha}. ${commitStandsText(result.commit_stands)}. ${wordsText(result)}.${retracted} ${proves}`,
    result.observed === ""
      ? "    The role recorded no reading."
      : `    Reading: ${oneLine(result.observed)}`,
  ];
}

/** One check, once: its place, what it serves, its words, whether a pass of it stands, why it is
 *  unproven where it is, and each of its results. */
function checkLines(
  check: IssueChainCheck,
  results: ReadonlyMap<string, IssueChainResult>
): string[] {
  const lines = [
    `Check ${check.block_id} of ${check.issue}, under ${parentText(check.parent)}, serving ${servingText(check.serves)}: ${oneLine(check.text)}`,
    `  ${passStandsText(check.pass_stands)}.`,
  ];
  const unproven = check.unproven ?? [];
  if (!check.proved && unproven.length > 0) {
    lines.push(`  Unproven: ${unproven.map((reason) => unprovenText(check, reason)).join("; ")}.`);
  }
  for (const id of check.results ?? []) {
    const result = results.get(id);
    if (result !== undefined) lines.push(...resultLines(result));
  }
  return lines;
}

/** The chain as `dispatch chain` prints it: the header, each requirement with its decisions and
 *  its checks, each check once with its standing and its results, each plan task, and the gaps. */
export function formatChain(chain: IssueChain): string {
  const requirements = chain.requirements ?? [];
  const decisions = new Map((chain.decisions ?? []).map((decision) => [decision.id, decision]));
  const checks = chain.checks ?? [];
  const byCheck = new Map(checks.map((check) => [check.id, check]));
  const results = new Map((chain.results ?? []).map((read) => [read.result.id, read]));
  const judged =
    chain.at === null
      ? "each check compared as dispatch results compares it"
      : `judged at ${chain.at}`;
  const lines = [
    `${chain.issue}: ${requirements.length} requirements, ${decisions.size} decisions, ${checks.length} checks, ${results.size} results, ${judged}.`,
  ];
  for (const requirement of requirements) {
    lines.push(
      `Requirement ${requirement.block_id}, ${requirement.proved ? "proved" : "unproven"}: ${oneLine(requirement.text)}`
    );
    const shaped = (requirement.decisions ?? []).flatMap((id) => decisions.get(id) ?? []);
    if (shaped.length === 0) lines.push("  No decision shaped it.");
    for (const decision of shaped) {
      // A person's decision is an answered ask, which has no block id of its own: its words are the
      // question it put and the answer the person gave.
      lines.push(
        decision.kind === "ask"
          ? `  Decision ask ${decision.id}: ${oneLine(decision.text)} The answer: ${oneLine(decision.answer ?? "")}`
          : `  Decision ${decision.block_id}: ${oneLine(decision.text)}`
      );
    }
    const serving = (requirement.checks ?? []).flatMap((id) => byCheck.get(id) ?? []);
    if (serving.length === 0) lines.push("  No check serves it.");
    for (const check of serving) {
      lines.push(
        `  Check ${check.block_id}, ${check.proved ? "proved" : "unproven"}: ${oneLine(check.text)}`
      );
    }
  }
  for (const check of checks) lines.push(...checkLines(check, results));
  for (const task of chain.plan_tasks ?? []) {
    lines.push(
      `Plan task ${task.block_id} of ${task.issue}, under ${parentText(task.parent)}, serving ${servingText(task.serves)}: ${oneLine(task.text)}`
    );
  }
  const gaps = chain.gaps ?? [];
  if (gaps.length === 0) {
    lines.push("Gaps: none.");
  } else {
    lines.push("Gaps:", ...gaps.map((gap) => `- ${gap.line}`));
  }
  return lines.join("\n");
}

/** `dispatch chain`: an issue's chain from each decision to the result that proves what it
 *  shaped, as Dispatch reads it in one read of the issue's tree. */
export async function chainCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const chain = await client.issueChain(stringArg(args, "issue"), optionalString(args, "at"));
  return {
    text: formatChain(chain),
    details: {
      issue: chain.issue,
      at: chain.at,
      requirements: (chain.requirements ?? []).length,
      checks: (chain.checks ?? []).length,
      results: (chain.results ?? []).length,
      gaps: (chain.gaps ?? []).map((gap) => gap.line),
    },
  };
}
