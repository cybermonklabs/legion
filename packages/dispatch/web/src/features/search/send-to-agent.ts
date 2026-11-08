import { api } from "../../api/client";
import type { Message } from "../../api/types";
import type { Attempt, Writes } from "../writes/write-store";
import { press } from "./offer";

/**
 * The words one send carries. The route the send goes through
 * (`POST /api/v1/agents/{session_id}/messages`) belongs to no issue, so the issue the person was
 * looking at is named in the text or the agent cannot tell which work this is about. Where the
 * bar's chip names a block of that issue (`Tab`), the block is named after the issue, in the
 * chip's own words, so the words the agent is handed are about the part the person was reading.
 * The person's own words follow it, as they typed them: nothing here rewrites them, and nothing
 * here adds a word of its own.
 */
export function handoffBody(issue: string, block: string | undefined, words: string): string {
  return `${block === undefined ? issue : `${issue}, ${block}`}: ${words}`;
}

/** What one approved offer sends. */
export interface SendToAgentWrite {
  /** The block of that issue the chip names, in the chip's words (`the block “…”`), and
   *  undefined where the chip names the issue itself. */
  readonly block: string | undefined;
  /** The issue the chip names, which the body names. */
  readonly issue: string;
  /** The session holding that issue's claim, which the message goes to. */
  readonly session: string;
  /** The words the person typed, as they typed them. */
  readonly words: string;
}

/** How the offer sends; the tests pass their own. It answers why the words did not reach the
 *  session, and undefined where they did. */
export type SendToAgent = (writes: Writes, write: SendToAgentWrite) => Promise<string | undefined>;

/**
 * Sends the person's words to the session on the issue, through the route every other client
 * sends a person's message to one session through. One press is one request, under a key of its
 * own, tried until Dispatch decides it and named in the shell's indicator while it is out;
 * `inline` because the bar says a refusal itself.
 *
 * `aside` is the mode: the words reach the session while its turn runs, which is what a person
 * handing over work at this moment means, and it is the mode the bar's other write (the first
 * message on a new issue) already uses.
 *
 * Dispatch answers the message once its first delivery attempt has settled, and an attempt can
 * settle as `failed` under a `201` - a session that does not advertise the mode is never
 * switched to another one, and the listener can refuse the send. The message is written either
 * way, so the answer here is the attempt's own reason rather than a throw: the bar says the
 * words did not reach the agent instead of saying they did.
 */
export async function sendToAgent(
  writes: Writes,
  { block, issue, session, words }: SendToAgentWrite
): Promise<string | undefined> {
  const write: Attempt<Message> = (bounds) =>
    api.createAgentMessage(
      session,
      { body: handoffBody(issue, block, words), delivery: "aside" },
      bounds
    );
  const message = await press(writes, `“${words}” to the agent on ${issue}`, write);
  const attempt = message?.deliveries.at(-1);
  if (attempt === undefined || attempt.state !== "failed") {
    return undefined;
  }
  return attempt.error ?? "Dispatch gave no reason.";
}
