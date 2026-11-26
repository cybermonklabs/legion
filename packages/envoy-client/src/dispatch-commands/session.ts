import type {
  InboxRow,
  MyReplyRow,
  SessionAnswersResponse,
  WhoamiResponse,
} from "@legion/contracts";
import { awaitsDecision, inboxThreadRow, serviceSubjectLabel } from "@legion/contracts";
import { actorText, deletedMark, editedMark } from "../actor-text";
import { textHead } from "../ask-answer";
import { type CredentialPendingRow, DispatchServiceError } from "../dispatch-http";
import { dispatchChildRef, dispatchDocumentRef, dispatchIssueRef } from "../dispatch-owner";
import { messageFor } from "../errors";
import { isNoAnswersRoute, renderSessionAnswersList } from "../session-answers";
import type { DispatchToolResult, SessionContext } from "./context";
import { dispatchAnswered, refusalWithCode } from "./failures";

/**
 * `dispatch answers`: the answers this session's asks have been given since it last read them.
 * It names no owner, so it runs on the session's context alone.
 */
export async function answersCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { configUrl, input, newClient: dispatchClient } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch answers");
  let response: SessionAnswersResponse;
  try {
    response = await dispatchClient().sessionAnswers(sessionId);
  } catch (error) {
    if (isNoAnswersRoute(error)) {
      throw new Error(
        `This Dispatch (${configUrl}) has no answers route yet: its build is older than the answers route.`
      );
    }
    throw error;
  }
  return {
    text: renderSessionAnswersList(response.items ?? [], { now: Date.now() }),
    details: { session: sessionId, since: response.since, total: response.total },
  };
}

/** The person a credential acts for, as `GET /api/v1/whoami` names them: the owner of a personal
 *  or broker-issued token, and a signed-in person's own login in the canonical lowercase every
 *  stored login and assignee carries. `null` for a credential that acts for nobody, which the
 *  shared token and a service-account token are. */
function whoamiPerson(identity: WhoamiResponse): string | null {
  return identity.kind === "agent" ? identity.owner : identity.login.toLowerCase();
}

/**
 * `dispatch whoami`: the identity Dispatch records this session's writes as. It names no owner,
 * so it runs on the session's context alone. A read-only credential, which Dispatch names by
 * `read` among its scopes, is told it may only read, with its cut where it has one, in place of
 * the sentences about the issues it would create.
 */
