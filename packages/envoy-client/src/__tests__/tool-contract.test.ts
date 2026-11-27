import { describe, expect, test } from "bun:test";
import {
  DELIVERY_DUPLICATE_WINDOW_HOURS,
  ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS,
  type SchemaApi,
  SESSION_LIVE_WINDOW_MS,
  SLACK_POST_TEXT_MAX,
  zodSchemaApi,
} from "@legion/contracts";
import { namesMissing, namesWhole } from "@legion/test-fixtures/names-whole";
import { z } from "zod";
import { delivered } from "../../test/delivered";
import { STORED_TOPIC_KINDS } from "../stream-topics";
import {
  envoyToolSpecs,
  MessageMetadataSchema,
  messageMetadataShape,
  NOT_SENT_GUIDE,
  PUBLISHED_DELIVERY,
  parseEnvoyToolArguments,
  publishConfirmationText,
  STORED_DELIVERY,
  sendConfirmationText,
  sessionsOnMachine,
  toMessageMetadata,
} from "../tool-contract";
import { ToolInputError } from "../tool-input-errors";
import {
  type ConnectionState,
  createEnvoyClient,
  type SessionState,
  UNRESPONSIVE_RULE,
} from "../transport";

/** The grace the unresponsive rule names, in minutes, from the constant the Dispatch server
 *  refuses a claim take by: the rule's text must carry that number, and a change of the constant
 *  must change the text rather than leave it saying a number nothing enforces. */
const GRACE_MINUTES = ISSUE_CLAIM_UNRESPONSIVE_GRACE_MS / 60_000;

/** The minutes a session stays live after its last registration, from the constant the listener
 *  reads a session's state by: a text that names the live window writes it as this number of
 *  minutes, and a change of the constant must change the text. */
const LIVE_WINDOW_MINUTES = SESSION_LIVE_WINDOW_MS / 60_000;

const schemaApi = zodSchemaApi(z);

