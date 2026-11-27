// The refusals a call meets while a document holds a decision block a person has yet to answer:
// an approval request over one (`dispatch request-approval`, and the `legion` tool's register_gate
// on an ancestor's basis), and an edit that would take one out of the document (`dispatch doc-edit`).
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

/** The words and rule a refusal over open decision blocks uses around the blocks it names:
 * `call`, the call it refused (`dispatch request-approval`, `legion register_gate`); `blocks`,
 * what its count calls an open block (`ask block` in `dispatch request-approval`'s words,
 * `decision block` in `legion register_gate`'s); `decidedBy`, below; `effect`, the sentence on
 * why an open block stops that call; `remedy`, what to do instead. */
export interface OpenDecisionBlockRefusal {
  readonly call: string;
  readonly blocks: string;
  /** Which state decides a block that is not open live: `"live"` takes the server's live ask
   * state once the live text answers `asks` (which `dispatch request-approval` uses, since
   * approval carry moves an open request to the version an answer writes), whereas `"version"`
   * also requires the latest saved version to record the block settled with the answer the server
   * holds (which `legion register_gate` on a basis uses, since a later version ends the basis).
   * `"version"` reads what the kept form writes on a block's opening line, so it goes with that
   * form, and with it this field and the value each caller sets. */
  // kept for one release: stored-ask-state
  readonly decidedBy: "live" | "version";
  readonly effect: string;
  readonly remedy: string;
}

/** The sentence every `remedy` over open decision blocks gives to reach the human: tell them
 * which block is open and ask them to answer it or to waive it. */
export const OPEN_BLOCK_ASK_THE_HUMAN =
  "Tell the human which block is open and ask them to answer it or to waive it.";

/** How long an answered block takes to reach a version: settlement writes the answer into one on
 * its own about two seconds after it. The line of a block still open in the latest saved version
 * waits on it (`refuseOpenDecisionBlocks`), and `OPEN_BLOCK_SETTLES` tells the agent it. */
const SETTLES_ON_ITS_OWN = "it settles into a version on its own in about two seconds";

/** The sentence `register_gate`'s `remedy` over open decision blocks adds after
 * `OPEN_BLOCK_ASK_THE_HUMAN`: an answered block settles into a version on its own, the wait
 * `refuseOpenDecisionBlocks` names under `"version"` (`SETTLES_ON_ITS_OWN`). */
