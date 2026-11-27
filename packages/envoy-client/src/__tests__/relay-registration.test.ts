// packages/envoy-client/src/__tests__/relay-registration.test.ts
//
// One registration of a session that holds a relay (`../relay-registration.ts`): the topics it
// carries, the outage told once, and the topics an edge listener refuses for the projects of the
// session's token dropped while every other refusal stays a refusal. Then the same registrations
// beside a relay socket (`../listener-relay.ts`) on the stand-in of
// `../../test/relay-socket-stand-in.ts`: the outage report the two share, and a refused topic
// dropped while a kept one goes on being delivered.

import { describe, expect, test } from "bun:test";
import {
  frame,
  opens,
  registerOnce,
  relaySocketFixtures,
  until,
} from "../../test/relay-socket-fixtures";
import { startRelaySocketStandIn } from "../../test/relay-socket-stand-in";
import { consumerDelivery, projectRefusal } from "../../test/subscribe-answer";
import { brokerCredential } from "../broker-credential";
import { createConsumerDelivery } from "../consumer-delivery";
import {
  type InboundUnavailable,
  inboundRestoredNoticeText,
  inboundUnavailable,
} from "../inbound-outage";
import {
  NoRelaySubjectError,
  type RegisteringRelay,
  RegistrationStoppingError,
  registerWithRelay,
} from "../relay-registration";
import { createEnvoyClient, EnvoyApiError, expandSubscriptionTopics } from "../transport";

const { started, standIn, podEnvironment, opened } = relaySocketFixtures("relay-registration");

const encoder = new TextEncoder();

const AGENT = "notifications.agent.ses_a";
const ISSUE_TOPIC = "notifications.dispatch.issue.DSP-1.>";

