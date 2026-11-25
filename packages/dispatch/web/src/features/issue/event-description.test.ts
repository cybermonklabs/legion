import { beforeAll, expect, test } from "bun:test";
import { DELETED_POST_DELIVERY_ERROR } from "@legion/contracts";

import type { DispatchEvent } from "../../api/types";
import { activityDescription } from "../activity/activity-model";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { eventDescription } from "./event-description";

// The rows draw through the shared renderer, whose Markdown engine loads lazily, and they run in
// random order, so the file loads the engine before its first row, whichever row that is.
beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The record an ask event's question is: the ask itself, on its issue. */
const ASK_SOURCE = { id: "ask-1", issue: "CORE-1", kind: "ask" } as const;

function commentEvent(type: "comment.reopened" | "comment.edited"): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_name: "spec",
      ask_id: null,
      author: { id: "alice", kind: "user" },
      body: "Updated wording",
      created_at: "2026-09-11T00:00:00Z",
      edited_at: null,
      id: "comment-1",
      issue_key: "CORE-1",
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
    },
    seq: 1,
    type,
  } as DispatchEvent;
}

function editedAskEvent(): Extract<DispatchEvent, { type: "ask.edited" }> {
  return {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-11T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: null,
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: "2026-09-11T00:00:00Z",
      edited_by: { id: "session-1", kind: "session" },
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [],
      previous: { multiple: false, options: [], question: "Draft?", urgency: "med" },
      question: "Publish?",
      state: "open",
      urgency: "med",
    },
    seq: 2,
    type: "ask.edited",
  };
}

test("eventDescription labels a reopened comment", () => {
  expect(eventDescription(commentEvent("comment.reopened"))).toEqual(["Comment reopened"]);
});

test("eventDescription labels an edited comment", () => {
  expect(eventDescription(commentEvent("comment.edited"))).toEqual(["Comment edited"]);
});

test("eventDescription labels an edited ask with its new question", () => {
  // The question is a part of its own, which the one reader draws (`SentenceParts`): the line
  // never carries the question's source.
  expect(eventDescription(editedAskEvent())).toEqual([
    "Ask edited: ",
    { prose: "Publish?", source: ASK_SOURCE, tree: undefined },
  ]);
});

test("eventDescription hands an ask's question the tree its event carries", () => {
  const tree = { content: [], type: "doc" };
  expect(eventDescription({ ...editedAskEvent(), trees: { "payload.question": tree } })).toEqual([
    "Ask edited: ",
    { prose: "Publish?", source: ASK_SOURCE, tree },
  ]);
});

// The line of a deleted post's event says who deleted it, where its body would be. The read of
// the event takes the body out and stamps the marks in its place (`api/events.go`); this payload
// keeps its body beside the marks, so only the line's own reading of them keeps the words off.
// The delete's own event reads the same, and a payload that carries the time alone says the post
// is deleted and names nobody.
test("eventDescription reads a deleted post's events as who deleted it, with no part of its body", () => {
  const alice = { id: "alice", kind: "user" } as const;
  const post = (type: DispatchEvent["type"], marks: Record<string, unknown>): DispatchEvent =>
    ({
      actor: alice,
      created_at: "2026-09-11T00:00:00Z",
      id: 5,
      issue_key: "CORE-1",
      notify: false,
      payload: {
        author: alice,
        body: "Ship it on Friday.",
        id: "post-1",
        issue_key: "CORE-1",
        ...marks,
      },
      seq: 5,
      type,
    }) as DispatchEvent;
  const deleted = { deleted_at: "2026-09-11T01:00:00Z", deleted_by: alice };

  for (const [type, kind] of [
    ["comment.created", "Comment"],
    ["comment.answered", "Comment"],
    ["comment.deleted", "Comment"],
    ["message.created", "Message"],
    ["message.answered", "Message"],
    ["message.deleted", "Message"],
  ] as const) {
    expect({ type, line: eventDescription(post(type, deleted)) }).toEqual({
      type,
      line: [`${kind} deleted by `, { actor: alice }],
    });
  }
  expect(eventDescription(post("comment.created", { deleted_at: deleted.deleted_at }))).toEqual([
    "Comment deleted",
  ]);
  expect(eventDescription(post("message.deleted", { deleted_at: deleted.deleted_at }))).toEqual([
    "Message deleted",
  ]);
  // The same payload with no marks is a post that stands, and its line is its body, a part of its
  // own that the one reader draws (`SentenceParts`).
  expect(eventDescription(post("message.created", {}))).toEqual([
    {
      prose: "Ship it on Friday.",
      source: { id: "post-1", issue: "CORE-1", kind: "message" },
      tree: undefined,
    },
  ]);
});

