import { type Mock, mock, spyOn } from "bun:test";

import {
  api,
  type DeclineAskInput,
  type WriteBounds,
  type WriteMethod,
  type WriteRequest,
} from "../api/client";
import type {
  AnswerAskInput,
  Ask,
  AskSnooze,
  Comment,
  CreateAgentMessageInput,
  CreateCommentInput,
  CreateIssueInput,
  InboxDismissRequest,
  InboxDismissResponse,
  InboxReadRequest,
  InboxReadResponse,
  Issue,
  Message,
  RepoProject,
  UpdateIssueInput,
  UserAgentState,
  UserAgentStateInput,
  UserIssueState,
} from "../api/types";

/** What a test answers the writes made from their request alone with (`answerSends`). */
export interface SendAnswers {
  /** Has `fn` answer every attempt of `request`, and hands `request` back. */
  readonly answer: <R extends WriteRequest>(
    request: R,
    fn: (bounds: WriteBounds) => Promise<unknown>
  ) => R;
  /** Every request answered here, in order. */
  readonly calls: readonly WriteRequest[];
  /** Puts `api.send` back. */
  readonly restore: () => void;
}

/**
 * Answers the writes a test makes from their request alone (`WriteSpec.request`, every plain write
 * on a line): each attempt of such a write is `api.send(request)` with the very request it named,
 * so `answer(request, fn)` has `fn` answer every attempt of it, telling `sent` the request first as
 * the client does. A request the test named no answer for goes through the real `api.send`.
 */
export function answerSends(): SendAnswers {
  const answers = new WeakMap<WriteRequest, (bounds: WriteBounds) => Promise<unknown>>();
  const calls: WriteRequest[] = [];
  const real = api.send.bind(api);
  const spy = spyOn(api, "send").mockImplementation(((
    request: WriteRequest,
    bounds: WriteBounds
  ) => {
    const answer = answers.get(request);
    if (answer === undefined) return real(request, bounds);
    calls.push(request);
    bounds.sent?.(request);
    return answer(bounds);
  }) as typeof api.send);
  return {
    answer: (request, fn) => {
      answers.set(request, fn);
      return request;
    },
    calls,
    restore: () => spy.mockRestore(),
  };
}

/** A route's answer, called with the segments its path's `*`s matched, the body and the bounds. */
type RouteAnswer = (...args: never[]) => Promise<unknown>;

/** The same answer as the router calls it, with what one request carries. */
type RouteCall = (...args: unknown[]) => Promise<unknown>;

/** One route a test answers: its method, its path with each `*` one segment, and the answer. */
interface Route {
  readonly method: WriteMethod;
  readonly path: RegExp;
  readonly answer: RouteCall;
}

/** The routes tests answer now, newest first, and the one spy on `api.send` that reads them. */
const routes: Route[] = [];
let routing: { restore: () => void } | undefined;

/**
 * Answers every write to Dispatch whose request is `method` on `path` - each `*` in it one path
 * segment - with a mock of its own, however the page made it: from its request alone (a plain write
 * on a line), or through the client method that builds the same request. The mock is called with
 * each segment the `*`s matched (decoded), the request's body and the attempt's bounds - its key
 * among them - so a test asserts the request a write sent, its method and path by the route it
 * answered on and its body and key by the call (`toHaveBeenCalledWith("CORE-1", { priority: 2 },
 * keyedAttempt)`). The newest route that matches answers; any other request goes to the real
 * `api.send`. A route answers until its test ends (`resetRouteAnswers`); `mockRestore` only
 * takes its answers back.
 */
export function answerRoute<F extends RouteAnswer>(method: WriteMethod, path: string): Mock<F> {
  const pattern = new RegExp(
    `^${path
      .split("*")
      .map((part) => part.replace(/[.?+^$|()[\]{}\\]/gu, "\\$&"))
      .join("([^/?]+)")}$`,
    "u"
  );
  // Unanswered until the test says how: the route's answer type is the test's to name.
  const unanswered = (async () => undefined) as unknown as F;
  const answer = mock<F>(unanswered);
  // The router hands each answer what one request carries, whatever the route's own signature.
  const call = answer as unknown as RouteCall;
  const route: Route = { answer: call, method, path: pattern };
  routes.unshift(route);
  if (routing === undefined) {
    const real = api.send.bind(api);
    const spy = spyOn(api, "send").mockImplementation(((
      request: WriteRequest,
      bounds: WriteBounds
    ) => {
      for (const { answer: answered, method: on, path: at } of routes) {
        const matched = request.method === on ? at.exec(request.path) : null;
        if (matched === null) continue;
        bounds.sent?.(request);
        return answered(...matched.slice(1).map(decodeURIComponent), request.body, bounds);
      }
      return real(request, bounds);
    }) as typeof api.send);
    routing = { restore: () => spy.mockRestore() };
  }
  return answer;
}

