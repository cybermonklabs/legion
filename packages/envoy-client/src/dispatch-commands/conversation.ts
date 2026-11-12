import type { Artifact, CreateCommentInput, MessageDeliveryMode } from "@legion/contracts";
import { commandLine } from "../dispatch-command";
import type { DispatchClient } from "../dispatch-http";
import {
  dispatchChildRef,
  dispatchIssueRef,
  issueTopic,
  parseDispatchRef,
} from "../dispatch-owner";
import { ToolInputError } from "../tool-input-errors";
import { renderAdvice } from "./advice";
import { movedApprovalLine, resolveReplyToAsk } from "./asks";
import { type CommandContext, type DispatchToolResult, optionalString, stringArg } from "./context";
import { anchor, resolveArtifact } from "./document-targets";
import { throughPostCorrections } from "./failures";
import {
  itemReplyTo,
  messageArgumentId,
  messageArgumentProblem,
  messageInReplyTo,
  refOwnerName,
  refuseItemOffRef,
  resolveIdPrefix,
} from "./refs";
import {
  commentOwnerResult,
  followsAsk,
  notSubscribed,
  resolvedTopic,
  writeResultDetails,
} from "./results";

/**
 * The comment a `comment` argument names: its full id, and the project document it is on where
 * the reference named one. A bare id is read as it is; a reference resolves a prefix against
 * its owner's comments and then holds the comment read to that owner, so a full id under the
 * wrong owner is refused rather than written to. `argumentProblems` refused any dispatch://
 * value that is not a comment reference.
 */
export async function resolveCommentArgument(
  tool: string,
  reference: string,
  client: DispatchClient
): Promise<{ readonly id: string; readonly document?: Artifact }> {
  const ref = reference.startsWith("dispatch://") ? parseDispatchRef(reference) : null;
  if (ref === null) return { id: reference };
  let id = reference;
  let document: Artifact | undefined;
  if (ref.owner.kind === "issue") {
    const issueKey = ref.owner.issue;
    id = await resolveIdPrefix(tool, "comment", ref.id, refOwnerName(ref), () =>
      client.getComments(issueKey)
    );
  } else {
    const artifact = (await resolveArtifact(client, ref.owner, ref.artifact, { canonical: true }))
      .artifact;
    document = artifact;
    id = await resolveIdPrefix(tool, "comment", ref.id, refOwnerName(ref), () =>
      client.getArtifactComments(artifact.id)
    );
  }
  const read = await client.getComment(id);
  await refuseItemOffRef(tool, "comment", ref, reference, read.comment, async () =>
    document === undefined ? "" : document.id
  );
  return { id, ...(document === undefined ? {} : { document }) };
}

/**
 * The message a `message` argument of an edit or a delete, or the `ref` of a `dispatch read`
 * history read, names: the full uuid it carries, or the id a reference's prefix matches among
 * its issue's messages, replies included. A bare id is the message itself; a reference holds the
 * message to the issue it names, as `resolveCommentArgument` holds a comment to its owner, so a
 * full uuid under the wrong issue is refused rather than written to. `argument` is the argument
 * the caller took the value from, which the refusal of a value naming no message names:
 * `argumentProblems` refuses a correction's `message` before the call, while a history read's
 * `ref` is checked there for its kind alone and reaches this refusal. `session` is the host
 * session the thread is read as, empty or undefined for a caller with none.
 */
export async function resolveMessageArgument(
  tool: string,
  argument: "message" | "ref",
  reference: string,
  client: DispatchClient,
  session: string | undefined
): Promise<string> {
  const named = messageArgumentId(reference);
  if (named === undefined) throw new ToolInputError(tool, [messageArgumentProblem(argument)]);
  const ref = named.ref;
  if (ref === undefined) return named.id;
  const issueKey = ref.owner.issue;
  const id = await resolveIdPrefix(tool, "message", named.id, issueKey, async () =>
    (await client.issueMessages(issueKey)).flatMap((thread) => [
      thread.message,
      ...(thread.replies ?? []),
    ])
  );
  // The thread the message belongs to, read by its id alone, names the issue it is really on:
  // the listing above resolves a prefix among this issue's messages, but a full uuid names a
  // message of any issue. A message is on an issue or on none, so no document is read for it.
  const read = await client.getMessageThread(id, session === "" ? undefined : session);
  await refuseItemOffRef(
    tool,
    "message",
    ref,
    reference,
    { id, issue_key: read.message.issue_key },
    async () => ""
  );
  return id;
}

/** The issue a corrected message sits on. Only a message on an issue is edited or deleted, a
 *  rule the server holds every such write to (409 NOT_AN_ISSUE_MESSAGE), so an answer carrying
 *  no issue is a server that does not keep it. */
