import type { ArtifactBlock, AskWithTrees, EditOp, TextAsk } from "@legion/contracts";
import { typedBlockIdAttribute } from "@legion/contracts";
import { commandName } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchIssueRef,
  documentLabel,
  parseDispatchRef,
} from "../dispatch-owner";
import type { ResolvedArtifact } from "./context";
import { liveTextAsks } from "./text-asks";

/** The asks of the document's `ask` blocks. An issue lists them under the issue, since the artifact
 * route refuses an issue document; an unlinked document lists its own. */
async function blockAsks(
  client: DispatchClient,
  resolved: ResolvedArtifact,
  state?: "all" | "open" | "answered"
): Promise<Array<AskWithTrees & { readonly block_id: string }>> {
  const asks = await (resolved.issue === undefined
    ? client.getArtifactAsks(resolved.artifact.id, state)
    : client.listIssueAsks(resolved.issue.key, state));
  return asks.filter(
    (ask): ask is AskWithTrees & { readonly block_id: string } =>
      typeof ask.block_id === "string" && ask.block_artifact?.id === resolved.artifact.id
  );
}

/** The lines of `lines` that open the ask block `blockId`: `:::ask{<id> …}`, the id as the
 *  renderer writes it (`typedBlockIdAttribute`: `#<id>`, or `id="…"` for an id the shortcut
 *  cannot hold). A line that quotes the opener (in code, say) is one of them too. */
function askOpeners(lines: readonly string[], blockId: string): string[] {
  const id = typedBlockIdAttribute(blockId);
  return lines.filter((line) => line.includes(`ask{${id} `) || line.includes(`ask{${id}}`));
}

/**
 * Refuses an approval request while the document holds an open ask block. A request asks
 * the human to agree to every point of the latest version, and an open block is a question they
 * have not decided yet. Their answer is held on the ask, not in the text, so it writes no version
 * and moves no request. The live document's `ask` blocks are judged against the latest version,
 * the one the request would name; a block not yet in that version counts as open.
 *
 * A person's answer is held once, on the ask's node, so a version this
 * release writes records no state on a block's opening line, and such a block is judged by the
 * server's read of its ask: it is open while its ask is open. A block copied from another
 * document's ask has no ask of its own and is judged by that source, whose state, id and document
 * the live text (`GET /artifacts/{id}/text`, read only for such a copy) answers in its `asks`
 * member, or, from a Dispatch of the release before, which sends none, writes on the copy's
 * opening line (liveTextAsks). A version the release before wrote records the state settlement
 * wrote into the block, and is read as it was written: a block it shows open counts as open even
 * when its ask is already answered or closed, since while that release serves its settlement
 * writes the answer into the text, which reaches a version when the document settles, about two
 * seconds later, or with the agent's next edit, whichever is first. An
 * answered block already carries its own record, so no edit is needed for that; a waiver carries
 * none, so the agent's next edit is where the human's decision gets written into the text. A
 * block is taken by the version alone only when every line opening it records a settled state:
 * one that records none sends it to the server's read, so a line quoting the opener (in code,
 * say) can add an open block but never hide one.
 *
 * A block removed from the document is not judged here; `refuseRemovingOpenDecisionBlocks` keeps
 * one whose ask is open in it. A document already approved at its latest version is left to the
 * server, which answers with that approval.
 */