function answeredAskEvent(
  previousAnswer?: Extract<DispatchEvent, { type: "ask.answered" }>["payload"]["previous_answer"]
): Extract<DispatchEvent, { type: "ask.answered" }> {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-11T00:00:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: true,
    payload: {
      anchor: null,
      artifact_id: null,
      asked_of: null,
      asked_of_at: null,
      asked_of_by: null,
      block_id: null,
      answer: { at: "2026-09-11T00:00:00Z", selected: ["Hold"], text: null, user: "alice" },
      author: { id: "session-1", kind: "session" },
      created_at: "2026-09-10T00:00:00Z",
      edited_at: null,
      id: "ask-1",
      issue_key: "CORE-1",
      kind: "question",
      multiple: false,
      opened_event_id: 1,
      options: [{ label: "Ship" }, { label: "Hold" }],
      question: "Publish?",
      state: "answered",
      urgency: "med",
      ...(previousAnswer === undefined ? {} : { previous_answer: previousAnswer }),
    },
    seq: 3,
    type: "ask.answered",
  };
}

test("eventDescription says an answer changed when it replaced an earlier one", () => {
  const question = { prose: "Publish?", source: ASK_SOURCE, tree: undefined };
  expect(eventDescription(answeredAskEvent())).toEqual(["Ask answered: ", question]);
  expect(
    eventDescription(
      answeredAskEvent({
        at: "2026-09-10T12:00:00Z",
        selected: ["Ship"],
        text: null,
        user: "alice",
      })
    )
  ).toEqual(["Answer changed: ", question]);
});

test("eventDescription labels a malformed decision", () => {
  const event: Extract<DispatchEvent, { type: "block.invalid" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-13T17:00:00Z",
    id: 3,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      block_id: "ask-block",
      version: 3,
      reason: 'ask block "ask-block" has an option without a label',
      disturbed_by: { id: "session-1", kind: "session" },
    },
    seq: 3,
    type: "block.invalid",
  };

  expect(eventDescription(event)).toEqual([
    'marked decision ask-block malformed: ask block "ask-block" has an option without a label',
  ]);
});

test("eventDescription reads a refused ci_check as the check's refusal", () => {
  const event: Extract<DispatchEvent, { type: "block.invalid" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-13T17:01:00Z",
    id: 4,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      block_id: "k1",
      version: 4,
      reason: "a check proved by CI carries no run",
      disturbed_by: { id: "session-1", kind: "session" },
      block_type: "check",
    },
    seq: 4,
    type: "block.invalid",
  };

  expect(eventDescription(event)).toEqual([
    "refused check k1: a check proved by CI carries no run",
  ]);
});

test("eventDescription names an added artifact", () => {
  const event: Extract<DispatchEvent, { type: "artifact.created" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-15T00:00:00Z",
    id: 4,
    issue_key: "OPS-52",
    notify: false,
    payload: {
      artifact: {
        created_at: "2026-09-15T00:00:00Z",
        created_by: { id: "session-1", kind: "session" },
        id: "artifact-1",
        issue_key: "OPS-52",
        kind: "doc",
        name: "cu-update-2026-09-15.md",
        primary: false,
        project: "OPS",
        ref_key: "OPS-52/cu-update-2026-09-15-md",
        session_id: null,
        slug: "cu-update-2026-09-15-md",
        versions: [],
      },
    },
    seq: 4,
    type: "artifact.created",
  };

  expect(eventDescription(event)).toEqual(["Added cu-update-2026-09-15.md"]);
});

test("eventDescription names a saved artifact version", () => {
  const event: Extract<DispatchEvent, { type: "artifact.version" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-09-15T00:00:00Z",
    id: 5,
    issue_key: "OPS-52",
    notify: false,
    payload: {
      artifact_id: "artifact-1",
      name: "cu-update-2026-09-15.md",
      version: {
        authors: [{ id: "session-1", kind: "session" }],
        created_at: "2026-09-15T00:00:00Z",
        named: false,
        number: 2,
        summary: null,
      },
    },
    seq: 5,
    type: "artifact.version",
  };

  expect(eventDescription(event)).toEqual(["Saved cu-update-2026-09-15.md v2"]);
});

