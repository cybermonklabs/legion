import { checkDispatchCall, dispatchServerUrls } from "./dispatch-commands/check";
import { commandContext } from "./dispatch-commands/command-context";
import {
  type DispatchToolResult,
  type ExecuteDispatchToolInput,
  sessionContext,
} from "./dispatch-commands/context";
import { dryRunAsksDispatch } from "./dispatch-commands/issues";
import { type DispatchCommand, dispatchCommandOf } from "./dispatch-commands/table";
import { ToolInputError } from "./tool-input-errors";

// What this module has always offered its callers, from the modules the command dispatcher is
// split into: the handlers are reached through the table alone, so none of them imports this one.
export { adviceMemory, resetAdviceMemory } from "./dispatch-commands/advice";
export type {
  CheckedCall,
  DispatchCallCheckInput,
  DispatchToolResult,
  ExecuteDispatchToolInput,
} from "./dispatch-commands/context";
export { asObject } from "./dispatch-commands/context";
export type {
  DocumentBlock,
  DocumentBlocks,
  DocumentTarget,
  DocumentTargetContext,
} from "./dispatch-commands/document-targets";
export {
  readDocumentBlocks,
  resolveIssueDocument,
  resolveIssueDocumentId,
} from "./dispatch-commands/document-targets";
export { dispatchRefFromUrl, linkBaseOf } from "./dispatch-commands/links";
export {
  OPEN_BLOCK_ASK_THE_HUMAN,
  OPEN_BLOCK_WAIVED,
  refuseOpenDecisionBlocks,
} from "./dispatch-commands/open-decision-blocks";
export { formatOpenAsksSummary, positionText } from "./dispatch-commands/render-item";
// The reader of a `dispatch://` reference the commands read theirs with, for a host that takes one
// of its own: the `legion` tool's register_gate basis names its issue's document by one.
export { parseDispatchRef } from "./dispatch-owner";
export { checkDispatchCall, dispatchServerUrls, dryRunAsksDispatch };

/** Validate and execute one native Dispatch tool against the JSON HTTP API. */
export async function executeDispatchTool(
  input: ExecuteDispatchToolInput
): Promise<DispatchToolResult> {
  // One validation pass: owner resolution, the strict schema, the cross-field hand checks and the
  // picture files; the call is refused once with every problem listed, or proceeds with arguments
  // the schema has fully validated (unknown keys and wrong types are impossible in a handler, so
  // structured arguments only need the contract's shape named).
  const checked = await checkDispatchCall({
    ...input,
    serverUrls: dispatchServerUrls(input.config),
  });
  if (checked.problems.length > 0) throw new ToolInputError(input.tool, checked.problems);
  // A dry run reaches a command only where the command answers it (`dryRunAsksDispatch`: a
  // `dispatch result` record): run as one, any other command would make its write.
  if (input.dryRun === true && !dryRunAsksDispatch(input.tool, checked.args)) {
    throw new Error(`dispatch ${input.tool}'s dry run runs no command`);
  }
  // The check reads the tool's own schema, so an unknown tool is already refused by the time this
  // runs; the lookup can still miss, so the name stays in the message. A spec that binds its
  // routes is served by the one generic command that calls them.
  const command: DispatchCommand | undefined = dispatchCommandOf(input.tool);
  if (command === undefined) throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  const session = sessionContext(input, checked);
  // `dispatch_open_asks`, `dispatch_answers` and `dispatch_whoami` answer from the session alone;
  // every other command is handed the context that resolves this call's origin.
  return command.origin === "none"
    ? command.run(session)
    : command.run(await commandContext(session));
}
