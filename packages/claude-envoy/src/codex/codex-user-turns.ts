import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery"
import type { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { acceptUserTurn } from "@legion/pi-shared/accept-user-turn"
import { typedInputReply } from "@legion/pi-shared/dispatch-user-turn"
import type { UserTurnRouter } from "../user-turn-router"
import type { UserTurnState } from "../user-turn-state"
import type { CodexInboxItem } from "./codex-inbox"

/**
 * What a frame from Dispatch's Agents page becomes in a Codex session (INFRA-3558), the Codex
 * counterpart of `../user-turn-router`.
 *
 * - A BTW is answered in a throwaway copy of the session's conversation (`askSideQuestion`,
 *   `./codex-cli`) and the answer posted back as its reply; the session's own conversation is
 *   left as it was.
 * - A person's Send or Aside is accepted through the step both other hosts take
 *   (`acceptUserTurn`, `@legion/pi-shared/accept-user-turn`), and the body Dispatch stored is put
 *   into the session as the person's own words (`deliver`, `./codex-delivery`).
 * - A `/` message is answered that it was not run: Codex runs no command it did not read from its
 *   own terminal.
 *
 * Everything else keeps the card it renders as.
 */
export interface CodexUserTurnOptions {
  readonly sessionId: () => string
  readonly state: UserTurnState
  readonly dispatch: (ms: number) => DispatchClient | undefined
  readonly reply: (
    delivery: DispatchDelivery,
    result: { readonly body?: string; readonly error?: string },
  ) => Promise<void>
  /** Puts a line into the session (`./codex-delivery`). */
  readonly deliver: (item: CodexInboxItem) => Promise<void>
  /** Answers a side question in a throwaway copy of the session's conversation. */
  readonly ask: (question: string) => Promise<string>
  /** Tells the live view the user message a person's message is about to become. */
  readonly expect: (text: string, dispatchMessageId: string) => void
  readonly warn: (text: string) => void
}

const ACCEPT_TIMEOUT_MS = 10_000

/** Why a `/` message from Dispatch was not run in a Codex session. */
export const CODEX_COMMAND_REFUSAL =
  "a Codex session runs only the commands typed at its own terminal; nothing was sent"

export function createCodexUserTurnRouter(options: CodexUserTurnOptions): UserTurnRouter {
  const { state, warn } = options

  const runBtw = (delivery: DispatchDelivery): void => {
    void (async () => {
      try {
        await options.reply(delivery, { body: await options.ask(delivery.body) })
      } catch (error) {
        await options.reply(delivery, { error: messageFor(error) })
      }
    })().catch((error: unknown) => {
      warn(`a BTW answer did not reach Dispatch — ${messageFor(error)}`)
    })
  }

  return {
    async route(rendered: RenderInboundResult) {
      const btw = rendered.delivery
      if (btw?.mode === "btw") {
        runBtw(btw)
        return true
      }
      const accepted = await acceptUserTurn({
        actorId: options.sessionId(),
        dispatch: () => options.dispatch(ACCEPT_TIMEOUT_MS),
        handle: state.handle,
        handled: state.handled,
        rendered,
        warn: (refusal) => {
          warn(
            `Dispatch message ${refusal.delivery.id} arrives as a card (${refusal.reason === "failed" ? messageFor(refusal.error) : refusal.reason})`,
          )
        },
      })
      if (accepted === undefined) return false
      const { delivery, turn } = accepted
      if (turn.body.startsWith("/")) {
        const said = typedInputReply(turn.body, { error: CODEX_COMMAND_REFUSAL })
        if (said !== undefined) {
          void options.reply(delivery, { body: said }).catch((error: unknown) => {
            warn(`a reply to message ${delivery.id} did not reach Dispatch — ${messageFor(error)}`)
          })
        }
        return true
      }
      options.expect(turn.body, turn.messageId)
      await options.deliver({
        content: turn.body,
        dispatchMessageId: turn.messageId,
        ...(rendered.envelope?.event_id === undefined
          ? {}
          : { eventId: rendered.envelope.event_id }),
      })
      return true
    },
    observe() {
      // Codex reports no mod events: the live view tags a person's row from the transcript.
    },
  }
}