describe("envoyToolSpecs", () => {
  test("defines the ten canonical Envoy tool names and slack_post", () => {
    expect(envoyToolSpecs.map((spec) => spec.name)).toEqual([
      "envoy_subscribe",
      "envoy_unsubscribe",
      "envoy_list",
      "envoy_inbox",
      "envoy_send",
      "envoy_publish",
      "envoy_role_set",
      "envoy_role_get",
      "envoy_whoami",
      "envoy_sessions",
      "slack_post",
    ]);
  });

  test("maps every tool descriptor to its harness-neutral operation", () => {
    expect(envoyToolSpecs.map((spec) => spec.operation)).toEqual([
      "subscribe",
      "unsubscribe",
      "listInterests",
      "inbox",
      "send",
      "publish",
      "setRole",
      "getRole",
      "whoami",
      "listSessions",
      "slackPost",
    ]);
  });

  test("exposes additive message metadata and listener session filters", () => {
    const send = envoyToolSpecs.find((spec) => spec.name === "envoy_send");
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");
    const sessions = envoyToolSpecs.find((spec) => spec.name === "envoy_sessions");

    expect(Object.keys(send?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "idempotency_key",
      "in_reply_to",
      "message",
      "session_id",
      "supersedes",
      "urgency",
    ]);
    expect(Object.keys(publish?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "expects_reply",
      "expires_at",
      "in_reply_to",
      "message",
      "supersedes",
      "topic",
      "urgency",
    ]);
    expect(Object.keys(sessions?.arguments(schemaApi) ?? {}).sort()).toEqual([
      "dir",
      "machine",
      "title",
    ]);
  });

  test("names every Envoy topic subject, and none of the retired ones", () => {
    const subscribe = envoyToolSpecs.find((spec) => spec.name === "envoy_subscribe");

    expect(subscribe?.description).toContain("agent.<session_id>");
    expect(subscribe?.description).toContain("role.<role>");
    expect(subscribe?.description).toContain("github.<owner>.<repo>.pr.<n>.>");
    for (const subject of ["pr.<n>.comment", "pr.<n>.review", "pr.<n>.mention", "pr.<n>.checks"]) {
      expect(subscribe?.description).toContain(subject);
    }
    // The pull request's lifecycle actions and the fields a closed one carries, each by name, and
    // the subject form a subscription's base is registered from.
    const lifecycle = [
      "opened",
      "synchronize",
      "closed",
      "merged",
      "merge_commit_sha",
      "merged_by",
      "head_sha",
    ];
    expect(namesMissing(subscribe?.description ?? "", lifecycle)).toEqual([]);
    expect(subscribe?.description).toContain("<subject>.>");
    expect(subscribe?.description).toContain("workflow.<file>.<action>");
    expect(namesWhole(subscribe?.description ?? "", "pr.<n>.check")).toBe(false);
    expect(subscribe?.description).not.toContain("pr.<n>.ci");
    expect(subscribe?.description).not.toContain("pr.<n>.checks.settled");
    expect(subscribe?.description).not.toContain("pr.<n>.merged");
    expect(subscribe?.description).not.toContain("pr.<n>.closed");
    expect(subscribe?.description).not.toContain("workflow.<file>.<action>.branch.");
    expect(subscribe?.description).toContain("slack.<team>.<channel>.thread.<ts>.message|mention");
    expect(subscribe?.description).toContain("ghostwispr.<session>.<kind>");
    expect(subscribe?.description).toContain("whatsapp.<phone>.<jid>.<kind>");
    expect(subscribe?.description).toContain("envoy.exceptions.<original-topic>");
  });

  // The texts that say what a subscription and a publish reach carry STORED_DELIVERY and
  // PUBLISHED_DELIVERY whole, each naming the 200 a stored publish answers. Their lists of kinds
  // are stream-topics.ts's STORED_TOPIC_KINDS, so no second list exists to hold.
  test("carries the stored-delivery clause in the subscribe tool and the published-delivery clause in the publish tool", () => {
    for (const [name, clause] of [
      ["envoy_subscribe", STORED_DELIVERY],
      ["envoy_publish", PUBLISHED_DELIVERY],
    ] as const) {
      expect(namesMissing(clause, ["200"]), name).toEqual([]);
      const description = envoyToolSpecs.find((spec) => spec.name === name)?.description ?? "";
      expect(description, name).toContain(clause);
    }
    // A subscribe is never refused for a topic no stream subject captures: the listener judges
    // capture on the deployed stream's subjects and answers a warning, which the tool shows.
    const subscribe = envoyToolSpecs.find((spec) => spec.name === "envoy_subscribe");
    expect(subscribe?.description).not.toContain("refused by name");
  });

  // R7 of dispatch://INFRA-3589: the listener's publish route refuses by name the agent topics,
  // Dispatch's and GitHub's lanes, the Legion wake subject and the listener's own lanes
  // (`publishHandler`). So the publish tool names each of them, and does not list every kind the
  // stream stores as stored by a publish, as the subscribe tool does.
  test("tells an agent each topic the listener refuses a publish to, and that every other topic the stream captures is stored", () => {
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish")?.description ?? "";
    const refused = [
      "notifications.dispatch.",
      "notifications.github.",
      "notifications.legion.wake",
      "notifications.envoy.kept.",
      "notifications.envoy.resume.",
      "notifications.envoy.takeover.",
    ];

    expect(namesMissing(publish, refused)).toEqual([]);
    expect(publish).not.toContain(STORED_TOPIC_KINDS.join(", "));
  });

  // The listener's 503 that says the message was not sent (`MESSAGE_NOT_SENT_PREFIX`): the bus
  // refused it before anything went out, so it is temporary and a resend stores it once. Both
  // tools carry NOT_SENT_GUIDE, the text that says so, so an agent neither waits on it as a send
  // that may have arrived nor gives up on the message. What the guide says is prose for the agent,
  // which no unit test holds.
  test("carries the guide to a message the listener did not send in both tools that send", () => {
    expect(NOT_SENT_GUIDE.trim()).not.toBe("");
    for (const name of ["envoy_send", "envoy_publish"]) {
      const description = envoyToolSpecs.find((spec) => spec.name === name)?.description ?? "";
      expect(description, name).toContain(NOT_SENT_GUIDE);
    }
  });

  // A text that tells a sender what a session's state means names the stopped and no_receiver
  // states a send answers with (`ConnectionState`), the live window in minutes and the duplicate
  // window in hours, each number from the constant that sets the window.
  test("names the recipient counts, the stopped and no_receiver states, the minutes of the live window and the hours of the duplicate window", () => {
    const publish = envoyToolSpecs.find((spec) => spec.name === "envoy_publish");
    const stopped: ConnectionState = "stopped";
    const noReceiver: ConnectionState = "no_receiver";
    const names = [
      stopped,
      noReceiver,
      `${LIVE_WINDOW_MINUTES} minutes`,
      `${DELIVERY_DUPLICATE_WINDOW_HOURS} hours`,
    ];

    for (const name of ["envoy_sessions", "envoy_role_get", "envoy_send", "envoy_publish"]) {
      const description = envoyToolSpecs.find((spec) => spec.name === name)?.description ?? "";
      expect(namesMissing(description, names), name).toEqual([]);
    }
    const counts = namesMissing(publish?.description ?? "", ["recipients", "recipients_connected"]);
    expect(counts).toEqual([]);
  });
});

