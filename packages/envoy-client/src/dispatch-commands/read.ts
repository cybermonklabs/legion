import type { IssueReadOptions } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchDocumentRef,
  dispatchIssueRef,
  documentLabel,
  type ParsedDispatchRef,
} from "../dispatch-owner";
import {
  askTexts,
  datedBodies,
  eventTextsWhole,
  type PicturesRead,
  readPictures,
} from "../dispatch-picture-tools";
import { type DatedText, picturesNewestFirst } from "../dispatch-pictures";
import { readAskRef } from "./asks";
import {
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalNumber,
  optionalString,
  stringArg,
} from "./context";
import { resolveMessageArgument } from "./conversation";
import { approvalLine, documentListLines, readRefOwner } from "./document-targets";
import { dispatchAnswered, refuseMissingOfRef, throughPostCorrections } from "./failures";
import { messageIdOf, refOwnerName, refTarget, refuseItemOffRef, resolveIdPrefix } from "./refs";
import {
  askPartNoun,
  blockLines,
  graphReferences,
  graphSectionLines,
  graphSections,
  issueCoverageOrReason,
  issueReferencesOrUnavailable,
  typedBlockOf,
} from "./render-graph";
import {
  childrenSummary,
  holdsSession,
  issueSummary,
  liveAgents,
  logSummary,
  olderServerWarnings,
  routeHeldBySession,
} from "./render-issue";
import { askSummary, commentSummary, messageSummary, postHistorySummary } from "./render-item";

/**
 * `dispatch read`: whatever the call names — a message's conversation, an ask, a comment, a
 * message, one block, a project's documents, a document's summary, an issue's log, its children,
 * or the issue itself. Reads report their owner and follow nothing; no result subscribes the
 * session.
 */
