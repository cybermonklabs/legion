import { ISSUE_UPDATE_PATCH_FIELDS } from "@legion/contracts";
import { mayHaveReachedServer } from "@legion/contracts/http-status";
import { commandLine } from "../dispatch-command";
import { DispatchGatewayError, DispatchNoRouteError, DispatchServiceError } from "../dispatch-http";
import type { Owner } from "../dispatch-owner";
import { messageFor } from "../errors";
import { optionalString } from "./context";

/**
 * A Dispatch refusal carrying its own code in the message the host shows: the code
 * (ISSUE_CLAIMED, CLAIM_CONTENDED, EXTERNAL_LINK_TAKEN, ...) is the part an agent acts on, and
 * the prose alone hides it. Only the message changes: every other field of the refusal
 * (`candidates`, `current`, `mismatches`, `issues`, `changed`, `unguardedBlocks`, `textRefusal`,
 * `hint`) rides along, since a caller reads them off the error it catches. An error that is not a
 * refusal is returned as it is, so a caller's `throw refusalWithCode(error)` rethrows it
 * untouched. This returns rather than throws, so the throw stays at the call site, where a reader
 * of the handler sees that the path ends there.
 *
 * A gateway's answer stays a `DispatchGatewayError`, and the caller's account of what its call did
 * joins it in one sentence. Where the request may have reached Dispatch (`mayHaveReachedDispatch`),
 * the client's advice could judge only from the method (a write is told to check whether it took
 * effect), so that account, which knows what its call did, takes its place and must say what to do;
 * otherwise the request never reached Dispatch, and the account follows the client's advice.
 */
export function refusalWithCode(error: unknown, ...clauses: string[]): unknown {
  const suffix = clauses.filter((clause) => clause !== "").join("; ");
  const joined = suffix === "" ? "" : `; ${suffix}`;
  if (error instanceof DispatchGatewayError) {
    let told = error.message;
    if (joined !== "") {
      told = error.mayHaveReachedDispatch
        ? `${error.answer}${joined}`
        : `${error.answer}, so ${error.advice}${joined}`;
    }
    return new DispatchGatewayError(
      error.status,
      error.answer,
      error.advice,
      `${error.code}: ${told}`
    );
  }
  if (!(error instanceof DispatchServiceError)) return error;
  // A message that ends a sentence (a parent-status conflict's does) takes the clauses as a
  // sentence of their own, as withAccount joins its account.
  const told =
    joined !== "" && /[.!?]$/.test(error.message)
      ? `${error.message} ${suffix.charAt(0).toUpperCase()}${suffix.slice(1)}`
      : `${error.message}${joined}`;
  return new DispatchServiceError(error.code, error.status, `${error.code}: ${told}`, error);
}

/**
 * What a `409 PARENT_STATUS_CONFLICT` prints for the command `tool` it answered, called with
 * `args` as its caller typed them (`runDispatchCli`): the refusal's own message, then each issue
 * that holds the rule as `KEY [status] title`, then what to do next. Dispatch judges only the pairs
 * a write changes, so that last part follows what the call changed, and names no step that the
 * rule refuses:
 *
 * - A create (`dispatch issue`) is refused only under a done parent, which the refusal names, and
 *   takes no flag that moves it: it prints how to reopen that parent first or file the issue
 *   elsewhere. The reopen takes `--include-parent`, as a reparent's move does, because the rule
 *   also refuses a plain status change of that parent where the issue above it is done or not
 *   started.
 * - A reparent (`dispatch issue-update --parent P`) is refused for P, which the issue is not under
 *   yet, and no flag of the command moves P: `--include-parent` moves the issues above the issue
 *   as it stands, and goes with no `--parent`. So it prints how to move P into work first, then
 *   the reparent to run again, naming `--issue` and `--status` only where the call did. A child
 *   that holds the rule against the status the reparent also wrote prints the flags that move the
 *   children, which go with `--status` alone: the status change goes first, in a call of its own,
 *   and the reparent after it, without `--status`.
 * - A status change prints the flags of `dispatch issue-update --status` that move the issues.
 *   Those flags go with the status alone (and the reason of a close), so beside any other field
 *   it also says to send the other changes in a call of their own.
 *
 * `issue` is absent from `args` when the owner came from `LEGION_ISSUE`, which a command run again
 * without `--issue` resolves the same way, so a line names `--issue` only where the call did.
 * Dispatch writes each issue's status into the message too, so a reader that takes no `issues`
 * still sees them. Undefined for any other error, and for a conflict that names no issue, which
 * prints as its message alone.
 */
