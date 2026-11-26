import type { ArtifactBlock, AskWithTrees, EditOp, TextAsk } from "@legion/contracts";
import { askOpenerAttributes, askOpenerLines, askOpeners } from "@legion/contracts";
import { commandName } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchIssueRef,
  documentLabel,
  parseDispatchRef,
} from "../dispatch-owner";
import type { ResolvedArtifact } from "./context";
import { liveTextAsks, openerAsks } from "./text-asks";

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

/** The words a refusal over open ask blocks says around the blocks it names: `call`, the call it
 * refused (`dispatch request-approval`, `legion register_gate`); `effect`, the sentence on why an
 * open block refuses that call; `remedy`, what to do instead; `block`, what the refusal calls a
 * block, `decision block` where it names none. */
export interface OpenDecisionBlockRefusal {
  readonly call: string;
  readonly effect: string;
  readonly remedy: string;
  readonly block?: string;
}

/** The two sentences the `legion` tool's register_gate remedy over open decision blocks gives
 * first, word for word: ask the human to answer or waive the block, and how an answered one
 * settles into a version, about two seconds later, as `refuseOpenDecisionBlocks` names an answered
 * block still open. `dispatch request-approval`'s remedy gives the first, then says where the
 * answer is held. */
export const OPEN_BLOCK_ASK_THE_HUMAN =
  "Tell the human which block is open and ask them to answer it or to waive it. Once it is answered, it settles into a version on its own in about two seconds.";

/** How every `remedy` over open decision blocks closes a block the human waives, which each one
 * continues with its own next step. */
export const OPEN_BLOCK_WAIVED =
  "If they waive it, close the block with dispatch resolve-ask (--kind resolved, their words as the --reason)";

/**
 * Refuses an approval request while the document holds an open ask block. A request asks
 * the human to agree to every point of the latest version, and an open block is a question they
 * have not decided yet. Their answer is held on the ask, not in the text. The live document's
 * `ask` blocks are judged against the latest version, the one the request would name; a block not
 * yet in that version counts as open.
 *
 * A Dispatch of this release answers each ask's state from the ask itself - a block's own ask from
 * the asks list, a copy's source from the live text's `asks` (`GET /artifacts/{id}/text`) - and
 * this test needs nothing more: wherever the live text answers `asks`, which only such a
 * Dispatch does, each block is judged by the server's ask, whatever state a stored version records
 * on its opening line, which this release keeps there only for the readers of the release before.
 * A Dispatch of the release before sends no `asks`, writes each answer into the block, and is read
 * as it reads itself: by the state the latest version records on the block's opening line, so a
 * block that version shows open counts as open even when its ask is already answered or closed,
 * since the answer reaches a version when the document settles, about two seconds later, or with
 * the agent's next edit, whichever is first; a copy is judged by its source, whose state, id and
 * document that Dispatch writes on the copy's opening line (liveTextAsks). An answered block
 * already carries its own record, so no edit is needed for that; a waiver carries none, so the
 * agent's next edit is where the human's decision gets written into the text. A block whose every
 * opening line records the state the server's ask holds, or that records none, is judged without
 * asking which release serves, since both read it alike. Only a real opening line counts
 * (`askOpenerLines`): a line that quotes the opener in fenced or indented code or in front matter
 * is the document's own text, and so is a later line of an inline code span that stands four
 * columns or more past its containers. A quote records no state and neither opens a block nor
 * hides one. The live text is read once, and only for a block that needs it.
 *
 * A block removed from the document is not judged here; `refuseRemovingOpenDecisionBlocks` keeps
 * one whose ask is open in it. A document already approved at its latest version is left to the
 * server, which answers with that approval.
 *
 * The `legion` tool makes the same test before `register_gate` opens a root's design-gate step on
 * an ancestor's approval without a person's review of the root's document. `refusal` supplies the
 * caller's words around the blocks named.
 */
