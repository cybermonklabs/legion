import { type DispatchToolRoute, dispatchToolSpecs } from "@legion/contracts";
import { apiCommand } from "./api";
import {
  askCommand,
  editAskCommand,
  followCommand,
  openAsksCommand,
  resolveAskCommand,
} from "./asks";
import type {
  CommandContext,
  DispatchToolResult,
  DryRunContext,
  DryRunVerdict,
  SessionContext,
} from "./context";
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
import { deployCommand, deploysCommand } from "./deploy";
import {
  artifactCommand,
  artifactDryRun,
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
 * What a command's `--dry-run` asks Dispatch after the call's own argument check passed: the
 * question the call asks before it writes, whose answer can refuse the call. The dry run prints
 * the refusal the step returns, and otherwise the arguments; it stores nothing. A command with no
 * step asks Dispatch nothing in its dry run, save a `dispatch result` record, whose dry run runs
 * as the record does (`dryRunAsksDispatch`).
 */
export type DispatchDryRunStep = (context: DryRunContext) => Promise<DryRunVerdict>;

/**
 * One command, and whether running it resolves this call's origin. `origin: "none"` is a command
 * that answers on the session's context alone, so no subprocess runs for it; every other command
 * is handed the context `commandContext` builds. `dryRun` is the command's dry-run step, when it
 * has one. `documentFields` are the command's fields it checks as a Markdown document's text, when
 * it has any. Given inline or read from a file or stdin, such a field's text is refused by
 * `runDispatchCli` as the upload of a Markdown file of the same bytes refuses it, the size first,
 * then the first U+0000 or byte that is not UTF-8 (`documentBytesRefusal`): bytes read whether or
 * not they are UTF-8, and a text decoded or given inline in its UTF-8 (`documentFieldRefusal`). A
 * text given in argv holds no U+0000 on any kernel, and Linux holds one argument to
 * MAX_ARG_STRLEN, 32 pages, which is 128 KiB on 4 KiB pages and 2 MiB on 64 KiB pages, so with
 * 4 KiB pages argv carries no text past 1 MiB, and with 64 KiB pages it can. An in-process
 * caller's text can be either on any kernel. Inline text is checked however it is given, so what
 * is refused does not depend on the page size. Any other field read from a file or stdin is no
 * document, and is refused only for bytes that are not UTF-8, at their first U+0000 or byte that
 * is not UTF-8 (`unstorableBytes`).
 * `documentPathField` is the command's field whose value names a local file it uploads, when it
 * has one: `runDispatchCli` reads such a file once where Dispatch stores it as a Markdown
 * document, refuses it as the upload does (`documentFileRead`), and hands the bytes it read to the
 * dry run's step, which does not read the file again, and to the call. Where AskGate reviews the
 * call, AskGate's check reads the file a second time for its digest, and the call checks and sends
 * the bytes that read took in place of these. A file it cannot read it leaves to the dry run's step
 * and the call, which read it and answer with the read's own error; a FIFO, a device or a socket it
 * does not read, nor does the dry run's step, and the call uploads it unchecked. Both checks run
 * once the flags are read, before the arguments are checked and before anything that needs
 * Dispatch, in the call and its dry run alike.
 */
export type DispatchCommand = (
  | {
      readonly origin: "resolved";
      readonly run: (context: CommandContext) => Promise<DispatchToolResult>;
    }
  | {
      readonly origin: "none";
      readonly run: (context: SessionContext) => Promise<DispatchToolResult>;
    }
) & {
  readonly dryRun?: DispatchDryRunStep;
  readonly documentFields?: readonly string[];
  readonly documentPathField?: string;
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
  dispatch_deploy: acting(deployCommand),
  dispatch_deploys: acting(deploysCommand),
  // `content` is the text `dispatch artifact` stores as a Markdown document, and `path` names the
  // file it uploads, which it stores as one when the file is Markdown.
  dispatch_artifact: {
    ...acting(artifactCommand),
    dryRun: artifactDryRun,
    documentFields: ["content"],
    documentPathField: "path",
  },
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
