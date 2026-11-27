/**
 * The accept step both Envoy plugins take before a frame becomes a person's own turn
 * (INFRA-1026): what it asks Dispatch, in what order it records the attempt, and every frame it
 * leaves as the card it was.
 */
import { expect, test } from "bun:test";
import { IDEMPOTENCY_ANSWER_NOT_KEPT, IDEMPOTENCY_KEY_COMMITTED } from "@legion/contracts";
import { type DispatchDelivery, renderInbound } from "@legion/envoy-client/delivery";
import { type DispatchClient, DispatchWriteLandedError } from "@legion/envoy-client/dispatch-http";
import { acceptUserTurn, type UserTurnRefusal } from "./accept-user-turn";

const SESSION = "ses_pi";
const DIRECT = `notifications.agent.${SESSION}`;
const MESSAGE = "44444444-4444-4444-8444-444444444444";

/** One of a person's direct messages to this session, as the listener publishes it. */
function personSends(
  body: string,
  overrides: {
    readonly mode?: "aside" | "steer" | "btw";
    readonly attempt?: number;
    readonly issueKey?: string;
  } = {}
): string {
  const issueKey = overrides.issueKey ?? null;
  return JSON.stringify({
    dedupe_key: `agent.${SESSION}.${MESSAGE}:${overrides.mode ?? "steer"}`,
    event_id: "evt-send",
    issued_at: 1_760_000_000_000,
    payload: JSON.stringify({
      delivery: { attempt: overrides.attempt ?? 1, mode: overrides.mode ?? "steer" },
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: issueKey,
        payload: {
          author: { id: "alice", kind: "user" },
          body,
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: MESSAGE,
          in_reply_to: null,
          issue_key: issueKey,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
    }),
    payload_summary: body,
    source: "dispatch",
    source_event_id: "src-send",
    topic: DIRECT,
  });
}

/** A host as thin as the step allows: the record of handled attempts, the accept route's answer,
 *  and what it was asked, in the order it happened. */
function host(
  input: {
    readonly accept?: (id: string, attempt: number) => unknown;
    readonly dispatch?: () => Promise<DispatchClient | undefined>;
    readonly handledAlready?: readonly string[];
  } = {}
) {
  const handled = new Set<string>(input.handledAlready ?? []);
  /** What a host writing its record from the delivery would write (the Oh My Pi extension's
   *  transcript entry), so the step is pinned to hand over the attempt it is about to spend. */
  const entries: { readonly attempt: number; readonly message_id: string }[] = [];
  const order: string[] = [];
  const refusals: UserTurnRefusal[] = [];
  return {
    actorId: SESSION,
    dispatch:
      input.dispatch ??
      (async () =>
        ({
          acceptMessageDelivery: async (id: string, attempt: number) => {
            order.push(`accept:${id}#${attempt}`);
            if (input.accept === undefined) {
              return { body: "Ship it.", delivery: "steer", message_id: id };
            }
            return input.accept(id, attempt);
          },
        }) as unknown as DispatchClient),
    entries,
    handle: (key: string, delivery: DispatchDelivery) => {
      order.push(`handle:${key}`);
      entries.push({ attempt: delivery.attempt, message_id: delivery.id });
      handled.add(key);
    },
    handled: (key: string) => handled.has(key),
    order,
    recorded: handled,
    refusals,
    warn: (refusal: UserTurnRefusal) => {
      refusals.push(refusal);
      order.push(`warn:${refusal.reason}`);
    },
  };
}

// The attempt is recorded before Dispatch is asked: a frame forged inside the accept's minute
// finds the attempt spent, whatever the accept answers.
test("records the attempt before it asks Dispatch, and injects the stored body", async () => {
  const session = host();
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("The frame's own words"), SESSION, DIRECT),
    ...session,
  });

  expect(accepted?.turn).toEqual({ body: "Ship it.", messageId: MESSAGE, mode: "steer" });
  expect(accepted?.delivery.attempt).toBe(1);
  expect(session.order).toEqual([`handle:${MESSAGE}#1`, `accept:${MESSAGE}#1`]);
  expect(session.entries).toEqual([{ attempt: 1, message_id: MESSAGE }]);
  expect(session.refusals).toEqual([]);
});

