import { resolve as resolvePath } from "node:path";
import type {
  Artifact,
  ArtifactBlock,
  EditOp,
  EditPrecondition,
  UploadPrecondition,
} from "@legion/contracts";
import { goBlankPattern } from "@legion/contracts/go-blank";
import type { DispatchClient } from "../dispatch-http";
import { dispatchDocumentRef, documentLabel, issueTopic } from "../dispatch-owner";
import { shownPictureLine } from "../dispatch-picture-tools";
import {
  oversizePictureText,
  PICTURE_SHOWN_MAX_BYTES,
  sniffPictureType,
  toolImage,
} from "../dispatch-pictures";
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
  stringArg,
} from "./context";
import { refuseOpenDecisionBlocks, refuseRemovingOpenDecisionBlocks } from "./decision-blocks";
import { approvalLine, documentPlaysPlan, liveBlocks } from "./document-targets";
import { openAnchoredThreads, waitsOnPerson } from "./render-item";
import {
  documentResultDetails,
  documentTopic,
  followedAskDetails,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";
import { answeredAskLines, liveTextAsks } from "./text-asks";
import {
  artifactNameExistsProblem,
  artifactUnderName,
  roundTripRouteAbsent,
  storedTextCheckFailed,
  storedTextProblem,
  withoutFinalLineFeed,
} from "./upload-checks";

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
  const approval = approvalLine(resolved.artifact, typeof resolved.issue?.closed_at === "string");
  // A live read's markdown holds no ask's state or answer: each answered ask is named beside the
  // token, from the text's asks (liveTextAsks). A version read prints the stored bytes, which are
  // the record a person approved, and names none.
  const answered = version === undefined ? answeredAskLines(liveTextAsks(document)) : [];
  const trailer = [
    ...("token" in document && document.token !== undefined
      ? [`Document token: ${document.token}`]
      : []),
    ...answered,
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
        `${resolved.artifact.name} (document id ${resolved.artifact.id}) is already approved at version ${result.version} by ${result.approval.by?.id ?? "unknown"}; no new request was opened. A change outside the ask blocks after approval makes it stale, so request again only for such a version, once the human has agreed to every point in it, with a --summary naming what changed since version ${result.version}. A human's answer to an ask block, or its resolve or decline, writes one version that changes only the block's state, and who answered and when, and the approval carries across it.`,
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
      `${outcome}${waitsOn === undefined ? "" : ` It waits on ${waitsOn}.`} The human's Inbox asks: ${JSON.stringify(result.ask.question)}. The answer arrives as artifact.approved or artifact.changes_requested. An edit before the answer moves this request to the new version and leaves it waiting on you, and a change outside the ask blocks after approval makes the approval stale: either way, request again for the new version once the human has agreed to every point in it, which hands this request back or opens a new one, with a --summary naming what changed since the earlier approval. A human's answer to an ask block, or its resolve or decline, writes one version that changes only the block's state, and who answered and when: an approval carries across it, and this request stays with whoever holds its turn.`,
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
  const { args, client, actor, input, documentOwner, issue } = context;
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
    isPlan: documentPlaysPlan(result.artifact),
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
