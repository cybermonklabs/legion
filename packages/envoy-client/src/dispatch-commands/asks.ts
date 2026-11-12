import type {
  AskRead,
  AskUrgency,
  AskWithTrees,
  CreateAskInput,
  EditAskInput,
  OpenAsk,
} from "@legion/contracts";
import { ASK_URGENCIES, isIdPrefix } from "@legion/contracts";
import { textHead } from "../ask-answer";
import { type DispatchClient, DispatchServiceError } from "../dispatch-http";
import { issueTopic, type Owner, parseDispatchRef } from "../dispatch-owner";
import { declinedAskLines, relatedCheckLines } from "../related-check";
import { ToolInputError } from "../tool-input-errors";
import {
  handBackApprovalCommand,
  movedApprovalClauses,
  movedApprovalDocuments,
  otherAdviceLines,
  renderAdvice,
  standingReviewLines,
} from "./advice";
import { askQuestionWithRef } from "./check";
import {
  asObject,
  type CommandContext,
  type DispatchToolResult,
  optionalBoolean,
  optionalString,
  type ResolvedArtifact,
  type SessionContext,
  stringArg,
  type ToolArguments,
} from "./context";
import { anchor, resolveArtifact } from "./document-targets";
import { askId, normalizeUUID, refuseItemOffRef, resolveIdPrefix } from "./refs";
import { formatOpenAsksSummary } from "./render-item";
import { askOwnerDetails, documentTopic, followedAskDetails, followsAsk } from "./results";

function askUrgency(args: ToolArguments): AskUrgency | undefined {
  const value = args.urgency;
  return ASK_URGENCIES.find((urgency) => urgency === value);
}
const askHintLimit = 8;

/** The open asks a refused `reply_to_ask` lists, each by the 8 characters that name it — a
 *  prefix this flag resolves — and the head of its question, at most askHintLimit of them. */
function openAskChoices(
  asks: readonly Pick<AskWithTrees, "id" | "question">[],
  owner: string
): string {
  if (asks.length === 0) return `${owner} holds no open ask.`;
  const hints = asks
    .slice(0, askHintLimit)
    .map((ask) => `${ask.id.slice(0, 8)}… ${textHead(ask.question)}`);
  return `The open asks of ${owner} are: ${hints.join(", ")}.`;
}

/** The refusal of a `reply_to_ask` value that is neither a whole ask id nor a prefix of one. */
function askIDInputProblem(
  asks: readonly Pick<AskWithTrees, "id" | "question">[],
  owner: string
): string {
  return `ask ids are uuids. Write the whole id, or at least its first 8 characters. ${openAskChoices(asks, owner)}`;
}

/** The line a reply on an approval request a newer version moved prints: no reply hands such a
 *  request back (the server's approvalMoved), only `request-approval` at the current version does.
 *  Undefined when the ask is anything else or its read fails, so the reply reports as before. */
export async function movedApprovalLine(
  client: DispatchClient,
  askID: string
): Promise<string | undefined> {
  const ask = (await client.getAsk(askID).catch(() => undefined))?.ask;
  const approval = ask?.approval;
  if (ask?.kind !== "approval" || approval === undefined) return undefined;
  // Every document the request names is read, not its lead pair alone: a request over a spec and
  // its issue's Verification document moves when either of the two does, and a write of the
  // Verification document moves the second one while the lead pair stands still. A Dispatch older
  // than this client sends no list at all, and for it the lead pair is the whole request.
  const listed = approval.documents ?? [];
  const documents =
    listed.length > 0
      ? listed
      : [
          {
            name: approval.name,
            requested_version: approval.requested_version,
            version: approval.version,
          },
        ];
  const moved = movedApprovalDocuments(documents);
  if (moved.length === 0) return undefined;
  // The first document that moved carries the `--summary` floor. The read is guarded because the
  // strictest tsconfig of this repository, packages/claude-envoy's, compiles these sources under
  // `noUncheckedIndexedAccess`, which types an indexed read as possibly `undefined`. The
  // `undefined` arm cannot be reached: `moved` is `documents` filtered and is not empty here, so
  // it holds a first element. A first element missing all the same is one more reason to answer
  // `undefined`.
  const firstMoved = moved[0];
  if (firstMoved === undefined) return undefined;
  const command = handBackApprovalCommand({
    owner:
      ask.issue_key === null
        ? { kind: "project", key: ask.approval_artifact?.project ?? "<project>" }
        : { kind: "issue", key: ask.issue_key },
    artifactID: approval.artifact_id,
    primary: ask.approval_artifact?.primary === true,
    // An ask carries no role for the documents of its request, so the flag is read from the
    // request's own shape: the only request that names two documents is an issue's spec with
    // that issue's Verification document, because `requestArtifactApproval`
    // (packages/envoy/internal/dispatch/api/reviews.go) appends a second document only under
    // `include_verification`, which it refuses on any document that is not the issue's spec.
    verification: documents.length > 1 && ask.approval_artifact?.primary === true,
    summaryVersion: firstMoved.requested_version,
  });
  return (
    "The reply did not hand the ask to the human: it is an open approval request, and " +
    `${movedApprovalClauses(moved)}. No reply hands a moved approval request back. Once the ` +
    `human has agreed to every point in it, run: ${command}`
  );
}

