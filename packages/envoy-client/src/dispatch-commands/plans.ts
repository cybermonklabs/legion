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

/** Each retro's rating with its reason, in the order the retros ran. The reason is a JSON string,
 *  so a quotation mark or a line break in it keeps the issue on one line. */
function ratingsText(ratings: readonly PlanRating[]): string {
  if (ratings.length === 0) return "ratings none";
  return `ratings ${ratings.map((rating) => `${rating.rating}/5 ${JSON.stringify(rating.reason)}`).join(", ")}`;
}

/** The issue's time in whole hours and the minutes left, rounded down, or `-` when Dispatch knows
 *  no start or no end of it. */
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
    ratingsText(issue.ratings ?? []),
    timeText(issue.duration_seconds),
    "failed steps",
    issue.failed_steps,
  ].join(" ");
}

/** `dispatch plans`: each finished issue of a project with its plan, its ratings, its time and its
 *  count of failed steps, as Dispatch lists them. */
export async function plansCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const since = optionalString(args, "since");
  const until = optionalString(args, "until");
  const read = await client.listProjectPlans(stringArg(args, "project"), { since, until });
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