describe("publishConfirmationText", () => {
  // The listener keeps a role message that no live session holds for one hour, and names the end
  // of that hour as kept_until. At that end it drops a copy no session took and sends the
  // publish's source session a message that names it (dispatch://INFRA-2846, R3). A listener of
  // an earlier release keeps for a time it does not name and sends no such message.
  test("a kept publish says until when it is kept", async () => {
    const keptUntil = Date.UTC(2026, 10, 15, 10, 30);
    const until = "2026-11-15T10:30:00.000Z";
    const lastSeen = Date.UTC(2026, 10, 15, 9, 20);
    const answers: Record<string, unknown>[] = [
      { kept: true, kept_reason: "unclaimed", kept_until: keptUntil },
      {
        kept: true,
        kept_reason: "holder_lapsed",
        kept_until: keptUntil,
        holder: "ses_reviewer",
        holder_state: "unresponsive",
        last_seen: lastSeen,
      },
      {
        kept: true,
        kept_reason: "holder_lapsed",
        kept_until: keptUntil,
        holder: "ses_merger",
        holder_state: "gone",
      },
      // A listener of an earlier release: kept, and no time named.
      { kept: true, kept_reason: "unclaimed" },
    ];
    const client = createEnvoyClient({
      baseUrl: "http://listener.test",
      fetch: async (_input, init) => {
        const body = JSON.parse(String(init?.body)) as { readonly topic: string };
        return new Response(
          JSON.stringify({
            event_id: "event-kept",
            source: "agent",
            source_event_id: "agent.ses_sender.event-kept",
            source_session: "ses_sender",
            topic: body.topic,
            dedupe_key: "agent.role.event-kept",
            issued_at: 3,
            payload_summary: "the worker is stalled",
            trace_id: "trace-kept",
            ...answers.shift(),
          }),
          { status: 202, headers: { "Content-Type": "application/json" } }
        );
      },
    });
    const publish = async (role: string) =>
      delivered(
        await client.publish({
          sourceSessionID: "ses_sender",
          topic: `notifications.role.${role}`,
          message: "the worker is stalled",
        })
      );
    const named = { senderNamed: true };

    // A role nobody claims: the line names kept_until as a UTC time, and says that a message
    // comes if no session takes the copy by then.
    const unclaimed = await publish("legion-architect");
    expect(unclaimed).toMatchObject({ kept: true, keptReason: "unclaimed", keptUntil });
    expect(publishConfirmationText(unclaimed, named)).toBe(
      `published event-kept. Envoy kept it for the role until ${until}: no live session holds the role now. The next session to hold the role before then gets it. If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late.`
    );
    // A publish that named no session is sent no such message, so its line promises none.
    const unnamed = publishConfirmationText(unclaimed, { senderNamed: false });
    expect(unnamed).toBe(
      `published event-kept. Envoy kept it for the role until ${until}: no live session holds the role now. The next session to hold the role before then gets it.`
    );

    // A holder that is unresponsive: the line names it and its state, and keeps the warning that
    // such a holder is not dead and that the sender starts no replacement for it.
    const unresponsive = await publish("reviewer");
    expect(unresponsive).toMatchObject({
      kept: true,
      keptReason: "holder_lapsed",
      keptUntil,
      holder: "ses_reviewer",
      holderState: "unresponsive",
      lastSeen,
    });
    const warned = publishConfirmationText(unresponsive, named);
    expect(warned).toBe(
      `published event-kept. Envoy kept it for the role until ${until}: its holder ses_reviewer is unresponsive, so no live session holds the role now. The next session to hold the role before then gets it. If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late. ${UNRESPONSIVE_RULE}`
    );
    expect(warned).toContain("An unresponsive session is not dead");
    expect(warned).toContain("Start no replacement for it");

    // A holder that is gone: named with its state, and with no warning, which is for an
    // unresponsive session alone.
    const gone = await publish("merger");
    expect(gone).toMatchObject({ keptReason: "holder_lapsed", holder: "ses_merger" });
    expect(gone.lastSeen).toBeUndefined();
    expect(publishConfirmationText(gone, named)).toBe(
      `published event-kept. Envoy kept it for the role until ${until}: its holder ses_merger is gone, so no live session holds the role now. The next session to hold the role before then gets it. If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late.`
    );

    // A listener of an earlier release names no time: the line says kept, with no time, and
    // promises neither a hand-over nor a message.
    const earlier = await publish("controller");
    expect(earlier).toMatchObject({ kept: true, keptReason: "unclaimed" });
    expect(earlier.keptUntil).toBeUndefined();
    const timeless = publishConfirmationText(earlier, named);
    expect(timeless).toBe(
      "published event-kept. Envoy kept it for the role: no live session holds the role now. Envoy did not say until when it keeps it."
    );

    // The tool's description names the result's kept_until field.
    const description =
      envoyToolSpecs.find((spec) => spec.name === "envoy_publish")?.description ?? "";
    expect(description).toContain("kept_until");
  });

  test("names the holder a delivered role message reached, and nothing else for a plain topic", () => {
    expect(
      publishConfirmationText(
        {
          envelope: { event_id: "event-held", topic: "notifications.role.architect" },
          holder: "ses_architect",
        },
        { senderNamed: true }
      )
    ).toBe("published event-held. Its role holder is ses_architect.");
    expect(
      publishConfirmationText(
        { envelope: { event_id: "event-team", topic: "notifications.team.deploy-squad.all" } },
        { senderNamed: true }
      )
    ).toBe("published event-team.");
  });

  // The description is what an agent reads at the moment it decides whether a peer is alive. The
  // old wording ("all live sessions", "up to 5 minutes") said a quiet session was a dead one, and
  // agents acted on it: role claims taken, replacements started (dispatch://INFRA-392).
  test("every tool that names another session carries the unresponsive rule", () => {
    const named = ["envoy_sessions", "envoy_role_get", "envoy_send", "envoy_publish"];

    for (const name of named) {
      const description = envoyToolSpecs.find((spec) => spec.name === name)?.description ?? "";
      expect(description).toContain(UNRESPONSIVE_RULE);
      const states: SessionState[] = ["live", "unresponsive", "gone"];
      expect(namesMissing(description, states), name).toEqual([]);
    }
  });
});