export async function whoamiCommand(context: SessionContext): Promise<DispatchToolResult> {
  const { input, newClient: dispatchClient } = context;
  const sessionId = input.sessionId?.trim();
  if (!sessionId) throw new Error("host session id is required for dispatch whoami");
  const client = dispatchClient();
  const identity = await client.whoami();
  const owner = whoamiPerson(identity);
  // A Dispatch that predates verified service tokens omits the field entirely.
  const service = identity.kind === "agent" ? (identity.service ?? null) : null;
  // So does one that predates broker-issued tokens, which are the only credential that proves
  // a session of its own: read either omission as that older build, not as "no session".
  const proved = identity.kind === "agent" ? (identity.session ?? null) : null;
  const project = identity.kind === "agent" ? (identity.project ?? null) : null;
  // A Dispatch that predates read-only tokens names no `read` and no cut, and has no read-only
  // credential to name.
  const readOnly = identity.scopes?.includes("read") === true;
  const cut = identity.kind === "agent" ? (identity.cut ?? null) : null;
  // The projects the credential lists, which only the agent shape carries (a browser's cookie
  // lists none). A full member's list decides only which private projects it reads, so it uses
  // every public project too; a restricted member's decides every project. A Dispatch that
  // predates the list leaves it out, as it does for a credential that lists none and for a broker
  // token while no project is private; project is then the one it names, if any.
  const projects =
    identity.kind === "agent" && identity.projects?.length ? identity.projects : null;
  const restricted = identity.kind === "agent" && identity.member_kind === "restricted";
  // The session Dispatch records the writes as: the one the credential proved, else this host's.
  const session = proved ?? sessionId;
  const unowned =
    "no owner, so an issue you create without an assignee takes its parent's assignee when the parent is in the same project, else the project's default assignee, which every project names, so the issue always has one.";
  // Whether Dispatch refuses every write this credential makes until a person signs in on its
  // machine (SIGN_IN_REQUIRED). Dispatch answers it as sign_in_required, from the same decision
  // its write check makes, so an exchange entry's token that names its account and no person,
  // whose writes Dispatch records under that account, is not told it may not write. whoami is
  // where a person asks what their credential may do, so for a caller that is refused it says so
  // rather than describing the issue creation the server would refuse, and names the command that
  // gets them a write. A Dispatch that predates the field omits it, and then the rule that
  // Dispatch kept is read from the shape: a broker token is the one credential that proves a
  // session of its own, so a caller that names a service and proves a session is the machine
  // nobody signed in on, which that Dispatch refuses every write. A verified service-account
  // token names a service and proves no session; it writes as before, and keeps the sentence it
  // had. A read-only credential is told that it reads only before this is read: Dispatch answers
  // it false, and no sign-in would let it write.
  const answered = identity.kind === "agent" ? identity.sign_in_required : undefined;
  const signInNeeded = answered ?? (owner === null && service !== null && proved !== null);
  // Who the session is, as each sentence names it: the person it acts for, else the service it
  // runs as. The text names the identity the way every other surface does; details keeps the
  // subject whole, because that is the persisted value.
  const who =
    owner !== null
      ? `acts for ${owner}`
      : service !== null
        ? `runs as service ${serviceSubjectLabel(service)}`
        : "runs";
  // The session the credential proved, where it is not this host's.
  const named =
    proved !== null && proved !== sessionId
      ? ` Your credential names session ${proved}, which is not this host session (${sessionId})`
      : "";
  let acting: string;
  let elsewhere =
    named === "" ? "" : `${named}: Dispatch records every write you make as ${proved}.`;
  if (readOnly) {
    // Dispatch refuses every write of a read-only credential with READ_ONLY_TOKEN, before any
    // other check, so neither the issues it would create nor a sign-in it would need are its
    // sentence, and it records no write as any session.
    acting = `Session ${session} ${who} with a read-only credential: it may read, and Dispatch refuses every write it makes.${cut === null ? "" : ` Its cut is ${cut}, the past time it reads Dispatch at.`}`;
    elsewhere = named === "" ? "" : `${named}.`;
  } else if (owner !== null) {
    acting = `Session ${session} ${who}: issues you create without an assignee are assigned to ${owner}.`;
  } else if (signInNeeded) {
    acting = `Session ${session} ${who} and no person, so it may read but Dispatch refuses every write it makes: run \`agent-secrets login\` on this machine to write as yourself.`;
  } else if (service !== null) {
    acting = `Session ${session} ${who}: ${unowned}`;
  } else {
    acting = `Session ${session} runs under the shared token: ${unowned}`;
  }
  const projectsNamed =
    projects === null
      ? ""
      : `${projects.length === 1 ? "project" : "projects"} ${projects.join(", ")}`;
  const writable =
    projects !== null
      ? restricted
        ? ` It may use ${projectsNamed} and no other.`
        : ` It may use ${projectsNamed}, every public project, and no other private project.`
      : project !== null && !readOnly
        ? ` It may write project ${project} and no other.`
        : "";
  return {
    text: acting + elsewhere + writable,
    details: { session, owner, service, project, ...(projects !== null ? { projects } : {}) },
  };
}

/**
 * `dispatch limits`: the bounds the server holds every write to, read from the server itself
 * (`GET /api/v1/limits`) rather than from a constant this client carries, so a client older or
 * newer than its Dispatch prints the numbers that Dispatch actually enforces. It names no owner,
 * so it runs on the session's context alone.
 */
export async function limitsCommand(context: SessionContext): Promise<DispatchToolResult> {
  const limits = await context.newClient().limits();
  const count = (value: number) => value.toLocaleString("en-US");
  return {
    text: [
      `One block of a body - a paragraph, list, quote, table or code block: ${count(limits.block_units)} characters (UTF-16 units).`,
      `Blocks in one body: ${count(limits.item_blocks)}.`,
      `An ask's question and its options together: ${count(limits.ask_units)} characters (UTF-16 units).`,
      `Options on one ask: ${count(limits.ask_options)}.`,
      `The body an event carries whole: ${count(limits.event_body_units)} characters (UTF-16 units); a longer body reaches a reader cut, and the item itself holds the rest.`,
      `One request: ${count(limits.request_bytes)} bytes.`,
      `One markdown document: ${count(limits.document_bytes)} bytes.`,
      `One handoff body: ${count(limits.handoff_bytes)} bytes.`,
      "Dispatch holds every write to these bounds and refuses a longer text, naming the block or the field that passed one.",
    ].join("\n"),
    details: { ...limits },
  };
}