export const OPEN_BLOCK_SETTLES = `Once it is answered, ${SETTLES_ON_ITS_OWN}.`;

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
 * this command needs nothing more: wherever the live text answers `asks`, which only such a
 * Dispatch does, each block is judged by the server's ask, whatever state a stored version records
 * on its opening line, which this release keeps there only for the readers of the release before.
 * A Dispatch of the release before sends no `asks`, writes each answer into the block, and is read
 * as it reads itself: by the state the latest version records on the block's opening line, so a
 * block that version shows open counts as open even when its ask is already answered or closed,
 * since the answer reaches a version when the document settles, about two seconds later, or with
 * the agent's next edit, whichever is first; a copy is judged by its source, whose state, id and
 * document that Dispatch writes on the copy's opening line (liveTextAsks). Under
 * `decidedBy: "version"` a copy there is judged by its source ask itself instead, read by the
 * copy's `copied_from` (`GET /asks/{id}`): that Dispatch writes a changed answer onto the source
 * ask and the source's opening line at once, and onto a copy's lines, live and in its versions,
 * only at the copy's own settlement. An answered block already carries its own record, so no edit
 * is needed for that; a waiver carries none, so the agent's next edit is where the human's decision
 * gets written into the text. A block whose every opening line records the state the server's ask
 * holds, or that records none, is judged without asking which release serves, since both read it
 * alike. Only a real opening line counts (`askOpenerLines`): a line that quotes the opener in
 * fenced or indented code or in front matter is the document's own text, and so is a later line of
 * an inline code span that stands four columns or more past its containers. A quote records no
 * state and neither opens a block nor hides one. The live text is read once, and only for a block
 * that needs it, and so is each copy's source ask.
 *
 * A block removed from the document is not judged here; `refuseRemovingOpenDecisionBlocks` keeps
 * one whose ask is open in it. A document already approved at its latest version is left to the
 * server, which answers with that approval.
 *
 * The `legion` tool makes the same test before `register_gate` opens a root's design-gate step on
 * an ancestor's approval, which no person's review of the root's own document stands behind
 * (dispatch://INFRA-3320, R1), with `decidedBy: "version"` so an answered block waits for the
 * saved version that holds the answer, who gave it and when, before the basis registers: a person
 * may change an answer while its ask stays answered, and the version settlement then writes ends
 * the basis. `refusal` carries that rule and each caller's own words around the blocks named.
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
  // The line of a block whose ask, or a copy's source, is answered or closed while the latest
  // saved version does not record it so, for an own ask and a copy alike: a version of the release
  // before shows such a block open until settlement writes the answer in, and a caller whose
  // `decidedBy` is `"version"` waits for the version that holds the answer the server holds.
  // `named` names the block, a copy with its source, and `state` is the state the check read, the
  // ask's own or the copy's. An answered block settles into a version on its own; a waived one
  // (resolved), own or copy, gets the decision written in with an edit, since a Dispatch of the
  // release before writes no record of a waiver.
  // kept for one release: release-before-text
  // kept for one release: stored-ask-state
  const stillOpen = (named: string, state: string): string =>
    state === "answered"
      ? `${named}, answered but still open in version ${latest}: ${SETTLES_ON_ITS_OWN}; wait for that version`
      : `${named}, ${state} but still open in version ${latest}: write the decision into the text with dispatch doc-edit, which writes a version that carries it`;
  // A block copied from another document's ask opens none: settlement names that ask and its
  // document, as the copy's `copied_from` and `copied_from_document`, so the source is where it is
  // answered. The document is named as the dashboard's decision card names it, its issue key and
  // slug or its project's `<project>/<slug>`, so the ask's address is the line's one link.
  const copySource = (
    copies: readonly TextAsk[]
  ): { readonly ref: string; readonly where: string } | undefined => {
    const sourceId = copies.map((copy) => copy.copied_from).find((value) => value !== undefined);
    const document = copies
      .map((copy) => copy.copied_from_document)
      .find((value) => value !== undefined);
    if (sourceId === undefined || document === undefined) return undefined;
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
    return { ref, where };
  };
  const openCopy = (named: string, copies: readonly TextAsk[]): string => {
    const source = copySource(copies);
    if (source === undefined) return `${named}, whose ask Dispatch has not opened yet`;
    return `${named}, a copy of ask ${source.ref}, which is open on ${source.where}: answer it there`;
  };
  // The source asks of the copies a Dispatch of the release before serves, each read once, the
  // first time a copy needs it, by the copy's `copied_from`.
  // kept for one release: release-before-text
  // kept for one release: stored-ask-state
  const sources = new Map<string, Promise<AskWithTrees>>();
  // `copy` with the state and the answer its source ask holds now, which a Dispatch of the release
  // before writes onto the copy's lines only at the copy's own settlement: the answerer and the
  // time as an own ask's are read below, and a state that is none of an ask's three as none. A
  // copy that names no source has none to read, and stays open.
  // kept for one release: release-before-text
  // kept for one release: stored-ask-state
  const withSourceAnswer = async (copy: TextAsk): Promise<TextAsk> => {
    const id = copy.copied_from;
    if (id === undefined) return { block_id: copy.block_id };
    let read = sources.get(id);
    if (read === undefined) {
      read = client.getAsk(id).then((source) => source.ask);
      sources.set(id, read);
    }
    const source = await read;
    const state = source.state;
    return {
      block_id: copy.block_id,
      copied_from: id,
      copied_from_document: copy.copied_from_document,
      state: state === "open" || state === "answered" || state === "resolved" ? state : undefined,
      answered_by: source.answer?.user || undefined,
      answered_at: source.answer?.at,
    };
  };
  /** The refusal's line for `block`, or undefined where the block is not open. */
  const judged = async (block: ArtifactBlock): Promise<string | undefined> => {
    const ask = asks.get(block.id);
    const named =
      ask === undefined
        ? `block ${block.id}`
        : `${JSON.stringify(ask.question)} (block ${block.id}, ask ${ask.id})`;
    const openers = askOpeners(openerLines, block.id);
    // What each of the block's opening lines records (askOpenerAttributes): its state decides a
    // block against a Dispatch of the release before (whose live text sends no `asks`), and under
    // `decidedBy: "version"`, which waits for the saved version to record the block settled, its
    // state, and who answered and when.
    // kept for one release: release-before-text
    // kept for one release: stored-ask-state
    const recorded = openers.map((line) => askOpenerAttributes(line));
    // kept for one release: release-before-text
    // kept for one release: stored-ask-state
    const states = recorded.map((attributes) => attributes?.state);
    if (states.length === 0) return `${named}, which version ${latest} does not hold yet`;
    // kept for one release: release-before-text
    const recordsOpen = states.includes("open");
    // kept for one release: release-before-text
    // kept for one release: stored-ask-state
    const recordsSettled = !recordsOpen && states.every((state) => state !== undefined);
    // Under `decidedBy: "version"`, whether every opening line records an answer by `answeredBy`
    // at `answeredAt`, the one the server holds now, as the kept form writes it (keptAskState):
    // the answerer as the ask's node names them, and the time as asks.AnsweredAtValue writes it,
    // RFC 3339 in UTC. The asks list and the live text serve both the same way, so a settled
    // version holds the same text; a time a row's own answer holds in another offset is the same
    // instant spelled otherwise. A person may change an answer while its ask stays answered, and
    // settlement then writes a version with the new answer, which ends a basis registered on the
    // one before.
    // kept for one release: stored-ask-state
    const recordsAnswer = (answeredBy: string | undefined, answeredAt: string | undefined) =>
      recorded.every((attributes) => {
        const at = attributes?.answered_at;
        return (
          attributes?.answered_by === answeredBy &&
          (at === answeredAt ||
            (at !== undefined &&
              answeredAt !== undefined &&
              Date.parse(at) === Date.parse(answeredAt)))
        );
      });
    if (ask !== undefined) {
      const open = ask.state === "open";
      // Under `decidedBy: "version"` (register_gate's basis), a block is not open only when the
      // ask is settled live and every opening line in the latest saved version records it settled,
      // an answered one with the answer the ask holds; the kept form writes no answerer an answer
      // names none of. A resolved block has no answerer.
      // kept for one release: stored-ask-state
      if (refusal.decidedBy === "version") {
        if (open) return named;
        const holdsTheAnswer =
          ask.state !== "answered" || recordsAnswer(ask.answer?.user || undefined, ask.answer?.at);
        return recordsSettled && holdsTheAnswer ? undefined : stillOpen(named, ask.state);
      }
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
    // A Dispatch of the release before is read as it reads itself under `decidedBy: "live"`: a copy
    // whose every opening line in the latest version records it settled is not open, and one a
    // line records open is open. Under `decidedBy: "version"` the copy is judged by its source ask
    // instead (withSourceAnswer): its own lines, live and in the version, take a changed answer of
    // the source only at the copy's own settlement, and the version that settlement writes ends a
    // basis registered on this one.
    // kept for one release: stored-ask-state
    if (refusal.decidedBy === "live") {
      // kept for one release: release-before-text
      if (!text.served && recordsSettled) return undefined;
      // kept for one release: release-before-text
      if (!text.served && recordsOpen) return openCopy(named, openerAsks(openers));
    }
    const live = text.asks.filter((copy) => copy.block_id === block.id);
    // kept for one release: release-before-text
    // kept for one release: stored-ask-state
    const copies =
      !text.served && refusal.decidedBy === "version"
        ? await Promise.all(live.map(withSourceAnswer))
        : live;
    const settled =
      copies.length > 0 &&
      copies.every((copy) => copy.state !== undefined && copy.state !== "open");
    if (!settled) return openCopy(named, copies);
    // Under `decidedBy: "version"`, a settled copy is not open only when every opening line in the
    // latest saved version records it settled, an answered one with the answer its source holds,
    // which the live text's `asks` give the copy on a Dispatch of this release, and the source ask
    // itself on one of the release before (withSourceAnswer). The line names the copy's source and
    // the state of the copy the version does not record.
    // kept for one release: stored-ask-state
    if (refusal.decidedBy === "version") {
      const unrecorded = copies.find(
        (copy) =>
          !recordsSettled ||
          (copy.state === "answered" && !recordsAnswer(copy.answered_by, copy.answered_at))
      )?.state;
      if (unrecorded !== undefined) {
        const source = copySource(copies);
        const what =
          source === undefined ? named : `${named}, a copy of ask ${source.ref} on ${source.where}`;
        return stillOpen(what, unrecorded);
      }
    }
    return undefined;
  };
  const open: string[] = [];
  for (const block of blocks) {
    const line = await judged(block);
    if (line !== undefined) open.push(line);
  }
  if (open.length === 0) return;
  const noun = refusal.blocks;
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
 * changed with `dispatch_edit_ask`; each keeps it. Costs nothing for an edit with no such
 * operation, then one `GET /artifacts/{id}/blocks`, and the owner's asks only when an operation
 * reaches an `ask` block.
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