export async function refuseOpenDecisionBlocks(
  client: DispatchClient,
  resolved: ResolvedArtifact,
  refusal: OpenDecisionBlockRefusal
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
  const openerLines = askOpenerLines(version.markdown);
  // The live text's asks, read once, the first time a block needs them: a Dispatch of this release
  // answers `asks`, and one of the release before sends none and writes them on each opening line.
  let live: Promise<{ readonly served: boolean; readonly asks: readonly TextAsk[] }> | undefined;
  const liveText = () =>
    (live ??= client.docRead(artifact.id).then((text) => ({
      // kept for one release: release-before-text
      served: text.asks !== undefined && text.asks !== null,
      asks: liveTextAsks(text),
    })));
  // A version of the release before shows open a block whose ask is answered or closed until the
  // version its settlement writes the answer into; a waived one (resolved) gets the decision
  // written in.
  // kept for one release: release-before-text
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
    const openers = askOpeners(openerLines, block.id);
    // The state each of the block's opening lines records (askOpenerAttributes), which decides a
    // block only against a Dispatch of the release before, whose live text sends no `asks`.
    // kept for one release: release-before-text
    const states = openers.map((line) => askOpenerAttributes(line)?.state);
    if (states.length === 0) return `${named}, which version ${latest} does not hold yet`;
    // kept for one release: release-before-text
    const recordsOpen = states.includes("open");
    // kept for one release: release-before-text
    const recordsSettled = !recordsOpen && states.every((state) => state !== undefined);
    if (ask !== undefined) {
      const open = ask.state === "open";
      // A Dispatch of this release sends `asks`, and the ask's own state decides. One of the
      // release before sends none, and there a version that records the block open while its ask
      // is not waits for the version that holds the answer.
      // kept for one release: release-before-text
      if ((!recordsOpen && !recordsSettled) || recordsOpen === open || (await liveText()).served) {
        return open ? named : undefined;
      }
      // kept for one release: release-before-text
      return recordsOpen ? stillOpen(named, ask.state) : undefined;
    }
    // A copy, judged by its source, which is read off the block's own opening lines alone
    // (openerAsks over `openers`, the version's real openers of the block): a line that quotes it
    // (in code, a fence opened on a footnote's label line included, or in front matter) names no
    // ask Dispatch recorded.
    const text = await liveText();
    // kept for one release: release-before-text
    if (!text.served && recordsSettled) return undefined;
    // kept for one release: release-before-text
    if (!text.served && recordsOpen) return openCopy(named, openerAsks(openers));
    const copies = text.asks.filter((copy) => copy.block_id === block.id);
    const settled =
      copies.length > 0 &&
      copies.every((copy) => copy.state !== undefined && copy.state !== "open");
    return settled ? undefined : openCopy(named, copies);
  };
  const open: string[] = [];
  for (const block of blocks) {
    const line = await judged(block);
    if (line !== undefined) open.push(line);
  }
  if (open.length === 0) return;
  const noun = refusal.block ?? "decision block";
  const count = open.length === 1 ? `1 open ${noun}` : `${open.length} open ${noun}s`;
  throw new Error(
    [
      `${refusal.call} was not called: ${artifact.name} (version ${latest}) has ${count}. ${refusal.effect}`,
      ...open.map((line) => `- ${line}`),
      refusal.remedy,
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
 * server's parser, so the executor fails closed. An open block is reworded with `replace`, moved
 * with `move`, or, when this session asked it, has its question, options, urgency or multiple
 * changed with `dispatch_edit_ask`; each keeps it. An answered block is none of this check's: it
 * stays in its document, and the server refuses an agent's removal or retype of one, `409
 * ANSWERED_ASK_KEPT`, so only a resolved block may be removed. Costs nothing for an edit with no
 * such operation, then one `GET /artifacts/{id}/blocks`, and the owner's asks only when an
 * operation reaches an `ask` block.
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
      "An answered ask block stays in its document (an agent's removal or retype of one is refused 409 ANSWERED_ASK_KEPT); only a resolved one may be removed. While an ask is open, reword its block with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch edit-ask if you asked it; each keeps it.",
    ].join("\n")
  );
}