export async function readCommand(context: CommandContext): Promise<DispatchToolResult> {
  const {
    args,
    client,
    input,
    linkBase,
    ownerArguments,
    shown,
    documentOwner,
    resolveDocument,
    issue,
    configUrl,
    hostSession,
  } = context;
  const message = optionalString(args, "message");
  // --history reads one post's replaced bodies in place of the post. The contract holds it
  // to a call that names a ref, and `argumentProblems` to a ref naming a comment or an
  // issue message, so only those two branches below read it.
  const history = optionalBoolean(args, "history") === true;
  // The pictures the shown texts embed, read beside the graph: a `Pictures:` section that
  // goes before `Referenced by:`, and the images it numbers.
  const picturesOf = (texts: readonly DatedText[]) =>
    readPictures(client, picturesNewestFirst(texts), shown);
  // The result's images. A tool result is the model's once it is returned, so they join
  // `shown` here, the last part of the result built, after every read that could fail it.
  const showImages = (pictures: PicturesRead) => {
    for (const address of pictures.shown) shown?.add(address);
    return pictures.images.length === 0 ? {} : { images: pictures.images };
  };
  if (message !== undefined) {
    const sessionId = input.sessionId?.trim();
    if (!sessionId) throw new Error("host session id is required for dispatch read --message");
    const thread = await client.getMessageThread(messageIdOf(message) as string, sessionId);
    const issueKey = thread.message.issue_key;
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([thread.message, ...(thread.replies ?? [])])),
      issueKey === null
        ? []
        : graphSections(
            client,
            dispatchChildRef(dispatchIssueRef(issueKey), "message", thread.message.id)
          ),
    ]);
    return {
      text: messageSummary(thread, [...pictures.lines, ...graph]),
      details: {
        message: thread.message.id,
        ...(issueKey === null ? {} : { issue: issueKey }),
      },
      ...showImages(pictures),
    };
  }
  // A project document's id, read once for an ask or comment its ref names: the prefix match,
  // the check that the item read is on that document, and the owner read of a failing item read
  // (refuseMissingOfRef) share it.
  let refDocumentId: Promise<string> | undefined;
  const documentIdOf = (ref: ParsedDispatchRef) => {
    refDocumentId ??= resolveDocument(ref.owner, ref.artifact).then(
      (resolved) => resolved.artifact.id
    );
    return refDocumentId;
  };
  /** The sentence a missing block's message ends with when the post that held the block is
   *  deleted: a delete clears every block the post held, and every read of the post says that it
   *  is deleted. The fact is the post's own `deleted_at`, read from the post
   *  itself; the words of the server's refusal are never read, because a reader of prose breaks
   *  on the day the prose changes (`skill://dispatch`: branch on an error's code, never on its
   *  text). The message read is the issue's, as the message read below makes it, because
   *  `GET /api/v1/messages/{id}` answers the root of that message's conversation rather than
   *  the message named (`readableMessageThread`, `api/messages.go`): a deleted reply's block
   *  would then say nothing, and a standing reply under a deleted root would say it falsely. A
   *  message block's owner is always an issue. Nothing for a block of a document or of an ask,
   *  neither of which a delete marks, for a post that stands, and where the post's own read
   *  fails, which leaves the message as it is.
   */
  const deletedPostClause = async (ref: ParsedDispatchRef): Promise<string> => {
    const item = ref.item;
    if (item === undefined || (item.kind !== "comment" && item.kind !== "message")) {
      return "";
    }
    const deleted = await (item.kind === "comment"
      ? client.getComment(item.id).then((read) => read.comment.deleted_at)
      : ref.owner.kind === "issue"
        ? client.getMessage(ref.owner.issue, item.id).then((read) => read.message.deleted_at)
        : Promise.resolve(undefined)
    ).catch(() => undefined);
    return deleted == null ? "" : ` This ${item.kind} is deleted.`;
  };
  if (ownerArguments.ref?.kind === "ask") {
    const ref = ownerArguments.ref;
    // The same undecidable `not found` the corrections and the two post reads met, one arm
    // over: a reference whose issue Dispatch does not hold reads the same as one whose ask it
    // does not. A reference naming a project document is read the same way, by the document.
    const askRead = await readAskRef(input.tool, ref.id, refOwnerName(ref), client, async () =>
      ref.owner.kind === "issue"
        ? client.listIssueAsks(ref.owner.issue)
        : client.getArtifactAsks(await documentIdOf(ref), "all")
    ).catch(async (error: unknown) => {
      throw await refuseMissingOfRef(
        error,
        "ask",
        ref.id,
        ref.owner,
        refOwnerName(ref),
        readRefOwner(client, ref.owner, () => documentIdOf(ref))
      );
    });
    await refuseItemOffRef(input.tool, "ask", ref, stringArg(args, "ref"), askRead.ask, () =>
      documentIdOf(ref)
    );
    const askRef = refTarget(ref, "ask", askRead.ask.id);
    // The ask's graph is read in both directions here rather than through `graphSections`:
    // the summary lists the edges at each of its parts under that part.
    const [pictures, incoming, outgoing] = await Promise.all([
      picturesOf(askTexts(askRead)),
      graphReferences(client, { to: askRef }),
      graphReferences(client, { from: askRef }),
    ]);
    return {
      text: askSummary(askRead, askRef, pictures.lines, incoming, outgoing),
      details:
        ref.owner.kind === "project" ? { project: ref.owner.project } : { issue: ref.owner.issue },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "comment") {
    const ref = ownerArguments.ref;
    const id = await resolveIdPrefix(input.tool, "comment", ref.id, refOwnerName(ref), async () =>
      ref.owner.kind === "issue"
        ? client.getComments(ref.owner.issue)
        : client.getArtifactComments(await documentIdOf(ref))
    );
    // The same undecidable `not found` the corrections met: a reader of this read must be able
    // to tell a post that is gone from an issue that was never there (dispatch://INFRA-1523).
    const comment = await client.getComment(id).catch(async (error: unknown) => {
      throw await refuseMissingOfRef(
        error,
        "comment",
        id,
        ref.owner,
        refOwnerName(ref),
        readRefOwner(client, ref.owner, () => documentIdOf(ref))
      );
    });
    await refuseItemOffRef(
      input.tool,
      "comment",
      ref,
      stringArg(args, "ref"),
      comment.comment,
      () => documentIdOf(ref)
    );
    if (history) {
      return {
        text: postHistorySummary(
          "Comment",
          refTarget(ref, "comment", id),
          id,
          await throughPostCorrections(input.tool, configUrl, client.commentHistory(id))
        ),
        details:
          ref.owner.kind === "project"
            ? { project: ref.owner.project, comment: id }
            : { issue: comment.comment.issue_key, comment: id },
      };
    }
    const commentRef = refTarget(ref, "comment", id);
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([comment.comment, ...(comment.replies ?? [])])),
      graphSections(client, commentRef),
    ]);
    return {
      text: commentSummary(comment, [...pictures.lines, ...graph]),
      details:
        ref.owner.kind === "project"
          ? { project: ref.owner.project }
          : { issue: comment.comment.issue_key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "message") {
    if (ownerArguments.ref.owner.kind !== "issue") {
      throw new Error("message references are issue-scoped");
    }
    const messageIssue = ownerArguments.ref.owner.issue;
    if (history) {
      // The history is read of the message the reference names, resolved as the edit and
      // the delete resolve their own `message` argument: a prefix resolves against the
      // issue's messages, and a message on another issue is refused rather than read.
      const id = await resolveMessageArgument(
        input.tool,
        "ref",
        stringArg(args, "ref"),
        client,
        hostSession
      );
      return {
        text: postHistorySummary(
          "Message",
          dispatchChildRef(dispatchIssueRef(messageIssue), "message", id),
          id,
          await throughPostCorrections(
            input.tool,
            configUrl,
            client.messageHistory(id, hostSession === "" ? undefined : hostSession)
          )
        ),
        details: { issue: messageIssue, message: id },
      };
    }
    const named = ownerArguments.ref;
    const messageRead = await client
      .getMessage(messageIssue, named.id)
      .catch(async (error: unknown) => {
        throw await refuseMissingOfRef(
          error,
          "message",
          named.id,
          { kind: "issue", issue: messageIssue },
          messageIssue,
          () => client.getIssue(messageIssue)
        );
      });
    const messageRef = dispatchChildRef(
      dispatchIssueRef(ownerArguments.ref.owner.issue),
      "message",
      ownerArguments.ref.id
    );
    const [pictures, graph] = await Promise.all([
      picturesOf(datedBodies([messageRead.message, ...(messageRead.replies ?? [])])),
      graphSections(client, messageRef),
    ]);
    return {
      text: messageSummary(messageRead, [...pictures.lines, ...graph]),
      details: { issue: messageRead.message.issue_key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "block") {
    const ref = ownerArguments.ref;
    const blockRef = stringArg(args, "ref");
    // The document, ask, comment or message the block is in: the reference before its `#b-`.
    const holder = blockRef.slice(0, blockRef.indexOf("#"));
    // A part of an ask is named by what it is — the question, an option or the answer — and
    // no node carries a part's type, so the ask's own read is read beside the graph. That
    // read only chooses the heading's word, so it is advisory: a failure of it leaves the
    // part under the general `Part` heading and the rest of the read unchanged.
    const askOfPart =
      ref.item?.kind === "ask" ? client.getAsk(ref.item.id).catch(() => undefined) : undefined;
    // The read names the block's node, so a 404 is the block's own answer: an unpinned ref
    // names a block no current version holds, and a pinned one's answer says which of its
    // version, that version's record or the block the server could not find. The unpinned
    // answer is rewritten in the reference's own words, and a delete, which clears every block
    // the post held, is the one cause of it that a reader needs told, since every read of the
    // post says that it is deleted. The post's own mark is what says so.
    const [incoming, outgoing, partOf, typed] = await Promise.all([
      client.getReferences({ to: blockRef }).catch(async (error: unknown) => {
        if (ref.pin === undefined && dispatchAnswered(error, 404)) {
          throw new Error(`No block ${ref.block} in ${holder}.${await deletedPostClause(ref)}`);
        }
        throw error;
      }),
      graphReferences(client, { from: blockRef }),
      askOfPart,
      // The live block of its document, for its type and the attributes it carries: a block
      // of a comment or a message has neither, and so has one no live version holds.
      typedBlockOf(client, ref),
    ]);
    const blockID = incoming.node.block_id ?? ref.block ?? "";
    // A part whose ask the read could not name reads as a `Part`, as a part the ask does not
    // place does.
    const noun =
      ref.item?.kind !== "ask"
        ? undefined
        : partOf === undefined
          ? "Part"
          : askPartNoun(partOf.ask, incoming.node, blockID);
    return {
      text: [
        ...blockLines(incoming.node, ref, typed, noun),
        ...graphSectionLines(incoming, outgoing),
      ].join("\n"),
      details: {
        ...(ref.owner.kind === "project"
          ? { project: ref.owner.project }
          : { issue: ref.owner.issue }),
        block: incoming.node.block_id ?? ref.block,
      },
    };
  }
  // A project named with no document reads as the list of its documents (those on no issue,
  // the ones a project document reference names), which a project document reference's
  // not-found hint points to.
  const projectOwner = documentOwner();
  if (projectOwner.kind === "project" && optionalString(args, "artifact") === undefined) {
    const documents = await client.listProjectArtifacts(projectOwner.project, true);
    return {
      text: [
        `Project: ${projectOwner.project}`,
        "Documents:",
        ...documentListLines(documents),
      ].join("\n"),
      details: { project: projectOwner.project },
    };
  }
  // A document's summary. An issue's document reads as a project's does, with the issue it
  // belongs to, so a document named by artifact, by ref, or by a search hit's link is the
  // thing read rather than its issue.
  const documentReference = optionalString(args, "artifact");
  if (documentOwner().kind === "project" || documentReference !== undefined) {
    const resolved = await resolveDocument(documentOwner(), documentReference);
    const { artifact } = resolved;
    const ownerKey = resolved.issue?.key ?? artifact.project;
    const documentRef = dispatchDocumentRef(ownerKey, artifact.slug);
    const approval = approvalLine(artifact, typeof resolved.issue?.closed_at === "string");
    return {
      text: [
        `Document: ${ownerKey} / ${artifact.name}`,
        ...(resolved.issue === undefined
          ? []
          : [`Issue: ${resolved.issue.key} [${resolved.issue.status}] ${resolved.issue.title}`]),
        `Reference: ${documentRef}`,
        `Versions: ${(artifact.versions ?? []).length}`,
        ...(approval === undefined ? [] : [approval]),
        ...(await graphSections(client, documentRef)),
      ].join("\n"),
      details: {
        ...(resolved.issue === undefined
          ? { project: artifact.project }
          : { issue: resolved.issue.key }),
        document: documentLabel(ownerKey, artifact.slug),
      },
    };
  }
  const issueKey = issue();
  // Which of the issue's events the issue read and its log list; `argumentProblems` refused a
  // count beside any other read.
  const readEvents: IssueReadOptions = {
    events: optionalBoolean(args, "all_events") === true ? "all" : optionalNumber(args, "events"),
  };
  if (ownerArguments.ref?.kind === "log") {
    const read = await client.read(issueKey, readEvents);
    const pictures = await picturesOf(await eventTextsWhole(client, read.events));
    return {
      text: [logSummary(read.issue, read.events), ...pictures.lines].join("\n"),
      details: { issue: read.issue.key },
      ...showImages(pictures),
    };
  }
  if (ownerArguments.ref?.kind === "children") {
    // The children list prints no events, so only the issue is read.
    const parent = await client.getIssue(issueKey);
    return {
      text: childrenSummary(parent),
      details: { issue: parent.key },
    };
  }
  const readPromise = client.read(issueKey, readEvents);
  // Start the independent, non-fatal closure lookup with the issue read. `read` retains
  // its internal issue-then-events order because event pagination starts at issue.last_seq.
  const referencesPromise = issueReferencesOrUnavailable(client, issueKey);
  const read = await readPromise;
  // The claim's holder must read the same here as on the issue page, so the label comes
  // from the live registry — asked for only when a session holds this issue.
  const [references, graph, agents, pictures, coverage] = await Promise.all([
    referencesPromise,
    graphSections(client, dispatchIssueRef(read.issue.key)),
    liveAgents(client, holdsSession(read.issue.claim) || routeHeldBySession(read.issue)),
    picturesOf([
      ...(read.issue.open_asks ?? []).map((ask) => ({ text: ask.question, at: ask.created_at })),
      ...(await eventTextsWhole(client, read.events)),
    ]),
    issueCoverageOrReason(client, read.issue.key),
  ]);
  return {
    text: issueSummary(
      read.issue,
      read.events,
      references,
      [...pictures.lines, ...graph],
      linkBase,
      coverage,
      agents
    ),
    details: { issue: read.issue.key },
    ...showImages(pictures),
    ...olderServerWarnings(read.issue),
  };
}
