import { expect, test } from "bun:test";
import { DELETED_POST_DELIVERY_ERROR, type SettledDeliveryState } from "@legion/contracts";

import type { DispatchEvent } from "../../api/types";
import { activityDescription, buildActivityItems, dateKey } from "./activity-model";

function message(seq: number, at: string): DispatchEvent {
  const actor = { id: "alice", kind: "user" as const };
  return {
    actor,
    created_at: at,
    id: seq,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      author: actor,
      body: `Message ${seq}`,
      broadcast_id: null,
      reply_block: null,
      created_at: at,
      deliveries: [],
      id: `message-${seq}`,
      in_reply_to: null,
      issue_key: "CORE-1",
      target: null,
    },
    seq,
    type: "message.created",
  };
}

// The day a line falls on is its local calendar day, so the fixtures are built from local
// components rather than from UTC instants that could land on either side of midnight.
test("day dividers name Today and Yesterday and spell an older day in full", () => {
  const today = new Date(2026, 8, 10, 12, 0, 0);
  const yesterday = new Date(2026, 8, 9, 12, 0, 0);
  const older = new Date(2025, 0, 3, 12, 0, 0);
  const items = buildActivityItems({
    events: [
      message(1, older.toISOString()),
      message(2, yesterday.toISOString()),
      message(3, today.toISOString()),
    ],
    lastReadSeq: 3,
    today: dateKey(today.toISOString()),
  });

  expect(items.flatMap((item) => (item.kind === "day-divider" ? [item.label] : []))).toEqual([
    "Today",
    "Yesterday",
    // A day in another year carries that year; the two days above carry none.
    new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "long",
      weekday: "long",
      year: "numeric",
    }).format(older),
  ]);
});

const projectEvent = {
  actor: { id: "alice", kind: "user" },
  created_at: "2026-10-25T00:00:00Z",
  id: 1,
  issue_key: null,
  notify: false,
  project: "CORE",
  seq: 1,
} as const;

test("an architecture import names the repository it came from; a retained event without one reads as before", () => {
  const commit = "3be61df5be90aa11bb22";
  const error = "duplicate component id api";
  expect(
    activityDescription({
      ...projectEvent,
      payload: { branch: "main", commit, components: 3, repo: "acme/infra" },
      type: "architecture.synced",
    })
  ).toEqual(["synced the architecture model from acme/infra at 3be61df5be90"]);
  expect(
    activityDescription({
      ...projectEvent,
      payload: { branch: "main", error, repo: "acme/infra" },
      type: "architecture.sync_failed",
    })
  ).toEqual(["failed to sync the architecture model from acme/infra: duplicate component id api"]);
  expect(
    activityDescription({
      ...projectEvent,
      payload: { commit, components: 3 },
      type: "architecture.synced",
    })
  ).toEqual(["synced the architecture model at 3be61df5be90"]);
  expect(
    activityDescription({ ...projectEvent, payload: { error }, type: "architecture.sync_failed" })
  ).toEqual(["failed to sync the architecture model: duplicate component id api"]);
});

test("a project's default assignee names the person it moved to; a retained clear reads as cleared", () => {
  expect(
    activityDescription({
      ...projectEvent,
      payload: { assignee: "bob", project: "CORE", thread_logins: ["alice", "bob"] },
      type: "settings.project_default_assignee.updated",
    })
  ).toEqual(["set the default assignee of CORE to bob"]);
  // Written before every project named a default: the stored row of a clear, and no
  // `thread_logins`.
  expect(
    activityDescription({
      ...projectEvent,
      payload: { assignee: null, project: "CORE" },
      type: "settings.project_default_assignee.updated",
    })
  ).toEqual(["cleared the default assignee of CORE"]);
});

function commentDelivery(receipt: {
  state: SettledDeliveryState;
  duplicate?: boolean;
  error?: string;
}): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-25T00:00:00Z",
    id: 1,
    issue_key: "CORE-1",
    notify: false,
    payload: {
      attempt: 1,
      comment_id: "comment-1",
      delivery: "btw",
      reply_id: null,
      session_id: null,
      target: "session:planner",
      ...(receipt.state === "failed" ? { error: "no live session planner" } : {}),
      ...receipt,
    },
    seq: 1,
    type: "comment.delivery",
  };
}