describe("one registration", () => {
  const UNCAPTURED = "notifications.team";
  const TEAM = "notifications.team.deploys";

  /** A relay that records what a registration stamped on it, and what it forgot. */
  function fakeRelay() {
    const followed: string[][] = [];
    const unfollowed: string[][] = [];
    return {
      followed,
      unfollowed,
      setFollowed: (topics: Iterable<string>) => followed.push([...topics]),
      unfollow: (topics: Iterable<string>) => unfollowed.push([...topics]),
      registration: () => ({ consumer: true, epoch: "epoch-1" }) as const,
    };
  }

  /** What a registration hands its host besides the topics: nothing a row below reads. */
  const quiet = { drop: () => undefined, log: () => undefined } as const;

  /** The listener's refusal of a registration, with this status and body. */
  function refusal(status: number, body: Readonly<Record<string, unknown>>): EnvoyApiError {
    return new EnvoyApiError({
      method: "POST",
      url: "http://listener.example/v1/interests/subscribe",
      status,
      responseBody: JSON.stringify(body),
    });
  }

  /**
   * Two registrations that overlap, both refused `denied`: the first waits on its answer while
   * `meanwhile` changes what the session follows and the second registers, refused and resent;
   * then the first is refused and resends. Answers every topic list each sent, in order.
   */
  async function overlappingRefusals(
    relay: RegisteringRelay,
    followed: Set<string>,
    denied: string,
    meanwhile: () => void
  ): Promise<(readonly string[])[]> {
    const sent: (readonly string[])[] = [];
    const refusedNow = () => refusal(403, projectRefusal([denied]));
    const registration = (answer: (topics: readonly string[]) => Promise<string>) =>
      registerWithRelay({
        relay,
        ownAgentSubject: AGENT,
        followed: () => followed,
        outage: inboundUnavailable(() => undefined),
        send: (topics) => {
          sent.push(topics);
          return answer(topics);
        },
        answered: () => undefined,
        drop: (topics) => {
          for (const topic of topics) followed.delete(topic);
        },
        log: () => undefined,
      });
    const firstAnswer = Promise.withResolvers<string>();
    let firstSent = false;
    const first = registration(() => {
      if (firstSent) return Promise.resolve("registered again");
      firstSent = true;
      return firstAnswer.promise;
    });
    meanwhile();
    await registration((topics) =>
      topics.includes(denied) ? Promise.reject(refusedNow()) : Promise.resolve("registered")
    );
    firstAnswer.reject(refusedNow());
    await first;
    return sent;
  }

  test("carries the topics the listener will store, and takes the answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];
    const sent: { topics: readonly string[]; epoch: string }[] = [];

    const result = await registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      // The bare base a stored row or a transcript brings back: the listener stores it for no
      // session and warns about it, so it never goes out.
      followed: () => [TEAM, UNCAPTURED, "notifications.agent.ses_b"],
      outage: inboundUnavailable(({ why }) => told.push(why), {
        restored: () => told.push("restored"),
      }),
      send: (topics, delivery) => {
        sent.push({ topics, epoch: delivery.epoch });
        return Promise.resolve("registered");
      },
      answered: (answer) => answers.push(answer),
      ...quiet,
    });

    expect(sent).toEqual([{ topics: [AGENT, TEAM], epoch: "epoch-1" }]);
    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([result, answers, told]).toEqual(["registered", ["registered"], []]);
  });

  test("a session whose relay could not be opened says why in the refusal and in the one outage notice", async () => {
    const told: string[] = [];
    const refusal = await registerWithRelay({
      relay: undefined,
      relayMissing: "Authentication Timeout",
      ownAgentSubject: AGENT,
      followed: () => [TEAM],
      // With no bound the start race is told at once, so this row reads the one notice it tells.
      outage: inboundUnavailable(({ why }) => told.push(why), {
        firstOpenBoundMs: 0,
        restored: () => told.push("restored"),
      }),
      send: () => Promise.resolve("registered"),
      answered: () => undefined,
      ...quiet,
    }).catch((error: unknown) => error);

    const expected =
      "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing, because its relay subject could not be opened: Authentication Timeout";
    if (!(refusal instanceof NoRelaySubjectError))
      throw new Error(`no refusal: ${String(refusal)}`);
    expect(refusal.message).toBe(expected);
    expect(told).toEqual([expected]);
  });

  test("a refused registration tells the session once and takes no answer", async () => {
    const relay = fakeRelay();
    const told: string[] = [];
    const answers: string[] = [];

    await expect(
      registerWithRelay({
        relay,
        ownAgentSubject: AGENT,
        followed: () => [TEAM],
        outage: inboundUnavailable(({ why }) => told.push(why), {
          restored: () => told.push("restored"),
        }),
        send: () => Promise.reject(new Error("no_grant")),
        answered: (answer: string) => answers.push(answer),
        ...quiet,
      })
    ).rejects.toThrow("no_grant");

    expect(relay.followed).toEqual([[AGENT, TEAM]]);
    expect([answers, told]).toEqual([[], ["no_grant"]]);
  });

  test("a registration accepted after an outage tells the session once that it receives again", async () => {
    const told: string[] = [];
    const outage = inboundUnavailable(({ why }) => told.push(why), {
      restored: () => told.push("restored"),
    });
    let refuse = true;
    const register = () =>
      registerWithRelay({
        relay: fakeRelay(),
        ownAgentSubject: AGENT,
        followed: () => [TEAM],
        outage,
        send: () =>
          refuse ? Promise.reject(new Error("no_grant")) : Promise.resolve("registered"),
        answered: () => undefined,
        ...quiet,
      }).catch(() => undefined);

    await register();
    await register();
    refuse = false;
    await register();
    // A heartbeat that is accepted again says nothing: the session already knows.
    await register();
    expect(told).toEqual(["no_grant", "restored"]);
    // A second outage is told again, and its end too.
    refuse = true;
    await register();
    refuse = false;
    await register();
    expect(told).toEqual(["no_grant", "restored", "no_grant", "restored"]);
    expect(inboundRestoredNoticeText("refused")).toContain("Envoy inbound restored");
  });

  test("any other refusal is a refusal as before: nothing dropped, nothing sent again", async () => {
    // The edge's own refusals that are not a project's: 503 `repositories_unknown` (it could not
    // read which repositories the session's projects hold; every topic is kept and the heartbeat
    // asks again) and 403 `token_not_admitted` (the token's fault, on any route). And a 403 whose
    // reason is not one of the two project refusals though it names topics, and one of those two
    // that names none. None drops a topic or sends again.
    for (const [status, body] of [
      [
        503,
        { error: "the project's repositories could not be read", reason: "repositories_unknown" },
      ],
      [403, { error: "the token is not admitted here", reason: "token_not_admitted" }],
      [403, { error: "bound to another session", reason: "session_not_bound", refused: [TEAM] }],
      [403, { error: "no project", reason: "topic_not_in_project" }],
    ] as const) {
      const told: string[] = [];
      const dropped: (readonly string[])[] = [];
      const sent: (readonly string[])[] = [];
      const refused = refusal(status, body);

      const outcome = await registerWithRelay({
        relay: fakeRelay(),
        ownAgentSubject: AGENT,
        followed: () => [TEAM],
        outage: inboundUnavailable(({ why }) => told.push(why), {
          restored: () => told.push("restored"),
        }),
        send: (topics) => {
          sent.push(topics);
          return Promise.reject(refused);
        },
        answered: () => undefined,
        drop: (topics) => dropped.push(topics),
        log: () => undefined,
      }).catch((error: unknown) => error);

      const answer = `${status} ${body.reason}`;
      expect(outcome).toBe(refused);
      expect({ answer, sent, dropped, told }).toEqual({
        answer,
        sent: [[AGENT, TEAM]],
        dropped: [],
        told: [refused.message],
      });
    }
  });

  test("a project refusal that names only the session's own subject drops nothing and is told", async () => {
    // The session's own agent subject is in every registration (`registrationTopics`): a refusal
    // naming it, or a topic the registration did not carry, leaves nothing to drop, so the
    // registration fails as any other refusal does rather than being sent again unchanged.
    const told: string[] = [];
    const dropped: (readonly string[])[] = [];
    const sent: (readonly string[])[] = [];
    const refused = refusal(
      403,
      projectRefusal([AGENT, "notifications.slack.alerts"], "topic_not_in_project")
    );

    const outcome = await registerWithRelay({
      relay: fakeRelay(),
      ownAgentSubject: AGENT,
      followed: () => [TEAM],
      outage: inboundUnavailable(({ why }) => told.push(why), {
        restored: () => told.push("restored"),
      }),
      send: (topics) => {
        sent.push(topics);
        return Promise.reject(refused);
      },
      answered: () => undefined,
      drop: (topics) => dropped.push(topics),
      log: () => undefined,
    }).catch((error: unknown) => error);

    expect(outcome).toBe(refused);
    expect([sent, dropped, told]).toEqual([[[AGENT, TEAM]], [], [refused.message]]);
  });

  test("a topic_not_in_project refusal naming a followed topic drops exactly it and registers the rest", async () => {
    // An edge listener answers a topic of no project of the token's (a Slack, team or WhatsApp
    // lane) 403 `topic_not_in_project`, naming it in `refused`: what a real edge listener answers a
    // registration following `notifications.slack.alerts`. It is dropped as the GitHub refusal
    // (`repository_not_in_project`) is, and the rest are registered again at once.
    const SLACK = "notifications.slack.alerts";
    const told: string[] = [];
    const dropped: (readonly string[])[] = [];
    const logs: string[] = [];
    const sent: (readonly string[])[] = [];
    const relay = fakeRelay();

    const result = await registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      followed: () => [ISSUE_TOPIC, SLACK],
      outage: inboundUnavailable(({ why }) => told.push(why), {
        restored: () => told.push("restored"),
      }),
      send: (topics) => {
        sent.push(topics);
        return topics.includes(SLACK)
          ? Promise.reject(refusal(403, projectRefusal([SLACK], "topic_not_in_project")))
          : Promise.resolve("registered");
      },
      answered: () => undefined,
      drop: (topics) => dropped.push(topics),
      log: (line) => logs.push(line),
    });

    expect({ result, sent, dropped, told, unfollowed: relay.unfollowed }).toEqual({
      result: "registered",
      sent: [
        [AGENT, ISSUE_TOPIC, SLACK],
        [AGENT, ISSUE_TOPIC],
      ],
      dropped: [[SLACK]],
      told: [],
      // The relay forgets the refused topic alone; what it follows otherwise stays as stamped.
      unfollowed: [[SLACK]],
    });
    expect(logs).toEqual([
      `the listener refused ${SLACK} for this session's projects (topic_not_in_project); the session no longer follows them and registers the rest`,
    ]);
  });

  test("a refusal answered after another registration added a topic keeps that topic, and its frame reaches the host", async () => {
    // Registrations overlap: one waits on its answer while the host follows a new topic and a
    // second registration of that topic is accepted. When the first is then refused a topic, only
    // that topic leaves the session's follows; the new one stays, and a frame on it is handed to
    // the host and answered done only after it was.
    const kept = "notifications.dispatch.issue.DSP-2.>";
    const added = "notifications.dispatch.issue.DSP-3.>";
    const denied = "notifications.github.acme.priv.>";
    const followed = new Set([kept, denied]);
    const handed: string[] = [];
    const receipts: string[] = [];
    const delivery = createConsumerDelivery({
      sessionID: "ses_a",
      host: {
        deliver: (frame) => {
          handed.push(frame.subject);
          frame.done();
        },
        gap: () => undefined,
        log: () => undefined,
        onTaskValue: () => undefined,
      },
      publish: (_reply, body) => {
        receipts.push(body);
      },
      ledger: { delivered: () => false, liveFailed: async () => true },
      persist: () => true,
    });
    await overlappingRefusals(delivery, followed, denied, () => followed.add(added));

    expect(Object.keys(delivery.snapshot().follows).sort()).toEqual([kept, added].sort());
    const headers: Record<string, string> = {
      "Envoy-Subject": "notifications.dispatch.issue.DSP-3.comment",
      "Envoy-Stream-Seq": "1",
      "Envoy-Stream-Id": "stream-1",
      "Envoy-Delivered": "1",
      "Envoy-Stored-At": new Date().toISOString(),
    };
    delivery.receive({
      reply: "notifications.envoy.relayreply.one",
      headers: { get: (name) => headers[name] ?? "" },
      data: new TextEncoder().encode(JSON.stringify({ event_id: "evt-1", source: "agent" })),
    });
    await until(
      () => receipts.includes("done"),
      5_000,
      () => `the frame was never answered done; receipts ${JSON.stringify(receipts)}`
    );
    expect(handed).toEqual(["notifications.dispatch.issue.DSP-3.comment"]);
    delivery.close();
  });

  test("a refusal answered after the session unsubscribed a topic resends without that topic", async () => {
    // The listener merges a registration's topics into the session's row, so a resend that named
    // a topic the session dropped while it waited would put the topic back. One registration
    // waits; the session unsubscribes a topic; a second registration is refused and resends; then
    // the first is refused, and its resend names neither the refused topic nor the dropped one.
    const kept = "notifications.dispatch.issue.DSP-4.>";
    const gone = "notifications.dispatch.issue.DSP-5.>";
    const denied = "notifications.github.acme.priv.>";
    const followed = new Set([kept, gone, denied]);
    const sent = await overlappingRefusals(fakeRelay(), followed, denied, () =>
      followed.delete(gone)
    );

    expect(sent).toEqual([
      [AGENT, kept, gone, denied],
      [AGENT, kept, denied],
      [AGENT, kept],
      [AGENT, kept],
    ]);
  });

  /**
   * One registration following ISSUE_TOPIC and SLACK whose first send the listener answers 403
   * `topic_not_in_project` naming SLACK, and every later send accepts. When `stopsMeanwhile`, the
   * host's stop begins while that first send waits. The host has one reading of its stop, which
   * its outage report reads too. Answers the refusal and what the registration came to.
   */
  async function refusedWhileStopping(stopsMeanwhile: boolean) {
    const SLACK = "notifications.slack.alerts";
    let stopping = false;
    const told: string[] = [];
    const sent: (readonly string[])[] = [];
    const dropped: (readonly string[])[] = [];
    const logs: string[] = [];
    const relay = fakeRelay();
    const refused = refusal(403, projectRefusal([SLACK], "topic_not_in_project"));
    const firstAnswer = Promise.withResolvers<string>();
    const registering = registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      followed: () => [ISSUE_TOPIC, SLACK],
      outage: inboundUnavailable(({ why }) => told.push(why), {
        restored: () => told.push("restored"),
        stopping: () => stopping,
      }),
      send: (topics) => {
        sent.push(topics);
        return sent.length === 1 ? firstAnswer.promise : Promise.resolve("registered");
      },
      answered: () => undefined,
      drop: (topics) => dropped.push(topics),
      log: (line) => logs.push(line),
      stopping: () => stopping,
    }).catch((error: unknown) => error);
    // The first send is on the wire. The stop begins, then the listener answers it.
    stopping = stopsMeanwhile;
    firstAnswer.reject(refused);
    const outcome = await registering;
    return { SLACK, refused, outcome, sent, dropped, logs, told, unfollowed: relay.unfollowed };
  }

  test("a project refusal answered once the host began stopping drops nothing, sends nothing again and tells nothing", async () => {
    // R24 of dispatch://INFRA-3589: a stopping session registers nothing and is told nothing. The
    // refusal is then a refusal as any other: the registration fails with it, and the topic it
    // names stays followed, since the session's record is deleted next.
    const { SLACK, refused, outcome, sent, dropped, logs, told, unfollowed } =
      await refusedWhileStopping(true);

    expect({ sent, dropped, logs, told, unfollowed }).toEqual({
      sent: [[AGENT, ISSUE_TOPIC, SLACK]],
      dropped: [],
      logs: [],
      told: [],
      unfollowed: [],
    });
    expect(outcome).toBe(refused);
  });

  test("the same project refusal answered while the host is not stopping drops the topic and registers the rest", async () => {
    // The control of the row above: only the stop differs.
    const { SLACK, outcome, sent, dropped, logs, told, unfollowed } =
      await refusedWhileStopping(false);

    expect({ outcome, sent, dropped, logs: logs.length, told, unfollowed }).toEqual({
      outcome: "registered",
      sent: [
        [AGENT, ISSUE_TOPIC, SLACK],
        [AGENT, ISSUE_TOPIC],
      ],
      dropped: [[SLACK]],
      logs: 1,
      told: [],
      unfollowed: [[SLACK]],
    });
  });

  test("a registration whose host began stopping before it was sent sends nothing and tells nothing", async () => {
    // This outage report reads no stop of its own, so it tells whatever it is handed: it tells
    // nothing only because the registration hands it nothing.
    const relay = fakeRelay();
    const told: string[] = [];
    const sent: (readonly string[])[] = [];
    const dropped: (readonly string[])[] = [];
    const logs: string[] = [];

    const outcome = await registerWithRelay({
      relay,
      ownAgentSubject: AGENT,
      followed: () => [TEAM],
      outage: inboundUnavailable(({ why }) => told.push(why), {
        restored: () => told.push("restored"),
      }),
      send: (topics) => {
        sent.push(topics);
        return Promise.resolve("registered");
      },
      answered: () => undefined,
      drop: (topics) => dropped.push(topics),
      log: (line) => logs.push(line),
      stopping: () => true,
    }).catch((error: unknown) => error);

    expect({ sent, dropped, logs, told, followed: relay.followed }).toEqual({
      sent: [],
      dropped: [],
      logs: [],
      told: [],
      followed: [],
    });
    if (!(outcome instanceof Error)) throw new Error(`no refusal: ${String(outcome)}`);
    expect(outcome.message).toBe("this session is stopping, so it registers nothing");
  });

  /**
   * One registration of a session that holds no relay subject, whose relay could not be opened,
   * with `stopping` as its host's stop reading. Its outage report is a stand-in that records each
   * call it is handed. Answers what the registration threw, the calls the report was handed, and
   * what was sent.
   */
  async function withoutRelayWhile(stopping: boolean) {
    const reported: unknown[][] = [];
    const sent: (readonly string[])[] = [];
    const outcome = await registerWithRelay({
      relay: undefined,
      relayMissing: "Authentication Timeout",
      ownAgentSubject: AGENT,
      followed: () => [TEAM],
      outage: {
        noRelay: (_error, relayMissing) => reported.push(["noRelay", relayMissing]),
        refused: () => reported.push(["refused"]),
        accepted: () => reported.push(["accepted"]),
      },
      send: (topics) => {
        sent.push(topics);
        return Promise.resolve("registered");
      },
      answered: () => undefined,
      ...quiet,
      stopping: () => stopping,
    }).catch((error: unknown) => error);
    return { outcome, reported, sent };
  }

  test("a session with no relay subject whose host began stopping is refused for the stop, and its outage report is handed nothing", async () => {
    const { outcome, reported, sent } = await withoutRelayWhile(true);

    expect(outcome).toBeInstanceOf(RegistrationStoppingError);
    expect({ reported, sent }).toEqual({ reported: [], sent: [] });
  });

  test("control: a session with no relay subject and no stop begun is refused for the missing relay subject, and its outage report is handed why", async () => {
    const { outcome, reported, sent } = await withoutRelayWhile(false);

    expect(outcome).toBeInstanceOf(NoRelaySubjectError);
    expect({ reported, sent }).toEqual({
      reported: [["noRelay", "Authentication Timeout"]],
      sent: [],
    });
  });
});

