import { resolve as resolvePath } from "node:path";
import type {
  Artifact,
  ArtifactBlock,
  EditOp,
  EditPrecondition,
  UploadPrecondition,
} from "@legion/contracts";
import { goBlankPattern } from "@legion/contracts/go-blank";
import { commandName } from "../dispatch-command";
import type { AsOfRead, DispatchClient } from "../dispatch-http";
import { dispatchDocumentRef, documentLabel, issueTopic } from "../dispatch-owner";
import { shownPictureLine } from "../dispatch-picture-tools";
import {
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  PICTURE_SNIFF_BYTES,
  readPictureHead,
  sniffPictureType,
  toolImage,
} from "../dispatch-pictures";
import { messageFor } from "../errors";
import { decisionBlockLines, declinedAskLines } from "../related-check";
import { ToolInputError } from "../tool-input-errors";
import { otherAdviceLines, renderAdvice } from "./advice";
import { leftOutAt, leftOutWords } from "./as-of";
import {
  askOnceWithinDryRun,
  type CommandContext,
  type DispatchToolResult,
  type DryRunContext,
  type DryRunVerdict,
  numberArg,
  optionalBoolean,
  optionalNumber,
  optionalPrecondition,
  optionalString,
  type ReadingContext,
  stringArg,
} from "./context";
import { approvalLine, documentPlaysPlan, liveBlocks } from "./document-targets";
import {
  OPEN_BLOCK_ASK_THE_HUMAN,
  OPEN_BLOCK_WAIVED,
  refuseOpenDecisionBlocks,
  refuseRemovingOpenDecisionBlocks,
} from "./open-decision-blocks";
import { openAnchoredThreads, waitsOnPerson } from "./render-item";
import {
  documentResultDetails,
  documentTopic,
  followedAskDetails,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";
import {
  artifactTextCheck,
  documentFileBytes,
  type StoredTextCheck,
  storedTextCheckFailed,
  uploadedFile,
} from "./stored-text";
import { artifactNameExistsProblem, artifactUnderName, uploadKindOf } from "./upload-checks";

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
 * bytes when it is not. The header names the version read by its hash, on the line under its
 * first: the latest version's `Document token:` line, as a document read prints its token, which
 * the upload of the next version names as its precondition; a named version's `Content: sha256:`
 * line, as a named document version's read prints its hash. It goes in the header, never after
 * the text, so the text after the header is the stored file unchanged, a leading byte order mark
 * included (the decoder keeps it), and a script or patch read back ends where the file ends. The
 * dispatch command changes two things, as it does for every output it prints: it adds one line
 * feed after a file that ends without one, and under Claude Code or Codex it cuts an output past
 * 25,000 characters and writes the whole of it to a file it names (`fitOutput` in
 * dispatch-cli.ts). Dispatch states both beside the version in the artifact's
 * version list, so no other read is made for them, and a Dispatch or a version that states none
 * prints no line: such a Dispatch takes no token on a file's upload. A file carries no anchors or
 * approval, so nothing else is read. A picture is shown whatever `shown` holds, and joins it
 * under its address, so the session's later reads name it instead of sending it. At `read`'s
 * cut, the version is the one that stood then.
 */
async function readUploadedFile(
  client: DispatchClient,
  artifact: Artifact,
  details: Record<string, unknown>,
  requested: number | undefined,
  owner: string,
  shown: Set<string> | undefined,
  read: AsOfRead = {}
): Promise<DispatchToolResult> {
  const versions = artifact.versions ?? [];
  const latest = Math.max(0, ...versions.map((version) => version.number));
  const number = requested ?? latest;
  const of = number === latest ? "" : ` of ${latest}`;
  const bytesRoute = `GET /api/v1/artifacts/${artifact.id}/versions/${number} serves its bytes.`;
  const stated = versions.find((version) => version.number === number);
  const hashLine =
    requested === undefined
      ? stated?.token === undefined
        ? ""
        : `\nDocument token: ${stated.token}`
      : stated?.sha256 === undefined
        ? ""
        : `\nContent: sha256:${stated.sha256}`;
  const tooLarge = (mime: string, bytes: number): DispatchToolResult => ({
    text:
      `${oversizePictureText(artifact.name, bytes)}, so dispatch doc-read cannot show this ` +
      `uploaded ${mime} picture (version ${number}${of}). ${bytesRoute}${hashLine}`,
    details,
  });
  if (
    artifact.kind === "image" &&
    stated?.size !== undefined &&
    stated.size > PICTURE_SHOWN_MAX_BYTES
  ) {
    return tooLarge(stated.mime ?? "image", stated.size);
  }
  const file = await client.fileVersion(artifact.id, number, read);
  const size = `${file.bytes.length.toLocaleString("en-US")} bytes`;
  if (artifact.kind === "image") {
    const image = toolImage(file.bytes);
    if (image !== undefined) {
      const address = `dispatch://${owner}/artifact/${artifact.slug}@v${number}`;
      shown?.add(address);
      return {
        text:
          `Picture ${artifact.name}: ${image.mimeType}, version ${number}${of}, ${size}.${hashLine}\n` +
          shownPictureLine(1, address, artifact.name, image.mimeType, file.bytes.length),
        details,
        images: [image],
      };
    }
    if (sniffPictureType(file.bytes) !== undefined) return tooLarge(file.mime, file.bytes.length);
  }
  let text: string;
  try {
    // ignoreBOM keeps a leading byte order mark as text, so the text encodes back to the stored
    // bytes.
    text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(file.bytes);
  } catch {
    return {
      text:
        `${artifact.name} is an uploaded ${file.mime} file (version ${number}${of}, ${size}) that is not ` +
        `UTF-8 text, so dispatch doc-read cannot show it. ${bytesRoute}${hashLine}`,
      details,
    };
  }
  return {
    text: `File ${artifact.name}: ${file.mime}, version ${number}${of}, ${size}.${hashLine}\n\n${text}`,
    details,
  };
}

/** How many characters of a block's text `dispatch doc-read --blocks` shows: enough to tell blocks
 *  apart without printing the document twice. */
const BLOCK_OPENING_CHARS = 60;

/** A table row's or cell's id as `dispatch doc-read --blocks` prints it: "no id yet" for one that
 *  Dispatch gave no id, and the words for one the answer left out (`leftOutWords`), as an answer
 *  at a cut does, since no record at the cut gives the ids of a table's rows and cells. */
function tablePartId(id: string | undefined, asOf: string | undefined): string {
  if (id === undefined) return `id ${leftOutWords(asOf)}`;
  return id === "" ? "no id yet" : `id ${id}`;
}

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
 * leave the offsets slicing text they were not computed over. At `read`'s cut each member the
 * answer left out prints as left out at that cut.
 */
async function listDocumentBlocks(
  client: DispatchClient,
  artifact: Artifact,
  read: AsOfRead = {}
): Promise<{ text: string; token: string | undefined; version: number | null }> {
  const live = await liveBlocks(client, artifact, read);
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
    // At a cut a block's token and anchored counts are left out, and so is the id of an untyped
    // block of a version no index holds: none prints as a value, nor as "no token".
    const { asOf } = read;
    const id: string | undefined = block.id;
    const shownId = id === undefined ? `(id ${leftOutWords(asOf)})` : id;
    const token = block.token ?? (asOf === undefined ? "no token" : `token ${leftOutAt(asOf)}`);
    const references: ArtifactBlock["references"] | undefined = block.references;
    const anchored =
      references === undefined
        ? `anchored comments and asks ${leftOutWords(asOf)}`
        : `${references.comments} comment${references.comments === 1 ? "" : "s"}, ${references.asks} ask${references.asks === 1 ? "" : "s"}`;
    lines.push(`${indent}${shownId} ${block.type} (${token}; ${anchored}): ${opening}`);
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
          lines.push(`${indent}  row ${row.index}, ${tablePartId(row.id, asOf)}`);
          for (const cell of row.cells ?? []) {
            const under = cell.header === "" ? "" : `, under ${JSON.stringify(cell.header)}`;
            lines.push(
              `${indent}    row ${cell.row} column ${cell.column}, ${tablePartId(cell.id, asOf)}${under}: ${cell.text === "" ? "(empty)" : cell.text}`
            );
          }
        }
      }
    }
    holders.push(block);
  }
  return { text: lines.join("\n"), token: live.token, version: live.version };
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
    linkBase,
  });
  // The issue's declined asks follow the blocks' own checks; renderAdvice's cadence line must
  // stay the result's last line. A project document stands on no issue, and the server sends
  // declined asks only for a write that does.
  const declinedLines =
    resolved.issue === undefined
      ? []
      : declinedAskLines(edited.advice, resolved.issue.key, linkBase);
  const adviceLines = await otherAdviceLines(() =>
    renderAdvice(input.tool, resolvedTopic(resolved).label, edited.advice, {
      isPlan: documentPlaysPlan(resolved.artifact),
    })
  );
  // What the edit wrote, as the server read it back off the tree it wrote: the markdown of
  // each block it added, changed or moved, and a line for each one it removed or emptied, so
  // `Applied 2 ops` no longer reads the same whether the edit wrote a list or a paragraph of
  // dashes. A batch that changed nothing carries none, and an older server omits the field,
  // which reads as it always did. It is printed under the decision-block lines, since up to
  // 2000 bytes of markdown between the first line and them would break the promise the tool
  // description makes of them: that they come after the result's first line
  // (packages/contracts/src/dispatch-tools.ts).
  const written = edited.written;
  const writtenLines = written === undefined || written === "" ? [] : [written];
  // The token of the document this edit produced, rendered as dispatch_doc_read renders it:
  // the next guarded edit passes it as `precondition.document` with no read in between. A
  // Dispatch server predating it returns none, and the result reads as it always did.
  const tokenTrailer = edited.token === undefined ? [] : [`Document token: ${edited.token}`];
  return {
    text: [
      `${applied}${await notSubscribed(resolvedTopic(resolved), input.subscriptions)}`,
      ...checkLines,
      ...declinedLines,
      ...writtenLines,
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

/** `dispatch doc-read`: a document's markdown, its blocks, or an uploaded file's content; at a
 *  cut, each of them as it stood then. */
export async function docReadCommand(
  context: ReadingContext<CommandContext>
): Promise<DispatchToolResult> {
  const { args, client, ownerArguments, shown, documentOwner, resolveDocument, reading } = context;
  const { cut } = reading;
  const { agentArtifact } = ownerArguments;
  if (agentArtifact !== undefined) {
    // A conversation's upload is a file or picture; Dispatch keeps no document there.
    const artifact = await client.getAgentArtifact(agentArtifact.session, agentArtifact.slug, cut);
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
      shown,
      cut
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
    const listed = await listDocumentBlocks(client, resolved.artifact, cut);
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
      shown,
      cut
    );
  }
  const documentPromise = client.docRead(resolved.artifact.id, version, cut);
  const threadsPromise = openAnchoredThreads(client, resolved, cut);
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
  const approval = approvalLine(resolved.artifact, typeof resolved.issue?.closed_at === "string");
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

/** `dispatch request-approval`'s words around the open blocks it refuses over. */
const APPROVAL_REQUEST_OPEN_BLOCKS = {
  effect:
    "Answering one writes a new version, which would move this request to that version and leave it waiting on you.",
  remedy:
    "Do not request approval over an open block, even when a human asked for it. " +
    OPEN_BLOCK_ASK_THE_HUMAN +
    " When the human approved an earlier version and their answer is the whole change since it, Dispatch carries the approval to the new version in their name: request nothing. When the answer changes design text outside the block, rewrite that text (never to restate the answer), then request approval again once the human has agreed to every point in the new version, with a one-sentence --summary naming what changed since the earlier approval. On a document nobody has approved, wait for that version, then request. " +
    OPEN_BLOCK_WAIVED +
    ", write their decision into the text with dispatch doc-edit, then request approval again once the human has agreed to every point in the new version. The call opens the request, or hands an open one back to the human.",
} as const;

/** `dispatch request-approval`: ask the human to approve this version of a document. */
export async function requestApprovalCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, ownerArguments, documentOwner, resolveDocument } = context;
  const artifactReference =
    optionalString(args, "artifact") ??
    (ownerArguments.ref?.kind === "spec" || ownerArguments.ref?.kind === "artifact"
      ? ownerArguments.ref.id
      : undefined);
  const resolved = await resolveDocument(documentOwner(), artifactReference);
  await refuseOpenDecisionBlocks(client, resolved, {
    call: `dispatch ${commandName(input.tool)}`,
    ...APPROVAL_REQUEST_OPEN_BLOCKS,
  });
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
  // Every branch here carries a question, a new request and a hand-back alike, so each names the
  // person it reaches. The settled branch above carries none and names nobody.
  const waitsOn = waitsOnPerson(result.ask);
  return {
    text: [
      `${outcome}${waitsOn === undefined ? "" : ` It waits on ${waitsOn}.`} The human's Inbox asks: ${JSON.stringify(result.ask.question)}. The answer arrives as artifact.approved or artifact.changes_requested. An edit before the answer moves this request to the new version and leaves it waiting on you, and a change outside the decision blocks after approval makes the approval stale: either way, request again for the new version once the human has agreed to every point in it, which hands this request back or opens a new one, with a --summary naming what changed since the earlier approval. The human's own answer to a decision block carries the approval when that answer is the whole change since the approved version, and answers an open request Approve in their name: request nothing then.`,
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

/** `dispatch artifact`: upload a document, a picture or a file to an issue or a project. */
export async function artifactCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, issue, newClient } = context;
  const summary = optionalString(args, "summary");
  const name = stringArg(args, "name");
  const content = optionalString(args, "content");
  const path = optionalString(args, "path");
  // The base an upload over a document that already exists replaces: the document token
  // alone, which is all the server takes from an upload. It rides every shape below, a create
  // that stores nothing included, because the server is what decides a token that names
  // nothing it can replace: it refuses a token on a first upload, and a blank write over a
  // held name whatever well-formed token it names, ahead of every check of the token against
  // the document; a token that is not well formed it answers as malformed first.
  const precondition = optionalPrecondition<UploadPrecondition>(args);
  // What every create carries, whatever it stores under the name.
  const options = {
    name,
    ...(summary === undefined ? {} : { summary }),
    ...(precondition === undefined ? {} : { precondition }),
    actor,
  };
  const artifactOwner = documentOwner();
  // --force asks for the next version of a document the owner holds under the name, which is
  // judged against the document it replaces rather than as a new one.
  const force = optionalBoolean(args, "force") === true;
  // The server's own `blank`: a body carrying no content, or content Go's
  // `strings.TrimSpace` empties - the `blank :=` line of jsonArtifactUpload in
  // packages/envoy/internal/dispatch/api/artifacts.go, whose set is goBlankPattern's.
  // JavaScript's `trim` is another set, so it is not read here: goBlankPattern's own comment
  // says which two characters the two sets differ on. The held-name check reads this predicate;
  // what the stored-text check makes of blank text is that check's own (storedTextCheck).
  const blank = content === undefined || goBlankPattern.test(content);
  // The file a path names, as its upload sends it (uploadedFile), under the type that upload
  // declares, which is what Dispatch reads for whether it stores the file as a Markdown document.
  // Such a file's bytes are checked here (documentFileBytes) and refused there, before anything is
  // asked, past the bytes one document may hold and then for its first U+0000 or byte that is not
  // UTF-8, in the upload's order and words, so the bytes the file round trip is asked about below
  // are the bytes sent. A file documentFileRead passes with no bytes (one Dispatch stores as its
  // bytes, or a FIFO, a device or a socket) is not read here, is asked nothing, and goes up as the
  // file. The bytes checked and sent are those the CLI read where it read them (`uploadBytes`:
  // AskGate's, else the Markdown file's it checked once the flags were read, `runDispatchCli`),
  // copied once into a plain buffer of their own, which a Blob takes; the file is read here only
  // where the caller holds none of its bytes.
  const file = path === undefined ? undefined : uploadedFile(input.cwd, path);
  const heldBytes = input.uploadBytes === undefined ? undefined : new Uint8Array(input.uploadBytes);
  const documentBytes = file === undefined ? undefined : await documentFileBytes(file, heldBytes);
  // The check of the text alone, the one the dry run makes (artifactTextCheck chooses it for both),
  // so the call and its dry run refuse a text alike. It comes before the read of the held name
  // below, which the dry run does not make, so a text it refuses is refused for the text under a
  // held name too, and so is inline text that is not blank whose check got no answer (`failed`,
  // storedTextCheckFailed), which the dry run passes with a warning. The held name refuses only a
  // text the check passes.
  //
  // Inline text takes storedTextCheck, which decides what each answer means for the call: text past
  // the bytes one document may hold is refused without asking, as the server refuses such an upload
  // whatever name it carries (documentTooLarge); a text the store would write under another
  // spelling, or one Dispatch's round trip refuses as its upload would, is refused; and so is an
  // upload of text that is not blank whose check did not run, since that check says nothing about
  // what the store would write. A text whose typed blocks' attributes differ from Dispatch's only
  // in their order is no other spelling: it goes up as Dispatch writes it (storedUpload). A
  // Markdown file's bytes take storedFileCheck, which asks the file round trip for its refusal
  // alone: the store keeps a Markdown file as it spells it, so no spelling is refused, and
  // Dispatch refuses there, in the upload's own words, what the upload of those bytes is refused
  // for. Without --force each check asks for the refusals of a new document, which this call
  // makes; with it, the upload may replace a document, which is judged by looser rules, so it
  // asks for none of them, and Dispatch answers as the most permissive replacement
  // takes the text: with the rendering it stores, which the inline check compares as any other, or
  // with a refusal every replacement makes. So the upload can refuse what the check did not ask
  // for: under a name the owner does not hold, --force uploads a new document, which makes those
  // refusals; under a name the owner holds, the replacement holds those rules for the blocks it
  // adds or changes, and refuses a rendering that grows past the bytes one document holds over the
  // version it replaces. A text the check leaves unchecked - a Dispatch without the route, or a
  // file or blank text whose check got no answer - goes up, and the upload makes its own refusal.
  // A file documentFileRead passes with no bytes (one Dispatch stores as its bytes, or a FIFO, a
  // device or a socket) is asked nothing.
  //
  // Inline text is asked through the call's own client, which asks again after a failed attempt,
  // since a failed check of text that is not blank refuses the upload. A file goes up whatever its
  // check failed with, so asking again would only delay the upload: its check goes through a
  // client of its own that asks once within the dry run's bound, with the dry run's own client
  // options (askOnceWithinDryRun).
  const check = artifactTextCheck({
    tool: input.tool,
    name,
    content,
    bytes: documentBytes,
    force,
    textClient: () => client,
    fileClient: () => newClient(askOnceWithinDryRun()),
  });
  const stored = await check?.();
  if (stored?.kind === "refused") throw stored.refusal;
  if (stored?.kind === "failed") throw storedTextCheckFailed(stored);
  // An upload under a name the owner already holds writes that document's next version, over
  // the text it holds, so the command reads what the owner holds and refuses it. --force is
  // what asks for the version, and the API writes one for every other caller as before.
  // A create that stores nothing - no path, and content absent or blank - is held to none of
  // this: it has no version to write, so the server refuses it under a held name whatever
  // force says, and that refusal names the rule which applies to it. Refusing it here would
  // answer it with --force, which writes nothing for a create that stores nothing, so it goes
  // through and reads the server's own answer.
  if ((path !== undefined || !blank) && !force) {
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
      // The kind Dispatch would store this upload as decides what the refusal may advise: an
      // upload of another kind than the name holds is refused under that name whatever it
      // carries, so it is told to take another name. Only a path that is not Markdown has bytes
      // to read, and only their first PICTURE_SNIFF_BYTES decide.
      const kind = await uploadKindOf(path, (named) =>
        input.uploadBytes === undefined
          ? readPictureHead(resolvePath(input.cwd, named))
          : Promise.resolve(input.uploadBytes.subarray(0, PICTURE_SNIFF_BYTES))
      );
      throw new ToolInputError(input.tool, [
        artifactNameExistsProblem(
          held,
          artifactOwner.kind === "project"
            ? `project ${artifactOwner.project}`
            : `issue ${issue()}`,
          kind
        ),
      ]);
    }
  }
  // A call with a path uploads that file, one with content stores that text, and one with
  // neither makes an empty document the writer fills in afterwards with dispatch doc-edit. The
  // file's bytes, where the call holds them - a document's, read above, or those AskGate's check
  // read - are sent as they were read, under the file's own type. The text is sent as given, or,
  // where it differs from what Dispatch stores only in the order of a typed block's attributes,
  // as Dispatch stores it, and the answer names each such block (the check's `reordered`).
  const fileBytes = documentBytes ?? heldBytes;
  const sent = stored?.kind === "reordered" ? stored.sent : content;
  const reordered = stored?.kind === "reordered" ? stored.lines : [];
  const artifactInput =
    file !== undefined
      ? {
          ...options,
          file: fileBytes === undefined ? file : new Blob([fileBytes], { type: file.type }),
        }
      : sent !== undefined
        ? { ...options, content: sent }
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
    isPlan: documentPlaysPlan(result.artifact),
  });
  return {
    text: [
      path === undefined && content === undefined
        ? `Made ${result.artifact.name} as an empty document (artifact slug ${result.artifact.slug}; ${artifactRef}). Write in it with dispatch doc-edit.${await notSubscribed(uploadOwner, input.subscriptions)}`
        : `Uploaded ${result.artifact.name} as version ${result.version.number} (artifact slug ${result.artifact.slug}; ${artifactRef})${await notSubscribed(uploadOwner, input.subscriptions)}`,
      ...reordered,
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

/** `text` as the end of a sentence: with a full stop added unless it already ends one. */
function endedSentence(text: string): string {
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

/**
 * `dispatch artifact --dry-run`'s step after the argument check: the call's own check of the text,
 * so the dry run refuses a text the call would refuse for its spelling or for the text alone, with
 * the call's own refusal and exit code, and stores nothing. The check is chosen as the call chooses
 * it (`artifactTextCheck`). Inline text takes the call's check (`storedTextCheck`), which decides
 * what blank text is asked. A `--path` file Dispatch stores as a Markdown document is checked as
 * the call checks it (`documentFileBytes`), in the bytes the CLI read of it (`uploadBytes`), or
 * read here where the CLI holds none, which makes the call's checks of its size and then of its
 * first U+0000 or byte that is not UTF-8, and then takes the call's file check
 * (`storedFileCheck`), which asks the file round trip for its refusal alone: the round trip
 * refuses, in the upload's own words, what the upload of that file is refused for, and the
 * call throws that refusal before it uploads. A file `documentFileRead` passes with no bytes (one
 * Dispatch stores as its bytes, or a FIFO, a device or a socket) is not read here, and the call
 * uploads the file itself; such a file, and a name alone, is asked nothing, by the call as by the
 * dry run. Both checks ask for a new document's refusals only without
 * `--force`, as the call's do, so every refusal the dry run gets is the call's. With `--force` they
 * ask for none of them, so a dry run passes, with no warning, a text the call's upload can then
 * refuse for one of them: under a name the owner does not hold, because that upload makes a new
 * document; under a name the owner holds, because a replacement holds those rules for the blocks
 * it adds or changes, and refuses a rendering that grows past the bytes one document holds over
 * the version it replaces. It reads nothing else the call reads: not whether the owner holds the
 * name, and none of the server's other refusals. The call makes its check before it reads the
 * held name, so a text the check refuses
 * gets the same refusal from both under a held name too; a text the check passes the call still
 * refuses under a name the owner holds, without `--force`, and the dry run passes it. A check that
 * cannot run passes, with a warning that says why and what the call does, as the check's kind
 * says it: inline text that is `unchecked` the call uploads unchecked; a file the call asks about
 * once more, as this step asks, and uploads unchecked if that gets no answer either; and a
 * `failed` text it refuses until Dispatch answers. A dry run that failed there would let an
 * AskGate older than DISPATCH_DRY_RUN_LOCAL, which runs the call after a failed dry run, post this
 * write unreviewed; AskGate now sets that variable, under which this step does not run. A text
 * the CLI refuses once the flags are read (`runDispatchCli`) never reaches this step.
 */
export async function artifactDryRun(context: DryRunContext): Promise<DryRunVerdict> {
  const { tool, args, cwd, uploadBytes, client, deadline, deadlineMs } = context;
  const content = optionalString(args, "content");
  const path = optionalString(args, "path");
  let bytes: Uint8Array | undefined;
  if (content === undefined && path !== undefined) {
    try {
      bytes = await documentFileBytes(uploadedFile(cwd, path), uploadBytes);
    } catch (error) {
      // The call checks the file the same way before it asks anything, and throws this.
      return {
        kind: "refused",
        refusal: error instanceof Error ? error : new Error(messageFor(error)),
      };
    }
  }
  const check = artifactTextCheck({
    tool,
    name: stringArg(args, "name"),
    content,
    bytes,
    force: optionalBoolean(args, "force") === true,
    textClient: client,
    fileClient: client,
  });
  if (check === undefined) return { kind: "passed" };
  // What the check is about, which each warning names, and what the call does where the check
  // gives no answer about it.
  const subject = content === undefined ? "the file" : "the text";
  const uploadsUnchecked =
    content === undefined
      ? "The call asks once more, and uploads the file unchecked if it gets no answer either."
      : "The call uploads the text unchecked.";
  const unasked = (why: string, then: string): DryRunVerdict => ({
    kind: "passed",
    warning: `dispatch: warning: the dry run could not ask Dispatch about ${subject}: ${endedSentence(why)} ${then}`,
  });
  let stored: StoredTextCheck;
  try {
    stored = await check();
  } catch (error) {
    // A session with no Dispatch, which the call refuses in the same words: the check throws
    // only what building the client throws.
    return unasked(messageFor(error), "The call is refused for the same reason.");
  }
  // A text whose typed blocks' attributes differ from Dispatch's only in their order the call
  // uploads as Dispatch writes it, refusing nothing, so the dry run passes it too.
  if (stored.kind === "kept" || stored.kind === "reordered") return { kind: "passed" };
  if (stored.kind === "refused") return { kind: "refused", refusal: stored.refusal };
  if (stored.kind === "unchecked" && stored.routeAbsent) {
    return {
      kind: "passed",
      warning: `dispatch: warning: this Dispatch cannot check ${subject}: ${endedSentence(stored.failure)} ${uploadsUnchecked}`,
    };
  }
  // The deadline's end reads as what it is: the client's own words for it are a write's, saying
  // that the write may or may not have taken effect, and the round trip writes nothing.
  return unasked(
    deadline.aborted ? `Dispatch gave no answer within ${deadlineMs / 1000} s` : stored.failure,
    stored.kind === "unchecked"
      ? uploadsUnchecked
      : "The call refuses the upload until Dispatch answers."
  );
}
