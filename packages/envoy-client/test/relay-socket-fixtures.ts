// packages/envoy-client/test/relay-socket-fixtures.ts
//
// What the relay socket's test files share: a stand-in listener and broker per row
// (`./relay-socket-stand-in.ts`), a pod's environment that trades at it, a relay socket opened on
// it, a relay frame as the listener sends one, and one registration through a shared outage
// report. `relaySocketFixtures` registers, in the file that calls it at its top level, the hooks
// that stop what each row started and forget the broker clients after it.

import { afterEach } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { brokerCredential, forgetBrokerClients } from "../src/broker-credential";
import type { InboundUnavailable } from "../src/consumer-delivery";
import type { EnvoyEnvironment } from "../src/envoy-settings";
import { connectListenerRelay, type ListenerRelay, type RelayOutage } from "../src/listener-relay";
import { registerWithRelay } from "../src/relay-registration";
import { EnvoyApiError } from "../src/transport";
import {
  type RelaySocketStandIn,
  type StandInFrame,
  type StandInUpgrade,
  startRelaySocketStandIn,
} from "./relay-socket-stand-in";

/** What a row of the relay socket's test files may say it heard on a socket it opened. */
export interface Heard {
  readonly logs?: string[];
  readonly asks?: { count: number };
  /** The outage report the socket shares with a row's registrations; one that tells nothing
   *  where a row gives none. */
  readonly outage?: RelayOutage;
  readonly boundMs?: number;
  /** While above 0, each time the relay asks for the upgrade's headers the trade fails, as a
   *  broker that refuses it does, and the count goes down by one. */
  readonly tradeFails?: { count: number };
}

/** The fixtures of one test file, its rows' cleanup registered (the file's header). */
export function relaySocketFixtures(prefix: string) {
  const tempDir = temporaryDirectories(prefix);
  /** What each row started, stopped after it in the reverse order. */
  const started: (() => unknown)[] = [];
  afterEach(async () => {
    for (const stop of started.splice(0).reverse()) await stop();
  });
  afterEach(forgetBrokerClients);

  function standIn(): RelaySocketStandIn {
    const server = startRelaySocketStandIn();
    started.push(() => server.stop());
    return server;
  }

  /** A pod's environment whose broker and listener are both `server`. */
  function podEnvironment(server: RelaySocketStandIn): EnvoyEnvironment {
    const dir = tempDir();
    const tokenFile = path.join(dir, "token");
    writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
    return {
      HOME: dir,
      ENVOY_URL: server.url,
      AGENT_SECRETS_URL: server.url,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    };
  }

  /** A relay socket for `session` at `server`, open once the first socket opens or `boundMs`
   *  pass; closed after the row. `heard.asks` counts each time the relay asks the credential for
   *  the upgrade's headers. */
  async function opened(
    server: RelaySocketStandIn,
    session: string | (() => string),
    heard: Heard = {}
  ): Promise<ListenerRelay> {
    const credential = brokerCredential(podEnvironment(server), session);
    const relay = await connectListenerRelay(
      {
        url: server.url,
        session,
        credential: {
          ...credential,
          headers: (...args) => {
            if (heard.asks !== undefined) heard.asks.count += 1;
            if (heard.tradeFails !== undefined && heard.tradeFails.count > 0) {
              heard.tradeFails.count -= 1;
              return Promise.reject(new Error("the secrets broker refused the trade (fixture)"));
            }
            return credential.headers(...args);
          },
        },
        outage: heard.outage ?? { refused: () => undefined },
        log: (line) => heard.logs?.push(line),
      },
      AbortSignal.timeout(heard.boundMs ?? 20_000)
    );
    started.push(() => relay.close());
    return relay;
  }

  return { tempDir, started, standIn, podEnvironment, opened };
}

/** One registration of `ses_a` through `outage`, as a host makes one: refused 503
 *  `repositories_unknown` (the edge could not read its projects' repositories) while `refuse()`
 *  holds, accepted otherwise. Answers once it settled, either way. */
export async function registerOnce(
  outage: InboundUnavailable,
  refuse: () => boolean
): Promise<void> {
  await registerWithRelay({
    relay: {
      setFollowed: () => undefined,
      unfollow: () => undefined,
      registration: () => ({ consumer: true, epoch: "epoch-1" }) as const,
    },
    ownAgentSubject: "notifications.agent.ses_a",
    followed: () => [],
    outage,
    send: () =>
      refuse()
        ? Promise.reject(
            new EnvoyApiError({
              method: "POST",
              url: "http://listener.example/v1/interests/subscribe",
              status: 503,
              responseBody: JSON.stringify({
                error: "the project's repositories could not be read",
                reason: "repositories_unknown",
              }),
            })
          )
        : Promise.resolve("registered"),
    answered: () => undefined,
    drop: () => undefined,
    log: () => undefined,
  }).catch(() => undefined);
}

/** Waits until `check` holds, failing with `what()` once `ms` pass. */
export async function until(check: () => boolean, ms: number, what: () => string): Promise<void> {
  const ends = Date.now() + ms;
  while (!check()) {
    if (Date.now() > ends) throw new Error(what());
    await Bun.sleep(5);
  }
}

/** A relay frame on `session`'s own agent subject, stored now, as the listener relays one: each
 *  header a list of its values, as the wire carries a `nats.Header` (`../src/listener-relay.ts`). */
export function frame(
  session: string,
  seq: number,
  extra: Readonly<Record<string, readonly string[]>> = {}
): StandInFrame {
  return {
    headers: {
      "Envoy-Subject": [`notifications.agent.${session}`],
      "Envoy-Stream-Seq": [String(seq)],
      "Envoy-Stream-Id": ["stream-1"],
      "Envoy-Delivered": ["1"],
      "Envoy-Stored-At": [new Date().toISOString()],
      ...extra,
    },
    data: JSON.stringify({ event_id: `evt-${seq}`, source: "agent" }),
  };
}

/** The upgrades `server` opened, in order. */
export function opens(server: RelaySocketStandIn): StandInUpgrade[] {
  return server.upgrades.filter((upgrade) => upgrade.status === 101);
}
