import { resolve as resolvePath } from "node:path";
import type {
  Artifact,
  ArtifactBlock,
  AskWithTrees,
  EditOp,
  EditPrecondition,
  UploadPrecondition,
} from "@legion/contracts";
import {
  ARTIFACT_NAME_EXISTS_RULE,
  PATH_KEEPS_BYTES,
  pathKeepsBytesOf,
  typedBlockIdAttribute,
} from "@legion/contracts";
import { actorText } from "../actor-text";
import { commandName } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  documentLabel,
  issueTopic,
  parseDispatchRef,
} from "../dispatch-owner";
import { shownPictureLine } from "../dispatch-picture-tools";
import {
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  sniffPictureType,
  toolImage,
} from "../dispatch-pictures";
import { messageFor } from "../errors";
import { decisionBlockLines, declinedAskLines } from "../related-check";
import { ToolInputError } from "../tool-input-errors";
import { otherAdviceLines, renderAdvice } from "./advice";
import {
  type CommandContext,
  type DispatchToolResult,
  numberArg,
  optionalBoolean,
  optionalNumber,
  optionalPrecondition,
  optionalString,
  type ResolvedArtifact,
  stringArg,
} from "./context";
import { approvalLine, liveBlocks } from "./document-targets";
import { dispatchAnswered } from "./failures";
import { openAnchoredThreads } from "./render-item";
import {
  documentResultDetails,
  documentTopic,
  followedAskDetails,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";

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
/**
 * dispatch_doc_read of an uploaded file or image. Dispatch serves a file's content only as the
 * bytes of one of its versions (its `/text` route answers a file 400 NOT_DOCUMENT), so this reads
 * the version asked for, or the latest. A picture a model takes (an image artifact whose bytes are
 * a PNG, JPEG, GIF or WebP of at most `PICTURE_SHOWN_MAX_BYTES`) comes back as an image block with
 * its name, type, version and size as the text, then the line naming it by its address
 * (`dispatch://<owner>/artifact/<slug>@v<N>`, `owner` an issue key, a project key or
 * `agent/<session id>`) as `dispatch_read` does, which a host reads back from a transcript; a
 * larger picture is described without its bytes being fetched when Dispatch states its size. Any
 * other file comes back as text when it is UTF-8, and is described with the route that serves its
 * bytes when it is not. A file carries no token, anchors or approval, so nothing else is read. A
 * picture is shown whatever `shown` holds, and joins it under its address, so the session's later
 * reads name it instead of sending it.
 */
async function readUploadedFile(
  client: DispatchClient,
  artifact: Artifact,
  details: Record<string, unknown>,
  requested: number | undefined,
  owner: string,
  shown: Set<string> | undefined
): Promise<DispatchToolResult> {
  const versions = artifact.versions ?? [];
  const latest = Math.max(0, ...versions.map((version) => version.number));
  const number = requested ?? latest;
  const of = number === latest ? "" : ` of ${latest}`;
  const bytesRoute = `GET /api/v1/artifacts/${artifact.id}/versions/${number} serves its bytes.`;
  const tooLarge = (mime: string, bytes: number): DispatchToolResult => ({
    text:
      `${oversizePictureText(artifact.name, bytes)}, so dispatch doc-read cannot show this ` +
      `uploaded ${mime} picture (version ${number}${of}). ${bytesRoute}`,
    details,
  });
  const stated = versions.find((version) => version.number === number);
  if (
    artifact.kind === "image" &&
    stated?.size !== undefined &&
    stated.size > PICTURE_SHOWN_MAX_BYTES
  ) {
    return tooLarge(stated.mime ?? "image", stated.size);
  }
  const file = await client.fileVersion(artifact.id, number);
  const size = `${file.bytes.length.toLocaleString("en-US")} bytes`;
  if (artifact.kind === "image") {
    const image = toolImage(file.bytes);
    if (image !== undefined) {
      const address = `dispatch://${owner}/artifact/${artifact.slug}@v${number}`;
      shown?.add(address);
      return {
        text:
          `Picture ${artifact.name}: ${image.mimeType}, version ${number}${of}, ${size}.\n` +
          shownPictureLine(1, address, artifact.name, image.mimeType, file.bytes.length),
        details,
        images: [image],
      };
    }
    if (sniffPictureType(file.bytes) !== undefined) return tooLarge(file.mime, file.bytes.length);
  }
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(file.bytes);
  } catch {
    return {
      text:
        `${artifact.name} is an uploaded ${file.mime} file (version ${number}${of}, ${size}) that is not ` +
        `UTF-8 text, so dispatch doc-read cannot show it. ${bytesRoute}`,
      details,
    };
  }
  return {
    text: `File ${artifact.name}: ${file.mime}, version ${number}${of}, ${size}.\n\n${text}`,
    details,
  };
}

/** How many characters of a block's text `dispatch doc-read --blocks` shows: enough to tell blocks
 *  apart without printing the document twice. */