test("eventDescription names the document and version of a review", () => {
  const approved: Extract<DispatchEvent, { type: "artifact.approved" }> = {
    actor: { id: "person@example.com", kind: "user" },
    created_at: "2026-09-15T00:00:00Z",
    id: 6,
    issue_key: "OPS-52",
    notify: true,
    payload: {
      actor: { id: "person@example.com", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      name: "spec.md",
      reason: null,
      version: 4,
    },
    seq: 6,
    type: "artifact.approved",
  };
  const changesRequested: Extract<DispatchEvent, { type: "artifact.changes_requested" }> = {
    actor: { id: "person@example.com", kind: "user" },
    created_at: "2026-09-16T00:00:00Z",
    id: 7,
    issue_key: "OPS-52",
    notify: true,
    payload: {
      actor: { id: "person@example.com", kind: "user" },
      artifact_id: "artifact-1",
      ask_id: null,
      name: "spec.md",
      reason: "Name the rollback step.",
      version: 5,
    },
    seq: 7,
    type: "artifact.changes_requested",
  };

  expect(eventDescription(approved)).toEqual(["Approved spec.md v4"]);
  expect(eventDescription(changesRequested)).toEqual(["Changes requested on spec.md v5"]);
});

test("eventDescription names the phase and version of a recorded handoff", () => {
  const event: Extract<DispatchEvent, { type: "handoff.recorded" }> = {
    actor: { id: "session-1", kind: "session" },
    created_at: "2026-10-26T00:00:00Z",
    id: 6,
    issue_key: "OPS-52",
    notify: true,
    payload: { handoff_id: "handoff-1", phase: "implement", version: 2 },
    seq: 6,
    type: "handoff.recorded",
  };

  expect(eventDescription(event)).toEqual(["Recorded the implement handoff, version 2"]);
});

test("eventDescription and the activity line name a person's spec edit by its versions", () => {
  const event: Extract<DispatchEvent, { type: "artifact.human_edited" }> = {
    actor: { id: "person@example.com", kind: "user" },
    created_at: "2026-10-24T00:00:00Z",
    id: 6,
    issue_key: "OPS-52",
    notify: true,
    payload: {
      artifact_id: "artifact-1",
      name: "spec.md",
      from_version: 4,
      to_version: 7,
      editors: ["person@example.com"],
      diff: "--- previous\n+++ current\n",
      diff_truncated: false,
    },
    seq: 6,
    type: "artifact.human_edited",
  };

  expect(eventDescription(event)).toEqual(["Edited spec.md v4 to v7"]);
  expect(activityDescription(event)).toEqual(["edited spec.md v4 to v7"]);
});

test("eventDescription names the child a reparent adds or removes", () => {
  const base = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-16T00:00:00Z",
    id: 2,
    issue_key: "CORE-1",
    notify: true,
    payload: { child_key: "CORE-12" },
    seq: 2,
  };
  expect(eventDescription({ ...base, type: "child.added" } as DispatchEvent)).toEqual([
    "Added child CORE-12",
  ]);
  expect(eventDescription({ ...base, type: "child.removed" } as DispatchEvent)).toEqual([
    "Removed child CORE-12",
  ]);
});

test("eventDescription names the repository an architecture import came from; a retained event without one reads as before", () => {
  const base = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-25T00:00:00Z",
    id: 3,
    issue_key: null,
    notify: false,
    project: "CORE",
    seq: 3,
  } as const;
  const commit = "3be61df5be90aa11bb22";
  const synced = (payload: Extract<DispatchEvent, { type: "architecture.synced" }>["payload"]) =>
    eventDescription({ ...base, payload, type: "architecture.synced" });
  const failed = (
    payload: Extract<DispatchEvent, { type: "architecture.sync_failed" }>["payload"]
  ) => eventDescription({ ...base, payload, type: "architecture.sync_failed" });

  expect(synced({ branch: "main", commit, components: 1, repo: "acme/infra" })).toEqual([
    "Architecture synced from acme/infra: 1 component at 3be61df5be90",
  ]);
  expect(
    failed({ branch: "main", error: "duplicate component id api", repo: "acme/infra" })
  ).toEqual(["Architecture sync from acme/infra failed: duplicate component id api"]);
  expect(synced({ commit, components: 3 })).toEqual([
    "Architecture synced: 3 components at 3be61df5be90",
  ]);
  expect(failed({ error: "duplicate component id api" })).toEqual([
    "Architecture sync failed: duplicate component id api",
  ]);
});

test("eventDescription names the person a project's default moved to; a retained clear reads as cleared", () => {
  const base = {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-25T00:00:00Z",
    id: 4,
    issue_key: null,
    notify: false,
    project: "CORE",
    seq: 4,
    type: "settings.project_default_assignee.updated",
  } as const;

  expect(
    eventDescription({
      ...base,
      payload: { assignee: "bob", project: "CORE", thread_logins: ["alice", "bob"] },
    })
  ).toEqual(["Default assignee updated: CORE → bob"]);
  // Written before every project named a default: the stored row of a clear, and no
  // `thread_logins`.
  expect(eventDescription({ ...base, payload: { assignee: null, project: "CORE" } })).toEqual([
    "Default assignee cleared: CORE",
  ]);
});

