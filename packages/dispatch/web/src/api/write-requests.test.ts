import { expect, test } from "bun:test";

import {
  agentTokenCreateRequest,
  agentTokenRevokeRequest,
  agentTokenScopesRequest,
  architectureSourceDeleteRequest,
  architectureSourceRequest,
  architectureSourceSyncRequest,
  artifactCommentRequest,
  askAnswerRequest,
  askDeclineRequest,
  askHandOnRequest,
  askSnoozeRequest,
  askUnsnoozeRequest,
  commentAcceptRequest,
  commentRejectRequest,
  commentReopenRequest,
  commentResolveRequest,
  decisionKeepRequest,
  deliverySettingsRequest,
  goesUnderKey,
  issueClaimReleaseRequest,
  issueCommentRequest,
  issueCreateRequest,
  issueStateRequest,
  issueUnsubscribeRequest,
  repoProjectDeleteRequest,
  repoProjectRequest,
  type WriteRequest,
} from "./client";

/**
 * Every write that waits its turn on a line is made from its request alone (`WriteSpec.request`),
 * so each has a builder beside its client method (dispatch://INFRA-299/spec#b-r6). These rows are
 * what each builder makes: the method and path the route table holds it under, and the body the
 * client's own method sends through it.
 */

/** One builder, and the request it makes. */
const requests: readonly { readonly what: string; readonly made: WriteRequest }[] = [
  { made: issueCreateRequest({ project: "CORE", title: "Ship it" }), what: "the issue create" },
  { made: issueClaimReleaseRequest("CORE-1"), what: "the claim release" },
  { made: issueUnsubscribeRequest("CORE-1", "session-1"), what: "the session unsubscribe" },
  { made: issueStateRequest("CORE-1", { pinned: true }), what: "the header's pin" },
  { made: issueCommentRequest("CORE-1", { body: "A change" }), what: "the decisions change" },
  {
    made: artifactCommentRequest("doc-1", { ask_id: "ask-1", body: "Ask back" }),
    what: "a clarification on a document's ask",
  },
  { made: commentResolveRequest("comment-1"), what: "the comment resolve" },
  { made: commentReopenRequest("comment-1"), what: "the comment reopen" },
  { made: commentAcceptRequest("comment-1"), what: "the suggestion accept" },
  { made: commentRejectRequest("comment-1"), what: "the suggestion reject" },
  { made: askSnoozeRequest("ask-1", "2026-11-13T00:00:00Z"), what: "the ask snooze" },
  { made: askUnsnoozeRequest("ask-1"), what: "the ask un-snooze" },
  { made: askAnswerRequest("ask-1", { expected_edited_at: null }), what: "the ask answer" },
  { made: askDeclineRequest("ask-1", { note: "Not mine" }), what: "the ask decline" },
  { made: askHandOnRequest("ask-1", "sami@theorem.dev"), what: "the ask hand-on" },
  { made: decisionKeepRequest("doc-1", "store", "sha256:1"), what: "the decisions keep" },
  { made: agentTokenCreateRequest({ name: "planner" }), what: "the token mint" },
  { made: agentTokenScopesRequest("token-1", ["person"]), what: "the token's scopes" },
  { made: agentTokenRevokeRequest("token-1"), what: "the token revoke" },
  {
    made: repoProjectRequest("acme/widgets", { project: "CORE" }),
    what: "the repository mapping",
  },
  { made: repoProjectDeleteRequest("acme/widgets"), what: "the mapping removal" },
  {
    made: architectureSourceRequest("CORE", "legion/arch", { branch: "main" }),
    what: "the architecture source save",
  },
  {
    made: architectureSourceSyncRequest("CORE", "legion/arch"),
    what: "the architecture source refresh",
  },
  {
    made: architectureSourceDeleteRequest("CORE", "legion/arch"),
    what: "the architecture source removal",
  },
  {
    made: deliverySettingsRequest({
      deploy_repo: "acme/widgets",
      deploy_workflow_path: ".github/workflows/deploy.yml",
      population_authors: ["octocat"],
      pr_checks_workflow_path: ".github/workflows/pr-checks.yml",
      production_job_names: ["widgets-release / widgets-release"],
    }),
    what: "the delivery settings save",
  },
];