/** The open asks a `reply_to_ask` may name, and the words a refusal calls their owner by: the
 *  issue's own open asks, or those of the project document the comment is written on. */
async function replyToAskCandidates(
  client: DispatchClient,
  owner: Owner,
  resolved: ResolvedArtifact | undefined
): Promise<{
  readonly asks: readonly Pick<AskWithTrees, "id" | "question">[];
  readonly owner: string;
}> {
  if (owner.kind === "issue") {
    const issue = resolved?.issue ?? (await client.getIssue(owner.issue));
    return { asks: issue.open_asks ?? [], owner: "this issue" };
  }
  if (resolved === undefined) throw new Error("project document is missing its resolved artifact");
  return {
    asks: await client.getArtifactAsks(resolved.artifact.id, "open"),
    owner: "this document",
  };
}

/**
 * The whole ask id `reply_to_ask` names: a whole uuid passes through, and a prefix of at least 8
 * hex characters is resolved against the owner's open asks by the resolver `dispatch read` uses
 * (resolveIdPrefix). A value that is neither, one that names no open ask, and one that names
 * more than one are each refused here, and each refusal lists those asks so the next call
 * carries an id that resolves. None of them prints the command's flags: the id is what the
 * reader must fix.
 */
export async function resolveReplyToAsk(
  tool: string,
  client: DispatchClient,
  target: Owner,
  resolved: ResolvedArtifact | undefined,
  id: string
): Promise<string> {
  const whole = normalizeUUID(id);
  if (whole !== undefined) return whole;
  const { asks, owner } = await replyToAskCandidates(client, target, resolved);
  if (!isIdPrefix(id)) {
    throw new ToolInputError(tool, [askIDInputProblem(asks, owner)], { aboutAFlag: false });
  }
  return resolveIdPrefix(tool, "ask", id, owner, async () => asks, {
    refusal: (found, all) =>
      found.length === 0
        ? `reply_to_ask ${id} names no open ask of ${owner}. ${openAskChoices(all, owner)}`
        : `reply_to_ask ${id} names ${found.length} open asks of ${owner}. Write more of the id. ${openAskChoices(all, owner)}`,
  });
}
const askIdShapeProblem = "ask ids are uuids (a prefix of at least 8 hex characters works)";

/**
 * The ask `dispatch read` of `dispatch://…/ask/<id>` names, read. An ask in a document has two
 * ids: its own uuid and the id of its `:::ask{#<id>}` block, which can look like a uuid too.
 * Either names it, whole or as an 8+ hex prefix exactly one of `list()`, the owner's asks, carries.
 * A full uuid is read as an ask id first; the list is read only when no ask has that id.
 */
export async function readAskRef(
  tool: string,
  id: string,
  ownerName: string,
  client: DispatchClient,
  list: () => Promise<readonly Pick<AskWithTrees, "id" | "block_id">[]>
): Promise<AskRead> {
  const fullID = normalizeUUID(id);
  if (fullID !== undefined) {
    try {
      return await client.getAsk(fullID);
    } catch (error) {
      if (!(error instanceof DispatchServiceError) || error.status !== 404) throw error;
      const blocks = (await list()).filter((ask) => ask.block_id?.toLowerCase() === fullID);
      if (blocks.length !== 1 || blocks[0] === undefined) throw error;
      return client.getAsk(blocks[0].id);
    }
  }
  const asks = await list();
  let matches = asks.filter((ask) => ask.block_id === id);
  if (matches.length === 0) {
    if (!isIdPrefix(id)) {
      throw new ToolInputError(tool, [
        `ask id ${id} must be a full uuid, a prefix of at least 8 hex characters, or the id of an :::ask block on ${ownerName}`,
      ]);
    }
    const prefix = id.toLowerCase();
    matches = asks.filter(
      (ask) =>
        ask.id.toLowerCase().startsWith(prefix) ||
        (ask.block_id?.toLowerCase().startsWith(prefix) ?? false)
    );
  }
  if (matches.length === 1 && matches[0] !== undefined) return client.getAsk(matches[0].id);
  throw new ToolInputError(tool, [
    matches.length === 0
      ? `ask id ${id} matches none of the asks on ${ownerName}; use the full id`
      : `ask id ${id} matches ${matches.length} asks on ${ownerName}; use the full id`,
  ]);
}