export function parentStatusConflictLines(
  error: unknown,
  tool: string,
  args: Readonly<Record<string, unknown>>
): string[] | undefined {
  if (!dispatchAnswered(error, 409) || error.code !== "PARENT_STATUS_CONFLICT") return undefined;
  const issues = error.issues ?? [];
  if (issues.length === 0) return undefined;
  const parents = issues.filter((issue) => issue.relation === "parent").map(({ key }) => key);
  const children = issues.filter((issue) => issue.relation === "child").map(({ key }) => key);
  return [
    messageFor(error),
    ...issues.map((issue) => `${issue.key} [${issue.status}] ${issue.title}`),
    ...nextSteps(tool, args, parents, children),
  ];
}

const CHILD_FLAGS =
  "dispatch issue-update --status takes --unlink-children, which removes its open children from it, or, with a status other than done, --include-children, which moves the open issues under the issue with it. A person signed in at a browser closes them with it on the issue's page.";

const PARENT_FLAG =
  "dispatch issue-update --status takes --include-parent, which moves each issue above the issue that is not in work to in_progress.";

/** The call that moves `key` into work, with the clause on what else it moves, as each line of
 *  `nextSteps` that names a parent writes it. */
function includeParentCall(key: string): string {
  return `${commandLine("dispatch_issue_update", { issue: key, status: "in_progress", include_parent: true })}, which also moves each issue above ${key} that is not in work`;
}

/** What the caller of `tool` with `args` does next about the issues a refusal names: `parents`,
 *  each key of relation `parent`, and `children`, each key of relation `child`
 *  (`parentStatusConflictLines`). */
function nextSteps(
  tool: string,
  args: Readonly<Record<string, unknown>>,
  parents: readonly string[],
  children: readonly string[]
): string[] {
  if (tool === "dispatch_issue") {
    return parents.map(
      (key) =>
        `dispatch issue takes no flag that moves a done parent. Reopen ${key} first, with ${includeParentCall(key)}, then run dispatch issue again; or run it with --parent naming an open issue, or with no --parent.`
    );
  }
  // Only dispatch issue-update's arguments are read here.
  const issueUpdate = tool === "dispatch_issue_update";
  const written: Readonly<Record<string, unknown>> = issueUpdate ? args : {};
  // The fields the call wrote, the issue it names aside.
  const fields = Object.keys(written).filter(
    (field) => field !== "issue" && written[field] !== undefined
  );
  const issue = optionalString(written, "issue");
  const childHolds = children.length > 0;
  const status = optionalString(written, "status");
  const parent = optionalString(written, "parent");
  const alone = `--status${status === "done" ? " and --reason" : ""} alone`;
  if (parent !== undefined && parent !== "") {
    // A child holds the rule only against the status the reparent also wrote. The flags that
    // move it go with the status alone, so the status change (with the reason of a close) goes
    // first, in a call of its own, and the reparent runs again without them.
    const issueFlag = issue === undefined ? "" : ` --issue ${issue}`;
    const statusFlag = status === undefined || childHolds ? "" : ` --status ${status}`;
    const kept = fields.filter(
      (field) => field !== "parent" && field !== "status" && !(childHolds && field === "reason")
    );
    const otherFlags = kept.length > 0 ? ", with the call's other flags" : "";
    const reparent = `dispatch issue-update${issueFlag} --parent ${parent}${statusFlag}${otherFlags}`;
    const afterStatus = childHolds ? ", after the status change," : "";
    return [
      ...(childHolds
        ? [
            `${CHILD_FLAGS} Each goes with ${alone}, so run the status change first, in a call of its own, and the reparent after it: ${reparent}.`,
          ]
        : []),
      ...parents.map(
        (key) =>
          `dispatch issue-update takes no flag that moves the parent a reparent names. Move ${key} into work first, with ${includeParentCall(key)}; then${afterStatus} run the reparent again: ${reparent}.`
      ),
    ];
  }
  const others = ISSUE_UPDATE_PATCH_FIELDS.some((field) => written[field] !== undefined);
  const apart = (subject: string): string =>
    others
      ? ` ${subject} with ${alone}, so send the call's other changes in a call of their own.`
      : "";
  return [
    ...(childHolds ? [`${CHILD_FLAGS}${apart("Each goes")}`] : []),
    ...(parents.length > 0 ? [`${PARENT_FLAG}${apart("It goes")}`] : []),
  ];
}

/** Whether Dispatch itself answered `status`; a gateway's answer of that status says nothing it decided. */
export function dispatchAnswered(error: unknown, status: number): error is DispatchServiceError {
  return error instanceof DispatchServiceError && error.fromDispatch && error.status === status;
}

