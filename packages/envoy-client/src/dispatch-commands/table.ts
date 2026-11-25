import type { dispatchToolSpecs } from "@legion/contracts";
import { apiCommand } from "./api";
import {
  askCommand,
  editAskCommand,
  followCommand,
  openAsksCommand,
  resolveAskCommand,
} from "./asks";
import type { CommandContext, DispatchToolResult, ReadingContext, SessionContext } from "./context";
import {
  commentCommand,
  deleteCommentCommand,
  deleteMessageCommand,
  editCommentCommand,
  editMessageCommand,
  messageCommand,
  resolveCommentCommand,
  sendToAgentCommand,
  suggestCommand,
} from "./conversation";
import {
  artifactCommand,
  docEditCommand,
  docReadCommand,
  planApproveCommand,
  requestApprovalCommand,
} from "./documents";
import { handoffReadCommand, handoffWriteCommand } from "./handoffs";
import {
  claimCommand,
  issueCommand,
  issuesCommand,
  issueUpdateCommand,
  relatedCommand,
  resultCommand,
  resultsCommand,
  searchCommand,
} from "./issues";
import { plansCommand } from "./plans";
import { prioritiesCommand } from "./priorities";
import { architectureSyncCommand, projectCommand, projectsCommand } from "./projects";
import { readCommand } from "./read";
import { reviewCommand } from "./reviews";
import { answersCommand, inboxCommand, limitsCommand, whoamiCommand } from "./session";
import { stepsCommand } from "./steps";

/** Every command the contract names, so a command added there without a handler here fails to
 *  type-check (`__tests__/dispatch-command-table.test.ts` holds the proof). */
export type DispatchToolName = (typeof dispatchToolSpecs)[number]["name"];

/**
 * One command, whether running it resolves this call's origin, and whether it is a read command.
 * `origin: "none"` is a command that answers on the session's context alone, so no subprocess
 * runs for it; every other command is handed the context `commandContext` builds. `reads: true`
 * is one of the seven read commands that take `--as-of` (dispatch://INFRA-4620): it reads whoami
 * before any other request and is handed what it read (`ReadingContext`).
 */
export type DispatchCommand =
  | {
      readonly origin: "resolved";
      readonly reads: false;
      readonly run: (context: CommandContext) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "none";
      readonly reads: false;
      readonly run: (context: SessionContext) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "resolved";
      readonly reads: true;
      readonly run: (context: ReadingContext<CommandContext>) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "none";
      readonly reads: true;
      readonly run: (context: ReadingContext<SessionContext>) => Promise<DispatchToolResult>;
    };

/** A command that acts for the session: it is handed the context whose origin was resolved. */
function acting(run: (context: CommandContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "resolved", reads: false, run };
}

/** A command that answers from the session alone, resolving no origin. */
function onSession(run: (context: SessionContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "none", reads: false, run };
}

/** A read command that acts for the session. */
function reading(
  run: (context: ReadingContext<CommandContext>) => Promise<DispatchToolResult>
): DispatchCommand {
  return { origin: "resolved", reads: true, run };
}

/** A read command that answers from the session alone. */
function readingOnSession(
  run: (context: ReadingContext<SessionContext>) => Promise<DispatchToolResult>
): DispatchCommand {
  return { origin: "none", reads: true, run };
}

/** What `dispatch <command>` does, one handler per command the contract names. */
export const dispatchCommands: Record<DispatchToolName, DispatchCommand> = {
  dispatch_issue: acting(issueCommand),
  dispatch_issue_update: acting(issueUpdateCommand),
  dispatch_claim: acting(claimCommand),
  dispatch_issues: acting(issuesCommand),
  dispatch_related: acting(relatedCommand),
  dispatch_search: reading(searchCommand),
  dispatch_project: acting(projectCommand),
  dispatch_projects: acting(projectsCommand),
  dispatch_priorities: acting(prioritiesCommand),
  dispatch_architecture_sync: acting(architectureSyncCommand),
  dispatch_ask: acting(askCommand),
  dispatch_edit_ask: acting(editAskCommand),
  dispatch_resolve_ask: acting(resolveAskCommand),
  dispatch_follow: acting(followCommand),
  dispatch_open_asks: readingOnSession(openAsksCommand),
  dispatch_comment: acting(commentCommand),
  dispatch_suggest: acting(suggestCommand),
  dispatch_resolve_comment: acting(resolveCommentCommand),
  dispatch_edit_comment: acting(editCommentCommand),
  dispatch_delete_comment: acting(deleteCommentCommand),
  dispatch_edit_message: acting(editMessageCommand),
  dispatch_delete_message: acting(deleteMessageCommand),
  dispatch_message: acting(messageCommand),
  dispatch_send_to_agent: acting(sendToAgentCommand),
  dispatch_doc_edit: acting(docEditCommand),
  dispatch_doc_read: reading(docReadCommand),
  dispatch_request_approval: acting(requestApprovalCommand),
  dispatch_plan_approve: acting(planApproveCommand),
  dispatch_review: acting(reviewCommand),
  dispatch_result: acting(resultCommand),
  dispatch_results: reading(resultsCommand),
  dispatch_artifact: acting(artifactCommand),
  dispatch_handoff_write: acting(handoffWriteCommand),
  dispatch_handoff_read: acting(handoffReadCommand),
  dispatch_steps: acting(stepsCommand),
  dispatch_plans: acting(plansCommand),
  dispatch_read: reading(readCommand),
  dispatch_answers: readingOnSession(answersCommand),
  dispatch_whoami: onSession(whoamiCommand),
  dispatch_limits: onSession(limitsCommand),
  dispatch_api: onSession(apiCommand),
  dispatch_inbox: readingOnSession(inboxCommand),
};