/** `PATCH /api/v1/issues/{key}` (`issuePatchRequest`): called with the key, the change and the
 *  bounds. */
export const answerIssuePatches = () =>
  answerRoute<(key: string, input: UpdateIssueInput, bounds: WriteBounds) => Promise<Issue>>(
    "PATCH",
    "/api/v1/issues/*"
  );

/** `POST /api/v1/issues` (`issueCreateRequest`). */
export const answerIssueCreates = () =>
  answerRoute<(input: CreateIssueInput, bounds: WriteBounds) => Promise<Issue>>(
    "POST",
    "/api/v1/issues"
  );

/** `PUT /api/v1/settings/repo-projects/{owner}/{repo}` (`repoProjectRequest`): called with the
 *  owner and the repository as two segments, the body and the bounds. */
export const answerRepoProjects = () =>
  answerRoute<
    (
      owner: string,
      repo: string,
      input: { project: string; default?: boolean },
      bounds: WriteBounds
    ) => Promise<RepoProject>
  >("PUT", "/api/v1/settings/repo-projects/*/*");

/** `DELETE /api/v1/settings/repo-projects/{owner}/{repo}` (`repoProjectDeleteRequest`). */
export const answerRepoProjectRemovals = () =>
  answerRoute<
    (owner: string, repo: string, body: undefined, bounds: WriteBounds) => Promise<unknown>
  >("DELETE", "/api/v1/settings/repo-projects/*/*");

/** `POST /api/v1/artifacts/{id}/blocks/{block_id}/keep` (`decisionKeepRequest`): called with the
 *  document, the block and the body the keep carries. */
export const answerDecisionKeeps = () =>
  answerRoute<
    (
      id: string,
      blockID: string,
      body: { text_sha256: string },
      bounds: WriteBounds
    ) => Promise<unknown>
  >("POST", "/api/v1/artifacts/*/blocks/*/keep");

/** `POST /api/v1/me/agent-tokens` (`agentTokenCreateRequest`). */
export const answerAgentTokenMints = () =>
  answerRoute<
    (input: { name: string; scopes?: string[] }, bounds: WriteBounds) => Promise<unknown>
  >("POST", "/api/v1/me/agent-tokens");

/** `PATCH /api/v1/me/agent-tokens/{id}` (`agentTokenScopesRequest`). */
export const answerAgentTokenScopes = () =>
  answerRoute<(id: string, body: { scopes: string[] }, bounds: WriteBounds) => Promise<unknown>>(
    "PATCH",
    "/api/v1/me/agent-tokens/*"
  );

/** `DELETE /api/v1/me/agent-tokens/{id}` (`agentTokenRevokeRequest`). */
export const answerAgentTokenRevokes = () =>
  answerRoute<(id: string, body: undefined, bounds: WriteBounds) => Promise<unknown>>(
    "DELETE",
    "/api/v1/me/agent-tokens/*"
  );

/** `PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}`
 *  (`architectureSourceRequest`): called with the project, the owner and the repository. */
export const answerArchitectureSources = () =>
  answerRoute<
    (
      project: string,
      owner: string,
      repo: string,
      input: { branch: string },
      bounds: WriteBounds
    ) => Promise<unknown>
  >("PUT", "/api/v1/projects/*/architecture-sources/*/*");

/** `POST /api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync`
 *  (`architectureSourceSyncRequest`). */
export const answerArchitectureSourceSyncs = () =>
  answerRoute<
    (
      project: string,
      owner: string,
      repo: string,
      body: undefined,
      bounds: WriteBounds
    ) => Promise<unknown>
  >("POST", "/api/v1/projects/*/architecture-sources/*/*/sync");

/** `DELETE /api/v1/projects/{key}/architecture-sources/{owner}/{repo}`
 *  (`architectureSourceDeleteRequest`). */
export const answerArchitectureSourceRemovals = () =>
  answerRoute<
    (
      project: string,
      owner: string,
      repo: string,
      body: undefined,
      bounds: WriteBounds
    ) => Promise<unknown>
  >("DELETE", "/api/v1/projects/*/architecture-sources/*/*");

/** `PUT /api/v1/settings/delivery` (`deliverySettingsRequest`). */
export const answerDeliverySettings = () =>
  answerRoute<(input: Record<string, unknown>, bounds: WriteBounds) => Promise<unknown>>(
    "PUT",
    "/api/v1/settings/delivery"
  );

/** `PUT /api/v1/me/issues/{key}/state` (`issueStateRequest`). */
export const answerIssueStates = () =>
  answerRoute<
    (key: string, input: Partial<UserIssueState>, bounds: WriteBounds) => Promise<UserIssueState>
  >("PUT", "/api/v1/me/issues/*/state");