describe("sendConfirmationText", () => {
  const envelope = { event_id: "evt_1" };
  const connected = { state: "connected" } as const;

  test("names the recipient's state, and tells the sender an unresponsive one is not a failure", () => {
    expect(
      sendConfirmationText({
        ...connected,
        envelope,
        recipient: "ses_peer",
        recipientState: "live",
        confirmed: true,
        duplicate: false,
      })
    ).toBe("sent evt_1 to ses_peer (live)");
    expect(
      sendConfirmationText({
        ...connected,
        envelope,
        recipient: "ses_peer",
        recipientState: "unresponsive",
        confirmed: false,
        duplicate: false,
      })
    ).toBe(
      "sent evt_1 to ses_peer (unresponsive, so it is stored until that session answers; An unresponsive session is not dead: it keeps its role claims and its issue claims, and a message sent to it is stored until it answers. Start no replacement for it and take no claim from it until it is gone, " +
        `or until ${GRACE_MINUTES} minutes have passed since its last_seen, the time of its last registration.) (recipient unconfirmed by listener)`
    );
  });

  // The listener sends the recipient's last registration beside an unresponsive state, and a
  // sender deciding whether to wait reads how long the peer has been silent, not only that it is
  // (dispatch://INFRA-392, R7).
  test("says since when an unresponsive recipient has been silent, from the listener's last_seen", () => {
    expect(
      sendConfirmationText({
        ...connected,
        envelope,
        recipient: "ses_peer",
        recipientState: "unresponsive",
        confirmed: true,
        lastSeen: 1_762_000_000_000,
        duplicate: false,
      })
    ).toContain("(unresponsive since 2025-11-01T12:26:40.000Z, so it is stored until");
  });
});