/** How much of an ask's question the Inbox summary's line carries. The event summaries' head
 *  (`textHead`) is longer: a question is one line among four counted bands here, and the whole of
 *  it is one `dispatch read` away. */
const INBOX_QUESTION_HEAD = 80;

/**
 * Whether the person has deferred this row and its moment is still ahead: the Inbox page's own
 * `isSnoozed` (`packages/dispatch/web/src/features/inbox/snooze.ts`), re-read here and held to
 * the page by Dispatch's browser test (`packages/dispatch/e2e/inbox-cli.e2e.ts`), which compares
 * this command's bands with the page's. The window is absolute, so nothing an agent does to the
 * ask shortens it; a row Dispatch lists with no snooze at all is not deferred, and neither is one
 * whose moment has passed.
 */
function inboxSnoozed(row: InboxRow, now: number): boolean {
  if (row.snoozed_until === null) return false;
  const at = Date.parse(row.snoozed_until);
  return !Number.isNaN(at) && at > now;
}

/**
 * Which band a row sits in: the page's `sectionOf` (`.../features/inbox/sections.ts`). A snooze
 * the person set wins over everything, because deferring an ask is them saying it does not need
 * them now; every other row sits under whose turn it is. A row the Inbox lists without a turn
 * belongs to no band, as on the page, and so is listed under no heading.
 */
function inboxSection(row: InboxRow, now: number): "agent" | "human" | "later" | undefined {
  if (inboxSnoozed(row, now)) return "later";
  const waiting = row.waiting_on;
  return waiting === "agent" || waiting === "human" ? waiting : undefined;
}

/** Where an inbox row's ask lives: the issue key or the document slug its line is named by, and
 *  the ask's own `dispatch://` address under that owner. Every row the Inbox lists has one owner
 *  or the other; a row with neither, which no read makes, is named by the ask's id alone. */
function inboxAskOwner(row: InboxRow): { readonly label: string; readonly ref: string } {
  const key = row.issue?.key ?? row.issue_key;
  if (key !== null) {
    return { label: key, ref: dispatchChildRef(dispatchIssueRef(key), "ask", row.id) };
  }
  const document = row.document;
  if (document !== undefined) {
    return {
      label: document.slug,
      ref: dispatchChildRef(dispatchDocumentRef(document.project, document.slug), "ask", row.id),
    };
  }
  return { label: row.id, ref: row.id };
}

function inboxAskLine(row: InboxRow): string {
  const { label, ref } = inboxAskOwner(row);
  const priority = row.priority === null ? "P-" : `P${row.priority}`;
  return `- ${label} · ${priority} · ${row.kind} · ${textHead(row.question, INBOX_QUESTION_HEAD)} · ${ref}`;
}

/** What an unread thread's line is named by: the issue key of a thread on an issue, the slug of
 *  one on a project document, and the session's name for an Agent row, which sits on neither.
 *  A Reply row with no owner at all is named by its thread id, the one name every row carries. */
function inboxThreadLabel(row: MyReplyRow): string {
  const thread = inboxThreadRow(row);
  if (thread.kind === "agent") return thread.session.name;
  return thread.issue?.key ?? thread.document?.slug ?? thread.thread_id;
}

/** How long ago the newest reply in a thread was written, in one short field: the largest whole
 *  unit that fits, since a list line wants an age rather than a timestamp. Anything under a
 *  minute reads `<1m`. */
