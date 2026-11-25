import { expect } from "bun:test";
import { agentStreamControlSubject } from "@legion/contracts";
import { settled, until } from "@legion/test-fixtures/wait";
import { createInbox, type NatsConnection } from "nats";
import { type NatsServer, sessionReplySubject } from "./nats-server";

/** The replay request, as Dispatch's `agentstream.NATS.Replay` sends it. */
const REPLAY_ASK = new TextEncoder().encode(JSON.stringify({ v: 1, type: "replay" }));

/** How long an ask goes unanswered before it is asked again: the scenario's pacing of its ask, not
 *  a deadline. Each ask of one wait goes to the same reply subject, so an answer to any of them
 *  ends the wait, however late it comes. */
const ASK_AGAIN_MS = 500;

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
 * answer, and the ask goes again. The wait for the answer is `settled`'s
 * (`@legion/test-fixtures/wait`), so a session that never answers fails it, naming the reply
 * subject, at its row's deadline. It throws once `requester` has closed: its subscription closes
 * with it.
 */
async function askUntilAnswered(
  requester: NatsConnection,
  sessionID: string,
  reply: string
): Promise<string> {
  const what = `a replay answer on ${reply}`;
  const answers = requester.subscribe(reply);
  const ask = (): void => {
    // A connection closed between two asks asks nothing more: the wait below ends on its
    // subscription's close.
    if (requester.isClosed()) return;
    requester.publish(agentStreamControlSubject(sessionID), REPLAY_ASK, { reply });
  };
  try {
    await requester.flush();
    // Undefined once the subscription has closed with no answer, as it does with its connection.
    const answered = (async () => {
      for await (const answer of answers) {
        if (answer.headers?.code !== 503) return answer.string();
      }
      return undefined;
    })();
    throwIfClosed(requester, what);
    ask();
    const askAgain = setInterval(ask, ASK_AGAIN_MS);
    let answer: string | undefined;
    try {
      answer = await settled(what, answered);
    } finally {
      clearInterval(askAgain);
    }
    if (answer === undefined) {
      throwIfClosed(requester, what);
      throw new Error(`stopped waiting for ${what}: its subscription closed with no answer`);
    }
    return answer;
  } finally {
    answers.unsubscribe();
  }
}

/** What a scenario needs: the server the session's grant is on, and the requester's connection. */
export interface ReplayGrantScenario {
  /** Started with `sessionGrantConfig` for `sessionID`. */
  readonly server: NatsServer;
  /** A connection as that configuration's requester, which may do anything. The caller closes it
   *  once its row has ended: each wait of the scenario throws once it has closed, so a row that
   *  ended stops there. */
  readonly requester: NatsConnection;
  readonly sessionID: string;
}

/**
 * The replay answer of a session holding the NATS auth service's session grant, whose plugin the
 * caller has booted on its own connection to `server` (the caller keeps the boot, the connection
 * and the shutdown its plugin needs). Three steps, each a wait of `@legion/test-fixtures/wait`
 * with no deadline of its own: a step whose answer or log line never comes fails, naming what it
 * waited for, at its row's deadline.
 *
 * 1. A replay asked on a reply subject naming the session (`sessionReplySubject`) is answered with
 *    the session's history. The plugin's control subscription is in place once one is answered,
 *    so the ask is repeated, on the same reply subject, until then.
 * 2. The control: the same request asked on the reply subject Dispatch asked on before, nats.go's
 *    own request inbox (`<inbox>.<n>`), gets no answer, and the server logs the refused publish.
 *    The request is asked again on each read of the log. A grant that admitted every `_INBOX.`
 *    subject fails here at once, with the answer that arrived.
 * 3. A replay asked on the session's own prefix after the refusal is answered again. The ask is
 *    repeated until it is, so one lost ask does not fail the row.
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
  try {
    await until(what, async () => {
      throwIfClosed(requester, what);
      // An answer that arrived is the failure itself: a grant that admits every `_INBOX.` subject
      // delivers it, and the server logs no refusal to wait for.
      if (arrived.length > 0) {
        throw new Error(
          `a replay asked on ${before} was answered there: ${JSON.stringify(arrived)}`
        );
      }
      // Asked again on each read of the log, as the other steps ask, so one lost ask does not
      // fail the row.
      requester.publish(agentStreamControlSubject(sessionID), REPLAY_ASK, { reply: before });
      return refused(await server.log());
    });
    await requester.flush();
    expect(arrived).toEqual([]);
  } finally {
    answers.unsubscribe();
  }

  const again = await askUntilAnswered(requester, sessionID, own());
  expect(JSON.parse(again)).toMatchObject({ session_id: sessionID });
}