/** The message an older build's comment edit refuses the `actor` this client sends with: its
 *  `decodeJSON` (`api/server.go`) writes "invalid JSON body: " and the decoder's error, which Go's
 *  `encoding/json` words `json: unknown field "actor"`. */
const OLDER_COMMENT_EDIT_ACTOR = 'invalid JSON body: json: unknown field "actor"';

/**
 * The answer of `call`, with an older Dispatch's refusal of a post correction replaced by the one
 * sentence that names the real cause. The plugins release from main as this merges while
 * Dispatch's own server is deployed by hand (dispatch://INFRA-1464), so for a while these
 * commands run against a server with no route by which an agent edits or deletes a post or reads
 * the bodies one replaced: its router answers `404 NOT_FOUND` "no route for …" to both deletes,
 * the message edit and the history reads. Every client call retries such an answer, so what
 * arrives here is `DispatchNoRouteError`, which a server with the route never answers: a roll
 * puts the route in place within a minute, while a Dispatch that predates post corrections
 * answers that way until it is deployed, and only this sentence tells an agent which to expect.
 * Its comment edit, the one route that was there before, answers this release's call by the
 * credential and the build. It first reads the person it acts for (`actingPerson`), which
 * answers a credential without the person scope `403 OAUTH_SCOPE_REQUIRED`; the route at this
 * head names no scope, so only an older build answers that code there. A credential with that
 * scope, a person's own token included, passes. A build older than #2461 then decodes the body
 * into an input holding only `body`, `base_revision` and `blocks`, refusing an undeclared
 * member; the body this client sends always carries `actor`, so it answers `400 INVALID_JSON`
 * naming the unknown field `actor` before it compares the author. A build from #2461 on ignores
 * that member and compares the comment's author with the person: it takes the edit of the
 * person's own comment and keeps no earlier body, and it refuses the edit of a comment a session
 * the token names wrote `403 NOT_AUTHOR`, which is passed through as it is, since a true
 * `NOT_AUTHOR` reads the same. Only the comment edit reads either of the first two codes as the
 * server's age, and only in those exact forms: an `INVALID_JSON` naming any other field is
 * passed through. Without this, an agent is told its
 * credential lacks a scope, or that its own request was malformed, and goes asking a person for
 * one or rewriting the call.
 *
 * Where `post` names the post the call was for, Dispatch's own `404 NOT_FOUND` of that post is
 * rewritten to name it, since `not found` alone says neither which post nor whether the route
 * was there. A router's no-route 404 is not that answer and keeps the sentence above.
 */
export async function throughPostCorrections<T>(
  tool: string,
  url: string,
  call: Promise<T>,
  post?: { readonly kind: "comment" | "message"; readonly id: string }
): Promise<T> {
  try {
    return await call;
  } catch (error) {
    const commentEditIsAge =
      tool === "dispatch_edit_comment" &&
      ((dispatchAnswered(error, 403) && error.code === "OAUTH_SCOPE_REQUIRED") ||
        (dispatchAnswered(error, 400) &&
          error.code === "INVALID_JSON" &&
          error.message === OLDER_COMMENT_EDIT_ACTOR));
    if (!(error instanceof DispatchNoRouteError) && !commentEditIsAge) {
      throw post === undefined ? error : refuseMissingPost(error, post);
    }
    throw new Error(
      `This Dispatch (${url}) predates post corrections: it has no route by which an agent edits ` +
        "or deletes a post or reads the bodies one replaced. Until it is deployed with this " +
        "release's server, a correction is one more post saying what changed."
    );
  }
}

/**
 * The refusal of a call on one post by its id: Dispatch's own `404 NOT_FOUND` names neither the
 * post nor the id, so a reader cannot tell a post that is not there from an issue or a route that
 * is not, and the id the call used says which (dispatch://INFRA-1523). Any other answer, a
 * router's no-route 404 among them, is returned as it is.
 */
export function refuseMissingPost(
  error: unknown,
  post: { readonly kind: "comment" | "message"; readonly id: string }
): unknown {
  return dispatchNotFound(error)
    ? refusalWithCode(error, `this Dispatch holds no ${post.kind} ${post.id}`)
    : error;
}

/**
 * Whether a write that failed with `error` may have taken effect anyway. Only an answer sent
 * before the write could apply proves it did not: Dispatch's own refusal (a 4xx but a 408), or a
 * gateway's answer that never reached Dispatch (`mayHaveReachedDispatch`), which the client throws
 * as such only when no earlier attempt of the write may have reached Dispatch either. A status
 * after which the request may have reached the server (`mayHaveReachedServer`: a 5xx, which
 * Dispatch can send after it committed, or a 408), a timeout, a transport error or a
 * `DispatchWriteUnsettledError` (an earlier attempt may have reached Dispatch) says nothing either
 * way.
 */