describe("a relay socket", () => {
  /** The host's outage report, shared by the socket and the registrations, and what it told. */
  const sharedOutage = () => {
    const told: string[] = [];
    const outage = inboundUnavailable(({ why }) => told.push(why), {
      restored: () => told.push("restored"),
    });
    return { told, outage };
  };

  /** After the reopen: a refused registration tells no "restored", and the next accepted one
   *  tells it once. */
  const restoredOnlyOnceAccepted = async (
    outage: InboundUnavailable,
    told: readonly string[]
  ): Promise<void> => {
    await registerOnce(outage, () => true);
    if (told.includes("restored")) {
      throw new Error(
        `the session was told its inbound was restored while every registration was refused: ${JSON.stringify(told)}`
      );
    }
    await registerOnce(outage, () => false);
    expect(told.slice(1)).toEqual(["restored"]);
  };

  test("a refused registration and then a 4401 whose fresh trade fails once tell the session no restored until a registration is accepted", async () => {
    // The socket's own trade fails inside an outage a refused registration told: the shared report
    // was told already, so the failed trade tells nothing, and the socket that opens after it must
    // not end an outage it cannot tell apart from the registration's.
    const server = standIn();
    const { told, outage } = sharedOutage();
    const tradeFails = { count: 0 };
    const relay = await opened(server, "ses_a", { outage, tradeFails });
    await registerOnce(outage, () => true);
    expect(told).toEqual([expect.stringContaining("consumer_error")]);

    tradeFails.count = 1;
    server.close(4401, "credential_expired");
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the 4401 close; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect(tradeFails.count).toBe(0);
    await relay.flush();
    await restoredOnlyOnceAccepted(outage, told);
  });

  test("a 4401 whose fresh trade fails once and then a refused registration tell the session no restored until a registration is accepted", async () => {
    // The other order: the socket's failed trade tells the outage, and a registration refused
    // after it tells nothing more. The socket that opens once the trade succeeds must not end it,
    // since the registration's cause still stands.
    const server = standIn();
    const { told, outage } = sharedOutage();
    const tradeFails = { count: 0 };
    const relay = await opened(server, "ses_a", { outage, tradeFails });

    tradeFails.count = 1;
    server.close(4401, "credential_expired");
    await until(
      () => told.length > 0,
      10_000,
      () => `the failed trade was never told; upgrades ${JSON.stringify(server.upgrades)}`
    );
    expect(told).toEqual([expect.stringContaining("the secrets broker refused the trade")]);
    await registerOnce(outage, () => true);
    await until(
      () => opens(server).length === 2,
      10_000,
      () => `no reopen after the failed trade; upgrades ${JSON.stringify(server.upgrades)}`
    );
    await relay.flush();
    await restoredOnlyOnceAccepted(outage, told);
  });

  test("a topic the edge refuses for the session's projects is dropped, and the rest go on being delivered", async () => {
    // An edge listener refuses a registration naming a topic of no project of the token's
    // (`repository_not_in_project`, `topic_not_in_project`) and names each refused topic as the
    // registration carried it. The session drops exactly those, says so once, and registers the
    // rest at once and at every heartbeat after; a frame on a topic it kept still reaches it.
    const kept = "notifications.dispatch.issue.PRIV-1.>";
    const refusedRoot = "notifications.github.acme.priv";
    const registered: (readonly string[])[] = [];
    const server = startRelaySocketStandIn({
      routes: async (request) => {
        if (new URL(request.url).pathname !== "/v1/interests/subscribe") return undefined;
        const body = (await request.json()) as {
          readonly session_id: string;
          readonly dir: string;
          readonly topics: readonly string[];
        };
        registered.push(body.topics);
        const refused = body.topics.filter((topic) => topic.startsWith(refusedRoot));
        if (refused.length > 0) {
          return Response.json(projectRefusal(refused), { status: 403 });
        }
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        });
      },
    });
    started.push(() => server.stop());
    const relay = await opened(server, "ses_a");
    const handed: string[] = [];
    const delivery = createConsumerDelivery({
      sessionID: "ses_a",
      host: {
        deliver: (frame) => {
          handed.push(frame.subject);
          frame.done();
        },
        gap: () => undefined,
        log: () => undefined,
        onTaskValue: () => undefined,
      },
      publish: (reply, body) => relay.publish(reply, encoder.encode(body)),
      ledger: { delivered: () => false, liveFailed: async () => true },
      persist: () => true,
    });
    const subscription = relay.subscribe(delivery.subject);
    void (async () => {
      for await (const message of subscription) delivery.receive(message);
    })();
    await relay.flush();
    const client = createEnvoyClient({
      baseUrl: server.url,
      fetch,
      credential: brokerCredential(podEnvironment(server), "ses_a"),
    });
    const followed = new Set(expandSubscriptionTopics([kept, `${refusedRoot}.>`]));
    const logs: string[] = [];
    const told: string[] = [];
    const outage = inboundUnavailable(({ why }) => told.push(why), {
      restored: () => told.push("restored"),
    });
    const heartbeat = () =>
      registerWithRelay({
        relay: delivery,
        ownAgentSubject: "notifications.agent.ses_a",
        followed: () => followed,
        outage,
        send: (topics, request) =>
          client.subscribe({
            sessionID: "ses_a",
            directory: "/work",
            topics,
            port: 0,
            title: "",
            driving: false,
            selfSubscribed: true,
            plugin: { name: "test", version: "0.0.0" },
            delivery: request,
          }),
        answered: () => undefined,
        drop: (topics) => {
          for (const topic of topics) followed.delete(topic);
        },
        log: (line) => logs.push(line),
      });

    await heartbeat();
    await heartbeat();

    const carriedRefused = registered
      .slice(1)
      .filter((topics) => topics.some((topic) => topic.startsWith(refusedRoot)));
    if (carriedRefused.length > 0) {
      throw new Error(
        `the stand-in saw a refused topic again after it refused it: ${JSON.stringify(carriedRefused)}`
      );
    }
    const keptTopics = [...expandSubscriptionTopics([kept])];
    expect(registered).toEqual([
      [
        "notifications.agent.ses_a",
        ...keptTopics,
        ...expandSubscriptionTopics([`${refusedRoot}.>`]),
      ],
      ["notifications.agent.ses_a", ...keptTopics],
      ["notifications.agent.ses_a", ...keptTopics],
    ]);
    expect([...followed]).toEqual(keptTopics);
    expect(logs).toEqual([
      `the listener refused ${expandSubscriptionTopics([`${refusedRoot}.>`]).join(", ")} for this session's projects (repository_not_in_project); the session no longer follows them and registers the rest`,
    ]);
    expect(told).toEqual([]);

    // A frame on the topic the session kept still reaches it, through its own consumer.
    const reply = server.deliver(
      "ses_a",
      frame("ses_a", 3, { "Envoy-Subject": ["notifications.dispatch.issue.PRIV-1.comment"] })
    );
    if (reply === undefined) throw new Error("the stand-in holds no relay subscription of ses_a");
    await until(
      () => server.receipts.some((receipt) => receipt.reply === reply && receipt.body === "done"),
      5_000,
      () =>
        `the frame on the kept topic was never answered done; the host was handed ${JSON.stringify(handed)}`
    );
    expect(handed).toEqual(["notifications.dispatch.issue.PRIV-1.comment"]);
  });
});