function messageIssueKey(message: { readonly issue_key: string | null }): string {
  if (message.issue_key === null) throw new Error("the corrected message names no issue");
  return message.issue_key;
}

/** `dispatch resolve-comment`: close a comment thread on an issue or a document. */
export async function resolveCommentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const reference = stringArg(args, "comment");
  const { id, document } = await resolveCommentArgument(input.tool, reference, client);
  const comment = await client.resolveComment(id, actor);
  const { label, details } = await commentOwnerResult(client, comment, document);
  return { text: `Resolved comment ${comment.id} on ${label}.`, details };
}

/** `dispatch edit-comment`: the author replaces the body of their own comment. */
export async function editCommentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, configUrl } = context;
  const reference = stringArg(args, "comment");
  const { id, document } = await resolveCommentArgument(input.tool, reference, client);
  const comment = await throughPostCorrections(
    input.tool,
    configUrl,
    client.editComment(id, { body: stringArg(args, "body"), actor })
  );
  const { label, details } = await commentOwnerResult(client, comment, document);
  return {
    text: `Edited comment ${comment.id} on ${label}. Dispatch keeps the body it replaced.`,
    details,
  };
}

/** `dispatch delete-comment`: the author withdraws their own comment. */
export async function deleteCommentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, configUrl } = context;
  const reference = stringArg(args, "comment");
  const { id, document } = await resolveCommentArgument(input.tool, reference, client);
  const comment = await throughPostCorrections(
    input.tool,
    configUrl,
    client.deleteComment(id, actor)
  );
  const { label, details } = await commentOwnerResult(client, comment, document);
  return {
    text: `Deleted comment ${comment.id} on ${label}. No read gives its body again.`,
    details,
  };
}

/** `dispatch edit-message`: the author replaces the body of their own issue message. */
export async function editMessageCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, configUrl, hostSession } = context;
  const id = await resolveMessageArgument(
    input.tool,
    "message",
    stringArg(args, "message"),
    client,
    hostSession
  );
  const message = await throughPostCorrections(
    input.tool,
    configUrl,
    client.editMessage(id, { body: stringArg(args, "body"), actor })
  );
  const issue = messageIssueKey(message);
  return {
    text: `Edited message ${message.id} on ${issue}. Dispatch keeps the body it replaced.`,
    details: { issue, message: message.id },
  };
}

/** `dispatch delete-message`: the author withdraws their own issue message. */
export async function deleteMessageCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, configUrl, hostSession } = context;
  const id = await resolveMessageArgument(
    input.tool,
    "message",
    stringArg(args, "message"),
    client,
    hostSession
  );
  const message = await throughPostCorrections(
    input.tool,
    configUrl,
    client.deleteMessage(id, actor)
  );
  const issue = messageIssueKey(message);
  return {
    text: `Deleted message ${message.id} on ${issue}. No read gives its body again.`,
    details: { issue, message: message.id },
  };
}