export function writeMayHaveLanded(error: unknown): boolean {
  if (error instanceof DispatchGatewayError) return error.mayHaveReachedDispatch;
  return !(error instanceof DispatchServiceError) || mayHaveReachedServer(error.status);
}

/** The reason a call posted before its close: the message's id and its `dispatch://` reference. */
export interface ClosingNote {
  readonly id: string;
  readonly ref: string;
}

/** What a call whose closing reason landed says when its close then failed with `error`: where the
 *  reason is, and whether the close may have landed anyway, since a blind retry would post the
 *  reason a second time. A gateway's timeout or rate limit refused nothing there is to fix. */
export function closeAccount(error: unknown, closingNote: ClosingNote): string {
  const posted = `the reason already landed as message ${closingNote.id} (${closingNote.ref})`;
  const fix =
    error instanceof DispatchGatewayError && error.transient
      ? ""
      : "fix what refused the close, then ";
  return writeMayHaveLanded(error)
    ? `${posted}, and the close may or may not have taken effect. Read the issue's status before retrying: done means it closed; otherwise retry with a reason that points at message ${closingNote.id}, since retrying this call posts its reason again`
    : `${posted} but the issue did not close. Retrying this call posts its reason again, so ${fix}retry with a reason that points at message ${closingNote.id}`;
}

/**
 * An error that is no refusal (a timeout, a transport error) with `account`, the caller's clause
 * on what its call did, after its message: joined to it with "; ", or as a sentence of its own
 * after a message that ends one ("The operation timed out.", "… able to access the url?").
 */
export function withAccount(error: unknown, account: string): Error {
  const message = messageFor(error);
  const told = /[.!?]$/.test(message)
    ? `${message} ${account.charAt(0).toUpperCase()}${account.slice(1)}`
    : `${message}; ${account}`;
  return new Error(told, { cause: error });
}

/**
 * Whether Dispatch itself answered `404 NOT_FOUND` for the thing asked for. A router's no-route
 * 404, which every build gives a path that names no route, is a `DispatchNoRouteError` and is no
 * answer about the thing: it is what tells a correction that the server predates post
 * corrections, so it is never read as a missing post.
 */
function dispatchNotFound(error: unknown): error is DispatchServiceError {
  return (
    !(error instanceof DispatchNoRouteError) &&
    dispatchAnswered(error, 404) &&
    error.code === "NOT_FOUND"
  );
}

/**
 * A document reference that names no document of its owner: the answer of a read that answered,
 * so an answer about the document. Its message is `documentReferenceProblem`'s
 * (`document-targets.ts`).
 */
export class DocumentNotFoundError extends Error {}

/**
 * The refusal a reference's own read of a post or an ask earns when Dispatch answers
 * `404 NOT_FOUND`: its `not found` names neither the item nor its owner, so a reader cannot tell
 * an item that is not there from an issue or a document that is not (dispatch://INFRA-1523).
 * `readOwner` reads the reference's own owner, and its answer decides the refusal: an issue
 * Dispatch itself does not hold is refused in those words, a document reference that names no
 * document of its owner meets that read's own refusal, and an owner that stands names the item it
 * holds no row of.
 *
 * A refusal names only what it read. `<owner> holds no <kind> <id>` is an answer about the owner,
 * so it is written only where the read of the owner answered. Where that read failed instead - a
 * gateway, a 5xx, a router's no-route answer, a transport error, or a refusal such as Dispatch's
 * `400 ARTIFACT_REFERENCE_AMBIGUOUS` for a filename two documents share - the refusal names the
 * failed read and its failure, the refusal's own words among them, and says nothing of whether
 * the owner holds the item.
 */
export async function refuseMissingOfRef(
  error: unknown,
  kind: "ask" | "comment" | "message",
  id: string,
  owner: Owner,
  ownerName: string,
  readOwner: () => Promise<unknown>
): Promise<unknown> {
  if (!dispatchNotFound(error)) return error;
  const missingOwner = await readOwner().then(
    () => undefined,
    (caught: unknown) => {
      if (owner.kind === "issue" && dispatchNotFound(caught)) {
        return refusalWithCode(error, `this Dispatch holds no issue ${owner.issue}`);
      }
      if (caught instanceof DocumentNotFoundError) return caught;
      return refusalWithCode(
        error,
        `the read of ${ownerName} failed, so this does not say whether ${ownerName} exists or` +
          ` holds ${kind} ${id}: ${messageFor(caught)}`
      );
    }
  );
  return missingOwner ?? refusalWithCode(error, `${ownerName} holds no ${kind} ${id}`);
}
