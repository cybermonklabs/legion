import {
  type AgentStreamControlMessage,
  agentStreamControlSubject,
  agentStreamFramesSubject,
  comparableLines,
} from "@legion/contracts"
import { messageFor } from "@legion/envoy-client/errors"
import { AgentStreamPublisher } from "@legion/pi-shared/agent-stream"
import type { ChannelConnection, ChannelTopicSubscription } from "../channel-topics"
import type { CodexTranscriptEvent } from "./codex-transcript"

/**
 * The Agents page's live view of a Codex session.
 *
 * The conversation comes from the session's transcript (`./codex-transcript`): what the person
 * typed, what the model wrote and its reasoning summaries, and each tool call with its output,
 * become messages on the publisher both other hosts use (`@legion/pi-shared/agent-stream`), which
 * turns them into the frames the page draws and publishes them only while a viewer watches. A
 * line the plugin's hook put into a running turn reaches the transcript as an instruction to the
 * model rather than a message of the conversation, so it is recorded here as the message it is
 * (`delivered`). The viewer's control subject is answered as the Claude Code and Oh My Pi
 * sessions answer it (`../agent-stream-runner`, `packages/pi-envoy`).
 */
export interface CodexAgentStream {
  /** Records one event of the transcript of the session now bound. */
  event(event: CodexTranscriptEvent): void
  /** Records a line the hook took into a running turn, as a message of the conversation, tagged
   *  with the Dispatch message it is where it is a person's own. */
  delivered(text: string, dispatchMessageId?: string): void
  /** Expects the user message a person's message is about to become through `codex queue`, so
   *  its row is tagged with the Dispatch message it is and the page shows it once. */
  expect(text: string, dispatchMessageId: string): void
  /** Moves the stream to another session id (`/new`): the conversation starts empty. */
  rebind(sessionId: string): void
  /** The session's NATS connection closed for good and the server opened `connection` in its
   *  place (`channelReopener`): frames go out on it from now on, and the control subject is taken
   *  on it again once the connection is admitted as the session (`admitted`). */
  useConnection(connection: ChannelConnection): void
  stop(): void
}

export interface CodexAgentStreamOptions {
  readonly connection: ChannelConnection
  readonly sessionId: string
  readonly now?: () => number
  readonly warn?: (text: string) => void
  /**
   * Settles once the session's NATS connection is admitted as `session`, where the connection is
   * admitted as one session at a time: under its broker token a `/new`'s new id is the
   * connection's only once the move lands (`followSession`), and a subscribe before then is
   * refused and closed by the server. Where this is absent the connection may take every id's
   * subjects, and the new control subject is taken at once (`AgentStreamOptions.admitted`, the
   * Claude Code live view's).
   */
  readonly admitted?: (session: string) => Promise<void>
}

const encoder = new TextEncoder()
const decoder = new TextDecoder()
/** How long a control subscription that ended waits before it is taken again. */
const RESUBSCRIBE_MS = 1_000
/** How many expected person messages are kept for tagging. */
const EXPECTED_MAX = 16
/** Codex's models, as the page names a model: `provider/id`. */
const CODEX_PROVIDER = "openai"