/** `PUT /api/v1/me/agents/{session}/state` (`agentStateRequest`). */
export const answerAgentStates = () =>
  answerRoute<
    (session: string, input: UserAgentStateInput, bounds: WriteBounds) => Promise<UserAgentState>
  >("PUT", "/api/v1/me/agents/*/state");

/** `PUT /api/v1/me/threads/{id}/read` (`threadReadRequest`). */
export const answerThreadReads = () =>
  answerRoute<
    (id: string, body: { read_through: string }, bounds: WriteBounds) => Promise<unknown>
  >("PUT", "/api/v1/me/threads/*/read");

/** `POST /api/v1/me/inbox/read` (`markInboxReadRequest`): the Inbox's Mark all read, called
 *  with the body and the bounds. */
export const answerInboxReads = () =>
  answerRoute<(body: InboxReadRequest, bounds: WriteBounds) => Promise<InboxReadResponse>>(
    "POST",
    "/api/v1/me/inbox/read"
  );

/** `POST /api/v1/me/inbox/dismiss` (`inboxDismissRequest`): the Inbox's Dismiss and its Move to
 *  Inbox, called with the body and the bounds. */
export const answerInboxDismisses = () =>
  answerRoute<(body: InboxDismissRequest, bounds: WriteBounds) => Promise<InboxDismissResponse>>(
    "POST",
    "/api/v1/me/inbox/dismiss"
  );

/** `PUT /api/v1/me/asks/{id}/snooze` (`askSnoozeRequest`). */
export const answerAskSnoozes = () =>
  answerRoute<
    (id: string, body: { snoozed_until: string }, bounds: WriteBounds) => Promise<AskSnooze>
  >("PUT", "/api/v1/me/asks/*/snooze");

/** `DELETE /api/v1/me/asks/{id}/snooze` (`askUnsnoozeRequest`). */
export const answerAskUnsnoozes = () =>
  answerRoute<(id: string, body: undefined, bounds: WriteBounds) => Promise<unknown>>(
    "DELETE",
    "/api/v1/me/asks/*/snooze"
  );

/** `POST /api/v1/asks/{id}/answer` (`askAnswerRequest`). */
export const answerAskAnswers = () =>
  answerRoute<(id: string, input: AnswerAskInput, bounds: WriteBounds) => Promise<Ask>>(
    "POST",
    "/api/v1/asks/*/answer"
  );

/** `POST /api/v1/asks/{id}/decline` (`askDeclineRequest`). */
export const answerAskDeclines = () =>
  answerRoute<(id: string, input: DeclineAskInput, bounds: WriteBounds) => Promise<Ask>>(
    "POST",
    "/api/v1/asks/*/decline"
  );

/** `PUT /api/v1/asks/{id}/asked-of` (`askHandOnRequest`): the ask handed on, called with the
 *  body the route takes rather than the login alone. */
export const answerAskHandOns = () =>
  answerRoute<(id: string, body: { asked_of: string }, bounds: WriteBounds) => Promise<Ask>>(
    "PUT",
    "/api/v1/asks/*/asked-of"
  );

/** `POST /api/v1/issues/{key}/comments` (`issueCommentRequest`). */
export const answerIssueComments = () =>
  answerRoute<(key: string, input: CreateCommentInput, bounds: WriteBounds) => Promise<Comment>>(
    "POST",
    "/api/v1/issues/*/comments"
  );

/** `POST /api/v1/artifacts/{id}/comments` (`artifactCommentRequest`). */
export const answerArtifactComments = () =>
  answerRoute<(id: string, input: CreateCommentInput, bounds: WriteBounds) => Promise<Comment>>(
    "POST",
    "/api/v1/artifacts/*/comments"
  );

/** One comment action: `POST /api/v1/comments/{id}/<kind>` (`commentResolveRequest` and its
 *  kin), called with the comment's id and the attempt's bounds. */
export const answerCommentActions = (kind: "accept" | "reject" | "reopen" | "resolve") =>
  answerRoute<(id: string, body: unknown, bounds: WriteBounds) => Promise<Comment>>(
    "POST",
    `/api/v1/comments/*/${kind}`
  );

/** `POST /api/v1/agents/{session}/messages` (`agentMessageRequest`). */
export const answerAgentMessages = () =>
  answerRoute<
    (session: string, input: CreateAgentMessageInput, bounds: WriteBounds) => Promise<Message>
  >("POST", "/api/v1/agents/*/messages");

/** Lets every route a test answered go, and puts `api.send` back: the unit-test preload, after
 *  each test. */
export function resetRouteAnswers(): void {
  routes.length = 0;
  routing?.restore();
  routing = undefined;
}