describe("publishConfirmationText", () => {
  const envelope = { event_id: "evt_1", topic: "notifications.role.reviewer" };

  test("names a role publish's holder and the state the listener resolved it in", () => {
    expect(
      publishConfirmationText(
        { envelope, holder: "ses_holder", holderState: "live" },
        { senderNamed: true }
      )
    ).toBe("published evt_1. Its role holder is ses_holder (live).");
    // A listener predating the field names the holder alone, and the line says only that.
    expect(publishConfirmationText({ envelope, holder: "ses_holder" }, { senderNamed: true })).toBe(
      "published evt_1. Its role holder is ses_holder."
    );
  });

  // A topic publish from a listener that sends no count (one that cannot read the known-session
  // bucket) is answered with the envelope alone, so the line names the event and nothing else.
  test("says only that a topic publish was published", () => {
    expect(
      publishConfirmationText(
        { envelope: { event_id: "evt_1", topic: "notifications.team.deploy-squad.all" } },
        { senderNamed: true }
      )
    ).toBe("published evt_1.");
  });
});

describe("envoy_send resend", () => {
  const key = "0f6c1e3a-7d2b-4c8e-9a51-3b7e2d4f6a80";

  test("takes the idempotency_key an unconfirmed send named", () => {
    expect(
      parseEnvoyToolArguments("send", { session_id: "ses_b", message: "hi", idempotency_key: key })
    ).toEqual({ session_id: "ses_b", message: "hi", idempotency_key: key });
  });

  test("refuses a key the listener would not store once", () => {
    for (const idempotency_key of ["retry-1", key.toUpperCase(), `${key}0`]) {
      expect(() =>
        parseEnvoyToolArguments("send", { session_id: "ses_b", message: "hi", idempotency_key })
      ).toThrow(ToolInputError);
    }
  });

  test("says a resend the listener already held stored no second copy", () => {
    const envelope = { event_id: "event-2" };
    const sent = {
      envelope,
      recipient: "ses_b",
      recipientState: "live",
      confirmed: true,
      state: "connected",
    } as const;
    expect(sendConfirmationText({ ...sent, duplicate: true })).toBe(
      "sent to ses_b once: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy"
    );
    expect(sendConfirmationText({ ...sent, duplicate: false })).toBe(
      "sent event-2 to ses_b (live)"
    );
  });

  test("a resend for a stopped target is queued once, and never says sent", () => {
    const text = sendConfirmationText({
      envelope: { event_id: "event-2" },
      recipient: "ses_b",
      recipientState: "live",
      confirmed: true,
      state: "stopped",
      lastSeen: Date.UTC(2026, 10, 2, 9, 15),
      duplicate: true,
    });

    expect(text).toBe(
      "queued once for ses_b: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy; the session is stopped (last seen 2026-11-02T09:15:00Z); if it runs again within 72 hours, it gets the message"
    );
    // The one stored copy waits on a session that is not running, so the line says queued and
    // leaves out the event id of the copy this resend did not store.
    expect(text).not.toContain("sent");
    expect(text).not.toContain("event-2");
  });
});

