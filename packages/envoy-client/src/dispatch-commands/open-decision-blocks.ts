// The refusals a call meets while a document holds a decision block a person has yet to answer:
// an approval request over one (`dispatch request-approval`, and the `legion` tool's register_gate
// on an ancestor's basis), and an edit that would take one out of the document (`dispatch doc-edit`).
import { type AskWithTrees, type EditOp, typedBlockIdAttribute } from "@legion/contracts";
import { commandName } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchIssueRef,
  documentLabel,
  parseDispatchRef,
} from "../dispatch-owner";
import type { ResolvedArtifact } from "./context";

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

/** The words a refusal over open decision blocks says around the blocks it names: `call`, the
 * call it refused (`dispatch request-approval`, `legion register_gate`); `effect`, the sentence
 * on what answering a block would do to that call; `remedy`, what to do instead. */
export interface OpenDecisionBlockRefusal {
  readonly call: string;
  readonly effect: string;
  readonly remedy: string;
}

/**
 * Refuses an approval request while the document holds an open decision block. A request names
 * the latest version, and a new version moves it to that version and leaves it waiting on its
 * agent, so a request over a block the human has yet to answer leaves their turn the moment they
 * answer it. The live document's `ask` blocks are judged by the latest version, the one the
 * request would name: a block that version shows open counts as open even when its ask is already
 * answered or closed, since that answer reaches a version only when the document settles, about
 * two seconds later, or with the agent's next edit, whichever is first. An answered block already
 * carries its own record, so no edit is needed for that; a waiver carries none, so the agent's
 * next edit is where the human's decision gets written into the text. A block not yet in that
 * version counts as open too. A block removed from the document is not judged here;
 * `refuseRemovingOpenDecisionBlocks` keeps one whose ask is open in it. A document already
 * approved at its latest version is left to the server, which answers with that approval.
 *
 * The `legion` tool makes the same test before `register_gate` opens a root's design-gate step on
 * an ancestor's approval, which no person's review of the root's own document stands behind
 * (dispatch://INFRA-3320, R1). `refusal` is each caller's own words around the blocks named.
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
  const lines = version.markdown.split("\n");
  // An answered block already carries its own record and settles into a version on its own; a
  // waived one (resolved) gets the decision written in.
  const stillOpen = (named: string, state: string): string =>
    state === "answered"
      ? `${named}, answered but still open in version ${latest}: it settles into a version on its own in about two seconds; wait for that version`
      : `${named}, ${state} but still open in version ${latest}: write the decision into the text with dispatch doc-edit, which writes a version that carries it`;
  const open = blocks.flatMap((block): string[] => {
    const ask = asks.get(block.id);
    // The block's opening lines, `:::ask{<id> … state="…"}`, the id as the renderer writes it
    // (`typedBlockIdAttribute`: `#<id>`, or `id="…"` for an id the shortcut cannot hold). Every
    // match counts, so a line that quotes the opener (in code, say) can add an open block but
    // never hide one; a block none of whose lines carries a state is open.
    const id = typedBlockIdAttribute(block.id);
    const openers = lines.filter(
      (line) => line.includes(`ask{${id} `) || line.includes(`ask{${id}}`)
    );
    const states = openers.map((line) => /\bstate="(\w+)"/.exec(line)?.[1]);
    const named =
      ask === undefined
        ? `block ${block.id}`
        : `${JSON.stringify(ask.question)} (block ${block.id}, ask ${ask.id})`;
    if (states.length === 0) return [`${named}, which version ${latest} does not hold yet`];
    if (!states.includes("open") && states.some((state) => state !== undefined)) return [];
    if (ask !== undefined) return ask.state === "open" ? [named] : [stillOpen(named, ask.state)];
    // A block copied from another document's ask opens none: settlement names that ask in
    // `copied_from`, its document in `copied_from_document`, and writes its state into the block,
    // which this version shows open, so the source is where it is answered. The document is named
    // as the dashboard's decision card names it, its issue key and slug or its project's
    // `<project>/<slug>`, so the ask's address is the line's one link.
    const attribute = (name: string): string | undefined =>
      openers
        .map((line) => new RegExp(`\\b${name}="([^"]+)"`).exec(line)?.[1])
        .find((value) => value !== undefined);
    const sourceId = attribute("copied_from");
    const document = attribute("copied_from_document");
    if (sourceId === undefined || document === undefined) {
      return [`${named}, whose ask Dispatch has not opened yet`];
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
    return [`${named}, a copy of ask ${ref}, which is open on ${where}: answer it there`];
  });
  if (open.length === 0) return;
  const count = open.length === 1 ? "1 open decision block" : `${open.length} open decision blocks`;
  throw new Error(
    [
      `${refusal.call} was not called: ${artifact.name} (version ${latest}) has ${count}. ${refusal.effect}`,
      ...open.map((line) => `- ${line}`),
      refusal.remedy,
    ].join("\n")
  );
}

/**
 * Refuses a document edit that would take a decision block out of the document while its ask is
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
      ? ["a decision block whose ask is", "question"]
      : [`${open.length} decision blocks whose asks are`, "questions"];
  throw new Error(
    [
      `dispatch ${commandName(tool)} was not called: it would remove ${what} still open, and the human's ${question} would leave their Inbox unanswered.`,
      ...open.map(
        (ask) => `- ${JSON.stringify(ask.question)} (block ${ask.block_id}, ask ${ask.id})`
      ),
      "A decision block leaves the document once its ask is answered or resolved. Until then, reword it with replace, relocate it with move, or change its question, options, urgency or multiple with dispatch edit-ask if you asked it; each keeps it.",
    ].join("\n")
  );
}