const BLOCK_OPENING_CHARS = 60;

/**
 * `dispatch doc-read --blocks`: the live document's blocks in document order, one line each, a
 * block indented under the block whose markdown range holds it. Each line carries the id, type,
 * token and anchor counts `/blocks` reports, then the block's opening words, sliced from the live
 * markdown by the block's byte range (Dispatch reports UTF-8 offsets into `/text`'s markdown). A
 * table lists its rows and cells from its `/blocks` entry, the indexes `delete_row` and
 * `delete_column` take placed by Dispatch from its tree; a Dispatch whose entry carries no rows is
 * said to report none, never answered from the markdown, since a column read from the markdown is
 * not the index `delete_column` takes once a cell spans columns. The markdown, the blocks, the
 * token and the version come from one answer (`liveBlocks`), so no write between two reads can
 * leave the offsets slicing text they were not computed over.
 */
async function listDocumentBlocks(
  client: DispatchClient,
  artifact: Artifact
): Promise<{ text: string; token: string | undefined; version: number | null }> {
  const live = await liveBlocks(client, artifact);
  const { bytes, blocks } = live;
  const decoder = new TextDecoder();
  const lines = [
    `Blocks of ${artifact.name}, in document order, each indented under the block holding it: id, type, (token; anchored comments and asks), opening words, then a typed block's serves and needs. A table lists its rows by the index delete_row takes and its cells by the row and the column delete_column takes.`,
  ];
  const holders: ArtifactBlock[] = [];
  for (const block of blocks) {
    while (holders.length > 0 && block.from >= (holders.at(-1)?.to ?? 0)) holders.pop();
    const indent = "  ".repeat(holders.length);
    const flat = decoder.decode(bytes.subarray(block.from, block.to)).replace(/\s+/g, " ").trim();
    // Cut by code point, never inside a surrogate pair, which would print U+FFFD.
    const points = Array.from(flat);
    const opening =
      points.length > BLOCK_OPENING_CHARS
        ? `${points.slice(0, BLOCK_OPENING_CHARS).join("")}…`
        : flat;
    const { comments, asks } = block.references;
    lines.push(
      `${indent}${block.id} ${block.type} (${block.token ?? "no token"}; ${comments} comment${comments === 1 ? "" : "s"}, ${asks} ask${asks === 1 ? "" : "s"}): ${opening}`
    );
    // Every pointer a typed block holds, in the order its type declares them: a reader follows
    // these between documents, and a `block[]` attribute a type declares later must not go
    // silently unprinted. A pointer is what the schema declares `block[]`, never what reads like
    // one: a check's `run` may quote a `dispatch://` URL, and it is a command, not a pointer.
    // Every attribute of a block is in `dispatch read` of its reference.
    for (const attribute of block.attributes ?? []) {
      if (attribute.kind === "block[]" && attribute.value !== "") {
        lines.push(`${indent}  ${attribute.name}: ${attribute.value}`);
      }
    }
    if (block.type === "table") {
      if (block.table === undefined) {
        lines.push(
          `${indent}  This Dispatch does not report table rows yet, so no row or column index is listed.`
        );
      } else {
        for (const row of block.table.rows ?? []) {
          lines.push(
            `${indent}  row ${row.index}, ${row.id === "" ? "no id yet" : `id ${row.id}`}`
          );
          for (const cell of row.cells ?? []) {
            const under = cell.header === "" ? "" : `, under ${JSON.stringify(cell.header)}`;
            lines.push(
              `${indent}    row ${cell.row} column ${cell.column}, ${cell.id === "" ? "no id yet" : `id ${cell.id}`}${under}: ${cell.text === "" ? "(empty)" : cell.text}`
            );
          }
        }
      }
    }
    holders.push(block);
  }
  return { text: lines.join("\n"), token: live.token, version: live.version };
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
 */
async function refuseOpenDecisionBlocks(
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
      `dispatch ${commandName(tool)} was not called: ${artifact.name} (version ${latest}) has ${count}. Answering one writes a new version, which would move this request to that version and leave it waiting on you.`,
      ...open.map((line) => `- ${line}`),
      "Do not request approval over an open block, even when a human asked for it. Tell the human which block is open and ask them to answer it or to waive it. Once it is answered, it settles into a version on its own in about two seconds. When the human approved an earlier version and their answer is the whole change since it, Dispatch carries the approval to the new version in their name: request nothing. When the answer changes design text outside the block, rewrite that text (never to restate the answer), then request approval again once the human has agreed to every point in the new version, with a one-sentence --summary naming what changed since the earlier approval. On a document nobody has approved, wait for that version, then request. If they waive it, close the block with dispatch resolve-ask (--kind resolved, their words as the --reason), write their decision into the text with dispatch doc-edit, then request approval again once the human has agreed to every point in the new version. The call opens the request, or hands an open one back to the human.",
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
async function refuseRemovingOpenDecisionBlocks(
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

/** `dispatch doc-edit`: apply one batch of block operations to a live document. */
export async function docEditCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, linkBase, documentOwner, resolveDocument } = context;
  const resolved = await resolveDocument(documentOwner(), optionalString(args, "artifact"));
  const ops = args.ops as EditOp[];
  const summary = optionalString(args, "summary");
  const precondition = optionalPrecondition<EditPrecondition>(args);
  await refuseRemovingOpenDecisionBlocks(client, input.tool, resolved, ops);
  const edited = await client.docEdit(resolved.artifact.id, {
    ops,
    ...(summary === undefined ? {} : { summary }),
    ...(precondition === undefined ? {} : { precondition }),
    actor,
  });
  const retyped = ops.filter((operation) => operation.op === "retype").length;
  const versionText =
    edited.version === null ? "no new version" : `version ${edited.version.number}`;
  const retypedText = retyped === 0 ? "" : `; retyped ${retyped} block${retyped === 1 ? "" : "s"}`;
  // A batch that left the document as it was mints no version: say so, and name the
  // operations that did nothing, so the next attempt is aimed at the quote, not the version.
  const nothingChanged = edited.changed === false;
  const head = nothingChanged
    ? `Applied ${edited.applied} ops${retypedText}; nothing changed (${versionText})`
    : `Applied ${edited.applied} ops${retypedText} (${versionText})`;
  const unchangedOps = edited.unchanged_ops ?? [];
  const unchangedText =
    unchangedOps.length === 0
      ? ""
      : `; ${unchangedOps.length === 1 ? "operation" : "operations"} ${unchangedOps.join(", ")} changed nothing`;
  // A change the live document no longer carries: a browser deletion that landed after this
  // edit's version was rendered and before it reached the room, which is past undoing, so the
  // version records text the live document does not have (LEGION-269). `null` is a check that
  // reached no verdict - the room is reloading, or holds a tree past the schema's depth bound,
  // which a re-read answers DOC_SCHEMA for; an older server omits the field and reads as it
  // always did.
  const lostOps = edited.lost_ops;
  const lostText =
    lostOps === undefined || (lostOps !== null && lostOps.length === 0)
      ? ""
      : lostOps === null
        ? "; could not confirm this edit survived, because the live document is being reloaded or holds a tree too deep to read — re-read it"
        : `; ${versionText} carries text the live document no longer has: a concurrent change removed what ${lostOps.length === 1 ? "operation" : "operations"} ${lostOps.join(", ")} wrote — re-read the document`;
  const applied = `${head}${unchangedText}${lostText}`;
  // An older server names no block, so the one not-made line it gets takes its search words
  // from the first ask block this edit's own markdown opens (its question comes first), else
  // from the document's issue title; a retype on a project document has neither, and the
  // line asks for the words instead.
  const editAskText =
    ops
      .map(
        (operation) =>
          /^:{3,}ask\{.*\n([\s\S]*)/mu.exec(operation.markdown ?? operation.with ?? "")?.[1]
      )
      .find((text) => text !== undefined) ??
    resolved.issue?.title ??
    "";
  const checkLines = decisionBlockLines(edited.advice, {
    project: resolved.artifact.project,
    text: editAskText,
    configUrl: linkBase,
  });
  // The issue's declined asks follow the blocks' own checks; renderAdvice's cadence line must
  // stay the result's last line. A project document stands on no issue, and the server sends
  // declined asks only for a write that does.
  const declinedLines =
    resolved.issue === undefined
      ? []
      : declinedAskLines(edited.advice, resolved.issue.key, linkBase);
  const adviceLines = await otherAdviceLines(() =>
    renderAdvice(input.tool, resolvedTopic(resolved).label, edited.advice, {})
  );
  // The token of the document this edit produced, rendered as dispatch_doc_read renders it:
  // the next guarded edit passes it as `precondition.document` with no read in between. A
  // Dispatch server predating it returns none, and the result reads as it always did.
  const tokenTrailer = edited.token === undefined ? [] : [`Document token: ${edited.token}`];
  return {
    text: [
      `${applied}${await notSubscribed(resolvedTopic(resolved), input.subscriptions)}`,
      ...checkLines,
      ...declinedLines,
      ...tokenTrailer,
      ...adviceLines,
    ].join("\n"),
    details: writeResultDetails(resolved, {
      applied: edited.applied,
      ...(edited.version === null ? {} : { version: edited.version.number }),
      ...(nothingChanged ? { changed: false } : {}),
      ...(lostOps === undefined ? {} : { lost_ops: lostOps }),
      ...(edited.token === undefined ? {} : { token: edited.token }),
      ...(edited.advice === undefined ? {} : { advice: edited.advice }),
    }),
  };
}

/** `dispatch doc-read`: a document's markdown, its blocks, or an uploaded file's content. */
export async function docReadCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, ownerArguments, shown, documentOwner, resolveDocument } = context;
  const { agentArtifact } = ownerArguments;
  if (agentArtifact !== undefined) {
    // A conversation's upload is a file or picture; Dispatch keeps no document there.
    const artifact = await client.getAgentArtifact(agentArtifact.session, agentArtifact.slug);
    if (optionalBoolean(args, "blocks") === true) {
      throw new Error(
        `${artifact.name} is an uploaded ${artifact.kind}, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.`
      );
    }
    return readUploadedFile(
      client,
      artifact,
      { session: agentArtifact.session, artifact: artifact.slug },
      optionalNumber(args, "version") ?? agentArtifact.version,
      `agent/${agentArtifact.session}`,
      shown
    );
  }
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const resolved = await resolveDocument(documentOwner(), artifactReference);
  const version = optionalNumber(args, "version") ?? ownerArguments.ref?.version;
  if (optionalBoolean(args, "blocks") === true) {
    if (resolved.artifact.kind === "file" || resolved.artifact.kind === "image") {
      throw new Error(
        `${resolved.artifact.name} is an uploaded ${resolved.artifact.kind}, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.`
      );
    }
    const listed = await listDocumentBlocks(client, resolved.artifact);
    // The token and the version come from the one answer the blocks came from, so the
    // trailer names the document this listing was read from and nothing else.
    const trailer = [
      ...(listed.token === undefined ? [] : [`Document token: ${listed.token}`]),
      `Document version: ${listed.version === null ? "none yet" : listed.version}`,
    ];
    return {
      text: `${listed.text}\n\n${trailer.join("\n")}`,
      details:
        resolved.owner.kind === "project"
          ? {
              project: resolved.artifact.project,
              document: documentLabel(resolved.artifact.project, resolved.artifact.slug),
            }
          : { issue: resolved.issue?.key },
    };
  }
  if (resolved.artifact.kind === "file" || resolved.artifact.kind === "image") {
    const { artifact } = resolved;
    return readUploadedFile(
      client,
      artifact,
      resolved.owner.kind === "project"
        ? {
            project: artifact.project,
            document: documentLabel(artifact.project, artifact.slug),
          }
        : { issue: resolved.issue?.key },
      version,
      resolved.owner.kind === "project" ? resolved.owner.project : resolved.owner.issue,
      shown
    );
  }
  const documentPromise = client.docRead(resolved.artifact.id, version);
  const threadsPromise = openAnchoredThreads(client, resolved);
  const threadsResultPromise = threadsPromise.then(
    (value) => ({ status: "fulfilled" as const, value }),
    (reason) => ({ status: "rejected" as const, reason })
  );
  // A document error takes precedence over a thread error. Capture the concurrent thread
  // failure so it is handled without delaying a document failure.
  const document = await documentPromise;
  const threadsResult = await threadsResultPromise;
  if (threadsResult.status === "rejected") throw threadsResult.reason;
  const threads = threadsResult.value;
  const approval = approvalLine(resolved.artifact);
  const trailer = [
    ...("token" in document && document.token !== undefined
      ? [`Document token: ${document.token}`]
      : []),
    ...("sha256" in document && document.sha256 !== undefined
      ? [`Content: sha256:${document.sha256}`]
      : []),
    ...(approval === undefined ? [] : [approval]),
  ];
  return {
    text: [
      document.markdown,
      ...(trailer.length === 0 ? [] : [trailer.join("\n")]),
      ...(threads.length === 0 ? [] : ["Open anchored threads:", ...threads]),
    ].join("\n\n"),
    details:
      resolved.owner.kind === "project"
        ? {
            project: resolved.artifact.project,
            document: `${resolved.artifact.project}/${resolved.artifact.slug}`,
          }
        : { issue: resolved.issue?.key },
  };
}