describe("message metadata", () => {
  test("maps validated wire fields to the client input shape", () => {
    const metadata = MessageMetadataSchema.parse({
      in_reply_to: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expects_reply: "required",
      expires_at: 1_788_956_000_000,
    });

    expect(toMessageMetadata(metadata)).toEqual({
      inReplyTo: "event-before",
      supersedes: "event-obsolete",
      urgency: "blocking",
      expectsReply: "required",
      expiresAt: 1_788_956_000_000,
    });
  });

  test("rejects fractional metadata expiration", () => {
    const schema = z.object(messageMetadataShape(zodSchemaApi(z)) as z.ZodRawShape);

    expect(schema.safeParse({ expires_at: 1.5 }).success).toBe(false);
  });

  test("identifies an invalid metadata enum by field", () => {
    const parsed = MessageMetadataSchema.safeParse({ urgency: "urgent" });
    if (parsed.success) throw new Error("invalid urgency unexpectedly parsed");

    expect(parsed.error.issues).toEqual([
      expect.objectContaining({
        path: ["urgency"],
        message: 'Invalid option: expected one of "low"|"med"|"high"|"blocking"',
      }),
    ]);
  });

  // Oh My Pi's `pi.zod` facade drops a description attached before `.optional()` on an enum or
  // on a field widened to `unknown`, which real Zod renders identically, so only the call order
  // shows it: on that host `expects_reply` and `urgency` lost the only text that names their
  // values and their meaning.
  test("every described Envoy tool field describes last, the one order the OMP facade keeps", () => {
    type Recorder = {
      readonly calls: readonly string[];
      optional(): Recorder;
      nullable(): Recorder;
      describe(text: string): Recorder;
    };
    const node = (calls: readonly string[]): Recorder => ({
      calls,
      optional: () => node([...calls, "optional"]),
      nullable: () => node([...calls, "nullable"]),
      describe: () => node([...calls, "describe"]),
    });
    const leaf = () => node([]);
    const api: SchemaApi<Recorder> = {
      string: leaf,
      number: leaf,
      boolean: leaf,
      enum: leaf,
      literal: leaf,
      array: leaf,
      unknown: leaf,
      object: leaf,
      refineObject: leaf,
    };

    const described = envoyToolSpecs.flatMap((spec) =>
      Object.entries(spec.arguments(api) as Record<string, Recorder>)
        .filter(([, field]) => field.calls.includes("describe"))
        .map(([name, field]) => [`${spec.name}.${name}`, field.calls.at(-1)] as const)
    );

    expect(described.map(([name]) => name)).toContain("envoy_send.expects_reply");
    expect(described.filter(([, last]) => last !== "describe")).toEqual([]);
  });

  test("refuses an Envoy tool call once with every problem named", () => {
    const failure = (() => {
      try {
        parseEnvoyToolArguments("send", { session_id: "s1", expects_reply: "no", msg: "hi" });
        return undefined;
      } catch (error) {
        return error;
      }
    })();
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "message is required (string)",
      'expects_reply must be one of false|optional|required; got "no"',
      'unknown field "msg"; allowed: session_id, message, in_reply_to, supersedes, urgency, expects_reply, expires_at, idempotency_key',
    ]);
    expect(failure.message).toContain("envoy_send was not called: 3 problems");
  });

  test("returns the typed arguments of a valid Envoy tool call", () => {
    expect(
      parseEnvoyToolArguments("publish", { topic: "t", message: "m", urgency: "high" })
    ).toEqual({ topic: "t", message: "m", urgency: "high" });
  });

  test("takes a Slack reply's channel, thread and text, the text at most the listener's cap", () => {
    const reply = {
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      text: "Filed as ACME-12.",
    };
    expect(parseEnvoyToolArguments("slackPost", reply)).toEqual(reply);
    const failure = (() => {
      try {
        parseEnvoyToolArguments("slackPost", {
          channel: "C00000000",
          text: "a".repeat(SLACK_POST_TEXT_MAX + 1),
        });
        return undefined;
      } catch (error) {
        return error;
      }
    })();
    if (!(failure instanceof ToolInputError)) throw new Error("expected ToolInputError");
    expect(failure.problems).toEqual([
      "thread_ts is required (string)",
      `text is 1 characters over the ${SLACK_POST_TEXT_MAX}-character limit (${SLACK_POST_TEXT_MAX + 1}/${SLACK_POST_TEXT_MAX})`,
    ]);
    expect(failure.message).toContain("slack_post was not called: 2 problems");
  });
});