/** The open asks this session authored: the read `dispatch_open_asks` reports. */
async function sessionOpenAsks(
  client: DispatchClient,
  sessionId: string | undefined
): Promise<readonly Pick<OpenAsk, "id" | "question">[]> {
  const session = sessionId?.trim();
  if (!session) throw new Error("host session id is required to read this session's open asks");
  return (await client.openAsks(session)).asks ?? [];
}

/** The asks a refused ask id can be corrected to, in the server's own hint shape. */
function openAskHints(asks: readonly Pick<OpenAsk, "id" | "question">[]): string {
  if (asks.length === 0) return "you have no open asks";
  const hints = asks
    .slice(0, askHintLimit)
    .map((ask) => `${ask.id.slice(0, 8)} — ${textHead(ask.question)}`);
  return `your open asks: ${hints.join("; ")}`;
}

/**
 * The ask uuid `dispatch_edit_ask`, `dispatch_resolve_ask`, and `dispatch_follow` write to.
 * A full uuid passes through; an 8+ hex prefix resolves against this session's own open asks;
 * anything else is refused before the write, naming those asks so the next call carries a real
 * id. An open-asks read that fails never masks that rule. An ask named by a dispatch:// reference
 * is read first and refused unless it is on the issue or document the reference names.
 */
async function resolveAskArgument(
  tool: string,
  args: ToolArguments,
  client: DispatchClient,
  sessionId: string | undefined
): Promise<{ readonly id: string; readonly read?: AskRead }> {
  const id = askId(args);
  if (normalizeUUID(id) === undefined && !isIdPrefix(id)) {
    const asks = await sessionOpenAsks(client, sessionId).catch(() => undefined);
    throw new ToolInputError(tool, [
      asks === undefined ? askIdShapeProblem : `${askIdShapeProblem}; ${openAskHints(asks)}`,
    ]);
  }
  const resolved = await resolveIdPrefix(tool, "ask", id, "this session", () =>
    sessionOpenAsks(client, sessionId)
  );
  const reference = stringArg(args, "ask");
  // `argumentProblems` refused a dispatch:// value that is not an ask reference.
  const ref = reference.startsWith("dispatch://") ? parseDispatchRef(reference) : null;
  if (ref === null) return { id: resolved };
  const read = await client.getAsk(resolved);
  await refuseItemOffRef(tool, "ask", ref, reference, read.ask, async () => {
    const document = await resolveArtifact(client, ref.owner, ref.artifact, { canonical: true });
    return document.artifact.id;
  });
  return { id: resolved, read };
}

/** `dispatch open-asks`: the open asks this session, or one project, is waiting on. */
export async function openAsksCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { input, linkBase, ownerArguments, newClient: dispatchClient } = context;
  const client = dispatchClient();
  const project = optionalString(ownerArguments.args, "project");
  if (project !== undefined) {
    const response = await client.openAsksForProject(project);
    return { text: formatOpenAsksSummary(response, linkBase), details: { ...response } };
  }
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch open-asks");
  const response = await client.openAsks(sessionId);
  return { text: formatOpenAsksSummary(response, linkBase), details: { ...response } };
}