// The issue log names the same five movements, in its own voice: it has no actor column beside
// the line, so a release says what happened and a takeover names who lost the claim.
type ClaimLogEvent = Extract<DispatchEvent, { type: "issue.claimed" | "issue.released" }>;

const claimLogEvent = (
  type: ClaimLogEvent["type"],
  payload: Partial<ClaimLogEvent["payload"]>
): ClaimLogEvent =>
  ({
    actor: { id: "alice", kind: "user" },
    created_at: "2026-09-24T07:00:00Z",
    id: 9,
    issue_key: "CORE-1",
    notify: false,
    payload: { key: "CORE-1", status: "in_progress", claim: null, ...payload },
    project: "CORE",
    seq: 9,
    type,
  }) as ClaimLogEvent;

const held = {
  actor: { id: "session-one", kind: "session" as const, origin: { session_title: "Implementer" } },
  at: "2026-09-24T06:00:00Z",
};

test.each([
  [
    "a first claim",
    claimLogEvent("issue.claimed", { claim: held, reason: "claimed" }),
    ["Issue claimed"],
  ],
  [
    "a takeover",
    claimLogEvent("issue.claimed", { claim: held, previous_claim: held, reason: "takeover" }),
    ["Claim taken from ", { actor: held.actor }],
  ],
  [
    "a release",
    claimLogEvent("issue.released", { previous_claim: held, reason: "released" }),
    ["Claim released"],
  ],
  [
    "a close",
    claimLogEvent("issue.released", { previous_claim: held, reason: "closed" }),
    ["Claim released on close"],
  ],
])("the issue log line for %s", (_name, event, expected) => {
  expect(eventDescription(event)).toEqual(expected);
});

// A session the log names by id (an unsubscribe, a follower) stays that session, for `AgentName`
// to name by its current title and link, rather than the raw id it printed before.
test("the issue log keeps the session an unsubscribe or a follower names", () => {
  const sessionEvent = (type: string) =>
    ({
      actor: { id: "alice", kind: "user" },
      created_at: "2026-09-24T07:00:00Z",
      id: 10,
      issue_key: "CORE-1",
      notify: false,
      payload: { ask_id: "ask-1", by: { id: "alice", kind: "user" }, session_id: "session-two" },
      seq: 10,
      type,
    }) as DispatchEvent;
  const agent = { sessionId: "session-two", storedTitle: undefined };

  expect(eventDescription(sessionEvent("subscription.removed"))).toEqual(["Unsubscribed ", agent]);
  expect(eventDescription(sessionEvent("ask.follower_added"))).toEqual([agent, " follows an ask"]);
});

const receipt = (
  type: "message.delivery" | "comment.delivery",
  state: "sent" | "queued" | "failed",
  duplicate: boolean,
  error?: string
) =>
  ({
    actor: { id: "system", kind: "system" },
    created_at: "2026-11-02T09:15:00Z",
    id: 11,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      delivery: "aside",
      duplicate,
      state,
      target: "session:planner",
      ...(error === undefined ? {} : { error }),
    },
    seq: 11,
    type,
  }) as DispatchEvent;

// A queued attempt waits for a session that is not running, so the line says queued even when the
// stream already held the message: "already delivered" would say the session has it.
test("the issue log reads a queued delivery receipt as queued, a queued repeat included", () => {
  expect(eventDescription(receipt("message.delivery", "queued", true))).toEqual([
    "Message queued: Aside",
  ]);
  expect(eventDescription(receipt("message.delivery", "sent", true))).toEqual([
    "Message already delivered: Aside",
  ]);
  expect(eventDescription(receipt("comment.delivery", "queued", true))).toEqual([
    "Comment queued: Aside",
  ]);
  expect(eventDescription(receipt("comment.delivery", "sent", true))).toEqual([
    "Comment already delivered: Aside",
  ]);
});

// A delete appends one failed receipt per send of its post it ended, with its own words. That
// receipt is no failed send: the line says the post's delete ended the send, as the Activity
// tab's does, while any other failure still reads as failed.
test("the issue log reads a receipt the post's delete appended as the delete ending the send", () => {
  expect(
    eventDescription(receipt("comment.delivery", "failed", false, DELETED_POST_DELIVERY_ERROR))
  ).toEqual(["Comment send ended by the post's delete: Aside"]);
  expect(
    eventDescription(receipt("comment.delivery", "failed", false, "no live session planner"))
  ).toEqual(["Comment failed: Aside"]);
  expect(
    eventDescription(receipt("message.delivery", "failed", false, DELETED_POST_DELIVERY_ERROR))
  ).toEqual(["Message send ended by the post's delete: Aside"]);
  expect(
    eventDescription(receipt("message.delivery", "failed", false, "no live session planner"))
  ).toEqual(["Message failed: Aside"]);
});