describe("sessionsOnMachine", () => {
  const session = (id: string, machineID: string, host: string) => ({
    session_id: id,
    machine_id: machineID,
    host,
    dir: "/work",
    port: 0,
    title: "",
    topics: [],
    roles: [],
    capabilities: [],
    self_subscribed: true,
    state: "live" as const,
  });
  const onHub = session("ses_on_hub", "envoy-hub", "gke-node-7");
  const onDevbox = session("ses_on_devbox", "devbox", "devbox");
  const sessions = [onHub, onDevbox];

  test("finds a session by the machine it runs on and by the listener it registered through", () => {
    expect(sessionsOnMachine(sessions, "envoy-hub")).toEqual([onHub]);
    expect(sessionsOnMachine(sessions, "gke-node-7")).toEqual([onHub]);
    expect(sessionsOnMachine(sessions, "devbox")).toEqual([onDevbox]);
    expect(sessionsOnMachine(sessions, "other")).toEqual([]);
  });

  test("no filter lists every session", () => {
    expect(sessionsOnMachine(sessions, undefined)).toEqual(sessions);
    expect(sessionsOnMachine(sessions, "")).toEqual(sessions);
  });

  test("a session an earlier listener reported with no host still matches its listener", () => {
    const hostless = {
      session_id: "ses_old",
      machine_id: "devbox",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      roles: [],
      capabilities: [],
      self_subscribed: true,
      state: "live" as const,
    };

    expect(sessionsOnMachine([hostless], "devbox")).toEqual([hostless]);
    expect(sessionsOnMachine([hostless], "gke-node-7")).toEqual([]);
  });
});