test("a mention receipt says whether the mention reached its target", () => {
  expect(activityDescription(commentDelivery({ state: "sent" }))).toEqual([
    "delivered a comment mention to session:planner",
  ]);
  expect(activityDescription(commentDelivery({ state: "failed" }))).toEqual([
    "could not deliver a comment mention to session:planner",
  ]);
  // INFRA-626: the listener holds a queued mention for a session that is not running, which is
  // neither a delivery nor a refusal, and the line says so as every queued attempt does.
  expect(activityDescription(commentDelivery({ state: "queued" }))).toEqual([
    "queued a comment mention for session:planner, which is stopped; it gets the message if it runs again within 72 hours",
  ]);
  // A repeat the stream already held reached nobody new, which the line says rather than
  // counting it as a second delivery.
  expect(activityDescription(commentDelivery({ duplicate: true, state: "sent" }))).toEqual([
    "already delivered a comment mention to session:planner",
  ]);
  // A queued repeat still waits for a session that is not running, so it reads as queued rather
  // than as delivered.
  expect(activityDescription(commentDelivery({ duplicate: true, state: "queued" }))).toEqual([
    "queued a comment mention for session:planner, which is stopped; it gets the message if it runs again within 72 hours",
  ]);
});

// A delete appends one failed receipt per send of its post it ended, with its own words. That
// receipt is no failure to deliver: the line says the post's delete ended the mention, while any
// other failure still says the mention could not be delivered.
test("a mention receipt the post's delete appended says the delete ended the mention", () => {
  expect(
    activityDescription(commentDelivery({ error: DELETED_POST_DELIVERY_ERROR, state: "failed" }))
  ).toEqual(["the post's delete ended a comment mention to session:planner"]);
  expect(
    activityDescription(commentDelivery({ error: "no live session planner", state: "failed" }))
  ).toEqual(["could not deliver a comment mention to session:planner"]);
});

function messageDelivery(state: SettledDeliveryState, error?: string): DispatchEvent {
  return {
    actor: { id: "alice", kind: "user" },
    created_at: "2026-10-25T00:00:00Z",
    id: 2,
    issue_key: null,
    notify: false,
    payload: {
      attempt: 1,
      delivery: "steer",
      message_id: "message-1",
      session_id: "planner",
      state,
      target: "session:planner",
      title: "planner",
      ...(error === undefined ? {} : { error }),
    },
    seq: 2,
    type: "message.delivery",
  };
}

// A message the listener holds for a session that is not running has not reached that session.
test("a message receipt says a queued message waits for the stopped session", () => {
  expect(activityDescription(messageDelivery("sent"))).toEqual(["delivered a message"]);
  expect(activityDescription(messageDelivery("queued"))).toEqual([
    "queued a message for session:planner, which is stopped; it gets the message if it runs again within 72 hours",
  ]);
});

// A failed message receipt is no delivery: it reads as a mention's failure does, naming the
// target, and the one a delete appended for a send it ended says the delete ended it.
test("a failed message receipt says it was not delivered, or that the post's delete ended it", () => {
  expect(activityDescription(messageDelivery("failed", "no live session planner"))).toEqual([
    "could not deliver a message to session:planner",
  ]);
  expect(activityDescription(messageDelivery("failed", DELETED_POST_DELIVERY_ERROR))).toEqual([
    "the post's delete ended a message to session:planner",
  ]);
});

test("a recorded handoff names its phase and version", () => {
  expect(
    activityDescription({
      actor: { id: "session-1", kind: "session" },
      created_at: "2026-10-26T00:00:00Z",
      id: 2,
      issue_key: "CORE-1",
      notify: true,
      payload: { handoff_id: "handoff-1", phase: "implement", version: 2 },
      seq: 2,
      type: "handoff.recorded",
    })
  ).toEqual(["recorded the implement handoff, version 2"]);
});