/** `dispatch ask`: open an ask on the issue, or on a passage of a document. */
export async function askCommand(context: CommandContext): Promise<DispatchToolResult> {
  const {
    args,
    client,
    actor,
    input,
    linkBase,
    documentOwner,
    resolveDocument,
    writePictures,
    issue,
  } = context;
  const anchorArgs = asObject(args.anchor);
  const owner = documentOwner();
  const artifactReference =
    optionalString(args, "artifact") ??
    (anchorArgs === null ? undefined : optionalString(anchorArgs, "artifact"));
  const resolved =
    owner.kind === "project" || artifactReference === undefined
      ? owner.kind === "project"
        ? await resolveDocument(owner, artifactReference)
        : undefined
      : await resolveDocument(owner, artifactReference);
  const options = args.options;
  const multiple = optionalBoolean(args, "multiple");
  const urgency = askUrgency(args);
  const anchored = anchorArgs && resolved ? anchor(resolved.artifact, anchorArgs) : undefined;
  const question = await writePictures(resolved, askQuestionWithRef(args));
  const askInput = {
    question,
    ...(Array.isArray(options)
      ? { options: options as NonNullable<CreateAskInput["options"]> }
      : {}),
    ...(multiple === undefined ? {} : { multiple }),
    ...(urgency === undefined ? {} : { urgency }),
    ...(anchored === undefined ? {} : { anchor: anchored }),
    actor,
  };
  const ask =
    resolved?.owner.kind === "project"
      ? await client.artifactAsk(resolved.artifact.id, askInput)
      : await client.ask(issue(), askInput);
  const askOwner =
    ask.issue_key !== null
      ? issueTopic(ask.issue_key)
      : resolved === undefined
        ? issueTopic(issue())
        : documentTopic(resolved.artifact);
  // The check's form sits right under the line naming the ask, the standing review under that,
  // and the issue's declined asks under that; renderAdvice's cadence line must stay the result's
  // last line.
  const checkLines = relatedCheckLines(ask.advice?.suggestions, {
    project: resolved?.artifact.project ?? (ask.issue_key ?? "").replace(/-[0-9]+$/, ""),
    text: stringArg(args, "question"),
    configUrl: linkBase,
  });
  const reviewLines = await otherAdviceLines(() => standingReviewLines(ask.advice));
  // An ask on a project document stands on no issue, and the server sends declined asks only
  // for a write that does, so there is nothing to print there.
  const declinedLines =
    ask.issue_key === null ? [] : declinedAskLines(ask.advice, ask.issue_key, linkBase);
  const adviceLines = await otherAdviceLines(() =>
    renderAdvice(input.tool, askOwner.label, ask.advice, {})
  );
  return {
    text: [
      `Asked ${ask.id} on ${askOwner.label} (urgency ${ask.urgency}): ${ask.question}`,
      ...checkLines,
      ...reviewLines,
      ...declinedLines,
      await followsAsk(askOwner, input.subscriptions),
      ...adviceLines,
    ].join("\n"),
    details: {
      ...(await followedAskDetails(client, ask, resolved?.artifact)),
      ...(ask.advice === undefined ? {} : { advice: ask.advice }),
    },
  };
}

/** `dispatch edit-ask`: reword an open ask, or change its options, urgency or multiple. */
export async function editAskCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const question = optionalString(args, "question");
  const options = args.options;
  const multiple = optionalBoolean(args, "multiple");
  const urgency = askUrgency(args);
  const { id } = await resolveAskArgument(input.tool, args, client, input.sessionId);
  const ask = await client.editAsk(id, {
    ...(question === undefined ? {} : { question }),
    ...(Array.isArray(options) ? { options: options as NonNullable<EditAskInput["options"]> } : {}),
    ...(multiple === undefined ? {} : { multiple }),
    ...(urgency === undefined ? {} : { urgency }),
    actor,
  });
  return {
    text: `Ask edited: ${ask.question}`,
    details: await askOwnerDetails(client, ask),
  };
}

/** `dispatch resolve-ask`: close an ask nobody needs to answer, with the reason. */
export async function resolveAskCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const kind = stringArg(args, "kind") as "retracted" | "resolved";
  const { id } = await resolveAskArgument(input.tool, args, client, input.sessionId);
  const ask = await client.resolveAsk(id, {
    kind,
    reason: stringArg(args, "reason"),
    actor,
  });
  if (ask.resolution === undefined) throw new Error("resolved ask is missing its resolution");
  return {
    text: `${kind === "retracted" ? "Retracted" : "Resolved"} ask ${ask.id}: ${ask.resolution.reason}`,
    details: await askOwnerDetails(client, ask),
  };
}

/** `dispatch follow`: have an ask's answer and replies reach this session, or stop them. */
export async function followCommand(context: CommandContext): Promise<DispatchToolResult> {
  const { args, client, actor, input } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch follow");
  const { id: ask, read: refRead } = await resolveAskArgument(input.tool, args, client, sessionId);
  const action = stringArg(args, "action");
  if (action === "unfollow") {
    await client.unfollowAsk(ask, sessionId, actor);
    return { text: `Unfollowed ask ${ask}.`, details: { ask } };
  }
  const read = refRead ?? (await client.getAsk(ask));
  await client.followAsk(ask, sessionId, actor);
  return {
    text: `Following ask ${ask}: its answer and replies reach this session directly.`,
    details: await followedAskDetails(client, read.ask),
  };
}
