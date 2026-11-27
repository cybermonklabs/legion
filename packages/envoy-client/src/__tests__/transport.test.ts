import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  type Envelope,
  EnvelopeSchema,
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  MESSAGE_NOT_SENT_PREFIX,
  MINTED_DEDUPE_KEY_PATTERN,
} from "@legion/contracts";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import envelopeWithoutRole from "../../../contracts/fixtures/envelope-without-role.json";
import noInterestsFixture from "../../../contracts/fixtures/envoy-session-no-interests.json";
import { delivered } from "../../test/delivered";
import { consumerDelivery } from "../../test/subscribe-answer";
import { bearerCredential, type RequestCredential } from "../bearer";
import { createConsumerDelivery, RelayHeader, type RelayReply } from "../consumer-delivery";
import { DedupeIdentitySchema, renderInbound } from "../delivery";
import { createDeliveryLedger, type FailureNotice } from "../delivery-ledger";
import { subscriptionRemovedTopics } from "../dispatch-subscribe";
import { machineID } from "../machine";
import type { TmuxPlace } from "../tmux-place";
import {
  publishConfirmationText,
  publishResultDetails,
  sendConfirmationText,
} from "../tool-contract";
import {
  createEnvoyClient,
  type DeliveryAnswer,
  EnvoyApiError,
  EnvoySendUnconfirmedError,
  type KeyTakenResult,
  mayActOnRoleRecord,
  newClientInstanceID,
  parseSubscribeAnswer,
  type RoleInfo,
  roleHeldBy,
} from "../transport";

/** The grace the unresponsive rule names, in minutes, from the constant the Dispatch server
 *  refuses a claim take by: a change of that constant must change the rule's text, and these
 *  expectations with it, rather than leaving the text saying a number nothing enforces. */
const GRACE_MINUTES = ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS / 60_000;

