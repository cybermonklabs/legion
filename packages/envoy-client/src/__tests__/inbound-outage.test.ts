import { describe, expect, test } from "bun:test";
import { BUS_UNANSWERED_PREFIX } from "@legion/contracts";
import { envoyNoticeText } from "../delivery-ledger";
import {
  type InboundOutage,
  type InboundOutageKind,
  type InboundUnavailable,
  inboundRestoredNoticeText,
  inboundUnavailable,
  inboundUnavailableNoticeText,
} from "../inbound-outage";
import { registerWithRelay } from "../relay-registration";
import { EnvoyApiError } from "../transport";
import { AGENT, SESSION } from "./consumer-session";

// The outage report a registration tells (`inboundUnavailable`), through `registerWithRelay`.
describe("one registration", () => {
  const TEAM = "notifications.team.deploys";

  /** A relay that takes what a registration stamps on it. */
  function fakeRelay() {
    return {
      setFollowed: () => undefined,
      unfollow: () => undefined,
      registration: () => ({ consumer: true, epoch: "epoch-1" }) as const,
    };
  }

  /** What a registration hands its host besides the topics: nothing a row below reads. */
  const quiet = { drop: () => undefined, log: () => undefined } as const;

  /** The start race's bound in these rows; production's is `INBOUND_FIRST_OPEN_BOUND_MS`. */
  const BOUND = 60_000;
  const NO_RELAY =
    "this session holds no relay subject, so the listener can give it no consumer and it can receive nothing";

  /**
   * One session's outage report on a clock the row moves (`at`), with what it told (each why, and
   * each outage whole), the clock at each time it said the session receives again, each reason it
   * handed `held`, and the two registrations a row makes: one with no relay subject, naming
   * `relayMissing` when a row passes one, and one whose send answers `answer`. `stop` begins the
   * session's stop.
   */
  function reportOnClock() {
    let clock = 0;
    let stopping = false;
    const told: string[] = [];
    const outages: InboundOutage[] = [];
    const restored: number[] = [];
    const held: (string | undefined)[] = [];
    const sent: unknown[] = [];
    const outage = inboundUnavailable(
      (toldOutage) => {
        told.push(toldOutage.why);
        outages.push(toldOutage);
      },
      {
        now: () => clock,
        firstOpenBoundMs: BOUND,
        restored: () => restored.push(clock),
        held: (relayMissing) => held.push(relayMissing),
        stopping: () => stopping,
      }
    );
    return {
      told,
      outages,
      restored,
      held,
      sent,
      at: (ms: number) => {
        clock = ms;
      },
      stop: () => {
        stopping = true;
      },
      withoutRelay: (relayMissing?: string) =>
        registerWithRelay({
          relay: undefined,
          relayMissing,
          ownAgentSubject: AGENT,
          followed: () => [TEAM],
          outage,
          send: (topics) => {
            sent.push(topics);
            return Promise.resolve("registered");
          },
          answered: () => undefined,
          ...quiet,
        }),
      withRelay: (answer: Promise<string>) =>
        registerWithRelay({
          relay: fakeRelay(),
          ownAgentSubject: AGENT,
          followed: () => [TEAM],
          outage,
          send: (topics) => {
            sent.push(topics);
            return answer;
          },
          answered: () => undefined,
          ...quiet,
        }),
    };
  }

  test("a session holding no relay subject at its start registers nothing, and is told once only when the bound has passed", async () => {
    const report = reportOnClock();

    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    report.at(BOUND - 1);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    // The relay may still open: inside the bound the session is told nothing.
    expect(report.told).toEqual([]);

    report.at(BOUND);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    // The next outage tick says nothing more: the session was told once. Nothing was sent: the
    // row "`held` is not called for a lost relay subject …" below shows a registration with a
    // relay subject sent.
    report.at(BOUND + 15_000);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect(report.sent).toEqual([]);
    expect(report.told).toEqual([NO_RELAY]);
  });

  // The host logs the start race at once (`held`), while the session's notice waits for the bound:
  // a start whose relay never opens shows in its log that a registration found no relay subject,
  // and why, long before the bound. It is logged once per outage, not at every try.
  test("the start race's first registration with no relay subject goes to `held` at once, naming why, and no later one does, while the session is told only past the bound", async () => {
    const report = reportOnClock();
    const why = `${NO_RELAY}, because its relay subject could not be opened: Authentication Timeout`;

    await expect(report.withoutRelay("Authentication Timeout")).rejects.toThrow(why);
    expect([report.held, report.told]).toEqual([["Authentication Timeout"], []]);

    report.at(BOUND - 1);
    await expect(report.withoutRelay("Authentication Timeout")).rejects.toThrow(why);
    expect([report.held, report.told]).toEqual([["Authentication Timeout"], []]);

    report.at(BOUND);
    await expect(report.withoutRelay("Authentication Timeout")).rejects.toThrow(why);
    report.at(BOUND + 15_000);
    await expect(report.withoutRelay("Authentication Timeout")).rejects.toThrow(why);
    expect([report.held, report.told]).toEqual([["Authentication Timeout"], [why]]);

    // A host that names no reason is handed none.
    const unnamed = reportOnClock();
    await expect(unnamed.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect([unnamed.held, unnamed.told]).toEqual([[undefined], []]);
  });

  test("`held` is not called for a lost relay subject after the first accepted registration, which is told at once, nor for a refusal, nor for a call the bound does not hold, nor once the stop began", async () => {
    const lost = reportOnClock();
    await lost.withRelay(Promise.resolve("registered"));
    // The control of the first row's `sent`: a registration with a relay subject is sent.
    expect(lost.sent).toEqual([[AGENT, TEAM]]);
    await expect(lost.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect([lost.held, lost.told]).toEqual([[], [NO_RELAY]]);

    const refused = reportOnClock();
    await expect(refused.withRelay(Promise.reject(new Error("no_grant")))).rejects.toThrow(
      "no_grant"
    );
    expect([refused.held, refused.told]).toEqual([[], ["no_grant"]]);

    const held: (string | undefined)[] = [];
    const told: string[] = [];
    await expect(
      registerWithRelay({
        relay: undefined,
        ownAgentSubject: AGENT,
        followed: () => [TEAM],
        outage: inboundUnavailable(({ why }) => told.push(why), {
          firstOpenBoundMs: 0,
          held: (relayMissing) => held.push(relayMissing),
        }),
        send: () => Promise.resolve("registered"),
        answered: () => undefined,
        ...quiet,
      })
    ).rejects.toThrow("holds no relay subject");
    expect([held, told]).toEqual([[], [NO_RELAY]]);

    const stopping = reportOnClock();
    stopping.stop();
    await expect(stopping.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect([stopping.held, stopping.told]).toEqual([[], []]);
  });

  test("a session whose relay opens inside the bound and whose registration is accepted is told nothing at all", async () => {
    const report = reportOnClock();

    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    report.at(BOUND - 1);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect(await report.withRelay(Promise.resolve("registered"))).toBe("registered");

    expect([report.told, report.restored]).toEqual([[], []]);
  });

  test("a session holding no relay subject past the bound is told once, then once that it receives again, and a second accepted registration tells nothing", async () => {
    const report = reportOnClock();

    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    report.at(BOUND);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect(report.told).toEqual([NO_RELAY]);

    report.at(BOUND + 15_000);
    await report.withRelay(Promise.resolve("registered"));
    expect(report.restored).toEqual([BOUND + 15_000]);
    report.at(BOUND + 105_000);
    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([[NO_RELAY], [BOUND + 15_000]]);
  });

  test("a registration refused while the start race is held is told at once", async () => {
    const report = reportOnClock();

    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    report.at(1_000);
    await expect(report.withRelay(Promise.reject(new Error("no_grant")))).rejects.toThrow(
      "no_grant"
    );

    expect(report.told).toEqual(["no_grant"]);
  });

  test("a session that loses its relay subject after its first accepted registration is told at once, and once that it receives again", async () => {
    const report = reportOnClock();

    await report.withRelay(Promise.resolve("registered"));
    report.at(1_000);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect([report.told, report.restored]).toEqual([[NO_RELAY], []]);

    report.at(16_000);
    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([[NO_RELAY], [16_000]]);
  });

  // R24 of dispatch://INFRA-3589: a stopping session closes its own relay on purpose, and its
  // record is deleted next, so a registration refused, answered or skipped during the stop says
  // nothing about what it receives.
  test("a session that has begun to stop is told nothing of a registration with no relay subject or a refused one, though it was told of both before", async () => {
    const report = reportOnClock();

    await report.withRelay(Promise.resolve("registered"));
    // Before the stop, each tells the session once, and the accepted registration after each
    // tells it that it receives again.
    report.at(1_000);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    await report.withRelay(Promise.resolve("registered"));
    report.at(2_000);
    await expect(report.withRelay(Promise.reject(new Error("no_grant")))).rejects.toThrow(
      "no_grant"
    );
    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([
      [NO_RELAY, "no_grant"],
      [1_000, 2_000],
    ]);

    report.stop();
    report.at(3_000);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    await expect(report.withRelay(Promise.reject(new Error("no_grant")))).rejects.toThrow(
      "no_grant"
    );
    expect([report.told, report.restored]).toEqual([
      [NO_RELAY, "no_grant"],
      [1_000, 2_000],
    ]);
  });

  test("a session told of an outage that then begins to stop is not told that it receives again", async () => {
    const report = reportOnClock();

    await report.withRelay(Promise.resolve("registered"));
    report.at(1_000);
    await expect(report.withoutRelay()).rejects.toThrow("holds no relay subject");
    expect(report.told).toEqual([NO_RELAY]);

    report.stop();
    report.at(2_000);
    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([[NO_RELAY], []]);
  });

  // The rows of the topics a registration carries, of a refused registration, of one whose relay
  // could not be opened and of one accepted after an outage are in `relay-registration.test.ts`.

  // Row (e) of check l23 (R24 of dispatch://INFRA-3589, against R26): the listener answers its
  // bus-outage 503 to a registration whose known-session record did not land, after it made the
  // session's consumer, so the session still receives what the stream stores for it. Such a
  // refusal is held as the start race is: no notice inside the bound, and a registration accepted
  // inside it clears it with none. Past the bound the session is told once, that the listener did
  // not record its registration, and never that it has no consumer.
  const UNRECORDED = `${BUS_UNANSWERED_PREFIX}: write the known-session record of ${SESSION}: context deadline exceeded`;
  const unrecorded = () =>
    new EnvoyApiError({
      method: "POST",
      url: "http://listener/v1/interests/subscribe",
      status: 503,
      responseBody: JSON.stringify({ error: UNRECORDED }),
    });

  test("(e) a registration answered with the bus-outage 503 at the session's start is held for the bound, and one accepted inside it clears it with no notice", async () => {
    const report = reportOnClock();

    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    report.at(BOUND - 1);
    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    expect(report.told).toEqual([]);

    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([[], []]);
  });

  test("(e) a registered session whose later registration meets the bus-outage 503 is held the same way, from that refusal", async () => {
    const report = reportOnClock();

    await report.withRelay(Promise.resolve("registered"));
    report.at(1_000);
    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    report.at(1_000 + BOUND - 1);
    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    expect(report.told).toEqual([]);

    await report.withRelay(Promise.resolve("registered"));
    expect([report.told, report.restored]).toEqual([[], []]);
  });

  test("(e) past the bound a bus-outage 503 is told once, as a registration not recorded for certain and not as a session with no consumer", async () => {
    const report = reportOnClock();

    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    report.at(BOUND);
    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    report.at(BOUND + 15_000);
    await expect(report.withRelay(Promise.reject(unrecorded()))).rejects.toThrow(UNRECORDED);
    expect(report.told).toEqual([UNRECORDED]);

    // Every host words what it was told with this one text.
    expect(report.outages).toEqual([{ kind: "unrecorded", why: UNRECORDED }]);
    const notice = inboundUnavailableNoticeText({ kind: "unrecorded", why: UNRECORDED });
    expect(notice).toMatch(/not record/i);
    expect(notice).not.toContain("no durable consumer");
    expect(notice).not.toContain("receives none");

    report.at(BOUND + 30_000);
    await report.withRelay(Promise.resolve("registered"));
    expect(report.restored).toEqual([BOUND + 30_000]);
  });

  // The accepted registration that ends a told episode is worded for the kind that episode told.
  // After a bus-outage 503 the consumer may have delivered all along, so that notice says the
  // listener recorded a registration, and never that the session received nothing.
  // `restored` builds the notice as every host does, from the outage it is handed.
  function reportRestoring() {
    let clock = 0;
    const told: string[] = [];
    const restored: string[] = [];
    const notices: string[] = [];
    const outage = inboundUnavailable(({ why }) => told.push(why), {
      now: () => clock,
      firstOpenBoundMs: BOUND,
      restored: ({ kind, why }) => {
        restored.push(why);
        notices.push(inboundRestoredNoticeText(kind));
      },
    });
    return {
      outage,
      told,
      restored,
      notices,
      at: (ms: number) => {
        clock = ms;
      },
    };
  }

  test("(e) the accepted registration after a told bus-outage 503 says the listener recorded a registration, and never that the session received nothing", () => {
    const report = reportRestoring();

    report.outage.refused(unrecorded());
    report.at(BOUND);
    report.outage.refused(unrecorded());
    expect(report.told).toEqual([UNRECORDED]);
    report.at(BOUND + 15_000);
    report.outage.accepted();

    expect(report.notices).toHaveLength(1);
    const notice = report.notices[0] ?? "";
    expect(notice).toContain("Envoy inbound restored");
    expect(notice).toContain("The listener recorded a registration of this session");
    expect(notice).toContain("arrives now");
    expect(notice).not.toMatch(/receive[sd]? nothing/i);
    expect(notice).not.toContain("receives Envoy messages again");
  });

  test("the accepted registration after a told refusal keeps its notice: the session receives again, and what was stored while it received nothing arrives now", () => {
    const report = reportRestoring();

    report.outage.refused(new Error("no_grant"));
    report.at(1_000);
    report.outage.accepted();

    expect(report.notices).toHaveLength(1);
    expect(report.notices[0]).toContain(
      "The listener accepted a registration of this session, so the session receives Envoy messages again. What the stream stored while it received nothing arrives now."
    );
  });

  test("`restored` is handed the why its episode told: a refusal's, then a bus-outage 503's", () => {
    const report = reportRestoring();

    report.outage.refused(new Error("no_grant"));
    report.at(1_000);
    report.outage.accepted();
    report.at(2_000);
    report.outage.refused(unrecorded());
    report.at(2_000 + BOUND);
    report.outage.refused(unrecorded());
    report.at(2_000 + BOUND + 15_000);
    report.outage.accepted();

    expect([report.told, report.restored]).toEqual([
      ["no_grant", UNRECORDED],
      ["no_grant", UNRECORDED],
    ]);
  });

  // Every kind words its notice and its restored notice in words of its own, and a refusal and a
  // lost relay subject in the same words: the session receives nothing, then receives again.
  test("each kind of outage is told, and its end told, in exactly its own words", () => {
    const unavailable = (kind: InboundOutageKind) =>
      inboundUnavailableNoticeText({ kind, why: `the why of ${kind}` });
    const noConsumer =
      "The listener gave this session no durable consumer, which is the only path an Envoy message reaches it by, so it receives none. Everything else works, and the session registers again at every heartbeat; what the stream stored meanwhile arrives once a registration is accepted.";
    const receivesAgain = envoyNoticeText({
      notice: "Envoy inbound restored",
      detail:
        "The listener accepted a registration of this session, so the session receives Envoy messages again. What the stream stored while it received nothing arrives now.",
    });

    expect(unavailable("unrecorded")).toBe(
      envoyNoticeText({
        notice: "Envoy inbound unavailable",
        error: "the why of unrecorded",
        detail:
          "This session's registration is not recorded for certain: the listener's message bus did not answer in time, so the listener may or may not have recorded it. Envoy messages may still reach the session, but nothing confirms it until a registration is accepted. Everything else works, and the session registers again at every heartbeat.",
      })
    );
    expect(unavailable("github_withheld")).toBe(
      envoyNoticeText({
        notice: "Envoy GitHub topics withheld",
        error: "the why of github_withheld",
        detail:
          "The listener could not read the repositories of this session's projects. So it follows none of this session's GitHub topics now, and it withholds their messages. It refused this registration. If the listener already held a registration of this session, it kept that registration and the session's consumer, so the session receives the messages of every other topic the listener follows for it; a topic the session began to follow after its last accepted registration is followed only once a registration is accepted. If the listener held no registration of this session, the session receives nothing until a registration is accepted. The session registers again at every heartbeat.",
      })
    );
    expect(unavailable("refused")).toBe(
      envoyNoticeText({
        notice: "Envoy inbound unavailable",
        error: "the why of refused",
        detail: noConsumer,
      })
    );
    expect(unavailable("no_relay")).toBe(
      envoyNoticeText({
        notice: "Envoy inbound unavailable",
        error: "the why of no_relay",
        detail: noConsumer,
      })
    );

    expect(inboundRestoredNoticeText("unrecorded")).toBe(
      envoyNoticeText({
        notice: "Envoy inbound restored",
        detail:
          "The listener recorded a registration of this session. A message the stream stored that has not reached the session arrives now.",
      })
    );
    expect(inboundRestoredNoticeText("github_withheld")).toBe(
      envoyNoticeText({
        notice: "Envoy GitHub topics followed again",
        detail:
          "The listener accepted a registration of this session, so it follows this session's GitHub topics again, and their messages reach the session again. A GitHub message the listener withheld meanwhile may be missing.",
      })
    );
    expect(inboundRestoredNoticeText("refused")).toBe(receivesAgain);
    expect(inboundRestoredNoticeText("no_relay")).toBe(receivesAgain);
  });

  // Finding 9 of the read-ahead at 73c124fe (dispatch://INFRA-3589): a told bus-outage 503 says
  // that the session's consumer may still deliver. A refusal or a lost relay subject after it
  // says that the session receives nothing, so it is told once more. The accepted registration
  // after it is worded for that later `why`: the session receives again.
  test("a refusal after a told bus-outage 503 is told once more, and the restore after it says the session receives again", () => {
    const report = reportRestoring();

    report.outage.accepted();
    report.at(1_000);
    report.outage.refused(unrecorded());
    report.at(1_000 + BOUND);
    report.outage.refused(unrecorded());
    expect(report.told).toEqual([UNRECORDED]);

    report.at(1_000 + BOUND + 15_000);
    report.outage.refused(new Error("no_grant"));
    // Once more, and no more: a later refusal or bus-outage 503 tells nothing.
    report.at(1_000 + BOUND + 30_000);
    report.outage.refused(new Error("no_grant"));
    report.outage.refused(unrecorded());
    expect(report.told).toEqual([UNRECORDED, "no_grant"]);

    report.at(1_000 + BOUND + 45_000);
    report.outage.accepted();
    expect(report.restored).toEqual(["no_grant"]);
    expect(report.notices).toHaveLength(1);
    expect(report.notices[0]).toContain(
      "The listener accepted a registration of this session, so the session receives Envoy messages again."
    );
  });

  test("a lost relay subject after a told bus-outage 503 is told once more, and the restore after it says the session receives again", () => {
    const report = reportRestoring();

    report.outage.accepted();
    report.at(1_000);
    report.outage.refused(unrecorded());
    report.at(1_000 + BOUND);
    report.outage.refused(unrecorded());
    report.at(1_000 + BOUND + 15_000);
    report.outage.noRelay(new Error(NO_RELAY));
    report.outage.noRelay(new Error(NO_RELAY));
    expect(report.told).toEqual([UNRECORDED, NO_RELAY]);

    report.at(1_000 + BOUND + 30_000);
    report.outage.accepted();
    expect(report.restored).toEqual([NO_RELAY]);
    expect(report.notices[0]).toContain("so the session receives Envoy messages again");
  });

  // R24 of dispatch://INFRA-3589, after main's #4810: an edge listener that could not read the
  // repositories of the session's projects refuses the registration 503 `repositories_unknown`
  // (the listener's `answerRepositoryOutage`), and it withholds the messages of the session's
  // GitHub topics. Where it already held the session's registration, it renews that entry and
  // keeps the session's consumer; where it held none, it creates none (`renewThroughOutage`). The
  // client cannot tell which. So the session is told that its GitHub topics are withheld, and
  // what it receives in each case, and never that it has no durable consumer. The accepted
  // registration after it says that the listener follows the GitHub topics again, and not that
  // the session receives again.
  const WITHHELD_ERROR =
    "the repositories of PRIV's issues could not be read, so no GitHub topic is followed now; register again shortly";
  const WITHHELD = `${WITHHELD_ERROR} (reason: repositories_unknown)`;
  const repositoriesUnknown = () =>
    new EnvoyApiError({
      method: "POST",
      url: "http://listener/v1/interests/subscribe",
      status: 503,
      responseBody: JSON.stringify({ error: WITHHELD_ERROR, reason: "repositories_unknown" }),
    });
  /** The listener's 503 `no_grant`: it can give the session no consumer at all. */
  const NO_GRANT_ERROR =
    "this listener may not manage session consumers, so it can give ses_a none";
  const NO_GRANT = `${NO_GRANT_ERROR} (reason: no_grant)`;
  const noGrant = () =>
    new EnvoyApiError({
      method: "POST",
      url: "http://listener/v1/interests/subscribe",
      status: 503,
      responseBody: JSON.stringify({ error: NO_GRANT_ERROR, reason: "no_grant" }),
    });

  /** The `notice:` value of a notice's text. */
  const noticeName = (text: string) => /notice: (.*)/.exec(text)?.[1];

  /**
   * One session's outage report on a clock the row moves (`at`), whose `tell` and `restored`
   * word what they are handed as every host does (`inboundUnavailableNoticeText`,
   * `inboundRestoredNoticeText`), with the kind of each outage it handed them; and one
   * registration whose send answers what `answer` returns.
   */
  function reportWording() {
    let clock = 0;
    const notices: string[] = [];
    const kinds: InboundOutageKind[] = [];
    const outage = inboundUnavailable(
      (told) => {
        kinds.push(told.kind);
        notices.push(inboundUnavailableNoticeText(told));
      },
      {
        now: () => clock,
        firstOpenBoundMs: BOUND,
        restored: ({ kind }) => {
          kinds.push(kind);
          notices.push(inboundRestoredNoticeText(kind));
        },
      }
    );
    return {
      outage,
      notices,
      kinds,
      at: (ms: number) => {
        clock = ms;
      },
      register: (answer: () => Promise<string>) =>
        registerWithRelay({
          relay: fakeRelay(),
          ownAgentSubject: AGENT,
          followed: () => [TEAM],
          outage,
          send: () => answer(),
          answered: () => undefined,
          ...quiet,
        }).catch(() => undefined),
    };
  }

  test("an edge listener's 503 repositories_unknown is told at once and once, as GitHub topics withheld with what the session receives where the listener held its registration and where it held none, and never as a session with no durable consumer", async () => {
    const report = reportWording();

    await report.register(() => Promise.resolve("registered"));
    report.at(1_000);
    await report.register(() => Promise.reject(repositoriesUnknown()));
    // The next heartbeat's refusal tells nothing more.
    report.at(16_000);
    await report.register(() => Promise.reject(repositoriesUnknown()));

    expect(report.notices).toHaveLength(1);
    const notice = report.notices[0] ?? "";
    expect(noticeName(notice)).toBe("Envoy GitHub topics withheld");
    expect(notice).toContain(WITHHELD);
    expect(notice).toContain(
      "The listener could not read the repositories of this session's projects. So it follows none of this session's GitHub topics now, and it withholds their messages. It refused this registration. If the listener already held a registration of this session, it kept that registration and the session's consumer, so the session receives the messages of every other topic the listener follows for it; a topic the session began to follow after its last accepted registration is followed only once a registration is accepted. If the listener held no registration of this session, the session receives nothing until a registration is accepted. The session registers again at every heartbeat."
    );
    // Nothing says that the session has no durable consumer, or that it receives none.
    expect(notice).not.toContain("Envoy inbound unavailable");
    expect(notice).not.toContain("no durable consumer");
    expect(notice).not.toContain("receives none");
    expect(report.kinds).toEqual(["github_withheld"]);
  });

  test("the accepted registration after a told 503 repositories_unknown says that the listener follows the session's GitHub topics again, and never that the session receives again", async () => {
    const report = reportWording();

    await report.register(() => Promise.reject(repositoriesUnknown()));
    report.at(15_000);
    await report.register(() => Promise.resolve("registered"));
    // A later accepted registration tells nothing more.
    report.at(30_000);
    await report.register(() => Promise.resolve("registered"));

    expect(report.notices).toHaveLength(2);
    const restored = report.notices[1] ?? "";
    expect(noticeName(restored)).toBe("Envoy GitHub topics followed again");
    expect(restored).toContain(
      "The listener accepted a registration of this session, so it follows this session's GitHub topics again, and their messages reach the session again. A GitHub message the listener withheld meanwhile may be missing."
    );
    expect(restored).not.toContain("Envoy inbound restored");
    expect(restored).not.toContain("receives Envoy messages again");
    expect(restored).not.toMatch(/received? nothing/);
    expect(report.kinds).toEqual(["github_withheld", "github_withheld"]);
  });

  test("a bus-outage 503 and a 503 no_grant are handed to the host with the kind each is, and keep their texts", async () => {
    const report = reportWording();

    await report.register(() => Promise.reject(unrecorded()));
    report.at(BOUND);
    await report.register(() => Promise.reject(unrecorded()));
    report.at(BOUND + 15_000);
    await report.register(() => Promise.resolve("registered"));
    report.at(BOUND + 30_000);
    await report.register(() => Promise.reject(noGrant()));
    report.at(BOUND + 45_000);
    await report.register(() => Promise.resolve("registered"));

    // The bus-outage 503 is a registration not recorded for certain, and the 503 no_grant a
    // refusal.
    expect(report.kinds).toEqual(["unrecorded", "unrecorded", "refused", "refused"]);
    expect(report.notices).toEqual([
      envoyNoticeText({
        notice: "Envoy inbound unavailable",
        error: UNRECORDED,
        detail:
          "This session's registration is not recorded for certain: the listener's message bus did not answer in time, so the listener may or may not have recorded it. Envoy messages may still reach the session, but nothing confirms it until a registration is accepted. Everything else works, and the session registers again at every heartbeat.",
      }),
      envoyNoticeText({
        notice: "Envoy inbound restored",
        detail:
          "The listener recorded a registration of this session. A message the stream stored that has not reached the session arrives now.",
      }),
      envoyNoticeText({
        notice: "Envoy inbound unavailable",
        error: NO_GRANT,
        detail:
          "The listener gave this session no durable consumer, which is the only path an Envoy message reaches it by, so it receives none. Everything else works, and the session registers again at every heartbeat; what the stream stored meanwhile arrives once a registration is accepted.",
      }),
      envoyNoticeText({
        notice: "Envoy inbound restored",
        detail:
          "The listener accepted a registration of this session, so the session receives Envoy messages again. What the stream stored while it received nothing arrives now.",
      }),
    ]);
  });

  // A told 503 repositories_unknown says that the session's other messages still reach it where
  // the listener already held its registration. A refusal that leaves the session no consumer, or
  // a lost relay subject, says that it receives nothing, so it is told once more, and the accepted
  // registration after it is worded for it.
  test("a 503 no_grant or a lost relay subject after a told 503 repositories_unknown is told once more, as a session that receives nothing, and the restore after it says the session receives again", () => {
    for (const [failure, kind] of [
      [(outage: InboundUnavailable) => outage.refused(noGrant()), "refused"],
      [(outage: InboundUnavailable) => outage.noRelay(new Error(NO_RELAY)), "no_relay"],
    ] as const) {
      const report = reportWording();

      report.outage.accepted();
      report.at(1_000);
      report.outage.refused(repositoriesUnknown());
      report.at(16_000);
      failure(report.outage);
      // Once more, and no more: the same failure again, or a 503 repositories_unknown, tells
      // nothing.
      report.at(31_000);
      failure(report.outage);
      report.outage.refused(repositoriesUnknown());
      report.at(46_000);
      report.outage.accepted();

      expect(report.notices.map(noticeName)).toEqual([
        "Envoy GitHub topics withheld",
        "Envoy inbound unavailable",
        "Envoy inbound restored",
      ]);
      expect(report.notices[1]).toContain(kind === "no_relay" ? NO_RELAY : NO_GRANT);
      expect(report.notices[1]).toContain("so it receives none");
      expect(report.notices[2]).toContain("so the session receives Envoy messages again");
      expect(report.kinds).toEqual(["github_withheld", kind, kind]);
    }
  });

  // A told bus-outage 503 says that the consumer may still deliver everything, so a 503
  // repositories_unknown after it, which says that the GitHub messages are withheld, is told. A
  // bus-outage 503 after a told 503 repositories_unknown says that no less reaches the session,
  // so it tells nothing.
  test("a 503 repositories_unknown after a told bus-outage 503 is told once more, and a bus-outage 503 after a told 503 repositories_unknown is not", () => {
    const afterUnrecorded = reportWording();
    afterUnrecorded.outage.refused(unrecorded());
    afterUnrecorded.at(BOUND);
    afterUnrecorded.outage.refused(unrecorded());
    afterUnrecorded.at(BOUND + 15_000);
    afterUnrecorded.outage.refused(repositoriesUnknown());
    afterUnrecorded.at(BOUND + 30_000);
    afterUnrecorded.outage.accepted();

    expect(afterUnrecorded.notices.map(noticeName)).toEqual([
      "Envoy inbound unavailable",
      "Envoy GitHub topics withheld",
      "Envoy GitHub topics followed again",
    ]);
    expect(afterUnrecorded.kinds).toEqual(["unrecorded", "github_withheld", "github_withheld"]);

    const afterWithheld = reportWording();
    afterWithheld.outage.refused(repositoriesUnknown());
    afterWithheld.at(15_000);
    afterWithheld.outage.refused(unrecorded());
    afterWithheld.at(15_000 + BOUND);
    afterWithheld.outage.refused(unrecorded());
    afterWithheld.at(30_000 + BOUND);
    afterWithheld.outage.accepted();

    expect(afterWithheld.notices.map(noticeName)).toEqual([
      "Envoy GitHub topics withheld",
      "Envoy GitHub topics followed again",
    ]);
    expect(afterWithheld.kinds).toEqual(["github_withheld", "github_withheld"]);
  });

  // The kind of a refusal is read from its typed error once: the bus-outage 503 is a 503 whose
  // error begins with the prefix (`isBusUnanswered`), a 503 `repositories_unknown` withholds the
  // GitHub topics, and every other failure is a refusal. A registration with no relay subject is
  // a kind of its own.
  test("each refusal is sorted into its kind by its error, and a registration with no relay subject into its own", () => {
    const answer = (status: number, body: Record<string, string>) =>
      new EnvoyApiError({
        method: "POST",
        url: "http://listener/v1/interests/subscribe",
        status,
        responseBody: JSON.stringify(body),
      });
    const rows: [unknown, InboundOutageKind][] = [
      [unrecorded(), "unrecorded"],
      [repositoriesUnknown(), "github_withheld"],
      [answer(403, { error: WITHHELD_ERROR, reason: "repositories_unknown" }), "github_withheld"],
      [noGrant(), "refused"],
      [answer(503, { error: "the consumer failed", reason: "consumer_error" }), "refused"],
      [answer(403, { error: "not admitted", reason: "token_not_admitted" }), "refused"],
      [answer(503, { error: `listener: ${BUS_UNANSWERED_PREFIX}` }), "refused"],
      [new Error("no_grant"), "refused"],
    ];
    for (const [error, kind] of rows) {
      const report = reportWording();
      report.outage.refused(error);
      // A bus-outage 503 is held for the bound, and told past it.
      report.at(BOUND);
      report.outage.refused(error);
      // The row's error is named beside its kinds, so a failure says which row it is.
      expect({ error: String(error), kinds: report.kinds }).toEqual({
        error: String(error),
        kinds: [kind],
      });
    }

    const lost = reportWording();
    lost.outage.accepted();
    lost.outage.noRelay(new Error(NO_RELAY));
    expect(lost.kinds).toEqual(["no_relay"]);
  });

  // The kind is read from the typed error (`isBusUnanswered`), not from the message: an error
  // whose message begins with the bus-outage prefix but is no 503 of the listener's is a refusal,
  // told at once, worded as a session that receives nothing. The listener writes that prefix only
  // in a 503, so a session meets this only from an error the listener did not send.
  test("an error whose message begins with the bus-outage prefix but is no 503 is a refusal, told at once", () => {
    for (const error of [
      new EnvoyApiError({
        method: "POST",
        url: "http://listener/v1/interests/subscribe",
        status: 500,
        responseBody: JSON.stringify({ error: UNRECORDED }),
      }),
      new Error(UNRECORDED),
    ]) {
      const report = reportWording();
      report.outage.refused(error);

      expect({ error: String(error), kinds: report.kinds }).toEqual({
        error: String(error),
        kinds: ["refused"],
      });
      expect(report.notices[0]).toContain("so it receives none");
    }
  });
});
