import type { FinishedIssue, PlanRating, PlanRecord } from "@legion/contracts";
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

/** The most of the scale a retro rates the plan on: each rating prints out of it. It is the
 *  contract's `LEGION_PLAN_RATING_MOST`, typed here so that the dispatch command's bundle stays
 *  without the daemon's API module, `@legion/contracts/legion-api`; the plans command's test holds
 *  the two together. */
const PLAN_RATING_MOST = 5;

/** Each retro's rating out of `PLAN_RATING_MOST` with its reason, in the order the retros ran.
 *  The reason is a JSON string with no line break in it, so a quotation mark or a line break of
 *  any kind in the reason keeps the issue on one line. */
function ratingsText(ratings: readonly PlanRating[] | null): string {
  if (ratings === null || ratings.length === 0) return "ratings none";
  return `ratings ${ratings.map((rating) => `${rating.rating}/${PLAN_RATING_MOST} ${oneLineJson(rating.reason)}`).join(", ")}`;
}

/** The issue's time in whole hours and the minutes left, rounded down, or `-` where Dispatch counts
 *  none: it knows no start of the issue, or no end, or the latest end is before the earliest
 *  start. */
function timeText(seconds: number | null): string {
  if (seconds === null) return "time -";
  const minutes = Math.floor(seconds / 60);
  return `time ${Math.floor(minutes / 60)}h${minutes % 60}m`;
}

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
  ].join(" ");
}

/** `dispatch plans`: each finished issue of a project with its plan, its ratings, its time and its
 *  count of failed steps, as Dispatch lists them. */
export async function plansCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const read = await client.listProjectPlans(stringArg(args, "project"), {
    since: optionalString(args, "since"),
    until: optionalString(args, "until"),
  });
  // The generated shape lets each list be null; Dispatch writes `[]`, and null reads the same, here
  // and in each part of an issue's line.
  const issues = read.issues ?? [];
  return {
    text: [`${read.project}: ${issues.length} finished issues`, ...issues.map(issueLine)].join(
      "\n"
    ),
    details: { project: read.project, issues },
  };
}