type RecordedFetch = {
  readonly fetch: (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
  readonly requests: Request[];
};

function recordFetch(responses: readonly Response[]): RecordedFetch {
  const requests: Request[] = [];
  let nextResponse = 0;

  return {
    fetch: async (input, init) => {
      requests.push(new Request(input, init));
      const response = responses[nextResponse];
      nextResponse += 1;
      if (!response) throw new Error("test fetch received an unexpected request");
      return response;
    },
    requests,
  };
}

const PLUGIN = { name: "@sjawhar/pi-envoy", version: "11.0.0" };

function jsonResponse(body: unknown): Response {
  return Response.json(body);
}

describe("EnvoyClient", () => {
  // The answer every stand-in listener here gives a registration, built the one way they all
  // build it (`consumerDelivery`). The malformed answers below, which no listener of this
  // build writes, are the ones this file builds by hand.
  const CONSUMER: DeliveryAnswer = consumerDelivery("ses_sender");
  const DELIVERY_REQUEST = {
    consumer: true,
    epoch: "epoch-1",
    resume: { stream_id: "stream-1", seq: 41 },
  } as const;

  test("posts a subscription using the listener request shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
        delivery: CONSUMER,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://127.0.0.1:39121/", fetch: recorded.fetch });

    const interest = await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      delivery: DELIVERY_REQUEST,
      clientInstanceID: "0f8c2b1a4d6e7f90",
      plugin: PLUGIN,
    });

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: undefined,
      delivery: CONSUMER,
    });
    expect(recorded.requests).toHaveLength(1);
    expect(recorded.requests[0]?.method).toBe("POST");
    expect(recorded.requests[0]?.url).toBe("http://127.0.0.1:39121/v1/interests/subscribe");
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      delivery: DELIVERY_REQUEST,
      client_instance_id: "0f8c2b1a4d6e7f90",
      host: machineID(),
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("a subscription without a client instance id carries no client_instance_id", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
        delivery: CONSUMER,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });

    const body = (await recorded.requests[0]?.json()) as Record<string, unknown>;
    // The key is absent, not undefined, so an older listener sees an unchanged body.
    expect(Object.hasOwn(body, "client_instance_id")).toBe(false);
    expect(body).toEqual({
      session_id: "ses_sender",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 13381,
      title: "Envoy QA",
      driving: true,
      delivery: DELIVERY_REQUEST,
      host: machineID(),
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  test("a subscription carries the session's tmux place, and no key when it has none", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      // A registration answered no consumer fails here, so the place these cases read rides a
      // registration that stood.
      delivery: CONSUMER,
    };
    const recorded = recordFetch([jsonResponse(interest), jsonResponse(interest)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const register = (place?: TmuxPlace) =>
      client.subscribe({
        sessionID: "ses_sender",
        directory: "/work",
        topics: ["notifications.agent.ses_sender"],
        port: 0,
        title: "Envoy QA",
        driving: true,
        delivery: DELIVERY_REQUEST,
        plugin: PLUGIN,
        ...(place === undefined ? {} : { place }),
      });

    const place: TmuxPlace = {
      socket: "/tmp/tmux-1000/default",
      server_pid: "4242",
      session_name: "legion",
      window_id: "@7",
      window_index: 3,
      window_name: "worker",
      pane_id: "%3",
      pane_index: 1,
      layout: "b2f1,208x52,0,0,5",
      zoomed: false,
    };
    await register(place);
    await register();

    expect(await recorded.requests[0]?.json()).toMatchObject({ tmux_place: place });
    const outside = (await recorded.requests[1]?.json()) as Record<string, unknown>;
    // The key is absent, not undefined: a registration that reports no place must not read as
    // one reporting an empty place.
    expect(Object.hasOwn(outside, "tmux_place")).toBe(false);
  });

  test("asks for consumer delivery and reads the consumer answer", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };
    const recorded = recordFetch([jsonResponse({ ...interest, delivery: CONSUMER })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: ["notifications.agent.ses_sender"],
      port: 0,
      title: "",
      driving: false,
      selfSubscribed: true,
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });

    expect(result.delivery).toEqual(CONSUMER);
    // The registration carries the plugin beside the delivery request.
    expect(await recorded.requests[0]?.json()).toMatchObject({
      self_subscribed: true,
      delivery: DELIVERY_REQUEST,
      plugin: "@sjawhar/pi-envoy",
      plugin_version: "11.0.0",
    });
  });

  // A session receives only what its own consumer holds, so an answer naming no consumer fails
  // the registration here rather than leaving the session registered and deaf.
  test("fails a registration whose answer names no consumer, with the listener's reason", async () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };
    const recorded = recordFetch([
      jsonResponse(interest),
      jsonResponse({ ...interest, delivery: { mode: "core", reason: "no_grant" } }),
      jsonResponse({ ...interest, delivery: { mode: "push" } }),
      jsonResponse({ ...interest, delivery: 7 }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const register = () =>
      client.subscribe({
        sessionID: "ses_sender",
        directory: "/work",
        topics: ["notifications.agent.ses_sender"],
        port: 0,
        title: "",
        driving: false,
        selfSubscribed: true,
        delivery: DELIVERY_REQUEST,
        plugin: PLUGIN,
      });

    // A listener of an earlier release answers no `delivery` field at all.
    await expect(register()).rejects.toThrow(/answered no delivery for ses_sender/);
    await expect(register()).rejects.toThrow(/answered delivery mode core \(no_grant\)/);
    await expect(register()).rejects.toThrow(/answered delivery mode push/);
    await expect(register()).rejects.toThrow(/cannot read/);
  });

  test("parseSubscribeAnswer throws on an absent, core or unreadable delivery answer", () => {
    const interest = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
    };

    expect(parseSubscribeAnswer({ ...interest, delivery: CONSUMER })).toEqual({
      ...interest,
      updated_at: undefined,
      delivery: CONSUMER,
    });
    // A listener of an earlier release answers no `consumer_start`, as CONSUMER above does, and
    // one that carries it is read with it: the session keeps it as the floor of its resume point.
    expect(
      parseSubscribeAnswer({ ...interest, delivery: { ...CONSUMER, consumer_start: 42 } })
    ).toEqual({
      ...interest,
      updated_at: undefined,
      delivery: { ...CONSUMER, consumer_start: 42 },
    });
    expect(() => parseSubscribeAnswer(interest)).toThrow(
      "the listener answered no delivery for ses_sender: it predates session consumers, and a session receives only what its own consumer holds"
    );
    expect(() => parseSubscribeAnswer({ ...interest, delivery: null })).toThrow(
      /answered no delivery/
    );
    expect(() =>
      parseSubscribeAnswer({ ...interest, delivery: { mode: "core", reason: "not_requested" } })
    ).toThrow(
      "the listener gave ses_sender no consumer: it answered delivery mode core (not_requested), and a session receives only what its own consumer holds"
    );
    expect(() => parseSubscribeAnswer({ ...interest, delivery: "consumer" })).toThrow(
      "the listener gave ses_sender no consumer: it answered a delivery field this build cannot read, and a session receives only what its own consumer holds"
    );
    // A consumer answer missing the subject the relay arrives on is unreadable too.
    expect(() =>
      parseSubscribeAnswer({
        ...interest,
        delivery: { mode: "consumer", stream_id: "s", gap: false },
      })
    ).toThrow(/answered delivery mode consumer/);
  });

  test("expands a concrete trailing wildcard for subscribe and unsubscribe", async () => {
    const wildcard = "notifications.github.example-org.example-repo.pr.42.>";
    const base = "notifications.github.example-org.example-repo.pr.42";
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: [base, wildcard],
        delivery: CONSUMER,
      }),
      new Response("ok"),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: [wildcard],
      port: 0,
      title: "",
      driving: true,
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });
    await client.unsubscribe({ sessionID: "ses_sender", topics: [wildcard] });

    expect(await recorded.requests[0]?.json()).toMatchObject({ topics: [base, wildcard] });
    expect(await recorded.requests[1]?.json()).toEqual({
      session_id: "ses_sender",
      topics: [base, wildcard],
    });
  });

  test("does not expand a trailing wildcard whose base contains a wildcard", async () => {
    const wildcard = "notifications.github.example-org.example-repo.pr.*.>";
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: [wildcard],
        delivery: CONSUMER,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.subscribe({
      sessionID: "ses_sender",
      directory: "/work",
      topics: [wildcard],
      port: 0,
      title: "",
      driving: true,
      delivery: DELIVERY_REQUEST,
      plugin: PLUGIN,
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({ topics: [wildcard] });
  });
  test("rejects a wildcard whose concrete base has empty segments", async () => {
    const recorded = recordFetch([]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    for (const topic of ["a..b.>", "a.>"]) {
      await expect(
        client.subscribe({
          sessionID: "ses_sender",
          directory: "/work",
          topics: [topic],
          port: 0,
          title: "",
          driving: true,
          delivery: DELIVERY_REQUEST,
          plugin: PLUGIN,
        })
      ).rejects.toThrow("concrete base");
    }

    expect(recorded.requests).toEqual([]);
  });

  test("posts an unsubscribe request with an empty topic list", async () => {
    const recorded = recordFetch([new Response("ok")]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unsubscribe({ sessionID: "ses_sender", topics: [] });

    expect(recorded.requests[0]?.url).toBe("http://listener/v1/interests/unsubscribe");
    expect(await recorded.requests[0]?.json()).toEqual({ session_id: "ses_sender", topics: [] });
  });

  test("an unsubscribe names the releasing process, and omits the key without one", async () => {
    const recorded = recordFetch([new Response("ok"), new Response("ok")]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unsubscribe({
      sessionID: "ses_sender",
      topics: ["notifications.agent.ses_sender"],
      clientInstanceID: "0f8c2b1a4d6e7f90",
    });
    await client.unsubscribe({
      sessionID: "ses_sender",
      topics: ["notifications.agent.ses_sender"],
    });

    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      topics: ["notifications.agent.ses_sender"],
      client_instance_id: "0f8c2b1a4d6e7f90",
    });
    const unnamed = (await recorded.requests[1]?.json()) as Record<string, unknown>;
    // The key is absent, not undefined: a release naming no process frees no claim, and an
    // older listener sees an unchanged body.
    expect(Object.hasOwn(unnamed, "client_instance_id")).toBe(false);
    expect(unnamed).toEqual({
      session_id: "ses_sender",
      topics: ["notifications.agent.ses_sender"],
    });
  });

  test("gets one session interest using the listener response shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.agent.ses_sender"],
        updated_at: 42,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const interest = await client.getInterest("ses_sender");

    expect(interest).toEqual({
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      topics: ["notifications.agent.ses_sender"],
      updated_at: 42,
    });
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/interests/ses_sender");
  });

  test("sends a failed request once more by default, and only once with retry false", async () => {
    const down = () => new Response("down", { status: 503 });
    for (const [retry, sent] of [
      [undefined, 2],
      [false, 1],
    ] as const) {
      const recorded = recordFetch([down(), down()]);
      const client = createEnvoyClient({
        baseUrl: "http://listener",
        fetch: recorded.fetch,
        ...(retry === undefined ? {} : { retry }),
      });
      await expect(client.getInterest("ses_sender")).rejects.toThrow("failed with 503");
      expect([retry, recorded.requests.length]).toEqual([retry, sent]);
    }
  });

  // A 408, a 429 and any 5xx are an outage (`@legion/contracts/http-status`), and a listener call
  // sends each once more but the 429: the listener's 429 is `sender_rate_limited`, whose sentence
  // the agent reads to decide when to send again.
  test("a listener call answered with an outage is sent once more, but a 429 is one request", async () => {
    const sent: Record<number, number> = {};
    for (const status of [408, 429, 500, 501, 503]) {
      const recorded = recordFetch(
        Array.from({ length: 2 }, () => Response.json({ error: `answered ${status}` }, { status }))
      );
      const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
      await expect(client.getInterest("ses_sender")).rejects.toThrow(`answered ${status}`);
      sent[status] = recorded.requests.length;
    }
    expect(sent).toEqual({ 408: 2, 429: 1, 500: 2, 501: 2, 503: 2 });
  });

  test("sends a direct message and parses the listener envelope", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-1",
        source: "agent",
        source_event_id: "agent.ses_sender.event-1",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-1",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-1",
        recipient: "ses_target",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_target",
        message: "hello",
      })
    );

    expect(result.envelope.topic).toBe("notifications.agent.ses_target");
    expect(result.recipient).toBe("ses_target");
    expect(result.confirmed).toBe(true);

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "agent",
      source_session: "ses_sender",
      target_session: "ses_target",
      message: "hello",
      idempotency_key: expect.any(String),
    });
  });

  test("returns the requested recipient when a legacy listener omits it", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-legacy",
        source: "agent",
        source_event_id: "agent.ses_sender.event-legacy",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-legacy",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-legacy",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(result).toMatchObject({
      envelope: { event_id: "event-legacy" },
      recipient: "ses_target",
      confirmed: false,
    });
  });

  // The listener answers a repeat of a key another sender's message holds with that message's ids
  // and times alone, its text fields empty (`requestSender.answer`), on a send, a publish's 200
  // and a role publish's 202 kept. Each reads as a key-taken result, not delivered, naming that
  // message's event id. The control is the envelope schema every answer was read by before,
  // which throws on that body; and a whole duplicate answer still reads as a delivered result.
  test("a send or a publish whose key another sender's message holds reads as key taken, not as a delivery", async () => {
    const taken = {
      event_id: "event-alice",
      source: "",
      source_event_id: "",
      topic: "notifications.agent.ses_target",
      dedupe_key: "agent.ses_target.key",
      issued_at: 7,
      payload_summary: "",
      trace_id: "",
      duplicate: true,
    };
    expect(() => EnvelopeSchema.parse(taken)).toThrow();
    const whole = {
      ...taken,
      source: "agent",
      source_event_id: "agent.ses_sender.event-alice",
      payload_summary: "hello",
      trace_id: "trace-alice",
    };
    const recorded = recordFetch([
      jsonResponse({
        ...taken,
        recipient: "ses_target",
        recipient_state: "live",
        state: "connected",
      }),
      jsonResponse(taken),
      new Response(
        JSON.stringify({ ...taken, kept: true, kept_reason: "unclaimed", kept_until: 9 }),
        { status: 202, headers: { "Content-Type": "application/json" } }
      ),
      jsonResponse({ ...whole, recipient: "ses_target" }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const keyTaken: KeyTakenResult = {
      keyTaken: { eventId: "event-alice", topic: "notifications.agent.ses_target", issuedAt: 7 },
      delivered: false,
    };

    expect(
      await client.send({
        sourceSessionID: "ses_bob",
        targetSessionID: "ses_target",
        message: "hi",
      })
    ).toEqual(keyTaken);
    expect(
      await client.publish({
        sourceSessionID: "ses_bob",
        topic: "notifications.team.x",
        message: "hi",
      })
    ).toEqual(keyTaken);
    expect(
      await client.publish({
        sourceSessionID: "ses_bob",
        topic: "notifications.role.r",
        message: "hi",
      })
    ).toEqual(keyTaken);
    const repeat = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_target",
        message: "hi",
      })
    );
    expect(repeat).toMatchObject({ duplicate: true, envelope: { event_id: "event-alice" } });
  });

  // The listener gives the key-taken answer only to a request that names it (`Envoy-Reads:
  // key-taken`, listenerwire.ReadsHeader), and any other the envelope it built for the repeat. A
  // send or a publish that names its source session names it; one that names none does not, since
  // the listener cannot tell its own retry from another sessionless sender's request. The control
  // is the sessionless call, which carries no such header.
  test("a send or a publish that names its session says it reads the key-taken answer, and one that names none does not", async () => {
    const whole = {
      event_id: "event-own",
      source: "agent",
      source_event_id: "agent.ses_sender.event-own",
      topic: "notifications.agent.ses_target",
      dedupe_key: "agent.ses_target.key",
      issued_at: 7,
      payload_summary: "hello",
      trace_id: "trace-own",
      recipient: "ses_target",
    };
    const recorded = recordFetch([
      jsonResponse(whole),
      jsonResponse(whole),
      jsonResponse(whole),
      jsonResponse(whole),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hi",
    });
    await client.publish({
      sourceSessionID: "ses_sender",
      topic: "notifications.team.x",
      message: "hi",
    });
    await client.send({ sourceSessionID: "", targetSessionID: "ses_target", message: "hi" });
    await client.publish({ sourceSessionID: "", topic: "notifications.team.x", message: "hi" });

    expect(recorded.requests.map((request) => request.headers.get("Envoy-Reads"))).toEqual([
      "key-taken",
      "key-taken",
      null,
      null,
    ]);
  });

  test("a send reads the target's state", async () => {
    const envelope = {
      event_id: "event-stopped",
      source: "agent",
      source_event_id: "agent.ses_sender.event-stopped",
      source_session: "ses_sender",
      topic: "notifications.agent.ses_target",
      dedupe_key: "agent.ses_target.event-stopped",
      issued_at: 1,
      payload_summary: "hello",
      trace_id: "trace-stopped",
      recipient: "ses_target",
    };
    const recorded = recordFetch([
      // A session with no entry but a record: `recipient_state` keeps the registry's three
      // values, and `state` beside it says the target is stopped.
      jsonResponse({
        ...envelope,
        recipient_state: "gone",
        state: "stopped",
        last_seen: 1_762_075_200_000,
      }),
      jsonResponse({ ...envelope, recipient_state: "live", state: "connected" }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const stopped = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_target",
        message: "hello",
      })
    );
    const connected = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_target",
        message: "hello",
      })
    );

    expect(stopped).toMatchObject({
      recipientState: "gone",
      state: "stopped",
      lastSeen: 1_762_075_200_000,
    });
    expect(connected.recipientState).toBe("live");
    expect(connected.state).toBe("connected");
    expect(connected.lastSeen).toBeUndefined();
  });

  test("a send a listener of an earlier release answers no state for reads as connected", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-stateless",
        source: "agent",
        source_event_id: "agent.ses_sender.event-stateless",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.event-stateless",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-stateless",
        recipient: "ses_target",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_target",
        message: "hello",
      })
    );

    expect(result.state).toBe("connected");
    expect(result.lastSeen).toBeUndefined();
  });

  test("a publish carries the listener's recipient counts, a count of none included", async () => {
    const envelope = {
      event_id: "event-counted",
      source: "agent",
      source_event_id: "agent.ses_sender.event-counted",
      source_session: "ses_sender",
      topic: "notifications.team.deploy-squad.all",
      dedupe_key: "agent.team.event-counted",
      issued_at: 2,
      payload_summary: "broadcast",
      trace_id: "trace-counted",
    };
    const recorded = recordFetch([
      jsonResponse({ ...envelope, recipients: 3, recipients_connected: 2 }),
      jsonResponse({ ...envelope, recipients: 0, recipients_connected: 0 }),
      jsonResponse(envelope),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const publish = async () =>
      delivered(
        await client.publish({
          sourceSessionID: "ses_sender",
          topic: "notifications.team.deploy-squad.all",
          message: "broadcast",
        })
      );

    expect(await publish()).toMatchObject({ recipients: 3, recipientsConnected: 2 });
    // Zero is a count the listener sends, not an absence: it is the answer that says the message
    // reached nobody.
    expect(await publish()).toMatchObject({ recipients: 0, recipientsConnected: 0 });
    const uncounted = await publish();
    expect(uncounted.recipients).toBeUndefined();
    expect(uncounted.recipientsConnected).toBeUndefined();
  });

  test("sends a human direct message without a source session", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-human",
        source: "human",
        source_event_id: "human.event-1",
        topic: "notifications.agent.ses_target",
        dedupe_key: "agent.ses_target.human-event-1",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-human",
        recipient: "ses_target",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.send({
      source: "human",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "human",
      target_session: "ses_target",
      message: "hello",
      idempotency_key: expect.any(String),
    });
  });

  test("omits an empty source session from a send and a publish", async () => {
    // A caller with no session of its own (a task subagent in a host that never registered
    // one) must send no `source_session` at all: the listener's own JSON drops an empty one on
    // the way out, leaving the recipient a message with no sender and no reply hint.
    const envelope = {
      event_id: "event-sourceless",
      source: "agent",
      source_event_id: "agent.event-sourceless",
      topic: "notifications.agent.ses_target",
      dedupe_key: "agent.ses_target.event-sourceless",
      issued_at: 1,
      payload_summary: "hello",
      trace_id: "trace-sourceless",
    };
    const recorded = recordFetch([jsonResponse(envelope), jsonResponse(envelope)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.send({ sourceSessionID: "", targetSessionID: "ses_target", message: "hello" });
    await client.publish({
      sourceSessionID: "",
      topic: "notifications.role.controller",
      message: "broadcast",
    });

    expect(await recorded.requests[0]?.json()).not.toHaveProperty("source_session");
    expect(await recorded.requests[1]?.json()).not.toHaveProperty("source_session");
  });

  test("publishes a broadcast using the listener request shape", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-2",
        source: "agent",
        source_event_id: "agent.ses_sender.event-2",
        source_session: "ses_sender",
        topic: "notifications.role.controller",
        dedupe_key: "agent.controller.event-2",
        issued_at: 2,
        payload_summary: "broadcast",
        trace_id: "trace-2",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = delivered(
      await client.publish({
        sourceSessionID: "ses_sender",
        topic: "notifications.role.controller",
        message: "broadcast",
        payload: `{"kind":"role-event"}`,
      })
    );
    expect(result).toMatchObject({ envelope: { event_id: "event-2" } });
    expect(result.holder).toBeUndefined();

    expect(await recorded.requests[0]?.json()).toMatchObject({
      source: "agent",
      source_session: "ses_sender",
      topic: "notifications.role.controller",
      message: "broadcast",
      payload: `{"kind":"role-event"}`,
      idempotency_key: expect.any(String),
    });
  });

  // A role publish names the holder the listener resolved and the state its record put that
  // holder in, under the two names every role answer uses; a publish to a plain topic names
  // neither, since the stream stores one copy each subscribed session's own consumer delivers
  // (`listenerwire.PublishResponse`).
  test("a role publish names its holder and that holder's state, and a topic publish names neither", async () => {
    const envelope = {
      event_id: "event-stored",
      source: "agent",
      source_event_id: "agent.ses_sender.event-stored",
      source_session: "ses_sender",
      topic: "notifications.team.deploy-squad.all",
      dedupe_key: "agent.team.event-stored",
      issued_at: 3,
      payload_summary: "broadcast",
      trace_id: "trace-3",
    };
    const recorded = recordFetch([
      jsonResponse({
        ...envelope,
        topic: "notifications.role.controller",
        holder: "ses_holder",
        holder_state: "live",
      }),
      jsonResponse(envelope),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const publish = async (topic: string) =>
      delivered(
        await client.publish({ sourceSessionID: "ses_sender", topic, message: "broadcast" })
      );

    const role = await publish("notifications.role.controller");
    const taken = await publish("notifications.team.deploy-squad.all");

    expect(role.holder).toBe("ses_holder");
    expect(role.holderState).toBe("live");
    expect(taken.holder).toBeUndefined();
    expect(taken.holderState).toBeUndefined();
  });

  test("reads a 202 kept answer as a published message the listener holds for the role's next holder", async () => {
    // No live session holds the role, so the listener kept the message on the role's kept lane
    // for one hour and answered 202. That is a success, not a refusal: the message reaches the
    // next session that holds the role live before kept_until, and `request`'s `response.ok`
    // test lets the body through. A holder that is not live is named with its state and its last
    // registration; a last registration of 0 is one the listener does not know.
    const keptBody = (role: string, fields: Record<string, unknown>) =>
      new Response(
        JSON.stringify({
          event_id: `event-${role}`,
          source: "agent",
          source_event_id: `agent.ses_sender.event-${role}`,
          source_session: "ses_sender",
          topic: `notifications.role.${role}`,
          dedupe_key: `agent.${role}.event-kept`,
          issued_at: 3,
          payload_summary: "the worker is stalled",
          trace_id: "trace-kept",
          kept: true,
          ...fields,
        }),
        { status: 202, headers: { "Content-Type": "application/json" } }
      );
    const recorded = recordFetch([
      keptBody("legion-architect", { kept_reason: "unclaimed", kept_until: 1_793_003_600_000 }),
      keptBody("reviewer", {
        kept_reason: "holder_lapsed",
        kept_until: 1_793_003_600_000,
        holder: "ses_reviewer",
        holder_state: "gone",
        last_seen: 0,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    const publish = async (role: string) =>
      delivered(
        await client.publish({
          sourceSessionID: "ses_sender",
          topic: `notifications.role.${role}`,
          message: "the worker is stalled",
        })
      );

    const result = await publish("legion-architect");
    expect(result.kept).toBe(true);
    expect(result.keptReason).toBe("unclaimed");
    expect(result.keptUntil).toBe(1_793_003_600_000);
    expect(result.envelope.event_id).toBe("event-legion-architect");

    const lapsed = await publish("reviewer");
    expect(lapsed).toMatchObject({
      kept: true,
      keptReason: "holder_lapsed",
      keptUntil: 1_793_003_600_000,
      holder: "ses_reviewer",
      holderState: "gone",
    });
    expect(lapsed).not.toHaveProperty("lastSeen");
  });

  test("sends keep on the wire only when the caller asked for it, and still throws the listener's 404", async () => {
    const refused = () =>
      new Response(
        JSON.stringify({ error: "no holder for role legion-architect", reason: "unclaimed" }),
        {
          status: 404,
          headers: { "Content-Type": "application/json" },
        }
      );
    const accepted = jsonResponse({
      event_id: "event-delivered",
      source: "agent",
      source_event_id: "agent.ses_sender.event-delivered",
      source_session: "ses_sender",
      topic: "notifications.role.legion-architect",
      dedupe_key: "agent.legion-architect.event-delivered",
      issued_at: 4,
      payload_summary: "ping",
      trace_id: "trace-delivered",
      holder: "ses_architect",
    });
    const recorded = recordFetch([accepted, refused()]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    // An unset `keep` sends no field at all, so an older listener sees an unchanged request.
    const plain = delivered(
      await client.publish({
        sourceSessionID: "ses_sender",
        topic: "notifications.role.legion-architect",
        message: "ping",
      })
    );
    expect(plain.kept).toBeUndefined();
    expect(plain.keptReason).toBeUndefined();
    expect(await recorded.requests[0]?.json()).not.toHaveProperty("keep");

    // A sender with a queue of its own asks for no keep, and gets today's 404 as an error.
    await expect(
      client.publish({
        sourceSessionID: "ses_sender",
        topic: "notifications.role.legion-architect",
        message: "ping",
        keep: false,
      })
    ).rejects.toThrow("no holder for role legion-architect");
    expect(await recorded.requests[1]?.json()).toMatchObject({ keep: false });
  });

  test("deletes a session registration", async () => {
    const recorded = recordFetch([new Response(null, { status: 200 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unregisterSession({ sessionID: "ses_closed" });

    expect(recorded.requests).toHaveLength(1);
    expect(recorded.requests[0]?.method).toBe("DELETE");
    // No client instance id, no query string: the request an older client sent.
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions/ses_closed");
  });

  test("a deregistration with a client instance id sends it as an encoded query parameter", async () => {
    const recorded = recordFetch([
      Response.json({ deleted: true }, { status: 200 }),
      Response.json(
        { deleted: false, reason: "the registration belongs to another client instance" },
        { status: 200 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.unregisterSession({ sessionID: "ses_closed", clientInstanceID: "0f8c/2b 1a" });
    // A refusal is a 200 whose body says the entry stayed; no caller acts on it.
    await client.unregisterSession({ sessionID: "ses_closed", clientInstanceID: "0f8c2b1a" });

    expect(recorded.requests[0]?.method).toBe("DELETE");
    expect(recorded.requests[0]?.url).toBe(
      "http://listener/v1/sessions/ses_closed?client_instance_id=0f8c%2F2b%201a"
    );
    expect(recorded.requests[1]?.url).toBe(
      "http://listener/v1/sessions/ses_closed?client_instance_id=0f8c2b1a"
    );
  });

  test("sets a role and returns the listener interest response", async () => {
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_sender",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.role.controller"],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.setRole({ sessionID: "ses_sender", role: "controller" });

    // The answer carries no warnings, as no answer of a listener predating the field does: [].
    expect(result).toEqual({
      claimed: true,
      interest: expect.objectContaining({ topics: ["notifications.role.controller"] }),
      warnings: [],
    });
    // A hard claim sends no `soft` field, so an older listener sees an
    // unchanged request.
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      role: "controller",
    });
  });

  test("a granted claim answers the listener's warnings beside the interest, never inside it", async () => {
    const handover =
      "no message of ses_old is handed over to this session: the claim's credential and ses_old's last registration name different principals";
    const edge =
      "this edge listener hands over no messages: the unread messages of ses_old stay on its own subject, and a claim through a shared listener hands them to this session";
    const recorded = recordFetch([
      jsonResponse({
        session_id: "ses_new",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.role.controller"],
        warnings: [handover, edge],
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.setRole({
      sessionID: "ses_new",
      role: "controller",
      previousSessionID: "ses_old",
    });

    expect(result).toEqual({
      claimed: true,
      interest: {
        session_id: "ses_new",
        machine_id: "host-a",
        dir: "/work",
        topics: ["notifications.role.controller"],
      },
      warnings: [handover, edge],
    });
  });

  test("a soft claim sends soft:true and maps the listener's 409 to a not-claimed result", async () => {
    const recorded = recordFetch([
      Response.json(
        { error: "role controller is held by ses_live", role: "controller", holder: "ses_live" },
        { status: 409 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.setRole({
      sessionID: "ses_resumer",
      role: "controller",
      soft: true,
    });

    expect(result).toEqual({ claimed: false, holder: "ses_live" });
    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_resumer",
      role: "controller",
      soft: true,
    });
  });

  test("previous_session_id is sent with a soft claim and a hard one, and only when it names a session", async () => {
    const interest = { session_id: "ses_child", machine_id: "host-a", dir: "/work", topics: [] };
    const recorded = recordFetch([
      jsonResponse(interest),
      jsonResponse(interest),
      jsonResponse(interest),
      jsonResponse(interest),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.setRole({
      sessionID: "ses_child",
      role: "controller",
      soft: true,
      previousSessionID: "ses_parent",
    });
    await client.setRole({
      sessionID: "ses_child",
      role: "controller",
      previousSessionID: "ses_parent",
    });
    await client.setRole({ sessionID: "ses_child", role: "controller" });
    await client.setRole({ sessionID: "ses_child", role: "controller", previousSessionID: "" });

    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
      soft: true,
      previous_session_id: "ses_parent",
    });
    // A hard claim names the session it replaces too, whose unread messages the listener hands
    // to the claimant; a listener older than that reads it on a soft claim alone.
    expect(await recorded.requests[1]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
      previous_session_id: "ses_parent",
    });
    // No predecessor, or an empty one, leaves the key off the wire.
    expect(await recorded.requests[2]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
    });
    expect(await recorded.requests[3]?.json()).toEqual({
      session_id: "ses_child",
      role: "controller",
    });
  });

  test("a claim names the process making it, and omits the key without one", async () => {
    const interest = { session_id: "ses_sender", machine_id: "host-a", dir: "/work", topics: [] };
    const recorded = recordFetch([jsonResponse(interest), jsonResponse(interest)]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await client.setRole({
      sessionID: "ses_sender",
      role: "controller",
      soft: true,
      clientInstanceID: "0f8c2b1a4d6e7f90",
    });
    await client.setRole({ sessionID: "ses_sender", role: "controller", soft: true });

    expect(await recorded.requests[0]?.json()).toEqual({
      session_id: "ses_sender",
      role: "controller",
      soft: true,
      client_instance_id: "0f8c2b1a4d6e7f90",
    });
    const unnamed = (await recorded.requests[1]?.json()) as Record<string, unknown>;
    // The key is absent, not undefined, so an older listener sees an unchanged body.
    expect(Object.hasOwn(unnamed, "client_instance_id")).toBe(false);
    expect(unnamed).toEqual({ session_id: "ses_sender", role: "controller", soft: true });
  });

  test("a 409 without the held-role shape stays a typed API error", async () => {
    const recorded = recordFetch([Response.json({ error: "duplicate" }, { status: 409 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await expect(
      client.setRole({ sessionID: "ses_x", role: "controller", soft: true })
    ).rejects.toBeInstanceOf(EnvoyApiError);
  });

  test("a 409 whose body does not arrive stays a typed API error, sent once", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        const body = new ReadableStream({
          pull(controller) {
            controller.error(new TypeError("body lost"));
          },
        });
        return new Response(body, { status: 409 });
      },
    });

    await expect(
      client.setRole({ sessionID: "ses_x", role: "controller", soft: true })
    ).rejects.toBeInstanceOf(EnvoyApiError);
    expect(calls).toBe(1);
  });

  test("lists listener sessions and retains their registration metadata, plugin or none", async () => {
    const unversioned = {
      session_id: "ses_sender",
      machine_id: "host-a",
      dir: "/work",
      port: 13381,
      title: "Envoy QA",
      topics: ["notifications.agent.ses_sender"],
      self_subscribed: false,
    };
    const versioned = {
      ...unversioned,
      session_id: "ses_versioned",
      topics: ["notifications.agent.ses_versioned"],
      plugin: "@sjawhar/claude-envoy",
      plugin_version: "afe10b2f3e1b",
    };
    const recorded = recordFetch([jsonResponse([unversioned, versioned])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    // A listener that names no state lists only sessions it holds a registration for, so every
    // row of its answer is live; reading the absence as anything else buries working sessions.
    // A row without roles or capabilities reads as holding none, so the lists are always there.
    expect(sessions).toEqual([
      { ...unversioned, state: "live", roles: [], capabilities: [] },
      { ...versioned, state: "live", roles: [], capabilities: [] },
    ]);
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions");
  });

  test("keeps each session's delivery mode apart from a listener that reports none", async () => {
    const row = {
      machine_id: "host-a",
      dir: "/work",
      port: 13381,
      title: "Envoy QA",
      topics: [],
      self_subscribed: false,
    };
    const relayed = { ...row, session_id: "ses_relayed", delivery: "consumer" };
    // A listener that reports delivery writes the key for every session, so "" is the core path
    // and an absent key is a listener older than the field: a measurement of how much traffic is
    // still on core counts them differently, and neither may become the other here.
    const core = { ...row, session_id: "ses_core", delivery: "" };
    const unreported = { ...row, session_id: "ses_unreported" };
    const recorded = recordFetch([jsonResponse([relayed, core, unreported])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    // Every row carries the state the listener put the session in, and one that names none
    // reads as live, so the delivery each row reports is read beside its state.
    expect(sessions).toEqual([
      { ...relayed, state: "live", roles: [], capabilities: [] },
      { ...core, state: "live", roles: [], capabilities: [] },
      { ...unreported, state: "live", roles: [], capabilities: [] },
    ]);
    expect(sessions.map((session) => session.delivery)).toEqual(["consumer", "", undefined]);
  });

  test("drops a session row's tmux place, and any other member this build does not read", async () => {
    const row = {
      session_id: "ses_placed",
      machine_id: "host-a",
      dir: "/work",
      port: 0,
      title: "Envoy QA",
      topics: ["notifications.agent.ses_placed"],
      self_subscribed: false,
    };
    const placed = {
      ...row,
      // The listener answers the place of a session in a pane. Nothing on this side reads it —
      // the one reader of this list is the `envoy_sessions` tool, which hands the model every
      // member of every row, and a layout string is up to 2 000 characters of them — so the
      // schema leaves it off and zod drops it.
      tmux_place: {
        socket: "/tmp/tmux-1000/default",
        server_pid: "4242",
        session_name: "legion",
        window_id: "@7",
        window_index: 3,
        window_name: "worker",
        pane_id: "%3",
        pane_index: 1,
        zoomed: true,
      },
    };
    const recorded = recordFetch([
      jsonResponse([
        placed,
        // A newer listener's row, read by this build: the member it does not know is dropped,
        // not refused, so one added field never blanks a machine's session list.
        { ...placed, session_id: "ses_newer", pane_title: "worker 3" },
      ]),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    // A row carrying a place still parses, which is what keeps an older reader working against a
    // listener that answers one.
    expect(sessions[0]).toEqual({ ...row, state: "live", roles: [], capabilities: [] });
    expect(sessions[1]).toEqual({
      ...row,
      session_id: "ses_newer",
      state: "live",
      roles: [],
      capabilities: [],
    });
  });

  test("reads a listener's null topics as an empty list and keeps the row's other values", async () => {
    const recorded = recordFetch([jsonResponse([noInterestsFixture.before_this_change])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    // updated_at is absent: the schema does not read it, so zod drops it.
    expect(sessions).toEqual([
      {
        session_id: "ses_infra742_no_interests",
        state: "live",
        machine_id: "envoy-fixture",
        host: "envoy-fixture",
        dir: "/work/infra-742-fixture",
        port: 0,
        title: "INFRA-742 fixture session",
        topics: [],
        roles: [],
        capabilities: [],
        self_subscribed: true,
        last_seen: 0,
        delivery: "consumer",
      },
    ]);
  });

  test("reads the fixed listener's empty lists unchanged", async () => {
    const recorded = recordFetch([jsonResponse([noInterestsFixture.after_this_change])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    expect(sessions[0]).toMatchObject({ topics: [], roles: [], capabilities: [] });
  });

  // Inline rather than in the fixture: the fixture states what a listener on main writes, and
  // these two rows — every list null, and every list's key absent — are what a tolerant reader
  // must also accept from a listener it does not deploy with.
  test("reads null and absent topics, roles and capabilities as empty lists", async () => {
    const base = {
      session_id: "ses_null",
      machine_id: "host-a",
      dir: "/work",
      port: 13381,
      title: "Envoy QA",
      self_subscribed: false,
    };
    const recorded = recordFetch([
      jsonResponse([
        { ...base, topics: null, roles: null, capabilities: null },
        { ...base, session_id: "ses_absent" },
      ]),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions();

    const empty = { topics: [], roles: [], capabilities: [] };
    expect(sessions).toEqual([
      { ...base, state: "live", ...empty },
      { ...base, session_id: "ses_absent", state: "live", ...empty },
    ]);
  });

  // The tolerance reads a null or an absent key, nothing more: a wrong element type is still a
  // contract break and must fail rather than read as empty.
  test("still rejects a session row whose topics hold a wrong element type", async () => {
    const recorded = recordFetch([
      jsonResponse([
        {
          session_id: "ses_bad",
          machine_id: "host-a",
          dir: "/work",
          port: 13381,
          title: "Envoy QA",
          topics: [1],
          self_subscribed: false,
        },
      ]),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    // The row is otherwise valid, so the one issue is the element's type.
    await expect(client.listSessions()).rejects.toMatchObject({
      issues: [{ code: "invalid_type", path: [0, "topics", 0] }],
    });
  });

  test("keeps an unresponsive row's state and its last registration, and asks for it by name", async () => {
    const stalled = {
      session_id: "ses_stalled",
      machine_id: "host-a",
      dir: "/work",
      port: 0,
      title: "Reviewer",
      topics: ["notifications.agent.ses_stalled"],
      self_subscribed: true,
      last_seen: 1_700_000_000_000,
      state: "unresponsive",
    };
    const recorded = recordFetch([jsonResponse([stalled])]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions({ include: "unresponsive" });

    // A caller that must see a stalled peer asks for it: without the selector the listener
    // answers the live rows alone, and the peer reads as a session nobody is running.
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions?include=unresponsive");
    expect(sessions[0]).toMatchObject({ state: "unresponsive", last_seen: 1_700_000_000_000 });
  });

  test("names the recipient's state on a send, and reads a listener that names none as live", async () => {
    const envelope = {
      event_id: "event-stalled",
      source: "agent",
      source_event_id: "agent.sender.event-stalled",
      topic: "notifications.agent.ses_stalled",
      dedupe_key: "event-stalled",
      issued_at: 1,
      payload_summary: "hello",
      trace_id: "trace-stalled",
      recipient: "ses_stalled",
    };
    const recorded = recordFetch([
      jsonResponse({ ...envelope, recipient_state: "unresponsive" }),
      jsonResponse(envelope),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const stalled = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_stalled",
        message: "hello",
      })
    );
    const older = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_stalled",
        message: "hello",
      })
    );

    expect(stalled.recipientState).toBe("unresponsive");
    expect(older.recipientState).toBe("live");
  });

  // A sender decides whether to wait on the recipient's state and on how long it has been
  // silent, and the listener sends both; dropping last_seen left the caller the state alone
  // (dispatch://INFRA-392, R7).
  test("carries the recipient's last registration from a send, and omits one the listener cannot name", async () => {
    const envelope = {
      event_id: "event-stalled",
      source: "agent",
      source_event_id: "agent.sender.event-stalled",
      topic: "notifications.agent.ses_stalled",
      dedupe_key: "event-stalled",
      issued_at: 1,
      payload_summary: "hello",
      trace_id: "trace-stalled",
      recipient: "ses_stalled",
      recipient_state: "unresponsive",
    };
    const recorded = recordFetch([
      jsonResponse({ ...envelope, last_seen: 1_700_000_000_000 }),
      jsonResponse(envelope),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const timed = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_stalled",
        message: "hello",
      })
    );
    const untimed = delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: "ses_stalled",
        message: "hello",
      })
    );

    expect(timed.lastSeen).toBe(1_700_000_000_000);
    expect(untimed.lastSeen).toBeUndefined();
  });

  // The refusal an agent reads at the moment it decides whether its peer is alive. The
  // listener's own text names no state, so an unresponsive holder's lapsed role read as a dead
  // one, and the next agent took its work (dispatch://INFRA-392, R7).
  test("a lapsed role's refusal says which state its holder is in, and that the claim stands", async () => {
    const recorded = recordFetch([
      Response.json(
        {
          error:
            "role reviewer holder ses_stalled lapsed; claim retained (last seen 1700000000000)",
          reason: "holder_lapsed",
          holder: "ses_stalled",
          holder_state: "unresponsive",
          last_seen: 1_700_000_000_000,
          claim_released: false,
        },
        { status: 404 }
      ),
      Response.json(
        {
          error: "role reviewer holder ses_ended lapsed; claim retained (last seen unavailable)",
          reason: "holder_lapsed",
          holder: "ses_ended",
          holder_state: "gone",
          claim_released: false,
        },
        { status: 404 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const stalled = await client.getRole("reviewer").catch((error: unknown) => error);
    const ended = await client.getRole("reviewer").catch((error: unknown) => error);

    expect(stalled).toBeInstanceOf(EnvoyApiError);
    const unresponsive = stalled as EnvoyApiError;
    expect(unresponsive.message).toBe(
      "role reviewer holder ses_stalled lapsed; claim retained (last seen 1700000000000) (reason: holder_lapsed) Holder ses_stalled is unresponsive, last seen 2023-11-14T22:13:20.000Z. An unresponsive session is not dead: it keeps its role claims and its issue claims, and a message sent to it is stored until it answers. Start no replacement for it and take no claim from it until it is gone, " +
        `or until ${GRACE_MINUTES} minutes have passed since its last_seen, the time of its last registration.`
    );
    expect(unresponsive.sessionState).toBe("unresponsive");
    expect(unresponsive.holder).toBe("ses_stalled");
    expect(unresponsive.lastSeen).toBe(1_700_000_000_000);
    expect(unresponsive.claimReleased).toBe(false);

    // A holder past the gone horizon is said to be gone, with no rule to wait on, and a time
    // the listener cannot name is left unsaid rather than guessed.
    const gone = ended as EnvoyApiError;
    expect(gone.message).toBe(
      "role reviewer holder ses_ended lapsed; claim retained (last seen unavailable) (reason: holder_lapsed) Holder ses_ended is gone."
    );
    expect(gone.sessionState).toBe("gone");
    expect(gone.lastSeen).toBeUndefined();
  });

  // A send refused for a session the listener will not answer for names that session's state
  // the same way, so "no live session" no longer reads as death on its own.
  test("a refused send says whether the session is unresponsive or gone", async () => {
    const recorded = recordFetch([
      Response.json(
        {
          error: "no live session ses_stalled",
          state: "unresponsive",
          last_seen: 1_700_000_000_000,
        },
        { status: 404 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const refusal = (await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_stalled", message: "hello" })
      .catch((error: unknown) => error)) as EnvoyApiError;

    expect(refusal.message).toStartWith(
      "no live session ses_stalled That session is unresponsive, last seen 2023-11-14T22:13:20.000Z. An unresponsive session is not dead:"
    );
    expect(refusal.sessionState).toBe("unresponsive");
    expect(refusal.lastSeen).toBe(1_700_000_000_000);
  });

  test("normalizes non-success responses into a typed API error", async () => {
    const recorded = recordFetch([new Response("invalid role", { status: 400 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const request = client.setRole({ sessionID: "ses_sender", role: "invalid role" });

    await expect(request).rejects.toEqual(
      new EnvoyApiError({
        method: "POST",
        url: "http://listener/v1/roles/set",
        status: 400,
        responseBody: "invalid role",
      })
    );
  });

  test("retries one 503 response and keeps the direct-send recipient", async () => {
    const recorded = recordFetch([
      Response.json({ error: "listener is starting" }, { status: 503 }),
      jsonResponse({
        event_id: "event-retried",
        source: "agent",
        source_event_id: "agent.sender.event-retried",
        source_session: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.agent.01a01111-2222-7333-4444-555555555555",
        dedupe_key: "event-retried",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-retried",
        recipient: "01a01111-2222-7333-4444-555555555555",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "01a00000-0000-7000-0000-000000000001",
      targetSessionID: "01a01111-2222-7333-4444-555555555555",
      message: "hello",
    });

    expect(recorded.requests).toHaveLength(2);
    expect(result).toMatchObject({
      recipient: "01a01111-2222-7333-4444-555555555555",
      envelope: { event_id: "event-retried" },
    });
  });

  // The listener writes no 408, so one is a hop's timeout on the send, which may come after the
  // hop forwarded it: the send goes again under the same key, and the second attempt's answer is
  // the send's.
  test("retries a send answered 408 under the same key, and the second attempt delivers it", async () => {
    const recorded = recordFetch([
      new Response("<html>408 Request Timeout</html>", { status: 408 }),
      jsonResponse({
        event_id: "event-retried",
        source: "agent",
        source_event_id: "agent.sender.event-retried",
        source_session: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.agent.01a01111-2222-7333-4444-555555555555",
        dedupe_key: "event-retried",
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-retried",
        recipient: "01a01111-2222-7333-4444-555555555555",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "01a00000-0000-7000-0000-000000000001",
      targetSessionID: "01a01111-2222-7333-4444-555555555555",
      message: "hello",
    });

    expect(recorded.requests).toHaveLength(2);
    const keys = await Promise.all(
      recorded.requests.map(async (request) => (await request.json()).idempotency_key)
    );
    expect(keys[0]).toBeString();
    expect(keys[1]).toBe(keys[0]);
    expect(result).toMatchObject({ envelope: { event_id: "event-retried" } });
  });
  test("retries a delivered direct message with one generated idempotency key", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key?: string };
        if (typeof body.idempotency_key !== "string") {
          return new Response("idempotency key required", { status: 400 });
        }
        idempotencyKeys.push(body.idempotency_key);
        if (idempotencyKeys.length === 1) {
          throw new TypeError("response lost after delivery");
        }
        if (body.idempotency_key !== idempotencyKeys[0]) {
          return new Response("duplicate message", { status: 409 });
        }
        return jsonResponse({
          event_id: "event-delivered",
          source: "agent",
          source_event_id: "agent.sender.event-delivered",
          source_session: "ses_sender",
          topic: "notifications.agent.ses_target",
          dedupe_key: "event-delivered",
          issued_at: 1,
          payload_summary: "hello",
          trace_id: "trace-delivered",
          recipient: "ses_target",
        });
      },
    });

    await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
    });

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
  });

  test("a send no attempt of which is answered is not confirmed delivered, and names the key a resend reuses", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key: string };
        idempotencyKeys.push(body.idempotency_key);
        // What AbortSignal.timeout raises when the listener holds the request past the deadline.
        throw new DOMException("The operation timed out.", "TimeoutError");
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
    const key = idempotencyKeys[0] as string;
    // The listener's test of a dedupe key a repeat is recognised by, so a resend under the key the
    // error names cannot deliver the message twice.
    expect(`agent.ses_target.${key}`).toMatch(new RegExp(MINTED_DEDUPE_KEY_PATTERN));
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(error).toMatchObject({
      targetSessionID: "ses_target",
      idempotencyKey: key,
      attempts: 2,
      resendSafe: true,
    });
    // The attempt that got no answer is named by the request and the failure's class: the
    // runtime's own message can repeat the host it dialed.
    expect((error as Error).message).toBe(
      "The send to ses_target is not confirmed delivered: the listener did not answer " +
        "(POST http://listener/v1/messages/send got no answer: TimeoutError (message withheld)) " +
        "on the last of 2 attempts. It may or may not have arrived. " +
        `Sending it again with idempotency_key ${key} within 72 hours is the safe resend: ` +
        "Envoy drops it as a repeat if the first copy arrived (a recipient that restarted in " +
        "between can rarely see it twice)."
    );
  });

  test("a send refused after an unanswered attempt stays unconfirmed: that attempt may have stored it", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        if (calls === 1) throw new DOMException("The operation timed out.", "TimeoutError");
        return Response.json({ error: "no live session ses_target" }, { status: 404 });
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(calls).toBe(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as EnvoySendUnconfirmedError).message).toStartWith(
      "The send to ses_target is not confirmed delivered: the listener answered 404 " +
        "(no live session ses_target) on the last of 2 attempts. It may or may not have arrived."
    );
  });

  // A redirect in front of the listener is never followed (A1 of the review of 87105ed7e8): a 301,
  // 302 or 303 would turn the send's POST into a GET, and a 307 or 308 would send it again to
  // another origin. The 3xx is the answer: it says nothing reached the listener, so the send is
  // refused, never "may or may not have arrived".
  test.each([
    301, 302, 303, 307, 308,
  ])("a send a %i answers is refused, and the redirect is not followed", async (status) => {
    let followed = 0;
    let sent = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: (async (_url: RequestInfo | URL, init?: RequestInit) => {
        sent += 1;
        if (init?.redirect !== "manual") {
          // What fetch does where it follows: the request goes to the other origin.
          followed += 1;
          return Response.json(
            { event_id: "evt-elsewhere", recipient: "ses_target", recipient_state: "live" },
            { status: 202 }
          );
        }
        return new Response("moved", {
          status,
          headers: { Location: "http://elsewhere.test/v1/messages/send" },
        });
      }) as unknown as typeof fetch,
    });
    const error = await client
      .send({ sourceSessionID: "ses_source", targetSessionID: "ses_target", message: "hello" })
      .catch((caught: unknown) => caught);
    expect(followed).toBe(0);
    expect(sent).toBe(1);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect((error as EnvoyApiError).details.status).toBe(status);
  });

  // F6 (the review of 81d860a8f7): whatever answered a redirect in the listener's place chose its
  // body, and a misconfigured proxy echoes the request back, the bearer with it. The error prints
  // that body as the Dispatch client prints a gateway's page, one line with the bearer redacted, and
  // names the origin the redirect pointed at; never the body whole.
  test("a send a redirect answers prints its body as one redacted line, with the redirect's origin", async () => {
    const listenerToken = fixtureSecret("listener");
    const echoed = `<html><body><h1>Moved</h1>\n<p>Authorization: Bearer ${listenerToken}</p>\n${"<p>filler</p>\n".repeat(400)}</body></html>`;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      credential: bearerCredential(listenerToken),
      fetch: (async () =>
        new Response(echoed, {
          status: 308,
          headers: {
            "Content-Type": "text/html",
            Location: "https://user:pass@elsewhere.test/v1/messages/send?token=abc",
          },
        })) as unknown as typeof fetch,
    });
    const error = await client
      .send({ sourceSessionID: "ses_source", targetSessionID: "ses_target", message: "hello" })
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(EnvoyApiError);
    const message = (error as EnvoyApiError).message;
    expect(message).toStartWith(
      "POST http://listener/v1/messages/send failed with 308 (a redirect to https://elsewhere.test, " +
        "which this client does not follow): a body that is not the listener's error JSON " +
        '("Moved Authorization: Bearer [redacted]'
    );
    expect(message).not.toContain(listenerToken);
    expect(message).not.toContain("\n");
    expect(message).not.toContain("pass@");
    expect(message.length).toBeLessThan(400);
  });

  // F6's class (the coordinator's ruling on the review of 81d860a8f7): any answer whose body is not
  // the listener's error JSON was written by something in its place, a proxy's or a load
  // balancer's page, which can echo the request back, the bearer with it. The error names that
  // body as the Dispatch client names a gateway's page: one line, the bearer and any
  // `Authorization` or `Bearer` value redacted, never the page whole.
  test.each([
    [403, "Forbidden"],
    [502, "Bad Gateway"],
  ] as const)("a %i page a proxy answers in the listener's place prints as one redacted line", async (status, phrase) => {
    const listenerToken = fixtureSecret("listener");
    const page =
      `<html><head><title>${status} ${phrase}</title><style>body{margin:0}</style></head>\n` +
      `<body><h1>${status} ${phrase}</h1>\n<pre>GET /v1/interests/ses_sender\n` +
      `Authorization: Bearer ${listenerToken}</pre>\n${"<p>filler</p>\n".repeat(400)}</body></html>`;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      credential: bearerCredential(listenerToken),
      retry: false,
      fetch: (async () =>
        new Response(page, {
          status,
          headers: { "Content-Type": "text/html" },
        })) as unknown as typeof fetch,
    });
    const error = await client.getInterest("ses_sender").catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(EnvoyApiError);
    const message = (error as EnvoyApiError).message;
    expect(message).toStartWith(
      `GET http://listener/v1/interests/ses_sender failed with ${status}: a body that is not the ` +
        `listener's error JSON ("${status} ${phrase} ${status} ${phrase} GET /v1/interests/ses_sender ` +
        "Authorization: Bearer [redacted]"
    );
    expect(message).not.toContain(listenerToken);
    expect(message).not.toContain("\n");
    expect(message).not.toContain("<");
    expect(message.length).toBeLessThan(400);
  });

  // M4 (the review of 99b1e438db): a page that echoes the request's `Authorization: DPoP` back is
  // redacted as a bearer's is, and so is the token the request was sent with when the credential
  // names another by the time the answer comes (a renewal between the two).
  test("a page that echoes a DPoP token sent before a renewal prints none of it", async () => {
    const sent = fixtureSecret("sent");
    const renewed = fixtureSecret("renewed");
    let presentations = 0;
    const credential: RequestCredential = {
      scheme: "DPoP",
      headers: () => {
        presentations += 1;
        return { Authorization: `DPoP ${sent}`, DPoP: "proof" };
      },
      secret: () => (presentations === 0 ? sent : renewed),
    };
    const page =
      `<html><body><h1>502 Bad Gateway</h1><pre>Authorization: DPoP ${sent}\n` +
      `X-Echo: DPoP ${sent.slice(2)}\nX-Raw: ${sent}</pre></body></html>`;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      credential,
      retry: false,
      fetch: (async () =>
        new Response(page, {
          status: 502,
          headers: { "Content-Type": "text/html" },
        })) as unknown as typeof fetch,
    });
    const error = await client.getInterest("ses_sender").catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(EnvoyApiError);
    const message = (error as EnvoyApiError).message;
    expect(message).toContain(
      '("502 Bad Gateway Authorization: DPoP [redacted] X-Echo: DPoP [redacted] X-Raw: [redacted]")'
    );
    expect(message).not.toContain(sent.slice(-12));
  });

  // A refused connection, or a name that does not resolve, reached no listener, so nothing was
  // stored: the send is refused, in the transport's own words, never "may or may not have
  // arrived" with a resend to make.
  test("a send to a listener that refuses its connection on every attempt is refused, not unconfirmed", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: (async () => {
        calls += 1;
        throw Object.assign(new TypeError("Unable to connect"), { code: "ECONNREFUSED" });
      }) as unknown as typeof fetch,
    });
    const error = await client
      .send({ sourceSessionID: "ses_source", targetSessionID: "ses_target", message: "hello" })
      .catch((caught: unknown) => caught);
    expect(calls).toBe(2);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as Error).message).toBe(
      "POST http://listener/v1/messages/send got no answer: ECONNREFUSED"
    );
  });

  test("a send whose 2xx answer does not parse is not confirmed delivered", async () => {
    const recorded = recordFetch([new Response("{", { status: 200 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as EnvoySendUnconfirmedError).last).toBeInstanceOf(SyntaxError);
  });

  test("a send under a key no repeat is recognised by does not call a resend safe", async () => {
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        throw new TypeError("connection refused");
      },
    });

    for (const [targetSessionID, idempotencyKey] of [
      ["ses_target", "retry-1"],
      ["ses.dotted", "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80"],
    ] as const) {
      const error = await client
        .send({ sourceSessionID: "ses_sender", targetSessionID, message: "hello", idempotencyKey })
        .then(
          () => undefined,
          (thrown: unknown) => thrown
        );

      expect(error).toMatchObject({ resendSafe: false });
      expect((error as Error).message).toEndWith(
        `Envoy cannot recognise a resend under idempotency_key ${idempotencyKey} as a repeat ` +
          "(the key is not a lower-case UUID, or the session id holds a dot): ask the recipient " +
          "whether it arrived before sending it again."
      );
    }
  });

  test("a send whose last attempt the listener answered with a 5xx is not confirmed delivered", async () => {
    const refusal = () =>
      Response.json(
        { error: "envoy: the message bus did not answer: context deadline exceeded" },
        { status: 503 }
      );
    const recorded = recordFetch([refusal(), refusal()]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    const unconfirmed = error as EnvoySendUnconfirmedError;
    expect(unconfirmed.last).toBeInstanceOf(EnvoyApiError);
    expect((unconfirmed.last as EnvoyApiError).details.status).toBe(503);
    expect(unconfirmed.message).toStartWith(
      "The send to ses_target is not confirmed delivered: the listener answered 503 " +
        "(envoy: the message bus did not answer: context deadline exceeded) on the last of 2 " +
        "attempts. It may or may not have arrived. Sending it again with idempotency_key "
    );
  });

  // The listener's 503 for a message the bus refused before it sent anything: the deployed stream
  // does not capture the topic, as during a hub move's quiesce (`bus.ErrSubjectNotDeployed`,
  // `writeBusUnavailable` in packages/envoy/cmd/listener/api.go). Its error opens with
  // `MESSAGE_NOT_SENT_PREFIX` and goes on to the bus's own reason.
  const NOT_SENT_ERROR =
    `${MESSAGE_NOT_SENT_PREFIX}: nothing was sent: the deployed stream does not capture a topic ` +
    "this build stores (stream ENVOY_NOTIFICATIONS, topic notifications.agent.ses_target)";
  const notSent = () =>
    Response.json({ error: NOT_SENT_ERROR }, { status: 503, headers: { "Retry-After": "1" } });

  // Nothing was stored on either attempt, so the send is the listener's answer, which says the
  // message was not sent and may be sent again later, never "may or may not have arrived" with a
  // resend to make under the key. The client still makes its second attempt, under the same key.
  test("a send the listener did not send on any attempt is that answer: nothing was stored", async () => {
    const recorded = recordFetch([notSent(), notSent()]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    // Both attempts carry the same body, so the same idempotency key.
    const first: unknown = await recorded.requests[0]?.json();
    expect(first).toMatchObject({ idempotency_key: expect.any(String) });
    expect(await recorded.requests[1]?.json()).toEqual(first);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).toMatchObject({ notSent: true, details: { status: 503 } });
    expect((error as Error).message).toBe(NOT_SENT_ERROR);
  });

  // The control: an attempt that got no answer may have stored the message, so a later attempt
  // the listener did not send says nothing of it, and the send stays unconfirmed.
  test("a send not sent after an unanswered attempt stays unconfirmed: that attempt may have stored it", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        if (calls === 1) throw new DOMException("The operation timed out.", "TimeoutError");
        return notSent();
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(calls).toBe(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as Error).message).toContain("It may or may not have arrived.");
  });

  test("a publish the listener did not send on any attempt is that answer: nothing was stored", async () => {
    const recorded = recordFetch([notSent(), notSent()]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .publish({ sourceSessionID: "ses_sender", topic: "notifications.team.x", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).toMatchObject({ notSent: true, details: { status: 503 } });
    expect((error as Error).message).toBe(NOT_SENT_ERROR);
  });

  // A publish has no error of its own for a message that may have been stored, so its last answer
  // is what it throws: one that says the message was not sent must not stand alone when an earlier
  // attempt got a 5xx or no answer, after which the stream may hold the message.
  test("a publish not sent after a 5xx does not say nothing was stored", async () => {
    const recorded = recordFetch([
      Response.json(
        { error: "envoy: the message bus did not answer: context deadline exceeded" },
        { status: 503 }
      ),
      notSent(),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .publish({ sourceSessionID: "ses_sender", topic: "notifications.team.x", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).toMatchObject({ notSent: false, details: { status: 503 } });
    expect((error as Error).message).toBe(
      `${NOT_SENT_ERROR} An earlier attempt got no answer, a 5xx or a 408, so that attempt may have ` +
        "stored the message."
    );
  });

  // The two rules meet here. A 408 is in the resend set (`mayHaveReachedServer`), so the send is
  // made again under its key, and the 408 may have reached the listener, so the send stays
  // unconfirmed even though the second attempt was the listener's word that it sent nothing.
  test("a send not sent after a 408 is sent again and stays unconfirmed: the 408 may have stored it", async () => {
    const recorded = recordFetch([
      Response.json({ error: "upstream request timeout" }, { status: 408 }),
      notSent(),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
  });

  // And the other order: the listener's not-sent 503 is sent again and counts for nothing, so the
  // 408 after it is what makes the send unconfirmed; a 408 outside the resend set would end the
  // send at once as the listener's refusal.
  test("a 408 after the listener's not-sent answer makes the send unconfirmed", async () => {
    const recorded = recordFetch([
      notSent(),
      Response.json({ error: "upstream request timeout" }, { status: 408 }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(2);
    expect(error).toBeInstanceOf(EnvoySendUnconfirmedError);
  });

  test("a send the listener refused with a 4xx is that refusal, sent once", async () => {
    const recorded = recordFetch([
      Response.json({ error: "no live session ses_target" }, { status: 404 }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect((error as Error).message).toBe("no live session ses_target");
  });

  // Each outage on every attempt of a send: a 5xx may have been stored, and so may a 408 (the
  // listener writes none, and a hop can answer 408 after it forwarded the send), so the send is not
  // confirmed; a 429 is the listener's word and is not sent again.
  test("a send answered with an outage on every attempt: a 408 or a 5xx is unconfirmed, a 429 is one request", async () => {
    const outcomes: Record<number, readonly [number, string]> = {};
    for (const status of [408, 429, 500, 501, 503]) {
      const recorded = recordFetch(
        Array.from({ length: 2 }, () => Response.json({ error: `answered ${status}` }, { status }))
      );
      const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
      const error = await client
        .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
        .then(
          () => undefined,
          (thrown: unknown) => thrown
        );
      outcomes[status] = [recorded.requests.length, (error as Error).name];
    }
    expect(outcomes).toEqual({
      408: [2, "EnvoySendUnconfirmedError"],
      429: [1, "EnvoyApiError"],
      500: [2, "EnvoySendUnconfirmedError"],
      501: [2, "EnvoySendUnconfirmedError"],
      503: [2, "EnvoySendUnconfirmedError"],
    });
  });

  // The listener's 429 for a sender past its limit (`senderRateLimitedAnswer`,
  // packages/envoy/cmd/listener/sender_limit.go): its sentence is what the agent reads, and its
  // wait is what a caller times a resend by.
  const RATE_LIMITED_SENTENCE =
    "Your sending is rate limited. This message was not sent. This listener lets one session " +
    "send 4 messages a minute and 2 at once. You can send it again after 15 seconds.";
  const RATE_LIMITED_BODY = {
    error: RATE_LIMITED_SENTENCE,
    reason: "sender_rate_limited",
    retry_after_seconds: 15,
    messages_per_minute: 4,
    burst: 2,
  };

  test("a send whose sender is rate limited is that refusal, with its wait, after one request", async () => {
    const recorded = recordFetch([Response.json(RATE_LIMITED_BODY, { status: 429 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).not.toBeInstanceOf(EnvoySendUnconfirmedError);
    expect(error).toMatchObject({ reason: "sender_rate_limited", retryAfterSeconds: 15 });
    expect((error as Error).message).toBe(`${RATE_LIMITED_SENTENCE} (reason: sender_rate_limited)`);
  });

  test("a publish whose sender is rate limited is that refusal, with its wait, after one request", async () => {
    const recorded = recordFetch([Response.json(RATE_LIMITED_BODY, { status: 429 })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const error = await client
      .publish({ sourceSessionID: "ses_sender", topic: "notifications.team.x", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(recorded.requests).toHaveLength(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect(error).toMatchObject({ reason: "sender_rate_limited", retryAfterSeconds: 15 });
    expect((error as Error).message).toBe(`${RATE_LIMITED_SENTENCE} (reason: sender_rate_limited)`);
  });

  test("a refusal whose body does not arrive is still the listener's refusal, sent once", async () => {
    let calls = 0;
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async () => {
        calls += 1;
        const body = new ReadableStream({
          pull(controller) {
            controller.error(new TypeError("body lost"));
          },
        });
        return new Response(body, { status: 409 });
      },
    });

    const error = await client
      .send({ sourceSessionID: "ses_sender", targetSessionID: "ses_target", message: "hello" })
      .then(
        () => undefined,
        (thrown: unknown) => thrown
      );

    expect(calls).toBe(1);
    expect(error).toBeInstanceOf(EnvoyApiError);
    expect((error as EnvoyApiError).details.status).toBe(409);
  });

  test("a resend under the named key sends that key and reports the listener's repeat", async () => {
    const key = "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80";
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-resent",
        source: "agent",
        source_event_id: "agent.ses_sender.event-resent",
        source_session: "ses_sender",
        topic: "notifications.agent.ses_target",
        dedupe_key: `agent.ses_target.${key}`,
        issued_at: 1,
        payload_summary: "hello",
        trace_id: "trace-resent",
        recipient: "ses_target",
        duplicate: true,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.send({
      sourceSessionID: "ses_sender",
      targetSessionID: "ses_target",
      message: "hello",
      idempotencyKey: key,
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({ idempotency_key: key });
    expect(result).toMatchObject({ recipient: "ses_target", confirmed: true, duplicate: true });
  });

  test("retries a delivered publish with one generated idempotency key", async () => {
    const idempotencyKeys: string[] = [];
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly idempotency_key?: string };
        if (typeof body.idempotency_key !== "string") {
          return new Response("idempotency key required", { status: 400 });
        }
        idempotencyKeys.push(body.idempotency_key);
        if (idempotencyKeys.length === 1) {
          throw new TypeError("response lost after delivery");
        }
        if (body.idempotency_key !== idempotencyKeys[0]) {
          return new Response("duplicate message", { status: 409 });
        }
        return jsonResponse({
          event_id: "event-published",
          source: "agent",
          source_event_id: "agent.sender.event-published",
          source_session: "ses_sender",
          topic: "notifications.role.legion-reviewer",
          dedupe_key: "event-published",
          issued_at: 1,
          payload_summary: "review this",
          trace_id: "trace-published",
        });
      },
    });

    await client.publish({
      sourceSessionID: "ses_sender",
      topic: "notifications.role.legion-reviewer",
      message: "review this",
    });

    expect(idempotencyKeys).toHaveLength(2);
    expect(idempotencyKeys[1]).toBe(idempotencyKeys[0]);
  });

  test("serializes all additive message fields and retains role holder results", async () => {
    const recorded = recordFetch([
      jsonResponse({
        event_id: "event-role",
        source: "agent",
        source_event_id: "agent.sender.event-role",
        source_session: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.role.legion-reviewer",
        dedupe_key: "event-role",
        issued_at: 1,
        payload_summary: "review this",
        trace_id: "trace-role",
        holder: "01a01111-2222-7333-4444-555555555555",
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const result = await client.publish({
      sourceSessionID: "01a00000-0000-7000-0000-000000000001",
      topic: "notifications.role.legion-reviewer",
      message: "review this",
      inReplyTo: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expectsReply: "required",
      expiresAt: Date.parse("2026-09-07T05:00:00Z"),
    });

    expect(await recorded.requests[0]?.json()).toMatchObject({
      in_reply_to: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expects_reply: "required",
      expires_at: Date.parse("2026-09-07T05:00:00Z"),
    });
    expect(result).toMatchObject({
      envelope: { event_id: "event-role" },
      holder: "01a01111-2222-7333-4444-555555555555",
    });
  });

  test("uses listener directory and title session filters and gets a live role holder", async () => {
    const recorded = recordFetch([
      jsonResponse([
        {
          session_id: "01a00000-0000-7000-0000-000000000001",
          machine_id: "example-host",
          dir: "/work/example",
          port: 0,
          title: "Reviewer",
          topics: ["notifications.role.legion-reviewer"],
          roles: ["legion-reviewer"],
          self_subscribed: true,
          last_seen: 42,
        },
      ]),
      jsonResponse({
        role: "legion-reviewer",
        holder: "01a00000-0000-7000-0000-000000000001",
        last_seen: 42,
      }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const sessions = await client.listSessions({ directory: "example", title: "Review" });
    const role = await client.getRole("legion-reviewer");

    expect(sessions[0]).toMatchObject({ roles: ["legion-reviewer"], last_seen: 42 });
    expect(recorded.requests[0]?.url).toBe("http://listener/v1/sessions?dir=example&title=Review");
    expect(role).toEqual({
      role: "legion-reviewer",
      holder: "01a00000-0000-7000-0000-000000000001",
      last_seen: 42,
      // The listener named no state, and a role answer it gives names a holder it resolved.
      state: "live",
      holder_state: undefined,
    });
    expect(recorded.requests[1]?.url).toBe("http://listener/v1/roles/legion-reviewer");
  });

  test("a role lookup gives the holder's process, and parses an answer naming none", async () => {
    const recorded = recordFetch([
      jsonResponse({
        role: "controller",
        holder: "ses_holder",
        last_seen: 42,
        state: "live",
        client_instance_id: "0f8c2b1a4d6e7f90",
      }),
      jsonResponse({ role: "controller", holder: "ses_holder", last_seen: 42, state: "live" }),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    const named = await client.getRole("controller");
    const unnamed = await client.getRole("controller");

    // The holder's process is what tells a caller that the role is held under its own session
    // id by another process, which `setRole`'s 409 does not say.
    expect(named.client_instance_id).toBe("0f8c2b1a4d6e7f90");
    // A listener predating the field still parses: the caller reads no holding process.
    expect(unnamed.client_instance_id).toBeUndefined();
    expect(unnamed).toMatchObject({ role: "controller", holder: "ses_holder" });
  });

  test("surfaces listener error text and expected fields without retrying a 400", async () => {
    const recorded = recordFetch([
      Response.json(
        {
          error: "message must not be empty",
          expected: ["message"],
        },
        { status: 400 }
      ),
    ]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

    await expect(
      client.publish({
        sourceSessionID: "01a00000-0000-7000-0000-000000000001",
        topic: "notifications.role.legion-reviewer",
        message: "",
        urgency: "low",
      })
    ).rejects.toThrow("message must not be empty (expected: message)");
    expect(recorded.requests).toHaveLength(1);
  });
  test("sends the token it is given as a bearer token to the listener", async () => {
    const recorded = recordFetch([new Response(null, { status: 200 })]);
    const listenerToken = fixtureSecret("listener");
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: recorded.fetch,
      credential: bearerCredential(listenerToken),
    });

    await client.unregisterSession({ sessionID: "ses_closed" });

    expect(recorded.requests[0]?.headers.get("Authorization")).toBe(`Bearer ${listenerToken}`);
  });

  test("sends no Authorization header without a token, whatever ENVOY_TOKEN says", async () => {
    const previous = process.env.ENVOY_TOKEN;
    const environmentToken = fixtureSecret("envoy");
    process.env.ENVOY_TOKEN = environmentToken;
    try {
      const recorded = recordFetch([new Response(null, { status: 200 })]);
      const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });

      await client.unregisterSession({ sessionID: "ses_closed" });

      expect(recorded.requests[0]?.headers.has("Authorization")).toBe(false);
    } finally {
      if (previous === undefined) delete process.env.ENVOY_TOKEN;
      else process.env.ENVOY_TOKEN = previous;
    }
  });

  test("a credential this machine could not mint leaves a listener that asks for none working", async () => {
    // The broker is down, or has refused this machine's trade. A listener that asks for no
    // credential answers the same with or without one, so its calls stand; one that did want it
    // answers 401, and the error names the trade that failed rather than the status alone.
    const unmintable: RequestCredential = {
      scheme: "DPoP",
      headers: () => Promise.reject(new Error("the secrets broker did not answer")),
      secret: () => "",
    };
    const open = recordFetch([new Response(null, { status: 200 })]);
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      fetch: open.fetch,
      credential: unmintable,
    });

    await client.unregisterSession({ sessionID: "ses_closed" });

    expect(open.requests[0]?.headers.has("Authorization")).toBe(false);

    const closed = recordFetch([
      new Response(null, { status: 401 }),
      new Response(null, { status: 401 }),
    ]);
    await expect(
      createEnvoyClient({
        baseUrl: "http://listener",
        fetch: closed.fetch,
        credential: unmintable,
      }).unregisterSession({ sessionID: "ses_closed" })
    ).rejects.toThrow("the secrets broker did not answer");
  });

  // A listener URL may carry a user and a password. The request goes to it as configured, and the
  // error a refused, unauthorised or unanswered call throws names the URL by its fixed phrase
  // alone, since the URL parser can split such a URL otherwise than its writer meant: a password
  // holding an '@' and no host reads as a host of its tail, which a resolver's error repeats.
  const user = "user-7f2a";
  const pw = "pw-4c7e19";
  test.each([
    ["a user and a password", `http://${user}:${pw}@listener`, pw],
    ["a password holding an '@' and no host", `http://${user}:p@ssw0rdTAIL7c`, "ssw0rdtail7c"],
  ])("names a listener URL that carries %s by its fixed phrase in the errors it throws", async (_, baseUrl, secret) => {
    const unmintable: RequestCredential = {
      scheme: "DPoP",
      headers: () => Promise.reject(new Error("the secrets broker did not answer")),
      secret: () => "",
    };
    const refused = recordFetch([new Response("no such route", { status: 404 })]);
    const closed = recordFetch([new Response(null, { status: 401 })]);
    const lost = (async () => {
      throw Object.assign(new TypeError(`getaddrinfo ENOTFOUND ${secret}`), {
        code: "ENOTFOUND",
        syscall: "getaddrinfo",
      });
    }) as unknown as typeof fetch;
    // Each error's message and the messages of its cause chain: what a log of it prints, without
    // the source lines Bun.inspect quotes from this file beside a stack.
    const failure = (call: Promise<unknown>) =>
      call.then(
        () => "",
        (error: unknown) => {
          const texts: string[] = [];
          for (let at: unknown = error; at instanceof Error; at = at.cause) texts.push(at.message);
          return texts.join(" <- ");
        }
      );

    const messages = [
      await failure(
        createEnvoyClient({ baseUrl, fetch: refused.fetch, retry: false }).getInterest("ses_sender")
      ),
      await failure(
        createEnvoyClient({
          baseUrl,
          fetch: closed.fetch,
          credential: unmintable,
          retry: false,
        }).getInterest("ses_sender")
      ),
      await failure(
        createEnvoyClient({ baseUrl, fetch: lost, retry: false }).getInterest("ses_sender")
      ),
    ];

    const shown = `the configured Envoy URL ${URL_NOT_PRINTED}`;
    expect(refused.requests[0]?.url.toLowerCase()).toBe(
      `${baseUrl}/v1/interests/ses_sender`.toLowerCase()
    );
    expect(messages[0]).toContain(
      `GET ${shown} failed with 404: a body that is not the listener's error JSON ("no such route")`
    );
    expect(messages[1]).toContain(
      `GET ${shown} answered 401, and this machine's Envoy credential could not be minted: ` +
        "the secrets broker did not answer"
    );
    expect(messages[2]).toContain(`GET ${shown} got no answer: getaddrinfo ENOTFOUND`);
    for (const message of messages) {
      expect(message).not.toContain("user-7f2a");
      expect(message.toLowerCase()).not.toContain(secret.toLowerCase());
    }
  });

  // The deadline is AbortSignal.timeout's, on the platform's clock, which no fake timer moves:
  // the row holds the request until its signal aborts and reads no clock to see that a short
  // deadline gave up first. Nothing answers the request, so a client left on its own timing
  // spends two attempts of LISTENER_CALL_TIMEOUT_MS with LISTENER_CALL_RETRY_DELAY_MS between
  // them and runs past 10 s, while the 50 ms this one is configured for spends two of those with
  // the same wait between, a third of a second of wall clock on any machine. The row's 5 s limit
  // is what catches the difference, and it stays on the row, below the suite's 30 s default, for
  // exactly that reason.
  test("gives up a call at the deadline its configuration names", async () => {
    const client = createEnvoyClient({
      baseUrl: "http://listener",
      timeoutMs: 50,
      fetch: async (_input, init) => {
        const signal = init?.signal;
        if (signal === undefined || signal === null) throw new Error("no deadline was set");
        const aborted = Promise.withResolvers<never>();
        signal.addEventListener("abort", () => aborted.reject(signal.reason), { once: true });
        return aborted.promise;
      },
    });
    const failure = await client.getRole("reviewer").catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(Error);
  }, 5_000);
});

describe("newClientInstanceID", () => {
  test("mints 128 random bits as 32 lower-case hex characters", () => {
    expect(newClientInstanceID()).toMatch(/^[0-9a-f]{32}$/);
  });

  test("mints a different id for each client instance", () => {
    expect(newClientInstanceID()).not.toBe(newClientInstanceID());
  });
});

describe("roleHeldBy", () => {
  /** A role lookup's 200 for `holder`, naming `instance` as the process whose claim stands. */
  const answer = (holder: string, instance?: string): RoleInfo => ({
    role: "legion-controller",
    holder,
    last_seen: 2,
    state: "live",
    ...(instance === undefined ? {} : { client_instance_id: instance }),
  });

  /** The same answer, with the hand-back mark the listener puts on a claim it could not free. */
  const handedBack = (holder: string, instance?: string): RoleInfo => ({
    ...answer(holder, instance),
    handed_back: true,
  });

  test("the lookup names this session and this process", () => {
    expect(roleHeldBy(answer("ses_a", "proc-one"), "ses_a", "proc-one")).toBe(true);
  });

  test("a claim this very session id carries under another process is not this one's", () => {
    expect(roleHeldBy(answer("ses_a", "another-process"), "ses_a", "proc-one")).toBe(false);
  });

  test("a claim naming no process is read as this one's, absent or empty alike", () => {
    expect(roleHeldBy(answer("ses_a"), "ses_a", "proc-one")).toBe(true);
    expect(roleHeldBy(answer("ses_a", ""), "ses_a", "proc-one")).toBe(true);
  });

  test("a caller naming no process holds nothing a claim names a process for", () => {
    expect(roleHeldBy(answer("ses_a", "proc-one"), "ses_a", undefined)).toBe(false);
    expect(roleHeldBy(answer("ses_a", "proc-one"), "ses_a", "")).toBe(false);
    expect(roleHeldBy(answer("ses_a"), "ses_a", "")).toBe(true);
  });

  test("another session's hold, and the 404 that names no holder, are neither of them this one's", () => {
    expect(roleHeldBy(answer("ses_other", "proc-one"), "ses_a", "proc-one")).toBe(false);
    expect(roleHeldBy(undefined, "ses_a", "proc-one")).toBe(false);
  });

  test("a claim marked handed back is no hold, even of the session and process it names", () => {
    expect(roleHeldBy(handedBack("ses_a", "proc-one"), "ses_a", "proc-one")).toBe(false);
    expect(roleHeldBy(handedBack("ses_a"), "ses_a", "proc-one")).toBe(false);
  });

  test("an unmarked claim is still this one's, however the listener says it is unmarked", () => {
    expect(roleHeldBy(answer("ses_a", "proc-one"), "ses_a", "proc-one")).toBe(true);
    expect(
      roleHeldBy({ ...answer("ses_a", "proc-one"), handed_back: false }, "ses_a", "proc-one")
    ).toBe(true);
    // A listener predating the field sends none, which reads as no mark.
    expect(Object.hasOwn(answer("ses_a", "proc-one"), "handed_back")).toBe(false);
  });
});

describe("mayActOnRoleRecord", () => {
  test("a record this process wrote is acted on whether or not it holds a role", () => {
    expect(mayActOnRoleRecord("proc-one", "proc-one", true)).toBe(true);
    expect(mayActOnRoleRecord("proc-one", "proc-one", false)).toBe(true);
  });

  test("a record naming no writer is acted on as it always was, absent or empty alike", () => {
    expect(mayActOnRoleRecord(undefined, "proc-one", true)).toBe(true);
    expect(mayActOnRoleRecord("", "proc-one", true)).toBe(true);
  });

  test("a record another process wrote is acted on only while this one holds no role", () => {
    expect(mayActOnRoleRecord("another-process", "proc-one", true)).toBe(false);
    expect(mayActOnRoleRecord("another-process", "proc-one", false)).toBe(true);
  });

  test("a reader naming no process is another process than any record names one", () => {
    expect(mayActOnRoleRecord("another-process", undefined, true)).toBe(false);
    expect(mayActOnRoleRecord("another-process", "", true)).toBe(false);
    expect(mayActOnRoleRecord("", "", true)).toBe(true);
  });
});

/**
 * Every reader of an envelope a listener delivers in this package, given one that carries `role`:
 * the field the listener stores on a message Dispatch's API sent to a role's holder for a route to
 * that role, and echoes in the answer to that send. Each row reads one envelope with the field and
 * without it, and checks that the reader reads every other field as before. A reader that parses
 * with `EnvelopeSchema` knows the field now, and the builds released before it did not: the row
 * named "released builds" reads the envelope with the schema those builds parse with. A reader
 * with a schema of its own (`InboundEnvelopeSchema`, `DedupeIdentitySchema`) or with none names no
 * `role` in this build either, so its row is also how a released build reads the field.
 */
describe("an envelope that carries the role field", () => {
  const ROLE = "legion-legion-legion-1-architect";
  const ROLE_TOPIC = `notifications.role.${ROLE}`;
  const SESSION = "ses_target";
  const AGENT = `notifications.agent.${SESSION}`;
  /** Every field `EnvelopeSchema` names except `role`, each one set, on `SESSION`'s own subject. */
  const withoutRole = {
    ...(envelopeWithoutRole as Omit<Envelope, "role">),
    topic: AGENT,
    dedupe_key: `agent.${SESSION}.0123456789abcdef0123456789abcdef`,
  } satisfies Omit<Envelope, "role">;
  const withRole = { ...withoutRole, role: ROLE };

  /**
   * Checks that `read`, one reader's read of `withRole`, carries the field on its envelope and is
   * otherwise `expected`, the same reader's read of `withoutRole`.
   */
  function expectReadAsWithout(read: { readonly envelope?: object }, expected: unknown): void {
    const { role, ...envelope } = (read.envelope ?? {}) as Record<string, unknown>;
    expect(role).toBe(ROLE);
    const rest: unknown = { ...read, envelope };
    expect(rest).toEqual(expected);
  }

  const targetedDispatchPayload = readFileSync(
    new URL("../../../contracts/fixtures/dispatch-targeted-delivery.json", import.meta.url),
    "utf8"
  );

  test("the envelope these rows read names every field of the contract, and the contract reads it whole", () => {
    expect(Object.keys(withRole).sort()).toEqual(Object.keys(EnvelopeSchema.shape).sort());
    expect(EnvelopeSchema.parse(withRole)).toEqual(withRole);
  });

  // Commit 9d258a95ea added `role` to `EnvelopeSchema` and changed nothing else in it, so the
  // schema without that key is the one every build released before it parses with, and the one a
  // send's and a publish's answer extend there (`EnvelopeResponseSchema`, `PublishResponseSchema`).
  // `z.object` strips a key it does not name, and `.extend` keeps that.
  test("released builds (EnvelopeSchema before the field): the field is dropped, and every other field reads as before", () => {
    const released = EnvelopeSchema.omit({ role: true });
    const read = released.parse(withRole);

    expect("role" in read).toBe(false);
    expect(read).toEqual(released.parse(withoutRole));
    expect(read).toEqual(withoutRole);
  });

  /** What a send's answer adds to the envelope it echoes (`sendResponse`, `cmd/listener/api.go`). */
  const sendAnswer = {
    recipient: SESSION,
    recipient_state: "unresponsive",
    state: "connected",
    last_seen: Date.parse("2026-11-18T08:58:00Z"),
    duplicate: false,
  } as const;

  async function sendAnswered(envelope: object) {
    const recorded = recordFetch([jsonResponse({ ...envelope, ...sendAnswer })]);
    const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
    return delivered(
      await client.send({
        sourceSessionID: "ses_sender",
        targetSessionID: SESSION,
        message: "Review the plan",
      })
    );
  }

  test("send (transport.ts, EnvelopeResponseSchema): an answer with the field reads as the answer without it, and its tool line is the same", async () => {
    const before = await sendAnswered(withoutRole);
    const after = await sendAnswered(withRole);

    expectReadAsWithout(after, before);
    expect(before.envelope).toEqual({ ...withoutRole, ...sendAnswer });
    expect(before).toMatchObject({
      recipient: SESSION,
      confirmed: true,
      recipientState: "unresponsive",
      state: "connected",
      lastSeen: sendAnswer.last_seen,
      duplicate: false,
    });
    expect(sendConfirmationText(after)).toBe(sendConfirmationText(before));
  });

  for (const [label, status, answer] of [
    ["stored for the role's live holder", 200, { holder: "ses_holder", holder_state: "live" }],
    [
      "kept for the role's next holder",
      202,
      {
        holder: "ses_holder",
        holder_state: "unresponsive",
        last_seen: Date.parse("2026-11-18T08:50:00Z"),
        kept: true,
        kept_reason: "holder_lapsed",
        kept_until: Date.parse("2026-11-18T10:00:00Z"),
      },
    ],
  ] as const) {
    test(`publish (transport.ts, PublishResponseSchema): an answer ${label} with the field reads as the answer without it, and its tool line and details are the same`, async () => {
      const published = async (envelope: object) => {
        const recorded = recordFetch([
          Response.json({ ...envelope, topic: ROLE_TOPIC, ...answer }, { status }),
        ]);
        const client = createEnvoyClient({ baseUrl: "http://listener", fetch: recorded.fetch });
        return delivered(
          await client.publish({
            sourceSessionID: "ses_sender",
            topic: ROLE_TOPIC,
            message: "Review the plan",
          })
        );
      };
      const before = await published(withoutRole);
      const after = await published(withRole);

      expectReadAsWithout(after, before);
      expect(before.envelope).toEqual({ ...withoutRole, topic: ROLE_TOPIC, ...answer });
      expect(before.holder).toBe("ses_holder");
      expect(publishConfirmationText(after, { senderNamed: true })).toBe(
        publishConfirmationText(before, { senderNamed: true })
      );
      expect(publishResultDetails(after)).toEqual(publishResultDetails(before));
    });
  }

  test("renderInbound (delivery.ts, InboundEnvelopeSchema): a frame with the field renders the card it renders without it, and keeps the field on the envelope it hands on", () => {
    const before = renderInbound(JSON.stringify(withoutRole), SESSION, AGENT);
    const after = renderInbound(JSON.stringify(withRole), SESSION, AGENT);

    expectReadAsWithout(after, before);
    expect(before.envelope).toEqual(withoutRole);
    expect(before.content).toContain("id: evt-role-field");
    expect(before.content).not.toContain("unrecognised");
  });

  test("renderInbound (delivery.ts): a Dispatch delivery with the field, as Dispatch's API sends one for a route to a role, reads its delivery, actor and card as without it", () => {
    const dispatchFrame = (envelope: object) =>
      JSON.stringify({ ...envelope, source: "dispatch", payload: targetedDispatchPayload });
    const before = renderInbound(dispatchFrame(withoutRole), SESSION, AGENT);
    const after = renderInbound(dispatchFrame(withRole), SESSION, AGENT);

    expectReadAsWithout(after, before);
    expect(before.delivery).toMatchObject({ resource: "message", mode: "btw" });
    expect(before.dispatchActor).toEqual({ kind: "user", id: "alice" });
  });

  test("renderInbound's tolerant read (TolerantInboundEnvelopeSchema): a frame with the field and a member it cannot read renders as without the field", () => {
    const unreadable = { issued_at: "not a time" };
    const before = renderInbound(JSON.stringify({ ...withoutRole, ...unreadable }), SESSION, AGENT);
    const after = renderInbound(JSON.stringify({ ...withRole, ...unreadable }), SESSION, AGENT);

    expectReadAsWithout(after, before);
    expect(before.content).toContain("unrecognised: issued_at");
    expect(before.content).toContain("id: evt-role-field");
  });

  test("the relay's repeat check (consumer-delivery.ts parseIdentity, DedupeIdentitySchema): a frame with the field is looked up under the identity it has without it", () => {
    const lookedUp = (envelope: object) => {
      const asked: unknown[] = [];
      const replies: RelayReply[] = [];
      const delivery = createConsumerDelivery({
        sessionID: SESSION,
        host: { deliver: () => undefined, gap: () => undefined, log: () => undefined },
        publish: (_reply, body) => {
          replies.push(body);
        },
        ledger: {
          // Written already: the relay answers the frame done and hands the host nothing.
          delivered: (identity) => {
            asked.push(identity);
            return true;
          },
          liveFailed: async () => true,
        },
        persist: () => true,
        every: () => () => undefined,
      });
      const headers: Record<string, string> = {
        [RelayHeader.subject]: AGENT,
        [RelayHeader.streamSeq]: "7",
        [RelayHeader.streamID]: "stream-1",
        [RelayHeader.delivered]: "1",
        [RelayHeader.storedAt]: "2026-11-18T09:00:01Z",
      };
      delivery.receive({
        data: new TextEncoder().encode(JSON.stringify(envelope)),
        headers: { get: (name) => headers[name] ?? "" },
        reply: "_INBOX.7",
      });
      delivery.close();
      return { asked, replies };
    };
    const before = lookedUp(withoutRole);

    expect(lookedUp(withRole)).toEqual(before);
    expect(before).toEqual({
      asked: [
        {
          event_id: withoutRole.event_id,
          dedupe_key: withoutRole.dedupe_key,
          source: "agent",
          source_event_id: withoutRole.source_event_id,
        },
      ],
      replies: ["received", "done"],
    });
    expect(DedupeIdentitySchema.parse(withRole)).toEqual(DedupeIdentitySchema.parse(withoutRole));
  });

  test("the failure notice (delivery-ledger.ts liveFailed): a frame with the field that the relay gives up on is named as without it", async () => {
    const shown = async (envelope: object) => {
      const notices: FailureNotice[] = [];
      const ledger = createDeliveryLedger({
        host: {
          sessionID: () => SESSION,
          followedSubjects: () => [AGENT],
          showFailureNotice: async (_session, notice) => {
            notices.push(notice);
          },
          failureNoticeFailed: () => undefined,
          log: () => undefined,
          write: () => undefined,
        },
        now: () => Date.parse("2026-11-18T09:10:00Z"),
      });
      const rendered = renderInbound(JSON.stringify(envelope), SESSION, AGENT).envelope;
      if (rendered === undefined) throw new Error("the frame rendered no envelope");
      expect(
        await ledger.liveFailed(SESSION, rendered, new Error("the host refused the card"))
      ).toBe(true);
      return notices;
    };
    const before = await shown(withoutRole);
    const after = await shown(withRole);

    expect(after).toHaveLength(1);
    expect(before).toHaveLength(1);
    expectReadAsWithout(after[0] ?? {}, before[0]);
    expect(before[0]).toMatchObject({
      eventId: withoutRole.event_id,
      sender: "ses_sender (Planner)",
      summary: "Review the plan",
      publishedAt: "2026-11-18T09:00:00Z",
      readWith: "envoy_inbox",
    });
  });

  test("subscriptionRemovedTopics (dispatch-subscribe.ts): a removal frame with the field names the topics it names without it", () => {
    const removal = {
      ...withoutRole,
      source: "dispatch",
      payload: JSON.stringify({
        issue_key: "DSP-1",
        type: "subscription.removed",
        actor: { kind: "user", id: "alice" },
        payload: {
          session_id: SESSION,
          by: { kind: "user", id: "alice" },
          topics: ["notifications.dispatch.issue.DSP-1.>"],
        },
      }),
    };
    const before = subscriptionRemovedTopics(JSON.stringify(removal), SESSION);

    expect(before).toEqual(["notifications.dispatch.issue.DSP-1.>"]);
    expect(subscriptionRemovedTopics(JSON.stringify({ ...removal, role: ROLE }), SESSION)).toEqual(
      before
    );
  });
});