// R6. Each write the requirement names is made from its request alone, so each has a builder of
// its own: this is the request each makes, by method and path.
test("every lined write's builder makes the request its route takes", () => {
  expect(requests.map(({ made, what }) => `${what}: ${made.method} ${made.path}`)).toEqual([
    "the issue create: POST /api/v1/issues",
    "the claim release: DELETE /api/v1/issues/CORE-1/claim",
    "the session unsubscribe: DELETE /api/v1/issues/CORE-1/subscribers/session-1",
    "the header's pin: PUT /api/v1/me/issues/CORE-1/state",
    "the decisions change: POST /api/v1/issues/CORE-1/comments",
    "a clarification on a document's ask: POST /api/v1/artifacts/doc-1/comments",
    "the comment resolve: POST /api/v1/comments/comment-1/resolve",
    "the comment reopen: POST /api/v1/comments/comment-1/reopen",
    "the suggestion accept: POST /api/v1/comments/comment-1/accept",
    "the suggestion reject: POST /api/v1/comments/comment-1/reject",
    "the ask snooze: PUT /api/v1/me/asks/ask-1/snooze",
    "the ask un-snooze: DELETE /api/v1/me/asks/ask-1/snooze",
    "the ask answer: POST /api/v1/asks/ask-1/answer",
    "the ask decline: POST /api/v1/asks/ask-1/decline",
    "the ask hand-on: PUT /api/v1/asks/ask-1/asked-of",
    "the decisions keep: POST /api/v1/artifacts/doc-1/blocks/store/keep",
    "the token mint: POST /api/v1/me/agent-tokens",
    "the token's scopes: PATCH /api/v1/me/agent-tokens/token-1",
    "the token revoke: DELETE /api/v1/me/agent-tokens/token-1",
    "the repository mapping: PUT /api/v1/settings/repo-projects/acme/widgets",
    "the mapping removal: DELETE /api/v1/settings/repo-projects/acme/widgets",
    "the architecture source save: PUT /api/v1/projects/CORE/architecture-sources/legion/arch",
    "the architecture source refresh: POST /api/v1/projects/CORE/architecture-sources/legion/arch/sync",
    "the architecture source removal: DELETE /api/v1/projects/CORE/architecture-sources/legion/arch",
    "the delivery settings save: PUT /api/v1/settings/delivery",
  ]);
});

// R6's reload rule reads the method: a `POST` and a `PATCH` go under an idempotency key, which
// Dispatch answers a repeat of with what the first did, and a `PUT` and a `DELETE` carry none.
// That is why a lined `DELETE` is kept for its order and dropped rather than sent again.
test("only the POSTs and PATCHes among the lined writes go under a key", () => {
  const keyed = requests.filter(({ made }) => goesUnderKey(made.method)).map(({ what }) => what);
  const unkeyed = requests.filter(({ made }) => !goesUnderKey(made.method)).map(({ what }) => what);
  expect(unkeyed).toEqual([
    "the claim release",
    "the session unsubscribe",
    "the header's pin",
    "the ask snooze",
    "the ask un-snooze",
    "the ask hand-on",
    "the token revoke",
    "the repository mapping",
    "the mapping removal",
    "the architecture source save",
    "the architecture source removal",
    "the delivery settings save",
  ]);
  expect(keyed).toContain("the ask answer");
  expect(keyed).toContain("the issue create");
});

// The body is the one the client method sent, so a write made from its request alone writes what
// the method wrote.
test("each builder carries the body its client method sent", () => {
  expect(issueStateRequest("CORE-1", { pinned: true }).body).toEqual({ pinned: true });
  expect(askHandOnRequest("ask-1", "sami@theorem.dev").body).toEqual({
    asked_of: "sami@theorem.dev",
  });
  expect(decisionKeepRequest("doc-1", "store", "sha256:1").body).toEqual({
    text_sha256: "sha256:1",
  });
  expect(agentTokenScopesRequest("token-1", ["person"]).body).toEqual({ scopes: ["person"] });
  expect(commentResolveRequest("comment-1").body).toEqual({});
  // The reopen route takes no body at all (`reopenComment`).
  expect(commentReopenRequest("comment-1").body).toBeUndefined();
});

// A repository is `owner/repo` and nothing else: a builder refuses anything else rather than
// sending a path with a hole in it (`repoParams`).
test("a repository that is not owner/repo is refused by the builder", () => {
  expect(() => repoProjectRequest("widgets", { project: "CORE" })).toThrow(
    "/api/v1/settings/repo-projects/{owner}/{repo} has no value for {repo}"
  );
  expect(() => architectureSourceSyncRequest("CORE", "")).toThrow(
    "repository must be formatted as owner/repo"
  );
});
