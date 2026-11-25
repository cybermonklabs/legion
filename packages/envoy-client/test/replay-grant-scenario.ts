import { expect } from "bun:test";
import { agentStreamControlSubject } from "@legion/contracts";
import { createInbox, type NatsConnection } from "nats";
import { type NatsServer, sessionReplySubject } from "./nats-server";

/** The replay request, as Dispatch's `agentstream.NATS.Replay` sends it. */
const REPLAY_ASK = new TextEncoder().encode(JSON.stringify({ v: 1, type: "replay" }));

/** How long an ask goes unanswered before it is asked again. A cadence, not a deadline: each ask
 *  of one wait goes to the same reply subject, so an answer to any of them ends the wait, however
 *  late it comes. */
const ASK_AGAIN_MS = 500;

/** How long after it starts each `docker logs` call is killed (`NatsServer.log`). An hour is past
 *  the timeout of each CI job that runs this scenario (pi-envoy's 15 minutes, claude-envoy's 10),
 *  so the kill decides no row: the calling row's limit ends a read that hangs. */
const LOG_READ_KILL_MS = 60 * 60_000;

/** Throws once `requester` has closed, naming what the scenario was waiting for. */
function throwIfClosed(requester: NatsConnection, what: string): void {
  if (requester.isClosed()) {
    throw new Error(`stopped waiting for ${what}: the requester's connection has closed`);
  }
}

/**
 * Asks session `sessionID` for its replay over `requester` on the reply subject `reply`, again
 * every `ASK_AGAIN_MS` until an answer arrives, and answers the first answer's text. A 503
 * status the server sends there says nothing subscribes the control subject yet, so it is no
 * answer, and the ask goes again. It has no deadline of its own: a session that never answers
 * holds the calling row to its limit, which names the row. It throws once `requester` has
 * closed.
 */
async function askUntilAnswered(
  requester: NatsConnection,
  sessionID: string,
  reply: string
): Promise<string> {
  const what = `a replay answer on ${reply}`;
  const answers = requester.subscribe(reply);
  try {
    await requester.flush();
    // Undefined once the subscription has closed with no answer, as it does with its connection.
    const answered = (async () => {
      for await (const answer of answers) {
        if (answer.headers?.code !== 503) return answer.string();
      }
      return undefined;
    })();
    const again = Symbol("ask again");
    for (;;) {
      throwIfClosed(requester, what);
      requester.publish(agentStreamControlSubject(sessionID), REPLAY_ASK, { reply });
      const answer = await Promise.race([answered, Bun.sleep(ASK_AGAIN_MS).then(() => again)]);
      if (typeof answer === "string") return answer;
      if (answer === undefined) {
        throwIfClosed(requester, what);
        throw new Error(`stopped waiting for ${what}: its subscription closed with no answer`);
      }
    }
  } finally {
    answers.unsubscribe();
  }
}

/** What a scenario needs: the server the session's grant is on, and the requester's connection. */
export interface ReplayGrantScenario {
  /** Started with `sessionGrantConfig` for `sessionID`. */
  readonly server: NatsServer;
  /** A connection as that configuration's requester, which may do anything. The caller closes it
   *  once its row has ended, its limit included: each wait of the scenario throws once it has
   *  closed, so a row its limit abandoned stops there. */
  readonly requester: NatsConnection;
  readonly sessionID: string;
}

/**
 * The replay answer of a session holding the NATS auth service's session grant, whose plugin the
 * caller has booted on its own connection to `server` (the caller keeps the boot, the connection
 * and the shutdown its plugin needs). Three steps, none with a deadline of its own: a step whose
 * answer or log line never comes holds the calling row to its limit, which names the row.
 *
 * 1. A replay asked on a reply subject naming the session (`sessionReplySubject`) is answered with
 *    the session's history. The plugin's control subscription is in place once one is answered,
 *    so the ask is repeated, on the same reply subject, until then.
 * 2. The control: the same request asked on the reply subject Dispatch asked on before, nats.go's
 *    own request inbox (`<inbox>.<n>`), gets no answer, and the server logs the refused publish.
 *    The request is asked again on each read of the log. A grant that admitted every `_INBOX.`
 *    subject fails here at once, with the answer that arrived.
 * 3. A replay asked on the session's own prefix after the refusal is answered again. The ask is
 *    repeated until it is, so one lost ask does not hold the row to its limit.
 */
export async function runReplayGrantScenario(scenario: ReplayGrantScenario): Promise<void> {
  const { server, requester, sessionID } = scenario;
  const own = () => sessionReplySubject(createInbox(), sessionID);
  // The answers and the refusal cross a real socket from a real server, so there is no clock to
  // fake; every wait is on its condition.
  const answer = await askUntilAnswered(requester, sessionID, own());
  expect(JSON.parse(answer)).toMatchObject({ v: 1, session_id: sessionID });

  // The control. The server never delivers a publish it refused, so its `Publish Violation` line is
  // the evidence: subscribe, ask until that line is there, and only then read what arrived.
  const before = `${createInbox()}.1`;
  const arrived: string[] = [];
  const answers = requester.subscribe(before, {
    callback: (_error, message) => arrived.push(message.string()),
  });
  await requester.flush();
  const refused = (log: string) =>
    log
      .split("\n")
      .some((line) => line.includes("Publish Violation") && line.includes(`"${before}"`));
  const what = `the server's log of a refused publish to ${before}`;
  let logged = "";
  try {
    while (!refused(logged)) {
      throwIfClosed(requester, what);
      // An answer that arrived is the failure itself: a grant that admits every `_INBOX.` subject
      // delivers it, and the server logs no refusal to wait for.
      if (arrived.length > 0) {
        throw new Error(
          `a replay asked on ${before} was answered there: ${JSON.stringify(arrived)}`
        );
      }
      // Asked again on each poll, as the other steps ask, so one lost ask does not hold the row
      // to its limit.
      requester.publish(agentStreamControlSubject(sessionID), REPLAY_ASK, { reply: before });
      await Bun.sleep(100);
      logged = await server.log(performance.now() + LOG_READ_KILL_MS);
    }
    await requester.flush();
    expect(arrived).toEqual([]);
  } finally {
    answers.unsubscribe();
  }

  const again = await askUntilAnswered(requester, sessionID, own());
  expect(JSON.parse(again)).toMatchObject({ session_id: sessionID });
}
