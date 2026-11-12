import {
  AssistantRuntimeProvider,
  type ThreadMessageLike,
  useExternalStoreRuntime,
} from "@assistant-ui/react";
import { type AgentStreamCommand, isShellCommandReport } from "@legion/contracts";
import { type ReactNode, useCallback, useLayoutEffect, useMemo } from "react";

import type { Message, MessageRead } from "../../api/types";
import { compareTimestamps } from "../../lib/timestamps";
import type { SessionSends } from "../conversation/useSessionSends";
import { isViewer } from "../refs/actor";
import { AgentThread } from "./AgentThread";
import {
  type AgentConversation,
  type DispatchTurn,
  dispatchTurns,
  toThreadMessages,
  turnSaysBody,
} from "./conversation";
import {
  type DispatchAuthor,
  dispatchMetadata,
  type KeyedMessage,
  turnKey,
} from "./dispatch-marks";
import { type LivePlace, useLiveViews } from "./live-views";
import { storedMessages } from "./stored-messages";

/** Who wrote a stored message, when it was not the viewer: its author as Dispatch recorded it
 *  (a session's stamped title with it, for `AgentName` to name it by), and its issue if any. */
function otherAuthor(message: Message, viewer: string | undefined): DispatchAuthor | undefined {
  return isViewer(message.author, viewer)
    ? undefined
    : { actor: message.author, issue: message.issue_key ?? undefined };
}

/** The id a stored Dispatch message is rendered under, which a streamed turn that took that
 *  message over carries as its `turn` mark: one key for both, spelt here alone. */
function dispatchId(messageId: string): string {
  return `dispatch:${messageId}`;
}

/**
 * Whether a streamed user turn is the stored message it names, so the view draws the pair once
 * rather than one under the other, and which of the two it is.
 *
 * A turn saying what the person wrote is that message, beside what the stream names of what the
 * message carried, such as a picture's `[image]` (`turnSaysBody`, which holds the row to the
 * body's own lines and to the body's own pictures): `"says-body"`. So is the report of a
 * person's `!command` (INFRA-812, R4): the session runs the line itself and publishes the
 * person's line and what the command printed as one user message, because the person ran it and
 * the host wrote nothing (`shellCommandReport` in `packages/pi-envoy/src/typed-shell.ts`). What
 * a report reads like is one rule both packages import rather than spell twice
 * (`isShellCommandReport` in `@legion/contracts`): the body is a `!` command, and the turn opens
 * with that whole body and a blank line, so the turn still says what the person said and the
 * rest of it - the fenced output and how the command ended - is the session's answer to that
 * line. A report is named as one (`"report"`) because the session also posts it as its reply to
 * that message, which the caller draws nowhere.
 *
 * Anything else is left to show beside the stored copy (`"no"`): the bus is open to any client,
 * and a tag is no licence to put words in a person's mouth.
 */
function takesOverStored(turn: DispatchTurn, body: string): "no" | "report" | "says-body" {
  if (isShellCommandReport(turn.text, body)) return "report";
  return turnSaysBody(turn, body) ? "says-body" : "no";
}

/**
 * Everything that reads the conversation, in one component so a boundary can be put around it.
 *
 * assistant-ui converts every message while the runtime is built, so a message it refuses throws
 * from `useExternalStoreRuntime` — in the render of whichever component calls it. Called from the
 * page, it would throw where no boundary the page renders can catch it, and a bad frame would take
 * the header and the delivery controls with the thread. Here, a boundary the page puts around this
 * component does catch it, and the page's own render never touches a frame.
 */