export async function refuseOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact
): Promise<void> {
  const artifact = resolved.artifact;
  const latest = artifact.approval?.latest_version;
  // No approval state means no document version to approve: the server's own refusal says so.
  if (latest === undefined || latest < 1 || artifact.approval?.state === "approved") return;
  const blocks = (await client.artifactBlocks(artifact.id)).filter((block) => block.type === "ask");
  if (blocks.length === 0) return;
  const [documentAsks, version] = await Promise.all([
    blockAsks(client, resolved),
    client.docRead(artifact.id, latest),
  ]);
  const asks = new Map(documentAsks.map((ask) => [ask.block_id, ask]));
  const lines = version.markdown.split("\n");
  // The live text's asks, read once, the first time a copy needs its source's state.
  let liveAsks: Promise<readonly TextAsk[]> | undefined;
  const stateOf = (line: string): string | undefined => /\bstate="(\w+)"/.exec(line)?.[1];
  // Only a version the release before wrote shows open a block whose ask is answered or closed.
  // While that release serves, its settlement writes an answer into the block, which settles into
  // a version on its own; a waived one (resolved) gets the decision written in.
  const stillOpen = (named: string, state: string): string =>
    state === "answered"
      ? `${named}, answered but still open in version ${latest}: it settles into a version on its own in about two seconds; wait for that version`
      : `${named}, ${state} but still open in version ${latest}: write the decision into the text with dispatch doc-edit, which writes a version that carries it`;
  // A block copied from another document's ask opens none: settlement names that ask and its
  // document, as the copy's `copied_from` and `copied_from_document`, so the source is where it is
  // answered. The document is named as the dashboard's decision card names it, its issue key and
  // slug or its project's `<project>/<slug>`, so the ask's address is the line's one link.
  const openCopy = (named: string, copies: readonly TextAsk[]): string => {
    const sourceId = copies.map((copy) => copy.copied_from).find((value) => value !== undefined);
    const document = copies
      .map((copy) => copy.copied_from_document)
      .find((value) => value !== undefined);
    if (sourceId === undefined || document === undefined) {
      return `${named}, whose ask Dispatch has not opened yet`;
    }
    const source = parseDispatchRef(document);
    const where =
      source === null
        ? document
        : source.owner.kind === "project"
          ? documentLabel(source.owner.project, source.id)
          : `${source.owner.issue} ${source.id}`;
    const ref =
      source?.owner.kind === "issue"
        ? dispatchChildRef(dispatchIssueRef(source.owner.issue), "ask", sourceId)
        : dispatchChildRef(document, "ask", sourceId);
    return `${named}, a copy of ask ${ref}, which is open on ${where}: answer it there`;
  };
  /** The refusal's line for `block`, or undefined where the block is not open. */
  const judged = async (block: ArtifactBlock): Promise<string | undefined> => {
    const ask = asks.get(block.id);
    const named =
      ask === undefined
        ? `block ${block.id}`
        : `${JSON.stringify(ask.question)} (block ${block.id}, ask ${ask.id})`;
    const openers = askOpeners(lines, block.id);
    const states = openers.map(stateOf);
    if (states.length === 0) return `${named}, which version ${latest} does not hold yet`;
    // The release before wrote the block's state into the version, which shows it open.
    if (states.includes("open")) {
      if (ask === undefined) return openCopy(named, liveTextAsks({ markdown: openers.join("\n") }));
      return ask.state === "open" ? named : stillOpen(named, ask.state);
    }
    if (states.every((state) => state !== undefined)) return undefined;
    // A line opening the block records no state: the server's read of its ask judges it.
    if (ask !== undefined) return ask.state === "open" ? named : undefined;
    liveAsks ??= client.docRead(artifact.id).then(liveTextAsks);
    const served = (await liveAsks).filter((copy) => copy.block_id === block.id);
    const settled =
      served.length > 0 &&
      served.every((copy) => copy.state !== undefined && copy.state !== "open");
    return settled ? undefined : openCopy(named, served);
  };
  const open: string[] = [];
  for (const block of blocks) {
    const line = await judged(block);
    if (line !== undefined) open.push(line);
  }
  if (open.length === 0) return;
  const count = open.length === 1 ? "1 open ask block" : `${open.length} open ask blocks`;
  throw new Error(
    [
      `dispatch ${commandName(tool)} was not called: ${artifact.name} (version ${latest}) has ${count}. Approving the document means agreeing to every point in it, and an open block is a question the human has not decided yet.`,
      ...open.map((line) => `- ${line}`),
      "Do not request approval over an open block, even when a human asked for it. Tell the human which block is open and ask them to answer it or to waive it. Their answer is held on the ask, not in the text: it writes no version and moves no approval, so once every block is answered, request approval of the latest version, after any version a line above says to wait for. When the answer changes design text outside the block, rewrite that text (never to restate the answer), then request approval once the human has agreed to every point in the new version, with a one-sentence --summary naming what changed since any earlier approval. If they waive it, close the block with dispatch resolve-ask (--kind resolved, their words as the --reason), write their decision into the text with dispatch doc-edit, then request approval again once the human has agreed to every point in the new version. The call opens the request, or hands an open one back to the human.",
    ].join("\n")
  );
}

/**
 * Refuses a document edit that would take an ask block out of the document while its ask is
 * open: a `delete` of the block or of a block holding it, or a `retype` of it into another type.
 * The edit writes its version at once; about two seconds later settlement retracts the ask in the
 * system's name and writes no version, so the human's question leaves the Inbox unanswered and an
 * approval request made after it names a version with no open block for
 * `refuseOpenDecisionBlocks` to find. An `insert` in the same batch that carries the block's id
 * does not exempt it: telling a block written back from an opener quoted in code needs the
 * server's parser, so the executor fails closed, as `refuseOpenDecisionBlocks` does for a quoted
 * opener. An open block is reworded with `replace`, moved with `move`, or, when this session asked
 * it, has its question, options, urgency or multiple changed with `dispatch_edit_ask`; each keeps
 * it. Costs nothing for an edit with no such operation, then one
 * `GET /artifacts/{id}/blocks`, and the owner's asks only when an operation reaches an `ask` block.
 */
export async function refuseRemovingOpenDecisionBlocks(
  client: DispatchClient,
  tool: string,
  resolved: ResolvedArtifact,
  ops: readonly EditOp[]
): Promise<void> {
  const removing = ops.filter(
    (operation) =>
      operation.block !== undefined &&
      (operation.op === "delete" || (operation.op === "retype" && operation.type !== "ask"))
  );
  if (removing.length === 0) return;
  const artifact = resolved.artifact;
  const blocks = await client.artifactBlocks(artifact.id);
  const askBlocks = blocks.filter((block) => block.type === "ask");
  const removed = new Set<string>();
  for (const operation of removing) {
    const target = blocks.find((block) => block.id === operation.block);
    if (target === undefined) continue;
    // A delete takes every block inside the one it names; a retype changes only that block.
    for (const block of askBlocks) {
      if (
        block.id === target.id ||
        (operation.op === "delete" && block.from >= target.from && block.to <= target.to)
      ) {
        removed.add(block.id);
      }
    }
  }
  if (removed.size === 0) return;
  const asks = await blockAsks(client, resolved, "open");
  const open = asks.filter((ask) => removed.has(ask.block_id));
  if (open.length === 0) return;
  const [what, question] =
    open.length === 1
      ? ["an ask block whose ask is", "question"]
      : [`${open.length} ask blocks whose asks are`, "questions"];
  throw new Error(
    [
      `dispatch ${commandName(tool)} was not called: it would remove ${what} still open, and the human's ${question} would leave their Inbox unanswered.`,
      ...open.map(
        (ask) => `- ${JSON.stringify(ask.question)} (block ${ask.block_id}, ask ${ask.id})`
      ),
      "An ask block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch edit-ask if you asked it; each keeps it.",
    ].join("\n")
  );
}
