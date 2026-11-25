import { type DispatchToolRoute, dispatchToolSpecs } from "@legion/contracts";
import { apiCommand } from "./api";
import {
  askCommand,
  editAskCommand,
  followCommand,
  openAsksCommand,
  resolveAskCommand,
} from "./asks";
import type { CommandContext, DispatchToolResult, SessionContext } from "./context";
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
import {
  architectureSyncCommand,
  projectCommand,
  projectsCommand,
  projectUpdateCommand,
} from "./projects";
import { readCommand } from "./read";
import { reviewCommand } from "./reviews";
import { routeCommand } from "./route-tools";
import { answersCommand, inboxCommand, limitsCommand, whoamiCommand } from "./session";
import { stepsCommand } from "./steps";

/** Every command the contract names. */
export type DispatchToolName = (typeof dispatchToolSpecs)[number]["name"];

/** Every command whose spec binds no route, each served by a command written by hand: a command
 *  added there without a handler here fails to type-check (`__tests__/dispatch-command-table.test.ts`
 *  holds the proof). A spec that binds its routes is served by `routeCommand` instead. */
export type HandWrittenToolName = Exclude<
  (typeof dispatchToolSpecs)[number],
  { readonly routes: readonly DispatchToolRoute[] }
>["name"];

/**
 * One command, and whether running it resolves this call's origin. `origin: "none"` is a command
 * that answers on the session's context alone, so no subprocess runs for it; every other command
 * is handed the context `commandContext` builds.
 */
export type DispatchCommand =
  | {
      readonly origin: "resolved";
      readonly run: (context: CommandContext) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "none";
      readonly run: (context: SessionContext) => Promise<DispatchToolResult>;
    };

/** A command that acts for the session: it is handed the context whose origin was resolved. */
function acting(run: (context: CommandContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "resolved", run };
}

/** A command that answers from the session alone, resolving no origin. */
function onSession(run: (context: SessionContext) => Promise<DispatchToolResult>): DispatchCommand {
  return { origin: "none", run };
}

/** What `dispatch <command>` does, one handler per command whose spec binds no route. */
export const dispatchCommands: Record<HandWrittenToolName, DispatchCommand> = {
  dispatch_issue: acting(issueCommand),
  dispatch_issue_update: acting(issueUpdateCommand),
  dispatch_claim: acting(claimCommand),
  dispatch_issues: acting(issuesCommand),
  dispatch_related: acting(relatedCommand),
  dispatch_search: acting(searchCommand),
  dispatch_project: acting(projectCommand),
  dispatch_projects: acting(projectsCommand),
  dispatch_project_update: acting(projectUpdateCommand),
  dispatch_priorities: acting(prioritiesCommand),
  dispatch_architecture_sync: acting(architectureSyncCommand),
  dispatch_ask: acting(askCommand),
  dispatch_edit_ask: acting(editAskCommand),
  dispatch_resolve_ask: acting(resolveAskCommand),
  dispatch_follow: acting(followCommand),
  dispatch_open_asks: onSession(openAsksCommand),
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
  dispatch_doc_read: acting(docReadCommand),
  dispatch_request_approval: acting(requestApprovalCommand),
  dispatch_plan_approve: acting(planApproveCommand),
  dispatch_review: acting(reviewCommand),
  dispatch_result: acting(resultCommand),
  dispatch_results: acting(resultsCommand),
  dispatch_artifact: acting(artifactCommand),
  dispatch_handoff_write: acting(handoffWriteCommand),
  dispatch_handoff_read: acting(handoffReadCommand),
  dispatch_steps: acting(stepsCommand),
  dispatch_plans: acting(plansCommand),
  dispatch_read: acting(readCommand),
  dispatch_answers: onSession(answersCommand),
  dispatch_whoami: onSession(whoamiCommand),
  dispatch_limits: onSession(limitsCommand),
  dispatch_api: onSession(apiCommand),
  dispatch_inbox: onSession(inboxCommand),
};

/** The command of `name`: its hand-written handler, or, for a spec that binds its routes, the one
 *  generic command that calls its route (`routeCommand`); undefined for no command of that name. */
export function dispatchCommandOf(name: string): DispatchCommand | undefined {
  const spec = dispatchToolSpecs.find((candidate) => candidate.name === name);
  if (spec === undefined) return undefined;
  if ("routes" in spec) return acting(routeCommand(spec.routes));
  return dispatchCommands[spec.name];
}
