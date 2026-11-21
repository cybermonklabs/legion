import { expect } from "bun:test";
import { agentStreamControlSubject } from "@legion/contracts";
import { createInbox, type NatsConnection } from "nats";
import { type NatsServer, sessionReplySubject } from "./nats-server";

/**
 * Asks session `sessionID` for its replay over `connection` on the reply subject `reply`, as
 * Dispatch's `agentstream.NATS.Replay` asks, and answers the first answer's text, or undefined
 * when none arrives within `withinMs`.
 */
export async function askReplay(
  connection: NatsConnection,
  sessionID: string,
  reply: string,
  withinMs: number
): Promise<string | undefined> {
  const answers = connection.subscribe(reply, { max: 1 });
  await connection.flush();
  connection.publish(
    agentStreamControlSubject(sessionID),
    new TextEncoder().encode(JSON.stringify({ v: 1, type: "replay" })),
    { reply }
  );
  const timer = setTimeout(() => answers.unsubscribe(), withinMs);
  try {
    for await (const answer of answers) return answer.string();
    return undefined;
  } finally {
    clearTimeout(timer);
  }
}

/** What a scenario needs: the server the session's grant is on, and the requester's connection. */
export interface ReplayGrantScenario {
  /** Started with `sessionGrantConfig` for `sessionID`. */
  readonly server: NatsServer;
  /** A connection as that configuration's requester, which may do anything. */
  readonly requester: NatsConnection;
  readonly sessionID: string;
}

/**
 * The replay answer of a session holding the NATS auth service's session grant, whose plugin the
 * caller has booted on its own connection to `server` (the caller keeps the boot, the connection
 * and the shutdown its plugin needs). Three steps, each with its own bound:
 *
 * 1. A replay asked on a reply subject naming the session (`sessionReplySubject`) is answered with
 *    the session's history. The plugin's control subscription is in place once one is answered,
 *    so the first ask is repeated until then.
 * 2. The control: the same request asked on the reply subject Dispatch asked on before, nats.go's
 *    own request inbox (`<inbox>.<n>`), gets no answer, and the server logs the refused publish.
 *    A grant that admitted every `_INBOX.` subject fails here, with the answer that arrived.
 * 3. A replay asked on the session's own prefix after the refusal is answered again.
 */
export async function runReplayGrantScenario(scenario: ReplayGrantScenario): Promise<void> {
  const { server, requester, sessionID } = scenario;
  const own = () => sessionReplySubject(createInbox(), sessionID);
  // The answers and the refusal cross a real socket from a real server, so there is no clock to
  // fake; every wait is on its condition, with a bound.
  const deadline = Date.now() + 10_000;
  let answer: string | undefined;
  while (answer === undefined) {
    if (Date.now() > deadline) throw new Error("no replay answer on the session's own prefix");
    answer = await askReplay(requester, sessionID, own(), 500);
  }
  expect(JSON.parse(answer)).toMatchObject({ v: 1, session_id: sessionID });

  // The control. The server never delivers a publish it refused, so its `Publish Violation` line is
  // the evidence: subscribe, ask, then wait on that line, and only then read what arrived.
  const before = `${createInbox()}.1`;
  const arrived: string[] = [];
  const answers = requester.subscribe(before, {
    callback: (_error, message) => arrived.push(message.string()),
  });
  await requester.flush();
  requester.publish(
    agentStreamControlSubject(sessionID),
    new TextEncoder().encode(JSON.stringify({ v: 1, type: "replay" })),
    { reply: before }
  );
  const refused = (log: string) =>
    log
      .split("\n")
      .some((line) => line.includes("Publish Violation") && line.includes(`"${before}"`));
  let logged = "";
  const logDeadline = Date.now() + 10_000;
  try {
    while (!refused(logged)) {
      if (Date.now() > logDeadline) {
        throw new Error(
          `the server never logged a refused publish to ${before}; arrived there: ${JSON.stringify(arrived)}\n${logged}`
        );
      }
      await Bun.sleep(100);
      logged = await server.log();
    }
    await requester.flush();
    expect(arrived).toEqual([]);
  } finally {
    answers.unsubscribe();
  }

  const again = await askReplay(requester, sessionID, own(), 5_000);
  expect(again === undefined ? undefined : JSON.parse(again)).toMatchObject({
    session_id: sessionID,
  });
}