/** `dispatch request-approval`: ask the human to approve this version of a document. */
export async function requestApprovalCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, ownerArguments, documentOwner, resolveDocument } = context;
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const resolved = await resolveDocument(documentOwner(), artifactReference);
  await refuseOpenDecisionBlocks(client, input.tool, resolved);
  const includeVerification = optionalBoolean(args, "include_verification") === true;
  const result = await client.requestApproval(resolved.artifact.id, {
    actor,
    summary: stringArg(args, "summary"),
    ...(includeVerification ? { include_verification: true } : {}),
  });
  // One line per requirement of the spec with no check, or the one line saying the spec
  // marks none, as Dispatch names them; never a missing plan task, since the plan comes
  // after the approval. A gap refuses nothing: the request is recorded, and a spec every
  // requirement of which has a check carries no line at all. A coverage the server could
  // not read says so, because silence here reads exactly like a spec with no gaps.
  const gaps = result.coverage ?? [];
  const coverageLines = result.coverage_unread
    ? [
        `Coverage of ${resolved.artifact.name}: unread. Dispatch could not read what the spec's requirements lack, so no line here says there is no gap; the request is recorded either way. Read the coverage again with dispatch read --issue, and mark each requirement the spec states with a requirement block and write a check that serves it, in the issue's Verification document, before the work starts.`,
      ]
    : gaps.length === 0
      ? []
      : [
          `Coverage of ${resolved.artifact.name}: ${gaps.map((gap) => `${gap}.`).join(" ")} Mark each requirement the spec states with a requirement block and write a check that serves it, in the issue's Verification document, before the work starts.`,
        ];
  if (result.ask === null) {
    return {
      text: [
        `${resolved.artifact.name} (document id ${resolved.artifact.id}) is already approved at version ${result.version} by ${result.approval.by?.id ?? "unknown"}; no new request was opened. A change outside the decision blocks after approval makes it stale, so request again only for such a version, once the human has agreed to every point in it, with a --summary naming what changed since version ${result.version}; the human's own answer to a decision block carries the approval when that answer is the whole change.`,
        ...coverageLines,
      ].join("\n"),
      details: {
        ...(resolved.owner.kind === "project"
          ? documentResultDetails(resolved.artifact)
          : { issue: resolved.issue?.key }),
        artifact: resolved.artifact.id,
        version: result.version,
      },
    };
  }
  const details = await followedAskDetails(client, result.ask, resolved.artifact);
  // A call that opened, reworded or handed back the request says so; one that found it already
  // waiting on the human says nothing changed, so a retry never reads as a fresh hand-back.
  const outcome = result.recorded
    ? `Approval requested for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}).`
    : `The approval request for ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${result.version} (ask ${result.ask.id}) already waits on the human, so this call changed nothing: nothing since it last reached the human (a newer version, a human's reply in its thread, or your progress note) left it waiting on you.`;
  return {
    text: [
      `${outcome} The human's Inbox asks: ${JSON.stringify(result.ask.question)}. The answer arrives as artifact.approved or artifact.changes_requested. An edit before the answer moves this request to the new version and leaves it waiting on you, and a change outside the decision blocks after approval makes the approval stale: either way, request again for the new version once the human has agreed to every point in it, which hands this request back or opens a new one, with a --summary naming what changed since the earlier approval. The human's own answer to a decision block carries the approval when that answer is the whole change since the approved version, and answers an open request Approve in their name: request nothing then.`,
      ...coverageLines,
    ].join("\n"),
    details: { ...details, artifact: resolved.artifact.id, version: result.version },
  };
}

