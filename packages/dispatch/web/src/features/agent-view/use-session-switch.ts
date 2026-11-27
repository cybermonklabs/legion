import { useCallback, useEffect, useRef, useState } from "react";

import type { CreateAgentMessageInput, DisplayTree, Message, MessageRead } from "../../api/types";
import { rowAnsweredWithError } from "../conversation/delivery";
import { storedMessages } from "./stored-messages";

/** How long the page waits for the session to answer a switch, from the moment Dispatch answered
 *  the switch's send. The session answers a built-in `/model` or `/effort` at once, even mid-turn,
 *  so a switch with no answer by then is one whose session is not running it. */
const SWITCH_ANSWER_DEADLINE_MS = 30_000;

/** A switch the header asks for: the model, as `provider/id` exactly as the session listed it, or
 *  the thinking level, as one of the levels its `/effort` offers. */
export interface SwitchRequest {
  readonly kind: "model" | "thinking";
  readonly value: string;
}

/**
 * What a switch sends the session: the line a person would type at its terminal, the value as
 * the session listed it and never rewritten, and `connected_only`.
 *
 * A `/model` or `/effort` line applies to the session that reads it, so one stored for a session
 * that is not running would change its model at an unknown later time, which is worse than no
 * switch at all. `connected_only` asks the listener to refuse a stopped target instead, Dispatch
 * records the attempt failed with `the session is stopped`, and the header says so (case 1
 * below). The flag is not what decides it: the server holds a person's own Send or Aside whose
 * whole body is one such line to a running session with or without it (`isSessionSwitch` in
 * `packages/envoy/internal/dispatch/api/messages.go`), since a tab loaded before the field
 * existed sends the switch without it. Every other message to that session is stored for it and
 * handed over when it runs again.
 */
export function switchMessageInput(request: SwitchRequest): CreateAgentMessageInput {
  return {
    body: request.kind === "model" ? `/model ${request.value}` : `/effort ${request.value}`,
    connected_only: true,
    delivery: "steer",
  };
}

/** Where a switch is: its send is out (`posting`), Dispatch stored it and the page waits for the
 *  session (`waiting`), the deadline passed with no answer (`unanswered`), or it has its answer
 *  (`ended`). Only `posting` and `waiting` hold the pickers and Send. */
type SwitchPhase = "posting" | "waiting" | "unanswered" | "ended";

/** The line the header shows about a switch: the page's own words, and after them the session's
 *  reply where the line quotes one. The reply is a stored message, so the line carries it as a
 *  part the one reader draws (`SessionSwitchLines`) rather than as the text it stores. */
export interface SwitchStatus {
  readonly words: string;
  readonly reply?: {
    readonly body: string;
    readonly id: string;
    readonly tree: DisplayTree | undefined;
  };
}

interface SwitchState {
  /** Which switch this is, so a send that answers after a newer one began changes nothing. */
  readonly id: number;
  readonly sessionId: string;
  /** The stored message the switch is, once Dispatch answered its send. */
  readonly messageId?: string;
  readonly phase: SwitchPhase;
  readonly status: SwitchStatus;
}

export interface SessionSwitch {
  /** True from the pick until the switch ends: its send is out, or the session has not answered
   *  it and the deadline has not passed. */
  readonly pending: boolean;
  /** The line the header shows about the latest switch, or undefined before any. */
  readonly status: SwitchStatus | undefined;
  /**
   * Starts a switch whose send is `post`, and resolves with Dispatch's answer to it - `null` for a
   * switch Dispatch made and has no answer to give for, whose reply the thread shows when it
   * comes. A send Dispatch refused rejects, and the switch is dropped: nothing reached the
   * session, and the caller shows the refusal as it shows a refused Send.
   */
  readonly run: (
    request: SwitchRequest,
    post: () => Promise<Message | null>
  ) => Promise<Message | null>;
}

const NO_ANSWER = `No answer came from the session within ${SWITCH_ANSWER_DEADLINE_MS / 1000} seconds. The header shows the model and thinking level the session last reported.`;

const ANSWER_LOST =
  "The switch was sent, but Dispatch lost its answer; the session's reply shows in the thread. The header shows the model and thinking level the session last reported.";

/**
 * The session's answer to the switch `messageId`, as the stored thread holds it: its reply, which
 * the line quotes as the one reader draws it, or, while the switch still waits, its latest
 * attempt failing with an error. A failed attempt is the session's answer only when the session's
 * own error reply failed it (`rowAnsweredWithError`); one Dispatch failed itself means no answer
 * came. A delivery the session accepted as its user's turn has not failed, whatever a later send
 * recorded. Undefined while there is neither.
 */