function inboxReplyAge(at: string, now: number): string {
  const minutes = Math.floor(Math.max(0, now - Date.parse(at)) / 60_000);
  if (minutes < 1) return "<1m";
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

/** One unread thread: who owns it, what kind of thread it is, the head of its subject, who wrote
 *  the newest reply and how long ago, and where to read it. A thread whose root its author
 *  deleted has no subject left, and the deleted line every read prints for one (`deletedMark`)
 *  stands in its place. A thread whose root its author edited carries the mark every line prints
 *  for one (`editedMark`) after its subject, which is the head of the root's words as they stand. */
function inboxThreadLine(row: MyReplyRow, now: number): string {
  return [
    `- ${inboxThreadLabel(row)}`,
    row.kind,
    ...(row.deleted_at == null
      ? [
          textHead(row.subject, INBOX_QUESTION_HEAD),
          ...(row.edited_at == null ? [] : [editedMark(row.edited_at)]),
        ]
      : [deletedMark(row.deleted_at, row.deleted_by)]),
    actorText(row.reply.author),
    inboxReplyAge(row.reply.created_at, now),
    row.ref,
  ].join(" · ");
}

/** One credential request: what it asks for, when, whether its deadline passed before anybody
 *  decided it, the sessions it names and the page where it is decided. An expired row, which no
 *  longer `awaitsDecision` (the rule the Inbox page reads too), is marked `expired` and its
 *  deadline as the broker wrote it, the same mark the Inbox page gives it, or `expired` alone
 *  from a broker that names no deadline; a pending row carries no mark, and so does a row with no
 *  `state`, which comes from a broker older than the mark. An expired row names no page either:
 *  the broker refuses every decision of it, so, as the Inbox page links that row nowhere, the
 *  line prints no `approve_url` the broker still sends. A row that names no session leaves that
 *  field out rather than printing two dashes, and a machine login, which is decided with
 *  `dispatch` rather than on a broker page, carries no url to print. */
function credentialRequestLine(request: CredentialPendingRow): string {
  const session = request.session ?? null;
  const pending = awaitsDecision(request);
  return [
    `- ${request.kind}`,
    (request.identifiers ?? []).join(", "),
    request.requested_at,
    ...(pending
      ? []
      : [request.expires_at === undefined ? "expired" : `expired ${request.expires_at}`]),
    ...(session === null
      ? []
      : [`session request=${session.request ?? "-"}, enrollment=${session.enrollment ?? "-"}`]),
    ...(!pending || request.approve_url === undefined ? [] : [request.approve_url]),
  ].join(" · ");
}

/** Why one of the Inbox's two side reads failed, as its warning line names it: the code and
 *  message Dispatch refused with, or whatever else went wrong. */
function inboxReadRefusal(error: unknown): string {
  return error instanceof DispatchServiceError
    ? `${error.code} ${error.message}`
    : messageFor(error);
}

/** What a side read settles into: the answer it gave, or the failure it ended in. */
type SettledSideRead<T> = { answer: T } | { error: unknown };

/** A read of the Inbox the command can do without, settled into its answer or its failure: the
 *  two reads it cannot do without throw first, and a rejection caught only after that would be
 *  an unhandled one. */
function settleSideRead<T>(read: Promise<T>): Promise<SettledSideRead<T>> {
  return read.then(
    (answer) => ({ answer }),
    (error: unknown) => ({ error })
  );
}

/** The answer of one such read, or nothing and a warning naming the band the caller loses: the
 *  asks are the greater part of the Inbox, so one failed read does not cost the caller the rest
 *  of it. A refusal for want of a scope is the exception: the credential itself is wrong, and
 *  reporting that as a missing section would hide it. */
function sideRead<T>(settled: SettledSideRead<T>, what: string, warnings: string[]): T | undefined {
  if (!("error" in settled)) return settled.answer;
  const error = settled.error;
  if (dispatchAnswered(error, 403) && error.code === "OAUTH_SCOPE_REQUIRED") {
    throw refusalWithCode(error);
  }
  warnings.push(`dispatch: warning: ${what} could not be read: ${inboxReadRefusal(error)}`);
  return undefined;
}

/** A side read refused with 403 DEPLOYMENT_READ_REFUSED, settled as the null a Dispatch with no
 *  secrets broker answers: the caller's credential may not read the whole deployment's rows, so
 *  it has none of them to list. Any other failure is left for `sideRead` to warn on. */
function withoutDeploymentRefusal<T>(
  settled: SettledSideRead<T | null>
): SettledSideRead<T | null> {
  return "error" in settled &&
    dispatchAnswered(settled.error, 403) &&
    settled.error.code === "DEPLOYMENT_READ_REFUSED"
    ? { answer: null }
    : settled;
}

/**
 * What the person's Inbox page shows, as the `dispatch inbox` command prints it: each heading
 * with its count, then the rows under it, in the order the page bands them. The rows within a
 * band are the server's order and nothing re-sorts them.
 *
 * `Needs you` is the page's `needsYou` (`.../features/inbox/BlockedOnYou.tsx`): the asks waiting
 * on a person that the viewer has not deferred and that wait on them (`waitingOnYou`),
 * plus every credential request still waiting on a decision, which is the viewer's by
 * construction because the list is read as `?approver=me`. Here that is the band's own rows and
 * nothing narrower, since `?assignee=me` keeps only the asks that wait on the caller, as the
 * page's default view does; counting the rows it prints is therefore the page's rule, and it is
 * the one count that cannot disagree with the lines under it. An expired credential request
 * waits on nobody (`awaitsDecision`), so it is not counted here, as the page does not count it,
 * though `Credential requests` lists and counts it until somebody dismisses it.
 *
 * `Unread threads` is the page's thread list (`GET /api/v1/me/replies?kind=reply,agent`): the
 * threads the person is reading that somebody else has written in since, and the messages they
 * sent an agent session that it answered, newest reply first.
 *
 * `Credential requests` is the broker's list as Dispatch relays it, in its order: the pending
 * requests first, then the expired ones nobody has dismissed.
 */
function formatInboxSummary(
  rows: readonly InboxRow[],
  threads: readonly MyReplyRow[],
  requests: readonly CredentialPendingRow[],
  now: number
): string {
  const sections = { agent: [] as InboxRow[], human: [] as InboxRow[], later: [] as InboxRow[] };
  for (const row of rows) {
    const section = inboxSection(row, now);
    if (section !== undefined) sections[section].push(row);
  }
  return [
    `Needs you ${sections.human.length + requests.filter(awaitsDecision).length}`,
    ...sections.human.map(inboxAskLine),
    "",
    `Waiting on agents ${sections.agent.length}`,
    ...sections.agent.map(inboxAskLine),
    "",
    `Later ${sections.later.length}`,
    ...sections.later.map(inboxAskLine),
    "",
    `Unread threads ${threads.length}`,
    ...threads.map((row) => inboxThreadLine(row, now)),
    "",
    `Credential requests ${requests.length}`,
    ...requests.map(credentialRequestLine),
  ].join("\n");
}

/**
 * `dispatch inbox`: what the person's Inbox page shows. It names no owner, so it runs on the
 * session's context alone.
 */
export async function inboxCommand(context: SessionContext): Promise<DispatchToolResult> {
  const client = context.newClient();
  // All four reads at once: they depend on nothing of each other, and the credential list is
  // the slow one, since Dispatch relays it to the secrets broker.
  const pending = settleSideRead(client.credentialPending());
  const unread = settleSideRead(client.myReplies());
  let identity: WhoamiResponse;
  let rows: InboxRow[];
  try {
    [identity, rows] = await Promise.all([client.whoami(), client.inbox("me")]);
  } catch (error) {
    // The inbox route needs `person`, and a credential without it is refused 403
    // OAUTH_SCOPE_REQUIRED, whose message names the scope: the code is what the caller acts
    // on, so it reaches the text. `whoami` needs no scope.
    throw refusalWithCode(error);
  }
  const viewer = whoamiPerson(identity);
  const warnings: string[] = [];
  // A Dispatch with no secrets broker answers null, which is an ordinary deployment: it has
  // no credential requests, so it lists and counts none. The list is a read of the whole
  // deployment, which Dispatch refuses a restricted member, and a credential acting for one, with
  // 403 DEPLOYMENT_READ_REFUSED (a full member's credential reads it whatever it lists): such a
  // caller has no credential list to read, as the web Inbox never asks them for it, so it is
  // listed and counted as none and warns nothing.
  const requests: readonly CredentialPendingRow[] =
    sideRead(withoutDeploymentRefusal(await pending), "credential requests", warnings)?.pending ??
    [];
  const threads: readonly MyReplyRow[] =
    sideRead(await unread, "unread threads", warnings)?.rows ?? [];
  return {
    text: formatInboxSummary(rows, threads, requests, Date.now()),
    details: {
      viewer,
      asks: rows.length,
      unread_threads: threads.length,
      credential_requests: requests.length,
    },
    ...(warnings.length === 0 ? {} : { warnings }),
  };
}