/** `dispatch plan-approve`: record the plan review's approval of an issue's plan. */
export async function planApproveCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, documentOwner, resolveDocument } = context;
  const planOwner = documentOwner();
  const named = optionalString(args, "artifact");
  // The server resolves a plan by its role and the dashboard by the issue's
  // plan_artifact_id, precisely because a hand-renamed document must not displace the Plan
  // tab; the filename is only what an issue whose detail names no plan falls back on.
  const byRole =
    named === undefined && planOwner.kind === "issue"
      ? ((await client.getIssue(planOwner.issue)).plan_artifact_id ?? "")
      : "";
  const resolved = await resolveDocument(planOwner, named ?? (byRole || "plan.md"));
  const version = numberArg(args, "version");
  const review = await client.planApprove(resolved.artifact.id, { actor, version });
  return {
    text: `The plan review approved ${resolved.artifact.name} (document id ${resolved.artifact.id}) at version ${review.version}; no human is asked to approve a plan. A later plan version makes this approval stale: run the review again and record it the same way.`,
    details: {
      issue: resolved.issue?.key,
      artifact: resolved.artifact.id,
      version: review.version,
    },
  };
}

/** The document an upload under `name` would write the next version of: the owner's artifact
 *  whose slug or filename that name is, as the upload itself resolves one (`$2 in (name, slug)`,
 *  the slug first). Undefined where the owner holds no such document. */
