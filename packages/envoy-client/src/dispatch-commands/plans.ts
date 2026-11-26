import type { FinishedIssue, PlanRating, PlanRecord } from "@legion/contracts";
import { DISPATCH_PLANS_DEFAULT_COUNT, LEGION_PLAN_RATING_MOST } from "@legion/contracts";
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

/** `dispatch plans`: finished issues of a project with their plan, their ratings, their time, their
 *  count of failed steps and their variant, newest close first, as Dispatch lists them. With no
 *  `since`, the newest DISPATCH_PLANS_DEFAULT_COUNT that have a plan or a rating, which Dispatch
 *  keeps and counts (`planned_or_rated` and `limit`), under a header that counts them and every
 *  finished issue; with `since`, every finished issue of the window, so that read sends neither. */
export async function plansCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const project = stringArg(args, "project");
  const since = optionalString(args, "since");
  const until = optionalString(args, "until");
  if (since !== undefined) {
    const read = await client.listProjectPlans(project, { since, until });
    const finished = read.issues ?? [];
    return {
      text: [
        `${read.project}: ${finished.length} finished issues`,
        ...finished.map(issueLine),
      ].join("\n"),
      details: { project: read.project, issues: finished },
    };
  }
  const read = await client.listProjectPlans(project, {
    until,
    planned_or_rated: true,
    limit: DISPATCH_PLANS_DEFAULT_COUNT,
  });
  const finished = read.issues ?? [];
  // What an owner can learn from: an issue that ran from a plan, or that a retro rated. Dispatch
  // answers only those, at most the limit, with both counts; one built before planned_or_rated and
  // limit leaves both out and answers every finished issue with no count, so the command keeps and
  // counts the issues itself, and each step below keeps all of an answer that is already kept.
  const planned = finished.filter(
    (issue) => issue.plan !== null || (issue.ratings ?? []).length > 0
  );
  const listed = planned.slice(0, DISPATCH_PLANS_DEFAULT_COUNT);
  const header =
    `${read.project}: ${listed.length} of ${read.total ?? planned.length} finished issues with a plan or a rating; ` +
    `${read.finished_total ?? finished.length} finished in all; --since <RFC 3339 time> prints every finished issue closed since then`;
  return {
    text: [header, ...listed.map(issueLine)].join("\n"),
    details: { project: read.project, issues: listed },
  };
}