describe("sendConfirmationText for a state", () => {
  const connected = {
    envelope: { event_id: "evt_01" },
    recipient: "ses_target",
    recipientState: "live",
    confirmed: true,
    state: "connected",
    duplicate: false,
  } as const;
  // 2026-11-02T09:15:00Z, as the listener sends it: unix milliseconds.
  const lastSeen = Date.UTC(2026, 10, 2, 9, 15);

  test("a connected target is sent to with its registry state, as before the state existed", () => {
    expect(sendConfirmationText(connected)).toBe("sent evt_01 to ses_target (live)");
    expect(sendConfirmationText({ ...connected, confirmed: false })).toBe(
      "sent evt_01 to ses_target (live) (recipient unconfirmed by listener)"
    );
  });

  test("a stopped target is queued, with when it was last seen as a UTC instant", () => {
    const text = sendConfirmationText({ ...connected, state: "stopped", lastSeen });

    expect(text).toBe(
      "queued evt_01 for ses_target: the session is stopped (last seen 2026-11-02T09:15:00Z); if it runs again within 72 hours, it gets the message"
    );
    // A model that reads "sent" waits for an answer from a session that is not running, so the
    // queued line says nothing of the kind.
    expect(text).not.toContain("sent");
  });

  // The probe wins over the registry's age: an entry whose plugin is gone answers stopped with
  // `recipient_state` still live or unresponsive, and one past the gone horizon with a record
  // answers gone. Either way the line is the queued one.
  test("a stopped target is queued whatever its registry state", () => {
    for (const recipientState of ["live", "unresponsive", "gone"] as const) {
      const text = sendConfirmationText({
        ...connected,
        recipientState,
        state: "stopped",
        lastSeen,
      });

      expect(text).toMatch(/^queued \S+ for /);
      expect(text).not.toContain("sent");
      expect(text).not.toContain(recipientState);
    }
  });

  test("a stopped target the listener gives no last seen for is still queued", () => {
    const text = sendConfirmationText({ ...connected, state: "stopped" });

    expect(text).toBe(
      "queued evt_01 for ses_target: the session is stopped; if it runs again within 72 hours, it gets the message"
    );
    expect(text).not.toContain("sent");
  });

  // A Claude Code terminal with no mod runs and holds its relay subject, but nothing writes a
  // message into its conversation. The listener answers no_receiver and names no registry state,
  // and the line must not call that session live (dispatch://INFRA-3589, R12).
  const noReceiver = {
    envelope: { event_id: "evt_01" },
    recipient: "ses_target",
    confirmed: true,
    state: "no_receiver",
    duplicate: false,
  } as const;

  test("a target with no conversation receiver is queued, and the line never says live", () => {
    const text = sendConfirmationText(noReceiver);

    expect(text).toBe(
      "queued evt_01 for ses_target: the session runs but takes no message into its conversation now; the message is stored, and if the session takes messages within 72 hours, it gets the message"
    );
    expect(text).not.toContain("live");
    expect(text).not.toContain("sent");
  });

  test("a resend to a target with no conversation receiver is queued once, and never says live", () => {
    const text = sendConfirmationText({ ...noReceiver, duplicate: true });

    expect(text).toBe(
      "queued once for ses_target: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy; the session runs but takes no message into its conversation now; the message is stored, and if the session takes messages within 72 hours, it gets the message"
    );
    expect(text).not.toContain("live");
    expect(text).not.toContain("evt_01");
  });
});

describe("publishConfirmationText counts", () => {
  const envelope = { event_id: "evt_02", topic: "notifications.team.deploy-squad.all" };
  const named = { senderNamed: true };

  test("counts the other sessions that get the message, and how many run now", () => {
    const text = publishConfirmationText(
      { envelope, recipients: 3, recipientsConnected: 2 },
      named
    );

    expect(text).toBe("published evt_02 to 3 other sessions (2 running now)");
  });

  test("one other session takes the singular", () => {
    expect(
      publishConfirmationText({ envelope, recipients: 1, recipientsConnected: 1 }, named)
    ).toBe("published evt_02 to 1 other session (1 running now)");
    expect(
      publishConfirmationText({ envelope, recipients: 1, recipientsConnected: 0 }, named)
    ).toBe("published evt_02 to 1 other session (0 running now)");
  });

  test("a count of none says the message reached nobody, and names the topic", () => {
    const text = publishConfirmationText(
      { envelope, recipients: 0, recipientsConnected: 0 },
      named
    );

    expect(text).toBe(
      "published evt_02; no other session subscribes to notifications.team.deploy-squad.all, so no other session gets it"
    );
  });
});
