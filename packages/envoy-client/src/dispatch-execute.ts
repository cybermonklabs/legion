import { readAtCut, withLeftOut } from "./dispatch-commands/as-of";
import { checkDispatchCall, dispatchServerUrls } from "./dispatch-commands/check";
import { commandContext } from "./dispatch-commands/command-context";
import {
  type DispatchToolResult,
  type ExecuteDispatchToolInput,
  sessionContext,
} from "./dispatch-commands/context";
import { dryRunAsksDispatch } from "./dispatch-commands/issues";
import {
  type DispatchCommand,
  type DispatchToolName,
  dispatchCommands,
} from "./dispatch-commands/table";
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
  // runs; the table is still read as a lookup that can miss, so the name stays in the message.
  const command: DispatchCommand | undefined = dispatchCommands[input.tool as DispatchToolName];
  if (command === undefined) throw new Error(`Unknown Dispatch tool: ${input.tool}`);
  const session = sessionContext(input, checked);
  // `dispatch_whoami` and `dispatch_limits` answer from the session alone; every other command
  // that reads no cut is handed the context that resolves this call's origin.
  if (!command.reads) {
    return command.origin === "none"
      ? command.run(session)
      : command.run(await commandContext(session));
  }
  // A read command reads whoami before any other request: a token's cut replaces `--as-of`
  // (dispatch://INFRA-4620). The owner a call names resolves at the cut too.
  const reading = await readAtCut(session);
  const result =
    command.origin === "none"
      ? await command.run({ ...session, reading })
      : await command.run({ ...(await commandContext(session, reading.cut)), reading });
  return withLeftOut(result, reading.cut);
}
