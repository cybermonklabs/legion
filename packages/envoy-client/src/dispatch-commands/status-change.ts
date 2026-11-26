import type {
  ISSUE_STATUS_OPTIONS,
  IssueDetails,
  StatusChangeAnswer,
  StatusChangeEntry,
} from "@legion/contracts";
import {
  type DispatchClient,
  DispatchServiceError,
  DispatchWriteLandedError,
} from "../dispatch-http";
import { issueTopic } from "../dispatch-owner";
import { type CommandContext, type DispatchToolResult, stringArg } from "./context";
import { type ClosingNote, closeAccount, refusalWithCode, withAccount } from "./failures";
import { notSubscribed } from "./results";

type IssueStatusOption = (typeof ISSUE_STATUS_OPTIONS)[number];

/** An issue as a read before a status change found it: its status, and its parent. */
interface IssueBefore {
  readonly status: string;
  readonly parent: string | null;
}

/**
 * What a status change's options may move, as it stood before the call: the issue the call names
 * and its children, which the read of that issue holds. Where an option moves the open issues
 * deeper under it (include_children), every issue of its project, since a parent is always in its
 * child's project. Where an option moves only the issues above it (include_parent), each of them,
 * read one parent at a time up to the root, which Dispatch keeps at most 32 levels above. The
 * route's answer names the status and the parent each issue has after the call, so this read is
 * what each line's "before" comes from. It is made before anything is written, so a read that
 * fails sends nothing.
 */
export async function issuesBeforeStatusChange(
  client: DispatchClient,
  before: IssueDetails,
  options: readonly IssueStatusOption[]
): Promise<ReadonlyMap<string, IssueBefore>> {
  const issues = new Map<string, IssueBefore>([
    [before.key, { status: before.status, parent: before.parent ?? null }],
  ]);
  for (const child of before.children ?? []) {
    issues.set(child.key, { status: child.status, parent: before.key });
  }
  try {
    if (options.includes("include_children")) {
      for (const row of await client.listAllIssues({ project: before.project })) {
        issues.set(row.key, { status: row.status, parent: row.parent ?? null });
      }
    } else if (options.includes("include_parent")) {
      let parent = before.parent ?? null;
      while (parent !== null && !issues.has(parent)) {
        const above = await client.getIssue(parent);
        issues.set(above.key, { status: above.status, parent: above.parent ?? null });
        parent = above.parent ?? null;
      }
    }
  } catch (error) {
    throw refusalWithCode(error);
  }
  return issues;
}

/**
 * The status change of `dispatch issue-update` with an option (`POST /api/v1/issues/{key}/status-change`),
 * sent after the closing reason, where the call has one, was posted. It prints one line for each
 * issue the call changed, in the order its steps ran: `KEY: status a -> b`, or `KEY: unlinked
 * from P` for a child the call removed from the issue. The issue the call names carries the
 * reason's line too, and is printed last when the call left its status as it was. A refusal keeps
 * what the steps before it changed, and the closing reason's account where one landed.
 */
export async function statusChangeCommand(
  context: CommandContext,
  before: IssueDetails,
  options: readonly IssueStatusOption[],
  issuesBefore: ReadonlyMap<string, IssueBefore>,
  closingNote: ClosingNote | undefined
): Promise<DispatchToolResult> {
  const { args, client, actor, input, issue } = context;
  const status = stringArg(args, "status");
  const change = (entry: StatusChangeEntry): string => {
    const was = issuesBefore.get(entry.key);
    if (entry.key !== before.key && entry.parent === null && was?.parent === before.key) {
      return `unlinked from ${before.key}`;
    }
    return was === undefined
      ? `status -> ${entry.status}`
      : `status ${was.status} -> ${entry.status}`;
  };
  let answer: StatusChangeAnswer;
  try {
    answer = await client.changeIssueStatus(issue(), {
      status,
      ...(options.includes("include_children") ? { include_children: true } : {}),
      ...(options.includes("unlink_children") ? { unlink_children: true } : {}),
      ...(options.includes("include_parent") ? { include_parent: true } : {}),
      actor,
    });
  } catch (error) {
    // The status change is this call's own last write: one that landed without its answer is
    // the call's landing, which the caller reports as done.
    if (error instanceof DispatchWriteLandedError) throw error;
    // A call refused midway committed the steps before the refusal, each a write of its own.
    const committed = error instanceof DispatchServiceError ? (error.changed ?? []) : [];
    const changed =
      committed.length === 0
        ? ""
        : `the steps before the refusal changed ${committed.map((entry) => `${entry.key} (${change(entry)})`).join(", ")}`;
    if (closingNote === undefined) throw refusalWithCode(error, changed);
    const landed = closeAccount(error, closingNote);
    if (error instanceof DispatchServiceError) throw refusalWithCode(error, changed, landed);
    throw withAccount(error, landed);
  }
  const suffix = await notSubscribed(issueTopic(before.key), input.subscriptions);
  const namedLine = (after: string): string =>
    `${before.key}: ${[
      ...(closingNote === undefined
        ? []
        : [`reason posted as message ${closingNote.id} (${closingNote.ref})`]),
      `status ${before.status} -> ${after}`,
    ].join("; ")}${suffix}`;
  const changed = answer.changed ?? [];
  const lines = changed.map((entry) =>
    entry.key === before.key ? namedLine(entry.status) : `${entry.key}: ${change(entry)}`
  );
  if (!changed.some((entry) => entry.key === before.key))
    lines.push(namedLine(answer.issue.status));
  return {
    text: lines.join("\n"),
    details: {
      issue: before.key,
      status: answer.issue.status,
      external_links: (answer.issue.external_links ?? []).map((link) => link.url),
      changed: changed.map(({ key, status, parent }) => ({ key, status, parent })),
      ...(closingNote === undefined ? {} : { message: closingNote.id }),
    },
  };
}