/** `dispatch comment`: post a comment, a reply in a thread, or a reply under an ask. */
export async function commentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, resolveDocument, writePictures, issue } =
    context;
  const artifactReference = optionalString(args, "artifact");
  const owner = documentOwner();
  const resolved =
    owner.kind === "project" || artifactReference === undefined
      ? owner.kind === "project"
        ? await resolveDocument(owner, artifactReference)
        : undefined
      : await resolveDocument(owner, artifactReference);
  const anchored = resolved ? anchor(resolved.artifact, args) : undefined;
  const replyTo = itemReplyTo(args, "reply_to");
  const replyToAskTarget = itemReplyTo(args, "reply_to_ask");
  const replyToAsk =
    replyToAskTarget === undefined
      ? undefined
      : await resolveReplyToAsk(input.tool, client, owner, resolved, replyToAskTarget.id);
  // reply_to and reply_to_ask are mutually exclusive, so at most one names a block: a block
  // of the comment reply_to answers, or the part of the ask reply_to_ask threads under.
  const replyToBlock = replyTo?.block ?? replyToAskTarget?.block;
  // The schema already refused reply_to alongside reply_to_ask, turn without reply_to_ask,
  // and a turn outside agent|human, so the value is the contract's shape.
  const requestedTurn = optionalString(args, "turn") as CreateCommentInput["turn"];
  const body = await writePictures(resolved, stringArg(args, "body"));
  const commentInput: CreateCommentInput = {
    body,
    ...(anchored === undefined ? {} : { anchor: anchored }),
    ...(replyTo === undefined ? {} : { reply_to: replyTo.id }),
    ...(replyToBlock === undefined ? {} : { reply_to_block: replyToBlock }),
    ...(replyToAsk === undefined ? {} : { ask_id: replyToAsk }),
    ...(requestedTurn === undefined ? {} : { turn: requestedTurn }),
    actor,
  };
  const comment =
    resolved?.owner.kind === "project"
      ? await client.artifactComment(resolved.artifact.id, commentInput)
      : await client.comment(issue(), commentInput);
  const commentOwner = resolved === undefined ? issueTopic(issue()) : resolvedTopic(resolved);
  const commentDetails =
    resolved === undefined
      ? { issue: comment.issue_key, comment: comment.id }
      : writeResultDetails(resolved, { comment: comment.id });
  const adviceLines = renderAdvice(input.tool, commentOwner.label, comment.advice, {
    isAskReply: replyToAsk !== undefined,
    replyToOwnAsk:
      replyToAsk !== undefined &&
      (comment.advice?.your_open_asks?.some((ask) => ask.id === replyToAsk) ?? false),
  });
  if (replyToAsk !== undefined) {
    // The server answers whom the ask waits on now that this comment is its newest reply. It
    // differs from this comment's turn for an agent reply on a moved approval request.
    const askState =
      comment.ask_waiting_on === undefined ? "" : `; ask now waiting on ${comment.ask_waiting_on}`;
    // A reply meant for the human that leaves the ask with the agent landed on an approval
    // request a newer version moved: say why, and name the call that hands it back.
    const moved =
      requestedTurn !== "agent" && comment.ask_waiting_on === "agent"
        ? await movedApprovalLine(client, replyToAsk)
        : undefined;
    return {
      text: [
        `Replied on ask ${replyToAsk} (comment ${comment.id}${askState}). ${await followsAsk(commentOwner, input.subscriptions)}`,
        ...(moved === undefined ? [] : [moved]),
        ...adviceLines,
      ].join("\n"),
      details: {
        ...commentDetails,
        ask: replyToAsk,
        follows: { ask: replyToAsk },
        ...(comment.ask_waiting_on === undefined ? {} : { ask_waiting_on: comment.ask_waiting_on }),
        ...(comment.advice === undefined ? {} : { advice: comment.advice }),
      },
    };
  }
  return {
    text: [
      `Posted comment ${comment.id}${await notSubscribed(commentOwner, input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details: {
      ...commentDetails,
      ...(comment.advice === undefined ? {} : { advice: comment.advice }),
    },
  };
}

/** `dispatch suggest`: propose a replacement for a passage of a document. */
export async function suggestCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, documentOwner, resolveDocument, issue } = context;
  const resolved = await resolveDocument(documentOwner(), stringArg(args, "artifact"));
  const anchored = anchor(resolved.artifact, args);
  if (anchored === undefined) throw new Error("quote is required");
  const body = optionalString(args, "body");
  const suggestionInput = {
    ...(body === undefined ? {} : { body }),
    anchor: anchored,
    replace_with: stringArg(args, "replace_with"),
    actor,
  };
  const comment =
    resolved.owner.kind === "project"
      ? await client.artifactSuggest(resolved.artifact.id, suggestionInput)
      : await client.suggest(issue(), suggestionInput);
  return {
    text: `Posted suggestion ${comment.id}${await notSubscribed(resolvedTopic(resolved), input.subscriptions)}`,
    details: writeResultDetails(resolved, { comment: comment.id }),
  };
}

/** How a posted message points the agent at the conversation it was written into. The two
 *  writes that post one - a reply on an issue, a message to a session - say the same sentence,
 *  and say it of their own message. */
function conversationReadBack(messageId: string): string {
  return `${commandLine("dispatch_read", { message: messageId })} reads the conversation back.`;
}

/** `dispatch message`: post on the issue's conversation, or answer a direct message. */
export async function messageCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input, owner, pictures, postWithPictures, issue } = context;
  const target = messageInReplyTo(args);
  const inReplyTo = target?.id;
  const written = stringArg(args, "body");
  if (owner === null && inReplyTo !== undefined) {
    // A direct message's conversation belongs to no issue, so its pictures belong to this
    // session's own conversation, where the person reads the reply.
    let body = written;
    if (pictures.length > 0) {
      const sessionId = input.sessionId?.trim();
      if (!sessionId) {
        throw new Error("host session id is required to send pictures in a direct-message reply");
      }
      body = await postWithPictures(written, `agent/${sessionId}`, (upload) =>
        client.agentArtifact(sessionId, upload)
      );
    }
    // No owner beside an `in_reply_to`: `resolveOwnerArguments` left it that way because the
    // caller named only the message it answers, which is a human's direct message to this
    // session - a conversation with no issue to post into. `POST /messages/{id}/reply` is
    // the route for it, and it names the delivery attempt being answered. A direct message
    // is delivered as attempt 1 when it is created, and a later retry never retires that
    // row, so 1 is the attempt this session was handed. Dispatch takes the reply as proof
    // the message arrived whatever that attempt's receipt says.
    // A model calling dispatch_message means to post, so it asks to follow up: once the
    // attempt is answered, Dispatch posts new text as this session's follow-up, threaded
    // under its first reply. The host's automatic BTW answer never asks (delivery.ts), so a
    // frame handed to the session twice cannot post a second answer.
    const reply = await client.messageReply(
      inReplyTo,
      { body, attempt: 1, actor },
      { followUp: true }
    );
    // Text this session already posted in the conversation (its first reply or an earlier
    // follow-up) posts nothing, and Dispatch says so, so the session is not told it sent
    // something new.
    if (reply.duplicate === true && reply.body === body) {
      return {
        text:
          `Dispatch already has this exact text in the conversation (message ${reply.id}); ` +
          "nothing new was posted. Send different text if you have more to say.",
        details: { message: reply.id, in_reply_to: inReplyTo, posted: false, duplicate: true },
      };
    }
    // A Dispatch that predates follow-ups ignores `?follow_up=true` and keeps one reply per
    // attempt: it answers any second call with the stored reply - often the host's own
    // automatic BTW answer, sent before the model got here - and posts nothing. The stored
    // body is how that reads apart from a send, so this says what happened instead of
    // reporting a send that did not occur.
    if (reply.body !== body) {
      return {
        text:
          `Message ${inReplyTo} was already answered by message ${reply.id}; Dispatch kept ` +
          "that reply and posted nothing. Wait for their next message rather than answering " +
          "this one again.",
        details: { message: reply.id, in_reply_to: inReplyTo, posted: false },
      };
    }
    const readBack = conversationReadBack(inReplyTo);
    const parent = reply.in_reply_to ?? undefined;
    const follows = parent === inReplyTo ? undefined : parent;
    return {
      text:
        follows === undefined
          ? `Replied to message ${inReplyTo} with message ${reply.id}. ${readBack}`
          : `Replied to message ${inReplyTo} with message ${reply.id}, a follow-up threaded ` +
            `under your reply ${follows}. ${readBack}`,
      details: {
        message: reply.id,
        in_reply_to: inReplyTo,
        posted: true,
        ...(follows === undefined ? {} : { follows }),
      },
    };
  }
  const issueKey = issue();
  const body = await postWithPictures(written, issueKey, (upload) =>
    client.artifact(issueKey, upload)
  );
  const message = await client.message(issueKey, {
    body,
    ...(inReplyTo === undefined ? {} : { in_reply_to: inReplyTo }),
    ...(target?.block === undefined ? {} : { in_reply_to_block: target.block }),
    actor,
  });
  const messageRef = dispatchChildRef(dispatchIssueRef(issueKey), "message", message.id);
  const adviceLines = renderAdvice(input.tool, issueKey, message.advice, {});
  return {
    text: [
      `Posted message ${message.id} (${messageRef})${await notSubscribed(issueTopic(issueKey), input.subscriptions)}`,
      ...adviceLines,
    ].join("\n"),
    details: {
      issue: issueKey,
      message: message.id,
      ...(message.advice === undefined ? {} : { advice: message.advice }),
    },
  };
}

/** `dispatch send-to-agent`: one message into a session's own conversation, which stands on no
 *  issue, so the call names the session rather than an owner. */
export async function sendToAgentCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client } = context;
  const session = stringArg(args, "session");
  const inReplyTo = optionalString(args, "in_reply_to");
  // The route answers once the first attempt has settled, so its state is what says whether
  // the session has the words or whether Dispatch is still trying.
  const message = await client.agentMessage(session, {
    body: stringArg(args, "body"),
    delivery: stringArg(args, "delivery") as MessageDeliveryMode,
    ...(inReplyTo === undefined ? {} : { in_reply_to: inReplyTo }),
  });
  // Go marshals an empty delivery slice as `null`, so the route table's type is nullable;
  // either way there is no attempt to read, which the `undefined` branch below already says.
  const attempt = message.deliveries?.at(-1);
  const readBack = conversationReadBack(message.id);
  const arrival =
    attempt === undefined
      ? "Dispatch recorded no delivery attempt for it"
      : attempt.state === "failed"
        ? `its delivery failed: ${attempt.error ?? "the listener gave no reason"}`
        : attempt.state === "pending"
          ? "its delivery is still on its way"
          : `it reached that session as ${attempt.delivery}`;
  return {
    text: `Sent message ${message.id} to session ${session}; ${arrival}. ${readBack}`,
    details: {
      session,
      message: message.id,
      ...(attempt === undefined ? {} : { delivery: attempt.state }),
      ...(inReplyTo === undefined ? {} : { in_reply_to: inReplyTo }),
    },
  };
}