export function startCodexAgentStream(options: CodexAgentStreamOptions): CodexAgentStream {
  /** The session's connection now; the server swaps in a new one when it closes for good. */
  let connection = options.connection
  const now = options.now ?? Date.now
  const warn =
    options.warn ?? ((text: string): void => void process.stderr.write(`envoy-channel: ${text}\n`))
  const publisher = new AgentStreamPublisher({
    now,
    // A closed connection throws on a publish, and a throw here would end the transcript's reader.
    // Its frames are dropped until the server swaps in a new connection; the ring keeps them for a
    // replay.
    publish: (subject, payload) => {
      if (!connection.isClosed()) connection.publish(subject, encoder.encode(payload))
    },
  })
  let session = options.sessionId
  let subscription: ChannelTopicSubscription | undefined
  let stopped = false
  let model: string | undefined
  /** The last timestamp handed to a message: each gets one strictly past it, since the publisher
   *  keys an assistant message by its timestamp. */
  let allocated = 0
  const expected: { key: string; dispatchMessageId: string }[] = []
  const toolNames = new Map<string, string>()

  const framesSubject = (): string => agentStreamFramesSubject(session)
  const at = (wanted: number): number => {
    allocated = Math.max(wanted, allocated + 1)
    return allocated
  }
  const record = (message: Record<string, unknown>, dispatchMessageId?: string): void => {
    publisher.record(framesSubject(), message, false, dispatchMessageId)
  }
  const tagFor = (text: string): string | undefined => {
    const key = comparableLines(text).join("\n")
    const index = expected.findIndex((entry) => entry.key === key)
    if (index < 0) return undefined
    const [entry] = expected.splice(index, 1)
    return entry?.dispatchMessageId
  }

  const publishSettings = (): void => {
    if (model === undefined) {
      publisher.setSettings(framesSubject(), { reported: false })
      return
    }
    const id = `${CODEX_PROVIDER}/${model}`
    publisher.setSettings(framesSubject(), {
      model: id,
      models: [id],
      reported: true,
      thinking: null,
    })
  }

  const pump = async (taken: ChannelTopicSubscription, forSession: string): Promise<void> => {
    try {
      for await (const message of taken) {
        try {
          const parsed: unknown = JSON.parse(decoder.decode(message.data))
          if (typeof parsed !== "object" || parsed === null) continue
          const request = parsed as AgentStreamControlMessage
          if (request.type !== "replay" && request.type !== "watch") continue
          const reply = message.reply
          const answer =
            request.type === "replay" && reply !== undefined
              ? JSON.stringify(publisher.replay(forSession))
              : undefined
          const arming = !publisher.watched
          publisher.noteViewer()
          if (arming) publisher.publishState(agentStreamFramesSubject(forSession))
          if (answer === undefined || reply === undefined) continue
          connection.publish(reply, encoder.encode(answer))
        } catch (error) {
          warn(`a live view control message failed — ${messageFor(error)}`)
        }
      }
    } catch {
      // A dead iterator is a dropped connection, taken again below.
    }
    if (subscription === taken) subscription = undefined
    if (stopped || session !== forSession) return
    setTimeout(() => {
      // Taken again meanwhile: by a `/new`'s new id, or on the connection the server swapped in.
      if (!stopped && session === forSession && subscription === undefined) {
        subscribeWhenAdmitted(forSession)
      }
    }, RESUBSCRIBE_MS).unref?.()
  }

  /** Takes the control subject on the connection held now. A closed one throws on a subscribe, so
   *  the subject waits for the connection the server swaps in (`useConnection`). */
  function subscribe(): void {
    if (stopped || connection.isClosed()) return
    subscription?.unsubscribe()
    const taken = connection.subscribe(agentStreamControlSubject(session))
    subscription = taken
    void pump(taken, session)
  }

  /** Takes `forSession`'s control subject once the connection may take it
   *  (`CodexAgentStreamOptions.admitted`), unless the stream moved on or took it meanwhile. */
  function subscribeWhenAdmitted(forSession: string): void {
    const admitted = options.admitted?.(forSession)
    if (admitted === undefined) {
      subscribe()
      return
    }
    void admitted.then(() => {
      if (!stopped && session === forSession && subscription === undefined) subscribe()
    })
  }
  subscribe()
  publishSettings()

  return {
    event(event) {
      if (stopped) return
      switch (event.kind) {
        case "settings":
          if (event.model !== model) {
            model = event.model
            publishSettings()
          }
          return
        case "user":
          record(
            {
              content: [{ text: event.text, type: "text" }],
              role: "user",
              tag: event.id,
              timestamp: at(event.at),
            },
            tagFor(event.text),
          )
          return
        case "assistant":
          record({
            content: [{ text: event.text, type: "text" }],
            ...(model === undefined ? {} : { model, provider: CODEX_PROVIDER }),
            role: "assistant",
            timestamp: at(event.at),
          })
          return
        case "reasoning":
          record({
            content: [{ thinking: event.text, type: "thinking" }],
            role: "assistant",
            timestamp: at(event.at),
          })
          return
        case "tool-call": {
          toolNames.set(event.callId, event.name)
          let args: unknown = event.arguments
          try {
            args = JSON.parse(event.arguments)
          } catch {
            // Arguments that are not JSON are shown as the text they are.
          }
          record({
            content: [{ arguments: args, id: event.callId, name: event.name, type: "toolCall" }],
            role: "assistant",
            timestamp: at(event.at),
          })
          return
        }
        case "tool-result":
          record({
            content: [{ text: event.output, type: "text" }],
            role: "toolResult",
            timestamp: at(event.at),
            toolCallId: event.callId,
            toolName: toolNames.get(event.callId) ?? "tool",
          })
          return
        default:
          return
      }
    },
    delivered(text, dispatchMessageId) {
      if (stopped) return
      // A person's message the hook took never becomes a user row of the transcript, so the
      // tag kept for that row is dropped rather than left for a later message with the same words.
      if (dispatchMessageId !== undefined) {
        const index = expected.findIndex((entry) => entry.dispatchMessageId === dispatchMessageId)
        if (index >= 0) expected.splice(index, 1)
      }
      record(
        { content: [{ text, type: "text" }], role: "user", timestamp: at(now()) },
        dispatchMessageId,
      )
    },
    expect(text, dispatchMessageId) {
      expected.push({ dispatchMessageId, key: comparableLines(text).join("\n") })
      while (expected.length > EXPECTED_MAX) expected.shift()
    },
    rebind(next) {
      if (stopped || next === session) return
      session = next
      publisher.reset()
      expected.length = 0
      toolNames.clear()
      publishSettings()
      // The old id's control subject goes at once, and the new id's is taken once the connection
      // may take it (`CodexAgentStreamOptions.admitted`): a later `/new` before then takes its own.
      subscription?.unsubscribe()
      subscription = undefined
      subscribeWhenAdmitted(next)
    },
    useConnection(next) {
      connection = next
      if (stopped) return
      // The subscription on the closed connection is dead; the new one is taken through the gate,
      // since a `/new` whose move had not landed when the connection closed is still not admitted.
      subscription?.unsubscribe()
      subscription = undefined
      subscribeWhenAdmitted(session)
    },
    stop() {
      stopped = true
      subscription?.unsubscribe()
    },
  }
}
