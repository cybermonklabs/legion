import type { FinishedIssue, PlanRating, PlanRecord } from "@legion/contracts";
import { LEGION_PLAN_RATING_MOST } from "@legion/contracts";
import type { CommandContext, DispatchToolResult } from "./context";
import { optionalString, stringArg } from "./context";

/** The plan an issue's newest run ran from: its source, how many tasks it has and the steps of the
 *  first, or `none` for an issue with no plan. */
function planText(plan: PlanRecord | null): string {
  if (plan === null) return "plan none";
  const tasks = plan.tasks ?? [];
  const first = tasks[0]?.steps ?? [];
  return `plan ${plan.source} (${tasks.length} tasks: ${first.length === 0 ? "-" : first.join(" ")})`;
}

/** The characters other than a line feed and a carriage return that a reader splitting lines the
 *  Unicode way breaks a line at, and JSON leaves as they are: NEXT LINE, LINE SEPARATOR and
 *  PARAGRAPH SEPARATOR. (JSON escapes every other one, each below U+0020.) */
const unescapedLineBreaks = /[\u0085\u2028\u2029]/g;

/** `text` as a JSON string that holds no line break: JSON's own escapes, and the `\u` escape of
 *  each of `unescapedLineBreaks`, which reads back as the same string. */
function oneLineJson(text: string): string {
  return JSON.stringify(text).replace(
    unescapedLineBreaks,
    (character) => `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`
  );
}

/** Each retro's rating out of the most of the contract's scale (`LEGION_PLAN_RATING_MOST`) with its
 *  reason, in the order the retros ran. The reason is a JSON string with no line break in it, so a
 *  quotation mark or a line break of any kind in the reason keeps the issue on one line. */
function ratingsText(ratings: readonly PlanRating[] | null): string {
  if (ratings === null || ratings.length === 0) return "ratings none";
  return `ratings ${ratings.map((rating) => `${rating.rating}/${LEGION_PLAN_RATING_MOST} ${oneLineJson(rating.reason)}`).join(", ")}`;
}

/** The issue's time in whole hours and the minutes left, rounded down, or `-` where Dispatch counts
 *  none: it knows no start of the issue, or no end, or the latest end is before the earliest
 *  start. */
function timeText(seconds: number | null): string {
  if (seconds === null) return "time -";
  const minutes = Math.floor(seconds / 60);
  return `time ${Math.floor(minutes / 60)}h${minutes % 60}m`;
}

/** One finished issue: its close, plan, ratings, time and failed steps, and last the experiment a
 *  draw put it in with the variant drawn, where its newest run's records name one. A Dispatch
 *  older than the two fields answers an issue without them, which names none either. */
function issueLine(issue: FinishedIssue): string {
  return [
    issue.issue,
    "closed",
    issue.closed_at,
    planText(issue.plan),
    ratingsText(issue.ratings),
    timeText(issue.duration_seconds),
    "failed steps",
    issue.failed_steps,
    ...(issue.experiment == null || issue.variant == null
      ? []
      : [`variant ${issue.experiment}/${issue.variant}`]),
  ].join(" ");
}

/** `dispatch plans`: each finished issue of a project with its plan, its ratings, its time, its
 *  count of failed steps and its variant, as Dispatch lists them. */
export async function plansCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const read = await client.listProjectPlans(stringArg(args, "project"), {
    since: optionalString(args, "since"),
    until: optionalString(args, "until"),
  });
  const issues = read.issues ?? [];
  return {
    text: [`${read.project}: ${issues.length} finished issues`, ...issues.map(issueLine)].join(
      "\n"
    ),
    details: { project: read.project, issues },
  };
}