function artifactUnderName(artifacts: readonly Artifact[], name: string): Artifact | undefined {
  const written = name.trim();
  return (
    artifacts.find((held) => held.slug === written) ??
    artifacts.find((held) => held.name === written)
  );
}

/** The refusal of an upload under a name its owner already holds: the document found, its kind,
 *  its newest version and who wrote that version, then the rule that names --force. */
function artifactNameExistsProblem(held: Artifact, owner: string): string {
  const versions = held.versions ?? [];
  const newest = versions[versions.length - 1];
  if (newest === undefined) {
    return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}), which has no version yet and which ${actorText(held.created_by)} created. ${ARTIFACT_NAME_EXISTS_RULE}`;
  }
  const authors = (newest.authors ?? []).map(actorText);
  const wroteIt = authors.length === 0 ? actorText(held.created_by) : authors.join(" and ");
  return `${owner} already holds ${held.name} (slug ${held.slug}, kind ${held.kind}) at version ${newest.number}, which ${wroteIt} wrote. ${ARTIFACT_NAME_EXISTS_RULE}`;
}

/**
 * How much of one line a refusal quotes whole. A line of a document is bounded by the document
 * itself, so one differing line can be as long as the whole text, and a quote nobody can read
 * to the end is one nobody acts on — the same reason STORED_TEXT_PRINTED bounds the stored
 * text below, and the cut is named the same way.
 */
const STORED_LINE_QUOTED = 200;

/** `line` as a refusal quotes it: its JSON spelling whole while the line is short, and
 *  otherwise that of its first STORED_LINE_QUOTED characters, with the cut named after it. */
function quotedStoredLine(line: string): string {
  if (line.length <= STORED_LINE_QUOTED) return JSON.stringify(line);
  const head = JSON.stringify(line.slice(0, STORED_LINE_QUOTED));
  return `${head} (cut after ${STORED_LINE_QUOTED} of its ${line.length} characters)`;
}

/** The first line the store would write otherwise, which is the line a reader fixes first. The
 *  two texts differ, so one of the three arms answers. Each quote is bounded, since the line it
 *  names can be as long as the document itself. */
function firstStoredDifference(given: string, stored: string): string {
  const written = given.split("\n");
  const holds = stored.split("\n");
  // Each side is read as `string | undefined`, since a line past the end of its own text is
  // undefined and the strict packages that compile this file say so. The two are not equal
  // here, so exactly one arm below answers and the pair can never both be undefined.
  for (let line = 0; line < Math.max(written.length, holds.length); line += 1) {
    const wrote = written[line];
    const held = holds[line];
    if (wrote === held) continue;
    if (wrote === undefined && held !== undefined) {
      return `the store writes a line ${line + 1}, ${quotedStoredLine(held)}`;
    }
    if (held === undefined && wrote !== undefined) {
      return `the store drops line ${line + 1}, ${quotedStoredLine(wrote)}`;
    }
    if (wrote !== undefined && held !== undefined) {
      return `line ${line + 1} reads ${quotedStoredLine(wrote)} and is stored as ${quotedStoredLine(held)}`;
    }
  }
  return "the stored text differs from the text given";
}

/**
 * How much of the stored text a refusal prints whole. Past it the refusal prints the first whole
 * lines that fit and says how much it cut: a document is up to 1 MiB, and a refusal nobody can
 * read to the end is one nobody acts on. The line a reader fixes first is named before the text
 * either way, so a cut refusal still says what to change.
 */
const STORED_TEXT_PRINTED = 2000;

/** The stored text as a refusal prints it, and what the refusal says of the cut: whole while it
 *  is short, and otherwise its first whole lines up to STORED_TEXT_PRINTED characters. */
function storedTextPrinted(stored: string): { readonly text: string; readonly cut: string } {
  if (stored.length <= STORED_TEXT_PRINTED) return { text: stored, cut: "" };
  const head = stored.slice(0, STORED_TEXT_PRINTED);
  const lastLineFeed = head.lastIndexOf("\n");
  const text = lastLineFeed === -1 ? head : head.slice(0, lastLineFeed + 1);
  return {
    text,
    cut: ` The text below is cut after ${text.length} of its ${stored.length} characters: change the line named above and send the text again.`,
  };
}

/**
 * The refusal of inline text the store would change: where the two first differ, the way out
 * this name has, and the text the store writes, so the writer can send exactly that text. The
 * markers hold the stored text apart from the prose around it. A name `--path` would store as
 * bytes has a second way out, which keeps the text as it was written; a Markdown name has none,
 * since `--path` parses and renders it the same way (`pathKeepsBytesOf`).
 */
function storedTextProblem(given: string, stored: string, name: string): string {
  const asBytes = pathKeepsBytesOf(name)
    ? ` To keep the text as you wrote it, send it as a file with ${PATH_KEEPS_BYTES}.`
    : "";
  const printed = storedTextPrinted(stored);
  return [
    `Dispatch stores this text under another spelling: ${firstStoredDifference(given, stored)}.`,
    ` Send the text Dispatch stores, which is between the two marker lines below.${asBytes}`,
    `${printed.cut}\n--- the text Dispatch stores ---\n${printed.text}`,
    "--- end of the text Dispatch stores ---",
  ].join("");
}

/**
 * The refusal of an upload whose stored-text check could not run. The check is the one thing that
 * says whether the store would write this text under another spelling, so a failure that is not
 * the route's absence leaves that question open, and an upload sent anyway would store the
 * changed text this check exists to stop.
 *
 * A body Dispatch itself refused as too large is the one such failure a repeat can never get
 * through, so that refusal names what to do instead of saying to send the upload again: the
 * text is past what the check reads, and a name whose extension keeps the bytes has `--path`,
 * which stores the file and asks this route nothing.
 */
function storedTextCheckFailed(error: unknown, name: string): string {
  const failure = `Dispatch could not say how it would store this text, so nothing was uploaded: POST /api/v1/markdown/round-trip failed with ${messageFor(error)}.`;
  if (!dispatchAnswered(error, 413)) {
    return `${failure} Send the upload again once that call is answered.`;
  }
  // Only a body past markdownRoundTripLimit reaches here, and that is always text past the
  // megabyte a markdown document may hold, so every remedy this names has to be one that
  // takes the text out of one document: a document of its own is no remedy at all, since the
  // upload refused is already one and a second would be refused the same way.
  const asBytes = pathKeepsBytesOf(name)
    ? ` Send it as a file with ${PATH_KEEPS_BYTES}, which asks this check nothing.`
    : "";
  return `${failure} This text is longer than the check reads, and sending the upload again does not change that: a markdown document is at most 1 MiB, so shorten it or split it across documents of at most that.${asBytes}`;
}

/**
 * Whether the round trip failed because Dispatch itself holds no such route: the 404 or 405 a
 * Dispatch older than POST /api/v1/markdown/round-trip answers, as its own JSON. Such a server
 * cannot say what it would store, and refused the upload itself before this check existed, so
 * the upload goes on and that server makes its own answer.
 *
 * A 404 or 405 page from an ingress, a proxy or a load balancer is `DispatchGatewayError`,
 * which says nothing about the route, so `dispatchAnswered` is what this asks: reading such a
 * page as an absent route would skip the check and store the respelled text it exists to stop.
 */
function roundTripRouteAbsent(error: unknown): boolean {
  return dispatchAnswered(error, 404) || dispatchAnswered(error, 405);
}

/** `text` without a final line feed, which is what the stored-text check compares. The store
 *  ends every document it holds with one, so text a writer gave without one comes back with one
 *  and nothing else changed (`markdown_round_trip_test.go` pins "one" stored as "one\n"). That
 *  one line feed is the only difference the check forgives. */
function withoutFinalLineFeed(text: string): string {
  return text.endsWith("\n") ? text.slice(0, -1) : text;
}

/**
 * Text Go's `strings.TrimSpace` empties: no characters at all, or only characters Go's
 * `unicode.IsSpace` reports - `\t`, `\n`, `\v`, `\f`, `\r`, a space, U+0085 and U+00A0, and the
 * Unicode `White_Space` property above Latin-1, which is U+1680, U+2000 to U+200A, U+2028,
 * U+2029, U+202F, U+205F and U+3000 (`IsSpace` in Go's `unicode/graphic.go`, reading the
 * `_White_Space` table of its `unicode/tables.go`).
 *
 * The set is written out because it is Go's and not JavaScript's: walked over every Unicode
 * scalar, `String.prototype.trim` and `strings.TrimSpace` differ on exactly two characters.
 * `trim` removes U+FEFF, which Go keeps, so `trim` calls U+FEFF blank where the server reads
 * content; Go removes U+0085, which `trim` keeps, so `trim` calls U+0085 content where the
 * server reads blank. This is the one place that pair is written out, and the lines that turn
 * on it cite this comment rather than repeating it.
 *
 * `packages/envoy/internal/dispatch/text/refs.go` records a split of the same class in another
 * pair of sets ("The whitespace is spelled out rather than written `\s`, which means another
 * set in each language: JavaScript's also matches a vertical tab and U+FEFF"). That pair is the
 * two languages' `\s`, and it differs in a vertical tab and U+FEFF, not in the two characters
 * above.
 *
 * `packages/contracts/fixtures/go-unicode-isspace.json` holds the 25 scalars `unicode.IsSpace`
 * reports. This pattern is held to that list over every scalar by `goBlankPattern` > `calls
 * blank exactly the scalars the committed list holds`, and Go's own predicate by
 * TestUnicodeIsSpaceReportsTheCommittedScalars in
 * `packages/envoy/internal/dispatch/text/whitespace_fixture_test.go`: a character either side
 * gains or loses reddens its test, and the list and this pattern are corrected together.
 */
export const goBlankPattern =
  /^[\t\n\v\f\r \u0085\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000]*$/u;

/** `dispatch artifact`: upload a document, a picture or a file to an issue or a project. */
export async function artifactCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, issue } = context;
  const summary = optionalString(args, "summary");
  const name = stringArg(args, "name");
  const content = optionalString(args, "content");
  const path = optionalString(args, "path");
  // The base an upload over a document that already exists replaces: the document token
  // alone, which is all the server takes from an upload. It rides every shape below, a create
  // that stores nothing included, because the server is what decides a token that names
  // nothing it can replace: it refuses a token on a first upload, and a blank write over a
  // held name whatever token it names.
  const precondition = optionalPrecondition<UploadPrecondition>(args);
  // What every create carries, whatever it stores under the name.
  const options = {
    name,
    ...(summary === undefined ? {} : { summary }),
    ...(precondition === undefined ? {} : { precondition }),
    actor,
  };
  const artifactOwner = documentOwner();
  // The server's own `blank`: a body carrying no content, or content Go's
  // `strings.TrimSpace` empties - the `blank :=` line of jsonArtifactUpload in
  // packages/envoy/internal/dispatch/api/artifacts.go, whose set is goBlankPattern's.
  // JavaScript's `trim` is another set, so it is not read here: goBlankPattern's own comment
  // says which two characters the two sets differ on. Both checks below read this one
  // predicate.
  const blank = content === undefined || goBlankPattern.test(content);
  // An upload under a name the owner already holds writes that document's next version, over
  // the text it holds, so the command reads what the owner holds and refuses it. --force is
  // what asks for the version, and the API writes one for every other caller as before.
  // A create that stores nothing - no path, and content absent or blank - is held to none of
  // this: it has no version to write, so the server refuses it under a held name whatever
  // force says, and that refusal names the rule which applies to it. Refusing it here would
  // answer it with --force, which writes nothing for a create that stores nothing, so it goes
  // through and reads the server's own answer.
  if ((path !== undefined || !blank) && optionalBoolean(args, "force") !== true) {
    const held = artifactUnderName(
      artifactOwner.kind === "project"
        ? // unlinked, as every other read of a project's own documents is: without it the
          // answer carries every issue-attached document of the project too, and an upload
          // naming one of those is refused although the server would write a new document.
          await client.listProjectArtifacts(artifactOwner.project, true)
        : await client.listIssueArtifacts(issue()),
      name
    );
    if (held !== undefined) {
      throw new ToolInputError(input.tool, [
        artifactNameExistsProblem(
          held,
          artifactOwner.kind === "project" ? `project ${artifactOwner.project}` : `issue ${issue()}`
        ),
      ]);
    }
  }
  // Inline text is stored as Markdown: Dispatch parses it and stores its rendering, so a
  // text it would write under another spelling is refused here rather than stored changed.
  // The round trip is the store's own parse and render (POST /api/v1/markdown/round-trip).
  // Only a Dispatch that holds no such route leaves the upload to make its own refusal, as
  // it did before this check: every other failure refuses the upload, since a check that did
  // not run says nothing about what the store would write. Blank text goes to the server
  // unchecked: the round trip answers the empty string for every text this predicate calls
  // blank, since storedMarkdown returns "" for whatever `strings.TrimSpace` empties before
  // it parses anything (that package's markdown_round_trip.go), and the empty document is
  // what such a create asks for. That short answer is not what the store writes for each of
  // them: the parse and render a seed runs keep every character of this set but a tab, a
  // line feed, a carriage return and a space, each of the rest rendering to itself.
  if (!blank) {
    let stored: string | undefined;
    try {
      stored = await client.markdownRoundTrip(content);
    } catch (error) {
      if (!roundTripRouteAbsent(error)) {
        throw new Error(storedTextCheckFailed(error, name), { cause: error });
      }
    }
    // The store ends a document with a line feed the writer's own text need not carry, and
    // that one line feed is no other spelling: both sides lose at most one before they are
    // compared. Every other difference is refused.
    if (stored !== undefined && withoutFinalLineFeed(stored) !== withoutFinalLineFeed(content)) {
      throw new ToolInputError(input.tool, [storedTextProblem(content, stored, name)]);
    }
  }
  // A call with a path uploads that file, one with content stores that text, and one with
  // neither makes an empty document the writer fills in afterwards with dispatch doc-edit.
  const artifactInput =
    path !== undefined
      ? {
          ...options,
          // The bytes AskGate's check read, when it read them, typed as Bun types the path.
          file:
            input.uploadBytes === undefined
              ? Bun.file(resolvePath(input.cwd, path))
              : // The copy gives the bytes a plain buffer of their own, which a Blob takes.
                new Blob([new Uint8Array(input.uploadBytes)], {
                  type: Bun.file(resolvePath(input.cwd, path)).type,
                }),
        }
      : content !== undefined
        ? { ...options, content }
        : options;
  const result =
    artifactOwner.kind === "project"
      ? await client.projectArtifact(artifactOwner.project, artifactInput)
      : await client.artifact(issue(), artifactInput);
  const artifactRef = dispatchDocumentRef(
    artifactOwner.kind === "project" ? artifactOwner.project : issue(),
    result.artifact.slug
  );
  const uploadOwner =
    artifactOwner.kind === "project" ? documentTopic(result.artifact) : issueTopic(issue());
  const adviceLines = renderAdvice(input.tool, uploadOwner.label, result.advice, {
    isPrimarySpec: result.artifact.primary || result.artifact.name === "spec.md",
  });
  return {
    text: [
      path === undefined && content === undefined
        ? `Made ${result.artifact.name} as an empty document (artifact slug ${result.artifact.slug}; ${artifactRef}). Write in it with dispatch doc-edit.${await notSubscribed(uploadOwner, input.subscriptions)}`
        : `Uploaded ${result.artifact.name} as version ${result.version.number} (artifact slug ${result.artifact.slug}; ${artifactRef})${await notSubscribed(uploadOwner, input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details:
      artifactOwner.kind === "project"
        ? {
            ...documentResultDetails(result.artifact),
            version: result.version.number,
            ...(result.advice === undefined ? {} : { advice: result.advice }),
          }
        : {
            issue: issue(),
            artifact: result.artifact.id,
            version: result.version.number,
            ...(result.advice === undefined ? {} : { advice: result.advice }),
          },
  };
}