function storedAnswer(
  stored: readonly MessageRead[],
  sessionId: string,
  messageId: string,
  waiting: boolean
): SwitchStatus | undefined {
  const messages = storedMessages(stored);
  const reply = messages.find(
    (message) =>
      message.in_reply_to === messageId &&
      message.author.kind === "session" &&
      message.author.id === sessionId
  );
  if (reply !== undefined) {
    return {
      reply: { body: reply.body, id: reply.id, tree: reply.body_tree },
      words: "The session answered: ",
    };
  }
  if (!waiting) return undefined;
  const switched = messages.find((message) => message.id === messageId);
  if (switched === undefined) return undefined;
  const deliveries = switched.deliveries ?? [];
  if (deliveries.some((attempt) => attempt.accepted_as === "user_turn")) return undefined;
  const attempt = deliveries.at(-1);
  if (attempt?.state !== "failed" || attempt.error === null) return undefined;
  return {
    words: rowAnsweredWithError(attempt)
      ? `The session answered: ${attempt.error}`
      : `No answer came from the session: ${attempt.error}`,
  };
}

/**
 * The live view's one switch at a time (INFRA-195). A switch is a person's Send of `/model` or
 * `/effort`, so it ends when the session answers that message, which `stored` (the session's
 * stored thread, refetched on each message event) carries:
 *
 * 1. Dispatch's answer to the send already records a failed attempt (no live session took it,
 *    or the session is stopped and the send asked for a running one).
 * 2. The session's reply to the message appears, including one already there when the send
 *    answered.
 * 3. The message's latest attempt turns failed with an error: the session's own error reply, or a
 *    failure Dispatch recorded itself, which means no answer came.
 * 4. `SWITCH_ANSWER_DEADLINE_MS` passes from the send's answer with none of these.
 *
 * A reply to the latest switch that lands after its deadline replaces the no-answer line. Only the
 * latest switch is watched, so an earlier switch's late reply shows in the thread and never
 * changes a newer switch's line. The header's values come from the session's `settings` frame
 * alone, so the order the reply and the frame arrive in does not matter.
 */
export function useSessionSwitch(
  sessionId: string,
  stored: readonly MessageRead[] | undefined
): SessionSwitch {
  const [current, setCurrent] = useState<SwitchState | null>(null);
  const latest = useRef(0);
  const deadline = useRef<number | undefined>(undefined);
  // A switch belongs to the session it was sent to: on another session's page it is not shown.
  const active = current?.sessionId === sessionId ? current : null;

  useEffect(() => () => window.clearTimeout(deadline.current), []);

  useEffect(() => {
    if (active?.messageId === undefined) return;
    if (active.phase !== "waiting" && active.phase !== "unanswered") return;
    const answer = storedAnswer(
      stored ?? [],
      sessionId,
      active.messageId,
      active.phase === "waiting"
    );
    if (answer === undefined) return;
    window.clearTimeout(deadline.current);
    setCurrent((previous) =>
      previous?.id === active.id ? { ...previous, phase: "ended", status: answer } : previous
    );
  }, [active, sessionId, stored]);

  const run = useCallback(
    async (
      request: SwitchRequest,
      post: () => Promise<Message | null>
    ): Promise<Message | null> => {
      latest.current += 1;
      const id = latest.current;
      window.clearTimeout(deadline.current);
      setCurrent({
        id,
        phase: "posting",
        sessionId,
        status: {
          words:
            request.kind === "model"
              ? `Switching model to ${request.value}…`
              : `Switching thinking level to ${request.value}…`,
        },
      });
      let answer: Message | null;
      try {
        answer = await post();
      } catch (error) {
        setCurrent((previous) => (previous?.id === id ? null : previous));
        throw error;
      }
      if (answer === null) {
        setCurrent((previous) =>
          previous?.id === id
            ? { ...previous, phase: "ended", status: { words: ANSWER_LOST } }
            : previous
        );
        return answer;
      }
      const attempt = (answer.deliveries ?? []).at(-1);
      if (attempt?.state === "failed") {
        const reason = attempt.error ?? "the attempt failed";
        setCurrent((previous) =>
          previous?.id === id
            ? {
                ...previous,
                messageId: answer.id,
                phase: "ended",
                status: { words: `No answer came from the session: ${reason}` },
              }
            : previous
        );
        return answer;
      }
      setCurrent((previous) =>
        previous?.id === id ? { ...previous, messageId: answer.id, phase: "waiting" } : previous
      );
      deadline.current = window.setTimeout(() => {
        setCurrent((previous) =>
          previous?.id === id && previous.phase === "waiting"
            ? { ...previous, phase: "unanswered", status: { words: NO_ANSWER } }
            : previous
        );
      }, SWITCH_ANSWER_DEADLINE_MS);
      return answer;
    },
    [sessionId]
  );

  return {
    pending: active?.phase === "posting" || active?.phase === "waiting",
    run,
    status: active?.status,
  };
}