export function AgentRuntimeThread({
  commands,
  conversation,
  empty,
  front,
  onSend,
  ownSendCount,
  sends,
  placeholder,
  replayCount,
  resetKey,
  sendDisabled,
  sessionId,
  stored,
  viewer,
}: {
  /** The slash commands the composer completes, or undefined to offer none. */
  commands: readonly AgentStreamCommand[] | undefined;
  conversation: AgentConversation;
  empty: string;
  /** Whether this is the view the person is looking at; the transcript's own scrolling reads
   *  it (`AgentThread`). */
  front: boolean;
  /** Hands `body` to the session as a write of its own, as the page's delivery mode stands. */
  onSend: (body: string) => void;
  /** Bumped once per message this viewer sent that the session accepted, so their own send takes
   *  them back to the live end (`AgentThread`'s `useLiveFollow`). */
  ownSendCount: number;
  /** Every message to the session Dispatch has not yet made, each shown as its own turn, and what
   *  acts on them. */
  sends: SessionSends;
  placeholder: string;
  /** Bumped once per rebuild of the whole transcript from the session's replay
   *  (`useAgentStream`'s `replays`). */
  replayCount: number;
  resetKey: string;
  /** Holds the composer's Send while a model or thinking switch awaits the session. */
  sendDisabled: boolean;
  sessionId: string;
  stored: readonly MessageRead[];
  /** The signed-in human's login: their own messages read as theirs, every other one names its
   *  author. */
  viewer: string | undefined;
}): ReactNode {
  const messages = useMemo(() => {
    const messagesOfReads = storedMessages(stored);
    // A person's direct message the session took as its own user turn is in the stream too,
    // normally tagged with its Dispatch id: then it shows once, where the session took it, still
    // naming whoever wrote it. An untagged one is returned unchanged and its stored copy is never
    // taken, so it shows twice (`packages/pi-envoy/AGENTS.md` says which turns go untagged). Any
    // bus client can publish a tag, so only a person's stored message can be one, and only a
    // streamed message saying what that person wrote - beside what the stream names of what the
    // message carried, such as a picture's `[image]` - or answering their `!` line, replaces its
    // stored copy (`takesOverStored`). A `!` line's report also reaches Dispatch as this
    // session's reply to that message, which is the same text once more: the turn the stream
    // took over is the one the reader gets, and that reply is drawn nowhere (`reported`).
    const byID = new Map(
      messagesOfReads
        .filter((message) => message.author.kind === "user")
        .map((message) => [message.id, message])
    );
    const turns = dispatchTurns(conversation);
    const taken = new Set<string>();
    // The `!` messages whose report the stream took over. The plugin posts that report twice -
    // this frame, and the reply on Dispatch every message to a session gets - so the stored
    // reply to one of these is the same text again, and the view draws it nowhere.
    const reported = new Set<string>();
    const streamed = toThreadMessages(conversation).map((message) => {
      const turn = turns.get(message.id ?? "");
      const source = byID.get(turn?.dispatchMessageId ?? "");
      if (turn === undefined || source === undefined) return message;
      // One read of the rule for this turn: it says both that the streamed turn replaces the
      // stored copy and, for a `!` report, that the session's reply saying it again is drawn
      // nowhere.
      const takeover = takesOverStored(turn, source.body);
      if (takeover === "no") return message;
      taken.add(source.id);
      if (takeover === "report") reported.add(source.id);
      // The streamed turn keeps the key the stored copy would have had, so it is one turn under
      // one key for `useLiveFollow` whether the reader sees this frame or, after a rebuild that
      // dropped it, the stored copy below under that same `dispatch:<id>` id.
      return {
        ...message,
        metadata: dispatchMetadata({
          author: otherAuthor(source, viewer),
          turn: dispatchId(source.id),
        }),
      };
    });
    // The session's own reply to a `!` message whose report the stream already took over: the
    // same text a third time, so the reader meets it nowhere.
    const answersReportedCommand = (message: Message): boolean =>
      message.author.kind === "session" &&
      message.author.id === sessionId &&
      message.in_reply_to !== null &&
      reported.has(message.in_reply_to);
    // Dispatch's side of the conversation, interleaved with the stream by time: what the session
    // wrote is its reply, what the viewer wrote is theirs, and anything anyone else sent the
    // session (another human's direct message, an issue message, another agent) says who. The
    // stream's times are whole milliseconds, so the merge below compares milliseconds; the stored
    // messages carry microseconds, so they are put in time order first, and the stable merge keeps
    // that order between two stored messages in one millisecond.
    const dispatch: KeyedMessage[] = messagesOfReads
      .filter((message) => !taken.has(message.id) && !answersReportedCommand(message))
      .sort((left, right) => compareTimestamps(left.created_at, right.created_at))
      .map((message) => {
        const content = [{ text: message.body, type: "text" as const }];
        const createdAt = new Date(message.created_at);
        const id = dispatchId(message.id);
        if (message.author.kind === "session" && message.author.id === sessionId) {
          return {
            content,
            createdAt,
            id,
            metadata: dispatchMetadata({ dispatch: true }),
            role: "assistant" as const,
            status: { reason: "stop", type: "complete" } as const,
          };
        }
        return {
          content,
          createdAt,
          id,
          metadata: dispatchMetadata({ author: otherAuthor(message, viewer) }),
          role: "user" as const,
        };
      });
    return [...streamed, ...dispatch].sort(
      (left, right) => (left.createdAt?.getTime() ?? 0) - (right.createdAt?.getTime() ?? 0)
    );
  }, [conversation, sessionId, stored, viewer]);

  // `MountedLiveViews` keys each view by its session, so this component never outlives a move to
  // another session - the shell mounts one of it per session - and a view that sleeps is
  // unmounted altogether (D6). So neither the box nor this component owns what the person has
  // written or where they were reading: the session's entry in the live-view store does.
  const views = useLiveViews();

  const runtime = useExternalStoreRuntime({
    // The frames already arrive in assistant-ui's own message shape, so the store's converter
    // is the identity: `conversation.ts` is the one place the session's wire shape is read.
    convertMessage: (message: ThreadMessageLike) => message,
    // Never running, and said so: left out, assistant-ui reads a running trailing assistant
    // message as a running thread, and while a thread runs without the `queue` capability it
    // disables Send and swallows Enter. Dispatch's runtime starts no run - the session decides
    // what a message does mid-turn by its delivery mode - and each streamed message carries its
    // own running or complete status (`toThreadMessages`), which still renders. Claiming the
    // `queue` capability instead would show a queue of messages nothing holds. Not running also
    // keeps assistant-ui from planting an empty upcoming assistant turn after the viewer's
    // newest message.
    isRunning: false,
    // The composer cannot send while a model or thinking switch is pending on this session, and
    // the box still takes the next message. A message still out, late or refused holds nothing:
    // the next one goes out behind it (INFRA-442).
    isSendDisabled: sendDisabled,
    messages,
    // assistant-ui's composer has already emptied the box when this runs. The write store owns the
    // message from here, under its key, and keeps its draft for Retry after a refusal.
    onNew: async (message: { content: readonly { type: string; text?: string }[] }) => {
      const body = message.content
        .filter((part) => part.type === "text")
        .map((part) => part.text ?? "")
        .join("\n")
        .trim();
      onSend(body);
    },
  });

  // The box holds this session's kept draft, and every change of it goes back to the store as it
  // is made. Before paint, so the box never shows one session's draft under another. React runs
  // this effect's own cleanup before it runs again and at unmount, so the last text is written
  // back under the session it was written to, whatever took the view away - a move to another
  // session, or the view sleeping, which unmounts it.
  useLayoutEffect(() => {
    const composer = runtime.thread.composer;
    composer.setText(views.draft(sessionId));
    const keep = () => views.setDraft(sessionId, composer.getState().text);
    const unsubscribe = composer.subscribe(keep);
    return () => {
      unsubscribe();
      keep();
    };
  }, [runtime, sessionId, views]);

  // Where this session's reader was, as the store holds it: read for the session being shown,
  // not watched, since the transcript below is what reports it and a render per scroll event
  // would buy nothing.
  const initialPlace = useMemo(() => views.place(sessionId), [sessionId, views]);
  const onPlace = useCallback(
    (place: LivePlace | null) => views.setPlace(sessionId, place),
    [sessionId, views]
  );

  // What `useLiveFollow` reads the transcript from, in render order; `AgentThread` renders the
  // same key into each turn's `data-message-id`, so the hook can find the turn a reader was on.
  const contentIds = useMemo(
    () => messages.map((message) => turnKey(message.metadata?.custom ?? {}, message.id)),
    [messages]
  );

  return (
    <AssistantRuntimeProvider runtime={runtime}>
      <AgentThread
        commands={commands}
        contentIds={contentIds}
        empty={empty}
        front={front}
        held={sendDisabled}
        initialPlace={initialPlace}
        onPlace={onPlace}
        ownSendCount={ownSendCount}
        placeholder={placeholder}
        replayCount={replayCount}
        resetKey={resetKey}
        sends={sends}
      />
    </AssistantRuntimeProvider>
  );
}
