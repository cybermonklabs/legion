import type { Priorities, PriorityIssue, PriorityWork } from "@legion/contracts";
import { actorLabel, ISSUE_STATUSES } from "@legion/contracts";
import { type CommandContext, type DispatchToolResult, optionalStringArray } from "./context";
import { claimText, holdsSession, type LiveAgents, liveAgents } from "./render-issue";

/** The open work under one priority, as `dispatch priorities` prints it under the priority's
 *  own line. The server answers every lifecycle status and 0 where none stands there, so only
 *  the statuses that hold work are named, in the lifecycle's order; a status this build does
 *  not know is kept and printed last, since the count it carries is work all the same. */
function priorityWorkLine(work: PriorityWork): string {
  const place = (status: string): number => {
    const at = (ISSUE_STATUSES as readonly string[]).indexOf(status);
    return at === -1 ? ISSUE_STATUSES.length : at;
  };
  const standing = Object.entries(work.by_status ?? {})
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => place(a) - place(b) || a.localeCompare(b));
  const open = standing.reduce((total, [, count]) => total + count, 0);
  return [
    open === 0
      ? "no open issues"
      : `${open} open (${standing.map(([status, count]) => `${status} ${count}`).join(", ")})`,
    `${work.claimed} claimed`,
    `${work.open_asks} open ${work.open_asks === 1 ? "ask" : "asks"}`,
    `${work.closed_last_7_days} closed in the last 7 days`,
    work.newest_event_at === null ? "no events" : `newest event ${work.newest_event_at}`,
  ].join(" · ");
}

/** One row of either fault list: where the issue stands, who holds it, and the cell only its
 *  own list fills — the priorities a parked issue advances and when it was parked, which the
 *  work under no priority has neither of. */
function priorityIssueLine(issue: PriorityIssue, agents: LiveAgents | undefined): string {
  const priorities = issue.priorities ?? [];
  const moved =
    issue.moved_at === null
      ? ""
      : ` · parked ${issue.moved_at}${issue.moved_by === null ? "" : ` by ${actorLabel(issue.moved_by, agents?.titles)}`}`;
  return (
    `- ${issue.key} [${issue.status}] ${issue.title} · ${issue.project}` +
    (issue.assignee === null ? "" : ` · ${issue.assignee}`) +
    (issue.claim === null ? "" : ` · claimed by ${claimText(issue.claim, agents)}`) +
    (priorities.length === 0 ? "" : ` · advances ${priorities.join(", ")}`) +
    moved +
    ` · changed ${issue.changed_at}`
  );
}

/** One fault's heading and rows: its count, how that count falls out by project or by priority,
 *  and the issues the server listed, which are the newest of them where the count is larger. */
function priorityFaultLines(
  heading: string,
  fault: {
    readonly count: number;
    readonly issues?: readonly PriorityIssue[] | null;
    readonly by?: Readonly<Record<string, number>> | null;
  },
  agents: LiveAgents | undefined
): string[] {
  if (fault.count === 0) return [`${heading}: none`];
  const by = Object.entries(fault.by ?? {})
    .sort(([a, countA], [b, countB]) => countB - countA || a.localeCompare(b))
    .map(([name, count]) => `${name} ${count}`);
  const issues = fault.issues ?? [];
  const showing = issues.length < fault.count ? `; showing the newest ${issues.length}` : "";
  return [
    `${heading} (${fault.count}${by.length === 0 ? "" : `: ${by.join(", ")}`}${showing}):`,
    ...issues.map((issue) => priorityIssueLine(issue, agents)),
  ];
}

/** What `dispatch priorities` prints: every live priority in rank order with the work under it,
 *  then the two faults. A deployment that has written no priority is answered by the server
 *  with the sentence saying so, which is printed as it came and is not an error. */
function formatPriorities(response: Priorities, agents: LiveAgents | undefined): string {
  const priorities = response.priorities ?? [];
  const parked = response.priority_work_parked;
  const unplaced = response.work_under_no_priority;
  // The server writes this field only for a deployment holding no live priority (`omitempty`).
  if (response.empty !== undefined) return response.empty;
  return [
    `${priorities.length} ${priorities.length === 1 ? "priority" : "priorities"}.`,
    ...priorities.flatMap((priority) => [
      `${priority.id} · ${priority.title} · owner ${priority.owner === "" ? "nobody" : priority.owner}`,
      `  ${priorityWorkLine(priority.work)}`,
    ]),
    "",
    ...priorityFaultLines(
      "Work under no priority",
      { count: unplaced.count, issues: unplaced.issues, by: unplaced.by_project },
      agents
    ),
    "",
    ...priorityFaultLines(
      "Priority work parked",
      { count: parked.count, issues: parked.issues, by: parked.by_priority },
      agents
    ),
  ].join("\n");
}

/** `dispatch priorities`: every live priority with the work under it, and the two faults. */
export async function prioritiesCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const projects = optionalStringArray(args, "project") ?? [];
  const response = await client.priorities(projects);
  const faulted = [
    ...(response.work_under_no_priority.issues ?? []),
    ...(response.priority_work_parked.issues ?? []),
  ];
  const agents = await liveAgents(
    client,
    faulted.some((row) => holdsSession(row.claim))
  );
  return {
    text: formatPriorities(response, agents),
    details: {
      priorities: (response.priorities ?? []).map((priority) => priority.id),
      work_under_no_priority: response.work_under_no_priority.count,
      priority_work_parked: response.priority_work_parked.count,
      ...(projects.length === 0 ? {} : { project: projects }),
    },
  };
}