// The route answers 404 for a transient database error too, so forgetting the attempt would let
// a later forged frame spend it: the record stands whatever the accept answers.
test("keeps the record when the accept fails, and the frame keeps its card", async () => {
  const session = host({
    accept: () => {
      throw new Error("404 MESSAGE_NOT_FOUND");
    },
  });
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("Ship it?"), SESSION, DIRECT),
    ...session,
  });

  expect(accepted).toBeUndefined();
  expect([...session.recorded]).toEqual([`${MESSAGE}#1`]);
  expect(session.order).toEqual([`handle:${MESSAGE}#1`, `accept:${MESSAGE}#1`, "warn:failed"]);
  const refusal = session.refusals[0];
  expect(refusal?.reason).toBe("failed");
  expect(refusal?.reason === "failed" ? String(refusal.error) : "").toContain(
    "404 MESSAGE_NOT_FOUND"
  );
});

test("an attempt this session already handled is never accepted twice", async () => {
  const session = host({ handledAlready: [`${MESSAGE}#1`] });
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("Ship it?"), SESSION, DIRECT),
    ...session,
  });

  expect(accepted).toBeUndefined();
  expect(session.order).toEqual(["warn:handled"]);
  expect(session.refusals.map((refusal) => refusal.reason)).toEqual(["handled"]);
});

// The accept's answer is JSON off the network: one without the stored body injects nothing, and
// the frame's own text is never the turn.
test("an accept answering without the stored body keeps the card", async () => {
  const session = host({ accept: () => ({ message_id: MESSAGE }) });
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("Ship it?"), SESSION, DIRECT),
    ...session,
  });

  expect(accepted).toBeUndefined();
  expect(session.order).toEqual([`handle:${MESSAGE}#1`, `accept:${MESSAGE}#1`, "warn:no-body"]);
});

// An accept Dispatch recorded and gave no copy of its answer for is no failure: the accept was
// made, and only the stored body a turn is made of is missing. Its reason says so, so the host
// says the accept was recorded and why no turn follows, never that Dispatch did not accept it.
test("an accept that landed with no answer keeps the card under its own reason", async () => {
  for (const landed of [
    new DispatchWriteLandedError(IDEMPOTENCY_KEY_COMMITTED, 409, "POST", "https://dispatch.test"),
    new DispatchWriteLandedError(IDEMPOTENCY_ANSWER_NOT_KEPT, 200, "POST", "https://dispatch.test"),
  ]) {
    const session = host({
      accept: () => {
        throw landed;
      },
    });
    const accepted = await acceptUserTurn({
      rendered: renderInbound(personSends("Ship it?"), SESSION, DIRECT),
      ...session,
    });

    expect(accepted).toBeUndefined();
    expect(session.order).toEqual([`handle:${MESSAGE}#1`, `accept:${MESSAGE}#1`, "warn:landed"]);
    const refusal = session.refusals[0];
    expect(refusal?.reason === "landed" ? refusal.error : undefined).toBe(landed);
  }
});

// A BTW is a side turn, never the session's own, so no attempt of one is ever spent.
test("a frame no person's direct Send or Aside could be is never recorded or accepted", async () => {
  const session = host();
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("Ship it?", { mode: "btw" }), SESSION, DIRECT),
    ...session,
  });

  expect(accepted).toBeUndefined();
  expect(session.order).toEqual([]);
  expect([...session.recorded]).toEqual([]);
});

// An issue message's flow replies on its issue, so it keeps its envelope: no attempt of one is
// spent, and Dispatch is never asked to accept it. It is the one kind of message its author can
// edit or delete, so no turn this step makes is ever the text of a post its author withdrew.
test("a person's message on an issue is never recorded or accepted, and keeps its card", async () => {
  const session = host();
  const rendered = renderInbound(personSends("Ship it?", { issueKey: "INFRA-7" }), SESSION, DIRECT);
  // The frame is the delivery a direct Send is, a person's steer, in all but its issue key.
  expect(rendered.delivery).toMatchObject({
    resource: "message",
    id: MESSAGE,
    attempt: 1,
    mode: "steer",
    issueKey: "INFRA-7",
  });
  expect(rendered.dispatchActor?.kind).toBe("user");

  const accepted = await acceptUserTurn({ rendered, ...session });

  expect(accepted).toBeUndefined();
  expect(session.order).toEqual([]);
  expect([...session.recorded]).toEqual([]);
  expect(session.refusals).toEqual([]);
});

// Dispatch not being configured is no refusal: there is nothing to accept with. The attempt is
// recorded all the same, since this session has handled it as a card.
test("a session with no Dispatch records the attempt and keeps the card without a word", async () => {
  const session = host({ dispatch: async () => undefined });
  const accepted = await acceptUserTurn({
    rendered: renderInbound(personSends("Ship it?"), SESSION, DIRECT),
    ...session,
  });

  expect(accepted).toBeUndefined();
  expect(session.order).toEqual([`handle:${MESSAGE}#1`]);
  expect(session.refusals).toEqual([]);
});
